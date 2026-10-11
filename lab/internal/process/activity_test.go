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

func shell(script string, timeout time.Duration) Options {
	return Options{Argv: []string{"sh", "-c", script}, Timeout: timeout}
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
	r := Run(context.Background(), shell("exec sleep 30", 400*time.Millisecond))
	if r.OK || !r.TimedOut || r.Interrupted || r.Overflow {
		t.Fatalf("not a pure timeout: %+v", r)
	}
	if r.TerminalState() != StateTimeout {
		t.Fatalf("terminal state %q", r.TerminalState())
	}
	if r.ElapsedMS < 400 || r.ElapsedMS > 10000 {
		t.Fatalf("elapsed %d ms is not bounded by the 400 ms timeout", r.ElapsedMS)
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
	r := Run(context.Background(), shell("printf partial; printf err >&2; exec sleep 30", 1500*time.Millisecond))
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
	marker := filepath.Join(t.TempDir(), "started")
	o := shell(`printf partial; : > "$FORGECELL_TEST_MARKER"; exec sleep 30`, time.Minute)
	o.Env = append(os.Environ(), "FORGECELL_TEST_MARKER="+marker)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Result, 1)
	go func() { done <- Run(ctx, o) }()
	deadline := time.Now().Add(15 * time.Second)
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
	case <-time.After(15 * time.Second):
		t.Fatal("cancellation did not stop the run")
	}
	if r.OK || !r.Interrupted || r.TimedOut || r.Overflow || r.TerminalState() != StateInterrupted {
		t.Fatalf("not a pure interruption: %+v", r)
	}
	requireActivity(t, r, 7, 0)
	requireLastOutputWithinRun(t, r)
	if r.ElapsedMS >= int64(time.Minute/time.Millisecond) {
		t.Fatalf("elapsed %d ms reached the timeout", r.ElapsedMS)
	}
}

func TestOutputLimitCountsDroppedBytesAndIsNotATimeout(t *testing.T) {
	script := "i=0; while [ $i -lt 5000 ]; do printf 0123456789012345678901234567890123456789; i=$((i+1)); done"
	o := shell(script, 20*time.Second)
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
	r := Run(context.Background(), shell("printf ok", 10*time.Second))
	if !r.OK || r.TerminalState() != StateSucceeded {
		t.Fatalf("%+v", r)
	}
	requireActivity(t, r, 2, 0)
	requireLastOutputWithinRun(t, r)
	if r.ElapsedMS >= 10_000 {
		t.Fatalf("elapsed %d ms reached the timeout", r.ElapsedMS)
	}
	silent := Run(context.Background(), shell("exit 0", 10*time.Second))
	if !silent.OK || silent.TerminalState() != StateSucceeded {
		t.Fatalf("%+v", silent)
	}
	if a := requireActivity(t, silent, 0, 0); a.LastOutputMS != nil {
		t.Fatalf("a silent success reported a last-output offset %d", *a.LastOutputMS)
	}
}

func TestNonzeroExitAndStartFailureAreFailedNotTimeouts(t *testing.T) {
	r := Run(context.Background(), shell("printf boom >&2; exit 7", 10*time.Second))
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
	script := fmt.Sprintf("printf %s; printf %s >&2; exec sleep 30", secretSentinel, secretSentinel)
	r := Run(context.Background(), shell(script, 1500*time.Millisecond))
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
	r := Run(context.Background(), shell(script, 20*time.Second))
	if !r.OK {
		t.Fatalf("%+v", r)
	}
	requireActivity(t, r, 2000, 1000)
	requireLastOutputWithinRun(t, r)
}
