//go:build linux || darwin

package resources

import (
	"errors"
	"os"
	"syscall"
)

// diskSpace returns the bytes available to this user and the size of the filesystem
// holding path.
func diskSpace(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	size := uint64(st.Bsize)
	return uint64(st.Bavail) * size, uint64(st.Blocks) * size, nil
}

// ownedByCurrentUser reports whether info, the Lstat of path, belongs to the user
// running Forgecell.
func ownedByCurrentUser(_ string, info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// pidAlive reports whether a process with pid exists. A process owned by another user
// (EPERM) is alive; only ESRCH proves it is gone.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
