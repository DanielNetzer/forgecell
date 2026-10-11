package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

const stallSecret = "sk-forgecell-test-secret-7f3a"

func ledgerText(t *testing.T, lab, id, ext string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(lab, "ledgers", id+ext))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func processStarts(t *testing.T, marker string) int {
	t.Helper()
	raw, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "started")
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake harness never created %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func diagnosticsLine(t *testing.T, markdown string) string {
	t.Helper()
	for _, line := range strings.Split(markdown, "\n") {
		if strings.HasPrefix(line, "Diagnostics: ") {
			return line
		}
	}
	t.Fatal("the ledger Markdown has no diagnostics line")
	return ""
}

// stallScript is a fake coding harness that records each start before it stalls.
func stallScript(marker, body string) string {
	return fmt.Sprintf("#!/bin/sh\ncat >/dev/null\necho started >> %q\n%s", marker, body)
}

func onlyAttempt(t *testing.T, r Record) HarnessAttempt {
	t.Helper()
	if len(r.HarnessAttempts) != 1 {
		t.Fatalf("expected exactly one harness attempt, got %d", len(r.HarnessAttempts))
	}
	return r.HarnessAttempts[0]
}

func requireStoredAttemptMatches(t *testing.T, o Options, r Record, a HarnessAttempt, digest string) {
	t.Helper()
	var stored Record
	if err := json.Unmarshal([]byte(ledgerText(t, o.LabDir, r.ID, ".json")), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.HarnessAttempts) != 1 || stored.HarnessAttempts[0].ID != a.ID || stored.HarnessAttempts[0].PlanDigest != digest {
		t.Fatalf("the stored ledger lost or relabelled the original attempt: %+v", stored.HarnessAttempts)
	}
	if got := stored.HarnessAttempts[0].Result.Diagnostics(); got.TerminalState != a.Result.TerminalState() || got.ElapsedMS != a.Result.ElapsedMS {
		t.Fatalf("stored diagnostics %+v differ from the attempt", got)
	}
	if a.Evidence == nil || stored.HarnessAttempts[0].Evidence == nil || *stored.HarnessAttempts[0].Evidence != *a.Evidence {
		t.Fatalf("the immutable result evidence reference changed: %+v", stored.HarnessAttempts[0].Evidence)
	}
	raw, err := os.ReadFile(filepath.Join(o.LabDir, a.Evidence.Path))
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != a.Evidence.SHA256 {
		t.Fatal("the immutable result file no longer matches its recorded hash")
	}
	var file struct {
		Activity  *process.Activity `json:"activity"`
		TimedOut  bool              `json:"timedOut"`
		AttemptID string            `json:"attemptId"`
	}
	if err = json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.AttemptID != a.ID || file.TimedOut != a.Result.TimedOut || file.Activity == nil || file.Activity.StdoutBytes != a.Result.Activity.StdoutBytes || file.Activity.StderrBytes != a.Result.Activity.StderrBytes {
		t.Fatalf("the immutable result file lacks the activity fields: %s", raw)
	}
}

func TestSilentCodingStallIsDiagnosableAndNeverReplayed(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "starts")
	o := timeoutFixture(t, 1500, 0, 0, stallScript(marker, "exec sleep 10\n"))
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	digest := r.Readiness.Plans[0].Digest
	a := onlyAttempt(t, r)
	if r.Status != "failed" || !a.Result.TimedOut || a.Result.Interrupted || a.Result.Overflow || a.Result.TerminalState() != process.StateTimeout {
		t.Fatalf("not a pure timeout: %s %+v", r.Status, a.Result)
	}
	if act := a.Result.Activity; act == nil || act.StdoutBytes != 0 || act.StderrBytes != 0 || act.LastOutputMS != nil {
		t.Fatalf("a silent stall must be a recorded silent run, not an unrecorded one: %+v", act)
	}
	if a.Result.ElapsedMS < 1500 {
		t.Fatalf("elapsed %d ms is below the 1500 ms timeout", a.Result.ElapsedMS)
	}
	requireStoredAttemptMatches(t, o, r, a, digest)

	markdown := ledgerText(t, o.LabDir, r.ID, ".md")
	for _, want := range []string{
		"Provider process OK: false; exit: ",
		"timeout: true; interruption: false; overflow: false\nReconciled: true;",
		fmt.Sprintf("Diagnostics: elapsedMs=%d; terminal state: timeout; output observed: stdout 0 B, stderr 0 B, last output at: none; reconnect evidence: unknown.", a.Result.ElapsedMS),
		"no output is not proof of a stall and output is not proof of progress",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("the Markdown ledger lacks %q:\n%s", want, markdown)
		}
	}
	if !strings.Contains(codingDetail(t, r), "Harness timed out") {
		t.Fatalf("the timeout gate message changed: %s", codingDetail(t, r))
	}

	if starts := processStarts(t, marker); starts != 1 {
		t.Fatalf("%d coding processes started for one approval", starts)
	}
	replay := o
	replay.Approve = digest
	if _, err = Run(context.Background(), replay); err == nil {
		t.Fatal("the consumed approval digest was accepted again")
	}
	if starts := processStarts(t, marker); starts != 1 {
		t.Fatalf("replaying the consumed digest started %d coding processes in total", starts)
	}
}

func TestPartialOutputThenCodingTimeoutKeepsOutputOutOfDiagnostics(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "starts")
	body := fmt.Sprintf("printf %s\nprintf %s >&2\nexec sleep 10\n", stallSecret, stallSecret)
	o := timeoutFixture(t, 1500, 0, 0, stallScript(marker, body))
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	a := onlyAttempt(t, r)
	if !a.Result.TimedOut || a.Result.TerminalState() != process.StateTimeout {
		t.Fatalf("not a timeout: %+v", a.Result)
	}
	n := int64(len(stallSecret))
	act := a.Result.Activity
	if act == nil || act.StdoutBytes != n || act.StderrBytes != n || act.LastOutputMS == nil || *act.LastOutputMS < 0 || *act.LastOutputMS > a.Result.ElapsedMS {
		t.Fatalf("partial output was not observed within the run: %+v", act)
	}
	requireStoredAttemptMatches(t, o, r, a, r.Readiness.Plans[0].Digest)

	line := diagnosticsLine(t, ledgerText(t, o.LabDir, r.ID, ".md"))
	want := fmt.Sprintf("Diagnostics: elapsedMs=%d; terminal state: timeout; output observed: stdout %d B, stderr %d B, last output at: %d ms; reconnect evidence: unknown.", a.Result.ElapsedMS, n, n, *act.LastOutputMS)
	if !strings.HasPrefix(line, want) {
		t.Fatalf("diagnostics line %q does not start with %q", line, want)
	}
	diagnostics, err := json.Marshal(a.Result.Diagnostics())
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"line": line, "json": string(diagnostics)} {
		if strings.Contains(text, stallSecret) {
			t.Fatalf("the %s leaked process output: %s", name, text)
		}
	}
	evidence, err := os.ReadFile(filepath.Join(o.LabDir, a.Evidence.Path))
	if err != nil || !strings.Contains(string(evidence), stallSecret) {
		t.Fatalf("fixture must prove the sentinel reached both pipes: %v", err)
	}
}

func TestCancelledCodingAttemptIsInterruptedNotTimeoutAndNotReplayed(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "starts")
	script := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nprintf partial\necho started >> %q\nexec sleep 10\n", marker)
	o := timeoutFixture(t, 60000, 0, 0, script)
	planned, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	digest := planned.Readiness.Plans[0].Digest
	approve := o
	approve.Approve = digest
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		r   Record
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := Run(ctx, approve)
		done <- outcome{r, err}
	}()
	waitForFile(t, marker)
	cancel()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("cancelling the approval run did not stop it")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	r := got.r
	a := onlyAttempt(t, r)
	if r.Status != "failed" || !a.Result.Interrupted || a.Result.TimedOut || a.Result.Overflow || a.Result.TerminalState() != process.StateInterrupted {
		t.Fatalf("not a pure interruption: %s %+v", r.Status, a.Result)
	}
	if act := a.Result.Activity; act == nil || act.StdoutBytes != 7 || act.StderrBytes != 0 || act.LastOutputMS == nil || *act.LastOutputMS > a.Result.ElapsedMS {
		t.Fatalf("the partial output seen before cancellation was not recorded: %+v", act)
	}
	requireStoredAttemptMatches(t, o, r, a, digest)
	if line := diagnosticsLine(t, ledgerText(t, o.LabDir, r.ID, ".md")); !strings.Contains(line, "terminal state: interrupted;") {
		t.Fatalf("diagnostics line does not report an interruption: %s", line)
	}
	if !strings.Contains(codingDetail(t, r), "interrupted=true") || strings.Contains(codingDetail(t, r), "Harness timed out") {
		t.Fatalf("an interruption must not be reported as a timeout: %s", codingDetail(t, r))
	}

	replay := o
	replay.Approve = digest
	if _, err = Run(context.Background(), replay); err == nil {
		t.Fatal("the consumed approval digest was accepted again")
	}
	if starts := processStarts(t, marker); starts != 1 {
		t.Fatalf("%d coding processes started in total", starts)
	}
}

func TestRenderedDiagnosticsSeparateSilenceFromNoRecord(t *testing.T) {
	last := int64(40)
	for _, tc := range []struct {
		name string
		r    process.Result
		want string
	}{
		{"older record", process.Result{}, "Diagnostics: elapsedMs=0; terminal state: unknown; output observed: not recorded; reconnect evidence: unknown."},
		{"recorded silence", process.Result{ElapsedMS: 9, TimedOut: true, Reconciled: true, Activity: &process.Activity{}}, "Diagnostics: elapsedMs=9; terminal state: timeout; output observed: stdout 0 B, stderr 0 B, last output at: none; reconnect evidence: unknown."},
		{"observed output", process.Result{ElapsedMS: 90, OK: true, Reconciled: true, Activity: &process.Activity{StdoutBytes: 3, StderrBytes: 2, LastOutputMS: &last}}, "Diagnostics: elapsedMs=90; terminal state: succeeded; output observed: stdout 3 B, stderr 2 B, last output at: 40 ms; reconnect evidence: unknown."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderHarnessDiagnostics(tc.r)
			if !strings.HasPrefix(got, tc.want) || !strings.HasSuffix(got, "\n") || strings.Count(got, "\n") != 1 {
				t.Fatalf("got %q, want prefix %q", got, tc.want)
			}
		})
	}
}

func requireAnalysisDiagnostics(t *testing.T, o Options, r Record, status string, state process.TerminalState) *process.Diagnostics {
	t.Helper()
	if len(r.AnalysisAttempts) != 1 {
		t.Fatalf("expected exactly one analysis attempt, got %+v", r.AnalysisAttempts)
	}
	attempt := r.AnalysisAttempts[0]
	d := attempt.Diagnostics
	if attempt.Ordinal != 1 || attempt.Status != status || d == nil {
		t.Fatalf("the attempt must keep its identity and carry diagnostics: %+v", attempt)
	}
	if d.TerminalState != state || d.Reconnect != "unknown" || d.ElapsedMS < 0 || d.Activity == nil {
		t.Fatalf("diagnostics %+v, want state %s with recorded activity", d, state)
	}
	if d.Activity.LastOutputMS != nil && (*d.Activity.LastOutputMS < 0 || *d.Activity.LastOutputMS > d.ElapsedMS) {
		t.Fatalf("last output %d ms lies outside the %d ms run", *d.Activity.LastOutputMS, d.ElapsedMS)
	}
	for _, ext := range []string{".json", ".md"} {
		if text := ledgerText(t, o.LabDir, r.ID, ext); !strings.Contains(text, fmt.Sprintf(`"terminalState": %q`, state)) {
			t.Fatalf("the %s ledger lacks the stored diagnostics", ext)
		}
	}
	return d
}

func TestAnalysisAttemptsRecordStallInterruptionAndSuccessDiagnostics(t *testing.T) {
	t.Run("silent stall times out", func(t *testing.T) {
		w := newWrapperLab(t, wrapperOptions{body: "cat >/dev/null\nexec sleep 30\n", timeoutMS: 2000})
		r, err := Run(context.Background(), w.o)
		if err != nil {
			t.Fatal(err)
		}
		d := requireAnalysisDiagnostics(t, w.o, r, "failed", process.StateTimeout)
		if d.Activity.StdoutBytes != 0 || d.Activity.StderrBytes != 0 || d.Activity.LastOutputMS != nil || d.ElapsedMS < 2000 {
			t.Fatalf("a silent stall must be recorded as silence: %+v", d)
		}
		if !strings.Contains(r.AnalysisAttempts[0].Error, "deadline") || len(r.HarnessAttempts) != 0 {
			t.Fatalf("stop-intake behaviour changed: %+v", r.AnalysisAttempts[0])
		}
	})
	t.Run("partial output then stall", func(t *testing.T) {
		w := newWrapperLab(t, wrapperOptions{body: "cat >/dev/null\nprintf partial\nexec sleep 30\n", timeoutMS: 2500})
		r, err := Run(context.Background(), w.o)
		if err != nil {
			t.Fatal(err)
		}
		d := requireAnalysisDiagnostics(t, w.o, r, "failed", process.StateTimeout)
		if d.Activity.StdoutBytes != 7 || d.Activity.LastOutputMS == nil {
			t.Fatalf("partial output was not observed: %+v", d.Activity)
		}
	})
	t.Run("cancellation is an interruption, not a timeout", func(t *testing.T) {
		w := newWrapperLab(t, wrapperOptions{body: "cat >/dev/null\nexec sleep 30\n"})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan Record, 1)
		go func() {
			r, _ := Run(ctx, w.o)
			done <- r
		}()
		waitForFile(t, w.ran)
		cancel()
		var r Record
		select {
		case r = <-done:
		case <-time.After(60 * time.Second):
			t.Fatal("cancelling intake did not stop the analysis wrapper")
		}
		if r.Status != "blocked" || len(r.HarnessAttempts) != 0 {
			t.Fatalf("an interrupted analysis must stop intake before coding: %s", r.Status)
		}
		requireAnalysisDiagnostics(t, w.o, r, "failed", process.StateInterrupted)
	})
	t.Run("non-zero exit is a failure", func(t *testing.T) {
		w := newWrapperLab(t, wrapperOptions{body: "cat >/dev/null\nprintf boom >&2\nexit 3\n"})
		r, err := Run(context.Background(), w.o)
		if err != nil {
			t.Fatal(err)
		}
		d := requireAnalysisDiagnostics(t, w.o, r, "failed", process.StateFailed)
		if d.Activity.StderrBytes != 4 || d.Activity.StdoutBytes != 0 {
			t.Fatalf("%+v", d.Activity)
		}
	})
	t.Run("completed attempts carry diagnostics too", func(t *testing.T) {
		w := newWrapperLab(t, wrapperOptions{})
		r, err := Run(context.Background(), w.o)
		if err != nil {
			t.Fatal(err)
		}
		d := requireAnalysisDiagnostics(t, w.o, r, "completed", process.StateSucceeded)
		if d.Activity.StdoutBytes == 0 || d.Activity.LastOutputMS == nil {
			t.Fatalf("the analysis response was not observed on stdout: %+v", d.Activity)
		}
	})
}

func TestSuppliedAnalysisRecordsNoDiagnosticsAndOlderLedgersDecode(t *testing.T) {
	o, _ := fixture(t)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.AnalysisAttempts) != 1 || r.AnalysisAttempts[0].Status != "completed" || r.AnalysisAttempts[0].Diagnostics != nil {
		t.Fatalf("a supplied analysis function observed no process: %+v", r.AnalysisAttempts)
	}
	for _, ext := range []string{".json", ".md"} {
		if strings.Contains(ledgerText(t, o.LabDir, r.ID, ext), `"diagnostics"`) {
			t.Fatalf("the %s ledger claims diagnostics nobody observed", ext)
		}
	}
	var older AnalysisAttempt
	if err = json.Unmarshal([]byte(`{"ordinal":1,"snapshotSha256":"abc","startedAt":"t","status":"failed","error":"analysis failed: x"}`), &older); err != nil || older.Diagnostics != nil || older.Status != "failed" {
		t.Fatalf("an older attempt must decode unchanged: %v %+v", err, older)
	}
}
