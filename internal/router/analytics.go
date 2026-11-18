package router

import "encoding/json"

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
	Requests           int `json:"inference_requests"`
	ClassifierRequests int `json:"classifier_requests"`
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
		KnownCosts: map[string]float64{}, ClassifierCosts: map[string]float64{}, EstimatedCosts: map[string]float64{},
		Scopes: map[string]*UsageScope{"all": {}, "main": {}, "background": {}, "classifier": {}},
	}
	rows, err := s.db.Query("SELECT payload FROM records")
	if err != nil {
		return out, err
	}
	tasks := map[string]bool{}
	for rows.Next() {
		var b string
		if err = rows.Scan(&b); err != nil {
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
		if r.Prompt != "" {
			tasks[r.Session+"\x00"+r.Prompt] = true
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
	out.KnownCostUSD = out.KnownCosts["USD"]
	out.ClassifierCostUSD = out.ClassifierCosts["USD"]
	feedback, err := s.Feedbacks()
	if err != nil {
		return out, err
	}
	for _, f := range feedback {
		if tasks[f.Session+"\x00"+f.Prompt] {
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
