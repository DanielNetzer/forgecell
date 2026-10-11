package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

func failedProtectedRun(tests ...verification.TestID) *verification.Result {
	return &verification.Result{Protected: &verification.Result{Checks: []verification.Observation{
		{ID: "go-test-lab", Category: "regression", Result: process.Result{OK: false}, FailedTests: tests},
		{ID: "go-vet-lab", Category: "regression", Result: process.Result{OK: true}, FailedTests: []verification.TestID{{Package: gatePackage, Test: "TestPassingCheckIsIgnored"}}},
	}}}
}

func TestScopeGateAdvisoryNamesTheRecordedFailingProtectedTests(t *testing.T) {
	r := gateRecord(gatePlan(), "scope-waiting")
	r.Verification = failedProtectedRun(verification.TestID{Package: gatePackage, Test: gateTest}, verification.TestID{Package: gatePackage, Test: "TestOther"})
	var out bytes.Buffer
	presentRun(&out, r)
	for _, want := range []string{"proposed for review; never auto-approved", gateFixture, gatePackage + " " + gateTest, gatePackage + " TestOther"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("scope gate missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "TestPassingCheckIsIgnored") || strings.Contains(out.String(), "EXPECTED PROTECTED-TEST FAILURES") {
		t.Fatalf("the advisory named a passing check or was rendered as a declaration:\n%s", out.String())
	}
}

func TestScopeGateAdvisoryNamesNoTestsWhenNoneWereRecorded(t *testing.T) {
	for name, verified := range map[string]*verification.Result{
		"no verification":      nil,
		"no protected run":     {},
		"no failing set saved": failedProtectedRun(),
	} {
		t.Run(name, func(t *testing.T) {
			r := gateRecord(gatePlan(), "scope-waiting")
			r.Verification = verified
			var out bytes.Buffer
			presentRun(&out, r)
			if !strings.Contains(out.String(), "never auto-approved") || strings.Contains(out.String(), "failed these tests") {
				t.Fatalf("unexpected advisory:\n%s", out.String())
			}
		})
	}
}

func TestScopeGateTellsTheReviewerAboutParentsAndRemovingPassingDeltas(t *testing.T) {
	var out bytes.Buffer
	presentRun(&out, gateRecord(gatePlan(gateDelta()), "scope-waiting"))
	for _, want := range []string{"parent of a declared subtest", "must be declared explicitly", "amend the plan to remove"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("scope gate missing %q in:\n%s", want, out.String())
		}
	}
}

func TestProposalMetaFollowsTheMetaRoleBinding(t *testing.T) {
	p := onboarding.Proposal{YAML: "kind: formula\nid: meta-role\nintake: {repo: acme/widgets}\nharness: {binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 1000}\nharnesses:\n  meta: {binding: codex, command: [/cli, __adapter, codex, /codex]}\natoms:\n  - {id: intake, type: intake}\n  - {id: harness, type: harness, binding: codex}\n  - {id: learn, type: learn}\n"}
	report := proposalMeta(p)
	if !reflect.DeepEqual(report.Command, []string{"/cli", "__adapter", "codex", "/codex"}) || report.Binding != "codex" || report.TimeoutMS != 1000 {
		t.Fatalf("a commandless learn Atom with harnesses.meta was reported as unbound: %+v", report)
	}
}
