package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/evaluation"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/learning"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// The gate scenario suite is candidate evidence, not independent acceptance. Every
// row builds Lab state under a temporary directory, runs one CLI command in
// process and expects allow (exit 0), hold (exit 0 without passing the gate) or
// deny (non-zero exit, an actionable message on stderr, nothing on stdout). Deny
// and hold rows must leave the Lab, the source repository's refs and the
// workspace tree unchanged and must never call gh beyond a read of the issue.
// Fixtures are hermetic: no global git configuration, no tokens, a shadow gh that
// records every call and a shadow git whose fetch and push do nothing.
//
// Add a row here whenever an approval, digest or handoff gate is added or changed,
// and register a new gate command in gsGates.

// gsGates maps every CLI command that enforces an approval or digest gate to a
// description. There is no distinct "handoff" command at this commit: handoff to a
// reviewer is the draft-PR publication that "deliver --approve" gates, and the
// Lab loop never delivers.
var gsGates = map[string]string{
	"init --approve":       "Formula approval (proposal digest, Lab baseline, exact active Formula)",
	"init --dismiss":       "Formula proposal dismissal (exact saved proposal, never after approval)",
	"init --rebind":        "binding-only rebind proposal, never combined with an approval or another change",
	"run --approve":        "scope plan approval (exact plan digest, Formula, issue and binding identity)",
	"amend":                "amendment bound to the previous plan digest and its Molecule",
	"recover":              "recovery bound to the interrupted plan digest and its Molecule",
	"loop":                 "scope policy approval; the loop never approves a human gate or delivers",
	"deliver --approve":    "delivery approval and handoff to a draft PR (exact preview digest)",
	"checks":               "CI evidence bound to the published delivery identity",
	"suggestion --approve": "Formula suggestion approval (exact saved hashes)",
	"evaluate --approve":   "evaluation activation (frozen plan digest)",
}

const (
	gsAllow = iota
	gsHold
	gsDeny
)

type gsCase struct {
	lab     string
	args    []string
	tools   gsTools
	ghReads bool
	repos   []string
	trees   []string
	after   func(t *testing.T)
}

type gsRow struct {
	name  string
	gate  string
	mode  int
	want  string
	setup func(t *testing.T) gsCase
}

func gsRun(args []string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, strings.NewReader(""), &out, &errOut, "gate-suite")
	return code, out.String(), errOut.String()
}

type gsTools struct{ log string }

func (g gsTools) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(g.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// gsEnv makes the process hermetic: no harness or Lab signal, no tokens, no global
// or system git configuration. A shadow gh (recording every call and answering
// only the supplied shell case branches) and a shadow git (fetch and push do
// nothing, everything else runs the real git) come first on PATH.
func gsEnv(t *testing.T, ghCases string) gsTools {
	t.Helper()
	for _, key := range []string{"CLAUDECODE", "CODEX_THREAD_ID", "CURSOR_AGENT", "TERM_PROGRAM", "FORGECELL_LAB", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(key, "")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "gh.log")
	gsWrite(t, filepath.Join(bin, "git"), "#!/bin/sh\ncase \"$1\" in fetch|push) exit 0;; esac\nexec '"+realGit+"' \"$@\"\n", 0700)
	gsWrite(t, filepath.Join(bin, "gh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+log+"'\ncase \"$*\" in\n"+ghCases+"esac\nexit 1\n", 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return gsTools{log: log}
}

func gsGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func gsWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func gsRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func gsAppend(t *testing.T, path, text string) {
	t.Helper()
	gsWrite(t, path, string(gsRead(t, path))+text, 0700)
}

func gsHash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func gsLoadRecord(t *testing.T, file string) molecule.Record {
	t.Helper()
	var r molecule.Record
	if err := json.Unmarshal(gsRead(t, file), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// gsSnapshot records every file hash and directory under root, except advisory
// flock files (never removed) and the contents of checkouts under workspaces.
func gsSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case rel == "workspaces":
			snapshot[rel] = "dir"
			return filepath.SkipDir
		case strings.HasSuffix(rel, ".loop-lock"):
			return nil
		case d.IsDir():
			snapshot[rel] = "dir"
		case d.Type()&fs.ModeSymlink != 0:
			snapshot[rel] = "symlink"
		default:
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			snapshot[rel] = gsHash(string(raw))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func gsDiff(before, after map[string]string) string {
	var changes []string
	for path, was := range before {
		if now, ok := after[path]; !ok {
			changes = append(changes, "removed "+path)
		} else if now != was {
			changes = append(changes, "changed "+path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			changes = append(changes, "created "+path)
		}
	}
	sort.Strings(changes)
	return strings.Join(changes, ", ")
}

// gsRefs identifies a repository's branches and HEAD.
func gsRefs(t *testing.T, dir string) string {
	t.Helper()
	return gsGit(t, dir, "for-each-ref", "--format=%(refname) %(objectname)") + "|" + gsGit(t, dir, "rev-parse", "HEAD")
}

// gsTree hashes every file of a checkout outside .git; a missing checkout is absent.
func gsTree(t *testing.T, dir string) string {
	t.Helper()
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return "absent"
	}
	sum := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			fmt.Fprintf(sum, "%s|%v\n", rel, d.Type())
			return nil
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		fmt.Fprintf(sum, "%s|%s\n", rel, gsHash(string(raw)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sum.Sum(nil))
}

type gsState struct {
	lab   map[string]string
	repos []string
	trees []string
}

func gsCapture(t *testing.T, c gsCase) gsState {
	t.Helper()
	s := gsState{lab: gsSnapshot(t, c.lab)}
	for _, repo := range c.repos {
		s.repos = append(s.repos, gsRefs(t, repo))
	}
	for _, tree := range c.trees {
		s.trees = append(s.trees, gsTree(t, tree))
	}
	return s
}

func gsStateDiff(before, after gsState) string {
	changes := gsDiff(before.lab, after.lab)
	for i := range before.repos {
		if before.repos[i] != after.repos[i] {
			changes += " source repository refs changed"
		}
	}
	for i := range before.trees {
		if before.trees[i] != after.trees[i] {
			changes += " workspace tree changed"
		}
	}
	return strings.TrimSpace(changes)
}

// gsApproveFormula writes the Formula and records the exact-byte approval a
// completed "init --approve" leaves behind.
func gsApproveFormula(t *testing.T, lab, yaml string) {
	t.Helper()
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	gsWrite(t, filepath.Join(lab, "formulas", f.ID+".yaml"), yaml, 0600)
	cfg, err := formula.FileHash(filepath.Join(lab, "lab.json"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := formula.Inspect(lab, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision := "gs-" + f.ID
	a := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: decision, SourceKind: "test-review", SourceID: decision}
	if err = formula.BeginActivation(lab, yaml, a, cfg, target.Formula.SHA256); err != nil {
		t.Fatal(err)
	}
	if err = formula.ApplyActivation(lab, a); err != nil {
		t.Fatal(err)
	}
	if err = formula.CompleteActivation(lab, a); err != nil {
		t.Fatal(err)
	}
}

const gsMinimalFormula = "kind: formula\nid: sample\natoms: [{type: harness}]\n"

// gsProposal saves one pending Formula proposal in an empty Lab.
func gsProposal(t *testing.T) (lab, id string, tools gsTools) {
	t.Helper()
	tools = gsEnv(t, "")
	lab = t.TempDir()
	p, err := onboarding.SaveProposal(lab, gsMinimalFormula)
	if err != nil {
		t.Fatal(err)
	}
	return lab, p.ID, tools
}

const (
	gsScopeAtom    = "{id: scope, type: gate, purpose: scope}"
	gsReviewAtom   = "{id: gate, type: gate, purpose: review}"
	gsPolicyScope  = "{id: scope, type: gate, purpose: scope, mode: policy, policy: auto-when-ready}"
	gsPolicyReview = "{id: gate, type: gate, purpose: review, mode: policy, policy: deliver-when-checks-pass}"
)

func gsMoleculeYAML(command, scope, review string) string {
	return "schemaVersion: v0\nkind: formula\nid: sample\nintake: {source: github-issues, repo: acme/widgets}\nharness:\n  binding: custom\n  command: " + command + "\natoms:\n  - {id: intake, type: intake}\n  - " + scope + "\n  - {id: harness, type: harness}\n  - {id: check, type: check}\n  - " + review + "\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"
}

func gsIssueJSON(body string) string {
	return fmt.Sprintf(`{"number":12,"title":"Change code","body":%q,"url":"https://github.com/acme/widgets/issues/12","state":"OPEN"}`, body)
}

type gsMolecule struct {
	root, repo, lab, harnessPath, issuePath string
	record                                  molecule.Record
	digest                                  string
	tools                                   gsTools
}

// gsSeedMolecule builds a real Molecule waiting at the scope gate: an approved
// Formula, a local git checkout whose origin names acme/widgets, a fake harness
// and a shadow gh that serves the issue from a file. Analysis and the first issue
// read use hooks, so no provider is started.
func gsSeedMolecule(t *testing.T) gsMolecule {
	return gsSeedWith(t, gsScopeAtom, gsReviewAtom, false)
}

func gsSeedWith(t *testing.T, scopeAtom, reviewAtom string, policyCheck bool) gsMolecule {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	gsWrite(t, filepath.Join(repo, "code.txt"), "base", 0600)
	gsWrite(t, filepath.Join(repo, "package.json"), `{"scripts":{"test":"test x = x"}}`, 0600)
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
	gsWrite(t, harnessPath, "#!/bin/sh\ncat > request.json\nprintf changed > code.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Implemented and tested\",\"paths\":[]}'\n", 0700)
	gsWrite(t, issuePath, gsIssueJSON("Write changed"), 0600)
	t.Chdir(repo)
	command, err := json.Marshal([]string{harnessPath})
	if err != nil {
		t.Fatal(err)
	}
	gsApproveFormula(t, lab, gsMoleculeYAML(string(command), scopeAtom, reviewAtom))
	checkArgv := []string{"/bin/sh", "-c", "test \"$(cat code.txt)\" = changed"}
	if policyCheck {
		checkArgv = []string{"/bin/sh", "-c", "test -f package.json"}
	}
	record, err := molecule.Run(context.Background(), molecule.Options{LabDir: lab, SourceRoot: repo, Issue: "12", TargetBranch: "main",
		Analyze: func(_ context.Context, _ formula.Binding, _ molecule.Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
			id := ""
			for _, e := range s.Evidence {
				if e.Path == "package.json" {
					id = e.ID
				}
			}
			return readiness.Analysis{Summary: "Change code", Scope: []readiness.ScopedPath{{Path: "code.txt", Reason: "Implementation", Evidence: []string{"issue"}}, {Path: "request.json", Reason: "Fixture harness trace", Evidence: []string{id}}}, Acceptance: []readiness.Criterion{{Description: "Code contains changed", Evidence: []string{"issue"}}}, Checks: []readiness.Check{{ID: "check", Category: "candidate", Argv: checkArgv, Dir: ".", TimeoutMS: 5000, Required: true, Definitions: []string{id}, Reason: "Fixture acceptance", Evidence: []string{"issue", id}}}}, nil
		},
		ReadIssue: func(context.Context, string, int64, string) (molecule.Issue, error) {
			return molecule.Issue{Number: 12, Repo: "acme/widgets", Title: "Change code", Body: "Write changed", URL: "https://github.com/acme/widgets/issues/12", State: "OPEN"}, nil
		}})
	if err != nil || record.Readiness == nil || len(record.Readiness.Plans) != 1 || record.Readiness.Phase != "scope-waiting" {
		t.Fatalf("scope-waiting Molecule was not seeded: %v %+v", err, record)
	}
	return gsMolecule{root: root, repo: repo, lab: lab, harnessPath: harnessPath, issuePath: issuePath, record: record, digest: record.Readiness.Plans[0].Digest, tools: tools}
}

func (m gsMolecule) c(args ...string) gsCase {
	return gsCase{lab: m.lab, args: args, tools: m.tools, ghReads: true, repos: []string{m.repo}, trees: []string{m.record.Workspace.Path}}
}

func (m gsMolecule) approve() gsCase {
	return m.c("run", "12", "--lab", m.lab, "--approve", m.digest)
}

func (m gsMolecule) ledger() string {
	return filepath.Join(m.lab, "ledgers", m.record.ID+".json")
}

func (m gsMolecule) lock(t *testing.T) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(m.lab, "ledgers", m.record.ID+".lock"), 0700); err != nil {
		t.Fatal(err)
	}
}

// gsAmend writes the current plan as the successor payload; the CLI derives the
// revision, parent and lifecycle fields from the Lab.
func gsAmend(t *testing.T, m gsMolecule, id, parent string) gsCase {
	t.Helper()
	raw, err := json.Marshal(m.record.Readiness.Plans[0].Plan)
	if err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(m.root, "plan.json")
	gsWrite(t, plan, string(raw), 0600)
	return m.c("amend", id, "--lab", m.lab, "--parent", parent, "--plan", plan)
}

// gsStubLoop lists only the seeded issue, so the loop never needs gh to list.
func gsStubLoop(t *testing.T) {
	t.Helper()
	previous := runLoop
	runLoop = func(ctx context.Context, lo molecule.LoopOptions) (molecule.LoopReport, error) {
		lo.ListIssues = func(context.Context, string) (intake.OpenIssues, error) {
			return intake.OpenIssues{Issues: []intake.OpenIssue{{Repository: "acme/widgets", Number: 12, Title: "Change code", URL: "https://github.com/acme/widgets/issues/12", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}}, nil
		}
		return molecule.RunLoop(ctx, lo)
	}
	t.Cleanup(func() { runLoop = previous })
}

const gsDeliveryFormula = "schemaVersion: v0\nkind: formula\nid: delivery-fixture\nintake:\n  source: github-issues\n  repo: owner/repo\nharness:\n  binding: unbound\natoms:\n  - id: intake\n    type: intake\n  - id: scope\n    type: gate\n    purpose: scope\n  - id: coding\n    type: harness\n  - id: checks\n    type: check\n  - id: review\n    type: gate\n    purpose: review\n  - id: publication\n    type: ship\n  - id: documentation\n    type: document\n"

// gsPRFlow answers the draft-PR calls of a successful publication.
const gsPRFlow = "\"pr list\"*) printf '[]'; exit 0;;\n\"pr create\"*) printf 'https://github.com/owner/repo/pull/7'; exit 0;;\n\"pr view\"*) printf '{\"url\":\"https://github.com/owner/repo/pull/7\",\"state\":\"OPEN\",\"baseRefName\":\"main\",\"headRefName\":\"forgecell/fixture\",\"headRefOid\":\"%s\",\"isDraft\":true,\"headRepository\":{\"name\":\"repo\"},\"headRepositoryOwner\":{\"login\":\"owner\"}}' \"$(git rev-parse HEAD)\"; exit 0;;\n"

type gsFinished struct {
	lab, root, workspace string
	tools                gsTools
}

// gsFinishedLab builds a finished, verified Molecule with a separate worktree, as
// a completed run leaves it, so delivery gates can be exercised offline.
func gsFinishedLab(t *testing.T, unverified bool, ghCases string) gsFinished {
	t.Helper()
	tools := gsEnv(t, ghCases)
	f, err := formula.Parse([]byte(gsDeliveryFormula))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.ValidateReadiness(); err != nil {
		t.Fatal(err)
	}
	atoms := make([]molecule.Atom, len(f.Atoms))
	for i, a := range f.Atoms {
		status := "done"
		switch a.ID {
		case "review":
			status = "waiting"
		case "publication", "documentation":
			status = "skipped"
		}
		atoms[i] = molecule.Atom{ID: a.ID, Type: a.Type, Status: status}
	}
	root := t.TempDir()
	gsWrite(t, filepath.Join(root, "a.txt"), "before\n", 0600)
	gsGit(t, root, "init", "-q", "-b", "main")
	gsGit(t, root, "config", "user.email", "test@example.com")
	gsGit(t, root, "config", "user.name", "Test")
	gsGit(t, root, "add", "a.txt")
	gsGit(t, root, "commit", "-qm", "base")
	sha := gsGit(t, root, "rev-parse", "HEAD")
	gsGit(t, root, "update-ref", "refs/remotes/origin/main", sha)
	gsGit(t, root, "remote", "add", "origin", "https://github.com/owner/repo.git")
	lab := t.TempDir()
	workspace := filepath.Join(lab, "workspaces", "fixture")
	gsGit(t, root, "worktree", "add", "-q", "-b", "forgecell/fixture", workspace, sha)
	r := molecule.Record{SchemaVersion: "v0", Kind: "molecule", ID: "mol-fixture", Status: "waiting", FormulaApproved: true, FormulaID: f.ID, FormulaSnapshot: molecule.Snapshot{YAML: f.YAML, SHA256: f.SHA256}, FinishedAt: "2026-09-29T00:00:00Z", Workspace: molecule.Workspace{SourceRoot: root, Path: workspace, Repo: "owner/repo", Branch: "forgecell/fixture", BaseCommit: sha}, Issue: molecule.Issue{Repo: "owner/repo", Number: 9}, Atoms: atoms}
	gsWrite(t, filepath.Join(workspace, "a.txt"), "after\n", 0600)
	p := readiness.Plan{SchemaVersion: "v1", MoleculeID: r.ID, Revision: 1, Inputs: readiness.Inputs{Repository: "owner/repo", TargetBranch: "main", BaseCommit: sha, FormulaSHA256: f.SHA256, EvidenceSHA256: strings.Repeat("b", 64), BindingSHA256: strings.Repeat("c", 64), Issue: readiness.Issue{Repository: "owner/repo", Number: 9, Title: "Change a", URL: "https://github.com/owner/repo/issues/9", State: "OPEN"}}, Evidence: []readiness.Evidence{{ID: "source", Path: "a.txt", SHA256: gsHash("before\n")}}, Analysis: readiness.Analysis{Summary: "Change a", Scope: []readiness.ScopedPath{{Path: "a.txt", Reason: "Requested change", Evidence: []string{"issue"}}}, Acceptance: []readiness.Criterion{{Description: "a contains after", Evidence: []string{"issue"}}}, Checks: []readiness.Check{{ID: "acceptance", Category: "independent-acceptance", IndependentProvenance: "Explicit delivery test fixture", Argv: []string{"test", "ok"}, Dir: ".", TimeoutMS: 1000, Required: true, Definitions: []string{"source"}, Reason: "Fixture", Evidence: []string{"issue"}}}}}
	s, err := readiness.NewState(p, "now")
	if err != nil {
		t.Fatal(err)
	}
	d, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if s, err = s.Approve(d, p.Inputs, "approve"); err != nil {
		t.Fatal(err)
	}
	if s, err = s.StartAttempt("start"); err != nil {
		t.Fatal(err)
	}
	if s, err = s.FinishAttempt("check-pending", "captured", "end"); err != nil {
		t.Fatal(err)
	}
	if s, err = s.RecordVerification(true, "fixture verified", "checked"); err != nil {
		t.Fatal(err)
	}
	r.Readiness = &s
	c, err := verification.Capture(context.Background(), workspace, sha, []string{"a.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.LabDir = lab
	r.Capture = &c
	report := `{"schemaVersion":"v1","outcome":"completed","reason":"Implemented and tested","paths":[]}`
	coding := harness.CodingOutcomeFromResult(report)
	result := process.Result{OK: true, Reconciled: true, Stdout: report}
	evidence, err := json.Marshal(struct {
		process.Result
		AttemptID  string `json:"attemptId"`
		PlanDigest string `json:"planDigest"`
	}{result, "completed", d})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.ToSlash(filepath.Join("verification-history", r.ID, "harness-completed-result.json"))
	gsWrite(t, filepath.Join(lab, path), string(evidence), 0600)
	ref := molecule.VerificationFile{Path: path, SHA256: gsHash(string(evidence))}
	result.Stdout = ""
	r.HarnessAttempts = []molecule.HarnessAttempt{{ID: "completed", PlanDigest: d, Result: result, Coding: &coding, Evidence: &ref, Capture: &c}}
	r.Verification = &verification.Result{SourceTree: c.Tree, RequiredChecksPassed: true, IndependentAcceptanceVerified: !unverified, Checks: []verification.Observation{{ID: "acceptance", Category: "independent-acceptance", SourceTree: c.Tree, Command: p.Analysis.Checks[0], Result: process.Result{OK: true}}}}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	gsWrite(t, filepath.Join(lab, "ledgers", r.ID+".json"), string(raw), 0600)
	return gsFinished{lab: lab, root: root, workspace: workspace, tools: tools}
}

func (f gsFinished) c(args ...string) gsCase {
	return gsCase{lab: f.lab, args: args, tools: f.tools, repos: []string{f.root}, trees: []string{f.workspace}}
}

func gsPreview(lab string, extra ...string) []string {
	return append(append([]string{"deliver", "mol-fixture", "--lab", lab, "--base", "main"}, extra...), "--", "a.txt")
}

// digest reviews the delivery preview and returns its exact digest.
func (f gsFinished) digest(t *testing.T) string {
	t.Helper()
	code, stdout, stderr := gsRun(gsPreview(f.lab))
	var p delivery.PreviewResult
	if code != 0 || json.Unmarshal([]byte(stdout), &p) != nil || p.Digest == "" {
		t.Fatalf("preview failed: %d %s %s", code, stdout, stderr)
	}
	return p.Digest
}

// gsPublishedDelivery writes the receipt and ledger of a published delivery and a
// shadow gh that reports one passing check for its exact commit.
func gsPublishedDelivery(t *testing.T, tamper, locked bool) gsCase {
	t.Helper()
	dir := t.TempDir()
	head := strings.Repeat("a", 40)
	p := delivery.PreviewResult{MoleculeID: "mol-ci", Repo: "owner/repo"}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p.Digest = gsHash(string(raw))
	if tamper {
		p.Tree = "changed-after-review"
	}
	receipt, err := json.Marshal(delivery.Receipt{State: "published", Preview: p, Commit: head, URL: "https://github.com/owner/repo/pull/9"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(molecule.Record{ID: "mol-ci", Issue: molecule.Issue{Repo: p.Repo}, Verification: &verification.Result{}})
	if err != nil {
		t.Fatal(err)
	}
	gsWrite(t, filepath.Join(dir, "deliveries", "mol-ci.json"), string(receipt), 0600)
	gsWrite(t, filepath.Join(dir, "ledgers", "mol-ci.json"), string(record), 0600)
	if locked {
		if err = os.Mkdir(filepath.Join(dir, ".delivery-lock"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	response := fmt.Sprintf(`{"headRefOid":%q,"statusCheckRollup":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}`, head)
	tools := gsEnv(t, "\"pr view 9 --repo owner/repo --json headRefOid,statusCheckRollup\") printf '%s' '"+response+"'; exit 0;;\n")
	return gsCase{lab: dir, tools: tools, args: []string{"checks", "--lab", dir, "--molecule", "mol-ci", "--repo", p.Repo, "--pr", "9", "--commit", head, "--required", "test"}}
}

// gsSuggestion saves one pending Formula suggestion. A tampered suggestion keeps
// the hashes recorded at review but carries different proposed bytes.
func gsSuggestion(t *testing.T, tamper bool) gsCase {
	t.Helper()
	tools := gsEnv(t, "")
	dir := t.TempDir()
	original := "kind: formula\nid: historical\nharness: {command: [should-never-run]}\natoms: [{type: harness}]\n"
	proposed := original + "# historical change\n"
	s := learning.Suggestion{ID: "old", FormulaID: "historical", Status: "pending", OriginalYAML: original, ProposedYAML: proposed, OriginalHash: gsHash(original), ProposedHash: gsHash(proposed), ProposedReady: true}
	args := []string{"suggestion", "old", "--lab", dir}
	if tamper {
		s.ProposedYAML += "# edited after review\n"
		args = append(args, "--approve")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	gsWrite(t, filepath.Join(dir, "suggestions", "old.json"), string(raw), 0600)
	return gsCase{lab: dir, args: args, tools: tools}
}

// gsRunnableSuggestion saves a runnable suggestion against an approved Formula.
func gsRunnableSuggestion(t *testing.T, status string) gsCase {
	t.Helper()
	tools := gsEnv(t, "")
	dir := t.TempDir()
	original := gsMoleculeYAML(`["/bin/sh"]`, gsScopeAtom, gsReviewAtom)
	proposed := original + "# reviewed change\n"
	gsApproveFormula(t, dir, original)
	loaded, err := formula.Load(dir, "sample")
	if err != nil {
		t.Fatal(err)
	}
	s := learning.Suggestion{ID: "next", FormulaID: "sample", Status: status, OriginApproval: loaded.Approval, OriginalYAML: original, ProposedYAML: proposed, OriginalHash: gsHash(original), ProposedHash: gsHash(proposed)}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	gsWrite(t, filepath.Join(dir, "suggestions", "next.json"), string(raw), 0600)
	return gsCase{lab: dir, tools: tools, args: []string{"suggestion", "next", "--lab", dir, "--approve"}, after: func(t *testing.T) {
		t.Helper()
		if status == "pending" && string(gsRead(t, filepath.Join(dir, "formulas", "sample.yaml"))) != proposed {
			t.Fatal("approval did not activate the exact proposed bytes")
		}
	}}
}

// gsEvaluation freezes an evaluation plan with a baseline and a candidate Formula
// differing only in harness instructions. The supplied function may break the
// frozen inputs after the plan hashed them; wrong supplies a mismatched digest.
func gsEvaluation(t *testing.T, mutate func(inputs string), wrong bool) gsCase {
	t.Helper()
	tools := gsEnv(t, "")
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	gsWrite(t, filepath.Join(repo, "value.txt"), "before", 0600)
	gsGit(t, repo, "init", "-q", "-b", "main")
	gsGit(t, repo, "config", "user.name", "Test")
	gsGit(t, repo, "config", "user.email", "test@example.test")
	gsGit(t, repo, "add", ".")
	gsGit(t, repo, "commit", "-qm", "base")
	gsGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	base := gsGit(t, repo, "rev-parse", "HEAD")
	inputs := filepath.Join(root, "inputs")
	harnessPath := filepath.Join(root, "harness")
	gsWrite(t, harnessPath, "#!/bin/sh\ncat > /dev/null\nprintf changed > value.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Implemented and tested\",\"paths\":[]}'\n", 0700)
	command, err := json.Marshal([]string{harnessPath})
	if err != nil {
		t.Fatal(err)
	}
	original := gsMoleculeYAML(string(command), gsScopeAtom, gsReviewAtom)
	proposed := strings.Replace(original, "  command:", "  instructions: Report actual outcomes separately.\n  command:", 1)
	approval, err := json.Marshal(evaluation.Approval{ID: "suggestion-gs", Status: "approved", ReviewedAt: "2026-01-01T00:00:00Z", OriginalYAML: original, ProposedYAML: proposed, OriginalHash: evaluation.Hash([]byte(original)), ProposedHash: evaluation.Hash([]byte(proposed))})
	if err != nil {
		t.Fatal(err)
	}
	task := []byte(`{"number":12,"repo":"acme/widgets","title":"Change value","body":"Change value to changed","url":"https://github.com/acme/widgets/issues/12","state":"OPEN"}`)
	acceptance := []byte("#!/bin/sh\ntest \"$(cat value.txt)\" = changed\n")
	gsWrite(t, filepath.Join(inputs, "approval.json"), string(approval), 0600)
	gsWrite(t, filepath.Join(inputs, "issue.json"), string(task), 0600)
	gsWrite(t, filepath.Join(inputs, "acceptance.sh"), string(acceptance), 0600)
	plan := evaluation.Plan{ReadinessVersion: 1, TargetBranch: "main", SchemaVersion: 1, Approval: "approval.json", ApprovalSHA256: evaluation.Hash(approval), Task: "issue.json", TaskSHA256: evaluation.Hash(task), Acceptance: "acceptance.sh", AcceptanceSHA256: evaluation.Hash(acceptance), BaseCommit: base, AllowedFiles: []string{"value.txt"}, RequireInitialFailure: true, Checks: []evaluation.Command{{Name: "acceptance", Dir: ".", Argv: []string{"/bin/sh", "{acceptance}"}, TimeoutMS: 5000}}}
	planRaw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planFile := filepath.Join(inputs, "inputs.json")
	gsWrite(t, planFile, string(planRaw), 0600)
	pair, err := evaluation.ValidateApproval(approval)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "results")
	approve := evaluation.ActivationDigest(planRaw, pair, repo, out)
	if wrong {
		approve = strings.Repeat("0", 64)
	}
	if mutate != nil {
		mutate(inputs)
	}
	return gsCase{lab: root, tools: tools, args: []string{"evaluate", "--inputs", planFile, "--source", repo, "--out", out, "--approve", approve}}
}

func gsAllowRow(name, gate, want string, setup func(t *testing.T) gsCase) gsRow {
	return gsRow{name: name, gate: gate, mode: gsAllow, want: want, setup: setup}
}

func gsHoldRow(name, gate, want string, setup func(t *testing.T) gsCase) gsRow {
	return gsRow{name: name, gate: gate, mode: gsHold, want: want, setup: setup}
}

func gsDenyRow(name, gate, want string, setup func(t *testing.T) gsCase) gsRow {
	return gsRow{name: name, gate: gate, mode: gsDeny, want: want, setup: setup}
}

func gsBlank(t *testing.T, args ...string) gsCase {
	t.Helper()
	tools := gsEnv(t, "")
	lab := t.TempDir()
	for i, arg := range args {
		if arg == "LAB" {
			args[i] = lab
		}
	}
	return gsCase{lab: lab, args: args, tools: tools}
}

var gsRows = []gsRow{
	gsAllowRow("init approve: pending proposal is approved", "init --approve", "approved", func(t *testing.T) gsCase {
		lab, id, tools := gsProposal(t)
		return gsCase{lab: lab, tools: tools, args: []string{"init", "--lab", lab, "--approve", id}}
	}),
	gsDenyRow("init approve: proposal edited after review", "init --approve", "proposal changed since review", func(t *testing.T) gsCase {
		lab, id, tools := gsProposal(t)
		file := filepath.Join(lab, "assays", id+".json")
		raw := gsRead(t, file)
		edited := bytes.Replace(raw, []byte("id: sample"), []byte("id: samplx"), 1)
		if bytes.Equal(raw, edited) {
			t.Fatal("proposal fixture was not edited")
		}
		gsWrite(t, file, string(edited), 0600)
		return gsCase{lab: lab, tools: tools, args: []string{"init", "--lab", lab, "--approve", id}}
	}),
	gsDenyRow("init approve: active Formula edited after proposal", "init --approve", "original active Formula changed", func(t *testing.T) gsCase {
		tools := gsEnv(t, "")
		lab := t.TempDir()
		gsApproveFormula(t, lab, "kind: formula\nid: active\natoms: [{type: harness}]\n")
		p, err := onboarding.SaveProposal(lab, gsMinimalFormula)
		if err != nil {
			t.Fatal(err)
		}
		gsAppend(t, filepath.Join(lab, "formulas", "active.yaml"), "# edited after the proposal\n")
		return gsCase{lab: lab, tools: tools, args: []string{"init", "--lab", lab, "--approve", p.ID}}
	}),
	gsDenyRow("init approve: lab.json corrupt after proposal", "init --approve", "stale approval refused", func(t *testing.T) gsCase {
		lab, id, tools := gsProposal(t)
		gsWrite(t, filepath.Join(lab, "lab.json"), "{", 0600)
		return gsCase{lab: lab, tools: tools, args: []string{"init", "--lab", lab, "--approve", id}}
	}),
	gsDenyRow("init approve: proposal already dismissed", "init --approve", "proposal already reviewed", func(t *testing.T) gsCase {
		lab, id, tools := gsProposal(t)
		if _, err := onboarding.Review(lab, id, "dismiss"); err != nil {
			t.Fatal(err)
		}
		return gsCase{lab: lab, tools: tools, args: []string{"init", "--lab", lab, "--approve", id}}
	}),
	gsDenyRow("init approve: malformed proposal id", "init --approve", "invalid assay id", func(t *testing.T) gsCase {
		lab, _, tools := gsProposal(t)
		return gsCase{lab: lab, tools: tools, args: []string{"init", "--lab", lab, "--approve", "assay-not-a-digest"}}
	}),
	gsDenyRow("init approve: concurrent Formula write holds the lock", "init --approve", "another Formula write is active", func(t *testing.T) gsCase {
		lab, id, tools := gsProposal(t)
		if err := os.Mkdir(filepath.Join(lab, ".formula-write.lock"), 0700); err != nil {
			t.Fatal(err)
		}
		return gsCase{lab: lab, tools: tools, args: []string{"init", "--lab", lab, "--approve", id}}
	}),
	gsAllowRow("init dismiss: pending proposal is dismissed", "init --dismiss", "dismissed", func(t *testing.T) gsCase {
		lab, id, tools := gsProposal(t)
		return gsCase{lab: lab, tools: tools, args: []string{"init", "--lab", lab, "--dismiss", id}}
	}),
	gsDenyRow("init dismiss: proposal already approved", "init --dismiss", "proposal already reviewed", func(t *testing.T) gsCase {
		lab, id, tools := gsProposal(t)
		if _, err := onboarding.Review(lab, id, "approve"); err != nil {
			t.Fatal(err)
		}
		return gsCase{lab: lab, tools: tools, args: []string{"init", "--lab", lab, "--dismiss", id}}
	}),
	gsDenyRow("init rebind: cannot be combined with --harness", "init --rebind", "--rebind and --harness cannot be combined", func(t *testing.T) gsCase {
		return gsBlank(t, "init", "--lab", "LAB", "--rebind", "codex", "--harness", "claude")
	}),
	gsDenyRow("init rebind: cannot be combined with an approval", "init --rebind", "--rebind proposes a change; review, approve or dismiss the saved proposal in a separate command", func(t *testing.T) gsCase {
		return gsBlank(t, "init", "--lab", "LAB", "--rebind", "codex", "--approve", "assay-"+strings.Repeat("a", 64))
	}),
	gsDenyRow("init rebind: needs a harness id", "init --rebind", "--rebind needs a harness ID", func(t *testing.T) gsCase {
		return gsBlank(t, "init", "--lab", "LAB", "--rebind", "")
	}),
	gsDenyRow("init rebind: cannot change timeout budgets", "init --rebind", "--rebind cannot be combined with the timeout flags", func(t *testing.T) gsCase {
		return gsBlank(t, "init", "--lab", "LAB", "--rebind", "codex", "--harness-timeout-ms", "1000")
	}),

	gsAllowRow("run approve: exact plan digest runs the approved scope", "run --approve", "· waiting", func(t *testing.T) gsCase {
		return gsSeedMolecule(t).approve()
	}),
	gsDenyRow("run approve: digest names no Molecule", "run --approve", "no Molecule matches this approval digest", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		m.digest = strings.Repeat("0", 64)
		return m.approve()
	}),
	gsDenyRow("run approve: malformed digest", "run --approve", "invalid approval digest", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		m.digest = "not-a-digest"
		return m.approve()
	}),
	gsDenyRow("run approve: Formula bytes changed after approval", "run --approve", "missing, stale or non-current exact-byte approval", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		gsAppend(t, filepath.Join(m.lab, "formulas", "sample.yaml"), "# comment-only edit\n")
		return m.approve()
	}),
	gsDenyRow("run approve: lab.json missing", "run --approve", "approve a Formula with init before running", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		if err := os.Remove(filepath.Join(m.lab, "lab.json")); err != nil {
			t.Fatal(err)
		}
		return m.approve()
	}),
	gsDenyRow("run approve: lab.json corrupt", "run --approve", "Formula execution approval rejected", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		gsWrite(t, filepath.Join(m.lab, "lab.json"), "{", 0600)
		return m.approve()
	}),
	gsDenyRow("run approve: issue body changed after approval", "run --approve", "readiness inputs changed; refresh intake before approval", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		gsWrite(t, m.issuePath, gsIssueJSON("A different request"), 0600)
		return m.approve()
	}),
	gsDenyRow("run approve: binding fingerprint changed", "run --approve", "readiness inputs changed; refresh intake before approval", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		gsAppend(t, m.harnessPath, "# changed after the plan was bound\n")
		return m.approve()
	}),
	gsDenyRow("run approve: workspace deleted", "run --approve", "repository origin changed", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		if err := os.RemoveAll(m.record.Workspace.Path); err != nil {
			t.Fatal(err)
		}
		return m.approve()
	}),
	gsDenyRow("run approve: ledger JSON truncated", "run --approve", "no Molecule matches this approval digest", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		raw := gsRead(t, m.ledger())
		gsWrite(t, m.ledger(), string(raw[:len(raw)/2]), 0600)
		return m.approve()
	}),
	gsDenyRow("run approve: concurrent attempt holds the Molecule lock", "run --approve", "Molecule is locked", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		m.lock(t)
		return m.approve()
	}),

	gsAllowRow("amend: successor bound to the current plan digest", "amend", "· waiting", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		return gsAmend(t, m, m.record.ID, m.digest)
	}),
	gsDenyRow("amend: parent digest belongs to a different Molecule", "amend", "Molecule and parent plan mismatch", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		return gsAmend(t, m, "mol-other", m.digest)
	}),
	gsDenyRow("amend: unknown parent digest", "amend", "no Molecule matches this approval digest", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		return gsAmend(t, m, m.record.ID, strings.Repeat("0", 64))
	}),
	gsDenyRow("amend: malformed parent digest", "amend", "invalid approval digest", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		return gsAmend(t, m, m.record.ID, "not-a-digest")
	}),
	gsDenyRow("amend: parent superseded by an earlier amendment", "amend", "no Molecule matches this approval digest", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		first := gsAmend(t, m, m.record.ID, m.digest)
		if code, stdout, stderr := gsRun(first.args); code != 0 {
			t.Fatalf("first amendment failed: %d %s %s", code, stdout, stderr)
		}
		return gsAmend(t, m, m.record.ID, m.digest)
	}),
	gsDenyRow("amend: concurrent attempt holds the Molecule lock", "amend", "Molecule is locked", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		m.lock(t)
		return gsAmend(t, m, m.record.ID, m.digest)
	}),

	gsDenyRow("recover: plan digest belongs to a different Molecule", "recover", "Molecule and plan mismatch", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		return m.c("recover", "mol-other", "--lab", m.lab, "--plan", m.digest, "--confirm-stopped")
	}),
	gsDenyRow("recover: stopped processes not confirmed", "recover", "then use --confirm-stopped", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		return m.c("recover", m.record.ID, "--lab", m.lab, "--plan", m.digest)
	}),
	gsDenyRow("recover: unknown plan digest", "recover", "no Molecule matches this approval digest", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		return m.c("recover", m.record.ID, "--lab", m.lab, "--plan", strings.Repeat("0", 64), "--confirm-stopped")
	}),
	gsDenyRow("recover: Molecule never ran coding", "recover", "no durable process reconciliation evidence", func(t *testing.T) gsCase {
		m := gsSeedMolecule(t)
		return m.c("recover", m.record.ID, "--lab", m.lab, "--plan", m.digest, "--confirm-stopped")
	}),

	gsAllowRow("loop: scope policy approves the scope gate and leaves the review gate waiting", "loop", "approved by policy", func(t *testing.T) gsCase {
		m := gsSeedWith(t, gsPolicyScope, gsPolicyReview, true)
		gsStubLoop(t)
		c := m.c("loop", "--lab", m.lab, "--once")
		c.after = func(t *testing.T) {
			t.Helper()
			r := gsLoadRecord(t, m.ledger())
			if len(r.PolicyDecisions) != 1 || r.PolicyDecisions[0].Gate != "scope" {
				t.Fatalf("expected one scope policy decision: %+v", r.PolicyDecisions)
			}
			reviewWaiting := false
			for _, a := range r.Atoms {
				if a.ID == "gate" {
					reviewWaiting = a.Status == "waiting"
				}
			}
			if !reviewWaiting || r.Status != "waiting" {
				t.Fatalf("the policy must not pass the human review gate: status %s atoms %+v", r.Status, r.Atoms)
			}
			if _, err := os.Stat(filepath.Join(m.lab, "deliveries")); !os.IsNotExist(err) {
				t.Fatal("the loop delivered")
			}
			for _, call := range m.tools.calls(t) {
				if !strings.HasPrefix(call, "issue view") {
					t.Fatalf("the loop called gh beyond reading the issue: %q", call)
				}
			}
		}
		return c
	}),
	gsHoldRow("loop: a human scope gate is never approved by the loop", "loop", "the scope gate needs a human", func(t *testing.T) gsCase {
		m := gsSeedWith(t, gsScopeAtom, gsPolicyReview, true)
		gsStubLoop(t)
		return m.c("loop", "--lab", m.lab, "--once")
	}),

	gsAllowRow("deliver preview: finished Molecule yields the reviewed digest", "deliver --approve", `"moleculeId":"mol-fixture"`, func(t *testing.T) gsCase {
		f := gsFinishedLab(t, false, "")
		return f.c(gsPreview(f.lab)...)
	}),
	gsAllowRow("deliver approve: exact digest publishes a draft PR", "deliver --approve", "published", func(t *testing.T) gsCase {
		f := gsFinishedLab(t, false, gsPRFlow)
		return f.c(gsPreview(f.lab, "--approve", f.digest(t), "--title", "Fixture")...)
	}),
	gsDenyRow("deliver approve: stale preview digest", "deliver --approve", "approval is stale: reviewed content changed", func(t *testing.T) gsCase {
		f := gsFinishedLab(t, false, "")
		return f.c(gsPreview(f.lab, "--approve", strings.Repeat("0", 64), "--title", "Fixture")...)
	}),
	gsDenyRow("deliver approve: malformed digest", "deliver --approve", "provide an exact preview digest and one-line title", func(t *testing.T) gsCase {
		f := gsFinishedLab(t, false, "")
		return f.c(gsPreview(f.lab, "--approve", "short", "--title", "Fixture")...)
	}),
	gsDenyRow("deliver approve: Molecule not finished", "deliver --approve", "delivery requires a finished approved Molecule", func(t *testing.T) gsCase {
		f := gsFinishedLab(t, false, "")
		file := filepath.Join(f.lab, "ledgers", "mol-fixture.json")
		var ledger map[string]any
		if err := json.Unmarshal(gsRead(t, file), &ledger); err != nil {
			t.Fatal(err)
		}
		ledger["finishedAt"] = ""
		raw, err := json.Marshal(ledger)
		if err != nil {
			t.Fatal(err)
		}
		gsWrite(t, file, string(raw), 0600)
		return f.c(gsPreview(f.lab, "--approve", strings.Repeat("0", 64), "--title", "Fixture")...)
	}),
	gsDenyRow("deliver approve: unverified acceptance without --accept-unverified", "deliver --approve", "independent acceptance is unverified; explicit human review with --accept-unverified is required", func(t *testing.T) gsCase {
		f := gsFinishedLab(t, true, gsPRFlow)
		return f.c(gsPreview(f.lab, "--approve", f.digest(t), "--title", "Fixture")...)
	}),
	gsDenyRow("deliver approve: concurrent delivery holds the lock", "deliver --approve", "delivery lock exists", func(t *testing.T) gsCase {
		f := gsFinishedLab(t, false, gsPRFlow)
		if err := os.Mkdir(filepath.Join(f.lab, ".delivery-lock"), 0700); err != nil {
			t.Fatal(err)
		}
		return f.c(gsPreview(f.lab, "--approve", strings.Repeat("0", 64), "--title", "Fixture")...)
	}),
	gsDenyRow("deliver approve: workspace deleted", "deliver --approve", "no such file or directory", func(t *testing.T) gsCase {
		f := gsFinishedLab(t, false, "")
		if err := os.RemoveAll(f.workspace); err != nil {
			t.Fatal(err)
		}
		return f.c(gsPreview(f.lab, "--approve", strings.Repeat("0", 64), "--title", "Fixture")...)
	}),

	gsAllowRow("checks: published delivery observes passing CI", "checks", "passed", func(t *testing.T) gsCase {
		return gsPublishedDelivery(t, false, false)
	}),
	gsDenyRow("checks: receipt tampered after review", "checks", "CI requires a matching published delivery receipt and ledger", func(t *testing.T) gsCase {
		return gsPublishedDelivery(t, true, false)
	}),
	gsDenyRow("checks: concurrent collection holds the delivery lock", "checks", "delivery/CI collection conflict", func(t *testing.T) gsCase {
		return gsPublishedDelivery(t, false, true)
	}),

	gsAllowRow("suggestion: saved suggestion can be inspected", "suggestion --approve", "Formula suggestion old", func(t *testing.T) gsCase {
		return gsSuggestion(t, false)
	}),
	gsAllowRow("suggestion approve: exact saved bytes become the active Formula", "suggestion --approve", "approved", func(t *testing.T) gsCase {
		return gsRunnableSuggestion(t, "pending")
	}),
	gsDenyRow("suggestion approve: proposed bytes edited after review", "suggestion --approve", "invalid or modified Formula suggestion", func(t *testing.T) gsCase {
		return gsSuggestion(t, true)
	}),
	gsDenyRow("suggestion approve: suggestion already dismissed", "suggestion --approve", "suggestion already reviewed", func(t *testing.T) gsCase {
		return gsRunnableSuggestion(t, "dismissed")
	}),

	gsAllowRow("evaluate approve: exact activation digest runs the paired trial", "evaluate --approve", "complete", func(t *testing.T) gsCase {
		return gsEvaluation(t, nil, false)
	}),
	gsDenyRow("evaluate approve: wrong activation digest", "evaluate --approve", "fresh exact evaluation approval required", func(t *testing.T) gsCase {
		return gsEvaluation(t, nil, true)
	}),
	gsDenyRow("evaluate approve: frozen approval changed after the plan hashed it", "evaluate --approve", "frozen input hash mismatch: approval.json", func(t *testing.T) gsCase {
		return gsEvaluation(t, func(inputs string) {
			gsAppend(t, filepath.Join(inputs, "approval.json"), " ")
		}, false)
	}),
	gsDenyRow("evaluate approve: plan lacks exact base, scope and checks", "evaluate --approve", "plan requires exact base, scope and independent checks", func(t *testing.T) gsCase {
		tools := gsEnv(t, "")
		dir := t.TempDir()
		plan := filepath.Join(dir, "plan.json")
		gsWrite(t, plan, `{"schemaVersion":1}`, 0600)
		return gsCase{lab: dir, tools: tools, args: []string{"evaluate", "--inputs", plan, "--source", dir, "--out", filepath.Join(dir, "out"), "--approve", strings.Repeat("0", 64)}}
	}),
}

func TestGateScenarios(t *testing.T) {
	for _, row := range gsRows {
		t.Run(row.name, func(t *testing.T) {
			c := row.setup(t)
			before := gsCapture(t, c)
			code, stdout, stderr := gsRun(c.args)
			if row.mode == gsAllow {
				if code != 0 || !strings.Contains(stdout, row.want) {
					t.Fatalf("allow row failed: code=%d want stdout %q\nstdout: %s\nstderr: %s", code, row.want, stdout, stderr)
				}
				if c.after != nil {
					c.after(t)
				}
				return
			}
			if row.mode == gsHold {
				if code != 0 || !strings.Contains(stdout, row.want) {
					t.Fatalf("hold row failed: code=%d want stdout %q\nstdout: %s\nstderr: %s", code, row.want, stdout, stderr)
				}
			} else {
				if code == 0 {
					t.Fatalf("gate failed open: %v\nstdout: %s", c.args, stdout)
				}
				if !strings.Contains(stderr, row.want) {
					t.Fatalf("message lacks %q\nstderr: %s", row.want, stderr)
				}
				if stdout != "" {
					t.Fatalf("denied command printed success output: %s", stdout)
				}
			}
			if strings.Contains(stderr, "executable file not found") {
				t.Fatalf("denied for a missing tool, not for the broken state: %s", stderr)
			}
			if changed := gsStateDiff(before, gsCapture(t, c)); changed != "" {
				t.Fatalf("the gate changed state: %s", changed)
			}
			for _, call := range c.tools.calls(t) {
				if !c.ghReads || !strings.HasPrefix(call, "issue view") {
					t.Fatalf("the gate called gh: %q", call)
				}
			}
		})
	}
}

func TestGateScenariosCoverEveryGateCommand(t *testing.T) {
	if len(gsRows) < 18 {
		t.Fatalf("the table holds %d rows; it needs at least 18", len(gsRows))
	}
	names := map[string]bool{}
	denied := map[string]int{}
	allowed := 0
	for _, row := range gsRows {
		if _, ok := gsGates[row.gate]; !ok {
			t.Errorf("row %q names gate %q, which is not in gsGates", row.name, row.gate)
		}
		if names[row.name] {
			t.Errorf("duplicate row %q", row.name)
		}
		names[row.name] = true
		if row.want == "" || row.setup == nil {
			t.Errorf("row %q needs a message and a setup", row.name)
		}
		if row.mode == gsAllow {
			allowed++
		} else {
			denied[row.gate]++
		}
	}
	if allowed < 3 {
		t.Errorf("only %d allow rows; each gate family needs one proving the fixture passes before it is corrupted", allowed)
	}
	for gate := range gsGates {
		if denied[gate] == 0 {
			t.Errorf("gate command %q has no malformed-state row that fails closed", gate)
		}
	}
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--help"}, strings.NewReader(""), &out, &stderr, "gate-suite"); code != 0 {
		t.Fatalf("help failed: %s", stderr.String())
	}
	registered := map[string]bool{}
	for gate := range gsGates {
		registered[strings.Fields(gate)[0]] = true
	}
	word := regexp.MustCompile(`^[a-z]+$`)
	for _, line := range strings.Split(out.String(), "\n") {
		first, _, _ := strings.Cut(line, " ")
		if !word.MatchString(first) {
			continue
		}
		gated := strings.Contains(line, "--approve") || strings.Contains(line, "--parent") || strings.Contains(line, "--confirm-stopped") || first == "handoff"
		if gated && !registered[first] {
			t.Errorf("CLI command %q takes an approval or digest flag but is not in gsGates; add it and add rows for it", first)
		}
	}
}

// Candidate documentation evidence; this does not establish independent acceptance.
func TestGateScenariosAreDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := strings.Join(strings.Fields(string(raw)), " ")
	for _, claim := range []string{"TestGateScenarios", "lab/internal/cli/gate_scenarios_test.go", "must add rows"} {
		if !strings.Contains(readme, claim) {
			t.Errorf("lab/README.md lacks the contributor note %q", claim)
		}
	}
}
