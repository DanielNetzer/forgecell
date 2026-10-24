package ui

import (
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// age moves a file's modification time out of the racy window, as a ledger
// written by an earlier run would be.
func age(t *testing.T, path string, ago time.Duration) {
	t.Helper()
	when := time.Now().Add(-ago)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// countingHandler counts ledger and receipt reads through the handler's seams.
func countingHandler(t *testing.T, lab string) (http.Handler, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	h := testHandler(t, Options{LabDir: lab})
	var records, receipts atomic.Int32
	hh := h.(*handler)
	hh.records = func(lab, id string) (molecule.Record, error) { records.Add(1); return molecule.ReadRecord(lab, id) }
	hh.receipts = func(lab, id string) (delivery.ReceiptStatus, bool, error) {
		receipts.Add(1)
		return delivery.ReadReceiptStatus(lab, id)
	}
	return h, &records, &receipts
}

func listOf(t *testing.T, h http.Handler) MoleculeList {
	t.Helper()
	var list MoleculeList
	decode(t, do(t, h, authed("/api/molecules")), &list)
	return list
}

func TestUIListCacheReusesUnchangedSummariesAndRereadsChangedOnes(t *testing.T) {
	lab, r := labFixture(t)
	writeReceipt(t, lab, r.ID, "committed", "")
	ledger, receipt := filepath.Join(lab, "ledgers", r.ID+".json"), filepath.Join(lab, "deliveries", r.ID+".json")
	age(t, ledger, time.Hour)
	age(t, receipt, time.Hour)
	h, records, receipts := countingHandler(t, lab)
	first := listOf(t, h)
	if records.Load() != 1 || receipts.Load() != 1 || len(first.Molecules) != 1 {
		t.Fatalf("first list: %d ledger and %d receipt reads, %+v", records.Load(), receipts.Load(), first.Molecules)
	}
	// An unchanged Lab is answered from the cache, even without If-None-Match.
	if again := listOf(t, h); records.Load() != 1 || receipts.Load() != 1 || again.Molecules[0] != first.Molecules[0] {
		t.Fatalf("unchanged list re-read: %d ledger and %d receipt reads", records.Load(), receipts.Load())
	}
	// A changed ledger invalidates its Summary.
	r.Status = "blocked"
	writeRecord(t, lab, r)
	age(t, ledger, 30*time.Minute)
	if list := listOf(t, h); records.Load() != 2 || list.Molecules[0].Lane != LaneBlocked {
		t.Fatalf("changed ledger: %d reads, lane %q", records.Load(), list.Molecules[0].Lane)
	}
	// So does a changed receipt, with the ledger untouched.
	writeReceipt(t, lab, r.ID, "published", "OPEN")
	age(t, receipt, 30*time.Minute)
	if list := listOf(t, h); records.Load() != 3 || receipts.Load() != 3 || list.Molecules[0].Delivery != "Draft published" {
		t.Fatalf("changed receipt: %d ledger and %d receipt reads, %+v", records.Load(), receipts.Load(), list.Molecules[0])
	}
	// A file written within the racy window is never cached.
	writeRecord(t, lab, r)
	listOf(t, h)
	listOf(t, h)
	if records.Load() != 5 {
		t.Fatalf("a just-written ledger was cached: %d reads", records.Load())
	}
	// A removed Molecule leaves the cache with its ledger.
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	if list := listOf(t, h); len(list.Molecules) != 0 || len(h.(*handler).cache.entries) != 0 {
		t.Fatalf("removed ledger still listed: %+v", list.Molecules)
	}
}

func TestUIConcurrentListRequestsBuildOnce(t *testing.T) {
	lab, r := labFixture(t)
	// An aged ledger keeps one ETag, so every request asks for the same build.
	age(t, filepath.Join(lab, "ledgers", r.ID+".json"), time.Hour)
	h, _, _ := countingHandler(t, lab)
	hh := h.(*handler)
	var reads atomic.Int32
	release := make(chan struct{})
	hh.records = func(lab, id string) (molecule.Record, error) {
		reads.Add(1)
		<-release
		return molecule.ReadRecord(lab, id)
	}
	const n = 8
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() { codes <- do(t, h, authed("/api/molecules")).Code }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		hh.cache.mu.Lock()
		joined := hh.cache.flight != nil && hh.cache.flight.waiters == n-1
		hh.cache.mu.Unlock()
		if joined {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("requests never joined one build")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	for i := 0; i < n; i++ {
		if code := <-codes; code != http.StatusOK {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	// The other requests joined the first build before it read anything, so the
	// ledger was read once.
	if reads.Load() != 1 {
		t.Fatalf("%d concurrent requests read the ledger %d times", n, reads.Load())
	}
}

func TestUIRedactionKeepsPathBoundariesAndScrubsLocalPaths(t *testing.T) {
	t.Setenv("HOME", "/Users/dan")
	lab := t.TempDir()
	x := newRedactor(lab, molecule.Record{})
	for in, want := range map[string]string{
		"/Users/dan/x and /Users/dan":                  "~/x and ~",
		"owner /Users/danielle/notes.md":               "owner [local path]",
		"read /etc/passwd.":                            "read [local path].",
		"ran `/opt/tool --flag` (in /tmp/w)":           "ran `[local path] --flag` (in [local path])",
		"path=/var/data,next and /Volumes/USB":         "path=[local path],next and [local path]",
		"open file:///Users/someone/r/a.go now":        "open [local path] now",
		"cat x|/home/u/.ssh/id >/root/out":             "cat x|[local path] >[local path]",
		"quoted “/private/var/folders/x” and ‘/srv/a’": "quoted “[local path]” and ‘[local path]’",
		lab + "/ledgers/m.json and " + lab:             "<lab>/ledgers/m.json and <lab>",
		lab + "2/other":                                "[local path]",
		// Repo-relative or URL text is not a local path.
		"see https://github.com/o/r/pull/5":       "see https://github.com/o/r/pull/5",
		"a/b and 10/24 and and/or":                "a/b and 10/24 and and/or",
		"GET /api/molecules and /docs/install.md": "GET /api/molecules and /docs/install.md",
		"JSON pointer /harnessAttempts/0 too":     "JSON pointer /harnessAttempts/0 too",
		"/variable and /usr/bin/go and /tmpfile":  "/variable and /usr/bin/go and /tmpfile",
		"<lab>/ledgers is already a label":        "<lab>/ledgers is already a label",
	} {
		if got := x.text(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
