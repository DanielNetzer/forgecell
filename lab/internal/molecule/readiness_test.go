package molecule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func TestRunWaitsForExactReadinessApproval(t *testing.T) {
	o, _ := fixture(t)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness == nil || r.Readiness.Phase != "scope-waiting" || r.FinishedAt != "" {
		t.Fatalf("%+v", r)
	}
	data, _ := os.ReadFile(filepath.Join(r.Workspace.Path, "code.txt"))
	if string(data) != "base" {
		t.Fatal("coding ran before scope approval")
	}
	o.Approve = r.Readiness.Plans[0].Digest
	approved, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if approved.ID != r.ID || approved.Verification == nil || !approved.Verification.RequiredChecksPassed || approved.Status != "waiting" {
		t.Fatalf("%+v", approved)
	}
	if _, err := Run(context.Background(), o); err == nil {
		t.Fatal("approval reused")
	}
}
func TestAmbiguousAnalysisNeverCodes(t *testing.T) {
	o, _ := fixture(t)
	o.Analyze = func(context.Context, formula.Binding, Issue, readiness.RepositorySnapshot) (readiness.Analysis, error) {
		return readiness.Analysis{Summary: "Need behavior clarified", Questions: []readiness.Question{{Question: "Which error should callers receive?", Reason: "Ticket does not specify public behavior", Evidence: []string{"issue"}}}}, nil
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "blocked" || r.Readiness != nil || r.Analysis == nil || len(r.Analysis.Questions) != 1 {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("ambiguous issue reached coding")
	}
}
func TestStaleIssueAndWorkspaceRefuseCoding(t *testing.T) {
	for _, kind := range []string{"issue", "workspace", "formula"} {
		t.Run(kind, func(t *testing.T) {
			o, _ := fixture(t)
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			o.Approve = r.Readiness.Plans[0].Digest
			switch kind {
			case "issue":
				reader := o.ReadIssue
				o.ReadIssue = func(ctx context.Context, repo string, n int64, dir string) (Issue, error) {
					i, e := reader(ctx, repo, n, dir)
					i.Body += " changed"
					return i, e
				}
			case "workspace":
				os.WriteFile(filepath.Join(r.Workspace.Path, "code.txt"), []byte("unapproved"), 0600)
			case "formula":
				f := filepath.Join(o.LabDir, "formulas", "sample.yaml")
				raw, _ := os.ReadFile(f)
				os.WriteFile(f, append(raw, []byte("\n# changed\n")...), 0600)
			}
			if _, err := Run(context.Background(), o); err == nil {
				t.Fatal("stale approval executed")
			}
			if _, err := os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
				t.Fatal("stale run reached coding")
			}
		})
	}
}

func TestDefinitionOverlapIsReviewableButCannotStartCoding(t *testing.T) {
	o, _ := fixture(t)
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, err := analyze(ctx, b, i, s)
		a.Scope = append(a.Scope, readiness.ScopedPath{Path: "package.json", Reason: "Update checker", Evidence: a.Checks[0].Definitions})
		return a, err
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness == nil || r.Status != "waiting" {
		t.Fatal("overlap should remain reviewable")
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "package.json [definition] checks check") {
		t.Fatal("overlap not surfaced before approval")
	}
	o.Approve = r.Readiness.Plans[0].Digest
	if _, err = Run(context.Background(), o); err == nil {
		t.Fatal("definition overlap reached coding without reviewed repair")
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("coding ran under overlapping definitions")
	}
}

func TestRepairCannotDropEarlierProtectedGate(t *testing.T) {
	o, _ := fixture(t)
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, e := analyze(ctx, b, i, s)
		a.Checks[0].Category = "regression"
		a.Scope = append(a.Scope, readiness.ScopedPath{Path: "package.json", Reason: "Checker", Evidence: a.Checks[0].Definitions})
		return a, e
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	parent := r.Readiness.Plans[0]
	next := copyPlan(t, parent.Plan)
	input := readiness.FileEvidence{Evidence: readiness.Evidence{ID: "human", Path: "acceptance.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("true")))}, Content: "true"}
	next.CheckInputs = []readiness.FileEvidence{input}
	next.Evidence = append(next.Evidence, input.Evidence)
	// A new check with a new identity must not substitute for the retained gate.
	next.Analysis.Checks[0].ID = "replacement"
	next.Analysis.Checks[0].ReviewedAcceptance = "human-check"
	next.Analysis.Checks = append(next.Analysis.Checks, readiness.Check{ID: "human-check", Category: "independent-acceptance", IndependentProvenance: "human review fixture", Argv: []string{"/bin/sh", "{input:human}"}, Dir: ".", TimeoutMS: 1000, Required: true, Definitions: []string{"human"}, Evidence: []string{"human"}, Reason: "Frozen external acceptance"})
	r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = r.Readiness.Plans[1].Digest
	if _, err = Run(context.Background(), o); err == nil {
		t.Fatal("repair removed prior protected gate")
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("unsafe repair reached coding")
	}
}

func TestRepairHistoryRetainsGatesAfterRepairMarkerRemoval(t *testing.T) {
	check := readiness.Check{ID: "baseline", Category: "regression", Required: true, Definitions: []string{"frozen"}}
	before := readiness.Plan{Analysis: readiness.Analysis{Checks: []readiness.Check{check}}}
	repaired := copyPlan(t, before)
	repaired.Analysis.Checks[0].ReviewedAcceptance = "human-check"
	restored := copyPlan(t, before)
	dropped := copyPlan(t, before)
	dropped.Analysis.Checks = nil
	plans := []readiness.Proposal{{Plan: before}, {Plan: repaired}, {Plan: restored}, {Plan: dropped}}
	if err := validateRepairHistory(plans); err == nil {
		t.Fatal("later amendment dropped retained protected gate")
	}
}

func TestInterruptedRecoveryPreservesWorkAndDoesNotReplay(t *testing.T) {
	o, _ := fixture(t)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	digest := r.Readiness.Plans[0].Digest
	s, err := r.Readiness.Approve(digest, r.Readiness.Plans[0].Plan.Inputs, stamp())
	if err != nil {
		t.Fatal(err)
	}
	s, err = s.StartAttempt(stamp())
	if err != nil {
		t.Fatal(err)
	}
	r.Readiness = &s
	if err = save(r); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.Workspace.Path, "code.txt"), []byte("partial paid work"), 0600)
	if _, err = Recover(context.Background(), r.LabDir, r.ID, digest, false); err == nil {
		t.Fatal("recovered without stopped-process confirmation")
	}
	_, err = Recover(context.Background(), r.LabDir, r.ID, digest, true)
	if err == nil {
		t.Fatal("lost harness result was treated as stopped-process evidence")
	}
	data, _ := os.ReadFile(filepath.Join(r.Workspace.Path, "code.txt"))
	if string(data) != "partial paid work" {
		t.Fatal("work lost")
	}
	o.Approve = digest
	if _, err = Run(context.Background(), o); err == nil {
		t.Fatal("interrupted coding replayed")
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("harness replayed")
	}
}

func TestAmendmentRetainsMoleculeAndRequiresNewApproval(t *testing.T) {
	o, _ := fixture(t)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	old := r.Readiness.Plans[0]
	next := old.Plan
	next.Analysis.Summary = "Clarified implementation scope"
	amended, err := Amend(context.Background(), r.LabDir, r.ID, old.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	if amended.ID != r.ID || len(amended.Readiness.Plans) != 2 || amended.Readiness.Plans[1].Plan.ParentDigest != old.Digest {
		t.Fatalf("lost revision chain: %+v", amended)
	}
	o.Approve = old.Digest
	if _, err = Run(context.Background(), o); err == nil {
		t.Fatal("stale approval accepted")
	}
	o.Approve = amended.Readiness.Plans[1].Digest
	done, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if done.ID != r.ID || done.Verification == nil || !done.Verification.RequiredChecksPassed {
		t.Fatalf("amendment failed: %+v", done)
	}
}

func TestOwnerlessLockIsNotRecoveredDuringApprovalStartup(t *testing.T) {
	o, _ := fixture(t)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	digest := r.Readiness.Plans[0].Digest
	if err = os.Mkdir(filepath.Join(r.LabDir, "ledgers", r.ID+".lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = Recover(context.Background(), r.LabDir, r.ID, digest, true); err == nil {
		t.Fatal("ownerless startup lock recovered")
	}
}

func TestAmendmentBindsArtifactInventory(t *testing.T) {
	o, _ := fixture(t)
	o.Artifacts = []readiness.ArtifactRoot{{Path: "output", CommandID: "check"}}
	// Use the actual approved check id from the controlled analysis.
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, e := analyze(ctx, b, i, s)
		o.Artifacts[0].CommandID = a.Checks[0].ID
		return a, e
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness == nil {
		t.Fatalf("%+v", r)
	}
	os.Mkdir(filepath.Join(r.Workspace.Path, "output"), 0700)
	os.WriteFile(filepath.Join(r.Workspace.Path, "output", "cache"), []byte("before"), 0600)
	old := r.Readiness.Plans[0]
	amended, err := Amend(context.Background(), r.LabDir, r.ID, old.Digest, old.Plan, "")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.Workspace.Path, "output", "cache"), []byte("after"), 0600)
	o.Approve = amended.Readiness.Plans[1].Digest
	if _, err = Run(context.Background(), o); err == nil {
		t.Fatal("artifact drift accepted")
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("coding started with changed artifacts")
	}
}

func TestHumanVerificationInputsJoinPendingPlanWithoutExecution(t *testing.T) {
	o, _ := fixture(t)
	content := "independently reviewed acceptance contract"
	sum := sha256.Sum256([]byte(content))
	input := readiness.FileEvidence{Evidence: readiness.Evidence{ID: "human-acceptance", Path: "acceptance.txt", SHA256: fmt.Sprintf("%x", sum)}, Content: content}
	o.CheckInputs = []readiness.FileEvidence{input}
	o.Checks = []readiness.Check{{ID: "human-check", Category: "independent-acceptance", Argv: []string{"/bin/false"}, Dir: ".", TimeoutMS: 1000, Required: true, Definitions: []string{input.ID}, Evidence: []string{input.ID}, Reason: "Human-defined behavior", IndependentProvenance: "reviewed fixture"}}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness == nil || r.Status != "waiting" {
		t.Fatalf("%+v", r)
	}
	plan := r.Readiness.Plans[0].Plan
	found := false
	for _, check := range plan.Analysis.Checks {
		if check.ID == "human-check" {
			found = true
		}
	}
	if !found || len(plan.CheckInputs) != 1 {
		t.Fatal("human acceptance lost")
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("coding ran before approval")
	}
}

func TestPendingChecksOnlyAmendmentDefersLiveRefreshToExactApproval(t *testing.T) {
	for _, drift := range []string{"none", "issue", "formula", "binding", "origin", "workspace", "artifacts"} {
		t.Run(drift, func(t *testing.T) {
			o, r, counts := pendingChecksFixture(t)
			reads := 0
			reader := o.ReadIssue
			o.ReadIssue = func(ctx context.Context, repo string, n int64, dir string) (Issue, error) {
				reads++
				i, err := reader(ctx, repo, n, dir)
				if drift == "issue" {
					i.Body += " changed"
				}
				return i, err
			}
			// Proposal corrections remain local even if consequential live inputs drift.
			var changed string
			if drift == "formula" {
				changed = filepath.Join(o.LabDir, "formulas", "sample.yaml")
			}
			if drift == "binding" {
				changed = filepath.Join(filepath.Dir(o.LabDir), "harness")
			}
			if changed != "" {
				raw, err := os.ReadFile(changed)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(changed, append(raw, []byte("\n# changed\n")...), 0700); err != nil {
					t.Fatal(err)
				}
			}
			for revision := 3; revision <= 4; revision++ {
				parent := r.Readiness.Plans[len(r.Readiness.Plans)-1]
				next := copyPlan(t, parent.Plan)
				next.Analysis.Summary = fmt.Sprintf("Corrected pending check revision %d", revision)
				next.Analysis.Checks[0].Argv = []string{"/bin/sh", "-c", fmt.Sprintf("echo check >> %q", filepath.Join(counts, "checks"))}
				var err error
				r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
				if err != nil {
					t.Fatal(err)
				}
			}
			if reads != 0 {
				t.Fatal("amendment refreshed live issue")
			}
			assertExecutionCounts(t, counts, 1, 1, 0)
			switch drift {
			case "origin":
				testGit(t, r.Workspace.Path, "remote", "set-url", "origin", "https://github.com/other/repository.git")
			case "workspace":
				if err := os.WriteFile(filepath.Join(r.Workspace.Path, "code.txt"), []byte("changed after amendment"), 0600); err != nil {
					t.Fatal(err)
				}
			case "artifacts":
				if err := os.Mkdir(filepath.Join(r.Workspace.Path, "output"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(r.Workspace.Path, "output", "cache"), []byte("changed after amendment"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			o.Approve = r.Readiness.Plans[len(r.Readiness.Plans)-1].Digest
			got, err := Run(context.Background(), o)
			if drift == "none" {
				if err != nil {
					t.Fatal(err)
				}
				if reads != 1 || got.Verification == nil || !got.Verification.RequiredChecksPassed {
					t.Fatal("exact approval did not refresh and verify")
				}
				assertExecutionCounts(t, counts, 1, 2, 0)
			} else {
				if err == nil {
					t.Fatal("drift passed exact approval")
				}
				assertExecutionCounts(t, counts, 1, 1, 0)
				saved, err := findPending(o.LabDir, o.Approve)
				if err != nil {
					t.Fatal(err)
				}
				if saved.Readiness.Phase != "scope-waiting" {
					t.Fatal("drift consumed approval")
				}
			}
		})
	}
}

func TestSupplementalAnalysisRecordedBeforeInvocation(t *testing.T) {
	o, _ := fixture(t)
	original := o.Analyze
	calls := 0
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		calls++
		entries, e := os.ReadDir(filepath.Join(o.LabDir, "ledgers"))
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			raw, e := os.ReadFile(filepath.Join(o.LabDir, "ledgers", entry.Name()))
			if e != nil {
				t.Fatal(e)
			}
			var r Record
			if json.Unmarshal(raw, &r) != nil {
				continue
			}
			if len(r.AnalysisAttempts) == calls {
				last := r.AnalysisAttempts[calls-1]
				if last.Status != "started" || last.SnapshotSHA256 == "" {
					t.Fatal("start not persisted")
				}
				if calls == 2 && (r.AnalysisAttempts[0].Response == nil || len(s.Collection) == 0) {
					t.Fatal("first response or snapshot not saved")
				}
				found = true
			}
		}
		if !found {
			t.Fatal("attempt ledger absent")
		}
		if calls == 1 {
			return readiness.Analysis{Summary: "Need named committed content", EvidenceRequests: []readiness.EvidenceRequest{{Path: "code.txt", Reason: "Inspect implementation", Evidence: []string{"issue"}}}}, nil
		}
		return original(ctx, b, i, s)
	}
	r, e := Run(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	if calls != 2 || len(r.AnalysisAttempts) != 2 || r.Status != "waiting" {
		t.Fatalf("calls=%d status=%s notes=%v", calls, r.Status, r.Notes)
	}
	o.Approve = r.Readiness.Plans[0].Digest
	if _, e = Run(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	if calls != 2 {
		t.Fatal("approval invoked model")
	}
}

func TestSupplementalAnalysisExhaustedNeverCodes(t *testing.T) {
	o, _ := fixture(t)
	o.Analyze = func(context.Context, formula.Binding, Issue, readiness.RepositorySnapshot) (readiness.Analysis, error) {
		return readiness.Analysis{Summary: "Missing content", EvidenceRequests: []readiness.EvidenceRequest{{Path: "missing/checker.sh", Reason: "Need behavior", Evidence: []string{"issue"}}}}, nil
	}
	r, e := Run(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "blocked" || len(r.AnalysisAttempts) != 2 || r.Readiness != nil {
		t.Fatalf("%+v", r)
	}
	if _, e = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(e) {
		t.Fatal("coding invoked")
	}
}

func TestClarificationRevisionsRetainEvidenceWithoutCheckout(t *testing.T) {
	o, _ := fixture(t)
	clear, reader := o.Analyze, o.ReadIssue
	prepared := 0
	o.PrepareWorkspace = func(context.Context, Workspace) error { prepared++; return nil }
	var records []Record
	var historical [][]byte
	for _, revision := range []string{"A", "B", "C"} {
		o.ReadIssue = func(ctx context.Context, repo string, n int64, dir string) (Issue, error) {
			i, e := reader(ctx, repo, n, dir)
			i.Body += revision
			i.UpdatedAt = revision
			return i, e
		}
		o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
			if entries, e := os.ReadDir(filepath.Join(o.LabDir, "workspaces")); e == nil && len(entries) != 0 {
				t.Fatal("checkout allocated before assessment")
			}
			if revision == "C" {
				return clear(ctx, b, i, s)
			}
			return readiness.Analysis{Summary: revision, Questions: []readiness.Question{{Question: "Clarify " + revision, Reason: "Missing behavior", Evidence: []string{"issue"}}}}, nil
		}
		r, e := Run(context.Background(), o)
		if e != nil {
			t.Fatal(e)
		}
		if r.Issue.UpdatedAt != revision || len(r.AnalysisAttempts) != 1 || r.AnalysisAttempts[0].Response == nil {
			t.Fatal("lost exact assessment")
		}
		if revision != "C" && (r.Workspace.Path != "" || r.Readiness != nil) {
			t.Fatal("ambiguous intake retained checkout or approval")
		}
		raw, e := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
		if e != nil {
			t.Fatal(e)
		}
		var fields struct {
			ClarificationPredecessors []string `json:"clarificationPredecessors"`
		}
		if e = json.Unmarshal(raw, &fields); e != nil {
			t.Fatal(e)
		}
		if len(fields.ClarificationPredecessors) != len(records) {
			t.Fatal("missing clarification history")
		}
		for j, previous := range records {
			if fields.ClarificationPredecessors[j] != previous.ID {
				t.Fatal("history order changed")
			}
			old, _ := os.ReadFile(filepath.Join(o.LabDir, "ledgers", previous.ID+".json"))
			if string(old) != string(historical[j]) {
				t.Fatal("historical ledger rewritten")
			}
		}
		records = append(records, r)
		historical = append(historical, raw)
	}
	r := records[2]
	if prepared != 0 || r.Readiness.Phase != "scope-waiting" || len(r.HarnessAttempts) != 0 {
		t.Fatal("retry implicitly approved")
	}
	o.Approve = r.Readiness.Plans[0].Digest
	if _, e := Run(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	if prepared != 1 {
		t.Fatal("preparation did not wait for approval")
	}
}

func TestHistoricalAtomProvenanceStaysUnknown(t *testing.T) {
	r := Record{LabDir: t.TempDir(), ID: "mol-history", Atoms: []Atom{{ID: "old", Type: "ship", Status: "done", Detail: "legacy detail"}}}
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "provenance") {
		t.Fatal("historical provenance inferred")
	}
	md, err := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Declared source: unknown", "Resolved source: unknown", "Planned action: unknown", "Observed action/result: unknown", "External action/result: unknown"} {
		if !strings.Contains(string(md), label) {
			t.Fatalf("missing %q", label)
		}
	}
}

func TestAtomProvenanceSeparatesIntentAndObservedExecution(t *testing.T) {
	o, _ := fixture(t)
	file := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	recipe := strings.ReplaceAll(string(raw), "id: harness, type: harness", "id: coding-custom, type: harness, source: declared-context, binding: declared-binding, workflows: [declared-workflow]")
	if err = os.WriteFile(file, []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
	approveFixture(t, o.LabDir, recipe, "provenance")
	reader := o.ReadIssue
	o.ReadIssue = func(ctx context.Context, repo string, n int64, dir string) (Issue, error) {
		entries, e := os.ReadDir(filepath.Join(o.LabDir, "ledgers"))
		if e != nil {
			t.Fatal(e)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			data, e := os.ReadFile(filepath.Join(o.LabDir, "ledgers", entry.Name()))
			if e != nil {
				t.Fatal(e)
			}
			var saved Record
			if e = json.Unmarshal(data, &saved); e != nil {
				t.Fatal(e)
			}
			if saved.Readiness == nil {
				a := saved.Atoms[0].Provenance.Actions
				if len(a) != 1 || a[0].Observed != nil || a[0].ResolvedSource != "supplied-issue-reader" {
					t.Fatal("reader intent not persisted")
				}
			}
		}
		return reader(ctx, repo, n, dir)
	}
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		entries, e := os.ReadDir(filepath.Join(o.LabDir, "ledgers"))
		if e != nil {
			t.Fatal(e)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			data, e := os.ReadFile(filepath.Join(o.LabDir, "ledgers", entry.Name()))
			if e != nil {
				t.Fatal(e)
			}
			var saved Record
			if e = json.Unmarshal(data, &saved); e != nil {
				t.Fatal(e)
			}
			actions := saved.Atoms[0].Provenance.Actions
			if len(actions) != 2 || actions[1].PlannedAction != "analyze-ticket" || actions[1].Observed != nil {
				t.Fatal("analysis intent not persisted")
			}
		}
		a, err := analyze(ctx, b, i, s)
		// The checker observes the ledger while executing, before its own result
		// can be recorded. This fails if verification intent persistence is removed.
		a.Checks[0].Argv[2] = fmt.Sprintf("grep -q '\"resolvedSource\": \"lab-independent-verification\"' %q/ledgers/*.json && ", o.LabDir) + a.Checks[0].Argv[2]
		return a, err
	}
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	guard := fmt.Sprintf("grep -q '\"resolvedSource\": \"bound-harness:custom\"' %q/ledgers/*.json || exit 23\n", o.LabDir)
	if err = os.WriteFile(script, []byte(strings.Replace(string(body), "cat > request.json", guard+"cat > request.json", 1)), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	coding := r.Atoms[2]
	p := coding.Provenance
	if coding.ID != "coding-custom" || p.DeclaredSource != "declared-context" || p.DeclaredBinding != "declared-binding" || !reflect.DeepEqual(p.DeclaredWorkflows, []string{"declared-workflow"}) {
		t.Fatalf("declarations lost: %+v", p)
	}
	actions := p.Actions
	if len(actions) != 1 || actions[0].AttemptID != r.HarnessAttempts[0].ID || actions[0].ResolvedSource != "bound-harness:custom" || actions[0].Observed == nil || actions[0].Observed.Result != "process-succeeded" || actions[0].Observed.ExternalAction != "" || actions[0].Observed.ExternalResult != "" || len(actions[0].Observed.Evidence) != 2 {
		t.Fatalf("incorrect provenance: %+v", actions)
	}
	for _, a := range r.Atoms[5:] {
		if a.Provenance.Execution != "skipped" || a.Provenance.SkipReason == "" || len(a.Provenance.Actions) != 0 {
			t.Fatal("missing explicit skip")
		}
	}
	data, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var restored Record
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Atoms, restored.Atoms) {
		t.Fatal("atom round trip changed")
	}
}
func TestAnalysisFailureRetainsIntentWithoutClaimingExternalAction(t *testing.T) {
	o, _ := fixture(t)
	o.Analyze = func(context.Context, formula.Binding, Issue, readiness.RepositorySnapshot) (readiness.Analysis, error) {
		return readiness.Analysis{}, fmt.Errorf("synthetic failure")
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	a := r.Atoms[0].Provenance.Actions[1]
	if a.AttemptID != "analysis-1" || a.Observed == nil || a.Observed.Result != "analysis-failed" || a.Observed.ExternalResult != "" {
		t.Fatalf("failure provenance: %+v", a)
	}
}
func TestInterruptedAnalysisRetainsUnknownObservation(t *testing.T) {
	o, _ := fixture(t)
	o.Analyze = func(context.Context, formula.Binding, Issue, readiness.RepositorySnapshot) (readiness.Analysis, error) {
		panic("interrupted")
	}
	func() {
		defer func() {
			if recover() != "interrupted" {
				t.Fatal("expected interruption")
			}
		}()
		_, _ = Run(context.Background(), o)
	}()
	entries, err := os.ReadDir(filepath.Join(o.LabDir, "ledgers"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var r Record
		if err = json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		actions := r.Atoms[0].Provenance.Actions
		if len(actions) != 2 || actions[1].Observed != nil || actions[1].AttemptID != "analysis-1" || r.AnalysisAttempts[0].Status != "started" {
			t.Fatal("interrupted observation inferred")
		}
		found = true
	}
	if !found {
		t.Fatal("missing ledger")
	}
}
func TestFailedProcessProvenanceSurvivesRecoveryAndAmendment(t *testing.T) {
	o, _ := fixture(t)
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	original, err := json.Marshal(r.Atoms[2].Provenance)
	if err != nil {
		t.Fatal(err)
	}
	action := r.Atoms[2].Provenance.Actions[0]
	if action.AttemptID != r.HarnessAttempts[0].ID || action.Observed == nil || action.Observed.Result != "process-failed" || action.Observed.ExternalAction != "" {
		t.Fatal("failure lost")
	}
	parent := r.Readiness.Plans[0]
	r, err = Recover(context.Background(), o.LabDir, r.ID, parent.Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	recovered, _ := json.Marshal(r.Atoms[2].Provenance)
	if !bytes.Equal(original, recovered) {
		t.Fatal("recovery rewrote provenance")
	}
	next := copyPlan(t, parent.Plan)
	next.Analysis.Summary = "Reviewed failed local attempt"
	r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	amended, _ := json.Marshal(r.Atoms[2].Provenance)
	if !bytes.Equal(original, amended) {
		t.Fatal("amendment rewrote provenance")
	}
}
func TestChecksOnlyRetainsOriginalCodingActionIdentity(t *testing.T) {
	o, _ := fixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(r.Atoms[2].Provenance)
	parent := r.Readiness.Plans[0]
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
	after, _ := json.Marshal(r.Atoms[2].Provenance)
	if !bytes.Equal(original, after) || len(r.HarnessAttempts) != 1 {
		t.Fatal("checks-only reassigned coding")
	}
	actions := r.Atoms[3].Provenance.Actions
	if len(actions) != 2 || actions[0].AttemptID == actions[1].AttemptID || actions[0].PlanDigest == actions[1].PlanDigest || len(actions[0].Observed.Evidence) == 0 || len(actions[1].Observed.Evidence) == 0 {
		t.Fatal("verification attempts not separated")
	}
}
func TestInterruptedHarnessRetainsProcessObservation(t *testing.T) {
	o, _ := fixture(t)
	signal := filepath.Join(t.TempDir(), "started")
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	if err := os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nprintf started > %q\nsleep 10\n", signal)), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = r.Readiness.Plans[0].Digest
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := os.Stat(signal); err == nil {
					cancel()
					return
				}
			}
		}
	}()
	r, err = Run(ctx, o)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	action := r.Atoms[2].Provenance.Actions[0]
	if !r.HarnessAttempts[0].Result.Interrupted || action.Observed == nil || action.Observed.Result != "process-interrupted" || action.AttemptID != r.HarnessAttempts[0].ID || len(action.Observed.Evidence) != 2 || action.Observed.ExternalResult != "" {
		t.Fatal("interruption lost")
	}
}
func TestVerificationFailureRetainsIndependentEvidence(t *testing.T) {
	o, _ := fixture(t)
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, err := analyze(ctx, b, i, s)
		a.Checks[0].Argv = []string{"/bin/sh", "-c", "exit 9"}
		return a, err
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	actions := r.Atoms[3].Provenance.Actions
	if len(actions) != 1 || actions[0].Observed == nil || actions[0].Observed.Result != "checks-failed" || len(actions[0].Observed.Evidence) == 0 || actions[0].PlanDigest != r.VerificationAttempts[0].PlanDigest {
		t.Fatal("verification failure lost")
	}
	for _, a := range r.Atoms[5:] {
		if a.Provenance.Execution != "skipped" {
			t.Fatal("failed checks claimed publication")
		}
	}
	if err := ValidateVerificationHistory(r); err != nil {
		t.Fatal(err)
	}
}
