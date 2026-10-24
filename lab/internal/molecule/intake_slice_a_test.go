package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// vetScripted makes the fixture analysis return a check labelled with the tool
// name vet wherever steps says "vet", and the valid analysis otherwise.
func vetScripted(t *testing.T, o *Options, steps ...string) (*int, *[]analysisCorrection) {
	t.Helper()
	valid := o.Analyze
	n := 0
	var seen []analysisCorrection
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		if n >= len(steps) {
			t.Fatalf("unexpected analysis invocation %d", n+1)
		}
		step := steps[n]
		n++
		c, _ := analysisCorrectionFrom(ctx)
		seen = append(seen, c)
		a, err := valid(ctx, b, i, s)
		if err != nil {
			t.Fatal(err)
		}
		if step == "vet" {
			a.Checks = append([]readiness.Check(nil), a.Checks...)
			a.Checks[0].Category = "vet"
		}
		return a, nil
	}
	return &n, &seen
}

func TestSliceAPlanValidationFailureGetsOneCorrectiveRetry(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	calls, corrections := vetScripted(t, &o, "vet", "valid")
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || r.Status != "waiting" || r.Readiness == nil || r.Readiness.Phase != "scope-waiting" || len(r.HarnessAttempts) != 0 {
		t.Fatalf("a plan-validation rejection must get a corrective retry: calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
	want := readiness.PlanValidationMessage + ": unknown check category: vet"
	if (*corrections)[0] != (analysisCorrection{}) || (*corrections)[1].ValidatorMessage != want || !strings.Contains((*corrections)[1].PreviousResponse, `"category":"vet"`) {
		t.Fatalf("the correction must carry the validator message and the rejected response: %+v", *corrections)
	}
	if len(r.AnalysisAttempts) != 2 || r.AnalysisAttempts[0].Status != "failed" || r.AnalysisAttempts[0].Error != want || r.AnalysisAttempts[0].Response == nil || r.AnalysisAttempts[1].Status != "completed" {
		t.Fatalf("attempts: %+v", r.AnalysisAttempts)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "Rejection class plan-validation has now used its retry (1 of 3)") {
		t.Fatalf("notes lack the plan-validation retry: %v", r.Notes)
	}
	if got := r.Readiness.Plans[0].Plan.Analysis.Checks[0].Category; got != "candidate" {
		t.Fatalf("the corrected plan has category %q", got)
	}

	request := analysisRequest(withAnalysisCorrection(context.Background(), (*corrections)[1]), Issue{}, readiness.RepositorySnapshot{}, readiness.VerificationInputs{})
	correction, ok := request["correction"].(map[string]string)
	if !ok || correction["validatorMessage"] != want {
		t.Fatalf("corrective request: %+v", request["correction"])
	}
	for _, category := range []string{"regression", "candidate", "setup", "independent-acceptance"} {
		if !strings.Contains(correction["instruction"], category) {
			t.Fatalf("the plan-validation instruction must list category %s: %q", category, correction["instruction"])
		}
	}
}

func TestSliceAPlanValidationFailureTwiceStopsIntake(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	calls, _ := vetScripted(t, &o, "vet", "vet")
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || r.Status != "blocked" || r.Readiness != nil || len(r.HarnessAttempts) != 0 {
		t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
	notes := strings.Join(r.Notes, "\n")
	if !strings.Contains(notes, readiness.PlanValidationMessage+": unknown check category: vet (the corrective analysis retry was also rejected; no coding harness was invoked)") {
		t.Fatalf("blocked reason: %v", r.Notes)
	}
}

// noopSwitchFixture replaces the fixture harness with one that writes nothing
// while the returned marker file exists. The script is written before intake, so
// the binding stays the one the plans were approved under.
func noopSwitchFixture(t *testing.T) (Options, string) {
	t.Helper()
	return switchFixture(t, "cat >/dev/null")
}

// switchFixture runs the shell command noop instead of the fixture's edits while
// the returned marker file exists.
func switchFixture(t *testing.T, noop string) (Options, string) {
	t.Helper()
	o, _ := fixture(t)
	marker := filepath.Join(t.TempDir(), "noop")
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	body := "#!/bin/sh\nif [ -e " + shellWord(marker) + " ]; then " + noop + "; else cat > request.json; printf changed > code.txt; fi\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Implemented and tested\",\"paths\":[]}'\n"
	if err := writeStub(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return o, marker
}

// amendWithAcceptance runs and verifies revision one, then proposes revision two
// with the given acceptance descriptions added.
func amendWithAcceptance(t *testing.T, o Options, added ...string) (Record, Options) {
	t.Helper()
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness == nil || r.Readiness.Phase != "review-waiting" || len(r.VerificationAttempts) != 1 {
		t.Fatalf("revision one did not verify: %+v", r.Notes)
	}
	parent := r.Readiness.Plans[0]
	next := copyPlan(t, parent.Plan)
	for _, description := range added {
		next.Analysis.Acceptance = append(next.Analysis.Acceptance, readiness.Criterion{Description: description, Evidence: []string{"issue"}})
	}
	r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = r.Readiness.Plans[1].Digest
	return r, o
}

func TestSliceANoOpAmendmentStopsWithoutVerification(t *testing.T) {
	t.Parallel()
	o, marker := noopSwitchFixture(t)
	amended, o := amendWithAcceptance(t, o, "Code also records the amendment")
	paused := amended.Readiness.Plans[1].Plan.PausedTree
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 2 || r.HarnessAttempts[1].Coding == nil || r.HarnessAttempts[1].Coding.Outcome != "completed" || r.Capture == nil || r.Capture.Tree != paused {
		t.Fatalf("the stub must complete without changing the paused tree: %+v", r.HarnessAttempts)
	}
	if len(r.VerificationAttempts) != 1 || r.Verification != nil {
		t.Fatalf("verification ran for an amendment that changed nothing: %d attempts", len(r.VerificationAttempts))
	}
	note := "Amendment r2 coding attempt produced no change (tree " + paused + " equals the parent revision's tree); verification was not run. Amend again with new work, or amend with continuation checks-only to verify the retained tree."
	if !slices.Contains(r.Notes, note) {
		t.Fatalf("notes lack %q: %v", note, r.Notes)
	}
	last := r.Readiness.Events[len(r.Readiness.Events)-1]
	if r.Status != "waiting" || r.Readiness.Phase != "review-waiting" || last.Kind != readiness.NoChangeAfterAmendment || last.Tree != paused {
		t.Fatalf("status=%s phase=%s last event=%+v", r.Status, r.Readiness.Phase, last)
	}
	roles, err := resolveExecutionRoles(r)
	if err != nil {
		t.Fatal(err)
	}
	if review := r.Atoms[roles.atoms[formula.ReviewApproval]]; review.Status != "waiting" || !strings.HasPrefix(review.Detail, "noChangeAfterAmendment: ") {
		t.Fatalf("the review gate must carry the outcome reason: %+v", review)
	}
	if check := r.Atoms[roles.atoms[formula.Verification]]; check.Status != "skipped" {
		t.Fatalf("the verification atom must be skipped: %+v", check)
	}
	if _, err = r.Readiness.ValidateForDelivery(); err == nil || err.Error() != "amendment produced no change; nothing was verified" {
		t.Fatalf("delivery of an unverified no-change amendment: %v", err)
	}
	if state, _, _ := classify(r); state != LoopWaiting {
		t.Fatalf("the loop must see a Molecule waiting for review, got %s", state)
	}
}

func TestSliceAAmendedCodingRequestCarriesExactlyTheNewAcceptance(t *testing.T) {
	t.Parallel()
	o, _ := noopSwitchFixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	first := readCodingRequest(t, r)
	if _, ok := first["newAcceptance"]; ok || strings.Contains(first["instruction"].(string), amendedPlanInstruction) {
		t.Fatalf("a revision-one request must be unchanged: %v", first)
	}
	parent := r.Readiness.Plans[0]
	next := copyPlan(t, parent.Plan)
	// A kept item, an exact duplicate of a kept item and a near-miss of one: only
	// descriptions absent from the parent plan, compared exactly, are new.
	kept := next.Analysis.Acceptance[0].Description
	next.Analysis.Acceptance = append(next.Analysis.Acceptance,
		readiness.Criterion{Description: "Code also records the amendment", Evidence: []string{"issue"}},
		readiness.Criterion{Description: kept + ".", Evidence: []string{"issue"}},
	)
	r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = r.Readiness.Plans[1].Digest
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	request := readCodingRequest(t, r)
	var got []string
	raw, _ := json.Marshal(request["newAcceptance"])
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Code also records the amendment", kept + "."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("newAcceptance %q, want %q", got, want)
	}
	if instruction := request["instruction"].(string); !strings.HasSuffix(instruction, " This is an amended plan: implement every item in newAcceptance; an unchanged tree does not satisfy them.") {
		t.Fatalf("instruction %q", instruction)
	}
	if len(r.VerificationAttempts) != 2 || r.Readiness.Phase != "review-waiting" {
		t.Fatalf("an amendment that changed the tree must verify normally: phase=%s notes=%v", r.Readiness.Phase, r.Notes)
	}
}

// noChangeAmendment verifies revision one, then runs revision two, which adds
// acceptance item added, through a harness that changes nothing.
func noChangeAmendment(t *testing.T, added string) (Record, Options, string) {
	t.Helper()
	o, marker := noopSwitchFixture(t)
	_, o = amendWithAcceptance(t, o, added)
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if last := r.Readiness.Events[len(r.Readiness.Events)-1]; last.Kind != readiness.NoChangeAfterAmendment {
		t.Fatalf("revision two did not stop as no change: %+v", last)
	}
	return r, o, marker
}

func TestSliceANewAcceptanceKeepsItemsOfASkippedRevision(t *testing.T) {
	t.Parallel()
	r, o, marker := noChangeAmendment(t, "X is implemented")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	parent := r.Readiness.Plans[1]
	next := copyPlan(t, parent.Plan)
	next.Analysis.Acceptance = append(next.Analysis.Acceptance, readiness.Criterion{Description: "Y is implemented", Evidence: []string{"issue"}})
	r, err := Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = r.Readiness.Plans[2].Digest
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	var got []string
	raw, _ := json.Marshal(readCodingRequest(t, r)["newAcceptance"])
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// Revision two produced nothing, so its item is still new against revision one.
	if want := []string{"X is implemented", "Y is implemented"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("newAcceptance %q, want %q", got, want)
	}
}

func TestSliceAChecksOnlyAmendmentVerifiesAnUnchangedTree(t *testing.T) {
	t.Parallel()
	r, o, _ := noChangeAmendment(t, "X is already implemented")
	parent := r.Readiness.Plans[1]
	next := copyPlan(t, parent.Plan)
	next.Continuation = "checks-only"
	r, err := Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
	if err != nil {
		t.Fatalf("a checks-only amendment after no change: %v", err)
	}
	o.Approve = r.Readiness.Plans[2].Digest
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 2 || len(r.VerificationAttempts) != 2 || r.Verification == nil || !r.Verification.RequiredChecksPassed || r.Readiness.Phase != "review-waiting" {
		t.Fatalf("checks-only must verify the retained tree: attempts=%d verifications=%d phase=%s notes=%v", len(r.HarnessAttempts), len(r.VerificationAttempts), r.Readiness.Phase, r.Notes)
	}
	if delivered, err := r.Readiness.ValidateForDelivery(); err != nil || delivered.Revision != 3 {
		t.Fatalf("the verified checks-only revision must be deliverable: %v", err)
	}
}

func TestSliceAArtifactOnlyChangeIsNotANoChange(t *testing.T) {
	t.Parallel()
	o, marker := switchFixture(t, "cat >/dev/null; mkdir -p output; printf after > output/cache")
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	parent := r.Readiness.Plans[0]
	next := copyPlan(t, parent.Plan)
	next.Analysis.Acceptance = append(next.Analysis.Acceptance, readiness.Criterion{Description: "The cache is refreshed", Evidence: []string{"issue"}})
	next.Artifacts = []readiness.ArtifactRoot{{Path: "output", CommandID: next.Analysis.Checks[0].ID}}
	if r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, ""); err != nil {
		t.Fatal(err)
	}
	paused := r.Readiness.Plans[1].Plan.PausedTree
	o.Approve = r.Readiness.Plans[1].Digest
	if err = os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if r.Capture == nil || r.Capture.Tree != paused || artifactHash(*r.Capture) == r.Readiness.Plans[1].Plan.PausedArtifactsSHA256 {
		t.Fatalf("the stub must change only artifacts: %+v", r.Capture)
	}
	for _, e := range r.Readiness.Events {
		if e.Kind == readiness.NoChangeAfterAmendment {
			t.Fatal("an artifact-only change was flagged as no change")
		}
	}
	if len(r.VerificationAttempts) != 2 {
		t.Fatalf("an artifact-only change must verify: %d attempts, notes=%v", len(r.VerificationAttempts), r.Notes)
	}
}

func TestSliceAInvalidCheckInputStopsIntakeWithoutAnalysis(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	calls := 0
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		calls++
		return analyze(ctx, b, i, s)
	}
	content := "reviewed acceptance contract"
	sum := sha256.Sum256([]byte(content + " tampered"))
	o.CheckInputs = []readiness.FileEvidence{{Evidence: readiness.Evidence{ID: "human-acceptance", Path: "acceptance.txt", SHA256: fmt.Sprintf("%x", sum)}, Content: content}}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(r.AnalysisAttempts) != 0 || r.Status != "blocked" || r.Readiness != nil {
		t.Fatalf("a configuration error must stop intake before analysis: calls=%d attempts=%d status=%s", calls, len(r.AnalysisAttempts), r.Status)
	}
	notes := strings.Join(r.Notes, "\n")
	if !strings.Contains(notes, "invalid, ambiguous or tampered reviewed check input: human-acceptance") || strings.Contains(notes, readiness.PlanValidationMessage) || strings.Contains(notes, "Rejection class") {
		t.Fatalf("notes: %v", r.Notes)
	}
}

func TestSliceAPlanValidationInstructionNamesCategoriesOnlyForACategoryError(t *testing.T) {
	t.Parallel()
	request := analysisRequest(withAnalysisCorrection(context.Background(), analysisCorrection{ValidatorMessage: readiness.PlanValidationMessage + ": invalid command argv or timeout"}), Issue{}, readiness.RepositorySnapshot{}, readiness.VerificationInputs{})
	instruction := request["correction"].(map[string]string)["instruction"]
	if !strings.Contains(instruction, "Fix the plan error the validator message reports") || strings.Contains(instruction, "independent-acceptance") {
		t.Fatalf("instruction %q", instruction)
	}
}

func TestSliceAEmptyNewAcceptanceOmitsTheAmendedInstruction(t *testing.T) {
	t.Parallel()
	o, _ := noopSwitchFixture(t)
	r, o := amendWithAcceptance(t, o)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	request := readCodingRequest(t, r)
	if added, ok := request["newAcceptance"].([]any); !ok || len(added) != 0 || strings.Contains(request["instruction"].(string), amendedPlanInstruction) {
		t.Fatalf("an amendment without new acceptance: newAcceptance=%v instruction=%q", request["newAcceptance"], request["instruction"])
	}
}

// readCodingRequest reads the request the fixture harness saved in the workspace.
func readCodingRequest(t *testing.T, r Record) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.Workspace.Path, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err = json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	return request
}
