package molecule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const policyMessage = "analysis cannot assert independent acceptance review; retain as a candidate check for separate human review"

// policyRejected is what a harness-validated response with a mislabelled check
// yields: the fully decoded analysis plus the typed validator error.
func policyRejected(t *testing.T, valid readiness.Analysis) (readiness.Analysis, error) {
	t.Helper()
	valid.Checks = append([]readiness.Check(nil), valid.Checks...)
	valid.Checks[0].Category = "independent-acceptance"
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	a, err := readiness.DecodeAnalysis(raw)
	if !readiness.IsPolicyViolation(err) {
		t.Fatalf("fixture is not a policy violation: %v", err)
	}
	return a, err
}

func evidenceRequest() readiness.Analysis {
	return readiness.Analysis{Summary: "Need named committed content", EvidenceRequests: []readiness.EvidenceRequest{{Path: "code.txt", Reason: "Inspect implementation", Evidence: []string{"issue"}}}}
}

func intakeActions(t *testing.T, r Record) []AtomAction {
	t.Helper()
	for _, a := range r.Atoms {
		if a.Type == "intake" && a.Provenance != nil {
			return a.Provenance.Actions
		}
	}
	t.Fatal("no intake provenance")
	return nil
}

// scripted drives the fake analysis function through a fixed sequence and records
// the corrective context each invocation received.
func scripted(t *testing.T, o *Options, steps ...string) (calls *int, corrections *[]analysisCorrection) {
	t.Helper()
	valid := o.Analyze
	n := 0
	var seen []analysisCorrection
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		if n >= len(steps) {
			t.Fatalf("unexpected analysis invocation %d", n+1)
		}
		step := steps[n]
		n++
		c, _ := analysisCorrectionFrom(ctx)
		seen = append(seen, c)
		a, err := valid(ctx, b, i, s)
		if err != nil {
			t.Fatal(err)
		}
		switch step {
		case "policy":
			return policyRejected(t, a)
		case "evidence":
			return evidenceRequest(), nil
		case "cap":
			return withEvidenceRequests(a, 20), nil
		case "cap-rejected":
			return capRejected(t, withEvidenceRequests(a, 20))
		case "seventeen":
			return withEvidenceRequests(a, 17), nil
		case "sixteen":
			return withEvidenceRequests(a, 16), nil
		case "twelve":
			return withEvidenceRequests(a, 12), nil
		case "bad-path":
			a.EvidenceRequests = []readiness.EvidenceRequest{{Path: "../escape", Reason: "needs content", Evidence: []string{"issue"}}}
			return a, nil
		case "bad-citation":
			a.EvidenceRequests = []readiness.EvidenceRequest{{Path: "code.txt", Reason: "needs content", Evidence: []string{"unknown-evidence"}}}
			return a, nil
		}
		return a, nil
	}
	return &n, &seen
}

const capMessage = "at most 16 evidence requests permitted"

func withEvidenceRequests(a readiness.Analysis, n int) readiness.Analysis {
	a.EvidenceRequests = nil
	for i := 0; i < n; i++ {
		a.EvidenceRequests = append(a.EvidenceRequests, readiness.EvidenceRequest{Path: fmt.Sprintf("docs/note-%02d.md", i), Reason: "Inspect committed content", Evidence: []string{"issue"}})
	}
	return a
}

// capRejected is what the default analysis path yields for an over-cap response:
// the harness boundary preserves the text and the typed validator error survives.
func capRejected(t *testing.T, over readiness.Analysis) (readiness.Analysis, error) {
	t.Helper()
	raw, err := json.Marshal(over)
	if err != nil {
		t.Fatal(err)
	}
	a, err := decodeAnalysisResult(process.Result{Error: harness.InvalidAnalysisPrefix + capMessage, Stdout: string(raw)})
	if !readiness.IsPolicyViolation(err) {
		t.Fatalf("fixture is not a cap rejection: %v", err)
	}
	return a, err
}

func TestAnalysisCorrectiveRetryEvidenceRequestCapThenTwelveYieldsPlan(t *testing.T) {
	t.Parallel()
	for _, first := range []string{"cap", "cap-rejected"} {
		t.Run(first, func(t *testing.T) {
			o, _ := fixture(t)
			calls, corrections := scripted(t, &o, first, "twelve", "valid")
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if *calls != 3 || r.Status != "waiting" || r.Readiness == nil || r.Readiness.Phase != "scope-waiting" {
				t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
			}
			if (*corrections)[0] != (analysisCorrection{}) {
				t.Fatalf("the first analysis must not carry a correction: %+v", (*corrections)[0])
			}
			second := (*corrections)[1]
			if second.ValidatorMessage != capMessage {
				t.Fatalf("corrective call did not receive the exact validator message: %q", second.ValidatorMessage)
			}
			if !strings.Contains(second.PreviousResponse, `"path":"docs/note-19.md"`) || !strings.Contains(second.PreviousResponse, `"summary":"Change code"`) {
				t.Fatalf("corrective call did not receive the 20-request response: %q", second.PreviousResponse)
			}
			if (*corrections)[2] != (analysisCorrection{}) {
				t.Fatalf("the supplemental pass after a corrected analysis carries no correction: %+v", (*corrections)[2])
			}
			if len(r.AnalysisAttempts) != 3 {
				t.Fatalf("all attempts must be recorded: %+v", r.AnalysisAttempts)
			}
			one, two, three := r.AnalysisAttempts[0], r.AnalysisAttempts[1], r.AnalysisAttempts[2]
			wantError := capMessage
			if first == "cap-rejected" {
				wantError = "analysis failed: " + harness.InvalidAnalysisPrefix + capMessage + " "
			}
			if one.Ordinal != 1 || one.Status != "failed" || one.Error != wantError || one.Response == nil || len(one.Response.EvidenceRequests) != 20 {
				t.Fatalf("the rejected over-cap attempt must keep its error and response: %+v", one)
			}
			if two.Ordinal != 2 || two.Status != "completed" || two.Response == nil || len(two.Response.EvidenceRequests) != 12 {
				t.Fatalf("the corrected attempt: %+v", two)
			}
			if three.Ordinal != 3 || three.Status != "completed" || three.Response == nil || len(three.Response.EvidenceRequests) != 0 {
				t.Fatalf("the post-supplement attempt: %+v", three)
			}
			if !strings.Contains(strings.Join(r.Notes, "\n"), "corrective analysis retry") {
				t.Fatalf("retry not recorded in notes: %v", r.Notes)
			}
			if r.Analysis == nil || len(r.Analysis.EvidenceRequests) != 0 || len(r.Readiness.Plans[0].Plan.Analysis.EvidenceRequests) != 0 {
				t.Fatalf("an over-cap or unresolved response reached the plan: %+v", r.Analysis)
			}
			seen := map[string]bool{}
			analysisActions := 0
			for _, a := range intakeActions(t, r) {
				if !strings.HasPrefix(a.AttemptID, "analysis-") {
					continue
				}
				analysisActions++
				if seen[a.AttemptID] || a.Observed == nil {
					t.Fatalf("analysis action ids must be unique and each observed: %+v", intakeActions(t, r))
				}
				seen[a.AttemptID] = true
			}
			if analysisActions != 3 {
				t.Fatalf("expected 3 analysis actions, got %d", analysisActions)
			}
		})
	}
}

func TestAnalysisCorrectiveRetryEvidenceRequestCapTwiceBlocks(t *testing.T) {
	t.Parallel()
	for _, steps := range [][]string{{"cap", "cap"}, {"cap-rejected", "cap-rejected"}, {"cap", "cap-rejected"}} {
		t.Run(strings.Join(steps, "+"), func(t *testing.T) {
			o, _ := fixture(t)
			calls, _ := scripted(t, &o, steps...)
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
				if a.Ordinal != i+1 || a.Status != "failed" || !strings.Contains(a.Error, capMessage) || a.Response == nil || len(a.Response.EvidenceRequests) != 20 {
					t.Fatalf("attempt %d: %+v", i, a)
				}
			}
			reason := strings.Join(r.Notes, "\n")
			for _, want := range []string{capMessage, "the corrective analysis retry was also rejected; no coding harness was invoked"} {
				if !strings.Contains(reason, want) {
					t.Fatalf("blocked reason lacks %q: %v", want, r.Notes)
				}
			}
		})
	}
}

func TestAnalysisCorrectiveRetryEvidenceRequestCapSharesTheSingleRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		steps    []string
		blocked  bool
		attempts []string
	}{
		{"cap then violation", []string{"cap", "policy", "valid"}, false, []string{"failed", "failed", "completed"}},
		{"violation then cap", []string{"policy", "cap", "valid"}, false, []string{"failed", "failed", "completed"}},
		{"evidence then cap then cap", []string{"evidence", "cap", "cap"}, true, []string{"completed", "failed", "failed"}},
		{"cap then evidence then cap", []string{"cap", "evidence", "cap"}, true, []string{"failed", "completed", "failed"}},
		{"cap then evidence twice is exhausted", []string{"cap", "evidence", "evidence"}, true, []string{"failed", "completed", "completed"}},
		{"evidence then cap then evidence is exhausted", []string{"evidence", "cap", "evidence"}, true, []string{"completed", "failed", "completed"}},
		{"cap then evidence then valid", []string{"cap", "evidence", "valid"}, false, []string{"failed", "completed", "completed"}},
		{"evidence then cap then valid", []string{"evidence", "cap", "valid"}, false, []string{"completed", "failed", "completed"}},
		{"violation then evidence then valid", []string{"policy", "evidence", "valid"}, false, []string{"failed", "completed", "completed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := fixture(t)
			calls, _ := scripted(t, &o, tc.steps...)
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if *calls != len(tc.steps) || *calls > 3 {
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
				if want == "failed" && ((a.Error != policyMessage && a.Error != capMessage) || a.Response == nil) {
					t.Fatalf("attempt %d lost its validator error or response: %+v", i, a)
				}
			}
			if tc.blocked && len(r.HarnessAttempts) != 0 {
				t.Fatal("coding harness invoked after a blocked intake")
			}
		})
	}
}

func TestAnalysisCorrectiveRetryEvidenceRequestCapBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		steps   []string
		retried bool
	}{
		{"sixteen requests are accepted", []string{"sixteen", "valid"}, false},
		{"seventeen requests are rejected", []string{"seventeen", "valid"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := fixture(t)
			calls, corrections := scripted(t, &o, tc.steps...)
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if *calls != 2 || r.Status != "waiting" || r.Readiness == nil {
				t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
			}
			if got := (*corrections)[1].ValidatorMessage == capMessage; got != tc.retried {
				t.Fatalf("retried=%t want %t", got, tc.retried)
			}
			if got := strings.Contains(strings.Join(r.Notes, "\n"), "corrective analysis retry"); got != tc.retried {
				t.Fatalf("retry note=%t want %t: %v", got, tc.retried, r.Notes)
			}
			if first := r.AnalysisAttempts[0]; (first.Status == "failed") != tc.retried {
				t.Fatalf("first attempt: %+v", first)
			}
		})
	}
}

func TestAnalysisCorrectiveRetryEvidenceRequestCapDoesNotRetryOtherRequestErrors(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"bad-path", "bad-citation"} {
		t.Run("cap then "+bad, func(t *testing.T) {
			o, _ := fixture(t)
			calls, _ := scripted(t, &o, "cap", bad)
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if *calls != 2 || r.Status != "blocked" || r.Readiness != nil || len(r.HarnessAttempts) != 0 {
				t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
			}
			if len(r.AnalysisAttempts) != 2 || r.AnalysisAttempts[0].Status != "failed" || r.AnalysisAttempts[1].Status != "completed" {
				t.Fatalf("attempts: %+v", r.AnalysisAttempts)
			}
			if strings.Contains(strings.Join(r.Notes, "\n"), "also rejected") {
				t.Fatalf("the later request error was treated as a second rejection: %v", r.Notes)
			}
		})
	}
}

// analysisRepoDir returns an empty repository root, so the analysis
// project-config scan stops inside the test tree instead of walking up to
// whatever lies above the temp directory.
func analysisRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The fake provider drives the real Execute path for an over-cap response: the
// rejected text must survive the harness boundary and the correction, with its
// cap-specific instruction, must reach the provider prompt.
func TestAnalysisCorrectiveRetryEvidenceRequestCapReachesProviderPrompt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	valid := readiness.Analysis{Summary: "Change code"}
	rejected, err := json.Marshal(withEvidenceRequests(valid, 20))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"structured_output": json.RawMessage(rejected)})
	if err != nil {
		t.Fatal(err)
	}
	stdin, out := filepath.Join(dir, "stdin.txt"), filepath.Join(dir, "envelope.json")
	if err = os.WriteFile(out, envelope, 0600); err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(dir, "provider")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in --help) printf '%%s' '--safe-mode --tools --strict-mcp-config --json-schema --no-chrome --disable-slash-commands'; exit 0;; esac\ncat > '%s'\ncat '%s'\n", stdin, out)
	if err = writeStub(provider, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx := withAnalysisCorrection(context.Background(), analysisCorrection{ValidatorMessage: capMessage, PreviousResponse: "PREVIOUS-RESPONSE-MARKER"})
	request := analysisRequest(ctx, Issue{Number: 1, Title: "t"}, readiness.RepositorySnapshot{}, readiness.VerificationInputs{})
	result := harness.Execute(ctx, harness.ExecuteOptions{ID: "claude-code", Executable: provider, Dir: analysisRepoDir(t), Request: request, Timeout: 30 * time.Second})
	a, err := decodeAnalysisResult(result.Process)
	if !readiness.IsPolicyViolation(err) || len(a.EvidenceRequests) != 20 || !strings.Contains(err.Error(), capMessage) {
		t.Fatalf("the rejected response did not survive the harness boundary: %v %+v", err, result.Process)
	}
	raw, err := os.ReadFile(stdin)
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(raw)
	for _, want := range []string{`"correction"`, "PREVIOUS-RESPONSE-MARKER", `"validatorMessage":"at most 16 evidence requests permitted"`, `"instruction":"Return the complete analysis again with at most 16 evidenceRequests, ordered by importance to the ticket scope`, "rejected by the Lab validator"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("provider prompt lacks %q:\n%s", want, prompt)
		}
	}
	policy := withAnalysisCorrection(context.Background(), analysisCorrection{ValidatorMessage: policyMessage, PreviousResponse: "x"})
	if _, ok := analysisRequest(policy, Issue{}, readiness.RepositorySnapshot{}, readiness.VerificationInputs{})["correction"].(map[string]string)["instruction"]; ok {
		t.Fatal("the independent-acceptance correction must not carry the cap instruction")
	}
}

func TestAnalysisCorrectiveRetryEvidenceRequestCapOnlyClassifiesValidatedRejections(t *testing.T) {
	t.Parallel()
	over, err := json.Marshal(withEvidenceRequests(readiness.Analysis{Summary: "Change code"}, 20))
	if err != nil {
		t.Fatal(err)
	}
	within, err := json.Marshal(withEvidenceRequests(readiness.Analysis{Summary: "Change code"}, 16))
	if err != nil {
		t.Fatal(err)
	}
	a, err := decodeAnalysisResult(process.Result{Error: harness.InvalidAnalysisPrefix + capMessage, Stdout: string(over)})
	if !readiness.IsPolicyViolation(err) || len(a.EvidenceRequests) != 20 || !strings.HasPrefix(err.Error(), "analysis failed: invalid readiness analysis: "+capMessage) {
		t.Fatalf("validated cap rejection not classified: %v %+v", err, a)
	}
	for _, tc := range []struct {
		name   string
		result process.Result
	}{
		{"no preserved text", process.Result{Error: harness.InvalidAnalysisPrefix + capMessage}},
		{"different validation message", process.Result{Error: harness.InvalidAnalysisPrefix + "at most 17 evidence requests permitted", Stdout: string(over)}},
		{"preserved text within the cap", process.Result{Error: harness.InvalidAnalysisPrefix + capMessage, Stdout: string(within)}},
		{"timeout", process.Result{Error: "context deadline exceeded", TimedOut: true, Stdout: string(over)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeAnalysisResult(tc.result); err == nil || readiness.IsPolicyViolation(err) {
				t.Fatalf("must not be retryable: %v", err)
			}
		})
	}
}

func TestAnalysisCorrectiveRetryPolicyViolationThenValidYieldsPlan(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	calls, corrections := scripted(t, &o, "policy", "valid")
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || r.Status != "waiting" || r.Readiness == nil || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("calls=%d status=%s notes=%v", *calls, r.Status, r.Notes)
	}
	first, second := (*corrections)[0], (*corrections)[1]
	if first != (analysisCorrection{}) {
		t.Fatalf("the first analysis must not carry a correction: %+v", first)
	}
	if second.ValidatorMessage != policyMessage {
		t.Fatalf("corrective call did not receive the exact validator message: %q", second.ValidatorMessage)
	}
	if !strings.Contains(second.PreviousResponse, `"independent-acceptance"`) || !strings.Contains(second.PreviousResponse, `"summary":"Change code"`) {
		t.Fatalf("corrective call did not receive the previous response: %q", second.PreviousResponse)
	}
	if len(r.AnalysisAttempts) != 2 || r.AnalysisAttempts[0].Status != "failed" || r.AnalysisAttempts[0].Error != policyMessage || r.AnalysisAttempts[1].Status != "completed" || r.AnalysisAttempts[1].Response == nil {
		t.Fatalf("both attempts must be recorded: %+v", r.AnalysisAttempts)
	}
	if r.AnalysisAttempts[0].Ordinal != 1 || r.AnalysisAttempts[1].Ordinal != 2 {
		t.Fatalf("ordinals: %+v", r.AnalysisAttempts)
	}
	if rejected := r.AnalysisAttempts[0].Response; rejected == nil || rejected.Checks[0].Category != "independent-acceptance" {
		t.Fatalf("the rejected response must stay in the ledger for audit: %+v", rejected)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "corrective analysis retry") {
		t.Fatalf("retry not recorded in notes: %v", r.Notes)
	}
	// The accepted analysis passed the unchanged validator: no independent acceptance.
	for _, c := range r.Readiness.Plans[0].Plan.Analysis.Checks {
		if c.Category == "independent-acceptance" || c.IndependentProvenance != "" {
			t.Fatalf("plan retained a model-asserted independent acceptance: %+v", c)
		}
	}
}

func TestAnalysisCorrectiveRetryBoundedSequences(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		steps    []string
		blocked  bool
		attempts []string
	}{
		{"violation twice blocks", []string{"policy", "policy"}, true, []string{"failed", "failed"}},
		{"violation then evidence then valid", []string{"policy", "evidence", "valid"}, false, []string{"failed", "completed", "completed"}},
		{"violation then evidence then violation", []string{"policy", "evidence", "policy"}, true, []string{"failed", "completed", "failed"}},
		{"violation then evidence twice is exhausted", []string{"policy", "evidence", "evidence"}, true, []string{"failed", "completed", "completed"}},
		{"evidence then violation then valid", []string{"evidence", "policy", "valid"}, false, []string{"completed", "failed", "completed"}},
		{"evidence then violation twice blocks", []string{"evidence", "policy", "policy"}, true, []string{"completed", "failed", "failed"}},
		{"evidence twice is exhausted", []string{"evidence", "evidence"}, true, []string{"completed", "completed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := fixture(t)
			calls, _ := scripted(t, &o, tc.steps...)
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if *calls != len(tc.steps) {
				t.Fatalf("calls=%d want %d", *calls, len(tc.steps))
			}
			if tc.blocked != (r.Status == "blocked") || tc.blocked != (r.Readiness == nil) {
				t.Fatalf("blocked=%t status=%s notes=%v", tc.blocked, r.Status, r.Notes)
			}
			if len(r.AnalysisAttempts) != len(tc.attempts) {
				t.Fatalf("attempts: %+v", r.AnalysisAttempts)
			}
			actions := intakeActions(t, r)
			seen := map[string]bool{}
			analysisActions := 0
			for i, want := range tc.attempts {
				if r.AnalysisAttempts[i].Status != want || r.AnalysisAttempts[i].Ordinal != i+1 {
					t.Fatalf("attempt %d: %+v want %s", i, r.AnalysisAttempts[i], want)
				}
				if want == "failed" && r.AnalysisAttempts[i].Error != policyMessage {
					t.Fatalf("attempt %d lost its own validator error: %q", i, r.AnalysisAttempts[i].Error)
				}
			}
			for _, a := range actions {
				if !strings.HasPrefix(a.AttemptID, "analysis-") {
					continue
				}
				analysisActions++
				if seen[a.AttemptID] || a.Observed == nil {
					t.Fatalf("analysis action ids must be unique and each observed: %+v", actions)
				}
				seen[a.AttemptID] = true
			}
			if analysisActions != len(tc.steps) {
				t.Fatalf("expected %d analysis actions, got %+v", len(tc.steps), actions)
			}
			if tc.blocked && len(r.HarnessAttempts) != 0 {
				t.Fatal("coding harness invoked after a blocked intake")
			}
		})
	}
}

func TestAnalysisCorrectiveRetryNeverRepeatsOtherErrors(t *testing.T) {
	t.Parallel()
	_, decodeErr := readiness.DecodeAnalysis([]byte(`{"summary":"x","surprise":1}`))
	harnessErr := fmt.Errorf("analysis failed: %w", errors.New("timed out"))
	badPath := readiness.Analysis{Summary: "x", EvidenceRequests: []readiness.EvidenceRequest{{Path: "../escape", Reason: "needs content", Evidence: []string{"issue"}}}}
	badCitation := readiness.Analysis{Summary: "x", EvidenceRequests: []readiness.EvidenceRequest{{Path: "code.txt", Reason: "needs content", Evidence: []string{"unknown-evidence"}}}}
	for _, tc := range []struct {
		name     string
		analysis readiness.Analysis
		err      error
	}{
		{"harness failure", readiness.Analysis{}, harnessErr},
		{"decode failure", readiness.Analysis{}, decodeErr},
		{"evidence request path validation", badPath, nil},
		{"evidence request citation validation", badCitation, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := fixture(t)
			calls := 0
			o.Analyze = func(context.Context, formula.Binding, Issue, readiness.RepositorySnapshot) (readiness.Analysis, error) {
				calls++
				return tc.analysis, tc.err
			}
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(r.AnalysisAttempts) != 1 || r.Status != "blocked" || r.Readiness != nil {
				t.Fatalf("calls=%d status=%s attempts=%+v", calls, r.Status, r.AnalysisAttempts)
			}
			if strings.Contains(strings.Join(r.Notes, "\n"), "corrective analysis retry") {
				t.Fatalf("non-policy error was treated as a retry: %v", r.Notes)
			}
		})
	}
}

func TestAnalysisCorrectiveRetryNeverRelaxesValidator(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	valid := o.Analyze
	calls := 0
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		calls++
		a, err := valid(ctx, b, i, s)
		if err != nil {
			t.Fatal(err)
		}
		// The corrective response keeps asserting acceptance: the validator is not relaxed.
		a.Checks[0].IndependentProvenance = "model says reviewed"
		raw, _ := json.Marshal(a)
		return readiness.DecodeAnalysis(raw)
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || r.Status != "blocked" || r.Readiness != nil {
		t.Fatalf("calls=%d status=%s", calls, r.Status)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), policyMessage) {
		t.Fatalf("blocked reason lost the validator message: %v", r.Notes)
	}
}

func TestAnalysisCorrectiveRetryDefaultAnalysisOnlyClassifiesValidatedRejections(t *testing.T) {
	t.Parallel()
	rejected, err := json.Marshal(readiness.Analysis{Summary: "Change code", Checks: []readiness.Check{{ID: "check", Category: "independent-acceptance", Argv: []string{"true"}, Dir: ".", TimeoutMS: 1000, Required: true, Reason: "r"}}})
	if err != nil {
		t.Fatal(err)
	}
	policyResult := process.Result{Error: harness.InvalidAnalysisPrefix + policyMessage, Stdout: string(rejected)}
	a, err := decodeAnalysisResult(policyResult)
	if !readiness.IsPolicyViolation(err) || a.Summary != "Change code" || !strings.HasPrefix(err.Error(), "analysis failed: invalid readiness analysis: "+policyMessage) {
		t.Fatalf("validated rejection not classified: %v %+v", err, a)
	}
	for _, tc := range []struct {
		name   string
		result process.Result
	}{
		{"timeout", process.Result{Error: "context deadline exceeded", TimedOut: true, Stdout: string(rejected)}},
		{"no preserved text", process.Result{Error: harness.InvalidAnalysisPrefix + policyMessage}},
		{"other validation", process.Result{Error: harness.InvalidAnalysisPrefix + "analysis needs a bounded summary", Stdout: string(rejected)}},
		{"raw envelope", process.Result{Error: harness.InvalidAnalysisPrefix + policyMessage, Stdout: `{"result":"x"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeAnalysisResult(tc.result); err == nil || readiness.IsPolicyViolation(err) {
				t.Fatalf("must not be retryable: %v", err)
			}
		})
	}
	ok, err := decodeAnalysisResult(process.Result{OK: true, Stdout: `{"summary":"fine"}`})
	if err != nil || ok.Summary != "fine" {
		t.Fatalf("%v %+v", err, ok)
	}
}

// A fake provider drives the real Execute path: the rejected text must survive the
// harness boundary, and the correction must reach the provider prompt.
func TestAnalysisCorrectiveRetryReachesProviderPrompt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rejected, err := json.Marshal(readiness.Analysis{Summary: "Change code", Checks: []readiness.Check{{ID: "check", Category: "independent-acceptance", Argv: []string{"true"}, Dir: ".", TimeoutMS: 1000, Required: true, Reason: "r"}}})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"structured_output": json.RawMessage(rejected)})
	if err != nil {
		t.Fatal(err)
	}
	stdin, out := filepath.Join(dir, "stdin.txt"), filepath.Join(dir, "envelope.json")
	if err = os.WriteFile(out, envelope, 0600); err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(dir, "provider")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in --help) printf '%%s' '--safe-mode --tools --strict-mcp-config --json-schema --no-chrome --disable-slash-commands'; exit 0;; esac\ncat > '%s'\ncat '%s'\n", stdin, out)
	if err = writeStub(provider, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx := withAnalysisCorrection(context.Background(), analysisCorrection{ValidatorMessage: policyMessage, PreviousResponse: "PREVIOUS-RESPONSE-MARKER"})
	request := analysisRequest(ctx, Issue{Number: 1, Title: "t"}, readiness.RepositorySnapshot{}, readiness.VerificationInputs{})
	result := harness.Execute(ctx, harness.ExecuteOptions{ID: "claude-code", Executable: provider, Dir: analysisRepoDir(t), Request: request, Timeout: 30 * time.Second})
	_, err = decodeAnalysisResult(result.Process)
	if !readiness.IsPolicyViolation(err) {
		t.Fatalf("the rejected response did not survive the harness boundary: %v %+v", err, result.Process)
	}
	raw, err := os.ReadFile(stdin)
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(raw)
	for _, want := range []string{`"correction"`, "PREVIOUS-RESPONSE-MARKER", `"validatorMessage":"analysis cannot assert independent acceptance review`, "rejected by the Lab validator"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("provider prompt lacks %q:\n%s", want, prompt)
		}
	}
	plain := analysisRequest(context.Background(), Issue{Number: 1, Title: "t"}, readiness.RepositorySnapshot{}, readiness.VerificationInputs{})
	if _, ok := plain["correction"]; ok {
		t.Fatal("first analysis request must not carry a correction")
	}
}
