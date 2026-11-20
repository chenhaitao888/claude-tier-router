package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

const complexThinkingRequest = `{"model":"router-auto","stream":true,"max_tokens":100,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"explain this function"},{"role":"assistant","content":[{"type":"thinking","thinking":"opaque","signature":"original-signature"},{"type":"redacted_thinking","data":"original-data"},{"type":"text","text":"answer"}]},{"role":"user","content":"分析并发架构和死锁路径"}],"future_field":{"ordered":[3,1,2]}}`

func TestComplexTaskOverridesThinkingHistory(t *testing.T) {
	for _, origin := range []string{"low", "high", ""} {
		for _, allowText := range []bool{false, true} {
			c := testConfig()
			c.Policy.AllowTextSwitch = allowText
			r := parseTestRequest(t, complexThinkingRequest)
			b := &branch{Current: origin, Cache: map[string][]cacheEntry{}}
			tier, reason, confidence := Rule(r)
			d := Decide(c, r, b, tier, reason, confidence)
			if d.Selected != "high" || d.Proposed != "high" || d.Error != "" || !d.SwitchAllowed || d.SwitchBlockedReason != "" || !slices.Contains(d.Reasons, "complexity_overrides_thinking") {
				t.Fatalf("origin=%q allow_text=%v: %+v", origin, allowText, d)
			}
			if skip := classifierSkip(c, r, b, tier); skip != "rule_decided" {
				t.Fatalf("classifier skip=%s", skip)
			}
		}
	}
}

func TestThinkingOverrideRetainsOtherLimits(t *testing.T) {
	for _, tc := range []struct {
		name, mode, origin, text, extra, recommend, reason, want, wantError string
		smallContext                                                        bool
	}{
		{name: "simple stays low", text: "解释这个函数", want: "low"},
		{name: "uncertain stays low", text: "继续处理", want: "low"},
		{name: "classifier recommendation alone does not override", text: "继续处理", recommend: "high", reason: "classifier_complex", want: "low"},
		{name: "server state stays low", extra: `,{"type":"server_tool_use","id":"server-tool"}`, want: "low"},
		{name: "unknown server state fails", origin: "unknown", extra: `,{"type":"server_tool_use","id":"server-tool"}`, wantError: "unknown_history_origin"},
		{name: "fixed low stays low", mode: "low", want: "low"},
		{name: "fixed high stays high", mode: "high", want: "high"},
		{name: "high context still bounded", smallContext: true, want: "high", wantError: "selected_model_capability_or_context_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			if tc.mode != "" {
				c.Mode = tc.mode
			}
			if tc.smallContext {
				c.High.ContextTokens = 1
			}
			body := complexThinkingRequest
			if tc.text != "" {
				body = strings.Replace(body, "分析并发架构和死锁路径", tc.text, 1)
			}
			body = strings.Replace(body, `{"type":"text","text":"answer"}`, `{"type":"text","text":"answer"}`+tc.extra, 1)
			r := parseTestRequest(t, body)
			origin := "low"
			if tc.origin == "unknown" {
				origin = ""
			}
			b := &branch{Current: origin, Cache: map[string][]cacheEntry{}}
			tier, reason, confidence := Rule(r)
			if tc.recommend != "" {
				tier, reason = tc.recommend, tc.reason
			}
			d := Decide(c, r, b, tier, reason, confidence)
			if d.Error != tc.wantError || (tc.want != "" && d.Selected != tc.want) {
				t.Fatalf("unexpected decision %+v", d)
			}
			if !tc.smallContext && slices.Contains(d.Reasons, "complexity_overrides_thinking") {
				t.Fatal("override escaped its scope", d)
			}
		})
	}
}

func TestThinkingUpgradeProxyAndDiagnostics(t *testing.T) {
	var highCalls, lowCalls atomic.Int64
	continuation := strings.Replace(complexThinkingRequest, `],"future_field"`, `,{"role":"assistant","content":[{"type":"thinking","thinking":"opaque-high","signature":"high-signature"},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"fixture.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"fixture result"}]}],"future_field"`, 1)
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var incoming struct{ Model string }
		json.Unmarshal(body, &incoming)
		wantBody := complexThinkingRequest
		switch incoming.Model {
		case "test-flash":
			lowCalls.Add(1)
			wantBody = sample
		case "test-opus":
			if highCalls.Add(1) > 1 {
				wantBody = continuation
			}
		default:
			t.Error("unexpected model", incoming.Model)
		}
		want, _ := ReplaceModel([]byte(wantBody), incoming.Model)
		if string(body) != string(want) {
			t.Error("request changed beyond model, or classifier was called")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.ReplaceAll(goodStream, "test-opus", incoming.Model))
	})
	g.Config.Mode = "auto"
	g.Config.Classifier.Enabled = true
	// Establish Flash as the actual source, then send the user's next complex task.
	for _, body := range []string{sample, complexThinkingRequest, continuation} {
		rr := httptest.NewRecorder()
		g.ServeHTTP(rr, request(body))
		if rr.Code != 200 {
			t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
		}
	}
	if highCalls.Load() != 2 || lowCalls.Load() != 1 {
		t.Fatal("wrong dispatch or paid classifier", highCalls.Load(), lowCalls.Load())
	}
	rows, err := s.Records(10)
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	var found bool
	for _, r := range rows {
		if r.Actual == "high" {
			found = true
			if !r.Complete || r.Decision.Selected != "high" || r.Decision.ClassifierSkipReason != "rule_decided" || r.Decision.RoutingVersion != RoutingVersion || !slices.Contains(r.Decision.Reasons, "complexity_overrides_thinking") {
				t.Fatal("upgrade absent from ledger", r)
			}
		}
	}
	if !found {
		t.Fatal("high inference not recorded")
	}
	health, err := s.RoutingHealth(context.Background(), g.RunID)
	if err != nil || health.ClassifierRequests != 0 || len(health.SwitchBlocked) != 0 {
		t.Fatal("diagnostics still report lock/classifier", health, err)
	}
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, httptest.NewRequest("GET", "http://localhost/api/info", nil))
	var info map[string]any
	if json.Unmarshal(rr.Body.Bytes(), &info) != nil || info["routing_version"] != RoutingVersion {
		t.Fatal("routing version missing", rr.Body.String())
	}
}

func TestThinkingUpgradeDoesNotFallbackOnUpstreamRejection(t *testing.T) {
	var calls atomic.Int64
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		want, _ := ReplaceModel([]byte(complexThinkingRequest), "test-opus")
		if string(body) != string(want) {
			t.Error("signature stripped or dispatched to wrong model")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"unsupported thinking history"}}`)
	})
	g.Config.Mode = "auto"
	g.Config.Classifier.Enabled = true
	b, release := g.Engine.Acquire("session\x00\x00main")
	b.Current = "low"
	release()
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, request(complexThinkingRequest))
	if rr.Code != 400 || calls.Load() != 1 || !strings.Contains(rr.Body.String(), "unsupported thinking history") {
		t.Fatal("rejection hidden or request retried", rr.Code, rr.Body.String(), calls.Load())
	}
	rows, err := s.Records(10)
	if err != nil || len(rows) != 1 || rows[0].Actual != "high" || rows[0].HTTPStatus != 400 || rows[0].Complete {
		t.Fatal("rejection not recorded", rows, err)
	}
	b, release = g.Engine.Acquire("session\x00\x00main")
	defer release()
	if b.Current != "low" {
		t.Fatal("failed upgrade changed history origin")
	}
}
