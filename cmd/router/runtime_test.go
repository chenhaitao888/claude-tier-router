package main

import (
	"bytes"
	"claude-tier-router/internal/router"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func awaitRuntime(t *testing.T, address string, clients int) runtimeInfo {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, err := probeRuntime(address)
		if err == nil && info.ActiveClients == clients {
			return info
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("runtime did not reach %d active clients", clients)
	return runtimeInfo{}
}

func TestRuntimeLeasesIdentityAndLocalAccess(t *testing.T) {
	control := &runtimeControl{info: runtimeInfo{Service: "claude-tier-router", Protocol: runtimeProtocol, RunID: "fixture-run"}, configID: "private-config-digest", build: "fixture-build", stop: make(chan struct{})}
	g := router.NewGateway(router.Config{}, nil)
	g.Runtime = control
	srv := httptest.NewServer(g)
	defer srv.Close()
	command := runtimeCommand{RunID: "fixture-run", ConfigID: "private-config-digest", Build: "fixture-build"}
	first, err := attachRuntime(srv.URL, command)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := attachRuntime(srv.URL, command)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	awaitRuntime(t, srv.URL, 2)
	if err := stopRuntime(srv.URL, "fixture-run", false); err == nil {
		t.Fatal("stopped active clients")
	}
	first.Close()
	awaitRuntime(t, srv.URL, 1)
	wrong := command
	wrong.ConfigID = "different-key-or-config"
	if l, err := attachRuntime(srv.URL, wrong); err == nil {
		l.Close()
		t.Fatal("shared incompatible config")
	}
	wrong = command
	wrong.Build = "different-binary"
	if l, err := attachRuntime(srv.URL, wrong); err == nil {
		l.Close()
		t.Fatal("shared incompatible binary")
	}
	wrong = command
	wrong.RunID = "stale-run"
	if l, err := attachRuntime(srv.URL, wrong); err == nil {
		l.Close()
		t.Fatal("attached to stale run")
	}
	data, _ := json.Marshal(runtimeCommand{RunID: "fixture-run", Force: true})
	req, _ := http.NewRequest("POST", srv.URL+"/api/runtime/stop", bytes.NewReader(data))
	req.Header.Set("Origin", "https://unrelated.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("cross-origin control accepted", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/api/runtime")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(raw, []byte("private-config-digest")) {
		t.Fatal("config digest publicly exposed")
	}
	second.Close()
	awaitRuntime(t, srv.URL, 0)
}

func TestRuntimeRejectsForeignServiceAndUnsafeState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"status":"ok"}`) }))
	defer srv.Close()
	if _, err := probeRuntime(srv.URL); err != errOtherService {
		t.Fatal("foreign health endpoint accepted", err)
	}
	for _, address := range []string{"http://external.example:8787", "http://127.0.0.1:8787/path", "https://127.0.0.1:8787", "http://user:secret@127.0.0.1:8787"} {
		if validRuntimeAddress(address) {
			t.Fatal("unsafe runtime address", address)
		}
	}
	c := router.Config{Listen: "127.0.0.1:0", Database: filepath.Join(t.TempDir(), "ledger.db")}
	state, _, _ := runtimeFiles(c)
	if err := publishRuntime(state, runtimeState{Address: "http://external.example:8787", RunID: "foreign"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := findRuntime(c); err == nil {
		t.Fatal("unsafe state followed")
	}
}

func TestRuntimeIdentityCannotBeSatisfiedByEchoingClientProof(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var command runtimeCommand
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("private-digest")) {
			t.Error("configuration digest sent to unverified listener")
		}
		json.Unmarshal(raw, &command)
		json.NewEncoder(w).Encode(runtimeInfo{RunID: command.RunID, Proof: command.ConfigProof})
	}))
	defer srv.Close()
	if lease, err := attachRuntime(srv.URL, runtimeCommand{RunID: "fake-run", ConfigID: "private-digest", Build: "same-build"}); err == nil {
		lease.Close()
		t.Fatal("foreign listener accepted by echoing client data")
	}
}

func TestRuntimeProtectsRequestsWithoutLauncherLease(t *testing.T) {
	control := &runtimeControl{info: runtimeInfo{Service: "claude-tier-router", Protocol: runtimeProtocol, RunID: "direct-client"}, stop: make(chan struct{})}
	started, release := make(chan struct{}), make(chan struct{})
	g := router.NewGateway(router.Config{}, nil)
	g.Runtime = control
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			close(started)
			<-release
			w.WriteHeader(200)
			return
		}
		g.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(control.trackRequests(handler))
	defer srv.Close()
	done := make(chan error, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	<-started
	info, err := probeRuntime(srv.URL)
	if err != nil || info.ActiveRequests != 1 || info.ActiveClients != 0 {
		close(release)
		<-done
		t.Fatal(info, err)
	}
	if err := stopRuntime(srv.URL, info.RunID, false); err == nil {
		close(release)
		<-done
		t.Fatal("stopped direct client's in-flight request")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	info, err = probeRuntime(srv.URL)
	if err != nil || info.ActiveRequests != 0 {
		t.Fatal(info, err)
	}
}

type capturedLaunch struct{ CWD, Settings, Address string }

func waitCapture(t *testing.T, path string) capturedLaunch {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		var capture capturedLaunch
		if err == nil && json.Unmarshal(data, &capture) == nil && capture.Address != "" {
			return capture
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("Claude fixture did not start", path)
	return capturedLaunch{}
}

func TestConcurrentLaunchersShareIndependentGateway(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX CLI fixture")
	}
	for _, mode := range []string{"fixed-port", "dynamic-port"} {
		t.Run(mode, func(t *testing.T) { testConcurrentLaunchers(t, mode == "dynamic-port") })
	}
}

func testConcurrentLaunchers(t *testing.T, dynamicPort bool) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "router")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"type": "message", "model": body.Model, "content": []any{map[string]string{"type": "text", "text": "fixture"}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 10, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 1}})
	}))
	defer up.Close()
	c := router.Config{Listen: "127.0.0.1:0", Database: filepath.Join(dir, "data", "ledger.db"), Mode: "auto", Policy: router.Policy{Version: "test", AllowTextSwitch: true, EstimatedOutputTokens: 100}, Classifier: router.ClassifierConfig{TimeoutMS: 1000, MaxChars: 1000}}
	if !dynamicPort {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		c.Listen = listener.Addr().String()
		listener.Close()
	}
	c.High = router.Provider{BaseURL: up.URL, Model: "fixture-high", APIKey: "fixture-high-secret", Auth: "api_key", ContextTokens: 200000, Cache: router.CacheConfig{Kind: "none", TTLSeconds: 300}}
	c.Low = c.High
	c.Low.Model = "fixture-low"
	c.Low.APIKey = "fixture-low-secret"
	config := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(c)
	writeFixture(t, config, string(raw), 0600)
	commands := filepath.Join(dir, "commands")
	testBinary, _ := os.Executable()
	quoted := "'" + strings.ReplaceAll(testBinary, "'", "'\\''") + "'"
	writeFixture(t, filepath.Join(commands, "claude"), "#!/bin/sh\nexec "+quoted+" -test.run=^TestClaudeStubProcess$ -- \"$@\"\n", 0700)
	var children []*exec.Cmd
	t.Cleanup(func() {
		for _, child := range children {
			if child.Process != nil && child.ProcessState == nil {
				child.Process.Signal(syscall.SIGTERM)
				child.Wait()
			}
		}
		if out, err := exec.Command(binary, "stop", "-config", config, "-force").CombinedOutput(); err != nil {
			t.Errorf("cleanup gateway: %v %s", err, out)
		}
	})
	start := func(name string, hold bool) (*exec.Cmd, string, string) {
		t.Helper()
		work := filepath.Join(dir, name)
		if err := os.MkdirAll(work, 0700); err != nil {
			t.Fatal(err)
		}
		capture := filepath.Join(work, "capture.json")
		release := filepath.Join(work, "release")
		cmd := exec.Command(binary, "claude", "-config", config)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "PATH="+commands+string(os.PathListSeparator)+os.Getenv("PATH"), "ROUTER_TEST_CAPTURE="+capture, "ROUTER_TEST_SESSION="+name)
		if hold {
			cmd.Env = append(cmd.Env, "ROUTER_TEST_WAIT_FILE="+release)
		}
		output, err := os.Create(filepath.Join(work, "output.txt"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { output.Close() })
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, cmd)
		return cmd, capture, release
	}
	first, cap1, release1 := start("project-one", true)
	second, cap2, release2 := start("project-two", true)
	one, two := waitCapture(t, cap1), waitCapture(t, cap2)
	if one.Address != two.Address || one.CWD == two.CWD {
		t.Fatal("did not share gateway or lost working directories", one, two)
	}
	info := awaitRuntime(t, one.Address, 2)
	if info.PID == first.Process.Pid || info.PID == second.Process.Pid || !info.Managed {
		t.Fatal("gateway lifetime belongs to launcher", info)
	}
	if out, err := exec.Command(binary, "stop", "-config", config).CombinedOutput(); err == nil {
		t.Fatal("stopped sessions without force", string(out))
	}
	// A changed key must never silently reuse another account, even if model IDs match.
	c.Low.APIKey = "new-fixture-secret"
	changed, _ := json.Marshal(c)
	writeFixture(t, config, string(changed), 0600)
	if out, err := exec.Command(binary, "claude", "-config", config).CombinedOutput(); err == nil || !strings.Contains(string(out), "configuration or binary differs") {
		t.Fatal("changed credentials accepted", err, string(out))
	}
	writeFixture(t, config, string(raw), 0600)
	writeFixture(t, release1, "done", 0600)
	if err := first.Wait(); err != nil {
		t.Fatal("first launcher", err)
	}
	if after := awaitRuntime(t, one.Address, 1); after.RunID != info.RunID {
		t.Fatal("closing first launcher restarted gateway")
	}
	third, cap3, _ := start("project-three", false)
	three := waitCapture(t, cap3)
	if err := third.Wait(); err != nil {
		t.Fatal(err)
	}
	if three.Address != one.Address {
		t.Fatal("later session could not reuse gateway")
	}
	awaitRuntime(t, one.Address, 1)
	// Closing a terminal must release its client lease without killing the service.
	interrupted, capInterrupted, _ := start("project-interrupted", true)
	if capture := waitCapture(t, capInterrupted); capture.Address != one.Address {
		t.Fatal("unexpected service")
	}
	awaitRuntime(t, one.Address, 2)
	closingSignal := syscall.SIGTERM
	if !dynamicPort {
		closingSignal = syscall.SIGHUP
	}
	interrupted.Process.Signal(closingSignal)
	interrupted.Wait()
	if after := awaitRuntime(t, one.Address, 1); after.RunID != info.RunID {
		t.Fatal("terminal close stopped gateway")
	}
	writeFixture(t, release2, "done", 0600)
	if err := second.Wait(); err != nil {
		t.Fatal("remaining session lost service", err)
	}
	awaitRuntime(t, one.Address, 0)
	s, err := router.OpenStore(c.Database)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Records(20)
	s.Close()
	sessions := map[string]bool{}
	for _, r := range rows {
		sessions[r.Session] = true
	}
	if err != nil || len(rows) != 4 || len(sessions) != 4 {
		t.Fatal("shared ledger merged session identities", rows, err)
	}
	// Idle service refreshes automatically after config edits.
	writeFixture(t, config, string(changed), 0600)
	fourth, cap4, _ := start("project-four", false)
	four := waitCapture(t, cap4)
	if err := fourth.Wait(); err != nil {
		t.Fatal(err)
	}
	newInfo := awaitRuntime(t, four.Address, 0)
	if newInfo.RunID == info.RunID {
		t.Fatal("idle configuration change not applied")
	}
	if _, err := os.Stat(one.Settings); !os.IsNotExist(err) {
		t.Fatal("first client settings leaked")
	}
	statePath, _, logPath := runtimeFiles(c)
	for _, path := range []string{statePath, logPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("fixture-high-secret")) || bytes.Contains(data, []byte("new-fixture-secret")) {
			t.Fatal("credential leaked to runtime state/log")
		}
		stat, _ := os.Stat(path)
		if stat.Mode().Perm() != 0600 {
			t.Fatal("runtime file permissions", path, stat.Mode())
		}
	}
}
