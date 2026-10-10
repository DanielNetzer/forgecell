package molecule

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func queueFixture(t *testing.T, dir, id, repo, status, at string) {
	t.Helper()
	raw := fmt.Sprintf(`{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":%q,"status":%q,"issue":{"repo":%q,"number":1},"startedAt":%q,"readiness":{"phase":"scope-pending"}}`, id, status, repo, at)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestIssueQueueInventory(t *testing.T) {
	lab := t.TempDir()
	dir := filepath.Join(lab, "ledgers")
	if s := ReadQueueInventory(lab).Summarize("o/r", 1); s.State != "untouched" || s.Partial {
		t.Fatal(s)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	queueFixture(t, dir, "a", "o/r", "running", "2026-01-01T00:00:00Z")
	queueFixture(t, dir, "b", "O/R", "allocating", "2026-01-02T00:00:00Z")
	queueFixture(t, dir, "c", "o/r", "blocked", "2026-01-03T00:00:00Z")
	queueFixture(t, dir, "d", "x/r", "running", "2026-01-04T00:00:00Z")
	s := ReadQueueInventory(lab).Summarize("o/r", 1)
	if s.Active != 2 || s.Blocked != 1 || !s.DuplicateActive || s.Latest == nil || s.Latest.ID != "c" || s.Latest.Phase != "scope-pending" || s.Partial {
		t.Fatalf("%+v", s)
	}
	queueFixture(t, dir, "e", "o/r", "future", "")
	s = ReadQueueInventory(lab).Summarize("o/r", 1)
	if !s.OrderingUncertain || s.Latest != nil {
		t.Fatal(s)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.json"), []byte(`{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"old","status":"waiting"}`), 0600); err != nil {
		t.Fatal(err)
	}
	q := ReadQueueInventory(lab)
	if q.Complete || len(q.Diagnostics) == 0 || q.Summarize("o/r", 2).State != "unknown" {
		t.Fatal(q)
	}
}
func TestIssueQueueUnsafeAndBounds(t *testing.T) {
	for _, kind := range []string{"symlink", "malformed", "schema", "identity", "large", "files", "entries", "directory", "repo", "url", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			lab := t.TempDir()
			dir := filepath.Join(lab, "ledgers")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "bad.json")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink("/dev/null", path)
			case "malformed":
				err = os.WriteFile(path, []byte("{"), 0600)
			case "schema":
				err = os.WriteFile(path, []byte(`{"schemaVersion":"v1"}`), 0600)
			case "identity":
				queueFixture(t, dir, "bad", "o/r", "waiting", "")
				err = os.Rename(path, filepath.Join(dir, "other.json"))
			case "large":
				err = os.WriteFile(path, bytes.Repeat([]byte(" "), 5_000_001), 0600)
			case "files":
				for i := 0; i < 257; i++ {
					queueFixture(t, dir, fmt.Sprintf("m%03d", i), "o/r", "waiting", "2026-01-01T00:00:00Z")
				}
			case "entries":
				for i := 0; i < 4097; i++ {
					if e := os.WriteFile(filepath.Join(dir, fmt.Sprint(i)), nil, 0600); e != nil {
						t.Fatal(e)
					}
				}
			case "directory":
				err = os.Remove(dir)
				if err == nil {
					err = os.WriteFile(dir, nil, 0600)
				}
			case "fifo":
				err = os.Mkdir(path, 0700)
			case "repo", "url":
				extra := `,"workspace":{"repo":"x/r"}`
				if kind == "url" {
					extra = `,"issue":{"repo":"o/r","number":1,"url":"https://github.com/x/r/issues/1"}`
				}
				err = os.WriteFile(path, []byte(`{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"o/r","number":1}`+extra+`}`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			q := ReadQueueInventory(lab)
			if q.Complete || len(q.Diagnostics) == 0 || q.Summarize("o/r", 2).State == "untouched" {
				t.Fatal(q)
			}
		})
	}
}
func TestIssueQueueTotalBytesAndOrdering(t *testing.T) {
	lab := t.TempDir()
	dir := filepath.Join(lab, "ledgers")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("m%d", i)
		queueFixture(t, dir, id, "o/r", "waiting", "2026-01-01T00:00:00Z")
		p := filepath.Join(dir, id+".json")
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, bytes.Repeat([]byte(" "), 5_000_000-len(raw))...)
		if err = os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	q := ReadQueueInventory(lab)
	s := q.Summarize("o/r", 1)
	if !q.Truncated || !s.Partial || len(s.Attempts) != 4 || s.Latest != nil {
		t.Fatal(q, s)
	}
	if err := os.Remove(filepath.Join(dir, "m4.json")); err != nil {
		t.Fatal(err)
	}
	q = ReadQueueInventory(lab)
	s = q.Summarize("o/r", 1)
	if !q.Complete || s.Latest == nil || s.Latest.ID != "m0" {
		t.Fatal(q, s)
	}
}
