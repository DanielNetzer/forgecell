package molecule

import (
	"os"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
)

// Serial: swaps the harness package-level launcher resolver.
func TestCodingAttemptResolvesItsLauncherOnceAndRecordsTheClassItWasGatedOn(t *testing.T) {
	o, _ := claudeAllowlistFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var resolved []string
	restore := harness.SetLauncherResolver(func(saved string) harness.LauncherIdentity {
		resolved = append(resolved, saved)
		id := harness.ResolveLauncher(saved)
		if len(resolved) > 1 {
			// A second resolution would disagree with the first, so the attempt
			// either blocks on it or records a class it was not gated on.
			id.Class = harness.LauncherUnknown
		}
		return id
	})
	t.Cleanup(restore)

	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0] != exe {
		t.Fatalf("saved launcher resolved %d times (%q), want exactly once for %q", len(resolved), resolved, exe)
	}
	if r.Status != "waiting" || len(r.HarnessAttempts) != 1 || !r.HarnessAttempts[0].Result.OK {
		t.Fatalf("the attempt gated on the resolved launcher did not run: %+v", r)
	}
	got := r.HarnessAttempts[0].Forgecell
	if got == nil || got.SavedLauncher != exe || got.LauncherClass != harness.LauncherRunning {
		t.Fatalf("recorded provenance %+v is not the identity the attempt was gated on (%s)", got, harness.LauncherRunning)
	}
}
