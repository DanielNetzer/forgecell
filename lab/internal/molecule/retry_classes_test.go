package molecule

import (
	"context"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// classScripted runs the intake analysis through a fixed sequence of steps. Besides
// the steps of scripted and inScopeScripted it knows "issue-def" (a check defined by
// the issue) and "new-file" (a check defined by a new in-scope file).
func classScripted(t *testing.T, o *Options, steps ...string) (*int, *[]analysisCorrection) {
	t.Helper()
	passthrough := make([]string, len(steps))
	for i, step := range steps {
		passthrough[i] = step
		if step == "issue-def" || step == "new-file" || step == "existing-file" {
			passthrough[i] = "valid"
		}
	}
	calls, seen := inScopeScripted(t, o, passthrough...)
	base := o.Analyze
	n := 0
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
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
		case "issue-def":
			a.Checks[0].Definitions = []string{"issue"}
		case "new-file":
			a.Scope = append(a.Scope, readiness.ScopedPath{Path: "pinned/pinned_test.go", Reason: "Tests of the new package", Evidence: []string{manifest}})
			a.Checks = append(a.Checks, readiness.Check{ID: "pinned", Category: "candidate", Argv: []string{"/bin/sh", "-c", "true"}, Dir: ".", TimeoutMS: 5000, Required: true, Definitions: []string{"pinned/pinned_test.go"}, Reason: "Tests of the new package", Evidence: []string{"issue", manifest}})
		case "existing-file":
			a.Checks[0].Definitions = []string{"code.txt"}
		}
		return a, nil
	}
	return calls, seen
}

func runClasses(t *testing.T, steps ...string) (Record, *int, *[]analysisCorrection) {
	t.Helper()
	o, _ := fixture(t)
	calls, corrections := classScripted(t, &o, steps...)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return r, calls, corrections
}

func TestHardening129EachPolicyClassGetsItsOwnCorrectiveRetry(t *testing.T) {
	r, calls, corrections := runClasses(t, "policy", "def", "valid")
	if *calls != 3 || r.Status != "waiting" || r.Readiness == nil || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("a labelling violation followed by an in-scope-definition rejection must reach a second correction: calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
	if (*corrections)[0] != (analysisCorrection{}) || (*corrections)[1].ValidatorMessage != policyMessage || !strings.HasPrefix((*corrections)[2].ValidatorMessage, readiness.InScopeDefinitionMessage) {
		t.Fatalf("each correction must carry the rejection that caused it: %+v", *corrections)
	}
	if len(r.AnalysisAttempts) != 3 || r.AnalysisAttempts[0].Status != "failed" || r.AnalysisAttempts[1].Status != "failed" || r.AnalysisAttempts[2].Status != "completed" {
		t.Fatalf("attempts: %+v", r.AnalysisAttempts)
	}
	for i, a := range r.AnalysisAttempts {
		if a.Ordinal != i+1 {
			t.Fatalf("attempt %d has ordinal %d", i, a.Ordinal)
		}
	}
	if r.Analysis == nil || len(r.Readiness.Plans) != 1 {
		t.Fatal("the corrected analysis did not reach the plan")
	}
	notes := strings.Join(r.Notes, "\n")
	for _, want := range []string{"Rejection class labelling has now used its retry (1 of 3)", "Rejection class in-scope-definition has now used its retry (2 of 3)"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("notes lack %q: %v", want, r.Notes)
		}
	}
}

func TestHardening129EveryOrderOfDistinctClassesIsRetried(t *testing.T) {
	for _, steps := range [][]string{
		{"def", "policy", "valid"}, {"cap", "policy", "valid"}, {"policy", "cap", "valid"}, {"cap", "def", "valid"}, {"def", "cap", "valid"},
		{"issue-def", "policy", "valid"}, {"policy", "def", "cap", "valid"},
	} {
		t.Run(strings.Join(steps, "+"), func(t *testing.T) {
			r, calls, _ := runClasses(t, steps...)
			if *calls != len(steps) || r.Status != "waiting" || r.Readiness == nil {
				t.Fatalf("calls=%d want %d status=%s notes=%v", *calls, len(steps), r.Status, r.Notes)
			}
		})
	}
}

func TestHardening129TheSameClassTwiceStopsIntake(t *testing.T) {
	for _, steps := range [][]string{{"policy", "policy"}, {"def", "def"}, {"cap", "cap"}, {"issue-def", "issue-def"}, {"policy", "def", "policy"}, {"policy", "def", "def"}} {
		t.Run(strings.Join(steps, "+"), func(t *testing.T) {
			r, calls, _ := runClasses(t, steps...)
			if *calls != len(steps) || r.Status != "blocked" || r.Readiness != nil || len(r.HarnessAttempts) != 0 {
				t.Fatalf("calls=%d want %d status=%s notes=%v", *calls, len(steps), r.Status, r.Notes)
			}
			if !strings.Contains(strings.Join(r.Notes, "\n"), "the corrective analysis retry was also rejected; no coding harness was invoked") {
				t.Fatalf("blocked reason: %v", r.Notes)
			}
		})
	}
}

func TestHardening129CorrectiveRetriesAreBoundedByThree(t *testing.T) {
	r, calls, _ := runClasses(t, "policy", "def", "cap", "issue-def")
	if *calls != 4 || r.Status != "blocked" || r.Readiness != nil || len(r.HarnessAttempts) != 0 || len(r.AnalysisAttempts) != 4 {
		t.Fatalf("a fourth class must not get a fourth retry: calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "the limit of 3 corrective analysis retries was reached; no coding harness was invoked") {
		t.Fatalf("blocked reason: %v", r.Notes)
	}
	for i, a := range r.AnalysisAttempts {
		if a.Status != "failed" || a.Response == nil || a.Ordinal != i+1 {
			t.Fatalf("attempt %d lost its error or response: %+v", i, a)
		}
	}
}

func TestHardening129SupplementalPassStillFitsAfterAllThreeRetries(t *testing.T) {
	r, calls, _ := runClasses(t, "policy", "def", "cap", "evidence", "valid")
	if *calls != 5 || r.Status != "waiting" || r.Readiness == nil || len(r.AnalysisAttempts) != 5 {
		t.Fatalf("three retries, one supplemental pass and the final analysis must fit: calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
}

func TestHardening129NonPolicyFailuresAreNotRetried(t *testing.T) {
	for _, step := range []string{"bad-path", "bad-citation"} {
		t.Run(step, func(t *testing.T) {
			r, calls, _ := runClasses(t, step)
			if *calls != 1 || r.Status != "blocked" || len(r.AnalysisAttempts) != 1 {
				t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
			}
		})
	}
}

func TestHardening129CheckForANewFileReachesThePlan(t *testing.T) {
	r, calls, corrections := runClasses(t, "new-file")
	if *calls != 1 || r.Status != "waiting" || r.Readiness == nil || (*corrections)[0] != (analysisCorrection{}) {
		t.Fatalf("a check defined by a new in-scope file must need no retry: calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
	plan := r.Readiness.Plans[0].Plan
	if len(plan.NewFiles) != 1 || plan.NewFiles[0] != "pinned/pinned_test.go" {
		t.Fatalf("the Lab must record the new file from the frozen tree: %v", plan.NewFiles)
	}
	if err := plan.ValidateForCoding(); err != nil {
		t.Fatalf("the plan must be codable: %v", err)
	}
	for _, c := range plan.Analysis.Checks {
		if c.ID == "pinned" && (c.Category != "candidate" || c.IndependentProvenance != "") {
			t.Fatalf("a new-file definition must stay a candidate check: %+v", c)
		}
	}
	notes := strings.Join(r.Notes, "\n")
	for _, want := range []string{"1 candidate check(s) are defined only by a new file", "pinned (defined by pinned/pinned_test.go)", "no frozen anchor", "never independent acceptance"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("the scope gate notes must disclose the new-file definition (%q): %v", want, r.Notes)
		}
	}
}

func TestHardening129PlansWithoutNewFileDefinitionsHaveNoNewFileNote(t *testing.T) {
	r, _, _ := runClasses(t, "valid")
	if r.Readiness == nil || strings.Contains(strings.Join(r.Notes, "\n"), "new file") {
		t.Fatalf("a plan without a new-file definition must not carry the note: %v", r.Notes)
	}
}

func TestHardening129AmendDerivesNewFilesFromTheFrozenTree(t *testing.T) {
	for name, forged := range map[string][]string{
		"dropped":                 nil,
		"claims an existing file": {"code.txt"},
		"claims every scope path": {"code.txt", "request.json", "pinned/pinned_test.go"},
	} {
		t.Run(name, func(t *testing.T) {
			o, _ := fixture(t)
			classScripted(t, &o, "new-file")
			r, err := Run(context.Background(), o)
			if err != nil || r.Readiness == nil {
				t.Fatalf("intake stopped: %v %v", err, r.Notes)
			}
			next := copyPlan(t, r.Readiness.Plans[0].Plan)
			next.NewFiles = forged
			r, err = Amend(context.Background(), o.LabDir, r.ID, r.Readiness.Plans[0].Digest, next, "")
			if err != nil {
				t.Fatalf("an amendment must get its new files from the frozen tree, not the payload: %v", err)
			}
			amended := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
			if len(amended.NewFiles) != 1 || amended.NewFiles[0] != "pinned/pinned_test.go" {
				t.Fatalf("amended new files %v, want only the cited scope path absent from the tree", amended.NewFiles)
			}
			if !strings.Contains(strings.Join(r.Notes, "\n"), "defined only by a new file") {
				t.Fatalf("the amendment must disclose the new-file definition: %v", r.Notes)
			}
		})
	}
}

func TestHardening129ExistingFileCannotPoseAsANewFile(t *testing.T) {
	r, calls, _ := runClasses(t, "existing-file")
	if *calls != 1 || r.Status != "blocked" || r.Readiness != nil {
		t.Fatalf("a scope path that exists in the frozen tree is not new, so citing it must stop: calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
}

func TestHardening129DefinitionCorrectionNamesTheNewFileAlternative(t *testing.T) {
	_, _, corrections := runClasses(t, "issue-def", "valid")
	second := (*corrections)[1]
	if !strings.HasPrefix(second.ValidatorMessage, readiness.DefinitionReferenceMessage) {
		t.Fatalf("the correction must carry the exact validator message: %q", second.ValidatorMessage)
	}
	request := analysisRequest(withAnalysisCorrection(context.Background(), second), Issue{}, readiness.RepositorySnapshot{}, readiness.VerificationInputs{})
	correction, ok := request["correction"].(map[string]string)
	if !ok || !strings.Contains(correction["instruction"], "new file") || !strings.Contains(correction["instruction"], "exact scope path") || correction["validatorMessage"] != second.ValidatorMessage {
		t.Fatalf("the corrective request must name the alternatives: %+v", request["correction"])
	}
	other := analysisRequest(withAnalysisCorrection(context.Background(), analysisCorrection{ValidatorMessage: policyMessage}), Issue{}, readiness.RepositorySnapshot{}, readiness.VerificationInputs{})
	if _, hasInstruction := other["correction"].(map[string]string)["instruction"]; hasInstruction {
		t.Fatal("an unrelated violation must not carry the definition instruction")
	}
}
