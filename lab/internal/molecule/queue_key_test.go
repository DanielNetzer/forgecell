package molecule

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func keyedQueueFixture(t *testing.T, dir, id, repo, key, status, at, extra string) {
	t.Helper()
	raw := fmt.Sprintf(`{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":%q,"status":%q,"issue":{"repo":%q,"number":0,"key":%q%s},"startedAt":%q,"readiness":{"phase":"scope-pending"}}`, id, status, repo, key, extra, at)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestQueueInventoryIncludesKeyedLedgers(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	dir := filepath.Join(lab, "ledgers")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if s := ReadQueueInventory(lab).SummarizeKey("o/r", "ENG-1"); s.State != StateUntouched || s.Partial {
		t.Fatalf("%+v", s)
	}
	queueFixture(t, dir, "n1", "o/r", "blocked", "2026-01-01T00:00:00Z")
	keyedQueueFixture(t, dir, "k1", "o/r", "ENG-1", "running", "2026-01-02T00:00:00Z", `,"url":"https://tracker.example.test/ENG-1"`)
	keyedQueueFixture(t, dir, "k2", "O/R", "ENG-1", "blocked", "2026-01-03T00:00:00Z", "")
	keyedQueueFixture(t, dir, "k3", "o/r", "ENG-2", "waiting", "2026-01-04T00:00:00Z", "")
	q := ReadQueueInventory(lab)
	if !q.Complete || len(q.Diagnostics) != 0 || len(q.Attempts) != 4 {
		t.Fatalf("inventory = %+v", q)
	}
	s := q.SummarizeKey("o/r", "ENG-1")
	if s.State != "recorded" || len(s.Attempts) != 2 || s.Active != 1 || s.Blocked != 1 || s.Latest == nil || s.Latest.ID != "k2" || s.Latest.Key != "ENG-1" || s.Latest.Number != 0 || s.Latest.Repository != "O/R" {
		t.Fatalf("keyed summary = %+v", s)
	}
	// Keys match exactly: the tracker defines key identity.
	if s := q.SummarizeKey("o/r", "eng-1"); s.State != StateUntouched {
		t.Fatalf("case-folded key matched: %+v", s)
	}
	if s := q.SummarizeKey("o/r", ""); s.State != StateUntouched {
		t.Fatalf("empty key matched: %+v", s)
	}
	// A numeric summary never includes a keyed attempt, and is unchanged.
	n := q.Summarize("o/r", 1)
	if n.State != "recorded" || len(n.Attempts) != 1 || n.Attempts[0].ID != "n1" || n.Attempts[0].Key != "" {
		t.Fatalf("numeric summary = %+v", n)
	}
	if s := q.Summarize("o/r", 0); s.State != StateUntouched {
		t.Fatalf("number 0 matched keyed attempts: %+v", s)
	}
}

func TestQueueInventoryRefusesMalformedKeyedLedgers(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"non-zero number": `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"o/r","number":3,"key":"ENG-1"}}`,
		"unusable key":    `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"o/r","number":0,"key":"###"}}`,
		"control key":     `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"o/r","number":0,"key":"ENG\u0007-1"}}`,
		"overlong key":    `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"o/r","number":0,"key":"` + strings.Repeat("A", 200) + `"}}`,
		"bad repository":  `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"../r","number":0,"key":"ENG-1"}}`,
		"http url":        `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"o/r","number":0,"key":"ENG-1","url":"http://tracker.example.test/ENG-1"}}`,
		"workspace repo":  `{"schemaVersion":"v0","kind":"molecule","formulaId":"f","id":"bad","status":"waiting","issue":{"repo":"o/r","number":0,"key":"ENG-1"},"workspace":{"repo":"x/y"}}`,
	}
	for name, raw := range cases {
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
			// An incomplete inventory never reports a keyed Issue as free to start.
			if s := q.SummarizeKey("o/r", "ENG-1"); s.Startable() || !s.Partial {
				t.Fatalf("summary = %+v", s)
			}
		})
	}
}

func TestIssueKeyLockNames(t *testing.T) {
	numeric, err := issueLockName("Acme/Widgets", 12)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^issue-[0-9a-f]{32}-12\.loop-lock$`).MatchString(numeric) {
		t.Fatalf("numeric lock = %q", numeric)
	}
	keyed, err := issueKeyLockName("Acme/Widgets", "ENG-1234")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^issue-[0-9a-f]{32}-eng-1234\.loop-lock$`).MatchString(keyed) || !isLoopLock(keyed) {
		t.Fatalf("keyed lock = %q", keyed)
	}
	// Repository case folds as it does for numeric locks.
	if again, _ := issueKeyLockName("acme/widgets", "ENG-1234"); again != keyed {
		t.Fatalf("keyed lock is case-sensitive in the repository: %q, %q", again, keyed)
	}
	names := map[string]string{numeric: "#12"}
	add := func(label, name string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if other, ok := names[name]; ok {
			t.Fatalf("%s and %s share lock %q", label, other, name)
		}
		names[name] = label
	}
	for _, key := range []string{"12", "ENG-1", "eng_1", "eng-1", "ENG 1", "ENG-1234"} {
		name, err := issueKeyLockName("acme/widgets", key)
		add("key "+key, name, err)
	}
	for _, n := range []int64{1, 1234} {
		name, err := issueLockName("acme/widgets", n)
		add(fmt.Sprint("#", n), name, err)
	}
	other, err := issueKeyLockName("acme/other", "ENG-1")
	add("other repo ENG-1", other, err)
	for _, c := range []struct{ repo, key string }{{"acme/widgets", "###"}, {"acme/widgets", ""}, {"acme", "ENG-1"}, {"../x/y", "ENG-1"}} {
		if name, err := issueKeyLockName(c.repo, c.key); err == nil {
			t.Fatalf("issueKeyLockName(%q, %q) = %q", c.repo, c.key, name)
		}
	}
}
