package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// sandbox is a PATH holding only go and git, plus a stub gh, provider CLIs and
// curl that record any call and fail. The git wrapper also records and refuses
// every network subcommand. Global and system Git configuration are cut off.
type sandbox struct{ log string }

func newSandbox(t *testing.T) sandbox {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "forbidden.log")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write("git", "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in fetch|push|pull|clone|ls-remote) printf 'git %s\\n' \"$*\" >> '"+log+"'; exit 1;; esac; done\nexec '"+realGit+"' \"$@\"\n")
	write("go", "#!/bin/sh\nexec '"+realGo+"' \"$@\"\n")
	for _, name := range []string{"gh", "claude", "codex", "cursor-agent", "curl"} {
		write(name, "#!/bin/sh\nprintf '%s %s\\n' "+name+" \"$*\" >> '"+log+"'\nexit 1\n")
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "FORGECELL_LAB"} {
		t.Setenv(key, "")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("PATH", bin)
	return sandbox{log: log}
}

func (s sandbox) assertUnused(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(s.log)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(raw)) != 0 {
		t.Fatalf("the demo invoked a forbidden tool:\n%s", raw)
	}
}

// assertNoLocalPaths fails when text names any of dirs, the home or the temp root.
func assertNoLocalPaths(t *testing.T, text string, dirs ...string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	for _, dir := range append(dirs, home, os.TempDir()) {
		for _, p := range []string{dir, resolvedPath(dir)} {
			p = strings.TrimSuffix(p, string(filepath.Separator))
			if len(p) > 1 && strings.Contains(text, p) {
				t.Fatalf("output names the local path %s:\n%s", p, text)
			}
		}
	}
}

func assertStagesInOrder(t *testing.T, text string, through int) {
	t.Helper()
	last := -1
	for i, heading := range StageHeadings {
		at := strings.Index(text, "== "+heading+" ==")
		if i >= through {
			if at >= 0 {
				t.Fatalf("stage %q printed although the run stopped earlier:\n%s", heading, text)
			}
			continue
		}
		if at <= last {
			t.Fatalf("stage %q is missing or out of order:\n%s", heading, text)
		}
		last = at
	}
}

func tempEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func readOnlyLedger(t *testing.T, lab string) molecule.Record {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(lab, "ledgers"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			files = append(files, e.Name())
		}
	}
	if len(files) != 1 {
		t.Fatalf("the kept Lab holds %d ledgers, want 1: %v", len(files), files)
	}
	raw, err := os.ReadFile(filepath.Join(lab, "ledgers", files[0]))
	if err != nil {
		t.Fatal(err)
	}
	// The same decoder forgecell ledger uses accepts it.
	if _, err = ledger.Decode(raw); err != nil {
		t.Fatalf("forgecell ledger would refuse the kept ledger: %v", err)
	}
	var r molecule.Record
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDemoRunsTheWholeLoopOfflineAndKeepsTheLab(t *testing.T) {
	sb := newSandbox(t)
	work := t.TempDir()
	t.Chdir(work)
	t.Setenv("TMPDIR", t.TempDir())
	var out bytes.Buffer
	if err := Run(context.Background(), Options{Keep: "kept-lab", Out: &out}); err != nil {
		t.Fatalf("demo failed: %v\n%s", err, out.String())
	}
	text := out.String()
	if !strings.HasPrefix(text, Banner+"\n") {
		t.Fatalf("the output does not start with the demo label:\n%s", text)
	}
	assertStagesInOrder(t, text, len(StageHeadings))
	sb.assertUnused(t)
	assertNoLocalPaths(t, text, work)
	for _, want := range []string{
		"Plan digest: ", "policy:" + formula.PolicyAutoWhenReady + "@", "a policy, never a human", "binds exactly the plan digest shown in stage 1",
		"Coding outcome: completed: Stub harness", "greeting-test [candidate]: passed", "sample-regression [regression]: passed",
		"Protected regression view (tests restored to the base commit):", "Review no-high-findings: 0 high, 0 medium, 1 low",
		"Preview digest: ", "+func Greeting(name string) string {", "+func TestGreeting(t *testing.T) {", "Implements issue #1.",
		"DEMO complete", "forgecell ledger mol-1-", "--lab 'kept-lab'",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if t.Failed() {
		t.Fatalf("output:\n%s", text)
	}

	r := readOnlyLedger(t, filepath.Join(work, "kept-lab"))
	want := map[string]string{"intake": "done", "scope": "done", "coding": "done", "checks": "done", "review": "done", "human-review": "waiting", "publication": "skipped", "documentation": "skipped"}
	if len(r.Atoms) != len(want) {
		t.Fatalf("the ledger holds %d Atoms, want %d: %+v", len(r.Atoms), len(want), r.Atoms)
	}
	for _, a := range r.Atoms {
		if want[a.ID] != a.Status {
			t.Errorf("Atom %s is %s, want %s: %s", a.ID, a.Status, want[a.ID], a.Detail)
		}
	}
	if r.Status != "waiting" || r.Readiness == nil || r.Readiness.Phase != "review-waiting" || len(r.Readiness.Plans) != 1 {
		t.Fatalf("the Molecule did not stop at the human review gate: %s %+v", r.Status, r.Readiness)
	}
	if len(r.PolicyDecisions) != 1 || !strings.HasPrefix(r.PolicyDecisions[0].ApprovedBy, "policy:"+formula.PolicyAutoWhenReady+"@") || r.PolicyDecisions[0].PlanDigest != r.Readiness.Plans[0].Digest {
		t.Fatalf("the scope approval is not the policy decision for the shown plan: %+v", r.PolicyDecisions)
	}
	if len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].Coding == nil || r.HarnessAttempts[0].Coding.Outcome != "completed" {
		t.Fatalf("coding did not complete through the stub: %+v", r.HarnessAttempts)
	}
	v := r.Verification
	if v == nil || !v.RequiredChecksPassed || v.Protected == nil || !v.Protected.RequiredChecksPassed || len(v.Checks) != 2 {
		t.Fatalf("the checks and the protected regression did not both pass: %+v", v)
	}
	if len(r.ReviewAttempts) != 1 || r.ReviewAttempts[0].Status != molecule.ReviewNoHighFindings || r.ReviewAttempts[0].Low != 1 {
		t.Fatalf("the review pass is missing: %+v", r.ReviewAttempts)
	}
	if r.Issue.Repo != Repository || r.Issue.Number != IssueNumber || r.FormulaID != formulaID {
		t.Fatalf("the ledger names another issue or Formula: %+v %s", r.Issue, r.FormulaID)
	}

	// The kept Lab is identifiable as a demo Lab, so delivery refuses it.
	kept := filepath.Join(work, "kept-lab")
	if demo, err := delivery.IsDemoLab(kept); !demo || err != nil {
		t.Fatalf("the kept Lab is not identified as a demo Lab: %t %v", demo, err)
	}
	o := delivery.Options{LabDir: kept, MoleculeID: r.ID, BaseBranch: baseBranch, Files: []string{"greeting.go"}}
	if _, err := delivery.Preview(context.Background(), o); !errors.Is(err, delivery.ErrDemoLab) {
		t.Fatalf("delivery preview ran on the kept demo Lab: %v", err)
	}
	if _, err := delivery.Publish(context.Background(), delivery.PublishOptions{Options: o, Approve: strings.Repeat("a", 64), Title: "Add Greeting"}); !errors.Is(err, delivery.ErrDemoLab) {
		t.Fatalf("delivery publish ran on the kept demo Lab: %v", err)
	}
	sb.assertUnused(t)
}

func TestDemoReportsAnInterruptionAndAProtectedViewThatDidNotRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	s := &session{ctx: ctx, out: &out}
	r := molecule.Record{ID: "mol-1-x", Atoms: []molecule.Atom{{ID: "intake", Status: "done"}, {ID: "scope", Status: "done"}, {ID: "coding", Status: "done"}, {ID: "checks", Status: "running"}},
		Verification: &verification.Result{}}
	s.detailed, s.headed = 3, 3
	err := s.stopped(r, errors.New("the Molecule did not reach the human review gate"))
	if err == nil || !strings.Contains(err.Error(), "interrupted during stage "+StageHeadings[3]) || strings.Contains(err.Error(), "did not reach the human review gate") {
		t.Fatalf("an interrupted run was not reported as interrupted: %v", err)
	}
	if text := out.String(); !strings.Contains(text, "Protected regression view: not run.") || strings.Contains(text, "not needed") {
		t.Fatalf("a missing protected view was not reported as not run:\n%s", text)
	}

	// The demo's own deadline, or a stop with a live context, is not an interruption.
	expired, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	<-expired.Done()
	for _, c := range []context.Context{expired, context.Background()} {
		s = &session{ctx: c, out: io.Discard}
		if err := s.stopped(r, errors.New("cause")); err == nil || strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), "cause") {
			t.Fatalf("a stop was reported as an interruption: %v", err)
		}
	}
}

func TestDemoRemovesItsTemporaryLab(t *testing.T) {
	sb := newSandbox(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	labTempRoot := os.Getenv(resources.LabTempRootEnv)
	before := tempEntries(t, labTempRoot)
	var out bytes.Buffer
	if err := Run(context.Background(), Options{Out: &out}); err != nil {
		t.Fatalf("demo failed: %v\n%s", err, out.String())
	}
	text := out.String()
	assertStagesInOrder(t, text, len(StageHeadings))
	sb.assertUnused(t)
	assertNoLocalPaths(t, text, tmp, labTempRoot)
	if !strings.Contains(text, "The temporary demo Lab was removed.") {
		t.Fatalf("the removal was not reported:\n%s", text)
	}
	if left := tempEntries(t, tmp); slices.ContainsFunc(left, func(n string) bool { return strings.HasPrefix(n, resources.TempPrefix+"demo-") }) {
		t.Fatalf("the temporary demo Lab was left behind: %v", left)
	}
	if after := tempEntries(t, labTempRoot); !slices.Equal(before, after) {
		t.Fatalf("the demo Lab's check TMPDIR was left behind: before %v, after %v", before, after)
	}
	for _, dir := range resources.Registered() {
		if strings.HasPrefix(dir, tmp) || strings.HasPrefix(dir, resolvedPath(tmp)) {
			t.Fatalf("the temporary demo Lab is still registered: %s", dir)
		}
	}
}

func TestDemoStopsSafelyOnHostileStubOutput(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		argv []string
		want string
	}{
		"malformed outcome":   {[]string{exe, testMalformedStub}, "Invalid or missing coding outcome"},
		"write outside scope": {[]string{exe, testOutsideScopeStub}, "notes.txt"},
		// The real stub refuses a request it does not serve and exits nonzero.
		"stub refusal": {[]string{exe, StubArg, "review"}, "Harness failed"},
	} {
		t.Run(name, func(t *testing.T) {
			sb := newSandbox(t)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			var out bytes.Buffer
			err := Run(context.Background(), Options{StubCommand: c.argv, Out: &out})
			text := out.String()
			if err == nil || !strings.Contains(err.Error(), "stopped at stage "+StageHeadings[2]) {
				t.Fatalf("the demo did not stop at coding: %v\n%s", err, text)
			}
			if !strings.Contains(text+err.Error(), c.want) {
				t.Fatalf("the stop does not say %q: %v\n%s", c.want, err, text)
			}
			assertStagesInOrder(t, text, 3)
			if strings.Contains(text, "Preview digest") || strings.Contains(text, "DEMO complete") {
				t.Fatalf("a stopped demo printed a delivery preview:\n%s", text)
			}
			sb.assertUnused(t)
			assertNoLocalPaths(t, text+err.Error(), tmp)
			if left := tempEntries(t, tmp); slices.ContainsFunc(left, func(n string) bool { return strings.HasPrefix(n, resources.TempPrefix+"demo-") }) {
				t.Fatalf("a stopped demo left its temporary Lab behind: %v", left)
			}
		})
	}
}

func TestDemoRefusesToStartWithoutGitOrGo(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var out bytes.Buffer
	err := Run(context.Background(), Options{Out: &out})
	if err == nil || !strings.Contains(err.Error(), "needs git on PATH") || !strings.Contains(err.Error(), "never needs gh") {
		t.Fatalf("missing tools were not refused: %v", err)
	}
	if out.String() != Banner+"\n" || len(tempEntries(t, tmp)) != 0 {
		t.Fatalf("a refused demo did work: %q %v", out.String(), tempEntries(t, tmp))
	}
}

func TestDemoRefusesUnsafeKeepTargets(t *testing.T) {
	sb := newSandbox(t)
	root := t.TempDir()
	t.Chdir(root)
	mustMkdir := func(p string) {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir("empty")
	mustMkdir("full")
	if err := os.WriteFile(filepath.Join("full", "keep.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("file", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("empty", "link"); err != nil {
		t.Fatal(err)
	}
	for keep, want := range map[string]string{
		"link":           "is a symbolic link",
		"full":           "is not empty",
		"file":           "is not a directory",
		"missing/parent": "could not be created",
	} {
		var out bytes.Buffer
		err := Run(context.Background(), Options{Keep: keep, Out: &out})
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "--keep '"+keep+"' refused") {
			t.Fatalf("--keep %s was not refused with %q: %v", keep, want, err)
		}
		assertNoLocalPaths(t, out.String()+err.Error(), root)
	}
	if got := tempEntries(t, "empty"); len(got) != 0 {
		t.Fatalf("the symbolic link target was written through: %v", got)
	}
	if got := tempEntries(t, "full"); !slices.Equal(got, []string{"keep.txt"}) {
		t.Fatalf("the non-empty directory was changed: %v", got)
	}
	sb.assertUnused(t)
}

// stubRequestJSON is the part of a coding request the stub reads, for the demo.
func stubRequestJSON(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	request := map[string]any{
		"kind":  "molecule",
		"issue": map[string]any{"repo": Repository, "number": IssueNumber},
		"approvedPlan": map[string]any{
			"inputs":   map[string]any{"repository": Repository},
			"analysis": map[string]any{"scope": []any{map[string]any{"path": "greeting.go"}, map[string]any{"path": "greeting_test.go"}}},
		},
	}
	if mutate != nil {
		mutate(request)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestStubWritesTheCannedChangeOnlyForThePristineDemoRequest(t *testing.T) {
	type stubCase struct {
		args    []string
		request string
		setup   func(t *testing.T, dir string)
		want    string
	}
	cases := map[string]stubCase{
		"review is not served": {args: []string{"review"}, request: stubRequestJSON(t, nil), want: "only serves the coding request"},
		"oversize request":     {request: strings.Repeat(" ", maxStubRequestBytes+1), want: "exceeds"},
		"not JSON":             {request: "coding please", want: "not JSON"},
		"analysis request":     {request: stubRequestJSON(t, func(m map[string]any) { m["kind"] = "ticket-analysis" }), want: "only the coding request"},
		"other repository":     {request: stubRequestJSON(t, func(m map[string]any) { m["issue"] = map[string]any{"repo": "acme/widgets", "number": IssueNumber} }), want: "only the demo issue"},
		"wider scope": {request: stubRequestJSON(t, func(m map[string]any) {
			m["approvedPlan"].(map[string]any)["analysis"] = map[string]any{"scope": []any{map[string]any{"path": "greeting.go"}, map[string]any{"path": "greeting_test.go"}, map[string]any{"path": "main.go"}}}
		}), want: "not the demo scope"},
		"edited checkout": {request: stubRequestJSON(t, nil), setup: func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "greeting.go"), []byte("package greeting\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "differs from the sample"},
		"symbolic link to the source": {request: stubRequestJSON(t, nil), setup: func(t *testing.T, dir string) {
			outside := filepath.Join(t.TempDir(), "greeting.go")
			if err := os.WriteFile(outside, sampleGreeting, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, "greeting.go")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, "greeting.go")); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if raw, _ := os.ReadFile(outside); !bytes.Equal(raw, sampleGreeting) {
					t.Error("the stub wrote through a symbolic link")
				}
			})
		}, want: "not a bounded regular file"},
		"existing test file": {request: stubRequestJSON(t, nil), setup: func(t *testing.T, dir string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "target"), filepath.Join(dir, "greeting_test.go")); err != nil {
				t.Fatal(err)
			}
		}, want: "greeting_test.go already exists"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := writeSample(dir); err != nil {
				t.Fatal(err)
			}
			if c.setup != nil {
				c.setup(t, dir)
			}
			before, _ := os.ReadFile(filepath.Join(dir, "greeting.go"))
			t.Chdir(dir)
			var out, stderr bytes.Buffer
			code := ServeStub(c.args, strings.NewReader(c.request), &out, &stderr)
			if code == 0 || out.Len() != 0 || !strings.Contains(stderr.String(), c.want) {
				t.Fatalf("not refused with %q: code %d, stdout %q, stderr %q", c.want, code, out.String(), stderr.String())
			}
			if after, _ := os.ReadFile(filepath.Join(dir, "greeting.go")); !bytes.Equal(before, after) {
				t.Fatal("a refused request changed greeting.go")
			}
			if name != "existing test file" {
				if _, err := os.Lstat(filepath.Join(dir, "greeting_test.go")); !os.IsNotExist(err) {
					t.Fatal("a refused request wrote greeting_test.go")
				}
			}
		})
	}

	t.Run("pristine demo request", func(t *testing.T) {
		dir := t.TempDir()
		if err := writeSample(dir); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		var out, stderr bytes.Buffer
		if code := ServeStub(nil, strings.NewReader(stubRequestJSON(t, nil)), &out, &stderr); code != 0 {
			t.Fatalf("code %d: %s", code, stderr.String())
		}
		outcome, err := harness.DecodeCodingOutcome(out.Bytes())
		if err != nil || outcome.Outcome != "completed" {
			t.Fatalf("the stub did not print a valid v1 coding outcome: %v %+v", err, outcome)
		}
		for file, want := range map[string]string{"greeting.go": stubGreeting, "greeting_test.go": stubGreetingTest} {
			if raw, err := os.ReadFile(filepath.Join(dir, file)); err != nil || string(raw) != want {
				t.Fatalf("%s was not written as the canned change: %v", file, err)
			}
		}
	})
}

func TestCannedAnalysisAndReviewPassTheRealValidators(t *testing.T) {
	snapshot := readiness.RepositorySnapshot{Evidence: []readiness.FileEvidence{
		{Evidence: readiness.Evidence{ID: "file-1", Path: "go.mod"}}, {Evidence: readiness.Evidence{ID: "file-2", Path: "greeting.go"}},
	}}
	a, err := cannedAnalysis(context.Background(), formula.Binding{}, fixtureIssue(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range a.Checks {
		if c.Category != "candidate" && c.Category != "regression" {
			t.Fatalf("check %s has category %s", c.ID, c.Category)
		}
	}
	if len(a.Unknowns) != 0 || len(a.Questions) != 0 || len(a.EvidenceRequests) != 0 {
		t.Fatal("the canned analysis would keep the scope gate human")
	}
	if _, err = cannedAnalysis(context.Background(), formula.Binding{}, fixtureIssue(), readiness.RepositorySnapshot{}); err == nil {
		t.Fatal("the canned analysis accepted evidence that is not the sample's")
	}
	other := fixtureIssue()
	other.Repo = "acme/widgets"
	if _, err = cannedAnalysis(context.Background(), formula.Binding{}, other, snapshot); err == nil {
		t.Fatal("the canned analysis accepted another issue")
	}

	review, err := harness.DecodeReview([]byte(stubReview))
	if err != nil || review.HighFindings() != 0 || !slices.Equal(review.ReviewedFiles, scopePaths) {
		t.Fatalf("the canned review fails the real validator: %v %+v", err, review)
	}
	if r := cannedReview(context.Background(), formula.Binding{}, "", 0, map[string]any{"kind": "molecule"}, nil); r.OK {
		t.Fatal("the stub reviewer answered a non-review request")
	}
	if r := cannedReview(context.Background(), formula.Binding{}, "", 0, map[string]any{"kind": harness.ReviewKind}, nil); !r.OK || r.Stdout != stubReview {
		t.Fatalf("the stub reviewer did not answer the review request: %+v", r)
	}
}

func TestDemoFormulaBindsOnlyTheStub(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command, err := stubCommand(nil)
	if err != nil || !slices.Equal(command, []string{exe, StubArg}) {
		t.Fatalf("the default stub command is %v (%v)", command, err)
	}
	yaml, err := demoFormula(command)
	if err != nil {
		t.Fatal(err)
	}
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.ValidateReadiness(); err != nil {
		t.Fatal(err)
	}
	roles, err := f.ExecutionRoles()
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := roles[formula.ScopeApproval].DeclaresPolicy(); !ok || id != formula.PolicyAutoWhenReady {
		t.Fatalf("the demo scope gate declares %q", id)
	}
	if _, ok := roles[formula.ReviewApproval].DeclaresPolicy(); ok {
		t.Fatal("the demo review gate must stay human")
	}
	if coding, _ := f.RoleBinding(formula.CodingHarness); !slices.Equal(coding.Command, command) || len(f.Extensions) != 0 {
		t.Fatalf("the demo coding binding is %v", coding.Command)
	}
	for _, bad := range [][]string{{exe, "__adapter", "claude-code", "/bin/claude"}, {"forgecell", StubArg}, {exe, ""}, {filepath.Dir(exe), StubArg}} {
		if _, err := stubCommand(bad); err == nil {
			t.Fatalf("stub command %v was accepted", bad)
		}
	}
}

func TestDisplayBlockIsTerminalSafeAndBounded(t *testing.T) {
	got := displayBlock("+ok\n\x1b]0;title\x07\u202eevil\tend", 1000)
	if strings.ContainsAny(got, "\x1b\x07\u202e") || !strings.Contains(got, "+ok\n") || !strings.Contains(got, "\tend") {
		t.Fatalf("unsafe display: %q", got)
	}
	if cut := displayBlock(strings.Repeat("x", 100), 10); !strings.HasPrefix(cut, strings.Repeat("x", 10)+"\n[display cut at 10 bytes") {
		t.Fatalf("not cut with a notice: %q", cut)
	}
}
