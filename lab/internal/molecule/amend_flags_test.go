package molecule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// amendFixture is a scope-waiting Molecule whose frozen tree also holds nested
// manifests, so evidence selection has more than the root package.json to choose from.
func amendFixture(t *testing.T) (Options, Record) {
	t.Helper()
	o, repo := fixture(t)
	for file, body := range map[string]string{"lab/go.mod": "module example.test/lab\n", "lab/web/package.json": "{}\n", "lab/main.go": "package main\n"} {
		if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(file)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, file), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	testGit(t, repo, "add", ".")
	testGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-qm", "manifests")
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness == nil || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("fixture not scope-waiting: %+v", r)
	}
	return o, r
}

func evidenceID(t *testing.T, p readiness.Plan, file string) string {
	t.Helper()
	for _, e := range p.Evidence {
		if e.Path == file {
			return e.ID
		}
	}
	t.Fatalf("no frozen evidence for %s", file)
	return ""
}

func latestPlan(r Record) readiness.Proposal {
	return r.Readiness.Plans[len(r.Readiness.Plans)-1]
}

func TestAmendFromLatestAddsScopeWithManifestEvidence(t *testing.T) {
	t.Parallel()
	o, r := amendFixture(t)
	parent := latestPlan(r)
	r, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{
		AddScope: []ScopeAddition{{Path: "lab/x/new.go", Reason: "New helper"}, {Path: "lab/web/app.js", Reason: "New page"}, {Path: "lab/main.go", Reason: "Wire the helper"}},
		Note:     "Resumed after a network drop",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := latestPlan(r)
	if got.Plan.Revision != 2 || got.Plan.ParentDigest != parent.Digest || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("revision %d parent %s phase %s", got.Plan.Revision, got.Plan.ParentDigest, r.Readiness.Phase)
	}
	if err = got.Plan.Validate(); err != nil {
		t.Fatal(err)
	}
	if err = r.RepositoryEvidence.ValidateScope(got.Plan.Analysis.Scope); err != nil {
		t.Fatal(err)
	}
	scope := got.Plan.Analysis.Scope
	want := map[string]string{
		"lab/x/new.go":   evidenceID(t, got.Plan, "lab/go.mod"),
		"lab/web/app.js": evidenceID(t, got.Plan, "lab/web/package.json"),
		// An existing path without its own frozen evidence cites its module manifest.
		"lab/main.go": evidenceID(t, got.Plan, "lab/go.mod"),
	}
	for _, s := range scope[len(scope)-3:] {
		if len(s.Evidence) != 1 || s.Evidence[0] != want[s.Path] {
			t.Fatalf("%s cites %v, want %s", s.Path, s.Evidence, want[s.Path])
		}
	}
	if got.Plan.OperatorNote != "Resumed after a network drop" || len(got.Plan.Analysis.Acceptance) != len(parent.Plan.Analysis.Acceptance) {
		t.Fatalf("note %q acceptance %d", got.Plan.OperatorNote, len(got.Plan.Analysis.Acceptance))
	}
	if summary := AmendDiffSummary(parent.Plan, got.Plan); summary != "+3 scope, note set" {
		t.Fatalf("summary %q", summary)
	}
}

func TestAmendScopeEvidenceSelection(t *testing.T) {
	t.Parallel()
	ev := func(id, file string) readiness.FileEvidence {
		return readiness.FileEvidence{Evidence: readiness.Evidence{ID: id, Path: file}}
	}
	snapshot := readiness.RepositorySnapshot{
		Files:    []readiness.TreeFile{{Path: "a/go.mod"}, {Path: "a/package.json"}, {Path: "a/b/pyproject.toml"}, {Path: "a/old.go"}, {Path: "README.md"}, {Path: "c/go.mod"}},
		Evidence: []readiness.FileEvidence{ev("m1", "a/go.mod"), ev("m2", "a/package.json"), ev("m3", "a/b/pyproject.toml"), ev("readme", "README.md"), ev("uncited", "c/go.mod")},
	}
	var plan []readiness.Evidence
	for _, e := range snapshot.Evidence[:4] {
		plan = append(plan, e.Evidence)
	}
	for _, c := range []struct{ file, want, err string }{
		{"a/new.go", "m1", ""},                // go.mod wins a tie with package.json
		{"a/b/c/new.py", "m3", ""},            // the nearest ancestor wins
		{"a/old.go", "m1", ""},                // existing, no own evidence: module manifest
		{"README.md", "readme", ""},           // existing with its own frozen evidence
		{"docs/new.md", "", "needs evidence"}, // no covering manifest
		{"c/new.go", "", "needs evidence"},    // the plan does not cite the manifest
		{"ab/new.go", "", "needs evidence"},   // a/ is not an ancestor of ab/
	} {
		got, err := scopeEvidence(c.file, plan, snapshot)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("%s: got %q, %v; want refusal", c.file, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("%s: got %q, %v; want %s", c.file, got, err, c.want)
		}
	}
	snapshot.Files = append(snapshot.Files, readiness.TreeFile{Path: "docs/old.md"})
	if got, err := scopeEvidence("docs/old.md", plan, snapshot); err != nil || got != "issue" {
		t.Fatalf("an existing uncovered path cites the issue: %q, %v", got, err)
	}
}

func TestAmendFromLatestRefusalsLeaveTheLedgerUnchanged(t *testing.T) {
	t.Parallel()
	o, r := amendFixture(t)
	before := ledgerBytes(t, r)
	long := strings.Repeat("x", readiness.MaxOperatorNoteBytes+1)
	many := make([]ScopeAddition, maxAmendAdditions+1)
	for i := range many {
		many[i] = ScopeAddition{Path: "lab/x/f" + strings.Repeat("a", i+1) + ".go", Reason: "r"}
	}
	for name, c := range map[string]struct {
		req  AmendRequest
		want string
	}{
		"frozen definition": {AmendRequest{AddScope: []ScopeAddition{{Path: "package.json", Reason: "Edit checker"}}}, readiness.InScopeDefinitionMessage},
		"already in scope":  {AmendRequest{AddScope: []ScopeAddition{{Path: "code.txt", Reason: "Again"}}}, "already in scope"},
		"traversal":         {AmendRequest{AddScope: []ScopeAddition{{Path: "../x.go", Reason: "Escape"}}}, "unsafe repository path"},
		"blank reason":      {AmendRequest{AddScope: []ScopeAddition{{Path: "lab/x/new.go", Reason: " "}}}, "--reason"},
		"too many":          {AmendRequest{AddScope: many}, "at most"},
		"note control":      {AmendRequest{Note: "status\x1b[31m"}, "control character"},
		"note newline":      {AmendRequest{Note: "line one\nline two"}, "control character"},
		"note too long":     {AmendRequest{Note: long}, "at most"},
		"duplicate item":    {AmendRequest{AddAcceptance: []string{"Code contains changed"}}, "already present"},
		"continuation":      {AmendRequest{Continuation: "adopt-failed-tree"}, "checks-only"},
		// checks-only needs captured, completed coding output; this Molecule has none.
		"checks-only pending": {AmendRequest{Continuation: "checks-only"}, "checks-only"},
	} {
		_, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, c.req)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v, want %q", name, err, c.want)
		}
		if name == "frozen definition" && !strings.HasPrefix(err.Error(), readiness.InScopeDefinitionMessage) {
			t.Fatalf("overlap refusal must carry the intake message verbatim: %v", err)
		}
		if strings.Contains(err.Error(), o.LabDir) {
			t.Fatalf("%s: refusal names the local Lab directory: %v", name, err)
		}
		if !bytes.Equal(before, ledgerBytes(t, r)) {
			t.Fatalf("%s: a refused amend changed the ledger", name)
		}
	}
	if _, err := AmendFromLatest(context.Background(), o.LabDir, "mol-missing", AmendRequest{}); err == nil || strings.Contains(err.Error(), o.LabDir) {
		t.Fatalf("a missing Molecule must be refused without naming the Lab directory: %v", err)
	}
}

func TestAmendFromLatestNoteReplacesAndReachesTheCodingHarness(t *testing.T) {
	t.Parallel()
	o, r := amendFixture(t)
	r, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Note: "first status"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Note: "second status", AddAcceptance: []string{"Docs describe the flag"}})
	if err != nil {
		t.Fatal(err)
	}
	second := r.Readiness.Plans[1].Plan
	got := latestPlan(r)
	if got.Plan.OperatorNote != "second status" {
		t.Fatalf("note %q", got.Plan.OperatorNote)
	}
	acceptance := got.Plan.Analysis.Acceptance
	last := acceptance[len(acceptance)-1]
	if len(acceptance) != 2 || last.Description != "Docs describe the flag" || len(last.Evidence) != 1 || last.Evidence[0] != "issue" {
		t.Fatalf("acceptance %+v", acceptance)
	}
	for _, c := range acceptance {
		if strings.Contains(c.Description, "status") {
			t.Fatalf("a note became an acceptance item: %+v", acceptance)
		}
	}
	if summary := AmendDiffSummary(second, got.Plan); summary != "+1 acceptance, note set" {
		t.Fatalf("summary %q", summary)
	}
	o.Approve = got.Digest
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(r.Workspace.Path, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Instruction   string         `json:"instruction"`
		NewAcceptance []string       `json:"newAcceptance"`
		ApprovedPlan  readiness.Plan `json:"approvedPlan"`
	}
	if err = json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if request.ApprovedPlan.OperatorNote != "second status" || strings.Contains(string(raw), "first status") || !strings.HasSuffix(request.Instruction, operatorNoteInstruction) {
		t.Fatalf("the coding harness did not receive only the latest note: %s", raw)
	}
	// Nothing was coded yet, so every item is new; the note is never one of them.
	if strings.Join(request.NewAcceptance, "|") != "Code contains changed|Docs describe the flag" {
		t.Fatalf("newAcceptance %q", request.NewAcceptance)
	}
}

func TestAmendFromLatestWithoutNoteClearsIt(t *testing.T) {
	t.Parallel()
	o, r := amendFixture(t)
	r, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Note: "status"})
	if err != nil {
		t.Fatal(err)
	}
	noted := latestPlan(r).Plan
	if r, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := latestPlan(r).Plan; got.OperatorNote != "" || AmendDiffSummary(noted, got) != "note cleared" {
		t.Fatalf("note %q summary %q", got.OperatorNote, AmendDiffSummary(noted, got))
	}
}

func TestAmendFromLatestChecksOnlyRunSkipsTheCodingHarness(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || !CompletedCoding(r) {
		t.Fatalf("fixture did not complete coding: %s", statePhase(r))
	}
	request := filepath.Join(r.Workspace.Path, "request.json")
	before, err := os.ReadFile(request)
	if err != nil {
		t.Fatal(err)
	}
	parent := latestPlan(r)
	if r, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Continuation: "checks-only"}); err != nil {
		t.Fatal(err)
	}
	got := latestPlan(r)
	if got.Plan.Continuation != "checks-only" || AmendDiffSummary(parent.Plan, got.Plan) != "continuation: checks-only" {
		t.Fatalf("continuation %q", got.Plan.Continuation)
	}
	o.Approve = got.Digest
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	started := false
	for _, e := range r.Readiness.Events {
		started = started || (e.Kind == "checks-only-started" && e.Digest == got.Digest)
	}
	after, _ := os.ReadFile(request)
	if !started || len(r.HarnessAttempts) != 1 || !bytes.Equal(before, after) || r.Verification == nil || !r.Verification.RequiredChecksPassed {
		t.Fatalf("checks-only must verify without coding: started=%t attempts=%d", started, len(r.HarnessAttempts))
	}
}

func TestAmendFromLatestLocking(t *testing.T) {
	t.Parallel()
	o, r := amendFixture(t)
	before := ledgerBytes(t, r)
	unlock, err := lockMolecule(o.LabDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Note: "x"})
	unlock()
	if !errors.Is(err, errLocked) || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("amend ran while the Molecule flock was held: %v", err)
	}
	lock := filepath.Join(o.LabDir, "ledgers", r.ID+".lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Note: "x"})
	if err == nil || !strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), o.LabDir) {
		t.Fatalf("amend ran while the approval lock was held: %v", err)
	}
	if _, statErr := os.Lstat(lock); statErr != nil {
		t.Fatal("a refused amend removed a lock it does not own")
	}
	if err = os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, ledgerBytes(t, r)) {
		t.Fatal("a locked amend changed the ledger")
	}
	// Successive amends chain: each builds on the revision the previous recorded.
	first, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Note: "one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Note: "two"})
	if err != nil {
		t.Fatal(err)
	}
	plans := second.Readiness.Plans
	if len(plans) != 3 || plans[2].Plan.ParentDigest != latestPlan(first).Digest || plans[1].Plan.ParentDigest != plans[0].Digest {
		t.Fatalf("history forked: %d plans", len(plans))
	}
	// A hand-written amend against the superseded parent cannot fork the history.
	if _, err = Amend(context.Background(), o.LabDir, r.ID, plans[1].Digest, plans[1].Plan, ""); err == nil {
		t.Fatal("an amend of a superseded parent was recorded")
	}
	if _, err = os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatalf("amend left its approval lock behind: %v", err)
	}
}
