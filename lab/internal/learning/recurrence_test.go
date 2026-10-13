package learning

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

const (
	boundYAML   = recipe + "  - type: learn\n    command: [meta-harness]\n"
	oldFinish   = "2020-01-01T10:00:00Z"
	futureStart = "2099-01-01T10:00:00Z"
)

// recurrenceLab approves yaml and returns the Lab directory.
func recurrenceLab(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	approveLearning(t, dir, yaml, "generation-1")
	return dir
}

type ledgerKind int

const (
	timedOut ledgerKind = iota
	succeeded
	policyStop
	protectedFailed
	plainFailure
)

// finishedAt returns the n-th minute after base, so ordering is fixed.
func finishedAt(base string, n int) string {
	t, err := time.Parse(time.RFC3339Nano, base)
	if err != nil {
		panic(err)
	}
	return t.Add(time.Duration(n) * time.Minute).Format(time.RFC3339Nano)
}

func writeMolecule(t *testing.T, lab, yaml, id, finished string, kind ledgerKind, mutate ...func(*molecule.Record)) {
	t.Helper()
	r := molecule.Record{ID: id, SchemaVersion: "v0", Kind: "molecule", Mode: "ticket", Status: "failed", FormulaID: "default", FormulaApproved: true, FormulaSnapshot: molecule.Snapshot{YAML: yaml, SHA256: hash(yaml)}, StartedAt: finished, FinishedAt: finished}
	switch kind {
	case timedOut:
		r.HarnessAttempts = []molecule.HarnessAttempt{{Result: process.Result{TimedOut: true}}}
	case succeeded:
		r.Status = "done"
	case policyStop:
		reason := "analysis cannot assert independent acceptance review; retain as a candidate check for separate human review"
		r.Status = "blocked"
		r.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, Status: "failed", Error: reason, Response: &readinessAnalysis}}
		r.Notes = []string{reason + " (the corrective analysis retry was also rejected; no coding harness was invoked)"}
	case protectedFailed:
		r.Verification = protectedFailure()
	}
	for _, m := range mutate {
		m(&r)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(lab, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(lab, "ledgers", id+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func requestFiles(t *testing.T, lab string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(lab, "learn-requests"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	return names
}

func writeTimeouts(t *testing.T, lab, yaml, prefix, base string, n int) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i+1)
		writeMolecule(t, lab, yaml, id, finishedAt(base, i), timedOut)
		ids = append(ids, id)
	}
	return ids
}

func TestRecurrenceThreeConsecutiveTimeoutsQueueExactlyOneLearnRequest(t *testing.T) {
	lab := recurrenceLab(t, boundYAML)
	ids := writeTimeouts(t, lab, boundYAML, "mol", oldFinish, 3)
	got, err := EvaluateRecurrence(lab)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Created) != 1 || len(got.Recommendations) != 0 {
		t.Fatalf("%+v", got)
	}
	r := got.Created[0]
	if r.Class != molecule.FailureProcessTimedOut || strings.Join(r.MoleculeIDs, ",") != strings.Join(ids, ",") || r.FormulaSHA256 != hash(boundYAML) || r.FormulaID != "default" || r.Status != "pending" || r.Threshold != 3 {
		t.Fatalf("%+v", r)
	}
	if files := requestFiles(t, lab); len(files) != 1 || files[0] != r.ID+".json" {
		t.Fatalf("learn requests belong under learn-requests, not ledgers: %v", files)
	}
	if _, err = os.Stat(filepath.Join(lab, "ledgers", r.ID+".json")); err == nil {
		t.Fatal("a request must not be written into ledgers/")
	}
	again, err := EvaluateRecurrence(lab)
	if err != nil || len(again.Created) != 0 || len(requestFiles(t, lab)) != 1 {
		t.Fatalf("re-running must not duplicate: %+v %v %v", again, err, requestFiles(t, lab))
	}
	writeMolecule(t, lab, boundYAML, "mol-4", finishedAt(oldFinish, 10), timedOut)
	writeMolecule(t, lab, boundYAML, "mol-5", finishedAt(oldFinish, 11), timedOut)
	again, err = EvaluateRecurrence(lab)
	if err != nil || len(again.Created) != 0 || len(requestFiles(t, lab)) != 1 {
		t.Fatalf("a pending request blocks the class: %+v %v", again, err)
	}
}

func TestRecurrenceConcurrentEvaluationWritesOneRequest(t *testing.T) {
	lab := recurrenceLab(t, boundYAML)
	writeTimeouts(t, lab, boundYAML, "mol", oldFinish, 3)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := EvaluateRecurrence(lab); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if files := requestFiles(t, lab); len(files) != 1 {
		t.Fatalf("%v", files)
	}
}

func TestRecurrenceBelowThresholdQueuesNothing(t *testing.T) {
	lab := recurrenceLab(t, boundYAML)
	writeTimeouts(t, lab, boundYAML, "mol", oldFinish, 2)
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 0 || len(got.Recommendations) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = os.Stat(filepath.Join(lab, "learn-requests")); err == nil {
		t.Fatal("a quiet evaluation must not touch the Lab")
	}
}

func TestRecurrenceThresholdComesFromTheFormula(t *testing.T) {
	for threshold, want := range map[int][]int{2: {1, 1}, 5: {0, 1}} {
		yaml := boundYAML + fmt.Sprintf("learning:\n  recurrenceThreshold: %d\n", threshold)
		lab := recurrenceLab(t, yaml)
		writeTimeouts(t, lab, yaml, "mol", oldFinish, 2)
		got, err := EvaluateRecurrence(lab)
		if err != nil || len(got.Created) != want[0] {
			t.Fatalf("threshold %d with 2 failures: %+v %v", threshold, got, err)
		}
		if threshold == 5 {
			writeTimeouts(t, lab, yaml, "later", finishedAt(oldFinish, 60), 3)
			got, err = EvaluateRecurrence(lab)
			if err != nil || len(got.Created) != want[1] || len(got.Created[0].MoleculeIDs) != 5 || got.Created[0].Threshold != 5 {
				t.Fatalf("threshold %d with 5 failures: %+v %v", threshold, got, err)
			}
		}
	}
}

func TestRecurrenceMixedFailuresDoNotTrigger(t *testing.T) {
	cases := map[string]func(t *testing.T, lab string){
		"success between": func(t *testing.T, lab string) {
			writeMolecule(t, lab, boundYAML, "m1", finishedAt(oldFinish, 1), timedOut)
			writeMolecule(t, lab, boundYAML, "m2", finishedAt(oldFinish, 2), timedOut)
			writeMolecule(t, lab, boundYAML, "m3", finishedAt(oldFinish, 3), succeeded)
			writeMolecule(t, lab, boundYAML, "m4", finishedAt(oldFinish, 4), timedOut)
		},
		"another class between": func(t *testing.T, lab string) {
			writeMolecule(t, lab, boundYAML, "m1", finishedAt(oldFinish, 1), timedOut)
			writeMolecule(t, lab, boundYAML, "m2", finishedAt(oldFinish, 2), timedOut)
			writeMolecule(t, lab, boundYAML, "m3", finishedAt(oldFinish, 3), policyStop)
			writeMolecule(t, lab, boundYAML, "m4", finishedAt(oldFinish, 4), timedOut)
		},
		"plain failure between": func(t *testing.T, lab string) {
			writeMolecule(t, lab, boundYAML, "m1", finishedAt(oldFinish, 1), timedOut)
			writeMolecule(t, lab, boundYAML, "m2", finishedAt(oldFinish, 2), plainFailure)
			writeMolecule(t, lab, boundYAML, "m3", finishedAt(oldFinish, 3), timedOut)
			writeMolecule(t, lab, boundYAML, "m4", finishedAt(oldFinish, 4), timedOut)
		},
		"three different classes": func(t *testing.T, lab string) {
			writeMolecule(t, lab, boundYAML, "m1", finishedAt(oldFinish, 1), timedOut)
			writeMolecule(t, lab, boundYAML, "m2", finishedAt(oldFinish, 2), policyStop)
			writeMolecule(t, lab, boundYAML, "m3", finishedAt(oldFinish, 3), protectedFailed)
		},
		"older Formula snapshot in the streak": func(t *testing.T, lab string) {
			old := boundYAML + "# earlier\n"
			writeMolecule(t, lab, old, "m1", finishedAt(oldFinish, 1), timedOut)
			writeMolecule(t, lab, boundYAML, "m2", finishedAt(oldFinish, 2), timedOut)
			writeMolecule(t, lab, boundYAML, "m3", finishedAt(oldFinish, 3), timedOut)
		},
		"ineligible Molecule with the class": func(t *testing.T, lab string) {
			writeMolecule(t, lab, boundYAML, "m1", finishedAt(oldFinish, 1), timedOut)
			writeMolecule(t, lab, boundYAML, "m2", finishedAt(oldFinish, 2), timedOut)
			writeMolecule(t, lab, boundYAML, "m3", finishedAt(oldFinish, 3), timedOut, func(r *molecule.Record) { r.FormulaApproved = false })
		},
		"damaged Formula snapshot": func(t *testing.T, lab string) {
			writeMolecule(t, lab, boundYAML, "m1", finishedAt(oldFinish, 1), timedOut)
			writeMolecule(t, lab, boundYAML, "m2", finishedAt(oldFinish, 2), timedOut)
			writeMolecule(t, lab, boundYAML, "m3", finishedAt(oldFinish, 3), timedOut, func(r *molecule.Record) { r.FormulaSnapshot.YAML += "# tampered\n" })
		},
		"another Formula": func(t *testing.T, lab string) {
			writeMolecule(t, lab, boundYAML, "m1", finishedAt(oldFinish, 1), timedOut)
			writeMolecule(t, lab, boundYAML, "m2", finishedAt(oldFinish, 2), timedOut)
			writeMolecule(t, lab, boundYAML, "m3", finishedAt(oldFinish, 3), timedOut, func(r *molecule.Record) { r.FormulaID = "other" })
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			lab := recurrenceLab(t, boundYAML)
			setup(t, lab)
			got, err := EvaluateRecurrence(lab)
			if err != nil || len(got.Created) != 0 || len(got.Recommendations) != 0 || len(requestFiles(t, lab)) != 0 {
				t.Fatalf("%+v %v %v", got, err, requestFiles(t, lab))
			}
		})
	}
}

func TestRecurrenceUnfinishedMoleculesNeitherCountNorBreakTheStreak(t *testing.T) {
	lab := recurrenceLab(t, boundYAML)
	writeMolecule(t, lab, boundYAML, "m1", finishedAt(oldFinish, 1), timedOut)
	writeMolecule(t, lab, boundYAML, "m2", finishedAt(oldFinish, 2), timedOut)
	writeMolecule(t, lab, boundYAML, "waiting", "", plainFailure, func(r *molecule.Record) { r.Status = "waiting"; r.StartedAt = finishedAt(oldFinish, 3) })
	writeMolecule(t, lab, boundYAML, "m3", finishedAt(oldFinish, 4), timedOut)
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 1 || strings.Join(got.Created[0].MoleculeIDs, ",") != "m1,m2,m3" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRecurrenceOrdersByFinishTimeThenID(t *testing.T) {
	lab := recurrenceLab(t, boundYAML)
	same := finishedAt(oldFinish, 1)
	writeMolecule(t, lab, boundYAML, "mol-b", same, timedOut)
	writeMolecule(t, lab, boundYAML, "mol-a", same, timedOut)
	writeMolecule(t, lab, boundYAML, "mol-0", finishedAt(oldFinish, 0), timedOut)
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 1 || strings.Join(got.Created[0].MoleculeIDs, ",") != "mol-0,mol-a,mol-b" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRecurrenceEveryClassIsEvaluatedIndependently(t *testing.T) {
	for class, kind := range map[string]ledgerKind{molecule.FailureAnalysisPolicyViolation: policyStop, molecule.FailureProtectedRegressionFailed: protectedFailed} {
		lab := recurrenceLab(t, boundYAML)
		for i := 1; i <= 3; i++ {
			writeMolecule(t, lab, boundYAML, fmt.Sprintf("m%d", i), finishedAt(oldFinish, i), kind)
		}
		got, err := EvaluateRecurrence(lab)
		if err != nil || len(got.Created) != 1 || got.Created[0].Class != class {
			t.Fatalf("%s: %+v %v", class, got, err)
		}
	}
}

func TestRecurrenceUnboundLearnAtomOnlyRecommends(t *testing.T) {
	lab := recurrenceLab(t, recipe)
	ids := writeTimeouts(t, lab, recipe, "mol", oldFinish, 3)
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 0 || len(got.Recommendations) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	line := got.Recommendations[0]
	for _, want := range append(ids, "init", "--meta-harness", molecule.FailureProcessTimedOut, "'"+lab+"'") {
		if !strings.Contains(line, want) {
			t.Fatalf("%q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "\n") || len(requestFiles(t, lab)) != 0 {
		t.Fatalf("one line and no request: %q %v", line, requestFiles(t, lab))
	}
	if _, err = os.Stat(filepath.Join(lab, "learn-requests")); err == nil {
		t.Fatal("an unbound Lab must not gain request state")
	}
}

func TestRecurrenceLearnAtomWithoutACommandIsUnbound(t *testing.T) {
	yaml := recipe + "  - type: learn\n"
	lab := recurrenceLab(t, yaml)
	writeTimeouts(t, lab, yaml, "mol", oldFinish, 3)
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 0 || len(got.Recommendations) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRecurrenceRecommendationQuotesTheLabPath(t *testing.T) {
	yaml := recipe
	lab := filepath.Join(t.TempDir(), "it's a lab")
	if err := os.MkdirAll(lab, 0700); err != nil {
		t.Fatal(err)
	}
	approveLearning(t, lab, yaml, "g")
	writeTimeouts(t, lab, yaml, "mol", oldFinish, 3)
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Recommendations) != 1 || !strings.Contains(got.Recommendations[0], `'`+strings.ReplaceAll(lab, `'`, `'\''`)+`'`) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRecurrenceSkipsAnUnreadableLedgerAndReportsItOnce(t *testing.T) {
	lab := recurrenceLab(t, boundYAML)
	ids := writeTimeouts(t, lab, boundYAML, "mol", oldFinish, 3)
	bad := `{"schemaVersion":"v0","kind":"molecule","id":"mol-bad","formulaId":"default","status":"failed","finishedAt":7}`
	if err := os.WriteFile(filepath.Join(lab, "ledgers", "mol-bad.json"), []byte(bad), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 1 || strings.Join(got.Created[0].MoleculeIDs, ",") != strings.Join(ids, ",") {
		t.Fatalf("one damaged ledger must not stop the other Molecules from queueing a request: %+v %v", got, err)
	}
	if len(got.Skipped) != 1 || !strings.HasPrefix(got.Skipped[0], "mol-bad.json: ") {
		t.Fatalf("the damaged ledger is reported once: %v", got.Skipped)
	}
}

func TestRecurrenceAnUnreadableSuggestionOnlySkipsItsOwnClass(t *testing.T) {
	lab := recurrenceLab(t, boundYAML)
	writeTimeouts(t, lab, boundYAML, "t", oldFinish, 3)
	first, err := EvaluateRecurrence(lab)
	if err != nil || len(first.Created) != 1 {
		t.Fatalf("%+v %v", first, err)
	}
	results := runFakeLearn(t, lab, boundYAML, nil)
	if len(results) != 1 || results[0].SuggestionID == "" {
		t.Fatalf("%+v", results)
	}
	if err = os.WriteFile(filepath.Join(lab, "suggestions", results[0].SuggestionID+".json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	// Both classes recur in these Molecules; only the timeout class owns the damaged suggestion.
	for i := 1; i <= 3; i++ {
		writeMolecule(t, lab, boundYAML, fmt.Sprintf("p%d", i), finishedAt(futureStart, i), timedOut, func(r *molecule.Record) { r.Verification = protectedFailure() })
	}
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 1 || got.Created[0].Class != molecule.FailureProtectedRegressionFailed || len(got.Skipped) != 1 || !strings.Contains(got.Skipped[0], molecule.FailureProcessTimedOut) {
		t.Fatalf("the unreadable suggestion must be reported without stopping the other class: %+v %v", got, err)
	}
	items, err := Pending(lab)
	if err == nil || !strings.Contains(err.Error(), "skipped unreadable items") || len(items) != 1 || items[0].Class != molecule.FailureProtectedRegressionFailed {
		t.Fatalf("%+v %v", items, err)
	}
}

func TestRecurrenceAnUnreadableRequestIsSkippedNotFatal(t *testing.T) {
	lab, _, request := queuedTimeouts(t, boundYAML)
	if err := os.WriteFile(filepath.Join(lab, "learn-requests", "learn-request-ffffffffffffffff.json"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	items, err := Pending(lab)
	if len(items) != 1 || items[0].RequestID != request.ID || err == nil || !strings.Contains(err.Error(), "learn-request-ffffffffffffffff.json") {
		t.Fatalf("%+v %v", items, err)
	}
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 0 || len(got.Skipped) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRecurrenceSkippedTextIsDisplaySafe(t *testing.T) {
	lab, _, _ := queuedTimeouts(t, boundYAML)
	name := "req-" + string(rune(0x202e)) + "evil" + string(rune(0x200b)) + "\x1b[31m.json"
	if err := os.WriteFile(filepath.Join(lab, "learn-requests", name), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Skipped) != 1 || !strings.Contains(got.Skipped[0], "evil") {
		t.Fatalf("%+v %v", got, err)
	}
	_, pendingErr := Pending(lab)
	for _, s := range []string{got.Skipped[0], fmt.Sprint(pendingErr)} {
		for _, r := range s {
			if r == 0x202e || r == 0x200b || r == 0x1b || r == '\n' {
				t.Fatalf("unsafe rune %U in %q", r, s)
			}
		}
	}
	many := make([]string, 12)
	for i := range many {
		many[i] = strings.Repeat("é", 500)
	}
	if bounded := boundSkipped(many); len(bounded) != maxSkippedReported+1 || bounded[maxSkippedReported] != "and 7 more" || len([]rune(bounded[0])) > 200 {
		t.Fatalf("%v", bounded)
	}
}

func TestRecurrenceNeedsAnApprovedActiveFormula(t *testing.T) {
	if _, err := EvaluateRecurrence(t.TempDir()); !errors.Is(err, ErrNoActiveFormula) {
		t.Fatalf("%v", err)
	}
	lab := recurrenceLab(t, boundYAML)
	writeTimeouts(t, lab, boundYAML, "mol", oldFinish, 3)
	if err := os.WriteFile(filepath.Join(lab, "formulas", "default.yaml"), []byte(boundYAML+"# edited\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateRecurrence(lab); err == nil || len(requestFiles(t, lab)) != 0 {
		t.Fatalf("an unapproved Formula must not queue learning: %v", err)
	}
}

func TestRecurrenceDismissedSuggestionRefiresOnlyAfterThresholdNewOccurrences(t *testing.T) {
	lab := recurrenceLab(t, boundYAML)
	writeTimeouts(t, lab, boundYAML, "old", oldFinish, 3)
	first, err := EvaluateRecurrence(lab)
	if err != nil || len(first.Created) != 1 {
		t.Fatalf("%+v %v", first, err)
	}
	results := runFakeLearn(t, lab, boundYAML, nil)
	if len(results) != 1 || results[0].SuggestionID == "" || results[0].Err != "" {
		t.Fatalf("%+v", results)
	}
	// The suggestion is pending, so even many new occurrences never refire.
	writeTimeouts(t, lab, boundYAML, "while-pending", finishedAt(oldFinish, 100), 4)
	if got, err := EvaluateRecurrence(lab); err != nil || len(got.Created) != 0 {
		t.Fatalf("a pending suggestion blocks the class: %+v %v", got, err)
	}
	if _, err = Review(lab, results[0].SuggestionID, "dismiss"); err != nil {
		t.Fatal(err)
	}
	// Occurrences from before the dismissal are not new.
	if got, err := EvaluateRecurrence(lab); err != nil || len(got.Created) != 0 {
		t.Fatalf("nothing new since the dismissal: %+v %v", got, err)
	}
	writeTimeouts(t, lab, boundYAML, "new", finishedAt(futureStart, 600), 2)
	if got, err := EvaluateRecurrence(lab); err != nil || len(got.Created) != 0 {
		t.Fatalf("two new occurrences are below the threshold: %+v %v", got, err)
	}
	writeMolecule(t, lab, boundYAML, "new-3", finishedAt(futureStart, 603), timedOut)
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 1 || strings.Join(got.Created[0].MoleculeIDs, ",") != "new-1,new-2,new-3" {
		t.Fatalf("%+v %v", got, err)
	}
	if len(requestFiles(t, lab)) != 2 {
		t.Fatalf("%v", requestFiles(t, lab))
	}
}

func TestRecurrenceFailedLearnRunKeepsTheRequestAndRefiresAfterNewOccurrences(t *testing.T) {
	failedAt := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	restore := clock
	clock = func() time.Time { return failedAt }
	defer func() { clock = restore }()
	lab := recurrenceLab(t, boundYAML)
	writeTimeouts(t, lab, boundYAML, "old", oldFinish, 3)
	first, err := EvaluateRecurrence(lab)
	if err != nil || len(first.Created) != 1 {
		t.Fatalf("%+v %v", first, err)
	}
	results := runFakeLearn(t, lab, boundYAML, func(string) process.Result { return process.Result{Error: "provider unavailable"} })
	if len(results) != 1 || results[0].Err == "" || results[0].SuggestionID != "" {
		t.Fatalf("%+v", results)
	}
	// Still retryable, so it still blocks nothing but is not duplicated by two new failures.
	writeTimeouts(t, lab, boundYAML, "after-1", finishedAt(futureStart, 0), 2)
	if got, err := EvaluateRecurrence(lab); err != nil || len(got.Created) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	items, err := Pending(lab)
	if err != nil || len(items) != 1 || items[0].Kind != "request" || items[0].LastFailure == "" || items[0].Attempts != 1 {
		t.Fatalf("%+v %v", items, err)
	}
	writeMolecule(t, lab, boundYAML, "after-3", finishedAt(futureStart, 5), timedOut)
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 1 || strings.Join(got.Created[0].MoleculeIDs, ",") != "after-1-1,after-1-2,after-3" {
		t.Fatalf("%+v %v", got, err)
	}
	items, err = Pending(lab)
	if err != nil || len(items) != 1 || items[0].RequestID != got.Created[0].ID {
		t.Fatalf("the failed request is superseded by the fresh one: %+v %v", items, err)
	}
}

func TestRecurrenceRefireSupersedesTheOldRequestBeforeWritingTheNewOne(t *testing.T) {
	failedAt := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	restore := clock
	clock = func() time.Time { return failedAt }
	defer func() { clock = restore }()
	lab := recurrenceLab(t, boundYAML)
	writeTimeouts(t, lab, boundYAML, "old", oldFinish, 3)
	if first, err := EvaluateRecurrence(lab); err != nil || len(first.Created) != 1 {
		t.Fatalf("%+v %v", first, err)
	}
	runFakeLearn(t, lab, boundYAML, func(string) process.Result { return process.Result{Error: "provider unavailable"} })
	ids := []string{"new-1", "new-2", "new-3"}
	for i, id := range ids {
		writeMolecule(t, lab, boundYAML, id, finishedAt(futureStart, i), timedOut)
	}
	// A directory where the new request belongs makes its write fail, like a crash
	// right after the old request was retired.
	blocker := filepath.Join(lab, "learn-requests", newRequestID(molecule.FailureProcessTimedOut, hash(boundYAML), ids)+".json")
	if err := os.Mkdir(blocker, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateRecurrence(lab); err == nil {
		t.Fatal("the blocked write must fail")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	requests, _, err := readRequests(lab)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range requests {
		if r.Status == requestPending {
			t.Fatalf("a crash must never leave a pending request behind the superseded one: %+v", requests)
		}
	}
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 1 || strings.Join(got.Created[0].MoleculeIDs, ",") != strings.Join(ids, ",") {
		t.Fatalf("the next evaluation queues the fresh request: %+v %v", got, err)
	}
}

func TestRecurrenceKeepsTheRequestDirectoryBounded(t *testing.T) {
	lab, _, _ := queuedTimeouts(t, boundYAML)
	dir := filepath.Join(lab, "learn-requests")
	for i := 0; i < maxTerminalRequests+10; i++ {
		r := Request{SchemaVersion: "v1", ID: fmt.Sprintf("learn-request-%016x", i+1), Class: molecule.FailureProtectedRegressionFailed, FormulaID: "default", FormulaSHA256: strings.Repeat("a", 64), MoleculeIDs: []string{"mol-x"}, Threshold: 3, CreatedAt: finishedAt(oldFinish, i), Status: requestStale}
		if err := saveRequest(lab, r); err != nil {
			t.Fatal(err)
		}
	}
	orphan := filepath.Join(dir, "learn-request-00000000000000aa.run-lock")
	if err := os.WriteFile(orphan, nil, 0600); err != nil {
		t.Fatal(err)
	}
	writeTimeouts(t, lab, boundYAML, "more", finishedAt(oldFinish, 500), 3)
	if _, err := EvaluateRecurrence(lab); err != nil {
		t.Fatal(err)
	}
	requests, _, err := readRequests(lab)
	if err != nil {
		t.Fatal(err)
	}
	terminal, pending := 0, 0
	for _, r := range requests {
		if r.Status == requestStale {
			terminal++
		}
		if r.Status == requestPending {
			pending++
		}
	}
	if terminal != maxTerminalRequests || pending != 1 {
		t.Fatalf("terminal=%d pending=%d", terminal, pending)
	}
	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("an orphan run lock must be removed")
	}
}

func TestRecurrenceFormulaChangeResetsCounters(t *testing.T) {
	lab := recurrenceLab(t, boundYAML)
	writeTimeouts(t, lab, boundYAML, "old", oldFinish, 3)
	first, err := EvaluateRecurrence(lab)
	if err != nil || len(first.Created) != 1 {
		t.Fatalf("%+v %v", first, err)
	}
	changed := boundYAML + "# generation 2\n"
	approveLearning(t, lab, changed, "generation-2")
	if got, err := EvaluateRecurrence(lab); err != nil || len(got.Created) != 0 {
		t.Fatalf("counters reset with the Formula: %+v %v", got, err)
	}
	if items, err := Pending(lab); err != nil || len(items) != 0 {
		t.Fatalf("a request for another snapshot is not shown: %+v %v", items, err)
	}
	writeTimeouts(t, lab, changed, "fresh", futureStart, 3)
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 1 || got.Created[0].FormulaSHA256 != hash(changed) {
		t.Fatalf("%+v %v", got, err)
	}
	requests, _, err := readRequests(lab)
	if err != nil {
		t.Fatal(err)
	}
	stale := 0
	for _, r := range requests {
		if r.Status == requestStale && r.FormulaSHA256 == hash(boundYAML) {
			stale++
		}
	}
	if stale != 1 {
		t.Fatalf("the old request must be retired: %+v", requests)
	}
}
