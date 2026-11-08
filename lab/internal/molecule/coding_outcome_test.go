package molecule

import (
	"context"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodingNonCompletionSkipsVerification(t *testing.T) {
	t.Parallel()
	for _, output := range []string{`{"schemaVersion":"v1","outcome":"blocked","reason":"Go cache is unwritable","paths":[]}`, `Implementation blocked by unwritable Go cache`, `{"schemaVersion":"v1","outcome":"completed","reason":"done","paths":[],"extra":true}`} {
		for _, edit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/edit=%t", output, edit), func(t *testing.T) {
				o, _ := fixture(t)
				counts := t.TempDir()
				body := "#!/bin/sh\ncat >/dev/null\n"
				if edit {
					body += "printf partial > code.txt\n"
				}
				body += "printf '%s' '" + output + "'\n"
				if err := writeStub(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
				r, err := Run(context.Background(), o)
				if err != nil {
					t.Fatal(err)
				}
				next := copyPlan(t, r.Readiness.Plans[0].Plan)
				check := next.Analysis.Checks[0]
				check.Argv = []string{"/bin/sh", "-c", fmt.Sprintf("echo call >> %q", filepath.Join(counts, "checks"))}
				next.Analysis.Checks[0] = check
				check.ID = "setup"
				check.Category = "setup"
				check.Argv = []string{"/bin/sh", "-c", fmt.Sprintf("echo call >> %q", filepath.Join(counts, "setup"))}
				next.Setup = []readiness.Check{check}
				r, err = Amend(context.Background(), o.LabDir, r.ID, r.Readiness.Plans[0].Digest, next, "")
				if err != nil {
					t.Fatal(err)
				}
				o.Approve = r.Readiness.Plans[1].Digest
				r, err = Run(context.Background(), o)
				if err != nil {
					t.Fatal(err)
				}
				if r.Verification != nil || len(r.VerificationAttempts) != 0 {
					t.Fatal("non-completion invoked verification")
				}
				assertExecutionCounts(t, counts, 0, 0, 0)
				if r.Status != "blocked" || r.Atoms[2].Status == "done" {
					t.Fatalf("non-completion promoted: %+v", r)
				}
				if r.Capture == nil || (len(r.Capture.Changes) > 0) != edit {
					t.Fatal("partial edits lost")
				}
				if !r.HarnessAttempts[0].Result.OK || r.HarnessAttempts[0].Result.Code != 0 {
					t.Fatal("transport truth lost")
				}
				if strings.Contains(output, `"outcome":"blocked"`) && r.HarnessAttempts[0].Coding.Reason != "Go cache is unwritable" {
					t.Fatal("blocker lost")
				}
			})
		}
	}
}

func TestCodingCompletionRequiresEditsOrExactNoChangePermission(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		outcome           string
		allow, edit, want bool
	}{
		{"completed", false, true, true}, {"completed", false, false, false},
		{"no-change", false, false, false}, {"no-change", true, false, true}, {"no-change", true, true, false},
	} {
		t.Run(fmt.Sprintf("%s/allow=%t/edit=%t", tc.outcome, tc.allow, tc.edit), func(t *testing.T) {
			o, _ := fixture(t)
			output := fmt.Sprintf(`{"schemaVersion":"v1","outcome":%q,"reason":"reported","paths":[]}`, tc.outcome)
			body := "#!/bin/sh\ncat >/dev/null\n"
			if tc.edit {
				body += "printf changed > code.txt\n"
			}
			body += "printf '%s' '" + output + "'\n"
			writeStub(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte(body), 0700)
			analyze := o.Analyze
			o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
				a, e := analyze(ctx, b, i, s)
				a.Checks[0].Argv = []string{"/bin/sh", "-c", "exit 0"}
				return a, e
			}
			r, e := Run(context.Background(), o)
			if e != nil {
				t.Fatal(e)
			}
			if tc.allow {
				p := r.Readiness.Plans[0].Plan
				p.AllowNoChange = true
				r, e = Amend(context.Background(), o.LabDir, r.ID, r.Readiness.Plans[0].Digest, p, "")
				if e != nil {
					t.Fatal(e)
				}
			}
			o.Approve = r.Readiness.Plans[len(r.Readiness.Plans)-1].Digest
			r, e = Run(context.Background(), o)
			if e != nil {
				t.Fatal(e)
			}
			if (r.Verification != nil) != tc.want || completedCoding(r) != tc.want {
				t.Fatalf("unexpected completion: phase=%s verification=%v", statePhase(r), r.Verification)
			}
			if !tc.want && len(r.VerificationAttempts) != 0 {
				t.Fatal("invalid completion ran checks")
			}
			if tc.want {
				r.HarnessAttempts[0].Coding.Reason = "tampered"
				if completedCoding(r) {
					t.Fatal("mutated report promoted")
				}
			}
		})
	}
}

func TestCodingEnvironmentBlockedGoesOnToChecks(t *testing.T) {
	t.Parallel()
	const denial = "go test: open /Users/x/Library/Caches/go-build/ab: operation not permitted"
	for _, edit := range []bool{true, false} {
		t.Run(fmt.Sprintf("edit=%t", edit), func(t *testing.T) {
			o, _ := fixture(t)
			counts := t.TempDir()
			body := "#!/bin/sh\ncat >/dev/null\n"
			if edit {
				body += "printf changed > code.txt\n"
			}
			body += "printf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"environment-blocked\",\"reason\":\"" + denial + "\",\"paths\":[]}'\n"
			if err := writeStub(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			analyze := o.Analyze
			o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
				a, e := analyze(ctx, b, i, s)
				a.Checks[0].Argv = []string{"/bin/sh", "-c", fmt.Sprintf("echo call >> %q", filepath.Join(counts, "checks"))}
				return a, e
			}
			r, err := runApprovedFixture(t, o)
			if err != nil {
				t.Fatal(err)
			}
			if r.HarnessAttempts[0].Coding == nil || r.HarnessAttempts[0].Coding.Outcome != EnvironmentBlockedOutcome || r.HarnessAttempts[0].Coding.Reason != denial {
				t.Fatalf("environment-blocked outcome not retained: %+v", r.HarnessAttempts[0].Coding)
			}
			if !edit {
				if r.Verification != nil || len(r.VerificationAttempts) != 0 || statePhase(r) != "unknown" || r.Status != "blocked" {
					t.Fatalf("environment-blocked without edits went on to checks: phase=%s status=%s", statePhase(r), r.Status)
				}
				return
			}
			if r.Verification == nil || !r.Verification.RequiredChecksPassed || len(r.VerificationAttempts) == 0 {
				t.Fatalf("environment-blocked did not run the approved checks: phase=%s verification=%+v", statePhase(r), r.Verification)
			}
			if calls, _ := os.ReadFile(filepath.Join(counts, "checks")); len(calls) == 0 {
				t.Fatal("approved check was not executed")
			}
			if r.Status != "waiting" || r.Atoms[2].Status != "done" {
				t.Fatalf("passing checks after environment-blocked did not wait for review: status=%s phase=%s", r.Status, statePhase(r))
			}
			markdown, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".md"))
			if err != nil {
				t.Fatal(err)
			}
			want := EnvironmentBlockedText(denial)
			if !strings.Contains(string(markdown), "Implementation outcome: environment-blocked") || !strings.Contains(string(markdown), want) || !strings.Contains(want, denial) {
				t.Fatalf("ledger does not quote the denial:\n%s", markdown)
			}
			if reason, ok := EnvironmentBlockedReason(r); !ok || reason != denial {
				t.Fatalf("EnvironmentBlockedReason = %q, %t", reason, ok)
			}
			// checks-only accepts the captured environment-blocked attempt as it
			// accepts completed, and verifies the retained tree without coding.
			if !completedCoding(r) {
				t.Fatal("environment-blocked attempt with captured changes is not eligible for checks-only")
			}
			parent := r.Readiness.Plans[0]
			next := copyPlan(t, parent.Plan)
			next.Continuation = "checks-only"
			if r, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, ""); err != nil {
				t.Fatalf("checks-only refused after environment-blocked: %v", err)
			}
			verifications := len(r.VerificationAttempts)
			o.Approve = r.Readiness.Plans[1].Digest
			if r, err = Run(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			if len(r.HarnessAttempts) != 1 || len(r.VerificationAttempts) <= verifications || r.Verification == nil || !r.Verification.RequiredChecksPassed {
				t.Fatalf("checks-only after environment-blocked: attempts=%d verifications=%d phase=%s", len(r.HarnessAttempts), len(r.VerificationAttempts), statePhase(r))
			}
			if calls, _ := os.ReadFile(filepath.Join(counts, "checks")); strings.Count(string(calls), "call") < 2 {
				t.Fatalf("checks-only did not rerun the approved check: %q", calls)
			}
		})
	}
}

func TestCodingScopeChangeRetainsWorkAndCannotStartChecks(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	writeStub(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte("#!/bin/sh\ncat >/dev/null\nprintf partial > code.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"scope-change\",\"reason\":\"helper needed\",\"paths\":[\"helper.go\"]}'\n"), 0700)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if statePhase(r) != "scope-change" || r.Verification != nil || len(r.VerificationAttempts) > 0 || completedCoding(r) || len(r.Capture.Changes) == 0 {
		t.Fatal("scope-change promoted or work lost")
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "helper.go")); !os.IsNotExist(err) {
		t.Fatal("requested scope written")
	}
}
