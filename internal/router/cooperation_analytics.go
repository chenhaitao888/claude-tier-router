package router

type CooperationStage struct {
	Usage        UsageScope         `json:"usage"`
	Costs        map[string]float64 `json:"costs"`
	UnknownCosts int                `json:"unknown_cost_requests"`
	Undispatched int                `json:"undispatched_requests"`
	DurationMS   int64              `json:"request_duration_ms"`
}

type CooperationSummary struct {
	Stages          map[string]*CooperationStage `json:"stages"`
	Executors       int                          `json:"execution_agents"`
	Reviewers       int                          `json:"review_agents"`
	NeedsDesign     int                          `json:"needs_design_reports"`
	ChangesRequired int                          `json:"changes_required_reports"`
	Resumes         int                          `json:"observed_resumes"`
	Outcomes        map[string]int               `json:"model_reported_outcomes"`
}

func addCooperationStage(stages map[string]*CooperationStage, r Record) {
	if r.Cooperation == nil {
		return
	}
	stage := stages[r.Cooperation.Stage]
	if stage == nil {
		stage = &CooperationStage{Costs: map[string]float64{}}
		stages[r.Cooperation.Stage] = stage
	}
	stage.DurationMS += max(0, r.DurationMS)
	if r.Actual == "" {
		stage.Undispatched++
		return
	}
	stage.Usage.add(r)
	if money := r.recordedCost(); money != nil {
		stage.Costs[money.Currency] += money.Amount
	} else {
		stage.UnknownCosts++
	}
}
func finishCooperationStages(stages map[string]*CooperationStage) {
	for _, stage := range stages {
		stage.Usage.finish()
	}
}
func (s *CooperationSummary) addRun(run CooperationRun) {
	if run.Role == "executor" {
		s.Executors++
	} else if run.Role == "reviewer" {
		s.Reviewers++
	}
	s.NeedsDesign += run.NeedsDesign
	s.ChangesRequired += run.ChangesRequired
	s.Resumes += max(0, run.Attempts-1)
	s.Outcomes[run.Role+":"+run.Status]++
}
