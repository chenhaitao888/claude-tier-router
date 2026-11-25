package router

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

const (
	formatResults = "format_results"
	writeApproved = "write_approved"
)

// Claude supplies the tool and agent IDs in PostToolUse. The continuation plan
// comes from the parent's Agent input. A worker cannot select a tier by putting
// a marker in its returned prose.
type agentReturnEvent struct {
	Tool  string `json:"tool_name"`
	Call  string `json:"tool_use_id"`
	Input struct {
		Prompt     string `json:"prompt"`
		Type       string `json:"subagent_type"`
		Background bool   `json:"run_in_background"`
	} `json:"tool_input"`
	Response json.RawMessage `json:"tool_response"`
}

func (s *Store) bindAgentReturn(session, prompt, parentAgent string, e agentReturnEvent) error {
	if parentAgent != "" || session == "" || prompt == "" || e.Call == "" || len(e.Call) > 200 ||
		(e.Tool != "Agent" && e.Tool != "Task") || e.Input.Background {
		return nil
	}
	role := cooperationRole(e.Input.Type)
	if role != "executor" && role != "reviewer" {
		return nil
	}
	var returned struct {
		Agent  string `json:"agentId"`
		Status string `json:"status"`
	}
	if json.Unmarshal(e.Response, &returned) != nil || returned.Agent == "" || returned.Status != "completed" {
		return nil
	}
	line, _, _ := strings.Cut(strings.TrimSpace(e.Input.Prompt), "\n")
	plan := "judgment"
	switch strings.TrimSpace(line) {
	case "Router-Next: format_results":
		plan = formatResults
	case "Router-Next: write_approved":
		plan = writeApproved
	}
	// Only bind a fresh run, once. Unknown/old clients and resumed workers
	// remain eligible for ordinary high-tier coordination.
	_, err := s.db.Exec(`UPDATE cooperation_runs
        SET payload=json_set(payload,'$.tool_use_id',?,'$.on_success',?)
        WHERE session=? AND prompt=? AND agent=? AND json_extract(payload,'$.role')=?
          AND json_extract(payload,'$.attempts')=1 AND json_extract(payload,'$.tool_use_id') IS NULL`,
		e.Call, plan, session, prompt, returned.Agent, role)
	return err
}

// This bit records the worker's structured report, not independent acceptance.
// Missing fields from older agents cannot be interpreted as empty findings.
func reportReadiness(status, message string) (ready, wholeTask bool) {
	if status != "completed" && status != "needs_parent_write" && status != "passed" {
		return false, false
	}
	var report struct {
		WholeTask   bool                             `json:"whole_task_checked"`
		Questions   []string                         `json:"open_questions"`
		Checks      []string                         `json:"checks"`
		Evidence    []string                         `json:"evidence"`
		Conflicts   []string                         `json:"conflicts"`
		Findings    []string                         `json:"findings"`
		Limitations []string                         `json:"limitations"`
		Drafts      []struct{ Path, Content string } `json:"drafts"`
	}
	hasText := func(s string) bool { return strings.TrimSpace(s) != "" }
	if json.Unmarshal([]byte(reportJSON(message)), &report) != nil || !slices.ContainsFunc(report.Checks, hasText) {
		return false, false
	}
	if len(report.Questions)+len(report.Findings)+len(report.Limitations)+len(report.Conflicts) > 0 {
		return false, false
	}
	if status == "passed" {
		clear := report.Findings != nil && report.Limitations != nil
		return clear, clear && report.WholeTask
	}
	if report.Questions == nil {
		return false, false
	}
	if status == "needs_parent_write" {
		return len(report.Drafts) == 1 && strings.TrimSpace(report.Drafts[0].Path) != "" && strings.TrimSpace(report.Drafts[0].Content) != "", false
	}
	return slices.ContainsFunc(report.Evidence, hasText), false
}

func (s *Store) coordinatorFollowup(req *Request, rec *Record) error {
	// One snapshot includes hooks AND workers seen only in the request ledger.
	// A missing Stop hook or an unfinished sibling cannot enable cheap delivery.
	rows, err := s.db.Query(`WITH workers(agent) AS (
        SELECT agent FROM cooperation_runs WHERE session=?1 AND prompt=?2
        UNION SELECT json_extract(payload,'$.agent_id') FROM records
         WHERE session=?1 AND prompt=?2 AND json_extract(payload,'$.cooperation.role') IN ('executor','reviewer'))
        SELECT r.payload FROM workers LEFT JOIN cooperation_runs r
          ON r.session=?1 AND r.prompt=?2 AND r.agent=workers.agent ORDER BY workers.agent`, rec.Session, rec.Prompt)
	if err != nil {
		return err
	}
	defer rows.Close()
	byCall := make(map[string]CooperationRun)
	var hold, issue string
	var lastIssue time.Time
	for rows.Next() {
		req.CooperationDelegated = true
		var raw sql.NullString
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		if !raw.Valid {
			hold = "missing_report"
			continue
		}
		var run CooperationRun
		if err = json.Unmarshal([]byte(raw.String), &run); err != nil {
			return err
		}
		var problem string
		switch {
		case run.Status == "running":
			hold = "running"
		case !run.DeliveryReady:
			problem = run.Status
			if problem == "completed" || problem == "needs_parent_write" || problem == "passed" {
				problem = "unresolved_report"
			}
		case run.ToolUseID == "":
			hold = "unconfirmed_step"
		}
		if problem != "" && run.Updated.After(lastIssue) {
			lastIssue, issue = run.Updated, problem
		}

		if run.ToolUseID != "" {
			if _, duplicate := byCall[run.ToolUseID]; duplicate {
				hold = "unconfirmed_step"
			}
			byCall[run.ToolUseID] = run
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rec.Cooperation.DelegationObserved = &req.CooperationDelegated
	if !req.CooperationDelegated {
		return nil
	}
	switch {
	case req.ToolFailureCount() > 0:
		hold = "tool_error"
	case req.ComplexContent:
		hold = "content_capability"
	case hold == "":
		var reviewedAt time.Time
		hold, reviewedAt = req.nextDelivery(byCall)
		if issue != "" && !reviewedAt.After(lastIssue) {
			hold = issue
		}
	}
	req.CooperationFollowup = hold
	rec.Cooperation.Followup = hold
	return nil
}

type deliveryBlock struct {
	Type     string
	Name     string
	ID       string
	ResultID string `json:"tool_use_id"`
	Error    bool   `json:"is_error"`
	Text     string
}

// Only a contiguous, successful tool round qualifies. New user instructions,
// unrelated tools, partial replies and compacted history require a fresh decision.
func (r Request) returnedTools(end int) (map[string]string, bool) {
	if end < 1 || r.Messages[end].Role != "user" || r.Messages[end-1].Role != "assistant" {
		return nil, false
	}
	var calls, results []deliveryBlock
	if json.Unmarshal(r.Messages[end-1].Content, &calls) != nil || json.Unmarshal(r.Messages[end].Content, &results) != nil {
		return nil, false
	}
	tools := map[string]string{}
	for _, call := range calls {
		if call.Type == "tool_use" {
			if call.ID == "" || call.Name == "" || tools[call.ID] != "" {
				return nil, false
			}
			tools[call.ID] = call.Name
		}
	}
	received := map[string]bool{}
	for _, result := range results {
		if result.Type == "text" {
			if t, _ := taskText(result.Text); t == "" {
				continue
			}
		}
		if result.Type != "tool_result" || result.Error || tools[result.ResultID] == "" || received[result.ResultID] {
			return nil, false
		}
		received[result.ResultID] = true
	}
	return tools, len(tools) > 0 && len(received) == len(tools)
}

func (r Request) nextDelivery(runs map[string]CooperationRun) (string, time.Time) {
	var reviewedAt time.Time
	end := len(r.Messages) - 1
	tools, ok := r.returnedTools(end)
	if !ok {
		return "unconfirmed_step", time.Time{}
	}
	// After a single successful Write for an approved draft, only reporting
	// that result remains. Do not carry the cheap recommendation to other tools.
	wrote := false
	if len(tools) == 1 {
		for _, name := range tools {
			wrote = name == "Write"
		}
	}
	if wrote {
		tools, ok = r.returnedTools(end - 2)
		if !ok {
			return "unconfirmed_step", time.Time{}
		}
	}
	plan := ""
	for id, name := range tools {
		run, found := runs[id]
		if !found || (name != "Agent" && name != "Task") {
			return "unconfirmed_step", time.Time{}
		}
		switch run.OnSuccess {
		case formatResults:
			if wrote || (run.Status != "completed" && run.Status != "passed") {
				return "unexpected_result", time.Time{}
			}
		case writeApproved:
			if len(tools) != 1 || (run.Status != "needs_parent_write" && run.Status != "passed") {
				return "unexpected_result", time.Time{}
			}
		default:
			return "judgment", time.Time{}
		}

		if run.Status == "passed" && run.WholeTaskChecked && run.Started.After(reviewedAt) {
			reviewedAt = run.Started
		}
		plan = run.OnSuccess
	}
	if wrote {
		return formatResults, reviewedAt
	}
	return plan, reviewedAt
}
