package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Prices struct {
	Version      string  `json:"version"`
	Currency     string  `json:"currency"`
	Input        float64 `json:"input_per_million"`
	CacheRead    float64 `json:"cache_read_per_million"`
	CacheWrite5m float64 `json:"cache_write_5m_per_million"`
	CacheWrite1h float64 `json:"cache_write_1h_per_million"`
	Output       float64 `json:"output_per_million"`
}

// Explicit zero prices are valid; missing price fields are not zero prices.
func (p *Prices) UnmarshalJSON(raw []byte) error {
	type plain Prices
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range []string{"version", "currency", "input_per_million", "cache_read_per_million", "cache_write_5m_per_million", "cache_write_1h_per_million", "output_per_million"} {
		value, ok := fields[key]
		if !ok || string(value) == "null" {
			return fmt.Errorf("prices: %s must be explicit", key)
		}
	}
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*p = Prices(value)
	return nil
}

type CacheConfig struct {
	Kind       string `json:"kind"`
	TTLSeconds int    `json:"ttl_seconds"`
}
type Provider struct {
	BaseURL       string      `json:"base_url"`
	Model         string      `json:"model"`
	APIKeyEnv     string      `json:"api_key_env"`
	Auth          string      `json:"auth"`
	ContextTokens int         `json:"context_tokens"`
	Cache         CacheConfig `json:"cache"`
	Prices        *Prices     `json:"prices"`
}
type Policy struct {
	Version               string  `json:"version"`
	AllowTextSwitch       bool    `json:"allow_text_switch"`
	MinSwitchSaving       float64 `json:"min_switch_saving"`
	EstimatedOutputTokens int     `json:"estimated_output_tokens"`
}
type ClassifierConfig struct {
	Enabled   bool `json:"enabled"`
	TimeoutMS int  `json:"timeout_ms"`
	MaxChars  int  `json:"max_chars"`
}
type Config struct {
	Listen          string           `json:"listen"`
	Database        string           `json:"database"`
	GatewayTokenEnv string           `json:"gateway_token_env"`
	Mode            string           `json:"mode"`
	High            Provider         `json:"high"`
	Low             Provider         `json:"low"`
	Policy          Policy           `json:"policy"`
	Classifier      ClassifierConfig `json:"classifier"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("configuration must contain one JSON object")
	}
	if c.Database == "" {
		return c, errors.New("database is required")
	}
	if !filepath.IsAbs(c.Database) {
		c.Database = filepath.Join(filepath.Dir(path), c.Database)
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("listen must be a loopback IP address")
	}
	if c.Database == "" || c.GatewayTokenEnv == "" {
		return errors.New("database and gateway_token_env are required")
	}
	switch c.Mode {
	case "shadow", "auto", "high", "low":
	default:
		return errors.New("mode must be shadow, auto, high or low")
	}
	if c.Policy.Version == "" || c.Policy.EstimatedOutputTokens <= 0 || c.Policy.MinSwitchSaving < 0 || c.Policy.MinSwitchSaving >= 1 {
		return errors.New("invalid routing policy")
	}
	if c.Classifier.TimeoutMS <= 0 || c.Classifier.MaxChars < 100 || c.Classifier.MaxChars > 20000 {
		return errors.New("invalid classifier bounds")
	}
	for name, p := range map[string]Provider{"high": c.High, "low": c.Low} {
		u, e := url.Parse(p.BaseURL)
		if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%s: invalid base_url", name)
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback()) {
			return fmt.Errorf("%s: use HTTPS or loopback HTTP", name)
		}
		if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/messages") {
			return fmt.Errorf("%s: base_url must not include /messages", name)
		}
		if p.Model == "" || p.APIKeyEnv == "" || p.ContextTokens <= 0 {
			return fmt.Errorf("%s: model, api_key_env and context_tokens required", name)
		}
		if p.Auth != "api_key" && p.Auth != "bearer" {
			return fmt.Errorf("%s: invalid auth", name)
		}
		if p.Cache.Kind != "explicit" && p.Cache.Kind != "automatic" && p.Cache.Kind != "none" {
			return fmt.Errorf("%s: invalid cache kind", name)
		}
		if p.Cache.TTLSeconds <= 0 {
			return fmt.Errorf("%s: positive cache TTL estimate required", name)
		}
		if p.Prices != nil {
			if p.Prices.Version == "" || p.Prices.Currency != "USD" {
				return fmt.Errorf("%s: price version and USD currency required", name)
			}
			for _, n := range []float64{p.Prices.Input, p.Prices.CacheRead, p.Prices.CacheWrite5m, p.Prices.CacheWrite1h, p.Prices.Output} {
				if n < 0 || math.IsInf(n, 0) || math.IsNaN(n) {
					return errors.New("prices must be finite and nonnegative")
				}
			}
		}
	}
	return nil
}
func (c Config) Provider(tier string) Provider {
	if tier == "low" {
		return c.Low
	}
	return c.High
}
func (p Provider) Endpoint(path string) string {
	base := strings.TrimRight(p.BaseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		path = strings.TrimPrefix(path, "/v1")
	}
	return base + path
}
func (c Config) CheckSecrets() error {
	for _, name := range []string{c.GatewayTokenEnv, c.High.APIKeyEnv, c.Low.APIKeyEnv} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			return fmt.Errorf("missing environment variable %s", name)
		}
	}
	if len(os.Getenv(c.GatewayTokenEnv)) < 16 {
		return errors.New("gateway token must be at least 16 characters")
	}
	return nil
}
