//go:build linux || darwin

package plugin

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

const noFollow = syscall.O_NOFOLLOW

func supported() error { return nil }

// configure starts the plugin in its own process group and makes cancellation
// kill the whole group, as process.Run does. The group is signaled only through
// exec's Cancel, which runs before Wait reaps the leader.
func configure(cmd *exec.Cmd) {
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

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
