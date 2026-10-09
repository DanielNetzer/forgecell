package evaluation

import (
	"context"
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func evaluationFixture(t *testing.T) (RunOptions, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.Mkdir(repo, 0700)
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repo
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %s %v", args, b, e)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-q")
	git("config", "user.email", "test@example.test")
	git("config", "user.name", "Test")
	git("remote", "add", "origin", "https://github.com/owner/repo.git")
	os.WriteFile(filepath.Join(repo, "value.txt"), []byte("before"), 0600)
	git("add", ".")
	git("commit", "-qm", "base")
	inputs := filepath.Join(root, "inputs")
	os.Mkdir(inputs, 0700)
	harness := filepath.Join(root, "harness")
	os.WriteFile(harness, []byte("#!/bin/sh\ncat > /dev/null\nprintf changed > value.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Implemented and tested\",\"paths\":[]}'\n"), 0700)
	var a Approval
	json.Unmarshal(approved(t), &a)
	command, _ := json.Marshal([]string{harness})
	a.OriginalYAML = strings.Replace(a.OriginalYAML, "[test-harness]", string(command), 1)
	a.OriginalYAML = strings.Replace(a.OriginalYAML, "atoms: [{id: gate, type: gate}]", "atoms: [{id: intake, type: intake}, {id: scope, type: gate, purpose: scope}, {id: harness, type: harness}, {id: check, type: check}, {id: gate, type: gate, purpose: review}, {id: ship, type: ship}, {id: document, type: document}]", 1)
	a.ProposedYAML = strings.Replace(a.OriginalYAML, "  command:", "  instructions: Report actual outcomes separately.\n  command:", 1)
	a.OriginalHash = Hash([]byte(a.OriginalYAML))
	a.ProposedHash = Hash([]byte(a.ProposedYAML))
	raw, _ := json.Marshal(a)
	os.WriteFile(filepath.Join(inputs, "approval.json"), raw, 0600)
	task := []byte(`{"number":12,"repo":"owner/repo","title":"Change value","body":"Change value to changed","url":"https://github.com/owner/repo/issues/12","state":"OPEN"}`)
	os.WriteFile(filepath.Join(inputs, "issue.json"), task, 0600)
	acceptance := []byte("#!/bin/sh\ntest \"$(cat value.txt)\" = changed\n")
	os.WriteFile(filepath.Join(inputs, "acceptance.sh"), acceptance, 0600)
	p := Plan{ReadinessVersion: 1, TargetBranch: "main", SchemaVersion: 1, Approval: "approval.json", ApprovalSHA256: Hash(raw), Task: "issue.json", TaskSHA256: Hash(task), Acceptance: "acceptance.sh", AcceptanceSHA256: Hash(acceptance), BaseCommit: git("rev-parse", "HEAD"), AllowedFiles: []string{"value.txt"}, RequireInitialFailure: true, Checks: []Command{{Name: "acceptance", Dir: ".", Argv: []string{"/bin/sh", "{acceptance}"}, TimeoutMS: 5000}}}
	raw, _ = json.Marshal(p)
	file := filepath.Join(inputs, "inputs.json")
	os.WriteFile(file, raw, 0600)
	return RunOptions{Inputs: file, SourceRoot: repo, Output: filepath.Join(root, "results")}, repo
}
func TestPairUsesMatchingBaseAndRetainsIndependentResults(t *testing.T) {
	o, repo := evaluationFixture(t)
	approveEvaluation(t, &o)
	report, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Attempts) != 2 || report.Status != "complete" {
		t.Fatalf("%+v", report)
	}
	for _, a := range report.Attempts {
		active, e := formula.Load(a.Molecule.LabDir, "")
		if e != nil || !active.Approved || active.Approval.SourceKind != "evaluation-activation" || active.Approval.SourceID != o.Approve || report.ActivationDigest != o.Approve || report.ActivationDecisionAt == "" {
			t.Fatalf("missing exact activation: %+v %v", active, e)
		}

		if !a.Correct || a.CostUSD != nil || a.Retries != nil || a.Tree == "" || a.Molecule.Workspace.BaseCommit != report.BaseCommit {
			t.Fatalf("%+v", a)
		}
	}
	if report.Attempts[0].Molecule.Workspace.Path == report.Attempts[1].Molecule.Workspace.Path {
		t.Fatal("variants reused checkout")
	}
	raw, _ := os.ReadFile(filepath.Join(repo, "value.txt"))
	if string(raw) != "before" {
		t.Fatal("source changed")
	}
	if _, err = Run(context.Background(), o); err == nil {
		t.Fatal("overwrote previous attempts")
	}
}
func TestModifiedFrozenCheckRefusedBeforeExecution(t *testing.T) {
	o, _ := evaluationFixture(t)
	os.WriteFile(filepath.Join(filepath.Dir(o.Inputs), "acceptance.sh"), []byte("exit 0"), 0600)
	if _, err := Run(context.Background(), o); err == nil {
		t.Fatal("modified acceptance accepted")
	}
}

func TestIgnoredCheckerModificationCannotProduceCorrectResult(t *testing.T) {
	o, repo := evaluationFixture(t)
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("cache/\n"), 0600)
	for _, args := range [][]string{{"add", ".gitignore"}, {"commit", "-qm", "ignore checker dependencies"}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("%s %v", b, e)
		}
	}
	c := exec.Command("git", "rev-parse", "HEAD")
	c.Dir = repo
	head, _ := c.Output()
	harness := filepath.Join(filepath.Dir(repo), "harness")
	os.WriteFile(harness, []byte("#!/bin/sh\ncat >/dev/null\nprintf 'exit 0\\n' > cache/checker\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Implemented and tested\",\"paths\":[]}'\n"), 0700)
	var p Plan
	raw, _ := os.ReadFile(o.Inputs)
	json.Unmarshal(raw, &p)
	p.BaseCommit = strings.TrimSpace(string(head))
	p.Artifacts = []readiness.ArtifactRoot{{Path: "cache", CommandID: "dependencies"}}
	p.Setup = []Command{{Name: "dependencies", Dir: ".", Argv: []string{"/bin/sh", "-c", "mkdir -p cache; printf 'exit 1\\n' > cache/checker"}, TimeoutMS: 5000}}
	acceptance := []byte("#!/bin/sh\nsh cache/checker\n")
	os.WriteFile(filepath.Join(filepath.Dir(o.Inputs), p.Acceptance), acceptance, 0600)
	p.AcceptanceSHA256 = Hash(acceptance)
	raw, _ = json.Marshal(p)
	os.WriteFile(o.Inputs, raw, 0600)
	approveEvaluation(t, &o)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Attempts {
		if a.Correct || !a.ScopeOK || len(a.ChangedFiles) != 0 {
			t.Fatalf("poisoned checker accepted: %+v", a)
		}
	}
}
func TestBlockedMoleculeCannotBeCorrect(t *testing.T) {
	o, _ := evaluationFixture(t)
	dir := filepath.Dir(o.Inputs)
	var a Approval
	raw, _ := os.ReadFile(filepath.Join(dir, "approval.json"))
	json.Unmarshal(raw, &a)
	for _, dest := range []*string{&a.OriginalYAML, &a.ProposedYAML} {
		*dest = strings.Replace(*dest, "{id: gate, type: gate, purpose: review}", "{id: unsupported, type: unsupported}, {id: gate, type: gate, purpose: review}", 1)
	}
	a.OriginalHash = Hash([]byte(a.OriginalYAML))
	a.ProposedHash = Hash([]byte(a.ProposedYAML))
	raw, _ = json.Marshal(a)
	os.WriteFile(filepath.Join(dir, "approval.json"), raw, 0600)
	var p Plan
	planRaw, _ := os.ReadFile(o.Inputs)
	json.Unmarshal(planRaw, &p)
	p.ApprovalSHA256 = Hash(raw)
	planRaw, _ = json.Marshal(p)
	os.WriteFile(o.Inputs, planRaw, 0600)
	approveEvaluation(t, &o)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range r.Attempts {
		if attempt.Molecule.Status != "blocked" || attempt.Correct {
			t.Fatalf("blocked run accepted: %+v", attempt)
		}
	}
}

func TestRecheckPreservesEvidenceAndDoesNotRerunHarness(t *testing.T) {
	o, repo := evaluationFixture(t)
	approveEvaluation(t, &o)
	r, err := Run(context.Background(), o)
	if err != nil || !r.Attempts[0].Correct {
		t.Fatalf("%+v %v", r, err)
	}
	original, err := os.ReadFile(filepath.Join(o.Output, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(filepath.Dir(repo), "harness"), []byte("#!/bin/sh\nexit 99\n"), 0700)
	checked, err := Recheck(context.Background(), repo, o.Output, o.Output+"-recheck", nil)
	if err != nil || len(checked.Attempts) != 2 {
		t.Fatalf("%+v %v", checked, err)
	}
	for _, a := range checked.Attempts {
		if !a.Correct {
			t.Fatalf("%+v", a)
		}
	}
	after, _ := os.ReadFile(filepath.Join(o.Output, "report.json"))
	if string(original) != string(after) {
		t.Fatal("original evidence changed")
	}
}

func approveEvaluation(t *testing.T, o *RunOptions) {
	t.Helper()
	raw, e := os.ReadFile(o.Inputs)
	if e != nil {
		t.Fatal(e)
	}
	var p Plan
	if e = json.Unmarshal(raw, &p); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(filepath.Dir(o.Inputs), p.Approval))
	if e != nil {
		t.Fatal(e)
	}
	pair, e := ValidateApproval(b)
	if e != nil {
		t.Fatal(e)
	}
	source, e := filepath.Abs(o.SourceRoot)
	if e != nil {
		t.Fatal(e)
	}
	output, e := filepath.Abs(o.Output)
	if e != nil {
		t.Fatal(e)
	}
	o.Approve = ActivationDigest(raw, pair, source, output)
}
