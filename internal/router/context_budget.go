package router

import "fmt"

// These are local estimates and configured limits, not provider token counts.
type ContextBudget struct {
	EstimatedInput int    `json:"estimated_input_tokens"`
	ReservedOutput int    `json:"reserved_output_tokens"`
	EstimatedTotal int    `json:"estimated_total_tokens"`
	HighLimit      int    `json:"configured_high_context_tokens"`
	LowLimit       int    `json:"configured_low_context_tokens"`
	EstimateMethod string `json:"estimate_method"`
}

func (b ContextBudget) limit(tier string) int {
	if tier == "low" {
		return b.LowLimit
	}
	return b.HighLimit
}

// Claude Code recognizes a 400 invalid_request_error with "prompt is too long"
// as a context failure. A generic 409 api_error bypasses that recovery path.
// Do not advertise content/history incompatibility as a context failure.
func decisionAPIError(d Decision) (status int, kind, message string) {
	if d.Error == "cooperation_executor_budget" {
		return 400, "invalid_request_error", "Router executor request budget reached. Stop this worker and return the blocker to the coordinator; review the plan before starting another bounded task."
	}
	if d.ContextBudget != nil {
		b := d.ContextBudget
		switch d.Error {
		case "selected_model_context_limit":
			return 400, "invalid_request_error", fmt.Sprintf("prompt is too long: Router estimates %d input tokens + %d reserved output tokens > %d configured context tokens for %s; this is a local estimate, not a provider token count", b.EstimatedInput, b.ReservedOutput, b.limit(d.Selected), d.Selected)
		case "output_budget_exceeds_context":
			return 400, "invalid_request_error", fmt.Sprintf("max_tokens (%d) must be less than the configured context window (%d) for %s; reduce max_tokens or correct context_tokens", b.ReservedOutput, b.limit(d.Selected), d.Selected)
		}
	}
	return 409, "api_error", d.Error
}
