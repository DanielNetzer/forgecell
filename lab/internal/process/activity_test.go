package process

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const secretSentinel = "sk-forgecell-test-secret-7f3a"

// stallTimeout is the real timeout under test for silent fixtures that stall on
// purpose, where the deadline itself is the behavior. Fixtures that must write
// output first expire on a readiness marker instead (expireOnMarker), so output
// scheduling under a loaded machine never races a deadline. The fixtures sleep
// well past it.
const stallTimeout = 10 * time.Second

// successBackstop bounds a run that is expected to finish, or to be stopped by
// a marker, on its own. It is not under test: it is far above any start-up
// delay on a loaded machine and still ends a real hang.
const successBackstop = 2 * time.Minute

// markerBackstop bounds how long a test waits for a fixture's readiness marker.
// Fixtures that are meant to be stopped sleep longer than it.
const markerBackstop = 50 * time.Second

func shell(script string, timeout time.Duration) Options {
	return Options{Argv: []string{"sh", "-c", script}, Timeout: timeout}
}

// markedShell is shell whose script finds the readiness marker path in
// $FORGECELL_TEST_MARKER, under the test's own temporary directory.
func markedShell(t *testing.T, script string, timeout time.Duration) (Options, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ready")
	o := shell(script, timeout)
	o.Env = append(os.Environ(), "FORGECELL_TEST_MARKER="+marker)
	return o, marker
}

// expireOnMarker returns a context that is cancelled with
// context.DeadlineExceeded as soon as marker exists, so a test reaches a
// deadline once the fixture has provably produced what the test needs instead
// of racing a real one. Without a parent deadline Run reports that cause as its
// own timeout; with parentDeadline the context also carries a deadline, beyond
// the marker backstop and earlier than the run's own Timeout, so Run reports a
// caller deadline. A fixture that never writes the marker is cancelled as an
// interruption once markerBackstop passes, which no deadline assertion accepts.
func expireOnMarker(t *testing.T, marker string, parentDeadline bool) context.Context {
	t.Helper()
	base, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(context.Canceled) })
	ctx := base
	if parentDeadline {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(base, time.Now().Add(2*markerBackstop))
		t.Cleanup(stop)
	}
	go func() {
		deadline := time.Now().Add(markerBackstop)
		for base.Err() == nil {
			if _, err := os.Stat(marker); err == nil {
				cancel(context.DeadlineExceeded)
				return
			}
			if time.Now().After(deadline) {
				cancel(context.Canceled)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return ctx
}

func requireActivity(t *testing.T, r Result, stdout, stderr int64) *Activity {
	t.Helper()
	if r.Activity == nil {
		t.Fatalf("activity was not recorded: %+v", r)
	}
	if r.Activity.StdoutBytes != stdout || r.Activity.StderrBytes != stderr {
		t.Fatalf("observed bytes stdout=%d stderr=%d, want %d and %d", r.Activity.StdoutBytes, r.Activity.StderrBytes, stdout, stderr)
	}
	return r.Activity
}

func requireLastOutputWithinRun(t *testing.T, r Result) {
	t.Helper()
	if r.Activity == nil || r.Activity.LastOutputMS == nil {
		t.Fatalf("no last-output offset recorded: %+v", r.Activity)
	}
	if last := *r.Activity.LastOutputMS; last < 0 || last > r.ElapsedMS {
		t.Fatalf("last output offset %d ms lies outside the %d ms run", last, r.ElapsedMS)
	}
}

func TestSilentStallTimesOutWithRecordedSilence(t *testing.T) {
	t.Parallel()
	r := Run(context.Background(), shell("exec sleep 60", stallTimeout))
	if r.OK || !r.TimedOut || r.ParentDeadline || r.Interrupted || r.Overflow {
		t.Fatalf("not a pure timeout: %+v", r)
	}
	if r.TerminalState() != StateTimeout {
		t.Fatalf("terminal state %q", r.TerminalState())
	}
	if r.ElapsedMS < stallTimeout.Milliseconds() || r.ElapsedMS >= 60000 {
		t.Fatalf("elapsed %d ms is not bounded by the %d ms timeout", r.ElapsedMS, stallTimeout.Milliseconds())
	}
	if a := requireActivity(t, r, 0, 0); a.LastOutputMS != nil {
		t.Fatalf("a silent run reported a last-output offset %d", *a.LastOutputMS)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"activity":{"stdoutBytes":0,"stderrBytes":0}`) {
		t.Fatalf("a recorded silent run must stay distinguishable from an unrecorded one: %s", raw)
	}
}

func TestPartialOutputThenStallRecordsLastOutput(t *testing.T) {
	t.Parallel()
	// The marker follows the output, so the expiry cannot race it; the stall
	// sleeps past the marker backstop.
	o, marker := markedShell(t, `printf partial; printf err >&2; : > "$FORGECELL_TEST_MARKER"; exec sleep 60`, time.Hour)
	r := Run(expireOnMarker(t, marker, false), o)
	if r.OK || !r.TimedOut || r.TerminalState() != StateTimeout {
		t.Fatalf("not a timeout: %+v", r)
	}
	requireActivity(t, r, 7, 3)
	requireLastOutputWithinRun(t, r)
	if r.Stdout != "partial" || r.Stderr != "err" {
		t.Fatalf("partial output was not preserved: %q %q", r.Stdout, r.Stderr)
	}
}

func TestCancellationAfterPartialOutputIsInterruptedNotTimeout(t *testing.T) {
	o, marker := markedShell(t, `printf partial; : > "$FORGECELL_TEST_MARKER"; exec sleep 120`, successBackstop)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Result, 1)
	go func() { done <- Run(ctx, o) }()
	deadline := time.Now().Add(markerBackstop)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake harness never signalled that it started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	var r Result
	select {
	case r = <-done:
	case <-time.After(markerBackstop):
		t.Fatal("cancellation did not stop the run")
	}
	if r.OK || !r.Interrupted || r.TimedOut || r.Overflow || r.TerminalState() != StateInterrupted {
		t.Fatalf("not a pure interruption: %+v", r)
	}
	requireActivity(t, r, 7, 0)
	requireLastOutputWithinRun(t, r)
	if r.ElapsedMS >= successBackstop.Milliseconds() {
		t.Fatalf("elapsed %d ms reached the timeout", r.ElapsedMS)
	}
}

func TestOutputLimitCountsDroppedBytesAndIsNotATimeout(t *testing.T) {
	script := "i=0; while [ $i -lt 5000 ]; do printf 0123456789012345678901234567890123456789; i=$((i+1)); done"
	o := shell(script, successBackstop)
	o.MaxOutputBytes = 1024
	r := Run(context.Background(), o)
	if r.OK || !r.Overflow || r.TimedOut || r.Interrupted || r.TerminalState() != StateOutputLimit {
		t.Fatalf("not a pure output-limit stop: %+v", r)
	}
	if len(r.Stdout) > 1024 {
		t.Fatalf("captured %d bytes past the cap", len(r.Stdout))
	}
	if a := r.Activity; a == nil || a.StdoutBytes <= 1024 || a.StderrBytes != 0 {
		t.Fatalf("bytes dropped past the cap must still be counted: %+v", a)
	}
	requireLastOutputWithinRun(t, r)
}

func TestSuccessfulRunRecordsActivityAndSucceeds(t *testing.T) {
	r := Run(context.Background(), shell("printf ok", successBackstop))
	if !r.OK || r.TerminalState() != StateSucceeded {
		t.Fatalf("%+v", r)
	}
	requireActivity(t, r, 2, 0)
	requireLastOutputWithinRun(t, r)
	if r.ElapsedMS >= successBackstop.Milliseconds() {
		t.Fatalf("elapsed %d ms reached the timeout", r.ElapsedMS)
	}
	silent := Run(context.Background(), shell("exit 0", successBackstop))
	if !silent.OK || silent.TerminalState() != StateSucceeded {
		t.Fatalf("%+v", silent)
	}
	if a := requireActivity(t, silent, 0, 0); a.LastOutputMS != nil {
		t.Fatalf("a silent success reported a last-output offset %d", *a.LastOutputMS)
	}
}

func TestNonzeroExitAndStartFailureAreFailedNotTimeouts(t *testing.T) {
	r := Run(context.Background(), shell("printf boom >&2; exit 7", successBackstop))
	if r.OK || r.Code != 7 || r.TerminalState() != StateFailed {
		t.Fatalf("%+v", r)
	}
	requireActivity(t, r, 0, 4)
	missing := Run(context.Background(), Options{Argv: []string{"/nonexistent/forgecell-test"}})
	if missing.OK || missing.TerminalState() != StateFailed {
		t.Fatalf("%+v", missing)
	}
	if missing.Activity != nil {
		t.Fatalf("a process that never started has no pipe observation: %+v", missing.Activity)
	}
}

func TestTerminalStateClassifierUsesOnlyExistingFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Result
		want TerminalState
	}{
		{"clean exit", Result{OK: true, Reconciled: true}, StateSucceeded},
		{"timeout", Result{TimedOut: true, Reconciled: true, Code: -1}, StateTimeout},
		{"parent deadline", Result{ParentDeadline: true, Reconciled: true, Code: -1}, StateParentDeadline},
		{"parent deadline then output cap is still the parent deadline", Result{ParentDeadline: true, Overflow: true, Reconciled: true}, StateParentDeadline},
		{"unreconciled outranks a parent deadline", Result{ParentDeadline: true, Reconciled: false, Uncertainty: "managed process group remains active"}, StateUnreconciled},
		{"interruption", Result{Interrupted: true, Reconciled: true, Code: -1}, StateInterrupted},
		{"output limit", Result{Overflow: true, Reconciled: true, Code: -1}, StateOutputLimit},
		{"timeout then output cap is still the timeout", Result{TimedOut: true, Overflow: true, Reconciled: true}, StateTimeout},
		{"interruption then output cap is still the interruption", Result{Interrupted: true, Overflow: true, Reconciled: true}, StateInterrupted},
		{"unreconciled outranks the cause that stopped it", Result{TimedOut: true, Reconciled: false, Uncertainty: "managed process group remains active"}, StateUnreconciled},
		{"non-zero exit", Result{Reconciled: true, Code: 7}, StateFailed},
		{"start failure", Result{Reconciled: true, Code: -1, Error: "executable file not found"}, StateFailed},
		{"synthesised refusal without a process", Result{Code: -1, Error: "binding was not invoked"}, StateFailed},
		{"exit zero rejected by the harness layer", Result{Reconciled: true, Error: "harness did not return a final result"}, StateFailed},
		{"zero value carries no evidence", Result{}, StateUnknown},
		{"older record without a reconciled flag is not claimed unreconciled", Result{Code: 0}, StateUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.TerminalState(); got != tc.want {
				t.Fatalf("terminal state %q, want %q", got, tc.want)
			}
			if got := tc.r.Diagnostics().Reconnect; got != ReconnectUnknown || ReconnectUnknown != "unknown" {
				t.Fatalf("reconnect evidence %q", got)
			}
		})
	}
}

func TestRecordedSilenceIsDistinguishableFromNoRecord(t *testing.T) {
	old, err := json.Marshal(Result{Reconciled: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(old), "activity") {
		t.Fatalf("an unrecorded result must not claim activity data: %s", old)
	}
	var decoded Result
	if err = json.Unmarshal(old, &decoded); err != nil || decoded.Activity != nil {
		t.Fatalf("older records must decode unchanged: %v %+v", err, decoded.Activity)
	}
	if d := (Result{Reconciled: true}).Diagnostics(); d.Activity != nil {
		t.Fatalf("diagnostics invented activity: %+v", d.Activity)
	}
	silent := Result{Reconciled: true, Activity: &Activity{}}
	if d := silent.Diagnostics(); d.Activity == nil || d.Activity.StdoutBytes != 0 || d.Activity.LastOutputMS != nil {
		t.Fatalf("a recorded silent run lost its record: %+v", d.Activity)
	}
	last := int64(5)
	source := Result{Reconciled: true, Activity: &Activity{StdoutBytes: 3, LastOutputMS: &last}}
	copied := source.Diagnostics()
	*copied.Activity.LastOutputMS = 99
	copied.Activity.StdoutBytes = 99
	if *source.Activity.LastOutputMS != 5 || source.Activity.StdoutBytes != 3 {
		t.Fatal("diagnostics alias the process result")
	}
}

func TestDiagnosticsNeverContainOutputText(t *testing.T) {
	t.Parallel()
	script := fmt.Sprintf(`printf %s; printf %s >&2; : > "$FORGECELL_TEST_MARKER"; exec sleep 60`, secretSentinel, secretSentinel)
	o, marker := markedShell(t, script, time.Hour)
	r := Run(expireOnMarker(t, marker, false), o)
	if !r.TimedOut || !strings.Contains(r.Stdout, secretSentinel) || !strings.Contains(r.Stderr, secretSentinel) {
		t.Fatalf("fixture did not observe the sentinel on both pipes: %+v", r)
	}
	requireActivity(t, r, int64(len(secretSentinel)), int64(len(secretSentinel)))
	d := r.Diagnostics()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"json": string(raw), "formatted": fmt.Sprintf("%+v %v %s", d, d.Activity, d.TerminalState)} {
		if strings.Contains(text, secretSentinel) || strings.Contains(text, "sleep") {
			t.Fatalf("diagnostics %s leaked output or argv: %s", name, text)
		}
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		switch key {
		case "elapsedMs", "activity", "terminalState", "reconnect":
		default:
			t.Fatalf("unexpected diagnostics field %q", key)
		}
	}
}

func TestActivityTrackingIsRaceFreeAcrossBothPipes(t *testing.T) {
	script := "i=0; while [ $i -lt 200 ]; do printf aaaaaaaaaa; printf bbbbb >&2; i=$((i+1)); done"
	r := Run(context.Background(), shell(script, successBackstop))
	if !r.OK {
		t.Fatalf("%+v", r)
	}
	requireActivity(t, r, 2000, 1000)
	requireLastOutputWithinRun(t, r)
}

func TestExpiredParentDeadlineIsNotTheProcessTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r := Run(ctx, shell("exec sleep 60", stallTimeout))
	if r.OK || r.TimedOut || !r.ParentDeadline || r.Interrupted || r.Overflow || !r.Reconciled || r.Uncertainty != "" {
		t.Fatalf("not a pure parent deadline: %+v", r)
	}
	if r.TerminalState() != StateParentDeadline || r.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("terminal state %q, error %q", r.TerminalState(), r.Error)
	}
	d := r.Diagnostics()
	if d.TerminalState != StateParentDeadline || d.Reconnect != ReconnectUnknown {
		t.Fatalf("diagnostics %+v", d)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		switch key {
		case "elapsedMs", "activity", "terminalState", "reconnect":
		default:
			t.Fatalf("the parent-deadline distinction must travel in terminalState only, got field %q", key)
		}
	}
	if fields["terminalState"] != "parent-deadline" {
		t.Fatalf("diagnostics JSON %s", raw)
	}
}

func TestParentDeadlineWhileRunningStopsTheProcessGroup(t *testing.T) {
	t.Parallel()
	// The caller's deadline expires once the output has been written, and the
	// context's own deadline stays earlier than the run's hour-long Timeout.
	o, marker := markedShell(t, `printf partial; : > "$FORGECELL_TEST_MARKER"; exec sleep 120`, time.Hour)
	r := Run(expireOnMarker(t, marker, true), o)
	if r.OK || r.TimedOut || !r.ParentDeadline || r.Interrupted || r.Overflow || !r.Reconciled || r.Uncertainty != "" {
		t.Fatalf("not a pure parent deadline: %+v", r)
	}
	if r.TerminalState() != StateParentDeadline || r.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("terminal state %q, error %q", r.TerminalState(), r.Error)
	}
	requireActivity(t, r, 7, 0)
	requireLastOutputWithinRun(t, r)
	if r.ElapsedMS >= 60000 {
		t.Fatalf("elapsed %d ms: the parent deadline did not stop the process", r.ElapsedMS)
	}
}

func TestOwnTimeoutAndCancellationKeepTheirLabelsUnderAParentDeadline(t *testing.T) {
	t.Run("own timeout before a later parent deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		r := Run(ctx, shell("exec sleep 60", time.Nanosecond))
		if r.OK || !r.TimedOut || r.ParentDeadline || r.Interrupted || r.Overflow || r.TerminalState() != StateTimeout || r.Error != context.DeadlineExceeded.Error() {
			t.Fatalf("the process's own timeout lost its label: %+v", r)
		}
	})
	t.Run("cancelling a deadline-bearing parent is an interruption", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		cancel()
		r := Run(ctx, shell("exec sleep 60", 2*time.Hour))
		if r.OK || r.TimedOut || r.ParentDeadline || !r.Interrupted || r.TerminalState() != StateInterrupted {
			t.Fatalf("a cancelled parent is not a deadline: %+v", r)
		}
	})
}

func TestIncompleteCoversEveryWayARunIsCutShort(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Result
		want bool
	}{
		{"clean exit", Result{OK: true, Reconciled: true}, false},
		{"plain failure", Result{Reconciled: true, Code: 1}, false},
		{"own timeout", Result{TimedOut: true, Reconciled: true, Code: -1}, true},
		{"caller deadline", Result{ParentDeadline: true, Reconciled: true, Code: 1}, true},
		{"interruption", Result{Interrupted: true, Reconciled: true, Code: 1}, true},
		{"output cap", Result{Overflow: true, Reconciled: true, Code: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.Incomplete(); got != tc.want {
				t.Fatalf("Incomplete() = %t, want %t for %+v", got, tc.want, tc.r)
			}
		})
	}
}

func TestParentDeadlineFlagIsAdditiveInTheRecord(t *testing.T) {
	plain, err := json.Marshal(Result{Reconciled: true, TimedOut: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "parentDeadline") {
		t.Fatalf("an ordinary result must keep its earlier encoding: %s", plain)
	}
	var older Result
	if err = json.Unmarshal(plain, &older); err != nil || older.ParentDeadline || older.TerminalState() != StateTimeout {
		t.Fatalf("older records must decode unchanged: %v %+v", err, older)
	}
	flagged, err := json.Marshal(Result{Reconciled: true, ParentDeadline: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(flagged), `"parentDeadline":true`) || !strings.Contains(string(flagged), `"timedOut":false`) {
		t.Fatalf("the flag is not recorded: %s", flagged)
	}
	var decoded Result
	if err = json.Unmarshal(flagged, &decoded); err != nil || decoded.TerminalState() != StateParentDeadline {
		t.Fatalf("the flag did not round-trip: %v %+v", err, decoded)
	}
}
