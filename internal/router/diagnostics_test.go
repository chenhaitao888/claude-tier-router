package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRoutingDiagnosticsIsolateRunsAndKeepHistory(t *testing.T) {
	g, s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("diagnostics called a model") })
	g.Config.Mode = "auto"
	g.Config.Classifier.Enabled = true
	g.Config.Policy.AllowTextSwitch = true
	other := NewGateway(g.Config, s)
	if g.RunID == "" || g.RunID == other.RunID {
		t.Fatal("gateway run IDs are not distinct")
	}
	when := time.Now().Add(time.Hour)
	old := []Record{
		{ID: "legacy", Kind: "inference", Decision: Decision{ClassifierSkipReason: "classifier_disabled"}},
		{ID: "old-run", RunID: "past", Kind: "inference", Decision: Decision{ClassifierSkipReason: "text_history_switch_disabled", SwitchBlockedReason: "text_history_switch_disabled"}},
		{ID: "old-classifier", RunID: "past", Kind: "classifier", ClassifierDiagnostic: &ClassifierDiagnostic{Status: "uncertain"}},
		{ID: "other-run", RunID: other.RunID, Kind: "inference", Decision: Decision{ClassifierSkipReason: "classifier_disabled"}},
		{ID: "current", RunID: g.RunID, Kind: "inference", Actual: "low", Complete: true, Usage: countedUsage(10, 20, 0, 5), Decision: Decision{ClassifierSkipReason: "rule_decided", CachePredictionVersion: "block-prefix-v4"}},
		{ID: "current-classifier", RunID: g.RunID, Kind: "classifier", ClassifierDiagnostic: &ClassifierDiagnostic{Status: "accepted"}},
	}
	for _, r := range old {
		r.Started = when
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.Records(100)
	if err != nil {
		t.Fatal(err)
	}
	var current, all RoutingDiagnostics
	getPage(t, g, "/api/routing-health", &current)
	if current.Scope != "current" || current.CurrentRunID != g.RunID || !current.CurrentConfig.ClassifierEnabled || !current.CurrentConfig.AllowTextSwitch || current.CurrentConfig.Mode != "auto" {
		t.Fatal("active configuration incorrect", current)
	}
	if current.Health.InferenceRequests != 1 || current.Health.ClassifierRequests != 1 || current.Health.ClassifierOutcomes["accepted"] != 1 || current.Health.ClassifierSkipped["rule_decided"] != 1 || len(current.Health.SwitchBlocked) != 0 {
		t.Fatal("historical decisions leaked into current run", current.Health)
	}
	getPage(t, g, "/api/routing-health?scope=all", &all)
	if all.Scope != "all" || all.Health.InferenceRequests != 4 || all.Health.ClassifierRequests != 2 || all.Health.ClassifierSkipped["classifier_disabled"] != 2 || all.Health.SwitchBlocked["text_history_switch_disabled"] != 1 || all.Health.ClassifierOutcomes["uncertain"] != 1 {
		t.Fatal("historical diagnostics lost", all.Health)
	}
	current = RoutingDiagnostics{}
	getPage(t, other, "/api/routing-health?scope=current", &current)
	if current.Health.InferenceRequests != 1 || current.Health.ClassifierRequests != 0 || current.Health.ClassifierSkipped["classifier_disabled"] != 1 {
		t.Fatal("concurrent gateway runs mixed")
	}
	restarted := NewGateway(g.Config, s)
	current = RoutingDiagnostics{}
	getPage(t, restarted, "/api/routing-health?scope=current", &current)
	if current.Health.InferenceRequests != 0 || current.Health.ClassifierRequests != 0 || len(current.Health.ClassifierSkipped) != 0 {
		t.Fatal("restart didn't isolate diagnostics")
	}
	var summary Summary
	getPage(t, restarted, "/api/summary", &summary)
	if summary.Requests != 4 || summary.ClassifierRequests != 2 || !reflect.DeepEqual(summary.RoutingHealth, all.Health) {
		t.Fatal("summary no longer covers full ledger")
	}
	after, err := s.Records(100)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("view mutated historical ledger")
	}
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, httptest.NewRequest("GET", "http://localhost/api/routing-health?scope=invalid", nil))
	if rr.Code != 400 {
		t.Fatal("invalid scope accepted")
	}
	r := httptest.NewRequest("GET", "http://localhost/api/routing-health", nil)
	r.Header.Set("Origin", "https://untrusted.example")
	rr = httptest.NewRecorder()
	g.ServeHTTP(rr, r)
	if rr.Code != 403 {
		t.Fatal("cross-origin diagnostic access accepted")
	}
}

func TestRoutingCacheCoverageExplainsMissingData(t *testing.T) {
	h := newRoutingHealth()
	partial := countedUsage(10, 0, 0, 5)
	partial.Read, partial.Write = nil, nil
	for _, r := range []Record{
		{Kind: "inference", Actual: "high", Complete: true, Usage: countedUsage(1, 0, 0, 1)},
		{Kind: "inference", Actual: "high", Complete: true, Usage: partial},
		{Kind: "inference", Actual: "high", Complete: false},
		{Kind: "inference", HTTPStatus: 409},
		{Kind: "inference", Actual: "high", CostStatus: "in_progress"},
		{Kind: "classifier", ClassifierDiagnostic: &ClassifierDiagnostic{Status: "invalid", Validation: "invalid_json"}},
	} {
		h.add(r)
	}
	if h.InferenceRequests != 5 || h.ClassifierRequests != 1 || h.CachePredictions["legacy"].Requests != 1 || len(h.CacheExcluded) != 4 {
		t.Fatal("cache eligibility incorrect", h)
	}
	for _, key := range []string{"missing_usage", "incomplete_response", "not_dispatched", "in_progress"} {
		if h.CacheExcluded[key] != 1 {
			t.Fatal("missing exclusion reason", key, h)
		}
	}
}

func TestGatewayRecordsRunForInferenceAndClassifier(t *testing.T) {
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !body.Stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"{\"tier\":\"low\",\"confidence\":0.9,\"reason\":\"simple\"}"}],"usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":20}}`)
		} else {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, strings.ReplaceAll(goodStream, "test-opus", "test-flash"))
		}
	})
	g.Config.Mode = "auto"
	g.Config.Classifier.Enabled = true
	g.Config.Policy.AllowTextSwitch = true
	g.Config.High.Prices = nil
	g.Config.Low.Prices = nil
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, request(strings.ReplaceAll(sample, "explain this function", "修复一个失败的单元测试")))
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	rows, err := s.Records(10)
	if err != nil || len(rows) != 2 {
		t.Fatal("expected inference and classifier", err, len(rows))
	}
	for _, row := range rows {
		if row.RunID != g.RunID {
			t.Fatal("missing run association", row.Kind)
		}
	}
	var d RoutingDiagnostics
	getPage(t, g, "/api/routing-health", &d)
	if d.Health.InferenceRequests != 1 || d.Health.ClassifierRequests != 1 || d.Health.ClassifierOutcomes["accepted"] != 1 {
		t.Fatal("current-run aggregation lost live classification", d.Health)
	}
}
