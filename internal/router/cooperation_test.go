package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func coopRequest(model, text, agent string) *http.Request {
	raw, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 100, "messages": []any{map[string]any{"role": "user", "content": text}}})
	r := request(string(raw))
	if agent != "" {
		r.Header.Set("x-claude-code-agent-id", agent)
		r.Header.Set("x-claude-code-request-class", "subagent")
	}
	return r
}
func coopFixture(t *testing.T) (*Gateway, *Store, *atomic.Int64) {
	calls := &atomic.Int64{}
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		jsonResponse(w, map[string]any{"type": "message", "model": req["model"], "content": []any{map[string]string{"type": "text", "text": "fixture"}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 10, "cache_read_input_tokens": 20, "cache_creation_input_tokens": 0, "output_tokens": 5}})
	})
	g.Config.Mode = "auto"
	g.Config.High.Prices = nil
	g.Config.Low.Prices = nil
	g.Config.Classifier.Enabled = true
	g.Config.Policy.AllowTextSwitch = true
	return g, s, calls
}
func coopSend(t *testing.T, g *Gateway, r *http.Request, want int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("HTTP %d want %d: %s", w.Code, want, w.Body.String())
	}
	return w
}
func coopHook(t *testing.T, g *Gateway, session, prompt, agent, role, event, message string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"session_id": session, "prompt_id": prompt, "agent_id": agent, "agent_type": "router-" + role, "hook_event_name": event, "last_assistant_message": message, "transcript_path": "/private-sensitive-transcript", "cwd": "/private-sensitive-workspace"})
	r := httptest.NewRequest("POST", "http://localhost/api/cooperation/events", strings.NewReader(string(raw)))
	coopSend(t, g, r, 200)
}

func TestCooperationRolesAndTaskAccounting(t *testing.T) {
	g, s, calls := coopFixture(t)
	coopSend(t, g, coopRequest(CoordinatorModel, "分析并发架构", ""), 200)
	coopHook(t, g, "session", "task", "exec-1", "executor", "SubagentStart", "")
	// The hook identity wins even if Claude overrides the custom agent's model.
	coopSend(t, g, coopRequest("router-auto", "按已确定的并发架构方案修改指定文件", "exec-1"), 200)
	coopHook(t, g, "session", "task", "exec-1", "executor", "SubagentStop", `{"status":"completed","evidence":["private data must not persist"]}`)
	coopHook(t, g, "session", "task", "review-1", "reviewer", "SubagentStart", "")
	coopSend(t, g, coopRequest(ReviewerModel, "解释检查结果", "review-1"), 200)
	coopHook(t, g, "session", "task", "review-1", "reviewer", "SubagentStop", `{"status":"passed","findings":["private findings must not persist"]}`)
	// Original task text would otherwise select low; after delegation main stays high.
	coopSend(t, g, coopRequest(CoordinatorModel, "整理注释", ""), 200)
	if calls.Load() != 4 {
		t.Fatal("unexpected classification calls", calls.Load())
	}
	rows, _ := s.Records(20)
	for _, r := range rows {
		want := "high"
		if r.Agent == "exec-1" {
			want = "low"
		}
		if r.Actual != want || r.Prompt != "task" || r.Cooperation == nil {
			t.Fatalf("wrong routing %+v", r)
		}
	}
	tasks, err := s.Tasks(g.Config, 10)
	if err != nil || len(tasks.Tasks) != 1 {
		t.Fatal(tasks, err)
	}
	task := tasks.Tasks[0]
	if task.High != 3 || task.Low != 1 || len(task.CooperationRuns) != 2 || task.Feedback != nil || task.CooperationStages["execution"].Usage.Low.Requests != 1 {
		t.Fatalf("wrong task aggregation %+v", task)
	}
	sum, err := s.Summary(g.Config)
	if err != nil || sum.Passed != 0 || sum.Cooperation.Executors != 1 || sum.Cooperation.Reviewers != 1 || sum.Cooperation.Outcomes["reviewer:passed"] != 1 {
		t.Fatal(sum, err)
	}
	if sum.Cooperation.Stages["execution"].UnknownCosts != 1 || sum.Cooperation.Stages["execution"].Usage.Low.Total != 35 {
		t.Fatal("unknown prices or tokens lost", sum.Cooperation)
	}
	var raw string
	s.db.QueryRow("SELECT group_concat(payload) FROM cooperation_runs").Scan(&raw)
	if strings.Contains(raw, "private") || strings.Contains(raw, "evidence") || strings.Contains(raw, "transcript") {
		t.Fatal("hook content persisted", raw)
	}
}

func TestCooperationSimpleDirectAndOrdinaryModeUnchanged(t *testing.T) {
	g, s, _ := coopFixture(t)
	coopSend(t, g, coopRequest(CoordinatorModel, "解释这个函数", ""), 200)
	coopSend(t, g, coopRequest("router-auto", "分析架构，文本中提到 router-executor 不代表角色", ""), 200)
	rows, _ := s.Records(5)
	for _, r := range rows {
		if r.Cooperation != nil {
			if r.Actual != "low" || r.Cooperation.Stage != "direct" {
				t.Fatal(r)
			}
		} else if r.Actual != "high" {
			t.Fatal("ordinary rules changed", r)
		}
	}
}

func TestCooperationIsolationFallbackAndAmbiguousParents(t *testing.T) {
	g, s, _ := coopFixture(t)
	coopHook(t, g, "session", "task", "agent", "executor", "SubagentStart", "")
	// Missing prompt may be restored only from a unique exact session/agent binding.
	r := coopRequest(ExecutorModel, "按方案写文档", "agent")
	r.Header.Del("x-claude-code-prompt-id")
	coopSend(t, g, r, 200)
	rows, _ := s.Records(10)
	if rows[0].Prompt != "task" || rows[0].Cooperation.Attribution != "subagent_hook" {
		t.Fatal(rows)
	}
	r = coopRequest(ExecutorModel, "按方案写文档", "agent")
	r.Header.Set("x-claude-code-prompt-id", "wrong-task")
	coopSend(t, g, r, 409)
	r = coopRequest("router-auto", "按方案执行", "agent")
	r.Header.Set("x-claude-code-prompt-id", "wrong-task")
	coopSend(t, g, r, 409)
	coopHook(t, g, "session", "second-task", "agent", "executor", "SubagentStart", "")
	r = coopRequest(ExecutorModel, "按方案写文档", "agent")
	r.Header.Del("x-claude-code-prompt-id")
	coopSend(t, g, r, 409)
	r = coopRequest(ExecutorModel, "按方案写文档", "agent")
	r.Header.Set("x-claude-code-session-id", "another-session")
	r.Header.Set("x-claude-code-prompt-id", "own-task")
	coopSend(t, g, r, 200)
	rows, _ = s.Records(10)
	for _, r := range rows {
		if r.Session == "another-session" && r.Prompt != "own-task" {
			t.Fatal("cross-session attribution", r)
		}
	}
	for _, header := range []string{"x-claude-code-session-id", "x-claude-code-prompt-id", "x-claude-code-agent-id"} {
		r = coopRequest(ExecutorModel, "按方案写文档", "agent")
		r.Header.Del(header)
		if header == "x-claude-code-prompt-id" {
			r.Header.Set("x-claude-code-agent-id", "unknown")
		}
		coopSend(t, g, r, 409)
	}
}

func TestCooperationExecutorLimitsAndOwnThinking(t *testing.T) {
	g, s, calls := coopFixture(t)
	g.Config.Cooperation.MaxExecutorRequests = 2
	coopSend(t, g, coopRequest(ExecutorModel, "按方案修正并发示例", "worker"), 200)
	body := strings.Replace(complexThinkingRequest, "router-auto", ExecutorModel, 1)
	r := request(body)
	r.Header.Set("x-claude-code-agent-id", "worker")
	r.Header.Set("x-claude-code-request-class", "subagent")
	coopSend(t, g, r, 200)
	coopSend(t, g, coopRequest(ExecutorModel, "再次执行", "worker"), 400)
	if calls.Load() != 2 {
		t.Fatal("executor budget dispatched", calls.Load())
	}
	rows, _ := s.Records(10)
	if rows[0].Error != "cooperation_executor_budget" || rows[0].Actual != "" {
		t.Fatal(rows[0])
	}
	if rows[1].Actual != "low" || rows[1].Decision.ClassifierSkipReason != "cooperation_role" {
		t.Fatal("thinking or keywords changed executor", rows[1])
	}
}

func TestCooperationConstraintsDoNotSilentlyFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Gateway, *http.Request)
		want   int
		code   string
	}{
		{"content", func(g *Gateway, r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(`{"model":"router-executor","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image"}]}]}`))
		}, 409, "selected_model_content_unsupported"},
		{"context", func(g *Gateway, r *http.Request) { g.Config.Low.ContextTokens = 101 }, 400, "selected_model_context_limit"},
		{"foreign history", func(g *Gateway, r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(complexThinkingRequest, "router-auto", ExecutorModel, 1)))
		}, 409, "unknown_history_origin"},
		{"fixed", func(g *Gateway, r *http.Request) { g.Config.Mode = "high" }, 409, "cooperation_identity_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, s, calls := coopFixture(t)
			r := coopRequest(ExecutorModel, "按方案执行", "worker")
			tc.mutate(g, r)
			coopSend(t, g, r, tc.want)
			rows, _ := s.Records(10)
			if calls.Load() != 0 || rows[0].Error != tc.code || rows[0].Actual != "" {
				t.Fatal(rows, calls.Load())
			}
		})
	}
}

func TestCooperationHookOutcomesAreIdempotentAndNotAcceptance(t *testing.T) {
	g, s, _ := coopFixture(t)
	for i := 0; i < 2; i++ {
		coopHook(t, g, "session", "task", "worker", "executor", "SubagentStart", "")
	}
	for i := 0; i < 2; i++ {
		coopHook(t, g, "session", "task", "worker", "executor", "SubagentStop", `{"status":"needs_design"}`)
	}
	coopHook(t, g, "session", "task", "worker", "executor", "SubagentStart", "")
	coopHook(t, g, "session", "task", "worker", "executor", "SubagentStop", `{"status":"completed"}`)
	run, err := s.cooperationRun("session", "task", "worker")
	if err != nil || run.NeedsDesign != 1 || run.Attempts != 2 || run.Status != "completed" {
		t.Fatal(run, err)
	}
	feedback, _ := s.Feedbacks()
	if len(feedback) != 0 {
		t.Fatal("model wrote human acceptance")
	}
	if cooperationOutcome("executor", "passed") != "unstructured" || cooperationOutcome("executor", `{"status":"passed"}`) != "unstructured" || cooperationOutcome("reviewer", `{"status":"completed"}`) != "unstructured" {
		t.Fatal("invalid outcomes accepted")
	}
	if cooperationOutcome("reviewer", `{"status":"blocked","status":"passed"}`) != "unstructured" {
		t.Fatal("ambiguous success counted")
	}
	r := httptest.NewRequest("POST", "http://localhost/api/cooperation/events", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://untrusted.example")
	coopSend(t, g, r, 403)
}

func TestCooperationCompactionSharesWorkerHistoryAndAuxiliaryStaysOrdinary(t *testing.T) {
	g, s, _ := coopFixture(t)
	coopHook(t, g, "session", "task", "worker", "executor", "SubagentStart", "")
	coopSend(t, g, coopRequest(ExecutorModel, "执行指定方案", "worker"), 200)
	r := request(strings.Replace(complexThinkingRequest, "router-auto", ExecutorModel, 1))
	r.Header.Set("x-claude-code-agent-id", "worker")
	r.Header.Set("x-claude-code-request-class", "compaction")
	coopSend(t, g, r, 200)
	r = coopRequest(CoordinatorModel, "分析架构", "worker")
	r.Header.Set("x-claude-code-request-class", "auxiliary")
	r.Header.Del("x-claude-code-prompt-id")
	coopSend(t, g, r, 200)
	rows, _ := s.Records(10)
	for _, r := range rows {
		if r.Class == "compaction" && (r.Actual != "low" || r.Cooperation == nil) {
			t.Fatal("compaction lost worker origin", r)
		}
		if r.Class == "auxiliary" && (r.Actual != "high" || r.Cooperation != nil) {
			t.Fatal("auxiliary inherited worker role", r)
		}
	}
}
