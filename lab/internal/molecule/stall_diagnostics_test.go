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
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
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
	deadline := time.Now().Add(markerBackstop)
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

// stallTimeoutMS is the Formula or wrapper timeout under test for every stall
// fixture. It is far above any start-up delay a loaded machine adds (10 s was
// not: under go test -count=3 -p 8 a wrapper's deadline expired before its
// script had started), and the fixtures sleep past it. The stall tests reach
// their timeout through expireWhenStarted once the fixture has started, so the
// timeout is never waited out; process.Run tests its own wall-clock timeout.
const stallTimeoutMS = 45000

// maxGateMessageBytes bounds a coding gate message with room for the longest
// temporary paths; the message must not grow with the harness output.
const maxGateMessageBytes = 4096

// requireGateDiagnostics checks that a gate message and the ledger notes carry
// the bounded diagnostics sentence, and that neither carries output text.
func requireGateDiagnostics(t *testing.T, r Record, sentence string) {
	t.Helper()
	detail := codingDetail(t, r)
	if !strings.Contains(detail, sentence) {
		t.Fatalf("the gate message lacks %q:\n%s", sentence, detail)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), sentence) {
		t.Fatalf("the ledger notes lack %q: %v", sentence, r.Notes)
	}
	if len(detail) > maxGateMessageBytes {
		t.Fatalf("the gate message is %d bytes, over the %d byte bound", len(detail), maxGateMessageBytes)
	}
	for name, text := range map[string]string{"gate message": detail, "notes": strings.Join(r.Notes, "\n")} {
		if strings.Contains(text, stallSecret) {
			t.Fatalf("the %s leaked process output: %s", name, text)
		}
	}
}

// onlyLedgerID finds the one Molecule ledger in a Lab whose Run has not returned.
func onlyLedgerID(t *testing.T, lab string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(lab, "ledgers", "mol-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one Molecule ledger, got %v (%v)", matches, err)
	}
	return strings.TrimSuffix(filepath.Base(matches[0]), ".json")
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
	t.Parallel()
	dir := t.TempDir()
	marker, ready := filepath.Join(dir, "starts"), filepath.Join(dir, "ready")
	// The shell creates the starts file before it writes the line, so the
	// deadline waits for a separate marker made after the start is recorded.
	o := timeoutFixture(t, stallTimeoutMS, 0, 0, stallScript(marker, fmt.Sprintf(": > %q\nexec sleep 60\n", ready)))
	r, err := runApprovedExpiring(t, o, ready)
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
	requireStoredAttemptMatches(t, o, r, a, digest)

	sentence := fmt.Sprintf("Diagnostics: elapsedMs=%d; terminal state: timeout; output observed: stdout 0 B, stderr 0 B, last output at: none; reconnect evidence: unknown.", a.Result.ElapsedMS)
	markdown := ledgerText(t, o.LabDir, r.ID, ".md")
	for _, want := range []string{
		"Provider process OK: false; exit: ",
		"timeout: true; interruption: false; overflow: false\nReconciled: true;",
		sentence,
		"no output is not proof of a stall and output is not proof of progress",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("the Markdown ledger lacks %q:\n%s", want, markdown)
		}
	}
	if !strings.Contains(codingDetail(t, r), "Harness timed out") {
		t.Fatalf("the timeout gate message changed: %s", codingDetail(t, r))
	}
	requireGateDiagnostics(t, r, sentence)

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
	t.Parallel()
	dir := t.TempDir()
	marker, written := filepath.Join(dir, "starts"), filepath.Join(dir, "written")
	// The deadline expires only after both pipes carry the sentinel.
	body := fmt.Sprintf("printf %s\nprintf %s >&2\n: > %q\nexec sleep 60\n", stallSecret, stallSecret, written)
	o := timeoutFixture(t, stallTimeoutMS, 0, 0, stallScript(marker, body))
	r, err := runApprovedExpiring(t, o, written)
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
	requireGateDiagnostics(t, r, want)
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
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "starts")
	ready := filepath.Join(dir, "ready")
	script := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nprintf partial\necho started >> %q\n: > %q\nexec sleep 60\n", marker, ready)
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
	waitForFile(t, ready)
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
	requireGateDiagnostics(t, r, fmt.Sprintf("Diagnostics: elapsedMs=%d; terminal state: interrupted; output observed: stdout 7 B, stderr 0 B, last output at: %d ms; reconnect evidence: unknown.", a.Result.ElapsedMS, *a.Result.Activity.LastOutputMS))

	replay := o
	replay.Approve = digest
	if _, err = Run(context.Background(), replay); err == nil {
		t.Fatal("the consumed approval digest was accepted again")
	}
	if starts := processStarts(t, marker); starts != 1 {
		t.Fatalf("%d coding processes started in total", starts)
	}
}

func TestNonTimeoutFailureGateMessageCarriesTheBoundedDiagnostics(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	body := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nprintf %s\nprintf %s >&2\nexit 7\n", stallSecret, stallSecret)
	if err := writeStub(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	a := onlyAttempt(t, r)
	if r.Status != "failed" || a.Result.Code != 7 || a.Result.TimedOut || a.Result.TerminalState() != process.StateFailed || a.Result.Activity == nil || a.Result.Activity.LastOutputMS == nil {
		t.Fatalf("not a plain failure: %s %+v", r.Status, a.Result)
	}
	n := int64(len(stallSecret))
	if a.Result.Activity.StdoutBytes != n || a.Result.Activity.StderrBytes != n {
		t.Fatalf("both pipes must carry the sentinel: %+v", a.Result.Activity)
	}
	detail := codingDetail(t, r)
	if !strings.HasPrefix(detail, "Harness failed: exit=7 ") || strings.Contains(detail, "Harness timed out") || strings.Contains(detail, "forgecell recover") {
		t.Fatalf("the failure message changed shape: %s", detail)
	}
	requireGateDiagnostics(t, r, fmt.Sprintf("Diagnostics: elapsedMs=%d; terminal state: failed; output observed: stdout %d B, stderr %d B, last output at: %d ms; reconnect evidence: unknown.", a.Result.ElapsedMS, n, n, *a.Result.Activity.LastOutputMS))
}

func TestExpiredCallerDeadlineIsNotReportedAsAHarnessTimeout(t *testing.T) {
	summary := harnessOutcomeSummary(process.Result{Code: -1, ElapsedMS: 12, ParentDeadline: true, Reconciled: true, Error: context.DeadlineExceeded.Error(), Activity: &process.Activity{}}, VerificationFile{Path: "evidence/result.json", SHA256: "abc"})
	for _, want := range []string{
		"Harness failed: exit=-1 ",
		"timeout=false",
		"Diagnostics: elapsedMs=12; terminal state: parent-deadline; output observed: stdout 0 B, stderr 0 B, last output at: none; reconnect evidence: unknown.",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("the summary lacks %q: %s", want, summary)
		}
	}
	if strings.Contains(summary, "Harness timed out") || strings.Contains(summary, "forgecell recover") {
		t.Fatalf("the generic failure summary must stay unchanged for a caller deadline: %s", summary)
	}

	gate := timeoutGate{Molecule: "mol-1", Lab: "/lab", Digest: "sha256:abc", Attempt: "attempt-1", TimeoutMS: 600000, Source: "formula", CeilingMS: 1200000}
	result := process.Result{Code: -1, ElapsedMS: 12, ParentDeadline: true, Reconciled: true, Error: context.DeadlineExceeded.Error(), Activity: &process.Activity{}}
	ref := VerificationFile{Path: "evidence/result.json", SHA256: "abc"}
	kept := harnessTimeoutSummary(gate, result, ref, verification.CaptureResult{Tree: "tree123"}, nil)
	for _, want := range []string{
		"Harness stopped by a caller deadline before its own timeout: elapsedMs=12 timeoutMs=600000 (formula)",
		"retained tree tree123",
		"forgecell recover mol-1 --plan sha256:abc --confirm-stopped",
		"forgecell amend mol-1 --parent sha256:abc",
		`"adoptAttempt":"attempt-1"`,
		"terminal state: parent-deadline",
		"a longer harnessTimeoutMs would not have helped",
	} {
		if !strings.Contains(kept, want) {
			t.Fatalf("the caller-deadline gate lacks %q:\n%s", want, kept)
		}
	}
	if strings.Contains(kept, "Harness timed out") || strings.Contains(kept, "set harnessTimeoutMs up to") {
		t.Fatalf("a caller deadline must not be worded as the harness timeout:\n%s", kept)
	}
}

// Live activity is only known once the process returns, so a saved in-flight
// attempt must read as unknown rather than as a silent run.
func TestInFlightAttemptsRenderUnknownNotZeroActivity(t *testing.T) {
	t.Parallel()
	const unknown = "Diagnostics: elapsedMs=0; terminal state: unknown; output observed: not recorded; reconnect evidence: unknown."
	stop := func(t *testing.T, cancel context.CancelFunc, done <-chan Record) Record {
		t.Helper()
		cancel()
		select {
		case r := <-done:
			return r
		case <-time.After(60 * time.Second):
			t.Fatal("cancelling the run did not stop it")
		}
		return Record{}
	}
	t.Run("coding attempt", func(t *testing.T) {
		t.Parallel()
		marker := filepath.Join(t.TempDir(), "starts")
		o := timeoutFixture(t, 60000, 0, 0, stallScript(marker, "printf partial\nexec sleep 60\n"))
		planned, err := Run(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		approve := o
		approve.Approve = planned.Readiness.Plans[0].Digest
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan Record, 1)
		go func() {
			r, _ := Run(ctx, approve)
			done <- r
		}()
		waitForFile(t, marker)
		line := diagnosticsLine(t, ledgerText(t, o.LabDir, planned.ID, ".md"))
		if !strings.HasPrefix(line, unknown) || strings.Contains(line, " B,") {
			t.Fatalf("an in-flight attempt must read as unknown, got %q", line)
		}
		var stored Record
		if err = json.Unmarshal([]byte(ledgerText(t, o.LabDir, planned.ID, ".json")), &stored); err != nil {
			t.Fatal(err)
		}
		if len(stored.HarnessAttempts) != 1 || stored.HarnessAttempts[0].Result.Activity != nil || stored.HarnessAttempts[0].Evidence != nil {
			t.Fatalf("an in-flight attempt must carry no activity evidence: %+v", stored.HarnessAttempts)
		}
		r := stop(t, cancel, done)
		a := onlyAttempt(t, r)
		if a.Result.TerminalState() != process.StateInterrupted || a.Result.Activity == nil {
			t.Fatalf("the finished attempt must replace the unknown reading: %+v", a.Result)
		}
		if line = diagnosticsLine(t, ledgerText(t, o.LabDir, r.ID, ".md")); !strings.Contains(line, "terminal state: interrupted;") {
			t.Fatalf("the finished attempt still reads as unknown: %s", line)
		}
	})
	t.Run("analysis attempt", func(t *testing.T) {
		t.Parallel()
		w := newWrapperLab(t, wrapperOptions{body: "cat >/dev/null\nprintf partial\nexec sleep 60\n"})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan Record, 1)
		go func() {
			r, _ := Run(ctx, w.o)
			done <- r
		}()
		waitForFile(t, w.ran)
		id := onlyLedgerID(t, w.o.LabDir)
		var stored Record
		if err := json.Unmarshal([]byte(ledgerText(t, w.o.LabDir, id, ".json")), &stored); err != nil {
			t.Fatal(err)
		}
		if len(stored.AnalysisAttempts) != 1 || stored.AnalysisAttempts[0].Status != "started" || stored.AnalysisAttempts[0].Diagnostics != nil {
			t.Fatalf("an in-flight analysis attempt must carry no diagnostics: %+v", stored.AnalysisAttempts)
		}
		for _, ext := range []string{".json", ".md"} {
			text := ledgerText(t, w.o.LabDir, id, ext)
			if strings.Contains(text, `"diagnostics"`) || strings.Contains(text, "\nDiagnostics: ") || strings.Contains(text, "output observed") {
				t.Fatalf("the %s ledger claims activity nobody has observed yet:\n%s", ext, text)
			}
		}
		r := stop(t, cancel, done)
		requireAnalysisDiagnostics(t, w.o, r, "failed", process.StateInterrupted)
	})
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
		{"parent deadline", process.Result{ElapsedMS: 7, ParentDeadline: true, Reconciled: true, Activity: &process.Activity{}}, "Diagnostics: elapsedMs=7; terminal state: parent-deadline; output observed: stdout 0 B, stderr 0 B, last output at: none; reconnect evidence: unknown."},
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
	last := "none"
	if d.Activity.LastOutputMS != nil {
		last = fmt.Sprintf("%d ms", *d.Activity.LastOutputMS)
	}
	sentence := fmt.Sprintf("\nDiagnostics: elapsedMs=%d; terminal state: %s; output observed: stdout %d B, stderr %d B, last output at: %s; reconnect evidence: unknown. Observed pipe bytes only", d.ElapsedMS, state, d.Activity.StdoutBytes, d.Activity.StderrBytes, last)
	if text := ledgerText(t, o.LabDir, r.ID, ".md"); !strings.Contains(text, sentence) || strings.Count(text, "\nDiagnostics: ") != 1 {
		t.Fatalf("the Markdown ledger lacks the bounded diagnostics sentence for the analysis attempt %q:\n%s", sentence, text)
	}
	return d
}

func TestAnalysisAttemptsRecordStallInterruptionAndSuccessDiagnostics(t *testing.T) {
	t.Parallel()
	t.Run("silent stall times out", func(t *testing.T) {
		t.Parallel()
		w := newWrapperLab(t, wrapperOptions{body: "cat >/dev/null\nexec sleep 60\n", timeoutMS: stallTimeoutMS})
		ctx, stop := expireWhenStarted(t, w.ran)
		defer stop()
		r, err := Run(ctx, w.o)
		if err != nil {
			t.Fatal(err)
		}
		d := requireAnalysisDiagnostics(t, w.o, r, "failed", process.StateTimeout)
		if d.Activity.StdoutBytes != 0 || d.Activity.StderrBytes != 0 || d.Activity.LastOutputMS != nil {
			t.Fatalf("a silent stall must be recorded as silence: %+v", d)
		}
		if !strings.Contains(r.AnalysisAttempts[0].Error, "deadline") || len(r.HarnessAttempts) != 0 {
			t.Fatalf("stop-intake behaviour changed: %+v", r.AnalysisAttempts[0])
		}
	})
	t.Run("partial output then stall", func(t *testing.T) {
		t.Parallel()
		written := filepath.Join(t.TempDir(), "written")
		w := newWrapperLab(t, wrapperOptions{body: fmt.Sprintf("cat >/dev/null\nprintf partial\n: > %q\nexec sleep 60\n", written), timeoutMS: stallTimeoutMS})
		ctx, stop := expireWhenStarted(t, written)
		defer stop()
		r, err := Run(ctx, w.o)
		if err != nil {
			t.Fatal(err)
		}
		d := requireAnalysisDiagnostics(t, w.o, r, "failed", process.StateTimeout)
		if d.Activity.StdoutBytes != 7 || d.Activity.LastOutputMS == nil {
			t.Fatalf("partial output was not observed: %+v", d.Activity)
		}
	})
	t.Run("cancellation is an interruption, not a timeout", func(t *testing.T) {
		t.Parallel()
		w := newWrapperLab(t, wrapperOptions{body: "cat >/dev/null\nexec sleep 60\n"})
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
		t.Parallel()
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
		t.Parallel()
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
	t.Parallel()
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
