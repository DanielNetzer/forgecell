package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

const (
	pluginPreviewProject  = "welltech-com/platform/backend"
	pluginPreviewIdentity = "gitlab.com/welltech-com/platform/backend"
	pluginPreviewOrigin   = "https://gitlab.com/welltech-com/platform/backend.git"
	pluginPreviewOther    = "https://gitlab.com/welltech-com/platform/other.git"
)

// pluginPreviewFormula is a plugin tracker and forge Formula on the nested
// project, with forge.identity set to identity when it is not empty.
func pluginPreviewFormula(identity string) string {
	yaml := "schemaVersion: v0\nkind: formula\nid: delivery-plugin-fixture\nintake:\n  source: jira\n  repo: " + pluginPreviewProject + "\nharness:\n  binding: unbound\natoms:\n" +
		"  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: coding, type: harness}\n  - {id: checks, type: check}\n" +
		"  - {id: review, type: gate, purpose: review}\n  - {id: publication, type: ship}\n  - {id: documentation, type: document}\n" +
		"plugins:\n  - {name: gitlab, manifest: plugins/gitlab/plugin.json, sha256: " + strings.Repeat("a", 64) + "}\n" +
		"  - {name: jira, manifest: plugins/jira/plugin.json, sha256: " + strings.Repeat("a", 64) + "}\n" +
		"forge:\n  plugin: gitlab\n  project: " + pluginPreviewProject + "\n"
	if identity != "" {
		yaml += "  identity: " + identity + "\n"
	}
	return yaml
}

type pluginPreview struct {
	o    Options
	root string
}

func (p pluginPreview) git(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = p.root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s %v", args, b, err)
	}
}

// pluginPreviewFixture is fixture for a finished keyed Molecule on the nested
// project, approved under yaml, whose checkout origin is origin.
func pluginPreviewFixture(t *testing.T, yaml, origin string) pluginPreview {
	t.Helper()
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ValidateReadiness(); err != nil {
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
	fx := pluginPreview{root: t.TempDir()}
	out := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = fx.root
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %s %v", args, b, e)
		}
		return strings.TrimSpace(string(b))
	}
	out("init", "-b", "main")
	out("config", "user.email", "test@example.com")
	out("config", "user.name", "Test")
	os.WriteFile(filepath.Join(fx.root, "a.txt"), []byte("before\n"), 0600)
	out("add", "a.txt")
	out("commit", "-m", "base")
	sha := out("rev-parse", "HEAD")
	out("update-ref", "refs/remotes/origin/main", sha)
	out("remote", "add", "origin", origin)
	lab := t.TempDir()
	workspace := filepath.Join(lab, "workspaces", "fixture")
	out("worktree", "add", "-b", "forgecell/fixture", workspace, sha)
	const key, url = "ENG-1", "https://tracker.example.test/browse/ENG-1"
	r := molecule.Record{SchemaVersion: "v0", Kind: "molecule", ID: "mol-fixture", Status: "waiting", FormulaApproved: true, FormulaID: f.ID, FormulaSnapshot: molecule.Snapshot{YAML: f.YAML, SHA256: f.SHA256}, FinishedAt: "2026-09-29T00:00:00Z", Workspace: molecule.Workspace{SourceRoot: fx.root, Path: workspace, Repo: pluginPreviewProject, Branch: "forgecell/fixture", BaseCommit: sha}, Issue: molecule.Issue{Repo: pluginPreviewProject, Key: key, URL: url}, Atoms: atoms}
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("after\n"), 0600)
	p := readiness.Plan{SchemaVersion: "v1", MoleculeID: r.ID, Revision: 1, Inputs: readiness.Inputs{Repository: pluginPreviewProject, TargetBranch: "main", BaseCommit: sha, FormulaSHA256: f.SHA256, EvidenceSHA256: strings.Repeat("b", 64), BindingSHA256: strings.Repeat("c", 64), Issue: readiness.Issue{Repository: pluginPreviewProject, Key: key, Title: "Change a", URL: url, State: "OPEN"}}, Evidence: []readiness.Evidence{{ID: "source", Path: "a.txt", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("before\n")))}}, Analysis: readiness.Analysis{Summary: "Change a", Scope: []readiness.ScopedPath{{Path: "a.txt", Reason: "Requested change", Evidence: []string{"issue"}}}, Acceptance: []readiness.Criterion{{Description: "a contains after", Evidence: []string{"issue"}}}, Checks: []readiness.Check{{ID: "acceptance", Category: "independent-acceptance", IndependentProvenance: "Explicit delivery test fixture", Argv: []string{"test", "ok"}, Dir: ".", TimeoutMS: 1000, Required: true, Definitions: []string{"source"}, Reason: "Fixture", Evidence: []string{"issue"}}}}}
	s, e := readiness.NewState(p, "now")
	if e != nil {
		t.Fatal(e)
	}
	d, _ := p.Digest()
	s, e = s.Approve(d, p.Inputs, "approve")
	if e != nil {
		t.Fatal(e)
	}
	s, _ = s.StartAttempt("start")
	s, _ = s.FinishAttempt("check-pending", "captured", "end")
	s, _ = s.RecordVerification(true, "fixture verified", "checked")
	r.Readiness = &s
	c, e := verification.Capture(context.Background(), workspace, sha, []string{"a.txt"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	r.LabDir = lab
	r.Capture = &c
	report := `{"schemaVersion":"v1","outcome":"completed","reason":"Implemented and tested","paths":[]}`
	coding := harness.CodingOutcomeFromResult(report)
	result := process.Result{OK: true, Reconciled: true, Stdout: report}
	evidence, _ := json.Marshal(struct {
		process.Result
		AttemptID  string `json:"attemptId"`
		PlanDigest string `json:"planDigest"`
	}{result, "completed", d})
	path := filepath.ToSlash(filepath.Join("verification-history", r.ID, "harness-completed-result.json"))
	os.MkdirAll(filepath.Dir(filepath.Join(lab, path)), 0700)
	os.WriteFile(filepath.Join(lab, path), evidence, 0600)
	ref := molecule.VerificationFile{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(evidence))}
	result.Stdout = ""
	r.HarnessAttempts = []molecule.HarnessAttempt{{ID: "completed", PlanDigest: d, Result: result, Coding: &coding, Evidence: &ref, Capture: &c}}
	r.Verification = &verification.Result{SourceTree: c.Tree, RequiredChecksPassed: true, IndependentAcceptanceVerified: true, Checks: []verification.Observation{{ID: "acceptance", Category: "independent-acceptance", SourceTree: c.Tree, Command: p.Analysis.Checks[0], Result: process.Result{OK: true}}}}
	raw, _ := json.Marshal(r)
	os.MkdirAll(filepath.Join(lab, "ledgers"), 0700)
	os.WriteFile(filepath.Join(lab, "ledgers", r.ID+".json"), raw, 0600)
	fx.o = Options{LabDir: lab, MoleculeID: r.ID, Files: []string{"a.txt"}, BaseBranch: "main"}
	return fx
}

const originRefusal = "Git origin or push destination differs from intake"

func TestPreviewOfAPluginForgeMoleculeOnAGitLabOrigin(t *testing.T) {
	fx := pluginPreviewFixture(t, pluginPreviewFormula(pluginPreviewIdentity), pluginPreviewOrigin)
	p, err := Preview(context.Background(), fx.o)
	if err != nil || !strings.Contains(p.Diff, "+after") || p.Repo != pluginPreviewProject {
		t.Fatalf("preview = %+v, %v", p, err)
	}
	// The other remote forms of the same project pass too.
	for _, origin := range []string{"git@gitlab.com:Welltech-Com/Platform/Backend.git", "ssh://git@gitlab.com/welltech-com/platform/backend"} {
		fx.git(t, "remote", "set-url", "origin", origin)
		if _, err := Preview(context.Background(), fx.o); err != nil {
			t.Fatalf("%s: %v", origin, err)
		}
	}
}

func TestPreviewOfAPluginForgeMoleculeRefusesAnotherDestination(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(t *testing.T, fx pluginPreview)
		want string
	}{
		"mismatched origin": {func(t *testing.T, fx pluginPreview) {
			fx.git(t, "remote", "set-url", "origin", pluginPreviewOther)
		}, originRefusal},
		"mismatched push URL": {func(t *testing.T, fx pluginPreview) {
			fx.git(t, "remote", "set-url", "--push", "origin", pluginPreviewOther)
		}, originRefusal},
		"second fetch URL": {func(t *testing.T, fx pluginPreview) {
			fx.git(t, "remote", "set-url", "--add", "origin", pluginPreviewOther)
		}, originRefusal},
		"GitHub origin": {func(t *testing.T, fx pluginPreview) {
			fx.git(t, "remote", "set-url", "origin", "https://github.com/welltech-com/platform/backend.git")
		}, originRefusal},
		"credentialed origin": {func(t *testing.T, fx pluginPreview) {
			fx.git(t, "remote", "set-url", "origin", "https://user:token@gitlab.com/welltech-com/platform/backend.git")
		}, originRefusal},
		"edited snapshot": {func(t *testing.T, fx pluginPreview) {
			file := filepath.Join(fx.o.LabDir, "ledgers", fx.o.MoleculeID+".json")
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			// The edited identity names the checkout's real origin; the snapshot's
			// recorded SHA-256 no longer binds these bytes, so it is refused. Only
			// the snapshot YAML holds the host-qualified identity.
			edited := strings.Replace(string(raw), pluginPreviewIdentity, "gitlab.com/welltech-com/platform/other", 1)
			if edited == string(raw) {
				t.Fatal("ledger has no identity to edit")
			}
			fx.git(t, "remote", "set-url", "origin", pluginPreviewOther)
			if err = os.WriteFile(file, []byte(edited), 0600); err != nil {
				t.Fatal(err)
			}
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			fx := pluginPreviewFixture(t, pluginPreviewFormula(pluginPreviewIdentity), pluginPreviewOrigin)
			c.edit(t, fx)
			// An edited snapshot is refused by an earlier binding check or by
			// snapshotForgeIdentity; either way it never reaches the origin check.
			if _, err := Preview(context.Background(), fx.o); err == nil || !strings.Contains(err.Error(), c.want) || (c.want == "" && strings.Contains(err.Error(), originRefusal)) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestPreviewOfAPluginForgeMoleculeWithoutIdentityIsRefused(t *testing.T) {
	// Neither the matching gitlab.com origin nor a GitHub origin passes: a forge
	// block without an identity never falls back to the GitHub check.
	for _, origin := range []string{pluginPreviewOrigin, "https://github.com/welltech-com/platform/backend.git"} {
		fx := pluginPreviewFixture(t, pluginPreviewFormula(""), origin)
		if _, err := Preview(context.Background(), fx.o); err == nil || err.Error() != molecule.ErrNoForgeIdentity.Error() {
			t.Fatalf("%s: err = %v", origin, err)
		}
	}
}

// TestDeliveryOfAMoleculeWithAMissingSnapshotIsRefused: a plugin-forge record
// whose snapshot bytes were blanked, while its SHA-256 and Formula id remain, is
// refused before any remote operation; it never falls back to the GitHub check,
// which a GitHub origin would otherwise pass.
func TestDeliveryOfAMoleculeWithAMissingSnapshotIsRefused(t *testing.T) {
	const want = "the Molecule's Formula snapshot is missing; nothing was published"
	for name, blank := range map[string]func(r *molecule.Record){
		"yaml only":              func(r *molecule.Record) { r.FormulaSnapshot.YAML = "" },
		"yaml and id":            func(r *molecule.Record) { r.FormulaSnapshot.YAML, r.FormulaID = "", "" },
		"on GitHub, yaml only":   func(r *molecule.Record) { r.FormulaSnapshot.YAML = "" },
		"on GitHub, yaml and id": func(r *molecule.Record) { r.FormulaSnapshot.YAML, r.FormulaID = "", "" },
	} {
		t.Run(name, func(t *testing.T) {
			fx := pluginPreviewFixture(t, pluginPreviewFormula(pluginPreviewIdentity), pluginPreviewOrigin)
			previewed, err := Preview(context.Background(), fx.o)
			if err != nil {
				t.Fatal(err)
			}
			// A GitHub origin naming the project would pass the GitHub check.
			if strings.HasPrefix(name, "on GitHub") {
				fx.git(t, "remote", "set-url", "origin", "https://github.com/welltech-com/platform/backend.git")
			}
			r, err := readFixtureRecord(fx.o)
			if err != nil {
				t.Fatal(err)
			}
			blank(&r)
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(fx.o.LabDir, "ledgers", r.ID+".json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = Preview(context.Background(), fx.o); err == nil || err.Error() != want {
				t.Fatalf("preview err = %v, want %q", err, want)
			}
			remote := 0
			runner := func(context.Context, process.Options) process.Result {
				remote++
				return process.Result{Error: "unexpected remote operation"}
			}
			_, err = Publish(context.Background(), PublishOptions{Options: fx.o, Approve: previewed.Digest, Title: "Change a", Runner: runner})
			if err == nil || err.Error() != want {
				t.Fatalf("publish err = %v, want %q", err, want)
			}
			if remote != 0 {
				t.Fatalf("%d remote operations ran; nothing may be pushed", remote)
			}
			if _, err = os.Lstat(filepath.Join(fx.o.LabDir, "deliveries", r.ID+".json")); !os.IsNotExist(err) {
				t.Fatalf("a delivery receipt was written: %v", err)
			}
		})
	}
}

// TestSnapshotForgeIdentityWithoutFormulaInformation: only a record with no
// Formula information at all keeps today's GitHub check; a SHA-256 or a Formula
// id alone is a missing snapshot.
func TestSnapshotForgeIdentityWithoutFormulaInformation(t *testing.T) {
	if id, err := snapshotForgeIdentity(molecule.Record{}); id != "" || err != nil {
		t.Fatalf("identity %q, err %v", id, err)
	}
	for _, r := range []molecule.Record{{FormulaID: "x"}, {FormulaSnapshot: molecule.Snapshot{SHA256: strings.Repeat("a", 64)}}} {
		if _, err := snapshotForgeIdentity(r); err == nil || err.Error() != "the Molecule's Formula snapshot is missing; nothing was published" {
			t.Fatalf("%+v: err = %v", r, err)
		}
	}
}

func TestPreviewOfAGitHubMoleculeKeepsTheGitHubCheck(t *testing.T) {
	o := fixture(t)
	if _, err := Preview(context.Background(), o); err != nil {
		t.Fatalf("a GitHub origin was refused: %v", err)
	}
	for _, args := range [][]string{{"remote", "set-url", "origin", "https://gitlab.com/owner/repo.git"}, {"remote", "set-url", "--push", "origin", "https://gitlab.com/owner/repo.git"}} {
		o := fixture(t)
		r, err := readFixtureRecord(o)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = r.Workspace.SourceRoot
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s %v", args, b, err)
		}
		if _, err := Preview(context.Background(), o); err == nil || err.Error() != originRefusal {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}

// readFixtureRecord reads the fixture's ledger record.
func readFixtureRecord(o Options) (molecule.Record, error) {
	var r molecule.Record
	raw, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", o.MoleculeID+".json"))
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(raw, &r)
}
