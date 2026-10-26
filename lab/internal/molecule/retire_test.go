package molecule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// labFiles maps every regular file under lab to its contents, skipping nothing,
// so a test can prove that retiring deleted or rewrote no record.
func labFiles(t *testing.T, lab string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(lab, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[path] = raw
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func retireLedgerBytes(t *testing.T, lab, id string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(lab, "ledgers", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Retiring a review-waiting Molecule appends one retired event, sets status
// retired in the JSON and Markdown ledgers and keeps every other file unchanged.
func TestRetireReviewWaitingMoleculeKeepsEveryRecord(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	item := loopItem(t, loopOnce(t, o, loopFixedList(loopOpen(12, 0))).Passes[0], 12)
	if item.Phase != "review-waiting" {
		t.Fatalf("fixture did not reach review: %+v", item)
	}
	before := labFiles(t, o.LabDir)
	r, err := Retire(o.LabDir, item.MoleculeID, "  superseded\tby a later \x1b attempt  ")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusRetired || r.Retired == nil || r.Retired.Reason != "supersededby a later  attempt" || r.Retired.PriorStatus != "waiting" {
		t.Fatalf("retired %q %+v", r.Status, r.Retired)
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Retired.At); err != nil {
		t.Fatalf("retired at %q: %v", r.Retired.At, err)
	}
	read, err := ReadRecord(o.LabDir, item.MoleculeID)
	if err != nil || read.Status != StatusRetired || read.Retired == nil || *read.Retired != *r.Retired || statePhase(read) != "review-waiting" {
		t.Fatalf("read back %v %q %+v", err, read.Status, read.Retired)
	}
	md, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", item.MoleculeID+".md"))
	if err != nil || !strings.Contains(string(md), "- Status: retired\n") || strings.Count(string(md), "## Retired") != 1 || !strings.Contains(string(md), "- Reason: "+oneLine(r.Retired.Reason)+"\n") {
		t.Fatalf("markdown ledger %v:\n%s", err, md)
	}
	after := labFiles(t, o.LabDir)
	base := filepath.Join(o.LabDir, "ledgers", item.MoleculeID)
	for path, raw := range before {
		now, ok := after[path]
		switch {
		case !ok:
			t.Errorf("retire deleted %s", path)
		case path != base+".json" && path != base+".md" && !bytes.Equal(raw, now):
			t.Errorf("retire rewrote %s", path)
		}
	}
	// The only change to the JSON ledger is the status and the retired event.
	var was, now map[string]json.RawMessage
	if json.Unmarshal(before[base+".json"], &was) != nil || json.Unmarshal(after[base+".json"], &now) != nil {
		t.Fatal("ledger is not JSON")
	}
	for key, value := range was {
		if key != "status" && !bytes.Equal(value, now[key]) {
			t.Errorf("retire changed ledger field %s", key)
		}
	}
	if len(now) != len(was)+1 || now["retired"] == nil {
		t.Errorf("retire added fields beyond the retired event: %d -> %d", len(was), len(now))
	}
}

// Each refusal leaves the ledger byte for byte unchanged and names no local path.
func TestRetireRefusals(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	lab := o.LabDir
	r := loopMolecule(t, o, 12)
	id := r.ID
	ledgers := filepath.Join(lab, "ledgers")
	set := func(edit func(*Record)) func(t *testing.T) func() {
		return func(t *testing.T) func() {
			original := retireLedgerBytes(t, lab, id)
			changed := r
			state := *r.Readiness
			state.Events = append([]readiness.Event(nil), r.Readiness.Events...)
			changed.Readiness = &state
			edit(&changed)
			if err := save(changed); err != nil {
				t.Fatal(err)
			}
			return func() {
				if err := os.WriteFile(filepath.Join(ledgers, id+".json"), original, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	file := func(path string, raw string) func(t *testing.T) func() {
		return func(t *testing.T) func() {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			return func() { os.Remove(path) }
		}
	}
	receipt := func(state string) func(t *testing.T) func() {
		return file(filepath.Join(lab, "deliveries", id+".json"), `{"state":"`+state+`","preview":{"moleculeId":"`+id+`"}}`)
	}
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T) func()
		reason  string
		want    error
	}{
		{"running", set(func(r *Record) { r.Status = "running" }), "stale", ErrRetireActive},
		{"allocating", set(func(r *Record) { r.Status = "allocating" }), "stale", ErrRetireActive},
		{"queued for checks", set(func(r *Record) {
			r.Status, r.Readiness.Phase = "running", "check-pending"
			r.Readiness.Events = append(r.Readiness.Events, readiness.Event{Kind: readiness.VerificationQueued, At: stamp()})
		}), "stale", ErrRetireActive},
		{"locked by another command", func(t *testing.T) func() {
			unlock, err := lockMolecule(lab, id)
			if err != nil {
				t.Fatal(err)
			}
			return unlock
		}, "stale", ErrRetireLocked},
		{"approval lock directory", func(t *testing.T) func() {
			lock := filepath.Join(ledgers, id+".lock")
			if err := os.Mkdir(lock, 0700); err != nil {
				t.Fatal(err)
			}
			return func() { os.Remove(lock) }
		}, "stale", ErrRetireLocked},
		{"already retired", set(func(r *Record) {
			r.Status, r.Retired = StatusRetired, &Retirement{At: "2026-10-26T10:00:00Z", Reason: "earlier", PriorStatus: "waiting"}
		}), "stale", ErrRetireRetired},
		{"prepared receipt", receipt("prepared"), "stale", ErrRetireDelivery},
		{"committed receipt", receipt("committed"), "stale", ErrRetireDelivery},
		{"malformed receipt", file(filepath.Join(lab, "deliveries", id+".json"), `{"state":`), "stale", ErrRetireDelivery},
		{"unknown receipt state", receipt("future"), "stale", ErrRetireDelivery},
		{"delivery lock", func(t *testing.T) func() {
			lock := filepath.Join(lab, ".delivery-lock")
			if err := os.Mkdir(lock, 0700); err != nil {
				t.Fatal(err)
			}
			return func() { os.Remove(lock) }
		}, "stale", ErrRetireDelivery},
		{"empty reason", nil, "", ErrRetireInvalid},
		{"blank reason", nil, "   ", ErrRetireInvalid},
		{"control characters only", nil, "\x07\x1b\u200e", ErrRetireInvalid},
		{"multi-line reason", nil, "one\ntwo", ErrRetireInvalid},
		{"carriage return", nil, "one\rtwo", ErrRetireInvalid},
		{"oversized reason", nil, strings.Repeat("é", MaxRetireReason+1), ErrRetireInvalid},
		{"huge reason", nil, strings.Repeat("a", 1<<20), ErrRetireInvalid},
		{"invalid UTF-8", nil, "stale \xff", ErrRetireInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.prepare != nil {
				defer tc.prepare(t)()
			}
			before := retireLedgerBytes(t, lab, id)
			_, err := Retire(lab, id, tc.reason)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), lab) || strings.Contains(err.Error(), os.TempDir()) {
				t.Errorf("refusal names a local path: %v", err)
			}
			if after := retireLedgerBytes(t, lab, id); !bytes.Equal(before, after) {
				t.Error("a refused retire changed the ledger")
			}
		})
	}
	for name, bad := range map[string]string{"invalid id": "../x", "missing": "mol-404-none"} {
		if _, err := Retire(lab, bad, "stale"); err == nil || strings.Contains(err.Error(), lab) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Retire(lab, "mol-404-none", "stale"); !errors.Is(err, ErrRetireMissing) {
		t.Errorf("missing: %v", err)
	}
	// A reason of exactly the bound is accepted, and a published receipt does not block.
	defer receipt("published")(t)()
	if _, err := Retire(lab, id, strings.Repeat("é", MaxRetireReason)); err != nil {
		t.Fatalf("a bounded reason with a published receipt was refused: %v", err)
	}
}

// The loop treats a retired Molecule as finished history: it is never resumed or
// changed, and a fresh Molecule starts for the same open Issue.
func TestLoopStartsAFreshMoleculeForAnIssueWhoseMoleculesAreRetired(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	old := loopMolecule(t, o, 12)
	if _, err := Retire(o.LabDir, old.ID, "stale attempt"); err != nil {
		t.Fatal(err)
	}
	retired := retireLedgerBytes(t, o.LabDir, old.ID)
	s := ReadQueueInventory(o.LabDir).Summarize("acme/widgets", 12)
	if !s.Startable() || s.State != StateRetired || s.Retired != 1 || s.Latest != nil || len(s.Attempts) != 1 {
		t.Fatalf("summary %+v", s)
	}
	analyses := countAnalysis(&o)
	item := loopItem(t, loopOnce(t, o, loopFixedList(loopOpen(12, 0))).Passes[0], 12)
	if !item.Started || item.MoleculeID == old.ID || item.Failed || item.State != LoopWaiting {
		t.Fatalf("%+v", item)
	}
	if len(analyses()) != 1 || len(loopRecords(t, o.LabDir)[12]) != 2 {
		t.Fatal("no fresh Molecule was started")
	}
	if !bytes.Equal(retired, retireLedgerBytes(t, o.LabDir, old.ID)) {
		t.Fatal("the loop changed a retired ledger")
	}
	// The fresh Molecule is now the Issue's latest; the retired one never is.
	s = ReadQueueInventory(o.LabDir).Summarize("acme/widgets", 12)
	if s.Startable() || s.Latest == nil || s.Latest.ID != item.MoleculeID || s.Retired != 1 || len(s.Attempts) != 2 {
		t.Fatalf("summary %+v", s)
	}
	second := loopItem(t, loopOnce(t, o, loopFixedList(loopOpen(12, 0))).Passes[0], 12)
	if second.Started || second.MoleculeID != item.MoleculeID {
		t.Fatalf("%+v", second)
	}
}

// A retired Molecule is never resurrected: approving its old digest, amending,
// rebasing or recovering it is refused with one message and leaves every
// ledger byte unchanged.
func TestRetiredMoleculeIsNeverResurrected(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, what string, before []byte, lab, id string, err error) {
		t.Helper()
		if !errors.Is(err, ErrMoleculeRetired) || err.Error() != "Molecule is retired; start a new Molecule for the Issue" {
			t.Errorf("%s: got %v, want the retired refusal", what, err)
		}
		if !bytes.Equal(before, retireLedgerBytes(t, lab, id)) {
			t.Errorf("%s changed a retired ledger", what)
		}
		if _, err := os.Lstat(filepath.Join(lab, "ledgers", id+".lock")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left the approval lock behind: %v", what, err)
		}
	}
	t.Run("scope-waiting", func(t *testing.T) {
		t.Parallel()
		o, _ := fixture(t)
		r, err := Run(context.Background(), o)
		if err != nil || statePhase(r) != "scope-waiting" {
			t.Fatalf("%v %+v", err, r.Readiness)
		}
		digest := r.Readiness.Plans[0].Digest
		if _, err = Retire(o.LabDir, r.ID, "superseded"); err != nil {
			t.Fatal(err)
		}
		before := retireLedgerBytes(t, o.LabDir, r.ID)
		o.Approve = digest
		_, err = Run(context.Background(), o)
		refused(t, "approving the old digest", before, o.LabDir, r.ID, err)
		_, err = Recover(context.Background(), o.LabDir, r.ID, digest, true)
		refused(t, "recover", before, o.LabDir, r.ID, err)
		_, err = RebaseFormula(context.Background(), o.LabDir, r.ID, digest)
		refused(t, "rebase", before, o.LabDir, r.ID, err)
		_, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Note: "revive"})
		refused(t, "amend", before, o.LabDir, r.ID, err)
		data, _ := os.ReadFile(filepath.Join(r.Workspace.Path, "code.txt"))
		if string(data) != "base" {
			t.Fatal("coding ran for a retired Molecule")
		}
	})
	t.Run("review-waiting", func(t *testing.T) {
		t.Parallel()
		o, _ := fixture(t)
		r, err := Run(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		o.Approve = r.Readiness.Plans[0].Digest
		if r, err = Run(context.Background(), o); err != nil || r.Status != "waiting" {
			t.Fatalf("%v %q", err, r.Status)
		}
		if _, err = Retire(o.LabDir, r.ID, "superseded"); err != nil {
			t.Fatal(err)
		}
		before := retireLedgerBytes(t, o.LabDir, r.ID)
		_, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Note: "revive"})
		refused(t, "amend", before, o.LabDir, r.ID, err)
		_, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Continuation: "checks-only"})
		refused(t, "checks-only amend", before, o.LabDir, r.ID, err)
		latest := r.Readiness.Plans[len(r.Readiness.Plans)-1].Digest
		_, err = Recover(context.Background(), o.LabDir, r.ID, latest, true)
		refused(t, "recover", before, o.LabDir, r.ID, err)
		// amendLocked refuses on its own, whichever caller reached it.
		retired, err := ReadRecord(o.LabDir, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = amendLocked(context.Background(), retired, latest, retired.Readiness.Plans[len(retired.Readiness.Plans)-1].Plan, "")
		refused(t, "amendLocked", before, o.LabDir, r.ID, err)
	})
}

// classify never reports a retired ledger as unknown, failed or resumable.
func TestClassifyRetiredIsFinishedHistory(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"scope-waiting", "review-waiting", "check-pending", ""} {
		r := Record{ID: "m", Status: StatusRetired}
		if phase != "" {
			r.Readiness = &readiness.State{Phase: phase}
		}
		state, reason, stopped := classify(r)
		if state != LoopRetired || stopped || !strings.Contains(reason, "retired") || atScopeGate(r) {
			t.Errorf("%s: %s %q %t", phase, state, reason, stopped)
		}
		item := settle(LoopItem{Started: true, Approved: true}, r)
		if item.Failed || item.State != LoopRetired {
			t.Errorf("%s: %+v", phase, item)
		}
		pass := LoopPass{Items: []LoopItem{item}}
		if pass.NonProgress() != 0 {
			t.Errorf("%s: a retired Molecule counted as non-progress", phase)
		}
	}
}

// Retired attempts never count as active, blocked or latest, and never make the
// ordering uncertain; a live attempt still blocks a fresh start.
func TestQueueSummaryRetiredAttempts(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	dir := filepath.Join(lab, "ledgers")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	queueFixture(t, dir, "a", "o/r", StatusRetired, "")
	queueFixture(t, dir, "b", "o/r", StatusRetired, "2026-01-03T00:00:00Z")
	s := ReadQueueInventory(lab).Summarize("o/r", 1)
	if s.State != StateRetired || !s.Startable() || s.Retired != 2 || s.Active != 0 || s.OrderingUncertain || s.Latest != nil || len(s.Attempts) != 2 {
		t.Fatalf("%+v", s)
	}
	queueFixture(t, dir, "c", "o/r", "blocked", "2026-01-01T00:00:00Z")
	s = ReadQueueInventory(lab).Summarize("o/r", 1)
	if s.State != "recorded" || s.Startable() || s.Latest == nil || s.Latest.ID != "c" || s.Blocked != 1 || s.Retired != 2 || len(s.Attempts) != 3 {
		t.Fatalf("%+v", s)
	}
	// A partial inventory never reads as startable, retired attempts or not.
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "c.json"))
	if s = ReadQueueInventory(lab).Summarize("o/r", 1); s.Startable() {
		t.Fatalf("%+v", s)
	}
}

// A reader that does not know retired still decodes a retired ledger: the
// shared decoder and header reader accept it, a Record without the retired
// field unmarshals it, and the inventory stays complete.
func TestRetiredLedgerDecodesForOlderReaders(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	r := loopMolecule(t, o, 12)
	if _, err := Retire(o.LabDir, r.ID, "stale"); err != nil {
		t.Fatal(err)
	}
	raw := retireLedgerBytes(t, o.LabDir, r.ID)
	l, err := ledger.Decode(raw)
	if err != nil || l.Status != StatusRetired || l.ID != r.ID {
		t.Fatalf("decode %v %+v", err, l)
	}
	if _, err := ledger.ReadHeader(bytes.NewReader(raw)); err != nil {
		t.Fatalf("header %v", err)
	}
	type olderRecord struct {
		ID        string           `json:"id"`
		Status    string           `json:"status"`
		Readiness *readiness.State `json:"readiness"`
		Notes     []string         `json:"notes"`
	}
	var older olderRecord
	if err := json.Unmarshal(raw, &older); err != nil || older.Status != StatusRetired || older.Readiness == nil {
		t.Fatalf("older reader %v %+v", err, older)
	}
	if _, err := ReadRecord(o.LabDir, r.ID); err != nil {
		t.Fatal(err)
	}
	if inv := ReadQueueInventory(o.LabDir); !inv.Complete {
		t.Fatalf("inventory %+v", inv)
	}
	// A ledger written before Retired existed has no retired field at all.
	var fields map[string]json.RawMessage
	before, _ := json.Marshal(Record{ID: "x", Status: "waiting"})
	if json.Unmarshal(before, &fields) != nil || fields["retired"] != nil {
		t.Fatal("an unretired ledger carries a retired field")
	}
}
