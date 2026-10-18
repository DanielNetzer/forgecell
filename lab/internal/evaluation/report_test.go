package evaluation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// reportAttempt synthesizes one recorded cohort attempt for the report builders.
func reportAttempt(task, variant string, rep int, correct, done bool, invalid string, cost *float64, models []string, elapsed int64) CohortAttempt {
	a := Attempt{Correct: correct, Invalid: invalid, ElapsedMS: elapsed, HarnessMS: elapsed / 2}
	if done {
		a.Molecule.Atoms = []molecule.Atom{{Type: "harness", Status: "done"}}
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
		"- task-a / base / rep-1: accepted; served: claude-opus-4-8; cost $0.100000; elapsed 5 ms\n",
		"- task-a / cand / rep-1: failed; served: claude-sonnet-5-5; cost unreported; elapsed 30 ms\n",
		"- task-b / base / rep-1: invalid:model-mismatch; served: claude-opus-5-5; cost unreported; elapsed 7 ms\n",
		"- task-b / cand / rep-1: incomplete; served: unavailable; cost unreported; elapsed 9 ms\n",
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
