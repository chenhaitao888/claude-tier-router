package router

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

const DelegationHookPath = "/api/cooperation/delegation"

var defaultBatchTools = map[string]string{
	"Glob": "suggest", "Grep": "suggest",
	"mcp__*__query_submit": "handoff", "mcp__*__ai_query_submit": "handoff",
}
var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (c CooperationConfig) batchTools() map[string]string {
	if c.BatchTools == nil {
		return defaultBatchTools
	}
	return c.BatchTools
}

func (c CooperationConfig) validateBatchTools() error {
	if len(c.BatchTools) > 100 {
		return errors.New("cooperation.batch_tools supports at most 100 tools")
	}
	for name, mode := range c.BatchTools {
		literal := strings.TrimPrefix(name, "mcp__*__")
		if len(name) > 200 || (literal != name && strings.Contains(literal, "__")) || !toolName.MatchString(literal) || name == "Agent" || name == "Task" ||
			(mode != "suggest" && mode != "handoff") {
			return fmt.Errorf("invalid cooperation.batch_tools entry %q: use an exact tool or mcp__*__method and suggest or handoff", name)
		}
	}
	return nil
}

// Both sides use anchored tool names. The only wildcard matches an MCP server,
// never a method such as poll_result or a different built-in tool.
func (c CooperationConfig) BatchToolMatcher() string {
	var patterns []string
	for name := range c.batchTools() {
		if method, wildcard := strings.CutPrefix(name, "mcp__*__"); wildcard {
			patterns = append(patterns, "mcp__[A-Za-z0-9_-]+__"+regexp.QuoteMeta(method))
		} else {
			patterns = append(patterns, regexp.QuoteMeta(name))
		}
	}
	if len(patterns) == 0 {
		return ""
	}
	sort.Strings(patterns)
	return "^(" + strings.Join(patterns, "|") + ")$"
}

func (c CooperationConfig) batchMode(tool string) string {
	rules := c.batchTools()
	if mode := rules[tool]; mode != "" {
		return mode
	}
	if serverMethod, ok := strings.CutPrefix(tool, "mcp__"); ok && toolName.MatchString(serverMethod) {
		if sep := strings.LastIndex(serverMethod, "__"); sep > 0 {
			return rules["mcp__*__"+serverMethod[sep+2:]]
		}
	}
	return ""
}

type batchEvent struct {
	Session string          `json:"session_id"`
	Prompt  string          `json:"prompt_id"`
	Agent   string          `json:"agent_id"`
	Event   string          `json:"hook_event_name"`
	Tool    string          `json:"tool_name"`
	Call    string          `json:"tool_use_id"`
	Input   json.RawMessage `json:"tool_input"`
}

type BatchCheck struct {
	Tier      string `json:"tier"`
	Tool      string `json:"tool"`
	State     string `json:"state"`
	Continued bool   `json:"continued,omitempty"`
}

func hasAgentTool(raw json.RawMessage) bool {
	var tools []struct{ Name string }
	if json.Unmarshal(raw, &tools) == nil {
		for _, tool := range tools {
			if tool.Name == "Agent" || tool.Name == "Task" {
				return true
			}
		}
	}
	return false
}

func batchDigest(raw json.RawMessage) string {
	var args map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&args) != nil || len(args) == 0 {
		return ""
	}
	// Marshal sorts object keys (including nested objects) and preserves large IDs.
	encoded, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func (s *Store) bindDelegationTool(r Record, call, tool string) error {
	if call == "" || len(call) > 200 || len(tool) > 200 {
		return nil
	}
	// Retried hook delivery uses the original decision. A tool ID emitted by
	// two model requests is ambiguous even if both have the same prompt ID.
	_, err := s.db.Exec(`INSERT INTO cooperation_batch_calls(session,call,tool,request) VALUES(?,?,?,?)
		ON CONFLICT(session,call,tool) DO UPDATE SET request='' WHERE request<>excluded.request`,
		r.Session, call, tool, r.ID)
	return err
}

func (g *Gateway) delegationHook(w http.ResponseWriter, r *http.Request) {
	var e batchEvent
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	if decoder.Decode(&e) != nil || decoder.Decode(new(any)) != io.EOF {
		errorJSON(w, 400, "invalid delegation hook")
		return
	}
	reply := map[string]any{}
	rules := g.Config.Cooperation
	if g.Config.Mode != "auto" || len(rules.batchTools()) == 0 || e.Agent != "" || e.Session == "" || e.Call == "" ||
		len(e.Call) > 200 || len(e.Session) > 200 || len(e.Prompt) > 200 || len(e.Tool) > 200 {
		jsonResponse(w, reply)
		return
	}
	var decision string
	var err error
	switch e.Event {
	case "PreToolUse":
		if mode := rules.batchMode(e.Tool); mode != "" {
			if digest := batchDigest(e.Input); digest != "" {
				decision, err = g.Store.checkBatch(e, digest, mode)
			}
		}
	case "PostToolUseFailure":
		var in struct {
			Type string `json:"subagent_type"`
		}
		if (e.Tool == "Agent" || e.Tool == "Task") && json.Unmarshal(e.Input, &in) == nil && in.Type == ExecutorModel {
			_, err = g.Store.db.Exec(`UPDATE cooperation_batch_calls SET outcome='executor_failed'
				WHERE session=? AND call=? AND tool=? AND request IN
				(SELECT id FROM records WHERE session=? AND (?='' OR prompt=?))`,
				e.Session, e.Call, e.Tool, e.Session, e.Prompt, e.Prompt)
		}
	}
	if err != nil {
		g.logError("could not record batch delegation check")
		jsonResponse(w, reply)
		return
	}
	const hint = "Repeated tool work: if the remaining steps and checks are defined and delegation is permitted, give them to a foreground router-executor with prior results and pending job IDs. Keep decisions and verification in the parent. If direct work is necessary, explain why and continue within the original permissions."
	switch decision {
	case "handoff":
		reply["hookSpecificOutput"] = map[string]string{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": hint}
	case "review_hint":
		review := "Continue mechanical batches under the agreed plan. Only if definitions or scope change, evidence conflicts, or correctness requires independent judgment, ask a foreground router-reviewer to check the specific uncertainty with the original criteria and evidence."
		reply["hookSpecificOutput"] = map[string]string{"hookEventName": "PreToolUse", "additionalContext": review}
	case "suggest":
		reply["hookSpecificOutput"] = map[string]string{"hookEventName": "PreToolUse", "additionalContext": hint}
	}
	jsonResponse(w, reply)
}

// One probe per (session, question, concrete tool). All new batches in the
// triggering model reply share one handoff; the parent can proceed on its next
// model turn. Re-delivery of the same hook always yields its original result.
func (s *Store) checkBatch(e batchEvent, digest, mode string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// Reserve the writer before taking a snapshot, including when another
	// gateway process is using the same ledger (SQLite WAL).
	if _, err := tx.Exec(`UPDATE cooperation_batch_calls SET outcome=outcome WHERE session=? AND call=? AND tool=?`, e.Session, e.Call, e.Tool); err != nil {
		return "", err
	}
	var request, prompt, outcome, tier string
	var agentAvailable bool
	err = tx.QueryRow(`SELECT r.id,COALESCE(a.root,r.prompt),c.outcome,json_extract(r.payload,'$.actual_tier'),json_extract(r.payload,'$.cooperation.agent_tool_available')
		FROM cooperation_batch_calls c JOIN records r ON r.id=c.request
		LEFT JOIN task_continuations a ON a.session=r.session AND a.prompt=r.prompt
		WHERE c.session=? AND c.call=? AND c.tool=? AND (?='' OR r.prompt=?)
		AND json_type(r.payload,'$.cooperation.agent_tool_available') IN ('true','false')`,
		e.Session, e.Call, e.Tool, e.Prompt, e.Prompt).Scan(&request, &prompt, &outcome, &tier, &agentAvailable)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil || outcome != "" {
		return outcome, err
	}
	// Independent tier ledgers keep a low-tier hint from consuming the
	// high-tier handoff if the task subsequently needs the coordinator.
	table := "cooperation_batches"
	if tier == "low" && mode == "handoff" {
		table = "cooperation_low_batches"
		mode = "review_hint"
	} else if tier != "high" {
		return "", nil
	}

	var first, signal, state string
	err = tx.QueryRow(`SELECT first_digest,signal_request,state FROM `+table+`
		WHERE session=? AND prompt=? AND tool=?`, e.Session, prompt, e.Tool).Scan(&first, &signal, &state)
	outcome = "pass"
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.Exec(`INSERT INTO `+table+`(session,prompt,tool,first_digest) VALUES(?,?,?,?)`, e.Session, prompt, e.Tool, digest)
	case err != nil:
		return "", err
	case digest == first:
		// Identical probe retries, including those with fresh tool call IDs.
	case signal == "" && !agentAvailable:
		_, err = tx.Exec(`UPDATE `+table+` SET state='no_agent' WHERE session=? AND prompt=? AND tool=?`, e.Session, prompt, e.Tool)
	case signal == "":
		outcome = mode
		_, err = tx.Exec(`UPDATE `+table+` SET signal_request=?,state=? WHERE session=? AND prompt=? AND tool=?`, request, mode, e.Session, prompt, e.Tool)
	case signal == request && state == "handoff":
		outcome = state
	case signal != request:
		_, err = tx.Exec(`UPDATE `+table+` SET continued=1 WHERE session=? AND prompt=? AND tool=?`, e.Session, prompt, e.Tool)
	}
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`UPDATE cooperation_batch_calls SET outcome=? WHERE session=? AND call=? AND tool=?`, outcome, e.Session, e.Call, e.Tool)
	if err != nil {
		return "", err
	}
	return outcome, tx.Commit()
}

func (s *Store) addDelegationChecks(groups map[taskKey]*TaskSummary, continuations taskContinuations) error {
	rows, err := s.db.Query(`SELECT session,prompt,tool,state,continued,'high' FROM cooperation_batches
		UNION ALL SELECT session,prompt,tool,state,continued,'low' FROM cooperation_low_batches
		UNION ALL SELECT DISTINCT r.session,r.prompt,'','executor_failed',0,'' FROM cooperation_batch_calls c
		JOIN records r ON r.id=c.request WHERE c.outcome='executor_failed' ORDER BY 1,2,3,6`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key taskKey
		var check BatchCheck
		if err := rows.Scan(&key.Session, &key.Prompt, &check.Tool, &check.State, &check.Continued, &check.Tier); err != nil {
			return err
		}
		if task := groups[continuations.root(key)]; task != nil {
			if check.State == "executor_failed" {
				task.ExecutorCallFailed = true
			} else {
				task.BatchChecks = append(task.BatchChecks, check)
			}
		}
	}
	return rows.Err()
}
