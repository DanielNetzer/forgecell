package molecule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

// holdKeyLock takes the start lock of key in acme/widgets as owner would, and
// returns its release.
func holdKeyLock(t *testing.T, lab, key, owner string) func() {
	t.Helper()
	name, err := issueKeyLockName("acme/widgets", key)
	if err != nil {
		t.Fatal(err)
	}
	file, release, err := flockLedgerHandle(lab, name)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeMarkerNote(file, owner); err != nil {
		release()
		t.Fatal(err)
	}
	return release
}

func keyedLedgerCount(t *testing.T, lab string) int {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(lab, "ledgers"))
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "mol-") && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

func TestRunIntakeKeyedRefusesAHeldStartLock(t *testing.T) {
	t.Parallel()
	o, calls := trackerPluginFixture(t, keyedIssueFixture())
	for owner, holder := range map[string]string{issueOwnerRun: "a forgecell run (from a terminal or a Lab UI)", issueOwnerLoop: "a running loop (forgecell loop or a Lab UI)"} {
		release := holdKeyLock(t, o.LabDir, "ENG-1234", owner)
		_, err := RunIntakeKeyed(context.Background(), o, "ENG-1234")
		release()
		want := ErrIssueBusy.Error() + ": " + holder + " holds the start lock for Issue ENG-1234; wait for it to finish, then run this again. Nothing was started"
		if !errors.Is(err, ErrIssueBusy) || err.Error() != want || strings.Contains(err.Error(), o.LabDir) {
			t.Fatalf("held by %s: %v", owner, err)
		}
	}
	if len(*calls) != 0 || keyedLedgerCount(t, o.LabDir) != 0 {
		t.Fatalf("a refused intake started: reads %q", *calls)
	}

	// A different key is not blocked, and its lock is released afterwards.
	release := holdKeyLock(t, o.LabDir, "ENG-1234", issueOwnerRun)
	defer release()
	o.ReadIssueKey = func(_ context.Context, repo, key, _ string) (Issue, error) {
		i := keyedIssueFixture()
		i.Key, i.URL = key, "https://tracker.example.test/browse/"+key
		return i, nil
	}
	r, err := RunIntakeKeyed(context.Background(), o, "ENG-9999")
	if err != nil || r.Issue.Key != "ENG-9999" || r.Readiness == nil || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("other key: %v (%+v)", err, r.Issue)
	}
	again := holdKeyLock(t, o.LabDir, "ENG-9999", issueOwnerRun)
	again()

	// An approval takes the Molecule lock, not the start lock, so a held start
	// lock does not refuse it.
	other := holdKeyLock(t, o.LabDir, "ENG-9999", issueOwnerLoop)
	defer other()
	o.Approve, o.AllowKeyedApproval = r.Readiness.Plans[0].Digest, true
	done, err := RunIntakeKeyed(context.Background(), o, "ENG-9999")
	if err != nil || done.ID != r.ID || len(done.HarnessAttempts) != 1 {
		t.Fatalf("approval under a held start lock: %v", err)
	}
}

func TestRunIntakeKeyedRefusesAnInvalidKeyBeforeAnyLock(t *testing.T) {
	t.Parallel()
	o, calls := trackerPluginFixture(t, keyedIssueFixture())
	for _, key := range []string{"", "ENG-1‮", "---", strings.Repeat("A", 129)} {
		if _, err := RunIntakeKeyed(context.Background(), o, key); err == nil {
			t.Errorf("key %q was accepted", key)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(o.LabDir, "ledgers"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "issue-") {
			t.Errorf("an invalid key named a lock: %s", e.Name())
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("an invalid key read an Issue: %q", *calls)
	}
}

// TestNumericApprovalNeverResumesAKeyedRecord: a keyed ledger edited to carry
// a number beside its Key is not resumed by a numeric approval of that number.
func TestNumericApprovalNeverResumesAKeyedRecord(t *testing.T) {
	t.Parallel()
	o, _ := trackerPluginFixture(t, keyedIssueFixture())
	r, err := RunKeyed(context.Background(), o, "ENG-1234")
	if err != nil || r.Readiness == nil {
		t.Fatalf("RunKeyed: %v", err)
	}
	r.Issue.Number = 12
	if err = save(r); err != nil {
		t.Fatal(err)
	}
	loaded, err := formula.Load(o.LabDir, "")
	if err != nil {
		t.Fatal(err)
	}
	numeric := 0
	o.ReadIssue = func(context.Context, string, int64, string) (Issue, error) {
		numeric++
		return Issue{}, nil
	}
	o.Approve = r.Readiness.Plans[0].Digest
	if _, err = resume(context.Background(), o, loaded, 12, "acme/widgets"); err == nil || err.Error() != "issue or Formula changed; refresh intake" {
		t.Fatalf("numeric resume of a keyed record: %v", err)
	}
	if saved := readLedgerRecord(t, o.LabDir, r.ID); len(saved.HarnessAttempts) != 0 || saved.Readiness.Phase != "scope-waiting" || numeric != 0 {
		t.Fatalf("refusal changed the Molecule: attempts %d phase %q reads %d", len(saved.HarnessAttempts), saved.Readiness.Phase, numeric)
	}
}
