package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const newOrderHeader = `{"schemaVersion":"v0","kind":"molecule","id":"mol-1","status":"waiting","formulaId":"f","mode":"ticket","startedAt":"2026-01-01T00:00:00Z","finishedAt":"","labDir":"/lab","issue":{"number":7,"repo":"o/r","title":"t","body":"b","url":"https://github.com/o/r/issues/7"},"workspace":{"path":"/w","repo":"o/r"},"readiness":{"schemaVersion":"v1","phase":"scope-waiting"`

func TestHeaderNewOrderStopsAfterTheHeader(t *testing.T) {
	body := newOrderHeader + `,"plans":[` + strings.Repeat(`{"x":"y"},`, 200_000) + `{}]},"atoms":[]}`
	h, decoded, err := readHeader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if decoded != int64(len(newOrderHeader)) {
		t.Fatalf("the header decode read %d bytes, want %d", decoded, len(newOrderHeader))
	}
	if h.ID != "mol-1" || h.Status != "waiting" || h.FormulaID != "f" || h.Mode != "ticket" || h.LabDir != "/lab" ||
		h.Issue.Repo != "o/r" || h.Issue.Number != 7 || h.Issue.URL == "" || h.Workspace == nil || h.Workspace.Repo != "o/r" ||
		h.StartedAt != "2026-01-01T00:00:00Z" || h.Readiness == nil || h.Readiness.Phase != "scope-waiting" {
		t.Fatalf("%+v", h)
	}
	// Without readiness, the first non-header key ends the decode.
	short := strings.SplitN(newOrderHeader, `,"readiness"`, 2)[0] + `,"formulaApproved"`
	h, decoded, err = readHeader(strings.NewReader(short + `:true,"atoms":[` + strings.Repeat(`1,`, 100_000) + `1]}`))
	if err != nil || h.Readiness != nil || h.ID != "mol-1" || decoded != int64(len(short)) {
		t.Fatalf("%+v %d %v", h, decoded, err)
	}
}

// After an early stop the rest is still checked, so a damaged ledger is refused
// as Decode refuses it, wherever the header decode stopped.
func TestHeaderRefusesDamageAfterTheHeader(t *testing.T) {
	withReadiness := newOrderHeader + `}`
	withoutReadiness := strings.SplitN(newOrderHeader, `,"readiness"`, 2)[0]
	insideReadiness := newOrderHeader
	for name, raw := range map[string]string{
		"garbage":           withReadiness + `,"atoms":[{"x": garbage`,
		"truncated":         withReadiness + `,"atoms":[`,
		"second object":     withReadiness + `} {"x":1}`,
		"garbage at key":    withoutReadiness + `,"atoms":[{"x": garbage`,
		"truncated at key":  withoutReadiness + `,"atoms":[`,
		"second at key":     withoutReadiness + `,"atoms":[]} {"x":1}`,
		"garbage inside":    insideReadiness + `,"plans":[{"x": garbage`,
		"truncated inside":  insideReadiness + `,"plans":[`,
		"second inside":     insideReadiness + `}} {"x":1}`,
		"unclosed":          withReadiness,
		"bad key separator": withoutReadiness + `,"atoms" []}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(raw)); err == nil {
				t.Fatal("Decode accepted the probe")
			}
			if h, err := ReadHeader(strings.NewReader(raw)); err == nil {
				t.Fatalf("ReadHeader accepted a damaged ledger: %+v", h)
			}
		})
	}
	for name, raw := range map[string]string{
		"after readiness": withReadiness + `,"atoms":[{"x":[1,{"y":null}]}]}  `,
		"at key":          withoutReadiness + `,"atoms":[]}`,
		"inside":          insideReadiness + `,"plans":[]},"notes":["n"]}`,
		"just closed":     withReadiness + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadHeader(strings.NewReader(raw)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHeaderOldOrderSkipsLargeValuesTokenByToken(t *testing.T) {
	big := `"policyDecisions":[` + strings.Repeat(`{"evidence":[{"a":[1,2,{"b":"c"}]}],"s":"`+strings.Repeat("x", 1000)+`"},`, 2000) + `{}],` +
		`"readiness":{"schemaVersion":"v1","phase":"scope-pending","plans":[[[{"deep":true}]]],"events":[]},`
	body := `{` + big + `"schemaVersion":"v0","kind":"molecule","id":"mol-2","mode":"ticket","status":"passed","formulaId":"f","formulaApproved":true,"startedAt":"2026-02-01T00:00:00Z","finishedAt":"x","labDir":"/lab","workspace":{"repo":"o/r"},"issue":{"repo":"o/r","number":3},"atoms":[],"notes":["n"]}`
	h, err := ReadHeader(strings.NewReader(body))
	if err != nil || h.ID != "mol-2" || h.Readiness == nil || h.Readiness.Phase != "scope-pending" || h.Issue.Number != 3 || h.Workspace.Repo != "o/r" {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestHeaderValidationMatchesDecode(t *testing.T) {
	for name, raw := range map[string]string{
		"valid":          `{"schemaVersion":"v0","kind":"molecule","id":"a","formulaId":"f","status":"s"}`,
		"folded keys":    `{"SchemaVersion":"v0","KIND":"molecule","Id":"a","formulaid":"f","status":"s"}`,
		"duplicate last": `{"schemaVersion":"v0","kind":"molecule","id":"a","formulaId":"f","status":"s","status":"t"}`,
		"null":           `null`,
		"array":          `[]`,
		"empty":          ``,
		"truncated":      `{"schemaVersion":"v0","kind":"molecule","id":"a","formulaId":"f","status":"s"`,
		"trailing":       `{"schemaVersion":"v0","kind":"molecule","id":"a","formulaId":"f","status":"s"} {}`,
		"trailing space": `{"schemaVersion":"v0","kind":"molecule","id":"a","formulaId":"f","status":"s"}   `,
		"schema":         `{"schemaVersion":"v1","kind":"molecule","id":"a","formulaId":"f","status":"s"}`,
		"kind":           `{"schemaVersion":"v0","kind":"other","id":"a","formulaId":"f","status":"s"}`,
		"bad id":         `{"schemaVersion":"v0","kind":"molecule","id":"../a","formulaId":"f","status":"s"}`,
		"no formula":     `{"schemaVersion":"v0","kind":"molecule","id":"a","status":"s"}`,
		"no status":      `{"schemaVersion":"v0","kind":"molecule","id":"a","formulaId":"f"}`,
		"typed status":   `{"schemaVersion":"v0","kind":"molecule","id":"a","formulaId":"f","status":5}`,
		"null status":    `{"schemaVersion":"v0","kind":"molecule","id":"a","formulaId":"f","status":null}`,
		"huge number":    `{"x":1e400,"schemaVersion":"v0","kind":"molecule","id":"a","formulaId":"f","status":"s"}`,
		"bad syntax":     `{"schemaVersion":"v0","kind":"molecule","id":"a","formulaId":"f","status":"s",}`,
	} {
		t.Run(name, func(t *testing.T) {
			want, wantErr := Decode([]byte(raw))
			got, err := ReadHeader(strings.NewReader(raw))
			if (wantErr == nil) != (err == nil) {
				t.Fatalf("Decode err %v, ReadHeader err %v", wantErr, err)
			}
			if err == nil && (got.ID != want.ID || got.FormulaID != want.FormulaID || got.Status != want.Status) {
				t.Fatalf("Decode %+v, ReadHeader %+v", want, got)
			}
		})
	}
}

func TestHeaderProjectionMatchesQueueProjection(t *testing.T) {
	const identity = `"schemaVersion":"v0","kind":"molecule","id":"a","formulaId":"f","status":"s"`
	for name, extra := range map[string]string{
		"none":               ``,
		"full":               `,"issue":{"url":"u","repo":"o/r","number":2},"workspace":{"repo":"o/r"},"startedAt":"t","readiness":{"phase":"p"}`,
		"null projections":   `,"workspace":null,"readiness":null`,
		"issue type":         `,"issue":"o/r#1"`,
		"number type":        `,"issue":{"number":"1"}`,
		"workspace type":     `,"workspace":[]`,
		"started type":       `,"startedAt":5`,
		"readiness type":     `,"readiness":[{"phase":"p"}]`,
		"readiness scalar":   `,"readiness":"p"`,
		"phase type":         `,"readiness":{"phase":{}}`,
		"readiness no phase": `,"readiness":{"plans":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := `{` + identity + extra + `}`
			l, err := Decode([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			want, wantErr := l.QueueProjection()
			got, err := ReadHeader(strings.NewReader(raw))
			var projection *ProjectionError
			if (wantErr == nil) != (err == nil) || (err != nil && !errors.As(err, &projection)) {
				t.Fatalf("QueueProjection err %v, ReadHeader err %v", wantErr, err)
			}
			if got.ID != "a" {
				t.Fatalf("a projection error dropped the identity: %+v", got)
			}
			if wantErr != nil {
				return
			}
			if got.Issue != want.Issue || got.StartedAt != want.StartedAt || (got.Workspace == nil) != (want.Workspace == nil) || (got.Readiness == nil) != (want.Readiness == nil) {
				t.Fatalf("QueueProjection %+v, ReadHeader %+v", want, got.QueueProjection)
			}
			if got.Readiness != nil && got.Readiness.Phase != want.Readiness.Phase {
				t.Fatalf("phase %q, want %q", got.Readiness.Phase, want.Readiness.Phase)
			}
		})
	}
}

func TestHeaderScanDirReportsEachFileByName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledgers")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("good.json", `{"schemaVersion":"v0","kind":"molecule","id":"good","formulaId":"f","status":"s","startedAt":"t"}`)
	write("bad.json", `{`)
	write("other.json", `{"schemaVersion":"v0","kind":"molecule","id":"good","formulaId":"f","status":"s"}`)
	write("proj.json", `{"schemaVersion":"v0","kind":"molecule","id":"proj","formulaId":"f","status":"s","issue":[]}`)
	write("big.json", string(bytes.Repeat([]byte(" "), MaxHeaderLedger+1)))
	write("trail.json", `{"schemaVersion":"v0","kind":"molecule","id":"trail","formulaId":"f","status":"s","startedAt":"t","finishedAt":"","labDir":"","mode":"","issue":{},"workspace":{},"readiness":{"phase":"p"},"atoms":[`)
	write("x.lock", ``)
	write("good.md", `report`)
	write("x.loop-lock", ``)
	write("notes.txt", `ignored`)
	if err := os.Symlink("/dev/null", filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0700); err != nil {
		t.Fatal(err)
	}
	scan := ScanDir(dir, ScanOptions{})
	if scan.Problem != "" || scan.Missing {
		t.Fatalf("%+v", scan)
	}
	want := map[string]ProblemKind{"bad.json": ProblemMalformed, "big.json": ProblemOversized, "good.json": ProblemNone, "link.json": ProblemUnsafe, "other.json": ProblemIdentity, "proj.json": ProblemProjection, "sub.json": ProblemUnsafe, "trail.json": ProblemMalformed}
	if len(scan.Files) != len(want) {
		t.Fatalf("%+v", scan.Files)
	}
	for _, f := range scan.Files {
		if f.Kind != want[f.Name] || (f.Kind != ProblemNone) != strings.HasPrefix(f.Problem, f.Name+": ") {
			t.Fatalf("%s: kind %d problem %q", f.Name, f.Kind, f.Problem)
		}
	}
	if !strings.Contains(scan.Files[1].Problem, "exceeds 5000000 bytes") || scan.Files[2].Header.StartedAt != "t" || scan.Files[5].Header.ID != "proj" {
		t.Fatalf("%+v", scan.Files)
	}
	// The byte budget counts the ledgers read; reaching it truncates the scan.
	// bad.json (1 byte) fits in 50 bytes, big.json is refused unread, good.json does not fit.
	if scan = ScanDir(dir, ScanOptions{MaxBytes: 50}); !scan.Truncated || len(scan.Files) != 2 || !strings.Contains(scan.Problem, "truncated at 50 bytes") {
		t.Fatalf("%+v", scan)
	}
	if scan = ScanDir(dir, ScanOptions{MaxFiles: 2}); !scan.Truncated || len(scan.Files) != 2 || !strings.Contains(scan.Problem, "2 JSON files") {
		t.Fatalf("%+v", scan)
	}
	if scan = ScanDir(filepath.Join(dir, "missing"), ScanOptions{}); !scan.Missing {
		t.Fatalf("%+v", scan)
	}
	link := filepath.Join(t.TempDir(), "ledgers")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if scan = ScanDir(link, ScanOptions{}); scan.Problem == "" || scan.Truncated || len(scan.Files) != 0 {
		t.Fatalf("a symlinked directory was followed: %+v", scan)
	}
}

func ledgerJSON(id, status string) string {
	return `{"schemaVersion":"v0","kind":"molecule","id":"` + id + `","formulaId":"f","status":"` + status + `"}`
}

// Every Molecule leaves a .md report, a .lock approval lock and .loop-lock files
// beside its ledger. Only the ledgers count toward the entry cap, so 3000
// Molecules (12000 entries) still give a complete scan.
func TestHeaderScanDirCountsOnlyLedgersTowardTheEntryCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledgers")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const molecules = 3000
	for i := 0; i < molecules; i++ {
		id := fmt.Sprintf("mol-%d-a", i)
		for name, body := range map[string]string{id + ".json": ledgerJSON(id, "passed"), id + ".md": "# report", id + ".loop-lock": "", fmt.Sprintf("issue-%d.loop-lock", i): ""} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(filepath.Join(dir, id+".lock"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	scan := ScanDir(dir, ScanOptions{MaxFiles: molecules})
	if scan.Problem != "" || scan.Truncated || len(scan.Files) != molecules {
		t.Fatalf("problem %q truncated %t files %d", scan.Problem, scan.Truncated, len(scan.Files))
	}
	for _, f := range scan.Files {
		if f.Kind != ProblemNone || f.Header.Status != "passed" {
			t.Fatalf("%+v", f)
		}
	}
	// The default file cap is well above this Lab, so the default scan is complete too;
	// the 1 GB byte budget, not the file count, is what bounds the work.
	if scan = ScanDir(dir, ScanOptions{}); scan.Problem != "" || scan.Truncated || len(scan.Files) != molecules {
		t.Fatalf("default scan: problem %q truncated %t files %d", scan.Problem, scan.Truncated, len(scan.Files))
	}
}

// A cache reuses a header while the file's device, inode, modification time and
// size are unchanged, and re-reads it once any of them changes.
func TestHeaderCacheReusesUnchangedLedgers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledgers")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	write := func(id, status string, at time.Time) {
		t.Helper()
		path := filepath.Join(dir, id+".json")
		if err := os.WriteFile(path, []byte(ledgerJSON(id, status)), 0600); err != nil {
			t.Fatal(err)
		}
		if !at.IsZero() {
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("a", "s", old)
	write("b", "s", old)
	write("c", "s", time.Time{}) // just written: too recent to cache
	reads := map[string]int{}
	cache := NewHeaderCache()
	scan := func() map[string]string {
		t.Helper()
		s := ScanDir(dir, ScanOptions{Cache: cache, Wrap: func(name string, r io.Reader) io.Reader { reads[name]++; return r }})
		if s.Problem != "" || len(s.Files) != 3 {
			t.Fatalf("%+v", s)
		}
		status := map[string]string{}
		for _, f := range s.Files {
			if f.Kind != ProblemNone {
				t.Fatalf("%+v", f)
			}
			status[f.Header.ID] = f.Header.Status
		}
		return status
	}
	scan()
	if got := scan(); reads["a.json"] != 1 || reads["b.json"] != 1 || reads["c.json"] != 2 || got["a"] != "s" {
		t.Fatalf("reads %v status %v", reads, got)
	}
	// A different size with the same modification time is re-read.
	write("a", "changed", old)
	if got := scan(); reads["a.json"] != 2 || reads["b.json"] != 1 || got["a"] != "changed" {
		t.Fatalf("reads %v status %v", reads, got)
	}
	// A different modification time with the same size is re-read.
	write("b", "t", old.Add(time.Minute))
	if got := scan(); reads["a.json"] != 2 || reads["b.json"] != 2 || got["b"] != "t" {
		t.Fatalf("reads %v status %v", reads, got)
	}
	// Without a cache every scan reads every ledger.
	ScanDir(dir, ScanOptions{Wrap: func(name string, r io.Reader) io.Reader { reads[name]++; return r }})
	if reads["a.json"] != 3 || reads["b.json"] != 3 {
		t.Fatalf("reads %v", reads)
	}
}
