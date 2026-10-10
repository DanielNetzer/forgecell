package molecule

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const (
	slowCompletingHarness = "#!/bin/sh\ncat >/dev/null\nsleep 1\nprintf changed > code.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Implemented and tested\",\"paths\":[]}'\n"
	stuckHarness          = "#!/bin/sh\ncat >/dev/null\nprintf changed > code.txt\nsleep 10\n"
)

// timeoutFixture approves a Formula with the given timeouts and a harness script,
// and widens the fixture scope so intake sees a larger ticket.
func timeoutFixture(t *testing.T, timeoutMS, maxMS, extraScope int, script string) Options {
	t.Helper()
	o, _ := fixture(t)
	raw, err := os.ReadFile(filepath.Join(o.LabDir, "formulas", "sample.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fields := ""
	if timeoutMS > 0 {
		fields += fmt.Sprintf("  timeoutMs: %d\n", timeoutMS)
	}
	if maxMS > 0 {
		fields += fmt.Sprintf("  maxTimeoutMs: %d\n", maxMS)
	}
	approveFixture(t, o.LabDir, strings.Replace(string(raw), "harness:\n", "harness:\n"+fields, 1), "timeout-fixture")
	if err = os.WriteFile(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	base := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, err := base(ctx, b, i, s)
		for n := 0; n < extraScope; n++ {
			a.Scope = append(a.Scope, readiness.ScopedPath{Path: fmt.Sprintf("extra%d.txt", n), Reason: "Additional scope", Evidence: a.Scope[1].Evidence})
		}
		return a, err
	}
	return o
}

func codingDetail(t *testing.T, r Record) string {
	t.Helper()
	for _, a := range r.Atoms {
		if a.Type == "harness" {
			return a.Detail
		}
	}
	t.Fatal("no harness atom")
	return ""
}

func TestHarnessTimeoutProposalIsDeterministicAndBounded(t *testing.T) {
	granted := formula.Binding{TimeoutMS: 900000, MaxTimeoutMS: 1800000}
	for _, tc := range []struct {
		name    string
		binding formula.Binding
		paths   int
		want    int
	}{
		{"small scope keeps the Formula timeout", granted, 8, 0},
		{"one path over the threshold", granted, 9, 1_020_000},
		{"thirteen files as in the 2026-10-10 timeout", granted, 13, 1_500_000},
		{"huge scope is clamped to the maximum", granted, 200, 1_800_000},
		{"no maximum grants no extension", formula.Binding{TimeoutMS: 900000}, 13, 0},
		{"maximum equal to the timeout grants no extension", formula.Binding{TimeoutMS: 900000, MaxTimeoutMS: 900000}, 13, 0},
		{"unset Formula timeout is never extended", formula.Binding{}, 13, 0},
		{"rounding cannot exceed the maximum", formula.Binding{TimeoutMS: 300, MaxTimeoutMS: 30000}, 9, 30000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := proposeHarnessTimeoutMS(tc.binding, tc.paths)
			if got != tc.want {
				t.Fatalf("proposeHarnessTimeoutMS(%+v, %d) = %d, want %d", tc.binding, tc.paths, got, tc.want)
			}
			if got != 0 && (got < tc.binding.TimeoutMS || got > tc.binding.TimeoutCeilingMS()) {
				t.Fatalf("proposal %d escapes [%d, %d]", got, tc.binding.TimeoutMS, tc.binding.TimeoutCeilingMS())
			}
			if again := proposeHarnessTimeoutMS(tc.binding, tc.paths); again != got {
				t.Fatal("proposal is not deterministic")
			}
		})
	}
}

func TestHarnessTimeoutIntakeProposesFromScopeAndOmitsItOtherwise(t *testing.T) {
	large := timeoutFixture(t, 900000, 1800000, 11, stuckHarness)
	r, err := Run(context.Background(), large)
	if err != nil {
		t.Fatal(err)
	}
	plan := r.Readiness.Plans[0].Plan
	if len(plan.Analysis.Scope) != 13 || plan.HarnessTimeoutMS != 1_500_000 {
		t.Fatalf("scope=%d proposal=%d", len(plan.Analysis.Scope), plan.HarnessTimeoutMS)
	}
	for name, o := range map[string]Options{
		"small scope":    timeoutFixture(t, 900000, 1800000, 0, stuckHarness),
		"no maximum":     timeoutFixture(t, 900000, 0, 11, stuckHarness),
		"legacy Formula": timeoutFixture(t, 0, 0, 11, stuckHarness),
	} {
		t.Run(name, func(t *testing.T) {
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if r.Readiness.Plans[0].Plan.HarnessTimeoutMS != 0 {
				t.Fatalf("unexpected proposal %d", r.Readiness.Plans[0].Plan.HarnessTimeoutMS)
			}
			raw, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "harnessTimeoutMs") {
				t.Fatal("an unextended plan must keep its earlier encoding")
			}
		})
	}
}

// The control run proves the harness needs more than the Formula timeout: the only
// difference between the two runs is the reviewed plan value.
func TestHarnessTimeoutPlanValueIsHonouredAndRecorded(t *testing.T) {
	control := timeoutFixture(t, 300, 30000, 0, slowCompletingHarness)
	r, err := runApprovedFixture(t, control)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness.Plans[0].Plan.HarnessTimeoutMS != 0 || len(r.HarnessAttempts) != 1 || !r.HarnessAttempts[0].Result.TimedOut || r.Status != "failed" {
		t.Fatalf("control did not time out at the Formula timeout: %s %+v", r.Status, r.HarnessAttempts)
	}
	o := timeoutFixture(t, 300, 30000, 9, slowCompletingHarness)
	r, err = runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	plan := r.Readiness.Plans[0].Plan
	if plan.HarnessTimeoutMS != 30000 {
		t.Fatalf("proposal %d", plan.HarnessTimeoutMS)
	}
	if len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].Result.TimedOut || !r.HarnessAttempts[0].Result.OK || r.Status != "waiting" || r.Verification == nil || !r.Verification.RequiredChecksPassed {
		t.Fatalf("the plan timeout was not honoured: %s %+v", r.Status, r.HarnessAttempts)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "harnessTimeoutMs 30000") {
		t.Fatalf("effective timeout not recorded: %v", r.Notes)
	}
	raw, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"harnessTimeoutMs": 30000`) {
		t.Fatal("reviewed timeout is not part of the immutable plan record")
	}
}

func TestHarnessTimeoutAboveMaximumIsRejectedBeforeCoding(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "coding-ran")
	script := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\necho ran >> %q\n", marker)
	o := timeoutFixture(t, 300, 30000, 0, script)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	parent := r.Readiness.Plans[0]

	t.Run("amend", func(t *testing.T) {
		next := copyPlan(t, parent.Plan)
		next.HarnessTimeoutMS = 30001
		_, err := Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
		if err == nil || !strings.Contains(err.Error(), "30001") || !strings.Contains(err.Error(), "30000") {
			t.Fatalf("over-maximum amendment must name both values: %v", err)
		}
		if _, err = findPending(o.LabDir, parent.Digest); err != nil {
			t.Fatalf("a rejected amendment must not persist a successor: %v", err)
		}
		next.HarnessTimeoutMS = 30000
		amended, err := Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, "")
		if err != nil {
			t.Fatalf("a timeout equal to the maximum is allowed: %v", err)
		}
		if got := amended.Readiness.Plans[1].Plan.HarnessTimeoutMS; got != 30000 {
			t.Fatalf("amended timeout %d", got)
		}
	})

	t.Run("approval", func(t *testing.T) {
		// A plan that is individually valid, but over this Formula's maximum, cannot
		// start coding even if the ledger was edited to carry it.
		tampered := r
		tampered.Readiness = nil
		raw, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &tampered); err != nil {
			t.Fatal(err)
		}
		tampered.Readiness.Plans = tampered.Readiness.Plans[:1]
		tampered.Readiness.Events = tampered.Readiness.Events[:1]
		plan := &tampered.Readiness.Plans[0]
		plan.Plan.HarnessTimeoutMS = 40000
		digest, err := plan.Plan.Digest()
		if err != nil {
			t.Fatal(err)
		}
		plan.Digest = digest
		tampered.Readiness.Events[0].Digest = digest
		tampered.Readiness.Phase = "scope-waiting"
		if err = save(tampered); err != nil {
			t.Fatal(err)
		}
		approve := o
		approve.Approve = digest
		if _, err = Run(context.Background(), approve); err == nil || !strings.Contains(err.Error(), "40000") || !strings.Contains(err.Error(), "30000") {
			t.Fatalf("approval must reject an over-maximum timeout naming both values: %v", err)
		}
		if _, err = os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("a coding process started under a rejected timeout")
		}
	})
}

func TestHarnessTimeoutGateMessageListsRecoverAndAmendCommands(t *testing.T) {
	for _, tc := range []struct {
		name      string
		maxMS     int
		extra     int
		wantMS    int
		wantAudit string
	}{
		{"formula timeout", 0, 0, 300, "Formula timeoutMs"},
		{"plan timeout", 600, 9, 600, "plan harnessTimeoutMs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := timeoutFixture(t, 300, tc.maxMS, tc.extra, stuckHarness)
			r, err := runApprovedFixture(t, o)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != "failed" || len(r.HarnessAttempts) != 1 || !r.HarnessAttempts[0].Result.TimedOut {
				t.Fatalf("not a timed-out attempt: %s %+v", r.Status, r.HarnessAttempts)
			}
			parent := r.Readiness.Plans[0]
			attempt := r.HarnessAttempts[0]
			detail := codingDetail(t, r)
			for _, want := range []string{
				"Harness timed out",
				fmt.Sprintf("timeoutMs=%d (%s)", tc.wantMS, tc.wantAudit),
				fmt.Sprintf("elapsedMs=%d", attempt.Result.ElapsedMS),
				"retained tree " + r.Capture.Tree,
				fmt.Sprintf("%d changed path(s)", len(r.Capture.Changes)),
				"violations: none",
				fmt.Sprintf("forgecell recover %s --plan %s --confirm-stopped --lab %q", r.ID, parent.Digest, r.LabDir),
				fmt.Sprintf("forgecell ledger %s --lab %q", r.ID, r.LabDir),
				`"continuation":"adopt-failed-tree"`,
				fmt.Sprintf(`"adoptAttempt":%q`, attempt.ID),
				fmt.Sprintf("forgecell amend %s --parent %s --plan FILE --lab %q", r.ID, parent.Digest, r.LabDir),
				attempt.Evidence.Path,
			} {
				if !strings.Contains(detail, want) {
					t.Fatalf("gate message lacks %q:\n%s", want, detail)
				}
			}
			if !strings.Contains(strings.Join(r.Notes, "\n"), "forgecell recover") {
				t.Fatal("the same guidance must reach the ledger notes")
			}
			// The advertised path must actually work for this attempt.
			if r, err = Recover(context.Background(), o.LabDir, r.ID, parent.Digest, true); err != nil {
				t.Fatalf("recover: %v", err)
			}
			next := copyPlan(t, parent.Plan)
			next.Continuation = "adopt-failed-tree"
			next.AdoptAttempt = attempt.ID
			if _, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, next, ""); err != nil {
				t.Fatalf("amend: %v", err)
			}
		})
	}
}

func TestHarnessTimeoutNonTimeoutFailureMessageIsUnchanged(t *testing.T) {
	o, _ := fixture(t)
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf failed >&2\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	detail := codingDetail(t, r)
	if !strings.HasPrefix(detail, "Harness failed: exit=7 ") || strings.Contains(detail, "forgecell recover") || strings.Contains(detail, "timed out") {
		t.Fatalf("%s", detail)
	}
}
