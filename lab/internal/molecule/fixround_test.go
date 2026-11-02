package molecule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// fixRoundReviewReply lists every path either round changes, so each review pass
// covers its whole change.
const fixRoundReviewReply = `{"findings":[],"verifiedSound":["nothing to report"],"reviewedFiles":["code.txt","request.json","fix.txt"]}`

// fixRoundStub codes the first round (code.txt becomes changed) and, run again on
// the delivered tree, copies the code.txt it finds into fix.txt: fix.txt then holds
// "changed" only when the fix round started from the delivered head.
const fixRoundStub = "#!/bin/sh\ncat > request.json\nif [ \"$(cat code.txt)\" = changed ]; then cat code.txt > fix.txt; fi\nprintf changed > code.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Implemented and tested\",\"paths\":[]}'\n"

type fixRoundFx struct {
	o         Options
	r         Record
	head      string
	headTree  string
	mu        sync.Mutex
	diffBases []string
	issue     Issue
}

func (fx *fixRoundFx) setIssue(i Issue) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.issue = i
}

func (fx *fixRoundFx) readIssue() Issue {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.issue
}

func (fx *fixRoundFx) bases() []string {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return slices.Clone(fx.diffBases)
}

// deliveredFixRoundFx runs the review fixture Molecule through coding, checks and a
// clean review, then commits the verified tree inside its workspace as Publish does,
// so the workspace HEAD is the delivered commit. keyed runs it as a keyed Issue.
func deliveredFixRoundFx(t *testing.T, keyed bool) *fixRoundFx {
	t.Helper()
	o, _ := reviewRunFixture(t, true, process.Result{OK: true, Stdout: fixRoundReviewReply})
	if err := writeStub(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte(fixRoundStub), 0o700); err != nil {
		t.Fatal(err)
	}
	fx := &fixRoundFx{issue: Issue{Number: 12, Repo: "acme/widgets", Title: "Change code", Body: "Write changed", URL: "https://github.com/acme/widgets/issues/12", State: "OPEN"}}
	if keyed {
		fx.issue = keyedIssueFixture()
		o.Issue, o.ReadIssue, o.AllowKeyedApproval = "", nil, true
		o.ReadIssueKey = func(context.Context, string, string, string) (Issue, error) { return fx.readIssue(), nil }
	} else {
		o.ReadIssue = func(context.Context, string, int64, string) (Issue, error) { return fx.readIssue(), nil }
	}
	o.ReviewDiff = func(_ context.Context, _ Workspace, base, _ string) (string, error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.diffBases = append(fx.diffBases, base)
		return "diff --git a/code.txt b/code.txt\n-base\n+changed\n", nil
	}
	fx.o = o
	r := fx.run(t, "")
	if r.Status != "waiting" || r.Readiness.Phase != "review-waiting" || r.Verification == nil || !r.Verification.RequiredChecksPassed {
		t.Fatalf("fixture was not verified: %s %s %q", r.Status, r.Readiness.Phase, r.Notes)
	}
	ws := r.Workspace.Path
	testGit(t, ws, "add", "--", "code.txt", "request.json")
	testGit(t, ws, "commit", "-qm", "delivered")
	fx.head = testGit(t, ws, "rev-parse", "HEAD")
	fx.headTree = testGit(t, ws, "rev-parse", "HEAD^{tree}")
	fx.r = r
	return fx
}

// run runs intake (approve empty) or the approval of digest, by number or key.
func (fx *fixRoundFx) run(t *testing.T, approve string) Record {
	t.Helper()
	o := fx.o
	o.Approve = approve
	run := func() (Record, error) {
		if o.ReadIssueKey != nil {
			return RunKeyed(context.Background(), o, "ENG-1234")
		}
		return Run(context.Background(), o)
	}
	r, err := run()
	if err != nil {
		t.Fatal(err)
	}
	if approve == "" {
		if r.Readiness == nil {
			t.Fatalf("fixture not ready: %q", r.Notes)
		}
		o.Approve = r.Readiness.Plans[0].Digest
		if r, err = run(); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (fx *fixRoundFx) request() FixRoundRequest {
	fp := strings.Repeat("f", 64)
	return FixRoundRequest{ID: fx.r.ID, Head: fx.head, HeadTree: fx.headTree, Round: 1, ExpectParent: fx.r.Readiness.Plans[len(fx.r.Readiness.Plans)-1].Digest,
		Findings:   []readiness.FixRoundFinding{{Fingerprint: fp, Reviewer: "0:forgecomments", Severity: "medium", Path: "fix.txt", Line: 1, Title: "The fix is not recorded"}},
		Acceptance: []readiness.Criterion{{Description: "fix.txt records the delivered code", Evidence: []string{readiness.FindingEvidenceID(fp)}}},
		Scope:      []ScopeAddition{{Path: "fix.txt", Reason: "Records the fix for the finding"}},
		Note:       "Fix round for the forgecomments finding"}
}

func (fx *fixRoundFx) ledger(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fx.o.LabDir, "ledgers", fx.r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestFixRoundOnADeliveredMoleculeCodesFromTheDeliveredHead(t *testing.T) {
	t.Parallel()
	fx := deliveredFixRoundFx(t, false)
	base := fx.r.Workspace.BaseCommit
	if bases := fx.bases(); len(bases) != 1 || bases[0] != base {
		t.Fatalf("the delivered round's review diffed from %q, want the intake base", bases)
	}
	// The Issue body was edited after delivery: the round records it.
	edited := fx.readIssue()
	edited.Body = "Write changed, and record the fix"
	fx.setIssue(edited)
	req := fx.request()
	req.Scope = append(req.Scope,
		ScopeAddition{Path: "package.json", Reason: "A protected check definition and manifest"},
		ScopeAddition{Path: ".github/workflows/ci.yml", Reason: "A sensitive CI path"},
		ScopeAddition{Path: "../outside.txt", Reason: "Outside the repository"},
		ScopeAddition{Path: "code.txt/inner", Reason: "Under a file"},
		ScopeAddition{Path: "code.txt", Reason: "Already in scope"})
	r, err := AmendFixRound(context.Background(), fx.o, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "waiting" || r.Readiness.Phase != "scope-waiting" || len(r.Readiness.Plans) != 2 {
		t.Fatalf("status %s phase %s plans %d", r.Status, r.Readiness.Phase, len(r.Readiness.Plans))
	}
	if got := namedAtom(t, r, "scope").Status; got != "waiting" {
		t.Fatalf("the scope gate is %s, want waiting", got)
	}
	latest := r.Readiness.Plans[1]
	p := latest.Plan
	if p.FixRound == nil || p.FixRound.Round != 1 || p.FixRound.Head != fx.head || p.FixRound.HeadTree != fx.headTree || len(p.FixRound.Findings) != 1 {
		t.Fatalf("fix round = %+v", p.FixRound)
	}
	if p.CaptureBase() != fx.head || p.Inputs.BaseCommit != base || p.PausedTree != fx.headTree {
		t.Fatalf("capture base %s, intake base %s, paused tree %s", p.CaptureBase(), p.Inputs.BaseCommit, p.PausedTree)
	}
	if !p.FixRound.IssueChanged || p.FixRound.IssueBodySHA256 != readiness.IssueBodySHA256(edited.Body) || p.Inputs.Issue.Body != "Write changed" {
		t.Fatalf("edited body: changed=%v sha=%s frozen body %q", p.FixRound.IssueChanged, p.FixRound.IssueBodySHA256, p.Inputs.Issue.Body)
	}
	if summary := AmendDiffSummary(r.Readiness.Plans[0].Plan, p); !strings.Contains(summary, "fix round 1") || !strings.Contains(summary, "Issue body changed since intake") {
		t.Fatalf("summary %q", summary)
	}
	var scope []string
	for _, s := range p.Analysis.Scope {
		scope = append(scope, s.Path)
	}
	if !slices.Equal(scope, []string{"code.txt", "request.json", "fix.txt"}) {
		t.Fatalf("scope %q", scope)
	}
	notes := strings.Join(r.Notes, "\n")
	for _, want := range []string{"2 finding path(s) match a protected path", "package.json", ".github/workflows/ci.yml", "1 finding path(s) lie outside the repository's frozen tree", "code.txt/inner", "1 finding path(s) are not clean repository paths"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("notes lack %q:\n%s", want, notes)
		}
	}
	if strings.Contains(notes, "../outside.txt") || strings.Contains(notes, fx.o.LabDir) {
		t.Fatalf("notes echo an unclean or local path:\n%s", notes)
	}
	proposed := false
	for _, e := range r.Readiness.Events {
		proposed = proposed || (e.Kind == FixRoundProposed && e.Digest == latest.Digest && strings.Contains(e.Detail, "Issue body changed"))
	}
	if !proposed {
		t.Fatalf("no fix-round-proposed event: %+v", r.Readiness.Events)
	}
	last := p.Analysis.Acceptance[len(p.Analysis.Acceptance)-1]
	if last.Evidence[0] != readiness.FindingEvidenceID(strings.Repeat("f", 64)) {
		t.Fatalf("acceptance %+v", last)
	}

	// The source checkout moving on does not block the round: the branch stays
	// rooted at the intake base on purpose.
	os.WriteFile(filepath.Join(fx.o.SourceRoot, "other.txt"), []byte("other"), 0o600)
	testGit(t, fx.o.SourceRoot, "add", "other.txt")
	testGit(t, fx.o.SourceRoot, "commit", "-qm", "moved on")

	done := fx.run(t, latest.Digest)
	if done.Status != "waiting" || done.Readiness.Phase != "review-waiting" || done.Verification == nil || !done.Verification.RequiredChecksPassed {
		t.Fatalf("fix round run: %s %s %q", done.Status, done.Readiness.Phase, done.Notes)
	}
	if data, _ := os.ReadFile(filepath.Join(done.Workspace.Path, "fix.txt")); string(data) != "changed" {
		t.Fatalf("coding did not start from the delivered head: fix.txt = %q", data)
	}
	if head := testGit(t, done.Workspace.Path, "rev-parse", "HEAD"); head != fx.head {
		t.Fatalf("the fix round moved HEAD to %s; nothing is delivered in this slice", head)
	}
	if changes := slices.Sorted(slices.Values(done.Capture.Changes)); !slices.Equal(changes, []string{"fix.txt", "request.json"}) {
		t.Fatalf("capture changes from the head = %q", changes)
	}
	if done.Verification.SourceTree != done.Capture.Tree {
		t.Fatalf("checks ran on %s, captured %s", done.Verification.SourceTree, done.Capture.Tree)
	}
	if bases := fx.bases(); len(bases) != 2 || bases[1] != fx.head {
		t.Fatalf("the fix round's review diffed from %q, want the delivered head", bases)
	}
	if len(done.ReviewAttempts) != 2 || done.ReviewAttempts[1].Status != ReviewNoHighFindings || done.ReviewAttempts[1].PlanDigest != latest.Digest {
		t.Fatalf("review attempts %+v", done.ReviewAttempts)
	}
	// The verified fix round may take the next round, from its own head.
	testGit(t, done.Workspace.Path, "add", "--", "fix.txt", "request.json")
	testGit(t, done.Workspace.Path, "commit", "-qm", "fix round 1")
	fx.r = done
	fx.head = testGit(t, done.Workspace.Path, "rev-parse", "HEAD")
	fx.headTree = testGit(t, done.Workspace.Path, "rev-parse", "HEAD^{tree}")
	second := fx.request()
	second.Round = 2
	second.Scope = nil
	second.Acceptance = []readiness.Criterion{{Description: "The second round keeps fix.txt", Evidence: []string{"issue"}}}
	r2, err := AmendFixRound(context.Background(), fx.o, second)
	if err != nil {
		t.Fatal(err)
	}
	if fr := r2.Readiness.Plans[2].Plan.FixRound; fr == nil || fr.Round != 2 || fr.Head != fx.head {
		t.Fatalf("round 2 = %+v", fr)
	}
}

func TestFixRoundRefusalsWriteNothing(t *testing.T) {
	t.Parallel()
	fx := deliveredFixRoundFx(t, false)
	before := fx.ledger(t)
	refused := func(name string, req FixRoundRequest, want string) {
		t.Helper()
		_, err := AmendFixRound(context.Background(), fx.o, req)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
		if got := fx.ledger(t); string(got) != string(before) {
			t.Fatalf("%s: the refusal wrote the ledger", name)
		}
	}
	req := fx.request()
	stale := req
	stale.ExpectParent = strings.Repeat("0", 64)
	if _, err := AmendFixRound(context.Background(), fx.o, stale); !errors.Is(err, ErrStalePlan) {
		t.Fatalf("stale parent: %v", err)
	}
	notHead := req
	notHead.Head = strings.Repeat("a", 40)
	refused("workspace HEAD", notHead, "the workspace HEAD is not the delivered head")
	atBase := req
	atBase.Head, atBase.HeadTree = fx.r.Workspace.BaseCommit, testGit(t, fx.r.Workspace.Path, "rev-parse", fx.r.Workspace.BaseCommit+"^{tree}")
	refused("head equal to base", atBase, "the fix round's head is the intake base commit")
	atBase.HeadTree = fx.headTree
	refused("head equal to base, verified tree", atBase, "the fix round's head is the intake base commit")
	wrongTree := req
	wrongTree.HeadTree = testGit(t, fx.r.Workspace.Path, "rev-parse", fx.r.Workspace.BaseCommit+"^{tree}")
	refused("tree not verified", wrongTree, "is not the verified tree")

	// A manual commit in the workspace is never a fix-round head: its tree is not the
	// verified tree, and naming the verified tree for it is a tree mismatch.
	ws := fx.r.Workspace.Path
	os.WriteFile(filepath.Join(ws, "code.txt"), []byte("manual"), 0o600)
	testGit(t, ws, "commit", "-qam", "manual")
	manual := req
	manual.Head = testGit(t, ws, "rev-parse", "HEAD")
	manual.HeadTree = testGit(t, ws, "rev-parse", "HEAD^{tree}")
	refused("manual commit", manual, "is not the verified tree")
	manual.HeadTree = fx.headTree
	refused("tree mismatch", manual, "the workspace HEAD tree is not the delivered tree")
	testGit(t, ws, "reset", "-q", "--hard", fx.head)
	round4 := req
	round4.Round = 4
	refused("round 4", round4, "at most 3 fix rounds")
	round2 := req
	round2.Round = 2
	refused("wrong round", round2, "fix round 2 must be round 1")
	absent := req
	absent.Acceptance = []readiness.Criterion{{Description: "Address a finding of another round", Evidence: []string{readiness.FindingEvidenceID(strings.Repeat("0", 64))}}}
	refused("finding not in the round", absent, "unknown or duplicate evidence citation: finding:")
	noAcceptance := req
	noAcceptance.Acceptance = nil
	refused("no acceptance", noAcceptance, "1 to 64 acceptance items")

	closed := fx.readIssue()
	closed.State = "CLOSED"
	fx.setIssue(closed)
	refused("closed Issue", req, "the Issue is closed, so fix rounds stop")
	retitled := closed
	retitled.State, retitled.Title = "OPEN", "Another title"
	fx.setIssue(retitled)
	refused("retitled Issue", req, "the Issue title changed since intake")
	retitled.Title = "Change code"
	fx.setIssue(retitled)

	// A dirty workspace is not the delivered head.
	stray := filepath.Join(fx.r.Workspace.Path, "code.txt")
	os.WriteFile(stray, []byte("uncommitted"), 0o600)
	refused("dirty workspace", req, "the workspace has changes beyond the delivered head")
	os.WriteFile(stray, []byte("changed"), 0o600)

	// The remaining refusals need a different ledger; each restores it afterwards.
	mutate := func(name, want string, edit func(*Record)) {
		t.Helper()
		// Two reads: a copy of one Record would share its pointers with the edit.
		r, err := readMolecule(fx.o.LabDir, fx.r.ID)
		if err != nil {
			t.Fatal(err)
		}
		original, err := readMolecule(fx.o.LabDir, fx.r.ID)
		if err != nil {
			t.Fatal(err)
		}
		edit(&r)
		if err = save(r); err != nil {
			t.Fatal(err)
		}
		changed := fx.ledger(t)
		_, err = AmendFixRound(context.Background(), fx.o, req)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
		if string(fx.ledger(t)) != string(changed) {
			t.Fatalf("%s: the refusal wrote the ledger", name)
		}
		if err = save(original); err != nil {
			t.Fatal(err)
		}
	}
	mutate("failing verification", "no passing verification", func(r *Record) { r.Verification.RequiredChecksPassed = false })
	mutate("no verification", "no passing verification", func(r *Record) { r.Verification = nil })
	mutate("review cap", "the review cap of 5 attempts is reached", func(r *Record) {
		for len(r.ReviewAttempts) < maxReviewAttempts {
			r.ReviewAttempts = append(r.ReviewAttempts, ReviewAttempt{Ordinal: len(r.ReviewAttempts), Status: ReviewNoHighFindings})
		}
	})

	// After every refusal the same request is still accepted.
	if _, err := AmendFixRound(context.Background(), fx.o, req); err != nil {
		t.Fatalf("the request after the refusals: %v", err)
	}
}

func TestKeyedFixRoundRefusesAClosedIssueAndApprovesByKey(t *testing.T) {
	t.Parallel()
	fx := deliveredFixRoundFx(t, true)
	if fx.r.Issue.Key != "ENG-1234" {
		t.Fatalf("issue %+v", fx.r.Issue)
	}
	before := fx.ledger(t)
	closed := fx.readIssue()
	closed.State = "CLOSED"
	fx.setIssue(closed)
	if _, err := AmendFixRound(context.Background(), fx.o, fx.request()); err == nil || err.Error() != "the Issue is closed, so fix rounds stop" {
		t.Fatalf("closed keyed Issue: %v", err)
	}
	if string(fx.ledger(t)) != string(before) {
		t.Fatal("the refusal wrote the ledger")
	}
	noReader := fx.o
	noReader.ReadIssueKey = nil
	if _, err := AmendFixRound(context.Background(), noReader, fx.request()); err == nil || !strings.Contains(err.Error(), "needs an Issue key reader") {
		t.Fatalf("keyed Molecule without a key reader: %v", err)
	}
	closed.State = "OPEN"
	fx.setIssue(closed)
	r, err := AmendFixRound(context.Background(), fx.o, fx.request())
	if err != nil {
		t.Fatal(err)
	}
	p := r.Readiness.Plans[1]
	if p.Plan.FixRound == nil || p.Plan.FixRound.IssueChanged {
		t.Fatalf("fix round %+v", p.Plan.FixRound)
	}
	// Closing the Issue between the proposal and its approval stops the round.
	closed.State = "CLOSED"
	fx.setIssue(closed)
	o := fx.o
	o.Approve = p.Digest
	if _, err = RunKeyed(context.Background(), o, "ENG-1234"); err == nil || err.Error() != "the Issue is closed, so fix rounds stop" {
		t.Fatalf("approval of a closed keyed Issue: %v", err)
	}
	closed.State = "OPEN"
	fx.setIssue(closed)
	done := fx.run(t, p.Digest)
	if done.Readiness.Phase != "review-waiting" || done.Verification == nil || !done.Verification.RequiredChecksPassed {
		t.Fatalf("keyed fix round run: %s %q", done.Readiness.Phase, done.Notes)
	}
	if data, _ := os.ReadFile(filepath.Join(done.Workspace.Path, "fix.txt")); string(data) != "changed" {
		t.Fatalf("fix.txt = %q", data)
	}
}

func TestFixRoundApprovalRefusesABodyEditedAfterTheProposal(t *testing.T) {
	t.Parallel()
	fx := deliveredFixRoundFx(t, false)
	r, err := AmendFixRound(context.Background(), fx.o, fx.request())
	if err != nil {
		t.Fatal(err)
	}
	edited := fx.readIssue()
	edited.Body = "Edited after the proposal"
	fx.setIssue(edited)
	o := fx.o
	o.Approve = r.Readiness.Plans[1].Digest
	if _, err = Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "changed after fix round 1 was proposed") {
		t.Fatalf("approval after a later body edit: %v", err)
	}
	edited.Body = "Write changed"
	fx.setIssue(edited)
	// The round's head tree is bound to the passing verification of the plan it amends.
	tampered, err := readMolecule(fx.o.LabDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	original, err := readMolecule(fx.o.LabDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tree, passed := fixRoundVerifiedTree(tampered, 1); !passed || tree != fx.headTree {
		t.Fatalf("verified tree %s passed=%v, want %s", tree, passed, fx.headTree)
	}
	for i := range tampered.VerificationAttempts {
		tampered.VerificationAttempts[i].SourceTree = strings.Repeat("e", 40)
	}
	if err = save(tampered); err != nil {
		t.Fatal(err)
	}
	if _, err = Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "is not the verified tree") {
		t.Fatalf("approval with a head tree that is not the verified tree: %v", err)
	}
	if err = save(original); err != nil {
		t.Fatal(err)
	}
	// A workspace moved off the delivered head starts no coding.
	testGit(t, r.Workspace.Path, "commit", "-q", "--allow-empty", "-m", "stray")
	if _, err = Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "not the fix round's delivered head") {
		t.Fatalf("approval off the delivered head: %v", err)
	}
	after, err := readMolecule(fx.o.LabDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Readiness.Phase != "scope-waiting" || len(after.HarnessAttempts) != 1 {
		t.Fatalf("a refused approval changed the Molecule: %s, %d attempts", after.Readiness.Phase, len(after.HarnessAttempts))
	}
}

func TestFixRoundIsKeptByLaterAmendmentsAndCannotBeDropped(t *testing.T) {
	t.Parallel()
	fx := deliveredFixRoundFx(t, false)
	r, err := AmendFixRound(context.Background(), fx.o, fx.request())
	if err != nil {
		t.Fatal(err)
	}
	// An ordinary amendment carries the fix round and its capture base.
	r, err = AmendFromLatest(context.Background(), fx.o.LabDir, r.ID, AmendRequest{AddAcceptance: []string{"A further item"}})
	if err != nil {
		t.Fatal(err)
	}
	latest := r.Readiness.Plans[len(r.Readiness.Plans)-1]
	if latest.Plan.FixRound == nil || latest.Plan.CaptureBase() != fx.head {
		t.Fatalf("the amendment dropped the fix round: %+v", latest.Plan.FixRound)
	}
	// A hand-written successor that drops it is refused before any write.
	before := fx.ledger(t)
	dropped := latest.Plan
	dropped.FixRound = nil
	// Without the round its finding citations are unknown, so they go too: the drop
	// itself is what is refused.
	dropped.Analysis.Acceptance = nil
	for _, c := range latest.Plan.Analysis.Acceptance {
		if !strings.HasPrefix(c.Evidence[0], readiness.FindingEvidencePrefix) {
			dropped.Analysis.Acceptance = append(dropped.Analysis.Acceptance, c)
		}
	}
	if _, err = Amend(context.Background(), fx.o.LabDir, r.ID, latest.Digest, dropped, ""); err == nil || !strings.Contains(err.Error(), "drops fix round 1") {
		t.Fatalf("an amendment dropped the fix round: %v", err)
	}
	if string(fx.ledger(t)) != string(before) {
		t.Fatal("the refused amendment wrote the ledger")
	}
}

// TestProtectedViewOfAFixRoundRestoresTheDeliveredTests builds the protected tree of
// a candidate that edits a test the delivered round added: a fix round restores the
// delivered version, a plan without one restores the intake base (no such test).
func TestProtectedViewOfAFixRoundRestoresTheDeliveredTests(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(resolvedTempDir(t), "repo")
	if err := os.CopyFS(repo, os.DirFS(baseRepo)); err != nil {
		t.Fatal(err)
	}
	base := testGit(t, repo, "rev-parse", "HEAD")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		testGit(t, repo, "add", "--", name)
	}
	write("x_test.go", "delivered")
	testGit(t, repo, "commit", "-qm", "delivered")
	head := testGit(t, repo, "rev-parse", "HEAD")
	write("x_test.go", "fix round")
	write("code.txt", "fixed")
	testGit(t, repo, "commit", "-qm", "candidate")
	candidate := testGit(t, repo, "rev-parse", "HEAD^{tree}")

	p := readiness.Plan{Inputs: readiness.Inputs{BaseCommit: base}, Analysis: readiness.Analysis{Checks: []readiness.Check{{ID: "go", Category: "regression"}}}}
	plain, err := verification.ProtectedTree(context.Background(), repo, p, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if out := testGit(t, repo, "ls-tree", "--name-only", plain); strings.Contains(out, "x_test.go") {
		t.Fatalf("without a fix round the protected view kept a test absent from the intake base:\n%s", out)
	}
	p.FixRound = &readiness.FixRoundRef{Round: 1, Head: head}
	protected, err := verification.ProtectedTree(context.Background(), repo, p, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := testGit(t, repo, "rev-parse", protected+":x_test.go"), testGit(t, repo, "rev-parse", head+":x_test.go"); got != want {
		t.Fatalf("the protected view restored %s, want the delivered test %s", got, want)
	}
	if got, want := testGit(t, repo, "rev-parse", protected+":code.txt"), testGit(t, repo, "rev-parse", candidate+":code.txt"); got != want {
		t.Fatal("the protected view did not keep the candidate's source")
	}
}

// TestInterruptedFixRoundCodingRecoversFromTheDeliveredHead interrupts the coding
// attempt of an approved fix round and recovers it: recovery captures from the
// delivered head, as coding did, instead of refusing the moved workspace HEAD.
func TestInterruptedFixRoundCodingRecoversFromTheDeliveredHead(t *testing.T) {
	t.Parallel()
	fx := deliveredFixRoundFx(t, false)
	r, err := AmendFixRound(context.Background(), fx.o, fx.request())
	if err != nil {
		t.Fatal(err)
	}
	digest := r.Readiness.Plans[1].Digest
	done := fx.run(t, digest)
	if done.Readiness.Phase != "review-waiting" || len(done.HarnessAttempts) != 2 {
		t.Fatalf("fix round run: %s, %d attempts", done.Readiness.Phase, len(done.HarnessAttempts))
	}
	// Rewind the ledger to the moment the fix round's coding ended, before the Lab
	// recorded its outcome: the attempt is interrupted mid-coding.
	for i, e := range done.Readiness.Events {
		if e.Kind == "attempt-check-pending" && e.Digest == digest {
			done.Readiness.Events = done.Readiness.Events[:i]
			break
		}
	}
	done.Readiness.Phase = "coding"
	for i := range done.Atoms {
		if done.Atoms[i].ID == "harness" {
			done.Atoms[i].Status = "active"
		}
	}
	if err = save(done); err != nil {
		t.Fatal(err)
	}
	recovered, err := Recover(context.Background(), fx.o.LabDir, done.ID, digest, true)
	if err != nil {
		t.Fatalf("a fix round interrupted during coding did not recover: %v", err)
	}
	if statePhase(recovered) != "interrupted" || len(recovered.HarnessAttempts) != 2 {
		t.Fatalf("recovery: phase %s, %d attempts", statePhase(recovered), len(recovered.HarnessAttempts))
	}
	last := recovered.Readiness.Events[len(recovered.Readiness.Events)-1]
	if last.Tree != done.Capture.Tree || recovered.Capture == nil || !slices.Equal(slices.Sorted(slices.Values(recovered.Capture.Changes)), []string{"fix.txt", "request.json"}) {
		t.Fatalf("recovery captured tree %s (capture %+v), want %s from the delivered head", last.Tree, recovered.Capture, done.Capture.Tree)
	}
	if head := testGit(t, recovered.Workspace.Path, "rev-parse", "HEAD"); head != fx.head {
		t.Fatalf("recovery moved HEAD to %s", head)
	}
}

// TestFixRoundScopeDropsAPathACheckNames proposes a frozen file that a check's argv
// names: buildRevision would refuse it, so the round drops it with a note and keeps
// the rest instead of refusing.
func TestFixRoundScopeDropsAPathACheckNames(t *testing.T) {
	t.Parallel()
	fx := deliveredFixRoundFx(t, false)
	p := copyPlan(t, fx.r.Readiness.Plans[0].Plan)
	// code.txt is a frozen file out of this plan's scope that no check cites as a
	// definition and that is not a sensitive path, so only the argv rule catches it.
	if sensitiveScopePath("code.txt") {
		t.Fatal("code.txt is a sensitive path; pick another frozen file")
	}
	p.Analysis.Scope = slices.DeleteFunc(p.Analysis.Scope, func(s readiness.ScopedPath) bool { return s.Path == "code.txt" })
	for i := range p.Analysis.Checks {
		p.Analysis.Checks[i].Definitions = nil
	}
	p.Analysis.Checks = append(p.Analysis.Checks, readiness.Check{ID: "show", Category: "regression", Argv: []string{"cat", "code.txt"}, Dir: ".", Required: true})
	proposed := []ScopeAddition{{Path: "code.txt", Reason: "The finding points at the code a check reads"}, {Path: "fix.txt", Reason: "Records the fix"}}
	kept, notes := fixRoundScope(fx.r, p, proposed)
	if len(kept) != 1 || kept[0].Path != "fix.txt" {
		t.Fatalf("kept %+v", kept)
	}
	if joined := strings.Join(notes, "\n"); !strings.Contains(joined, "1 finding path(s) match a protected path") || !strings.Contains(joined, "code.txt") {
		t.Fatalf("notes %q", notes)
	}
	// The kept scope passes the rule buildRevision applies; the dropped path does not.
	if _, err := buildRevision(fx.r, p, AmendRequest{AddScope: kept}); err != nil {
		t.Fatalf("buildRevision refused the kept scope: %v", err)
	}
	if _, err := buildRevision(fx.r, p, AmendRequest{AddScope: proposed}); err == nil {
		t.Fatal("buildRevision accepted a path a check's argv names; the probe guards nothing")
	}
}

func TestFixRoundHeadMustBeADeliveredCommitOfTheVerifiedTree(t *testing.T) {
	t.Parallel()
	base, head, tree := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	p := readiness.Plan{Inputs: readiness.Inputs{BaseCommit: base}}
	if err := fixRoundHeadDelivered(p, head, tree, tree); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ head, tree, verified, want string }{
		"head is base":      {base, tree, tree, "the intake base commit"},
		"tree not verified": {head, tree, strings.Repeat("4", 40), "is not the verified tree"},
		"no verified tree":  {head, tree, "", "is not the verified tree"},
	} {
		if err := fixRoundHeadDelivered(p, c.head, c.tree, c.verified); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}
