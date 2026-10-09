//go:build linux || darwin

package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
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
func reconcileProcess(cmd *exec.Cmd) (bool, string) {
	err := syscall.Kill(-cmd.Process.Pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true, ""
	}
	return false, "managed process group remains active or cannot be verified stopped"
}
