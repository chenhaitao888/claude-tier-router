package router

import (
	"encoding/json"
	"fmt"
)

// Provider usage fields are disjoint in the supported Anthropic contract.
// Streaming usage snapshots replace fields, never add cumulative values.
type Usage struct {
	Input        *int64 `json:"input_tokens"`
	Read         *int64 `json:"cache_read_input_tokens"`
	Write        *int64 `json:"cache_creation_input_tokens"`
	Output       *int64 `json:"output_tokens"`
	Write5m      *int64 `json:"write_5m_tokens"`
	Write1h      *int64 `json:"write_1h_tokens"`
	ExtraCharges bool   `json:"extra_charges_unpriced"`
}

func (u *Usage) Merge(raw json.RawMessage) error {
	var m map[string]json.RawMessage
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	for key, dst := range map[string]**int64{"input_tokens": &u.Input, "cache_read_input_tokens": &u.Read, "cache_creation_input_tokens": &u.Write, "output_tokens": &u.Output} {
		if b, ok := m[key]; ok && string(b) != "null" {
			var n int64
			if json.Unmarshal(b, &n) != nil || n < 0 {
				return fmt.Errorf("invalid usage %s", key)
			}
			*dst = &n
		}
	}
	if b, ok := m["cache_creation"]; ok {
		var v struct {
			Five *int64 `json:"ephemeral_5m_input_tokens"`
			Hour *int64 `json:"ephemeral_1h_input_tokens"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		if v.Five != nil {
			if *v.Five < 0 {
				return fmt.Errorf("negative cache write")
			}
			u.Write5m = v.Five
		}
		if v.Hour != nil {
			if *v.Hour < 0 {
				return fmt.Errorf("negative cache write")
			}
			u.Write1h = v.Hour
		}
	}
	if b, ok := m["server_tool_use"]; ok {
		var counts map[string]int64
		if json.Unmarshal(b, &counts) == nil {
			for _, n := range counts {
				if n > 0 {
					u.ExtraCharges = true
				}
			}
		}
	}
	return nil
}
func num(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
func (u Usage) TotalInput() int64 { return num(u.Input) + num(u.Read) + num(u.Write) }
func (u Usage) Cost(p Provider, complete bool) (*float64, string) {
	if !complete {
		return nil, "incomplete_response"
	}
	if u.Input == nil || u.Output == nil || u.Read == nil || u.Write == nil {
		return nil, "missing_usage"
	}
	if p.Prices == nil {
		return nil, "prices_not_configured"
	}
	if u.ExtraCharges {
		return nil, "unpriced_server_tools"
	}
	five, hour := num(u.Write5m), num(u.Write1h)
	if num(u.Write) > 0 && (u.Write5m == nil || u.Write1h == nil) {
		return nil, "cache_write_duration_unknown"
	}
	if five+hour != num(u.Write) {
		return nil, "inconsistent_cache_usage"
	}
	pr := p.Prices
	cost := (float64(*u.Input)*pr.Input + float64(*u.Read)*pr.CacheRead + float64(five)*pr.CacheWrite5m + float64(hour)*pr.CacheWrite1h + float64(*u.Output)*pr.Output) / 1e6
	return &cost, "usage_calculated"
}
