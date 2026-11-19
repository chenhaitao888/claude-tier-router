package router

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Optional local, offline audit. The fixture contains private request bodies;
// keep it outside the repository. No endpoints, credentials or ledger writes.
func TestLocalCacheReplay(t *testing.T) {
	path := os.Getenv("ROUTER_REPLAY_FIXTURE")
	if path == "" {
		t.Skip("set ROUTER_REPLAY_FIXTURE to a local offline replay fixture")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Request  json.RawMessage `json:"request"`
		Usage    Usage           `json:"usage"`
		Started  time.Time       `json:"started_at"`
		Actual   string          `json:"actual_tier"`
		Previous int             `json:"previous_prediction"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	b := &branch{Cache: map[string][]cacheEntry{}}
	oldZero, newZero, hits, skipped := 0, 0, 0, 0
	var oldError, newError, observedTotal, predictedTotal int64
	rules := map[string]int{}
	for _, row := range rows {
		r, err := ParseRequest(row.Request)
		if err != nil {
			t.Fatal(err)
		}
		tier, _, _ := Rule(r)
		rules[tier]++
		if _, reason := historyConstraint(c, r, b); reason != "" {
			skipped++
		}
		p := c.Provider(row.Actual)
		prediction := cachedTokens(r, p, b.Cache[row.Actual], row.Started)
		observed := num(row.Usage.Read)
		oldDelta, newDelta := int64(row.Previous)-observed, int64(prediction)-observed
		if oldDelta < 0 {
			oldDelta = -oldDelta
		}
		if newDelta < 0 {
			newDelta = -newDelta
		}
		oldError += oldDelta
		newError += newDelta
		observedTotal += observed
		predictedTotal += int64(prediction)
		if num(row.Usage.Read) > 0 {
			hits++
			if row.Previous == 0 {
				oldZero++
			}
			if prediction == 0 {
				newZero++
			}
		}
		Observe(b, r, p, row.Actual, row.Usage, row.Started, true)
	}
	t.Logf("historical requests=%d observed_hits=%d old_zero_predictions=%d new_zero_predictions=%d history_locks=%d rule_low=%d rule_high=%d uncertain=%d", len(rows), hits, oldZero, newZero, skipped, rules["low"], rules["high"], rules[""])
	t.Logf("observed_read=%d predicted_read=%d old_absolute_error=%d new_absolute_error=%d", observedTotal, predictedTotal, oldError, newError)
}
