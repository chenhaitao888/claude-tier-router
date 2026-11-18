package router

import (
	"strings"
	"sync"
	"time"
)

type Decision struct {
	Recommended    string   `json:"recommended"`
	Proposed       string   `json:"proposed"`
	Selected       string   `json:"selected"`
	Reasons        []string `json:"reasons"`
	Classifier     string   `json:"classifier"`
	Confidence     float64  `json:"confidence"`
	HighCost       *float64 `json:"estimated_high_usd"`
	LowCost        *float64 `json:"estimated_low_usd"`
	HighNativeCost *float64 `json:"estimated_high_cost"`
	LowNativeCost  *float64 `json:"estimated_low_cost"`
	HighCurrency   string   `json:"estimated_high_currency"`
	LowCurrency    string   `json:"estimated_low_currency"`
	CostComparable bool     `json:"cost_comparable"`
	HighCached     int      `json:"estimated_high_cached_tokens"`
	LowCached      int      `json:"estimated_low_cached_tokens"`
	Error          string   `json:"error,omitempty"`
}
type cacheEntry struct {
	Prefix string
	Tokens int
	At     time.Time
}
type branch struct {
	mu      sync.Mutex
	Current string
	Cache   map[string][]cacheEntry
	Last    time.Time
	refs    int
}
type Engine struct {
	mu       sync.Mutex
	branches map[string]*branch
}

func NewEngine() *Engine { return &Engine{branches: map[string]*branch{}} }
func (e *Engine) Acquire(key string) (*branch, func()) {
	e.mu.Lock()
	if len(e.branches) >= 2048 {
		var oldest string
		var at time.Time
		for k, b := range e.branches {
			if b.refs == 0 && (oldest == "" || b.Last.Before(at)) {
				oldest = k
				at = b.Last
			}
		}
		if oldest != "" {
			delete(e.branches, oldest)
		}
	}
	b := e.branches[key]
	if b == nil {
		b = &branch{Cache: map[string][]cacheEntry{}}
		e.branches[key] = b
	}
	b.refs++
	e.mu.Unlock()
	b.mu.Lock()
	return b, func() { b.mu.Unlock(); e.mu.Lock(); b.refs--; b.Last = time.Now(); e.mu.Unlock() }
}
func Rule(r Request) (string, string, float64) {
	if r.ComplexContent {
		return "high", "content_requires_high_capability", 1
	}
	if r.ToolFailureCount() > 0 {
		return "high", "tool_error_signal", 0.8
	}
	text := strings.ToLower(r.Text)
	for _, term := range []string{"architecture", "deadlock", "race condition", "security", "migration", "distributed", "跨模块", "架构", "死锁", "并发", "安全漏洞", "分布式", "重构", "又错", "仍然失败"} {
		if strings.Contains(text, term) {
			return "high", "complexity_rule", 0.85
		}
	}
	if len(text) > 0 && len(text) < 1800 {
		for _, term := range []string{"explain", "summarize", "translate", "rename", "typo", "format", "解释", "总结", "翻译", "重命名", "拼写", "格式", "注释"} {
			if strings.Contains(text, term) {
				return "low", "simple_task_rule", 0.8
			}
		}
	}
	return "", "uncertain", 0
}
func cachedTokens(r Request, p Provider, entries []cacheEntry, now time.Time) int {
	if p.Cache.Kind == "none" {
		return 0
	}
	if p.Cache.Kind == "explicit" && !r.CacheEnabled {
		return 0
	}
	best := 0
	for _, e := range entries {
		if now.Sub(e.At) >= time.Duration(p.Cache.TTLSeconds)*time.Second {
			continue
		}
		for _, prefix := range r.Prefixes {
			if e.Prefix == prefix && e.Tokens > best {
				best = e.Tokens
			}
		}
	}
	if best > r.Tokens {
		best = r.Tokens
	}
	// Local prefixes are predictive. Discount observed coverage for uncertainty.
	return best * 8 / 10
}
func Estimate(r Request, p Provider, cached, output int) *float64 {
	if p.Prices == nil {
		return nil
	}
	if r.MaxTokens > 0 && output > r.MaxTokens {
		output = r.MaxTokens
	}
	writeRate := p.Prices.Input
	if p.Cache.Kind == "explicit" && r.CacheEnabled {
		writeRate = p.Prices.CacheWrite5m
		if r.Cache1h {
			writeRate = p.Prices.CacheWrite1h
		}
	}
	cost := (float64(r.Tokens-cached)*writeRate + float64(cached)*p.Prices.CacheRead + float64(output)*p.Prices.Output) / 1e6
	return &cost
}
func Decide(c Config, r Request, b *branch, recommend, reason string, confidence float64) Decision {
	d := Decision{Recommended: recommend, Confidence: confidence, Reasons: []string{reason}, Classifier: "rules"}
	if d.Recommended == "" {
		d.Recommended = "high"
		d.Reasons = append(d.Reasons, "uncertain_use_high")
	}
	now := time.Now()
	d.HighCached = cachedTokens(r, c.High, b.Cache["high"], now)
	d.LowCached = cachedTokens(r, c.Low, b.Cache["low"], now)
	high, low := c.High, c.Low
	high.Prices, _ = c.High.Prices.At(now)
	low.Prices, _ = c.Low.Prices.At(now)
	d.HighNativeCost = Estimate(r, high, d.HighCached, c.Policy.EstimatedOutputTokens)
	d.LowNativeCost = Estimate(r, low, d.LowCached, c.Policy.EstimatedOutputTokens)
	if high.Prices != nil {
		d.HighCurrency = high.Prices.Currency
	}
	if low.Prices != nil {
		d.LowCurrency = low.Prices.Currency
	}
	if d.HighCurrency == "USD" {
		d.HighCost = d.HighNativeCost
	}
	if d.LowCurrency == "USD" {
		d.LowCost = d.LowNativeCost
	}
	d.CostComparable = d.HighNativeCost != nil && d.LowNativeCost != nil && d.HighCurrency == d.LowCurrency
	output := r.MaxTokens
	if output == 0 {
		output = c.Policy.EstimatedOutputTokens
	}
	eligibleHigh := r.Tokens+output <= c.High.ContextTokens
	eligibleLow := r.Tokens+output <= c.Low.ContextTokens && !r.ComplexContent
	// High's compatibility with these features still needs a real-provider preflight.
	if r.Protected {
		eligibleLow = false
	}
	d.Proposed = d.Recommended
	if d.Recommended == "low" {
		if d.LowNativeCost == nil || d.HighNativeCost == nil {
			d.Reasons = append(d.Reasons, "prices_unknown_use_recommendation")
		} else if !d.CostComparable {
			d.Reasons = append(d.Reasons, "currency_not_comparable")
		} else if *d.HighNativeCost <= *d.LowNativeCost {
			d.Proposed = "high"
			d.Reasons = append(d.Reasons, "high_estimated_cheaper")
		}
	}
	if d.Proposed == "low" && !eligibleLow {
		d.Proposed = "high"
		d.Reasons = append(d.Reasons, "low_capability_or_context_limit")
	}
	if b.Current == "high" && d.Proposed == "low" && d.CostComparable && *d.HighNativeCost > 0 {
		if (*d.HighNativeCost-*d.LowNativeCost) / *d.HighNativeCost < c.Policy.MinSwitchSaving {
			d.Proposed = "high"
			d.Reasons = append(d.Reasons, "switch_saving_below_threshold")
		}
	}
	// For supported history from low, protection means keep low rather than upgrade.
	if r.HasHistory {
		if b.Current == "" && (r.Protected || !c.Policy.AllowTextSwitch) {
			if c.Mode == "auto" {
				d.Error = "unknown_history_origin"
				d.Reasons = append(d.Reasons, "resume_with_fixed_model_to_establish_origin")
			}
		} else if b.Current != "" && (r.Protected || !c.Policy.AllowTextSwitch) && d.Proposed != b.Current {
			d.Proposed = b.Current
			d.Reasons = append(d.Reasons, "history_compatibility_lock")
		}
	}
	d.Selected = d.Proposed
	switch c.Mode {
	case "shadow":
		d.Selected = "high"
		d.Reasons = append(d.Reasons, "shadow_executes_high")
	case "high", "low":
		d.Selected = c.Mode
		d.Reasons = append(d.Reasons, "fixed_mode")
	}
	// Fixed mode is the user's explicit assertion of history compatibility.
	if c.Mode == "low" {
		eligibleLow = r.Tokens+output <= c.Low.ContextTokens
	}
	if d.Selected == "low" && b.Current == "low" && r.Protected {
		eligibleLow = r.Tokens+output <= c.Low.ContextTokens
	}
	if (d.Selected == "high" && !eligibleHigh) || (d.Selected == "low" && !eligibleLow) {
		d.Error = "selected_model_capability_or_context_limit"
	}
	return d
}
func Observe(b *branch, r Request, p Provider, tier string, u Usage, at time.Time, complete bool) {
	// A partial response can carry history back to the client: retain its owner.
	b.Current = tier
	if !complete || len(r.Prefixes) == 0 {
		return
	}
	n := int(num(u.Read) + num(u.Write))
	if n <= 0 {
		return
	}
	entries := append(b.Cache[tier], cacheEntry{r.Prefixes[len(r.Prefixes)-1], n, at})
	if len(entries) > 16 {
		entries = entries[len(entries)-16:]
	}
	b.Cache[tier] = entries
}
