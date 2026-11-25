package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Request struct {
	Model                string
	RequestClass         string
	CooperationRole      string
	CooperationDelegated bool
	CooperationFollowup  string
	Raw                  []byte
	Fields               map[string]json.RawMessage
	Messages             []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	Stream            bool
	Tokens            int
	MaxTokens         int
	Text              string
	RecentText        string
	HasHistory        bool
	Protected         bool
	ComplexContent    bool
	ToolErrors        int
	CacheEnabled      bool
	Cache1h           bool
	Prefixes          []string
	PrefixTokens      []int
	CacheBreakpoints  []bool
	CacheableTokens   int
	TaskExtraction    string
	ReminderBlocks    int
	ProtectionReasons []string
	CacheShape        map[string]string
	NonPromptBytes    int
}

func ParseRequest(raw []byte) (Request, error) {
	r := Request{Raw: raw}
	if _, err := ReplaceModel(raw, "validation"); err != nil {
		return r, err
	}
	if err := json.Unmarshal(raw, &r.Fields); err != nil {
		return r, err
	}
	json.Unmarshal(r.Fields["model"], &r.Model)
	if err := json.Unmarshal(r.Fields["messages"], &r.Messages); err != nil || len(r.Messages) == 0 {
		return r, errors.New("messages must be a nonempty array")
	}
	if b, ok := r.Fields["stream"]; ok {
		if err := json.Unmarshal(b, &r.Stream); err != nil {
			return r, err
		}
	}
	if b, ok := r.Fields["max_tokens"]; ok {
		if json.Unmarshal(b, &r.MaxTokens) != nil || r.MaxTokens <= 0 {
			return r, errors.New("max_tokens must be positive")
		}
	}
	// Estimate tokens conservatively; the provider's usage remains authoritative.
	r.Tokens = len(raw)/2 + 1
	var recent []string
	for _, m := range r.Messages {
		if m.Role == "assistant" {
			r.HasHistory = true
		}
		var content any
		if err := json.Unmarshal(m.Content, &content); err != nil {
			return r, err
		}
		texts, removed := taskText(plainText(content))
		r.ReminderBlocks += removed
		if m.Role == "user" && strings.TrimSpace(texts) != "" {
			r.Text = texts
		}
		if texts != "" {
			recent = append(recent, m.Role+": "+texts)
		}
	}
	// Inspect all fields for cache hints and unsupported server state.
	var all any
	json.Unmarshal(raw, &all)
	scanContent(all, &r)
	r.TaskExtraction = "last_user_text"
	if r.ReminderBlocks > 0 {
		r.TaskExtraction = "system_reminders_removed"
	}
	if r.Text == "" {
		r.TaskExtraction = "no_task_text"
	}
	r.RecentText = strings.Join(recent, "\n")
	r.RecentText = tailChars(r.RecentText, 24000)
	r.buildPrefixes()
	return r, nil
}
func scanContent(v any, r *Request) {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["cache_control"]; ok {
			r.CacheEnabled = true
		}
		if ttl, ok := x["ttl"].(string); ok && ttl == "1h" {
			r.Cache1h = true
		}
		if t, ok := x["type"].(string); ok {
			switch t {
			case "thinking", "redacted_thinking", "server_tool_use", "web_search_tool_result", "code_execution_tool_result", "mcp_tool_use", "mcp_tool_result", "container_upload":
				r.Protected = true
				found := false
				for _, reason := range r.ProtectionReasons {
					if reason == t {
						found = true
					}
				}
				if !found {
					r.ProtectionReasons = append(r.ProtectionReasons, t)
				}
			case "image", "document", "search_result":
				r.ComplexContent = true
			}
		}
		for key, v := range x {
			if key == "input" || key == "input_schema" || key == "metadata" {
				continue
			}
			scanContent(v, r)
		}
	case []any:
		for _, v := range x {
			scanContent(v, r)
		}
	}
}

// Claude Code embeds project instructions in user messages. Exclude only the
// recognized wrapper from routing features; Raw is never changed. An unfinished
// reminder is also excluded, rather than treating its contents as a new task.
var reminderStart = regexp.MustCompile(`<system-reminder(?:\s+[^>]*)?>`)

func taskText(text string) (string, int) {
	var out strings.Builder
	removed := 0
	for {
		span := reminderStart.FindStringIndex(text)
		if span == nil {
			out.WriteString(text)
			break
		}
		out.WriteString(text[:span[0]])
		removed++
		text = text[span[1]:]
		end := strings.Index(text, "</system-reminder>")
		if end < 0 {
			break
		}
		out.WriteByte('\n')
		text = text[end+len("</system-reminder>"):]
	}
	return strings.TrimSpace(out.String()), removed
}

func tailChars(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[len(r)-max:])
}
func plainText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var out []string
		for _, v := range x {
			if b, ok := v.(map[string]any); ok && b["type"] == "text" {
				if s, ok := b["text"].(string); ok {
					out = append(out, s)
				}
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

// ReplaceModel preserves all original bytes except the top-level model string.
// Duplicate root fields are rejected to avoid inconsistent routing/parsing.
func ReplaceModel(raw []byte, model string) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, errors.New("body must be a JSON object")
	}
	seen := map[string]bool{}
	start, end := -1, -1
	for d.More() {
		t, err = d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, errors.New("invalid object key")
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate field %s", key)
		}
		seen[key] = true
		before := int(d.InputOffset())
		var v json.RawMessage
		if err = d.Decode(&v); err != nil {
			return nil, err
		}
		if key == "model" {
			var name string
			if json.Unmarshal(v, &name) != nil || name == "" {
				return nil, errors.New("model must be a nonempty string")
			}
			start = before
			for start < len(raw) && (raw[start] == ':' || raw[start] == ' ' || raw[start] == '\n' || raw[start] == '\r' || raw[start] == '\t') {
				start++
			}
			end = int(d.InputOffset())
		}
	}
	if _, err = d.Token(); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	if start < 0 {
		return nil, errors.New("model is required")
	}
	value, _ := json.Marshal(model)
	out := make([]byte, 0, len(raw)+len(value))
	out = append(out, raw[:start]...)
	out = append(out, value...)
	out = append(out, raw[end:]...)
	return out, nil
}
func (r Request) ToolFailureCount() int {
	if len(r.Messages) == 0 {
		return 0
	}
	var blocks []struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
	}
	json.Unmarshal(r.Messages[len(r.Messages)-1].Content, &blocks)
	n := 0
	for _, b := range blocks {
		if b.Type == "tool_result" && b.IsError {
			n++
		}
	}
	return n
}

// Fingerprinting is separate from forwarding: moving cache breakpoints should
// not hide reusable history. The estimate still needs provider usage evidence.
func fingerprintJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	var tree any
	json.Unmarshal(raw, &tree)
	normalizeCacheMarkers(tree)
	result, _ := json.Marshal(tree)
	return result
}
func normalizeCacheMarkers(v any) {
	switch x := v.(type) {
	case map[string]any:
		_, role := x["role"]
		_, schema := x["input_schema"]
		t, _ := x["type"].(string)
		switch t {
		case "text", "image", "document", "tool_use", "tool_result", "thinking":
			role = true
		}
		if role || schema {
			delete(x, "cache_control")
		}
		for k, child := range x {
			if k != "input" && k != "input_schema" {
				normalizeCacheMarkers(child)
			}
		}
	case []any:
		for _, child := range x {
			normalizeCacheMarkers(child)
		}
	}
}
