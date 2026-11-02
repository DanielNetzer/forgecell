package molecule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQueueInventoryJoinsNestedKeyedLedgers(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	dir := filepath.Join(lab, "ledgers")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keyedQueueFixture(t, dir, "k1", nestedProject, "ENG-1", "running", "2026-01-01T00:00:00Z", `,"url":"https://tracker.example.test/ENG-1"`)
	keyedQueueFixture(t, dir, "k2", "Welltech-Com/Platform/Backend", "ENG-1", "blocked", "2026-01-02T00:00:00Z", "")
	keyedQueueFixture(t, dir, "k3", "welltech-com/platform/frontend", "ENG-1", "waiting", "2026-01-03T00:00:00Z", "")
	keyedQueueFixture(t, dir, "k4", "o/r", "ENG-1", "waiting", "2026-01-04T00:00:00Z", "")
	q := ReadQueueInventory(lab)
	if !q.Complete || len(q.Diagnostics) != 0 || len(q.Attempts) != 4 {
		t.Fatalf("inventory = %+v", q)
	}
	s := q.SummarizeKey(nestedProject, "ENG-1")
	if s.State != "recorded" || len(s.Attempts) != 2 || s.Latest == nil || s.Latest.ID != "k2" || s.Latest.Repository != "Welltech-Com/Platform/Backend" {
		t.Fatalf("nested summary = %+v", s)
	}
	if s := q.SummarizeKey("welltech-com/platform", "ENG-1"); s.State != StateUntouched {
		t.Fatalf("a parent group matched: %+v", s)
	}
	if s := q.SummarizeKey("o/r", "ENG-1"); len(s.Attempts) != 1 || s.Attempts[0].ID != "k4" {
		t.Fatalf("two-segment summary = %+v", s)
	}
}

func TestQueueInventoryRefusesMalformedNestedLedgers(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"traversal":        `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"a/../b","number":0,"key":"ENG-1"}}`,
		"empty segment":    `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"a//b","number":0,"key":"ENG-1"}}`,
		"too many groups":  `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"` + strings.Repeat("g/", 22) + `g","number":0,"key":"ENG-1"}}`,
		"trailing .git":    `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"a/b/c.git","number":0,"key":"ENG-1"}}`,
		"workspace repo":   `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"a/b/c","number":0,"key":"ENG-1"},"workspace":{"repo":"a/b/d"}}`,
		"numeric nested":   `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"a/b/c","number":3}}`,
		"nested with both": `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"a/b/c","number":3,"key":"ENG-1"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lab := t.TempDir()
			dir := filepath.Join(lab, "ledgers")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			q := ReadQueueInventory(lab)
			if q.Complete || len(q.Diagnostics) != 1 || len(q.Attempts) != 0 || !strings.HasPrefix(q.Diagnostics[0], "bad.json: ") {
				t.Fatalf("inventory = %+v", q)
			}
		})
	}
}
