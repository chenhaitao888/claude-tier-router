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

func TestContextLimitUsesRecoverableAnthropicError(t *testing.T) {
	var calls atomic.Int64
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.ReplaceAll(goodStream, "test-opus", "test-flash"))
	})
	g.Config.Mode = "low"
	g.Config.Low.ContextTokens = 1000
	body := strings.Replace(sample, "explain this function", strings.Repeat("private-fixture ", 200), 1)
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, request(body))
	var response struct {
		Error       struct{ Type, Message string }
		RouterError string         `json:"router_error"`
		Budget      *ContextBudget `json:"context_budget"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 400 || response.Error.Type != "invalid_request_error" || !strings.Contains(response.Error.Message, "prompt is too long") || !strings.Contains(response.Error.Message, "local estimate") || response.RouterError != "selected_model_context_limit" {
		t.Fatal("client cannot distinguish context failure", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "private-fixture") || calls.Load() != 0 {
		t.Fatal("rejected input exposed or forwarded")
	}
	b := response.Budget
	if b == nil || b.EstimatedInput <= 900 || b.ReservedOutput != 100 || b.EstimatedTotal != b.EstimatedInput+100 || b.LowLimit != 1000 || b.HighLimit != g.Config.High.ContextTokens {
		t.Fatal("missing or incorrect context budget", b)
	}
	rows, err := s.Records(10)
	if err != nil || len(rows) != 1 || rows[0].HTTPStatus != 400 || rows[0].Actual != "" || rows[0].CostStatus != "not_dispatched" || rows[0].Decision.ContextBudget == nil || rr.Header().Get("X-Router-Request-Id") != rows[0].ID {
		t.Fatal("context rejection lost from ledger", rows, err)
	}
	// A subsequent compacted request can proceed; the router did not issue a
	// paid retry or mutate the caller's message history on its own.
	rr = httptest.NewRecorder()
	g.ServeHTTP(rr, request(sample))
	if rr.Code != 200 || calls.Load() != 1 {
		t.Fatal("smaller request could not recover", rr.Code, calls.Load())
	}
}

func TestNonContextErrorsCannotTriggerPromptCompaction(t *testing.T) {
	budget := &ContextBudget{EstimatedInput: 10, ReservedOutput: 1000, EstimatedTotal: 1010, HighLimit: 1000, LowLimit: 1000}
	for _, code := range []string{"selected_model_content_unsupported", "unknown_history_origin", "output_budget_exceeds_context"} {
		status, kind, message := decisionAPIError(Decision{Selected: "low", Error: code, ContextBudget: budget})
		if strings.Contains(message, "prompt is too long") {
			t.Fatal("non-context error would cause unnecessary compression", code, message)
		}
		if code == "output_budget_exceeds_context" {
			if status != 400 || kind != "invalid_request_error" || !strings.Contains(message, "max_tokens") {
				t.Fatal(status, kind, message)
			}
		} else if status != 409 || kind != "api_error" || message != code {
			t.Fatal(status, kind, message)
		}
	}
}

func TestContextBudgetExposesLocalEstimateAndOutputReservation(t *testing.T) {
	c := testConfig()
	c.Mode = "high"
	c.High.ContextTokens = 1000
	b := &branch{}
	for _, tc := range []struct {
		input, output int
		want          string
	}{
		{800, 100, ""},
		{901, 100, "selected_model_context_limit"},
		{10, 1000, "output_budget_exceeds_context"},
	} {
		d := Decide(c, Request{Tokens: tc.input, MaxTokens: tc.output}, b, "high", "complexity_rule", 1)
		if d.Error != tc.want || d.ContextBudget.EstimatedInput != tc.input || d.ContextBudget.ReservedOutput != tc.output || d.ContextBudget.HighLimit != 1000 {
			t.Fatal("wrong context diagnosis", d)
		}
	}
}
