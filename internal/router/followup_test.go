package router

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

const lookupReport = `{"status":"completed","evidence":["table name at line 9"],"checks":["mapping verified"],"open_questions":[]}`
const returnedLookup = `{"model":"router-coordinator","max_tokens":100,"messages":[{"role":"user","content":"分析架构"},{"role":"assistant","content":[{"type":"tool_use","name":"Agent","id":"call1","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"Router-Next: format_results"}]}]}`

func agentReceipt(t *testing.T, g *Gateway, agent, role, call, plan string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"session_id": "session", "prompt_id": "task", "hook_event_name": "PostToolUse", "tool_name": "Agent", "tool_use_id": call, "tool_input": map[string]any{"subagent_type": "router-" + role, "prompt": "Router-Next: " + plan + "\nExecute the specified contract"}, "tool_response": map[string]any{"status": "completed", "agentId": agent}})
	if err != nil {
		t.Fatal(err)
	}
	coopSend(t, g, httptest.NewRequest("POST", "http://localhost/api/cooperation/events", strings.NewReader(string(raw))), 200)
}

func TestCoordinatorDeliveryRouting(t *testing.T) {
	for _, tc := range []struct{ name, plan, report, body, setting, followup, recommend, actual string }{
		{"return table", formatResults, lookupReport, returnedLookup, "", formatResults, "low", "low"},
		{"same facts need analysis", "judgment", lookupReport, returnedLookup, "", "judgment", "high", "high"},
		{"conflicting evidence", formatResults, strings.Replace(lookupReport, `"checks"`, `"conflicts":["two table names"],"checks"`, 1), returnedLookup, "", "unresolved_report", "high", "high"},
		{"unanswered question", formatResults, strings.Replace(lookupReport, "[]", `["which source?"]`, 1), returnedLookup, "", "unresolved_report", "high", "high"},
		{"parallel running", formatResults, lookupReport, returnedLookup, "pending", "running", "high", "high"},
		{"missing sibling hook", formatResults, lookupReport, returnedLookup, "missing", "missing_report", "high", "high"},
		{"wrong receipt", formatResults, lookupReport, strings.ReplaceAll(returnedLookup, "call1", "call2"), "", "unconfirmed_step", "high", "high"},
		{"failed tool", formatResults, lookupReport, strings.Replace(returnedLookup, `"tool_use_id"`, `"is_error":true,"tool_use_id"`, 1), "", "tool_error", "high", "high"},
		{"changed user instructions", formatResults, lookupReport, strings.Replace(returnedLookup, `"type":"tool_result"`, `"type":"text","text":"Compare the tables"}, {"type":"tool_result"`, 1), "", "unconfirmed_step", "high", "high"},
		{"unexpected draft", writeApproved, lookupReport, returnedLookup, "", "unexpected_result", "high", "high"},
		{"thinking history stays high", formatResults, lookupReport, strings.Replace(returnedLookup, `"type":"tool_use"`, `"type":"thinking","thinking":"fixture","signature":"abc"},{"type":"tool_use"`, 1), "", formatResults, "low", "high"},
		{"text switch disabled", formatResults, lookupReport, returnedLookup, "noSwitch", formatResults, "low", "high"},
		{"cost veto", formatResults, lookupReport, returnedLookup, "cheaperHigh", formatResults, "low", "high"},
		{"no classifier for blocked handoff", formatResults, `{"status":"blocked"}`, returnedLookup, "", "blocked", "high", "high"},
		{"receipt missing", formatResults, lookupReport, returnedLookup, "noReceipt", "unconfirmed_step", "high", "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, s, calls := lookupFixture(t)
			coopSend(t, g, coopRequest(CoordinatorModel, "分析并发架构", ""), 200)
			coopHook(t, g, "session", "task", "w", "executor", "SubagentStart", "")
			coopHook(t, g, "session", "task", "w", "executor", "SubagentStop", tc.report)
			if tc.setting != "noReceipt" {
				agentReceipt(t, g, "w", "executor", "call1", tc.plan)
			}
			switch tc.setting {
			case "pending":
				coopHook(t, g, "session", "task", "other", "reviewer", "SubagentStart", "")
			case "missing":
				coopSend(t, g, coopRequest(ExecutorModel, "lookup", "missing-hook-worker"), 200)
			case "noSwitch":
				g.Config.Policy.AllowTextSwitch = false
			case "cheaperHigh":
				g.Config.High.Prices = &Prices{Version: "test", Currency: "USD"}
				g.Config.Low.Prices = &Prices{Version: "test", Currency: "USD", Input: 1, Output: 1}
			}
			before := calls.Load()
			coopSend(t, g, request(tc.body), 200)
			rows, err := s.Records(1)
			if err != nil || len(rows) != 1 {
				t.Fatal(err, rows)
			}
			r := rows[0]
			if calls.Load() != before+1 || r.Actual != tc.actual || r.Decision.Recommended != tc.recommend || r.Cooperation.Followup != tc.followup {
				t.Fatalf("followup=%s actual=%s decision=%+v", r.Cooperation.Followup, r.Actual, r.Decision)
			}
		})
	}
}

func TestFinalReviewCanResolvePreviousBlocker(t *testing.T) {
	for _, whole := range []bool{false, true} {
		g, s, _ := lookupFixture(t)
		coopSend(t, g, coopRequest(CoordinatorModel, "分析架构", ""), 200)
		coopHook(t, g, "session", "task", "w", "executor", "SubagentStart", "")
		coopHook(t, g, "session", "task", "w", "executor", "SubagentStop", `{"status":"needs_design"}`)
		coopHook(t, g, "session", "task", "r", "reviewer", "SubagentStart", "")
		report, _ := json.Marshal(map[string]any{"status": "passed", "checks": []string{"verified corrected query against the original contract"}, "findings": []string{}, "limitations": []string{}, "whole_task_checked": whole})
		coopHook(t, g, "session", "task", "r", "reviewer", "SubagentStop", string(report))
		agentReceipt(t, g, "r", "reviewer", "call1", formatResults)
		coopSend(t, g, request(returnedLookup), 200)
		rows, _ := s.Records(1)
		if (rows[0].Actual == "low") != whole {
			t.Fatal(whole, rows[0].Decision, rows[0].Cooperation)
		}
	}
}

func TestParallelAgentReturnsRequireCompleteMatchingPlans(t *testing.T) {
	for _, secondPlan := range []string{formatResults, "judgment"} {
		g, s, _ := lookupFixture(t)
		for agent, plan := range map[string]string{"a": formatResults, "b": secondPlan} {
			coopHook(t, g, "session", "task", agent, "executor", "SubagentStart", "")
			coopHook(t, g, "session", "task", agent, "executor", "SubagentStop", lookupReport)
			agentReceipt(t, g, agent, "executor", "call-"+agent, plan)
		}
		body := `{"model":"router-coordinator","max_tokens":100,"messages":[{"role":"user","content":"分析架构"},{"role":"assistant","content":[{"type":"tool_use","id":"call-a","name":"Agent"},{"type":"tool_use","id":"call-b","name":"Agent"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-b","content":"b"},{"type":"tool_result","tool_use_id":"call-a","content":"a"}]}]}`
		coopSend(t, g, request(body), 200)
		rows, _ := s.Records(1)
		if (rows[0].Actual == "low") != (secondPlan == formatResults) {
			t.Fatal(secondPlan, rows[0].Decision)
		}
	}
}
