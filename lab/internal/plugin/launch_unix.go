//go:build linux || darwin

package plugin

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const noFollow = syscall.O_NOFOLLOW

// pPID is waitid's P_PID id type on Linux and macOS.
const pPID = 1

func supported() error { return nil }

// configure starts the plugin in its own process group and makes cancellation
// kill the whole group, as process.Run does. The group is signaled only through
// exec's Cancel and reap, both before Wait reaps the leader.
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

// reap waits for the plugin to exit without reaping it, kills whatever it left
// in its process group while the unreaped leader still holds the group id (so
// the id cannot have been reused), then reaps it. A plugin that exits cleanly
// therefore leaves no background child holding needs.env secrets or the stderr
// pipe, and its own exit status is kept. If the non-reaping wait fails the
// group is not signaled, because its id might no longer be the plugin's.
func reap(cmd *exec.Cmd) error {
	pid := cmd.Process.Pid
	if waitExited(pid) == nil {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
	return cmd.Wait()
}

// waitExited blocks until the process has terminated and leaves it waitable.
// waitid can report a stop or continue even when only WEXITED is asked for (as
// macOS does), so waitTerminated accepts a report only when its si_code says the
// process terminated.
func waitExited(pid int) error {
	return waitTerminated(func(info *siginfo) (bool, error) {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(info)), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		switch errno {
		case 0:
			return false, nil
		case syscall.EINTR:
			return true, nil
		default:
			return false, errno
		}
	})
}

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
