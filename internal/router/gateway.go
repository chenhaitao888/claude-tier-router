package router

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

type Gateway struct {
	Config Config
	Store  *Store
	Engine *Engine
	Client *http.Client
	token  string
	Logger *log.Logger
}

func NewGateway(c Config, s *Store) *Gateway {
	return &Gateway{Config: c, Store: s, Engine: NewEngine(), token: c.Token(), Logger: log.Default(),
		Client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 60 * time.Second, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 32, DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (g *Gateway) logError(message string) { g.Logger.Print(message) }
func (g *Gateway) authorized(r *http.Request) bool {
	for _, v := range []string{strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), r.Header.Get("x-api-key")} {
		if g.token != "" && subtle.ConstantTimeCompare([]byte(v), []byte(g.token)) == 1 {
			return true
		}
	}
	return false
}
func errorJSON(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": message}})
}
func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" {
		jsonResponse(w, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/" && r.Method == "GET" {
		g.dashboard(w, r)
		return
	}
	if !g.authorized(r) {
		errorJSON(w, 401, "valid gateway token required")
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/messages":
		g.messages(w, r)
	case r.Method == "POST" && r.URL.Path == "/v1/messages/count_tokens":
		errorJSON(w, 501, "token counting is not implemented; client may estimate locally")
	case r.Method == "HEAD" && r.URL.Path == "/api/hello":
		w.WriteHeader(http.StatusOK)
	case r.Method == "GET" && r.URL.Path == "/api/records":
		rows, err := g.Store.Records(500)
		if err != nil {
			errorJSON(w, 500, "could not read records")
			return
		}
		for i := range rows {
			rows[i].EstimateUnpriced(g.Config)
		}
		jsonResponse(w, rows)
	case r.Method == "GET" && r.URL.Path == "/api/summary":
		v, err := g.Store.Summary(g.Config)
		if err != nil {
			errorJSON(w, 500, "could not read summary")
			return
		}
		jsonResponse(w, v)
	case r.Method == "GET" && r.URL.Path == "/api/feedback":
		v, err := g.Store.Feedbacks()
		if err != nil {
			errorJSON(w, 500, "could not read feedback")
			return
		}
		jsonResponse(w, v)
	case r.Method == "POST" && r.URL.Path == "/api/feedback":
		var f Feedback
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		d.DisallowUnknownFields()
		if d.Decode(&f) != nil || d.Decode(new(any)) != io.EOF {
			errorJSON(w, 400, "invalid feedback")
			return
		}
		if err := g.Store.Feedback(f); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		jsonResponse(w, map[string]bool{"saved": true})
	case r.Method == "GET" && r.URL.Path == "/api/info":
		jsonResponse(w, map[string]any{"mode": g.Config.Mode, "policy_version": g.Config.Policy.Version, "allow_text_switch": g.Config.Policy.AllowTextSwitch, "classifier_enabled": g.Config.Classifier.Enabled, "high_model": g.Config.High.Model, "low_model": g.Config.Low.Model})
	default:
		errorJSON(w, 404, "endpoint not found")
	}
}
func (g *Gateway) upstream(ctx context.Context, p Provider, path, query string, body []byte, headers http.Header) (*http.Response, error) {
	endpoint := p.Endpoint(path)
	if query != "" {
		endpoint += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range headers {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "anthropic-") || strings.HasPrefix(lower, "x-claude-code-") {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if p.Auth == "bearer" {
		req.Header.Set("Authorization", "Bearer "+p.Key())
	} else {
		req.Header.Set("x-api-key", p.Key())
	}
	return g.Client.Do(req)
}
func (g *Gateway) messages(w http.ResponseWriter, httpReq *http.Request) {
	started := time.Now()
	id := newID()
	body, err := io.ReadAll(http.MaxBytesReader(w, httpReq.Body, 32<<20))
	if err != nil {
		errorJSON(w, 413, "request exceeds limit or cannot be read")
		return
	}
	req, err := ParseRequest(body)
	if err != nil {
		errorJSON(w, 400, err.Error())
		return
	}
	session := httpReq.Header.Get("x-claude-code-session-id")
	if session == "" {
		session = httpReq.Header.Get("x-router-session-id")
	}
	if session == "" {
		session = "unattributed-" + id
	}
	prompt := httpReq.Header.Get("x-claude-code-prompt-id")
	agent := httpReq.Header.Get("x-claude-code-agent-id")
	class := httpReq.Header.Get("x-claude-code-request-class")
	if class == "" {
		class = "main"
	}
	if len(session) > 200 || len(prompt) > 200 || len(agent) > 200 || len(class) > 100 {
		errorJSON(w, 400, "invalid attribution identifiers")
		return
	}
	routingClass := class
	if routingClass == "compaction" {
		routingClass = "main"
	}
	branchKey := session + "\x00" + agent + "\x00" + routingClass
	if class == "auxiliary" {
		branchKey += "\x00" + prompt
	}
	b, release := g.Engine.Acquire(branchKey)
	defer release()
	if httpReq.Context().Err() != nil {
		return
	}
	rec := Record{ID: id, Kind: "inference", Session: session, Prompt: prompt, Agent: agent, Class: class, Started: started, Mode: g.Config.Mode, Policy: g.Config.Policy.Version, CostStatus: "in_progress"}
	if err = g.Store.Save(rec); err != nil {
		errorJSON(w, 503, "request ledger unavailable")
		return
	}
	defer func() {
		rec.DurationMS = time.Since(started).Milliseconds()
		if rec.Actual != "" {
			rec.FinalizeCost()
			if rec.Model != g.Config.Provider(rec.Actual).Model {
				rec.CostUSD = nil
				rec.CostAmount = nil
				rec.CostStatus = "reported_model_differs_from_price_model"
			}
		}
		if g.Store.Save(rec) != nil {
			g.logError("failed to persist inference result")
		}
	}()
	tier, reason, confidence := Rule(req)
	classified := false
	if tier == "" && g.Config.Classifier.Enabled && (g.Config.Mode == "auto" || g.Config.Mode == "shadow") {
		tier, reason, confidence = g.classify(httpReq.Context(), rec, req)
		classified = true
	}
	rec.Decision = Decide(g.Config, req, b, tier, reason, confidence)
	if classified {
		rec.Decision.Classifier = "low_model"
	}
	if rec.Decision.Error != "" {
		rec.HTTPStatus = 409
		rec.Error = rec.Decision.Error
		rec.CostStatus = "not_dispatched"
		errorJSON(w, 409, rec.Decision.Error)
		return
	}
	rec.Actual = rec.Decision.Selected
	p := g.Config.Provider(rec.Actual)
	rec.Model = p.Model
	rec.PricedAt = time.Now()
	rec.Prices, rec.PricePeriod = p.Prices.At(rec.PricedAt)
	outgoing, err := ReplaceModel(body, p.Model)
	if err != nil {
		rec.Error = "request_mapping_error"
		errorJSON(w, 400, rec.Error)
		return
	}
	// Persist the decision before spending on the selected provider.
	if g.Store.Save(rec) != nil {
		rec.Error = "ledger_unavailable"
		errorJSON(w, 503, rec.Error)
		return
	}
	ctx, cancel := context.WithTimeout(httpReq.Context(), 30*time.Minute)
	defer cancel()
	resp, err := g.upstream(ctx, p, "/v1/messages", httpReq.URL.RawQuery, outgoing, httpReq.Header)
	if err != nil {
		rec.Error = "upstream_transport_error"
		rec.HTTPStatus = 502
		errorJSON(w, 502, rec.Error)
		return
	}
	defer resp.Body.Close()
	rec.HTTPStatus = resp.StatusCode
	for k, vs := range resp.Header {
		lower := strings.ToLower(k)
		if lower == "content-type" || lower == "retry-after" || lower == "x-should-retry" || lower == "request-id" || lower == "content-encoding" || strings.HasPrefix(lower, "anthropic-") {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
	}
	w.Header().Set("X-Router-Request-Id", id)
	w.WriteHeader(resp.StatusCode)
	stream := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	var obs observer
	var capture bytes.Buffer
	captureOverflow := false
	buf := make([]byte, 16<<10)
	first := true
	readOK := true
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if first {
				rec.FirstByteMS = time.Since(started).Milliseconds()
				first = false
			}
			if stream {
				obs.Feed(buf[:n])
			} else if capture.Len()+n <= 32<<20 {
				capture.Write(buf[:n])
			} else {
				captureOverflow = true
			}
			if _, err = w.Write(buf[:n]); err != nil {
				rec.Error = "client_write_error"
				readOK = false
				break
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				rec.Error = "upstream_read_error"
				readOK = false
			}
			break
		}
	}
	if stream {
		rec.Usage = obs.Usage
		rec.Complete = readOK && resp.StatusCode == 200 && obs.Complete()
		if obs.StreamError {
			rec.Error = "upstream_stream_error"
		} else if !rec.Complete && rec.Error == "" {
			rec.Error = "incomplete_stream"
		}
		if obs.Model != "" {
			rec.Model = obs.Model
		}
	} else if !captureOverflow {
		var msg struct {
			Type       string          `json:"type"`
			Model      string          `json:"model"`
			Usage      json.RawMessage `json:"usage"`
			StopReason *string         `json:"stop_reason"`
		}
		if json.Unmarshal(capture.Bytes(), &msg) == nil {
			usageErr := rec.Usage.Merge(msg.Usage)
			rec.Complete = readOK && resp.StatusCode == 200 && msg.Type == "message" && msg.StopReason != nil && usageErr == nil
			if msg.Model != "" {
				rec.Model = msg.Model
			}
		}
		if !rec.Complete && rec.Error == "" {
			rec.Error = "upstream_error_or_incomplete_response"
		}
	} else {
		rec.Error = "response_observation_limit"
	}
	if resp.StatusCode == 200 {
		Observe(b, req, p, rec.Actual, rec.Usage, started, rec.Complete)
	}
}
