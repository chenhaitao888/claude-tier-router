package router

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInlineConfigCredentialsAndGeneratedToken(t *testing.T) {
	c := testConfig()
	c.GatewayTokenEnv = ""
	c.High.APIKeyEnv, c.Low.APIKeyEnv = "", ""
	c.High.APIKey, c.Low.APIKey = "inline-high-key", "inline-low-key"
	path := filepath.Join(t.TempDir(), "config.json")
	before, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = loaded.PrepareSecrets(); err != nil {
		t.Fatal(err)
	}
	token := loaded.Token()
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 24 {
		t.Fatal("gateway token was not randomly generated")
	}
	if err = loaded.PrepareSecrets(); err != nil || loaded.Token() != token {
		t.Fatal("preparing the same process changed its credential")
	}
	second := c
	if err = second.PrepareSecrets(); err != nil || second.Token() == token {
		t.Fatal("new process reused generated credential")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("preparation changed the user's config file")
	}
	if loaded.High.Key() != c.High.APIKey || loaded.Low.Key() != c.Low.APIKey {
		t.Fatal("inline keys were not loaded")
	}
}

func TestCredentialPrecedenceAndEnvironmentCompatibility(t *testing.T) {
	c := testConfig()
	t.Setenv(c.High.APIKeyEnv, "environment-high")
	t.Setenv(c.Low.APIKeyEnv, "environment-low")
	t.Setenv(c.GatewayTokenEnv, "environment-gateway-token")
	if err := c.PrepareSecrets(); err != nil {
		t.Fatal(err)
	}
	if c.High.Key() != "environment-high" || c.Low.Key() != "environment-low" || c.Token() != "environment-gateway-token" {
		t.Fatal("old environment configuration stopped working")
	}
	c.High.APIKey, c.Low.APIKey = "inline-high", "inline-low"
	c.GatewayToken = "inline-gateway-token"
	if err := c.PrepareSecrets(); err != nil {
		t.Fatal(err)
	}
	if c.High.Key() != "inline-high" || c.Low.Key() != "inline-low" || c.Token() != "inline-gateway-token" {
		t.Fatal("environment overrode explicit config credentials")
	}
	// An unset old gateway_token_env no longer requires manual setup.
	c.GatewayToken = ""
	t.Setenv(c.GatewayTokenEnv, "")
	if err := c.PrepareSecrets(); err != nil || len(c.Token()) != 48 {
		t.Fatal("missing optional gateway environment variable prevented startup")
	}
}

func TestMissingProviderKeyAndShortGatewayCredential(t *testing.T) {
	c := testConfig()
	c.High.APIKeyEnv, c.Low.APIKeyEnv = "", ""
	c.Low.APIKey = "inline-low"
	if err := c.Validate(); err == nil {
		t.Fatal("missing high credential source accepted")
	}
	if err := c.PrepareSecrets(); err == nil || !strings.Contains(err.Error(), "high: set api_key") {
		t.Fatal("missing high credential was not explained")
	}
	c.High.APIKey = "inline-high"
	c.GatewayToken = "short"
	if err := c.PrepareSecrets(); err == nil {
		t.Fatal("explicit invalid gateway token silently replaced")
	}
}

func TestInlineKeysUsedUpstreamAndKeptOutOfStatistics(t *testing.T) {
	for _, tier := range []string{"high", "low"} {
		t.Run(tier, func(t *testing.T) {
			var upstreamHeaders http.Header
			g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				upstreamHeaders = r.Header.Clone()
				w.Header().Set("Content-Type", "text/event-stream")
				body := goodStream
				if tier == "low" {
					body = strings.ReplaceAll(body, "test-opus", "test-flash")
				}
				io.WriteString(w, body)
			})
			c := g.Config
			c.Mode = tier
			c.High.APIKey, c.Low.APIKey = "inline-high-secret", "inline-low-secret"
			c.High.APIKeyEnv, c.Low.APIKeyEnv, c.GatewayTokenEnv = "", "", ""
			if err := c.PrepareSecrets(); err != nil {
				t.Fatal(err)
			}
			g = NewGateway(c, s)
			req := request(sample)
			req.Header.Set("Authorization", "Bearer "+c.Token())
			rr := httptest.NewRecorder()
			g.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("inline configuration request failed: %d", rr.Code)
			}
			if tier == "high" {
				if upstreamHeaders.Get("x-api-key") != c.High.APIKey || upstreamHeaders.Get("Authorization") != "" {
					t.Fatal("inline high credential not forwarded correctly")
				}
			} else if upstreamHeaders.Get("Authorization") != "Bearer "+c.Low.APIKey || upstreamHeaders.Get("x-api-key") != "" {
				t.Fatal("inline low credential not forwarded correctly")
			}
			for _, path := range []string{"/api/info", "/api/records", "/api/summary"} {
				req = httptest.NewRequest("GET", "http://localhost"+path, nil)
				req.Header.Set("Authorization", "Bearer "+c.Token())
				rr = httptest.NewRecorder()
				g.ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					t.Fatalf("statistics request failed: %d", rr.Code)
				}
				for _, secret := range []string{c.High.APIKey, c.Low.APIKey, c.Token()} {
					if strings.Contains(rr.Body.String(), secret) {
						t.Fatal("statistics exposed a credential")
					}
				}
			}
		})
	}
}
