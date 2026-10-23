package molecule

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
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
	o, _ := fixture(t)
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
}
