package molecule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCheckOnlyRetryRetainsHistoryAndNeverCodes(t *testing.T) {
	o, _ := fixture(t)
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, e := analyze(ctx, b, i, s)
		a.Checks[0].Argv = []string{"/bin/sh", "-c", "echo first-failure; exit 1"}
		return a, e
	}
	r, e := runApprovedFixture(t, o)
	if e != nil {
		t.Fatal(e)
	}
	if r.Verification == nil || r.Verification.RequiredChecksPassed {
		t.Fatal("expected failed check")
	}
	trace := filepath.Join(r.Workspace.Path, "request.json")
	before, _ := os.ReadFile(trace)
	p := r.Readiness.Plans[len(r.Readiness.Plans)-1]
	raw, _ := json.Marshal(p.Plan)
	var next map[string]any
	json.Unmarshal(raw, &next)
	next["continuation"] = "checks-only"
	raw, _ = json.Marshal(next)
	var plan readiness.Plan
	json.Unmarshal(raw, &plan)
	plan.Analysis.Checks[0].Argv = []string{"/bin/sh", "-c", "echo retry-passed"}
	amended, e := Amend(context.Background(), o.LabDir, r.ID, p.Digest, plan, "")
	if e != nil {
		t.Fatal(e)
	}
	o.Approve = amended.Readiness.Plans[len(amended.Readiness.Plans)-1].Digest
	r, e = Run(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(trace)
	if string(before) != string(after) {
		t.Fatal("check retry invoked coding harness")
	}
	raw, _ = json.Marshal(r)
	var record map[string]any
	json.Unmarshal(raw, &record)
	history, _ := record["verificationAttempts"].([]any)
	if len(history) != 2 {
		t.Fatalf("lost verification history: %d", len(history))
	}
	if _, e = r.Readiness.ValidateForDelivery(); e != nil {
		t.Fatal(e)
	}
}

func TestCheckOnlyRejectsDriftAndUncertainCoding(t *testing.T) {
	for _, kind := range []string{"source", "scope", "binding", "uncertain"} {
		t.Run(kind, func(t *testing.T) {
			o, _ := fixture(t)
			r, e := runApprovedFixture(t, o)
			if e != nil {
				t.Fatal(e)
			}
			p := r.Readiness.Plans[0]
			next := p.Plan
			next.Continuation = "checks-only"
			switch kind {
			case "source":
				os.WriteFile(filepath.Join(r.Workspace.Path, "code.txt"), []byte("drift"), 0600)
			case "scope":
				next.Analysis.Scope = next.Analysis.Scope[:1]
			case "uncertain":
				r.Atoms[2].Status = "active"
				r.Readiness.Phase = "interrupted"
				if e = save(r); e != nil {
					t.Fatal(e)
				}
			}
			amended, e := Amend(context.Background(), o.LabDir, r.ID, p.Digest, next, "")
			if kind != "binding" {
				if e == nil {
					t.Fatal("unsafe retry admitted")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := os.ReadFile(filepath.Join(o.LabDir, "formulas/sample.yaml"))
			os.WriteFile(filepath.Join(o.LabDir, "formulas/sample.yaml"), append(raw, []byte("# binding drift\n")...), 0600)
			o.Approve = amended.Readiness.Plans[1].Digest
			if _, e = Run(context.Background(), o); e == nil {
				t.Fatal("changed Formula accepted")
			}
		})
	}
}

func TestReviewedDefinitionRepairChecksOnlyPreservesExactSourceAndHistory(t *testing.T) {
	o, _ := fixture(t)
	harness := filepath.Join(filepath.Dir(o.LabDir), "harness")
	raw, err := os.ReadFile(harness)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\nprintf '%s' '{\"scripts\":{\"test\":\"true\"}}' > package.json\n")...)
	if err = os.WriteFile(harness, raw, 0700); err != nil {
		t.Fatal(err)
	}
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, e := analyze(ctx, b, i, s)
		a.Scope = append(a.Scope, readiness.ScopedPath{Path: "package.json", Reason: "Edit frozen checker manifest", Evidence: a.Checks[0].Definitions})
		a.Checks[0].Category = "regression"
		return a, e
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	parent := r.Readiness.Plans[0]
	next := copyPlan(t, parent.Plan)
	content := "exit 1\n"
	input := readiness.FileEvidence{Evidence: readiness.Evidence{ID: "human", Path: "acceptance.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}, Content: content}
	next.CheckInputs = []readiness.FileEvidence{input}
	next.Evidence = append(next.Evidence, input.Evidence)
	next.Analysis.Checks[0].ReviewedAcceptance = "human-check"
	next.Analysis.Checks = append(next.Analysis.Checks, readiness.Check{ID: "human-check", Category: "independent-acceptance", IndependentProvenance: "human fixture review", Argv: []string{"/bin/sh", "{input:human}"}, Dir: ".", TimeoutMS: 1000, Required: true, Definitions: []string{"human"}, Evidence: []string{"human"}, Reason: "Frozen external acceptance"})
	r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("repair proposal executed coding")
	}
	o.Approve = parent.Digest
	if _, err = Run(context.Background(), o); err == nil {
		t.Fatal("stale approval accepted repair")
	}
	o.Approve = r.Readiness.Plans[1].Digest
	r, err = Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verification == nil || r.Verification.RequiredChecksPassed || r.Verification.Protected == nil || !r.Verification.Protected.RequiredChecksPassed {
		t.Fatal("failed human acceptance lost distinct protected success")
	}
	tree := r.Capture.Tree
	artifacts := artifactHash(*r.Capture)
	trace, _ := os.ReadFile(filepath.Join(r.Workspace.Path, "request.json"))
	historicalPlans, _ := json.Marshal(r.Readiness.Plans)
	historicalEvents, _ := json.Marshal(r.Readiness.Events)
	historicalAttempts, _ := json.Marshal(r.VerificationAttempts)
	evidenceBefore, _, err := LearningVerificationEvidence(r, 4000000)
	if err != nil {
		t.Fatal(err)
	}
	parent = r.Readiness.Plans[1]
	next = copyPlan(t, parent.Plan)
	next.Continuation = "checks-only"
	next.CheckInputs[0].Content = "test \"$(cat code.txt)\" = changed\n"
	next.CheckInputs[0].SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(next.CheckInputs[0].Content)))
	next.Evidence[len(next.Evidence)-1] = next.CheckInputs[0].Evidence
	r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Capture.Tree != tree || artifactHash(*r.Capture) != artifacts {
		t.Fatal("checks-only proposal changed source/artifacts")
	}
	plans, _ := json.Marshal(r.Readiness.Plans[:2])
	events, _ := json.Marshal(r.Readiness.Events[:len(r.Readiness.Events)-1])
	attempts, _ := json.Marshal(r.VerificationAttempts)
	evidenceAfter, _, err := LearningVerificationEvidence(r, 4000000)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plans, historicalPlans) || !bytes.Equal(events, historicalEvents) || !bytes.Equal(attempts, historicalAttempts) || !reflect.DeepEqual(evidenceBefore, evidenceAfter) {
		t.Fatal("repair rewrote immutable history")
	}
	o.Approve = r.Readiness.Plans[2].Digest
	r, err = Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(r.Workspace.Path, "request.json"))
	if !bytes.Equal(trace, after) || r.Capture.Tree != tree || artifactHash(*r.Capture) != artifacts || len(r.HarnessAttempts) != 1 || len(r.VerificationAttempts) != 2 {
		t.Fatal("repair replayed coding or lost source/history")
	}
	if r.Verification == nil || !r.Verification.RequiredChecksPassed || !r.Verification.IndependentAcceptanceVerified || r.Verification.Protected == nil || !r.Verification.Protected.RequiredChecksPassed {
		t.Fatal("safe repaired continuation failed")
	}
}

func TestVerificationEvidenceSurvivesAmendmentAndRejectsCorruption(t *testing.T) {
	o, _ := fixture(t)
	r, e := runApprovedFixture(t, o)
	if e != nil {
		t.Fatal(e)
	}
	a := r.VerificationAttempts[0]
	if a.Status != "passed" || a.Result == nil || len(a.Observations) == 0 {
		t.Fatal("missing evidence")
	}
	path := filepath.Join(o.LabDir, a.Result.Path)
	before, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	p := r.Readiness.Plans[0]
	next := p.Plan
	next.Continuation = "checks-only"
	amended, e := Amend(context.Background(), o.LabDir, r.ID, p.Digest, next, "")
	if e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) || len(amended.VerificationAttempts) != 1 {
		t.Fatal("amendment lost evidence")
	}
	os.WriteFile(path, []byte("corrupted"), 0600)
	o.Approve = amended.Readiness.Plans[1].Digest
	if _, e = Run(context.Background(), o); e == nil {
		t.Fatal("corrupted history accepted")
	}
}

func TestLargeVerificationOutputIsRetainedOutsideLedger(t *testing.T) {
	o, _ := fixture(t)
	a := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		v, e := a(ctx, b, i, s)
		v.Checks[0].Argv = []string{"/bin/sh", "-c", "head -c 200000 /dev/zero"}
		return v, e
	}
	r, e := runApprovedFixture(t, o)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if len(raw) > 100000 {
		t.Fatalf("output duplicated in ledger: %d", len(raw))
	}
	values, _, e := LearningVerificationEvidence(r, 4_000_000)
	if e != nil || len(values) != 1 {
		t.Fatalf("%v", e)
	}
	var result verification.Result
	if e = json.Unmarshal(values[0], &result); e != nil {
		t.Fatal(e)
	}
	if len(result.Checks[0].Result.Stdout) != 200000 {
		t.Fatal("captured output lost")
	}
	if _, _, e = LearningVerificationEvidence(r, 100); e == nil {
		t.Fatal("oversized learning evidence silently truncated")
	}
}

func TestRecoverInterruptedVerificationRetainsCompletedObservations(t *testing.T) {
	o, _ := fixture(t)
	r, e := runApprovedFixture(t, o)
	if e != nil {
		t.Fatal(e)
	}
	p := r.Readiness.Plans[0]
	r.Readiness.Phase = "check-pending"
	if e = beginVerification(&r, p.Plan, r.Capture.Tree); e != nil {
		t.Fatal(e)
	}
	if e = recordObservation(&r, r.Verification.Checks[0]); e != nil {
		t.Fatal(e)
	}
	ledger := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
	before, e := os.ReadFile(ledger)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Recover(context.Background(), o.LabDir, r.ID, p.Digest, true); e == nil {
		t.Fatal("unfinished verification accepted without final process evidence")
	}
	next := p.Plan
	next.Continuation = "checks-only"
	if _, e = Amend(context.Background(), o.LabDir, r.ID, p.Digest, next, ""); e == nil {
		t.Fatal("amendment bypassed uncertain verification")
	}
	after, e := os.ReadFile(ledger)
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal changed retained history")
	}
	var retained Record
	if e = json.Unmarshal(after, &retained); e != nil {
		t.Fatal(e)
	}
	if len(retained.VerificationAttempts) != 2 || retained.VerificationAttempts[1].Status != "started" || len(retained.VerificationAttempts[1].Observations) != 1 {
		t.Fatal("unfinished history lost")
	}
	retained.LabDir = o.LabDir
	if e = ValidateVerificationHistory(retained); e != nil {
		t.Fatal(e)
	}
}

func copyPlan(t *testing.T, p readiness.Plan) readiness.Plan {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out readiness.Plan
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Count actual process executions outside the captured source checkout.
func pendingChecksFixture(t *testing.T) (Options, Record, string) {
	t.Helper()
	o, _ := fixture(t)
	counts := t.TempDir()
	harness := filepath.Join(filepath.Dir(o.LabDir), "harness")
	raw, err := os.ReadFile(harness)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte(fmt.Sprintf("\necho coding >> %q\n", filepath.Join(counts, "coding")))...)
	if err = os.WriteFile(harness, raw, 0700); err != nil {
		t.Fatal(err)
	}
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, err := analyze(ctx, b, i, s)
		a.Checks[0].Argv = []string{"/bin/sh", "-c", fmt.Sprintf("echo check >> %q; exit 1", filepath.Join(counts, "checks"))}
		return a, err
	}
	o.Artifacts = []readiness.ArtifactRoot{{Path: "output", CommandID: "check"}}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verification == nil || r.Verification.RequiredChecksPassed {
		t.Fatal("expected failed verification")
	}
	next := copyPlan(t, r.Readiness.Plans[0].Plan)
	next.Continuation = "checks-only"
	r, err = Amend(context.Background(), o.LabDir, r.ID, r.Readiness.Plans[0].Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	return o, r, counts
}

func assertExecutionCounts(t *testing.T, dir string, coding, checks, setup int) {
	t.Helper()
	for name, want := range map[string]int{"coding": coding, "checks": checks, "setup": setup} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if got := len(bytes.Fields(raw)); got != want {
			t.Fatalf("%s calls = %d, want %d", name, got, want)
		}
	}
}

func TestPendingChecksOnlyCorrectionsRetainHistoryWithoutExecution(t *testing.T) {
	o, r, counts := pendingChecksFixture(t)

	historicalPlans, _ := json.Marshal(r.Readiness.Plans)
	historicalEvents, _ := json.Marshal(r.Readiness.Events)
	historicalAttempts, _ := json.Marshal(r.VerificationAttempts)
	evidenceBefore, _, err := LearningVerificationEvidence(r, 4_000_000)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"exit 1\n", "test \"$(cat code.txt)\" = changed\n"} {
		parent := r.Readiness.Plans[len(r.Readiness.Plans)-1]
		next := copyPlan(t, parent.Plan)
		input := readiness.FileEvidence{Evidence: readiness.Evidence{ID: "frozen-check", Path: "acceptance.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}, Content: content}
		next.CheckInputs = []readiness.FileEvidence{input}
		next.Evidence = append([]readiness.Evidence{}, next.Evidence...)
		if len(parent.Plan.CheckInputs) == 0 {
			next.Evidence = append(next.Evidence, input.Evidence)
		} else {
			next.Evidence[len(next.Evidence)-1] = input.Evidence
		}
		next.Analysis.Checks[0].Argv = []string{"/bin/sh", "-c", fmt.Sprintf("echo check >> %q; /bin/sh \"$1\"", filepath.Join(counts, "checks")), "frozen-check", "{input:frozen-check}"}
		next.Analysis.Checks[0].Definitions = []string{input.ID}
		next.Analysis.Checks[0].Evidence = []string{input.ID}
		setup := next.Analysis.Checks[0]
		setup.ID, setup.Category = "prepare", "setup"
		setup.Argv = []string{"/bin/sh", "-c", fmt.Sprintf("echo setup >> %q", filepath.Join(counts, "setup"))}
		next.Setup = []readiness.Check{setup}
		r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
		if err != nil {
			t.Fatal(err)
		}
		latest := r.Readiness.Plans[len(r.Readiness.Plans)-1]
		if latest.Digest == parent.Digest || latest.Plan.ParentDigest != parent.Digest || latest.Plan.Revision != parent.Plan.Revision+1 || r.Readiness.Phase != "scope-waiting" {
			t.Fatal("invalid pending successor")
		}
		if latest.Plan.PausedTree != parent.Plan.PausedTree || latest.Plan.PausedArtifactsSHA256 != parent.Plan.PausedArtifactsSHA256 || !reflect.DeepEqual(latest.Plan.Inputs, parent.Plan.Inputs) {
			t.Fatal("frozen identities changed")
		}
		assertExecutionCounts(t, counts, 1, 1, 0)
	}
	plans, _ := json.Marshal(r.Readiness.Plans[:2])
	events, _ := json.Marshal(r.Readiness.Events[:len(r.Readiness.Events)-2])
	attempts, _ := json.Marshal(r.VerificationAttempts)
	evidenceAfter, _, err := LearningVerificationEvidence(r, 4_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plans, historicalPlans) || !bytes.Equal(events, historicalEvents) || !bytes.Equal(attempts, historicalAttempts) || !reflect.DeepEqual(evidenceBefore, evidenceAfter) {
		t.Fatal("historical evidence or decisions changed")
	}
	for _, obsolete := range r.Readiness.Plans[:len(r.Readiness.Plans)-1] {
		o.Approve = obsolete.Digest
		if _, err = Run(context.Background(), o); err == nil {
			t.Fatal("obsolete approval accepted")
		}
		assertExecutionCounts(t, counts, 1, 1, 0)
	}
	o.Approve = r.Readiness.Plans[len(r.Readiness.Plans)-1].Digest
	r, err = Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	assertExecutionCounts(t, counts, 1, 2, 1)
	if r.Verification == nil || !r.Verification.RequiredChecksPassed || len(r.VerificationAttempts) != 2 {
		t.Fatal("latest exact approval did not verify retained source")
	}
	if _, err = r.Readiness.ValidateForDelivery(); err != nil {
		t.Fatal(err)
	}
}

func TestPendingChecksOnlyRejectsUnsafeAmendments(t *testing.T) {
	for _, kind := range []string{"tree", "scope", "artifacts", "inventory", "paused-tree", "paused-inventory", "uncertain", "unapproved-coding", "unknown-coding-plan", "failed-coding", "violation", "issue", "formula", "binding", "repository", "base", "evidence", "coding-parent", "bad-input", "bad-reference"} {
		t.Run(kind, func(t *testing.T) {
			o, r, counts := pendingChecksFixture(t)
			parent := r.Readiness.Plans[1]
			next := copyPlan(t, parent.Plan)
			switch kind {
			case "tree":
				if err := os.WriteFile(filepath.Join(r.Workspace.Path, "code.txt"), []byte("drift"), 0600); err != nil {
					t.Fatal(err)
				}
			case "scope":
				next.Analysis.Scope = next.Analysis.Scope[:1]
			case "artifacts":
				next.Artifacts[0].MaxFiles = 1
			case "inventory":
				if err := os.Mkdir(filepath.Join(r.Workspace.Path, "output"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(r.Workspace.Path, "output", "cache"), []byte("drift"), 0600); err != nil {
					t.Fatal(err)
				}
			case "paused-tree":
				next.PausedTree = next.Inputs.BaseCommit
			case "paused-inventory":
				next.PausedArtifactsSHA256 = fmt.Sprintf("%064d", 0)
			case "uncertain":
				r.Atoms[2].Status = "active"
			case "unapproved-coding":
				for i := range r.Readiness.Events {
					if r.Readiness.Events[i].Kind == "approved" {
						r.Readiness.Events[i].Kind = "dismissed"
					}
				}
			case "unknown-coding-plan":
				for i := range r.Readiness.Events {
					switch r.Readiness.Events[i].Kind {
					case "approved", "attempt-started", "attempt-check-pending":
						r.Readiness.Events[i].Digest = fmt.Sprintf("%064d", 0)
					}
				}
			case "failed-coding":
				code := 1
				r.Atoms[2].ExitCode = &code
			case "violation":
				r.Readiness.Events = append([]readiness.Event{{Kind: "attempt-scope-violation", Digest: r.Readiness.Plans[0].Digest, Tree: r.Capture.Tree}}, r.Readiness.Events...)
			case "issue":
				next.Inputs.Issue.Body += " drift"
			case "formula":
				next.Inputs.FormulaSHA256 = fmt.Sprintf("%064d", 0)
			case "binding":
				next.Inputs.BindingSHA256 = fmt.Sprintf("%064d", 0)
			case "repository":
				next.Inputs.Repository = "other/repo"
			case "base":
				next.Inputs.BaseCommit = fmt.Sprintf("%040d", 0)
			case "evidence":
				next.Inputs.EvidenceSHA256 = fmt.Sprintf("%064d", 0)
			case "coding-parent":
				r.Readiness.Plans[1].Plan.Continuation = ""
				r.Readiness.Plans[1].Digest, _ = r.Readiness.Plans[1].Plan.Digest()
				parent = r.Readiness.Plans[1]
			case "bad-input", "bad-reference":
				content := "exit 0\n"
				input := readiness.FileEvidence{Evidence: readiness.Evidence{ID: "frozen", Path: "acceptance.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}, Content: content}
				next.Evidence = append(next.Evidence, input.Evidence)
				if kind == "bad-input" {
					input.Content = "exit 1\n"
				} else {
					next.Evidence[len(next.Evidence)-1].SHA256 = fmt.Sprintf("%064d", 0)
				}
				next.CheckInputs = []readiness.FileEvidence{input}
			}
			if err := save(r); err != nil {
				t.Fatal(err)
			}
			ledger := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
			before, err := os.ReadFile(ledger)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, ""); err == nil {
				t.Fatal("unsafe pending amendment admitted")
			}
			after, err := os.ReadFile(ledger)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("rejected amendment mutated ledger")
			}
			assertExecutionCounts(t, counts, 1, 1, 0)
		})
	}
}

func TestPendingAmendmentsKeepLegacyVerificationReadable(t *testing.T) {
	o, _ := fixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	// Older ledgers store only the inline projection and omit completion exit codes.
	if err = os.RemoveAll(filepath.Join(o.LabDir, "verification-history", r.ID)); err != nil {
		t.Fatal(err)
	}
	r.VerificationAttempts = nil
	r.HarnessAttempts = nil // Legacy ledgers predate durable harness history.
	r.Atoms[2].ExitCode = nil
	if err = save(r); err != nil {
		t.Fatal(err)
	}
	parent := r.Readiness.Plans[0]
	next := copyPlan(t, parent.Plan)
	next.Continuation = "checks-only"
	ledger := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, ""); err == nil {
		t.Fatal("legacy verification without durable process evidence authorized continuation")
	}
	after, err := os.ReadFile(ledger)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused legacy amendment changed ledger")
	}
	var retained Record
	if err = json.Unmarshal(after, &retained); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(retained.Verification, r.Verification) || len(retained.VerificationAttempts) != 0 {
		t.Fatal("legacy verification evidence is no longer readable or was promoted")
	}
}

func TestPendingChecksOnlyRetainsReviewedHistoricalViolation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			o, r, counts := pendingChecksFixture(t)
			tree := r.Capture.Tree
			violation := readiness.Event{Kind: "attempt-scope-violation", Digest: r.Readiness.Plans[0].Digest, Tree: tree}
			if legacy {
				violation.Tree = ""
			}
			// A separately reviewed violation is bound to the retained successor's tree.
			reviewed := readiness.Event{Kind: "existing-diff-reviewed", Digest: violation.Digest, Tree: tree}
			r.Readiness.Events = append([]readiness.Event{violation, reviewed}, r.Readiness.Events...)
			if err := save(r); err != nil {
				t.Fatal(err)
			}
			parent := r.Readiness.Plans[1]
			next := copyPlan(t, parent.Plan)
			next.Analysis.Summary = "Correct checks with reviewed historical evidence"
			amended, err := Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(amended.Readiness.Events[:2], r.Readiness.Events[:2]) {
				t.Fatal("reviewed historical violation erased")
			}
			assertExecutionCounts(t, counts, 1, 1, 0)
		})
	}
}

func TestClarificationHistoryValidatesIdentityAndOrdersTies(t *testing.T) {
	o, _ := fixture(t)
	for _, id := range []string{"mol-b", "mol-a", "mol-other", "mol-wrong-name", "mol-wrong-lab"} {
		r := Record{SchemaVersion: "v0", Kind: "molecule", ID: id, Mode: "ticket", Status: "blocked", FormulaID: "sample", LabDir: o.LabDir, StartedAt: "2026-01-01T00:00:00Z", Issue: Issue{Repo: "acme/widgets", Number: 12}, Workspace: Workspace{Repo: "acme/widgets"}}
		if id == "mol-other" {
			r.Issue.Repo = "other/repo"
		}
		if id == "mol-wrong-lab" {
			r.LabDir = "/other"
		}
		raw, _ := json.Marshal(r)
		name := id
		if id == "mol-wrong-name" {
			name = "wrong"
		}
		os.MkdirAll(filepath.Join(o.LabDir, "ledgers"), 0700)
		if err := os.WriteFile(filepath.Join(o.LabDir, "ledgers", name+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ids, truncated, err := clarificationHistory(o.LabDir, "acme/widgets", 12)
	if err != nil || truncated || !reflect.DeepEqual(ids, []string{"mol-a", "mol-b"}) {
		t.Fatalf("ids=%v truncated=%t err=%v", ids, truncated, err)
	}
	for j := 0; j < 257; j++ {
		os.WriteFile(filepath.Join(o.LabDir, "ledgers", fmt.Sprintf("a-%03d.json", j)), []byte("{}"), 0600)
	}
	_, truncated, err = clarificationHistory(o.LabDir, "acme/widgets", 12)
	if err != nil || !truncated {
		t.Fatal("history scan unbounded", err)
	}
}

func TestInterruptedAnalysisRetainsStartedSnapshotWithoutCheckout(t *testing.T) {
	o, _ := fixture(t)
	o.Analyze = func(context.Context, formula.Binding, Issue, readiness.RepositorySnapshot) (readiness.Analysis, error) {
		panic("synthetic interruption")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("missing interruption")
			}
		}()
		Run(context.Background(), o)
	}()
	entries, err := os.ReadDir(filepath.Join(o.LabDir, "ledgers"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(o.LabDir, "ledgers", entry.Name()))
		var r Record
		if err = json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		if r.Workspace.Path != "" || r.RepositoryEvidence == nil || r.AnalysisAttempts[0].Status != "started" || r.Issue.Body == "" {
			t.Fatal("interrupted evidence lost")
		}
		return
	}
	t.Fatal("interrupted ledger absent")
}

func TestInterruptedAllocationCannotResolveRunnableApproval(t *testing.T) {
	o, _ := fixture(t)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	r.CheckoutStatus = "allocating"
	r.Workspace.Path = ""
	if err = save(r); err != nil {
		t.Fatal(err)
	}
	if _, err = findPending(o.LabDir, r.Readiness.Plans[0].Digest); err == nil {
		t.Fatal("interrupted allocation resolved as runnable")
	}
}
