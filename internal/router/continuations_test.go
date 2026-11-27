package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func notificationFor(call string) string {
	return "<system-reminder>\n[SYSTEM NOTIFICATION - NOT USER INPUT]\n" +
		"This is an automated background-task event, NOT a message from the user.\n" +
		"<task-notification><task-id>fixture</task-id><tool-use-id>" + call +
		"</tool-use-id><status>completed</status><summary>Wait finished</summary></task-notification>\n</system-reminder>"
}

func TestBackgroundContinuationSharesQuestionAndBatchReview(t *testing.T) {
	g, s, _ := lookupFixture(t)
	g.Config.StoreTaskSummaries = true
	const queryTool = "mcp__warehouse__ai_query_submit"
	const task = "列出字段"
	first := batchEvent{Session: "session", Event: "PreToolUse", Tool: queryTool, Call: "query-first", Prompt: "original", Input: json.RawMessage(`{"sql":"select 1"}`)}
	emitBatch(t, g, "original", CoordinatorModel, task, true, true, first, batchEvent{Tool: "Bash", Call: "wait-first"})
	if batchHook(t, g, first) != "pass" {
		t.Fatal("initial Flash probe was paused")
	}

	next := first
	next.Call, next.Prompt, next.Input = "query-next", "resume", json.RawMessage(`{"sql":"select 2"}`)
	resume := request(fmt.Sprintf(`{"model":%q,"max_tokens":100,"tools":[{"name":"Agent"}],"messages":[
		{"role":"user","content":%q},{"role":"assistant","content":[{"type":"thinking","thinking":"prior Flash work","signature":"fixture"}]},
		{"role":"user","content":[{"type":"text","text":%q}]}]}`,
		CoordinatorModel, task, notificationFor("wait-first")))
	resume.Header.Set("x-claude-code-prompt-id", "resume")
	emitBatchRequest(t, g, resume, true, next, batchEvent{Tool: "Bash", Call: "wait-second"})
	if batchHook(t, g, next) != "review_hint" || batchHook(t, g, next) != "review_hint" {
		t.Fatal("a new batch in a continuation must retain the original probe and request a review")
	}

	later := next
	later.Call = "query-after-review"
	resume = request(fmt.Sprintf(`{"model":%q,"max_tokens":100,"tools":[{"name":"Agent"}],"messages":[
		{"role":"user","content":%q},{"role":"assistant","content":[{"type":"thinking","thinking":"prior Flash work","signature":"fixture"}]},
		{"role":"user","content":%q}]}`, CoordinatorModel, task, notificationFor("wait-first")))
	resume.Header.Set("x-claude-code-prompt-id", "resume")
	emitBatchRequest(t, g, resume, true, later)
	if batchHook(t, g, later) != "pass" {
		t.Fatal("the checkpoint must allow the parent to proceed on the next reply")
	}

	coopHook(t, g, "session", "resume", "review-agent", "reviewer", "SubagentStart", "")
	review := coopRequest(ReviewerModel, "Review given date and join definitions", "review-agent")
	review.Header.Set("x-claude-code-prompt-id", "resume")
	coopSend(t, g, review, 200)
	coopHook(t, g, "session", "resume", "review-agent", "reviewer", "SubagentStop",
		`{"status":"changes_required","checks":["checked partitions"],"findings":["missing day"],"limitations":[]}`)

	nextResume := request(fmt.Sprintf(`{"model":%q,"max_tokens":100,"messages":[
		{"role":"user","content":%q},{"role":"assistant","content":"waiting"},
		{"role":"user","content":%q},{"role":"assistant","content":"waiting again"},
		{"role":"user","content":%q}]}`, CoordinatorModel, task, notificationFor("unbound-older"), notificationFor("wait-second")))
	nextResume.Header.Set("x-claude-code-prompt-id", "resume-again")
	emitBatchRequest(t, g, nextResume, true)

	tasks, err := s.Tasks(g.Config, 20)
	if err != nil || tasks.Total != 1 {
		t.Fatal(tasks, err)
	}
	result := tasks.Tasks[0]
	if result.Prompt != "original" || result.ContinuationCount != 2 || result.Label == nil || result.Label.Title != task ||
		result.Requests != 5 || result.High != 1 || result.Low != 4 || len(result.CooperationRuns) != 1 ||
		len(result.BatchChecks) != 1 || result.BatchChecks[0].Tier != "low" || result.BatchChecks[0].State != "review_hint" || !result.BatchChecks[0].Continued {
		t.Fatal(result)
	}
	page, err := s.RecordsPage(context.Background(), g.Config, Pagination{Page: 1, PageSize: 20}, "session", "original")
	if err != nil || page.Total != 5 {
		t.Fatal(page, err)
	}
	// Existing API callers can open or evaluate the question using any child ID.
	aliasPage, err := s.RecordsPage(context.Background(), g.Config, Pagination{Page: 1, PageSize: 10}, "session", "resume-again")
	if err != nil || aliasPage.Total != page.Total {
		t.Fatal(aliasPage, err)
	}
	aliasRows, err := s.TaskRecords("session", "resume", 10)
	if err != nil || len(aliasRows) != page.Total {
		t.Fatal(aliasRows, err)
	}
	post := func(path, body string) {
		t.Helper()
		coopSend(t, g, httptest.NewRequest("POST", "http://localhost"+path, strings.NewReader(body)), 200)
	}
	post("/api/feedback", `{"session_id":"session","prompt_id":"resume","outcome":"failed"}`)
	post("/api/feedback", `{"session_id":"session","prompt_id":"resume-again","outcome":"passed"}`)
	post("/api/task-title", `{"session_id":"session","prompt_id":"resume","title":"确认的原问题"}`)
	var summary Summary
	getPage(t, g, "/api/summary", &summary)
	var taskPage TaskPage
	getPage(t, g, "/api/tasks", &taskPage)
	if summary.Tasks != 1 || summary.Passed != 1 || summary.Failed != 0 || summary.Unevaluated != 0 || summary.Requests != 5 ||
		taskPage.Pagination.Total != 1 || taskPage.TaskList.Total != 1 || taskPage.Tasks[0].Label.Title != "确认的原问题" || taskPage.Tasks[0].Feedback.Outcome != "passed" {
		t.Fatal(summary, taskPage)
	}
	var feedback []FeedbackView
	getPage(t, g, "/api/feedback", &feedback)
	if len(feedback) != 1 || feedback[0].Prompt != "original" || feedback[0].TaskLabel.Title != "确认的原问题" {
		t.Fatal(feedback)
	}
	var protectedFlash int
	var highReview bool
	for _, r := range page.Records {
		if r.RootPrompt != "original" || r.TaskLabel == nil || r.TaskLabel.Prompt != "original" {
			t.Fatal("linked detail should use the original question label", r)
		}
		switch {
		case r.Prompt == "resume" && r.Cooperation.Role == "coordinator" && r.Actual == "low" &&
			r.Decision.SwitchBlockedReason == "protected_history_requires_same_model":
			protectedFlash++
		case r.Cooperation.Role == "reviewer" && r.Actual == "high":
			highReview = true
		}
	}
	if protectedFlash != 2 || !highReview {
		t.Fatal("Flash thinking and Opus review must use separate branches", page)
	}
}

func TestBackgroundContinuationIsolation(t *testing.T) {
	g, s, _ := lookupFixture(t)
	emitBatch(t, g, "one", "router-auto", "列出字段", false, true, batchEvent{Tool: "Bash", Call: "wait-one"})
	emitBatch(t, g, "two", "router-auto", "列出字段", false, true, batchEvent{Tool: "Bash", Call: "wait-two"})

	tests := []struct{ name, text, session string }{
		{"unknown", notificationFor("unknown"), "session"},
		{"wrong-session", notificationFor("wait-one"), "other-session"},
		{"mixed-roots", notificationFor("wait-one") + notificationFor("wait-two"), "session"},
		{"human-input", notificationFor("wait-one") + "\n列出字段", "session"},
		{"typed-markup", "<task-notification><tool-use-id>wait-one</tool-use-id></task-notification>", "session"},
	}
	for _, tc := range tests {
		r := request(fmt.Sprintf(`{"model":"router-auto","max_tokens":100,"messages":[
			{"role":"user","content":"列出字段"},{"role":"assistant","content":"waiting"},
			{"role":"user","content":%q}]}`, tc.text))
		r.Header.Set("x-claude-code-prompt-id", tc.name)
		r.Header.Set("x-claude-code-session-id", tc.session)
		emitBatchRequest(t, g, r, true)
	}
	var links int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM task_continuations").Scan(&links); err != nil || links != 0 {
		t.Fatal("independent and ambiguous events must remain separate", links, err)
	}
	tasks, err := s.Tasks(g.Config, 20)
	if err != nil || tasks.Total != 7 {
		t.Fatal(tasks, err)
	}
}

func TestOnlyCurrentNotificationCanStartContinuation(t *testing.T) {
	g, s, _ := lookupFixture(t)
	g.Config.StoreTaskSummaries = true
	emitBatch(t, g, "parent", CoordinatorModel, "列出字段", true, true, batchEvent{Tool: "Bash", Call: "wait"})
	n := notificationFor("wait")
	history := []any{map[string]any{"role": "user", "content": "列出字段"}, map[string]any{"role": "assistant", "content": "waiting"}, map[string]any{"role": "user", "content": n}, map[string]any{"role": "assistant", "content": "finished"}}
	image := map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": "AA=="}}
	for _, tc := range []struct {
		name        string
		turns       []any
		root, title string
	}{
		{"photo", []any{[]any{image}}, "photo", ""},
		{"photo-plus-notice", []any{[]any{map[string]any{"type": "text", "text": n}, image}}, "photo-plus-notice", ""},
		{"attachment-then-notice", []any{[]any{map[string]any{"type": "document"}}, n}, "attachment-then-notice", ""},
		{"human-then-notice", []any{"新问题", n}, "human-then-notice", "新问题"},
		{"malformed", []any{n + strings.TrimSuffix(n, "</system-reminder>")}, "malformed", ""},
		{"missing-id", []any{n + strings.Replace(n, "<tool-use-id>wait</tool-use-id>", "", 1)}, "missing-id", ""},
		{"unknown-id", []any{notificationFor("unseen")}, "unknown-id", ""},
		{"unmarked-event", []any{n + strings.Replace(n, backgroundNotificationMarker, "", 1)}, "unmarked-event", ""},
		{"multiple-only", []any{n, n}, "parent", "列出字段"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := append([]any{}, history...)
			for _, content := range tc.turns {
				messages = append(messages, map[string]any{"role": "user", "content": content})
			}
			raw, err := json.Marshal(map[string]any{"model": CoordinatorModel, "max_tokens": 50, "messages": messages})
			if err != nil {
				t.Fatal(err)
			}
			r := request(string(raw))
			r.Header.Set("x-claude-code-prompt-id", tc.name)
			emitBatchRequest(t, g, r, true)
			page, err := s.RecordsPage(context.Background(), g.Config, Pagination{Page: 1, PageSize: 20}, "session", tc.name)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range page.Records {
				if row.Prompt != tc.name || row.Kind != "inference" {
					continue
				}
				title := ""
				if row.TaskLabel != nil {
					title = row.TaskLabel.Title
				}
				if row.RootPrompt != tc.root || title != tc.title {
					t.Fatalf("root=%q title=%q", row.RootPrompt, title)
				}
				return
			}
			t.Fatal("missing original request identity")
		})
	}
}

func TestBatchHintKeepsMechanicalWorkLowAndComplexWorkEligible(t *testing.T) {
	for _, tc := range []struct{ name, content, tier string }{
		{"mechanical", `{"type":"tool_result","tool_use_id":"next","content":"ok"}`, "low"},
		{"complex", `{"type":"text","text":"分析并发架构"}`, "high"},
		{"image", `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}`, "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, s, _ := lookupFixture(t)
			const tool = "mcp__warehouse__query_submit"
			first := batchEvent{Session: "session", Prompt: "task", Tool: tool, Call: "first", Event: "PreToolUse", Input: json.RawMessage(`{"page":1}`)}
			next := first
			next.Call = "next"
			next.Input = json.RawMessage(`{"page":2}`)
			sibling := next
			sibling.Call = "parallel"
			emitBatch(t, g, "task", CoordinatorModel, "列出字段", true, true, first, next, sibling)
			if batchHook(t, g, first) != "pass" || batchHook(t, g, next) != "review_hint" || batchHook(t, g, sibling) != "pass" {
				t.Fatal("unexpected low-tier batch action")
			}
			thinking := ""
			if tc.name != "mechanical" {
				thinking = `{"type":"thinking","thinking":"prior work","signature":"fixture"},`
			}
			r := request(fmt.Sprintf(`{"model":%q,"max_tokens":100,"messages":[{"role":"user","content":"列出字段"},{"role":"assistant","content":[%s{"type":"tool_use","id":"next","name":%q,"input":{"page":2}}]},{"role":"user","content":[%s]}]}`, CoordinatorModel, thinking, tool, tc.content))
			emitBatchRequest(t, g, r, true)
			rows, err := s.Records(1)
			if err != nil || len(rows) != 1 || rows[0].Actual != tc.tier || rows[0].Error != "" {
				t.Fatal(rows, err)
			}
		})
	}
}

// Exercise the shape of the reported workload with synthetic IDs, no providers.
func TestRepeatedWakeupsCountRequestsOnce(t *testing.T) {
	g, s, _ := lookupFixture(t)
	const root = "question"
	var requests, submissions, hints int
	lastCall := ""
	for turn := 0; turn < 17; turn++ {
		prompt := root
		if turn > 0 {
			prompt = fmt.Sprintf("wake-%d", turn)
		}
		requestCount := 10
		if turn < 11 {
			requestCount++
		}
		queryCount := 1
		if turn >= 1 && turn <= 5 {
			queryCount++
		}
		for step := 0; step < requestCount; step++ {
			requests++
			available := true
			r := Record{ID: fmt.Sprintf("model-%d", requests), Session: "session", Prompt: prompt, Kind: "inference", Class: "main", Actual: "low", Cooperation: &CooperationTrace{Role: "coordinator", Stage: "direct", AgentToolAvailable: &available}}
			var calls []string
			if step == 0 && turn > 0 {
				calls = []string{lastCall}
			}
			if err := s.startRequest(r, calls); err != nil {
				t.Fatal(err)
			}
			if step < queryCount {
				submissions++
				e := batchEvent{Session: r.Session, Prompt: prompt, Event: "PreToolUse", Call: fmt.Sprintf("submit-%d", submissions), Tool: "mcp__warehouse__query_submit", Input: json.RawMessage(fmt.Sprintf(`{"partition":%d}`, submissions))}
				if err := s.bindDelegationTool(r, e.Call, e.Tool); err != nil {
					t.Fatal(err)
				}
				switch result := batchHook(t, g, e); result {
				case "review_hint":
					hints++
				case "pass":
				default:
					t.Fatalf("interrupted at %d: %s", submissions, result)
				}
			}
			if step == requestCount-1 {
				lastCall = "wait-" + r.ID
				if err := s.bindDelegationTool(r, lastCall, "Monitor"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	tasks, err := s.Tasks(g.Config, 20)
	if err != nil || len(tasks.Tasks) != 1 {
		t.Fatal(tasks, err)
	}
	summary, err := s.Summary()
	if err != nil || summary.Tasks != 1 || summary.Low != 181 || summary.Unevaluated != 1 || tasks.Tasks[0].Low != 181 || tasks.Tasks[0].ContinuationCount != 16 || submissions != 22 || hints != 1 {
		t.Fatal(summary, tasks, submissions, hints, err)
	}
}

func TestContinuationSkipsAmbiguousSourceAndKeepsExistingQuestions(t *testing.T) {
	g, s, _ := lookupFixture(t)
	emitBatch(t, g, "first", "router-auto", "列出字段", false, true, batchEvent{Tool: "Bash", Call: "dup"}, batchEvent{Tool: "Bash", Call: "wait"})
	emitBatch(t, g, "second", "router-auto", "列出字段", false, true, batchEvent{Tool: "Agent", Call: "dup"})
	for _, tc := range []struct{ prompt, call, root string }{{"ambiguous", "dup", "ambiguous"}, {"second", "wait", "second"}, {"early-aux", "wait", "first"}} {
		if tc.prompt == "early-aux" {
			if err := s.Save(Record{ID: "aux", Session: "session", Prompt: tc.prompt, Kind: "inference", Class: "auxiliary"}); err != nil {
				t.Fatal(err)
			}
		}
		r := Record{ID: "new-" + tc.prompt, Session: "session", Prompt: tc.prompt, Kind: "inference", Class: "main"}
		if err := s.startRequest(r, []string{tc.call}); err != nil {
			t.Fatal(err)
		}
		root, err := s.existingTaskPrompt("session", tc.prompt)
		if err != nil || root != tc.root {
			t.Fatal(tc, root, err)
		}
	}
}

func TestNativeSystemNoticeAndToolResults(t *testing.T) {
	g, s, _ := lookupFixture(t)
	g.Config.StoreTaskSummaries = true
	emitBatch(t, g, "parent", "router-auto", "列出字段", false, true, batchEvent{Tool: "Bash", Call: "bg"}, batchEvent{Tool: "Bash", Call: "fg"})
	emitBatch(t, g, "other", "router-auto", "列出字段", false, true, batchEvent{Tool: "Bash", Call: "other-call"})
	type message struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	block := func(kind, key, value string) []map[string]string {
		return []map[string]string{{"type": kind, key: value}}
	}
	wire := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(notificationFor("bg"), "<system-reminder>"), "</system-reminder>"))
	notice := message{"system", block("text", "text", wire)}
	meta := message{"system", "# Environment"}
	for _, tc := range []struct {
		name        string
		tail        []message
		root, title string
	}{
		{"native", []message{notice}, "parent", "列出字段"},
		{"known-tool", []message{{"user", block("tool_result", "tool_use_id", "fg")}, notice, meta}, "parent", "列出字段"},
		{"tool-alone", []message{{"user", block("tool_result", "tool_use_id", "fg")}, meta}, "tool-alone", ""},
		{"cross-tool", []message{{"user", block("tool_result", "tool_use_id", "other-call")}, notice}, "cross-tool", ""},
		{"human-with-system", []message{{"user", "新问题"}, notice, meta}, "human-with-system", "新问题"},
		{"unwrapped-user", []message{{"user", wire}}, "unwrapped-user", wire},
		{"system-then-attachment", []message{notice, {"user", block("image", "caption", "")}, meta}, "system-then-attachment", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := append([]message{{"user", "列出字段"}, {"assistant", "waiting"}}, tc.tail...)
			raw, err := json.Marshal(map[string]any{"model": "router-auto", "max_tokens": 100, "messages": messages})
			if err != nil {
				t.Fatal(err)
			}
			r := request(string(raw))
			r.Header.Set("x-claude-code-prompt-id", tc.name)
			emitBatchRequest(t, g, r, true)
			page, err := s.RecordsPage(context.Background(), g.Config, Pagination{Page: 1, PageSize: 50}, "session", tc.name)
			if err != nil {
				t.Fatal(err)
			}
			title, _ := taskSummary(tc.title)
			for _, row := range page.Records {
				if row.Prompt == tc.name && row.Kind == "inference" {
					got := ""
					if row.TaskLabel != nil {
						got = row.TaskLabel.Title
					}
					if row.RootPrompt != tc.root || got != title {
						t.Fatal(row.RootPrompt, got, tc)
					}
					return
				}
			}
			t.Fatal("missing row")
		})
	}
}

func TestLowHintDoesNotConsumeHighHandoff(t *testing.T) {
	g, s, _ := lookupFixture(t)
	makeCall := func(id string, n int) batchEvent {
		return batchEvent{Session: "session", Prompt: "task", Event: "PreToolUse", Tool: "mcp__warehouse__query_submit", Call: id, Input: json.RawMessage(fmt.Sprintf(`{"p":%d}`, n))}
	}
	for _, tc := range []struct{ prefix, task, want string }{{"low", "列出字段", "review_hint"}, {"high", "分析并发架构", "handoff"}} {
		first, next := makeCall(tc.prefix+"1", 1), makeCall(tc.prefix+"2", 2)
		emitBatch(t, g, "task", CoordinatorModel, tc.task, true, true, first, next)
		if batchHook(t, g, first) != "pass" || batchHook(t, g, next) != tc.want {
			t.Fatal(tc)
		}
	}
	list, err := s.Tasks(g.Config, 10)
	if err != nil || len(list.Tasks) != 1 || len(list.Tasks[0].BatchChecks) != 2 {
		t.Fatal(list, err)
	}
}

func TestLegacyChildFeedbackCountsOnce(t *testing.T) {
	g, s, _ := lookupFixture(t)
	emitBatch(t, g, "original", "router-auto", "列出字段", false, true, batchEvent{Tool: "Monitor", Call: "wait"})
	r := Record{ID: "wake", Kind: "inference", Class: "main", Session: "session", Prompt: "child"}
	if err := s.startRequest(r, []string{"wait"}); err != nil {
		t.Fatal(err)
	}
	// A previous deployment might leave a feedback row keyed to the raw prompt.
	raw, err := json.Marshal(Feedback{Session: "session", Prompt: "child", Outcome: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO feedback VALUES(?,?,?)", "session", "child", string(raw)); err != nil {
		t.Fatal(err)
	}
	views, err := s.FeedbackViews()
	if err != nil || len(views) != 1 || views[0].Prompt != "original" || views[0].Outcome != "failed" {
		t.Fatal(views, err)
	}
	if err := s.Feedback(Feedback{Session: "session", Prompt: "original", Outcome: "passed"}); err != nil {
		t.Fatal(err)
	}
	summary, err := s.Summary()
	if err != nil || summary.Tasks != 1 || summary.Passed != 1 || summary.Failed != 0 || summary.Unevaluated != 0 {
		t.Fatal(summary, err)
	}
}
