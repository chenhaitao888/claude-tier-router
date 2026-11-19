package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func getPage(t *testing.T, g *Gateway, path string, out any) {
	t.Helper()
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, httptest.NewRequest("GET", "http://localhost"+path, nil))
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), out) != nil {
		t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
	}
}

func TestPaginationBeyondOldCapsAndExactTaskFilter(t *testing.T) {
	g, s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("pagination dispatched a model") })
	when := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	// Deliberately tie timestamps to exercise deterministic ordering.
	for i := 0; i < 1120; i++ {
		session := "long-session"
		if i >= 1005 {
			session = fmt.Sprintf("session-%03d", i)
		}
		if err := s.Save(Record{ID: fmt.Sprintf("r-%04d", i), Kind: "inference", Session: session, Prompt: "same-prompt", Class: "main", Started: when}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveTaskLabel(TaskLabel{Session: "long-session", Prompt: "same-prompt", Title: "long question", Source: "manual"}, true); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{10, 20, 50} {
		seenTasks, seenRecords := map[string]bool{}, map[string]bool{}
		for page := 1; ; page++ {
			var result TaskPage
			getPage(t, g, fmt.Sprintf("/api/tasks?page=%d&page_size=%d", page, size), &result)
			if result.Pagination.Total != 116 || result.TaskList.Total != 116 || len(result.Tasks) > size {
				t.Fatal("task total or limit incorrect", result)
			}
			for _, task := range result.Tasks {
				if seenTasks[task.Session] {
					t.Fatal("task repeated across pages")
				}
				seenTasks[task.Session] = true
				if task.Session == "long-session" && task.Requests != 1005 {
					t.Fatal("task aggregate truncated by pagination")
				}
			}
			if page == result.TotalPages {
				break
			}
		}
		for page := 1; ; page++ {
			var result RecordPage
			getPage(t, g, fmt.Sprintf("/api/records?page=%d&page_size=%d", page, size), &result)
			if result.Total != 1120 || len(result.Records) > size {
				t.Fatal("request total or limit incorrect")
			}
			for _, row := range result.Records {
				if seenRecords[row.ID] {
					t.Fatal("record repeated across pages")
				}
				seenRecords[row.ID] = true
			}
			if page == result.TotalPages {
				break
			}
		}
		if len(seenTasks) != 116 || len(seenRecords) != 1120 {
			t.Fatal("old records inaccessible")
		}
	}
	var last RecordPage
	getPage(t, g, "/api/task-records?session_id=long-session&prompt_id=same-prompt&page=999999999&page_size=50", &last)
	if last.Page != 21 || last.Total != 1005 || len(last.Records) != 5 {
		t.Fatal("out of range page not clamped", last.Pagination)
	}
	for _, r := range last.Records {
		if r.Session != "long-session" || r.TaskLabel == nil || r.TaskLabel.Title != "long question" {
			t.Fatal("task identity or title incorrect")
		}
	}
	getPage(t, g, "/api/task-records?session_id=long&prompt_id=same-prompt", &last)
	if last.Total != 0 || last.Page != 1 || last.TotalPages != 1 || last.Records == nil || len(last.Records) != 0 {
		t.Fatal("empty/exact filter incorrect")
	}
}

func TestPaginationValidation(t *testing.T) {
	for _, query := range []string{"page=0", "page=-1", "page=1.5", "page=", "page=9999999999999999999999", "page_size=100", "page_size=0", "page_size=20&page_size=10", "page=1&page=2"} {
		q, _ := url.ParseQuery(query)
		if _, err := parsePagination(q); err == nil {
			t.Fatal("accepted invalid pagination", query)
		}
	}
	g, _ := fixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid API dispatched") })
	for _, path := range []string{"/api/tasks?page=0", "/api/records?page_size=100", "/api/task-records?session_id=only"} {
		rr := httptest.NewRecorder()
		g.ServeHTTP(rr, httptest.NewRequest("GET", "http://localhost"+path, nil))
		if rr.Code != 400 {
			t.Fatal(path, rr.Code)
		}
	}
}

func TestLocalGatewayWithoutCredentialsAndBrowserBoundary(t *testing.T) {
	g, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "real-high-key" {
			t.Error("upstream credential lost")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, goodStream)
	})
	r := request(sample)
	r.Header.Del("Authorization")
	r.Header.Del("x-api-key")
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Fatal("local inference still requires token", rr.Code)
	}
	for _, tc := range []struct {
		host, origin, site string
		status             int
	}{
		{"localhost:8787", "http://localhost:8787", "same-origin", 200},
		{"127.0.0.1:8787", "", "", 200},
		{"[::1]:8787", "http://[::1]:8787", "same-origin", 200},
		{"evil.example:8787", "", "", 403},
		{"localhost:8787", "https://evil.example", "", 403},
		{"localhost:8787", "http://localhost:9999", "", 403},
		{"localhost:8787", "null", "", 403},
		{"localhost:8787", "", "cross-site", 403},
	} {
		r := httptest.NewRequest("GET", "http://"+tc.host+"/api/info", nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		rr := httptest.NewRecorder()
		g.ServeHTTP(rr, r)
		if rr.Code != tc.status {
			t.Fatal(tc, rr.Code)
		}
	}
	c := testConfig()
	c.Listen = "0.0.0.0:8787"
	if c.Validate() == nil {
		t.Fatal("public listener accepted")
	}
}

func TestRetiredGatewayConfigFieldsAreIgnored(t *testing.T) {
	raw, _ := json.Marshal(testConfig())
	raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"gateway_token":"old","gateway_token_env":"UNSET_LEGACY"}`)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c.High.APIKey, c.Low.APIKey = "high", "low"
	if err = c.CheckSecrets(); err != nil {
		t.Fatal(err)
	}
	current, _ := json.Marshal(c)
	if strings.Contains(string(current), "gateway_token") {
		t.Fatal("retired credential retained")
	}
}
