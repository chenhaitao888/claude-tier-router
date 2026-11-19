package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInlineConfigCredentials(t *testing.T) {
	c := testConfig()
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
	if err = loaded.CheckSecrets(); err != nil {
		t.Fatal(err)
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
	if err := c.CheckSecrets(); err != nil {
		t.Fatal(err)
	}
	if c.High.Key() != "environment-high" || c.Low.Key() != "environment-low" {
		t.Fatal("old environment configuration stopped working")
	}
	c.High.APIKey, c.Low.APIKey = "inline-high", "inline-low"
	if err := c.CheckSecrets(); err != nil {
		t.Fatal(err)
	}
	if c.High.Key() != "inline-high" || c.Low.Key() != "inline-low" {
		t.Fatal("environment overrode explicit config credentials")
	}

}

func TestMissingProviderKey(t *testing.T) {
	c := testConfig()
	c.High.APIKeyEnv, c.Low.APIKeyEnv = "", ""
	c.Low.APIKey = "inline-low"
	if err := c.Validate(); err == nil {
		t.Fatal("missing high credential source accepted")
	}
	if err := c.CheckSecrets(); err == nil || !strings.Contains(err.Error(), "high: set api_key") {
		t.Fatal("missing high credential was not explained")
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
			c.High.APIKeyEnv, c.Low.APIKeyEnv = "", ""
			if err := c.CheckSecrets(); err != nil {
				t.Fatal(err)
			}
			g = NewGateway(c, s)
			req := request(sample)
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
				rr = httptest.NewRecorder()
				g.ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					t.Fatalf("statistics request failed: %d", rr.Code)
				}
				for _, secret := range []string{c.High.APIKey, c.Low.APIKey} {
					if strings.Contains(rr.Body.String(), secret) {
						t.Fatal("statistics exposed a credential")
					}
				}
			}
		})
	}
}
