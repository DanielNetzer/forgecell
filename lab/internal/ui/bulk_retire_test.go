package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// closedLab is a fixture Lab of attempts for Issues 20-27, and closedFetcher is
// GitHub's side of it: Issues 20, 21, 23, 24 and 25 closed, 22 open, 26 and 27
// not read. Only mol-20-blocked and mol-21-waiting are attempts for closed
// Issues that bulk retire may retire: 22's Issue is open, 23 is running, 24 is
// retired already, 25 has a delivery in progress, 26's state is unknown and 27
// belongs to another repository.
func closedLab(t *testing.T) string {
	t.Helper()
	lab, base := labFixture(t)
	os.Remove(filepath.Join(lab, "ledgers", base.ID+".json"))
	add := func(id string, n int64, status, phase string) molecule.Record {
		r := base
		r.ID, r.Status, r.Issue.Number = id, status, n
		r.Issue.URL = fmt.Sprintf("https://github.com/o/r/issues/%d", n)
		r.Readiness = &readiness.State{SchemaVersion: "v1", Phase: phase}
		return r
	}
	for _, r := range []molecule.Record{
		add("mol-20-blocked", 20, "blocked", "failed"),
		add("mol-21-waiting", 21, "waiting", "scope-waiting"),
		add("mol-22-open", 22, "blocked", "failed"),
		add("mol-23-running", 23, "running", "coding"),
		add("mol-25-delivery", 25, "completed", "review-waiting"),
		add("mol-26-unknown", 26, "blocked", "failed"),
	} {
		writeRecord(t, lab, r)
	}
	retired := add("mol-24-retired", 24, molecule.StatusRetired, "failed")
	retired.Retired = &molecule.Retirement{At: "2026-10-26T10:00:00Z", Reason: "stale", PriorStatus: "blocked"}
	writeRecord(t, lab, retired)
	elsewhere := add("mol-27-elsewhere", 27, "blocked", "failed")
	elsewhere.Issue.Repo, elsewhere.Workspace.Repo, elsewhere.Issue.URL = "x/y", "x/y", "https://github.com/x/y/issues/27"
	writeRecord(t, lab, elsewhere)
	writeReceipt(t, lab, "mol-25-delivery", "committed", "")
	return lab
}

func closedFetcher() *syncFetcher {
	f := &syncFetcher{issues: map[int64]forgesync.Issue{}, prs: map[int64]forgesync.PR{}}
	f.listing.Issues = []forgesync.Issue{openSyncIssue(22)}
	f.listing.Total = 1
	for _, n := range []int64{20, 21, 23, 24, 25, 27} {
		closed := openSyncIssue(n)
		closed.State, closed.StateReason = forgesync.IssueClosed, forgesync.ReasonCompleted
		f.issues[n] = closed
	}
	return f
}

var closedWant = []string{"mol-20-blocked", "mol-21-waiting"}

func closedBody(v RetireClosedView) string {
	return fmt.Sprintf(`{"count":%d,"digest":%q}`, v.Count, v.Digest)
}

// The list holds exactly the closed-Issue, non-running, no-delivery attempts;
// the confirmation retires each of them through the engine with the fixed
// reason and reports each one committed from the ledger read-back. The list is
// then empty, and nothing else in the Lab was retired.
func TestUIBulkRetireRetiresExactlyTheClosedIssueAttempts(t *testing.T) {
	lab := closedLab(t)
	h, _ := syncHandler(t, Options{LabDir: lab}, closedFetcher())
	v := viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK)
	var ids []string
	for _, m := range v.Molecules {
		ids = append(ids, m.ID)
	}
	if !v.Available || v.Truncated || v.Count != 2 || !reflect.DeepEqual(ids, closedWant) || v.Digest != listDigest(closedWant) {
		t.Fatalf("list %+v", v)
	}
	if v.Molecules[0].Issue != 20 || v.Molecules[0].Lane != LaneBlocked || v.Molecules[1].Lane != LaneDecision {
		t.Fatalf("items %+v", v.Molecules)
	}
	var reasons []string
	retire := h.(*handler).retire
	h.(*handler).retire = func(lab, id, reason string) (molecule.Record, error) {
		reasons = append(reasons, reason)
		return retire(lab, id, reason)
	}
	report := viewOf[RetireClosedReport](t, do(t, h, gatePost("/api/retire-closed", closedBody(v))), http.StatusOK)
	if report.Attempted != 2 || report.Retired != 2 || report.Refused != 0 || len(report.Results) != 2 {
		t.Fatalf("report %+v", report)
	}
	for i, res := range report.Results {
		if res.ID != closedWant[i] || !res.Committed || res.Category != "" {
			t.Fatalf("result %+v", res)
		}
	}
	if !reflect.DeepEqual(reasons, []string{BulkRetireReason, BulkRetireReason}) {
		t.Fatalf("reasons %q", reasons)
	}
	for _, id := range []string{"mol-20-blocked", "mol-21-waiting", "mol-22-open", "mol-23-running", "mol-24-retired", "mol-25-delivery", "mol-26-unknown", "mol-27-elsewhere"} {
		r, err := molecule.ReadRecord(lab, id)
		if err != nil {
			t.Fatal(err)
		}
		want := id == "mol-20-blocked" || id == "mol-21-waiting" || id == "mol-24-retired"
		if (r.Status == molecule.StatusRetired) != want {
			t.Errorf("%s: status %s", id, r.Status)
		}
		if (id == "mol-20-blocked" || id == "mol-21-waiting") && (r.Retired == nil || r.Retired.Reason != BulkRetireReason) {
			t.Errorf("%s: retirement %+v", id, r.Retired)
		}
	}
	if again := viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK); !again.Available || again.Count != 0 || len(again.Molecules) != 0 {
		t.Fatalf("after: %+v", again)
	}
}

// Each Molecule's result is reported on its own: an engine refusal is a fixed
// category, an engine call the ledger does not record is not committed, and a
// refusal never stops the rest.
func TestUIBulkRetireReportsEachResult(t *testing.T) {
	lab := closedLab(t)
	h, _ := syncHandler(t, Options{LabDir: lab}, closedFetcher())
	v := viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK)
	h.(*handler).retire = func(lab, id, reason string) (molecule.Record, error) {
		if id == "mol-20-blocked" {
			return molecule.Record{}, fmt.Errorf("%w at %s", molecule.ErrRetireLocked, lab)
		}
		return molecule.Record{}, nil
	}
	w := do(t, h, gatePost("/api/retire-closed", closedBody(v)))
	report := viewOf[RetireClosedReport](t, w, http.StatusOK)
	if report.Attempted != 2 || report.Retired != 0 || report.Refused != 2 {
		t.Fatalf("report %+v", report)
	}
	if r := report.Results[0]; r.ID != "mol-20-blocked" || r.Committed || r.Category != "locked" || r.Error == "" {
		t.Fatalf("refused %+v", r)
	}
	if r := report.Results[1]; r.ID != "mol-21-waiting" || r.Committed || r.Category != "unreadable" {
		t.Fatalf("unrecorded %+v", r)
	}
	if strings.Contains(w.Body.String(), lab) {
		t.Fatalf("the report names the Lab path: %s", w.Body)
	}
}

// The bulk retire runs under one overall deadline. When it passes, each
// remaining Molecule is reported not attempted, never dropped, and is neither
// retired nor sent to the engine; it is listed again afterwards. The deadline
// is a seam: the test expires it after the first retire, so no wall clock runs.
func TestUIBulkRetireReportsNotAttemptedPastTheDeadline(t *testing.T) {
	lab := closedLab(t)
	h, _ := syncHandler(t, Options{LabDir: lab}, closedFetcher())
	v := viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hh := h.(*handler)
	hh.closedDeadline = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
	var retired []string
	retire := hh.retire
	hh.retire = func(lab, id, reason string) (molecule.Record, error) {
		retired = append(retired, id)
		defer cancel()
		return retire(lab, id, reason)
	}
	report := viewOf[RetireClosedReport](t, do(t, h, gatePost("/api/retire-closed", closedBody(v))), http.StatusOK)
	if report.Attempted != 1 || report.Retired != 1 || report.Refused != 0 || report.NotAttempted != 1 || len(report.Results) != 2 {
		t.Fatalf("report %+v", report)
	}
	if r := report.Results[0]; r.ID != closedWant[0] || !r.Committed {
		t.Fatalf("first %+v", r)
	}
	if r := report.Results[1]; r.ID != closedWant[1] || r.Committed || r.Category != "not-attempted" || r.Error == "" {
		t.Fatalf("second %+v", r)
	}
	if !reflect.DeepEqual(retired, closedWant[:1]) {
		t.Fatalf("engine calls %v", retired)
	}
	if r, err := molecule.ReadRecord(lab, closedWant[1]); err != nil || r.Status == molecule.StatusRetired {
		t.Fatalf("a not-attempted Molecule was retired: %v %s", err, r.Status)
	}
	if again := viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK); again.Count != 1 || again.Molecules[0].ID != closedWant[1] {
		t.Fatalf("after: %+v", again)
	}
	// An already expired deadline attempts nothing and reports every Molecule.
	expired, stop := context.WithCancel(context.Background())
	stop()
	hh.closedDeadline = func() (context.Context, context.CancelFunc) { return expired, func() {} }
	v = viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK)
	report = viewOf[RetireClosedReport](t, do(t, h, gatePost("/api/retire-closed", closedBody(v))), http.StatusOK)
	if report.Attempted != 0 || report.NotAttempted != 1 || len(report.Results) != 1 || report.Results[0].Category != "not-attempted" || len(retired) != 1 {
		t.Fatalf("expired %+v after %v", report, retired)
	}
}

// Without current, complete Issue state nothing is listed and the confirmation
// is refused; a list that changed since it was shown is refused as stale; and
// nothing is retired in any of these.
func TestUIBulkRetireRefusesWithoutCurrentIssueState(t *testing.T) {
	lab := closedLab(t)
	f := closedFetcher()
	h, clk := syncHandler(t, Options{LabDir: lab}, f)
	calls := countRetire(h, nil)
	good := viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK)
	before := snapshot(t, lab)

	// A changed list: a count or digest other than the current one.
	for _, body := range []string{fmt.Sprintf(`{"count":1,"digest":%q}`, good.Digest), fmt.Sprintf(`{"count":2,"digest":%q}`, listDigest([]string{"mol-20-blocked"}))} {
		refusalOf(t, do(t, h, gatePost("/api/retire-closed", body)), http.StatusConflict, "stale")
	}
	// GitHub fails: the last good snapshot is no longer current.
	f.mu.Lock()
	f.err = errors.New("gh: rate limited at " + lab)
	f.mu.Unlock()
	clk.add(3 * forgesync.TTL)
	v := viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK)
	if v.Available || v.Reason != closedUnavailable || len(v.Molecules) != 0 || v.Digest != "" {
		t.Fatalf("unavailable list %+v", v)
	}
	refusalOf(t, do(t, h, gatePost("/api/retire-closed", closedBody(good))), http.StatusConflict, "issue-state")
	// No repository.
	h.(*handler).forge.repository = func(string) string { return "" }
	if v := viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK); v.Available || v.Reason != closedNoRepository {
		t.Fatalf("no repository %+v", v)
	}
	refusalOf(t, do(t, h, gatePost("/api/retire-closed", closedBody(good))), http.StatusConflict, "issue-state")
	if *calls != 0 {
		t.Fatalf("a refused bulk retire reached the engine %d times", *calls)
	}
	if after := snapshot(t, lab); after != before {
		t.Fatal("a refused bulk retire changed the Lab")
	}

	// Truncated Issue states are incomplete, so nothing is listed.
	lab2 := closedLab(t)
	h2, _ := syncHandler(t, Options{LabDir: lab2}, &truncatedFetcher{closedFetcher()})
	if v := viewOf[RetireClosedView](t, do(t, h2, authed("/api/retire-closed")), http.StatusOK); v.Available || v.Reason != closedIncomplete {
		t.Fatalf("truncated states %+v", v)
	}
}

type truncatedFetcher struct{ *syncFetcher }

func (f *truncatedFetcher) States(ctx context.Context, repo string, issues, prs []int64) (forgesync.States, error) {
	s, err := f.syncFetcher.States(ctx, repo, issues, prs)
	s.Truncated = true
	return s, err
}

// The POST has the write protection and a strict body, and sample data
// refuses both routes; none reaches the engine.
func TestUIBulkRetireGuardsAndDemo(t *testing.T) {
	lab := closedLab(t)
	h, _ := syncHandler(t, Options{LabDir: lab}, closedFetcher())
	calls := countRetire(h, nil)
	v := viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK)
	before := snapshot(t, lab)
	for name, forged := range forgedWrites("/api/retire-closed", closedBody(v)) {
		if w := do(t, h, forged); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	for name, body := range map[string]string{
		"unknown field": fmt.Sprintf(`{"count":2,"digest":%q,"reason":"mine"}`, v.Digest),
		"ids field":     fmt.Sprintf(`{"count":2,"digest":%q,"ids":["mol-22-open"]}`, v.Digest),
		"zero count":    fmt.Sprintf(`{"count":0,"digest":%q}`, v.Digest),
		"huge count":    fmt.Sprintf(`{"count":%d,"digest":%q}`, maxBulkRetire+1, v.Digest),
		"short digest":  `{"count":2,"digest":"abc"}`,
		"two objects":   closedBody(v) + `{}`,
		"oversized":     `{"count":2` + strings.Repeat(" ", maxGateBody) + `}`,
	} {
		if w := do(t, h, gatePost("/api/retire-closed", body)); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if *calls != 0 {
		t.Fatalf("refused requests reached the engine %d times", *calls)
	}
	if after := snapshot(t, lab); after != before {
		t.Fatal("refused requests changed the Lab")
	}
	f := closedFetcher()
	demo, _ := syncHandler(t, Options{Demo: true}, f)
	demoCalls := countRetire(demo, nil)
	refusalOf(t, do(t, demo, authed("/api/retire-closed")), http.StatusNotFound, "sample")
	refusalOf(t, do(t, demo, gatePost("/api/retire-closed", closedBody(v))), http.StatusNotFound, "sample")
	if *demoCalls != 0 || f.calls() != 0 {
		t.Fatal("sample data reached the engine or GitHub")
	}
}
