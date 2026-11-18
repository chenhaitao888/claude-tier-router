package router

import (
	"errors"
	"fmt"
	"math"
	"time"
	_ "time/tzdata"
)

type PriceWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}
type PeakPricing struct {
	Timezone        string        `json:"timezone"`
	Weekdays        []int         `json:"weekdays"`
	Windows         []PriceWindow `json:"windows"`
	OffPeakDates    []string      `json:"off_peak_dates"`
	CalendarFrom    string        `json:"calendar_from"`
	CalendarThrough string        `json:"calendar_through"`
	Rates           *Prices       `json:"rates"`
}

func (p Prices) Validate() error {
	if p.Version == "" || (p.Currency != "USD" && p.Currency != "CNY") {
		return errors.New("price version and USD or CNY currency required")
	}
	for _, n := range []float64{p.Input, p.CacheRead, p.CacheWrite5m, p.CacheWrite1h, p.Output} {
		if n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return errors.New("prices must be finite and nonnegative")
		}
	}
	s := p.Peak
	if s == nil {
		return nil
	}
	if s.Rates == nil || s.Rates.Peak != nil || s.Rates.Currency != p.Currency {
		return errors.New("peak rates must use the same currency and cannot contain another schedule")
	}
	if err := s.Rates.Validate(); err != nil {
		return err
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("invalid pricing timezone: %w", err)
	}
	from, e1 := time.Parse("2006-01-02", s.CalendarFrom)
	through, e2 := time.Parse("2006-01-02", s.CalendarThrough)
	if e1 != nil || e2 != nil || from.After(through) {
		return errors.New("pricing calendar range required")
	}
	if len(s.Weekdays) == 0 || len(s.Windows) == 0 {
		return errors.New("peak weekdays and windows required")
	}
	for _, d := range s.Weekdays {
		if d < 0 || d > 6 {
			return errors.New("weekdays must be 0 (Sunday) through 6")
		}
	}
	for _, w := range s.Windows {
		a, e1 := time.Parse("15:04", w.Start)
		b, e2 := time.Parse("15:04", w.End)
		if e1 != nil || e2 != nil || !a.Before(b) || a.Format("15:04") != w.Start || b.Format("15:04") != w.End {
			return errors.New("peak windows must be HH:MM start < end")
		}
	}
	for _, day := range s.OffPeakDates {
		d, err := time.Parse("2006-01-02", day)
		if err != nil || d.Before(from) || d.After(through) {
			return errors.New("holiday date outside pricing calendar")
		}
	}
	return nil
}

// At selects a frozen native-currency tariff. Windows are start-inclusive,
// end-exclusive. A missing future holiday calendar is unknown, not off-peak.
func (p *Prices) At(at time.Time) (*Prices, string) {
	if p == nil {
		return nil, "prices_not_configured"
	}
	copy := *p
	copy.Peak = nil
	if p.Peak == nil {
		return &copy, "fixed"
	}
	s := p.Peak
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil || s.Rates == nil {
		return nil, "pricing_schedule_invalid"
	}
	local := at.In(loc)
	day := local.Format("2006-01-02")
	if day < s.CalendarFrom || day > s.CalendarThrough {
		return nil, "pricing_calendar_unavailable"
	}
	for _, holiday := range s.OffPeakDates {
		if day == holiday {
			return &copy, "off_peak"
		}
	}
	weekday := false
	for _, d := range s.Weekdays {
		if int(local.Weekday()) == d {
			weekday = true
		}
	}
	if weekday {
		clock := local.Format("15:04")
		for _, window := range s.Windows {
			if clock >= window.Start && clock < window.End {
				copy = *s.Rates
				copy.Peak = nil
				return &copy, "peak"
			}
		}
	}
	return &copy, "off_peak"
}

type Money struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}
type RetrospectiveCost struct {
	Money
	PriceVersion string `json:"price_version"`
	Period       string `json:"period"`
	Basis        string `json:"basis"`
}

func (r *Record) FinalizeCost() {
	r.CostAmount, r.CostStatus = r.Usage.Cost(Provider{Prices: r.Prices}, r.Complete)
	r.CostCurrency = ""
	r.CostUSD = nil
	if r.Prices != nil {
		r.CostCurrency = r.Prices.Currency
	}
	if r.Prices == nil && r.PricePeriod == "pricing_calendar_unavailable" {
		r.CostStatus = r.PricePeriod
	}
	if r.CostAmount != nil && r.CostCurrency == "USD" {
		r.CostUSD = r.CostAmount
	}
}

// Cost estimates for unpriced history are view-only. Never replace the ledger
// or an existing price snapshot, or apply a model's rate to a different model.
func (r *Record) EstimateUnpriced(c Config) {
	if r.Prices != nil || r.CostUSD != nil || r.CostAmount != nil || r.CostStatus != "prices_not_configured" || !r.Complete {
		return
	}
	if r.Actual != "high" && r.Actual != "low" {
		return
	}
	p := c.Provider(r.Actual)
	if r.Model != p.Model {
		return
	}
	prices, period := p.Prices.At(r.Started)
	amount, status := r.Usage.Cost(Provider{Prices: prices}, true)
	if amount != nil && status == "usage_calculated" {
		r.Retrospective = &RetrospectiveCost{Money: Money{*amount, prices.Currency}, PriceVersion: prices.Version, Period: period, Basis: "current_config_at_request_time"}
	}
}
