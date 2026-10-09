package readiness

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
)

type Proposal struct {
	Plan   Plan   `json:"plan"`
	Digest string `json:"digest"`
}
type Event struct {
	Tree   string `json:"tree,omitempty"`
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	At     string `json:"at"`
	Detail string `json:"detail"`
}

// State is a ledger extension. Transitions return a deep copy, retaining earlier
// proposals and observations. The orchestrator must hold its exclusive Molecule
// lock and persist StartAttempt BEFORE invoking any coding process. A persisted
// coding state has an uncertain outcome until reconciled; it cannot auto-retry.
type State struct {
	SchemaVersion string     `json:"schemaVersion"`
	Phase         string     `json:"phase"`
	Plans         []Proposal `json:"plans"`
	Events        []Event    `json:"events"`
}

func cloneState(s State) State {
	raw, _ := json.Marshal(s)
	var out State
	_ = json.Unmarshal(raw, &out)
	return out
}
func NewState(p Plan, at string) (State, error) {
	digest, err := p.Digest()
	if err != nil {
		return State{}, err
	}
	if p.Revision != 1 {
		return State{}, fmt.Errorf("first proposal must be revision one")
	}
	s := State{SchemaVersion: "v1", Phase: "scope-waiting", Plans: []Proposal{{Plan: p, Digest: digest}}, Events: []Event{{Kind: "proposed", Digest: digest, At: at}}}
	return cloneState(s), nil
}
func (s State) current() (Proposal, error) {
	if s.SchemaVersion != "v1" || len(s.Plans) == 0 || len(s.Events) == 0 {
		return Proposal{}, fmt.Errorf("missing readiness history")
	}
	for i, p := range s.Plans {
		digest, err := p.Plan.Digest()
		if err != nil || digest != p.Digest || p.Plan.Revision != i+1 {
			return Proposal{}, fmt.Errorf("readiness proposal changed or is invalid")
		}
		if i > 0 && (p.Plan.ParentDigest != s.Plans[i-1].Digest || !sameInputs(p.Plan.Inputs, s.Plans[0].Plan.Inputs) || p.Plan.MoleculeID != s.Plans[0].Plan.MoleculeID) {
			return Proposal{}, fmt.Errorf("invalid amendment chain")
		}
	}
	return s.Plans[len(s.Plans)-1], nil
}
func sameInputs(a, b Inputs) bool {
	a.Issue.UpdatedAt = ""
	b.Issue.UpdatedAt = ""
	return reflect.DeepEqual(a, b)
}
func (s State) transition(kind, phase, detail, at string) (State, error) {
	p, err := s.current()
	if err != nil {
		return s, err
	}
	out := cloneState(s)
	out.Phase = phase
	out.Events = append(out.Events, Event{Kind: kind, Digest: p.Digest, At: at, Detail: detail})
	return out, nil
}
func (s State) Approve(digest string, current Inputs, at string) (State, error) {
	p, err := s.current()
	if err != nil {
		return s, err
	}
	if s.Phase != "scope-waiting" || digest != p.Digest {
		return s, fmt.Errorf("approval requires the exact pending plan digest")
	}
	if !sameInputs(p.Plan.Inputs, current) {
		return s, fmt.Errorf("readiness inputs changed; refresh intake before approval")
	}
	return s.transition("approved", "approved", "Scope approved; no publication authorized.", at)
}
func (s State) Dismiss(digest, reason, at string) (State, error) {
	p, err := s.current()
	if err != nil {
		return s, err
	}
	if s.Phase != "scope-waiting" || digest != p.Digest {
		return s, fmt.Errorf("dismissal requires the exact pending plan")
	}
	return s.transition("dismissed", "dismissed", reason, at)
}
func (s State) StartChecks(at string) (State, error) {
	p, err := s.current()
	if err != nil {
		return s, err
	}
	if s.Phase != "approved" || (p.Plan.Continuation != "checks-only" && p.Plan.Continuation != "adopt-failed-tree") {
		return s, fmt.Errorf("checks require an approved check-only plan")
	}
	if p.Plan.Continuation == "adopt-failed-tree" {
		adopted := false
		for _, e := range s.Events {
			if e.Digest == p.Digest && e.Kind == "failed-tree-adopted" && e.Tree == p.Plan.PausedTree && e.Detail == p.Plan.AdoptAttempt {
				adopted = true
			}
		}
		if !adopted {
			return s, fmt.Errorf("failed-tree continuation requires exact human adoption")
		}
	}
	return s.transition("checks-only-started", "check-pending", "Reuse retained coding output; no harness invocation.", at)
}

func (s State) StartAttempt(at string) (State, error) {
	p, err := s.current()
	if err != nil {
		return s, err
	}
	if p.Plan.Continuation != "" {
		return s, fmt.Errorf("retained-tree continuation cannot start coding")
	}
	if s.Phase != "approved" {
		return s, fmt.Errorf("coding requires an unused approval; uncertain attempts cannot automatically repeat")
	}
	return s.transition("attempt-started", "coding", "Persist this intent before invoking the harness.", at)
}
func (s State) FinishAttempt(outcome, detail, at string) (State, error) {
	if s.Phase != "coding" {
		return s, fmt.Errorf("no running attempt to finish")
	}
	switch outcome {
	case "check-pending", "scope-change", "scope-violation", "failed", "blocked", "unknown":
	default:
		return s, fmt.Errorf("unsupported coding outcome")
	}
	return s.transition("attempt-"+outcome, outcome, detail, at)
}
func (s State) Amend(p Plan, at string) (State, error) {
	previous, err := s.current()
	if err != nil {
		return s, err
	}
	// Violating writes are deliberately excluded: they need a separate exact-diff
	// review, not an amendment that retroactively grants permission.
	if s.Phase != "blocked" && s.Phase != "unknown" && s.Phase != "scope-change" && s.Phase != "dismissed" && s.Phase != "scope-waiting" && s.Phase != "failed" && s.Phase != "interrupted" && s.Phase != "review-waiting" {
		return s, fmt.Errorf("stop for a scope change before proposing an amendment")
	}
	if p.Revision != previous.Plan.Revision+1 || p.ParentDigest != previous.Digest || p.MoleculeID != previous.Plan.MoleculeID || !sameInputs(p.Inputs, previous.Plan.Inputs) {
		return s, fmt.Errorf("amendment must retain intake identity and bind the previous plan")
	}
	digest, err := p.Digest()
	if err != nil {
		return s, err
	}
	out := cloneState(s)
	out.Plans = append(out.Plans, Proposal{Plan: p, Digest: digest})
	out.Phase = "scope-waiting"
	out.Events = append(out.Events, Event{Kind: "amended", Digest: digest, At: at, Detail: "Retained work requires approval of the successor plan and paused tree."})
	return cloneState(out), nil
}

func (s State) RecordVerification(passed bool, detail, at string) (State, error) {
	if s.Phase != "check-pending" {
		return s, fmt.Errorf("verification requires a completed coding attempt")
	}
	phase := "failed"
	if passed {
		phase = "review-waiting"
	}
	return s.transition("verification-"+phase, phase, detail, at)
}

// ValidateForDelivery refuses missing, uncertain or unreviewed execution history.
func (s State) ValidateForDelivery() (Plan, error) {
	p, err := s.current()
	if err != nil {
		return Plan{}, err
	}
	if s.Phase != "review-waiting" {
		return Plan{}, fmt.Errorf("readiness has not reached final review")
	}
	approved, started, finished, verified := false, false, false, false
	adopted := false
	for _, e := range s.Events {
		if e.Kind == "attempt-scope-violation" {
			reviewed := false
			for _, decision := range s.Events {
				if decision.Kind == "existing-diff-reviewed" && decision.Digest == e.Digest && decision.Tree != "" && (e.Tree == "" || e.Tree == decision.Tree) {
					for _, proposal := range s.Plans {
						if proposal.Plan.ParentDigest == e.Digest && proposal.Plan.PausedTree == decision.Tree {
							reviewed = true
						}
					}
				}
			}
			if !reviewed {
				return Plan{}, fmt.Errorf("unresolved historical scope violation requires exact-diff review")
			}
		}
		if e.Digest != p.Digest {
			continue
		}
		switch e.Kind {
		case "failed-tree-adopted":
			if approved || e.Tree != p.Plan.PausedTree || e.Detail != p.Plan.AdoptAttempt {
				return Plan{}, fmt.Errorf("invalid adoption decision order or identity")
			}
			adopted = true
		case "approved":
			if p.Plan.Continuation == "adopt-failed-tree" && !adopted {
				return Plan{}, fmt.Errorf("failed-tree approval preceded human adoption")
			}
			approved = true
		case "attempt-started":
			if !approved {
				return Plan{}, fmt.Errorf("coding preceded approval")
			}
			started = true
		case "checks-only-started":
			if !approved || (p.Plan.Continuation != "checks-only" && p.Plan.Continuation != "adopt-failed-tree") {
				return Plan{}, fmt.Errorf("unapproved verification continuation")
			}
			started = true
			finished = true
		case "attempt-check-pending":
			if !started {
				return Plan{}, fmt.Errorf("missing coding attempt")
			}
			finished = true
		case "verification-review-waiting":
			if !finished {
				return Plan{}, fmt.Errorf("verification preceded coding")
			}
			verified = true
		case "attempt-scope-violation", "attempt-failed", "attempt-blocked", "attempt-unknown":
			return Plan{}, fmt.Errorf("unresolved execution violation")
		}
	}
	if !approved || !started || !finished || !verified {
		return Plan{}, fmt.Errorf("incomplete readiness decision history")
	}
	return p.Plan, nil
}

// Interrupted work is retained, never silently retried or called successful.
func (s State) Interrupt(digest, detail, at string) (State, error) {
	p, err := s.current()
	if err != nil {
		return s, err
	}
	if digest != p.Digest || (s.Phase != "scope-change" && s.Phase != "blocked" && s.Phase != "unknown" && s.Phase != "coding" && s.Phase != "check-pending" && s.Phase != "approved" && s.Phase != "failed" && s.Phase != "scope-violation") {
		return s, fmt.Errorf("only an unfinished approved attempt can be recovered")
	}
	return s.transition("interrupted", "interrupted", detail, at)
}

func (s State) ReviewExistingDiff(digest, tree, at string) (State, error) {
	p, err := s.current()
	if err != nil {
		return s, err
	}
	if p.Digest != digest || s.Phase != "scope-violation" || !regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`).MatchString(tree) {
		return s, fmt.Errorf("existing diff review requires the violated plan and exact captured tree")
	}
	out, err := s.transition("existing-diff-reviewed", "scope-change", "Human reviewed the exact existing violating diff; violation remains in history.", at)
	if err == nil {
		out.Events[len(out.Events)-1].Tree = tree
	}
	return out, err
}
