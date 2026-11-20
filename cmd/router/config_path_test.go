package main

import (
	"claude-tier-router/internal/router"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeFixture(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func TestConfigDiscoveryAndExplicitOverride(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkout, work, home := filepath.Join(dir, "router checkout"), filepath.Join(dir, "work"), filepath.Join(dir, "user config")
	binary := filepath.Join(checkout, "bin", "router")
	writeFixture(t, binary, "fixture", 0700)
	paths := []string{filepath.Join(checkout, "config.json"), filepath.Join(checkout, "bin", "config.json"), filepath.Join(home, ".config", "claude-tier-router", "config.json"), filepath.Join(work, "config.json")}
	for _, path := range paths {
		writeFixture(t, path, "{}", 0600)
	}
	installed := filepath.Join(dir, "router")
	if err := os.Symlink(binary, installed); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	explicit, err := defaultConfigPath("other.json", "missing executable", work, home)
	if err != nil || explicit != filepath.Join(work, "other.json") {
		t.Fatal("explicit path must take precedence", explicit, err)
	}
	for _, want := range paths {
		got, err := defaultConfigPath("", installed, work, home)
		if err != nil || got != want {
			t.Fatalf("got %s (%v), want %s", got, err, want)
		}
		if err := os.Remove(want); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := defaultConfigPath("", installed, work, home); err == nil || !strings.Contains(err.Error(), "-config") {
		t.Fatal("missing config has no actionable error", err)
	}
	if err := os.Mkdir(paths[0], 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, paths[3], "{}", 0600)
	if _, err := defaultConfigPath("", installed, work, home); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatal("invalid preferred config silently fell back", err)
	}
}

func TestInstalledCommandPreservesWorkDirectoryAndDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("installer targets macOS/Linux shells")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkout, work, commands := filepath.Join(dir, "router checkout"), filepath.Join(dir, "work project"), filepath.Join(dir, "commands")
	binary := filepath.Join(checkout, "bin", "router")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	config := `{"listen":"127.0.0.1:0","database":"data/router.db","mode":"auto","high":{"base_url":"http://127.0.0.1:9","model":"fixture-high","api_key":"fixture-high-secret","auth":"api_key","context_tokens":200000,"cache":{"kind":"none","ttl_seconds":300}},"low":{"base_url":"http://127.0.0.1:9","model":"fixture-low","api_key":"fixture-low-secret","auth":"api_key","context_tokens":200000,"cache":{"kind":"none","ttl_seconds":300}},"policy":{"version":"test","estimated_output_tokens":100},"classifier":{"timeout_ms":1000,"max_chars":1000}}`
	writeFixture(t, filepath.Join(checkout, "config.json"), config, 0600)
	t.Cleanup(func() {
		cmd := exec.Command(binary, "stop", "-config", filepath.Join(checkout, "config.json"), "-force")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("stop fixture gateway: %v %s", err, out)
		}
	})
	writeFixture(t, filepath.Join(work, "config.json"), "business configuration must not be loaded", 0600)
	if err := os.MkdirAll(commands, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binary, filepath.Join(commands, "router")); err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quotedTest := "'" + strings.ReplaceAll(testBinary, "'", "'\\''") + "'"
	writeFixture(t, filepath.Join(commands, "claude"), "#!/bin/sh\nexec "+quotedTest+" -test.run=^TestClaudeStubProcess$ -- \"$@\"\n", 0700)
	database := filepath.Join(checkout, "data", "router.db")
	s, err := router.OpenStore(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(router.Record{ID: "existing-record", Kind: "inference"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	capture := filepath.Join(dir, "capture.json")
	cmd := exec.Command("/bin/sh", "-c", "router claude -- --continue")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "PATH="+commands+string(os.PathListSeparator)+os.Getenv("PATH"), "ROUTER_TEST_CAPTURE="+capture)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), filepath.Join(checkout, "config.json")) || strings.Contains(string(out), "fixture-high-secret") {
		t.Fatalf("installed launch: %v %s", err, out)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		CWD, Settings string
		Args          []string
	}
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	if captured.CWD != work || !reflect.DeepEqual(captured.Args, []string{"--continue"}) {
		t.Fatal("changed Claude working directory or arguments", captured)
	}
	if _, err := os.Stat(captured.Settings); !os.IsNotExist(err) {
		t.Fatal("temporary session settings not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(work, "data")); !os.IsNotExist(err) {
		t.Fatal("database was created in work project")
	}
	s, err = router.OpenStore(database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.Records(10)
	if err != nil || len(rows) != 1 || rows[0].ID != "existing-record" {
		t.Fatal("existing ledger not preserved", rows, err)
	}
	after, _ := os.ReadFile(filepath.Join(checkout, "config.json"))
	if string(after) != config {
		t.Fatal("configuration modified")
	}
}

// This helper is the fake Claude executable. It verifies a live local gateway
// without sending model requests, then captures only non-secret launch details.
func TestClaudeStubProcess(t *testing.T) {
	capture := os.Getenv("ROUTER_TEST_CAPTURE")
	if capture == "" {
		return
	}
	for i, arg := range os.Args {
		if arg != "--settings" || i+1 == len(os.Args) {
			continue
		}
		path := os.Args[i+1]
		raw, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(raw), "fixture-high-secret") || strings.Contains(string(raw), "fixture-low-secret") {
			t.Fatal("invalid session settings", err)
		}
		var settings struct{ Env map[string]string }
		if err := json.Unmarshal(raw, &settings); err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(settings.Env["ANTHROPIC_BASE_URL"] + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal("gateway is not healthy", resp.StatusCode)
		}
		cwd, _ := os.Getwd()
		address := settings.Env["ANTHROPIC_BASE_URL"]
		if session := os.Getenv("ROUTER_TEST_SESSION"); session != "" {
			request, _ := http.NewRequest("POST", address+"/v1/messages", strings.NewReader(`{"model":"router-auto","max_tokens":100,"messages":[{"role":"user","content":"explain the fixture"}]}`))
			request.Header.Set("x-claude-code-session-id", session)
			request.Header.Set("x-claude-code-prompt-id", "same-prompt")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal("fixture inference failed", response.StatusCode)
			}
		}
		out, _ := json.Marshal(map[string]any{"cwd": cwd, "settings": path, "args": os.Args[i+2:], "address": address})
		if err := os.WriteFile(capture, out, 0600); err != nil {
			t.Fatal(err)
		}
		if wait := os.Getenv("ROUTER_TEST_WAIT_FILE"); wait != "" {
			deadline := time.Now().Add(45 * time.Second)
			for {
				if _, err := os.Stat(wait); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture session timed out")
				}
				time.Sleep(25 * time.Millisecond)
			}
			response, err := http.Get(address + "/healthz")
			if err != nil {
				t.Fatal("gateway disappeared while another session exited", err)
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal(response.StatusCode)
			}
		}
		fmt.Println("stub Claude connected to router")
		return
	}
	t.Fatal("session settings not supplied")
}
