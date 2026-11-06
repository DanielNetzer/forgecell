//go:build linux || darwin

package process

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReturnedProcessHasReconciliationEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		code       int
		timeout    time.Duration
	}{
		{"success", "echo", 0, successBackstop},
		{"nonzero-exit", "unicode", 7, successBackstop},
		{"timeout", "wait", -1, 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Run the test binary directly: Wait reaps the only process in its
			// managed group, so nothing is left over for reconciliation to stop
			// and no ReconcileNote is recorded.
			o := options(tc.mode)
			o.Timeout = tc.timeout
			r := Run(context.Background(), o)
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var evidence map[string]any
			if err := json.Unmarshal(raw, &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence["reconciled"] != true || r.Uncertainty != "" {
				t.Fatalf("missing durable stopped-process evidence: %s", raw)
			}
			if _, ok := evidence["reconcileNote"]; ok {
				t.Fatalf("note recorded without leftovers: %s", raw)
			}
			if r.Code != tc.code || r.TimedOut != (tc.mode == "wait") || r.OK != (tc.code == 0) {
				t.Fatalf("unexpected process outcome: %s", raw)
			}
		})
	}
}

// A leader that exits 0 leaving a live member in its group has that leftover
// stopped; the run stays successful and records why it took longer.
func TestLiveManagedDescendantIsStoppedAndReconciled(t *testing.T) {
	r := Run(context.Background(), Options{Argv: []string{"/bin/sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $!; exit 0"}})
	raw, _ := json.Marshal(r)
	if !r.OK || !r.Reconciled || r.Uncertainty != "" || r.Error != "" {
		t.Fatalf("leftover not reconciled: %s", raw)
	}
	if !strings.Contains(r.ReconcileNote, "leftover") || !strings.HasSuffix(r.ReconcileNote, "with SIGTERM") {
		t.Fatalf("note does not name SIGTERM: %s", raw)
	}
	if !strings.Contains(string(raw), `"reconcileNote":`) {
		t.Fatalf("note not serialized: %s", raw)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(r.Stdout))
	if err != nil {
		t.Fatalf("no leftover pid: %s", raw)
	}
	assertGone(t, pid)
}

// The leftover holds the inherited stdout pipe, so Wait ends with ErrWaitDelay;
// once the leftover is stopped that is success with possibly incomplete output.
func TestLeftoverHoldingPipeIsReconciledAfterWaitDelay(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	r := Run(context.Background(), leftoverOptions("leftover-inherit", pidFile))
	raw, _ := json.Marshal(r)
	if !r.OK || !r.Reconciled || r.Uncertainty != "" || r.Error != "" || r.Code != 0 {
		t.Fatalf("drain-only leftover failed the run: %s", raw)
	}
	if !strings.Contains(r.ReconcileNote, "with SIGTERM") || !strings.Contains(r.ReconcileNote, "output after the harness exited may be incomplete") {
		t.Fatalf("note: %s", raw)
	}
	assertGone(t, readPid(t, pidFile))
}

func TestLeftoverIgnoringSIGTERMIsKilled(t *testing.T) {
	previous := reconcileTermGrace
	reconcileTermGrace = 300 * time.Millisecond
	t.Cleanup(func() { reconcileTermGrace = previous })
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	r := Run(context.Background(), leftoverOptions("leftover-ignore-term", pidFile))
	raw, _ := json.Marshal(r)
	if !r.OK || !r.Reconciled || r.Uncertainty != "" || r.Error != "" {
		t.Fatalf("SIGTERM-ignoring leftover not reconciled: %s", raw)
	}
	if !strings.HasSuffix(r.ReconcileNote, "with SIGKILL after SIGTERM") {
		t.Fatalf("note does not name SIGKILL: %s", raw)
	}
	assertGone(t, readPid(t, pidFile))
}

// A group that never goes away keeps today's failure; each signal follows a
// successful probe immediately.
func TestLeftoverThatCannotBeStoppedStaysUnreconciled(t *testing.T) {
	var events []string
	stubReconcile(t, func(int) error {
		events = append(events, "probe")
		return nil
	}, func(_ int, sig syscall.Signal) error {
		events = append(events, sig.String())
		return nil
	})
	r := Run(context.Background(), options("echo"))
	const text = "managed process group remains active or cannot be verified stopped"
	if r.OK || r.Reconciled || r.Uncertainty != text || r.Error != text || r.ReconcileNote != "" {
		t.Fatalf("%+v", r)
	}
	var signals []string
	for i, e := range events {
		if e != "probe" {
			if i == 0 || events[i-1] != "probe" {
				t.Fatalf("signal not right after a successful probe: %v", events)
			}
			signals = append(signals, e)
		}
	}
	if strings.Join(signals, ",") != syscall.SIGTERM.String()+","+syscall.SIGKILL.String() {
		t.Fatalf("signals %v", signals)
	}
}

// A probe that cannot confirm the group is ours (EPERM) never leads to a signal.
func TestReconcileNeverSignalsUnverifiedGroup(t *testing.T) {
	stubReconcile(t, func(int) error { return syscall.EPERM }, func(int, syscall.Signal) error {
		t.Error("signalled a group the probe did not confirm")
		return nil
	})
	if r := Run(context.Background(), options("echo")); r.OK || r.Reconciled || r.Uncertainty == "" {
		t.Fatalf("%+v", r)
	}
}

// A process that left the group is neither signalled nor claimed.
func TestReconcileLeavesDetachedProcess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	r := Run(context.Background(), leftoverOptions("detached", pidFile))
	pid := readPid(t, pidFile)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	if !r.OK || !r.Reconciled || r.Uncertainty != "" || r.ReconcileNote != "" {
		t.Fatalf("%+v", r)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("detached process was stopped: %v", err)
	}
}

// A shell whose child is killed with the group leaves a zombie until init
// reaps it; reconciliation must wait for that instead of reporting a live group.
func TestKilledGroupWithChildReconcilesAfterTimeout(t *testing.T) {
	for i := 0; i < 20; i++ {
		r := Run(context.Background(), Options{Argv: []string{"/bin/sh", "-c", "sleep 10"}, Timeout: 50 * time.Millisecond})
		if !r.TimedOut || !r.Reconciled || r.Uncertainty != "" {
			t.Fatalf("run %d: killed group not reconciled: %+v", i, r)
		}
	}
}

func stubReconcile(t *testing.T, probe func(int) error, sig func(int, syscall.Signal) error) {
	t.Helper()
	grace, term, p, s := reconcileGrace, reconcileTermGrace, probeGroup, signalGroup
	reconcileGrace, reconcileTermGrace, probeGroup, signalGroup = 400*time.Millisecond, 100*time.Millisecond, probe, sig
	t.Cleanup(func() { reconcileGrace, reconcileTermGrace, probeGroup, signalGroup = grace, term, p, s })
}

func leftoverOptions(mode, pidFile string) Options {
	return Options{
		Argv:    []string{os.Args[0], "-test.run=^TestReconcileHelperProcess$", "--", mode},
		Env:     append(os.Environ(), "FORGECELL_TEST_RECONCILE=1", "FORGECELL_TEST_CHILD_PID="+pidFile),
		Timeout: successBackstop,
	}
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// The group was verified gone, which includes zombies, so the pid no longer exists.
func assertGone(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("leftover %d still exists: %v", pid, err)
	}
}

// TestReconcileHelperProcess is a leader that exits 0 once its child is ready.
func TestReconcileHelperProcess(t *testing.T) {
	if os.Getenv("FORGECELL_TEST_RECONCILE") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "child-sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "child-ignore-term":
		signal.Ignore(syscall.SIGTERM)
		os.WriteFile(os.Getenv("FORGECELL_TEST_READY"), nil, 0600)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	childMode := "child-sleep"
	if mode == "leftover-ignore-term" {
		childMode = "child-ignore-term"
	}
	ready := os.Getenv("FORGECELL_TEST_CHILD_PID") + ".ready"
	child := exec.Command(os.Args[0], "-test.run=^TestReconcileHelperProcess$", "--", childMode)
	child.Env = append(os.Environ(), "FORGECELL_TEST_READY="+ready)
	switch mode {
	case "leftover-inherit":
		child.Stdout = os.Stdout
	case "detached":
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	if childMode == "child-ignore-term" {
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	tmp := os.Getenv("FORGECELL_TEST_CHILD_PID") + ".tmp"
	os.WriteFile(tmp, []byte(strconv.Itoa(child.Process.Pid)), 0600)
	os.Rename(tmp, os.Getenv("FORGECELL_TEST_CHILD_PID"))
	os.Exit(0)
}
