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

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

func fixture(t *testing.T) Options {
	return fixtureWithRepair(t, false)
}

func fixtureWithRepair(t *testing.T, repair bool) Options {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %s %v", args, b, e)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-b", "main")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("before\n"), 0600)
	git("add", "a.txt")
	git("commit", "-m", "base")
	sha := git("rev-parse", "HEAD")
	git("update-ref", "refs/remotes/origin/main", sha)
	git("remote", "add", "origin", "https://github.com/owner/repo.git")
	lab := t.TempDir()
	workspace := filepath.Join(lab, "workspaces", "fixture")
	git("worktree", "add", "-b", "forgecell/fixture", workspace, sha)
	r := molecule.Record{SchemaVersion: "v0", Kind: "molecule", ID: "mol-fixture", Status: "waiting", FormulaApproved: true, FinishedAt: "2026-09-29T00:00:00Z", Workspace: molecule.Workspace{SourceRoot: root, Path: workspace, Repo: "owner/repo", Branch: "forgecell/fixture", BaseCommit: sha}, Issue: molecule.Issue{Repo: "owner/repo", Number: 9}, Atoms: []molecule.Atom{{Type: "harness", Status: "done"}}}
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("after\n"), 0600)
	p := readiness.Plan{SchemaVersion: "v1", MoleculeID: r.ID, Revision: 1, Inputs: readiness.Inputs{Repository: "owner/repo", TargetBranch: "main", BaseCommit: sha, FormulaSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64), BindingSHA256: strings.Repeat("c", 64), Issue: readiness.Issue{Repository: "owner/repo", Number: 9, Title: "Change a", URL: "https://github.com/owner/repo/issues/9", State: "OPEN"}}, Evidence: []readiness.Evidence{{ID: "source", Path: "a.txt", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("before\n")))}}, Analysis: readiness.Analysis{Summary: "Change a", Scope: []readiness.ScopedPath{{Path: "a.txt", Reason: "Requested change", Evidence: []string{"issue"}}}, Acceptance: []readiness.Criterion{{Description: "a contains after", Evidence: []string{"issue"}}}, Checks: []readiness.Check{{ID: "acceptance", Category: "independent-acceptance", IndependentProvenance: "Explicit delivery test fixture", Argv: []string{"test", "ok"}, Dir: ".", TimeoutMS: 1000, Required: true, Definitions: []string{"source"}, Reason: "Fixture", Evidence: []string{"issue"}}}}}
	if repair {
		input := readiness.FileEvidence{Evidence: readiness.Evidence{ID: "human", Path: "acceptance.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("test -f a.txt\n")))}, Content: "test -f a.txt\n"}
		p.CheckInputs = []readiness.FileEvidence{input}
		p.Evidence = append(p.Evidence, input.Evidence)
		p.Analysis.Checks = []readiness.Check{
			{ID: "checker", Category: "regression", ReviewedAcceptance: "acceptance", Argv: []string{"/bin/sh", "-c", "test -f a.txt"}, Dir: ".", TimeoutMS: 1000, Required: true, Definitions: []string{"source"}, Reason: "Retain frozen checker", Evidence: []string{"issue"}},
			{ID: "acceptance", Category: "independent-acceptance", IndependentProvenance: "Separate human fixture review", Argv: []string{"/bin/sh", "{input:human}"}, Dir: ".", TimeoutMS: 1000, Required: true, Definitions: []string{"human"}, Reason: "External acceptance", Evidence: []string{"human"}},
		}
		p.Policy = readiness.ExecutionPolicy{Environment: []string{"PATH"}, SetupNetwork: "unrestricted", CheckNetwork: "unrestricted", Containment: "filtered-environment"}
		if err := p.ValidateForCoding(); err != nil {
			t.Fatal(err)
		}
	}
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
	r.FormulaSnapshot.SHA256 = p.Inputs.FormulaSHA256
	c, e := verification.Capture(context.Background(), workspace, sha, []string{"a.txt"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	r.LabDir = lab
	r.Capture = &c
	r.Atoms = []molecule.Atom{{Type: "intake", Status: "done"}, {Type: "gate", Status: "done"}, {Type: "harness", Status: "done"}}
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
	if repair {
		v := verification.Run(context.Background(), workspace, filepath.Join(t.TempDir(), "verify"), p, c)
		if !v.RequiredChecksPassed || !v.IndependentAcceptanceVerified || v.Protected == nil || v.Checks[0].Category != "candidate" {
			t.Fatalf("repaired execution: %+v", v)
		}
		r.Verification = &v
	}
	raw, _ := json.Marshal(r)
	os.MkdirAll(filepath.Join(lab, "ledgers"), 0700)
	os.WriteFile(filepath.Join(lab, "ledgers", r.ID+".json"), raw, 0600)
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("after\n"), 0600)
	return Options{LabDir: lab, MoleculeID: r.ID, Files: []string{"a.txt"}, BaseBranch: "main"}
}
func TestPreviewBindsContentWithoutStaging(t *testing.T) {
	o := fixture(t)
	p, err := Preview(context.Background(), o)
	if err != nil || !strings.Contains(p.Diff, "+after") {
		t.Fatalf("%+v %v", p, err)
	}
	cmd := exec.Command("git", "diff", "--cached", "--exit-code")
	cmd.Dir = filepath.Join(o.LabDir, "workspaces", "fixture")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("index changed: %s", b)
	}
	again, err := Preview(context.Background(), o)
	if err != nil || again.Digest != p.Digest {
		t.Fatal("preview not deterministic")
	}
	os.WriteFile(filepath.Join(cmd.Dir, "a.txt"), []byte("changed again\n"), 0600)
	_, err = Preview(context.Background(), o)
	if err == nil {
		t.Fatal("stale content not detected")
	}
}
func TestPreviewRejectsOutsideScopeAndDestinations(t *testing.T) {
	for _, kind := range []string{"outside", "push", "directory", "pathspec", "head"} {
		t.Run(kind, func(t *testing.T) {
			o := fixture(t)
			w := filepath.Join(o.LabDir, "workspaces", "fixture")
			switch kind {
			case "outside":
				os.WriteFile(filepath.Join(w, "extra"), []byte("no"), 0600)
			case "push":
				cmd := exec.Command("git", "remote", "set-url", "--push", "origin", "https://github.com/other/repo.git")
				cmd.Dir = w
				if err := cmd.Run(); err != nil {
					t.Fatal(err)
				}
			case "directory":
				o.Files = []string{"."}
			case "pathspec":
				o.Files = []string{"*"}
			case "head":
				cmd := exec.Command("git", "checkout", "--detach")
				cmd.Dir = w
				if err := cmd.Run(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Preview(context.Background(), o); err == nil {
				t.Fatal("unsafe preview accepted")
			}
		})
	}
}

func TestPreviewReviewedRepairRequiresExactEvidence(t *testing.T) {
	for _, kind := range []string{"valid", "command", "tree", "category", "acceptance-reference", "protected-command", "protected-tree", "protected-category", "protected-acceptance"} {
		t.Run(kind, func(t *testing.T) {
			o := fixtureWithRepair(t, true)
			file := filepath.Join(o.LabDir, "ledgers", o.MoleculeID+".json")
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var r molecule.Record
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "command":
				r.Verification.Checks[0].Command.Argv = []string{"true"}
			case "tree":
				r.Verification.Checks[0].SourceTree = r.Workspace.BaseCommit
			case "category":
				r.Verification.Checks[0].Category = "independent-acceptance"
			case "acceptance-reference":
				r.Verification.Checks[0].Command.ReviewedAcceptance = "other"
			case "protected-command":
				r.Verification.Protected.Checks[0].Command.Argv = []string{"true"}
			case "protected-tree":
				r.Verification.Protected.Checks[0].SourceTree = r.Verification.SourceTree
			case "protected-category":
				r.Verification.Protected.Checks[0].Category = "candidate"
			case "protected-acceptance":
				r.Verification.Protected.Checks[0].Command.ReviewedAcceptance = "other"
			}
			raw, err = json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = Preview(context.Background(), o)
			if kind == "valid" && err != nil {
				t.Fatal(err)
			}
			if kind != "valid" && err == nil {
				t.Fatal("altered evidence accepted")
			}
		})
	}
}

func TestPreviewRejectsUnreviewedAncestry(t *testing.T) {
	o := fixture(t)
	w := filepath.Join(o.LabDir, "workspaces", "fixture")
	// A target ref from an unrelated history would pull pre-existing work into the PR.
	cmd := exec.Command("git", "hash-object", "-t", "tree", "/dev/null")
	cmd.Dir = w
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "commit-tree", strings.TrimSpace(string(raw)), "-m", "unrelated root")
	cmd.Dir = w
	raw, err = cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "update-ref", "refs/remotes/origin/main", strings.TrimSpace(string(raw)))
	cmd.Dir = w
	if err = cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err = Preview(context.Background(), o); err == nil {
		t.Fatal("unreviewed ancestor commits accepted")
	}
}

func TestDeliveryRejectsMissingAndInvalidCodingEvidence(t *testing.T) {
	for _, kind := range []string{"legacy-done-only", "missing-outcome", "blocked", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			o := fixture(t)
			file := filepath.Join(o.LabDir, "ledgers", o.MoleculeID+".json")
			raw, _ := os.ReadFile(file)
			var r molecule.Record
			json.Unmarshal(raw, &r)
			switch kind {
			case "legacy-done-only":
				r.HarnessAttempts = nil
			case "missing-outcome":
				r.HarnessAttempts[0].Coding = nil
			default:
				a := &r.HarnessAttempts[0]
				report := `{"schemaVersion":"v1","outcome":"blocked","reason":"cache denied","paths":[]}`
				if kind == "malformed" {
					report = "completed"
				}
				coding := harness.CodingOutcomeFromResult(report)
				a.Coding = &coding
				result := a.Result
				result.Stdout = report
				evidence, _ := json.Marshal(struct {
					process.Result
					AttemptID  string `json:"attemptId"`
					PlanDigest string `json:"planDigest"`
				}{result, a.ID, a.PlanDigest})
				os.WriteFile(filepath.Join(r.LabDir, a.Evidence.Path), evidence, 0600)
				a.Evidence.SHA256 = fmt.Sprintf("%x", sha256.Sum256(evidence))
			}
			raw, _ = json.Marshal(r)
			os.WriteFile(file, raw, 0600)
			if _, err := Preview(context.Background(), o); err == nil {
				t.Fatal("invalid implementation evidence delivered")
			}
		})
	}
}
