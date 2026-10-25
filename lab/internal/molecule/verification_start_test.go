package molecule

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// manyChecks makes the fixture analysis propose n copies of its check wherever
// steps says "many", and the valid analysis otherwise.
func manyChecks(t *testing.T, o *Options, n int, steps ...string) *int {
	t.Helper()
	valid := o.Analyze
	calls := 0
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		if calls >= len(steps) {
			t.Fatalf("unexpected analysis invocation %d", calls+1)
		}
		step := steps[calls]
		calls++
		a, err := valid(ctx, b, i, s)
		if err != nil || step != "many" {
			return a, err
		}
		base := a.Checks[0]
		a.Checks = nil
		for k := 0; k < n; k++ {
			c := base
			c.ID = fmt.Sprintf("check-%d", k)
			a.Checks = append(a.Checks, c)
		}
		return a, nil
	}
	return &calls
}

// TestHardening209IntakeRejectsPlanAboveCommandLimit: 16 proposed checks plus the
// Formula baseline make 17, which intake rejects as a correctable plan-validation
// policy violation before any coding harness runs.
func TestHardening209IntakeRejectsPlanAboveCommandLimit(t *testing.T) {
	t.Parallel()
	want := readiness.PlanValidationMessage + ": plan has 17 setup and check commands; " + readiness.PlanCommandLimitMessage
	t.Run("corrected", func(t *testing.T) {
		t.Parallel()
		o := baselineFixture(t)
		calls := manyChecks(t, &o, 16, "many", "valid")
		r, err := Run(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if *calls != 2 || r.Status != "waiting" || statePhase(r) != "scope-waiting" || len(r.HarnessAttempts) != 0 {
			t.Fatalf("a plan above the limit must get a corrective retry: calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
		}
		if len(r.AnalysisAttempts) != 2 || r.AnalysisAttempts[0].Status != "failed" || r.AnalysisAttempts[0].Error != want {
			t.Fatalf("attempts: %+v", r.AnalysisAttempts)
		}
		if got := readiness.PolicyClass(&readiness.PolicyViolation{Reason: r.AnalysisAttempts[0].Error}); got != readiness.PolicyClassPlanValidation {
			t.Fatalf("rejection class %q", got)
		}
		if n := len(r.Readiness.Plans[0].Plan.Analysis.Checks); n > readiness.MaxPlanCommands {
			t.Fatalf("the accepted plan has %d checks", n)
		}
	})
	t.Run("rejected twice", func(t *testing.T) {
		t.Parallel()
		o := baselineFixture(t)
		calls := manyChecks(t, &o, 16, "many", "many")
		r, err := Run(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if *calls != 2 || r.Status != "blocked" || r.Readiness != nil || len(r.HarnessAttempts) != 0 {
			t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
		}
		if !strings.Contains(strings.Join(r.Notes, "\n"), want) {
			t.Fatalf("blocked reason: %v", r.Notes)
		}
		if AnalysisPolicyRejectionClass(r) != readiness.PolicyClassPlanValidation {
			t.Fatalf("class %q", AnalysisPolicyRejectionClass(r))
		}
	})
}

// TestHardening209InjectedVerificationRefusalLeavesMoleculeAmendable replaces
// beginVerification, so it is not parallel: parallel tests resume only after it
// has restored the seam.
func TestHardening209InjectedVerificationRefusalLeavesMoleculeAmendable(t *testing.T) {
	o, _ := fixture(t)
	original := startVerification
	startVerification = func(*Record, readiness.Plan, string) error { return fmt.Errorf("injected refusal") }
	defer func() { startVerification = original }()

	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatalf("a verification refusal must close the attempt, not escape: %v", err)
	}
	saved, err := ReadRecord(o.LabDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []Record{r, saved} {
		if rec.Status == "running" || statePhase(rec) != "failed" {
			t.Fatalf("status %s phase %s; the attempt must be closed with a failed outcome", rec.Status, statePhase(rec))
		}
		last := rec.Readiness.Events[len(rec.Readiness.Events)-1]
		if last.Kind != "verification-failed" || !strings.Contains(last.Detail, "injected refusal") {
			t.Fatalf("last event %+v", last)
		}
		if rec.Atoms[3].Status != "failed" || !strings.Contains(rec.Atoms[3].Detail, "Verification was not started: injected refusal") {
			t.Fatalf("check atom %+v", rec.Atoms[3])
		}
		if len(rec.VerificationAttempts) != 0 {
			t.Fatalf("no verification attempt started: %+v", rec.VerificationAttempts)
		}
	}
	startVerification = original
	// Amendable without forgecell recover.
	parent := saved.Readiness.Plans[len(saved.Readiness.Plans)-1]
	amended, err := Amend(context.Background(), o.LabDir, saved.ID, parent.Digest, copyPlan(t, parent.Plan), "")
	if err != nil {
		t.Fatalf("the Molecule must stay amendable: %v", err)
	}
	if statePhase(amended) != "scope-waiting" || len(amended.Readiness.Plans) != 2 {
		t.Fatalf("amendment: phase %s", statePhase(amended))
	}
}
