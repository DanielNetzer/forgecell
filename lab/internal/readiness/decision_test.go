package readiness

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestApprovalIsExactAndSingleUse(t *testing.T) {
	p := fixture()
	state, err := NewState(p, "now")
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := p.Digest()
	if _, err = state.Approve("wrong", p.Inputs, "next"); err == nil {
		t.Fatal("wrong approval accepted")
	}
	stale := p.Inputs
	stale.Issue.Body += " changed"
	if _, err = state.Approve(digest, stale, "next"); err == nil {
		t.Fatal("stale issue approved")
	}
	approved, err := state.Approve(digest, p.Inputs, "next")
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "scope-waiting" || len(state.Events) != 1 {
		t.Fatal("original state mutated")
	}
	if approved.Phase != "approved" || len(approved.Events) != 2 {
		t.Fatalf("%+v", approved)
	}
	running, err := approved.StartAttempt("start")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = running.StartAttempt("again"); err == nil {
		t.Fatal("uncertain coding attempt automatically repeated")
	}
	if _, err = running.Approve(digest, p.Inputs, "again"); err == nil {
		t.Fatal("running plan reapproved")
	}
	stopped, err := running.FinishAttempt("failed", "timeout", "end")
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped.Events) != 4 || stopped.Phase != "failed" {
		t.Fatalf("%+v", stopped)
	}
}
func TestAmendmentRetainsWorkAndHistory(t *testing.T) {
	p := fixture()
	s, _ := NewState(p, "now")
	d, _ := p.Digest()
	s, _ = s.Approve(d, p.Inputs, "approved")
	s, _ = s.StartAttempt("start")
	s, err := s.FinishAttempt("scope-change", "Need shared helper", "stop")
	if err != nil {
		t.Fatal(err)
	}
	next := fixture()
	next.Revision = 2
	next.ParentDigest = d
	next.PausedTree = next.Inputs.BaseCommit
	next.Analysis.Scope = append(next.Analysis.Scope, ScopedPath{Path: "lab/helper.go", Reason: "Shared helper", Evidence: []string{"manifest"}})
	amended, err := s.Amend(next, "amended")
	if err != nil {
		t.Fatal(err)
	}
	if len(amended.Plans) != 2 || len(amended.Events) != 5 || amended.Phase != "scope-waiting" {
		t.Fatalf("%+v", amended)
	}
	if _, err = amended.Approve(d, p.Inputs, "wrong"); err == nil {
		t.Fatal("parent approval resumed amendment")
	}
	nd, _ := next.Digest()
	denied, err := amended.Dismiss(nd, "keep work", "denied")
	if err != nil {
		t.Fatal(err)
	}
	if denied.Phase != "dismissed" || len(denied.Plans) != 2 || len(denied.Events) != 6 {
		t.Fatalf("%+v", denied)
	}
	if _, err = denied.StartAttempt("oops"); err == nil {
		t.Fatal("denied plan executed")
	}
	drift := next
	drift.Inputs.Issue.Body = "different issue"
	if _, err = s.Amend(drift, "bad"); err == nil {
		t.Fatal("amendment hid changed intake")
	}
}
func TestStateRejectsCorruptedPlan(t *testing.T) {
	p := fixture()
	s, _ := NewState(p, "now")
	d, _ := p.Digest()
	s.Plans[0].Plan.Analysis.Scope[0].Path = "other.go"
	if _, err := s.Approve(d, p.Inputs, "next"); err == nil {
		t.Fatal("corrupted proposal approved")
	}
}

func TestRepeatedPendingChecksOnlyAmendmentsRequireFreshExactDecision(t *testing.T) {
	p := fixture()
	s, err := NewState(p, "proposed")
	if err != nil {
		t.Fatal(err)
	}
	d := s.Plans[0].Digest
	s, err = s.Approve(d, p.Inputs, "human approved coding")
	if err != nil {
		t.Fatal(err)
	}
	s, err = s.StartAttempt("coding")
	if err != nil {
		t.Fatal(err)
	}
	s, err = s.FinishAttempt("check-pending", "captured", "captured")
	if err != nil {
		t.Fatal(err)
	}
	s, err = s.RecordVerification(false, "failed check", "verified")
	if err != nil {
		t.Fatal(err)
	}
	for revision := 2; revision <= 4; revision++ {
		before, _ := json.Marshal(s)
		parent := s.Plans[len(s.Plans)-1]
		next := parent.Plan
		next.Continuation = "checks-only"
		next.Revision = revision
		next.ParentDigest = parent.Digest
		next.PausedTree = p.Inputs.BaseCommit
		next.Analysis.Summary = "Corrected check proposal " + string(rune('0'+revision))
		stale := next
		stale.Inputs.FormulaSHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		if _, err = s.Amend(stale, "bad"); err == nil {
			t.Fatal("amendment changed frozen Formula")
		}
		amended, err := s.Amend(next, "human proposed correction")
		if err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(s)
		if !bytes.Equal(before, after) {
			t.Fatal("parent state mutated")
		}
		plans, _ := json.Marshal(s.Plans)
		retainedPlans, _ := json.Marshal(amended.Plans[:revision-1])
		events, _ := json.Marshal(s.Events)
		retainedEvents, _ := json.Marshal(amended.Events[:len(s.Events)])
		if !bytes.Equal(plans, retainedPlans) || !bytes.Equal(events, retainedEvents) {
			t.Fatal("historical plans or exact human decisions changed")
		}
		s = amended
		if _, err = s.StartChecks("unapproved"); err == nil {
			t.Fatal("old approval started successor checks")
		}
		if _, err = s.StartAttempt("unapproved"); err == nil {
			t.Fatal("old approval started coding")
		}
		for _, old := range s.Plans[:revision-1] {
			if _, err = s.Approve(old.Digest, p.Inputs, "obsolete approval"); err == nil {
				t.Fatal("obsolete digest approved")
			}
		}
	}
	latest := s.Plans[len(s.Plans)-1]
	changed := p.Inputs
	changed.Issue.Body += " drift"
	if _, err = s.Approve(latest.Digest, changed, "stale inputs"); err == nil {
		t.Fatal("stale exact approval accepted")
	}
	s, err = s.Approve(latest.Digest, p.Inputs, "human approved latest exact digest")
	if err != nil {
		t.Fatal(err)
	}
	s, err = s.StartChecks("checks")
	if err != nil {
		t.Fatal(err)
	}
	s, err = s.RecordVerification(true, "passed", "verified")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ValidateForDelivery(); err != nil {
		t.Fatal(err)
	}
}

func TestNonCompletionConsumesApprovalAndCannotVerify(t *testing.T) {
	for _, outcome := range []string{"blocked", "unknown"} {
		p := fixture()
		s, _ := NewState(p, "proposed")
		d, _ := p.Digest()
		s, _ = s.Approve(d, p.Inputs, "approved")
		s, _ = s.StartAttempt("start")
		s, err := s.FinishAttempt(outcome, "cache denied", "stop")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.RecordVerification(true, "passed", "check"); err == nil {
			t.Fatal("non-completion verified")
		}
		if _, err = s.StartAttempt("retry"); err == nil {
			t.Fatal("approval automatically reused")
		}
		if _, err = s.ValidateForDelivery(); err == nil {
			t.Fatal("non-completion delivered")
		}
	}
}

// amendedCoding returns a state whose revision-two plan, paused on tree, is coding.
func amendedCoding(t *testing.T, tree string) (State, Plan) {
	t.Helper()
	p := fixture()
	s, _ := NewState(p, "proposed")
	d, _ := p.Digest()
	s, _ = s.Approve(d, p.Inputs, "approved")
	s, _ = s.StartAttempt("start")
	s, _ = s.FinishAttempt("check-pending", "captured", "captured")
	s, err := s.RecordVerification(true, "passed", "verified")
	if err != nil {
		t.Fatal(err)
	}
	next := fixture()
	next.Revision, next.ParentDigest, next.PausedTree = 2, d, tree
	next.Analysis.Acceptance = append(next.Analysis.Acceptance, Criterion{Description: "Leading tokens return an error", Evidence: []string{"issue"}})
	if s, err = s.Amend(next, "amended"); err != nil {
		t.Fatal(err)
	}
	nd, _ := next.Digest()
	if s, err = s.Approve(nd, next.Inputs, "approved again"); err != nil {
		t.Fatal(err)
	}
	if s, err = s.StartAttempt("start again"); err != nil {
		t.Fatal(err)
	}
	return s, next
}

func TestRecordNoChangeWaitsForReviewAndRefusesDelivery(t *testing.T) {
	tree := strings.Repeat("f", 40)
	s, next := amendedCoding(t, tree)
	for name, other := range map[string]string{"another tree": strings.Repeat("e", 40), "no tree": ""} {
		if _, err := s.RecordNoChange(other, "nochange"); err == nil {
			t.Fatalf("%s: a tree other than the paused tree recorded no change", name)
		}
	}
	stopped, err := s.RecordNoChange(tree, "nochange")
	if err != nil {
		t.Fatal(err)
	}
	last := stopped.Events[len(stopped.Events)-1]
	if stopped.Phase != "review-waiting" || NoChangeAfterAmendment != "no-change-after-amendment" || last.Kind != NoChangeAfterAmendment || last.Tree != tree || !strings.Contains(last.Detail, tree) {
		t.Fatalf("no-change state %+v", stopped)
	}
	if s.Phase != "coding" {
		t.Fatal("original state mutated")
	}
	if _, err = stopped.ValidateForDelivery(); err == nil || err.Error() != "amendment produced no change; nothing was verified" {
		t.Fatalf("delivery of an unverified no-change amendment: %v", err)
	}
	if _, err = stopped.RecordVerification(true, "passed", "check"); err == nil {
		t.Fatal("a no-change state accepted verification")
	}
	if _, err = stopped.RecordNoChange(tree, "again"); err == nil {
		t.Fatal("no change recorded twice")
	}

	// A later amendment proceeds normally from review-waiting.
	nd, _ := next.Digest()
	third := fixture()
	third.Revision, third.ParentDigest, third.PausedTree = 3, nd, tree
	third.Analysis.Acceptance = append(append([]Criterion{}, next.Analysis.Acceptance...), Criterion{Description: "Empty input returns an error", Evidence: []string{"issue"}})
	amended, err := stopped.Amend(third, "amended again")
	if err != nil {
		t.Fatalf("an amendment after no change: %v", err)
	}
	td, _ := third.Digest()
	if amended, err = amended.Approve(td, third.Inputs, "approved"); err == nil {
		amended, err = amended.StartAttempt("start")
	}
	if err == nil {
		amended, err = amended.FinishAttempt("check-pending", "captured", "captured")
	}
	if err == nil {
		amended, err = amended.RecordVerification(true, "passed", "verified")
	}
	if err != nil {
		t.Fatal(err)
	}
	if delivered, err := amended.ValidateForDelivery(); err != nil || delivered.Revision != 3 {
		t.Fatalf("the later amendment must deliver normally: %v", err)
	}
}

func TestRecordNoChangeRequiresAnAmendedCodingAttempt(t *testing.T) {
	p := fixture()
	s, _ := NewState(p, "proposed")
	d, _ := p.Digest()
	s, _ = s.Approve(d, p.Inputs, "approved")
	if _, err := s.RecordNoChange(p.Inputs.BaseCommit, "nochange"); err == nil {
		t.Fatal("no change recorded outside the coding phase")
	}
	s, _ = s.StartAttempt("start")
	if _, err := s.RecordNoChange(p.Inputs.BaseCommit, "nochange"); err == nil {
		t.Fatal("no change recorded for revision one")
	}
	tree := strings.Repeat("f", 40)
	amended, _ := amendedCoding(t, tree)
	pending, err := amended.FinishAttempt("check-pending", "captured", "captured")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pending.RecordNoChange(tree, "nochange"); err == nil {
		t.Fatal("no change recorded after coding finished")
	}
}
