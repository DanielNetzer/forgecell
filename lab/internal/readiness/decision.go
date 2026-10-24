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
	// FormulaFrom, FormulaTo and Decision are set only on a FormulaRebased event:
	// the parent plan's and the rebased plan's Formula SHA-256, and the decision ID
	// that activated the new Formula. Absent keeps every earlier ledger unchanged.
	FormulaFrom string `json:"formulaFrom,omitempty"`
	FormulaTo   string `json:"formulaTo,omitempty"`
	Decision    string `json:"decision,omitempty"`
}

// FormulaRebased is the event kind of an amendment that carries a plan to a
// launcher-only rebind of its Formula. It is the only boundary across which the
// frozen Formula and binding identities of consecutive plans may differ.
const FormulaRebased = "formula-rebased"

// FormulaRebaseReason is the fixed reason a rebase event records.
const FormulaRebaseReason = "Launcher-only rebind of the active Formula; retained tree, scope, checks and acceptance are unchanged and the successor plan requires exact approval."

var decisionID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

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
		if i > 0 && (p.Plan.ParentDigest != s.Plans[i-1].Digest || p.Plan.MoleculeID != s.Plans[0].Plan.MoleculeID) {
			return Proposal{}, fmt.Errorf("invalid amendment chain")
		}
	}
	if err := s.ValidateFormulaLineage(); err != nil {
		return Proposal{}, fmt.Errorf("invalid amendment chain")
	}
	return s.Plans[len(s.Plans)-1], nil
}
func sameInputs(a, b Inputs) bool {
	a.Issue.UpdatedAt = ""
	b.Issue.UpdatedAt = ""
	return reflect.DeepEqual(a, b)
}

// sameInputsExceptFormula compares inputs with the Formula and binding identities,
// which a launcher-only rebind changes, set aside.
func sameInputsExceptFormula(a, b Inputs) bool {
	a.FormulaSHA256, a.BindingSHA256 = "", ""
	b.FormulaSHA256, b.BindingSHA256 = "", ""
	return sameInputs(a, b)
}

// ValidateFormulaLineage requires consecutive plans to share their frozen inputs,
// except across exactly one valid FormulaRebased event recorded for the successor:
// there the Formula and binding identities may differ, the Formula SHA-256 must,
// and the event must name both SHAs and a well-formed activating decision. Any
// rebase event that does not mark such a boundary is refused, so a ledger cannot
// carry a stray or forged one.
func (s State) ValidateFormulaLineage() error {
	rebases := map[string]Event{}
	for _, e := range s.Events {
		if e.Kind != FormulaRebased {
			if e.FormulaFrom != "" || e.FormulaTo != "" || e.Decision != "" {
				return fmt.Errorf("Formula identity recorded outside a rebase event")
			}
			continue
		}
		if _, twice := rebases[e.Digest]; twice {
			return fmt.Errorf("duplicate Formula rebase event")
		}
		rebases[e.Digest] = e
	}
	used := 0
	for i := 1; i < len(s.Plans); i++ {
		prev, next := s.Plans[i-1].Plan.Inputs, s.Plans[i].Plan.Inputs
		e, rebased := rebases[s.Plans[i].Digest]
		if !rebased {
			if !sameInputs(prev, next) {
				return fmt.Errorf("plan inputs changed without a Formula rebase")
			}
			continue
		}
		used++
		if !sameInputsExceptFormula(prev, next) || prev.FormulaSHA256 == next.FormulaSHA256 ||
			e.FormulaFrom != prev.FormulaSHA256 || e.FormulaTo != next.FormulaSHA256 ||
			!hex256.MatchString(e.FormulaFrom) || !hex256.MatchString(e.FormulaTo) || !decisionID.MatchString(e.Decision) {
			return fmt.Errorf("invalid Formula rebase boundary")
		}
	}
	if used != len(rebases) {
		return fmt.Errorf("Formula rebase event names no plan boundary")
	}
	return nil
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

// RebaseFormula records a successor plan that differs from the current one only in
// its frozen Formula and binding identities and its runtime-owned lifecycle fields.
// The caller proves that the Formula change is launcher-only and that decision
// activated it; this transition binds those facts into one FormulaRebased event and
// leaves the plan waiting for exact approval. It never approves.
func (s State) RebaseFormula(p Plan, decision, at string) (State, error) {
	previous, err := s.current()
	if err != nil {
		return s, err
	}
	if s.Phase != "blocked" && s.Phase != "unknown" && s.Phase != "scope-change" && s.Phase != "dismissed" && s.Phase != "scope-waiting" && s.Phase != "failed" && s.Phase != "interrupted" && s.Phase != "review-waiting" {
		return s, fmt.Errorf("stop for a scope change before proposing an amendment")
	}
	from, to := previous.Plan.Inputs.FormulaSHA256, p.Inputs.FormulaSHA256
	if p.Revision != previous.Plan.Revision+1 || p.ParentDigest != previous.Digest || p.MoleculeID != previous.Plan.MoleculeID ||
		!sameInputsExceptFormula(p.Inputs, previous.Plan.Inputs) || from == to || !hex256.MatchString(to) || !decisionID.MatchString(decision) {
		return s, fmt.Errorf("Formula rebase must retain intake identity, bind the previous plan and name its activating decision")
	}
	digest, err := p.Digest()
	if err != nil {
		return s, err
	}
	out := cloneState(s)
	out.Plans = append(out.Plans, Proposal{Plan: p, Digest: digest})
	out.Phase = "scope-waiting"
	out.Events = append(out.Events, Event{Kind: FormulaRebased, Digest: digest, At: at, FormulaFrom: from, FormulaTo: to, Decision: decision,
		Detail: fmt.Sprintf("Formula SHA-256 %s rebased to %s by activating decision %s. %s", from, to, decision, FormulaRebaseReason)})
	if _, err = out.current(); err != nil {
		return s, err
	}
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

// NoChangeAfterAmendment is the event kind of an amended coding attempt whose
// captured tree equals the parent revision's paused tree.
const NoChangeAfterAmendment = "no-change-after-amendment"

// RecordNoChange ends an amended coding attempt that produced no change. Nothing
// is verified: the Molecule waits for human review, delivery refuses it, and a
// further amendment may proceed from that state.
func (s State) RecordNoChange(tree, at string) (State, error) {
	p, err := s.current()
	if err != nil {
		return s, err
	}
	if s.Phase != "coding" || p.Plan.Revision < 2 || p.Plan.Continuation != "" || !gitHash.MatchString(tree) || tree != p.Plan.PausedTree {
		return s, fmt.Errorf("no-change requires a running amended coding attempt that kept the parent revision's tree")
	}
	out, err := s.transition(NoChangeAfterAmendment, "review-waiting", "Amended coding attempt produced no change (tree "+tree+"); verification was not run.", at)
	if err == nil {
		out.Events[len(out.Events)-1].Tree = tree
	}
	return out, err
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
	if last := s.Events[len(s.Events)-1]; last.Kind == NoChangeAfterAmendment {
		verified := false
		for _, e := range s.Events {
			if e.Digest == p.Digest && e.Kind == "verification-review-waiting" {
				verified = true
			}
		}
		if !verified {
			return Plan{}, fmt.Errorf("amendment produced no change; nothing was verified")
		}
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
