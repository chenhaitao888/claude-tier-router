package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

type Pagination struct {
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

func parsePagination(q url.Values) (Pagination, error) {
	p := Pagination{Page: 1, PageSize: 20}
	for name, target := range map[string]*int{"page": &p.Page, "page_size": &p.PageSize} {
		if values, ok := q[name]; ok {
			if len(values) != 1 {
				return p, errors.New("duplicate pagination parameter")
			}
			value, err := strconv.Atoi(values[0])
			if err != nil || value < 1 {
				return p, errors.New("page and page_size must be positive integers")
			}
			*target = value
		}
	}
	if p.PageSize != 10 && p.PageSize != 20 && p.PageSize != 50 {
		return p, errors.New("page_size must be 10, 20 or 50")
	}
	return p, nil
}

func (p *Pagination) setTotal(total int) {
	p.Total = total
	p.TotalPages = max(1, (total+p.PageSize-1)/p.PageSize)
	p.Page = min(p.Page, p.TotalPages)
}

type TaskPage struct {
	TaskList
	Pagination
}

func (s *Store) TasksPage(c Config, p Pagination) (TaskPage, error) {
	list, err := s.allTasks(c)
	if err != nil {
		return TaskPage{}, err
	}
	p.setTotal(list.Total)
	start := (p.Page - 1) * p.PageSize
	list.Tasks = list.Tasks[start:min(start+p.PageSize, list.Total)]
	return TaskPage{list, p}, nil
}

type RecordView struct {
	Record
	RootPrompt string     `json:"root_prompt_id,omitempty"`
	TaskLabel  *TaskLabel `json:"task_label,omitempty"`
}
type RecordPage struct {
	Records []RecordView `json:"records"`
	Pagination
}

type FeedbackView struct {
	Feedback
	TaskLabel *TaskLabel `json:"task_label,omitempty"`
}

// Feedback history keeps readable names even when its task is on another page.
func (s *Store) FeedbackViews() ([]FeedbackView, error) {
	rows, err := s.db.Query(`WITH entries AS (
		SELECT f.*, COALESCE(a.root,f.prompt) AS root FROM feedback f
		LEFT JOIN task_continuations a ON a.session=f.session AND a.prompt=f.prompt
	), ranked AS (
		SELECT *, ROW_NUMBER() OVER (PARTITION BY session,root ORDER BY
			julianday(json_extract(payload,'$.updated_at')) DESC, (prompt=root) DESC,prompt) AS n FROM entries
	) SELECT f.payload,f.root,l.payload FROM ranked f LEFT JOIN task_labels l ON f.session=l.session AND f.root=l.prompt WHERE f.n=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []FeedbackView{}
	for rows.Next() {
		var raw string
		var label sql.NullString
		var view FeedbackView
		var root string
		if err = rows.Scan(&raw, &root, &label); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &view.Feedback); err != nil {
			return nil, err
		}
		view.Prompt = root
		if label.Valid {
			if err = json.Unmarshal([]byte(label.String), &view.TaskLabel); err != nil {
				return nil, err
			}
		}
		result = append(result, view)
	}
	return result, rows.Err()
}

func (s *Store) RecordsPage(ctx context.Context, c Config, p Pagination, session, prompt string) (RecordPage, error) {
	result := RecordPage{Records: []RecordView{}}
	// Count and rows must describe the same snapshot while requests are arriving.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	from := " FROM task_record_roots r"
	where, order := "", "r.started DESC, r.id DESC"
	args := []any{}
	if session != "" {
		where, order = " WHERE r.session=?1 AND r.root_prompt="+resolvedPromptSQL, "r.started ASC, r.id ASC"
		args = append(args, session, prompt)
	}
	var total int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*)"+from+where, args...).Scan(&total); err != nil {
		return result, err
	}
	p.setTotal(total)
	result.Pagination = p
	args = append(args, p.PageSize, (p.Page-1)*p.PageSize)
	rows, err := tx.QueryContext(ctx, "SELECT r.payload, r.root_prompt, l.payload"+from+" LEFT JOIN task_labels l ON r.session=l.session AND r.root_prompt=l.prompt"+where+" ORDER BY "+order+" LIMIT ? OFFSET ?", args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var label sql.NullString
		var view RecordView
		if err = rows.Scan(&raw, &view.RootPrompt, &label); err != nil {
			return result, err
		}
		if err = json.Unmarshal([]byte(raw), &view.Record); err != nil {
			return result, err
		}
		if label.Valid {
			if err = json.Unmarshal([]byte(label.String), &view.TaskLabel); err != nil {
				return result, err
			}
		}
		view.EstimateUnpriced(c)
		result.Records = append(result.Records, view)
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	if err = rows.Close(); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (g *Gateway) listPage(w http.ResponseWriter, r *http.Request) {
	p, err := parsePagination(r.URL.Query())
	if err != nil {
		errorJSON(w, 400, err.Error())
		return
	}
	if r.URL.Path == "/api/tasks" {
		result, err := g.Store.TasksPage(g.Config, p)
		if err != nil {
			errorJSON(w, 500, "could not read tasks")
			return
		}
		jsonResponse(w, result)
		return
	}
	var session, prompt string
	if r.URL.Path == "/api/task-records" {
		session, prompt = r.URL.Query().Get("session_id"), r.URL.Query().Get("prompt_id")
		if session == "" || prompt == "" {
			errorJSON(w, 400, "session_id and prompt_id required")
			return
		}
	}
	result, err := g.Store.RecordsPage(r.Context(), g.Config, p, session, prompt)
	if err != nil {
		errorJSON(w, 500, "could not read task requests")
		return
	}
	jsonResponse(w, result)
}
