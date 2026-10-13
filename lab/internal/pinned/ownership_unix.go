//go:build linux || darwin

package pinned

import (
	"fmt"
	"os"
	"syscall"
)

// trustedUID says which owners may hold a pinned file or one of its ancestors:
// root and the user running Forgecell.
var trustedUID = func(uid uint32) bool { return uid == 0 || uid == uint32(os.Geteuid()) }

func uidOf(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint32(st.Uid), true
}

// LinkCount reports the hard-link count of a file, and false when the platform
// does not expose it.
func LinkCount(info os.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}

// OpenNoFollow opens the file at path for reading without following a symbolic
// link, and without blocking on a planted FIFO.
func OpenNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// ChmodNoFollow changes the mode of the regular file or directory at path
// without ever following a symbolic link: the path is opened with O_NOFOLLOW and
// the mode is applied to that handle. O_NONBLOCK keeps a planted FIFO from
// blocking the open. It refuses a path that is not the regular file or directory
// described by want.
func ChmodNoFollow(path string, mode os.FileMode, want os.FileInfo) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	got, err := file.Stat()
	if err != nil {
		return err
	}
	if !got.Mode().IsRegular() && !got.IsDir() {
		return fmt.Errorf("%s is not a regular file or directory", path)
	}
	if !os.SameFile(want, got) {
		return fmt.Errorf("%s was replaced while its mode was being changed", path)
	}
	return file.Chmod(mode)
}
