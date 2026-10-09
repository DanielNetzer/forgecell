//go:build !linux && !darwin

package process

import "os/exec"

// Other platforms retain Go's child-process cancellation. They are not release
// targets until process-tree cleanup is implemented and verified for that OS.
func configureProcess(cmd *exec.Cmd) {}

func reconcileProcess(cmd *exec.Cmd) (bool, string) {
	return false, "process-tree reconciliation is unsupported on this platform"
}
