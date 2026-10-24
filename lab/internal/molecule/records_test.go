package molecule

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func recordFixture(t *testing.T, lab, id, status string) Record {
	t.Helper()
	r := Record{SchemaVersion: "v0", Kind: "molecule", ID: id, Status: status, FormulaID: "f", Mode: "ticket", StartedAt: "2026-01-01T00:00:00Z", LabDir: lab, Issue: Issue{Repo: "o/r", Number: 7, Title: "t", URL: "https://github.com/o/r/issues/7"}, Workspace: Workspace{Repo: "o/r", Branch: "b"}}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "ledgers", id+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return r
}

// snapshotDir records every name, size and modification time under dir, so a
// test can assert that a read changed nothing.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out[path] = fmt.Sprintf("%s/%s/%d", info.ModTime().Format(time.RFC3339Nano), info.Mode(), info.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReadRecordIdentityAndBounds(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	want := recordFixture(t, lab, "m1", "waiting")
	got, err := ReadRecord(lab, "m1")
	if err != nil || got.ID != want.ID || got.Issue.Title != "t" || got.Workspace.Branch != "b" {
		t.Fatalf("ReadRecord = %+v, %v", got, err)
	}
	dir := filepath.Join(lab, "ledgers")
	for name, setup := range map[string]func() string{
		"invalid id": func() string { return "../m1" },
		"missing":    func() string { return "absent" },
		"symlink": func() string {
			if err := os.Symlink(filepath.Join(dir, "m1.json"), filepath.Join(dir, "link.json")); err != nil {
				t.Fatal(err)
			}
			return "link"
		},
		"directory": func() string {
			if err := os.Mkdir(filepath.Join(dir, "dir.json"), 0700); err != nil {
				t.Fatal(err)
			}
			return "dir"
		},
		"oversized": func() string {
			if err := os.WriteFile(filepath.Join(dir, "big.json"), bytes.Repeat([]byte(" "), 5_000_001), 0600); err != nil {
				t.Fatal(err)
			}
			return "big"
		},
		"identity": func() string {
			raw, err := os.ReadFile(filepath.Join(dir, "m1.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "other.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			return "other"
		},
		"foreign lab": func() string {
			r := recordFixture(t, lab, "foreign", "waiting")
			r.LabDir = "/elsewhere"
			raw, _ := json.Marshal(r)
			if err := os.WriteFile(filepath.Join(dir, "foreign.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			return "foreign"
		},
		"schema": func() string {
			if err := os.WriteFile(filepath.Join(dir, "schema.json"), []byte(`{"schemaVersion":"v1","kind":"molecule","id":"schema","formulaId":"f","status":"s"}`), 0600); err != nil {
				t.Fatal(err)
			}
			return "schema"
		},
	} {
		id := setup()
		if _, err := ReadRecord(lab, id); err == nil {
			t.Errorf("%s: ReadRecord(%q) was accepted", name, id)
		}
	}
}

func TestReadRecordRefusesSymlinkedLedgerDirectory(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	recordFixture(t, source, "m1", "waiting")
	lab := t.TempDir()
	if err := os.Symlink(filepath.Join(source, "ledgers"), filepath.Join(lab, "ledgers")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecord(lab, "m1"); err == nil {
		t.Fatal("a symlinked ledger directory was followed")
	}
	if inv := ListRecords(lab, func(Record) { t.Fatal("visited a record through a symlinked directory") }); inv.Complete {
		t.Fatalf("inventory through a symlinked directory reported complete: %+v", inv)
	}
}

func TestListRecordsVisitsValidLedgersAndNamesTheRest(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	if inv := ListRecords(lab, func(Record) { t.Fatal("visited a record in an empty Lab") }); !inv.Missing || !inv.Complete {
		t.Fatalf("empty Lab: %+v", inv)
	}
	recordFixture(t, lab, "a", "running")
	recordFixture(t, lab, "b", "blocked")
	dir := filepath.Join(lab, "ledgers")
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.json"), bytes.Repeat([]byte(" "), 5_000_001), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("report"), 0600); err != nil {
		t.Fatal(err)
	}
	before := snapshotDir(t, lab)
	var ids []string
	inv := ListRecords(lab, func(r Record) { ids = append(ids, r.ID) })
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("visited %v", ids)
	}
	if inv.Complete || !inv.Truncated || len(inv.Diagnostics) != 2 {
		t.Fatalf("inventory %+v", inv)
	}
	after := snapshotDir(t, lab)
	if len(before) != len(after) {
		t.Fatalf("ListRecords changed the Lab: %v -> %v", before, after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("ListRecords changed %s", k)
		}
	}
}

// ListLedgers hands over ids only, so a caller that skips the read reads
// nothing, and a visit error is named under the ledger's file.
func TestListRecordsLedgerIDsLeaveTheReadToTheCaller(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	recordFixture(t, lab, "a", "running")
	recordFixture(t, lab, "b", "blocked")
	var ids []string
	inv := ListLedgers(lab, func(id string) error {
		ids = append(ids, id)
		if id == "b" {
			return fmt.Errorf("refused")
		}
		return nil
	})
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("visited %v", ids)
	}
	if inv.Complete || len(inv.Diagnostics) != 1 || inv.Diagnostics[0] != "b.json: refused" {
		t.Fatalf("inventory %+v", inv)
	}
}
