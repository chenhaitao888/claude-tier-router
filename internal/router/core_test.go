package router

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	p := Provider{BaseURL: "http://127.0.0.1:1", Model: "test-opus", APIKeyEnv: "TEST_HIGH_KEY", Auth: "api_key", ContextTokens: 200000, Cache: CacheConfig{"explicit", 300}, Prices: &Prices{Version: "test", Currency: "USD", Input: 3, CacheRead: 0.1, CacheWrite5m: 3.75, CacheWrite1h: 6, Output: 15}}
	low := p
	low.Model = "test-flash"
	low.APIKeyEnv = "TEST_LOW_KEY"
	low.Auth = "bearer"
	low.Cache.Kind = "automatic"
	low.Prices = &Prices{Version: "test", Currency: "USD", Input: 1, CacheRead: 0.01, Output: 2}
	return Config{Listen: "127.0.0.1:0", Database: "test.db", Mode: "auto", High: p, Low: low, Policy: Policy{Version: "test-v1", AllowTextSwitch: true, MinSwitchSaving: 0.15, EstimatedOutputTokens: 100}, Classifier: ClassifierConfig{TimeoutMS: 1000, MaxChars: 6000}}
}
func TestReplaceModelPreservesOtherBytes(t *testing.T) {
	raw := []byte("{\n  \"model\" : \"alias\", \"messages\": [{\"role\":\"user\",\"content\":\"<a>&你好\"}], \"future\": { \"b\":2, \"a\": 1 } }")
	out, err := ReplaceModel(raw, "test-flash")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(raw), "\"alias\"", "\"test-flash\"", 1)
	if string(out) != want {
		t.Fatalf("changed other bytes: %s", out)
	}
	for _, invalid := range []string{`{"model":"a","model":"b"}`, `{"model":"a"} {}`, `[]`, `{"model":null}`, `{"messages":[]}`} {
		if _, err := ReplaceModel([]byte(invalid), "x"); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}
func TestUsageSnapshotsAndCost(t *testing.T) {
	var u Usage
	for _, raw := range []string{`{"input_tokens":100,"cache_read_input_tokens":1000,"cache_creation_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":10},"output_tokens":1}`, `{"output_tokens":7}`, `{"output_tokens":9}`} {
		if err := u.Merge(json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if *u.Output != 9 || u.TotalInput() != 1150 {
		t.Fatalf("bad snapshots %+v", u)
	}
	c := testConfig()
	cost, status := u.Cost(c.High, true)
	want := (100*3.0 + 1000*0.1 + 40*3.75 + 10*6 + 9*15) / 1e6
	if status != "usage_calculated" || cost == nil || math.Abs(*cost-want) > 1e-12 {
		t.Fatalf("cost %v %s", cost, status)
	}
	if cost, _ := u.Cost(c.High, false); cost != nil {
		t.Fatal("incomplete response priced")
	}
	u.Write1h = nil
	if cost, status := u.Cost(c.High, true); cost != nil || status != "cache_write_duration_unknown" {
		t.Fatal("guessed cache TTL")
	}
}
func TestUsageUnknownAndExtraCharges(t *testing.T) {
	c := testConfig()
	var u Usage
	u.Merge(json.RawMessage(`{"input_tokens":1,"output_tokens":2}`))
	if cost, _ := u.Cost(c.Low, true); cost != nil {
		t.Fatal("missing usage priced")
	}
	u.Merge(json.RawMessage(`{"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`))
	c.Low.Prices = nil
	if cost, status := u.Cost(c.Low, true); cost != nil || status != "prices_not_configured" {
		t.Fatal("unknown prices treated as zero")
	}
	c = testConfig()
	u.Merge(json.RawMessage(`{"server_tool_use":{"web_search_requests":1}}`))
	if cost, status := u.Cost(c.Low, true); cost != nil || status != "unpriced_server_tools" {
		t.Fatal("ignored tool fees")
	}
	if err := u.Merge(json.RawMessage(`{"output_tokens":-1}`)); err == nil {
		t.Fatal("accepted negative usage")
	}
}
func TestSSEObserverFragmentationAndCompletion(t *testing.T) {
	stream := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"test\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":20,\"cache_creation_input_tokens\":0,\"output_tokens\":0}}}\r\n\r\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":8}}\n\n"
	var o observer
	for _, b := range []byte(stream) {
		o.Feed([]byte{b})
	}
	if o.Complete() {
		t.Fatal("missing stop marked complete")
	}
	o.Feed([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	if !o.Complete() || num(o.Usage.Output) != 8 {
		t.Fatalf("bad complete usage %+v", o)
	}
	o.Feed([]byte("data: {\"type\":\"error\"}\n\n"))
	if o.Complete() {
		t.Fatal("stream error considered success")
	}
}
func TestCacheCostCanKeepHighAndExpirySwitches(t *testing.T) {
	c := testConfig()
	r := Request{Tokens: 100000, MaxTokens: 100, CacheEnabled: true, Prefixes: []string{"same"}}
	b := &branch{Current: "high", Cache: map[string][]cacheEntry{"high": {{"same", 100000, time.Now()}}}}
	d := Decide(c, r, b, "low", "simple_task_rule", 0.8)
	if d.Selected != "high" || *d.HighCost >= *d.LowCost {
		t.Fatalf("failed cache decision %+v", d)
	}
	b.Cache["high"][0].At = time.Now().Add(-time.Hour)
	d = Decide(c, r, b, "low", "simple_task_rule", 0.8)
	if d.Selected != "low" {
		t.Fatalf("expired cache stuck %+v", d)
	}
}
func TestModesAndHistoryProtection(t *testing.T) {
	c := testConfig()
	r := Request{Tokens: 50, MaxTokens: 100, HasHistory: true, Protected: true}
	b := &branch{Current: "low", Cache: map[string][]cacheEntry{}}
	d := Decide(c, r, b, "high", "complexity_rule", 1)
	if d.Selected != "low" || d.Error != "" {
		t.Fatalf("protected history switched %+v", d)
	}
	b.Current = ""
	d = Decide(c, r, b, "high", "complexity_rule", 1)
	if d.Error != "unknown_history_origin" {
		t.Fatalf("unknown origin accepted %+v", d)
	}
	c.Mode = "high"
	if d = Decide(c, r, b, "low", "simple", 1); d.Selected != "high" || d.Error != "" {
		t.Fatalf("fixed mode %+v", d)
	}
	c.Mode = "shadow"
	r = Request{Tokens: 50}
	if d = Decide(c, r, b, "low", "simple", 1); d.Proposed != "low" || d.Selected != "high" {
		t.Fatalf("shadow mode %+v", d)
	}
	c.Mode = "auto"
	c.Low.Prices = nil
	if d = Decide(c, r, b, "low", "simple", 1); d.Selected != "low" || d.CostComparable {
		t.Fatal("unknown package price prevented capability-based routing")
	}
}
func TestConfigurationAndURLs(t *testing.T) {
	c := testConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"https://provider.example/anthropic", "https://provider.example/anthropic/v1"} {
		p := c.Low
		p.BaseURL = base
		if got := p.Endpoint("/v1/messages"); got != "https://provider.example/anthropic/v1/messages" {
			t.Fatal(got)
		}
	}
	c.Listen = "0.0.0.0:8787"
	if c.Validate() == nil {
		t.Fatal("public listener accepted")
	}
	c = testConfig()
	c.High.BaseURL = "http://remote.example"
	if c.Validate() == nil {
		t.Fatal("remote cleartext accepted")
	}
}
func TestRequestFeaturesAndToolResult(t *testing.T) {
	r, err := ParseRequest([]byte(`{"model":"a","messages":[{"role":"user","content":"explain this"},{"role":"assistant","content":[{"type":"thinking","thinking":"opaque"},{"type":"tool_use","id":"x","name":"Bash","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","is_error":true,"content":"failed"}]}],"stream":true,"max_tokens":100}`))
	if err != nil || !r.Protected || !r.HasHistory || r.ToolFailureCount() != 1 || r.Text != "explain this" {
		t.Fatalf("bad features %+v %v", r, err)
	}
	if tier, reason, _ := Rule(r); tier != "high" || reason != "tool_error_signal" {
		t.Fatal(tier, reason)
	}
}

func TestPriceFieldsMustBeExplicit(t *testing.T) {
	var p Prices
	if json.Unmarshal([]byte(`{"version":"v1","currency":"USD","input_per_million":1}`), &p) == nil {
		t.Fatal("missing prices became free")
	}
	if json.Unmarshal([]byte(`{"version":"v1","currency":"USD","input_per_million":1,"cache_read_per_million":0,"cache_write_5m_per_million":0,"cache_write_1h_per_million":0,"output_per_million":1}`), &p) != nil {
		t.Fatal("explicit zeros rejected")
	}
}
func TestCacheFingerprintSurvivesMovingBreakpoint(t *testing.T) {
	first, err := ParseRequest([]byte(`{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"explain this","cache_control":{"type":"ephemeral"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	next, err := ParseRequest([]byte(`{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"explain this"}]},{"role":"assistant","content":"answer"},{"role":"user","content":[{"type":"text","text":"continue","cache_control":{"type":"ephemeral"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if first.Prefixes[0] != next.Prefixes[0] {
		t.Fatal("moving marker broke prefix match")
	}
	changed, _ := ParseRequest([]byte(strings.Replace(string(next.Raw), "explain this", "different content", 1)))
	if changed.Prefixes[0] == first.Prefixes[0] {
		t.Fatal("content change reused prefix")
	}
}
