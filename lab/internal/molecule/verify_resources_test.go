package molecule

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// guardReadings is a machine with ample disk and the given available memory.
func guardReadings(memory uint64) resources.Readings {
	return resources.Readings{Volumes: []resources.Volume{{Path: "/lab", Free: 50 * resources.GiB, Total: 100 * resources.GiB}}, Memory: memory, MemoryKnown: true}
}

// waitForCancel is a guard Sleep that never lets time pass on its own: the soft-floor
// wait gets no re-check and a monitor samples only once, until stopped.
func waitForCancel(ctx context.Context, _ time.Duration) bool {
	<-ctx.Done()
	return false
}

func noteContaining(r Record, text string) bool {
	for _, n := range r.Notes {
		if strings.Contains(n, text) {
			return true
		}
	}
	return false
}

func TestVerifySoftFloorStopsCleanlyWithoutStartingChecks(t *testing.T) {
	o, _ := fixture(t)
	low := resources.Readings{Volumes: []resources.Volume{{Path: "/lab", Free: resources.GiB, Total: 100 * resources.GiB}}, Memory: 8 * resources.GiB, MemoryKnown: true}
	reads, waits := 0, 0
	o.Resources = &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings {
		reads++
		return low
	}), Sleep: func(context.Context, time.Duration) bool {
		waits++
		return true
	}}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "blocked" || !noteContaining(r, "resources: low disk") || !noteContaining(r, "verification was not started") {
		t.Fatalf("status %q notes %v; want a clean resources: low disk stop", r.Status, r.Notes)
	}
	if r.Atoms[3].Status != "blocked" {
		t.Fatalf("check atom = %q, want blocked", r.Atoms[3].Status)
	}
	if len(r.VerificationAttempts) != 0 {
		t.Fatalf("verification started below the soft floor: %+v", r.VerificationAttempts)
	}
	// The full 10-minute wait was simulated: 21 checks, 20 waits of 30 s, no real sleep.
	if reads != 21 || waits != 20 {
		t.Fatalf("reads %d, waits %d; want 21 and 20", reads, waits)
	}
}

func TestVerifyHardFloorCancelsInFlightCheckAndRecordsReason(t *testing.T) {
	o, _ := fixture(t)
	marker := filepath.Join(t.TempDir(), "check-started")
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, err := analyze(ctx, b, i, s)
		if err == nil {
			// The check signals that it is running, then outlives any test.
			a.Checks[0].Argv = []string{"/bin/sh", "-c", "touch '" + marker + "'; sleep 60"}
			a.Checks[0].TimeoutMS = 120000
		}
		return a, err
	}
	// Readings fall below the memory hard floor once the check is running.
	o.Resources = &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings {
		if _, err := os.Stat(marker); err == nil {
			return guardReadings(resources.GiB)
		}
		return guardReadings(8 * resources.GiB)
	}), Sleep: func(ctx context.Context, _ time.Duration) bool {
		// Each monitor sample waits on the check's progress, not on a clock.
		for {
			if _, err := os.Stat(marker); err == nil {
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(5 * time.Millisecond):
			}
		}
	}}
	started := time.Now()
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 50*time.Second {
		t.Fatal("the hard floor did not cancel the in-flight check")
	}
	if !noteContaining(r, "resources-exhausted: low memory") || !noteContaining(r, "in-flight checks were cancelled") {
		t.Fatalf("notes %v; want the resources-exhausted reason recorded", r.Notes)
	}
	if len(r.VerificationAttempts) != 1 || r.VerificationAttempts[0].Status == "passed" {
		t.Fatalf("verification attempts %+v; want one attempt that did not pass", r.VerificationAttempts)
	}
	if r.Atoms[3].Status == "done" {
		t.Fatal("a cancelled check must not mark verification done")
	}
}

func TestVerifyPassingChecksGetNoCancellationNote(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	o, _ := fixture(t)
	// The Lab's check TMPDIR is created under the temp root (the test's $TMPDIR); remove it after.
	t.Cleanup(func() {
		if labTmp, err := resources.LabTempPath(o.LabDir); err == nil {
			_ = resources.RemoveTree(labTmp)
		}
	})
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, err := analyze(ctx, b, i, s)
		if err == nil {
			// A go argv records a cache state; a non-go one (sh, gofmt) records none.
			a.Checks[0].Argv = []string{"go", "env", "GOFLAGS"}
		}
		return a, err
	}
	o.Resources = &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings {
		return guardReadings(8 * resources.GiB)
	}), Sleep: waitForCancel}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.VerificationAttempts) != 1 || r.VerificationAttempts[0].Status != "passed" {
		t.Fatalf("verification attempts %+v; want one passing attempt", r.VerificationAttempts)
	}
	if noteContaining(r, "resources") {
		t.Fatalf("notes %v; a passing verification must carry no resources note", r.Notes)
	}
	// The views ran with the Lab's stable TMPDIR and the shared Go cache, and the ledger
	// states each check's cache state. The fixture has no go.mod, so its change lies
	// outside every module root and every go check ran fresh, saying why.
	// That TMPDIR sits under the temp root, outside the Lab, and <lab>/tmp is unused.
	labTmp, err := resources.LabTempPath(o.LabDir)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(labTmp); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("stable Lab TMPDIR: %v %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(labTmp, resources.LabTempMarker)); err != nil {
		t.Fatalf("stable Lab TMPDIR has no Forgecell marker: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(o.LabDir, "tmp")); !os.IsNotExist(err) {
		t.Fatalf("<lab>/tmp must no longer be created: %v", err)
	}
	if r.Verification == nil || len(r.Verification.Checks) == 0 {
		t.Fatalf("verification %+v", r.Verification)
	}
	for _, c := range r.Verification.Checks {
		want := ""
		if c.Command.Argv[0] == "go" {
			want = resources.CacheFreshNonModule
		}
		if c.Cache != want {
			t.Fatalf("check %s cache state %q, want %q", c.ID, c.Cache, want)
		}
	}
	ledger, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".md"))
	if err != nil || !strings.Contains(string(ledger), "Candidate Go test cache: ") || !strings.Contains(string(ledger), "cache: disabled (fresh: non-module change)") {
		t.Fatalf("ledger cache line missing: %v\n%s", err, ledger)
	}
}

// TestGoCacheLinesReportCachedVersusRun renders the ledger cache summary per view, and
// nothing for a record that predates the cache fields.
func TestGoCacheLinesReportCachedVersusRun(t *testing.T) {
	v := verification.Result{Checks: []verification.Observation{
		{ID: "acceptance", Cache: resources.CacheFresh, Packages: &verification.PackageCounts{Run: 1}},
		{ID: "go-test", Cache: resources.CacheEnabled, Packages: &verification.PackageCounts{Cached: 40, Run: 3}},
		{ID: "gofmt", Cache: resources.CacheEnabled},
	}}
	v.Protected = &verification.Result{Checks: []verification.Observation{{ID: "go-test", Cache: resources.CacheEnabled, Packages: &verification.PackageCounts{Cached: 43}}}}
	got := goCacheLines(v)
	want := "Candidate Go test cache: 40 packages served (cached), 4 run; `acceptance` cache: disabled (fresh); `go-test` cache: enabled; `gofmt` cache: enabled\n" +
		"Protected regression Go test cache: 43 packages served (cached), 0 run; `go-test` cache: enabled\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := goCacheLines(verification.Result{Checks: []verification.Observation{{ID: "old"}}}); got != "" {
		t.Fatalf("an old record rendered %q", got)
	}
}
