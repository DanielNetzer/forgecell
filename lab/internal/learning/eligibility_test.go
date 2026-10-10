package learning

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLearningDoesNotInferOutcomeFromFinishedAt(t *testing.T) {
	for _, phase := range []string{"scope-waiting", "coding", "check-pending", "approved"} {
		r := molecule.Record{ID: "mol-test", FinishedAt: "old", Status: "waiting", Readiness: &readiness.State{Phase: phase}}
		if _, err := classifyEvidence(t.TempDir(), r); err == nil {
			t.Fatalf("admitted unfinished %s", phase)
		}
	}
	r := molecule.Record{ID: "mol-test", FinishedAt: "done", Status: "failed"}
	e, err := classifyEvidence(t.TempDir(), r)
	if err != nil || e.Outcome != "failed" {
		t.Fatalf("%+v %v", e, err)
	}
	r.Status = "done"
	e, err = classifyEvidence(t.TempDir(), r)
	if err != nil || e.Outcome != "historical-unknown" {
		t.Fatalf("legacy success invented: %+v %v", e, err)
	}
}

func TestVerifiedAndPublicationEvidenceRemainDistinct(t *testing.T) {
	r := verifiedCIRecord(t)
	dir := t.TempDir()
	e, err := classifyEvidence(dir, r)
	if err != nil || e.Outcome != "verified-undelivered" {
		t.Fatalf("%+v %v", e, err)
	}
	os.MkdirAll(filepath.Join(dir, "deliveries"), 0700)
	receipt := delivery.Receipt{State: "committed"}
	raw, _ := json.Marshal(receipt)
	file := filepath.Join(dir, "deliveries", "mol-1.json")
	os.WriteFile(file, raw, 0600)
	if _, err = classifyEvidence(dir, r); err == nil {
		t.Fatal("uncertain publication accepted")
	}
	receipt.State = "published"
	receipt.Commit = strings.Repeat("1", 40)
	receipt.URL = "https://github.com/owner/repo/pull/1"
	receipt.Preview = delivery.PreviewResult{MoleculeID: r.ID, Repo: r.Issue.Repo, BaseCommit: r.Workspace.BaseCommit, Branch: r.Workspace.Branch, Tree: r.Verification.SourceTree}
	data, _ := json.Marshal(receipt.Preview)
	sum := sha256.Sum256(data)
	receipt.Preview.Digest = fmt.Sprintf("%x", sum)
	for _, draft := range []bool{false, true} {
		receipt.DraftAtPublication = draft
		raw, _ = json.Marshal(receipt)
		os.WriteFile(file, raw, 0600)
		e, err = classifyEvidence(dir, r)
		want := "published-draft-status-unknown"
		if draft {
			want = "draft-published"
		}
		if err != nil || e.Outcome != want {
			t.Fatalf("%+v %v", e, err)
		}
	}
	for _, state := range []string{"CLOSED", "MERGED", "OPEN", "UNKNOWN"} {
		receipt.Lifecycle = &delivery.LifecycleObservation{State: state, ObservedAt: "2026-10-09T00:00:00Z", Reconciliation: "head moved or uncertain"}
		raw, _ = json.Marshal(receipt)
		os.WriteFile(file, raw, 0600)
		e, err = classifyEvidence(dir, r)
		if err != nil || e.Outcome != "draft-published" || e.Lifecycle == nil || *e.Lifecycle != *receipt.Lifecycle || e.Commit != receipt.Commit || e.URL != receipt.URL {
			t.Fatalf("historical outcome rewritten: %+v %v", e, err)
		}
	}
	receipt.Preview.Tree = strings.Repeat("0", 40)
	raw, _ = json.Marshal(receipt)
	os.WriteFile(file, raw, 0600)
	if _, err = classifyEvidence(dir, r); err == nil {
		t.Fatal("mismatched receipt accepted")
	}
}

func verifiedCIRecord(t *testing.T) molecule.Record {
	t.Helper()
	p := readiness.Plan{SchemaVersion: "v1", MoleculeID: "mol-1", Revision: 1, Inputs: readiness.Inputs{Repository: "owner/repo", TargetBranch: "main", BaseCommit: strings.Repeat("a", 40), FormulaSHA256: strings.Repeat("b", 64), EvidenceSHA256: strings.Repeat("c", 64), BindingSHA256: strings.Repeat("d", 64), Issue: readiness.Issue{Repository: "owner/repo", Number: 1, URL: "https://github.com/owner/repo/issues/1", State: "OPEN", Title: "Fix"}}, Evidence: []readiness.Evidence{{ID: "source", Path: "a.go", SHA256: strings.Repeat("e", 64)}}, Analysis: readiness.Analysis{Summary: "Fix", Scope: []readiness.ScopedPath{{Path: "a.go", Reason: "Fix", Evidence: []string{"issue"}}}, Acceptance: []readiness.Criterion{{Description: "Works", Evidence: []string{"issue"}}}, Checks: []readiness.Check{{ID: "test", Category: "candidate", Argv: []string{"true"}, Dir: ".", TimeoutMS: 1000, Required: true, Definitions: []string{"source"}, Reason: "Test", Evidence: []string{"issue"}}}}}
	s, err := readiness.NewState(p, "start")
	if err != nil {
		t.Fatal(err)
	}
	d, _ := p.Digest()
	s, err = s.Approve(d, p.Inputs, "approve")
	if err != nil {
		t.Fatal(err)
	}
	s, _ = s.StartAttempt("code")
	s, _ = s.FinishAttempt("check-pending", "captured", "end")
	s, _ = s.RecordVerification(true, "passed", "verified")
	r := molecule.Record{ID: "mol-1", Readiness: &s, Status: "waiting", FinishedAt: "done", Issue: molecule.Issue{Repo: "owner/repo"}, Workspace: molecule.Workspace{BaseCommit: p.Inputs.BaseCommit, Branch: "forgecell/test"}, Verification: &verification.Result{RequiredChecksPassed: true, SourceTree: strings.Repeat("f", 40)}}
	return r
}
