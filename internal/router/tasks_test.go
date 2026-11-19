package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestQuestionSummariesAreOptInBoundedAndStable(t *testing.T) {
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, goodStream)
	})
	body := strings.Replace(sample, "explain this function", "explain a private question", 1)
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, request(body))
	tasks, _ := s.Tasks(g.Config, 100)
	if tasks.Total != 1 || tasks.Tasks[0].Label != nil {
		t.Fatal("recorded title without opt-in")
	}
	g.Config.StoreTaskSummaries = true
	text := `<system-reminder id="test">private project rules</system-reminder>` + strings.Repeat("问", 85)
	encoded, _ := json.Marshal(text)
	body = strings.Replace(sample, `"explain this function"`, string(encoded), 1)
	rr = httptest.NewRecorder()
	g.ServeHTTP(rr, request(body))
	tasks, _ = s.Tasks(g.Config, 100)
	label := tasks.Tasks[0].Label
	if label == nil || !label.Truncated || utf8.RuneCountInString(label.Title) != 80 || strings.Contains(label.Title, "private") {
		t.Fatal(label)
	}
	rr = httptest.NewRecorder()
	g.ServeHTTP(rr, request(sample))
	tasks, _ = s.Tasks(g.Config, 100)
	if tasks.Tasks[0].Label.Title != label.Title {
		t.Fatal("tool continuation replaced question title")
	}
	if err := s.SaveTaskLabel(TaskLabel{Session: "session", Prompt: "task", Title: "手动名称", Source: "manual"}, true); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	g.ServeHTTP(rr, request(body))
	tasks, _ = s.Tasks(g.Config, 100)
	if tasks.Tasks[0].Label.Title != "手动名称" || tasks.Tasks[0].Label.Source != "manual" {
		t.Fatal("automatic summary overwrote manual name")
	}
}

func TestTasksAggregateFullLedgerAndIsolateFeedback(t *testing.T) {
	g, s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("aggregation must not call models") })
	base := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	usd, cny := 0.01, 0.03
	for i := 0; i < 510; i++ {
		r := Record{ID: fmt.Sprint("a", i), Kind: "inference", Session: "session-a", Prompt: "same-prompt", Class: "main", Started: base.Add(time.Duration(i) * time.Second), DurationMS: 100, Actual: "high", Complete: true, StopReason: "tool_use", Usage: countedUsage(10, 10, 0, 2), CostAmount: &usd, CostCurrency: "USD", CostStatus: "usage_calculated"}
		if i == 509 {
			r.StopReason = "end_turn"
		}
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []Record{
		{ID: "other-session", Kind: "inference", Session: "session-b", Prompt: "same-prompt", Class: "main", Started: base, Actual: "low", Complete: true, StopReason: "end_turn", Usage: countedUsage(5, 0, 0, 5), CostAmount: &cny, CostCurrency: "CNY"},
		{ID: "classifier", Kind: "classifier", Session: "session-a", Prompt: "same-prompt", Started: base, Actual: "low", CostAmount: &cny, CostCurrency: "CNY"},
		{ID: "child", Kind: "inference", Session: "session-a", Prompt: "same-prompt", Agent: "child", Class: "subagent", Started: base.Add(time.Hour), Complete: true, StopReason: "tool_use"},
		{ID: "aux", Kind: "inference", Session: "session-a", Class: "auxiliary", Started: base, CostAmount: &usd, CostCurrency: "USD"},
		{ID: "aux-only", Kind: "inference", Session: "session-c", Prompt: "internal-prompt", Class: "auxiliary", Started: base},
	} {
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, sid := range []string{"session-a", "session-b"} {
		if err := s.SaveTaskLabel(TaskLabel{Session: sid, Prompt: "same-prompt", Title: "同名问题", Source: "transcript"}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Feedback(Feedback{Session: "session-b", Prompt: "same-prompt", Outcome: "passed"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.Tasks(g.Config, 100)
	if err != nil || list.Total != 2 || list.UnattributedRequests != 2 {
		t.Fatal(list, err)
	}
	for _, task := range list.Tasks {
		if task.Session == "session-a" {
			if task.Requests != 511 || task.ClassifierRequests != 1 || task.High != 510 || task.Tokens != 510*22 || task.State != "response_finished" || task.Feedback != nil || task.Costs["USD"] < 5.09 || task.Costs["CNY"] != cny {
				t.Fatalf("bad full aggregation %+v", task)
			}
		} else if task.Feedback == nil || task.Feedback.Outcome != "passed" {
			t.Fatal("feedback assigned to wrong session")
		}
	}
	records, err := s.TaskRecords("session-b", "same-prompt", 1000)
	if err != nil || len(records) != 1 || records[0].ID != "other-session" {
		t.Fatal(records, err)
	}
	limited, _ := s.Tasks(g.Config, 1)
	if limited.Total != 2 || len(limited.Tasks) != 1 {
		t.Fatal("limit applied before aggregation")
	}
	if s.Feedback(Feedback{Session: "session-c", Prompt: "internal-prompt", Outcome: "passed"}) == nil {
		t.Fatal("background-only request accepted as a user task")
	}
}

func TestTaskSummaryUnicodeAndManualTitleAPI(t *testing.T) {
	title, truncated := taskSummary(strings.Repeat("🙂", 81))
	if !truncated || !utf8.ValidString(title) || utf8.RuneCountInString(title) != 80 {
		t.Fatal("Unicode title cut incorrectly")
	}
	g, s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("title API must not invoke models") })
	s.Save(Record{ID: "r", Kind: "inference", Session: "A", Prompt: "P", Class: "main", Started: time.Now()})
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"session_id":"A","prompt_id":"wrong","title":"name"}`, 400},
		{`{"session_id":"A","prompt_id":"P","title":""}`, 400},
		{`{"session_id":"A","prompt_id":"P","title":"<script>test</script>","source":"request"}`, 200},
	} {
		r := httptest.NewRequest("POST", "http://localhost/api/task-title", strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		g.ServeHTTP(rr, r)
		if rr.Code != tc.status {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	list, _ := s.Tasks(g.Config, 100)
	if list.Tasks[0].Label.Source != "manual" {
		t.Fatal("caller spoofed title source")
	}
	r := httptest.NewRequest("GET", "http://localhost/api/tasks", nil)
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Fatal("local tasks should load without authentication")
	}
}
