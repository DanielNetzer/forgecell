package ui

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// actionSeams replaces the engine calls behind the next actions: the derived
// actions are fixed, and recover and amend record each call in order and write
// the ledger as the engine would, so the read-back can see it. No git, harness
// or check process starts.
type actionSeams struct {
	mu         sync.Mutex
	offered    []molecule.NextAction
	previews   []bool
	log        []string
	amends     []molecule.AmendRequest
	recovers   []bool
	recoverErr error
	amendErr   error
}

func (s *actionSeams) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

func installActions(t *testing.T, h http.Handler, s *actionSeams) *actionSeams {
	t.Helper()
	hh := h.(*handler)
	hh.deriveActions = func(_ context.Context, _ string, _ molecule.Record, preview bool, _ time.Duration) []molecule.NextAction {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.previews = append(s.previews, preview)
		return append([]molecule.NextAction(nil), s.offered...)
	}
	hh.recover = func(_ context.Context, lab, id, digest string, confirmed bool) (molecule.Record, error) {
		s.mu.Lock()
		s.log, s.recovers = append(s.log, "recover"), append(s.recovers, confirmed)
		err := s.recoverErr
		s.mu.Unlock()
		if err != nil {
			return molecule.Record{}, err
		}
		r, err := molecule.ReadRecord(lab, id)
		if err != nil {
			return r, err
		}
		r.Status, r.Readiness.Phase = "blocked", "interrupted"
		return r, saveRecord(lab, r)
	}
	hh.amend = func(_ context.Context, lab, id string, req molecule.AmendRequest) (molecule.Record, error) {
		s.mu.Lock()
		s.log, s.amends = append(s.log, "amend"), append(s.amends, req)
		err := s.amendErr
		s.mu.Unlock()
		if err != nil {
			return molecule.Record{}, err
		}
		r, err := molecule.ReadRecord(lab, id)
		if err != nil {
			return r, err
		}
		latest := r.Readiness.Plans[len(r.Readiness.Plans)-1]
		if req.ExpectParent != latest.Digest {
			return r, molecule.ErrStalePlan
		}
		next := latest.Plan
		next.Revision, next.ParentDigest = latest.Plan.Revision+1, latest.Digest
		if req.Continuation != "" || req.AcceptProtectedFailures {
			next.Continuation = "checks-only"
		}
		r.Readiness.Plans = append(r.Readiness.Plans, readiness.Proposal{Plan: next, Digest: successorDigest})
		r.Status, r.Readiness.Phase = "waiting", "scope-waiting"
		return r, saveRecord(lab, r)
	}
	return s
}

// blockedFixture is gateFixture after a failed verification: blocked, with its
// one plan as the parent the actions bind.
func blockedFixture(t *testing.T) (string, molecule.Record, string) {
	t.Helper()
	lab, r, digest := gateFixture(t)
	r.Status, r.Readiness.Phase = "failed", "failed"
	writeRecord(t, lab, r)
	return lab, r, digest
}

func actionBody(digest string) string { return `{"digest":"` + digest + `"}` }

// testDeltas are the tests the accept-failures fixtures offer; acceptBody binds
// them by their DeltasDigest, as the served page does.
var testDeltas = []readiness.ProtectedDelta{{CheckID: "go-test-lab", Package: "example.test/lab/pin", Test: "TestX", ScopePath: "lab/pin/pin_test.go"}}

func acceptBody(digest string, deltas []readiness.ProtectedDelta) string {
	return `{"digest":"` + digest + `","deltas":"` + DeltasDigest(deltas) + `"}`
}

func acceptOffer(digest string) molecule.NextAction {
	return molecule.NextAction{Kind: molecule.NextAcceptProtectedFailures, Digest: digest, Deltas: testDeltas}
}

func actionPath(id, kind string) string { return "/api/molecules/" + id + "/actions/" + kind }

// The engine wiring: the actions are molecule.NextActions and recover is
// molecule.Recover, and /api/lab lists both new actions with Request changes last.
func TestUIActionsAreWiredToTheEngine(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()}).(*handler)
	if reflect.ValueOf(h.deriveActions).Pointer() != reflect.ValueOf(molecule.NextActions).Pointer() || reflect.ValueOf(h.recover).Pointer() != reflect.ValueOf(molecule.Recover).Pointer() {
		t.Fatal("the next actions are not wired to molecule.NextActions and molecule.Recover")
	}
	for _, source := range []string{"", "/src/checkout"} {
		lab := viewOf[LabView](t, do(t, testHandler(t, Options{LabDir: t.TempDir(), SourceRoot: source}), authed("/api/lab")), http.StatusOK)
		if !slices.Contains(lab.Actions, UnblockAction) || !slices.Contains(lab.Actions, BulkRetireAction) || lab.Actions[len(lab.Actions)-1] != AmendAction {
			t.Errorf("source %q: actions %v", source, lab.Actions)
		}
	}
	for path, want := range map[string][2]string{
		"/api/molecules/mol-1/actions/accept-failures": {"mol-1", ActionAcceptFailures},
		"/api/molecules/mol-1/actions/scope-change":    {"mol-1", ActionScopeChange},
		"/api/molecules/mol-1/actions/rerun-checks":    {"mol-1", ActionRerunChecks},
		"/api/molecules/mol-1/actions/recover":         {"mol-1", ActionRecover},
	} {
		if id, kind, ok := actionRoute(path); !ok || id != want[0] || kind != want[1] {
			t.Errorf("%s routed to %q %q %t", path, id, kind, ok)
		}
	}
	for _, path := range []string{"/api/molecules/mol-1/actions/approve", "/api/molecules/mol-1/actions/retire", "/api/molecules/mol-1/actions/fresh-intake", "/api/molecules/a/b/actions/recover", "/api/molecules/mol-1/actions/", "/api/molecules/mol-1/actions"} {
		if _, _, ok := actionRoute(path); ok {
			t.Errorf("%s routed", path)
		}
	}
}

// GET /actions lists exactly what the engine offers, with its digest, the exact
// tests accept-failures declares, the scope paths and reason, the stopped
// confirmation where recovery runs, and a fresh intake only with a source
// checkout. It previews; nothing is written.
func TestUIActionsViewListsTheEngineActions(t *testing.T) {
	lab, r, digest := blockedFixture(t)
	r.Analysis = &readiness.Analysis{}
	writeRecord(t, lab, r)
	offered := []molecule.NextAction{
		acceptOffer(digest),
		{Kind: molecule.NextApproveScopeChange, Digest: digest, Paths: []string{"lab/x/new.go"}, Reason: "helper needed"},
		{Kind: molecule.NextRerunChecks, Digest: digest, RecoverFirst: true},
		{Kind: molecule.NextRecover, Digest: digest},
		{Kind: molecule.NextFreshIntake, Questions: []string{"Which flag?"}},
		{Kind: molecule.NextRetire},
	}
	for _, source := range []string{"", "/src/checkout"} {
		h := testHandler(t, Options{LabDir: lab, SourceRoot: source})
		s := installActions(t, h, &actionSeams{offered: offered})
		before := snapshot(t, lab)
		v := viewOf[ActionsView](t, do(t, h, authed("/api/molecules/"+r.ID+"/actions")), http.StatusOK)
		want := []string{ActionAcceptFailures, ActionScopeChange, ActionRerunChecks, ActionRecover, ActionRetire}
		if source != "" {
			want = []string{ActionAcceptFailures, ActionScopeChange, ActionRerunChecks, ActionRecover, ActionFreshIntake, ActionRetire}
		}
		var kinds []string
		for _, a := range v.Actions {
			kinds = append(kinds, a.Kind)
			if a.Label == "" || a.Copy == "" || strings.Contains(a.Copy, "CLI") {
				t.Errorf("%s: label %q copy %q", a.Kind, a.Label, a.Copy)
			}
		}
		if !reflect.DeepEqual(kinds, want) || v.MoleculeID != r.ID || v.Blocking == "" || v.NotOffered == nil || len(v.NotOffered) != 0 {
			t.Fatalf("source %q: kinds %v blocking %q not offered %v", source, kinds, v.Blocking, v.NotOffered)
		}
		accept, scope, rerun, recover := v.Actions[0], v.Actions[1], v.Actions[2], v.Actions[3]
		if accept.Digest != digest || accept.DeltasDigest != DeltasDigest(testDeltas) || !reflect.DeepEqual(accept.Deltas, []DeltaView{{Package: "example.test/lab/pin", Test: "TestX", Check: "go-test-lab", File: "lab/pin/pin_test.go"}}) || accept.ConfirmStopped {
			t.Errorf("accept %+v", accept)
		}
		if !reflect.DeepEqual(scope.Paths, []string{"lab/x/new.go"}) || scope.Reason != "helper needed" || scope.ConfirmStopped {
			t.Errorf("scope %+v", scope)
		}
		if !rerun.RecoverFirst || !rerun.ConfirmStopped || !recover.ConfirmStopped {
			t.Errorf("rerun %+v recover %+v", rerun, recover)
		}
		if source != "" {
			if fresh := v.Actions[4]; fresh.Issue != r.Issue.Number || fresh.IssueURL != r.Issue.URL || !reflect.DeepEqual(fresh.Questions, []string{"Which flag?"}) || fresh.Digest != "" {
				t.Errorf("fresh %+v", fresh)
			}
		}
		if !reflect.DeepEqual(s.previews, []bool{true}) || len(s.calls()) != 0 {
			t.Fatalf("previews %v calls %v", s.previews, s.calls())
		}
		if after := snapshot(t, lab); after != before {
			t.Fatal("reading the actions changed the Lab")
		}
	}
	// Sample data has no actions view.
	if w := do(t, testHandler(t, Options{Demo: true}), authed("/api/molecules/demo-101/actions")); w.Code != http.StatusNotFound {
		t.Fatalf("demo actions view: %d", w.Code)
	}
}

// Accept by-design failures and Approve the requested scope change each send the
// CLI's engine request with the shown digest, never approve, and show committed
// only from the read-back of a successor of exactly that digest.
func TestUIActionsRecordTheRevisionWithoutApproving(t *testing.T) {
	for _, c := range []struct {
		kind, body string
		offered    molecule.NextAction
		want       molecule.AmendRequest
	}{
		{ActionAcceptFailures, `,"deltas":"` + DeltasDigest(testDeltas) + `"`, molecule.NextAction{Kind: molecule.NextAcceptProtectedFailures, Deltas: testDeltas}, molecule.AmendRequest{AcceptProtectedFailures: true}},
		{ActionScopeChange, `,"paths":["lab/x/new.go"],"reason":"Edited by the operator"`, molecule.NextAction{Kind: molecule.NextApproveScopeChange, Paths: []string{"lab/x/new.go"}, Reason: "helper needed"}, molecule.AmendRequest{AddScope: []molecule.ScopeAddition{{Path: "lab/x/new.go", Reason: "Edited by the operator"}}}},
		{ActionRerunChecks, "", molecule.NextAction{Kind: molecule.NextRerunChecks}, molecule.AmendRequest{Continuation: "checks-only"}},
	} {
		t.Run(c.kind, func(t *testing.T) {
			lab, r, digest := blockedFixture(t)
			h := testHandler(t, Options{LabDir: lab})
			engine := refuseEngine(t, h)
			c.offered.Digest = digest
			s := installActions(t, h, &actionSeams{offered: []molecule.NextAction{c.offered}})
			body := `{"digest":"` + digest + `"` + c.body + `}`
			v := viewOf[ActionResultView](t, do(t, h, gatePost(actionPath(r.ID, c.kind), body)), http.StatusOK)
			c.want.ExpectParent = digest
			if !reflect.DeepEqual(s.calls(), []string{"amend"}) || !reflect.DeepEqual(s.amends[0], c.want) {
				t.Fatalf("engine calls %v %+v, want %+v", s.calls(), s.amends, c.want)
			}
			if !v.Committed || v.Recovered || v.Revision != 2 || v.Digest != successorDigest || v.Kind != c.kind || v.Canonical.Phase != "scope-waiting" {
				t.Fatalf("read-back %+v", v)
			}
			// Only accept-failures derives its tests again, to bind the list shown.
			if !reflect.DeepEqual(s.previews, []bool{c.kind == ActionAcceptFailures}) {
				t.Fatalf("previews %v", s.previews)
			}
			if scope, _, _ := engine.counts(); scope != 0 || h.(*handler).jobs.busy(r.ID) {
				t.Fatal("an action approved or started a job")
			}
			// The successor waits for its own approval through the scope gate.
			if d := detailOf(t, h, r.ID); d.View.Decision.Primary == nil || d.View.Decision.Primary.Digest != successorDigest {
				t.Fatalf("after the action the decision is %+v", d.View.Decision)
			}
		})
	}
	// An engine call that records nothing is never shown as committed.
	lab, r, digest := blockedFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	s := installActions(t, h, &actionSeams{offered: []molecule.NextAction{acceptOffer(digest)}})
	h.(*handler).amend = func(context.Context, string, string, molecule.AmendRequest) (molecule.Record, error) {
		return molecule.Record{}, nil
	}
	if v := viewOf[ActionResultView](t, do(t, h, gatePost(actionPath(r.ID, ActionAcceptFailures), acceptBody(digest, testDeltas))), http.StatusOK); v.Committed || v.Digest != "" {
		t.Fatalf("nothing read back, yet %+v", v)
	}
	if len(s.calls()) != 0 {
		t.Fatalf("calls %v", s.calls())
	}
}

// Re-run checks on a resource-refused check-pending Molecule recovers first, as
// forgecell recover --confirm-stopped does, then records the checks-only
// revision; it needs the stopped confirmation. Recover alone records the
// interruption and is committed only when the ledger reads back interrupted.
func TestUIActionsRecoverFirstThenChecksOnly(t *testing.T) {
	lab, r, digest := gateFixture(t)
	r.Status, r.Readiness.Phase = "blocked", "check-pending"
	writeRecord(t, lab, r)
	h := testHandler(t, Options{LabDir: lab})
	s := installActions(t, h, &actionSeams{offered: []molecule.NextAction{{Kind: molecule.NextRerunChecks, Digest: digest, RecoverFirst: true}}})
	before := snapshot(t, lab)
	refusalOf(t, do(t, h, gatePost(actionPath(r.ID, ActionRerunChecks), actionBody(digest))), http.StatusBadRequest, "confirm-stopped")
	if len(s.calls()) != 0 || snapshot(t, lab) != before {
		t.Fatalf("an unconfirmed recovery reached the engine: %v", s.calls())
	}
	v := viewOf[ActionResultView](t, do(t, h, gatePost(actionPath(r.ID, ActionRerunChecks), `{"digest":"`+digest+`","confirmStopped":true}`)), http.StatusOK)
	if !reflect.DeepEqual(s.calls(), []string{"recover", "amend"}) || !reflect.DeepEqual(s.recovers, []bool{true}) {
		t.Fatalf("engine calls %v", s.calls())
	}
	if want := (molecule.AmendRequest{Continuation: "checks-only", ExpectParent: digest}); !reflect.DeepEqual(s.amends[0], want) {
		t.Fatalf("amend %+v", s.amends[0])
	}
	if !v.Recovered || !v.Committed || v.Digest != successorDigest {
		t.Fatalf("read-back %+v", v)
	}

	// Recover alone.
	lab2, r2, digest2 := gateFixture(t)
	r2.Status = "running"
	writeRecord(t, lab2, r2)
	h2 := testHandler(t, Options{LabDir: lab2})
	s2 := installActions(t, h2, &actionSeams{offered: []molecule.NextAction{{Kind: molecule.NextRecover, Digest: digest2}}})
	refusalOf(t, do(t, h2, gatePost(actionPath(r2.ID, ActionRecover), actionBody(digest2))), http.StatusBadRequest, "confirm-stopped")
	v = viewOf[ActionResultView](t, do(t, h2, gatePost(actionPath(r2.ID, ActionRecover), `{"digest":"`+digest2+`","confirmStopped":true}`)), http.StatusOK)
	if !reflect.DeepEqual(s2.calls(), []string{"recover"}) || !v.Committed || !v.Recovered || v.Digest != "" {
		t.Fatalf("recover calls %v view %+v", s2.calls(), v)
	}
	// A refused recovery stops before the amend and is served as a fixed refusal.
	lab3, r3, digest3 := gateFixture(t)
	r3.Status, r3.Readiness.Phase = "blocked", "check-pending"
	writeRecord(t, lab3, r3)
	h3 := testHandler(t, Options{LabDir: lab3})
	leak := errors.New("Molecule is locked at " + lab3)
	s3 := installActions(t, h3, &actionSeams{offered: []molecule.NextAction{{Kind: molecule.NextRerunChecks, Digest: digest3, RecoverFirst: true}}, recoverErr: leak})
	w := do(t, h3, gatePost(actionPath(r3.ID, ActionRerunChecks), `{"digest":"`+digest3+`","confirmStopped":true}`))
	refusalOf(t, w, http.StatusConflict, "locked")
	if !reflect.DeepEqual(s3.calls(), []string{"recover"}) || strings.Contains(w.Body.String(), lab3) {
		t.Fatalf("calls %v body %s", s3.calls(), w.Body)
	}
}

// Every action route has the gate write protection and a strict body; an action
// the engine would refuse, a stale digest or changed scope paths, a retired
// Molecule and sample data never reach the engine, and nothing changes.
func TestUIActionsRefusalsNeverReachTheEngine(t *testing.T) {
	lab, r, digest := blockedFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	s := installActions(t, h, &actionSeams{offered: []molecule.NextAction{
		acceptOffer(digest),
		{Kind: molecule.NextApproveScopeChange, Digest: digest, Paths: []string{"lab/x/new.go"}},
		{Kind: molecule.NextRerunChecks, Digest: digest},
		{Kind: molecule.NextRecover, Digest: digest},
	}})
	engine := refuseEngine(t, h)
	before := snapshot(t, lab)
	scopeBody := `{"digest":"` + digest + `","paths":["lab/x/new.go"],"reason":"r"}`
	bodies := map[string]string{ActionAcceptFailures: acceptBody(digest, testDeltas), ActionScopeChange: scopeBody, ActionRerunChecks: actionBody(digest), ActionRecover: `{"digest":"` + digest + `","confirmStopped":true}`}
	for kind, body := range bodies {
		for name, forged := range forgedWrites(actionPath(r.ID, kind), body) {
			if w := do(t, h, forged); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d %s", kind, name, w.Code, w.Body)
			}
		}
		get := authed(actionPath(r.ID, kind))
		get.header = map[string]string{sessionHeader: theSession}
		if w := do(t, h, get); w.Code == http.StatusOK {
			t.Errorf("GET %s served %d", kind, w.Code)
		}
	}
	for name, c := range map[string]struct{ kind, body string }{
		"approve field":       {ActionAcceptFailures, `{"digest":"` + digest + `","approve":true}`},
		"run field":           {ActionScopeChange, `{"digest":"` + digest + `","paths":["lab/x/new.go"],"reason":"r","run":true}`},
		"short digest":        {ActionAcceptFailures, `{"digest":"abc"}`},
		"no digest":           {ActionRerunChecks, `{}`},
		"accept with paths":   {ActionAcceptFailures, `{"digest":"` + digest + `","paths":["a.go"]}`},
		"accept confirmed":    {ActionAcceptFailures, `{"digest":"` + digest + `","confirmStopped":true}`},
		"recover with reason": {ActionRecover, `{"digest":"` + digest + `","reason":"r","confirmStopped":true}`},
		"scope no paths":      {ActionScopeChange, `{"digest":"` + digest + `","reason":"r"}`},
		"scope no reason":     {ActionScopeChange, `{"digest":"` + digest + `","paths":["lab/x/new.go"],"reason":""}`},
		"scope multi-line":    {ActionScopeChange, `{"digest":"` + digest + `","paths":["lab/x/new.go"],"reason":"a\nb"}`},
		"scope traversal":     {ActionScopeChange, `{"digest":"` + digest + `","paths":["../x.go"],"reason":"r"}`},
		"scope absolute":      {ActionScopeChange, `{"digest":"` + digest + `","paths":["/etc/x"],"reason":"r"}`},
		"scope too many":      {ActionScopeChange, `{"digest":"` + digest + `","paths":["a","b","c","d","e","f","g","h","i"],"reason":"r"}`},
		"trailing data":       {ActionAcceptFailures, acceptBody(digest, testDeltas) + `{}`},
		"oversized":           {ActionAcceptFailures, `{"digest":"` + digest + `"` + strings.Repeat(" ", maxGateBody) + `}`},
		"accept no deltas":    {ActionAcceptFailures, actionBody(digest)},
		"accept short deltas": {ActionAcceptFailures, `{"digest":"` + digest + `","deltas":"abc"}`},
		"rerun with deltas":   {ActionRerunChecks, `{"digest":"` + digest + `","deltas":"` + DeltasDigest(testDeltas) + `"}`},
		"scope with deltas":   {ActionScopeChange, `{"digest":"` + digest + `","deltas":"` + DeltasDigest(testDeltas) + `","paths":["lab/x/new.go"],"reason":"r"}`},
	} {
		if w := do(t, h, gatePost(actionPath(r.ID, c.kind), c.body)); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	// A stale digest, a test list other than the one offered, or scope paths
	// other than the ones offered.
	other := strings.Repeat("0", 64)
	refusalOf(t, do(t, h, gatePost(actionPath(r.ID, ActionAcceptFailures), acceptBody(other, testDeltas))), http.StatusConflict, "stale")
	refusalOf(t, do(t, h, gatePost(actionPath(r.ID, ActionAcceptFailures), `{"digest":"`+digest+`","deltas":"`+other+`"}`)), http.StatusConflict, "stale")
	refusalOf(t, do(t, h, gatePost(actionPath(r.ID, ActionScopeChange), `{"digest":"`+digest+`","paths":["lab/x/other.go"],"reason":"r"}`)), http.StatusConflict, "stale")
	// An unknown action, Molecule or id is not served.
	if w := do(t, h, gatePost("/api/molecules/"+r.ID+"/actions/approve", actionBody(digest))); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("unknown action: %d", w.Code)
	}
	if w := do(t, h, gatePost(actionPath("absent", ActionAcceptFailures), acceptBody(digest, testDeltas))); w.Code != http.StatusNotFound {
		t.Errorf("absent Molecule: %d", w.Code)
	}
	if w := do(t, h, gatePost(actionPath("bad.id", ActionAcceptFailures), acceptBody(digest, testDeltas))); w.Code != http.StatusBadRequest {
		t.Errorf("invalid id: %d", w.Code)
	}
	if after := snapshot(t, lab); after != before {
		t.Fatalf("refused requests changed the Lab:\n%s\n---\n%s", before, after)
	}
	if len(s.calls()) != 0 || engine.calls() != 0 {
		t.Fatalf("refused requests reached the engine: %v, %d gate calls", s.calls(), engine.calls())
	}

	// An action the engine would refuse now is not offered, so it is refused.
	s.mu.Lock()
	s.offered = []molecule.NextAction{{Kind: molecule.NextRetire}}
	s.mu.Unlock()
	for kind, body := range bodies {
		refusalOf(t, do(t, h, gatePost(actionPath(r.ID, kind), body)), http.StatusConflict, "not-offered")
	}
	// A retired Molecule is refused before the engine.
	retired := r
	retired.Status = molecule.StatusRetired
	retired.Retired = &molecule.Retirement{At: "2026-10-26T20:00:00Z", Reason: "stale", PriorStatus: "failed"}
	writeRecord(t, lab, retired)
	if w := do(t, h, gatePost(actionPath(r.ID, ActionAcceptFailures), acceptBody(digest, testDeltas))); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"category":"retired"`) {
		t.Fatalf("retired: %d %s", w.Code, w.Body)
	}
	if len(s.calls()) != 0 {
		t.Fatalf("refused requests reached the engine: %v", s.calls())
	}

	// Sample data refuses every action.
	demo := testHandler(t, Options{Demo: true})
	ds := installActions(t, demo, &actionSeams{offered: []molecule.NextAction{acceptOffer(digest)}})
	for kind, body := range bodies {
		refusalOf(t, do(t, demo, gatePost(actionPath("demo-101", kind), body)), http.StatusNotFound, "sample")
	}
	if len(ds.calls()) != 0 || len(ds.previews) != 0 {
		t.Fatal("sample data reached the engine")
	}
}

// Accept by-design failures is bound to the exact tests shown. A newer
// verification attempt under the same plan digest that failed other tests
// changes the derived list, so the POST carrying the shown list's digest is
// refused as stale before the engine; the list shown now is accepted.
func TestUIActionsAcceptFailuresBindsTheTestListShown(t *testing.T) {
	lab, r, digest := blockedFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	s := installActions(t, h, &actionSeams{offered: []molecule.NextAction{acceptOffer(digest)}})
	shown := viewOf[ActionsView](t, do(t, h, authed("/api/molecules/"+r.ID+"/actions")), http.StatusOK).Actions[0]
	if shown.Kind != ActionAcceptFailures || shown.DeltasDigest != DeltasDigest(testDeltas) {
		t.Fatalf("shown %+v", shown)
	}
	// A new verification attempt of the same plan fails TestY in another file.
	newer := []readiness.ProtectedDelta{{CheckID: "go-test-lab", Package: "example.test/lab/pin", Test: "TestY", ScopePath: "lab/pin/other_test.go"}}
	s.mu.Lock()
	s.offered = []molecule.NextAction{{Kind: molecule.NextAcceptProtectedFailures, Digest: digest, Deltas: newer}}
	s.mu.Unlock()
	before := snapshot(t, lab)
	w := do(t, h, gatePost(actionPath(r.ID, ActionAcceptFailures), `{"digest":"`+digest+`","deltas":"`+shown.DeltasDigest+`"}`))
	refusalOf(t, w, http.StatusConflict, "stale")
	if !strings.Contains(w.Body.String(), "failing tests changed") || len(s.calls()) != 0 || snapshot(t, lab) != before {
		t.Fatalf("a stale test list reached the engine: %v %s", s.calls(), w.Body)
	}
	// The POST derived the tests again (a preview) rather than trusting the page.
	if !s.previews[len(s.previews)-1] {
		t.Fatalf("previews %v", s.previews)
	}
	// No derivable test list is never accepted either.
	s.mu.Lock()
	s.offered = []molecule.NextAction{{Kind: molecule.NextAcceptProtectedFailures, Digest: digest}}
	s.mu.Unlock()
	refusalOf(t, do(t, h, gatePost(actionPath(r.ID, ActionAcceptFailures), acceptBody(digest, nil))), http.StatusConflict, "stale")
	// The list shown now records the revision.
	s.mu.Lock()
	s.offered = []molecule.NextAction{{Kind: molecule.NextAcceptProtectedFailures, Digest: digest, Deltas: newer}}
	s.mu.Unlock()
	if v := viewOf[ActionResultView](t, do(t, h, gatePost(actionPath(r.ID, ActionAcceptFailures), acceptBody(digest, newer))), http.StatusOK); !v.Committed || !reflect.DeepEqual(s.calls(), []string{"amend"}) {
		t.Fatalf("current list: %+v %v", v, s.calls())
	}
}

// When re-run checks recovers first and the checks-only amend after it fails,
// the refusal says the recovery was recorded and that no new plan version
// exists, never that nothing was recorded; the ledger keeps the recovery.
func TestUIActionsRecoveredButNotAmended(t *testing.T) {
	for _, amendErr := range []error{molecule.ErrStalePlan, errors.New("checks-only requires unchanged scope at /Users/someone/lab"), context.DeadlineExceeded} {
		lab, r, digest := gateFixture(t)
		r.Status, r.Readiness.Phase = "blocked", "check-pending"
		writeRecord(t, lab, r)
		h := testHandler(t, Options{LabDir: lab})
		s := installActions(t, h, &actionSeams{offered: []molecule.NextAction{{Kind: molecule.NextRerunChecks, Digest: digest, RecoverFirst: true}}, amendErr: amendErr})
		w := do(t, h, gatePost(actionPath(r.ID, ActionRerunChecks), `{"digest":"`+digest+`","confirmStopped":true}`))
		refusalOf(t, w, http.StatusConflict, "recovered-not-amended")
		body := w.Body.String()
		if !strings.Contains(body, "recovery was recorded") || !strings.Contains(body, "no new plan version exists") || !strings.Contains(body, "reload") || strings.Contains(body, "nothing was recorded") || strings.Contains(body, "/Users/") {
			t.Fatalf("%v: %s", amendErr, body)
		}
		if !strings.Contains(body, categoryReasons["recovered-not-amended"]) {
			t.Fatalf("%v: no reason: %s", amendErr, body)
		}
		if !reflect.DeepEqual(s.calls(), []string{"recover", "amend"}) {
			t.Fatalf("calls %v", s.calls())
		}
		if after, err := molecule.ReadRecord(lab, r.ID); err != nil || after.Readiness.Phase != "interrupted" || len(after.Readiness.Plans) != 1 {
			t.Fatalf("the recovery was not kept: %v %+v", err, after.Readiness)
		}
	}
	// Without recovering first, an amend refusal keeps its own category.
	lab, r, digest := blockedFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	installActions(t, h, &actionSeams{offered: []molecule.NextAction{{Kind: molecule.NextRerunChecks, Digest: digest}}, amendErr: molecule.ErrStalePlan})
	refusalOf(t, do(t, h, gatePost(actionPath(r.ID, ActionRerunChecks), actionBody(digest))), http.StatusConflict, "stale")
}

// Scope-change shows and posts the ledger's exact paths. A path the page
// cannot show as itself (here one the redactor rewrites) is not offered, with a
// plain reason, and its POST is refused before the engine.
func TestUIActionsScopeChangeUsesTheExactLedgerPaths(t *testing.T) {
	lab, r, digest := blockedFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	exact := []string{"lab/x/new.go", "lab/x/new_test.go"}
	s := installActions(t, h, &actionSeams{offered: []molecule.NextAction{{Kind: molecule.NextApproveScopeChange, Digest: digest, Paths: exact, Reason: "helper needed"}, {Kind: molecule.NextRetire}}})
	v := viewOf[ActionsView](t, do(t, h, authed("/api/molecules/"+r.ID+"/actions")), http.StatusOK)
	if len(v.Actions) != 2 || !reflect.DeepEqual(v.Actions[0].Paths, exact) || len(v.NotOffered) != 0 {
		t.Fatalf("view %+v", v)
	}
	secret := "lab/ghp_" + strings.Repeat("A", 24) + ".go"
	if readiness.ValidatePath(secret) != nil {
		t.Fatal("the fixture path is not a valid repository path")
	}
	s.mu.Lock()
	s.offered = []molecule.NextAction{{Kind: molecule.NextApproveScopeChange, Digest: digest, Paths: []string{"lab/x/new.go", secret}, Reason: "helper needed"}, {Kind: molecule.NextRetire}}
	s.mu.Unlock()
	w := do(t, h, authed("/api/molecules/"+r.ID+"/actions"))
	v = viewOf[ActionsView](t, w, http.StatusOK)
	if len(v.Actions) != 1 || v.Actions[0].Kind != ActionRetire || len(v.NotOffered) != 1 || v.NotOffered[0].Kind != ActionScopeChange || v.NotOffered[0].Reason != scopePathsNotShown || strings.Contains(w.Body.String(), "ghp_") {
		t.Fatalf("view %+v", v)
	}
	before := snapshot(t, lab)
	body := `{"digest":"` + digest + `","paths":["lab/x/new.go","` + secret + `"],"reason":"r"}`
	refusalOf(t, do(t, h, gatePost(actionPath(r.ID, ActionScopeChange), body)), http.StatusConflict, "not-offered")
	if len(s.calls()) != 0 || snapshot(t, lab) != before {
		t.Fatalf("calls %v", s.calls())
	}
}

// A revision recorded after the UI checked the shown digest but before the engine
// call is refused by the engine under its lock and served as stale; engine text
// is never served.
func TestUIActionsEngineRefusalsAreFixedCategories(t *testing.T) {
	for err, category := range map[error]string{
		molecule.ErrStalePlan: "stale",
		errors.New("--accept-protected-failures refused: the candidate view failed at /Users/someone/lab"): "refused",
		errors.New("Molecule is locked: another approval, amend or recover holds it"):                      "locked",
		context.DeadlineExceeded: "interrupted",
	} {
		lab, r, digest := blockedFixture(t)
		h := testHandler(t, Options{LabDir: lab})
		installActions(t, h, &actionSeams{offered: []molecule.NextAction{acceptOffer(digest)}, amendErr: err})
		w := do(t, h, gatePost(actionPath(r.ID, ActionAcceptFailures), acceptBody(digest, testDeltas)))
		code := map[string]int{"stale": http.StatusConflict, "refused": http.StatusUnprocessableEntity, "locked": http.StatusConflict, "interrupted": http.StatusServiceUnavailable}[category]
		refusalOf(t, w, code, category)
		if strings.Contains(w.Body.String(), "/Users/") || strings.Contains(w.Body.String(), `"committed":true`) {
			t.Fatalf("%v: %s", err, w.Body)
		}
	}
}
