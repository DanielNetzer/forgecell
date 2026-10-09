//go:build !darwin && !linux

package molecule

import "fmt"

func confirmProcessExited(pid int) error {
	return fmt.Errorf("process recovery is not supported on this platform")
}
