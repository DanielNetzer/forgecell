package evaluation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// cohortReadings is a machine with the given free disk (of 100 GiB) and available memory.
func cohortReadings(freeDisk, memory uint64) resources.Readings {
	return resources.Readings{Volumes: []resources.Volume{{Path: "/lab", Free: freeDisk, Total: 100 * resources.GiB}}, Memory: memory, MemoryKnown: true}
}

// idleSleep is a guard Sleep that lets no time pass on its own: a monitor samples once
// and then waits until it is stopped.
func idleSleep(ctx context.Context, _ time.Duration) bool {
	<-ctx.Done()
	return false
}

func attemptRecorded(o CohortOptions, task, variant string, rep int) bool {
	_, err := os.Stat(filepath.Join(o.Output, "attempts", task, variant, "rep-"+itoa(rep), "attempt.json"))
	return err == nil
}

func TestCohortGuardCapsParallelByMemoryAndNotesIt(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	o.Parallel = 3
	o.Guard = &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings {
		return cohortReadings(50*resources.GiB, 8*resources.GiB)
	}), Sleep: idleSleep}
	var logged []string
	o.Log = func(s string) { logged = append(logged, s) }
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "complete" || len(r.Attempts) != 2 {
		t.Fatalf("status=%s attempts=%d", r.Status, len(r.Attempts))
	}
	// 8 GiB allows one attempt per 4 GiB: min(3, max(1, 2)) = 2.
	want := "--parallel 3 reduced to 2"
	noted := false
	for _, n := range r.Notes {
		noted = noted || strings.Contains(n, want)
	}
	if !noted || len(logged) != 1 || !strings.Contains(logged[0], want) {
		t.Fatalf("notes %v, log %v; want %q in both", r.Notes, logged, want)
	}
	for _, cases := range []struct {
		requested int
		memory    uint64
		want      int
	}{{3, 8 * resources.GiB, 2}, {3, 3 * resources.GiB, 1}, {2, 64 * resources.GiB, 2}, {4, 17 * resources.GiB, 4}} {
		if got := resources.EffectiveParallel(cases.requested, cases.memory); got != cases.want {
			t.Fatalf("EffectiveParallel(%d, %s) = %d, want %d", cases.requested, resources.Format(cases.memory), got, cases.want)
		}
	}
}

func TestCohortSoftFloorLeavesCoordinateUnrecordedAndStops(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	waits := 0
	// 10 GiB free of 100 GiB is below the 15 GiB soft floor but above the 5 GiB hard one.
	o.Guard = &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings {
		return cohortReadings(10*resources.GiB, 8*resources.GiB)
	}), Sleep: func(ctx context.Context, d time.Duration) bool {
		if d == resources.DefaultSample {
			return idleSleep(ctx, d)
		}
		waits++
		return true
	}}
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "stopped" || !strings.HasPrefix(r.Stopped, "resources: low disk") {
		t.Fatalf("status=%s stopped=%q; want a resources: low disk stop", r.Status, r.Stopped)
	}
	if len(r.Attempts) != 0 || attemptRecorded(o, "task-a", "base", 1) {
		t.Fatalf("attempts %d; no attempt may start or be recorded below the soft floor", len(r.Attempts))
	}
	// The 10-minute wait at 30 s per re-check was simulated without sleeping.
	if waits != 20 {
		t.Fatalf("soft-floor waits = %d, want 20", waits)
	}
}

func TestCohortHardFloorDiscardsAttemptAndResumeReRunsIt(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("FORGECELL_TEST_MARKER", marker)
	o := cohortFixture(t, []variantSpec{{name: "base", body: stickyHarness, timeoutMS: 30000}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	o.Parallel = 1
	// Readings fall below the memory hard floor once the base harness is running (it
	// writes the marker before it sleeps).
	running := func() bool { _, err := os.Stat(marker); return err == nil }
	o.Guard = &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings {
		if running() {
			return cohortReadings(50*resources.GiB, resources.GiB)
		}
		return cohortReadings(50*resources.GiB, 8*resources.GiB)
	}), Sleep: func(ctx context.Context, _ time.Duration) bool {
		// A monitor sample waits on the harness's progress, not on a clock.
		for !running() {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(5 * time.Millisecond):
			}
		}
		return true
	}}
	started := time.Now()
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatalf("a hard-floor stop must be clean, got %v", err)
	}
	if time.Since(started) > 50*time.Second {
		t.Fatal("the hard floor did not cancel the in-flight attempt")
	}
	if r.Status != "stopped" || !strings.HasPrefix(r.Stopped, "resources-exhausted: low memory") {
		t.Fatalf("status=%s stopped=%q; want a resources-exhausted stop", r.Status, r.Stopped)
	}
	if attemptRecorded(o, "task-a", "base", 1) || len(r.Attempts) != 0 {
		t.Fatalf("the cancelled attempt was recorded (attempts %d); it must be discarded", len(r.Attempts))
	}
	// Resume with ample resources re-runs the discarded coordinate.
	o.Guard = &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings {
		return cohortReadings(50*resources.GiB, 8*resources.GiB)
	}), Sleep: idleSleep}
	r2, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if r2.Status != "complete" || len(r2.Attempts) != 2 || !attemptRecorded(o, "task-a", "base", 1) {
		t.Fatalf("resume status=%s attempts=%d; want the discarded attempt re-run", r2.Status, len(r2.Attempts))
	}
}
