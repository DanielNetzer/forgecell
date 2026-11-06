//go:build linux || darwin

package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

// Seams for tests; both address the whole group created for this invocation.
var (
	probeGroup  = func(pgid int) error { return syscall.Kill(-pgid, 0) }
	signalGroup = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }
)

// Probe only the group created for this invocation. Detached descendants (a new
// session or group) are outside this managed-group observation, not a claim of
// host containment, and are never signalled.
//
// When the run was not cancelled and members remain after the leader was waited
// on, they are leftovers: stop them with SIGTERM, then SIGKILL. POSIX does not
// reuse a PID as a process ID while a process group with that ID still exists,
// so a probe that succeeds means the group is still ours; a signal is sent only
// right after such a probe. A cancelled run was already sent SIGKILL by Cancel.
//
// A killed group member stays a zombie until its new parent (init) reaps it,
// and zombies still count as group members, so an immediate probe can report a
// group that is already dead. Re-probe until reconcileGrace before concluding.
// The returned note names the signal that cleared leftovers, if any.
func reconcileProcess(cmd *exec.Cmd, cancelled bool) (bool, string, string) {
	pgid := cmd.Process.Pid
	deadline := time.Now().Add(reconcileGrace)
	termWait := min(reconcileTermGrace, reconcileGrace/2)
	var attempted, delivered syscall.Signal
	var killAt time.Time
	for {
		err := probeGroup(pgid)
		if errors.Is(err, syscall.ESRCH) {
			return true, "", leftoverNote(delivered)
		}
		now := time.Now()
		if now.After(deadline) {
			return false, "managed process group remains active or cannot be verified stopped", ""
		}
		if err == nil && !cancelled {
			var sig syscall.Signal
			switch {
			case attempted == 0:
				sig, killAt = syscall.SIGTERM, now.Add(termWait)
			case attempted == syscall.SIGTERM && !now.Before(killAt):
				sig = syscall.SIGKILL
			}
			if sig != 0 {
				attempted = sig
				signalErr := signalGroup(pgid, sig)
				if errors.Is(signalErr, syscall.ESRCH) {
					return true, "", leftoverNote(delivered)
				}
				if signalErr == nil {
					delivered = sig
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func leftoverNote(delivered syscall.Signal) string {
	switch delivered {
	case syscall.SIGTERM:
		return "leftover process group members remained after the harness exited and were stopped with SIGTERM"
	case syscall.SIGKILL:
		return "leftover process group members remained after the harness exited and were stopped with SIGKILL after SIGTERM"
	}
	return ""
}

// reconcileGrace bounds how long reconciliation waits for the group to be gone,
// including stopping leftover members. The wait ends as soon as the group is
// gone, so the bound is only reached while members are still alive; a loaded
// machine can take well over 500 ms to reap a killed group, which must not read
// as an unreconciled process.
var reconcileGrace = 5 * time.Second

// reconcileTermGrace bounds how long leftover members have after SIGTERM before
// SIGKILL, capped at half of reconcileGrace so SIGKILL is always tried.
var reconcileTermGrace = 2 * time.Second
