package learning

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
)

func eligibilityHeadSnapshot(commit, status, at string) checks.Snapshot {
	s := checks.Snapshot{Repo: "owner/repo", PR: 9, Commit: commit, ObservedHead: commit, CheckedAt: at, Required: []string{"test"}, Status: status, Checks: []checks.Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS", URL: "https://github.com/owner/repo/actions/runs/1"}}}
	if status == "failed" {
		s.Checks[0].Conclusion = "FAILURE"
	}
	return s
}

// Learning evidence names the change request's current head, and CI recorded
// for the first commit never classifies a fix round's head.
func TestLearningEvidenceFollowsTheReceiptHead(t *testing.T) {
	d := t.TempDir()
	r := verifiedCIRecord(t)
	if err := os.MkdirAll(filepath.Join(d, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	ledger, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(d, "ledgers", r.ID+".json"), ledger, 0600); err != nil {
		t.Fatal(err)
	}
	first := strings.Repeat("1", 40)
	receipt := delivery.Receipt{State: "published", DraftAtPublication: true, Commit: first, URL: "https://github.com/owner/repo/pull/9", Preview: delivery.PreviewResult{MoleculeID: r.ID, Repo: r.Issue.Repo, BaseCommit: r.Workspace.BaseCommit, Branch: r.Workspace.Branch, Tree: r.Verification.SourceTree}}
	raw, _ := json.Marshal(receipt.Preview)
	receipt.Preview.Digest = fmt.Sprintf("%x", sha256.Sum256(raw))
	raw, _ = json.Marshal(receipt)
	if err := os.MkdirAll(filepath.Join(d, "deliveries"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "deliveries", r.ID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	o := delivery.Options{LabDir: d, MoleculeID: r.ID}
	if _, err := delivery.AppendCI(o, eligibilityHeadSnapshot(first, "passed", "2026-11-02T10:00:00Z")); err != nil {
		t.Fatal(err)
	}
	e, err := classifyEvidence(d, r)
	if err != nil || e.Commit != first || e.CIClassification != "passed" || len(e.CIObservations) != 1 {
		t.Fatalf("before a fix round: %+v %v", e, err)
	}

	head := strings.Repeat("2", 40)
	fr := delivery.FixRound{Round: 1, Parent: first, Commit: head, Tree: strings.Repeat("3", 40), PlanDigest: strings.Repeat("4", 64), VerificationTree: r.Verification.SourceTree, Findings: []string{strings.Repeat("5", 64)}, PushedAt: "2026-11-02T11:00:00Z", ObservedHead: head}
	if _, err := delivery.AppendFixRound(o, fr); err != nil {
		t.Fatal(err)
	}
	e, err = classifyEvidence(d, r)
	if err != nil || e.Commit != head || e.Outcome != "draft-published" {
		t.Fatalf("after a fix round: %+v %v", e, err)
	}
	if len(e.CIObservations) != 1 || e.CIObservations[0].Identity.Commit != first {
		t.Fatalf("the first commit's CI is no longer listed: %+v", e.CIObservations)
	}
	if e.CIClassification != "unknown" {
		t.Fatalf("the first commit's pass classified the head: %s", e.CIClassification)
	}
	if _, err := delivery.AppendCI(o, eligibilityHeadSnapshot(head, "failed", "2026-11-02T12:00:00Z")); err != nil {
		t.Fatal(err)
	}
	e, err = classifyEvidence(d, r)
	if err != nil || e.CIClassification != "failure" || len(e.CIObservations) != 2 {
		t.Fatalf("head CI: %+v %v", e, err)
	}
}
