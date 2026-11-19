package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Demo contacts loopback mock providers only. Prices and usage are synthetic.
func RunDemo(listen string) error {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		events := []map[string]any{
			{"type": "message_start", "message": map[string]any{"id": "demo-message", "type": "message", "role": "assistant", "model": req.Model, "content": []any{}, "usage": map[string]int{"input_tokens": 120, "cache_read_input_tokens": 800, "cache_creation_input_tokens": 0, "output_tokens": 0}}},
			{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}},
			{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "模拟回答，没有调用真实模型。"}},
			{"type": "content_block_stop", "index": 0},
			{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 64}},
			{"type": "message_stop"},
		}
		for _, event := range events {
			b, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], b)
			w.(http.Flusher).Flush()
		}
	}))
	defer mock.Close()
	os.Setenv("ROUTER_DEMO_KEY", "mock-key")
	prices := func(in, read, out float64) *Prices {
		return &Prices{Version: "synthetic-demo-v1", Currency: "USD", Input: in, CacheRead: read, CacheWrite5m: in * 1.25, CacheWrite1h: in * 2, Output: out}
	}
	c := Config{Listen: listen, Database: "data/demo.db", StoreTaskSummaries: true, Mode: "auto",
		High:   Provider{BaseURL: mock.URL, Model: "demo-opus", APIKeyEnv: "ROUTER_DEMO_KEY", Auth: "api_key", ContextTokens: 200000, Cache: CacheConfig{"explicit", 300}, Prices: prices(5, 0.5, 25)},
		Low:    Provider{BaseURL: mock.URL, Model: "demo-flash", APIKeyEnv: "ROUTER_DEMO_KEY", Auth: "bearer", ContextTokens: 128000, Cache: CacheConfig{"automatic", 300}, Prices: prices(0.3, 0.03, 1.2)},
		Policy: Policy{Version: "demo-v1", AllowTextSwitch: true, MinSwitchSaving: 0.15, EstimatedOutputTokens: 1024}, Classifier: ClassifierConfig{TimeoutMS: 2000, MaxChars: 6000},
	}
	if err := c.Validate(); err != nil {
		return err
	}
	s, err := OpenStore(c.Database)
	if err != nil {
		return err
	}
	defer s.Close()
	g := NewGateway(c, s)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	go func() { errs <- server.Serve(ln) }()
	defer server.Close()
	address := "http://" + ln.Addr().String()
	session := "demo-" + newID()[:8]
	for i, text := range []string{"解释这个简单函数", "设计跨模块并发架构", "把注释翻译成英文", "定位分布式死锁"} {
		body, _ := json.Marshal(map[string]any{"model": "router-auto", "max_tokens": 1000, "stream": true, "messages": []any{map[string]string{"role": "user", "content": text}}})
		req, _ := http.NewRequest("POST", address+"/v1/messages", bytes.NewReader(body))
		req.Header.Set("x-claude-code-session-id", session)
		req.Header.Set("x-claude-code-prompt-id", fmt.Sprintf("demo-task-%d", i+1))
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			return e
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("demo seed failed: %d", resp.StatusCode)
		}
	}
	if err = s.Feedback(Feedback{Session: session, Prompt: "demo-task-1", Outcome: "passed", Note: "模拟人工验收"}); err != nil {
		return err
	}
	fmt.Println("Demo dashboard:", address)
	fmt.Println("Synthetic data only; no external model requests. Ctrl+C to stop.")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errs:
		return err
	}
}
