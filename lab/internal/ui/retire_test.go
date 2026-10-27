package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// countRetire replaces the retire seam with one that counts and refuses.
func countRetire(h http.Handler, err error) *int {
	n := new(int)
	h.(*handler).retire = func(lab, id, reason string) (molecule.Record, error) {
		*n++
		return molecule.Record{}, err
	}
	return n
}

// The retire route retires a review-waiting Molecule through the real engine and
// reports committed only from the ledger read-back; the Molecule then sits in the
// Retired lane, out of All and Needs my decision, and analytics still counts it.
func TestUIRetireCommitsOnlyFromLedgerReadBack(t *testing.T) {
	lab, r := labFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	var list MoleculeList
	decode(t, do(t, h, authed("/api/molecules")), &list)
	if list.Counts["all"] != 1 || list.Counts[LaneDecision] != 1 || list.Counts[LaneRetired] != 0 {
		t.Fatalf("before: %v", list.Counts)
	}
	w := do(t, h, gatePost("/api/molecules/"+r.ID+"/retire", `{"reason":"superseded by a later Molecule"}`))
	var v RetireView
	decode(t, w, &v)
	if !v.Committed || v.MoleculeID != r.ID || v.RetiredAt == "" || v.Canonical.Status != molecule.StatusRetired || v.Canonical.Lane != LaneRetired || v.Canonical.Unreadable {
		t.Fatalf("retire view %+v", v)
	}
	if strings.Contains(w.Body.String(), lab) {
		t.Fatalf("response names the Lab path: %s", w.Body)
	}
	var d Detail
	decode(t, do(t, h, authed("/api/molecules/"+r.ID)), &d)
	if d.Lane != LaneRetired || d.Gate.State != GateNone || d.Next.Title != "Retired" || d.Retired == nil || d.Retired.Reason != "superseded by a later Molecule" || d.Retired.PriorStatus != "waiting" {
		t.Fatalf("detail lane %s gate %+v next %+v retired %+v", d.Lane, d.Gate, d.Next, d.Retired)
	}
	list = MoleculeList{}
	decode(t, do(t, h, authed("/api/molecules")), &list)
	if len(list.Molecules) != 1 || list.Molecules[0].Lane != LaneRetired || list.Counts["all"] != 0 || list.Counts[LaneDecision] != 0 || list.Counts[LaneRetired] != 1 {
		t.Fatalf("after: %v %+v", list.Counts, list.Molecules)
	}
	var a AnalyticsView
	decode(t, do(t, h, authed("/api/analytics")), &a)
	if a.Molecules != 1 || a.Counts[LaneRetired] != 1 || a.Counts["all"] != 0 || a.Time.WallClock.Molecules != 1 {
		t.Fatalf("analytics %d %v %+v", a.Molecules, a.Counts, a.Time.WallClock)
	}
	// A second retire is refused with a fixed category and reason.
	again := do(t, h, gatePost("/api/molecules/"+r.ID+"/retire", `{"reason":"again"}`))
	var refusal gateRefusal
	if again.Code != http.StatusConflict || json.Unmarshal(again.Body.Bytes(), &refusal) != nil || refusal.Category != "retired" || refusal.Reason == "" {
		t.Fatalf("second retire %d %s", again.Code, again.Body)
	}
}

// A retire POST lacking any part of the write protection, with a bad body, or in
// demo mode never reaches the engine and writes nothing.
func TestUIRetireGuards(t *testing.T) {
	lab, r := labFixture(t)
	before := snapshot(t, lab)
	h := testHandler(t, Options{LabDir: lab})
	calls := countRetire(h, nil)
	path := "/api/molecules/" + r.ID + "/retire"
	for name, forged := range forgedWrites(path, `{"reason":"stale"}`) {
		if w := do(t, h, forged); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	bodies := map[string]string{
		"unknown field": `{"reason":"stale","bulk":true}`,
		"two objects":   `{"reason":"stale"}{}`,
		"oversized":     `{"reason":"` + strings.Repeat("a", 5000) + `"}`,
		"empty reason":  `{"reason":""}`,
		"controls only": `{"reason":"\u0007\u0001"}`,
		"multi-line":    `{"reason":"one\ntwo"}`,
		"too long":      `{"reason":"` + strings.Repeat("é", molecule.MaxRetireReason+1) + `"}`,
		"not JSON":      `reason=stale`,
	}
	for name, body := range bodies {
		w := do(t, h, gatePost(path, body))
		var refusal gateRefusal
		if w.Code != http.StatusBadRequest || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Category != "invalid" || refusal.Reason == "" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if w := do(t, h, gatePost("/api/molecules/bad.id/retire", `{"reason":"stale"}`)); w.Code != http.StatusBadRequest {
		t.Errorf("invalid id: %d", w.Code)
	}
	if w := do(t, h, gatePost("/api/molecules/"+r.ID+"/x/retire", `{"reason":"stale"}`)); w.Code == http.StatusOK {
		t.Errorf("nested path accepted: %d", w.Code)
	}
	demo := testHandler(t, Options{Demo: true})
	demoCalls := countRetire(demo, nil)
	w := do(t, demo, gatePost(path, `{"reason":"stale"}`))
	var refusal gateRefusal
	if w.Code != http.StatusNotFound || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Category != "sample" {
		t.Errorf("demo: %d %s", w.Code, w.Body)
	}
	if *calls != 0 || *demoCalls != 0 {
		t.Fatalf("refused retires reached the engine %d and %d times", *calls, *demoCalls)
	}
	if after := snapshot(t, lab); after != before {
		t.Fatalf("a refused retire changed the Lab:\n%s\n---\n%s", before, after)
	}
}

// Every engine refusal is served as its fixed category, message and reason; the
// engine text, which could name local paths, is never served, and a refusal is
// never committed.
func TestUIRetireRefusalsShowFixedReasons(t *testing.T) {
	lab, r := labFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	leak := " at /Users/someone/secret/lab"
	cases := map[error]struct {
		code     int
		category string
	}{
		molecule.ErrRetireActive:   {http.StatusConflict, "active"},
		molecule.ErrRetireLocked:   {http.StatusConflict, "locked"},
		molecule.ErrRetireRetired:  {http.StatusConflict, "retired"},
		molecule.ErrRetireDelivery: {http.StatusConflict, "delivery"},
		molecule.ErrRetireMissing:  {http.StatusNotFound, "missing"},
		molecule.ErrRetireLedger:   {http.StatusUnprocessableEntity, "refused"},
		molecule.ErrRetireWrite:    {http.StatusInternalServerError, "write-failed"},
		errors.New("other" + leak): {http.StatusUnprocessableEntity, "refused"},
	}
	for sentinel, want := range cases {
		countRetire(h, fmt.Errorf("%w%s", sentinel, leak))
		w := do(t, h, gatePost("/api/molecules/"+r.ID+"/retire", `{"reason":"stale"}`))
		var refusal gateRefusal
		if w.Code != want.code || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Category != want.category || refusal.Error == "" || refusal.Reason == "" {
			t.Errorf("%v: %d %s", sentinel, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "/Users/") || strings.Contains(w.Body.String(), `"committed":true`) {
			t.Errorf("%v leaked or committed: %s", sentinel, w.Body)
		}
	}
	// A refused engine call writes nothing, so the read-back stays in its lane.
	var d Detail
	decode(t, do(t, h, authed("/api/molecules/"+r.ID)), &d)
	if d.Lane != LaneDecision || d.Retired != nil {
		t.Fatalf("detail after refusals: %s %+v", d.Lane, d.Retired)
	}
}

// A retire that the engine accepts but the ledger does not record is never shown
// as committed.
func TestUIRetireWithoutLedgerEventIsNotCommitted(t *testing.T) {
	lab, r := labFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	calls := countRetire(h, nil)
	var v RetireView
	decode(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/retire", `{"reason":"stale"}`)), &v)
	if *calls != 1 || v.Committed || v.Canonical.Lane != LaneDecision {
		t.Fatalf("calls %d view %+v", *calls, v)
	}
}

// Approving the old digest of a retired scope-waiting Molecule is refused with
// the retired category before any job starts, and writes nothing.
func TestUIScopeApprovalRefusesARetiredMolecule(t *testing.T) {
	lab, r, digest := gateFixture(t)
	retired := r
	retired.Status = molecule.StatusRetired
	retired.Retired = &molecule.Retirement{At: "2026-10-26T10:00:00Z", Reason: "superseded", PriorStatus: r.Status}
	writeRecord(t, lab, retired)
	before := snapshot(t, lab)
	h := testHandler(t, Options{LabDir: lab})
	engine := refuseEngine(t, h)
	w := do(t, h, gatePost("/api/molecules/"+r.ID+"/scope", `{"digest":"`+digest+`"}`))
	var refusal gateRefusal
	if w.Code != http.StatusConflict || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Category != "retired" || refusal.Reason == "" {
		t.Fatalf("approve retired: %d %s", w.Code, w.Body)
	}
	if v := jobOf(t, do(t, h, authed("/api/molecules/"+r.ID+"/job")), http.StatusOK); v.State != "none" {
		t.Fatalf("a job started for a retired Molecule: %+v", v)
	}
	if engine.calls() != 0 {
		t.Fatal("approving a retired Molecule reached the engine")
	}
	if after := snapshot(t, lab); after != before {
		t.Fatalf("approving a retired Molecule changed the Lab:\n%s\n---\n%s", before, after)
	}
}

// The real Lab lists retire among its actions, with or without a source
// checkout; sample data lists no actions at all.
func TestUIRetireAction(t *testing.T) {
	for _, source := range []string{"", "/src/checkout"} {
		var lab LabView
		decode(t, do(t, testHandler(t, Options{LabDir: t.TempDir(), SourceRoot: source}), authed("/api/lab")), &lab)
		if !slices.Contains(lab.Actions, RetireAction) {
			t.Errorf("source %q: actions %v", source, lab.Actions)
		}
	}
	if !slices.Contains(DriveActions, RetireAction) {
		t.Errorf("drive actions %v", DriveActions)
	}
	if id, action, ok := driveRoute("/api/molecules/mol-1/retire"); !ok || id != "mol-1" || action != "retire" {
		t.Errorf("route %q %q %t", id, action, ok)
	}
	// There is no bulk form: only one Molecule id routes.
	for _, path := range []string{"/api/molecules/retire", "/api/molecules/a/b/retire", "/api/molecules/all/retire/x", "/api/retire"} {
		if _, _, ok := driveRoute(path); ok {
			t.Errorf("%s routed", path)
		}
	}
}

// The served script has the Retired lane and filter, keeps retired Molecules out
// of All and the headline counts, and posts the retire reason as JSON.
func TestUIServedJSRetire(t *testing.T) {
	js := servedJS(t)
	for _, want := range []string{
		"['retired', 'Retired']",
		"state.filter === 'all' ? live(m) : m.lane === state.filter",
		"const all = molecules().filter(live);",
		"submitGate('/retire', JSON.stringify({ reason: reason }))",
		"gate.retire.committed ?",
		"hasAction('retire Molecule')",
		"Object.entries(ALL_LANES)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}
