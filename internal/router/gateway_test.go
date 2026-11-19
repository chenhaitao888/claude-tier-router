package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const token = "integration-test-token-long"
const sample = `{"model":"router-auto","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"explain this function"}],"future_field":{"ordered":[3,1,2]}}`
const goodStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"test-opus\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":20,\"cache_creation_input_tokens\":0,\"output_tokens\":0}}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func fixture(t *testing.T, h http.HandlerFunc) (*Gateway, *Store) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	c := testConfig()
	c.High.BaseURL = up.URL
	c.Low.BaseURL = up.URL
	c.Mode = "high"
	t.Setenv(c.High.APIKeyEnv, "real-high-key")
	t.Setenv(c.Low.APIKeyEnv, "real-low-key")
	s, err := OpenStore(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	g := NewGateway(c, s)
	return g, s
}
func request(body string) *http.Request {
	r := httptest.NewRequest("POST", "http://localhost/v1/messages?beta=true", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("x-api-key", "caller-key-must-not-leak")
	r.Header.Set("anthropic-beta", "future-beta")
	r.Header.Set("x-claude-code-session-id", "session")
	r.Header.Set("x-claude-code-prompt-id", "task")
	return r
}
func TestProxyBytePreservationCredentialsAndLedger(t *testing.T) {
	var got []byte
	var headers http.Header
	var query string
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		headers = r.Header.Clone()
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", "provider-id")
		io.WriteString(w, goodStream)
	})
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, request(sample))
	if rr.Code != 200 || rr.Body.String() != goodStream {
		t.Fatalf("proxy changed stream %d %s", rr.Code, rr.Body.String())
	}
	if string(got) != strings.Replace(sample, "router-auto", "test-opus", 1) {
		t.Fatalf("changed payload %s", got)
	}
	if headers.Get("Authorization") != "" || headers.Get("x-api-key") != "real-high-key" || headers.Get("anthropic-beta") != "future-beta" || query != "beta=true" {
		t.Fatalf("bad headers/query %v %s", headers, query)
	}
	records, err := s.Records(10)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	rec := records[0]
	if !rec.Complete || num(rec.Usage.Output) != 9 || rec.CostUSD == nil || rec.Prompt != "task" {
		t.Fatalf("bad ledger %+v", rec)
	}
	if err = s.Feedback(Feedback{Session: "session", Prompt: "task", Outcome: "passed", Note: "verified tests"}); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summary()
	if err != nil || sum.Passed != 1 || sum.Requests != 1 || sum.UnknownCost != 0 {
		t.Fatal(sum, err)
	}
}
func TestCrossOriginAndTokenCountingDoNotDispatch(t *testing.T) {
	var count atomic.Int64
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) { count.Add(1) })
	req := request(sample)
	req.Header.Set("Origin", "https://untrusted.example")
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatal(rr.Code)
	}
	req = request(sample)
	req.URL.Path = "/v1/messages/count_tokens"
	rr = httptest.NewRecorder()
	g.ServeHTTP(rr, req)
	if rr.Code != 501 {
		t.Fatal(rr.Code)
	}
	records, _ := s.Records(10)
	if count.Load() != 0 || len(records) != 0 {
		t.Fatal("non-inference dispatched")
	}
}
func TestTruncatedStreamRemainsUnknown(t *testing.T) {
	truncated := strings.Split(goodStream, "event: message_stop")[0]
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, truncated)
	})
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, request(sample))
	records, _ := s.Records(10)
	if records[0].Complete || records[0].CostUSD != nil || records[0].Error != "incomplete_stream" {
		t.Fatalf("bad truncation %+v", records[0])
	}
	if rr.Body.String() != truncated {
		t.Fatal("invented stream tail")
	}
}
func TestStreamFlushesBeforeUpstreamFinishes(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	g, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, goodStream)
	})
	server := httptest.NewServer(g)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/messages", strings.NewReader(sample))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("buffered stream", err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "event: ping\n" {
		t.Fatal(line, err)
	}
	once.Do(func() { close(release) })
	io.Copy(io.Discard, resp.Body)
}
func TestCancellationReachesProvider(t *testing.T) {
	arrived := make(chan struct{})
	canceled := make(chan struct{})
	g, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(arrived)
		<-r.Context().Done()
		close(canceled)
	})
	server := httptest.NewServer(g)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/messages", strings.NewReader(sample))
	req.Header.Set("Authorization", "Bearer "+token)
	done := make(chan struct{})
	go func() {
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		close(done)
	}()
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("provider not reached")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not receive cancellation")
	}
	<-done
}
func TestClassifierIsSeparatelyBilledAndDoesNotRecurse(t *testing.T) {
	var classifyCalls, inferCalls atomic.Int64
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		json.Unmarshal(raw, &body)
		if _, yes := body["system"]; yes {
			classifyCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer real-low-key" || r.Header.Get("x-api-key") != "" {
				t.Error("classifier wrong credentials")
			}
			json.NewEncoder(w).Encode(map[string]any{"type": "message", "stop_reason": "end_turn", "content": []any{map[string]string{"type": "text", "text": `{"tier":"low","confidence":0.9,"reason":"simple"}`}}, "usage": map[string]int{"input_tokens": 15, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 10}})
			return
		}
		inferCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.ReplaceAll(goodStream, "test-opus", "test-flash"))
	})
	g.Config.Mode = "auto"
	g.Config.Classifier.Enabled = true
	body := strings.Replace(sample, "explain this function", "make the requested change", 1)
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, request(body))
	if rr.Code != 200 || classifyCalls.Load() != 1 || inferCalls.Load() != 1 {
		t.Fatal(rr.Code, classifyCalls.Load(), inferCalls.Load())
	}
	rows, _ := s.Records(10)
	if len(rows) != 2 {
		t.Fatal(rows)
	}
	sum, _ := s.Summary()
	if sum.ClassifierRequests != 1 || sum.Low != 1 || sum.ClassifierCostUSD <= 0 {
		t.Fatal(sum)
	}
}
func TestRedirectNeverLeaksUpstreamKey(t *testing.T) {
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	g, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) })
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, request(sample))
	if rr.Code != 307 || hits.Load() != 0 {
		t.Fatal("followed credential redirect")
	}
}
func TestConcurrentBranchesAndDurableFeedback(t *testing.T) {
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, goodStream)
	})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := request(sample)
			r.Header.Set("x-claude-code-agent-id", fmt.Sprint(i))
			rr := httptest.NewRecorder()
			g.ServeHTTP(rr, r)
			if rr.Code != 200 {
				t.Error(rr.Code)
			}
		}(i)
	}
	wg.Wait()
	sum, err := s.Summary()
	if err != nil || sum.Requests != 12 {
		t.Fatal(sum, err)
	}
	if err = s.Feedback(Feedback{Session: "session", Prompt: "task", Outcome: "passed"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Feedback(Feedback{Session: "session", Prompt: "task", Outcome: "failed"}); err != nil {
		t.Fatal(err)
	}
	sum, err = s.Summary()
	if err != nil || sum.Failed != 1 || sum.Passed != 0 {
		t.Fatal(sum, err)
	}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://localhost/api/records", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	g.ServeHTTP(rr, r)
	if bytes.Contains(rr.Body.Bytes(), []byte("real-high-key")) || bytes.Contains(rr.Body.Bytes(), []byte("explain this function")) {
		t.Fatal("sensitive body persisted")
	}
}

func TestCompactionUsesOwningBranch(t *testing.T) {
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, goodStream)
	})
	g.Config.Mode = "auto"
	g.Config.Policy.AllowTextSwitch = false
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, request(strings.Replace(sample, "explain this function", "architecture", 1)))
	body := `{"model":"router-auto","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"architecture"},{"role":"assistant","content":[{"type":"thinking","thinking":"opaque"},{"type":"text","text":"answer"}]},{"role":"user","content":"summarize"}]}`
	req := request(body)
	req.Header.Set("x-claude-code-request-class", "compaction")
	rr = httptest.NewRecorder()
	g.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("compaction lost origin: %d %s", rr.Code, rr.Body.String())
	}
	rows, _ := s.Records(10)
	if len(rows) != 2 || rows[0].Actual != "high" {
		t.Fatal(rows)
	}
}
