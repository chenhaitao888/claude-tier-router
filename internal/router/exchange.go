package router

import (
	"errors"
	"math"
)

// A user-specified planning rate, never a claim about settlement or spot FX.
type ExchangeRate struct {
	Version   string  `json:"version"`
	CNYPerUSD float64 `json:"cny_per_usd"`
}

func (r ExchangeRate) Validate() error {
	if r.Version == "" || r.CNYPerUSD <= 0 || math.IsNaN(r.CNYPerUSD) || math.IsInf(r.CNYPerUSD, 0) {
		return errors.New("exchange_rate needs a version and a finite positive cny_per_usd")
	}
	return nil
}

func (r *ExchangeRate) usd(amount *float64, currency string) *float64 {
	if amount == nil {
		return nil
	}
	if currency == "USD" {
		n := *amount
		return &n
	}
	if currency != "CNY" || r == nil || r.Validate() != nil {
		return nil
	}
	n := *amount / r.CNYPerUSD
	return &n
}
