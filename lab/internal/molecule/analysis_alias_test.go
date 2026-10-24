package molecule

import (
	"context"
	"testing"
)

func TestAnalysisLedgerNotAliasedAfterEvidenceThenTwoRejections(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	calls, _ := scripted(t, &o, "evidence", "policy", "policy")
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 3 || r.Status != "blocked" || r.Readiness != nil {
		t.Fatalf("the repeated class must stop intake: calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
	if r.Analysis == nil || r.Analysis.Summary != evidenceRequest().Summary || len(r.Analysis.EvidenceRequests) != 1 {
		t.Fatalf("the ledger must keep the validated analysis, not a rejected one: %+v", r.Analysis)
	}
	if len(r.AnalysisAttempts) != 3 {
		t.Fatalf("attempts: %+v", r.AnalysisAttempts)
	}
	for i := 1; i < 3; i++ {
		response := r.AnalysisAttempts[i].Response
		if r.AnalysisAttempts[i].Status != "failed" || response == nil || response.Checks[0].Category != "independent-acceptance" {
			t.Fatalf("the rejected response of attempt %d must stay in the attempts: %+v", i+1, r.AnalysisAttempts[i])
		}
	}
}

func TestAnalysisLedgerNotAliasedCompletedAnalysisIsACopy(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	scripted(t, &o, "evidence", "policy", "valid")
	r, err := Run(context.Background(), o)
	if err != nil || r.Status != "waiting" || r.Analysis == nil {
		t.Fatalf("%v status=%s notes=%v", err, r.Status, r.Notes)
	}
	if len(r.Analysis.EvidenceRequests) != 0 || r.Analysis.Checks[0].Category == "independent-acceptance" {
		t.Fatalf("the ledger Analysis must be the final validated analysis: %+v", r.Analysis)
	}
	last := r.AnalysisAttempts[len(r.AnalysisAttempts)-1].Response
	if last == nil || last == r.Analysis {
		t.Fatal("the ledger Analysis must not alias the attempt response")
	}
}
