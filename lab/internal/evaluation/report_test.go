package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// reportAttempt synthesizes one recorded cohort attempt for the report builders.
func reportAttempt(task, variant string, rep int, correct, done bool, invalid string, cost *float64, models []string, elapsed int64) CohortAttempt {
	a := Attempt{Correct: correct, Invalid: invalid, ElapsedMS: elapsed, HarnessMS: elapsed / 2}
	if done {
		a.Molecule.Atoms = []molecule.Atom{{Type: "harness", Status: "done"}}
	}
	if done && invalid == "" {
		// A correct attempt passed hidden acceptance; the fixture's incorrect one failed it.
		// An invalid attempt records none.
		hidden := correct
		a.HiddenAcceptance = &hidden
	}
	if cost != nil || len(models) > 0 {
		a.Molecule.HarnessAttempts = []molecule.HarnessAttempt{{Usage: &harness.Usage{TotalCostUSD: cost, CostBasis: "list", EffectiveModels: models}}}
	}
	pass := correct
	a.RequiredChecks = &pass
	return CohortAttempt{Task: task, Variant: variant, Repetition: rep, Attempt: a}
}

// TestCohortReportExcludesInvalidAttempts: an invalid attempt stays in Attempts, the
// reported cost and its coverage, and is excluded from completions, passes, required
// checks, medians and the per-accepted cost; one note names the exclusion.
func TestCohortReportExcludesInvalidAttempts(t *testing.T) {
	attempts := []CohortAttempt{
		reportAttempt("task-a", "base", 1, true, true, "", fp(0.25), []string{"claude-opus-4-8"}, 100),
		reportAttempt("task-a", "base", 2, false, true, "model-mismatch", fp(0.5), []string{"claude-opus-5-5"}, 9000),
		reportAttempt("task-a", "cand", 1, true, true, "", nil, nil, 200),
	}
	base := CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 2, PrimaryOutcome: "costPerAcceptedOutcome"}
	r := buildCohortReport(base, []string{"base", "cand"}, map[string]string{"base": "x", "cand": "y"}, []string{"task-a"}, attempts)
	v := r.Variants[0]
	if v.Attempts != 2 || v.Invalid != 1 || len(v.InvalidReasons) != 1 || v.InvalidReasons["model-mismatch"] != 1 {
		t.Fatalf("invalid counts: %+v", v)
	}
	if v.Completions != 1 || v.HiddenAcceptancePass != 1 || v.RequiredChecksN != 1 || v.RequiredChecksPass == nil || *v.RequiredChecksPass != 1 || v.N.Attempts != 1 {
		t.Fatalf("invalid attempt counted in rates: %+v", v)
	}
	if v.ElapsedMedianMS != 100 || v.ElapsedP90MS != 100 || v.HarnessMedianMS != 50 || v.HarnessP90MS != 50 {
		t.Fatalf("invalid attempt counted in medians: %+v", v)
	}
	if v.ReportedCostUSD == nil || *v.ReportedCostUSD != 0.75 || v.CostCoverage != "2 of 2" {
		t.Fatalf("invalid attempt's spent cost dropped: %+v", v)
	}
	if v.CostPerAcceptedUSD == nil || *v.CostPerAcceptedUSD != 0.25 || v.AcceptedCostCoverage != "1 of 1" {
		t.Fatalf("invalid attempt counted in cost per accepted: %v %s", v.CostPerAcceptedUSD, v.AcceptedCostCoverage)
	}
	if strings.Join(v.EffectiveModels, ",") != "claude-opus-4-8,claude-opus-5-5" {
		t.Fatalf("served models hidden: %v", v.EffectiveModels)
	}
	if c := r.Variants[1]; c.Invalid != 0 || c.InvalidReasons != nil || c.N.Attempts != 1 {
		t.Fatalf("valid variant changed: %+v", c)
	}
	// The invalid attempt is incorrect, but it is not counted as a failure, so the adverse
	// counts add up over the valid attempts.
	if v.Failures != 0 || v.Timeouts != 0 || v.ScopeViolations != 0 || v.ScopeChanges != 0 || v.Amendments != 0 {
		t.Fatalf("invalid attempt counted in adverse counts: %+v", v)
	}
	cell := r.PerTask[0].Variants[0]
	if cell.Attempts != 2 || cell.Invalid != 1 || cell.Completions != 1 || cell.HiddenAcceptancePass != 1 || cell.RequiredChecksN != 1 || cell.Failures != 0 {
		t.Fatalf("per-task cell counted the invalid attempt: %+v", cell)
	}
	notes := 0
	for _, n := range r.Notes {
		if n == "1 attempt(s) excluded as invalid; see each attempt's invalid field." {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("invalid note count = %d: %v", notes, r.Notes)
	}
	// Rebuilding the report never duplicates the note.
	again := buildCohortReport(base, []string{"base", "cand"}, map[string]string{"base": "x", "cand": "y"}, []string{"task-a"}, attempts)
	if len(again.Notes) != len(r.Notes) {
		t.Fatalf("notes accumulated: %v", again.Notes)
	}
	md := renderCohortMarkdown(r)
	for _, want := range []string{"n=1 valid attempts over 1 task(s) × 2 rep(s); 1 invalid excluded", "- Invalid (excluded from rates and medians): 1 (model-mismatch=1)", "| task-a | 1/1 (to 0, fail 0, invalid 1) |"} {
		if !strings.Contains(md, want) {
			t.Fatalf("report.md missing %q:\n%s", want, md)
		}
	}
	// Without invalid attempts there is no note.
	clean := buildCohortReport(base, []string{"base", "cand"}, map[string]string{"base": "x", "cand": "y"}, []string{"task-a"}, attempts[:1])
	for _, n := range clean.Notes {
		if strings.Contains(n, "excluded as invalid") {
			t.Fatalf("invalid note without invalid attempts: %v", clean.Notes)
		}
	}
}

// TestCohortReportAttemptsSection renders one line per task, variant and repetition with
// the outcome, served models, cost and elapsed time, and a failed check's last 20 lines.
func TestCohortReportAttemptsSection(t *testing.T) {
	failed := reportAttempt("task-a", "cand", 1, false, true, "", nil, []string{"claude-sonnet-5-5"}, 30)
	var out strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&out, "out-%02d\n", i)
	}
	failed.FailedCheckOutput = []FailedCheckOutput{{ID: "go-test", Code: 1, StdoutTail: out.String(), StderrTail: "boom"}, {ID: "go-vet", Protected: true, Code: 2}}
	attempts := []CohortAttempt{
		reportAttempt("task-a", "base", 1, true, true, "", fp(0.1), []string{"claude-opus-4-8"}, 5),
		failed,
		reportAttempt("task-b", "base", 1, false, true, "model-mismatch", nil, []string{"claude-opus-5-5"}, 7),
		reportAttempt("task-b", "cand", 1, false, false, "", nil, nil, 9),
	}
	base := CohortReport{SchemaVersion: 1, Tasks: 2, Repetitions: 1, PrimaryOutcome: "hiddenAcceptancePassRate"}
	md := renderCohortMarkdown(buildCohortReport(base, []string{"base", "cand"}, map[string]string{"base": "x", "cand": "y"}, []string{"task-a", "task-b"}, attempts))
	for _, want := range []string{
		"## Attempts\n",
		"- task-a / base / rep-1: accepted; served: claude-opus-4-8; cost $0.100000; elapsed 5 ms; hidden acceptance passed\n",
		"- task-a / cand / rep-1: failed; served: claude-sonnet-5-5; cost unreported; elapsed 30 ms; hidden acceptance failed\n",
		"- task-b / base / rep-1: invalid:model-mismatch; served: claude-opus-5-5; cost unreported; elapsed 7 ms; hidden acceptance invalid:model-mismatch\n",
		"- task-b / cand / rep-1: incomplete; served: unavailable; cost unreported; elapsed 9 ms; hidden acceptance not run\n",
		"  - failed check `go-test` exit 1; last 20 lines of stdout:\n",
		"        out-11\n",
		"        out-30\n",
		"  - failed check `go-vet` (protected) exit 2; last 20 lines of stdout:\n\n        (no stdout)\n",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("report.md missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "out-10\n") {
		t.Fatalf("report.md shows more than the last 20 lines:\n%s", md)
	}
}

// TestCohortReportHiddenAcceptanceIndependent: hidden-acceptance pass counts attempts whose
// acceptance passed whatever their other checks did, over the attempts where it ran; an
// attempt where it never ran (nil) is excluded from HiddenAcceptanceN. Correct still
// requires every check.
func TestCohortReportHiddenAcceptanceIndependent(t *testing.T) {
	yes, no := true, false
	regression := reportAttempt("task-a", "base", 1, false, true, "", nil, nil, 1)
	regression.HiddenAcceptance = &yes
	regression.RegressionFailedTests = []string{"TestFlakyA", "TestFlakyB"}
	rejected := reportAttempt("task-a", "base", 2, false, true, "", nil, nil, 1)
	rejected.HiddenAcceptance = &no
	incomplete := reportAttempt("task-a", "base", 3, false, false, "", nil, nil, 1)
	accepted := reportAttempt("task-a", "base", 4, true, true, "", nil, nil, 1)
	attempts := []CohortAttempt{regression, rejected, incomplete, accepted}
	r := buildCohortReport(CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 4}, []string{"base"}, map[string]string{"base": "x"}, []string{"task-a"}, attempts)
	v := r.Variants[0]
	if v.HiddenAcceptancePass != 2 || v.HiddenAcceptanceN != 3 || v.Correct != 1 || v.Failures != 3 || v.N.Attempts != 4 {
		t.Fatalf("hidden acceptance not counted independently: %+v", v)
	}
	if cell := r.PerTask[0].Variants[0]; cell.HiddenAcceptancePass != 2 {
		t.Fatalf("per-task cell hidden-acceptance pass = %d, want 2", cell.HiddenAcceptancePass)
	}
	md := renderCohortMarkdown(r)
	for _, want := range []string{
		"- Hidden-acceptance pass 2 of 3-ran (67%); correct (acceptance and every required check): 1 (25%; n=4 attempts over 1 task(s) × 4 rep(s))\n",
		"- task-a / base / rep-1: failed; served: unavailable; cost unreported; elapsed 1 ms; hidden acceptance passed\n  - regression failed tests: TestFlakyA, TestFlakyB\n",
		"- task-a / base / rep-2: failed; served: unavailable; cost unreported; elapsed 1 ms; hidden acceptance failed\n",
		"- task-a / base / rep-3: incomplete; served: unavailable; cost unreported; elapsed 1 ms; hidden acceptance not run\n",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("report.md missing %q:\n%s", want, md)
		}
	}
}

// TestRegressionFailedTestsParsedFromTail: an attempt whose candidate verification passed
// acceptance but failed a regression check records the sorted, unique Go tests named by
// the "--- FAIL:" lines of the failed check's retained output (two in the candidate view,
// one repeated on stderr, a subtest in the protected run; a failed setup is not a
// regression check); a failed acceptance records false and no tests, and an incomplete
// attempt records nil.
func TestRegressionFailedTestsParsedFromTail(t *testing.T) {
	plain := process.Result{OK: false, Code: 1, Stdout: "=== RUN   TestZeta\n--- FAIL: TestZeta (0.01s)\n    x_test.go:9: boom\n--- FAIL: TestAlpha (2.00s)\nFAIL\n", Stderr: "--- FAIL: TestZeta (0.01s)\n"}
	history := func(acceptanceOK bool) molecule.Record {
		return writeHistory(t, verification.Result{
			SchemaVersion: "v1",
			Setup:         []verification.Observation{{ID: "setup", Result: process.Result{OK: false, Code: 1, Stdout: "--- FAIL: TestSetupOnly (0.00s)\n"}}},
			Checks:        []verification.Observation{{ID: "acceptance", Result: process.Result{OK: acceptanceOK}}, {ID: "go-test", Result: plain}},
			Protected:     &verification.Result{SchemaVersion: "v1", Checks: []verification.Observation{{ID: "go-test", Result: process.Result{OK: false, Code: 1, Stdout: "    --- FAIL: TestAlpha/sub (0.00s)\n"}}}},
		})
	}
	attempt := func(acceptanceOK bool) Attempt {
		a := reportAttempt("task-a", "base", 1, false, true, "", nil, nil, 1).Attempt
		a.HiddenAcceptance = nil
		record := history(acceptanceOK)
		a.FailedCheckOutput = failedCheckOutput(record)
		a.Molecule.ID, a.Molecule.LabDir, a.Molecule.VerificationAttempts = record.ID, record.LabDir, record.VerificationAttempts
		a.Molecule.Verification = &verification.Result{Checks: []verification.Observation{
			{ID: "acceptance", Result: process.Result{OK: acceptanceOK}},
			{ID: "go-test", Result: process.Result{OK: false, Code: 1}},
		}}
		return a
	}
	passed := attempt(true)
	setHiddenAcceptance(&passed)
	if passed.HiddenAcceptance == nil || !*passed.HiddenAcceptance {
		t.Fatalf("hidden acceptance = %v, want true", passed.HiddenAcceptance)
	}
	if got := strings.Join(passed.RegressionFailedTests, ","); got != "TestAlpha,TestAlpha/sub,TestZeta" {
		t.Fatalf("regression failed tests = %q", got)
	}
	failed := attempt(false)
	setHiddenAcceptance(&failed)
	if failed.HiddenAcceptance == nil || *failed.HiddenAcceptance || failed.RegressionFailedTests != nil {
		t.Fatalf("failed acceptance recorded %v %v", failed.HiddenAcceptance, failed.RegressionFailedTests)
	}
	incomplete := attempt(true)
	incomplete.Molecule.Atoms = nil
	setHiddenAcceptance(&incomplete)
	if incomplete.HiddenAcceptance != nil || incomplete.RegressionFailedTests != nil {
		t.Fatalf("incomplete attempt recorded %v %v", incomplete.HiddenAcceptance, incomplete.RegressionFailedTests)
	}
	if got := failedGoTests("ok\n"); got != nil {
		t.Fatalf("no FAIL lines parsed as %v", got)
	}
	if got := strings.Join(failedGoTests(plain.Stdout), ","); got != "TestAlpha,TestZeta" {
		t.Fatalf("two FAIL lines in one tail parsed as %q", got)
	}
}

// TestHiddenAcceptanceTimedOutIsNotRun: an acceptance check cut short by its own timeout
// (a real stub that outlives it) did not complete, so it records nil, never false, and is
// excluded from HiddenAcceptanceN, in either run it is read from.
func TestHiddenAcceptanceTimedOutIsNotRun(t *testing.T) {
	timedOut := process.Run(context.Background(), process.Options{Argv: []string{"/bin/sh", "-c", "sleep 5"}, Dir: t.TempDir(), Stdin: []byte{}, Timeout: 100 * time.Millisecond})
	if timedOut.OK || !timedOut.Incomplete() {
		t.Fatalf("stub acceptance did not time out: %+v", timedOut)
	}
	molecular := reportAttempt("task-a", "base", 1, false, true, "", nil, nil, 1)
	molecular.Molecule.Verification = &verification.Result{Checks: []verification.Observation{{ID: "acceptance", Result: timedOut}}}
	setHiddenAcceptance(&molecular.Attempt)
	independent := reportAttempt("task-a", "base", 2, false, true, "", nil, nil, 1)
	independent.Verification.Checks = []Observation{{Command: Command{Name: "acceptance"}, Result: timedOut}}
	independent.Molecule.Verification = &verification.Result{Checks: []verification.Observation{{ID: "acceptance", Result: process.Result{OK: false, Code: 1}}}}
	setHiddenAcceptance(&independent.Attempt)
	for _, a := range []CohortAttempt{molecular, independent} {
		if a.HiddenAcceptance != nil {
			t.Fatalf("rep-%d: timed-out acceptance recorded %v, want nil", a.Repetition, *a.HiddenAcceptance)
		}
	}
	r := buildCohortReport(CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 2}, []string{"base"}, map[string]string{"base": "x"}, []string{"task-a"}, []CohortAttempt{molecular, independent})
	if v := r.Variants[0]; v.HiddenAcceptanceN != 0 || v.HiddenAcceptancePass != 0 || v.N.Attempts != 2 {
		t.Fatalf("timed-out acceptance counted: %+v", v)
	}
}

// TestHiddenAcceptanceFromIndependentVerification: hidden acceptance comes from the
// independent verification that sets Correct whenever it observed acceptance, so
// Correct==true implies HiddenAcceptance==true even when the Molecule's run disagreed;
// the Molecule's candidate observation is used only when independent verification never
// ran. Failing test names come from both runs' failed regression checks.
func TestHiddenAcceptanceFromIndependentVerification(t *testing.T) {
	correct := reportAttempt("task-a", "base", 1, true, true, "", nil, nil, 1)
	correct.Verification = Verification{Correct: true, Checks: []Observation{
		{Command: Command{Name: "acceptance"}, Result: process.Result{OK: true}},
		{Command: Command{Name: "go-test"}, Result: process.Result{OK: true}},
	}}
	correct.Molecule.Verification = &verification.Result{Checks: []verification.Observation{{ID: "acceptance", Result: process.Result{OK: false, Code: 1}}}}
	setHiddenAcceptance(&correct.Attempt)
	if !correct.Correct || correct.HiddenAcceptance == nil || !*correct.HiddenAcceptance {
		t.Fatalf("correct attempt hidden acceptance = %v, want true", correct.HiddenAcceptance)
	}

	flaky := reportAttempt("task-a", "base", 2, false, true, "", nil, nil, 1)
	flaky.Verification = Verification{Checks: []Observation{
		{Command: Command{Name: "acceptance"}, Result: process.Result{OK: true}},
		{Command: Command{Name: "go-test"}, Result: process.Result{OK: false, Code: 1, Stdout: "--- FAIL: TestIndependent (0.00s)\n", Truncated: true}},
	}}
	flaky.FailedCheckOutput = []FailedCheckOutput{{ID: "go-test", Phase: failedPhaseCheck, Code: 1, FailedTests: []string{"TestMolecule"}}}
	setHiddenAcceptance(&flaky.Attempt)
	if flaky.HiddenAcceptance == nil || !*flaky.HiddenAcceptance || strings.Join(flaky.RegressionFailedTests, ",") != "TestIndependent,TestMolecule" || !flaky.RegressionFailedTestsTruncated {
		t.Fatalf("flaky attempt hidden=%v tests=%v truncated=%v", flaky.HiddenAcceptance, flaky.RegressionFailedTests, flaky.RegressionFailedTestsTruncated)
	}

	fallback := reportAttempt("task-a", "base", 3, false, true, "", nil, nil, 1)
	fallback.Verification = Verification{Error: "run did not finish within scope at an acceptable gate"}
	fallback.Molecule.Verification = &verification.Result{Checks: []verification.Observation{{ID: "acceptance", Result: process.Result{OK: true}}}}
	setHiddenAcceptance(&fallback.Attempt)
	if fallback.HiddenAcceptance == nil || !*fallback.HiddenAcceptance {
		t.Fatalf("fallback hidden acceptance = %v, want true from the Molecule", fallback.HiddenAcceptance)
	}

	md := renderCohortMarkdown(buildCohortReport(CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 3}, []string{"base"}, map[string]string{"base": "x"}, []string{"task-a"}, []CohortAttempt{correct, flaky, fallback}))
	if want := "  - regression failed tests: TestIndependent, TestMolecule (output truncated)\n"; !strings.Contains(md, want) {
		t.Fatalf("report.md missing %q:\n%s", want, md)
	}
	for _, a := range []CohortAttempt{correct, flaky, fallback} {
		if a.Correct && (a.HiddenAcceptance == nil || !*a.HiddenAcceptance) {
			t.Fatalf("rep-%d: correct without a passing hidden acceptance", a.Repetition)
		}
	}
}

// TestInvalidAttemptHiddenAcceptanceExcluded: an invalid attempt whose acceptance passed
// records nil, is excluded from HiddenAcceptancePass and HiddenAcceptanceN (even a record
// carrying a value is not counted), and its Attempts line names the invalid reason.
func TestInvalidAttemptHiddenAcceptanceExcluded(t *testing.T) {
	invalid := reportAttempt("task-a", "base", 1, false, true, invalidModelMismatch, nil, nil, 1)
	invalid.Verification.Checks = []Observation{{Command: Command{Name: "acceptance"}, Result: process.Result{OK: true}}}
	setHiddenAcceptance(&invalid.Attempt)
	if invalid.HiddenAcceptance != nil {
		t.Fatalf("invalid attempt recorded hidden acceptance %v", *invalid.HiddenAcceptance)
	}
	yes := true
	carried := reportAttempt("task-a", "base", 2, false, true, invalidModelMismatch, nil, nil, 1)
	carried.HiddenAcceptance = &yes
	r := buildCohortReport(CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 2}, []string{"base"}, map[string]string{"base": "x"}, []string{"task-a"}, []CohortAttempt{invalid, carried})
	if v := r.Variants[0]; v.HiddenAcceptancePass != 0 || v.HiddenAcceptanceN != 0 || v.Invalid != 2 {
		t.Fatalf("invalid attempts counted: %+v", v)
	}
	if cell := r.PerTask[0].Variants[0]; cell.HiddenAcceptancePass != 0 {
		t.Fatalf("invalid attempt counted in per-task cell: %+v", cell)
	}
	if want := "- task-a / base / rep-1: invalid:model-mismatch; served: unavailable; cost unreported; elapsed 1 ms; hidden acceptance invalid:model-mismatch\n"; !strings.Contains(renderCohortMarkdown(r), want) {
		t.Fatalf("report.md missing %q", want)
	}
}

// TestCohortReportNotesUnrecordedHiddenAcceptance: a resumed attempt.json written before
// hiddenAcceptance existed lacks the key; it is excluded from the hidden-acceptance rate
// and the report says so, while a current record (null when acceptance never ran) is not.
func TestCohortReportNotesUnrecordedHiddenAcceptance(t *testing.T) {
	dir := t.TempDir()
	current := reportAttempt("task-a", "base", 1, false, false, "", nil, nil, 1)
	current.StartedAt = "2026-10-18T00:00:00Z"
	currentPath := filepath.Join(dir, "current.json")
	if err := saveAttemptRecord(currentPath, current); err != nil {
		t.Fatal(err)
	}
	if raw := mustRead(t, currentPath); !strings.Contains(string(raw), `"hiddenAcceptance": null`) {
		t.Fatalf("current record lacks a null hiddenAcceptance:\n%s", raw)
	}
	legacy := reportAttempt("task-a", "base", 2, true, true, "", nil, nil, 1)
	legacy.StartedAt = "2026-10-18T00:00:00Z"
	raw, err := json.Marshal(attemptRecord{CohortAttempt: legacy})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "hiddenAcceptance")
	raw, _ = json.Marshal(fields)
	legacyPath := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	const note = "Some attempts were recorded before hiddenAcceptance existed; they are excluded from the hidden-acceptance rate."
	loadedCurrent, ok := loadAttemptRecord(currentPath, "task-a", "base", 1)
	if !ok {
		t.Fatal("current record did not load")
	}
	if r := buildCohortReport(CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 1}, []string{"base"}, map[string]string{"base": "x"}, []string{"task-a"}, []CohortAttempt{loadedCurrent}); strings.Contains(strings.Join(r.Notes, "\n"), note) {
		t.Fatalf("current record noted as unrecorded: %v", r.Notes)
	}
	loadedLegacy, ok := loadAttemptRecord(legacyPath, "task-a", "base", 2)
	if !ok || loadedLegacy.HiddenAcceptance != nil {
		t.Fatalf("legacy record ok=%v hiddenAcceptance=%v", ok, loadedLegacy.HiddenAcceptance)
	}
	r := buildCohortReport(CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 2}, []string{"base"}, map[string]string{"base": "x"}, []string{"task-a"}, []CohortAttempt{loadedCurrent, loadedLegacy})
	if v := r.Variants[0]; v.HiddenAcceptanceN != 0 || v.HiddenAcceptancePass != 0 || v.Correct != 1 {
		t.Fatalf("legacy record counted: %+v", v)
	}
	md := renderCohortMarkdown(r)
	if !strings.Contains(strings.Join(r.Notes, "\n"), note) || !strings.Contains(md, note) || !strings.Contains(md, "rep-2: accepted; served: unavailable; cost unreported; elapsed 1 ms; hidden acceptance not recorded\n") {
		t.Fatalf("report lacks the unrecorded note or line: %v\n%s", r.Notes, md)
	}
}

// TestCohortReportMarksTruncatedFirstTailLine: when the shown lines start at a tail cut
// from a longer stream, the first line is marked as partial; when only the last 20 of
// more lines are shown, nothing is marked.
func TestCohortReportMarksTruncatedFirstTailLine(t *testing.T) {
	a := reportAttempt("task-a", "base", 1, false, true, "", nil, nil, 1)
	a.FailedCheckOutput = []FailedCheckOutput{
		{ID: "short", Phase: "check", Code: 1, StdoutTail: "tail-of-line\nlast\n", StdoutTruncated: true},
		{ID: "long", Phase: "check", Code: 1, StdoutTail: strings.Repeat("partial\n", 30), StdoutTruncated: true},
	}
	md := renderCohortMarkdown(buildCohortReport(CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 1}, []string{"base", "cand"}, map[string]string{}, []string{"task-a"}, []CohortAttempt{a}))
	if !strings.Contains(md, "        …tail-of-line\n        last\n") {
		t.Fatalf("truncated first line not marked:\n%s", md)
	}
	if strings.Count(md, "…") != 1 {
		t.Fatalf("unexpected truncation markers:\n%s", md)
	}
}

// TestMaximalFailedTailsResumeAndStayOutOfReportJSON: an attempt.json carrying maximal
// tails for many failed commands (two views × stdout and stderr × 8 KiB, with worst-case
// JSON escaping) is over the general input bound, still loads on resume so the attempt
// is not re-run, and report.json never carries the tails.
func TestMaximalFailedTailsResumeAndStayOutOfReportJSON(t *testing.T) {
	worst := strings.Repeat("\x01", failedOutputTailBytes) // each byte escapes to \u0001
	ca := reportAttempt("task-a", "base", 1, false, true, "", nil, nil, 5)
	ca.StartedAt = "2026-10-17T00:00:00Z"
	for i := 0; i < 24; i++ {
		for _, protected := range []bool{false, true} {
			ca.FailedCheckOutput = append(ca.FailedCheckOutput, FailedCheckOutput{ID: fmt.Sprintf("check-%d", i), Phase: "check", Protected: protected, Code: 1, StdoutTail: worst, StdoutTruncated: true, StderrTail: worst, StderrTruncated: true})
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "attempt.json")
	if err := saveAttemptRecord(path, ca); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() <= 2_000_000 || st.Size() > maxAttemptRecordBytes {
		t.Fatalf("attempt.json is %d bytes; the case must exceed the general bound and fit the record bound", st.Size())
	}
	loaded, done := loadAttemptRecord(path, "task-a", "base", 1)
	if !done || len(loaded.FailedCheckOutput) != 48 || loaded.FailedCheckOutput[47].StderrTail != worst {
		t.Fatalf("maximal attempt.json not resumed: done=%v entries=%d", done, len(loaded.FailedCheckOutput))
	}
	r := buildCohortReport(CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 1}, []string{"base", "cand"}, map[string]string{}, []string{"task-a"}, []CohortAttempt{loaded})
	if err := saveCohortReport(dir, r); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "failedCheckOutput") || strings.Contains(string(raw), `\u0001`) {
		t.Fatal("report.json carries failed-check tails")
	}
	var back CohortReport
	if err := json.Unmarshal(raw, &back); err != nil || len(back.Attempts) != 1 || back.Attempts[0].Task != "task-a" || back.Attempts[0].StartedAt != ca.StartedAt {
		t.Fatalf("report.json attempts do not round-trip: %v %+v", err, back.Attempts)
	}
	if _, err := bounded(filepath.Join(dir, "report.json")); err != nil {
		t.Fatalf("report.json exceeds the resume read bound: %v", err)
	}
	md, _ := os.ReadFile(filepath.Join(dir, "report.md"))
	if !strings.Contains(string(md), "failed check `check-23` (protected) exit 1") {
		t.Fatal("report.md lost the tails rendered from the attempt record")
	}
}
