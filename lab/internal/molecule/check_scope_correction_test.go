package molecule

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// inScopeScripted adds the steps "def", "argv" and "test-argv" to scripted: the valid analysis with its check defined by, or naming, an in-scope file.
func inScopeScripted(t *testing.T, o *Options, steps ...string) (*int, *[]analysisCorrection) {
	t.Helper()
	passthrough := make([]string, len(steps))
	for i, step := range steps {
		passthrough[i] = step
		switch step {
		case "def", "argv", "test-argv":
			passthrough[i] = "valid"
		}
	}
	calls, seen := scripted(t, o, passthrough...)
	base := o.Analyze
	n := 0
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		if n >= len(steps) {
			t.Fatalf("unexpected analysis invocation %d", n+1)
		}
		step := steps[n]
		n++
		a, err := base(ctx, b, i, s)
		if err != nil {
			return a, err
		}
		manifest := ""
		for _, e := range s.Evidence {
			if e.Path == "package.json" {
				manifest = e.ID
			}
		}
		a.Checks = append([]readiness.Check(nil), a.Checks...)
		switch step {
		case "def":
			a.Scope = append(a.Scope, readiness.ScopedPath{Path: "package.json", Reason: "Edit the manifest the check is defined by", Evidence: []string{manifest}})
		case "argv":
			a.Checks[0].Argv = []string{"test", "-f", "code.txt"}
		case "test-argv":
			a.Scope = append(a.Scope, readiness.ScopedPath{Path: "check_test.go", Reason: "New test file", Evidence: []string{manifest}})
			a.Checks[0].Argv = []string{"/bin/sh", "check_test.go"}
		}
		return a, nil
	}
	return calls, seen
}

func TestInScopeDefinitionRetriedOnceThenYieldsPlanWithoutTheCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ first, in string }{
		{"def", `"path":"package.json"`},
		{"argv", `"test","-f","code.txt"`},
	} {
		t.Run(tc.first, func(t *testing.T) {
			o, _ := fixture(t)
			calls, corrections := inScopeScripted(t, &o, tc.first, "valid")
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if *calls != 2 || r.Status != "waiting" || r.Readiness == nil || r.Readiness.Phase != "scope-waiting" {
				t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
			}
			if (*corrections)[0] != (analysisCorrection{}) {
				t.Fatalf("the first analysis must not carry a correction: %+v", (*corrections)[0])
			}
			second := (*corrections)[1]
			if !strings.HasPrefix(second.ValidatorMessage, "drop or replace checks defined by in-scope files") || !strings.Contains(second.ValidatorMessage, "(check)") {
				t.Fatalf("corrective call did not receive the exact validator message: %q", second.ValidatorMessage)
			}
			if !strings.Contains(second.PreviousResponse, tc.in) || !strings.Contains(second.PreviousResponse, `"summary":"Change code"`) {
				t.Fatalf("corrective call did not receive the rejected response: %q", second.PreviousResponse)
			}
			if len(r.AnalysisAttempts) != 2 {
				t.Fatalf("both attempts must be recorded: %+v", r.AnalysisAttempts)
			}
			one, two := r.AnalysisAttempts[0], r.AnalysisAttempts[1]
			if one.Ordinal != 1 || one.Status != "failed" || one.Error != second.ValidatorMessage || one.Response == nil {
				t.Fatalf("the rejected attempt must keep its error and response: %+v", one)
			}
			if two.Ordinal != 2 || two.Status != "completed" || two.Response == nil {
				t.Fatalf("the corrected attempt: %+v", two)
			}
			if !strings.Contains(strings.Join(r.Notes, "\n"), "corrective analysis retry") {
				t.Fatalf("retry not recorded in notes: %v", r.Notes)
			}
			plan := r.Readiness.Plans[0].Plan
			if err = readiness.ValidateInScopeDefinitions(plan.Analysis, plan.Evidence); err != nil {
				t.Fatalf("the plan still has a check defined by an in-scope file: %v", err)
			}
			for _, overlap := range plan.DefinitionOverlaps() {
				if overlap.Kind == "definition" {
					t.Fatalf("definition overlap reached the plan: %+v", overlap)
				}
			}
			if err = plan.ValidateForCoding(); err != nil {
				t.Fatalf("the corrected plan must pass the run-time guard: %v", err)
			}
			analysisActions := map[string]bool{}
			for _, a := range intakeActions(t, r) {
				if strings.HasPrefix(a.AttemptID, "analysis-") {
					if analysisActions[a.AttemptID] || a.Observed == nil {
						t.Fatalf("analysis action ids must be unique and each observed: %+v", intakeActions(t, r))
					}
					analysisActions[a.AttemptID] = true
				}
			}
			if len(analysisActions) != 2 {
				t.Fatalf("expected 2 analysis actions, got %+v", intakeActions(t, r))
			}
		})
	}
}

func TestInScopeDefinitionTwiceBlocksWithoutCoding(t *testing.T) {
	t.Parallel()
	for _, steps := range [][]string{{"def", "def"}, {"argv", "argv"}, {"def", "argv"}} {
		t.Run(strings.Join(steps, "+"), func(t *testing.T) {
			o, _ := fixture(t)
			calls, _ := inScopeScripted(t, &o, steps...)
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if *calls != 2 || r.Status != "blocked" || r.Readiness != nil || r.Analysis != nil {
				t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
			}
			if len(r.AnalysisAttempts) != 2 || len(r.HarnessAttempts) != 0 {
				t.Fatalf("attempts=%+v harness=%+v", r.AnalysisAttempts, r.HarnessAttempts)
			}
			for i, a := range r.AnalysisAttempts {
				if a.Ordinal != i+1 || a.Status != "failed" || !strings.HasPrefix(a.Error, readiness.InScopeDefinitionMessage) || a.Response == nil {
					t.Fatalf("attempt %d: %+v", i, a)
				}
			}
			reason := strings.Join(r.Notes, "\n")
			for _, want := range []string{readiness.InScopeDefinitionMessage, "the corrective analysis retry was also rejected; no coding harness was invoked"} {
				if !strings.Contains(reason, want) {
					t.Fatalf("blocked reason lacks %q: %v", want, r.Notes)
				}
			}
		})
	}
}

func TestInScopeDefinitionSharesTheSingleRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		steps    []string
		blocked  bool
		attempts []string
	}{
		{"definition then violation", []string{"def", "policy", "valid"}, false, []string{"failed", "failed", "completed"}},
		{"violation then definition", []string{"policy", "def", "valid"}, false, []string{"failed", "failed", "completed"}},
		{"definition then cap", []string{"def", "cap", "valid"}, false, []string{"failed", "failed", "completed"}},
		{"cap then definition", []string{"cap", "def", "valid"}, false, []string{"failed", "failed", "completed"}},
		{"evidence then definition twice", []string{"evidence", "def", "def"}, true, []string{"completed", "failed", "failed"}},
		{"definition then evidence then definition", []string{"def", "evidence", "def"}, true, []string{"failed", "completed", "failed"}},
		{"definition then evidence three times is exhausted", []string{"def", "evidence", "evidence", "evidence"}, true, []string{"failed", "completed", "completed", "completed"}},
		{"definition then evidence twice then valid", []string{"def", "evidence", "evidence", "valid"}, false, []string{"failed", "completed", "completed", "completed"}},
		{"evidence then definition then valid", []string{"evidence", "def", "valid"}, false, []string{"completed", "failed", "completed"}},
		{"definition then evidence then valid", []string{"def", "evidence", "valid"}, false, []string{"failed", "completed", "completed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := fixture(t)
			calls, _ := inScopeScripted(t, &o, tc.steps...)
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if *calls != len(tc.steps) || *calls > 4 {
				t.Fatalf("calls=%d want %d", *calls, len(tc.steps))
			}
			if tc.blocked != (r.Status == "blocked") || tc.blocked != (r.Readiness == nil) {
				t.Fatalf("blocked=%t status=%s notes=%v", tc.blocked, r.Status, r.Notes)
			}
			if len(r.AnalysisAttempts) != len(tc.attempts) {
				t.Fatalf("attempts: %+v", r.AnalysisAttempts)
			}
			for i, want := range tc.attempts {
				a := r.AnalysisAttempts[i]
				if a.Status != want || a.Ordinal != i+1 {
					t.Fatalf("attempt %d: %+v want %s", i, a, want)
				}
				if want == "failed" && ((a.Error != policyMessage && a.Error != capMessage && !strings.HasPrefix(a.Error, readiness.InScopeDefinitionMessage)) || a.Response == nil) {
					t.Fatalf("attempt %d lost its validator error or response: %+v", i, a)
				}
			}
			if tc.blocked && len(r.HarnessAttempts) != 0 {
				t.Fatal("coding harness invoked after a blocked intake")
			}
		})
	}
}

func TestInScopeDefinitionAbsentTakesNoRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, step, note string
	}{
		{"no overlap", "valid", ""},
		{"test path named in argv", "test-argv", "[test-source]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := fixture(t)
			calls, corrections := inScopeScripted(t, &o, tc.step)
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if *calls != 1 || r.Status != "waiting" || r.Readiness == nil || len(r.AnalysisAttempts) != 1 || r.AnalysisAttempts[0].Status != "completed" {
				t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
			}
			if (*corrections)[0] != (analysisCorrection{}) || strings.Contains(strings.Join(r.Notes, "\n"), "corrective analysis retry") {
				t.Fatalf("an unaffected analysis must not be retried: %v", r.Notes)
			}
			if tc.note != "" && !strings.Contains(strings.Join(r.Notes, "\n"), tc.note) {
				t.Fatalf("the guard's non-blocking assessment is missing %q: %v", tc.note, r.Notes)
			}
		})
	}
}

func TestInScopeDefinitionRuleSkipsHumanSuppliedChecks(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	content := "independently reviewed acceptance contract"
	input := readiness.FileEvidence{Evidence: readiness.Evidence{ID: "human-acceptance", Path: "acceptance.txt", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}, Content: content}
	o.CheckInputs = []readiness.FileEvidence{input}
	o.Checks = []readiness.Check{{ID: "human-check", Category: "independent-acceptance", Argv: []string{"test", "-f", "code.txt"}, Dir: ".", TimeoutMS: 30000, Required: true, Definitions: []string{input.ID}, Evidence: []string{input.ID}, Reason: "Human-defined behavior", IndependentProvenance: "reviewed fixture"}}
	calls, _ := inScopeScripted(t, &o, "valid")
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || r.Status != "waiting" || r.Readiness == nil {
		t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
	found := false
	for _, check := range r.Readiness.Plans[0].Plan.Analysis.Checks {
		found = found || check.ID == "human-check"
	}
	if !found {
		t.Fatal("the human-supplied check was dropped")
	}
}
