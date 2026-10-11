package molecule

import (
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"path"
	"slices"
	"strings"
)

// sensitiveScopeListVersion names the reviewed control-file lists below. A change
// to either list needs a new version and a fresh human review of the list.
const sensitiveScopeListVersion = "v1"

// sensitiveScopeSegments are directory names that hold CI, tooling or hook control
// files; a scope path with any such segment is sensitive. `scripts` is declined
// wholesale because the baseline checks may run any file under it.
var sensitiveScopeSegments = []string{".github", ".claude", ".forgecell", ".githooks", ".husky", "scripts"}

// sensitiveScopeBasenames are control files that are sensitive at any depth.
var sensitiveScopeBasenames = []string{
	".gitlab-ci.yml", "go.mod", "go.sum",
	"package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml",
	"Makefile", "GNUmakefile",
}

// sensitiveScopeRule reports the rule that makes a repository path sensitive.
// Matching ignores case because the Lab runs on case-insensitive file systems.
func sensitiveScopeRule(p string) (string, bool) {
	segments := strings.Split(p, "/")
	for _, segment := range segments {
		for _, name := range sensitiveScopeSegments {
			if strings.EqualFold(segment, name) {
				return "path segment " + name, true
			}
		}
	}
	base := segments[len(segments)-1]
	for _, name := range sensitiveScopeBasenames {
		if strings.EqualFold(base, name) {
			return "file name " + name, true
		}
	}
	return "", false
}

// checkCitedPaths maps each lower-cased repository path a check relies on to a
// description of the first check that cites it: a frozen definition file or an
// argv element that is exactly that path.
func checkCitedPaths(plan readiness.Plan) map[string]string {
	evidencePaths := map[string]string{}
	for _, e := range plan.Evidence {
		evidencePaths[e.ID] = e.Path
	}
	cited := map[string]string{}
	note := func(p, description string) {
		key := strings.ToLower(p)
		if _, ok := cited[key]; !ok {
			cited[key] = description
		}
	}
	for _, c := range slices.Concat(plan.Analysis.Checks, plan.Setup) {
		for _, id := range c.Definitions {
			if p, ok := evidencePaths[id]; ok {
				note(p, fmt.Sprintf("definition of check %s", c.ID))
			}
		}
		for _, arg := range c.Argv {
			note(path.Clean(arg), fmt.Sprintf("argv of check %s", c.ID))
		}
	}
	return cited
}

type PolicyOutcome string

const (
	// PolicyApprove means every condition of a declared policy holds.
	PolicyApprove PolicyOutcome = "approve"
	// PolicyDecline means a policy is declared but a condition does not hold, or
	// the record cannot be trusted. A human must decide.
	PolicyDecline PolicyOutcome = "decline"
	// PolicyHuman means the gate declares no evaluable policy. It is a human gate.
	PolicyHuman PolicyOutcome = "human"
)

// PolicyEvaluation is the result of judging a gate. Evaluating never approves,
// writes a ledger or starts a process; applying a decision is a separate step.
type PolicyEvaluation struct {
	Outcome       PolicyOutcome
	Policy        string
	Gate          string
	PlanDigest    string
	FormulaSHA256 string
	Reason        string
	Detail        string
	Evidence      []PolicyEvidence
}

// ApprovedBy is the identity recorded for an automatic decision. It names the
// policy and the exact Formula bytes, and is never a human.
func (e PolicyEvaluation) ApprovedBy() string {
	return fmt.Sprintf("policy:%s@%s", e.Policy, e.FormulaSHA256)
}

// EvaluateGatePolicy judges the gate that fills a role in the recorded Formula
// snapshot. Only an exact `mode: policy` with an implemented policy can approve;
// every other gate, including a declared but unevaluated policy, is human.
func EvaluateGatePolicy(r Record, role formula.ExecutionRole) PolicyEvaluation {
	eval := PolicyEvaluation{Outcome: PolicyDecline, FormulaSHA256: r.FormulaSnapshot.SHA256}
	if _, err := resolveExecutionRoles(r); err != nil {
		return eval.withReason("formula-snapshot-invalid", err.Error())
	}
	f, err := formula.Parse([]byte(r.FormulaSnapshot.YAML))
	if err != nil {
		return eval.withReason("formula-snapshot-invalid", err.Error())
	}
	roles, err := f.ExecutionRoles()
	if err != nil {
		return eval.withReason("formula-snapshot-invalid", err.Error())
	}
	gate, ok := roles[role]
	if !ok || gate.Type != "gate" {
		eval.Outcome = PolicyHuman
		return eval.withReason("not-a-gate", fmt.Sprintf("role %s is not a gate", role))
	}
	eval.Gate = gate.ID
	eval.Outcome = PolicyHuman
	id, declared := gate.DeclaresPolicy()
	if !declared {
		return eval.withReason("gate-is-human", "the gate does not declare mode: policy")
	}
	eval.Policy = id
	if role != formula.ScopeApproval || id != formula.PolicyAutoWhenReady {
		return eval.withReason("policy-not-evaluated", fmt.Sprintf("policy %s is declared but not evaluated yet; a human decides", id))
	}
	eval.Outcome = PolicyDecline
	return evaluateAutoWhenReady(r, f, eval)
}

func (e PolicyEvaluation) withReason(reason, detail string) PolicyEvaluation {
	e.Reason, e.Detail = reason, detail
	return e
}

func evaluateAutoWhenReady(r Record, f formula.Formula, eval PolicyEvaluation) PolicyEvaluation {
	decline := func(reason, format string, args ...any) PolicyEvaluation {
		return eval.withReason(reason, fmt.Sprintf(format, args...))
	}
	if !r.FormulaApproved {
		return decline("formula-not-approved", "the Formula was not approved for execution")
	}
	if r.CheckoutStatus != "available" || r.Workspace.Path == "" {
		return decline("checkout-unavailable", "the coding checkout is %q", r.CheckoutStatus)
	}
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		return decline("no-readiness-plan", "the Molecule has no proposed plan")
	}
	if r.Readiness.Phase != "scope-waiting" {
		return decline("not-scope-waiting", "the readiness phase is %q", r.Readiness.Phase)
	}
	if len(r.HarnessAttempts) > 0 || len(r.Readiness.Plans) != 1 {
		return decline("not-first-attempt", "the policy only approves a first plan with no prior coding attempt")
	}
	n := len(r.Readiness.Plans) - 1
	proposal := r.Readiness.Plans[n]
	plan := proposal.Plan
	if len(plan.ExpectedProtectedDeltas) > 0 {
		// Excusing protected-regression failures always needs a human decision.
		return decline("plan-has-protected-deltas", "the plan declares %d expected protected-regression delta(s), which require human review", len(plan.ExpectedProtectedDeltas))
	}
	if len(plan.Artifacts) > 0 {
		return decline("plan-has-artifacts", "the plan allows %d artifact write root(s), which require human review", len(plan.Artifacts))
	}
	if len(plan.CheckInputs) > 0 {
		return decline("plan-has-check-inputs", "the plan carries %d human-supplied check input(s), which require human review", len(plan.CheckInputs))
	}
	base := fmt.Sprintf("/readiness/plans/%d", n)
	if plan.Continuation != "" {
		return decline("plan-continuation", "plan continuation %q is not a first scope approval", plan.Continuation)
	}
	if r.Analysis == nil {
		return decline("analysis-missing", "the ledger has no intake analysis")
	}
	if len(r.Analysis.Questions) > 0 {
		return decline("analysis-has-questions", "the intake analysis has %d open question(s)", len(r.Analysis.Questions))
	}
	if len(plan.Analysis.Questions) > 0 {
		return decline("plan-has-questions", "the plan has %d open question(s)", len(plan.Analysis.Questions))
	}
	if len(plan.Analysis.Scope) == 0 {
		return decline("scope-empty", "the plan scopes no paths")
	}
	for _, s := range plan.Analysis.Scope {
		if err := readiness.ValidatePath(s.Path); err != nil {
			return decline("scope-path-invalid", "scope path %q: %v", s.Path, err)
		}
	}
	for _, s := range plan.Analysis.Scope {
		if rule, ok := sensitiveScopeRule(s.Path); ok {
			return decline("scope-path-sensitive", "scope path %q matches %s of sensitive scope list %s, which requires human review", s.Path, rule, sensitiveScopeListVersion)
		}
	}
	cited := checkCitedPaths(plan)
	for _, s := range plan.Analysis.Scope {
		if description, ok := cited[strings.ToLower(s.Path)]; ok {
			return decline("scope-path-cited-by-check", "scope path %q is cited by the %s, which requires human review", s.Path, description)
		}
	}
	if len(plan.CodingAllowlist) == 0 {
		return decline("allowlist-missing", "the plan carries no derived coding allowlist")
	}
	derived, err := harness.DeriveCodingAllowlist(f.YAML, plan.Analysis.Checks)
	if err != nil {
		return decline("allowlist-underivable", "%v", err)
	}
	if !slices.Equal(derived, plan.CodingAllowlist) {
		return decline("allowlist-mismatch", "the plan allowlist differs from the one derived from its checks and the Formula")
	}
	digest, err := plan.Digest()
	if err != nil {
		return decline("plan-invalid", "%v", err)
	}
	if digest != proposal.Digest {
		return decline("plan-digest-mismatch", "the stored plan digest does not match the plan")
	}
	if err = plan.ValidateForCoding(); err != nil {
		return decline("plan-not-codable", "%v", err)
	}
	// Approve returns a copy and is used only to validate the proposal history; the copy is discarded.
	if _, err = r.Readiness.Approve(digest, plan.Inputs, ""); err != nil {
		return decline("readiness-history-invalid", "%v", err)
	}
	eval.Outcome = PolicyApprove
	eval.PlanDigest = digest
	eval.Evidence = []PolicyEvidence{
		{Pointer: "/formulaSnapshot/sha256", Observed: r.FormulaSnapshot.SHA256},
		{Pointer: "/checkoutStatus", Observed: r.CheckoutStatus},
		{Pointer: "/readiness/phase", Observed: r.Readiness.Phase},
		{Pointer: base + "/digest", Observed: digest},
		{Pointer: "/analysis/questions", Observed: "0 questions"},
		{Pointer: base + "/plan/analysis/questions", Observed: "0 questions"},
		{Pointer: base + "/plan/analysis/scope", Observed: fmt.Sprintf("%d paths, all exact repository paths", len(plan.Analysis.Scope))},
		{Pointer: base + "/plan/analysis/scope", Observed: fmt.Sprintf("no path matches sensitive scope list %s", sensitiveScopeListVersion)},
		{Pointer: base + "/plan/analysis/checks", Observed: "no scope path is cited by a check definition or an exact check argument"},
		{Pointer: base + "/plan/artifacts", Observed: "0 artifact write roots"},
		{Pointer: base + "/plan/checkInputs", Observed: "0 human-supplied check inputs"},
		{Pointer: base + "/plan/codingAllowlist", Observed: fmt.Sprintf("%d entries, equal to the allowlist derived from the Formula and plan checks", len(derived))},
	}
	return eval
}

// RecordScopePolicyApproval re-evaluates the scope gate and, only on approve,
// returns a copy of the record with the decision appended. It does not save the
// record and does not approve anything: Run --approve stays the sole approval
// path, and a caller must persist the decision before using it.
func RecordScopePolicyApproval(r Record) (Record, error) {
	eval := EvaluateGatePolicy(r, formula.ScopeApproval)
	if eval.Outcome != PolicyApprove {
		return r, fmt.Errorf("scope policy did not approve (%s): %s: %s", eval.Outcome, eval.Reason, eval.Detail)
	}
	for _, d := range r.PolicyDecisions {
		if d.Gate == eval.Gate && d.PlanDigest == eval.PlanDigest {
			return r, fmt.Errorf("a policy decision for this gate and plan is already recorded")
		}
	}
	decision := PolicyDecision{Policy: eval.Policy, ApprovedBy: eval.ApprovedBy(), Gate: eval.Gate, PlanDigest: eval.PlanDigest, FormulaSHA256: eval.FormulaSHA256, At: stamp(), Evidence: slices.Clone(eval.Evidence)}
	r.PolicyDecisions = append(slices.Clone(r.PolicyDecisions), decision)
	return r, nil
}
