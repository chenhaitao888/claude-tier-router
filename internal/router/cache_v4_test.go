package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const safeguardSample = `{"model":"auto","max_tokens":100,"thinking":{"type":"adaptive"},"safeguards":[{"type":"dangerous_tool_use","classifier_context":{"v":1,"live_cwd":"fixture-private-context","prior_turn_context":[]}}],"system":[{"type":"text","text":"stable system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"analyze concurrency","cache_control":{"type":"ephemeral"}}]}]}`

func TestClassifierContextDoesNotInvalidatePromptCache(t *testing.T) {
	c := testConfig()
	b := &branch{Cache: map[string][]cacheEntry{}}
	now := time.Now()
	first := parseTestRequest(t, safeguardSample)
	usage := countedUsage(2, 0, 100, 10)
	Observe(b, first, c.High, "high", usage, now, true)
	body := strings.Replace(safeguardSample, `"prior_turn_context":[]`, `"prior_turn_context":[{"request":"new-shell-call","tool_use_ids":["tool1"],"context":{"live_cwd":"a/different/path"}}]`, 1)
	next := parseTestRequest(t, body)
	n, diag := predictCache(next, c.High, b.Cache["high"], b.Shapes["high"], now)
	if n != 80 || diag.Reason != "prefix_match" || !diag.ClassifierContextChanged || len(diag.ChangedSections) != 0 {
		t.Fatalf("metadata broke stable prefix: %d %+v", n, diag)
	}
	out, err := ReplaceModel(next.Raw, "test-opus")
	if err != nil || string(out) != strings.Replace(body, `"model":"auto"`, `"model":"test-opus"`, 1) {
		t.Fatal("forwarded safeguard or safety context altered")
	}
	for _, change := range []struct{ old, new string }{
		{`"v":1`, `"v":2`},
		{`"dangerous_tool_use"`, `"future_guard"`},
		{`"type":"dangerous_tool_use"`, `"type":"dangerous_tool_use","future_policy":"new-policy"`},
		{`"type":"adaptive"`, `"type":"enabled","budget_tokens":2000`},
		{`"stable system"`, `"different system"`},
	} {
		r := parseTestRequest(t, strings.Replace(body, change.old, change.new, 1))
		n, d := predictCache(r, c.High, b.Cache["high"], b.Shapes["high"], now)
		if n != 0 || d.Reason != "prefix_changed" {
			t.Fatalf("ignored relevant change %s: %d %+v", change.new, n, d)
		}
	}
}

func TestCacheDiagnosticsExplainColdExpiredAndChanged(t *testing.T) {
	c := testConfig()
	r := parseTestRequest(t, safeguardSample)
	b := &branch{Cache: map[string][]cacheEntry{}}
	now := time.Now()
	_, d := predictCache(r, c.High, nil, nil, now)
	if d.Reason != "no_local_evidence" {
		t.Fatal(d)
	}
	Observe(b, r, c.High, "high", countedUsage(2, 0, 100, 10), now, true)
	_, d = predictCache(r, c.High, b.Cache["high"], b.Shapes["high"], now.Add(301*time.Second))
	if d.Reason != "evidence_expired" || d.LiveEntries != 0 {
		t.Fatal(d)
	}
	r = parseTestRequest(t, strings.Replace(safeguardSample, "stable system", "changed", 1))
	_, d = predictCache(r, c.High, b.Cache["high"], b.Shapes["high"], now)
	if len(d.ChangedSections) != 1 || d.ChangedSections[0] != "system" {
		t.Fatal(d)
	}
}

func TestGatewayCacheAfterShellContextUpdate(t *testing.T) {
	calls := 0
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(string(body["safeguards"]), "classifier_context") {
			t.Error("guard context was stripped upstream")
		}
		read, write := 0, 100
		if calls > 0 {
			read, write = 100, 0
		}
		calls++
		json.NewEncoder(w).Encode(map[string]any{"type": "message", "model": "test-opus", "stop_reason": "end_turn", "content": []any{}, "usage": map[string]any{"input_tokens": 2, "cache_read_input_tokens": read, "cache_creation_input_tokens": write, "output_tokens": 10, "cache_creation": map[string]int{"ephemeral_5m_input_tokens": write, "ephemeral_1h_input_tokens": 0}}})
	})
	for _, body := range []string{safeguardSample, strings.Replace(safeguardSample, `"prior_turn_context":[]`, `"prior_turn_context":[{"request":"new-shell-context"}]`, 1)} {
		rr := httptest.NewRecorder()
		g.ServeHTTP(rr, request(body))
		if rr.Code != 200 {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	rows, _ := s.Records(10)
	last := rows[0]
	if last.Decision.HighCached != 80 || last.CacheObservation != "evidence_updated" || !last.Decision.HighCache.ClassifierContextChanged {
		t.Fatalf("unexpected diagnostic %+v", last.Decision.HighCache)
	}
	stored, _ := json.Marshal(rows)
	if strings.Contains(string(stored), "fixture-private-context") || strings.Contains(string(stored), "new-shell-context") {
		t.Fatal("cache diagnostics leaked payload")
	}
	sum, _ := s.Summary()
	stats := sum.RoutingHealth.CachePredictions["block-prefix-v4"]
	if stats.ZeroPredictionHits != 0 || stats.MetadataChanges != 1 {
		t.Fatal(stats)
	}
}

// Use local CLI captures to test the exact request shape without contacting
// any provider. Synthetic usage only exercises bookkeeping, not provider hits.
func TestLocalWireCapture(t *testing.T) {
	path := os.Getenv("ROUTER_WIRE_CAPTURE")
	if path == "" {
		t.Skip("set ROUTER_WIRE_CAPTURE to a local synthetic CLI capture")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Request json.RawMessage `json:"request"`
	}
	if json.Unmarshal(raw, &rows) != nil {
		t.Fatal("invalid capture")
	}
	c := testConfig()
	b := &branch{Cache: map[string][]cacheEntry{}}
	now := time.Now()
	checks := 0
	for i, row := range rows {
		r := parseTestRequest(t, string(row.Request))
		p := c.High
		if i > 0 {
			n, d := predictCache(r, p, b.Cache["high"], b.Shapes["high"], now)
			if n == 0 {
				t.Fatalf("request %d: unexpected miss %+v", i, d)
			}
			checks++
		}
		Observe(b, r, p, "high", countedUsage(2, 0, int64(r.CacheableTokens), 10), now, true)
	}
	t.Logf("wire requests=%d warm predictions=%d (synthetic usage; no provider calls)", len(rows), checks)
}
