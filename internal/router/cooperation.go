package router

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// These aliases and hook events are explicit local client instructions. Prompt
// text, tool output and x-claude-code-agent-type=custom never assign a role.
const CoordinatorModel = "router-coordinator"
const ExecutorModel = "router-executor"
const ReviewerModel = "router-reviewer"

type CooperationTrace struct {
	Role               string `json:"role"`
	Stage              string `json:"stage"`
	Attribution        string `json:"attribution"`
	Followup           string `json:"followup,omitempty"`
	DelegationObserved *bool  `json:"delegation_observed,omitempty"`
}
type CooperationRun struct {
	Session          string    `json:"session_id"`
	Prompt           string    `json:"prompt_id"`
	Agent            string    `json:"agent_id"`
	Role             string    `json:"role"`
	Status           string    `json:"status"`
	Started          time.Time `json:"started_at"`
	Updated          time.Time `json:"updated_at"`
	Attempts         int       `json:"attempts"`
	DeliveryReady    bool      `json:"delivery_ready,omitempty"`
	WholeTaskChecked bool      `json:"whole_task_checked,omitempty"`
	ToolUseID        string    `json:"tool_use_id,omitempty"`
	OnSuccess        string    `json:"on_success,omitempty"`
	NeedsDesign      int       `json:"needs_design_reports"`
	ChangesRequired  int       `json:"changes_required_reports"`
}

func cooperationRole(model string) string {
	switch model {
	case CoordinatorModel:
		return "coordinator"
	case ExecutorModel:
		return "executor"
	case ReviewerModel:
		return "reviewer"
	default:
		return ""
	}
}
func cooperationStage(role string) string {
	switch role {
	case "executor":
		return "execution"
	case "reviewer":
		return "review"
	default:
		return "coordination"
	}
}

// Hook bodies are consumed transiently. Only IDs, enum outcomes, counters and
// timestamps are stored; never transcript paths, assistant text or evidence.
func (g *Gateway) cooperationEvent(w http.ResponseWriter, r *http.Request) {
	if g.Config.Mode != "auto" {
		errorJSON(w, 409, "cooperation requires mode auto")
		return
	}
	var event struct {
		Session string `json:"session_id"`
		Prompt  string `json:"prompt_id"`
		Agent   string `json:"agent_id"`
		Type    string `json:"agent_type"`
		Event   string `json:"hook_event_name"`
		Message string `json:"last_assistant_message"`
		agentReturnEvent
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	if d.Decode(&event) != nil || d.Decode(new(any)) != io.EOF {
		errorJSON(w, 400, "invalid cooperation hook")
		return
	}
	if event.Event == "PostToolUse" {
		if err := g.Store.bindAgentReturn(event.Session, event.Prompt, event.Agent, event.agentReturnEvent); err != nil {
			errorJSON(w, 503, "could not record agent return")
			return
		}
		jsonResponse(w, map[string]any{})
		return
	}
	role := cooperationRole(event.Type)
	if role != "executor" && role != "reviewer" {
		errorJSON(w, 400, "unknown cooperation agent")
		return
	}
	if event.Session == "" || event.Prompt == "" || event.Agent == "" || len(event.Session) > 200 || len(event.Prompt) > 200 || len(event.Agent) > 200 {
		errorJSON(w, 400, "cooperation hooks require session_id, prompt_id and agent_id")
		return
	}
	if event.Event != "SubagentStart" && event.Event != "SubagentStop" {
		errorJSON(w, 400, "unsupported cooperation event")
		return
	}
	status := "running"
	if event.Event == "SubagentStop" {
		status = cooperationOutcome(role, event.Message)
	}
	ready, wholeTask := reportReadiness(status, event.Message)
	if err := g.Store.saveCooperationEvent(CooperationRun{Session: event.Session, Prompt: event.Prompt, Agent: event.Agent, Role: role, Status: status, DeliveryReady: ready, WholeTaskChecked: wholeTask}, event.Event); err != nil {
		errorJSON(w, 409, "could not record cooperation event or agent identity changed")
		return
	}
	jsonResponse(w, map[string]any{})
}

func reportJSON(message string) string {
	message = strings.TrimSpace(message)
	if strings.HasPrefix(message, "```json\n") && strings.HasSuffix(message, "\n```") {
		message = strings.TrimSuffix(strings.TrimPrefix(message, "```json\n"), "\n```")
	}
	return message
}

func cooperationOutcome(role, message string) string {
	message = reportJSON(message)
	// Ignore all explanation fields, but do not scan prose for a success keyword.
	var result struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(message), &result) != nil {
		return "unstructured"
	}
	decoder := json.NewDecoder(strings.NewReader(message))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return "unstructured"
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "unstructured"
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return "unstructured"
		}
		seen[key] = true
		var ignored json.RawMessage
		if decoder.Decode(&ignored) != nil {
			return "unstructured"
		}
	}
	if role == "executor" && (result.Status == "completed" || result.Status == "needs_design" || result.Status == "blocked" || result.Status == "needs_parent_write") {
		return result.Status
	}
	if role == "reviewer" && (result.Status == "passed" || result.Status == "changes_required" || result.Status == "blocked") {
		return result.Status
	}
	return "unstructured"
}

func (s *Store) saveCooperationEvent(next CooperationRun, event string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRow("SELECT payload FROM cooperation_runs WHERE session=? AND prompt=? AND agent=?", next.Session, next.Prompt, next.Agent).Scan(&raw)
	now := time.Now().UTC()
	run := next
	run.Started, run.Updated, run.Attempts = now, now, 1
	if err == nil {
		if json.Unmarshal([]byte(raw), &run) != nil || run.Role != next.Role {
			return errors.New("agent role changed")
		}
		// Duplicate starts/stops are idempotent. A new start after a stop is an
		// observed resume, not a claim that a new worker was a rework attempt.
		if event == "SubagentStart" {
			if run.Status == "running" {
				return nil
			}
			run.Attempts++
			run.ToolUseID, run.OnSuccess = "", ""
		} else if run.Status != "running" {
			return nil
		}
		run.Status, run.Updated = next.Status, now
		run.DeliveryReady, run.WholeTaskChecked = next.DeliveryReady, next.WholeTaskChecked
	} else if err != sql.ErrNoRows {
		return err
	}
	if event == "SubagentStop" {
		if run.Status == "needs_design" {
			run.NeedsDesign++
		}
		if run.Status == "changes_required" {
			run.ChangesRequired++
		}
	}
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO cooperation_runs VALUES(?,?,?,?) ON CONFLICT(session,prompt,agent) DO UPDATE SET payload=excluded.payload", run.Session, run.Prompt, run.Agent, string(data))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) cooperationRun(session, prompt, agent string) (*CooperationRun, error) {
	query := "SELECT payload FROM cooperation_runs WHERE session=? AND agent=?"
	args := []any{session, agent}
	if prompt != "" {
		query += " AND prompt=?"
		args = append(args, prompt)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found *CooperationRun
	for rows.Next() {
		if found != nil {
			return nil, errors.New("ambiguous cooperation parent prompt")
		}
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var run CooperationRun
		if err = json.Unmarshal([]byte(raw), &run); err != nil {
			return nil, err
		}
		found = &run
	}
	return found, rows.Err()
}
func (s *Store) cooperationRuns() ([]CooperationRun, error) {
	rows, err := s.db.Query("SELECT payload FROM cooperation_runs ORDER BY session,prompt,agent")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []CooperationRun{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var run CooperationRun
		if err = json.Unmarshal([]byte(raw), &run); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (g *Gateway) identifyCooperation(req *Request, rec *Record) error {
	if rec.Class == "auxiliary" {
		return nil
	}
	role := cooperationRole(req.Model)
	var bound *CooperationRun
	var err error
	if rec.Agent != "" {
		bound, err = g.Store.cooperationRun(rec.Session, rec.Prompt, rec.Agent)
		if err != nil {
			return err
		}
		if bound == nil && rec.Prompt != "" {
			other, e := g.Store.cooperationRun(rec.Session, "", rec.Agent)
			if e != nil || other != nil {
				return errors.New("cooperation agent is not bound to this prompt; a new SubagentStart is required")
			}
		}
	}
	attribution := "gateway_headers"
	if bound != nil {
		role = bound.Role // hook identity also survives Claude's per-call model override
		if rec.Prompt == "" {
			rec.Prompt = bound.Prompt
			attribution = "subagent_hook"
		}
	}
	if role == "" {
		return nil
	}
	if g.Config.Mode != "auto" {
		return errors.New("cooperation aliases require mode auto")
	}
	if role == "coordinator" && (rec.Agent != "" || rec.Class != "main") {
		// An auxiliary request may inherit the main model alias. It is not a
		// planning request; use ordinary routing and keep its original class.
		return nil
	}
	if strings.HasPrefix(rec.Session, "unattributed-") || rec.Prompt == "" {
		return errors.New("cooperation requires Claude session and prompt hint headers")
	}
	if role != "coordinator" && (rec.Agent == "" || (rec.Class != "subagent" && rec.Class != "compaction")) {
		return errors.New("cooperation worker requires a subagent identity")
	}
	req.CooperationRole = role
	rec.Cooperation = &CooperationTrace{Role: role, Stage: cooperationStage(role), Attribution: attribution}
	return nil
}

func (g *Gateway) cooperationDecision(req Request, rec Record, b *branch) Decision {
	target := "high"
	if rec.Cooperation.Role == "executor" {
		target = "low"
	}
	d := Decide(g.Config, req, b, target, "cooperation_"+rec.Cooperation.Role, 1)
	// Explicit roles stay on their own provider. Preserve all cache estimates
	// while bypassing task-keyword and price-based reassignment of a worker.
	d.Recommended, d.Proposed, d.Selected = target, target, target
	d.Reasons = []string{"cooperation_" + rec.Cooperation.Role, "cooperation_dedicated_branch"}
	d.Classifier, d.ClassifierSkipReason = "cooperation", "cooperation_role"
	d.Error = ""
	output := d.ContextBudget.ReservedOutput
	switch {
	case output >= d.ContextBudget.limit(target):
		d.Error = "output_budget_exceeds_context"
	case req.Tokens+output > d.ContextBudget.limit(target):
		d.Error = "selected_model_context_limit"
	case target == "low" && req.ComplexContent:
		d.Error = "selected_model_content_unsupported"
	case req.Protected && b.Current != "" && b.Current != target:
		d.Error = "cooperation_history_conflict"
	case req.Protected && b.Current == "":
		d.Error = "unknown_history_origin"
	}
	if rec.Cooperation.Role == "executor" && d.Error == "" {
		var count int
		err := g.Store.db.QueryRow("SELECT count(*) FROM records WHERE session=? AND prompt=? AND json_extract(payload,'$.agent_id')=? AND json_extract(payload,'$.actual_tier')!='' AND kind='inference'", rec.Session, rec.Prompt, rec.Agent).Scan(&count)
		if err != nil {
			d.Error = "cooperation_ledger_unavailable"
		} else if count >= g.Config.Cooperation.ExecutorLimit() {
			d.Error = "cooperation_executor_budget"
		}
	}
	return d
}
