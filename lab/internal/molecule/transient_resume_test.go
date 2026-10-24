package molecule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
)

// transientFailure is a Claude CLI final JSON for a provider connection that dropped:
// the shape #170 and #189 recorded. It carries a reported cost, so both attempts' usage
// is visible. The provider script single-quotes it, so it holds no apostrophe.
const transientFailure = `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"API Error: Cannot reach the API server - check your internet or DNS (ENOTFOUND)","total_cost_usd":0.25}`

// transientResumeFixture binds the approved Formula to a fake claude that writes partial
// work and fails with failure for its first fails calls, then completes. Each call's argv
// and stdin are kept outside the checkout as call-N. The backoff and the host check are
// replaced, and the waits the Molecule asked for are returned.
func transientResumeFixture(t *testing.T, fails int, failure string, resolve func(n int) error) (Options, string, *[]int) {
	t.Helper()
	o, _ := claudeAllowlistFixture(t)
	root := filepath.Dir(o.SourceRoot)
	calls := filepath.Join(root, "calls")
	if err := os.Mkdir(calls, 0700); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
n=$(ls %[1]q | wc -l | tr -d ' ')
n=$((n+1))
{ printf '%%s\n' "$@"; cat; } > %[1]q/call-$n
if [ "$n" -le %[2]d ]; then
  printf partial > code.txt
  printf '%%s' '%[3]s'
  exit 1
fi
printf changed > code.txt
printf '%%s' '{"structured_output":{"schemaVersion":"v1","outcome":"completed","reason":"implemented","paths":[]},"total_cost_usd":0.5}'
`, calls, fails, failure)
	if err := writeStub(filepath.Join(root, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	waits := &[]int{}
	o.TransientBackoff = func(_ context.Context, n int) error {
		*waits = append(*waits, n)
		return nil
	}
	o.ResolveProvider = func(context.Context) error {
		if resolve != nil {
			return resolve(len(*waits))
		}
		return nil
	}
	return o, calls, waits
}

func readCall(t *testing.T, calls string, n int) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(calls, fmt.Sprintf("call-%d", n)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func callCount(t *testing.T, calls string) int {
	t.Helper()
	entries, err := os.ReadDir(calls)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

const resumeText = "The previous attempt was interrupted by a provider network error; the workspace contains its partial work. Review it and complete every acceptance item. If the retained work already satisfies every acceptance item, report outcome completed, not no-change."

// TestTransientProviderResumeCompletesAfterOneENOTFOUND: a provider that fails once with
// ENOTFOUND and then succeeds leaves the Molecule completed with 2 coding attempts on the
// same plan digest, the resume note and both attempts' usage, with no human action.
func TestTransientProviderResumeCompletesAfterOneENOTFOUND(t *testing.T) {
	t.Parallel()
	o, calls, waits := transientResumeFixture(t, 1, transientFailure, nil)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "waiting" || !completedCoding(r) || r.Verification == nil || !r.Verification.RequiredChecksPassed {
		t.Fatalf("resumed Molecule did not complete: status=%s phase=%s notes=%v", r.Status, statePhase(r), r.Notes)
	}
	if len(r.HarnessAttempts) != 2 || callCount(t, calls) != 2 || !slices.Equal(*waits, []int{1}) {
		t.Fatalf("attempts=%d calls=%d waits=%v", len(r.HarnessAttempts), callCount(t, calls), *waits)
	}
	first, second := r.HarnessAttempts[0], r.HarnessAttempts[1]
	if first.Classification != harness.TransientProviderError || first.Resumed != "" || first.Coding == nil || first.Coding.Outcome != "unknown" || !strings.Contains(first.Coding.Reason, "ENOTFOUND") {
		t.Fatalf("first attempt: %+v", first)
	}
	if second.Classification != "" || second.Resumed != "resumed after transient-provider-error (1 of 2)" || second.Coding == nil || second.Coding.Outcome != "completed" {
		t.Fatalf("second attempt: %+v", second)
	}
	digest := r.Readiness.Plans[0].Digest
	if first.PlanDigest != digest || second.PlanDigest != digest || len(r.Readiness.Plans) != 1 {
		t.Fatalf("resume changed the approved plan: %s %s plans=%d", first.PlanDigest, second.PlanDigest, len(r.Readiness.Plans))
	}
	if first.Usage == nil || first.Usage.TotalCostUSD == nil || *first.Usage.TotalCostUSD != 0.25 || second.Usage == nil || second.Usage.TotalCostUSD == nil || *second.Usage.TotalCostUSD != 0.5 {
		t.Fatalf("usage of both attempts not kept: %+v %+v", first.Usage, second.Usage)
	}
	if !strings.Contains(strings.Join(UsageSummaryLines(r), "\n"), "$0.750000 total") {
		t.Fatalf("reported cost does not sum both attempts:\n%s", strings.Join(UsageSummaryLines(r), "\n"))
	}
	if strings.Contains(readCall(t, calls, 1), resumeText) || !strings.Contains(readCall(t, calls, 2), resumeText) {
		t.Fatal("the resume instruction was not added to exactly the resumed attempt")
	}
	note := false
	for _, n := range r.Notes {
		if strings.Contains(n, "resumed after transient-provider-error (1 of 2)") && strings.Contains(n, first.ID) && strings.Contains(n, digest) {
			note = true
		}
	}
	if !note {
		t.Fatalf("no resume note in the ledger: %v", r.Notes)
	}
	data, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Record
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.HarnessAttempts) != 2 || saved.HarnessAttempts[0].Classification != harness.TransientProviderError || saved.HarnessAttempts[1].Resumed == "" {
		t.Fatalf("persisted ledger lost the resume: %+v", saved.HarnessAttempts)
	}
}

// TestTransientProviderResumeStopsAfterTwoResumes: three consecutive transient failures
// stop after 2 resumes, with a clear reason naming the exhausted retries.
func TestTransientProviderResumeStopsAfterTwoResumes(t *testing.T) {
	t.Parallel()
	o, calls, waits := transientResumeFixture(t, 3, transientFailure, nil)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "failed" || r.Verification != nil || len(r.HarnessAttempts) != 3 || callCount(t, calls) != 3 || !slices.Equal(*waits, []int{1, 2}) {
		t.Fatalf("status=%s attempts=%d calls=%d waits=%v", r.Status, len(r.HarnessAttempts), callCount(t, calls), *waits)
	}
	for i, want := range []string{"", "resumed after transient-provider-error (1 of 2)", "resumed after transient-provider-error (2 of 2)"} {
		if a := r.HarnessAttempts[i]; a.Resumed != want || a.Classification != harness.TransientProviderError {
			t.Fatalf("attempt %d: resumed=%q classification=%q", i, a.Resumed, a.Classification)
		}
	}
	if !TransientRetriesExhausted(r) || !slices.Equal(r.TransientExhausted, []string{r.HarnessAttempts[0].ID, r.HarnessAttempts[1].ID, r.HarnessAttempts[2].ID}) {
		t.Fatalf("exhausted resumes not reported: %v", r.TransientExhausted)
	}
	detail := r.Atoms[2].Detail
	if r.Atoms[2].Status != "failed" || !strings.Contains(detail, "transient-provider-error retries exhausted after 2 automatic resumes and 2 provider checks; last error: API Error: Cannot reach the API server") || strings.Contains(detail, "stayed unreachable") {
		t.Fatalf("unclear stop reason: %s %q", r.Atoms[2].Status, detail)
	}
}

// TestTransientProviderResumeCountsAnUnresolvedHost: a provider host that does not resolve
// after the first backoff spends that resume; the second resume then runs.
// TestTransientProviderResumeSkipsAnUnresolvedHostWithoutUsingAResume: a failed provider
// check is noted and waited past, but only the resume that starts takes a slot.
func TestTransientProviderResumeSkipsAnUnresolvedHostWithoutUsingAResume(t *testing.T) {
	t.Parallel()
	o, calls, waits := transientResumeFixture(t, 1, transientFailure, func(n int) error {
		if n == 1 {
			return errors.New("provider host api.anthropic.com did not resolve")
		}
		return nil
	})
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "waiting" || len(r.HarnessAttempts) != 2 || callCount(t, calls) != 2 || !slices.Equal(*waits, []int{1, 2}) {
		t.Fatalf("status=%s attempts=%d calls=%d waits=%v", r.Status, len(r.HarnessAttempts), callCount(t, calls), *waits)
	}
	if r.HarnessAttempts[1].Resumed != "resumed after transient-provider-error (1 of 2)" {
		t.Fatalf("resumed=%q", r.HarnessAttempts[1].Resumed)
	}
	if !slices.ContainsFunc(r.Notes, func(n string) bool {
		return strings.Contains(n, "Automatic resume 1 of 2") && strings.Contains(n, "did not resolve")
	}) {
		t.Fatalf("unresolved host not noted: %v", r.Notes)
	}
}

// TestTransientProviderResumeNeverRetriesOtherFailures: a non-network api_error, a
// refusal and a permission error end the coding stage after one attempt.
func TestTransientProviderResumeNeverRetriesOtherFailures(t *testing.T) {
	t.Parallel()
	for name, failure := range map[string]string{
		"non-network api_error": `{"type":"result","is_error":true,"terminal_reason":"api_error","result":"API Error: 400 invalid_request_error"}`,
		"refusal":               `{"type":"result","is_error":true,"terminal_reason":"refusal","result":"Declined (ENOTFOUND)"}`,
		"permission":            `{"type":"result","is_error":true,"terminal_reason":"permission_denied","result":"Permission denied"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o, calls, waits := transientResumeFixture(t, 3, failure, nil)
			r, err := runApprovedFixture(t, o)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.HarnessAttempts) != 1 || callCount(t, calls) != 1 || len(*waits) != 0 || r.HarnessAttempts[0].Classification != "" || r.Status != "failed" {
				t.Fatalf("status=%s attempts=%d calls=%d waits=%v", r.Status, len(r.HarnessAttempts), callCount(t, calls), *waits)
			}
			if TransientRetriesExhausted(r) {
				t.Fatal("a non-transient failure reads as provider-unavailable")
			}
		})
	}
}

// TestTransientProviderResumeStopsWhenTheHostNeverResolves: provider checks that keep
// failing end the stage after maxProviderWaits with the last check's error, and that
// stop is the genuine exhausted path.
func TestTransientProviderResumeStopsWhenTheHostNeverResolves(t *testing.T) {
	t.Parallel()
	o, calls, waits := transientResumeFixture(t, 3, transientFailure, func(int) error {
		return errors.New("provider host api.anthropic.com did not resolve")
	})
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "failed" || len(r.HarnessAttempts) != 1 || callCount(t, calls) != 1 || !slices.Equal(*waits, []int{1, 2, 3, 4}) {
		t.Fatalf("status=%s attempts=%d calls=%d waits=%v", r.Status, len(r.HarnessAttempts), callCount(t, calls), *waits)
	}
	if !TransientRetriesExhausted(r) {
		t.Fatal("a host that never resolved is not reported as exhausted")
	}
	if detail := r.Atoms[2].Detail; !strings.Contains(detail, "after 0 automatic resumes and 4 provider checks; last error: provider host api.anthropic.com did not resolve") {
		t.Fatalf("stop reason: %q", detail)
	}
}

// TestTransientProviderResumeInterruptedDuringBackoffIsNotExhausted: an interrupt during
// the wait stops the stage with its own reason, never as provider-unavailable.
func TestTransientProviderResumeInterruptedDuringBackoffIsNotExhausted(t *testing.T) {
	t.Parallel()
	o, calls, _ := transientResumeFixture(t, 3, transientFailure, nil)
	r, err := Run(context.Background(), o)
	if err != nil || r.Readiness == nil {
		t.Fatalf("fixture not ready: %v", err)
	}
	o.Approve = r.Readiness.Plans[0].Digest
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.TransientBackoff = func(ctx context.Context, _ int) error {
		cancel()
		return ctx.Err()
	}
	r, err = Run(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "failed" || len(r.HarnessAttempts) != 1 || callCount(t, calls) != 1 || r.HarnessAttempts[0].Classification != harness.TransientProviderError {
		t.Fatalf("status=%s attempts=%d calls=%d", r.Status, len(r.HarnessAttempts), callCount(t, calls))
	}
	if TransientRetriesExhausted(r) || len(r.TransientExhausted) != 0 {
		t.Fatalf("an interrupted wait reads as provider-unavailable: %v", r.TransientExhausted)
	}
	if detail := r.Atoms[2].Detail; !strings.Contains(detail, "Automatic resume 1 of 2 after transient-provider-error was interrupted before it started") {
		t.Fatalf("stop reason: %q", detail)
	}
}

// TestTransientProviderResumeStopsWhenTheFormulaChanged: a Formula reactivated during the
// wait stops the stage before the resume starts, with a clear reason, and is not
// provider-unavailable.
func TestTransientProviderResumeStopsWhenTheFormulaChanged(t *testing.T) {
	t.Parallel()
	o, calls, _ := transientResumeFixture(t, 3, transientFailure, nil)
	raw, err := os.ReadFile(filepath.Join(o.LabDir, "formulas", "sample.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	o.ResolveProvider = func(context.Context) error {
		approveFixture(t, o.LabDir, string(raw), "reactivated-during-backoff")
		return nil
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "failed" || len(r.HarnessAttempts) != 1 || callCount(t, calls) != 1 {
		t.Fatalf("status=%s attempts=%d calls=%d", r.Status, len(r.HarnessAttempts), callCount(t, calls))
	}
	if TransientRetriesExhausted(r) {
		t.Fatal("a Formula change reads as provider-unavailable")
	}
	if detail := r.Atoms[2].Detail; !strings.Contains(detail, "Automatic resume 1 of 2 after transient-provider-error was not started: the Formula changed since approval") {
		t.Fatalf("stop reason: %q", detail)
	}
}

// TestTransientRetriesExhaustedReadsOnlyTheMarker: a ledger whose last attempt is
// transient but that carries no marker (every other stop: extensions, capacity, token or
// save errors, interrupts) is not exhausted, and a marker followed by a later attempt is
// stale.
func TestTransientRetriesExhaustedReadsOnlyTheMarker(t *testing.T) {
	t.Parallel()
	transient := []HarnessAttempt{{ID: "a1", Classification: harness.TransientProviderError}, {ID: "a2", Classification: harness.TransientProviderError, Resumed: "resumed after transient-provider-error (1 of 2)"}}
	if TransientRetriesExhausted(Record{HarnessAttempts: transient}) {
		t.Fatal("a transient last attempt without the marker reads as exhausted")
	}
	if !TransientRetriesExhausted(Record{HarnessAttempts: transient, TransientExhausted: []string{"a1", "a2"}}) {
		t.Fatal("the marker is not read")
	}
	later := append(slices.Clone(transient), HarnessAttempt{ID: "a3"})
	if TransientRetriesExhausted(Record{HarnessAttempts: later, TransientExhausted: []string{"a1", "a2"}}) {
		t.Fatal("a stale marker reads as exhausted")
	}
	if TransientRetriesExhausted(Record{TransientExhausted: []string{"a1"}}) {
		t.Fatal("a marker without attempts reads as exhausted")
	}
}
