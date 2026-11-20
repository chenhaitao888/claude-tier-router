package main

import (
	"bytes"
	"claude-tier-router/internal/router"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"time"
)

const runtimeProtocol = "shared-v1"

type runtimeInfo struct {
	Service        string `json:"service"`
	Protocol       string `json:"protocol"`
	RunID          string `json:"run_id"`
	PID            int    `json:"pid"`
	ActiveClients  int    `json:"active_clients"`
	ActiveRequests int    `json:"active_requests"`
	Managed        bool   `json:"managed"`
	Stopping       bool   `json:"stopping"`
	Proof          string `json:"proof,omitempty"`
}
type runtimeState struct {
	Address string `json:"address"`
	RunID   string `json:"run_id"`
}
type runtimeControl struct {
	mu              sync.Mutex
	info            runtimeInfo
	configID, build string
	stop            chan struct{}
}
type runtimeCommand struct {
	RunID       string `json:"run_id"`
	ConfigID    string `json:"-"`
	ConfigProof string `json:"config_proof,omitempty"`
	Nonce       string `json:"nonce,omitempty"`
	Build       string `json:"build,omitempty"`
	Force       bool   `json:"force,omitempty"`
}
type runtimeLease struct {
	Address string
	Info    runtimeInfo
	body    io.ReadCloser
}

func (l *runtimeLease) Close() error { return l.body.Close() }

func (c *runtimeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, reason string) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": reason})
	}
	if r.Method == "GET" && r.URL.Path == "/api/runtime" {
		c.mu.Lock()
		info := c.info
		c.mu.Unlock()
		json.NewEncoder(w).Encode(info)
		return
	}
	if r.Method != "POST" || (r.URL.Path != "/api/runtime/attach" && r.URL.Path != "/api/runtime/stop") {
		fail(404, "unknown runtime endpoint")
		return
	}
	var command runtimeCommand
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if d.Decode(&command) != nil || d.Decode(new(any)) != io.EOF {
		fail(400, "invalid runtime command")
		return
	}
	c.mu.Lock()
	if command.RunID != c.info.RunID || c.info.Stopping {
		c.mu.Unlock()
		fail(409, "runtime_changed")
		return
	}
	if r.URL.Path == "/api/runtime/stop" {
		if (c.info.ActiveClients > 0 || c.info.ActiveRequests > 0) && !command.Force {
			c.mu.Unlock()
			fail(409, "active_clients")
			return
		}
		c.info.Stopping = true
		c.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]bool{"stopping": true})
		close(c.stop)
		return
	}
	if len(command.Nonce) != 32 || !hmac.Equal([]byte(command.ConfigProof), []byte(runtimeProof(c.configID, command, "request"))) || command.Build != c.build {
		c.mu.Unlock()
		fail(409, "configuration_or_binary_changed")
		return
	}
	c.info.ActiveClients++
	info := c.info
	info.Proof = runtimeProof(c.configID, command, "response")
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.info.ActiveClients--; c.mu.Unlock() }()
	if json.NewEncoder(w).Encode(info) != nil {
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	// A held connection is the lease. Normal exit, a killed launcher, or a closed
	// terminal releases it without PID polling or a stale registration file.
	<-r.Context().Done()
}

func (c *runtimeControl) trackRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/messages" {
			c.mu.Lock()
			if c.info.Stopping {
				c.mu.Unlock()
				http.Error(w, "Router is stopping", http.StatusServiceUnavailable)
				return
			}
			c.info.ActiveRequests++
			c.mu.Unlock()
			defer func() { c.mu.Lock(); c.info.ActiveRequests--; c.mu.Unlock() }()
		}
		next.ServeHTTP(w, r)
	})
}

func runtimeFiles(c router.Config) (state, lock, log string) {
	hash := sha256.Sum256([]byte(c.Listen))
	base := filepath.Join(filepath.Dir(c.Database), ".router-"+hex.EncodeToString(hash[:8]))
	return base + ".json", base + ".lock", base + ".log"
}

func runtimeProof(configID string, command runtimeCommand, direction string) string {
	h := hmac.New(sha256.New, []byte(configID))
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", direction, command.RunID, command.Build, command.Nonce)
	return hex.EncodeToString(h.Sum(nil))
}

func runtimeIdentity(c router.Config) (configID, build string, err error) {
	// Compare effective credentials without transmitting or publishing API keys.
	c.High.APIKey, c.Low.APIKey = c.High.Key(), c.Low.Key()
	c.High.APIKeyEnv, c.Low.APIKeyEnv = "", ""
	raw, err := json.Marshal(c)
	if err != nil {
		return "", "", err
	}
	hash := sha256.Sum256(raw)
	configID = hex.EncodeToString(hash[:])
	executable, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	f, err := os.Open(executable)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", "", err
	}
	return configID, hex.EncodeToString(h.Sum(nil)), nil
}

func runtimeClient(stream bool) *http.Client {
	client := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, ResponseHeaderTimeout: 2 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("runtime redirect refused") }}
	if !stream {
		client.Timeout = 2 * time.Second
	}
	return client
}

func validRuntimeAddress(address string) bool {
	u, err := url.Parse(address)
	return err == nil && u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback() && u.Port() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

var errOtherService = errors.New("configured port belongs to an older Router or another service; close the old instance or choose a different listen port")
var errRuntimeMismatch = errors.New("configuration_or_binary_changed")

func probeRuntime(address string) (runtimeInfo, error) {
	var info runtimeInfo
	if !validRuntimeAddress(address) {
		return info, errors.New("invalid local runtime address")
	}
	client := runtimeClient(false)
	defer client.CloseIdleConnections()
	resp, err := client.Get(address + "/api/runtime")
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&info) != nil || info.Service != "claude-tier-router" || info.Protocol != runtimeProtocol || info.RunID == "" {
		return info, errOtherService
	}
	return info, nil
}

func findRuntime(c router.Config) (string, runtimeInfo, error) {
	_, port, _ := net.SplitHostPort(c.Listen)
	address := "http://" + c.Listen
	if port == "0" {
		path, _, _ := runtimeFiles(c)
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", runtimeInfo{}, err
		}
		var state runtimeState
		if json.Unmarshal(raw, &state) != nil || !validRuntimeAddress(state.Address) {
			return "", runtimeInfo{}, errors.New("invalid runtime state file")
		}
		info, err := probeRuntime(state.Address)
		if errors.Is(err, errOtherService) || (err == nil && info.RunID != state.RunID) {
			// An ephemeral port can be reused after a crash. Start a new listener
			// rather than attaching to or stopping whoever now occupies that port.
			return "", runtimeInfo{}, errors.New("stale dynamic-port runtime state")
		}
		return state.Address, info, err
	}
	info, err := probeRuntime(address)
	return address, info, err
}

func attachRuntime(address string, command runtimeCommand) (*runtimeLease, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	command.Nonce = hex.EncodeToString(nonce[:])
	command.ConfigProof = runtimeProof(command.ConfigID, command, "request")
	data, _ := json.Marshal(command)
	client := runtimeClient(true)
	resp, err := client.Post(address+"/api/runtime/attach", "application/json", bytes.NewReader(data))
	if err != nil {
		client.CloseIdleConnections()
		return nil, err
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		defer client.CloseIdleConnections()
		var failure struct{ Error string }
		json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&failure)
		if failure.Error == "configuration_or_binary_changed" {
			return nil, errRuntimeMismatch
		}
		return nil, fmt.Errorf("runtime attach: %s", failure.Error)
	}
	var info runtimeInfo
	if json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&info) != nil || info.RunID != command.RunID || !hmac.Equal([]byte(info.Proof), []byte(runtimeProof(command.ConfigID, command, "response"))) {
		resp.Body.Close()
		client.CloseIdleConnections()
		return nil, errors.New("runtime changed while attaching")
	}
	return &runtimeLease{Address: address, Info: info, body: resp.Body}, nil
}

func stopRuntime(address, runID string, force bool) error {
	data, _ := json.Marshal(runtimeCommand{RunID: runID, Force: force})
	client := runtimeClient(false)
	defer client.CloseIdleConnections()
	resp, err := client.Post(address+"/api/runtime/stop", "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("gateway has active sessions or has changed; close those sessions first, or use router stop -force to interrupt them")
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		info, err := probeRuntime(address)
		if err != nil || info.RunID != runID {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("gateway has not stopped yet")
}

func ensureRuntime(c router.Config, configPath string) (*runtimeLease, error) {
	configID, build, err := runtimeIdentity(c)
	if err != nil {
		return nil, err
	}
	_, _, logPath := runtimeFiles(c)
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(12 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		address, info, probeErr := findRuntime(c)
		if errors.Is(probeErr, errOtherService) {
			return nil, probeErr
		}
		if probeErr == nil && !info.Stopping {
			lease, err := attachRuntime(address, runtimeCommand{RunID: info.RunID, ConfigID: configID, Build: build})
			if err == nil {
				return lease, nil
			}
			if !errors.Is(err, errRuntimeMismatch) {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			// Stop checks active leases atomically, so another launcher cannot be
			// disconnected by the idle configuration/binary refresh race.
			if info.ActiveClients != 0 || info.ActiveRequests != 0 || !info.Managed {
				return nil, fmt.Errorf("gateway configuration or binary differs (active sessions: %d, in-flight requests: %d); close active sessions and restart the gateway", info.ActiveClients, info.ActiveRequests)
			}
			if err := stopRuntime(address, info.RunID, false); err != nil {
				return nil, err
			}
			started = false
		}
		if !started && (probeErr != nil || !info.Stopping) {
			if err := startRuntimeProcess(configPath, logPath); err != nil {
				return nil, err
			}
			started = true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("gateway startup did not complete; see %s", logPath)
}

func startRuntimeProcess(configPath, logPath string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	input, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer input.Close()
	cmd := exec.Command(executable, "serve", "-config", configPath, "-shared-child")
	cmd.Dir = filepath.Dir(configPath)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, output
	cmd.SysProcAttr = backgroundAttributes()
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap short-lived race losers without tying the daemon to this client
	return nil
}

func publishRuntime(path string, state runtimeState) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".router-runtime-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(state); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func removeRuntime(path, runID string) {
	data, err := os.ReadFile(path)
	var state runtimeState
	if err == nil && json.Unmarshal(data, &state) == nil && state.RunID == runID {
		os.Remove(path)
	}
}

func serveRuntime(c router.Config, sharedChild bool) error {
	statePath, lockPath, _ := runtimeFiles(c)
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		return err
	}
	lock, err := lockRuntime(lockPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, _, err := findRuntime(c); err == nil || errors.Is(err, errOtherService) {
		if sharedChild && err == nil {
			return nil // another concurrently started process already owns the listener
		}
		return errors.New("listen port already has a service; use router claude to share a Router")
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	store, err := router.OpenStore(c.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	gateway := router.NewGateway(c, store)
	configID, build, err := runtimeIdentity(c)
	if err != nil {
		return err
	}
	control := &runtimeControl{info: runtimeInfo{Service: "claude-tier-router", Protocol: runtimeProtocol, RunID: gateway.RunID, PID: os.Getpid(), Managed: sharedChild}, configID: configID, build: build, stop: make(chan struct{})}
	gateway.Runtime = control
	server := &http.Server{Handler: control.trackRequests(gateway), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10}
	errs := make(chan error, 1)
	go func() { errs <- server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if server.Shutdown(ctx) != nil {
			server.Close()
		}
	}()
	address := "http://" + listener.Addr().String()
	if err := publishRuntime(statePath, runtimeState{Address: address, RunID: gateway.RunID}); err != nil {
		return err
	}
	defer removeRuntime(statePath, gateway.RunID)
	lock.Close() // listener and state are now ready for concurrent launchers
	fmt.Println("Shared dashboard:", address, "| mode:", c.Mode)
	return waitRuntime(control.stop, errs)
}

func waitRuntime(stop <-chan struct{}, errs <-chan error) error {
	ctx, cancel := signal.NotifyContext(context.Background(), launcherSignals()...)
	defer cancel()
	select {
	case <-stop:
		return nil
	case <-ctx.Done():
		return nil
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
