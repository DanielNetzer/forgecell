package molecule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Corruption must be rejected before approval consumption or any ledger write.
func TestExecutionIdentityRejectsWithoutMutation(t *testing.T) {
	for _, kind := range []string{"yaml", "coherent-snapshot", "hash", "formula-id", "extra", "missing", "duplicate", "type"} {
		for _, operation := range []string{"resume", "amend", "recover"} {
			t.Run(kind+"/"+operation, func(t *testing.T) {
				o, _ := fixture(t)
				calls := filepath.Join(t.TempDir(), "calls")
				script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
				body, e := os.ReadFile(script)
				if e != nil {
					t.Fatal(e)
				}
				body = append(body, []byte(fmt.Sprintf("echo call >> %q\n", calls))...)
				if e = os.WriteFile(script, body, 0700); e != nil {
					t.Fatal(e)
				}
				r, err := Run(context.Background(), o)
				if err == nil && operation == "recover" {
					o.Approve = r.Readiness.Plans[0].Digest
					r, err = Run(context.Background(), o)
				}
				if err != nil {
					t.Fatal(err)
				}
				p := r.Readiness.Plans[0]
				switch kind {
				case "yaml":
					r.FormulaSnapshot.YAML += "\n# changed\n"
				case "coherent-snapshot":
					r.FormulaSnapshot.YAML += "\n# altered snapshot\n"
					r.FormulaSnapshot.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(r.FormulaSnapshot.YAML)))
				case "hash":
					r.FormulaSnapshot.SHA256 = "changed"
				case "formula-id":
					r.FormulaID = "other"
				case "extra":
					r.Atoms = append(r.Atoms, Atom{ID: "extra", Type: "check"})
				case "missing":
					r.Atoms = r.Atoms[1:]
				case "duplicate":
					r.Atoms[2].ID = r.Atoms[0].ID
				case "type":
					r.Atoms[2].Type = "check"
				}
				if err = save(r); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				beforeCalls, _ := os.ReadFile(calls)
				switch operation {
				case "resume":
					o.Approve = p.Digest
					_, err = Run(context.Background(), o)
				case "amend":
					_, err = Amend(context.Background(), o.LabDir, r.ID, p.Digest, p.Plan, "")
				case "recover":
					_, err = Recover(context.Background(), o.LabDir, r.ID, p.Digest, true)
				}
				if err == nil {
					t.Fatal("invalid identity accepted")
				}
				if !strings.Contains(err.Error(), "Formula") && !strings.Contains(err.Error(), "Atom") {
					t.Fatalf("rejected for unrelated reason: %v", err)
				}
				after, e := os.ReadFile(path)
				if e != nil || !bytes.Equal(before, after) {
					t.Fatal("ledger changed")
				}
				afterCalls, _ := os.ReadFile(calls)
				if !bytes.Equal(beforeCalls, afterCalls) {
					t.Fatal("coding invoked")
				}
			})
		}
	}
}

func roleFixture(t *testing.T) Options {
	t.Helper()
	o, _ := fixture(t)
	path := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"intake", "scope", "harness", "check", "gate", "ship", "document"} {
		raw = bytes.ReplaceAll(raw, []byte("id: "+id+","), []byte("id: custom-"+id+","))
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	approveFixture(t, o.LabDir, string(raw), "custom-roles")
	return o
}

func reorderRecordedAtoms(t *testing.T, r Record) Record {
	t.Helper()
	for i, j := 0, len(r.Atoms)-1; i < j; i, j = i+1, j-1 {
		r.Atoms[i], r.Atoms[j] = r.Atoms[j], r.Atoms[i]
	}
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func namedAtom(t *testing.T, r Record, id string) Atom {
	t.Helper()
	for _, a := range r.Atoms {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("missing %s", id)
	return Atom{}
}

func TestRoleExecutionLifecycle(t *testing.T) {
	for _, mode := range []string{"ordinary", "amend", "recovery-checks-only", "failed-adoption"} {
		t.Run(mode, func(t *testing.T) {
			o := roleFixture(t)
			if mode == "failed-adoption" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte("#!/bin/sh\ncat > request.json\nprintf changed > code.txt\nprintf partial\nexit 7\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			r = reorderRecordedAtoms(t, r)
			parent := r.Readiness.Plans[0]
			if mode == "amend" {
				next := copyPlan(t, parent.Plan)
				next.Analysis.Summary = "Reviewed successor scope"
				r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
				if err != nil {
					t.Fatal(err)
				}
				if namedAtom(t, r, "custom-scope").Status != "waiting" {
					t.Fatal("wrong scope gate")
				}
				o.Approve = parent.Digest
				if _, err = Run(context.Background(), o); err == nil {
					t.Fatal("stale approval accepted")
				}
			}
			o.Approve = r.Readiness.Plans[len(r.Readiness.Plans)-1].Digest
			r, err = Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ordinary" || mode == "amend" {
				if !CompletedCoding(r) || namedAtom(t, r, "custom-check").Status != "done" || namedAtom(t, r, "custom-gate").Status != "waiting" || namedAtom(t, r, "custom-ship").Status != "skipped" {
					t.Fatal("incorrect role transition")
				}
				coding := namedAtom(t, r, "custom-harness")
				check := namedAtom(t, r, "custom-check")
				if len(r.HarnessAttempts) != 1 || len(r.VerificationAttempts) != 1 || len(coding.Provenance.Actions) != 1 || len(check.Provenance.Actions) != 1 {
					t.Fatal("missing or duplicated execution evidence")
				}
				if coding.Provenance.Actions[0].PlanDigest != o.Approve || check.Provenance.Actions[0].PlanDigest != o.Approve || coding.Provenance.Actions[0].AttemptID == check.Provenance.Actions[0].AttemptID {
					t.Fatal("execution evidence routed to wrong approval or attempt")
				}
				return
			}
			original, err := os.ReadFile(filepath.Join(r.LabDir, r.HarnessAttempts[0].Evidence.Path))
			if err != nil {
				t.Fatal(err)
			}
			codingBefore, _ := json.Marshal(namedAtom(t, r, "custom-harness").Provenance)
			if mode == "recovery-checks-only" {
				for i, e := range r.Readiness.Events {
					if e.Kind == "attempt-check-pending" {
						r.Readiness.Events = r.Readiness.Events[:i]
						break
					}
				}
				r.Readiness.Phase = "coding"
				for i := range r.Atoms {
					if r.Atoms[i].ID == "custom-harness" {
						r.Atoms[i].Status = "active"
					}
				}
				if err = save(r); err != nil {
					t.Fatal(err)
				}
			}
			r, err = Recover(context.Background(), o.LabDir, r.ID, parent.Digest, true)
			if err != nil {
				t.Fatal(err)
			}
			if statePhase(r) != "interrupted" || len(r.HarnessAttempts) != 1 {
				t.Fatal("recovery did not retain the interrupted attempt")
			}
			if mode == "recovery-checks-only" && (!CompletedCoding(r) || namedAtom(t, r, "custom-harness").Status != "done") {
				t.Fatal("recovery did not resolve the durable coding outcome by identity")
			}
			next := copyPlan(t, parent.Plan)
			next.Continuation = "checks-only"
			if mode == "failed-adoption" {
				if CompletedCoding(r) {
					t.Fatal("failed coding promoted")
				}
				if _, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, ""); err == nil {
					t.Fatal("failed coding accepted checks-only")
				}
				next.Continuation = "adopt-failed-tree"
				next.AdoptAttempt = r.HarnessAttempts[0].ID
			}
			r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
			if err != nil {
				t.Fatal(err)
			}
			o.Approve = r.Readiness.Plans[1].Digest
			r, err = Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			after, e := os.ReadFile(filepath.Join(r.LabDir, r.HarnessAttempts[0].Evidence.Path))
			if e != nil || !bytes.Equal(original, after) {
				t.Fatal("original outcome rewritten")
			}
			codingAfter, _ := json.Marshal(namedAtom(t, r, "custom-harness").Provenance)
			if !bytes.Equal(codingBefore, codingAfter) || len(r.HarnessAttempts) != 1 || r.Verification == nil || !r.Verification.RequiredChecksPassed {
				t.Fatal("coding replayed or evidence changed")
			}
			if mode == "failed-adoption" {
				if !FailedTreeAdopted(r) || CompletedCoding(r) || namedAtom(t, r, "custom-harness").Status != "failed" {
					t.Fatal("failed adoption promoted original result")
				}
			} else {
				actions := namedAtom(t, r, "custom-check").Provenance.Actions
				if len(r.VerificationAttempts) != 2 || len(actions) != 2 || actions[0].AttemptID == actions[1].AttemptID || actions[0].PlanDigest == actions[1].PlanDigest {
					t.Fatal("verification attempts merged")
				}
			}
			if namedAtom(t, r, "custom-gate").Status != "waiting" {
				t.Fatal("review gate bypassed")
			}
		})
	}
}
