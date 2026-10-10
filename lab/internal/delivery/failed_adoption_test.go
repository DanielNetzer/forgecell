package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

func failedAdoptionFixture(t *testing.T) (Options, molecule.Record) {
	t.Helper()
	o := fixture(t)
	raw, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", o.MoleculeID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var r molecule.Record
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	r.LabDir = o.LabDir
	p := r.Readiness.Plans[0].Plan
	p.Artifacts = []readiness.ArtifactRoot{{Path: "output", CommandID: "acceptance"}}
	r.Issue = molecule.Issue{Repo: p.Inputs.Issue.Repository, Number: p.Inputs.Issue.Number, URL: p.Inputs.Issue.URL, Title: p.Inputs.Issue.Title, Body: p.Inputs.Issue.Body, State: p.Inputs.Issue.State}
	c, err := verification.Capture(context.Background(), r.Workspace.Path, r.Workspace.BaseCommit, []string{"a.txt"}, p.Artifacts)
	if err != nil {
		t.Fatal(err)
	}
	store := func(name string, value any) molecule.VerificationFile {
		bytes, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.ToSlash(filepath.Join("verification-history", r.ID, name))
		os.MkdirAll(filepath.Dir(filepath.Join(o.LabDir, path)), 0700)
		if err = os.WriteFile(filepath.Join(o.LabDir, path), bytes, 0600); err != nil {
			t.Fatal(err)
		}
		return molecule.VerificationFile{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(bytes))}
	}
	outcome := process.Result{Code: 7, Reconciled: true, Stderr: "original failure"}
	originalDigest, _ := p.Digest()
	ref := store("harness-failed-attempt-result.json", struct {
		process.Result
		AttemptID  string `json:"attemptId"`
		PlanDigest string `json:"planDigest"`
	}{outcome, "failed-attempt", originalDigest})
	s, err := readiness.NewState(p, "proposed")
	if err != nil {
		t.Fatal(err)
	}
	d, _ := p.Digest()
	s, _ = s.Approve(d, p.Inputs, "approve")
	s, _ = s.StartAttempt("start")
	s, _ = s.FinishAttempt("failed", "original failure", "failed")
	s, _ = s.Interrupt(d, "known stopped failure retained", "recovered")
	s.Events[len(s.Events)-1].Tree = c.Tree
	next := p
	next.Revision = 2
	next.ParentDigest = d
	next.PausedTree = c.Tree
	next.Continuation = "adopt-failed-tree"
	next.AdoptAttempt = "failed-attempt"
	artifacts, _ := json.Marshal(c.Artifacts)
	next.PausedArtifactsSHA256 = fmt.Sprintf("%x", sha256.Sum256(artifacts))
	s, err = s.Amend(next, "amend")
	if err != nil {
		t.Fatal(err)
	}
	digest := s.Plans[1].Digest
	s.Events = append(s.Events, readiness.Event{Kind: "failed-tree-adopted", Digest: digest, Tree: c.Tree, Detail: next.AdoptAttempt})
	s, err = s.Approve(digest, next.Inputs, "approve adopted")
	if err != nil {
		t.Fatal(err)
	}
	s, err = s.StartChecks("checks")
	if err != nil {
		t.Fatal(err)
	}
	s, err = s.RecordVerification(true, "verified adopted tree", "verified")
	if err != nil {
		t.Fatal(err)
	}
	r.Readiness = &s
	r.Capture = &c
	for i := range r.Atoms {
		if r.Atoms[i].ID == "coding" {
			r.Atoms[i].Status = "failed"
		}
	}
	outcome.Stderr = "" // Ledger projection omits streams retained in the immutable result.
	r.HarnessAttempts = []molecule.HarnessAttempt{{ID: next.AdoptAttempt, PlanDigest: d, Result: outcome, Evidence: &ref, Capture: &c, CaptureEvidence: []molecule.VerificationFile{store("capture.json", c)}, Recovered: true}}
	return o, r
}

func TestDeliveryRequiresExactFailedTreeAdoption(t *testing.T) {
	for _, kind := range []string{"valid", "missing-review", "later-attempt", "tree", "artifacts", "uncertain", "live-artifacts", "stale-issue", "stale-formula"} {
		t.Run(kind, func(t *testing.T) {
			o, r := failedAdoptionFixture(t)
			switch kind {
			case "missing-review":
				for i := range r.Readiness.Events {
					if r.Readiness.Events[i].Kind == "failed-tree-adopted" {
						r.Readiness.Events[i].Kind = "ignored"
					}
				}
			case "later-attempt":
				a := r.HarnessAttempts[0]
				a.ID = "later"
				r.HarnessAttempts = append(r.HarnessAttempts, a)
			case "tree":
				r.Capture.Tree = r.Workspace.BaseCommit
			case "artifacts":
				r.Capture.Artifacts = []verification.ArtifactInventory{{}}
			case "live-artifacts":
				os.MkdirAll(filepath.Join(r.Workspace.Path, "output"), 0700)
				os.WriteFile(filepath.Join(r.Workspace.Path, "output", "cache"), []byte("drift"), 0600)
			case "stale-issue":
				r.Issue.Body += "changed"
			case "stale-formula":
				r.FormulaSnapshot.SHA256 = "changed"
			case "uncertain":
				r.HarnessAttempts[0].Result.Reconciled = false
			}
			raw, _ := json.Marshal(r)
			if err := os.WriteFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Preview(context.Background(), o)
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe adopted failed work accepted for delivery")
			}
			if r.Atoms[2].Status != "failed" {
				t.Fatal("original failure was rewritten")
			}
		})
	}
}
