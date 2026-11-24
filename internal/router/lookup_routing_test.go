package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLookupTaskUsesLowAcrossToolContinuation(t *testing.T) {
	g, s, calls := lookupFixture(t)
	const task = "帮我看下新架构的询价、追加的埋点表是哪些"
	first := `{"model":"router-coordinator","max_tokens":100,"messages":[{"role":"user","content":"` + task + `"}]}`
	// Tool results do not replace the task, and incidental domain terminology
	// must not turn repeated retrieval into architecture design.
	next := strings.TrimSuffix(first, "]}") + `,{"role":"assistant","content":[{"type":"tool_use","id":"read-1","name":"Read","input":{"file_path":"MetricConstant.java"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"read-1","content":"新架构 metric constants"}]}]}`
	for _, body := range []string{first, next} {
		coopSend(t, g, request(body), http.StatusOK)
	}
	rows, err := s.Records(10)
	if err != nil || len(rows) != 2 || calls.Load() != 2 {
		t.Fatalf("expected two direct requests without classification: records=%d calls=%d err=%v", len(rows), calls.Load(), err)
	}
	for _, r := range rows {
		if r.Actual != "low" || r.Cooperation == nil || r.Cooperation.Stage != "direct" {
			t.Fatalf("lookup did not use direct low route: tier=%s decision=%+v", r.Actual, r.Decision)
		}
	}
}

func TestLookupIntentKeepsDesignAndAuxiliaryOnHigh(t *testing.T) {
	for _, tc := range []struct{ task, class, tier string }{
		{"列出新架构的字段和常量", "main", "low"},
		{"Find the table names used by the existing architecture.", "main", "low"},
		{"查找并发模块的配置项定义", "subagent", "low"},
		{"找出新架构的埋点表，并分析数据丢失的原因", "main", "high"},
		{"列出分布式架构的表名并评估安全漏洞", "main", "high"},
		{"Find table names and design an architecture migration.", "main", "high"},
		{"Find the architecture tables causing deadlocks.", "main", "high"},
		{"找出新架构里导致死锁的字段", "main", "high"},
		{"查找架构问题", "main", "high"},
		{"帮我看下新架构的询价、追加的埋点表是哪些", "auxiliary", "high"},
		{"帮我看下新架构的询价、追加的埋点表是哪些", "compaction", "high"},
	} {
		t.Run(tc.class+"/"+tc.task, func(t *testing.T) {
			g, s, calls := lookupFixture(t)
			r := coopRequest("router-auto", tc.task, "")
			r.Header.Set("x-claude-code-request-class", tc.class)
			coopSend(t, g, r, http.StatusOK)
			rows, err := s.Records(10)
			if err != nil || len(rows) != 1 || calls.Load() != 1 || rows[0].Actual != tc.tier {
				t.Fatalf("tier=%s records=%+v calls=%d err=%v", tc.tier, rows, calls.Load(), err)
			}
		})
	}
}

func TestCooperationRecordsObservedDelegation(t *testing.T) {
	g, s, _ := lookupFixture(t)
	coopSend(t, g, coopRequest(CoordinatorModel, "分析并发架构", ""), http.StatusOK)
	coopHook(t, g, "session", "task", "worker", "executor", "SubagentStart", "")
	coopSend(t, g, coopRequest(ExecutorModel, "列出新架构的埋点表", "worker"), http.StatusOK)
	coopSend(t, g, coopRequest(CoordinatorModel, "列出新架构的埋点表", ""), http.StatusOK)
	rows, err := s.Records(10)
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	for i, r := range rows {
		raw, _ := json.Marshal(r)
		var stored struct {
			Cooperation struct {
				Observed *bool `json:"delegation_observed"`
			} `json:"cooperation"`
		}
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		if r.Agent != "" {
			if r.Actual != "low" || stored.Cooperation.Observed != nil {
				t.Fatal("worker lost its dedicated low route or gained parent-only status", r)
			}
			continue
		}
		// Records are newest first: the final coordinator has observed a worker,
		// while its original planning request had not. Both must remain high.
		if r.Actual != "high" || stored.Cooperation.Observed == nil || *stored.Cooperation.Observed != (i == 0) {
			t.Fatalf("missing/incorrect delegation observation: %s", raw)
		}
	}
}

func TestLookupDoesNotDowngradeMixedOrUnresolvedTasks(t *testing.T) {
	for _, task := range []string{
		"列出新架构的表名，并验收埋点方案",
		"列出新架构的表名，并重写数据访问层",
		"找出新架构里发生数据竞争的字段",
		"找出新架构里不一致的字段",
		"列出新架构的表名，并解释事务一致性约束",
		"新架构应该使用哪些字段",
		"List table names and rewrite the distributed storage layer.",
		"Find definitions and explain the distributed transaction protocol.",
		"What fields should the new distributed architecture use?",
		"List the tables whose distributed transactions can lose writes.",
	} {
		t.Run(task, func(t *testing.T) {
			tier, reason, _ := Rule(Request{Text: task, RequestClass: "main"})
			if tier != "high" || reason != "complexity_rule" {
				t.Fatalf("mixed task bypassed complexity rules: tier=%s reason=%s", tier, reason)
			}
		})
	}
}

func TestLookupStillComparesProviderCosts(t *testing.T) {
	g, s, calls := lookupFixture(t)
	g.Config.High.Prices = &Prices{Version: "test", Currency: "USD"}
	g.Config.Low.Prices = &Prices{Version: "test", Currency: "USD", Input: 1, Output: 2}
	coopSend(t, g, coopRequest("router-auto", "列出新架构的表名", ""), http.StatusOK)
	rows, err := s.Records(10)
	if err != nil || len(rows) != 1 || calls.Load() != 1 {
		t.Fatal(rows, calls.Load(), err)
	}
	r := rows[0]
	if r.Decision.Recommended != "low" || r.Actual != "high" || !r.Decision.CostComparable || !strings.Contains(strings.Join(r.Decision.Reasons, ","), "high_estimated_cheaper") {
		t.Fatal("lookup bypassed cost comparison", r.Decision)
	}
}

func TestLookupRoutingRetainsHistoryContentAndModeGuards(t *testing.T) {
	const lookup = "列出新架构的埋点表"
	for _, tc := range []struct {
		name, mode, extra, history string
		seedHigh, disableSwitch    bool
		lowLimit                   int
		status                     int
		tier                       string
	}{
		{name: "plain history can switch", seedHigh: true, status: 200, tier: "low"},
		{name: "disabled switching", seedHigh: true, disableSwitch: true, status: 200, tier: "high"},
		{name: "thinking stays high", seedHigh: true, history: `{"type":"thinking","thinking":"opaque","signature":"sig"}`, status: 200, tier: "high"},
		{name: "unknown thinking origin", history: `{"type":"thinking","thinking":"opaque","signature":"sig"}`, status: 409},
		{name: "server state stays high", seedHigh: true, history: `{"type":"server_tool_use","id":"s"}`, status: 200, tier: "high"},
		{name: "image needs high", extra: `,{"type":"image"}`, status: 200, tier: "high"},
		{name: "tool failure needs high", extra: `,{"type":"tool_result","tool_use_id":"t","is_error":true,"content":"failed"}`, status: 200, tier: "high"},
		{name: "low context insufficient", lowLimit: 1, status: 200, tier: "high"},
		{name: "fixed high", mode: "high", status: 200, tier: "high"},
		{name: "shadow executes high", mode: "shadow", status: 200, tier: "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, s, calls := lookupFixture(t)
			if tc.mode != "" {
				g.Config.Mode = tc.mode
			}
			g.Config.Policy.AllowTextSwitch = !tc.disableSwitch
			if tc.lowLimit > 0 {
				g.Config.Low.ContextTokens = tc.lowLimit
			}
			if tc.seedHigh {
				coopSend(t, g, coopRequest("router-auto", "分析并发架构", ""), http.StatusOK)
			}
			prefix := ""
			if tc.seedHigh || tc.history != "" {
				block := tc.history
				if block == "" {
					block = `{"type":"text","text":"known context"}`
				}
				prefix = `{"role":"assistant","content":[` + block + `]},`
			}
			body := `{"model":"router-auto","max_tokens":100,"messages":[` + prefix + `{"role":"user","content":[{"type":"text","text":"` + lookup + `"}` + tc.extra + `]}]}`
			before := calls.Load()
			coopSend(t, g, request(body), tc.status)
			rows, err := s.Records(10)
			if err != nil || len(rows) == 0 || rows[0].Actual != tc.tier {
				t.Fatalf("expected tier %q: records=%+v err=%v", tc.tier, rows, err)
			}
			if tc.status != http.StatusOK && calls.Load() != before {
				t.Fatal("blocked history dispatched an upstream request")
			}
		})
	}
}

type lookupTransport func(*http.Request) (*http.Response, error)

func (f lookupTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the real gateway, parser, routing and SQLite ledger without network
// listeners or paid upstreams. Only the provider's HTTP transport is replaced.
func lookupFixture(t *testing.T) (*Gateway, *Store, *atomic.Int64) {
	t.Helper()
	c := testConfig()
	c.High.Prices, c.Low.Prices = nil, nil
	c.High.APIKey, c.Low.APIKey = "test-high-key", "test-low-key"
	c.Classifier.Enabled = true
	s, err := OpenStore(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	g := NewGateway(c, s)
	calls := &atomic.Int64{}
	g.Client = &http.Client{Transport: lookupTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		var payload struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		jsonResponse(w, map[string]any{"type": "message", "model": payload.Model, "content": []any{map[string]string{"type": "text", "text": "fixture"}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 10, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 5}})
		return w.Result(), nil
	})}
	return g, s, calls
}
