package router

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func flashPrices() *Prices {
	peak := &Prices{Version: "flash-peak", Currency: "CNY", Input: 2, CacheRead: .04, Output: 8}
	return &Prices{Version: "flash-offpeak", Currency: "CNY", Input: 1, CacheRead: .02, Output: 4,
		Peak: &PeakPricing{Timezone: "Asia/Shanghai", Weekdays: []int{1, 2, 3, 4, 5}, Windows: []PriceWindow{{"09:00", "12:00"}, {"14:00", "18:00"}}, OffPeakDates: []string{"2026-10-01"}, CalendarFrom: "2026-01-01", CalendarThrough: "2027-12-31", Rates: peak}}
}
func instant(s string) time.Time {
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return at
}
func countedUsage(input, read, write, output int64) Usage {
	return Usage{Input: &input, Read: &read, Write: &write, Output: &output}
}

func TestPeakPricingBoundariesWeekendsHolidaysAndTimezone(t *testing.T) {
	p := flashPrices()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		at, period string
		input      float64
	}{
		{"2026-11-18T08:59:59+08:00", "off_peak", 1},
		{"2026-11-18T09:00:00+08:00", "peak", 2},
		{"2026-11-18T11:59:59+08:00", "peak", 2},
		{"2026-11-18T12:00:00+08:00", "off_peak", 1},
		{"2026-11-18T13:59:59+08:00", "off_peak", 1},
		{"2026-11-18T14:00:00+08:00", "peak", 2},
		{"2026-11-18T17:59:59+08:00", "peak", 2},
		{"2026-11-18T18:00:00+08:00", "off_peak", 1},
		{"2026-11-18T01:00:00Z", "peak", 2},
		{"2026-11-21T10:00:00+08:00", "off_peak", 1},
		{"2026-10-01T10:00:00+08:00", "off_peak", 1},
		// Adjusted working Saturdays remain weekends under the provider rule.
		{"2026-10-10T10:00:00+08:00", "off_peak", 1},
	} {
		t.Run(tc.at, func(t *testing.T) {
			rate, period := p.At(instant(tc.at))
			if period != tc.period || rate == nil || rate.Input != tc.input || rate.Peak != nil {
				t.Fatalf("wrong active tariff %v %s", rate, period)
			}
		})
	}
	if p, reason := p.At(instant("2028-01-03T10:00:00+08:00")); p != nil || reason != "pricing_calendar_unavailable" {
		t.Fatal("guessed an unknown holiday calendar")
	}
}

func TestNativeCurrencySnapshotDoesNotBecomeUSD(t *testing.T) {
	p := flashPrices()
	at := instant("2026-11-18T14:00:00+08:00")
	r := Record{Complete: true, Usage: countedUsage(1000000, 1000000, 0, 1000000), PricedAt: at}
	r.Prices, r.PricePeriod = p.At(at)
	// A price edit or end-of-response time must not change the dispatched rate.
	p.Peak.Rates.Input = 999
	r.FinalizeCost()
	if r.CostCurrency != "CNY" || r.CostUSD != nil || r.CostAmount == nil || math.Abs(*r.CostAmount-10.04) > 1e-9 || r.PricePeriod != "peak" {
		t.Fatalf("wrong native bill %+v", r)
	}
	u := r.Usage
	if cost, reason := u.Cost(Provider{Prices: p}, true); cost != nil || reason != "price_period_not_selected" {
		t.Fatal("unresolved scheduled tariff was charged")
	}
}

func TestPricingConfigurationValidation(t *testing.T) {
	p := flashPrices()
	raw, _ := json.Marshal(p)
	var decoded Prices
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Prices){
		func(p *Prices) { p.Peak.Rates.Currency = "USD" },
		func(p *Prices) { p.Peak.Windows[0].Start = "9:00" },
		func(p *Prices) { p.Peak.Windows[0].End = "08:00" },
		func(p *Prices) { p.Peak.Timezone = "invalid/zone" },
		func(p *Prices) { p.Peak.OffPeakDates = []string{"2028-01-01"} },
		func(p *Prices) { p.Peak.Rates.Peak = p.Peak },
	} {
		p := flashPrices()
		mutate(p)
		if p.Validate() == nil {
			t.Fatal("invalid tariff accepted")
		}
	}
	c, err := LoadConfig("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.High.Prices.Currency != "USD" || c.High.Prices.CacheWrite1h != 8 || c.Low.Prices.Currency != "CNY" || c.Low.Prices.Peak == nil {
		t.Fatal("example prices not configured")
	}
}

func TestRoutingNeverComparesDifferentCurrencyAmounts(t *testing.T) {
	c := testConfig()
	c.Low.Prices.Currency = "CNY"
	d := Decide(c, Request{Tokens: 100, MaxTokens: 100}, &branch{Cache: map[string][]cacheEntry{}}, "low", "simple", .9)
	if d.CostComparable || d.LowCost != nil || d.LowNativeCost == nil || d.LowCurrency != "CNY" || d.Selected != "low" || !strings.Contains(strings.Join(d.Reasons, ","), "currency_not_comparable") {
		t.Fatalf("mixed currencies in routing %+v", d)
	}
}

func TestUsageSharesWorkWithoutAnyPrices(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	records := []Record{
		{ID: "1", Actual: "high", Kind: "inference", Class: "main", Complete: true, Usage: countedUsage(80, 10, 10, 100)},
		{ID: "2", Actual: "low", Kind: "inference", Class: "subagent", Complete: true, Usage: countedUsage(20, 0, 0, 30)},
		{ID: "3", Actual: "low", Kind: "inference", Class: "auxiliary", Complete: true, Usage: countedUsage(50, 0, 0, 50)},
		{ID: "4", Actual: "low", Kind: "classifier", Class: "router_classifier", Complete: true, Usage: countedUsage(400, 0, 0, 100)},
		{ID: "5", Actual: "high", Kind: "inference", Class: "main", Complete: false, Usage: countedUsage(90, 0, 0, 1)},
		{ID: "6", Kind: "inference", Class: "main", HTTPStatus: 409},
	}
	for _, r := range records {
		if err = s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	out, err := s.Summary()
	if err != nil {
		t.Fatal(err)
	}
	all := out.Scopes["all"]
	main := out.Scopes["main"]
	if out.Requests != 5 || out.Undispatched != 1 || all.Requests != 4 || all.Tokens != 350 || all.MissingTokenRequests != 1 || *all.High.RequestShare != .5 || math.Abs(*all.High.TokenShare-200.0/350) > 1e-10 {
		t.Fatalf("bad share denominators %+v %+v", out, all)
	}
	if main.Requests != 3 || main.Tokens != 250 || *main.High.TokenShare != .8 || out.Scopes["background"].Tokens != 100 || out.Scopes["classifier"].Tokens != 500 {
		t.Fatal("request classes mixed")
	}
	if out.UnknownCost != 6 || len(out.KnownCosts) != 0 {
		t.Fatal("unknown package price was treated as free")
	}
}

func TestZeroUsageShareIsUnknownAndMissingUsageIsExcluded(t *testing.T) {
	var scope UsageScope
	scope.finish()
	if scope.High.TokenShare != nil || scope.High.RequestShare != nil {
		t.Fatal("empty denominator became zero percent")
	}
	u := countedUsage(100, 0, 0, 1)
	u.Read = nil
	scope.add(Record{Actual: "high", Complete: true, Usage: u})
	scope.finish()
	if scope.Tokens != 0 || scope.MissingTokenRequests != 1 || scope.High.TokenShare != nil || *scope.High.RequestShare != 1 {
		t.Fatal("missing usage counted as complete tokens")
	}
	scope.add(Record{Actual: "low", Complete: true, Usage: countedUsage(10, 0, 0, 5)})
	scope.finish()
	if scope.High.TokenShare != nil || scope.Low.TokenShare == nil || *scope.Low.TokenShare != 1 {
		t.Fatal("a model with entirely missing usage was shown as zero percent")
	}
}

func TestMultiCurrencyTotalsAndViewOnlyHistoryEstimates(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "money.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	usd, cny := .1, .2
	rows := []Record{
		{ID: "legacy-usd", Kind: "inference", Actual: "high", CostUSD: &usd},
		{ID: "native-cny", Kind: "inference", Actual: "low", CostAmount: &cny, CostCurrency: "CNY"},
		{ID: "unpriced", Kind: "inference", Actual: "low", Model: "test-flash", Started: instant("2026-11-18T14:00:00+08:00"), Complete: true, CostStatus: "prices_not_configured", Usage: countedUsage(1000000, 1000000, 0, 1000000)},
		{ID: "other-model", Kind: "inference", Actual: "low", Model: "some-other-model", Complete: true, CostStatus: "prices_not_configured", Usage: countedUsage(10, 0, 0, 1)},
	}
	for _, r := range rows {
		if err = s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	c := testConfig()
	c.Low.Prices = flashPrices()
	out, err := s.Summary(c)
	if err != nil {
		t.Fatal(err)
	}
	if out.KnownCosts["USD"] != .1 || out.KnownCosts["CNY"] != .2 || out.KnownCostUSD != .1 || math.Abs(out.EstimatedCosts["CNY"]-10.04) > 1e-9 || out.EstimatedRequests != 1 || out.UnestimatedCost != 1 {
		t.Fatalf("invalid money aggregation %+v", out)
	}
	stored, _ := s.Records(10)
	for _, r := range stored {
		if r.ID == "unpriced" && (r.Prices != nil || r.CostAmount != nil || r.Retrospective != nil) {
			t.Fatal("history estimate mutated original ledger")
		}
	}
	withSnapshot := rows[2]
	withSnapshot.Prices = c.Low.Prices
	withSnapshot.EstimateUnpriced(c)
	if withSnapshot.Retrospective != nil {
		t.Fatal("overrode original snapshot")
	}
}

func TestStatisticsAPIShowsEstimatesWithoutPersistingThem(t *testing.T) {
	g, s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("statistics called a model") })
	g.Config.Low.Prices = flashPrices()
	r := Record{ID: "history", Kind: "inference", Actual: "low", Model: g.Config.Low.Model, Started: instant("2026-11-18T10:00:00+08:00"), Complete: true, Usage: countedUsage(1000, 1000, 0, 1000), CostStatus: "prices_not_configured"}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://localhost/api/records", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, req)
	var page RecordPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	rows := page.Records
	if len(rows) != 1 || rows[0].Retrospective == nil || rows[0].Retrospective.Currency != "CNY" {
		t.Fatal("history estimate not exposed")
	}
	if rows[0].CostAmount != nil || rows[0].CostUSD != nil {
		t.Fatal("estimate claimed as recorded cost")
	}
}

func TestGatewayPersistsNativeCostAndSummaryDoesNotTreatItAsUnknown(t *testing.T) {
	g, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.ReplaceAll(goodStream, "test-opus", "test-flash"))
	})
	g.Config.Mode = "low"
	g.Config.Low.Prices = flashPrices()
	g.Config.Low.Prices.Peak = nil
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, request(sample))
	if rr.Code != 200 {
		t.Fatalf("proxy failed: %d", rr.Code)
	}
	rows, err := s.Records(10)
	if err != nil || len(rows) != 1 {
		t.Fatal("missing record", err)
	}
	r := rows[0]
	want := (10 + .4 + 36) / 1e6
	if r.CostUSD != nil || r.CostAmount == nil || math.Abs(*r.CostAmount-want) > 1e-12 || r.CostCurrency != "CNY" || r.PricedAt.IsZero() || r.Prices.Peak != nil {
		t.Fatalf("native price not persisted %+v", r)
	}
	summary, err := s.Summary(g.Config)
	if err != nil || summary.UnknownCost != 0 || summary.KnownCostUSD != 0 || math.Abs(summary.KnownCosts["CNY"]-want) > 1e-12 {
		t.Fatal("native cost counted incorrectly", summary, err)
	}
}
