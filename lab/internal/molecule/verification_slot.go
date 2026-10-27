package molecule

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"sync"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// verificationSlotFile is the Lab's first verification slot, a flock in the
// ledgers directory; slot i > 0 is verificationSlotName(i). The first keeps the
// name a single-slot Lab always used, so an older binary and this one never run
// checks in the same first slot. Their loopLockSuffix keeps them out of the
// ledger inventory's entry cap, and like every flockLedgerFile lock they are never
// removed: the operating system releases one when its holder exits, including on a
// crash.
const verificationSlotFile = "verification-slot" + loopLockSuffix

// verificationSlotName is the lock file of slot i, for i below
// resources.MaxVerificationSlots.
func verificationSlotName(i int) string {
	if i == 0 {
		return verificationSlotFile
	}
	return fmt.Sprintf("verification-slot-%d%s", i, loopLockSuffix)
}

// verificationSlotPollInterval is how often a queued Molecule retries the slot.
const verificationSlotPollInterval = 2 * time.Second

// QueuedForChecksLabel is how a Molecule waiting for the slot is reported.
const QueuedForChecksLabel = "queued for checks"

// tryVerificationSlot takes one slot's lock without blocking; tests replace it.
var tryVerificationSlot = func(lab, name string) (func(), error) { return flockLedgerFile(lab, name) }

// verificationSlotTicker paces a queued Molecule's retries; tests replace it
// with a ticker they drive.
var verificationSlotTicker = func() (<-chan time.Time, func()) {
	t := time.NewTicker(verificationSlotPollInterval)
	return t.C, t.Stop
}

// verificationSlotLimit is how many slots the Molecule may choose from now, and why
// fewer than configured; tests replace it.
var verificationSlotLimit = labVerificationSlots

// labVerificationSlots is the Lab's configured slot limit (resources.json,
// default 1), reduced by the resource guard: extra slots are refused below a soft
// floor or without memory for them. A config that cannot be read uses one slot.
func labVerificationSlots(o Options) (int, string) {
	configured, err := resources.LoadVerificationSlots(o.LabDir)
	if err != nil {
		return 1, err.Error() + "; one verification slot is used"
	}
	if o.Resources == nil {
		return configured, ""
	}
	return o.Resources.VerificationSlots(configured)
}

// acquireVerificationSlot takes the Lab's only verification slot; see
// acquireVerificationSlots.
func acquireVerificationSlot(ctx context.Context, lab string, queued func() error) (func(), error) {
	release, _, err := acquireVerificationSlots(ctx, lab, func() int { return 1 }, queued)
	return release, err
}

// acquireVerificationSlots takes the first free one of the Lab's verification
// slots below limit(), which is asked again on every try that finds the first slot
// held, so a resource change applies to the next try. When every slot is held,
// queued runs once before the first retry and the wait polls until a slot is free
// or ctx ends. The wait has no deadline of its own: queue time is not check time. A
// lock error other than contention is returned at once.
func acquireVerificationSlots(ctx context.Context, lab string, limit func() int, queued func() error) (func(), int, error) {
	release, slot, err := trySlots(lab, limit)
	if !errors.Is(err, errLocked) {
		return release, slot, err
	}
	if err = ctx.Err(); err != nil {
		return nil, -1, err
	}
	if err = queued(); err != nil {
		return nil, -1, err
	}
	tick, stop := verificationSlotTicker()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil, -1, ctx.Err()
		case <-tick:
		}
		// A cancellation that raced the tick wins, so a stopped Molecule never starts checks.
		if err = ctx.Err(); err != nil {
			return nil, -1, err
		}
		release, slot, err = trySlots(lab, limit)
		if !errors.Is(err, errLocked) {
			return release, slot, err
		}
	}
}

// trySlots tries slots in order and returns the first one taken, or errLocked when
// all are held. The first slot is always tried; limit is asked only when it is held,
// so an uncontended Molecule takes no extra resource sample. The limit is kept within
// 1..resources.MaxVerificationSlots.
func trySlots(lab string, limit func() int) (func(), int, error) {
	release, err := trySlot(lab, verificationSlotName(0))
	if !errors.Is(err, errLocked) {
		if err != nil {
			return nil, -1, err
		}
		return release, 0, nil
	}
	n := max(1, min(limit(), resources.MaxVerificationSlots))
	for i := 1; i < n; i++ {
		release, err := trySlot(lab, verificationSlotName(i))
		if !errors.Is(err, errLocked) {
			if err != nil {
				return nil, -1, err
			}
			return release, i, nil
		}
	}
	return nil, -1, errLocked
}

// trySlot is tryVerificationSlot with any path dropped from its error, so a
// ledger note never carries the Lab's absolute location.
func trySlot(lab, name string) (func(), error) {
	release, err := tryVerificationSlot(lab, name)
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return nil, fmt.Errorf("%s %s: %w", pathErr.Op, name, pathErr.Err)
	}
	return release, err
}

// QueuedForChecks reports a running Molecule that waits for the Lab's
// verification slot.
func QueuedForChecks(r Record) bool {
	return (r.Status == "running" || r.Status == "allocating") && r.Readiness != nil && r.Readiness.QueuedForChecks()
}

// maxQueueScan bounds how many running Molecules QueuedMolecules reads.
const maxQueueScan = 64

// QueuedMolecules lists the running Molecules in lab that wait for the
// verification slot, in the order they queued (earliest first). It reads at most
// maxQueueScan running ledgers in check-pending and reports truncated when there
// were more; unreadable ledgers are skipped.
func QueuedMolecules(lab string) (ids []string, truncated bool) {
	type queuedAt struct{ id, at string }
	queued, read := []queuedAt{}, 0
	for _, a := range ReadQueueInventory(lab).Attempts {
		if (a.Status != "running" && a.Status != "allocating") || a.Phase != "check-pending" {
			continue
		}
		if read == maxQueueScan {
			truncated = true
			break
		}
		read++
		r, err := readMolecule(lab, a.ID)
		if err == nil && QueuedForChecks(r) {
			queued = append(queued, queuedAt{r.ID, r.Readiness.Events[len(r.Readiness.Events)-1].At})
		}
	}
	sort.SliceStable(queued, func(i, j int) bool {
		if queued[i].at != queued[j].at {
			return queued[i].at < queued[j].at
		}
		return queued[i].id < queued[j].id
	})
	ids = []string{}
	for _, q := range queued {
		ids = append(ids, q.id)
	}
	return ids, truncated
}

// queuePosition is the 1-based place of id among the Lab's queued Molecules and
// how many are queued, or 0 when id is not among those read.
func queuePosition(lab, id string) (position, total int) {
	ids, _ := QueuedMolecules(lab)
	for i, queued := range ids {
		if queued == id {
			return i + 1, len(ids)
		}
	}
	return 0, len(ids)
}

// recordSlotEvent appends a verification slot event and saves the ledger.
func recordSlotEvent(r *Record, kind string) error {
	state, err := r.Readiness.RecordVerificationSlot(kind, stamp())
	if err != nil {
		return err
	}
	r.Readiness = &state
	return save(*r)
}

// Phases timed between coding capture and the first check, beside the views' own
// (verification.PhaseWorkspace and the rest).
const (
	PhaseTreeCapture  = "tree capture"
	PhaseSlotWait     = "waiting for a slot"
	PhaseResourceWait = "waiting for capacity"
	PhaseCheckTempDir = "check TMPDIR prepare"
	maxRecordedPhases = 32
)

// PhaseTiming is one timed step between coding capture and the first check. Its
// phase and view are fixed names, never paths or output.
type PhaseTiming struct {
	Phase      string `json:"phase"`
	View       string `json:"view,omitempty"`
	DurationMS int64  `json:"durationMs"`
}

// phaseClock reads the time phases are measured with; tests replace it.
var phaseClock = time.Now

// phaseLog collects PhaseTiming entries, at most maxRecordedPhases, from any number
// of goroutines. It implements verification.PhaseObserver.
type phaseLog struct {
	mu      sync.Mutex
	entries []PhaseTiming
	cut     bool
}

// Begin starts timing phase in view and returns the function that ends it; only
// the first call of that function records the phase.
func (l *phaseLog) Begin(view, phase string) func() {
	start := phaseClock()
	var once sync.Once
	return func() { once.Do(func() { l.add(view, phase, phaseClock().Sub(start)) }) }
}

// add records one phase of duration d, within maxRecordedPhases.
func (l *phaseLog) add(view, phase string, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) >= maxRecordedPhases {
		l.cut = true
		return
	}
	l.entries = append(l.entries, PhaseTiming{Phase: phase, View: view, DurationMS: max(d, 0).Milliseconds()})
}

// timings returns a copy of the recorded phases.
func (l *phaseLog) timings() ([]PhaseTiming, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]PhaseTiming(nil), l.entries...), l.cut
}

// phaseNote renders the timings as one ledger note, naming the longest phase.
func phaseNote(timings []PhaseTiming, cut bool) string {
	if len(timings) == 0 {
		return ""
	}
	longest := 0
	text := ""
	for i, t := range timings {
		if t.DurationMS > timings[longest].DurationMS {
			longest = i
		}
		name := t.Phase
		if t.View != "" {
			name = t.View + " " + t.Phase
		}
		if text != "" {
			text += "; "
		}
		text += fmt.Sprintf("%s %s", name, (time.Duration(t.DurationMS) * time.Millisecond).String())
	}
	longestName := timings[longest].Phase
	if timings[longest].View != "" {
		longestName = timings[longest].View + " " + longestName
	}
	note := "Verification preparation phases: " + text + ". Longest: " + longestName + "."
	if cut {
		note += fmt.Sprintf(" Only the first %d phases are recorded.", maxRecordedPhases)
	}
	return note
}

// queueNote is the ledger note of a Molecule that queued for checks.
func queueNote(position, total, slots int, reason string) string {
	note := fmt.Sprintf("Queued for checks: every verification slot of this Lab is in use (%d slot(s)).", slots)
	if position > 0 {
		note = fmt.Sprintf("Queued for checks at position %d of %d: every verification slot of this Lab is in use (%d slot(s)).", position, total, slots)
	}
	if reason != "" {
		note += " " + reason + "."
	}
	return note
}
