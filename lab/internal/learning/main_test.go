package learning

import (
	"fmt"
	"os"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	// Fixture Labs keep their Lab temp directories under a private root that is
	// removed afterwards, never in the user's real temp root.
	cleanup, err := resources.IsolateLabTempRoot("learning-labtmp-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cleanup()
	return m.Run()
}
