package router

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

// Claude can emit a system-role notification or a user-role system reminder.
const backgroundNotificationMarker = "[SYSTEM NOTIFICATION - NOT USER INPUT]"

const maxNotificationCalls = 32

var notificationCallID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

type continuationNotice struct {
	calls           []string
	hasNotification bool
}

// A tool result can accompany a system notification. Require its exact origin
// too; a tool result on its own never establishes a new question's parent.
func (n *continuationNotice) add(role string, content any) bool {
	switch v := content.(type) {
	case string:
		return n.addText(role, v)
	case []any:
		for _, block := range v {
			obj, _ := block.(map[string]any)
			switch obj["type"] {
			case "text":
				text, ok := obj["text"].(string)
				if !ok || !n.addText(role, text) {
					return false
				}
			case "tool_result":
				id, _ := obj["tool_use_id"].(string)
				if role != "user" || !notificationCallID.MatchString(id) || len(n.calls) >= maxNotificationCalls {
					return false
				}
				n.calls = append(n.calls, id)
			default:
				return false
			}
		}
		return len(v) > 0
	}
	return false
}

func (n *continuationNotice) addText(role, text string) bool {
	var ids []string
	var ok bool
	text = strings.TrimSpace(text)
	if payload, marked := strings.CutPrefix(text, backgroundNotificationMarker); role == "system" && marked {
		ids, ok = notificationIDs(payload)
	} else if role == "system" && !strings.HasPrefix(text, "<system-reminder") {
		// Normal system metadata (e.g. environment) can trail a human turn.
		return !strings.Contains(text, "<task-notification")
	} else {
		ids, ok = reminderCalls(text)
	}
	if !ok || len(n.calls)+len(ids) > maxNotificationCalls {
		return false
	}
	if len(ids) > 0 {
		n.hasNotification = true
		n.calls = append(n.calls, ids...)
	}
	return true
}

func reminderCalls(text string) ([]string, bool) {
	var calls []string
	for rest := strings.TrimSpace(text); rest != ""; {
		start := reminderStart.FindStringIndex(rest)
		if start == nil || start[0] != 0 {
			return nil, false
		}
		body, tail, closed := strings.Cut(rest[start[1]:], "</system-reminder>")
		if !closed {
			return nil, false
		}
		if payload, marked := strings.CutPrefix(strings.TrimSpace(body), backgroundNotificationMarker); marked {
			ids, ok := notificationIDs(payload)
			if !ok || len(calls)+len(ids) > maxNotificationCalls {
				return nil, false
			}
			calls = append(calls, ids...)
		} else if strings.Contains(body, "<task-notification") {
			return nil, false
		}
		rest = strings.TrimSpace(tail)
	}
	return calls, true
}

// Claude includes prose before the XML. Only a direct tool-use-id child of a
// task-notification is metadata; a tool result quoted in its summary is not.
func notificationIDs(payload string) ([]string, bool) {
	decoder := xml.NewDecoder(strings.NewReader(payload))
	var calls []string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return calls, len(calls) != 0
		}
		if err != nil {
			return nil, false
		}
		switch node := token.(type) {
		case xml.CharData:
		case xml.StartElement:
			var event struct {
				IDs []string `xml:"tool-use-id"`
			}
			if node.Name.Local != "task-notification" || decoder.DecodeElement(&event, &node) != nil || len(event.IDs) != 1 {
				return nil, false
			}
			id := strings.TrimSpace(event.IDs[0])
			if !notificationCallID.MatchString(id) || len(calls) >= maxNotificationCalls {
				return nil, false
			}
			calls = append(calls, id)
		default:
			return nil, false
		}
	}
}

func backgroundTool(name string) bool {
	switch name {
	case "Bash", "Agent", "Task", "Monitor":
		return true
	}
	return false
}

// Filters accept either the original prompt or one of its continuation IDs.
const resolvedPromptSQL = `COALESCE((SELECT root FROM task_continuations WHERE session=?1 AND prompt=?2),?2)`

type taskContinuations map[taskKey]string

func (c taskContinuations) root(key taskKey) taskKey {
	if root := c[key]; root != "" {
		return taskKey{key.Session, root}
	}
	return key
}

// Insert the first record and its attribution atomically. The record insert
// obtains SQLite's writer lock before checking prior prompts and tool IDs.
func (s *Store) startRequest(r Record, calls []string) error {
	if len(calls) == 0 || len(calls) > maxNotificationCalls || r.Class != "main" || r.Agent != "" || r.Prompt == "" || strings.HasPrefix(r.Session, "unattributed-") {
		return s.Save(r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := saveRecord(ctx, tx, r); err != nil {
		return err
	}
	if err := bindTaskContinuation(tx, r, calls); err != nil {
		return err
	}
	return tx.Commit()
}

func bindTaskContinuation(tx *sql.Tx, r Record, calls []string) error {
	session, prompt := r.Session, r.Prompt
	var alreadyKnown bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM records WHERE session=?1 AND prompt=?2 AND id<>?3 AND kind='inference'
		AND COALESCE(json_extract(payload,'$.request_class'),'main') NOT IN ('auxiliary','compaction'))
		OR EXISTS(SELECT 1 FROM task_continuations WHERE session=?1 AND prompt=?2)`, session, prompt, r.ID).Scan(&alreadyKnown); err != nil || alreadyKnown {
		return err
	}
	root := ""
	for _, call := range calls {
		if call == "" {
			return nil
		}
		var origin string
		// Ambiguous or missing bindings cannot establish a parent.
		err := tx.QueryRow(`SELECT MIN(r.root_prompt)
			FROM cooperation_batch_calls c
			LEFT JOIN task_record_roots r ON r.id=c.request AND r.session=c.session
			WHERE c.session=? AND c.call=?
			HAVING COUNT(*)=1 AND COUNT(r.id)=1 AND MIN(r.prompt)<>'' AND MIN(c.tool) IN ('Bash','Agent','Task','Monitor')`,
			session, call).Scan(&origin)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if origin == prompt || (root != "" && origin != root) {
			return nil
		}
		root = origin
	}
	_, err := tx.Exec("INSERT INTO task_continuations(session,prompt,root) VALUES(?,?,?)", session, prompt, root)
	return err
}
