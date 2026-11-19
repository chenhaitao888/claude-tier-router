package router

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func parseTestRequest(t *testing.T, body string) Request {
	t.Helper()
	r, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTaskExtractionIgnoresRemindersWithoutRewritingPayload(t *testing.T) {
	text := "<system-reminder>" + strings.Repeat("architecture distributed security 项目规范\n", 200) + "</system-reminder>\n解释ExecuteActivity这个函数，并给出一个使用示例"
	encoded, _ := json.Marshal(text)
	body := `{"model":"auto","messages":[{"role":"user","content":` + string(encoded) + `},{"role":"assistant","content":"answer"},{"role":"user","content":[{"type":"tool_result","content":"architecture error","tool_use_id":"t"},{"type":"text","text":"<system-reminder>security</system-reminder>"}]}]}`
	r := parseTestRequest(t, body)
	tier, _, _ := Rule(r)
	if tier != "low" || r.Text != "解释ExecuteActivity这个函数，并给出一个使用示例" || r.ReminderBlocks != 2 || strings.Contains(r.RecentText, "项目规范") {
		t.Fatalf("task extraction failed: %s %s", tier, r.Text)
	}
	out, _ := ReplaceModel(r.Raw, "flash")
	if string(out) != strings.Replace(body, `"model":"auto"`, `"model":"flash"`, 1) {
		t.Fatal("routing altered forwarded instructions")
	}
	// Chinese tasks use character limits rather than UTF-8 byte lengths.
	r.Text = "解释" + strings.Repeat("中文", 400)
	if tier, _, _ := Rule(r); tier != "low" {
		t.Fatal("character bound uses bytes")
	}
	r.Text = "分析这个项目的并发架构，找出可能的死锁路径并说明依据。"
	if tier, _, _ := Rule(r); tier != "high" {
		t.Fatal("complex task downgraded")
	}
}

func TestClassifierInputRetainsTaskAndValidUnicode(t *testing.T) {
	r := Request{Text: "查找这个问题", RecentText: strings.Repeat("历史", 1000)}
	var input struct {
		Task    string `json:"task"`
		Context string `json:"recent_context"`
	}
	text := classifierInput(r, 100)
	if json.Unmarshal([]byte(text), &input) != nil || input.Task != r.Text || !utf8.ValidString(text) || utf8.RuneCountInString(input.Task+input.Context) > 100 {
		t.Fatal("lost task or exceeded context bound")
	}
}

func TestClassifierDecisionValidation(t *testing.T) {
	valid := `{"tier":"low","confidence":0.9,"reason":"simple"}`
	for _, tc := range []struct{ name, text, code string }{
		{"valid", valid, "ok"}, {"fenced", "```json\n" + valid + "\n```", "ok"},
		{"empty", "", "empty_output"}, {"array", "[]", "expected_object"},
		{"missing", `{"tier":"low","reason":"simple"}`, "missing_confidence"},
		{"null", `{"tier":"low","reason":"simple","confidence":null}`, "missing_confidence"},
		{"type", `{"tier":"low","reason":"simple","confidence":"0.9"}`, "invalid_field_type"},
		{"range", `{"tier":"low","reason":"simple","confidence":2}`, "confidence_out_of_range"},
		{"duplicate", `{"tier":"low","tier":"high"}`, "duplicate_field"},
		{"extra", `{"explanation":"secret"}`, "unknown_field"},
		{"reason", `{"tier":"low","confidence":0.9,"reason":"very simple"}`, "invalid_reason"},
		{"contradiction", `{"tier":"low","confidence":0.9,"reason":"complex"}`, "inconsistent_decision"},
		{"trailing", valid + " prose", "trailing_content"}, {"two", valid + valid, "trailing_content"},
		{"truncated", `{"tier":"low"`, "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got, _ := parseClassification(tc.text)
			if got != tc.code {
				t.Fatalf("got %s want %s", got, tc.code)
			}
		})
	}
}

func TestClassifierDiagnosticsAndConservativeFallback(t *testing.T) {
	for _, tc := range []struct{ stop, output, status, validation string }{
		{"end_turn", "```json\n{\"tier\":\"low\",\"confidence\":0.9,\"reason\":\"simple\"}\n```", "accepted", "ok"},
		{"end_turn", `{"tier":"low","confidence":0.55,"reason":"simple"}`, "uncertain", "ok"},
		{"end_turn", `{"tier":"low","reason":"simple"}`, "invalid", "missing_confidence"},
		{"max_tokens", `{"tier":"low","confidence":0.9,"reason":"simple"}`, "invalid", "unexpected_stop_reason"},
	} {
		t.Run(tc.status+tc.validation, func(t *testing.T) {
			g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var payload map[string]any
				json.Unmarshal(raw, &payload)
				if _, ok := payload["system"]; ok {
					json.NewEncoder(w).Encode(map[string]any{"type": "message", "stop_reason": tc.stop, "content": []any{map[string]string{"type": "text", "text": tc.output}}, "usage": map[string]int{"input_tokens": 10, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 10}})
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, goodStream)
				}
			})
			g.Config.Mode = "auto"
			g.Config.Classifier.Enabled = true
			rr := httptest.NewRecorder()
			g.ServeHTTP(rr, request(strings.Replace(sample, "explain this function", "do the requested change", 1)))
			rows, _ := s.Records(10)
			if rr.Code != 200 || len(rows) != 2 {
				t.Fatalf("status %d, records %d", rr.Code, len(rows))
			}
			for _, rec := range rows {
				if rec.Kind == "classifier" {
					if rec.ClassifierDiagnostic.Status != tc.status || rec.ClassifierDiagnostic.Validation != tc.validation {
						t.Fatal(rec.ClassifierDiagnostic)
					}
					stored, _ := json.Marshal(rec)
					if strings.Contains(string(stored), tc.output) {
						t.Fatal("raw classifier output persisted")
					}
				} else if tc.status != "accepted" && rec.Actual != "high" {
					t.Fatal("invalid/uncertain decision downgraded")
				}
			}
		})
	}
}

func TestProtectedHistorySkipsClassifierAndCleanBoundaryUnlocks(t *testing.T) {
	var calls atomic.Int64
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, goodStream)
	})
	g.Config.Mode = "auto"
	g.Config.Classifier.Enabled = true
	b, release := g.Engine.Acquire("session\x00\x00main")
	b.Current = "high"
	release()
	body := `{"model":"auto","messages":[{"role":"user","content":"do the change"},{"role":"assistant","content":[{"type":"thinking","thinking":"opaque","signature":"signed"}]},{"role":"user","content":"continue"}],"max_tokens":100,"stream":true}`
	req := request(body)
	req.Header.Set("x-claude-code-prompt-id", "new-prompt")
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, req)
	rows, _ := s.Records(10)
	if rr.Code != 200 || calls.Load() != 1 || len(rows) != 1 || rows[0].Actual != "high" || rows[0].Decision.ClassifierSkipReason != "protected_history_requires_same_model" {
		t.Fatal("paid classifier or switched protected history", rows)
	}
	// Actual clean history, even within the same session, permits routing again.
	rr = httptest.NewRecorder()
	g.ServeHTTP(rr, request(sample))
	rows, _ = s.Records(10)
	if rr.Code != 200 || rows[0].Actual != "low" || !rows[0].Decision.SwitchAllowed {
		t.Fatal("clean boundary stayed locked")
	}
	// Restart/resume has no in-memory origin; reject before either paid call.
	g.Engine = NewEngine()
	before := calls.Load()
	rr = httptest.NewRecorder()
	g.ServeHTTP(rr, request(body))
	if rr.Code != 409 || calls.Load() != before {
		t.Fatal("unknown protected origin dispatched")
	}
}

func TestCacheBlockPrefixesSurviveTailEditsAndMessageGrowth(t *testing.T) {
	c := testConfig()
	now := time.Now()
	b := &branch{Cache: map[string][]cacheEntry{}}
	first := parseTestRequest(t, `{"model":"auto","system":[{"type":"text","text":"stable rules","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"stable question","cache_control":{"type":"ephemeral"}},{"type":"text","text":"uncached tail"}]}]}`)
	Observe(b, first, c.High, "high", countedUsage(10, 0, 100, 20), now, true)
	next := parseTestRequest(t, strings.Replace(strings.Replace(string(first.Raw), `"uncached tail"`, `"new tail and an appended block"`, 1), `"ephemeral"`, `"ephemeral","ttl":"1h"`, 1))
	if n := cachedTokens(next, c.High, b.Cache["high"], now); n <= 0 {
		t.Fatal("stable cached blocks lost when tail changed")
	}
	changed := parseTestRequest(t, strings.Replace(string(next.Raw), "stable rules", "different rules", 1))
	if n := cachedTokens(changed, c.High, b.Cache["high"], now); n != 0 {
		t.Fatal("reused changed system prefix")
	}
	if n := cachedTokens(next, c.High, b.Cache["high"], now.Add(301*time.Second)); n != 0 {
		t.Fatal("cache did not expire")
	}
	if n := cachedTokens(next, c.Low, b.Cache["low"], now); n != 0 {
		t.Fatal("cache shared across providers")
	}
}

func TestAutomaticColdCacheAndIncompleteResponses(t *testing.T) {
	c := testConfig()
	r := parseTestRequest(t, `{"model":"auto","messages":[{"role":"user","content":"question"}]}`)
	now := time.Now()
	b := &branch{Cache: map[string][]cacheEntry{}}
	Observe(b, r, c.Low, "low", countedUsage(100, 0, 0, 10), now, false)
	if len(b.Cache["low"]) != 0 {
		t.Fatal("partial response established cache")
	}
	Observe(b, r, c.Low, "low", countedUsage(100, 0, 0, 10), now, true)
	if cachedTokens(r, c.Low, b.Cache["low"], now) == 0 {
		t.Fatal("automatic cold input never seeded cache")
	}
}

func TestCacheLookbackAndUncachedTailPricing(t *testing.T) {
	c := testConfig()
	r := Request{Tokens: 1000, MaxTokens: 10, CacheEnabled: true, CacheableTokens: 600, CacheBreakpoints: make([]bool, 22), Prefixes: make([]string, 22)}
	r.Prefixes[0] = "old"
	r.CacheBreakpoints[21] = true
	if cachedTokens(r, c.High, []cacheEntry{{"old", 100, time.Now()}}, time.Now()) != 0 {
		t.Fatal("explicit lookback exceeded")
	}
	cost := Estimate(r, c.High, 500, 10)
	want := (400*c.High.Prices.Input + 100*c.High.Prices.CacheWrite5m + 500*c.High.Prices.CacheRead + 10*c.High.Prices.Output) / 1e6
	if math.Abs(*cost-want) > 1e-12 {
		t.Fatalf("uncached tail priced as cache write: %g != %g", *cost, want)
	}
}

func TestExchangeRateDecisionSnapshotAndValidation(t *testing.T) {
	c := testConfig()
	c.High.Cache.Kind = "none"
	c.High.Prices.Input = 1
	c.Low.Prices.Input = 2
	c.Low.Prices.Currency = "CNY"
	r := Request{Tokens: 100000, MaxTokens: 1}
	b := &branch{Cache: map[string][]cacheEntry{}}
	c.Policy.ExchangeRate = &ExchangeRate{Version: "fixed", CNYPerUSD: 6.8}
	d := Decide(c, r, b, "low", "simple", .9)
	if !d.CostComparable || d.Selected != "low" || d.ComparisonCurrency != "USD" || math.Abs(*d.ComparisonLow-*d.LowNativeCost/6.8) > 1e-12 || d.LowCurrency != "CNY" || d.LowCost != nil {
		t.Fatalf("bad FX decision %+v", d)
	}
	c.Policy.ExchangeRate.CNYPerUSD = 1
	if d.ExchangeRate.CNYPerUSD != 6.8 {
		t.Fatal("FX snapshot mutated")
	}
	d = Decide(c, r, b, "low", "simple", .9)
	if d.Selected != "high" {
		t.Fatal("FX didn't affect cost decision")
	}
	for _, rate := range []float64{0, -1, math.Inf(1), math.NaN()} {
		c.Policy.ExchangeRate.CNYPerUSD = rate
		if c.Validate() == nil {
			t.Fatal("invalid FX accepted")
		}
	}
}

func TestRoutingHealthSeparatesPredictionVersions(t *testing.T) {
	h := RoutingHealth{ClassifierOutcomes: map[string]int{}, ClassifierSkipped: map[string]int{}, SwitchBlocked: map[string]int{}, CachePredictions: map[string]*CachePredictionStats{}}
	r := Record{Kind: "inference", Actual: "high", Complete: true, Usage: countedUsage(10, 100, 0, 1)}
	h.add(r)
	r.Decision.CachePredictionVersion = "block-prefix-v3"
	r.Decision.HighCached = 80
	h.add(r)
	if h.CachePredictions["legacy"].ZeroPredictionHits != 1 || h.CachePredictions["block-prefix-v3"].ZeroPredictionHits != 0 || h.CachePredictions["block-prefix-v3"].AbsoluteError != 20 {
		t.Fatal("mixed prediction versions", h)
	}
}

func TestOutputLimitIsRecordedWithoutLosingBillableUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, strings.Replace(goodStream, `"usage":{"output_tokens":9}`, `"delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":9}`, 1))
				} else {
					io.WriteString(w, `{"type":"message","model":"test-opus","stop_reason":"max_tokens","content":[],"usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":9}}`)
				}
			})
			rr := httptest.NewRecorder()
			g.ServeHTTP(rr, request(sample))
			rows, _ := s.Records(10)
			sum, _ := s.Summary()
			if len(rows) != 1 || rows[0].StopReason != "max_tokens" || !rows[0].Complete || rows[0].CostAmount == nil || sum.RoutingHealth.OutputLimited != 1 {
				t.Fatal("output limit lost or billable response discarded", rows)
			}
		})
	}
}
