package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestBatchToolRules(t *testing.T) {
	for _, tc := range []struct {
		rules      map[string]string
		tool, want string
	}{
		{nil, "mcp__warehouse-a__ai_query_submit", "handoff"},
		{nil, "mcp__reports__query_submit", "handoff"},
		{nil, "mcp__reports__query_result", ""},
		{nil, "Grep", "suggest"}, {nil, "Glob", "suggest"}, {nil, "Bash", ""}, {nil, "Write", ""},
		{map[string]string{}, "Glob", ""},
		{map[string]string{"mcp__docs__export": "handoff"}, "mcp__docs__export", "handoff"},
		{map[string]string{"mcp__*__fetch": "suggest", "mcp__catalog__fetch": "handoff"}, "mcp__catalog__fetch", "handoff"},
	} {
		c := CooperationConfig{BatchTools: tc.rules}
		matcher := c.BatchToolMatcher()
		match := matcher != "" && regexp.MustCompile(matcher).MatchString(tc.tool)
		if c.validateBatchTools() != nil || c.batchMode(tc.tool) != tc.want || match != (tc.want != "") {
			t.Fatal(tc, matcher)
		}
	}
	for _, invalid := range []map[string]string{{".*": "handoff"}, {"Agent": "suggest"}, {"Read": "allow"}, {"mcp__*__": "handoff"}} {
		c := testConfig()
		c.Cooperation.BatchTools = invalid
		if c.Validate() == nil {
			t.Fatal("accepted invalid tool rule", invalid)
		}
	}
	if a, b := batchDigest([]byte(`{"n":9007199254740993,"other":{"b":2,"a":1}}`)), batchDigest([]byte(`{"other":{"a":1,"b":2},"n":9007199254740993}`)); a == "" || a != b || a == batchDigest([]byte(`{"n":9007199254740992,"other":{"a":1,"b":2}}`)) {
		t.Fatal("unstable fingerprint or lost precision")
	}
}

func batchHook(t *testing.T, g *Gateway, e batchEvent) string {
	t.Helper()
	body, _ := json.Marshal(e)
	w := coopSend(t, g, httptest.NewRequest("POST", "http://localhost"+DelegationHookPath, bytes.NewReader(body)), 200)
	var resp struct {
		Output struct {
			Permission string `json:"permissionDecision"`
			Context    string `json:"additionalContext"`
			Reason     string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Output.Permission == "deny" && strings.Contains(resp.Output.Reason, ExecutorModel) {
		return "handoff"
	}
	if resp.Output.Permission == "" && strings.Contains(resp.Output.Context, ReviewerModel) {
		return "review_hint"
	}
	if resp.Output.Permission != "" {
		t.Fatal("unexpected permission change", resp)
	}
	if strings.Contains(resp.Output.Context, ExecutorModel) {
		return "suggest"
	}
	return "pass"
}

// Feed a real gateway request backed by a synthetic model. The hook has to
// identify the exact emitted call, whether the provider used SSE or JSON.
func emitBatch(t *testing.T, g *Gateway, prompt, model, task string, agentTool, stream bool, calls ...batchEvent) {
	t.Helper()
	reqTools := ""
	if agentTool {
		reqTools = `,"tools":[{"name":"Agent","input_schema":{"type":"object"}}]`
	}
	r := request(fmt.Sprintf(`{"model":%q,"max_tokens":100,"messages":[{"role":"user","content":%q}]%s}`, model, task, reqTools))
	r.Header.Set("x-claude-code-prompt-id", prompt)
	emitBatchRequest(t, g, r, stream, calls...)
}

func emitBatchRequest(t *testing.T, g *Gateway, request *http.Request, stream bool, calls ...batchEvent) {
	t.Helper()
	var blocks []map[string]string
	for _, c := range calls {
		blocks = append(blocks, map[string]string{"type": "tool_use", "name": c.Tool, "id": c.Call})
	}
	g.Client.Transport = lookupTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		if stream {
			var buf strings.Builder
			for _, block := range blocks {
				event, _ := json.Marshal(map[string]any{"type": "content_block_start", "content_block": block})
				fmt.Fprintf(&buf, "data: %s\n\n", event)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, strings.Replace(goodStream, "event: message_delta", buf.String()+"event: message_delta", 1))
		} else {
			jsonResponse(w, map[string]any{"type": "message", "model": "test-opus", "content": blocks, "stop_reason": "tool_use", "usage": map[string]int{"input_tokens": 1, "output_tokens": 2}})
		}
		return w.Result(), nil
	})
	coopSend(t, g, request, 200)
}

func TestBatchDelegationAcrossToolsAndHookRetries(t *testing.T) {
	const highTask = "分析并发架构"
	for _, tc := range []struct {
		tool, mode string
		custom     bool
	}{
		{"mcp__warehouse__ai_query_submit", "handoff", false}, {"Grep", "suggest", false}, {"mcp__docs__render_page", "handoff", true},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			g, s, _ := lookupFixture(t)
			if tc.custom {
				g.Config.Cooperation.BatchTools = map[string]string{tc.tool: tc.mode}
			}
			e := batchEvent{Session: "session", Tool: tc.tool, Call: "a", Event: "PreToolUse", Input: json.RawMessage(`{"scope":"private","page":1}`)}
			retry := e
			retry.Call = "retry"
			retry.Input = json.RawMessage(`{"page":1,"scope":"private"}`)
			b := e
			b.Call = "b"
			b.Input = json.RawMessage(`{"scope":"private","page":2}`)
			sibling := b
			sibling.Call = "c"
			emitBatch(t, g, "task", CoordinatorModel, highTask, true, true, e, retry, b, sibling)
			for _, wrong := range []batchEvent{{Prompt: "other"}, {Agent: "worker"}, {Call: "foreign"}} {
				copy := e
				if wrong.Prompt != "" {
					copy.Prompt = wrong.Prompt
				}
				if wrong.Agent != "" {
					copy.Agent = wrong.Agent
				}
				if wrong.Call != "" {
					copy.Call = wrong.Call
				}
				if batchHook(t, g, copy) != "pass" {
					t.Fatal(wrong)
				}
			}
			if batchHook(t, g, e) != "pass" || batchHook(t, g, retry) != "pass" || batchHook(t, g, b) != tc.mode || batchHook(t, g, b) != tc.mode {
				t.Fatal("probe/retry/handoff")
			}
			want := "pass"
			if tc.mode == "handoff" {
				want = tc.mode
			}
			if got := batchHook(t, g, sibling); got != want {
				t.Fatal("parallel sibling", got)
			}
			later := b
			later.Call = "later"
			later.Prompt = "task"
			agent := e
			agent.Tool = "Agent"
			agent.Call = "launch"
			agent.Event = "PostToolUseFailure"
			agent.Input = json.RawMessage(`{"subagent_type":"router-executor"}`)
			emitBatch(t, g, "task", CoordinatorModel, highTask, true, false, later, agent)
			batchHook(t, g, agent)
			if batchHook(t, g, later) != "pass" || batchHook(t, g, b) != tc.mode {
				t.Fatal("parent should proceed while re-delivery remains idempotent")
			}
			tasks, err := s.Tasks(g.Config, 10)
			if err != nil || len(tasks.Tasks) != 1 {
				t.Fatal(tasks, err)
			}
			task := tasks.Tasks[0]
			if len(task.BatchChecks) != 1 || task.BatchChecks[0] != (BatchCheck{Tier: "high", Tool: tc.tool, State: tc.mode, Continued: true}) || !task.ExecutorCallFailed || len(task.CooperationRuns) != 0 {
				t.Fatal(task)
			}
			var digest string
			if err = s.db.QueryRow("SELECT first_digest FROM cooperation_batches").Scan(&digest); err != nil || len(digest) != 64 || strings.Contains(digest, "private") {
				t.Fatal(digest, err)
			}
		})
	}
}

func TestBatchGuardScope(t *testing.T) {
	g, s, _ := lookupFixture(t)
	e := batchEvent{Session: "session", Tool: "Grep", Event: "PreToolUse", Call: "duplicated", Input: json.RawMessage(`{"pattern":"one"}`)}
	next := e
	next.Call = "second"
	next.Input = json.RawMessage(`{"pattern":"two"}`)
	for _, p := range []string{"q1", "q2"} {
		emitBatch(t, g, p, CoordinatorModel, "分析并发架构", true, true, e)
	}
	e.Prompt = "q1"
	if batchHook(t, g, e) != "pass" {
		t.Fatal("ambiguous even with prompt hint")
	}
	for _, tc := range []struct {
		prompt, model, question string
		hasAgent                bool
	}{
		{"no-agent", CoordinatorModel, "分析并发架构", false}, {"cheap", CoordinatorModel, "列出字段", true}, {"ordinary", "router-auto", "分析并发架构", true},
	} {
		first, b := e, next
		first.Prompt, b.Prompt = tc.prompt, tc.prompt
		first.Call = tc.prompt + "-one"
		b.Call = tc.prompt + "-two"
		emitBatch(t, g, tc.prompt, tc.model, tc.question, tc.hasAgent, false, first, b)
		if batchHook(t, g, first) != "pass" || batchHook(t, g, b) != "pass" {
			t.Fatal(tc)
		}
	}
	var rows int
	var state string
	if err := s.db.QueryRow("SELECT COUNT(*),MIN(state) FROM cooperation_batches").Scan(&rows, &state); err != nil || rows != 1 || state != "no_agent" {
		t.Fatal(rows, state, err)
	}
}

func TestParallelBatchProbes(t *testing.T) {
	g, _, _ := lookupFixture(t)
	var events []batchEvent
	for i := range 8 {
		events = append(events, batchEvent{Session: "session", Tool: "mcp__warehouse__query_submit", Call: fmt.Sprint(i), Event: "PreToolUse", Input: json.RawMessage(fmt.Sprintf(`{"partition":%d}`, i))})
	}
	emitBatch(t, g, "task", CoordinatorModel, "分析并发架构", true, true, events...)
	results := make(chan string, len(events))
	var wg sync.WaitGroup
	for _, e := range events {
		wg.Add(1)
		go func() { defer wg.Done(); results <- batchHook(t, g, e) }()
	}
	wg.Wait()
	close(results)
	var pass int
	for result := range results {
		if result == "pass" {
			pass++
		}
	}
	if pass != 1 {
		t.Fatalf("%d parallel probes passed", pass)
	}
}
