package router

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestProviderBaseURLAllowsLocalHTTP(t *testing.T) {
	for _, tier := range []string{"high", "low"} {
		for _, tc := range []struct {
			url string
			ok  bool
		}{
			{"http://localhost:8000", true},
			{"http://LOCALHOST:8000/v1", true},
			{"http://127.0.0.1:8000", true},
			{"http://[::1]:8000", true},
			{"https://provider.example/anthropic", true},
			{"http://provider.example:8000", false},
			{"http://192.168.1.10:8000", false},
			{"http://0.0.0.0:8000", false},
			{"http://localhost.evil.example:8000", false},
			{"http://evil.localhost:8000", false},
			{"http://localhost@evil.example:8000", false},
			{"ftp://localhost:8000", false},
			{"http://localhost:8000/v1/messages", false},
		} {
			t.Run(tier+"/"+tc.url, func(t *testing.T) {
				c := testConfig()
				if tier == "high" {
					c.High.BaseURL = tc.url
				} else {
					c.Low.BaseURL = tc.url
				}
				err := c.Validate()
				if (err == nil) != tc.ok {
					t.Fatalf("Validate() = %v; accepted want %t", err, tc.ok)
				}
			})
		}
	}
}

func TestProxyThroughLocalhostUpstream(t *testing.T) {
	g, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Error("unexpected upstream path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, goodStream)
	})
	u, err := url.Parse(g.Config.High.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = net.JoinHostPort("localhost", u.Port())
	g.Config.High.BaseURL = u.String()
	if err := g.Config.Validate(); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, request(sample))
	if rr.Code != http.StatusOK || rr.Body.String() != goodStream {
		t.Fatalf("localhost proxy failed: %d %s", rr.Code, rr.Body.String())
	}
}
