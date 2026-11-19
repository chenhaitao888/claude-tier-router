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

func TestSessionSettingsOverrideRoutingOnly(t *testing.T) {
	c := router.Config{High: router.Provider{APIKey: "upstream-high-secret", ContextTokens: 200000}, Low: router.Provider{APIKey: "upstream-low-secret", ContextTokens: 100000}}
	custom := `{"env":{"ANTHROPIC_BASE_URL":"http://localhost:8000","ANTHROPIC_API_KEY":"old-key","CLAUDE_CODE_USE_BEDROCK":"1","MY_PROJECT_VAR":"keep"},"model":"old-model","apiKeyHelper":"old-helper","permissions":{"deny":["Bash(rm *)"]},"hooks":{"SessionStart":[]},"enabledPlugins":{"example@local":true}}`
	for _, form := range []string{"inline", "file", "equals"} {
		t.Run(form, func(t *testing.T) {
			value := custom
			if form == "file" {
				value = filepath.Join(t.TempDir(), "custom settings.json")
				if err := os.WriteFile(value, []byte(custom), 0600); err != nil {
					t.Fatal(err)
				}
			}
			original := []string{"--continue", "--settings", value, "--effort", "high"}
			if form == "equals" {
				original = []string{"--continue", "--settings=" + value, "--effort", "high"}
			}
			before := append([]string{}, original...)
			args, cleanup, err := launchArgs(original, c, "http://127.0.0.1:18789")
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if !reflect.DeepEqual(original, before) || !reflect.DeepEqual(args[2:], []string{"--continue", "--effort", "high"}) {
				t.Fatal("caller arguments changed", args)
			}
			if args[0] != "--settings" || strings.Contains(strings.Join(args, " "), "old-key") {
				t.Fatal("settings not supplied through private file")
			}
			raw, err := os.ReadFile(args[1])
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), c.High.APIKey) || strings.Contains(string(raw), c.Low.APIKey) {
				t.Fatal("upstream credentials leaked to child")
			}
			var settings struct {
				Env    map[string]string `json:"env"`
				Model  string            `json:"model"`
				Helper string            `json:"apiKeyHelper"`
			}
			if err = json.Unmarshal(raw, &settings); err != nil {
				t.Fatal(err)
			}
			for k, v := range routingEnv(c, "http://127.0.0.1:18789") {
				if settings.Env[k] != v {
					t.Fatal("routing key not pinned", k)
				}
			}
			for k, v := range conflictingEnv {
				if settings.Env[k] != v {
					t.Fatal("conflicting key active", k)
				}
			}
			if settings.Env["MY_PROJECT_VAR"] != "keep" || settings.Model != "router-auto" || settings.Helper != "" {
				t.Fatal("wrong settings merge")
			}
			var old, current map[string]any
			json.Unmarshal([]byte(custom), &old)
			json.Unmarshal(raw, &current)
			for _, k := range []string{"permissions", "hooks", "enabledPlugins"} {
				if !reflect.DeepEqual(old[k], current[k]) {
					t.Fatal("unrelated settings changed", k)
				}
			}
			info, _ := os.Stat(args[1])
			if info.Mode().Perm() != 0600 {
				t.Fatal("settings permissions", info.Mode())
			}
			if form == "file" {
				after, _ := os.ReadFile(value)
				if string(after) != custom {
					t.Fatal("user settings file modified")
				}
			}
			cleanup()
			if _, err = os.Stat(filepath.Dir(args[1])); !os.IsNotExist(err) {
				t.Fatal("temporary settings not removed")
			}
		})
	}
}

func TestLaunchSettingsValidationAndLiteralArguments(t *testing.T) {
	c := router.Config{}
	for _, args := range [][]string{{"--settings"}, {"--settings="}, {"--settings", "null"}, {"--settings", "{bad"}, {"--settings", `{"env":null}`}, {"--settings", `{"env":{"X":1}}`}, {"--settings", "{}", "--settings", "{}"}, {"--settings", t.TempDir()}, {"--settings", "{" + strings.Repeat(" ", 2<<20) + "}"}} {
		_, cleanup, err := launchArgs(args, c, "http://127.0.0.1:8787")
		if cleanup != nil {
			cleanup()
		}
		if err == nil {
			t.Fatal("invalid settings accepted")
		}
	}
	original := []string{"--", "--settings", "literal prompt"}
	args, cleanup, err := launchArgs(original, c, "http://127.0.0.1:8787")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !reflect.DeepEqual(args[2:], original) {
		t.Fatal("literal prompt interpreted as options")
	}
}
