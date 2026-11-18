package main

import (
	"claude-tier-router/internal/router"
	"strings"
	"testing"
)

func TestLauncherOverridesOnlyChildEnvironment(t *testing.T) {
	t.Setenv("TEST_TOKEN", "local-gateway-token")
	c := router.Config{GatewayTokenEnv: "TEST_TOKEN", High: router.Provider{ContextTokens: 200000}, Low: router.Provider{ContextTokens: 100000}}
	old := []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=old-secret", "CLAUDE_CODE_USE_BEDROCK=1", "ANTHROPIC_BASE_URL=https://old", "ANTHROPIC_MODEL=old"}
	env := launchEnv(old, c, "http://127.0.0.1:8787")
	m := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	if m["ANTHROPIC_API_KEY"] != "" || m["CLAUDE_CODE_USE_BEDROCK"] != "" || m["ANTHROPIC_AUTH_TOKEN"] != "local-gateway-token" || m["PATH"] != "/usr/bin" || m["ANTHROPIC_MODEL"] != "router-auto" || m["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] != "75000" {
		t.Fatal(m)
	}
	if old[1] != "ANTHROPIC_API_KEY=old-secret" {
		t.Fatal("mutated caller environment")
	}
}

func TestLauncherWithInlineKeysNeedsNoEnvironment(t *testing.T) {
	c := router.Config{
		High: router.Provider{APIKey: "inline-high-secret", ContextTokens: 200000},
		Low:  router.Provider{APIKey: "inline-low-secret", ContextTokens: 100000},
	}
	if err := c.PrepareSecrets(); err != nil {
		t.Fatal(err)
	}
	env := launchEnv([]string{"PATH=/usr/bin"}, c, "http://127.0.0.1:8787")
	found := false
	for _, entry := range env {
		if entry == "ANTHROPIC_AUTH_TOKEN="+c.Token() {
			found = true
		}
		if strings.Contains(entry, c.High.APIKey) || strings.Contains(entry, c.Low.APIKey) {
			t.Fatal("upstream config keys were copied into child environment")
		}
	}
	if !found {
		t.Fatal("child did not receive the gateway's generated credential")
	}
}
