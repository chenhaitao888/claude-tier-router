package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func rubricFeedback() Feedback {
	f := Feedback{Session: "session", Prompt: "task", Rubric: "api_explanation"}
	for _, c := range QualityRubrics()[0].Criteria {
		f.Checks = append(f.Checks, QualityCheck{ID: c.ID, Outcome: "passed", Evidence: "fixture/source.go:10 and test output recorded"})
	}
	return f
}
func TestQualityRequiresCompleteChecksAndEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Feedback)
		want    string
		invalid bool
	}{
		{"passed", func(f *Feedback) {}, "passed", false},
		{"unknown", func(f *Feedback) { f.Checks[2].Outcome = "unknown"; f.Checks[2].Evidence = "not run" }, "unknown", false},
		{"failed_over_unknown", func(f *Feedback) { f.Checks[0].Outcome = "failed"; f.Checks[2].Outcome = "unknown" }, "failed", false},
		{"missing", func(f *Feedback) { f.Checks = f.Checks[:2] }, "", true},
		{"duplicate", func(f *Feedback) { f.Checks[1].ID = f.Checks[0].ID }, "", true},
		{"unsupported", func(f *Feedback) { f.Rubric = "unknown-rubric" }, "", true},
		{"missing_evidence", func(f *Feedback) { f.Checks[0].Evidence = "  " }, "", true},
		{"conflict", func(f *Feedback) { f.Outcome = "passed"; f.Checks[1].Outcome = "failed" }, "", true},
		{"old_version", func(f *Feedback) { f.RubricVersion = "old" }, "", true},
		{"legacy", func(f *Feedback) { f.Rubric = ""; f.Checks = nil; f.Outcome = "unknown" }, "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := rubricFeedback()
			tc.mutate(&f)
			err := f.validateQuality()
			if (err != nil) != tc.invalid || (err == nil && f.Outcome != tc.want) {
				t.Fatal(f, err)
			}
		})
	}
}

func TestQualityFeedbackAPIStoresEvidenceAndDerivedOutcome(t *testing.T) {
	g, s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("quality review must not invoke model") })
	s.Save(Record{ID: "test", Kind: "inference", Session: "session", Prompt: "task", Started: time.Now(), Actual: "low"})
	f := rubricFeedback()
	f.Source = "automated"
	f.Checks[1].Outcome = "failed"
	f.Checks[1].Evidence = "StartToCloseTimeout is not the only allowed timeout; SDK activity.go:120"
	raw, _ := json.Marshal(f)
	req := httptest.NewRequest("POST", "http://localhost/api/feedback", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	feedback, _ := s.Feedbacks()
	if len(feedback) != 1 || feedback[0].Outcome != "failed" || feedback[0].Source != "manual" || feedback[0].RubricVersion != "1" {
		t.Fatal(feedback)
	}
	sum, _ := s.Summary()
	if sum.EvidenceFailed != 1 || sum.EvidencePassed != 0 || sum.Failed != 1 {
		t.Fatal(sum)
	}
	f = feedback[0]
	f.Outcome = "passed"
	f.Checks[1].Outcome = "unknown"
	if s.Feedback(f) == nil {
		t.Fatal("forced pass bypassed incomplete checklist")
	}
	f.Outcome = ""
	if err := s.Feedback(f); err != nil {
		t.Fatal(err)
	}
	sum, _ = s.Summary()
	if sum.EvidenceUnknown != 1 || sum.EvidenceReviews != 1 || sum.EvidenceFailed != 0 {
		t.Fatal("review update double-counted", sum)
	}
}
