package cli

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"github.com/DanielNetzer/forgecell/lab/internal/ui"
)

// The Lab UI's "Approve the requested scope change" builds its engine request
// with ui.ActionEngineRequest. On the same Lab state it records exactly the
// revision forgecell amend --add-scope PATH --reason TEXT records: the same plan
// digest. The ledger is restored between the two runs, so both start from the
// same latest plan.
func TestUIScopeChangeActionRecordsTheCLIRevisionDigest(t *testing.T) {
	m := gsSeedMolecule(t)
	before := gsRead(t, m.ledger())
	req, ok := ui.ActionEngineRequest(ui.ActionScopeChange, m.digest, []string{"src/new.go"}, "Helper needed")
	if !ok || req.ExpectParent != m.digest || req.Run {
		t.Fatalf("request %+v %t", req, ok)
	}
	r, err := molecule.AmendFromLatest(context.Background(), m.lab, m.record.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	fromUI := r.Readiness.Plans[len(r.Readiness.Plans)-1]
	for _, e := range r.Readiness.Events {
		if e.Kind == "approved" && e.Digest == fromUI.Digest {
			t.Fatal("the UI request approved its own revision")
		}
	}
	if err = os.WriteFile(m.ledger(), before, 0600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := gsRun([]string{"amend", m.record.ID, "--lab", m.lab, "--add-scope", "src/new.go", "--reason", "Helper needed"})
	if code != 0 {
		t.Fatalf("CLI amend failed: %d %s", code, errOut)
	}
	cli := gsLoadRecord(t, m.ledger())
	fromCLI := cli.Readiness.Plans[len(cli.Readiness.Plans)-1]
	if len(cli.Readiness.Plans) != 2 || fromCLI.Digest != fromUI.Digest || fromCLI.Plan.ParentDigest != m.digest {
		t.Fatalf("CLI digest %s, UI digest %s (%d plans)", fromCLI.Digest, fromUI.Digest, len(cli.Readiness.Plans))
	}
}

// Every UI action that records a revision sends the request the CLI flags
// build, plus only the digest the operator was shown: --accept-protected-failures,
// --add-scope PATH --reason TEXT for each path, and --continuation checks-only.
// None of them sets --run, so none approves.
func TestUIActionEngineRequestsAreTheCLIRequests(t *testing.T) {
	digest := "5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"
	for kind, cli := range map[string]molecule.AmendRequest{
		ui.ActionAcceptFailures: {AcceptProtectedFailures: true},
		ui.ActionScopeChange:    {AddScope: []molecule.ScopeAddition{{Path: "src/a.go", Reason: "Needed"}, {Path: "src/b.go", Reason: "Needed"}}},
		ui.ActionRerunChecks:    {Continuation: "checks-only"},
	} {
		got, ok := ui.ActionEngineRequest(kind, digest, []string{"src/a.go", "src/b.go"}, "Needed")
		if !ok || got.ExpectParent != digest || got.Run {
			t.Fatalf("%s: %+v %t", kind, got, ok)
		}
		if kind != ui.ActionScopeChange && len(got.AddScope) != 0 {
			t.Fatalf("%s carried scope paths: %+v", kind, got)
		}
		got.ExpectParent = ""
		if !reflect.DeepEqual(got, cli) {
			t.Fatalf("%s: %+v, want the CLI's %+v", kind, got, cli)
		}
	}
	for _, kind := range []string{ui.ActionRecover, ui.ActionFreshIntake, ui.ActionRetire, "approve"} {
		if _, ok := ui.ActionEngineRequest(kind, digest, nil, ""); ok {
			t.Fatalf("%s built an amend request", kind)
		}
	}
}

// parityLab is every file under a Lab except the coding checkouts and the
// advisory loop-lock files, so one engine path can be undone before the other
// runs from exactly the same state.
type parityLab struct {
	files map[string][]byte
	dirs  map[string]bool
}

func paritySkip(rel string) bool {
	return rel == "workspaces" || strings.HasPrefix(rel, "workspaces/") || strings.HasSuffix(rel, ".loop-lock")
}

func paritySave(t *testing.T, lab string) parityLab {
	t.Helper()
	s := parityLab{files: map[string][]byte{}, dirs: map[string]bool{}}
	err := filepath.WalkDir(lab, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(lab, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case paritySkip(rel):
			if d.IsDir() {
				return filepath.SkipDir
			}
		case d.IsDir():
			s.dirs[rel] = true
		default:
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			s.files[rel] = raw
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// parityRestore removes what was created since s and rewrites every saved file.
func parityRestore(t *testing.T, lab string, s parityLab) {
	t.Helper()
	err := filepath.WalkDir(lab, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(lab, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case paritySkip(rel):
			if d.IsDir() {
				return filepath.SkipDir
			}
		case d.IsDir() && !s.dirs[rel]:
			if e := os.RemoveAll(path); e != nil {
				return e
			}
			return filepath.SkipDir
		case !d.IsDir():
			if _, saved := s.files[rel]; !saved {
				return os.Remove(path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, raw := range s.files {
		gsWrite(t, filepath.Join(lab, filepath.FromSlash(rel)), string(raw), 0600)
	}
}

func parityLatest(t *testing.T, m gsMolecule) readiness.Proposal {
	t.Helper()
	r := gsLoadRecord(t, m.ledger())
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		t.Fatal("no plan")
	}
	return r.Readiness.Plans[len(r.Readiness.Plans)-1]
}

func parityIssue(context.Context, string, int64, string) (molecule.Issue, error) {
	return molecule.Issue{Number: 12, Repo: "acme/widgets", Title: "Change code", Body: "Write changed", URL: "https://github.com/acme/widgets/issues/12", State: "OPEN"}, nil
}

const parityCompleted = `{"schemaVersion":"v1","outcome":"completed","reason":"Implemented and tested","paths":[]}`

// paritySeed is gsSeedWith with extra repository files, its own harness script
// and analysis additions: a real Molecule waiting at the scope gate.
func paritySeed(t *testing.T, files map[string]string, harness string, extend func(a *readiness.Analysis, evidence map[string]string)) gsMolecule {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	gsWrite(t, filepath.Join(repo, "code.txt"), "base", 0600)
	gsWrite(t, filepath.Join(repo, "package.json"), `{"scripts":{"test":"test x = x"}}`, 0600)
	for path, body := range files {
		gsWrite(t, filepath.Join(repo, filepath.FromSlash(path)), body, 0600)
	}
	harnessPath := filepath.Join(root, "harness")
	issuePath := filepath.Join(root, "issue.json")
	tools := gsEnv(t, "\"issue view\"*) cat '"+issuePath+"'; exit 0;;\n")
	gsGit(t, repo, "init", "-q", "-b", "main")
	gsGit(t, repo, "config", "user.name", "Test")
	gsGit(t, repo, "config", "user.email", "test@example.test")
	gsGit(t, repo, "add", ".")
	gsGit(t, repo, "commit", "-qm", "base")
	gsGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	lab := filepath.Join(root, "lab")
	gsWrite(t, harnessPath, harness, 0700)
	gsWrite(t, issuePath, gsIssueJSON("Write changed"), 0600)
	t.Chdir(repo)
	command, err := json.Marshal([]string{harnessPath})
	if err != nil {
		t.Fatal(err)
	}
	gsApproveFormula(t, lab, gsMoleculeYAML(string(command), gsScopeAtom, gsReviewAtom))
	record, err := molecule.Run(context.Background(), molecule.Options{LabDir: lab, SourceRoot: repo, Issue: "12", TargetBranch: "main", ReadIssue: parityIssue,
		Analyze: func(_ context.Context, _ formula.Binding, _ molecule.Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
			evidence := map[string]string{}
			for _, e := range s.Evidence {
				evidence[e.Path] = e.ID
			}
			id := evidence["package.json"]
			a := readiness.Analysis{Summary: "Change code", Scope: []readiness.ScopedPath{{Path: "code.txt", Reason: "Implementation", Evidence: []string{"issue"}}, {Path: "request.json", Reason: "Fixture harness trace", Evidence: []string{id}}}, Acceptance: []readiness.Criterion{{Description: "Code contains changed", Evidence: []string{"issue"}}}, Checks: []readiness.Check{{ID: "check", Category: "candidate", Argv: []string{"/bin/sh", "-c", "test \"$(cat code.txt)\" = changed"}, Dir: ".", TimeoutMS: 5000, Required: true, Definitions: []string{id}, Reason: "Fixture acceptance", Evidence: []string{"issue", id}}}}
			if extend != nil {
				extend(&a, evidence)
			}
			return a, nil
		}})
	if err != nil || record.Readiness == nil || len(record.Readiness.Plans) != 1 || record.Readiness.Phase != "scope-waiting" {
		t.Fatalf("scope-waiting Molecule was not seeded: %v %+v", err, record)
	}
	return gsMolecule{root: root, repo: repo, lab: lab, harnessPath: harnessPath, issuePath: issuePath, record: record, digest: record.Readiness.Plans[0].Digest, tools: tools}
}

// parityApprove approves the seeded plan and runs it, through the engine the
// CLI's run --approve calls.
func parityApprove(t *testing.T, m gsMolecule, guard *resources.Guard) molecule.Record {
	t.Helper()
	r, err := molecule.Run(context.Background(), molecule.Options{LabDir: m.lab, SourceRoot: m.repo, Issue: "12", TargetBranch: "main", ReadIssue: parityIssue, Approve: m.digest, Resources: guard})
	if err != nil {
		t.Fatalf("approved run: %v", err)
	}
	return r
}

func parityOffered(t *testing.T, m gsMolecule, r molecule.Record, kind string) molecule.NextAction {
	t.Helper()
	for _, a := range molecule.NextActions(context.Background(), m.lab, r, true, time.Minute) {
		if a.Kind == kind {
			return a
		}
	}
	t.Fatalf("%s is not offered", kind)
	return molecule.NextAction{}
}

// parityGo answers `go test` from the pinned test source the check runs
// against: the candidate's says "new" and passes, the base version the
// protected view restores says "old" and fails TestX, by design. No Go
// toolchain runs and nothing depends on time.
const parityGo = `#!/bin/sh
[ "$1" = test ] || exit 2
pkg=example.test/lab/pin
json=no
for a in "$@"; do
	[ "$a" = -json ] && json=yes
done
if grep -q new pin/pin_test.go; then
	if [ $json = yes ]; then
		printf '{"Action":"run","Package":"%s","Test":"TestX"}\n{"Action":"pass","Package":"%s","Test":"TestX"}\n{"Action":"pass","Package":"%s"}\n' "$pkg" "$pkg" "$pkg"
	else
		printf 'ok  \t%s\t0.01s\n' "$pkg"
	fi
	exit 0
fi
if [ $json = yes ]; then
	printf '{"Action":"run","Package":"%s","Test":"TestX"}\n{"Action":"fail","Package":"%s","Test":"TestX"}\n{"Action":"fail","Package":"%s"}\n' "$pkg" "$pkg" "$pkg"
else
	printf -- '--- FAIL: TestX\nFAIL\t%s\t0.01s\n' "$pkg"
fi
exit 1
`

// Accept by-design test failures: on a Molecule whose new code passed and
// whose protected regression run failed only the pinned test, the UI's engine
// request records exactly the revision forgecell amend
// --accept-protected-failures records from the same restored Lab, and that
// revision declares exactly the tests the UI preview listed.
func TestUIAcceptFailuresActionRecordsTheCLIRevisionDigest(t *testing.T) {
	const pinned = "lab/pin/pin_test.go"
	harness := "#!/bin/sh\ncat > request.json\nprintf changed > code.txt\nprintf 'package pin\\n\\n// pinned: new\\n' > " + pinned + "\nprintf '%s' '" + parityCompleted + "'\n"
	m := paritySeed(t, map[string]string{"lab/go.mod": "module example.test/lab\n", pinned: "package pin\n\n// pinned: old\n"}, harness, func(a *readiness.Analysis, evidence map[string]string) {
		manifest := evidence["lab/go.mod"]
		a.Scope = append(a.Scope, readiness.ScopedPath{Path: pinned, Reason: "The pinned behaviour changes by design", Evidence: []string{"issue"}})
		a.Checks = append(a.Checks, readiness.Check{ID: "go-test-lab", Category: "regression", Argv: []string{"go", "test", "-json", "./..."}, Dir: "lab", TimeoutMS: 60000, Required: true, Definitions: []string{manifest}, Reason: "Module tests", Evidence: []string{"issue", manifest}})
	})
	bin := t.TempDir()
	gsWrite(t, filepath.Join(bin, "go"), parityGo, 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if p, err := exec.LookPath("go"); err != nil || p != filepath.Join(bin, "go") {
		t.Fatalf("the fake go is not first on PATH: %s %v", p, err)
	}
	r := parityApprove(t, m, nil)
	if r.Verification == nil || r.Verification.Protected == nil || r.Verification.Protected.RequiredChecksPassed {
		t.Fatalf("fixture did not fail only in the protected view: %+v", r.Verification)
	}
	a := parityOffered(t, m, r, molecule.NextAcceptProtectedFailures)
	if a.Digest != m.digest || len(a.Deltas) != 1 || a.Deltas[0].Test != "TestX" || a.Deltas[0].ScopePath != pinned {
		t.Fatalf("offered %+v", a)
	}
	saved := paritySave(t, m.lab)
	req, _ := ui.ActionEngineRequest(ui.ActionAcceptFailures, a.Digest, nil, "")
	if _, err := molecule.AmendFromLatest(context.Background(), m.lab, m.record.ID, req); err != nil {
		t.Fatal(err)
	}
	fromUI := parityLatest(t, m)
	parityRestore(t, m.lab, saved)
	if code, _, errOut := gsRun([]string{"amend", m.record.ID, "--lab", m.lab, "--accept-protected-failures"}); code != 0 {
		t.Fatalf("CLI amend failed: %d %s", code, errOut)
	}
	fromCLI := parityLatest(t, m)
	if fromCLI.Digest != fromUI.Digest || fromCLI.Plan.ParentDigest != m.digest || fromCLI.Plan.Revision != 2 {
		t.Fatalf("CLI digest %s, UI digest %s", fromCLI.Digest, fromUI.Digest)
	}
	// The digest the page binds is the list the CLI's revision declares.
	if ui.DeltasDigest(fromCLI.Plan.ExpectedProtectedDeltas) != ui.DeltasDigest(a.Deltas) {
		t.Fatalf("declared %+v, previewed %+v", fromCLI.Plan.ExpectedProtectedDeltas, a.Deltas)
	}
}

// Approve the requested scope change: on a Molecule whose coding harness asked
// for one more path, the UI's request with the offered paths records exactly
// the revision forgecell amend --add-scope PATH --reason TEXT records.
func TestUIScopeChangeOutcomeActionRecordsTheCLIRevisionDigest(t *testing.T) {
	harness := "#!/bin/sh\ncat > request.json\nprintf partial > code.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"scope-change\",\"reason\":\"Helper needed\",\"paths\":[\"src/new.go\"]}'\n"
	m := paritySeed(t, nil, harness, nil)
	r := parityApprove(t, m, nil)
	a := parityOffered(t, m, r, molecule.NextApproveScopeChange)
	if !reflect.DeepEqual(a.Paths, []string{"src/new.go"}) || a.Reason != "Helper needed" || a.Digest != m.digest {
		t.Fatalf("offered %+v", a)
	}
	saved := paritySave(t, m.lab)
	req, _ := ui.ActionEngineRequest(ui.ActionScopeChange, a.Digest, a.Paths, a.Reason)
	if _, err := molecule.AmendFromLatest(context.Background(), m.lab, m.record.ID, req); err != nil {
		t.Fatal(err)
	}
	fromUI := parityLatest(t, m)
	parityRestore(t, m.lab, saved)
	if code, _, errOut := gsRun([]string{"amend", m.record.ID, "--lab", m.lab, "--add-scope", "src/new.go", "--reason", "Helper needed"}); code != 0 {
		t.Fatalf("CLI amend failed: %d %s", code, errOut)
	}
	if fromCLI := parityLatest(t, m); fromCLI.Digest != fromUI.Digest || fromCLI.Plan.ParentDigest != m.digest {
		t.Fatalf("CLI digest %s, UI digest %s", fromCLI.Digest, fromUI.Digest)
	}
}

// Re-run checks on the same code, from a Molecule stopped below the resource
// floor before its checks: the UI recovers first and then amends checks-only,
// exactly as forgecell recover --plan DIGEST --confirm-stopped and amend
// --continuation checks-only do, from the same restored Lab; and from the
// recovered state, the plain checks-only action records the CLI's revision too.
func TestUIRerunChecksActionRecordsTheCLIRevisionDigest(t *testing.T) {
	m := paritySeed(t, nil, "#!/bin/sh\ncat > request.json\nprintf changed > code.txt\nprintf '%s' '"+parityCompleted+"'\n", nil)
	low := resources.Readings{Volumes: []resources.Volume{{Path: "/lab", Free: resources.GiB, Total: 100 * resources.GiB}}, Memory: 8 * resources.GiB, MemoryKnown: true}
	guard := &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings { return low }), Sleep: func(context.Context, time.Duration) bool { return true }}
	r := parityApprove(t, m, guard)
	if r.Status != "blocked" || len(r.VerificationAttempts) != 0 {
		t.Fatalf("fixture %s with %d verification attempts", r.Status, len(r.VerificationAttempts))
	}
	a := parityOffered(t, m, r, molecule.NextRerunChecks)
	if !a.RecoverFirst || a.Digest != m.digest {
		t.Fatalf("offered %+v", a)
	}
	pending := paritySave(t, m.lab)
	// The UI path: recover with the confirmation, then the checks-only request.
	if _, err := molecule.Recover(context.Background(), m.lab, m.record.ID, a.Digest, true); err != nil {
		t.Fatal(err)
	}
	recovered := paritySave(t, m.lab)
	if b := parityOffered(t, m, gsLoadRecord(t, m.ledger()), molecule.NextRerunChecks); b.RecoverFirst || b.Digest != a.Digest {
		t.Fatalf("after recovery %+v", b)
	}
	req, _ := ui.ActionEngineRequest(ui.ActionRerunChecks, a.Digest, nil, "")
	if _, err := molecule.AmendFromLatest(context.Background(), m.lab, m.record.ID, req); err != nil {
		t.Fatal(err)
	}
	fromUI := parityLatest(t, m)
	if fromUI.Plan.Continuation != "checks-only" || fromUI.Plan.ParentDigest != m.digest {
		t.Fatalf("UI revision %+v", fromUI.Plan)
	}
	// The CLI path from the same check-pending Lab.
	parityRestore(t, m.lab, pending)
	if code, _, errOut := gsRun([]string{"recover", m.record.ID, "--lab", m.lab, "--plan", a.Digest, "--confirm-stopped"}); code != 0 {
		t.Fatalf("CLI recover failed: %d %s", code, errOut)
	}
	if code, _, errOut := gsRun([]string{"amend", m.record.ID, "--lab", m.lab, "--continuation", "checks-only"}); code != 0 {
		t.Fatalf("CLI amend failed: %d %s", code, errOut)
	}
	if fromCLI := parityLatest(t, m); fromCLI.Digest != fromUI.Digest {
		t.Fatalf("recover first: CLI digest %s, UI digest %s", fromCLI.Digest, fromUI.Digest)
	}
	// Plain checks-only from the recovered Lab: CLI first, then the UI request.
	parityRestore(t, m.lab, recovered)
	if code, _, errOut := gsRun([]string{"amend", m.record.ID, "--lab", m.lab, "--continuation", "checks-only"}); code != 0 {
		t.Fatalf("CLI amend failed: %d %s", code, errOut)
	}
	fromCLI := parityLatest(t, m)
	parityRestore(t, m.lab, recovered)
	if _, err := molecule.AmendFromLatest(context.Background(), m.lab, m.record.ID, req); err != nil {
		t.Fatal(err)
	}
	if again := parityLatest(t, m); again.Digest != fromCLI.Digest || again.Digest != fromUI.Digest {
		t.Fatalf("checks-only: CLI digest %s, UI digest %s", fromCLI.Digest, again.Digest)
	}
}
