package router

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type ActiveRoutingConfig struct {
	Mode              string `json:"mode"`
	ClassifierEnabled bool   `json:"classifier_enabled"`
	AllowTextSwitch   bool   `json:"allow_text_switch"`
}

type RoutingDiagnostics struct {
	Scope         string              `json:"scope"`
	CurrentRunID  string              `json:"current_run_id"`
	StartedAt     time.Time           `json:"started_at"`
	CurrentConfig ActiveRoutingConfig `json:"current_config"`
	Health        RoutingHealth       `json:"routing_health"`
}

// Run IDs, rather than time windows, isolate a restarted gateway and concurrent
// gateways sharing a ledger. Records from old versions remain in all-history.
func (s *Store) RoutingHealth(ctx context.Context, runID string) (RoutingHealth, error) {
	out := newRoutingHealth()
	query := "SELECT payload FROM records"
	args := []any{}
	if runID != "" {
		query += " WHERE json_extract(payload, '$.run_id')=?"
		args = append(args, runID)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return out, err
		}
		var record Record
		if err = json.Unmarshal([]byte(raw), &record); err != nil {
			return out, err
		}
		out.add(record)
	}
	return out, rows.Err()
}

func (g *Gateway) routingHealth(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "current"
	}
	if scope != "current" && scope != "all" {
		errorJSON(w, 400, "scope must be current or all")
		return
	}
	runID := g.RunID
	if scope == "all" {
		runID = ""
	}
	health, err := g.Store.RoutingHealth(r.Context(), runID)
	if err != nil {
		errorJSON(w, 500, "could not read routing diagnostics")
		return
	}
	jsonResponse(w, RoutingDiagnostics{Scope: scope, CurrentRunID: g.RunID, StartedAt: g.StartedAt,
		CurrentConfig: ActiveRoutingConfig{Mode: g.Config.Mode, ClassifierEnabled: g.Config.Classifier.Enabled, AllowTextSwitch: g.Config.Policy.AllowTextSwitch}, Health: health})
}
