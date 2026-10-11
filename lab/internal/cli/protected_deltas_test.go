package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

const (
	gateFixture = "lab/internal/harness/testdata/contracts.json"
	gatePackage = "github.com/owner/repo/lab/internal/harness"
	gateTest    = "TestAdapterCompatibility/claude-code/molecule"
)

func gatePlan(deltas ...readiness.ProtectedDelta) readiness.Plan {
	return readiness.Plan{Analysis: readiness.Analysis{Scope: []readiness.ScopedPath{{Path: gateFixture, Reason: "Pinned contract fixture"}}, Checks: []readiness.Check{{ID: "go-test-lab", Category: "regression", Argv: []string{"go", "test", "-json", "./..."}, Dir: "lab", Required: true}}}, ExpectedProtectedDeltas: deltas}
}

func gateDelta() readiness.ProtectedDelta {
	return readiness.ProtectedDelta{CheckID: "go-test-lab", Package: gatePackage, Test: gateTest, ScopePath: gateFixture, Reason: "The approved change alters the pinned argv"}
}

func gateRecord(p readiness.Plan, phase string) molecule.Record {
	return molecule.Record{ID: "mol-1", Readiness: &readiness.State{Phase: phase, Plans: []readiness.Proposal{{Plan: p, Digest: "digest-1"}}}}
}

func TestProtectedDeltaScopeGateListsDeclaredDeltas(t *testing.T) {
	var out bytes.Buffer
	presentRun(&out, gateRecord(gatePlan(gateDelta()), "scope-waiting"))
	for _, want := range []string{"EXPECTED PROTECTED-TEST FAILURES", "approving this exact digest accepts", gatePackage + " " + gateTest, "check go-test-lab", "justified by scope path " + gateFixture, "The approved change alters the pinned argv"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("scope gate missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "never auto-approved") {
		t.Fatalf("advisory shown although a delta is declared:\n%s", out.String())
	}
}

func TestProtectedDeltaScopeGateShowsLabelledAdvisory(t *testing.T) {
	var out bytes.Buffer
	presentRun(&out, gateRecord(gatePlan(), "scope-waiting"))
	if !strings.Contains(out.String(), "proposed for review; never auto-approved") || !strings.Contains(out.String(), gateFixture) {
		t.Fatalf("missing advisory in:\n%s", out.String())
	}
	if strings.Contains(out.String(), "EXPECTED PROTECTED-TEST FAILURES") {
		t.Fatalf("advisory was rendered as a declaration:\n%s", out.String())
	}
}

func TestProtectedDeltaReviewListsAcceptedDeltasAndWarnings(t *testing.T) {
	r := gateRecord(gatePlan(gateDelta()), "review-waiting")
	r.Verification = &verification.Result{RequiredChecksPassed: true, AcceptedDeltas: []readiness.ProtectedDelta{gateDelta()}, Warnings: []string{"declared protected delta did not fail: " + gatePackage + " TestOther (check go-test-lab)"}}
	var out bytes.Buffer
	presentRun(&out, r)
	for _, want := range []string{"Required checks passed: true", "ACCEPTED PROTECTED DELTAS", gatePackage + " " + gateTest, "justified by scope path " + gateFixture, "Warning: declared protected delta did not fail: " + gatePackage + " TestOther"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("review output missing %q in:\n%s", want, out.String())
		}
	}
}

func TestProtectedDeltaReviewWithoutDeltasIsUnchanged(t *testing.T) {
	r := gateRecord(readiness.Plan{}, "review-waiting")
	r.Verification = &verification.Result{RequiredChecksPassed: true}
	var out bytes.Buffer
	presentRun(&out, r)
	for _, unwanted := range []string{"ACCEPTED PROTECTED DELTAS", "Warning:", "EXPECTED PROTECTED-TEST FAILURES", "never auto-approved"} {
		if strings.Contains(out.String(), unwanted) {
			t.Fatalf("unexpected %q in:\n%s", unwanted, out.String())
		}
	}
}
