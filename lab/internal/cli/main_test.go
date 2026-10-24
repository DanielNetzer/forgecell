package cli

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/install"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	// Fixture Labs keep their Lab temp directories under a private root that is
	// removed afterwards, never in the user's real temp root.
	cleanup, err := resources.IsolateLabTempRoot("cli-labtmp-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cleanup()
	// Fixture release executables answer the version and protocol probes at once;
	// only a loaded machine makes them slow, so widen the production 15 s budget.
	install.ProbeTimeout = 2 * time.Minute
	return m.Run()
}
