package router

import (
	"encoding/json"
	"strings"
)

type ModelUsage struct {
	Requests             int      `json:"requests"`
	TokenRequests        int      `json:"token_requests"`
	MissingTokenRequests int      `json:"missing_token_requests"`
	Input                int64    `json:"input_tokens"`
	Output               int64    `json:"output_tokens"`
	CacheRead            int64    `json:"cache_read_tokens"`
	CacheWrite           int64    `json:"cache_write_tokens"`
	Total                int64    `json:"total_tokens"`
	RequestShare         *float64 `json:"request_share"`
	TokenShare           *float64 `json:"token_share"`
}
type UsageScope struct {
	Requests             int        `json:"requests"`
	Tokens               int64      `json:"total_tokens"`
	TokenRequests        int        `json:"token_requests"`
	MissingTokenRequests int        `json:"missing_token_requests"`
	High                 ModelUsage `json:"high"`
	Low                  ModelUsage `json:"low"`
}

func (s *UsageScope) add(r Record) {
	var bucket *ModelUsage
	switch r.Actual {
	case "high":
		bucket = &s.High
	case "low":
		bucket = &s.Low
	default:
		return
	}
	s.Requests++
	bucket.Requests++
	if !r.Complete || !r.Usage.HasAllTokens() {
		s.MissingTokenRequests++
		bucket.MissingTokenRequests++
		return
	}
	u := r.Usage
	s.TokenRequests++
	bucket.TokenRequests++
	bucket.Input += u.TotalInput()
	bucket.Output += num(u.Output)
	bucket.CacheRead += num(u.Read)
	bucket.CacheWrite += num(u.Write)
	total := u.TotalInput() + num(u.Output)
	bucket.Total += total
	s.Tokens += total
}
func (s *UsageScope) finish() {
	for _, m := range []*ModelUsage{&s.High, &s.Low} {
		if s.Requests > 0 {
			v := float64(m.Requests) / float64(s.Requests)
			m.RequestShare = &v
		}
		if s.Tokens > 0 && (m.TokenRequests > 0 || m.Requests == 0) {
			v := float64(m.Total) / float64(s.Tokens)
			m.TokenShare = &v
		}
	}
}

type Summary struct {
	Cooperation        CooperationSummary `json:"cooperation"`
	Requests           int                `json:"inference_requests"`
	ClassifierRequests int                `json:"classifier_requests"`
	// Compatibility fields contain USD only. Native currencies are never added.
	KnownCostUSD      float64                `json:"known_cost_usd"`
	ClassifierCostUSD float64                `json:"classifier_cost_usd"`
	KnownCosts        map[string]float64     `json:"known_costs"`
	ClassifierCosts   map[string]float64     `json:"classifier_costs"`
	EstimatedCosts    map[string]float64     `json:"retrospective_estimated_costs"`
	EstimatedRequests int                    `json:"retrospective_estimated_requests"`
	UnknownCost       int                    `json:"unknown_cost_requests"`
	UnestimatedCost   int                    `json:"unestimated_cost_requests"`
	Complete          int                    `json:"complete_requests"`
	High              int                    `json:"high_requests"`
	Low               int                    `json:"low_requests"`
	Undispatched      int                    `json:"undispatched_requests"`
	CacheRead         int64                  `json:"cache_read_tokens"`
	InputTotal        int64                  `json:"input_tokens_total"`
	OutputTotal       int64                  `json:"output_tokens_total"`
	Tasks             int                    `json:"tasks"`
	Passed            int                    `json:"passed_tasks"`
	Failed            int                    `json:"failed_tasks"`
	Unevaluated       int                    `json:"unevaluated_tasks"`
	Scopes            map[string]*UsageScope `json:"usage_scopes"`
	RoutingHealth     RoutingHealth          `json:"routing_health"`
	EvidenceReviews   int                    `json:"evidence_reviews"`
	EvidencePassed    int                    `json:"evidence_passed"`
	EvidenceFailed    int                    `json:"evidence_failed"`
	EvidenceUnknown   int                    `json:"evidence_unknown"`
}

type RoutingHealth struct {
	InferenceRequests  int                              `json:"inference_requests"`
	ClassifierRequests int                              `json:"classifier_requests"`
	CacheExcluded      map[string]int                   `json:"cache_excluded"`
	OutputLimited      int                              `json:"output_limited_requests"`
	ClassifierOutcomes map[string]int                   `json:"classifier_outcomes"`
	ClassifierSkipped  map[string]int                   `json:"classifier_skipped"`
	SwitchBlocked      map[string]int                   `json:"switch_blocked"`
	CachePredictions   map[string]*CachePredictionStats `json:"cache_predictions"`
}

func newRoutingHealth() RoutingHealth {
	return RoutingHealth{ClassifierOutcomes: map[string]int{}, ClassifierSkipped: map[string]int{}, SwitchBlocked: map[string]int{}, CachePredictions: map[string]*CachePredictionStats{}, CacheExcluded: map[string]int{}}
}

type CachePredictionStats struct {
	Requests            int            `json:"requests"`
	Predicted           int64          `json:"predicted_read_tokens"`
	Observed            int64          `json:"observed_read_tokens"`
	ZeroPredictionHits  int            `json:"zero_prediction_hits"`
	PredictedHitsMissed int            `json:"predicted_hits_missed"`
	AbsoluteError       int64          `json:"absolute_error_tokens"`
	MissReasons         map[string]int `json:"zero_prediction_reasons"`
	MetadataChanges     int            `json:"classifier_context_changes"`
}

func (h *RoutingHealth) add(r Record) {
	if r.Kind == "classifier" {
		h.ClassifierRequests++
		status := "legacy_unclassified"
		if r.ClassifierDiagnostic != nil {
			status = r.ClassifierDiagnostic.Status
			if status == "invalid" || status == "failed" {
				status += ":" + r.ClassifierDiagnostic.Validation
			}
		} else if r.Error != "" {
			status = r.Error
		}
		h.ClassifierOutcomes[status]++
		return
	}
	if r.Kind != "inference" {
		return
	}
	h.InferenceRequests++
	if r.StopReason == "max_tokens" || r.StopReason == "model_context_window_exceeded" {
		h.OutputLimited++
	}
	if reason := r.Decision.ClassifierSkipReason; reason != "" {
		h.ClassifierSkipped[reason]++
	}
	if reason := r.Decision.SwitchBlockedReason; reason != "" {
		h.SwitchBlocked[reason]++
	}
	excluded := ""
	switch {
	case r.CostStatus == "in_progress":
		excluded = "in_progress"
	case r.Actual != "high" && r.Actual != "low":
		excluded = "not_dispatched"
	case !r.Complete:
		excluded = "incomplete_response"
	case !r.Usage.HasAllTokens():
		excluded = "missing_usage"
	}
	if excluded != "" {
		if h.CacheExcluded == nil {
			h.CacheExcluded = map[string]int{}
		}
		h.CacheExcluded[excluded]++
		return
	}
	version := r.Decision.CachePredictionVersion
	if version == "" {
		version = "legacy"
	}
	stats := h.CachePredictions[version]
	if stats == nil {
		stats = &CachePredictionStats{MissReasons: map[string]int{}}
		h.CachePredictions[version] = stats
	}
	predicted := int64(r.Decision.HighCached)
	diagnostic := r.Decision.HighCache
	if r.Actual == "low" {
		predicted = int64(r.Decision.LowCached)
		diagnostic = r.Decision.LowCache
	}
	observed := num(r.Usage.Read)
	stats.Requests++
	stats.Predicted += predicted
	stats.Observed += observed
	if predicted == 0 && observed > 0 {
		stats.ZeroPredictionHits++
		reason := diagnostic.Reason
		if reason == "" {
			reason = "not_recorded"
		}
		if len(diagnostic.ChangedSections) > 0 {
			reason += ":" + strings.Join(diagnostic.ChangedSections, ",")
		}
		stats.MissReasons[reason]++
	}
	if diagnostic.ClassifierContextChanged {
		stats.MetadataChanges++
	}
	if predicted > 0 && observed == 0 {
		stats.PredictedHitsMissed++
	}
	errorTokens := predicted - observed
	if errorTokens < 0 {
		errorTokens = -errorTokens
	}
	stats.AbsoluteError += errorTokens
}

func (r Record) recordedCost() *Money {
	if r.CostAmount != nil && (r.CostCurrency == "USD" || r.CostCurrency == "CNY") {
		return &Money{*r.CostAmount, r.CostCurrency}
	}
	if r.CostUSD != nil {
		return &Money{*r.CostUSD, "USD"}
	}
	return nil
}

func (s *Store) Summary(configs ...Config) (Summary, error) {
	out := Summary{
		Cooperation: CooperationSummary{Stages: map[string]*CooperationStage{}, Outcomes: map[string]int{}},
		KnownCosts:  map[string]float64{}, ClassifierCosts: map[string]float64{}, EstimatedCosts: map[string]float64{},
		Scopes:        map[string]*UsageScope{"all": {}, "main": {}, "background": {}, "classifier": {}},
		RoutingHealth: newRoutingHealth(),
	}
	rows, err := s.db.Query("SELECT payload,root_prompt FROM task_record_roots")
	if err != nil {
		return out, err
	}
	tasks := map[taskKey]bool{}
	for rows.Next() {
		var b, root string
		if err = rows.Scan(&b, &root); err != nil {
			rows.Close()
			return out, err
		}
		var r Record
		if err = json.Unmarshal([]byte(b), &r); err != nil {
			rows.Close()
			return out, err
		}
		if len(configs) > 0 {
			r.EstimateUnpriced(configs[0])
		}
		out.RoutingHealth.add(r)
		addCooperationStage(out.Cooperation.Stages, r)
		if money := r.recordedCost(); money != nil {
			out.KnownCosts[money.Currency] += money.Amount
			if r.Kind == "classifier" {
				out.ClassifierCosts[money.Currency] += money.Amount
			}
		} else {
			out.UnknownCost++
			if r.Retrospective != nil {
				out.EstimatedRequests++
				out.EstimatedCosts[r.Retrospective.Currency] += r.Retrospective.Amount
			} else {
				out.UnestimatedCost++
			}
		}
		if r.Kind == "classifier" {
			out.ClassifierRequests++
			out.Scopes["classifier"].add(r)
			continue
		}
		out.Requests++
		if r.Complete {
			out.Complete++
		}
		switch r.Actual {
		case "high":
			out.High++
		case "low":
			out.Low++
		default:
			out.Undispatched++
		}
		out.Scopes["all"].add(r)
		scope := "main"
		if r.Class == "auxiliary" || r.Class == "compaction" {
			scope = "background"
		}
		out.Scopes[scope].add(r)
		out.CacheRead += num(r.Usage.Read)
		out.InputTotal += r.Usage.TotalInput()
		out.OutputTotal += num(r.Usage.Output)
		if r.Kind == "inference" && r.Session != "" && root != "" && r.Class != "auxiliary" && r.Class != "compaction" {
			tasks[taskKey{r.Session, root}] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, scope := range out.Scopes {
		scope.finish()
	}
	finishCooperationStages(out.Cooperation.Stages)
	runs, err := s.cooperationRuns()
	if err != nil {
		return out, err
	}
	for _, run := range runs {
		out.Cooperation.addRun(run)
	}
	out.KnownCostUSD = out.KnownCosts["USD"]
	out.ClassifierCostUSD = out.ClassifierCosts["USD"]
	feedback, err := s.Feedbacks()
	if err != nil {
		return out, err
	}
	for _, f := range feedback {
		if tasks[taskKey{f.Session, f.Prompt}] {
			if f.Rubric != "" {
				out.EvidenceReviews++
				switch f.Outcome {
				case "passed":
					out.EvidencePassed++
				case "failed":
					out.EvidenceFailed++
				default:
					out.EvidenceUnknown++
				}
			}
			switch f.Outcome {
			case "passed":
				out.Passed++
			case "failed":
				out.Failed++
			}
		}
	}
	out.Tasks = len(tasks)
	out.Unevaluated = out.Tasks - out.Passed - out.Failed
	return out, nil
}
