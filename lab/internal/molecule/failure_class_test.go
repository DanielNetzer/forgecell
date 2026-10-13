package molecule

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

const finishedAt = "2026-10-10T10:00:00Z"

func classRecord(mutate func(*Record)) Record {
	r := Record{SchemaVersion: "v0", Kind: "molecule", ID: "mol-1", Mode: "ticket", Status: "failed", FormulaID: "sample", FinishedAt: finishedAt}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func rejectedAttempt(reason string) AnalysisAttempt {
	return AnalysisAttempt{Ordinal: 1, Status: "failed", Error: reason, Response: &readiness.Analysis{}}
}

func intakeStopped(reason string, suffix string) func(*Record) {
	return func(r *Record) {
		r.Status = "blocked"
		r.AnalysisAttempts = []AnalysisAttempt{rejectedAttempt(reason)}
		r.Notes = []string{"earlier note", reason + suffix}
	}
}

func TestFailureClassTimedOut(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Record)
		want   bool
	}{
		"timed out coding attempt": {func(r *Record) { r.HarnessAttempts = []HarnessAttempt{{Result: process.Result{TimedOut: true}}} }, true},
		"timeout then a failed retry that did not time out": {func(r *Record) {
			r.HarnessAttempts = []HarnessAttempt{{Result: process.Result{TimedOut: true}}, {Result: process.Result{Code: 1}}}
		}, false},
		"timeout then a successful attempt": {func(r *Record) {
			r.HarnessAttempts = []HarnessAttempt{{Result: process.Result{TimedOut: true}}, {Result: process.Result{OK: true}}}
		}, false},
		"retry timed out after an earlier plain failure": {func(r *Record) {
			r.HarnessAttempts = []HarnessAttempt{{Result: process.Result{Code: 1}}, {Result: process.Result{TimedOut: true}}}
		}, true},
		"timeout, amended, then verification passed": {func(r *Record) {
			r.HarnessAttempts = []HarnessAttempt{{Result: process.Result{TimedOut: true}}}
			r.Verification = &verification.Result{RequiredChecksPassed: true, Protected: &verification.Result{RequiredChecksPassed: true}}
		}, false},
		"timeout, then verification failed": {func(r *Record) {
			r.HarnessAttempts = []HarnessAttempt{{Result: process.Result{TimedOut: true}}}
			r.Verification = &verification.Result{Error: "check failed"}
		}, true},
		"plain failure":    {func(r *Record) { r.HarnessAttempts = []HarnessAttempt{{Result: process.Result{Code: 1}}} }, false},
		"interrupted only": {func(r *Record) { r.HarnessAttempts = []HarnessAttempt{{Result: process.Result{Interrupted: true}}} }, false},
		"overflow only":    {func(r *Record) { r.HarnessAttempts = []HarnessAttempt{{Result: process.Result{Overflow: true}}} }, false},
		"no attempts":      {nil, false},
		"analysis timeout is not a coding timeout": {func(r *Record) {
			r.AnalysisAttempts = []AnalysisAttempt{{Status: "failed", Error: "process timed out"}}
			r.Notes = []string{"analysis timed out"}
		}, false},
		"unfinished": {func(r *Record) {
			r.FinishedAt = ""
			r.HarnessAttempts = []HarnessAttempt{{Result: process.Result{TimedOut: true}}}
		}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := HasFailureClass(classRecord(c.mutate), FailureProcessTimedOut); got != c.want {
				t.Fatalf("got %t want %t", got, c.want)
			}
		})
	}
}

func TestFailureClassAnalysisPolicyViolation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Record)
		want   string
	}{
		"labelling rejected twice":      {intakeStopped(readiness.LabellingViolationMessage, retryAlsoRejectedSuffix), readiness.PolicyClassLabelling},
		"in-scope definition rejected":  {intakeStopped(readiness.InScopeDefinitionMessage+": code.txt", retryAlsoRejectedSuffix), readiness.PolicyClassInScopeDefinition},
		"definition reference rejected": {intakeStopped(readiness.DefinitionReferenceMessage+": issue", retryAlsoRejectedSuffix), readiness.PolicyClassDefinitionRef},
		"baseline id rejected":          {intakeStopped(readiness.BaselineIDMessage+": ci", retryAlsoRejectedSuffix), readiness.PolicyClassBaselineID},
		"evidence request cap rejected": {intakeStopped(readiness.EvidenceRequestCapMessage(), retryAlsoRejectedSuffix), readiness.PolicyClassEvidenceRequestCap},
		"retry limit of three reached":  {intakeStopped(readiness.LabellingViolationMessage, " (the limit of 3 corrective analysis retries was reached; no coding harness was invoked)"), readiness.PolicyClassLabelling},
		"unclassified violation":        {intakeStopped("some other policy text", retryAlsoRejectedSuffix), readiness.PolicyClassOther},
		"non-policy failure has no body": {func(r *Record) {
			r.Status = "blocked"
			attempt := rejectedAttempt("bad path")
			attempt.Response = nil
			r.AnalysisAttempts = []AnalysisAttempt{attempt}
			r.Notes = []string{"bad path"}
		}, ""},
		"stopped for another reason": {func(r *Record) {
			intakeStopped(readiness.LabellingViolationMessage, retryAlsoRejectedSuffix)(r)
			r.Notes = append(r.Notes, "Formula changed during intake")
		}, ""},
		"reached a plan": {func(r *Record) {
			intakeStopped(readiness.LabellingViolationMessage, retryAlsoRejectedSuffix)(r)
			r.Readiness = &readiness.State{Phase: "failed"}
		}, ""},
		"coded after intake": {func(r *Record) {
			intakeStopped(readiness.LabellingViolationMessage, retryAlsoRejectedSuffix)(r)
			r.HarnessAttempts = []HarnessAttempt{{}}
		}, ""},
		"corrected, then waiting on questions": {func(r *Record) {
			intakeStopped(readiness.LabellingViolationMessage, retryAlsoRejectedSuffix)(r)
			r.AnalysisAttempts = append(r.AnalysisAttempts, AnalysisAttempt{Ordinal: 2, Status: "completed", Response: &readiness.Analysis{}})
			r.Notes = []string{"Clarification required. No coding harness was invoked."}
		}, ""},
		"not blocked": {func(r *Record) {
			intakeStopped(readiness.LabellingViolationMessage, retryAlsoRejectedSuffix)(r)
			r.Status = "failed"
		}, ""},
		"unfinished": {func(r *Record) {
			intakeStopped(readiness.LabellingViolationMessage, retryAlsoRejectedSuffix)(r)
			r.FinishedAt = ""
		}, ""},
		"no attempts": {nil, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := classRecord(c.mutate)
			if got := AnalysisPolicyRejectionClass(r); got != c.want {
				t.Fatalf("class %q want %q", got, c.want)
			}
			if got := HasFailureClass(r, FailureAnalysisPolicyViolation); got != (c.want != "") {
				t.Fatalf("got %t", got)
			}
		})
	}
}

func TestFailureClassProtectedRegressionFailed(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Record)
		want   bool
	}{
		"protected view failed": {func(r *Record) {
			r.Verification = &verification.Result{Protected: &verification.Result{RequiredChecksPassed: false}}
		}, true},
		"protected view failed after candidate passed": {func(r *Record) {
			r.Verification = &verification.Result{RequiredChecksPassed: true, Protected: &verification.Result{}}
		}, true},
		"protected view passed": {func(r *Record) {
			r.Verification = &verification.Result{Protected: &verification.Result{RequiredChecksPassed: true}}
		}, false},
		"only the candidate view failed": {func(r *Record) { r.Verification = &verification.Result{Error: "check failed"} }, false},
		"protected tree errored before a run": {func(r *Record) {
			r.Verification = &verification.Result{Error: "protected tree: boom"}
		}, false},
		"no verification": {nil, false},
		"unfinished": {func(r *Record) {
			r.FinishedAt = ""
			r.Verification = &verification.Result{Protected: &verification.Result{}}
		}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := HasFailureClass(classRecord(c.mutate), FailureProtectedRegressionFailed); got != c.want {
				t.Fatalf("got %t want %t", got, c.want)
			}
		})
	}
}

func TestFailureClassSurvivesTheLedgerRoundTrip(t *testing.T) {
	r := classRecord(func(r *Record) {
		r.HarnessAttempts = []HarnessAttempt{{Result: process.Result{TimedOut: true}}}
		r.Verification = &verification.Result{Protected: &verification.Result{}}
	})
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back Record
	if err = json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got, want := FailureClassesOf(back), []string{FailureProcessTimedOut, FailureProtectedRegressionFailed}; !reflect.DeepEqual(got, want) || !reflect.DeepEqual(FailureClassesOf(r), want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if !ValidFailureClass(FailureAnalysisPolicyViolation) || ValidFailureClass("process-timed-out; rm") {
		t.Fatal("only the enumerated classes are valid")
	}
}

func TestFailureClassOldLedgerWithoutAttemptResultsIsNotCounted(t *testing.T) {
	raw := `{"schemaVersion":"v0","kind":"molecule","id":"mol-old","mode":"ticket","status":"failed","formulaId":"sample","finishedAt":"2026-10-10T10:00:00Z","harnessAttempts":[{"id":"a1"}]}`
	var r Record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	if got := FailureClassesOf(r); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestFailureClassAnalysisPolicyViolationOnTheRealIntakeStopPath(t *testing.T) {
	for _, c := range []struct {
		steps []string
		want  string
	}{
		{[]string{"policy", "policy"}, readiness.PolicyClassLabelling},
		{[]string{"def", "def"}, readiness.PolicyClassInScopeDefinition},
		{[]string{"cap", "cap"}, readiness.PolicyClassEvidenceRequestCap},
		{[]string{"issue-def", "issue-def"}, readiness.PolicyClassDefinitionRef},
		{[]string{"policy", "def", "cap", "issue-def"}, readiness.PolicyClassDefinitionRef},
	} {
		t.Run(strings.Join(c.steps, "+"), func(t *testing.T) {
			r, _, _ := runClasses(t, c.steps...)
			if r.Status != "blocked" || r.FinishedAt == "" {
				t.Fatalf("expected a stopped intake: %s %v", r.Status, r.Notes)
			}
			if got := AnalysisPolicyRejectionClass(r); got != c.want {
				t.Fatalf("class %q want %q; notes %v", got, c.want, r.Notes)
			}
			if !HasFailureClass(r, FailureAnalysisPolicyViolation) {
				t.Fatal("the real stop path must classify as analysis-policy-violation")
			}
		})
	}
	t.Run("a corrected analysis that waits is not a violation", func(t *testing.T) {
		r, _, _ := runClasses(t, "policy", "valid")
		if HasFailureClass(r, FailureAnalysisPolicyViolation) {
			t.Fatalf("status %s notes %v", r.Status, r.Notes)
		}
	})
}

func writeClassLedger(t *testing.T, lab string, r Record) {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(lab, "ledgers")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, r.ID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFailureClassReadRecentFinishedOrdersAndFilters(t *testing.T) {
	lab := t.TempDir()
	if records, skipped, err := ReadRecentFinished(lab); err != nil || records != nil || skipped != nil {
		t.Fatalf("a Lab without ledgers has no records: %v %v %v", records, skipped, err)
	}
	for _, r := range []Record{
		classRecord(func(r *Record) { r.ID = "mol-c"; r.FinishedAt = "2026-10-10T10:00:00Z" }),
		classRecord(func(r *Record) { r.ID = "mol-b"; r.FinishedAt = "2026-10-10T10:00:00Z" }),
		classRecord(func(r *Record) { r.ID = "mol-a"; r.FinishedAt = "2026-10-10T09:59:59.5Z" }),
		classRecord(func(r *Record) { r.ID = "mol-waiting"; r.Status = "waiting"; r.FinishedAt = "" }),
		classRecord(func(r *Record) { r.ID = "mol-other"; r.Mode = "other" }),
	} {
		writeClassLedger(t, lab, r)
	}
	if err := os.WriteFile(filepath.Join(lab, "ledgers", "notes.json"), []byte(`{"hello":"world"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "ledgers", "mol-a.md"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	records, skipped, err := ReadRecentFinished(lab)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("%v %v", err, skipped)
	}
	var ids []string
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "mol-a,mol-b,mol-c" {
		t.Fatalf("finish time, then id: %v", ids)
	}
}

func TestFailureClassReadRecentFinishedSkipsAndReportsAnUnreadableLedger(t *testing.T) {
	lab := t.TempDir()
	writeClassLedger(t, lab, classRecord(nil))
	if err := os.WriteFile(filepath.Join(lab, "ledgers", "mol-bad.json"), []byte(`{"schemaVersion":"v0","kind":"molecule","id":"mol-bad","formulaId":"sample","status":"failed","finishedAt":7}`), 0600); err != nil {
		t.Fatal(err)
	}
	records, skipped, err := ReadRecentFinished(lab)
	if err != nil || len(records) != 1 || records[0].ID != "mol-1" || len(skipped) != 1 || !strings.HasPrefix(skipped[0], "mol-bad.json: ") {
		t.Fatalf("one damaged ledger must not hide the others: %v %v %v", records, skipped, err)
	}
}
