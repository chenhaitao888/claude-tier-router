package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"time"
)

type Record struct {
	ID          string    `json:"id"`
	ParentID    string    `json:"parent_id,omitempty"`
	Kind        string    `json:"kind"`
	Session     string    `json:"session_id"`
	Prompt      string    `json:"prompt_id"`
	Agent       string    `json:"agent_id"`
	Class       string    `json:"request_class"`
	Started     time.Time `json:"started_at"`
	Mode        string    `json:"mode"`
	Policy      string    `json:"policy_version"`
	Decision    Decision  `json:"decision"`
	Actual      string    `json:"actual_tier"`
	Model       string    `json:"model"`
	Prices      *Prices   `json:"price_snapshot"`
	HTTPStatus  int       `json:"http_status"`
	Complete    bool      `json:"complete"`
	Error       string    `json:"error,omitempty"`
	DurationMS  int64     `json:"duration_ms"`
	FirstByteMS int64     `json:"first_byte_ms"`
	Usage       Usage     `json:"usage"`
	CostUSD     *float64  `json:"cost_usd"`
	CostStatus  string    `json:"cost_status"`
}
type Feedback struct {
	Session string    `json:"session_id"`
	Prompt  string    `json:"prompt_id"`
	Outcome string    `json:"outcome"`
	Note    string    `json:"note"`
	Source  string    `json:"source"`
	Updated time.Time `json:"updated_at"`
}
type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS records(id TEXT PRIMARY KEY, started TEXT NOT NULL, session TEXT NOT NULL, prompt TEXT NOT NULL, kind TEXT NOT NULL, payload TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS records_started ON records(started);
 CREATE TABLE IF NOT EXISTS feedback(session TEXT NOT NULL, prompt TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(session,prompt));
 PRAGMA user_version=1;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Save(r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx, "INSERT INTO records VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload", r.ID, r.Started.UTC().Format(time.RFC3339Nano), r.Session, r.Prompt, r.Kind, string(b))
	return err
}
func (s *Store) Records(limit int) ([]Record, error) {
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query("SELECT payload FROM records ORDER BY started DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Record{}
	for rows.Next() {
		var b string
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		var r Record
		if err = json.Unmarshal([]byte(b), &r); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *Store) Feedback(f Feedback) error {
	if f.Session == "" || f.Prompt == "" || len(f.Note) > 2000 {
		return errors.New("session_id, prompt_id required; note max 2000 bytes")
	}
	if f.Outcome != "passed" && f.Outcome != "failed" && f.Outcome != "unknown" {
		return errors.New("outcome must be passed, failed or unknown")
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM records WHERE session=? AND prompt=? AND kind='inference'", f.Session, f.Prompt).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("task not found")
	}
	f.Source = "manual"
	f.Updated = time.Now().UTC()
	b, _ := json.Marshal(f)
	_, err := s.db.Exec("INSERT INTO feedback VALUES(?,?,?) ON CONFLICT(session,prompt) DO UPDATE SET payload=excluded.payload", f.Session, f.Prompt, string(b))
	return err
}
func (s *Store) Feedbacks() ([]Feedback, error) {
	rows, err := s.db.Query("SELECT payload FROM feedback")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Feedback{}
	for rows.Next() {
		var b string
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		var f Feedback
		if err = json.Unmarshal([]byte(b), &f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

type Summary struct {
	Requests           int     `json:"inference_requests"`
	ClassifierRequests int     `json:"classifier_requests"`
	KnownCostUSD       float64 `json:"known_cost_usd"`
	ClassifierCostUSD  float64 `json:"classifier_cost_usd"`
	UnknownCost        int     `json:"unknown_cost_requests"`
	Complete           int     `json:"complete_requests"`
	High               int     `json:"high_requests"`
	Low                int     `json:"low_requests"`
	CacheRead          int64   `json:"cache_read_tokens"`
	InputTotal         int64   `json:"input_tokens_total"`
	Tasks              int     `json:"tasks"`
	Passed             int     `json:"passed_tasks"`
	Failed             int     `json:"failed_tasks"`
	Unevaluated        int     `json:"unevaluated_tasks"`
}

func (s *Store) Summary() (Summary, error) {
	var out Summary
	rows, err := s.db.Query("SELECT payload FROM records")
	if err != nil {
		return out, err
	}
	tasks := map[string]bool{}
	for rows.Next() {
		var b string
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return out, err
		}
		var r Record
		if err = json.Unmarshal([]byte(b), &r); err != nil {
			rows.Close()
			return out, err
		}
		if r.CostUSD == nil {
			out.UnknownCost++
		} else {
			out.KnownCostUSD += *r.CostUSD
		}
		if r.Kind == "classifier" {
			out.ClassifierRequests++
			if r.CostUSD != nil {
				out.ClassifierCostUSD += *r.CostUSD
			}
			continue
		}
		out.Requests++
		if r.Complete {
			out.Complete++
		}
		if r.Actual == "high" {
			out.High++
		}
		if r.Actual == "low" {
			out.Low++
		}
		out.CacheRead += num(r.Usage.Read)
		out.InputTotal += r.Usage.TotalInput()
		if r.Prompt != "" {
			tasks[r.Session+"\x00"+r.Prompt] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	fs, err := s.Feedbacks()
	if err != nil {
		return out, err
	}
	for _, f := range fs {
		if tasks[f.Session+"\x00"+f.Prompt] {
			if f.Outcome == "passed" {
				out.Passed++
			}
			if f.Outcome == "failed" {
				out.Failed++
			}
		}
	}
	out.Tasks = len(tasks)
	out.Unevaluated = out.Tasks - out.Passed - out.Failed
	return out, nil
}
