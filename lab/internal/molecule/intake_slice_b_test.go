package molecule

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// sliceBRequests drives intake with one evidence-request response per entry of
// requests, then the fixture's valid analysis when valid is true.
func sliceBRequests(t *testing.T, o *Options, valid bool, requests ...string) *int {
	t.Helper()
	original := o.Analyze
	calls := 0
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		calls++
		if calls <= len(requests) {
			return readiness.Analysis{Summary: "Need committed content", EvidenceRequests: []readiness.EvidenceRequest{{Path: requests[calls-1], Reason: "Inspect implementation", Evidence: []string{"issue"}}}}, nil
		}
		if !valid {
			t.Fatalf("unexpected analysis invocation %d", calls)
		}
		return original(ctx, b, i, s)
	}
	return &calls
}

func TestSliceBTwoSupplementalPassesReachAPlan(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	calls := sliceBRequests(t, &o, true, "code.txt#L1-9", "package.json")
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 3 || len(r.AnalysisAttempts) != 3 || r.Status != "waiting" || r.Readiness == nil {
		t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
	if r.RepositoryEvidence == nil || r.RepositoryEvidence.SupplementalPasses() != 2 {
		t.Fatalf("two supplemental passes must be recorded: %+v", r.RepositoryEvidence)
	}
	found := false
	for _, e := range r.Readiness.Plans[0].Plan.Evidence {
		if e.Path == "code.txt#L1-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("clamped excerpt missing from plan evidence: %+v", r.Readiness.Plans[0].Plan.Evidence)
	}
	reason := r.RepositoryEvidence.Collection[len(r.RepositoryEvidence.Collection)-2].Outcomes[0].Reason
	if reason != "collected; range clamped to the end of the file at line 1" {
		t.Fatalf("clamp reason %q", reason)
	}
	// Exact approval re-collects both passes from the base commit and matches the plan.
	o.Approve = r.Readiness.Plans[0].Digest
	if _, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *calls != 3 {
		t.Fatal("approval invoked the model")
	}
}

func TestSliceBThirdEvidenceRequestStopsIntakeWithUnmetList(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	calls := sliceBRequests(t, &o, false, "code.txt#L1-1", "missing/checker.sh", "code.txt#L5-9")
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 3 || len(r.AnalysisAttempts) != 3 || r.Status != "blocked" || r.Readiness != nil || len(r.HarnessAttempts) != 0 {
		t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
	want := "Supplemental evidence passes exhausted; unmet requests: code.txt#L5-9 (not collected: no supplemental pass remains). No coding harness was invoked."
	if !strings.Contains(strings.Join(r.Notes, "\n"), want) {
		t.Fatalf("stop reason lacks %q: %v", want, r.Notes)
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("coding invoked")
	}
}

func TestSliceBExhaustionListsEachUnmetReason(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	original := o.Analyze
	calls := 0
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		calls++
		if calls > 3 {
			t.Fatalf("unexpected analysis invocation %d", calls)
		}
		a, err := original(ctx, b, i, s)
		a.EvidenceRequests = []readiness.EvidenceRequest{{Path: "missing/checker.sh", Reason: "Need behavior", Evidence: []string{"issue"}}, {Path: ".env#L1-2", Reason: "Need config", Evidence: []string{"issue"}}}
		return a, err
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	want := "unmet requests: missing/checker.sh (absent from frozen commit); .env#L1-2 (sensitive, private or generated path)."
	if calls != 3 || r.Status != "blocked" || !strings.Contains(strings.Join(r.Notes, "\n"), want) {
		t.Fatalf("calls=%d status=%s notes=%v", calls, r.Status, r.Notes)
	}
	if 1+maxCorrectiveAnalyses+2 != maxAnalysisInvocations {
		t.Fatalf("maxAnalysisInvocations=%d must cover two supplemental passes", maxAnalysisInvocations)
	}
}

func TestSliceBExcerptDefinitionTakesTheCorrectiveRetry(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	original := o.Analyze
	calls := 0
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		calls++
		if calls == 1 {
			return readiness.Analysis{Summary: "Need committed content", EvidenceRequests: []readiness.EvidenceRequest{{Path: "code.txt#L1-1", Reason: "Inspect implementation", Evidence: []string{"issue"}}}}, nil
		}
		a, err := original(ctx, b, i, s)
		if calls == 2 {
			excerpt := ""
			for _, e := range s.Evidence {
				if e.Path == "code.txt#L1-1" {
					excerpt = e.ID
				}
			}
			if excerpt == "" {
				t.Fatal("excerpt evidence missing")
			}
			a.Checks = append([]readiness.Check(nil), a.Checks...)
			a.Checks[0].Definitions = append(append([]string(nil), a.Checks[0].Definitions...), excerpt)
		}
		return a, err
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(r.AnalysisAttempts) != 3 || r.Status != "waiting" || r.Readiness == nil {
		t.Fatalf("calls=%d status=%s notes=%v", calls, r.Status, r.Notes)
	}
	rejected := r.AnalysisAttempts[1]
	if rejected.Status != "failed" || !strings.HasPrefix(rejected.Error, readiness.WholeFileDefinitionMessage+": check cites ") || !strings.Contains(rejected.Error, "(code.txt#L1-1)") {
		t.Fatalf("excerpt definition not rejected as a policy violation: %+v", rejected)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "Rejection class "+readiness.PolicyClassWholeFileDef+" has now used its retry") {
		t.Fatalf("corrective retry not recorded: %v", r.Notes)
	}
}

func TestSliceBExhaustionNamesAlreadyCollectedRequestsSeparately(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	calls := sliceBRequests(t, &o, false, "code.txt#L1-1", "missing/checker.sh", "code.txt#L1-1")
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	want := "Supplemental evidence passes exhausted; unmet requests: none; already collected: code.txt#L1-1. No coding harness was invoked."
	if *calls != 3 || r.Status != "blocked" || !strings.Contains(strings.Join(r.Notes, "\n"), want) {
		t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
}
