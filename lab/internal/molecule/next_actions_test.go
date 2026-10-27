package molecule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

func nextKinds(actions []NextAction) []string {
	out := []string{}
	for _, a := range actions {
		out = append(out, a.Kind)
	}
	return out
}

func nextAction(actions []NextAction, kind string) (NextAction, bool) {
	for _, a := range actions {
		if a.Kind == kind {
			return a, true
		}
	}
	return NextAction{}, false
}

func approvedDigest(r Record, digest string) bool {
	for _, e := range r.Readiness.Events {
		if e.Kind == "approved" && e.Digest == digest {
			return true
		}
	}
	return false
}

// A by-design protected failure offers exactly accept-protected-failures and
// retire. The preview names the tests the engine then declares, the amend with
// the shown parent records them without approving, and the old digest is stale.
func TestNextActionsOfferAcceptProtectedFailuresWithTheDeclaredTests(t *testing.T) {
	o, r := acceptFixture(t, "by-design")
	actions := NextActions(context.Background(), o.LabDir, r, true, time.Minute)
	if got := nextKinds(actions); !reflect.DeepEqual(got, []string{NextAcceptProtectedFailures, NextRetire}) {
		t.Fatalf("actions %v", got)
	}
	a, _ := nextAction(actions, NextAcceptProtectedFailures)
	parent := latestPlan(r)
	if a.Digest != parent.Digest || len(a.Deltas) != 1 || a.Deltas[0].Package != acceptPackage || a.Deltas[0].Test != "TestX" || a.Deltas[0].ScopePath != acceptTest || a.Deltas[0].CheckID != acceptCheck {
		t.Fatalf("action %+v", a)
	}
	before := ledgerBytes(t, r)
	if again := NextActions(context.Background(), o.LabDir, r, true, time.Minute); !reflect.DeepEqual(again, actions) || string(before) != string(ledgerBytes(t, r)) {
		t.Fatal("deriving the actions changed the ledger or is not repeatable")
	}
	amended, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AcceptProtectedFailures: true, ExpectParent: a.Digest})
	if err != nil {
		t.Fatal(err)
	}
	got := latestPlan(amended)
	if got.Plan.ParentDigest != a.Digest || !reflect.DeepEqual(got.Plan.ExpectedProtectedDeltas, a.Deltas) || approvedDigest(amended, got.Digest) || amended.Readiness.Phase != "scope-waiting" {
		t.Fatalf("revision %+v deltas %+v", got.Plan.ParentDigest, got.Plan.ExpectedProtectedDeltas)
	}
	// The new revision waits for its own approval: nothing more is offered but retire.
	if got := nextKinds(NextActions(context.Background(), o.LabDir, amended, true, time.Minute)); !reflect.DeepEqual(got, []string{NextRetire}) {
		t.Fatalf("after the amend: %v", got)
	}
	if _, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AcceptProtectedFailures: true, ExpectParent: a.Digest}); !errors.Is(err, ErrStalePlan) {
		t.Fatalf("stale parent: %v", err)
	}
}

// A protected failure the engine would refuse to declare is never offered, and
// neither is a re-run of checks for a check that failed on its own code.
func TestNextActionsLeaveOutAnAcceptanceTheEngineRefuses(t *testing.T) {
	for _, mode := range []string{"candidate-fails", "other-package", "panic", "pass"} {
		t.Run(mode, func(t *testing.T) {
			o, r := acceptFixture(t, mode)
			if got := nextKinds(NextActions(context.Background(), o.LabDir, r, true, time.Minute)); !reflect.DeepEqual(got, []string{NextRetire}) {
				t.Fatalf("actions %v", got)
			}
			if _, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AcceptProtectedFailures: true}); err == nil {
				t.Fatal("the engine accepted what was not offered")
			}
		})
	}
}

// A coding scope-change request offers exactly its paths and reason; the --add-scope
// amend with the shown parent records them and never approves. Invalid,
// too many or outdated requests are not offered.
func TestNextActionsScopeChangeOffersTheRequestedPaths(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	writeStub(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte("#!/bin/sh\ncat >/dev/null\nprintf partial > code.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"scope-change\",\"reason\":\"helper needed\",\"paths\":[\"helper.go\"]}'\n"), 0700)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if statePhase(r) != "scope-change" {
		t.Fatalf("phase %s", statePhase(r))
	}
	actions := NextActions(context.Background(), o.LabDir, r, true, time.Minute)
	if got := nextKinds(actions); !reflect.DeepEqual(got, []string{NextApproveScopeChange, NextRetire}) {
		t.Fatalf("actions %v", got)
	}
	a, _ := nextAction(actions, NextApproveScopeChange)
	if a.Digest != latestPlan(r).Digest || !reflect.DeepEqual(a.Paths, []string{"helper.go"}) || a.Reason != "helper needed" {
		t.Fatalf("action %+v", a)
	}
	for name, mutate := range map[string]func(*HarnessAttempt){
		"traversal":     func(h *HarnessAttempt) { h.Coding.Paths = []string{"../x.go"} },
		"duplicate":     func(h *HarnessAttempt) { h.Coding.Paths = []string{"a.go", "a.go"} },
		"too many":      func(h *HarnessAttempt) { h.Coding.Paths = strings.Split("a b c d e f g h i", " ") },
		"no paths":      func(h *HarnessAttempt) { h.Coding.Paths = nil },
		"older plan":    func(h *HarnessAttempt) { h.PlanDigest = strings.Repeat("0", 64) },
		"not requested": func(h *HarnessAttempt) { h.Coding.Outcome = "blocked" },
	} {
		changed := r
		changed.HarnessAttempts = append([]HarnessAttempt(nil), r.HarnessAttempts...)
		coding := *changed.HarnessAttempts[0].Coding
		changed.HarnessAttempts[0].Coding = &coding
		mutate(&changed.HarnessAttempts[0])
		if _, ok := nextAction(NextActions(context.Background(), o.LabDir, changed, true, time.Minute), NextApproveScopeChange); ok {
			t.Errorf("%s: scope change offered", name)
		}
	}
	amended, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AddScope: []ScopeAddition{{Path: "helper.go", Reason: a.Reason}}, ExpectParent: a.Digest})
	if err != nil {
		t.Fatal(err)
	}
	got := latestPlan(amended)
	scope := got.Plan.Analysis.Scope
	if got.Plan.ParentDigest != a.Digest || scope[len(scope)-1].Path != "helper.go" || approvedDigest(amended, got.Digest) || amended.Readiness.Phase != "scope-waiting" {
		t.Fatalf("revision parent %s scope %+v phase %s", got.Plan.ParentDigest, scope, amended.Readiness.Phase)
	}
	if _, ok := nextAction(NextActions(context.Background(), o.LabDir, amended, true, time.Minute), NextApproveScopeChange); ok {
		t.Fatal("the scope change is still offered after its revision")
	}
}

// A Molecule stopped below the soft resource floor before its checks started is
// offered a re-run of checks after recovery, as forgecell recover --confirm-stopped
// then amend --continuation checks-only do; the checks-only revision runs the
// checks on the same code without coding again.
func TestNextActionsRerunChecksFromAResourceRefusedCheckPendingMolecule(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	low := resources.Readings{Volumes: []resources.Volume{{Path: "/lab", Free: resources.GiB, Total: 100 * resources.GiB}}, Memory: 8 * resources.GiB, MemoryKnown: true}
	o.Resources = &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings { return low }), Sleep: func(context.Context, time.Duration) bool { return true }}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "blocked" || statePhase(r) != "check-pending" || len(r.VerificationAttempts) != 0 {
		t.Fatalf("fixture %s %s", r.Status, statePhase(r))
	}
	actions := NextActions(context.Background(), o.LabDir, r, true, time.Minute)
	if got := nextKinds(actions); !reflect.DeepEqual(got, []string{NextRerunChecks, NextRetire}) {
		t.Fatalf("actions %v", got)
	}
	a, _ := nextAction(actions, NextRerunChecks)
	if !a.RecoverFirst || a.Digest != latestPlan(r).Digest {
		t.Fatalf("action %+v", a)
	}
	// The checks-only amend alone is refused here; recovery comes first.
	if _, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Continuation: "checks-only", ExpectParent: a.Digest}); err == nil {
		t.Fatal("checks-only was accepted before recovery")
	}
	recovered, err := Recover(context.Background(), o.LabDir, r.ID, a.Digest, true)
	if err != nil {
		t.Fatal(err)
	}
	after := NextActions(context.Background(), o.LabDir, recovered, true, time.Minute)
	if b, ok := nextAction(after, NextRerunChecks); !ok || b.RecoverFirst || b.Digest != a.Digest {
		t.Fatalf("after recovery %+v", after)
	}
	amended, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{Continuation: "checks-only", ExpectParent: a.Digest})
	if err != nil {
		t.Fatal(err)
	}
	got := latestPlan(amended)
	if got.Plan.Continuation != "checks-only" || got.Plan.ParentDigest != a.Digest || approvedDigest(amended, got.Digest) {
		t.Fatalf("revision %+v", got.Plan)
	}
	o.Resources, o.Approve = nil, got.Digest
	done, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if done.Verification == nil || !done.Verification.RequiredChecksPassed || len(done.HarnessAttempts) != 1 {
		t.Fatalf("checks-only run: %s %+v attempts %d", statePhase(done), done.Verification, len(done.HarnessAttempts))
	}
}

// An abandoned running Molecule is offered recovery only, and only as the loop's
// recovery proposal would name it with evidence Recover accepts. A ledger the
// loop names but whose harness evidence Recover refuses, one with no
// reconciled attempt, and one written within the timeout are offered nothing.
func TestNextActionsRecoverOnlyForAnAbandonedMolecule(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	r.Status = "running"
	if err = save(r); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(filepath.Join(o.LabDir, "ledgers", r.ID+".json"), old, old); err != nil {
		t.Fatal(err)
	}
	actions := NextActions(context.Background(), o.LabDir, r, true, triggersTimeout)
	if got := nextKinds(actions); !reflect.DeepEqual(got, []string{NextRecover}) || actions[0].Digest != latestPlan(r).Digest {
		t.Fatalf("actions %+v", actions)
	}
	// A ledger written within the timeout may still be live: nothing is offered.
	if got := NextActions(context.Background(), o.LabDir, r, true, 48*time.Hour); len(got) != 0 {
		t.Fatalf("a possibly live Molecule offered %v", nextKinds(got))
	}
	for _, attempt := range []string{"tree", "unreconciled", ""} {
		t.Run(attempt, func(t *testing.T) {
			lo, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
			crashed := triggersInterrupted(t, lo, attempt)
			if got := NextActions(context.Background(), lo.LabDir, crashed, true, triggersTimeout); len(got) != 0 {
				t.Fatalf("actions %v", nextKinds(got))
			}
			if _, err := Recover(context.Background(), lo.LabDir, crashed.ID, latestPlan(crashed).Digest, true); err == nil {
				t.Fatal("Recover accepted what was not offered")
			}
		})
	}
}

// A stopped intake offers a fresh intake with its bounded open questions; a
// retired Molecule offers nothing, and a held delivery lock or an unpublished
// receipt keeps retire out.
func TestNextActionsFreshIntakeAndRetire(t *testing.T) {
	t.Parallel()
	lab := resolvedTempDir(t)
	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	questions := []readiness.Question{}
	for i := 0; i < maxNextActionQuestions+2; i++ {
		questions = append(questions, readiness.Question{Question: "Which flag?"})
	}
	r := Record{ID: "mol-12-abc", Status: "blocked", Issue: Issue{Number: 12, Repo: "acme/widgets"}, Analysis: &readiness.Analysis{Questions: questions}}
	actions := NextActions(context.Background(), lab, r, true, time.Minute)
	if got := nextKinds(actions); !reflect.DeepEqual(got, []string{NextFreshIntake, NextRetire}) {
		t.Fatalf("actions %v", got)
	}
	if len(actions[0].Questions) != maxNextActionQuestions || actions[0].Digest != "" {
		t.Fatalf("fresh intake %+v", actions[0])
	}
	exhausted := r
	exhausted.Analysis, exhausted.Notes = nil, []string{"Supplemental evidence passes exhausted after 2 passes."}
	if got := nextKinds(NextActions(context.Background(), lab, exhausted, true, time.Minute)); !reflect.DeepEqual(got, []string{NextFreshIntake, NextRetire}) {
		t.Fatalf("exhausted: %v", got)
	}
	other := exhausted
	other.Notes = []string{"Something else"}
	if got := nextKinds(NextActions(context.Background(), lab, other, true, time.Minute)); !reflect.DeepEqual(got, []string{NextRetire}) {
		t.Fatalf("other block: %v", got)
	}
	retired := r
	retired.Status, retired.Retired = StatusRetired, &Retirement{At: "2026-10-26T10:00:00Z", Reason: "stale", PriorStatus: "blocked"}
	if got := NextActions(context.Background(), lab, retired, true, time.Minute); len(got) != 0 {
		t.Fatalf("retired: %v", nextKinds(got))
	}
	if RetireDeliveryInProgress(lab, r.ID) {
		t.Fatal("no delivery, yet in progress")
	}
	deliveries := filepath.Join(lab, "deliveries")
	if err := os.MkdirAll(deliveries, 0700); err != nil {
		t.Fatal(err)
	}
	for state, inProgress := range map[string]bool{"published": false, "prepared": true, "committed": true, "bogus": true} {
		if err := os.WriteFile(filepath.Join(deliveries, r.ID+".json"), []byte(`{"state":"`+state+`"}`), 0600); err != nil {
			t.Fatal(err)
		}
		_, offered := nextAction(NextActions(context.Background(), lab, r, true, time.Minute), NextRetire)
		if RetireDeliveryInProgress(lab, r.ID) != inProgress || offered == inProgress {
			t.Errorf("receipt %s: in progress %t, retire offered %t", state, RetireDeliveryInProgress(lab, r.ID), offered)
		}
	}
	if err := os.Remove(filepath.Join(deliveries, r.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(lab, ".delivery-lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, offered := nextAction(NextActions(context.Background(), lab, r, true, time.Minute), NextRetire); offered || !RetireDeliveryInProgress(lab, r.ID) {
		t.Fatal("retire offered under the Lab delivery lock")
	}
	if !RetireDeliveryInProgress(lab, "../x") {
		t.Fatal("an unsafe id is not refused")
	}
}
