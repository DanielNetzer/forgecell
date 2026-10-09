//go:build darwin || linux

package molecule

import (
	"errors"
	"fmt"
	"syscall"
)

func confirmProcessExited(pid int) error {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return fmt.Errorf("Lab process %d is active or cannot be verified stopped; recovery refused", pid)
}
