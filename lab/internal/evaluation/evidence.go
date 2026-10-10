package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// Evidence limits apply to the entire retained chain, not each parent separately.
const EvidenceFileLimit = 8_000_000
const EvidenceTotalLimit = 24_000_000
const EvidenceReferenceLimit = 128
const EvidenceDepthLimit = 8

type Snapshot struct {
	Files   map[string][]byte `json:"files"`
	Missing []string          `json:"missing,omitempty"`
}
type Evidence struct {
	Status            string     `json:"status"`
	Reason            string     `json:"reason,omitempty"`
	Snapshots         []Snapshot `json:"snapshots"`
	Reports           []Report   `json:"reports"`
	Plan              *Plan      `json:"plan,omitempty"`
	VerificationPlans []Plan     `json:"verificationPlans,omitempty"`
	Approval          []byte     `json:"approval,omitempty"`
	Limitations       []string   `json:"limitations"`
}

// ReadLocal does not follow symlinks within or at the supplied root. Roots are
// explicit local directories; no reference can escape one or trigger execution.
// Only exact OS /tmp and /var aliases are allowed above (never at) the root.
func ReadLocal(root, name string) ([]byte, error) {
	if !relative(name, false) || len(strings.Split(filepath.ToSlash(name), "/")) > EvidenceDepthLimit {
		return nil, fmt.Errorf("invalid evidence reference")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	ancestor := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator)) {
		ancestor = filepath.Join(ancestor, part)
		st, e := os.Lstat(ancestor)
		if e != nil {
			return nil, e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(ancestor)
			systemAlias := (ancestor == "/tmp" && target == "private/tmp") || (ancestor == "/var" && target == "private/var")
			if e != nil || ancestor == absolute || !systemAlias {
				return nil, fmt.Errorf("symlink in evidence root")
			}
			// Only these exact OS aliases may occur above the supplied root.
			continue
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("invalid evidence root directory")
		}
	}
	st, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("evidence root must be a directory")
	}
	path := root
	parts := strings.Split(name, string(filepath.Separator))
	for i, part := range parts {
		path = filepath.Join(path, part)
		st, err = os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !st.IsDir()) {
			return nil, fmt.Errorf("symlink or invalid evidence directory")
		}
	}
	if !st.Mode().IsRegular() || st.Size() > EvidenceFileLimit {
		return nil, fmt.Errorf("expected bounded regular evidence")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return nil, fmt.Errorf("evidence changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, EvidenceFileLimit+1))
	if len(raw) > EvidenceFileLimit {
		return nil, fmt.Errorf("evidence file limit exceeded")
	}
	return raw, err
}
func ValidateSnapshots(ss []Snapshot) error {
	if len(ss) == 0 || len(ss) > EvidenceDepthLimit {
		return fmt.Errorf("evidence chain depth exceeded or missing")
	}
	total, count := 0, 0
	for _, s := range ss {
		for name, b := range s.Files {
			if !relative(name, false) || len(strings.Split(filepath.ToSlash(name), "/")) > EvidenceDepthLimit || len(b) > EvidenceFileLimit {
				return fmt.Errorf("invalid or oversized snapshot reference")
			}
			count++
			total += len(b)
		}
		count += len(s.Missing)
	}
	if total > EvidenceTotalLimit || count > EvidenceReferenceLimit {
		return fmt.Errorf("evidence snapshot budget exceeded")
	}
	return nil
}

// ReadEvidence snapshots only known inputs. Parent directories are explicit,
// nearest first, because a legacy recheck report records a hash, not a path.
func ReadEvidence(dir string, parents []string) (Evidence, error) {
	if len(parents) >= EvidenceDepthLimit {
		return Evidence{}, fmt.Errorf("evidence chain depth exceeded")
	}
	var ss []Snapshot
	for _, root := range append([]string{dir}, parents...) {
		s := Snapshot{Files: map[string][]byte{}}
		add := func(name string) error {
			raw, e := ReadLocal(root, name)
			if os.IsNotExist(e) {
				s.Missing = append(s.Missing, name)
				return nil
			}
			if e != nil {
				return e
			}
			s.Files[name] = raw
			return nil
		}
		if err := add("report.json"); err != nil {
			return Evidence{}, err
		}
		var r Report
		json.Unmarshal(s.Files["report.json"], &r)
		if noteHash(r, "Verification plan SHA-256: ") != "" {
			if err := add("verification-plan.json"); err != nil {
				return Evidence{}, err
			}
			var p Plan
			json.Unmarshal(s.Files["verification-plan.json"], &p)
			if !relative(p.Acceptance, false) {
				return Evidence{}, fmt.Errorf("invalid acceptance input path")
			}
			if err := add("acceptance" + filepath.Ext(p.Acceptance)); err != nil {
				return Evidence{}, err
			}
		} else {
			for _, name := range []string{"inputs/plan.json", "inputs/approval.json", "inputs/issue.json", "baseline/evaluation-decision.json", "candidate/evaluation-decision.json"} {
				if err := add(name); err != nil {
					return Evidence{}, err
				}
			}
			var p Plan
			json.Unmarshal(s.Files["inputs/plan.json"], &p)
			if !relative(p.Acceptance, false) { // malformed/missing plans remain inspectable
				if len(s.Files["inputs/plan.json"]) != 0 {
					return Evidence{}, fmt.Errorf("invalid acceptance input path")
				}
			} else if err := add("inputs/acceptance" + filepath.Ext(p.Acceptance)); err != nil {
				return Evidence{}, err
			}
		}
		ss = append(ss, s)
		if err := ValidateSnapshots(ss); err != nil {
			return Evidence{}, err
		}
	}
	return InspectEvidence(ss), nil
}
func noteHash(r Report, prefix string) string {
	value := ""
	for _, n := range r.Notes {
		if strings.HasPrefix(n, prefix) {
			value = strings.TrimPrefix(n, prefix)
		}
	}
	return value
}
func InspectEvidence(ss []Snapshot) Evidence {
	e := Evidence{Snapshots: ss, Status: "incomplete", Limitations: []string{"A recorded pair is local feasibility evidence, not statistical or causal proof of improvement.", "Hashes establish byte identity, not authenticity. Model identity, cost, retries and production quality are unknown unless separately evidenced."}}
	fail := func(status, reason string) Evidence { e.Status = status; e.Reason = reason; return e }
	if err := ValidateSnapshots(ss); err != nil {
		return fail("incomplete", err.Error())
	}
	for _, s := range ss {
		var r Report
		if json.Unmarshal(s.Files["report.json"], &r) != nil {
			return fail("incomplete", "missing or invalid report")
		}
		e.Reports = append(e.Reports, r)
		if len(s.Missing) > 0 {
			return fail("incomplete", "missing frozen inputs: "+strings.Join(s.Missing, ", "))
		}
	}
	// Resolve the original input identity before validating any revised checks.
	last := ss[len(ss)-1]
	original := e.Reports[len(ss)-1]
	if noteHash(original, "Verification plan SHA-256: ") != "" {
		return fail("incomplete", "recheck requires its original report and frozen input chain")
	}
	raw := last.Files["inputs/plan.json"]
	var p Plan
	if json.Unmarshal(raw, &p) != nil {
		return fail("incomplete", "missing or invalid frozen plan")
	}
	e.Plan = &p
	e.Approval = last.Files["inputs/approval.json"]
	if Hash(raw) != original.PlanSHA256 || Hash(e.Approval) != p.ApprovalSHA256 || Hash(last.Files["inputs/issue.json"]) != p.TaskSHA256 || Hash(last.Files["inputs/acceptance"+filepath.Ext(p.Acceptance)]) != p.AcceptanceSHA256 {
		return fail("stale", "frozen input bytes differ from recorded identities")
	}
	pair, err := ValidateApproval(e.Approval)
	if err != nil {
		return fail("unsupported", err.Error())
	}
	type executionDecision struct {
		Digest          string `json:"digest"`
		ApprovedAt      string `json:"approvedAt"`
		PlanSHA256      string `json:"planSha256"`
		BaselineSHA256  string `json:"baselineSha256"`
		CandidateSHA256 string `json:"candidateSha256"`
		SourceRoot      string `json:"sourceRoot"`
		Output          string `json:"output"`
	}
	var decisions [2]executionDecision
	for i, role := range []string{"baseline", "candidate"} {
		if json.Unmarshal(last.Files[role+"/evaluation-decision.json"], &decisions[i]) != nil {
			return fail("incomplete", "missing execution decision evidence")
		}
		d := decisions[i]
		if d.SourceRoot == "" || d.Output == "" || d.Digest != original.ActivationDigest || d.ApprovedAt != original.ActivationDecisionAt || d.PlanSHA256 != Hash(raw) || d.BaselineSHA256 != pair.Baseline.SHA256 || d.CandidateSHA256 != pair.Candidate.SHA256 || ActivationDigest(raw, pair, d.SourceRoot, d.Output) != d.Digest {
			return fail("stale", "execution decision differs from frozen activation inputs")
		}
	}
	if decisions[0] != decisions[1] {
		return fail("stale", "variant execution decisions differ")
	}
	var issue molecule.Issue
	if json.Unmarshal(last.Files["inputs/issue.json"], &issue) != nil || issue.Number < 1 || issue.Repo != pair.Baseline.Intake.Repo || issue.URL != fmt.Sprintf("https://github.com/%s/issues/%d", issue.Repo, issue.Number) {
		return fail("stale", "frozen task differs from Formula intake")
	}
	for i := len(ss) - 1; i >= 0; i-- {
		r := e.Reports[i]
		effective := p
		if i < len(ss)-1 {
			parent := e.Reports[i+1]
			if noteHash(r, "Original report SHA-256: ") != Hash(ss[i+1].Files["report.json"]) {
				return fail("stale", "recheck parent report identity differs")
			}
			revised := ss[i].Files["verification-plan.json"]
			if Hash(revised) != noteHash(r, "Verification plan SHA-256: ") || json.Unmarshal(revised, &effective) != nil {
				return fail("stale", "recheck verification plan identity differs")
			}
			comparison := effective
			comparison.OriginalCheckFiles = p.OriginalCheckFiles
			if !reflect.DeepEqual(comparison, p) || len(effective.OriginalCheckFiles) < len(p.OriginalCheckFiles) || !reflect.DeepEqual(effective.OriginalCheckFiles[:len(p.OriginalCheckFiles)], p.OriginalCheckFiles) {
				return fail("stale", "recheck changed frozen task or check commands")
			}
			if Hash(ss[i].Files["acceptance"+filepath.Ext(effective.Acceptance)]) != p.AcceptanceSHA256 {
				return fail("stale", "recheck acceptance bytes differ")
			}
			if r.ActivationDigest != parent.ActivationDigest || r.ActivationDecisionAt != parent.ActivationDecisionAt {
				return fail("stale", "recheck activation identity differs")
			}
			for j := range r.Attempts {
				if j >= len(parent.Attempts) {
					return fail("incomplete", "recheck attempt count differs")
				}
				a, b := r.Attempts[j], parent.Attempts[j]
				a.Verification, b.Verification = Verification{}, Verification{}
				a.Checks, b.Checks = nil, nil
				a.Correct, b.Correct = false, false
				a.Error, b.Error = "", ""
				if !reflect.DeepEqual(a, b) {
					return fail("stale", "recheck changed captured attempt")
				}
			}
		}
		if err := validateReport(r, effective, pair, issue, Hash(raw)); err != nil {
			return fail("incomplete", err.Error())
		}
		e.VerificationPlans = append(e.VerificationPlans, effective)
		for _, attempt := range r.Attempts {
			if err := validateFrozenReadiness(attempt, p, raw, last.Files["inputs/acceptance"+filepath.Ext(p.Acceptance)], issue); err != nil {
				return fail("incomplete", err.Error())
			}
		}
	}
	e.Status = "comparable"
	return e
}
func validateReport(r Report, p Plan, pair Pair, issue molecule.Issue, planHash string) error {
	if r.SchemaVersion != 1 || r.Status != "complete" || r.FinishedAt == "" || len(r.Attempts) != 2 || r.ActivationDigest == "" || r.ActivationDecisionAt == "" {
		return fmt.Errorf("report is incomplete or lacks execution decision")
	}
	if p.SchemaVersion != 1 || p.ReadinessVersion != 1 || p.TargetBranch == "" || !commitPattern.MatchString(p.BaseCommit) || len(p.AllowedFiles) == 0 || len(p.Checks) == 0 || r.PlanSHA256 != planHash || r.TaskSHA256 != p.TaskSHA256 || r.ApprovalSHA256 != p.ApprovalSHA256 || r.ApprovalID != pair.Approval.ID || r.BaseCommit != p.BaseCommit {
		return fmt.Errorf("frozen report/plan identities differ")
	}
	names := map[string]bool{}
	acceptance := 0
	for _, c := range append(append([]Command{}, p.Setup...), p.Checks...) {
		if c.Name == "" || names[c.Name] || !relative(c.Dir, true) || len(c.Argv) == 0 || c.Argv[0] == "" || c.TimeoutMS < 1 || c.TimeoutMS > 3_600_000 {
			return fmt.Errorf("invalid frozen check definition")
		}
		names[c.Name] = true
	}
	for _, c := range p.Checks {
		if c.Name == "acceptance" {
			acceptance++
		}
	}
	if acceptance != 1 {
		return fmt.Errorf("missing acceptance definition")
	}
	scope := map[string]bool{}
	for _, f := range p.AllowedFiles {
		if !relative(f, false) {
			return fmt.Errorf("invalid frozen scope")
		}
		scope[f] = true
	}
	for _, f := range p.OriginalCheckFiles {
		if !relative(f, false) || !scope[f] {
			return fmt.Errorf("invalid protected check input")
		}
	}
	for i, a := range r.Attempts {
		f := pair.Baseline
		if i == 1 {
			f = pair.Candidate
		}
		if a.Variant != []string{"baseline", "candidate"}[i] || a.FormulaSHA256 != f.SHA256 || a.Molecule.FormulaID != f.ID || !a.Molecule.FormulaApproved || a.Molecule.FormulaSnapshot.SHA256 != f.SHA256 || a.Molecule.FormulaSnapshot.YAML != f.YAML || a.Molecule.Workspace.BaseCommit != p.BaseCommit || !reflect.DeepEqual(a.Molecule.Issue, issue) {
			return fmt.Errorf("attempt role, Formula or frozen task differs")
		}
		if !commitPattern.MatchString(a.Tree) || a.Verification.SourceTree != a.Tree {
			return fmt.Errorf("missing or mismatched captured verification tree")
		}
		if err := observations(p.Setup, a.Setup); err != nil {
			return err
		}
		if p.RequireInitialFailure {
			var initial []Command
			for _, c := range p.Checks {
				if c.Name == "acceptance" {
					initial = append(initial, c)
				}
			}
			if err := observations(initial, a.Preflight); err != nil {
				return err
			}
			for _, o := range a.Preflight {
				r := o.Result
				if r.OK || r.Code != 1 || (r.Error != "" && r.Error != "exit status 1") || r.TimedOut || r.Interrupted || r.Overflow || !r.Reconciled || r.Uncertainty != "" {
					return fmt.Errorf("initial acceptance failure is missing or uncertain")
				}
			}
		} else if len(a.Preflight) != 0 {
			return fmt.Errorf("unexpected initial acceptance observations")
		}
		if !reflect.DeepEqual(a.Checks, a.Verification.Checks) {
			return fmt.Errorf("check observation projections differ")
		}
		if err := observations(p.Setup, a.Verification.Setup); err != nil {
			return err
		}
		if err := observations(p.Checks, a.Checks); err != nil {
			return err
		}
		passed := a.ScopeOK && a.Verification.Error == ""
		executed := false
		for _, atom := range a.Molecule.Atoms {
			if atom.Type == "harness" && atom.Status == "done" {
				executed = true
			}
			if atom.Status == "failed" || atom.Status == "blocked" {
				passed = false
			}
		}
		for _, obs := range append(append([]Observation{}, a.Verification.Setup...), a.Checks...) {
			if !obs.Result.OK {
				passed = false
			}
		}
		passed = passed && executed && (a.Molecule.Status == "waiting" || a.Molecule.Status == "recorded")
		if a.Correct != passed || a.Verification.Correct != passed {
			return fmt.Errorf("correctness flag is unsupported by complete observations")
		}
		for _, file := range a.ChangedFiles {
			if !scope[file] && a.ScopeOK {
				return fmt.Errorf("scope flag contradicts changed files")
			}
		}
	}
	return nil
}
func observations(commands []Command, obs []Observation) error {
	if len(commands) != len(obs) {
		return fmt.Errorf("incomplete frozen command observations")
	}
	for i, c := range commands {
		r := obs[i].Result
		if !reflect.DeepEqual(c, obs[i].Command) {
			return fmt.Errorf("observed command differs from frozen definition")
		}
		if r.OK && (r.Code != 0 || r.Error != "" || r.TimedOut || r.Interrupted || r.Overflow || !r.Reconciled || r.Uncertainty != "") {
			return fmt.Errorf("inconsistent observed result")
		}
	}
	return nil
}

// The readiness plan remains bound to the original evaluation plan, including
// on rechecks. A revised independent verification plan is a separate identity.
func validateFrozenReadiness(a Attempt, p Plan, planRaw, acceptance []byte, issue molecule.Issue) error {
	state := a.Molecule.Readiness
	if state == nil || len(state.Plans) != 1 {
		return fmt.Errorf("missing or unsupported frozen readiness history")
	}
	proposal := state.Plans[0]
	rp := proposal.Plan
	digest, err := rp.Digest()
	if err != nil || digest != proposal.Digest {
		return fmt.Errorf("frozen readiness plan differs from its digest")
	}
	expectedIssue := readiness.Issue{Repository: issue.Repo, Number: issue.Number, URL: issue.URL, State: issue.State, Title: issue.Title, Body: issue.Body, UpdatedAt: issue.UpdatedAt}
	if rp.MoleculeID != a.Molecule.ID || rp.Inputs.FormulaSHA256 != a.FormulaSHA256 || rp.Inputs.BaseCommit != p.BaseCommit || rp.Inputs.TargetBranch != p.TargetBranch || !reflect.DeepEqual(rp.Inputs.Issue, expectedIssue) {
		return fmt.Errorf("frozen readiness task or Formula identity differs")
	}
	var o molecule.Options
	configureReadiness(&o, p, planRaw, acceptance)
	expected, err := o.Analyze(context.Background(), formula.Binding{}, issue, readiness.RepositorySnapshot{})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(rp.CheckInputs, o.CheckInputs) || !reflect.DeepEqual(rp.Setup, o.Setup) || !reflect.DeepEqual(rp.Analysis.Checks, expected.Checks) || !reflect.DeepEqual(rp.Analysis.Scope, expected.Scope) || !reflect.DeepEqual(rp.Analysis.Acceptance, expected.Acceptance) || !reflect.DeepEqual(rp.Artifacts, o.Artifacts) {
		return fmt.Errorf("frozen readiness checks or acceptance inputs differ")
	}
	if a.Correct {
		if _, err := state.ValidateForDelivery(); err != nil {
			return fmt.Errorf("incomplete readiness approval history: %w", err)
		}
	}
	return nil
}
