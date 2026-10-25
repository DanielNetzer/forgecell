package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const testOrigin = "http://" + testHost

// fakeEngine replaces the engine seams; it counts every call, so a test can
// prove a refused or duplicate request never reached the engine. No git, gh or
// harness process starts.
type fakeEngine struct {
	mu        sync.Mutex
	scopes    []molecule.Options
	previews  []delivery.Options
	publishes []delivery.PublishOptions
	scope     func(context.Context, molecule.Options) error
	preview   func(context.Context, delivery.Options) (delivery.PreviewResult, error)
	publish   func(context.Context, delivery.PublishOptions) error
}

func (e *fakeEngine) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.scopes) + len(e.previews) + len(e.publishes)
}

func (e *fakeEngine) counts() (scope, preview, publish int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.scopes), len(e.previews), len(e.publishes)
}

func installEngine(h http.Handler, e *fakeEngine) *fakeEngine {
	hh := h.(*handler)
	hh.runScope = func(ctx context.Context, o molecule.Options) (molecule.Record, error) {
		e.mu.Lock()
		e.scopes = append(e.scopes, o)
		run := e.scope
		e.mu.Unlock()
		if run == nil {
			return molecule.Record{}, errors.New("no scope seam")
		}
		return molecule.Record{}, run(ctx, o)
	}
	hh.preview = func(ctx context.Context, o delivery.Options) (delivery.PreviewResult, error) {
		e.mu.Lock()
		e.previews = append(e.previews, o)
		run := e.preview
		e.mu.Unlock()
		if run == nil {
			return delivery.PreviewResult{}, errors.New("no preview seam")
		}
		return run(ctx, o)
	}
	hh.publish = func(ctx context.Context, o delivery.PublishOptions) (delivery.Receipt, error) {
		e.mu.Lock()
		e.publishes = append(e.publishes, o)
		run := e.publish
		e.mu.Unlock()
		if run == nil {
			return delivery.Receipt{}, errors.New("no publish seam")
		}
		return delivery.Receipt{}, run(ctx, o)
	}
	return e
}

// refuseEngine installs seams that only count and refuse.
func refuseEngine(t *testing.T, h http.Handler) *fakeEngine {
	t.Helper()
	return installEngine(h, &fakeEngine{})
}

// gateFixture is labFixture waiting for scope approval of one exact plan.
func gateFixture(t *testing.T) (string, molecule.Record, string) {
	t.Helper()
	lab, r := labFixture(t)
	digest := strings.Repeat("ab", 32)
	plan := readiness.Plan{Revision: 1, Inputs: readiness.Inputs{Repository: "o/r", TargetBranch: "main", BaseCommit: r.Workspace.BaseCommit}, Analysis: readiness.Analysis{Scope: []readiness.ScopedPath{{Path: "b.go"}, {Path: "a.go"}}}}
	r.Readiness = &readiness.State{SchemaVersion: "v1", Phase: "scope-waiting", Plans: []readiness.Proposal{{Plan: plan, Digest: digest}}, Events: []readiness.Event{{Kind: "proposed", Digest: digest, At: "2026-10-24T10:00:01Z"}}}
	writeRecord(t, lab, r)
	return lab, r, digest
}

func gatePost(path, body string) request {
	r := authed(path)
	r.method, r.origin, r.body = http.MethodPost, testOrigin, body
	r.header = map[string]string{"Content-Type": "application/json"}
	return r
}

// forgedWrites are gate POSTs that each lack one part of the write protection.
func forgedWrites(path, body string) map[string]request {
	out := map[string]request{}
	add := func(name string, mutate func(*request)) {
		r := gatePost(path, body)
		mutate(&r)
		out[name] = r
	}
	add("no cookie", func(r *request) { r.cookie = nil; r.header[sessionHeader] = theSession })
	add("wrong cookie", func(r *request) {
		r.cookie = &http.Cookie{Name: "forgecell_ui_4321", Value: strings.Repeat("0", 64)}
		r.header[sessionHeader] = theSession
	})
	add("no tab header", func(r *request) { r.header[sessionHeader] = "" })
	add("no origin", func(r *request) { r.origin = "" })
	add("foreign origin", func(r *request) { r.origin = "http://evil.example" })
	add("other port origin", func(r *request) { r.origin = "http://127.0.0.1:9999" })
	add("null origin", func(r *request) { r.origin = "null" })
	add("cross-site", func(r *request) { r.site = "cross-site" })
	add("same-site", func(r *request) { r.site = "same-site" })
	add("user-initiated", func(r *request) { r.site = "none" })
	add("rebinding host", func(r *request) { r.host = "evil.example:4321" })
	return out
}

func waitJob(t *testing.T, h http.Handler, id string) {
	t.Helper()
	g := h.(*handler).jobs
	g.mu.Lock()
	j := g.jobs[id]
	g.mu.Unlock()
	if j == nil {
		t.Fatal("no job")
	}
	<-j.done
}

func jobOf(t *testing.T, w *httptest.ResponseRecorder, code int) GateJobView {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status %d, want %d: %s", w.Code, code, w.Body)
	}
	var v GateJobView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	return v
}

func refusalOf(t *testing.T, w *httptest.ResponseRecorder, code int, category string) {
	t.Helper()
	var v gateRefusal
	if w.Code != code || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Category != category || v.Error == "" {
		t.Fatalf("status %d, want %d %s: %s", w.Code, code, category, w.Body)
	}
}

// noLeak fails when a served body carries a local path or a secret.
func noLeak(t *testing.T, body string, extra ...string) {
	t.Helper()
	for _, leak := range append([]string{"/Users/", "/home/", "/private/", "/tmp/", "ghp_"}, extra...) {
		if strings.Contains(body, leak) {
			t.Errorf("response carries %q: %s", leak, body)
		}
	}
}

func TestUIGateWriteProtectionRefusesForgedAndMalformedWrites(t *testing.T) {
	lab, r, digest := gateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	engine := refuseEngine(t, h)
	before := snapshot(t, lab)
	scope := `{"digest":"` + digest + `"}`
	for _, action := range []string{"scope", "delivery/preview", "delivery/publish"} {
		for name, forged := range forgedWrites("/api/molecules/"+r.ID+"/"+action, scope) {
			if w := do(t, h, forged); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d %s", name, action, w.Code, w.Body)
			}
		}
	}
	path := "/api/molecules/" + r.ID
	for name, req := range map[string]request{
		"no content type": func() request { q := gatePost(path+"/scope", scope); delete(q.header, "Content-Type"); return q }(),
		"form body": func() request {
			q := gatePost(path+"/scope", "digest="+digest)
			q.header["Content-Type"] = "application/x-www-form-urlencoded"
			return q
		}(),
		"unknown field":    gatePost(path+"/scope", `{"digest":"`+digest+`","approve":true}`),
		"trailing data":    gatePost(path+"/scope", scope+`{}`),
		"oversized":        gatePost(path+"/scope", `{"digest":"`+digest+`"`+strings.Repeat(" ", maxGateBody)+`}`),
		"array":            gatePost(path+"/scope", `[]`),
		"short digest":     gatePost(path+"/scope", `{"digest":"abc"}`),
		"uppercase digest": gatePost(path+"/scope", `{"digest":"`+strings.ToUpper(digest)+`"}`),
		"preview fields":   gatePost(path+"/delivery/preview", `{"files":["x"]}`),
		"no title":         gatePost(path+"/delivery/publish", `{"digest":"`+digest+`"}`),
		"two-line title":   gatePost(path+"/delivery/publish", `{"digest":"`+digest+`","title":"a\nb"}`),
		"padded title":     gatePost(path+"/delivery/publish", `{"digest":"`+digest+`","title":" a"}`),
		"control title":    gatePost(path+"/delivery/publish", `{"digest":"`+digest+`","title":"a\u0007"}`),
		"long title":       gatePost(path+"/delivery/publish", `{"digest":"`+digest+`","title":"`+strings.Repeat("t", maxTitleBytes+1)+`"}`),
		"bad id":           gatePost("/api/molecules/a.b/scope", scope),
	} {
		refusalOf(t, do(t, h, req), http.StatusBadRequest, "invalid")
		_ = name
	}
	if w := do(t, h, gatePost("/api/molecules/absent/scope", scope)); w.Code != http.StatusNotFound {
		t.Errorf("absent Molecule: %d", w.Code)
	}
	if after := snapshot(t, lab); after != before {
		t.Fatalf("refused writes changed the Lab:\n%s\n---\n%s", before, after)
	}
	if n := engine.calls(); n != 0 {
		t.Fatalf("refused writes reached the engine %d times", n)
	}
	// The same request with every protection in place reaches the gate.
	engine.scope = func(context.Context, molecule.Options) error { return nil }
	jobOf(t, do(t, h, gatePost(path+"/scope", scope)), http.StatusAccepted)
	waitJob(t, h, r.ID)
	if n := engine.calls(); n != 1 {
		t.Fatalf("the protected write ran %d engine calls", n)
	}
}

func TestUIGateDemoRefusesEveryGate(t *testing.T) {
	h := testHandler(t, Options{Demo: true})
	engine := refuseEngine(t, h)
	for _, action := range []string{"scope", "delivery/preview", "delivery/publish"} {
		refusalOf(t, do(t, h, gatePost("/api/molecules/demo-101/"+action, `{}`)), http.StatusNotFound, "sample")
	}
	if w := do(t, h, authed("/api/molecules/demo-101/job")); w.Code != http.StatusNotFound {
		t.Fatalf("demo job: %d", w.Code)
	}
	if engine.calls() != 0 {
		t.Fatal("sample data reached the engine")
	}
}

// Scope approval runs molecule.Run once with the exact digest, refuses a
// duplicate while it runs, and shows as committed only after the ledger records
// the approval. It never previews or publishes.
func TestUIGateScopeApprovalCommitsOnlyAfterTheLedgerReadBack(t *testing.T) {
	lab, r, digest := gateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	started, release := make(chan molecule.Options, 4), make(chan bool)
	engine := installEngine(h, &fakeEngine{scope: func(ctx context.Context, o molecule.Options) error {
		started <- o
		if <-release {
			approved := r
			approved.Readiness = &readiness.State{SchemaVersion: "v1", Phase: "approved", Plans: r.Readiness.Plans, Events: append(append([]readiness.Event(nil), r.Readiness.Events...), readiness.Event{Kind: "approved", Digest: o.Approve, At: "2026-10-24T10:10:00Z"})}
			writeRecord(t, lab, approved)
		}
		return nil
	}})
	path := "/api/molecules/" + r.ID
	body := `{"digest":"` + digest + `"}`
	if w := do(t, h, authed(path+"/job")); w.Code != http.StatusNotFound {
		t.Fatalf("job before any submission: %d", w.Code)
	}
	v := jobOf(t, do(t, h, gatePost(path+"/scope", body)), http.StatusAccepted)
	if v.Kind != "scope" || v.Digest != digest || v.State != "running" || v.Committed || v.Canonical.Source != "ledger" || v.Canonical.Phase != "scope-waiting" {
		t.Fatalf("accepted %+v", v)
	}
	o := <-started
	want := molecule.Options{LabDir: lab, Issue: "o/r#7", FormulaID: "f", Base: r.Workspace.BaseCommit, TargetBranch: "main", Approve: digest}
	if !reflect.DeepEqual(o, want) {
		t.Fatalf("engine options %+v, want %+v", o, want)
	}
	// Duplicate submissions and a preview are refused while the job runs.
	refusalOf(t, do(t, h, gatePost(path+"/scope", body)), http.StatusConflict, "busy")
	refusalOf(t, do(t, h, gatePost(path+"/delivery/publish", `{"digest":"`+digest+`","title":"t"}`)), http.StatusConflict, "busy")
	refusalOf(t, do(t, h, gatePost(path+"/delivery/preview", `{}`)), http.StatusConflict, "busy")
	if v := jobOf(t, do(t, h, authed(path+"/job")), http.StatusOK); v.State != "running" || v.Committed {
		t.Fatalf("running %+v", v)
	}
	// The job finishes without a ledger change: finished is not committed.
	release <- false
	waitJob(t, h, r.ID)
	if v := jobOf(t, do(t, h, authed(path+"/job")), http.StatusOK); v.State != "finished" || v.Committed || v.Canonical.Approved {
		t.Fatalf("finished without a ledger change %+v", v)
	}
	// A deliberate new submission runs once more; the ledger now records it.
	jobOf(t, do(t, h, gatePost(path+"/scope", body)), http.StatusAccepted)
	<-started
	release <- true
	waitJob(t, h, r.ID)
	if v := jobOf(t, do(t, h, authed(path+"/job")), http.StatusOK); v.State != "finished" || !v.Committed || !v.Canonical.Approved || v.Canonical.Phase != "approved" {
		t.Fatalf("committed %+v", v)
	}
	// The approved digest is no longer pending: a replay is stale, not a run.
	refusalOf(t, do(t, h, gatePost(path+"/scope", body)), http.StatusConflict, "stale")
	if scope, preview, publish := engine.counts(); scope != 2 || preview != 0 || publish != 0 {
		t.Fatalf("engine calls scope %d preview %d publish %d", scope, preview, publish)
	}
	if _, err := os.Lstat(filepath.Join(lab, "deliveries")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scope approval wrote a delivery: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(lab, ".delivery-lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scope approval took the delivery lock: %v", err)
	}
}

func TestUIGateScopeRefusesStaleDigestsWithoutTheEngine(t *testing.T) {
	lab, r, digest := gateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	engine := refuseEngine(t, h)
	before := snapshot(t, lab)
	path := "/api/molecules/" + r.ID + "/scope"
	refusalOf(t, do(t, h, gatePost(path, `{"digest":"`+strings.Repeat("cd", 32)+`"}`)), http.StatusConflict, "stale")
	// An amended plan supersedes the shown digest.
	amended := r
	amended.Readiness = &readiness.State{SchemaVersion: "v1", Phase: "scope-waiting", Plans: append(append([]readiness.Proposal(nil), r.Readiness.Plans...), readiness.Proposal{Plan: readiness.Plan{Revision: 2, ParentDigest: digest}, Digest: strings.Repeat("ef", 32)}), Events: r.Readiness.Events}
	writeRecord(t, lab, amended)
	before = snapshot(t, lab)
	refusalOf(t, do(t, h, gatePost(path, `{"digest":"`+digest+`"}`)), http.StatusConflict, "stale")
	// A Molecule not waiting for scope approval has no pending digest.
	amended.Readiness.Phase = "review-waiting"
	writeRecord(t, lab, amended)
	before = snapshot(t, lab)
	refusalOf(t, do(t, h, gatePost(path, `{"digest":"`+strings.Repeat("ef", 32)+`"}`)), http.StatusConflict, "stale")
	if after := snapshot(t, lab); after != before {
		t.Fatal("a stale submission changed the Lab")
	}
	if engine.calls() != 0 {
		t.Fatal("a stale submission reached the engine")
	}
}

// Engine refusals become categories with fixed messages; their text, which can
// name local paths, is never served, and nothing is retried.
func TestUIGateEngineRefusalsShowCategoriesAndNeverRetry(t *testing.T) {
	for _, tc := range []struct{ err, category, state string }{
		{"Molecule is locked by a running loop or another command; wait for it to finish, then run this again: /Users/someone/lab/ledgers/x.loop-lock", "locked", "refused"},
		{"Molecule is locked; inspect an interrupted attempt before recovery: mkdir /Users/someone/lab/ledgers/x.lock: file exists", "locked", "refused"},
		{"approval requires the exact pending plan digest", "stale", "refused"},
		{"readiness inputs changed; refresh intake before approval", "stale", "refused"},
		{"git rev-parse failed: fatal: /home/someone/src", "refused", "refused"},
		{"context canceled", "interrupted", "interrupted"},
	} {
		lab, r, digest := gateFixture(t)
		h := testHandler(t, Options{LabDir: lab})
		engine := installEngine(h, &fakeEngine{scope: func(context.Context, molecule.Options) error {
			if tc.err == "context canceled" {
				return fmt.Errorf("run: %w", context.Canceled)
			}
			return errors.New(tc.err)
		}})
		path := "/api/molecules/" + r.ID
		jobOf(t, do(t, h, gatePost(path+"/scope", `{"digest":"`+digest+`"}`)), http.StatusAccepted)
		waitJob(t, h, r.ID)
		w := do(t, h, authed(path+"/job"))
		v := jobOf(t, w, http.StatusOK)
		if v.State != tc.state || v.Category != tc.category || v.Error == "" || v.Committed {
			t.Errorf("%q: %+v", tc.err, v)
		}
		noLeak(t, w.Body.String(), lab, "ledgers/", "file exists", "fatal")
		if n := engine.calls(); n != 1 {
			t.Errorf("%q: %d engine calls; a refusal must not be retried", tc.err, n)
		}
	}
}

func TestUIGatePreviewBindsTheApprovedPlanAndRedacts(t *testing.T) {
	lab, r, _ := gateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	digest := strings.Repeat("12", 32)
	secret := "ghp_" + strings.Repeat("A", 36)
	engine := installEngine(h, &fakeEngine{preview: func(_ context.Context, o delivery.Options) (delivery.PreviewResult, error) {
		return delivery.PreviewResult{Digest: digest, MoleculeID: o.MoleculeID, Repo: "o/r", BaseBranch: o.BaseBranch, BaseCommit: r.Workspace.BaseCommit, Branch: r.Workspace.Branch, Tree: strings.Repeat("e", 40), Files: o.Files, AcceptanceUnverified: true, Diff: "+token = " + secret + "\n+path " + filepath.Join(lab, "x") + " and /Users/someone/y\n"}, nil
	}})
	w := do(t, h, gatePost("/api/molecules/"+r.ID+"/delivery/preview", `{}`))
	var p PreviewView
	decode(t, w, &p)
	if p.Digest != digest || p.BaseBranch != "main" || !reflect.DeepEqual(p.Files, []string{"b.go", "a.go"}) || !p.AcceptanceUnverified || p.DiffTruncated || p.Branch != r.Workspace.Branch {
		t.Fatalf("preview %+v", p)
	}
	if !strings.Contains(p.Diff, "<lab>") || !strings.Contains(p.Diff, "[local path]") || !strings.Contains(p.Diff, secretMark) {
		t.Fatalf("diff not redacted: %q", p.Diff)
	}
	noLeak(t, w.Body.String(), lab, secret)
	if scope, preview, publish := engine.counts(); scope != 0 || preview != 1 || publish != 0 {
		t.Fatalf("preview ran scope %d preview %d publish %d", scope, preview, publish)
	}
	o := engine.previews[0]
	if o.LabDir != lab || o.MoleculeID != r.ID || o.BaseBranch != "main" || !reflect.DeepEqual(o.Files, []string{"b.go", "a.go"}) {
		t.Fatalf("preview options %+v", o)
	}
	// A diff beyond the bound is cut and says so.
	engine.preview = func(context.Context, delivery.Options) (delivery.PreviewResult, error) {
		return delivery.PreviewResult{Digest: digest, Diff: strings.Repeat("x", maxPreviewDiff+10)}, nil
	}
	decode(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/delivery/preview", `{}`)), &p)
	if !p.DiffTruncated || len(p.Diff) != maxPreviewDiff || p.DiffBytes != maxPreviewDiff+10 {
		t.Fatalf("bounded diff: truncated %v len %d bytes %d", p.DiffTruncated, len(p.Diff), p.DiffBytes)
	}
	// Refusals keep only their category.
	for err, want := range map[error][2]any{
		delivery.ErrDemoLab: {http.StatusUnprocessableEntity, "demo"},
		errors.New("delivery tree differs from verified source"):                      {http.StatusConflict, "stale"},
		errors.New("file was not approved before coding: " + filepath.Join(lab, "z")): {http.StatusUnprocessableEntity, "refused"},
	} {
		engine.preview = func(context.Context, delivery.Options) (delivery.PreviewResult, error) {
			return delivery.PreviewResult{}, err
		}
		w := do(t, h, gatePost("/api/molecules/"+r.ID+"/delivery/preview", `{}`))
		refusalOf(t, w, want[0].(int), want[1].(string))
		noLeak(t, w.Body.String(), lab)
	}
	// Without an approved plan there is nothing to preview.
	bare := r
	bare.Readiness = nil
	writeRecord(t, lab, bare)
	refusalOf(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/delivery/preview", `{}`)), http.StatusConflict, "not-ready")
}

// Publication sends exactly the preview digest, title and acceptUnverified,
// runs as one job, and shows as committed only once the receipt says so.
func TestUIGatePublishSendsTheExactReviewAndCommitsOnTheReceipt(t *testing.T) {
	lab, r, _ := gateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	started, release := make(chan delivery.PublishOptions, 2), make(chan struct{})
	engine := installEngine(h, &fakeEngine{publish: func(_ context.Context, o delivery.PublishOptions) error {
		started <- o
		<-release
		writeReceipt(t, lab, r.ID, "published", "OPEN")
		return nil
	}})
	digest := strings.Repeat("34", 32)
	path := "/api/molecules/" + r.ID
	body := `{"digest":"` + digest + `","title":"Fix it · #7","acceptUnverified":true}`
	v := jobOf(t, do(t, h, gatePost(path+"/delivery/publish", body)), http.StatusAccepted)
	if v.Kind != "publish" || v.State != "running" || v.Committed || v.Canonical.Source != "receipt" || v.Canonical.Receipt != "" {
		t.Fatalf("accepted %+v", v)
	}
	o := <-started
	want := delivery.PublishOptions{Options: delivery.Options{LabDir: lab, MoleculeID: r.ID, BaseBranch: "main", Files: []string{"b.go", "a.go"}}, Approve: digest, Title: "Fix it · #7", AcceptUnverified: true}
	if !reflect.DeepEqual(o, want) {
		t.Fatalf("publish options %+v, want %+v", o, want)
	}
	refusalOf(t, do(t, h, gatePost(path+"/delivery/publish", body)), http.StatusConflict, "busy")
	if v := jobOf(t, do(t, h, authed(path+"/job")), http.StatusOK); v.Committed || v.Canonical.Published {
		t.Fatalf("committed before the receipt %+v", v)
	}
	close(release)
	waitJob(t, h, r.ID)
	v = jobOf(t, do(t, h, authed(path+"/job")), http.StatusOK)
	if v.State != "finished" || !v.Committed || v.Canonical.Receipt != "published" || v.Canonical.Lifecycle != "OPEN" || !v.Canonical.Published || v.Canonical.URL != "https://github.com/o/r/pull/5" {
		t.Fatalf("published %+v", v)
	}
	if scope, preview, publish := engine.counts(); scope != 0 || preview != 0 || publish != 1 {
		t.Fatalf("engine calls scope %d preview %d publish %d", scope, preview, publish)
	}
}

// An ambiguous publication is read back from the receipt, never resubmitted;
// mismatched receipts, stale digests and the delivery lock are categories.
func TestUIGatePublishRefusalsReadBackTheReceiptAndNeverRetry(t *testing.T) {
	for _, tc := range []struct {
		err, category, state, receipt, lifecycle string
		committed                                bool
	}{
		{"PR reconciliation required: gh failed or uncertain: exec /Users/someone/bin/gh", "reconciliation", "reconciliation-required", "committed", "UNKNOWN", true},
		{"resume must match the recorded review content, title and file scope", "receipt-mismatch", "refused", "prepared", "", false},
		{"approval is stale: reviewed content changed", "stale", "refused", "", "", false},
		// An earlier attempt's committed receipt must not make a refused decision look committed.
		{"resume must match the recorded review content, title and file scope", "receipt-mismatch", "refused", "committed", "OPEN", false},
		{"approval is stale: reviewed content changed", "stale", "refused", "committed", "UNKNOWN", false},
		{"delivery lock exists; verify no delivery process is active before recovering it: mkdir /Users/someone/lab/.delivery-lock: file exists", "locked", "refused", "", "", false},
		{"independent acceptance is unverified; explicit human review with --accept-unverified is required before publication", "acceptance-unverified", "refused", "", "", false},
	} {
		lab, r, _ := gateFixture(t)
		h := testHandler(t, Options{LabDir: lab})
		engine := installEngine(h, &fakeEngine{publish: func(context.Context, delivery.PublishOptions) error {
			if tc.receipt != "" {
				writeReceipt(t, lab, r.ID, tc.receipt, tc.lifecycle)
			}
			return errors.New(tc.err)
		}})
		path := "/api/molecules/" + r.ID
		jobOf(t, do(t, h, gatePost(path+"/delivery/publish", `{"digest":"`+strings.Repeat("56", 32)+`","title":"t"}`)), http.StatusAccepted)
		waitJob(t, h, r.ID)
		w := do(t, h, authed(path+"/job"))
		v := jobOf(t, w, http.StatusOK)
		if v.State != tc.state || v.Category != tc.category || v.Committed != tc.committed || v.Canonical.Receipt != tc.receipt || v.Canonical.Lifecycle != tc.lifecycle || v.Canonical.Published {
			t.Errorf("%q: %+v", tc.err, v)
		}
		noLeak(t, w.Body.String(), lab, "file exists", "exec")
		if n := engine.calls(); n != 1 {
			t.Errorf("%q: %d engine calls; an ambiguous or refused publication must not be retried", tc.err, n)
		}
	}
}

func TestUIGateUnreadableReceiptIsNeverCommitted(t *testing.T) {
	lab, r, _ := gateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	installEngine(h, &fakeEngine{publish: func(context.Context, delivery.PublishOptions) error {
		if err := os.MkdirAll(filepath.Join(lab, "deliveries"), 0700); err != nil {
			return err
		}
		return os.Symlink(filepath.Join(lab, "ledgers", r.ID+".json"), filepath.Join(lab, "deliveries", r.ID+".json"))
	}})
	path := "/api/molecules/" + r.ID
	jobOf(t, do(t, h, gatePost(path+"/delivery/publish", `{"digest":"`+strings.Repeat("78", 32)+`","title":"t"}`)), http.StatusAccepted)
	waitJob(t, h, r.ID)
	if v := jobOf(t, do(t, h, authed(path+"/job")), http.StatusOK); v.State != "finished" || v.Committed || !v.Canonical.Unreadable {
		t.Fatalf("symlinked receipt %+v", v)
	}
}

// The registry runs one job per Molecule, holds at most maxGateJobs, evicts
// finished jobs first, and stops with the server, cancelling running jobs.
func TestUIGateJobRegistryIsBoundedAndStops(t *testing.T) {
	g := newGateJobs()
	release := make(chan struct{})
	block := func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	own := make(chan struct{})
	first, err := g.start("m0", "scope", "d", func(context.Context) error { <-own; return nil })
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxGateJobs; i++ {
		if _, err := g.start(fmt.Sprintf("m%d", i), "scope", "d", block); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.start("m0", "scope", "d", block); !errors.Is(err, errGateBusy) {
		t.Fatalf("second job for one Molecule: %v", err)
	}
	if _, err := g.start("extra", "scope", "d", block); !errors.Is(err, errGateFull) {
		t.Fatalf("job beyond the bound: %v", err)
	}
	// Finish exactly one job; a new Molecule then evicts it.
	close(own)
	<-first.done
	extra, err := g.start("extra", "scope", "d", func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("eviction of a finished job: %v", err)
	}
	<-extra.done
	if _, ok := g.get("m0"); ok {
		t.Fatal("the finished job was not evicted")
	}
	g.mu.Lock()
	n := len(g.jobs)
	g.mu.Unlock()
	if n != maxGateJobs {
		t.Fatalf("registry holds %d jobs", n)
	}
	g.stop(jobStopGrace)
	if s, _ := g.get("m1"); s.state != "interrupted" {
		t.Fatalf("a running job after stop: %+v", s)
	}
	if _, err := g.start("later", "scope", "d", block); !errors.Is(err, errGateStopped) {
		t.Fatalf("a job after stop: %v", err)
	}
	// A panicking engine call becomes a refusal, not a crash.
	p := newGateJobs()
	j, err := p.start("m", "scope", "d", func(context.Context) error { panic("boom") })
	if err != nil {
		t.Fatal(err)
	}
	<-j.done
	if s, _ := p.get("m"); s.state != "refused" || strings.Contains(s.message, "boom") {
		t.Fatalf("panicked job %+v", s)
	}
	p.stop(jobStopGrace)
}

// The page submits gates with the exact digest it showed, disables duplicate
// submissions, reads canonical state back instead of retrying, and builds
// everything from text nodes.
func TestUIGateServedJSSubmitsOnceAndReadsBack(t *testing.T) {
	js := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/app.js")).Body.String()
	for _, want := range []string{
		"method: 'POST'",
		"'Content-Type': 'application/json'",
		"'/scope'",
		"'/delivery/preview'",
		"'/delivery/publish'",
		"'/job'",
		"if (state.gate.pending) return null;",
		"disabled: state.gate.pending",
		"JSON.stringify({ digest: digest })",
		"JSON.stringify({ digest: preview.digest, title: title, acceptUnverified: accept })",
		"readBack(",
		"job.committed",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	for _, gone := range []string{"approvals are not available in this UI", "/merge", "/deploy"} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js still has %q", gone)
		}
	}
}
