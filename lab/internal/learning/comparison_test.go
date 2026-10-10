package learning

import (
	"context"
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/evaluation"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyComparisonIsExplicitAndDoesNotActivate(t *testing.T) {
	d, p := fixture(t)
	before, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
	got, err := Read(d, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	var view map[string]any
	json.Unmarshal(raw, &view)
	c, ok := view["comparison"].(map[string]any)
	if !ok || c["status"] != "missing" {
		t.Fatalf("missing comparison state: %s", raw)
	}
	after, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
	if string(before) != string(after) {
		t.Fatal("inspection activated Formula")
	}
}

func manualFixture(t *testing.T) (string, Suggestion) {
	t.Helper()
	d, p := fixture(t)
	p.ProposedYAML = strings.Replace(recipe, "Check tests.", "Run targeted tests.", 1)
	p.ProposedHash = hash(p.ProposedYAML)
	if err := save(d, p); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(d, "ledgers"), 0700)
	for i, id := range []string{"mol-before", "mol-after"} {
		yaml := p.OriginalYAML
		if i == 1 {
			yaml = p.ProposedYAML
		}
		r := molecule.Record{ID: id, SchemaVersion: "v0", Kind: "molecule", Mode: "ticket", FormulaID: p.FormulaID, FormulaApproved: true, FinishedAt: stamp(), Status: "failed", FormulaSnapshot: molecule.Snapshot{YAML: yaml, SHA256: hash(yaml)}, Issue: molecule.Issue{Number: int64(i + 1), Repo: "owner/repo"}}
		raw, _ := json.Marshal(r)
		os.WriteFile(filepath.Join(d, "ledgers", id+".json"), raw, 0600)
	}
	return d, p
}
func TestManualComparisonRoundtripIdentityAndNoActivation(t *testing.T) {
	d, p := manualFixture(t)
	before, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
	options := LinkOptions{Baseline: "mol-before", Candidate: "mol-after", Basis: "Human comparison of related failures; task and checks differ."}
	got, err := LinkComparison(d, p.ID, options)
	if err != nil {
		t.Fatal(err)
	}
	if got.Comparison.Status != "manual" || len(got.Comparison.Links) != 1 || got.Comparison.Links[0].Kind != "manual" || len(got.Comparison.Links[0].Differences) == 0 {
		t.Fatalf("%+v", got.Comparison)
	}
	again, err := LinkComparison(d, p.ID, options)
	if err != nil || len(again.Comparison.Links) != 1 {
		t.Fatalf("not idempotent: %+v %v", again, err)
	}
	os.Remove(filepath.Join(d, "ledgers/mol-before.json"))
	got, err = Read(d, p.ID)
	if err != nil || got.Comparison.Status != "manual" {
		t.Fatalf("lost immutable history: %+v %v", got.Comparison, err)
	}
	p.OriginApproval.DecisionID = "different"
	save(d, p)
	got, err = Read(d, p.ID)
	if err != nil || got.Comparison.Status != "stale" {
		t.Fatalf("different approval: %+v %v", got.Comparison, err)
	}
	after, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
	if string(before) != string(after) {
		t.Fatal("link activated Formula")
	}
}
func TestManualComparisonRejectsMissingBasisAndSymlink(t *testing.T) {
	d, p := manualFixture(t)
	if _, err := LinkComparison(d, p.ID, LinkOptions{Baseline: "mol-before", Candidate: "mol-after"}); err == nil {
		t.Fatal("missing human basis accepted")
	}
	path := filepath.Join(d, "ledgers/mol-before.json")
	raw, _ := os.ReadFile(path)
	os.Remove(path)
	target := filepath.Join(d, "original.json")
	os.WriteFile(target, raw, 0600)
	os.Symlink(target, path)
	if _, err := LinkComparison(d, p.ID, LinkOptions{Baseline: "mol-before", Candidate: "mol-after", Basis: "Compare failures"}); err == nil {
		t.Fatal("symlink accepted")
	}
}

// A local shell fixture exercises the evaluator without any model or service.
func controlledFixture(t *testing.T) (string, Suggestion, string) {
	t.Helper()
	d, p := fixture(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.Mkdir(repo, 0700)
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repo
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("fixture git: %s %v", b, e)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.test")
	git("remote", "add", "origin", "https://github.com/owner/repo.git")
	os.WriteFile(filepath.Join(repo, "value.txt"), []byte("before"), 0600)
	git("add", ".")
	git("commit", "-qm", "fixture")
	shell := filepath.Join(root, "harness")
	os.WriteFile(shell, []byte("#!/bin/sh\ncat >/dev/null\nprintf changed > value.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Fixture changed value\",\"paths\":[]}'\n"), 0700)
	command, _ := json.Marshal([]string{shell})
	p.OriginalYAML = strings.Replace(recipe, "harness:", "intake: {repo: owner/repo}\nharness:", 1)
	p.OriginalYAML = strings.Replace(p.OriginalYAML, "command: [echo]", "command: "+string(command), 1)
	p.ProposedYAML = strings.Replace(p.OriginalYAML, "Check tests.", "Run targeted tests.", 1)
	p.OriginalHash = hash(p.OriginalYAML)
	p.ProposedHash = hash(p.ProposedYAML)
	p.OriginApproval.SHA256 = p.OriginalHash
	// The saved test decision authorizes only the test evaluator's fake shell.
	p.Status = "approved"
	p.ReviewedAt = stamp()
	if err := save(d, p); err != nil {
		t.Fatal(err)
	}
	inputs := filepath.Join(root, "inputs")
	os.Mkdir(inputs, 0700)
	approval, _ := os.ReadFile(filepath.Join(d, "suggestions", p.ID+".json"))
	issue := []byte(`{"number":1,"repo":"owner/repo","title":"Fixture","body":"Change value","url":"https://github.com/owner/repo/issues/1","state":"OPEN"}`)
	acceptance := []byte("#!/bin/sh\ntest \"$(cat value.txt)\" = changed\n")
	for name, b := range map[string][]byte{"approval.json": approval, "issue.json": issue, "acceptance.sh": acceptance} {
		os.WriteFile(filepath.Join(inputs, name), b, 0600)
	}
	plan := evaluation.Plan{SchemaVersion: 1, ReadinessVersion: 1, TargetBranch: "main", Approval: "approval.json", ApprovalSHA256: hash(string(approval)), Task: "issue.json", TaskSHA256: hash(string(issue)), Acceptance: "acceptance.sh", AcceptanceSHA256: hash(string(acceptance)), BaseCommit: git("rev-parse", "HEAD"), AllowedFiles: []string{"value.txt"}, Checks: []evaluation.Command{{Name: "acceptance", Dir: ".", Argv: []string{"/bin/sh", "{acceptance}"}, TimeoutMS: 5000}}}
	raw, _ := json.Marshal(plan)
	file := filepath.Join(inputs, "plan.json")
	os.WriteFile(file, raw, 0600)
	out := filepath.Join(root, "results")
	pair, e := evaluation.ValidateApproval(approval)
	if e != nil {
		t.Fatal(e)
	}
	report, e := evaluation.Run(context.Background(), evaluation.RunOptions{Inputs: file, SourceRoot: repo, Output: out, Approve: evaluation.ActivationDigest(raw, pair, repo, out)})
	if e != nil || !report.Attempts[0].Correct {
		t.Fatalf("fixture evaluator: %v", e)
	}
	return d, p, out
}
func TestControlledLinksRetainConflictsAndExactIdentity(t *testing.T) {
	d, p, out := controlledFixture(t)
	original, _ := os.ReadFile(filepath.Join(out, "report.json"))
	suggestionBefore, _ := os.ReadFile(filepath.Join(d, "suggestions", p.ID+".json"))
	formulaBefore, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
	got, err := LinkComparison(d, p.ID, LinkOptions{Evaluation: out})
	if err != nil || got.Comparison.Status != "comparable" {
		t.Fatalf("%s %s %v", got.Comparison.Status, got.Comparison.Reason, err)
	}
	// Inspection and identical links are read-only with respect to source evidence.
	got, err = LinkComparison(d, p.ID, LinkOptions{Evaluation: out})
	if err != nil || len(got.Comparison.Links) != 1 {
		t.Fatalf("idempotency: %v", err)
	}
	reportAfter, _ := os.ReadFile(filepath.Join(out, "report.json"))
	if string(reportAfter) != string(original) {
		t.Fatal("linked evidence overwritten")
	}
	var r evaluation.Report
	json.Unmarshal(original, &r)
	r.Attempts[1].Checks[0].Result.OK = false
	r.Attempts[1].Checks[0].Result.Code = 1
	r.Attempts[1].Verification.Checks = r.Attempts[1].Checks
	r.Attempts[1].Correct = false
	r.Attempts[1].Verification.Correct = false
	raw, _ := json.Marshal(r)
	os.WriteFile(filepath.Join(out, "report.json"), raw, 0600)
	got, err = LinkComparison(d, p.ID, LinkOptions{Evaluation: out})
	if err != nil || got.Comparison.Status != "conflicting" || len(got.Comparison.Links) != 2 {
		t.Fatalf("conflict: %s %s %v", got.Comparison.Status, got.Comparison.Reason, err)
	}
	// Historical links survive active Formula drift and removal of their originals.
	os.WriteFile(filepath.Join(d, "formulas/default.yaml"), []byte(recipe+"# later active Formula\n"), 0600)
	os.RemoveAll(out)
	got, err = Read(d, p.ID)
	if err != nil || got.Comparison.Status != "conflicting" {
		t.Fatalf("historical read: %s %v", got.Comparison.Status, err)
	}
	os.WriteFile(filepath.Join(d, "formulas/default.yaml"), formulaBefore, 0600)
	after, _ := os.ReadFile(filepath.Join(d, "suggestions", p.ID+".json"))
	if string(after) != string(suggestionBefore) {
		t.Fatal("linking mutated suggestion review")
	}
	p.OriginApproval.DecisionID = "new-human-approval"
	save(d, p)
	got, err = Read(d, p.ID)
	if err != nil || got.Comparison.Status != "stale" {
		t.Fatalf("approval identity: %s %v", got.Comparison.Status, err)
	}
}
func TestComparisonTamperAndInterruptedWriteRemainReadable(t *testing.T) {
	d, p := manualFixture(t)
	options := LinkOptions{Baseline: "mol-before", Candidate: "mol-after", Basis: "Related stopped work"}
	got, err := LinkComparison(d, p.ID, options)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(d, "comparisons", p.ID)
	os.WriteFile(filepath.Join(root, ".comparison-interrupted"), []byte("partial"), 0600)
	got, err = Read(d, p.ID)
	if err != nil || got.Comparison.Status != "manual" {
		t.Fatal("partial write exposed")
	}
	link := filepath.Join(root, got.Comparison.Links[0].ID+".json")
	os.WriteFile(link, []byte("{}"), 0600)
	got, err = Read(d, p.ID)
	if err != nil || got.Comparison.Status != "stale" {
		t.Fatalf("tamper: %s %v", got.Comparison.Status, err)
	}
	if _, err = Review(d, p.ID, "dismiss"); err != nil {
		t.Fatalf("optional corruption blocked review: %v", err)
	}
}

func TestMalformedOriginApprovalFailsClosed(t *testing.T) {
	d, p, out := controlledFixture(t)
	evidence, err := evaluation.ReadEvidence(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	var approval map[string]any
	json.Unmarshal(evidence.Approval, &approval)
	approval["originApproval"] = "malformed"
	raw, _ := json.Marshal(approval)
	evidence.Snapshots[0].Files["inputs/approval.json"] = raw
	record := comparisonRecord{Version: 1, Identity: comparisonIdentity(p), Kind: "controlled", Snapshots: evidence.Snapshots}
	raw, _ = json.Marshal(record)
	if err := comparisonDirectory(d, p.ID); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "comparisons", p.ID, hash(string(raw))+".json"), raw, 0600)
	got, err := Read(d, p.ID)
	if err != nil || got.Comparison.Status != "stale" || !strings.Contains(got.Comparison.Reason, "approval") {
		t.Fatalf("malformed origin not rejected: %s %s %v", got.Comparison.Status, got.Comparison.Reason, err)
	}
}

func TestPrettyPrintedOriginApprovalAndDismissedLink(t *testing.T) {
	d, p, out := controlledFixture(t)
	// MarshalIndent saved the approval with whitespace; typed identity must match.
	got, err := LinkComparison(d, p.ID, LinkOptions{Evaluation: out})
	if err != nil || got.Comparison.Status != "comparable" {
		t.Fatalf("pretty approval: %s %s %v", got.Comparison.Status, got.Comparison.Reason, err)
	}
	d, p = manualFixture(t)
	if _, err = LinkComparison(d, p.ID, LinkOptions{Baseline: "mol-before", Candidate: "mol-after", Basis: "Human comparison"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Review(d, p.ID, "dismiss"); err != nil {
		t.Fatal(err)
	}
	got, err = Read(d, p.ID)
	if err != nil || got.Comparison.Status != "manual" {
		t.Fatalf("dismiss invalidated evidence: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(d, "suggestions", p.ID+".json"))
	var saved Suggestion
	json.Unmarshal(raw, &saved)
	if len(saved.Comparison.Links) != 0 {
		t.Fatal("derived links persisted")
	}
}

func TestIncompleteControlledLinksPreserveDiagnosis(t *testing.T) {
	for _, scenario := range []string{"missing-input", "parentless-recheck"} {
		t.Run(scenario, func(t *testing.T) {
			d, p, out := controlledFixture(t)
			reason := "missing frozen inputs: inputs/approval.json"
			if scenario == "missing-input" {
				if err := os.Remove(filepath.Join(out, "inputs/approval.json")); err != nil {
					t.Fatal(err)
				}
			} else {
				next := out + "-recheck"
				if _, err := evaluation.Recheck(context.Background(), filepath.Join(filepath.Dir(out), "repo"), out, next, nil); err != nil {
					t.Fatal(err)
				}
				out = next
				reason = "recheck requires its original report and frozen input chain"
			}
			before, err := os.ReadFile(filepath.Join(out, "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				got, err := LinkComparison(d, p.ID, LinkOptions{Evaluation: out})
				if err != nil || got.Comparison.Status != "incomplete" || got.Comparison.Reason != reason || len(got.Comparison.Links) != 1 {
					t.Fatalf("diagnosis lost: %+v %v", got.Comparison, err)
				}
				link := got.Comparison.Links[0]
				if link.Status != "incomplete" || link.Reason != reason {
					t.Fatalf("link diagnosis lost: %+v", link)
				}
			}
			after, err := os.ReadFile(filepath.Join(out, "report.json"))
			if err != nil || string(before) != string(after) {
				t.Fatal("link changed report", err)
			}
		})
	}
}
