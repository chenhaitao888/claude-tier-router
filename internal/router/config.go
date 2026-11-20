package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Prices struct {
	Version      string       `json:"version"`
	Currency     string       `json:"currency"`
	Input        float64      `json:"input_per_million"`
	CacheRead    float64      `json:"cache_read_per_million"`
	CacheWrite5m float64      `json:"cache_write_5m_per_million"`
	CacheWrite1h float64      `json:"cache_write_1h_per_million"`
	Output       float64      `json:"output_per_million"`
	Peak         *PeakPricing `json:"peak,omitempty"`
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
	APIKey        string      `json:"api_key,omitempty"`
	APIKeyEnv     string      `json:"api_key_env,omitempty"`
	Auth          string      `json:"auth"`
	ContextTokens int         `json:"context_tokens"`
	Cache         CacheConfig `json:"cache"`
	Prices        *Prices     `json:"prices"`
}
type Policy struct {
	Version               string        `json:"version"`
	AllowTextSwitch       bool          `json:"allow_text_switch"`
	MinSwitchSaving       float64       `json:"min_switch_saving"`
	EstimatedOutputTokens int           `json:"estimated_output_tokens"`
	ExchangeRate          *ExchangeRate `json:"exchange_rate,omitempty"`
}
type ClassifierConfig struct {
	Enabled   bool `json:"enabled"`
	TimeoutMS int  `json:"timeout_ms"`
	MaxChars  int  `json:"max_chars"`
}
type CooperationConfig struct {
	Enabled             bool `json:"enabled"`
	MaxExecutorRequests int  `json:"max_executor_requests,omitempty"`
}

func (c CooperationConfig) ExecutorLimit() int {
	if c.MaxExecutorRequests == 0 {
		return 12
	}
	return c.MaxExecutorRequests
}

type Config struct {
	Listen             string            `json:"listen"`
	Database           string            `json:"database"`
	Mode               string            `json:"mode"`
	High               Provider          `json:"high"`
	Low                Provider          `json:"low"`
	Policy             Policy            `json:"policy"`
	Classifier         ClassifierConfig  `json:"classifier"`
	Cooperation        CooperationConfig `json:"cooperation,omitempty"`
	StoreTaskSummaries bool              `json:"store_task_summaries,omitempty"`
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
	// Accept and discard retired local credentials so existing configs still load.
	legacy := struct {
		*Config
		GatewayToken    json.RawMessage `json:"gateway_token"`
		GatewayTokenEnv json.RawMessage `json:"gateway_token_env"`
	}{Config: &c}
	if err = d.Decode(&legacy); err != nil {
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
	if c.Cooperation.MaxExecutorRequests < 0 || c.Cooperation.MaxExecutorRequests > 100 {
		return errors.New("cooperation.max_executor_requests must be between 1 and 100 (or 0 for default 12)")
	}
	if c.Cooperation.Enabled && c.Mode != "auto" {
		return errors.New("cooperation requires mode auto")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("listen must be a loopback IP address")
	}
	if c.Database == "" {
		return errors.New("database is required")
	}
	switch c.Mode {
	case "shadow", "auto", "high", "low":
	default:
		return errors.New("mode must be shadow, auto, high or low")
	}
	if c.Policy.Version == "" || c.Policy.EstimatedOutputTokens <= 0 || c.Policy.MinSwitchSaving < 0 || c.Policy.MinSwitchSaving >= 1 {
		return errors.New("invalid routing policy")
	}
	if c.Policy.ExchangeRate != nil {
		if err := c.Policy.ExchangeRate.Validate(); err != nil {
			return err
		}
	}
	if c.Classifier.TimeoutMS <= 0 || c.Classifier.MaxChars < 100 || c.Classifier.MaxChars > 20000 {
		return errors.New("invalid classifier bounds")
	}
	for name, p := range map[string]Provider{"high": c.High, "low": c.Low} {
		u, e := url.Parse(p.BaseURL)
		if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%s: invalid base_url", name)
		}
		// localhost is a hostname, so net.ParseIP alone does not recognize it.
		local := strings.EqualFold(u.Hostname(), "localhost") || net.ParseIP(u.Hostname()).IsLoopback()
		if u.Scheme != "https" && !(u.Scheme == "http" && local) {
			return fmt.Errorf("%s: use HTTPS or local HTTP (localhost, 127.0.0.1 or ::1)", name)
		}
		if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/messages") {
			return fmt.Errorf("%s: base_url must not include /messages", name)
		}
		if p.Model == "" || p.ContextTokens <= 0 {
			return fmt.Errorf("%s: model and context_tokens required", name)
		}
		if strings.TrimSpace(p.APIKey) == "" && p.APIKeyEnv == "" {
			return fmt.Errorf("%s: api_key or api_key_env required", name)
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
			if err := p.Prices.Validate(); err != nil {
				return fmt.Errorf("%s: %w", name, err)
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

// Inline values take precedence; existing environment-based configurations work.
func (p Provider) Key() string {
	if key := strings.TrimSpace(p.APIKey); key != "" {
		return key
	}
	return strings.TrimSpace(os.Getenv(p.APIKeyEnv))
}

func (c Config) CheckSecrets() error {
	for _, item := range []struct {
		name     string
		provider Provider
	}{{"high", c.High}, {"low", c.Low}} {
		if item.provider.Key() == "" {
			return fmt.Errorf("%s: set api_key in config or provide the configured api_key_env", item.name)
		}
	}
	return nil
}
