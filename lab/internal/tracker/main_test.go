package tracker

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

// runMain points TMPDIR at a private root removed afterwards, so the fake gh
// that t.TempDir holds never lands in the user's real temp root.
func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "tracker-tmp-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	if err := os.Setenv("TMPDIR", dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}
