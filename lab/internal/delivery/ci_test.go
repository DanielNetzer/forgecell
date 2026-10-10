package delivery

import (
	"context"
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ciFixture(t *testing.T) (Options, checks.Snapshot) {
	t.Helper()
	o := Options{LabDir: t.TempDir(), MoleculeID: "mol-ci"}
	p := PreviewResult{MoleculeID: o.MoleculeID, Repo: "owner/repo"}
	p.Digest = digest(p)
	r := Receipt{Preview: p, State: "published", Commit: strings.Repeat("a", 40), URL: "https://github.com/owner/repo/pull/9"}
	if err := writeReceipt(o, r); err != nil {
		t.Fatal(err)
	}
	record := molecule.Record{ID: o.MoleculeID, Issue: molecule.Issue{Repo: p.Repo}, Verification: &verification.Result{SourceTree: p.Tree}}
	raw, _ := json.Marshal(record)
	os.MkdirAll(filepath.Join(o.LabDir, "ledgers"), 0700)
	os.WriteFile(filepath.Join(o.LabDir, "ledgers", o.MoleculeID+".json"), raw, 0600)
	s, err := checks.Collect(context.Background(), checks.Options{Repo: p.Repo, PR: 9, Commit: r.Commit, Required: []string{"test"}, Runner: func(context.Context, process.Options) process.Result {
		return process.Result{OK: true, Stdout: `{"headRefOid":"` + r.Commit + `","statusCheckRollup":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}`}
	}})
	if err != nil {
		t.Fatal(err)
	}
	return o, s
}
func TestCIHistory(t *testing.T) {
	o, s := ciFixture(t)
	original, _ := os.ReadFile(filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json"))
	for _, status := range []string{"passed", "pending", "failed", "missing", "unknown", "stale"} {
		current := s
		current.Status = status
		switch status {
		case "pending":
			current.Checks = []checks.Check{{Name: "test", Status: "IN_PROGRESS"}}
		case "failed":
			current.Checks = []checks.Check{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}}
		case "missing", "unknown", "stale":
			current.Checks = nil
		}
		if status == "stale" {
			current.ObservedHead = strings.Repeat("b", 40)
		}
		if _, err := AppendCI(o, current); err != nil {
			t.Fatal(err)
		}
	}
	h, err := ReadCI(o)
	if err != nil || len(h) != 6 || h[0].Snapshot.Status != "passed" || h[5].Snapshot.Status != "stale" {
		t.Fatalf("%+v %v", h, err)
	}
	after, _ := os.ReadFile(filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json"))
	if string(after) != string(original) {
		t.Fatal("receipt rewritten")
	}
	files, _ := filepath.Glob(filepath.Join(o.LabDir, "deliveries", o.MoleculeID, "ci", "*.json"))
	raw, _ := os.ReadFile(files[0])
	var a CIObservation
	json.Unmarshal(raw, &a)
	a.Snapshot.Status = "failed"
	raw, _ = json.Marshal(a)
	os.WriteFile(files[0], raw, 0600)
	if _, err := ReadCI(o); err == nil {
		t.Fatal("tampering admitted")
	}
}
func TestCIRejectsMismatchesAndWriteFailure(t *testing.T) {
	o, s := ciFixture(t)
	for _, mutate := range []func(*checks.Snapshot){func(s *checks.Snapshot) { s.Repo = "other/repo" }, func(s *checks.Snapshot) { s.PR++ }, func(s *checks.Snapshot) { s.Commit = strings.Repeat("b", 40) }, func(s *checks.Snapshot) { s.Detail = strings.Repeat("x", checks.MaxSnapshotBytes) }} {
		bad := s
		mutate(&bad)
		if _, err := AppendCI(o, bad); err == nil {
			t.Fatal("invalid evidence admitted")
		}
	}
	os.WriteFile(filepath.Join(o.LabDir, "deliveries", o.MoleculeID), []byte("blocked"), 0600)
	if _, err := AppendCI(o, s); err == nil {
		t.Fatal("write failure ignored")
	}
}

func TestCIAppendConflictsAndIdentityTampering(t *testing.T) {
	o, s := ciFixture(t)
	if _, err := AppendCI(o, s); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(o.LabDir, ".delivery-lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendCI(o, s); err == nil {
		t.Fatal("concurrent conflict not reported")
	}
	os.Remove(filepath.Join(o.LabDir, ".delivery-lock"))
	h, err := ReadCI(o)
	if err != nil || len(h) != 1 {
		t.Fatalf("history lost: %+v %v", h, err)
	}
	receiptFile := filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json")
	raw, _ := os.ReadFile(receiptFile)
	var r Receipt
	json.Unmarshal(raw, &r)
	r.Commit = strings.Repeat("b", 40)
	raw, _ = json.Marshal(r)
	os.WriteFile(receiptFile, raw, 0600)
	if _, err := ReadCI(o); err == nil {
		t.Fatal("receipt replacement admitted")
	}
}

func TestCIBoundsRetainAcknowledgedHistory(t *testing.T) {
	o, s := ciFixture(t)
	for i := 0; i < MaxCIObservations; i++ {
		if _, err := AppendCI(o, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AppendCI(o, s); err == nil {
		t.Fatal("count bound ignored")
	}
	h, err := ReadCI(o)
	if err != nil || len(h) != MaxCIObservations {
		t.Fatalf("bounded history lost: %d %v", len(h), err)
	}
}

func TestCIConcurrentAppendsNeverLoseAcknowledgedHistory(t *testing.T) {
	o, s := ciFixture(t)
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { _, err := AppendCI(o, s); results <- err }()
	}
	acknowledged := 0
	for i := 0; i < 8; i++ {
		if <-results == nil {
			acknowledged++
		}
	}
	h, err := ReadCI(o)
	if err != nil || len(h) != acknowledged || acknowledged == 0 {
		t.Fatalf("acknowledged=%d history=%d error=%v", acknowledged, len(h), err)
	}
}

func TestCIAppendCountsActualPersistedBytes(t *testing.T) {
	o, s := ciFixture(t)
	for i := 0; i < 15; i++ {
		if _, err := AppendCI(o, s); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(o.LabDir, "deliveries", o.MoleculeID, "ci", "*.json"))
	for _, file := range files {
		raw, _ := os.ReadFile(file)
		raw = append(raw, []byte(strings.Repeat(" ", 260_000-len(raw)))...)
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if h, err := ReadCI(o); err != nil || len(h) != 15 {
		t.Fatalf("valid padded history: %d %v", len(h), err)
	}
	s.Status = "unknown"
	s.Checks = nil
	for i := 0; i < 100; i++ {
		s.Checks = append(s.Checks, checks.Check{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS", URL: strings.Repeat("x", 2000)})
	}
	if _, err := AppendCI(o, s); err == nil {
		t.Fatal("append ignored actual history byte budget")
	}
	if h, err := ReadCI(o); err != nil || len(h) != 15 {
		t.Fatalf("prior history changed: %d %v", len(h), err)
	}
}

func TestOlderSuccessCannotHideNewerUncertainty(t *testing.T) {
	o, s := ciFixture(t)
	newer := s
	newer.Status = "unknown"
	newer.Checks = nil
	newer.CheckedAt = "2026-10-09T08:00:01Z"
	older := s
	older.CheckedAt = "2026-10-09T08:00:00Z"
	if _, err := AppendCI(o, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendCI(o, older); err != nil {
		t.Fatal(err)
	}
	h, err := ReadCI(o)
	if err != nil {
		t.Fatal(err)
	}
	if got := CIClassification(h); got != "unknown" {
		t.Fatalf("older success hid newer uncertainty: %s", got)
	}
}

func TestCICollectionSerializesInvertedLookupOrder(t *testing.T) {
	o, s := ciFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	options := checks.Options{Repo: s.Repo, PR: s.PR, Commit: s.Commit, Required: s.Required}
	options.Runner = func(context.Context, process.Options) process.Result {
		close(started)
		<-release // An earlier start would otherwise observe after the competing lookup.
		return process.Result{OK: true, Stdout: `{"headRefOid":"` + s.Commit + `","statusCheckRollup":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}`}
	}
	go func() { _, err := CollectCI(context.Background(), o, options); done <- err }()
	<-started
	competing := options
	queried := false
	competing.Runner = func(context.Context, process.Options) process.Result {
		queried = true
		return process.Result{OK: true, Stdout: `{"headRefOid":"` + strings.Repeat("b", 40) + `","statusCheckRollup":[]}`}
	}
	_, conflict := CollectCI(context.Background(), o, competing)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if conflict == nil || queried {
		t.Fatal("overlapping collection reached GitHub; inverted observation order possible")
	}
	if _, err := CollectCI(context.Background(), o, competing); err != nil {
		t.Fatal(err)
	}
	h, err := ReadCI(o)
	if err != nil || len(h) != 2 || h[0].Snapshot.Status != "passed" || h[1].Snapshot.Status != "stale" || CIClassification(h) != "unknown" {
		t.Fatalf("historical pass or latest stale evidence lost: %+v %v", h, err)
	}
}

func TestCIClockRegressionCannotReviveHistoricalPass(t *testing.T) {
	o, s := ciFixture(t)
	s.CheckedAt = "2026-10-09T08:00:01Z"
	if _, err := AppendCI(o, s); err != nil {
		t.Fatal(err)
	}
	s.CheckedAt = "2026-10-09T08:00:00Z"
	s.ObservedHead = strings.Repeat("b", 40)
	s.Status = "stale"
	if _, err := AppendCI(o, s); err != nil {
		t.Fatal(err)
	}
	h, err := ReadCI(o)
	if err != nil || CIClassification(h) != "unknown" {
		t.Fatalf("clock regression revived historical pass: %+v %v", h, err)
	}
}
