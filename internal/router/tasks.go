package router

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
)

const TaskSummaryChars = 80

func (s *Store) taskExists(session, prompt string) (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT count(*) FROM records WHERE session=? AND prompt=? AND kind='inference' AND COALESCE(json_extract(payload,'$.request_class'),'main') NOT IN ('auxiliary','compaction')", session, prompt).Scan(&n)
	return n > 0, err
}

type TaskLabel struct {
	Session   string    `json:"session_id"`
	Prompt    string    `json:"prompt_id"`
	Title     string    `json:"title"`
	Truncated bool      `json:"truncated"`
	Source    string    `json:"source"`
	Updated   time.Time `json:"updated_at"`
}

// Cut before whitespace folding, so at most the first 80 task characters are
// persisted. Rendering always uses textContent; no prompt HTML is interpreted.
func taskSummary(text string) (string, bool) {
	chars := []rune(strings.TrimSpace(text))
	truncated := len(chars) > TaskSummaryChars
	if truncated {
		chars = chars[:TaskSummaryChars]
	}
	for i, r := range chars {
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			chars[i] = ' '
		}
	}
	return strings.Join(strings.Fields(string(chars)), " "), truncated
}

// Automatic capture and exact-ID history imports only fill missing labels.
// An explicit manual rename may replace a label, never its task identity.
func (s *Store) SaveTaskLabel(label TaskLabel, replace bool) error {
	if label.Session == "" || label.Prompt == "" || len(label.Session) > 200 || len(label.Prompt) > 200 {
		return errors.New("session_id and prompt_id required")
	}
	if label.Source != "request" && label.Source != "transcript" && label.Source != "manual" {
		return errors.New("invalid title source")
	}
	var clipped bool
	label.Title, clipped = taskSummary(label.Title)
	label.Truncated = label.Truncated || clipped
	if label.Title == "" {
		return errors.New("task title is empty")
	}
	exists, err := s.taskExists(label.Session, label.Prompt)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("task not found")
	}
	label.Updated = time.Now().UTC()
	raw, err := json.Marshal(label)
	if err != nil {
		return err
	}
	query := "INSERT INTO task_labels VALUES(?,?,?) ON CONFLICT(session,prompt) DO NOTHING"
	if replace {
		query = "INSERT INTO task_labels VALUES(?,?,?) ON CONFLICT(session,prompt) DO UPDATE SET payload=excluded.payload"
	}
	_, err = s.db.Exec(query, label.Session, label.Prompt, string(raw))
	return err
}

type taskKey struct{ Session, Prompt string }
type TaskSummary struct {
	Session            string             `json:"session_id"`
	Prompt             string             `json:"prompt_id"`
	Label              *TaskLabel         `json:"label,omitempty"`
	Started            time.Time          `json:"started_at"`
	Updated            time.Time          `json:"updated_at"`
	ElapsedMS          int64              `json:"elapsed_ms"`
	Requests           int                `json:"requests"`
	MainRequests       int                `json:"main_requests"`
	BackgroundRequests int                `json:"background_requests"`
	ClassifierRequests int                `json:"classifier_requests"`
	High               int                `json:"high_requests"`
	Low                int                `json:"low_requests"`
	Tokens             int64              `json:"total_tokens"`
	MissingUsage       int                `json:"missing_usage_requests"`
	Costs              map[string]float64 `json:"costs"`
	EstimatedCosts     map[string]float64 `json:"retrospective_estimated_costs"`
	UnknownCosts       int                `json:"unknown_cost_requests"`
	State              string             `json:"state"`
	Feedback           *Feedback          `json:"feedback,omitempty"`
	latest             Record
	latestPriority     int
}
type TaskList struct {
	Tasks                []TaskSummary `json:"tasks"`
	Total                int           `json:"total_tasks"`
	UnattributedRequests int           `json:"unattributed_requests"`
}

func taskState(r Record) string {
	if r.StopReason == "max_tokens" || r.StopReason == "model_context_window_exceeded" {
		return "output_limited"
	}
	if r.CostStatus == "in_progress" {
		return "in_progress"
	}
	if !r.Complete {
		return "interrupted"
	}
	switch r.StopReason {
	case "end_turn", "stop_sequence":
		return "response_finished"
	case "tool_use", "pause_turn":
		return "awaiting_followup"
	default:
		return "transport_complete"
	}
}

// Aggregate from the entire ledger, not the last 500 requests shown in the
// technical timeline. Keys always contain both full IDs, never titles/prefixes.
func (s *Store) Tasks(c Config, limit int) (TaskList, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	result, err := s.allTasks(c)
	if len(result.Tasks) > limit {
		result.Tasks = result.Tasks[:limit]
	}
	return result, err
}

func (s *Store) allTasks(c Config) (TaskList, error) {
	result := TaskList{Tasks: []TaskSummary{}}
	rows, err := s.db.Query("SELECT payload FROM records ORDER BY started ASC, id ASC")
	if err != nil {
		return result, err
	}
	groups := map[taskKey]*TaskSummary{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var r Record
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			break
		}
		if r.Kind != "inference" && r.Kind != "classifier" {
			continue
		}
		if r.Prompt == "" || r.Session == "" {
			if r.Kind == "inference" {
				result.UnattributedRequests++
			}
			continue
		}
		key := taskKey{r.Session, r.Prompt}
		g := groups[key]
		if g == nil {
			g = &TaskSummary{Session: r.Session, Prompt: r.Prompt, Started: r.Started, Costs: map[string]float64{}, EstimatedCosts: map[string]float64{}, latestPriority: -1}
			groups[key] = g
		}
		if r.Started.Before(g.Started) {
			g.Started = r.Started
		}
		end := r.Started.Add(time.Duration(max(0, r.DurationMS)) * time.Millisecond)
		if end.After(g.Updated) {
			g.Updated = end
		}
		r.EstimateUnpriced(c)
		if money := r.recordedCost(); money != nil {
			g.Costs[money.Currency] += money.Amount
		} else {
			g.UnknownCosts++
			if r.Retrospective != nil {
				g.EstimatedCosts[r.Retrospective.Currency] += r.Retrospective.Amount
			}
		}
		if r.Kind == "classifier" {
			g.ClassifierRequests++
			continue
		}
		g.Requests++
		isMain := r.Class != "auxiliary" && r.Class != "compaction"
		if isMain {
			g.MainRequests++
		} else {
			g.BackgroundRequests++
		}
		priority := 0
		if isMain {
			priority = 1
			if r.Agent == "" && (r.Class == "main" || r.Class == "") {
				priority = 2
			}
		}
		if priority > g.latestPriority || (priority == g.latestPriority && !r.Started.Before(g.latest.Started)) {
			g.latest = r
			g.latestPriority = priority
		}
		switch r.Actual {
		case "high":
			g.High++
		case "low":
			g.Low++
		}
		if r.Complete && r.Usage.HasAllTokens() {
			g.Tokens += r.Usage.TotalInput() + num(r.Usage.Output)
		} else {
			g.MissingUsage++
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return result, err
	}
	labels, err := s.db.Query("SELECT payload FROM task_labels")
	if err != nil {
		return result, err
	}
	for labels.Next() {
		var raw string
		if err = labels.Scan(&raw); err != nil {
			break
		}
		var label TaskLabel
		if err = json.Unmarshal([]byte(raw), &label); err != nil {
			break
		}
		if g := groups[taskKey{label.Session, label.Prompt}]; g != nil {
			g.Label = &label
		}
	}
	if err == nil {
		err = labels.Err()
	}
	labels.Close()
	if err != nil {
		return result, err
	}
	feedback, err := s.Feedbacks()
	if err != nil {
		return result, err
	}
	for _, f := range feedback {
		if g := groups[taskKey{f.Session, f.Prompt}]; g != nil {
			copy := f
			g.Feedback = &copy
		}
	}
	for _, g := range groups {
		if g.Requests == 0 {
			continue
		}
		if g.MainRequests == 0 {
			result.UnattributedRequests += g.Requests
			continue
		}
		g.ElapsedMS = max(0, g.Updated.Sub(g.Started).Milliseconds())
		g.State = taskState(g.latest)
		result.Tasks = append(result.Tasks, *g)
	}
	sort.Slice(result.Tasks, func(i, j int) bool {
		a, b := result.Tasks[i], result.Tasks[j]
		if a.Started.Equal(b.Started) {
			if a.Session == b.Session {
				return a.Prompt < b.Prompt
			}
			return a.Session < b.Session
		}
		return a.Started.After(b.Started)
	})
	result.Total = len(result.Tasks)
	return result, nil
}

func (s *Store) TaskRecords(session, prompt string, limit int) ([]Record, error) {
	if session == "" || prompt == "" {
		return nil, errors.New("session_id and prompt_id required")
	}
	if limit < 1 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.Query("SELECT payload FROM records WHERE session=? AND prompt=? ORDER BY started ASC, id ASC LIMIT ?", session, prompt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var r Record
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
