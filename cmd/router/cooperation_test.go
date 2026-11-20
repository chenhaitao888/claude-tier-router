package main

import (
	"claude-tier-router/internal/router"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCooperativeLaunchMergesSessionSettings(t *testing.T) {
	c := router.Config{Cooperation: router.CooperationConfig{Enabled: true}, High: router.Provider{ContextTokens: 200000}, Low: router.Provider{ContextTokens: 128000}}
	file := filepath.Join(t.TempDir(), "extra.md")
	os.WriteFile(file, []byte("Preserve existing project convention."), 0600)
	input := []string{"--settings", `{"env":{"KEEP":"yes"},"hooks":{"SubagentStart":[{"matcher":"other","hooks":[{"type":"command","command":"echo user-hook"}]}]},"permissions":{"deny":["Bash(git push *)"]}}`, "--agents", `{"my-agent":{"description":"custom","prompt":"Keep me","model":"inherit"}}`, "--append-system-prompt", "Keep the user language.", "--append-system-prompt-file", file, "--", "--agents", "literal"}
	before := append([]string{}, input...)
	args, cleanup, err := launchArgs(input, c, "http://127.0.0.1:8787")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !reflect.DeepEqual(input, before) {
		t.Fatal("modified caller args")
	}
	if !reflect.DeepEqual(args[len(args)-3:], []string{"--", "--agents", "literal"}) {
		t.Fatal("interpreted literal flags", args)
	}
	if strings.Contains(strings.Join(args, " "), "user-hook") {
		t.Fatal("settings exposed in argv")
	}
	var agents map[string]struct {
		Model      string   `json:"model"`
		Prompt     string   `json:"prompt"`
		Disallowed []string `json:"disallowedTools"`
	}
	var promptPath string
	for i, arg := range args[:len(args)-3] {
		if arg == "--agents" {
			json.Unmarshal([]byte(args[i+1]), &agents)
		}
		if arg == "--append-system-prompt-file" {
			promptPath = args[i+1]
		}
	}
	if len(agents) != 3 || agents[router.ExecutorModel].Model != router.ExecutorModel || agents[router.ReviewerModel].Model != router.ReviewerModel || agents["my-agent"].Prompt != "Keep me" {
		t.Fatal("agents not preserved", agents)
	}
	if !reflect.DeepEqual(agents[router.ExecutorModel].Disallowed, []string{"Agent", "Task"}) {
		t.Fatal("worker can recurse")
	}
	text, err := os.ReadFile(promptPath)
	if err != nil || !strings.Contains(string(text), "Keep the user language.") || !strings.Contains(string(text), "Preserve existing project convention.") || !strings.Contains(string(text), "Router cooperation mode") {
		t.Fatal("lost prompt instructions", err)
	}
	raw, err := os.ReadFile(args[1])
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Model       string                       `json:"model"`
		Env         map[string]string            `json:"env"`
		Hooks       map[string][]json.RawMessage `json:"hooks"`
		Permissions map[string][]string          `json:"permissions"`
	}
	json.Unmarshal(raw, &settings)
	if settings.Model != router.CoordinatorModel || settings.Env["KEEP"] != "yes" || settings.Env["CLAUDE_CODE_SUBAGENT_MODEL_FORCE"] != "0" || len(settings.Hooks["SubagentStart"]) != 2 || len(settings.Hooks["SubagentStop"]) != 1 || len(settings.Permissions["deny"]) != 1 {
		t.Fatal("invalid session settings", settings)
	}
	for _, p := range []string{args[1], promptPath} {
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0600 {
			t.Fatal("private file permissions", info.Mode())
		}
	}
	cleanup()
	if _, err := os.Stat(promptPath); !os.IsNotExist(err) {
		t.Fatal("session prompt leaked")
	}
}

func TestCooperativeLaunchRejectsConflictingOptions(t *testing.T) {
	c := router.Config{Cooperation: router.CooperationConfig{Enabled: true}}
	for _, args := range [][]string{{"--model", "opus"}, {"--agents", `{"router-executor":{}}`}, {"--agents", "{}", "--agents", "{}"}, {"--agents", "[]"}, {"--append-system-prompt-file", t.TempDir()}, {"--append-system-prompt"}, {"--settings", `{"hooks":null}`}} {
		_, cleanup, err := launchArgs(args, c, "http://127.0.0.1:8787")
		if cleanup != nil {
			cleanup()
		}
		if err == nil {
			t.Fatal("accepted conflicting options", args)
		}
	}
}

func TestCooperationDoesNotChangeSharedRuntimeIdentity(t *testing.T) {
	// -cooperate is applied to a copy only after loading the shared config. The
	// server supports both aliases concurrently; providers/config stay identical.
	c := router.Config{High: router.Provider{ContextTokens: 200000}, Low: router.Provider{ContextTokens: 128000}}
	copy := c
	copy.Cooperation.Enabled = true
	plain := routingEnv(c, "http://127.0.0.1:8787")
	coop := routingEnv(copy, "http://127.0.0.1:8787")
	if c.Cooperation.Enabled || plain["ANTHROPIC_MODEL"] != "router-auto" || coop["ANTHROPIC_MODEL"] != router.CoordinatorModel || plain["ANTHROPIC_BASE_URL"] != coop["ANTHROPIC_BASE_URL"] {
		t.Fatal("session option leaked into shared settings")
	}
}
