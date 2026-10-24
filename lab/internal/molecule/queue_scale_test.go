package molecule

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// countedRead counts the bytes read through it and, every 16 reads, samples the
// heap, so the peak includes garbage not yet collected.
type countedRead struct {
	r     io.Reader
	n     *int64
	reads *int
	peak  *uint64
}

func (c countedRead) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	if *c.reads++; *c.reads%16 == 0 {
		*c.peak = max(*c.peak, heapNow())
	}
	return n, err
}

func heapNow() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

type scaleLedger struct {
	id, status, phase, started string
	number                     int64
	newOrder                   bool
	size                       int
}

// scaleRecord is a ledger of at least size bytes, either in the Record's
// header-first order or in the order Labs wrote before it.
func scaleRecord(t *testing.T, lab string, l scaleLedger, size int) []byte {
	t.Helper()
	issue := Issue{Number: l.number, Repo: "acme/widgets", Title: "t", Body: "b", URL: fmt.Sprintf("https://github.com/acme/widgets/issues/%d", l.number), State: "OPEN"}
	if l.newOrder {
		var notes []string
		if size > 0 {
			notes = make([]string, size/1000+20)
		}
		for i := range notes {
			notes[i] = strings.Repeat("n", 990)
		}
		r := Record{SchemaVersion: "v0", Kind: "molecule", ID: l.id, Status: l.status, FormulaID: "sample", Mode: "ticket", StartedAt: l.started, LabDir: lab,
			Issue: issue, Workspace: Workspace{Repo: "acme/widgets"}, Readiness: &readiness.State{SchemaVersion: "v1", Phase: l.phase}, Notes: notes}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	issueJSON, _ := json.Marshal(issue)
	var b bytes.Buffer
	b.WriteString(`{"policyDecisions":[`)
	for b.Len() < size {
		b.WriteString(`{"policy":"p","approvedBy":"a","evidence":[{"pointer":"/x","observed":"` + strings.Repeat("o", 900) + `"}]},`)
	}
	fmt.Fprintf(&b, `{}],"readiness":{"schemaVersion":"v1","phase":%q,"plans":[],"events":[]},"schemaVersion":"v0","kind":"molecule","id":%q,"mode":"ticket","status":%q,"formulaId":"sample","formulaApproved":true,"formulaSnapshot":{"yaml":"","sha256":""},"startedAt":%q,"finishedAt":"","labDir":%q,"workspace":{"repo":"acme/widgets"},"issue":%s,"atoms":[],"notes":[]}`,
		l.phase, l.id, l.status, l.started, lab, issueJSON)
	return b.Bytes()
}

// A Lab of 300 ledgers, half saved in the Record's header-first order and half in
// the order Labs wrote before it, together larger than the former 20 MB cap. The
// inventory must be complete and correct, decode at most 64 KB of each new-order
// ledger before only validating the rest, and keep its peak heap bounded. The
// ledgers are about 120 KB each; FORGECELL_LARGE_LAB_TEST=1 uses 1.5 MB ledgers
// (about 450 MB). No wall-clock bound is asserted.
func TestQueueInventoryLargeLab(t *testing.T) {
	size := 120_000
	if os.Getenv("FORGECELL_LARGE_LAB_TEST") == "1" {
		if testing.Short() {
			t.Skip("the 1.5 MB variant writes about 450 MB of ledgers")
		}
		size = 1_500_000
	}
	o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	dir := filepath.Join(o.LabDir, "ledgers")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const ledgers = 300
	statuses := []string{"passed", "blocked", "failed"}
	phases := []string{"scope-waiting", "complete", "verifying"}
	want := map[string]scaleLedger{}
	total := 0
	for i := 0; i < ledgers; i++ {
		l := scaleLedger{id: fmt.Sprintf("mol-%d-big%03d", 1000+i%100, i), status: statuses[i%3], phase: phases[(i/3)%3],
			started: fmt.Sprintf("2026-01-01T00:%02d:%02dZ", i/60, i%60), number: int64(1000 + i%100), newOrder: i%2 == 0}
		raw := scaleRecord(t, o.LabDir, l, size)
		if len(raw) < size || len(raw) > 5_000_000 {
			t.Fatalf("fixture %s is %d bytes", l.id, len(raw))
		}
		if err := os.WriteFile(filepath.Join(dir, l.id+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		l.size, total = len(raw), total+len(raw)
		want[l.id] = l
	}
	if total <= 20_000_000 {
		t.Fatalf("the Lab holds %d bytes, not more than the former cap", total)
	}

	runtime.GC()
	base := heapNow()
	peak := base
	read := map[string]*int64{}
	reads := 0
	wrap := func(name string, r io.Reader) io.Reader {
		n := new(int64)
		read[name] = n
		return countedRead{r, n, &reads, &peak}
	}
	scan := scanLedgers(o.LabDir, wrap)
	peak = max(peak, heapNow())
	if scan.Problem != "" || len(scan.Files) != ledgers {
		t.Fatalf("problem %q files %d", scan.Problem, len(scan.Files))
	}
	newOrderHeaderBytes := int64(0)
	for _, f := range scan.Files {
		l := want[strings.TrimSuffix(f.Name, ".json")]
		if f.Kind != ledger.ProblemNone {
			t.Fatalf("%s: %s", f.Name, f.Problem)
		}
		// Every ledger is read to its end: decoded or, after the header, validated.
		if n := *read[f.Name]; n != int64(l.size) {
			t.Fatalf("%s: read %d of %d bytes", f.Name, n, l.size)
		}
		if l.newOrder {
			if f.HeaderBytes > 64<<10 {
				t.Fatalf("%s: decoded %d bytes of a new-order ledger", f.Name, f.HeaderBytes)
			}
			newOrderHeaderBytes += f.HeaderBytes
		} else if f.HeaderBytes < int64(size) {
			t.Fatalf("%s: an old-order ledger's header ends past its padding, yet only %d bytes were decoded", f.Name, f.HeaderBytes)
		}
	}
	if newOrderHeaderBytes > ledgers/2*64<<10 {
		t.Fatalf("decoded %d bytes across the new-order ledgers", newOrderHeaderBytes)
	}
	if peak > base && peak-base > 32<<20 {
		t.Fatalf("the heap peaked %d bytes above its base during the scan", peak-base)
	}

	q := ReadQueueInventory(o.LabDir)
	if !q.Complete || q.Truncated || len(q.Diagnostics) != 0 || len(q.Attempts) != ledgers {
		t.Fatalf("complete %t truncated %t diagnostics %v attempts %d", q.Complete, q.Truncated, q.Diagnostics, len(q.Attempts))
	}
	for _, a := range q.Attempts {
		l, ok := want[a.ID]
		if !ok || a.Repository != "acme/widgets" || a.Number != l.number || a.Status != l.status || a.Phase != l.phase || a.StartedAt != l.started {
			t.Fatalf("attempt %+v, want %+v", a, l)
		}
	}

	// The shared reader serves clarification history the same way.
	ids, truncated, err := clarificationHistory(o.LabDir, "acme/widgets", 1007)
	if err != nil || truncated || len(ids) != 3 || ids[0] != "mol-1007-big007" || ids[2] != "mol-1007-big207" {
		t.Fatalf("ids %v truncated %t err %v", ids, truncated, err)
	}

	// The loop is not stopped by an inventory larger than the former 20 MB cap, and
	// the ledger it saves is decoded only up to its header.
	report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
	item := loopItem(t, report.Passes[0], 12)
	if !item.Started || item.Failed || item.MoleculeID == "" || report.Failed() {
		t.Fatalf("%+v", item)
	}
	if q = ReadQueueInventory(o.LabDir); !q.Complete || len(q.Attempts) != ledgers+1 {
		t.Fatalf("complete %t attempts %d diagnostics %v", q.Complete, len(q.Attempts), q.Diagnostics)
	}
	for _, f := range scanLedgers(o.LabDir, nil).Files {
		if f.Name == item.MoleculeID+".json" && (f.Kind != ledger.ProblemNone || f.HeaderBytes > 64<<10) {
			t.Fatalf("%+v", f)
		}
	}

	// One oversized ledger still makes the inventory incomplete and is named.
	if err := os.WriteFile(filepath.Join(dir, "mol-huge.json"), bytes.Repeat([]byte(" "), 6_000_000), 0600); err != nil {
		t.Fatal(err)
	}
	q = ReadQueueInventory(o.LabDir)
	if q.Complete || !q.Truncated || len(q.Diagnostics) != 1 || q.Diagnostics[0] != "mol-huge.json: file exceeds 5000000 bytes" {
		t.Fatalf("complete %t truncated %t diagnostics %v", q.Complete, q.Truncated, q.Diagnostics)
	}
}

func writeQueueLedgers(t *testing.T, lab string, n int, companions bool) {
	t.Helper()
	dir := filepath.Join(lab, "ledgers")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		l := scaleLedger{id: fmt.Sprintf("mol-%d-q", i+1), status: "passed", phase: "complete", started: "2026-01-01T00:00:00Z", number: int64(i + 1), newOrder: true}
		if err := os.WriteFile(filepath.Join(dir, l.id+".json"), scaleRecord(t, lab, l, 0), 0600); err != nil {
			t.Fatal(err)
		}
		if !companions {
			continue
		}
		for _, name := range []string{l.id + ".md", l.id + loopLockSuffix, fmt.Sprintf("issue-%d%s", i, loopLockSuffix)} {
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(filepath.Join(dir, l.id+".lock"), 0700); err != nil {
			t.Fatal(err)
		}
	}
}

// Each Molecule's .md report, .lock approval lock and .loop-lock files do not
// count toward the 4096-entry cap: 2000 Molecules, 10000 entries, are complete.
func TestQueueInventoryCountsOnlyLedgersTowardTheEntryCap(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	writeQueueLedgers(t, lab, 2000, true)
	q := ReadQueueInventory(lab)
	if !q.Complete || q.Truncated || len(q.Attempts) != 2000 {
		t.Fatalf("complete %t truncated %t diagnostics %v attempts %d", q.Complete, q.Truncated, q.Diagnostics, len(q.Attempts))
	}
}

// Repeated inventories in one process, such as a loop pass and its re-read under
// an Issue lock, reuse unchanged headers and re-read a changed ledger. Not
// parallel, so no other test's scans share the process cache meanwhile.
func TestQueueInventoryReusesUnchangedHeaders(t *testing.T) {
	lab := t.TempDir()
	writeQueueLedgers(t, lab, 3, false)
	old := time.Now().Add(-time.Hour)
	path := func(i int) string { return filepath.Join(lab, "ledgers", fmt.Sprintf("mol-%d-q.json", i)) }
	for i := 1; i <= 3; i++ {
		if err := os.Chtimes(path(i), old, old); err != nil {
			t.Fatal(err)
		}
	}
	reads := map[string]int{}
	wrap := func(name string, r io.Reader) io.Reader { reads[name]++; return r }
	inventory := func() QueueInventory {
		t.Helper()
		q := readQueueInventory(lab, wrap)
		if !q.Complete || len(q.Attempts) != 3 {
			t.Fatalf("%+v", q)
		}
		return q
	}
	inventory()
	inventory()
	if len(reads) != 3 || reads["mol-1-q.json"] != 1 || reads["mol-2-q.json"] != 1 || reads["mol-3-q.json"] != 1 {
		t.Fatalf("the second inventory re-read unchanged ledgers: %v", reads)
	}
	// A rewritten ledger (another size) and a touched one (another mtime) are re-read.
	raw, err := os.ReadFile(path(2))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"status":"passed"`), []byte(`"status":"blocked"`), 1)
	if err := os.WriteFile(path(2), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path(2), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path(3), old.Add(time.Minute), old.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	q := inventory()
	if reads["mol-1-q.json"] != 1 || reads["mol-2-q.json"] != 2 || reads["mol-3-q.json"] != 2 {
		t.Fatalf("reads %v", reads)
	}
	for _, a := range q.Attempts {
		if a.ID == "mol-2-q" && a.Status != "blocked" {
			t.Fatalf("a changed ledger kept its cached status: %+v", a)
		}
	}
}

// A Record marshals its header fields first, in this order, so ledger.ReadHeader
// stops before the large attempt arrays.
func TestQueueInventoryRecordHeaderOrder(t *testing.T) {
	t.Parallel()
	r := Record{Readiness: &readiness.State{}, PolicyDecisions: []PolicyDecision{{}}, AnalysisAttempts: []AnalysisAttempt{{}}, HarnessAttempts: []HarnessAttempt{{}},
		ReviewAttempts: []ReviewAttempt{{}}, VerificationAttempts: []VerificationAttempt{{}}, Notes: []string{"n"}}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	header := []string{"schemaVersion", "kind", "id", "status", "formulaId", "mode", "startedAt", "finishedAt", "labDir", "issue", "workspace", "readiness"}
	if len(keys) < len(header) || strings.Join(keys[:len(header)], ",") != strings.Join(header, ",") {
		t.Fatalf("the header does not lead the Record: %v", keys)
	}
	for _, large := range []string{"policyDecisions", "analysisAttempts", "harnessAttempts", "reviewAttempts", "verificationAttempts", "atoms", "notes"} {
		found := false
		for _, k := range keys[len(header):] {
			found = found || k == large
		}
		if !found {
			t.Fatalf("%s missing after the header: %v", large, keys)
		}
	}
	// readiness.State leads with its phase, so the header ends inside readiness.
	state, _ := json.Marshal(readiness.State{})
	if !bytes.HasPrefix(state, []byte(`{"schemaVersion":"","phase":`)) {
		t.Fatalf("readiness phase is not at its head: %s", state)
	}
}
