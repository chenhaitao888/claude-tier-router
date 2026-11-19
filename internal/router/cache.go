package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Prefixes follow the provider's tools -> system -> messages order. Block
// boundaries survive a growing/merged message and a moved cache breakpoint.
// Do not normalize text, tool arguments or thinking signatures.
func (r *Request) buildPrefixes() {
	seed := map[string]json.RawMessage{}
	for k, v := range r.Fields {
		switch k {
		case "model", "tools", "system", "messages", "stream", "max_tokens", "metadata", "cache_control", "temperature", "top_p", "top_k", "stop_sequences":
		default:
			seed[k] = v // Unknown options conservatively invalidate predictions.
		}
	}
	// This is classifier-side context, not the generation prompt. Claude Code
	// updates prior_turn_context after shell calls. Keep the safeguard type and
	// all unknown fields/versions in the key, and NEVER change forwarded bytes.
	if raw, ok := seed["safeguards"]; ok {
		normalized := cacheSafeguards(raw)
		seed["safeguards"] = normalized
		r.NonPromptBytes = max(0, len(raw)-len(normalized))
		// The same metadata must not inflate the generation-input estimate.
		r.Tokens = max(1, (len(r.Raw)-r.NonPromptBytes)/2+1)
	}
	h := sha256.New()
	h.Write(fingerprintJSON(seed))
	r.CacheShape = map[string]string{}
	for name, value := range map[string]any{"parameters": seed, "tools": r.Fields["tools"], "system": r.Fields["system"]} {
		digest := sha256.Sum256(fingerprintJSON(value))
		r.CacheShape[name] = hex.EncodeToString(digest[:])
	}
	digest := sha256.Sum256(fingerprintJSON(r.Fields["safeguards"]))
	r.CacheShape["classifier_context"] = hex.EncodeToString(digest[:])
	count := 0
	add := func(section, role string, block any) {
		data := fingerprintJSON([]any{section, role, block})
		h.Write(data)
		count += len(data)/2 + 1
		r.Prefixes = append(r.Prefixes, hex.EncodeToString(h.Sum(nil)))
		r.PrefixTokens = append(r.PrefixTokens, count)
		marked := false
		if obj, ok := block.(map[string]any); ok {
			_, marked = obj["cache_control"]
		}
		r.CacheBreakpoints = append(r.CacheBreakpoints, marked)
		if marked {
			r.CacheableTokens = count
		}
	}
	var tools []any
	json.Unmarshal(r.Fields["tools"], &tools)
	for _, tool := range tools {
		add("tools", "", tool)
	}
	blocks := func(section, role string, raw json.RawMessage) {
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return
		}
		if text, ok := value.(string); ok {
			value = []any{map[string]any{"type": "text", "text": text}}
		}
		if items, ok := value.([]any); ok {
			for _, item := range items {
				add(section, role, item)
			}
		}
	}
	blocks("system", "", r.Fields["system"])
	for _, msg := range r.Messages {
		blocks("messages", msg.Role, msg.Content)
	}
	if len(r.CacheBreakpoints) > 0 {
		r.CacheShape["last_prefix"] = r.Prefixes[len(r.Prefixes)-1]
		if raw, ok := r.Fields["cache_control"]; ok && string(raw) != "null" {
			r.CacheBreakpoints[len(r.CacheBreakpoints)-1] = true
			r.CacheableTokens = count
		}
	}
	// Relative prefix estimates use the same scale as the conservative request
	// estimate. Actual observed usage calibrates stored coverage after dispatch.
	if count > 0 {
		for i := range r.PrefixTokens {
			r.PrefixTokens[i] = r.PrefixTokens[i] * r.Tokens / count
		}
		r.CacheableTokens = r.CacheableTokens * r.Tokens / count
	}
}

func cacheSafeguards(raw json.RawMessage) json.RawMessage {
	var guards []map[string]any
	if json.Unmarshal(raw, &guards) != nil {
		return raw
	}
	changed := false
	for _, guard := range guards {
		ctx, ok := guard["classifier_context"].(map[string]any)
		if guard["type"] == "dangerous_tool_use" && ok && ctx["v"] == float64(1) {
			delete(guard, "classifier_context")
			changed = true
		}
	}
	if !changed {
		return raw
	}
	normalized, err := json.Marshal(guards)
	if err != nil {
		return raw
	}
	return normalized
}

type CacheDiagnostic struct {
	Reason                   string   `json:"reason"`
	Entries                  int      `json:"entries"`
	LiveEntries              int      `json:"live_entries"`
	MatchedPrefixes          int      `json:"matched_prefixes"`
	OutsideLookback          int      `json:"outside_lookback"`
	ChangedSections          []string `json:"changed_sections,omitempty"`
	ClassifierContextChanged bool     `json:"classifier_context_changed"`
	NonPromptBytes           int      `json:"classifier_context_bytes_excluded"`
}

func predictCache(r Request, p Provider, entries []cacheEntry, previous map[string]string, now time.Time) (int, CacheDiagnostic) {
	d := CacheDiagnostic{Entries: len(entries), NonPromptBytes: r.NonPromptBytes}
	d.ClassifierContextChanged = previous["classifier_context"] != "" && previous["classifier_context"] != r.CacheShape["classifier_context"]
	for _, name := range []string{"parameters", "tools", "system"} {
		if previous[name] != "" && previous[name] != r.CacheShape[name] {
			d.ChangedSections = append(d.ChangedSections, name)
		}
	}
	if p.Cache.Kind == "none" || (p.Cache.Kind == "explicit" && !r.CacheEnabled) {
		d.Reason = "cache_disabled"
		return 0, d
	}
	if len(entries) == 0 {
		d.Reason = "no_local_evidence"
		return 0, d
	}
	eligible := make(map[string]bool, len(r.Prefixes))
	all := make(map[string]bool, len(r.Prefixes))
	for i, prefix := range r.Prefixes {
		all[prefix] = true
		if p.Cache.Kind == "automatic" || len(r.CacheBreakpoints) == 0 {
			eligible[prefix] = true
			continue
		}
		// Explicit lookback is limited to 20 blocks before each breakpoint.
		for j := i; j < len(r.CacheBreakpoints) && j < i+20; j++ {
			if r.CacheBreakpoints[j] {
				eligible[prefix] = true
				break
			}
		}
	}
	if len(d.ChangedSections) == 0 && previous["last_prefix"] != "" && !all[previous["last_prefix"]] {
		d.ChangedSections = append(d.ChangedSections, "message_history")
	}
	best := 0
	for _, entry := range entries {
		age := now.Sub(entry.At)
		if age < 0 || age >= time.Duration(p.Cache.TTLSeconds)*time.Second {
			continue
		}
		d.LiveEntries++
		if all[entry.Prefix] && !eligible[entry.Prefix] {
			d.OutsideLookback++
		}
		if eligible[entry.Prefix] {
			d.MatchedPrefixes++
		}
		if eligible[entry.Prefix] && entry.Tokens > best {
			best = entry.Tokens
		}
	}
	if best > r.Tokens {
		best = r.Tokens
	}
	d.Reason = "prefix_match"
	if best == 0 {
		switch {
		case d.LiveEntries == 0:
			d.Reason = "evidence_expired"
		case d.OutsideLookback > 0:
			d.Reason = "outside_lookback"
		default:
			d.Reason = "prefix_changed"
		}
	}
	return best * 8 / 10, d // Prediction, not a guarantee of a provider hit.
}

func cachedTokens(r Request, p Provider, entries []cacheEntry, now time.Time) int {
	n, _ := predictCache(r, p, entries, nil, now)
	return n
}

func Observe(b *branch, r Request, p Provider, tier string, u Usage, at time.Time, complete bool) string {
	// A partial response can carry protected history back to the client.
	b.Current = tier
	if !complete {
		return "response_not_eligible"
	}
	if p.Cache.Kind == "none" {
		return "cache_disabled"
	}
	if len(r.Prefixes) == 0 {
		return "no_prefixes"
	}
	if !u.HasAllTokens() {
		return "usage_incomplete"
	}
	if b.Shapes == nil {
		b.Shapes = map[string]map[string]string{}
	}
	b.Shapes[tier] = r.CacheShape
	last := len(r.Prefixes) - 1
	n := int(num(u.Read) + num(u.Write))
	if p.Cache.Kind == "automatic" {
		// Automatic providers cache input even on a cold request; no explicit
		// cache_creation field is needed to establish a reusable prefix.
		n = int(u.TotalInput())
	} else {
		last = -1
		for i, marked := range r.CacheBreakpoints {
			if marked {
				last = i
			}
		}
		if last < 0 {
			return "no_cache_breakpoint"
		}
		if n <= 0 {
			return "no_cache_usage"
		}
	}
	if n <= 0 || len(r.PrefixTokens) <= last || r.PrefixTokens[last] <= 0 {
		return "coverage_unavailable"
	}
	entries := b.Cache[tier]
	for i := 0; i <= last; i++ {
		// A changed tail may leave only a shorter cached block reusable. Usage
		// doesn't give per-block counts, so allocate observed coverage by size.
		tokens := n * r.PrefixTokens[i] / r.PrefixTokens[last]
		entry := cacheEntry{r.Prefixes[i], tokens, at}
		found := false
		for j := range entries {
			if entries[j].Prefix == entry.Prefix {
				entries[j] = entry
				found = true
				break
			}
		}
		if !found {
			entries = append(entries, entry)
			if len(entries) > 256 {
				entries = entries[len(entries)-256:]
			}
		}
	}
	b.Cache[tier] = entries
	return "evidence_updated"
}
