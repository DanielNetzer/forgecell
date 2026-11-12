package ui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// inboxFindingsBase is the Inbox entry inboxView alone gives the Molecule.
func inboxFindingsBase(lab string, r molecule.Record) InboxView {
	x := newRedactor(lab, r)
	d, _ := loadDelivery(delivery.ReadReceiptStatus, lab, r)
	lane := Lane(r.Status, phase(r), d)
	return inboxView(x, r, lane, Activity(lane, molecule.QueuedForChecks(r)), d)
}

func inboxFindingsJSON(t *testing.T, v InboxView) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestInboxFindingsKindsAndTheirPrecedence(t *testing.T) {
	a := findingsTestFinding("a", "high", "src/a.go", "Nil deref")
	b := findingsTestFinding("b", "low", "docs/x.md", "Typo")
	open := func(t *testing.T, lab, id string) {
		findingsTestWrite(t, lab, id, nil, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true, a, b))
	}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, lab string, r *molecule.Record)
		kind  string
	}{
		{"open findings", func(t *testing.T, lab string, r *molecule.Record) { open(t, lab, r.ID) }, InboxFindings},
		{"verified fix round needing confirmation", func(t *testing.T, lab string, r *molecule.Record) {
			open(t, lab, r.ID)
			findingsTestPending(r, 1, findingsTestCommit, findingsTestTree, true, true, b.Fingerprint)
		}, InboxFixRound},
		{"fix round before findings even without findings", func(t *testing.T, lab string, r *molecule.Record) {
			findingsTestPending(r, 1, findingsTestCommit, findingsTestTree, true, true)
		}, InboxFixRound},
		{"verified fix round needing no confirmation", func(t *testing.T, lab string, r *molecule.Record) {
			open(t, lab, r.ID)
			findingsTestPending(r, 1, findingsTestCommit, findingsTestTree, true, false, b.Fingerprint)
		}, InboxRecorded},
		{"findings pending in a running fix round", func(t *testing.T, lab string, r *molecule.Record) {
			open(t, lab, r.ID)
			findingsTestPending(r, 1, findingsTestCommit, findingsTestTree, false, true, a.Fingerprint)
		}, InboxRecorded},
		{"fix round waiting for scope approval", func(t *testing.T, lab string, r *molecule.Record) {
			open(t, lab, r.ID)
			findingsTestPending(r, 1, findingsTestCommit, findingsTestTree, false, true, a.Fingerprint)
			r.Readiness.Phase = "scope-waiting"
		}, InboxScope},
		{"blocked", func(t *testing.T, lab string, r *molecule.Record) {
			open(t, lab, r.ID)
			findingsTestPending(r, 1, findingsTestCommit, findingsTestTree, true, true, b.Fingerprint)
			r.Status = "blocked"
		}, InboxBlocked},
		{"retired", func(t *testing.T, lab string, r *molecule.Record) {
			open(t, lab, r.ID)
			r.Status = molecule.StatusRetired
		}, ""},
		{"findings of an older commit", func(t *testing.T, lab string, r *molecule.Record) {
			findingsTestWrite(t, lab, r.ID, nil, findingsTestRound(strings.Repeat("6", 40), delivery.FindingsComplete, true, a))
		}, InboxRecorded},
		{"every finding dismissed", func(t *testing.T, lab string, r *molecule.Record) {
			findingsTestWrite(t, lab, r.ID, []delivery.Dismissal{{Fingerprint: a.Fingerprint, Reviewer: "0:critic", Reason: "noise", At: "2026-11-01T11:40:00Z"}}, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true, a))
		}, InboxRecorded},
		{"no complete round", func(t *testing.T, lab string, r *molecule.Record) {
			findingsTestWrite(t, lab, r.ID, nil, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, false, a))
		}, InboxRecorded},
		{"no findings record", func(t *testing.T, lab string, r *molecule.Record) {}, InboxRecorded},
		{"no reviewers in the snapshot", func(t *testing.T, lab string, r *molecule.Record) {
			open(t, lab, r.ID)
			*r = findingsTestRecord(lab, r.ID, false)
		}, InboxRecorded},
		{"merged", func(t *testing.T, lab string, r *molecule.Record) {
			open(t, lab, r.ID)
			raw, err := os.ReadFile(filepath.Join(lab, "deliveries", r.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			merged := strings.Replace(string(raw), `"state":"published"`, `"state":"published","lifecycle":{"state":"MERGED","observedAt":"2026-11-02T10:00:00Z"}`, 1)
			if err := os.WriteFile(filepath.Join(lab, "deliveries", r.ID+".json"), []byte(merged), 0600); err != nil {
				t.Fatal(err)
			}
		}, InboxRecorded},
	} {
		t.Run(c.name, func(t *testing.T) {
			lab := t.TempDir()
			r := findingsTestRecord(lab, findingsTestID, true)
			findingsTestReceipt(t, lab, r.ID)
			c.setup(t, lab, &r)
			got := summarize(lab, r, delivery.ReadReceiptStatus).Inbox
			base := inboxFindingsBase(lab, r)
			if got.Kind != c.kind {
				t.Fatalf("kind %q, want %q: %+v", got.Kind, c.kind, got)
			}
			switch c.kind {
			case InboxFindings:
				if got.Title != "Reviewer findings to triage" || got.Action != "Review findings" || got.Summary != "2 open reviewer findings on the draft." {
					t.Fatalf("findings entry %+v", got)
				}
			case InboxFixRound:
				if got.Title != "Fix round ready to push" || got.Action != "Review & push" || got.Summary != "Fix round 1 is verified and waits for your push confirmation." {
					t.Fatalf("fix-round entry %+v", got)
				}
			default:
				// Every other kind is inboxView's own entry, byte for byte.
				if inboxFindingsJSON(t, got) != inboxFindingsJSON(t, base) {
					t.Fatalf("entry changed:\n%s\n%s", inboxFindingsJSON(t, got), inboxFindingsJSON(t, base))
				}
				return
			}
			if got.Step != base.Step || got.StepLabel != base.StepLabel || got.StepSince != base.StepSince || got.StepNote != base.StepNote {
				t.Fatalf("steps changed: %+v, base %+v", got, base)
			}
		})
	}
}

// Item 4 of #386: a merged or closed change request has no findings or
// fix-round entry, whether its receipt observed it or the entry already
// carries the merge; a live one keeps its entry.
func TestInboxFindingsMergedOrClosedDropsTheEntry(t *testing.T) {
	a := findingsTestFinding("a", "high", "src/a.go", "Nil deref")
	b := findingsTestFinding("b", "low", "docs/x.md", "Typo")
	for _, state := range []string{"MERGED", "CLOSED"} {
		for name, pending := range map[string]bool{"findings": false, "fix round": true} {
			t.Run(state+" "+name, func(t *testing.T) {
				lab := t.TempDir()
				r := findingsTestRecord(lab, findingsTestID, true)
				findingsTestReceipt(t, lab, r.ID)
				findingsTestWrite(t, lab, r.ID, nil, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true, a, b))
				if pending {
					findingsTestPending(&r, 1, findingsTestCommit, findingsTestTree, true, true, b.Fingerprint)
				}
				findingsTestLifecycle(t, lab, r.ID, state)
				got := summarize(lab, r, delivery.ReadReceiptStatus).Inbox
				if got.Kind == InboxFindings || got.Kind == InboxFixRound || inboxFindingsJSON(t, got) != inboxFindingsJSON(t, inboxFindingsBase(lab, r)) {
					t.Fatalf("entry %+v", got)
				}
			})
		}
	}
	// An entry that already carries the merge is left as it is, even over a
	// receipt still observed open.
	lab := t.TempDir()
	r := findingsTestRecord(lab, findingsTestID, true)
	findingsTestReceipt(t, lab, r.ID)
	findingsTestWrite(t, lab, r.ID, nil, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true, a, b))
	v := InboxView{Kind: InboxRecorded, Title: "Merged", Action: "Open", Merged: true}
	inboxFindingsOverlay(lab, r, &v)
	if v.Kind != InboxRecorded || v.Title != "Merged" || !v.Merged {
		t.Fatalf("merged entry %+v", v)
	}
	live := InboxView{Kind: InboxRecorded, Title: "Draft published", Action: "Open"}
	inboxFindingsOverlay(lab, r, &live)
	if live.Kind != InboxFindings {
		t.Fatalf("live entry %+v", live)
	}
	// /api/sync counts a findings entry under Needs you only while GitHub has
	// not seen its pull request merged or closed. The pull request decides,
	// whatever bucket the Issue or the ledger lane files the state under.
	issue := func(state, reason, reopenedAt string) *forgesync.Issue {
		return &forgesync.Issue{Number: 351, Found: true, State: state, StateReason: reason, ReopenedAt: reopenedAt}
	}
	pr := func(state string) map[int64]forgesync.PR {
		p := forgesync.PR{Number: 374, Found: true, State: state}
		if state == forgesync.PRMerged {
			p.MergedAt, p.MergeCommit = "2026-11-01T10:00:00Z", findingsTestCommit
		}
		return map[int64]forgesync.PR{374: p}
	}
	mol := func(lane string) forgesync.Molecule {
		return forgesync.Molecule{ID: findingsTestID, Lane: lane, StartedAt: "2026-11-01T09:00:00Z", PRNumber: 374, PublishedCommit: findingsTestCommit}
	}
	open, closed := forgesync.IssueOpen, forgesync.IssueClosed
	for name, c := range map[string]struct {
		issue *forgesync.Issue
		pr    string
		lane  string
		live  bool
	}{
		"merged":                      {issue(closed, forgesync.ReasonCompleted, ""), forgesync.PRMerged, forgesync.LaneFinished, false},
		"merged, Issue open":          {issue(open, "", ""), forgesync.PRMerged, forgesync.LaneFinished, false},
		"PR closed":                   {issue(open, "", ""), forgesync.PRClosed, forgesync.LaneFinished, false},
		"not planned, PR merged":      {issue(closed, forgesync.ReasonNotPlanned, ""), forgesync.PRMerged, forgesync.LaneFinished, false},
		"closed elsewhere, PR closed": {issue(closed, forgesync.ReasonCompleted, ""), forgesync.PRClosed, forgesync.LaneFinished, false},
		"reopened after merge":        {issue(open, forgesync.ReasonReopened, "2026-11-02T10:00:00Z"), forgesync.PRMerged, forgesync.LaneFinished, false},
		"live ledger lane, PR merged": {issue(open, "", ""), forgesync.PRMerged, forgesync.LaneDecision, false},
		"draft PR open":               {issue(open, "", ""), forgesync.PROpen, forgesync.LaneFinished, true},
		"open PR, Issue closed":       {issue(closed, forgesync.ReasonCompleted, ""), forgesync.PROpen, forgesync.LaneFinished, true},
		"PR not checked":              {issue(open, "", ""), "", forgesync.LaneFinished, true},
		"GitHub not checked":          {nil, "", forgesync.LaneFinished, true},
		"live ledger lane, PR open":   {issue(open, "", ""), forgesync.PROpen, forgesync.LaneDecision, true},
	} {
		prs := map[int64]forgesync.PR{}
		if c.pr != "" {
			prs = pr(c.pr)
		}
		d := forgesync.ForMolecule(c.issue, prs, mol(c.lane))
		if got := syncFindingsLive(d); got != c.live {
			t.Errorf("%s (%s, bucket %s): findings entry live %v, want %v", name, d.Kind, d.Bucket, got, c.live)
		}
	}
}

// Without reviewers, every existing kind is exactly inboxView's.
func TestInboxFindingsLeavesEveryExistingKindByteIdentical(t *testing.T) {
	cases := map[string]func(t *testing.T) (string, molecule.Record){
		"review gate": func(t *testing.T) (string, molecule.Record) { return labFixture(t) },
		"scope":       func(t *testing.T) (string, molecule.Record) { lab, r, _ := gateFixture(t); return lab, r },
		"blocked": func(t *testing.T) (string, molecule.Record) {
			lab, r := labFixture(t)
			r.Status = "blocked"
			return lab, r
		},
		"running": func(t *testing.T) (string, molecule.Record) {
			lab, r := labFixture(t)
			r.Status, r.Readiness.Phase = "running", "coding"
			return lab, r
		},
		"recorded": func(t *testing.T) (string, molecule.Record) {
			lab, r := labFixture(t)
			writeReceipt(t, lab, r.ID, "published", "OPEN")
			return lab, r
		},
		"merged": func(t *testing.T) (string, molecule.Record) {
			lab, r := labFixture(t)
			writeReceipt(t, lab, r.ID, "published", "MERGED")
			return lab, r
		},
		"retired": func(t *testing.T) (string, molecule.Record) {
			lab, r := labFixture(t)
			r.Status = molecule.StatusRetired
			return lab, r
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			lab, r := setup(t)
			got := summarize(lab, r, delivery.ReadReceiptStatus).Inbox
			if inboxFindingsJSON(t, got) != inboxFindingsJSON(t, inboxFindingsBase(lab, r)) {
				t.Fatalf("entry changed: %+v", got)
			}
		})
	}
}

func TestInboxFindingsServedJSRegistersBothKinds(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	core := do(t, h, authed("/js/core.js"))
	inbox := do(t, h, authed("/js/inbox.js"))
	if core.Code != http.StatusOK || inbox.Code != http.StatusOK {
		t.Fatalf("modules: %d %d", core.Code, inbox.Code)
	}
	for _, want := range []string{"const NEEDS_KINDS = ['scope', 'publish', 'blocked'];", "NEEDS_KINDS.push('fix-round', 'findings');"} {
		if !strings.Contains(core.Body.String(), want) {
			t.Errorf("core.js lacks %q", want)
		}
	}
	for _, want := range []string{"'fix-round': ['is-ready', '✓']", "findings: ['is-needs', '◇']"} {
		if !strings.Contains(inbox.Body.String(), want) {
			t.Errorf("inbox.js lacks %q", want)
		}
	}
	for _, body := range []string{core.Body.String(), inbox.Body.String()} {
		if strings.Contains(body, "innerHTML") {
			t.Error("a module uses innerHTML")
		}
	}
}

func TestInboxFindingsMarkNeedsYouInSync(t *testing.T) {
	lab := t.TempDir()
	a := findingsTestFinding("a", "high", "src/a.go", "Nil deref")
	triage := findingsTestRecord(lab, findingsTestID, true)
	push := findingsTestRecord(lab, "mol-351-abcdef", true)
	quiet := findingsTestRecord(lab, "mol-352-abcdef", true)
	for _, r := range []molecule.Record{triage, push, quiet} {
		findingsTestReceipt(t, lab, r.ID)
	}
	findingsTestWrite(t, lab, triage.ID, nil, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true, a))
	findingsTestPending(&push, 1, findingsTestCommit, findingsTestTree, true, true)
	h, fake := findingsTestHandler(t, lab, "", triage, push, quiet)
	var list MoleculeList
	decode(t, do(t, h, authed("/api/molecules")), &list)
	kinds := map[string]string{}
	for _, m := range list.Molecules {
		kinds[m.ID] = m.Inbox.Kind
	}
	if kinds[triage.ID] != InboxFindings || kinds[push.ID] != InboxFixRound || kinds[quiet.ID] != InboxRecorded {
		t.Fatalf("kinds %v", kinds)
	}
	var s SyncView
	decode(t, do(t, h, authed("/api/sync")), &s)
	if !s.NeedsYou[triage.ID] || !s.NeedsYou[push.ID] || s.NeedsYou[quiet.ID] {
		t.Fatalf("needs you %v", s.NeedsYou)
	}
	if c, p, v, u := fake.counts(); c+p+v+u != 0 {
		t.Fatalf("listing called the engine: %d %d %d %d", c, p, v, u)
	}
}

// A findings record written outside the UI (the CLI's --collect or dismiss)
// changes the cached Summary's key, so the listed Inbox entry follows it; with
// no findings record the Summary stays cached as before.
func TestInboxFindingsRecordWrittenOutsideTheUIChangesTheCachedEntry(t *testing.T) {
	lab := t.TempDir()
	r := findingsTestRecord(lab, findingsTestID, true)
	findingsTestReceipt(t, lab, r.ID)
	h, _ := findingsTestHandler(t, lab, "", r)
	hh := h.(*handler)
	age(t, filepath.Join(lab, "ledgers", r.ID+".json"), time.Hour)
	age(t, filepath.Join(lab, "deliveries", r.ID+".json"), time.Hour)
	reads := 0
	records := hh.records
	hh.records = func(lab, id string) (molecule.Record, error) {
		reads++
		return records(lab, id)
	}
	kind := func() string {
		t.Helper()
		var list MoleculeList
		decode(t, do(t, h, authed("/api/molecules")), &list)
		if len(list.Molecules) != 1 {
			t.Fatalf("molecules %+v", list.Molecules)
		}
		return list.Molecules[0].Inbox.Kind
	}
	if got := kind(); got != InboxRecorded {
		t.Fatalf("kind %q before any findings", got)
	}
	if got := kind(); got != InboxRecorded || reads != 1 {
		t.Fatalf("kind %q after %d reads: the Summary without a findings record was not cached", got, reads)
	}
	a := findingsTestFinding("a", "high", "src/a.go", "Nil deref")
	findingsTestWrite(t, lab, r.ID, nil, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true, a))
	age(t, filepath.Join(lab, "deliveries", r.ID+".findings.json"), time.Hour)
	if got := kind(); got != InboxFindings || reads != 2 {
		t.Fatalf("kind %q after %d reads: the findings record did not refresh the Summary", got, reads)
	}
	if got := kind(); got != InboxFindings || reads != 2 {
		t.Fatalf("kind %q after %d reads: the Summary with a findings record was not cached", got, reads)
	}
	findingsTestWrite(t, lab, r.ID, []delivery.Dismissal{{Fingerprint: a.Fingerprint, Reviewer: "0:critic", Reason: "noise", At: "2026-11-01T11:40:00Z"}}, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true, a))
	age(t, filepath.Join(lab, "deliveries", r.ID+".findings.json"), 30*time.Minute)
	if got := kind(); got != InboxRecorded || reads != 3 {
		t.Fatalf("kind %q after %d reads: the dismissal did not refresh the Summary", got, reads)
	}
}

func TestInboxFindingsWritesDropTheCachedSummary(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()}).(*handler)
	h.cache.replace(map[string]cachedSummary{findingsTestID: {summary: Summary{ID: findingsTestID}}, "mol-1-abcdef": {summary: Summary{ID: "mol-1-abcdef"}}})
	h.findingsForget(findingsTestID)
	if _, ok := h.cache.entries[findingsTestID]; ok {
		t.Fatal("the cached Summary was kept")
	}
	if _, ok := h.cache.entries["mol-1-abcdef"]; !ok {
		t.Fatal("another Molecule's Summary was dropped")
	}
}
