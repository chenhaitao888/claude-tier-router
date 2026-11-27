package router

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const RoutingVersion = "continuation-batches-v1"

type Decision struct {
	RoutingVersion         string          `json:"routing_version,omitempty"`
	Recommended            string          `json:"recommended"`
	Proposed               string          `json:"proposed"`
	Selected               string          `json:"selected"`
	Reasons                []string        `json:"reasons"`
	Classifier             string          `json:"classifier"`
	Confidence             float64         `json:"confidence"`
	HighCost               *float64        `json:"estimated_high_usd"`
	LowCost                *float64        `json:"estimated_low_usd"`
	HighNativeCost         *float64        `json:"estimated_high_cost"`
	LowNativeCost          *float64        `json:"estimated_low_cost"`
	HighCurrency           string          `json:"estimated_high_currency"`
	LowCurrency            string          `json:"estimated_low_currency"`
	CostComparable         bool            `json:"cost_comparable"`
	HighCached             int             `json:"estimated_high_cached_tokens"`
	LowCached              int             `json:"estimated_low_cached_tokens"`
	HighCache              CacheDiagnostic `json:"high_cache_diagnostic"`
	LowCache               CacheDiagnostic `json:"low_cache_diagnostic"`
	SwitchAllowed          bool            `json:"switch_allowed"`
	SwitchBlockedReason    string          `json:"switch_blocked_reason,omitempty"`
	ClassifierSkipReason   string          `json:"classifier_skip_reason,omitempty"`
	TaskChars              int             `json:"task_chars"`
	TaskExtraction         string          `json:"task_extraction,omitempty"`
	ProtectionReasons      []string        `json:"protection_reasons,omitempty"`
	ComparisonCurrency     string          `json:"comparison_currency,omitempty"`
	ComparisonHigh         *float64        `json:"comparison_high_cost"`
	ComparisonLow          *float64        `json:"comparison_low_cost"`
	ExchangeRate           *ExchangeRate   `json:"exchange_rate_snapshot,omitempty"`
	CachePredictionVersion string          `json:"cache_prediction_version,omitempty"`
	ContextBudget          *ContextBudget  `json:"context_budget,omitempty"`
	Error                  string          `json:"error,omitempty"`
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
	Shapes  map[string]map[string]string
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
	if r.CooperationRole == "coordinator" && r.CooperationDelegated {
		if !r.ComplexContent && r.ToolFailureCount() == 0 {
			switch r.CooperationFollowup {
			case formatResults, writeApproved:
				return "low", "cooperation_" + r.CooperationFollowup, 1
			}
		}
		return "high", "cooperation_coordination", 1
	}
	if r.ComplexContent {
		return "high", "content_requires_high_capability", 1
	}
	if r.ToolFailureCount() > 0 {
		return "high", "tool_error_signal", 0.8
	}
	text := strings.ToLower(r.Text)
	// A bounded identifier lookup may mention the architecture it searches.
	// It is not a design request. Do not apply this exception to background
	// prompts (which may include security/permission checks) or compaction.
	if (r.RequestClass == "" || r.RequestClass == "main" || r.RequestClass == "subagent") && isLookupTask(text) {
		return "low", "lookup_task_rule", 0.8
	}
	for _, term := range []string{"architecture", "deadlock", "race condition", "security", "migration", "distributed", "跨模块", "架构", "死锁", "并发", "安全漏洞", "分布式", "重构", "又错", "仍然失败"} {
		if strings.Contains(text, term) {
			return "high", "complexity_rule", 0.85
		}
	}
	if len(text) > 0 && utf8.RuneCountInString(text) < 1800 {
		for _, term := range []string{"explain", "summarize", "translate", "rename", "typo", "format", "解释", "总结", "翻译", "重命名", "拼写", "格式", "注释"} {
			if strings.Contains(text, term) {
				return "low", "simple_task_rule", 0.8
			}
		}
	}
	return "", "uncertain", 0
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
	cacheable := r.Tokens
	if p.Cache.Kind == "explicit" && len(r.CacheBreakpoints) > 0 {
		cacheable = r.CacheableTokens
	}
	cached = max(0, min(cached, cacheable))
	write := max(0, cacheable-cached)
	ordinary := max(0, r.Tokens-cacheable)
	cost := (float64(write)*writeRate + float64(ordinary)*p.Prices.Input + float64(cached)*p.Prices.CacheRead + float64(output)*p.Prices.Output) / 1e6
	return &cost
}
func Decide(c Config, r Request, b *branch, recommend, reason string, confidence float64) Decision {
	d := Decision{RoutingVersion: RoutingVersion, Recommended: recommend, Confidence: confidence, Reasons: []string{reason}, Classifier: "rules", TaskChars: utf8.RuneCountInString(r.Text), TaskExtraction: r.TaskExtraction, ProtectionReasons: r.ProtectionReasons, CachePredictionVersion: "block-prefix-v4"}
	_, d.SwitchBlockedReason = historyConstraint(c, r, b)
	d.SwitchAllowed = d.SwitchBlockedReason == ""
	if override := thinkingOverrideReason(c, r); override != "" {
		d.Reasons = append(d.Reasons, override)
	}
	if d.Recommended == "" {
		d.Recommended = "high"
		d.Reasons = append(d.Reasons, "uncertain_use_high")
	}
	now := time.Now()
	d.HighCached, d.HighCache = predictCache(r, c.High, b.Cache["high"], b.Shapes["high"], now)
	d.LowCached, d.LowCache = predictCache(r, c.Low, b.Cache["low"], b.Shapes["low"], now)
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
	if d.CostComparable {
		d.ComparisonHigh, d.ComparisonLow, d.ComparisonCurrency = d.HighNativeCost, d.LowNativeCost, d.HighCurrency
	} else if c.Policy.ExchangeRate != nil && c.Policy.ExchangeRate.Validate() == nil {
		d.ComparisonHigh = c.Policy.ExchangeRate.usd(d.HighNativeCost, d.HighCurrency)
		d.ComparisonLow = c.Policy.ExchangeRate.usd(d.LowNativeCost, d.LowCurrency)
		if d.ComparisonHigh != nil && d.ComparisonLow != nil {
			d.CostComparable = true
			d.ComparisonCurrency = "USD"
			snapshot := *c.Policy.ExchangeRate
			d.ExchangeRate = &snapshot
			d.Reasons = append(d.Reasons, "configured_exchange_rate")
		}
	}
	output := r.MaxTokens
	if output == 0 {
		output = c.Policy.EstimatedOutputTokens
	}
	d.ContextBudget = &ContextBudget{EstimatedInput: r.Tokens, ReservedOutput: output, EstimatedTotal: r.Tokens + output, HighLimit: c.High.ContextTokens, LowLimit: c.Low.ContextTokens, EstimateMethod: "request_bytes_div_2_excluding_known_metadata"}
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
		} else if *d.ComparisonHigh <= *d.ComparisonLow {
			d.Proposed = "high"
			d.Reasons = append(d.Reasons, "high_estimated_cheaper")
		}
	}
	if d.Proposed == "low" && !eligibleLow {
		d.Proposed = "high"
		d.Reasons = append(d.Reasons, lowRestriction(c, r))
	}
	if b.Current == "high" && d.Proposed == "low" && d.CostComparable && *d.ComparisonHigh > 0 {
		if (*d.ComparisonHigh-*d.ComparisonLow) / *d.ComparisonHigh < c.Policy.MinSwitchSaving {
			d.Proposed = "high"
			d.Reasons = append(d.Reasons, "switch_saving_below_threshold")
		}
	}
	// For supported history from low, protection means keep low rather than upgrade.
	if d.SwitchBlockedReason != "" {
		d.Reasons = append(d.Reasons, d.SwitchBlockedReason)
		if b.Current == "" {
			if c.Mode == "auto" {
				d.Error = "unknown_history_origin"
				d.Reasons = append(d.Reasons, "resume_with_fixed_model_to_establish_origin")
			}
		} else if d.Proposed != b.Current {
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
	if d.Selected == "low" && b.Current == "low" && r.Protected && c.Mode != "low" {
		// Same-source history permits continuing its protected blocks, but does
		// not grant support for images, documents or other high-only content.
		eligibleLow = r.Tokens+output <= c.Low.ContextTokens && !r.ComplexContent
	}
	if d.Error == "" && ((d.Selected == "high" && !eligibleHigh) || (d.Selected == "low" && !eligibleLow)) {
		switch {
		case output >= d.ContextBudget.limit(d.Selected):
			d.Error = "output_budget_exceeds_context"
		case r.Tokens+output > d.ContextBudget.limit(d.Selected):
			d.Error = "selected_model_context_limit"
		default:
			d.Error = "selected_model_content_unsupported"
		}
	}
	return d
}

// Local complexity and content capability rules may upgrade despite thinking history.
// This is a routing policy override, not a claim that foreign signatures are
// compatible. Forwarding still preserves every block; upstream errors surface.
// Server-side state and unknown protection types retain their history lock.
func thinkingOverrideReason(c Config, r Request) string {
	if c.Mode != "auto" || !r.HasHistory || !r.Protected || len(r.ProtectionReasons) == 0 {
		return ""
	}
	for _, kind := range r.ProtectionReasons {
		if kind != "thinking" && kind != "redacted_thinking" {
			return ""
		}
	}
	tier, reason, _ := Rule(r)
	if tier == "high" {
		switch reason {
		case "complexity_rule":
			return "complexity_overrides_thinking"
		case "content_requires_high_capability":
			return "capability_overrides_thinking"
		case "cooperation_coordination":
			return "cooperation_coordination_overrides_thinking"
		}
	}
	return ""
}

// A new prompt ID alone does not clear protected history. In auto mode only
// explicit complexity/capability rules can override a thinking-only history lock.
func historyConstraint(c Config, r Request, b *branch) (string, string) {
	if !r.HasHistory {
		return "", ""
	}
	if thinkingOverrideReason(c, r) != "" {
		return "", ""
	}
	if !r.Protected && c.Policy.AllowTextSwitch {
		return "", ""
	}
	if b.Current == "" {
		return "", "unknown_history_origin"
	}
	if r.Protected {
		return b.Current, "protected_history_requires_same_model"
	}
	return b.Current, "text_history_switch_disabled"
}

func lowRestriction(c Config, r Request) string {
	if r.Protected {
		return "low_protected_content"
	}
	if r.ComplexContent {
		return "low_unsupported_content"
	}
	output := r.MaxTokens
	if output == 0 {
		output = c.Policy.EstimatedOutputTokens
	}
	if r.Tokens+output > c.Low.ContextTokens {
		return "low_context_limit"
	}
	return ""
}

func classifierSkip(c Config, r Request, b *branch, tier string) string {
	if c.Mode == "high" || c.Mode == "low" {
		return "fixed_mode"
	}
	if thinkingOverrideReason(c, r) != "" {
		return "rule_decided"
	}
	if _, reason := historyConstraint(c, r, b); reason != "" {
		return reason
	}
	if reason := lowRestriction(c, r); reason != "" {
		return reason
	}
	output := r.MaxTokens
	if output == 0 {
		output = c.Policy.EstimatedOutputTokens
	}
	if r.Tokens+output > c.High.ContextTokens {
		return "high_context_limit"
	}
	if tier != "" {
		return "rule_decided"
	}
	if !c.Classifier.Enabled {
		return "classifier_disabled"
	}
	return ""
}
