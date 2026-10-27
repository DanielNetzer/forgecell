package molecule

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// fakePhaseClock replaces phaseClock with one that advances one second per reading,
// so every timed phase lasts a whole, known number of seconds.
func fakePhaseClock(t *testing.T) {
	t.Helper()
	original := phaseClock
	t.Cleanup(func() { phaseClock = original })
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	phaseClock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Second)
		return now
	}
}

// TestPhaseLogTimesAndBoundsPhases: a phase lasts from Begin to its end, ending it again
// records nothing, the log keeps at most maxRecordedPhases and says when it cut, and the
// note names the longest phase. Serial: it replaces phaseClock.
func TestPhaseLogTimesAndBoundsPhases(t *testing.T) {
	fakePhaseClock(t)
	l := &phaseLog{}
	end := l.Begin("candidate", verification.PhaseWorkspace)
	end()
	end()
	l.add("", PhaseSlotWait, 3*time.Minute)
	l.add("", PhaseTreeCapture, -time.Second)
	got, cut := l.timings()
	want := []PhaseTiming{{Phase: verification.PhaseWorkspace, View: "candidate", DurationMS: 1000}, {Phase: PhaseSlotWait, DurationMS: 180000}, {Phase: PhaseTreeCapture, DurationMS: 0}}
	if cut || !slices.Equal(got, want) {
		t.Fatalf("timings %+v cut %v", got, cut)
	}
	note := phaseNote(got, cut)
	if !strings.HasPrefix(note, "Verification preparation phases: candidate workspace prepare 1s; waiting for a slot 3m0s; tree capture 0s.") || !strings.HasSuffix(note, "Longest: waiting for a slot.") {
		t.Fatalf("note %q", note)
	}
	for i := 0; i < 2*maxRecordedPhases; i++ {
		l.add("", PhaseCheckTempDir, time.Millisecond)
	}
	if got, cut = l.timings(); len(got) != maxRecordedPhases || !cut || !strings.Contains(phaseNote(got, cut), "Only the first 32 phases are recorded.") {
		t.Fatalf("%d phases cut %v", len(got), cut)
	}
	if phaseNote(nil, false) != "" {
		t.Fatal("an empty log rendered a note")
	}
}

// TestVerificationRecordsPhaseTimings: a Molecule's verification attempt records the
// timed phases between coding capture and its first check (tree capture, waiting for a
// slot, check TMPDIR prepare and each view's workspace, environment and cache
// preparation) with the slot it ran in, and the ledger notes them. Serial: it replaces
// phaseClock.
func TestVerificationRecordsPhaseTimings(t *testing.T) {
	fakePhaseClock(t)
	o, _ := fixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.VerificationAttempts) == 0 {
		t.Fatalf("no verification ran: phase %s notes %v", statePhase(r), r.Notes)
	}
	a := r.VerificationAttempts[len(r.VerificationAttempts)-1]
	names := []string{}
	for _, p := range a.Phases {
		names = append(names, strings.TrimSpace(p.View+" "+p.Phase))
		if p.DurationMS < 0 || p.DurationMS%1000 != 0 {
			t.Fatalf("phase %+v was not measured with the injected clock", p)
		}
		if p.Phase == PhaseTreeCapture && p.DurationMS != 1000 {
			t.Fatalf("tree capture %+v", p)
		}
	}
	for _, want := range []string{PhaseTreeCapture, PhaseSlotWait, PhaseCheckTempDir, "candidate " + verification.PhaseWorkspace, "candidate " + verification.PhaseEnvironment, "candidate " + verification.PhaseCache} {
		if !slices.Contains(names, want) {
			t.Fatalf("phases %v lack %q", names, want)
		}
	}
	if a.Slot == nil || *a.Slot != 0 || a.Slots < 1 {
		t.Fatalf("slot %v of %d", a.Slot, a.Slots)
	}
	found := false
	for _, note := range r.Notes {
		if strings.HasPrefix(note, "Verification preparation phases: tree capture 1s;") {
			found = !strings.Contains(note, o.LabDir)
		}
	}
	if !found {
		t.Fatalf("notes %v", r.Notes)
	}
	saved, err := ReadRecord(o.LabDir, r.ID)
	if err != nil || len(saved.VerificationAttempts) == 0 || len(saved.VerificationAttempts[len(saved.VerificationAttempts)-1].Phases) != len(a.Phases) {
		t.Fatalf("the phases were not saved: %v", err)
	}
}
