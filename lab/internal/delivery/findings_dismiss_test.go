package delivery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	dismissTestID     = "mol-dismiss"
	dismissTestCommit = "0123456789abcdef0123456789abcdef01234567"
)

func dismissTestNow() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }

// dismissTestFinding is an open finding of reviewer index of plugin critic.
func dismissTestFinding(index int, severity, title string) Finding {
	return Finding{Fingerprint: FindingFingerprint(ReviewerKey(index, "critic"), severity, "src/app.go", title), Severity: severity, Path: "src/app.go", Title: title, PluginID: "F"}
}

// dismissTestLab writes a findings record of one complete round holding the
// findings, each reviewer's list by index, and returns the Lab.
func dismissTestLab(t *testing.T, perReviewer ...[]Finding) string {
	t.Helper()
	lab := t.TempDir()
	if err := os.MkdirAll(filepath.Join(lab, "deliveries"), 0o700); err != nil {
		t.Fatal(err)
	}
	round := FindingsRound{Commit: dismissTestCommit, Ref: "7", CollectedAt: "2026-11-02T12:00:00Z", Complete: true}
	for i, findings := range perReviewer {
		round.Reviewers = append(round.Reviewers, ReviewerRound{Reviewer: ReviewerKey(i, "critic"), Plugin: "critic", Status: FindingsComplete, Findings: findings})
	}
	record := FindingsRecord{SchemaVersion: findingsVersion, MoleculeID: dismissTestID, Rounds: []FindingsRound{round}}
	if err := writeFindings(lab, dismissTestID, record); err != nil {
		t.Fatal(err)
	}
	return lab
}

func dismissTestRaw(t *testing.T, lab string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(lab, "deliveries", dismissTestID+".findings.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestDismissAndUndismissFinding(t *testing.T) {
	high, low := dismissTestFinding(0, "high", "Unchecked error"), dismissTestFinding(1, "low", "Style nit")
	lab := dismissTestLab(t, []Finding{high}, []Finding{low})
	before := dismissTestRaw(t, lab)
	if strings.Contains(before, "dismissed") {
		t.Fatalf("a record without dismissals names them: %s", before)
	}
	if err := DismissFinding(lab, dismissTestID, high.Fingerprint, "  A false positive: the error is checked by the caller.  ", dismissTestNow); err != nil {
		t.Fatal(err)
	}
	record, err := ReadFindings(lab, dismissTestID)
	if err != nil {
		t.Fatal(err)
	}
	want := Dismissal{Fingerprint: high.Fingerprint, Reviewer: "0:critic", Reason: "A false positive: the error is checked by the caller.", At: "2026-11-02T12:00:00Z"}
	if len(record.Dismissed) != 1 || record.Dismissed[0] != want {
		t.Fatalf("dismissed = %+v", record.Dismissed)
	}
	s := Summarize(record, map[string]string{"0:critic": "high"})
	if s.Dismissed != 1 || s.OpenCount() != 1 || s.Open[0].Findings[0].Fingerprint != low.Fingerprint {
		t.Fatalf("summary = %+v", s)
	}
	if _, err := os.Lstat(filepath.Join(lab, ".delivery-lock")); !os.IsNotExist(err) {
		t.Fatalf("the delivery lock was left behind: %v", err)
	}
	if err := UndismissFinding(lab, dismissTestID, high.Fingerprint); err != nil {
		t.Fatal(err)
	}
	// A record whose last dismissal is removed is byte-identical to one that never had any.
	if after := dismissTestRaw(t, lab); after != before {
		t.Fatalf("undismissed record differs:\n%s\nwant\n%s", after, before)
	}
	record, _ = ReadFindings(lab, dismissTestID)
	if s := Summarize(record, nil); s.Dismissed != 0 || s.OpenCount() != 2 {
		t.Fatalf("summary after undismiss = %+v", s)
	}
}

func TestDismissFindingRefusals(t *testing.T) {
	high := dismissTestFinding(0, "high", "Unchecked error")
	other := strings.Repeat("e", 64)
	cases := []struct {
		name, id, fingerprint, reason, want string
		setup                               func(t *testing.T, lab string)
	}{
		{"unknown fingerprint", dismissTestID, other, "Not real", "not in any recorded findings round", nil},
		{"duplicate", dismissTestID, high.Fingerprint, "Again", "already dismissed", func(t *testing.T, lab string) {
			if err := DismissFinding(lab, dismissTestID, high.Fingerprint, "First", dismissTestNow); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty reason", dismissTestID, high.Fingerprint, "", "needs a reason", nil},
		{"blank reason", dismissTestID, high.Fingerprint, " \t ", "needs a reason", nil},
		{"oversized reason", dismissTestID, high.Fingerprint, strings.Repeat("é", MaxDismissalReasonRunes+1), "at most 500", nil},
		{"control character", dismissTestID, high.Fingerprint, "Bell\x07here", "without control or format", nil},
		{"newline", dismissTestID, high.Fingerprint, "Two\nlines", "without control or format", nil},
		{"bidi override", dismissTestID, high.Fingerprint, "Looks ‮ok", "without control or format", nil},
		{"invalid UTF-8", dismissTestID, high.Fingerprint, "bad \xff", "plain text", nil},
		{"invalid fingerprint", dismissTestID, "ABC", "Reason", "64 lower-case hex", nil},
		{"invalid id", "../mol", high.Fingerprint, "Reason", "invalid Molecule id", nil},
		{"lock held", dismissTestID, high.Fingerprint, "Reason", "another delivery operation holds the lock", func(t *testing.T, lab string) {
			if err := os.Mkdir(filepath.Join(lab, ".delivery-lock"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlinked record", dismissTestID, high.Fingerprint, "Reason", "not a bounded regular file", func(t *testing.T, lab string) {
			file := filepath.Join(lab, "deliveries", dismissTestID+".findings.json")
			target := filepath.Join(lab, "elsewhere.json")
			if err := os.Rename(file, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, file); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lab := dismissTestLab(t, []Finding{high})
			if c.setup != nil {
				c.setup(t, lab)
			}
			before, _ := os.ReadFile(filepath.Join(lab, "elsewhere.json"))
			beforeRecord, _ := os.ReadFile(filepath.Join(lab, "deliveries", dismissTestID+".findings.json"))
			err := DismissFinding(lab, c.id, c.fingerprint, c.reason, dismissTestNow)
			if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), lab) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			after, _ := os.ReadFile(filepath.Join(lab, "elsewhere.json"))
			afterRecord, _ := os.ReadFile(filepath.Join(lab, "deliveries", dismissTestID+".findings.json"))
			if string(after) != string(before) || string(afterRecord) != string(beforeRecord) {
				t.Fatal("a refused dismissal wrote the record")
			}
			if info, err := os.Lstat(filepath.Join(lab, "deliveries", dismissTestID+".findings.json")); c.name == "symlinked record" && (err != nil || info.Mode()&os.ModeSymlink == 0) {
				t.Fatalf("the symlink was replaced: %v", err)
			}
		})
	}
	// No record at all: nothing to dismiss, and nothing is written.
	empty := t.TempDir()
	if err := DismissFinding(empty, dismissTestID, high.Fingerprint, "Reason", dismissTestNow); err == nil || !strings.Contains(err.Error(), "not in any recorded findings round") {
		t.Fatalf("no record: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(empty, "deliveries")); !os.IsNotExist(err) {
		t.Fatalf("a refused dismissal created deliveries: %v", err)
	}
}

func TestUndismissFindingRefusals(t *testing.T) {
	high := dismissTestFinding(0, "high", "Unchecked error")
	lab := dismissTestLab(t, []Finding{high})
	before := dismissTestRaw(t, lab)
	for _, c := range []struct{ id, fingerprint, want string }{
		{dismissTestID, high.Fingerprint, "is not dismissed"},
		{dismissTestID, "nope", "64 lower-case hex"},
		{"../mol", high.Fingerprint, "invalid Molecule id"},
	} {
		if err := UndismissFinding(lab, c.id, c.fingerprint); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	if dismissTestRaw(t, lab) != before {
		t.Fatal("a refused undismissal wrote the record")
	}
}

func TestDismissAndUndismissRefuseADemoLab(t *testing.T) {
	high := dismissTestFinding(0, "high", "Unchecked error")
	lab := dismissTestLab(t, []Finding{high})
	if err := DismissFinding(lab, dismissTestID, high.Fingerprint, "Before demo", dismissTestNow); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "lab.json"), []byte(`{"activeFormulaId":"forgecell-demo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := dismissTestRaw(t, lab)
	if err := DismissFinding(lab, dismissTestID, high.Fingerprint, "Again", dismissTestNow); !errors.Is(err, ErrDemoLab) {
		t.Fatalf("DismissFinding on a demo Lab: %v", err)
	}
	if err := UndismissFinding(lab, dismissTestID, high.Fingerprint); !errors.Is(err, ErrDemoLab) {
		t.Fatalf("UndismissFinding on a demo Lab: %v", err)
	}
	// The guard runs first: even an invalid id is refused as a demo Lab, and
	// no lock is taken.
	if err := DismissFinding(lab, "../mol", "x", "", dismissTestNow); !errors.Is(err, ErrDemoLab) {
		t.Fatalf("DismissFinding guard order: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(lab, ".delivery-lock")); !os.IsNotExist(err) {
		t.Fatalf("a demo Lab refusal took the delivery lock: %v", err)
	}
	if dismissTestRaw(t, lab) != before {
		t.Fatal("a demo Lab refusal wrote the record")
	}
}

func TestDismissalsAreBounded(t *testing.T) {
	// Two reviewers, 251 findings each: more findings than dismissals allowed.
	var first, second []Finding
	for i := 0; i < 251; i++ {
		first = append(first, dismissTestFinding(0, "medium", fmt.Sprintf("finding %d", i)))
		second = append(second, dismissTestFinding(1, "medium", fmt.Sprintf("finding %d", i)))
	}
	all := append(append([]Finding{}, first...), second...)
	lab := dismissTestLab(t, first, second)
	record, err := ReadFindings(lab, dismissTestID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range all[:MaxDismissals-1] {
		record.Dismissed = append(record.Dismissed, Dismissal{Fingerprint: f.Fingerprint, Reviewer: "0:critic", Reason: "Bulk", At: "2026-11-02T12:00:00Z"})
	}
	if err := writeFindings(lab, dismissTestID, record); err != nil {
		t.Fatal(err)
	}
	if err := DismissFinding(lab, dismissTestID, all[MaxDismissals-1].Fingerprint, "The last one", dismissTestNow); err != nil {
		t.Fatalf("dismissal %d: %v", MaxDismissals, err)
	}
	before := dismissTestRaw(t, lab)
	if err := DismissFinding(lab, dismissTestID, all[MaxDismissals].Fingerprint, "One too many", dismissTestNow); err == nil || !strings.Contains(err.Error(), "at most 500 dismissals") {
		t.Fatalf("dismissal %d: %v", MaxDismissals+1, err)
	}
	if dismissTestRaw(t, lab) != before {
		t.Fatal("a refused dismissal wrote the record")
	}
	record, err = ReadFindings(lab, dismissTestID)
	if err != nil {
		t.Fatal(err)
	}
	if s := Summarize(record, nil); s.Dismissed != MaxDismissals || s.OpenCount() != len(all)-MaxDismissals {
		t.Fatalf("summary dismissed %d open %d", s.Dismissed, s.OpenCount())
	}
	// A record holding more than the bound is refused when read.
	record.Dismissed = append(record.Dismissed, Dismissal{Fingerprint: all[MaxDismissals].Fingerprint, Reviewer: "1:critic", Reason: "Over", At: "2026-11-02T12:00:00Z"})
	if err := writeFindings(lab, dismissTestID, record); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFindings(lab, dismissTestID); err == nil {
		t.Fatal("a record over the dismissal bound was read")
	}
}

func TestReadFindingsRefusesMalformedDismissals(t *testing.T) {
	high := dismissTestFinding(0, "high", "Unchecked error")
	good := Dismissal{Fingerprint: high.Fingerprint, Reviewer: "0:critic", Reason: "Fine", At: "2026-11-02T12:00:00Z"}
	for name, list := range map[string][]Dismissal{
		"duplicate":        {good, good},
		"bad fingerprint":  {{Fingerprint: "x", Reviewer: good.Reviewer, Reason: good.Reason, At: good.At}},
		"bad reviewer":     {{Fingerprint: good.Fingerprint, Reviewer: "critic", Reason: good.Reason, At: good.At}},
		"empty reason":     {{Fingerprint: good.Fingerprint, Reviewer: good.Reviewer, Reason: "", At: good.At}},
		"untrimmed reason": {{Fingerprint: good.Fingerprint, Reviewer: good.Reviewer, Reason: " Fine", At: good.At}},
		"control reason":   {{Fingerprint: good.Fingerprint, Reviewer: good.Reviewer, Reason: "a\x1bb", At: good.At}},
		"bad time":         {{Fingerprint: good.Fingerprint, Reviewer: good.Reviewer, Reason: good.Reason, At: "yesterday"}},
	} {
		lab := dismissTestLab(t, []Finding{high})
		record, err := ReadFindings(lab, dismissTestID)
		if err != nil {
			t.Fatal(err)
		}
		record.Dismissed = list
		if err := writeFindings(lab, dismissTestID, record); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFindings(lab, dismissTestID); err == nil || !strings.Contains(err.Error(), "exceeds a bound or is malformed") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestDismissalOfADroppedFindingStaysReadable(t *testing.T) {
	// A dismissal whose finding a bound dropped from every round still reads,
	// and simply matches nothing.
	high := dismissTestFinding(0, "high", "Unchecked error")
	lab := dismissTestLab(t, []Finding{high})
	record, err := ReadFindings(lab, dismissTestID)
	if err != nil {
		t.Fatal(err)
	}
	record.Dismissed = []Dismissal{{Fingerprint: strings.Repeat("d", 64), Reviewer: "0:critic", Reason: "Old", At: "2026-11-02T12:00:00Z"}}
	if err := writeFindings(lab, dismissTestID, record); err != nil {
		t.Fatal(err)
	}
	record, err = ReadFindings(lab, dismissTestID)
	if err != nil {
		t.Fatal(err)
	}
	if s := Summarize(record, nil); s.Dismissed != 0 || s.OpenCount() != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if err := UndismissFinding(lab, dismissTestID, strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
}
