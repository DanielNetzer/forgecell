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

// Probe only the group created for this invocation. Never signal a group after
// Wait: its leader identity may already have been reused. Detached descendants
// are outside this managed-group observation, not a claim of host containment.
//
// A SIGKILLed group member stays a zombie until its new parent (init) reaps
// it, and zombies still count as group members, so an immediate probe can
// report a group that is already dead. Re-probe briefly before concluding.
func reconcileProcess(cmd *exec.Cmd) (bool, string) {
	deadline := time.Now().Add(reconcileGrace)
	for {
		err := syscall.Kill(-cmd.Process.Pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true, ""
		}
		if time.Now().After(deadline) {
			return false, "managed process group remains active or cannot be verified stopped"
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// reconcileGrace bounds how long reconciliation waits for killed group members
// to be reaped. It never signals the group, so leader-PID reuse is not a risk.
// The wait ends as soon as the group is gone, so the bound is only reached
// while members are still alive; a loaded machine can take well over 500 ms to
// reap a killed group, which must not read as an unreconciled process.
var reconcileGrace = 5 * time.Second
