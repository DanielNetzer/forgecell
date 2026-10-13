//go:build !linux && !darwin

package pinned

import (
	"fmt"
	"os"
)

// Ownership and link counts are not read on other platforms; CheckPlacement
// already refuses to run on Windows.
var trustedUID = func(uid uint32) bool { return true }

func uidOf(info os.FileInfo) (uint32, bool) { return 0, false }

// LinkCount reports the hard-link count of a file, and false when the platform
// does not expose it.
func LinkCount(info os.FileInfo) (uint64, bool) { return 0, false }

// OpenNoFollow is unsupported where O_NOFOLLOW is unavailable.
func OpenNoFollow(path string) (*os.File, error) {
	return nil, fmt.Errorf("cannot open %s without following symbolic links on this platform", path)
}

// ChmodNoFollow is unsupported where O_NOFOLLOW is unavailable.
func ChmodNoFollow(path string, mode os.FileMode, want os.FileInfo) error {
	return fmt.Errorf("cannot change the mode of %s without following symbolic links on this platform", path)
}
