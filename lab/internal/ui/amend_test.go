package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// fakeAmend replaces the amend seam: it records every call and, unless err is
// set, appends the successor revision to the ledger as the engine would, so the
// read-back can see it. No git or harness process starts.
type fakeAmend struct {
	mu    sync.Mutex
	calls []molecule.AmendRequest
	err   error
	write bool
}

func (f *fakeAmend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var successorDigest = strings.Repeat("5e", 32)

func installAmend(t *testing.T, h http.Handler, f *fakeAmend) *fakeAmend {
	t.Helper()
	hh := h.(*handler)
	hh.amend = func(_ context.Context, lab, id string, req molecule.AmendRequest) (molecule.Record, error) {
		f.mu.Lock()
		f.calls = append(f.calls, req)
		err, write := f.err, f.write
		f.mu.Unlock()
		if err != nil {
			return molecule.Record{}, err
		}
		r, rerr := molecule.ReadRecord(lab, id)
		if rerr != nil {
			return r, rerr
		}
		if write {
			latest := r.Readiness.Plans[len(r.Readiness.Plans)-1]
			next := latest.Plan
			next.Revision, next.ParentDigest = latest.Plan.Revision+1, latest.Digest
			r.Readiness.Plans = append(r.Readiness.Plans, readiness.Proposal{Plan: next, Digest: successorDigest})
			if err := saveRecord(lab, r); err != nil {
				return r, err
			}
		}
		return r, nil
	}
	return f
}

func amendBody(digest string) string {
	return `{"digest":"` + digest + `","addAcceptance":["The help text names the flag"],"addScope":[{"path":"lab/x/new.go","reason":"New helper"}]}`
}

// Request changes calls the engine path of forgecell amend with exactly the
// request the CLI builds for --add-scope lab/x/new.go --reason "New helper"
// --add-acceptance "The help text names the flag", and nothing else: no note,
// continuation, accepted tree or --run. It never approves: no scope job runs and
// the new revision waits for its own approval. Committed comes only from the
// ledger read-back of a successor of exactly the submitted digest.
func TestUIAmendRecordsTheCLIRevisionWithoutApproving(t *testing.T) {
	lab, r, digest := gateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	if reflect.ValueOf(h.(*handler).amend).Pointer() != reflect.ValueOf(molecule.AmendFromLatest).Pointer() {
		t.Fatal("Request changes is not wired to molecule.AmendFromLatest")
	}
	engine := refuseEngine(t, h)
	f := installAmend(t, h, &fakeAmend{write: true})
	v := viewOf[AmendView](t, do(t, h, gatePost("/api/molecules/"+r.ID+"/amend", amendBody(digest))), http.StatusOK)
	cli := molecule.AmendRequest{AddScope: []molecule.ScopeAddition{{Path: "lab/x/new.go", Reason: "New helper"}}, AddAcceptance: []string{"The help text names the flag"}}
	if f.count() != 1 || f.calls[0].ExpectParent != digest {
		t.Fatalf("engine request %+v does not expect the shown digest %s", f.calls, digest)
	}
	// Apart from the digest the user saw, it is exactly the CLI's request.
	sent := f.calls[0]
	sent.ExpectParent = ""
	if !reflect.DeepEqual(sent, cli) {
		t.Fatalf("engine request %+v, want the CLI's %+v", sent, cli)
	}
	if f.calls[0].Run || f.calls[0].Note != "" || f.calls[0].Continuation != "" || f.calls[0].AcceptTree != "" || f.calls[0].AcceptProtectedFailures {
		t.Fatalf("Request changes asked for more than the additions: %+v", f.calls[0])
	}
	if !v.Committed || v.Revision != 2 || v.Digest != successorDigest || v.MoleculeID != r.ID || v.Canonical.Phase != "scope-waiting" {
		t.Fatalf("read-back %+v", v)
	}
	if scope, _, _ := engine.counts(); scope != 0 || h.(*handler).jobs.busy(r.ID) {
		t.Fatal("Request changes approved or started a job")
	}
	// The detail now waits for approval of the new digest, offered as a new primary.
	if d := detailOf(t, h, r.ID); d.View.Decision.Primary == nil || d.View.Decision.Primary.Digest != successorDigest {
		t.Fatalf("after the amendment the decision is %+v", d.View.Decision)
	}
	// The old digest is stale now, for Request changes and approval alike.
	refusalOf(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/amend", amendBody(digest))), http.StatusConflict, "stale")
	refusalOf(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/scope", `{"digest":"`+digest+`"}`)), http.StatusConflict, "stale")
	if f.count() != 1 {
		t.Fatalf("a stale request reached the engine: %d calls", f.count())
	}

	// An engine that records nothing is never shown as committed.
	lab2, r2, digest2 := gateFixture(t)
	h2 := testHandler(t, Options{LabDir: lab2})
	installAmend(t, h2, &fakeAmend{})
	if v := viewOf[AmendView](t, do(t, h2, gatePost("/api/molecules/"+r2.ID+"/amend", amendBody(digest2))), http.StatusOK); v.Committed || v.Digest != "" {
		t.Fatalf("no successor read back, yet %+v", v)
	}
}

// Request changes has the gate write protection and a strict body: forged and
// malformed requests, stale digests, retired Molecules, busy Molecules and
// sample data never reach the engine, and nothing in the Lab changes.
func TestUIAmendRefusalsNeverReachTheEngine(t *testing.T) {
	lab, r, digest := gateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	f := installAmend(t, h, &fakeAmend{write: true})
	engine := refuseEngine(t, h)
	before := snapshot(t, lab)
	path := "/api/molecules/" + r.ID + "/amend"
	for name, forged := range forgedWrites(path, amendBody(digest)) {
		if w := do(t, h, forged); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	many := make([]string, maxUIAmendAdditions+1)
	for i := range many {
		many[i] = fmt.Sprintf(`"note %d"`, i)
	}
	for name, body := range map[string]string{
		"nothing to add": `{"digest":"` + digest + `","addAcceptance":[],"addScope":[]}`,
		"approve field":  `{"digest":"` + digest + `","addAcceptance":["x"],"approve":true}`,
		"run field":      `{"digest":"` + digest + `","addAcceptance":["x"],"run":true}`,
		"note field":     `{"digest":"` + digest + `","addAcceptance":["x"],"note":"n"}`,
		"short digest":   `{"digest":"abc","addAcceptance":["x"]}`,
		"no digest":      `{"addAcceptance":["x"]}`,
		"blank note":     `{"digest":"` + digest + `","addAcceptance":["  "]}`,
		"control note":   `{"digest":"` + digest + `","addAcceptance":["a\u0007"]}`,
		"long note":      `{"digest":"` + digest + `","addAcceptance":["` + strings.Repeat("n", maxUIAmendText+1) + `"]}`,
		"too many":       `{"digest":"` + digest + `","addAcceptance":[` + strings.Join(many, ",") + `]}`,
		"traversal":      `{"digest":"` + digest + `","addScope":[{"path":"../x.go","reason":"r"}]}`,
		"absolute":       `{"digest":"` + digest + `","addScope":[{"path":"/etc/x","reason":"r"}]}`,
		"git dir":        `{"digest":"` + digest + `","addScope":[{"path":"a/.git/config","reason":"r"}]}`,
		"option path":    `{"digest":"` + digest + `","addScope":[{"path":"-x","reason":"r"}]}`,
		"no reason":      `{"digest":"` + digest + `","addScope":[{"path":"lab/x.go","reason":""}]}`,
		"duplicate path": `{"digest":"` + digest + `","addScope":[{"path":"lab/x.go","reason":"r"},{"path":"lab/x.go","reason":"r"}]}`,
		"scope extra":    `{"digest":"` + digest + `","addScope":[{"path":"lab/x.go","reason":"r","evidence":["issue"]}]}`,
		"trailing data":  amendBody(digest) + `{}`,
		"oversized":      `{"digest":"` + digest + `"` + strings.Repeat(" ", maxGateBody) + `,"addAcceptance":["x"]}`,
		"form body":      "digest=" + digest,
	} {
		q := gatePost(path, body)
		if name == "form body" {
			q.header["Content-Type"] = "application/x-www-form-urlencoded"
		}
		if w := do(t, h, q); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	refusalOf(t, do(t, h, gatePost(path, amendBody(strings.Repeat("0", 64)))), http.StatusConflict, "stale")
	if w := do(t, h, gatePost("/api/molecules/absent/amend", amendBody(digest))); w.Code != http.StatusNotFound {
		t.Errorf("absent Molecule: %d", w.Code)
	}
	if after := snapshot(t, lab); after != before {
		t.Fatalf("refused requests changed the Lab:\n%s\n---\n%s", before, after)
	}
	if f.count() != 0 || engine.calls() != 0 {
		t.Fatalf("refused requests reached the engine: %d amend, %d gate calls", f.count(), engine.calls())
	}

	// A running gate job for the Molecule keeps Request changes out.
	block := make(chan struct{})
	engine.scope = func(ctx context.Context, _ molecule.Options) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil
	}
	jobOf(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/scope", `{"digest":"`+digest+`"}`)), http.StatusAccepted)
	refusalOf(t, do(t, h, gatePost(path, amendBody(digest))), http.StatusConflict, "busy")
	close(block)
	waitJob(t, h, r.ID)

	// A retired Molecule is refused before the engine.
	retired := r
	retired.Status = molecule.StatusRetired
	retired.Retired = &molecule.Retirement{At: "2026-10-26T20:00:00Z", Reason: "stale", PriorStatus: "waiting"}
	writeRecord(t, lab, retired)
	if w := do(t, h, gatePost(path, amendBody(digest))); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"category":"retired"`) {
		t.Fatalf("retired: %d %s", w.Code, w.Body)
	}
	if f.count() != 0 {
		t.Fatalf("a busy or retired request reached the engine %d times", f.count())
	}

	demo := testHandler(t, Options{Demo: true})
	installAmend(t, demo, f)
	refusalOf(t, do(t, demo, gatePost("/api/molecules/demo-101/amend", amendBody(digest))), http.StatusNotFound, "sample")
	if f.count() != 0 {
		t.Fatal("sample data reached the engine")
	}
}

// Engine refusals are served as fixed categories with actionable reasons; the
// engine's text, which can name local paths, is never served.
func TestUIAmendEngineRefusalsShowFixedCategories(t *testing.T) {
	for text, want := range map[string]struct {
		code     int
		category string
	}{
		"lab/x/new.go is already in scope":                                  {http.StatusConflict, "duplicate"},
		"acceptance item already present: x":                                {http.StatusConflict, "duplicate"},
		`unsafe repository path: "../x"`:                                    {http.StatusUnprocessableEntity, "scope-refused"},
		"new path x needs evidence for its parent component: no frozen ...": {http.StatusUnprocessableEntity, "scope-refused"},
		readiness.InScopeDefinitionMessage:                                  {http.StatusUnprocessableEntity, "scope-refused"},
		"coding checkout unavailable; refresh intake":                       {http.StatusConflict, "checkout"},
		"Molecule is locked: another approval, amend or recover holds it":   {http.StatusConflict, "locked"},
		"something else at /Users/someone/lab/ledgers/x.json":               {http.StatusUnprocessableEntity, "refused"},
	} {
		lab, r, digest := gateFixture(t)
		h := testHandler(t, Options{LabDir: lab})
		f := installAmend(t, h, &fakeAmend{err: errors.New(text)})
		w := do(t, h, gatePost("/api/molecules/"+r.ID+"/amend", amendBody(digest)))
		refusalOf(t, w, want.code, want.category)
		if strings.Contains(w.Body.String(), text) || f.count() != 1 {
			t.Errorf("%q: served %s after %d calls", text, w.Body, f.count())
		}
	}
	lab, r, digest := gateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	installAmend(t, h, &fakeAmend{err: fmt.Errorf("wrapped: %w", molecule.ErrMoleculeRetired)})
	if w := do(t, h, gatePost("/api/molecules/"+r.ID+"/amend", amendBody(digest))); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"category":"retired"`) {
		t.Fatalf("engine retired refusal: %d %s", w.Code, w.Body)
	}
	installAmend(t, h, &fakeAmend{err: context.DeadlineExceeded})
	refusalOf(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/amend", amendBody(digest))), http.StatusServiceUnavailable, "interrupted")
}

// A plan revision recorded after the UI checked the shown digest, but before the
// engine call, is refused by molecule.AmendFromLatest under its lock and served as
// stale, saying a newer plan exists; nothing more is recorded.
func TestUIAmendRefusesALedgerAdvancedBeforeTheEngine(t *testing.T) {
	lab, r, digest := gateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	var sent molecule.AmendRequest
	h.(*handler).amend = func(ctx context.Context, lab, id string, req molecule.AmendRequest) (molecule.Record, error) {
		sent = req
		// Another command records a successor between the UI check and this call.
		advanced, err := molecule.ReadRecord(lab, id)
		if err != nil {
			return advanced, err
		}
		latest := advanced.Readiness.Plans[len(advanced.Readiness.Plans)-1]
		next := latest.Plan
		next.Revision, next.ParentDigest = latest.Plan.Revision+1, latest.Digest
		advanced.Readiness.Plans = append(advanced.Readiness.Plans, readiness.Proposal{Plan: next, Digest: successorDigest})
		if err := saveRecord(lab, advanced); err != nil {
			return advanced, err
		}
		return molecule.AmendFromLatest(ctx, lab, id, req)
	}
	w := do(t, h, gatePost("/api/molecules/"+r.ID+"/amend", amendBody(digest)))
	refusalOf(t, w, http.StatusConflict, "stale")
	if !strings.Contains(w.Body.String(), "a newer plan exists") || sent.ExpectParent != digest {
		t.Fatalf("stale race served %s for expected parent %q", w.Body, sent.ExpectParent)
	}
	after, err := molecule.ReadRecord(lab, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(after.Readiness.Plans); n != 2 || after.Readiness.Plans[1].Digest != successorDigest {
		t.Fatalf("the stale amend recorded a revision: %d plans", n)
	}
}

// /api/lab lists Request changes, with or without a source checkout.
func TestUIAmendAction(t *testing.T) {
	for _, source := range []string{"", "/src/checkout"} {
		lab := viewOf[LabView](t, do(t, testHandler(t, Options{LabDir: t.TempDir(), SourceRoot: source}), authed("/api/lab")), http.StatusOK)
		if !reflect.DeepEqual(lab.Actions[len(lab.Actions)-1:], []string{AmendAction}) || len(lab.Gates) != 2 {
			t.Errorf("source %q: actions %v gates %v", source, lab.Actions, lab.Gates)
		}
	}
}
