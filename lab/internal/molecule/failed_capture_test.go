package molecule

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFailedHarnessCapturesPartialWork(t *testing.T) {
	for _, outside := range []bool{false, true} {
		t.Run(map[bool]string{false: "in-scope", true: "out-of-scope"}[outside], func(t *testing.T) {
			o, _ := fixture(t)
			script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
			body := "#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nprintf failed >&2\n"
			if outside {
				body += "printf stray > stray.txt\n"
			}
			body += "exit 7\n"
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			r, err := runApprovedFixture(t, o)
			if err != nil {
				t.Fatal(err)
			}
			if r.Capture == nil || r.Capture.Tree == "" {
				t.Fatal("failed harness tree was not captured")
			}
			if outside && len(r.Capture.Violations) != 1 {
				t.Fatalf("missing violation: %+v", r.Capture)
			}
			if r.Atoms[2].Status != "failed" {
				t.Fatal("failed coding became successful")
			}
			digest := r.Readiness.Plans[0].Digest
			if _, err = Recover(context.Background(), o.LabDir, r.ID, digest, true); err != nil {
				t.Fatalf("known stopped failure cannot be recovered: %v", err)
			}
		})
	}
}

func TestApprovedFailedTreeVerificationDoesNotReplayHarness(t *testing.T) {
	o, _ := fixture(t)
	count := filepath.Join(t.TempDir(), "calls")
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	body := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\necho call >> %q\nprintf partial-output\nprintf failed >&2\nexit 7\n", count)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	parent := r.Readiness.Plans[0]
	next := copyPlan(t, parent.Plan)
	next.Continuation = "checks-only"
	if _, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, ""); err == nil {
		t.Fatal("ordinary checks-only accepted failed coding")
	}
	r, err = Recover(context.Background(), o.LabDir, r.ID, parent.Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	next.Continuation = "adopt-failed-tree"
	next.AdoptAttempt = r.HarnessAttempts[0].ID
	r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = r.Readiness.Plans[1].Digest
	r, err = Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !FailedTreeAdopted(r) || r.Verification == nil || !r.Verification.RequiredChecksPassed || r.Atoms[2].Status != "failed" {
		t.Fatalf("adopted work was not verified separately: %+v", r)
	}
	if _, err = r.Readiness.ValidateForDelivery(); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(count)
	if err != nil || string(calls) != "call\n" {
		t.Fatalf("paid harness replayed: %q %v", calls, err)
	}
	ref := r.HarnessAttempts[0].Evidence
	raw, err := os.ReadFile(filepath.Join(r.LabDir, ref.Path))
	if err != nil {
		t.Fatal(err)
	}
	var result process.Result
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Code != 7 || result.Stdout != "partial-output" || result.Stderr != "failed" {
		t.Fatalf("failed evidence lost: %+v", result)
	}
}

func TestFailedTreeAdoptionRejectsDriftAndUncertainty(t *testing.T) {
	for _, kind := range []string{"tree", "attempt", "artifacts", "process", "lost-result", "unreviewed", "issue"} {
		t.Run(kind, func(t *testing.T) {
			o, _ := fixture(t)
			script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
			os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nexit 7\n"), 0700)
			r, err := runApprovedFixture(t, o)
			if err != nil {
				t.Fatal(err)
			}
			p := r.Readiness.Plans[0]
			if kind != "unreviewed" {
				r, err = Recover(context.Background(), o.LabDir, r.ID, p.Digest, true)
				if err != nil {
					t.Fatal(err)
				}
			}
			next := copyPlan(t, p.Plan)
			next.Continuation = "adopt-failed-tree"
			next.AdoptAttempt = r.HarnessAttempts[0].ID
			switch kind {
			case "tree":
				os.WriteFile(filepath.Join(r.Workspace.Path, "code.txt"), []byte("drift"), 0600)
			case "attempt":
				next.AdoptAttempt = "stale"
			case "artifacts":
				next.Artifacts = []readiness.ArtifactRoot{{Path: "output", CommandID: "check"}}
			case "process":
				r.HarnessAttempts[0].Result.Reconciled = false
				save(r)
			case "lost-result":
				r.HarnessAttempts[0].Evidence = nil
				save(r)
			case "issue":
				next.Inputs.Issue.Body += " drift"
			}
			if _, err = Amend(context.Background(), o.LabDir, r.ID, p.Digest, next, ""); err == nil {
				t.Fatal("unsafe adoption accepted")
			}
		})
	}
}

func TestFailedHarnessCaptureErrorRetainsOutcome(t *testing.T) {
	o, _ := fixture(t)
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nrm code.txt\nmkdir code.txt\nprintf kept\nexit 9\n"), 0700)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	a := r.HarnessAttempts[0]
	if a.Result.Code != 9 || a.Evidence == nil || a.CaptureError == "" || r.Capture.Tree != "" {
		t.Fatalf("incomplete capture accepted: %+v", a)
	}
	next := copyPlan(t, r.Readiness.Plans[0].Plan)
	next.Continuation = "adopt-failed-tree"
	next.AdoptAttempt = a.ID
	if _, err = Amend(context.Background(), o.LabDir, r.ID, a.PlanDigest, next, ""); err == nil {
		t.Fatal("incomplete tree adopted")
	}
}

func TestFailedOutOfScopeAdoptionRequiresSeparateDiffReview(t *testing.T) {
	o, _ := fixture(t)
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nprintf stray > stray.txt\nexit 7\n"), 0700)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	p := r.Readiness.Plans[0]
	r, err = Recover(context.Background(), o.LabDir, r.ID, p.Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	next := copyPlan(t, p.Plan)
	next.Continuation = "adopt-failed-tree"
	next.AdoptAttempt = r.HarnessAttempts[0].ID
	next.Analysis.Scope = append(next.Analysis.Scope, readiness.ScopedPath{Path: "stray.txt", Reason: "Reviewed existing failed write", Evidence: next.Analysis.Scope[1].Evidence})
	if _, err = Amend(context.Background(), o.LabDir, r.ID, p.Digest, next, ""); err == nil {
		t.Fatal("violating diff was silently adopted")
	}
	r, err = Amend(context.Background(), o.LabDir, r.ID, p.Digest, next, r.Capture.Tree)
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = r.Readiness.Plans[1].Digest
	r, err = Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !FailedTreeAdopted(r) || r.Atoms[2].Status != "failed" {
		t.Fatal("failure was erased")
	}
	if len(r.HarnessAttempts[0].Capture.Violations) != 1 {
		t.Fatal("original violating capture was erased")
	}
	if _, err = r.Readiness.ValidateForDelivery(); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedHarnessCapturesUsingUncancelledContext(t *testing.T) {
	o, _ := fixture(t)
	signal := filepath.Join(t.TempDir(), "started")
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nprintf started > %q\nsleep 10\n", signal)), 0700)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = r.Readiness.Plans[0].Digest
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
				if _, e := os.Stat(signal); e == nil {
					cancel()
					return
				}
			}
		}
	}()
	watchdog := time.AfterFunc(10*time.Second, cancel)
	defer watchdog.Stop()
	r, err = Run(ctx, o)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || !r.HarnessAttempts[0].Result.Interrupted || r.Capture == nil || r.Capture.Tree == "" || r.HarnessAttempts[0].CaptureError != "" {
		t.Fatalf("cancellation lost capture: %+v", r.HarnessAttempts)
	}
}

func TestTimedOutHarnessRetainsPartialTree(t *testing.T) {
	o, _ := fixture(t)
	recipePath := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	recipe, err := os.ReadFile(recipePath)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(recipe), "harness:\n", "harness:\n  timeoutMs: 100\n", 1)
	approveFixture(t, o.LabDir, text, "timeout-fixture")
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nsleep 10\n"), 0700)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || !r.HarnessAttempts[0].Result.TimedOut || r.Capture == nil || r.Capture.Tree == "" || r.HarnessAttempts[0].CaptureError != "" {
		t.Fatalf("timeout lost capture: %+v", r.HarnessAttempts)
	}
}

func TestLostHarnessResultRefusesRecoveryWithSurvivingProcess(t *testing.T) {
	o, _ := fixture(t)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	p := r.Readiness.Plans[0]
	s, err := r.Readiness.Approve(p.Digest, p.Plan.Inputs, stamp())
	if err != nil {
		t.Fatal(err)
	}
	s, err = s.StartAttempt(stamp())
	if err != nil {
		t.Fatal(err)
	}
	r.Readiness = &s
	r.HarnessAttempts = []HarnessAttempt{{ID: "lost", PlanDigest: p.Digest}}
	if err = save(r); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("/bin/sleep", "10")
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	if _, err = Recover(context.Background(), o.LabDir, r.ID, p.Digest, true); err == nil {
		t.Fatal("lost result admitted despite surviving process")
	}
	// Recovery must not signal an unrelated process to manufacture certainty.
	if err = exec.Command("/bin/kill", "-0", fmt.Sprint(child.Process.Pid)).Run(); err != nil {
		t.Fatal("recovery affected surviving process")
	}
}

func TestRecoveryKeepsDurableFailureBeforePhaseSelection(t *testing.T) {
	o, _ := fixture(t)
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nexit 7\n"), 0700)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	digest := r.Readiness.Plans[0].Digest
	// Reproduce the durable boundary after Result, before capture/state selection.
	r.Readiness.Events = r.Readiness.Events[:len(r.Readiness.Events)-1]
	r.Readiness.Phase = "coding"
	r.Atoms[2].Status = "active"
	r.HarnessAttempts[0].Capture = nil
	r.HarnessAttempts[0].CaptureEvidence = nil
	if err = save(r); err != nil {
		t.Fatal(err)
	}
	r, err = Recover(context.Background(), o.LabDir, r.ID, digest, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Atoms[2].Status != "failed" || !failedTreeEligible(r, r.HarnessAttempts[0].ID) {
		t.Fatal("known failure was rewritten as unknown")
	}
}

func TestRecoveryRefusesLaterUncertainVerification(t *testing.T) {
	for _, kind := range []string{"started", "missing", "setup", "check", "protected", "stopped"} {
		t.Run(kind, func(t *testing.T) {
			o, _ := fixture(t)
			script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
			if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nexit 7\n"), 0700); err != nil {
				t.Fatal(err)
			}
			r, err := runApprovedFixture(t, o)
			if err != nil {
				t.Fatal(err)
			}
			p := r.Readiness.Plans[0]
			if !r.HarnessAttempts[0].Result.Reconciled {
				t.Fatal("coding was not reconciled")
			}
			if err = beginVerification(&r, p.Plan, r.Capture.Tree); err != nil {
				t.Fatal(err)
			}
			if kind != "started" {
				v := verification.Result{SchemaVersion: "v1", SourceTree: r.Capture.Tree}
				obs := verification.Observation{ID: "uncertain", Result: process.Result{Reconciled: false, Uncertainty: "descendant may still be live"}}
				switch kind {
				case "setup":
					v.Setup = []verification.Observation{obs}
				case "stopped":
					obs.Result.Reconciled = true
					obs.Result.Uncertainty = ""
					v.Checks = []verification.Observation{obs}
				case "check":
					v.Checks = []verification.Observation{obs}
				case "protected":
					v.Protected = &verification.Result{Checks: []verification.Observation{obs}}
				}
				if err = completeVerification(&r, v); err != nil {
					t.Fatal(err)
				}
				if kind == "missing" {
					r.VerificationAttempts[0].Result = nil
				}
			}
			if err = save(r); err != nil {
				t.Fatal(err)
			}
			if kind == "stopped" {
				if _, err = Recover(context.Background(), o.LabDir, r.ID, p.Digest, true); err != nil {
					t.Fatal(err)
				}
				return
			}
			child := exec.Command("/bin/sleep", "30")
			if err = child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { child.Process.Kill(); child.Wait() }()
			before, err := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = Recover(context.Background(), o.LabDir, r.ID, p.Digest, true); err == nil {
				t.Fatal("recovery trusted coding evidence over later uncertain verification")
			}
			if err = exec.Command("/bin/kill", "-0", fmt.Sprint(child.Process.Pid)).Run(); err != nil {
				t.Fatal("recovery affected surviving process")
			}
			after, err := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+".json"))
			if err != nil || string(before) != string(after) {
				t.Fatal("refused recovery changed ledger")
			}
		})
	}
}

func TestFailureOutputRemainsInEvidenceWithoutLedgerDuplication(t *testing.T) {
	o, _ := fixture(t)
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nawk 'BEGIN { for (i=0; i<900000; i++) printf \"%c\", 1 }'\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	assertEvidence := func(r Record) {
		t.Helper()
		a := r.HarnessAttempts[0]
		raw, err := os.ReadFile(filepath.Join(r.LabDir, a.Evidence.Path))
		if err != nil {
			t.Fatal(err)
		}
		var result process.Result
		if err = json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if result.Stdout != strings.Repeat("\x01", 900000) || result.Code != 7 || a.Capture == nil || a.Capture.Tree == "" {
			t.Fatal("failed output or tree lost")
		}
		ledger, err := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+".json"))
		if err != nil || len(ledger) > 100000 {
			t.Fatalf("ledger duplicates streams: %d %v", len(ledger), err)
		}
	}
	assertEvidence(r)
	// Crash boundary: durable result saved before failure phase selection.
	r.Readiness.Events = r.Readiness.Events[:len(r.Readiness.Events)-1]
	r.Readiness.Phase = "coding"
	r.Atoms[2].Status = "active"
	if err = save(r); err != nil {
		t.Fatal(err)
	}
	r, err = Recover(context.Background(), o.LabDir, r.ID, r.Readiness.Plans[0].Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	assertEvidence(r)
	if r.Atoms[2].Status != "failed" {
		t.Fatal("recovery erased failure")
	}
}

func TestUncertainCompletedVerificationBlocksAmendmentAndExecution(t *testing.T) {
	for _, command := range []string{"setup", "check", "protected"} {
		for _, continuation := range []string{"", "checks-only"} {
			for _, entry := range []string{"amend", "execute"} {
				t.Run(command+"/"+continuation+"/"+entry, func(t *testing.T) {
					o, _ := fixture(t)
					r, err := runApprovedFixture(t, o)
					if err != nil {
						t.Fatal(err)
					}
					parent := r.Readiness.Plans[len(r.Readiness.Plans)-1]
					next := copyPlan(t, parent.Plan)
					next.Continuation = continuation
					if entry == "execute" {
						r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
						if err != nil {
							t.Fatal(err)
						}
						o.Approve = r.Readiness.Plans[len(r.Readiness.Plans)-1].Digest
					}
					if err = beginVerification(&r, parent.Plan, r.Capture.Tree); err != nil {
						t.Fatal(err)
					}
					obs := verification.Observation{ID: "uncertain", Result: process.Result{Code: 7, Reconciled: false, Uncertainty: "descendant may still be live"}}
					v := verification.Result{SchemaVersion: "v1", SourceTree: r.Capture.Tree}
					switch command {
					case "setup":
						v.Setup = []verification.Observation{obs}
					case "check":
						v.Checks = []verification.Observation{obs}
					case "protected":
						v.Protected = &verification.Result{Checks: []verification.Observation{obs}}
					}
					if err = completeVerification(&r, v); err != nil {
						t.Fatal(err)
					}
					if entry == "amend" {
						r.Readiness.Phase = "failed"
					}
					if err = save(r); err != nil {
						t.Fatal(err)
					}
					ledger := filepath.Join(r.LabDir, "ledgers", r.ID+".json")
					before, err := os.ReadFile(ledger)
					if err != nil {
						t.Fatal(err)
					}
					code, err := os.ReadFile(filepath.Join(r.Workspace.Path, "code.txt"))
					if err != nil {
						t.Fatal(err)
					}
					if entry == "amend" {
						_, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
					} else {
						_, err = Run(context.Background(), o)
					}
					if err == nil || !strings.Contains(err.Error(), "stopped-process evidence") {
						t.Fatalf("uncertain completed verification was not refused: %v", err)
					}
					after, err := os.ReadFile(ledger)
					if err != nil || string(before) != string(after) {
						t.Fatal("refusal changed ledger")
					}
					afterCode, err := os.ReadFile(filepath.Join(r.Workspace.Path, "code.txt"))
					if err != nil || string(code) != string(afterCode) {
						t.Fatal("refusal changed workspace")
					}
				})
			}
		}
	}
}

func TestSemanticFailedTreeAdoptionPreservesProcessSuccess(t *testing.T) {
	for _, report := range []string{`{"schemaVersion":"v1","outcome":"blocked","reason":"cache denied","paths":[]}`, `malformed`} {
		for _, crash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/crash=%t", report, crash), func(t *testing.T) {
				o, _ := fixture(t)
				os.WriteFile(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nprintf '%s' '"+report+"'\n"), 0700)
				r, err := runApprovedFixture(t, o)
				if err != nil {
					t.Fatal(err)
				}
				original := *r.HarnessAttempts[0].Coding
				parent := r.Readiness.Plans[0]
				if crash {
					r.Readiness.Phase = "coding"
					r.Readiness.Events = r.Readiness.Events[:len(r.Readiness.Events)-1]
					if err = save(r); err != nil {
						t.Fatal(err)
					}
				}
				r, err = Recover(context.Background(), o.LabDir, r.ID, parent.Digest, true)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(original, *r.HarnessAttempts[0].Coding) || !r.HarnessAttempts[0].Result.OK || completedCoding(r) {
					t.Fatal("semantic failure or transport truth lost")
				}
				next := copyPlan(t, parent.Plan)
				next.Continuation = "checks-only"
				if _, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, ""); err == nil {
					t.Fatal("semantic failure bypassed adoption")
				}
				next.Continuation = "adopt-failed-tree"
				next.AdoptAttempt = r.HarnessAttempts[0].ID
				r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
				if err != nil {
					t.Fatal(err)
				}
				o.Approve = r.Readiness.Plans[1].Digest
				r, err = Run(context.Background(), o)
				if err != nil {
					t.Fatal(err)
				}
				if !FailedTreeAdopted(r) || completedCoding(r) || r.Verification == nil || !r.Verification.RequiredChecksPassed || !reflect.DeepEqual(original, *r.HarnessAttempts[0].Coding) {
					t.Fatal("adoption erased implementation outcome")
				}
			})
		}
	}
}

func TestRecoveryAfterAdoptedViolationPreservesCaptureEvidence(t *testing.T) {
	o, _ := fixture(t)
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nprintf stray > stray.txt\nexit 7\n"), 0700)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	p := r.Readiness.Plans[0]
	r, err = Recover(context.Background(), o.LabDir, r.ID, p.Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	next := copyPlan(t, p.Plan)
	next.Analysis.Checks[0].Argv = []string{"/bin/sh", "-c", "exit 1"}
	next.Continuation = "adopt-failed-tree"
	next.AdoptAttempt = r.HarnessAttempts[0].ID
	next.Analysis.Scope = append(next.Analysis.Scope, readiness.ScopedPath{Path: "stray.txt", Reason: "Reviewed existing failed write", Evidence: next.Analysis.Scope[1].Evidence})
	if _, err = Amend(context.Background(), o.LabDir, r.ID, p.Digest, next, ""); err == nil {
		t.Fatal("violating diff was silently adopted")
	}
	r, err = Amend(context.Background(), o.LabDir, r.ID, p.Digest, next, r.Capture.Tree)
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = r.Readiness.Plans[1].Digest
	r, err = Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !FailedTreeAdopted(r) || r.Atoms[2].Status != "failed" {
		t.Fatal("failure was erased")
	}
	if len(r.HarnessAttempts[0].Capture.Violations) != 1 {
		t.Fatal("original violating capture was erased")
	}
	original := *r.HarnessAttempts[0].Capture
	latest := r.Readiness.Plans[1]
	r, err = Recover(context.Background(), o.LabDir, r.ID, latest.Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, *r.HarnessAttempts[0].Capture) {
		t.Fatal("recovery erased original capture violations")
	}
	if err = ValidateVerificationHistory(r); err != nil {
		t.Fatal(err)
	}
	next = copyPlan(t, latest.Plan)
	if _, err = Amend(context.Background(), o.LabDir, r.ID, latest.Digest, next, ""); err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 {
		t.Fatal("coding replayed")
	}
}

func TestRecoveryCompletedCaptureBeforePhaseSelection(t *testing.T) {
	o, _ := fixture(t)
	counter := filepath.Join(t.TempDir(), "calls")
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte(fmt.Sprintf("echo call >> %q\n", counter))...)
	if err = os.WriteFile(script, body, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	parent := r.Readiness.Plans[0]
	for i, e := range r.Readiness.Events {
		if e.Kind == "attempt-check-pending" {
			r.Readiness.Events = r.Readiness.Events[:i]
			break
		}
	}
	r.Readiness.Phase = "coding"
	r.Atoms[2].Status = "active"
	if err = save(r); err != nil {
		t.Fatal(err)
	}
	r, err = Recover(context.Background(), o.LabDir, r.ID, parent.Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	if !completedCoding(r) {
		t.Fatal("durable completed capture lost")
	}
	next := copyPlan(t, parent.Plan)
	next.Continuation = "checks-only"
	r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = r.Readiness.Plans[1].Digest
	r, err = Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(counter)
	if err != nil || string(calls) != "call\n" {
		t.Fatalf("harness calls: %q %v", calls, err)
	}
	if len(r.HarnessAttempts) != 1 {
		t.Fatal("coding replayed")
	}
}

func TestSemanticViolationRetainsBothOutcomes(t *testing.T) {
	for _, outcome := range []string{"blocked", "unknown", "scope-change"} {
		t.Run(outcome, func(t *testing.T) {
			o, _ := fixture(t)
			counter := filepath.Join(t.TempDir(), "calls")
			report := `malformed`
			if outcome == "blocked" {
				report = `{"schemaVersion":"v1","outcome":"blocked","reason":"denied","paths":[]}`
			}
			if outcome == "scope-change" {
				report = `{"schemaVersion":"v1","outcome":"scope-change","reason":"need scope","paths":["stray.txt"]}`
			}
			os.WriteFile(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nprintf stray > stray.txt\nprintf '%s' '"+report+"'\n"+fmt.Sprintf("echo call >> %q\n", counter)), 0700)
			r, err := runApprovedFixture(t, o)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range r.Readiness.Events {
				if e.Kind == "attempt-"+outcome {
					found = true
				}
			}
			if !found || statePhase(r) != "scope-violation" || len(r.VerificationAttempts) != 0 {
				t.Fatal("semantic outcome lost behind violation")
			}
			parent := r.Readiness.Plans[0]
			r, err = Recover(context.Background(), o.LabDir, r.ID, parent.Digest, true)
			if err != nil {
				t.Fatal(err)
			}
			next := copyPlan(t, parent.Plan)
			next.Continuation = "adopt-failed-tree"
			next.AdoptAttempt = r.HarnessAttempts[0].ID
			next.Analysis.Scope = append(next.Analysis.Scope, readiness.ScopedPath{Path: "stray.txt", Reason: "Reviewed diff", Evidence: next.Analysis.Scope[1].Evidence})
			if _, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, ""); err == nil {
				t.Fatal("missing diff review accepted")
			}
			r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, r.Capture.Tree)
			if err != nil {
				t.Fatal(err)
			}
			o.Approve = r.Readiness.Plans[1].Digest
			r, err = Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(counter)
			if err != nil || string(calls) != "call\n" {
				t.Fatalf("harness calls: %q %v", calls, err)
			}
			if len(r.HarnessAttempts) != 1 || !FailedTreeAdopted(r) {
				t.Fatal("adoption replayed or lost")
			}
		})
	}
}
