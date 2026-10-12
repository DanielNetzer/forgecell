// Package readiness describes reviewable ticket scope. It never invokes a model,
// grants permissions, executes commands or imports the Molecule orchestrator.
package readiness

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	pathpkg "path"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const MaxAnalysisBytes = 256_000

type Issue struct {
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	URL        string `json:"url"`
	State      string `json:"state"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	UpdatedAt  string `json:"updatedAt"`
}
type Inputs struct {
	Repository           string   `json:"repository"`
	TargetBranch         string   `json:"targetBranch"`
	BaseCommit           string   `json:"baseCommit"`
	FormulaSHA256        string   `json:"formulaSha256"`
	EvidenceSHA256       string   `json:"evidenceSha256"`
	BindingSHA256        string   `json:"bindingSha256"`
	Issue                Issue    `json:"issue"`
	MandatoryConstraints []string `json:"mandatoryConstraints"`
}

// Evidence is a reference to committed regular-file content. The collector owns
// the corresponding bytes; IDs, paths and hashes are frozen in each plan.
type Evidence struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type ScopedPath struct {
	Path     string   `json:"path"`
	Reason   string   `json:"reason"`
	Evidence []string `json:"evidence"`
}
type Criterion struct {
	Description string   `json:"description"`
	Evidence    []string `json:"evidence"`
}
type Assessment = Criterion

type Question struct {
	Question string   `json:"question"`
	Reason   string   `json:"reason"`
	Evidence []string `json:"evidence"`
}
type Check struct {
	ID          string   `json:"id"`
	Category    string   `json:"category"`
	Argv        []string `json:"argv"`
	Dir         string   `json:"dir"`
	TimeoutMS   int      `json:"timeoutMs"`
	Required    bool     `json:"required"`
	Definitions []string `json:"definitions"`
	Reason      string   `json:"reason"`
	Evidence    []string `json:"evidence"`
	// Only substantive separate review can supply independent provenance. Analysis
	// output alone must not promote model-selected tests to independent acceptance.
	IndependentProvenance string `json:"independentProvenance"`
	// ReviewedAcceptance explicitly permits candidate execution of scoped checker
	// edits. Frozen definitions remain required in the protected regression view.
	ReviewedAcceptance string `json:"reviewedAcceptance,omitempty"`
}
type ArtifactRoot struct {
	Path      string `json:"path"`
	CommandID string `json:"commandId"`
	MaxFiles  int    `json:"maxFiles,omitempty"`
	MaxBytes  int64  `json:"maxBytes,omitempty"`
	MaxDepth  int    `json:"maxDepth,omitempty"`
}
type ExecutionPolicy struct {
	Environment     []string `json:"environment"`
	CredentialNames []string `json:"credentialNames"`
	SetupNetwork    string   `json:"setupNetwork"`
	CheckNetwork    string   `json:"checkNetwork"`
	Containment     string   `json:"containment"`
}
type EvidenceRequest struct {
	Path     string   `json:"path"`
	Reason   string   `json:"reason"`
	Evidence []string `json:"evidence"`
}
type Analysis struct {
	EvidenceRequests []EvidenceRequest `json:"evidenceRequests,omitempty"`
	Summary          string            `json:"summary"`
	Scope            []ScopedPath      `json:"scope"`
	Acceptance       []Criterion       `json:"acceptance"`
	Checks           []Check           `json:"checks"`
	Impacts          []Assessment      `json:"impacts"`
	Unknowns         []string          `json:"unknowns"`
	Questions        []Question        `json:"questions"`
}
type Plan struct {
	AllowNoChange         bool            `json:"allowNoChange,omitempty"`
	AdoptAttempt          string          `json:"adoptAttempt,omitempty"`
	Continuation          string          `json:"continuation,omitempty"`
	PausedArtifactsSHA256 string          `json:"pausedArtifactsSha256,omitempty"`
	CheckInputs           []FileEvidence  `json:"checkInputs"`
	SchemaVersion         string          `json:"schemaVersion"`
	MoleculeID            string          `json:"moleculeId"`
	Revision              int             `json:"revision"`
	ParentDigest          string          `json:"parentDigest"`
	PausedTree            string          `json:"pausedTree"`
	Inputs                Inputs          `json:"inputs"`
	Evidence              []Evidence      `json:"evidence"`
	Analysis              Analysis        `json:"analysis"`
	Setup                 []Check         `json:"setup"`
	Artifacts             []ArtifactRoot  `json:"artifacts"`
	Policy                ExecutionPolicy `json:"policy"`
	// CodingAllowlist is the exact tool allowance for a Claude Code coding attempt,
	// derived from the checks and Formula snapshot above. It joins the plan digest,
	// so a change needs re-approval. Absent means no allowance beyond the base list.
	CodingAllowlist []string `json:"codingAllowlist,omitempty"`
	// HarnessTimeoutMS is a reviewed per-Molecule coding timeout. Zero keeps the
	// Formula timeout and leaves the digest of every earlier plan unchanged. The
	// Formula-declared maximum is enforced where the Formula is known.
	HarnessTimeoutMS int `json:"harnessTimeoutMs,omitempty"`
	// ExpectedProtectedDeltas lists exact Go tests that may fail in the protected
	// regression view because the approved scope intentionally changes what they
	// pin. It joins the plan digest, so only the exact human approval accepts it.
	// Empty keeps the digest of every earlier plan unchanged.
	ExpectedProtectedDeltas []ProtectedDelta `json:"expectedProtectedDeltas,omitempty"`
	// FormulaBaseline lists the ids of the checks the approved Formula requires in
	// every plan. Each must match a required regression check below, so a baseline
	// check can be told apart from a candidate. It joins the plan digest; empty
	// keeps the digest of every earlier plan unchanged.
	FormulaBaseline []string `json:"formulaBaseline,omitempty"`
	// NewFiles lists the scope paths that were absent from the frozen repository tree
	// when the Lab built this plan and that a candidate check cites as its definition
	// by exact path. The Lab derives it from the frozen tree, never from model output,
	// and verification re-checks the base commit. It joins the plan digest; empty keeps
	// the digest of every earlier plan unchanged.
	NewFiles []string `json:"newFiles,omitempty"`
}

// ProtectedDelta names one test, matched by exact Go package import path and
// exact test name (subtests as Test/sub/case), that may fail in the protected run
// of a required regression check. ScopePath is the approved protected test source
// whose intentional change justifies the failure.
type ProtectedDelta struct {
	CheckID   string   `json:"checkId"`
	Package   string   `json:"package"`
	Test      string   `json:"test"`
	ScopePath string   `json:"scopePath"`
	Reason    string   `json:"reason"`
	Evidence  []string `json:"evidence"`
}

// MaxProtectedDeltas bounds a declaration so it stays reviewable.
const MaxProtectedDeltas = 64

// MaxHarnessTimeoutMS is the same hard cap the Formula and checks use.
const MaxHarnessTimeoutMS = 3600000

// PolicyViolation marks a complete, well-formed analysis that was rejected for a
// policy, labelling or evidence-request-cap reason, as opposed to missing evidence
// or malformed output.
type PolicyViolation struct{ Reason string }

func (e *PolicyViolation) Error() string { return e.Reason }

// LabellingViolationMessage is the exact rejection of an analysis that labels its
// own checks as independent acceptance.
const LabellingViolationMessage = "analysis cannot assert independent acceptance review; retain as a candidate check for separate human review"

// Policy violation classes. Each class gets its own corrective analysis retry.
const (
	PolicyClassLabelling          = "labelling"
	PolicyClassInScopeDefinition  = "in-scope-definition"
	PolicyClassEvidenceRequestCap = "evidence-request-cap"
	PolicyClassDefinitionRef      = "definition-reference"
	PolicyClassOther              = "other"
)

// PolicyClass names the class of a policy violation from its exact validator text.
func PolicyClass(v *PolicyViolation) string {
	switch {
	case v == nil:
		return PolicyClassOther
	case v.Reason == LabellingViolationMessage:
		return PolicyClassLabelling
	case strings.HasPrefix(v.Reason, InScopeDefinitionMessage):
		return PolicyClassInScopeDefinition
	case v.Reason == EvidenceRequestCapMessage():
		return PolicyClassEvidenceRequestCap
	case strings.HasPrefix(v.Reason, DefinitionReferenceMessage):
		return PolicyClassDefinitionRef
	}
	return PolicyClassOther
}

// IsPolicyViolation reports whether err is a rejection that a corrective analysis
// pass may address. It never reflects a relaxed validator.
func IsPolicyViolation(err error) bool {
	var violation *PolicyViolation
	return errors.As(err, &violation)
}

var hex256 = regexp.MustCompile(`^[a-f0-9]{64}$`)
var gitHash = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var codingAllowlistEntry = regexp.MustCompile(`^(?:Read|Glob|Grep|Edit|Write|Bash\([A-Za-z0-9][A-Za-z0-9_./=:@% +*-]*\))$`)

// validateCodingAllowlist is syntactic and bounded; the harness package owns the
// command policy and rederives the exact list before coding starts.
func validateCodingAllowlist(entries []string) error {
	if len(entries) > 512 {
		return fmt.Errorf("too many coding allowlist entries")
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if len(entry) > 512 || !codingAllowlistEntry.MatchString(entry) || seen[entry] {
			return fmt.Errorf("invalid or duplicate coding allowlist entry")
		}
		seen[entry] = true
	}
	return nil
}

func textOK(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 16000 && !strings.ContainsRune(s, 0)
}

// ValidatePath is syntactic; callers must additionally check repository modes,
// ancestors, submodules and nested repositories against the frozen tree/disk.
func ValidatePath(s string) error {
	if s == "" || s == "." || path.IsAbs(s) || path.Clean(s) != s || strings.HasPrefix(s, "-") || strings.ContainsAny(s, "\\*?[]:") {
		return fmt.Errorf("expected an exact normalized repository path: %q", s)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("control character in path")
		}
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." || strings.EqualFold(part, ".git") {
			return fmt.Errorf("unsafe repository path: %q", s)
		}
	}
	return nil
}

// DecodeAnalysis rejects unknown fields, duplicate keys, trailing JSON and
// over-limit output. It retains clarification questions without minting approval.
func DecodeAnalysis(raw []byte) (Analysis, error) {
	var a Analysis
	if len(raw) > MaxAnalysisBytes {
		return a, fmt.Errorf("analysis exceeds %d bytes", MaxAnalysisBytes)
	}
	if err := uniqueJSON(raw); err != nil {
		return a, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, err
	}
	if !textOK(a.Summary) {
		return a, fmt.Errorf("analysis needs a bounded summary")
	}
	for _, c := range a.Checks {
		if c.Category == "independent-acceptance" || c.IndependentProvenance != "" || c.ReviewedAcceptance != "" {
			return a, &PolicyViolation{Reason: LabellingViolationMessage}
		}
	}
	if err := ValidateEvidenceRequests(a.EvidenceRequests, nil); err != nil {
		return a, err
	}
	return a, nil
}
func uniqueJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func() error
	value = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				s, ok := key.(string)
				if !ok || seen[s] {
					return fmt.Errorf("invalid or duplicate JSON key: %v", key)
				}
				seen[s] = true
				if err = value(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("analysis must contain exactly one JSON object")
	}
	return nil
}
func (p Plan) Validate() error {
	if p.Continuation != "" && p.Continuation != "checks-only" && p.Continuation != "adopt-failed-tree" {
		return fmt.Errorf("unsupported continuation")
	}
	if p.Continuation != "" && p.Revision == 1 {
		return fmt.Errorf("checks-only requires retained coding output")
	}
	if (p.Continuation == "adopt-failed-tree" && (!identifier.MatchString(p.AdoptAttempt) || !hex256.MatchString(p.PausedArtifactsSHA256))) || (p.Continuation != "adopt-failed-tree" && p.AdoptAttempt != "") {
		return fmt.Errorf("failed-tree adoption requires an exact attempt and artifact identity")
	}
	for _, constraint := range p.Inputs.MandatoryConstraints {
		kind, digest, ok := strings.Cut(constraint, ":")
		if !ok || (kind != "github-ruleset-sha256" && kind != "github-protection-sha256") || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(digest) {
			return fmt.Errorf("unsupported mandatory policy constraint")
		}
	}
	if p.SchemaVersion != "v1" || !identifier.MatchString(p.MoleculeID) || p.Revision < 1 {
		return fmt.Errorf("invalid readiness identity")
	}
	if p.Revision == 1 {
		if p.ParentDigest != "" || p.PausedTree != "" || p.PausedArtifactsSHA256 != "" {
			return fmt.Errorf("initial plan cannot have an amendment parent/tree")
		}
	} else if !hex256.MatchString(p.ParentDigest) || !gitHash.MatchString(p.PausedTree) {
		return fmt.Errorf("amendment requires parent digest and paused tree")
	}
	in := p.Inputs
	if !textOK(in.Repository) || !textOK(in.TargetBranch) || !gitHash.MatchString(in.BaseCommit) || !hex256.MatchString(in.FormulaSHA256) || !hex256.MatchString(in.EvidenceSHA256) || !hex256.MatchString(in.BindingSHA256) {
		return fmt.Errorf("incomplete frozen inputs")
	}
	issue := in.Issue
	if issue.Repository != in.Repository || issue.Number < 1 || issue.State != "OPEN" || !textOK(issue.Title) || issue.URL != fmt.Sprintf("https://github.com/%s/issues/%d", in.Repository, issue.Number) {
		return fmt.Errorf("issue must be open and match the selected repository and identity")
	}
	refs := map[string]bool{"issue": true}
	if len(p.Evidence) > 256 {
		return fmt.Errorf("too many evidence references")
	}
	evidencePaths := map[string]bool{}
	for _, e := range p.Evidence {
		if evidencePaths[e.Path] {
			return fmt.Errorf("ambiguous evidence path: %s", e.Path)
		}
		evidencePaths[e.Path] = true
		if !identifier.MatchString(e.ID) || refs[e.ID] || !hex256.MatchString(e.SHA256) {
			return fmt.Errorf("invalid or duplicate evidence identity: %s", e.ID)
		}
		if err := ValidatePath(e.Path); err != nil {
			return err
		}
		refs[e.ID] = true
	}
	if err := validateCheckInputs(p.CheckInputs); err != nil {
		return err
	}
	if len(p.CheckInputs) > 32 {
		return fmt.Errorf("too many reviewed check inputs")
	}
	for _, input := range p.CheckInputs {
		if len(input.Content) > 64000 || fmt.Sprintf("%x", sha256.Sum256([]byte(input.Content))) != input.SHA256 {
			return fmt.Errorf("reviewed check input bytes do not match hash")
		}
		found := false
		for _, ref := range p.Evidence {
			if ref == input.Evidence {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("reviewed check input lacks exact evidence reference")
		}
	}
	cited := func(ids []string) error {
		if len(ids) == 0 || len(ids) > 256 {
			return fmt.Errorf("evidence citation required")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !refs[id] || seen[id] {
				return fmt.Errorf("unknown or duplicate evidence citation: %s", id)
			}
			seen[id] = true
		}
		return nil
	}
	a := p.Analysis
	if !textOK(a.Summary) || len(a.Scope) == 0 || len(a.Scope) > 256 || len(a.Acceptance) == 0 || len(a.Acceptance) > 100 || len(a.Checks) == 0 || len(a.Checks) > 64 {
		return fmt.Errorf("plan requires bounded summary, scope, acceptance and checks")
	}
	if len(a.EvidenceRequests) > 0 {
		return fmt.Errorf("unresolved evidence requests prevent approval")
	}
	if len(a.Questions) > 0 {
		return fmt.Errorf("clarification required before approval")
	}
	paths := map[string]bool{}
	for _, s := range a.Scope {
		if err := ValidatePath(s.Path); err != nil {
			return err
		}
		if paths[s.Path] || !textOK(s.Reason) {
			return fmt.Errorf("duplicate scope or missing reason: %s", s.Path)
		}
		paths[s.Path] = true
		if err := cited(s.Evidence); err != nil {
			return err
		}
	}
	for _, input := range p.CheckInputs {
		if paths[input.Path] {
			return fmt.Errorf("reviewed check input overlaps source scope: %s", input.Path)
		}
	}
	for _, c := range append(append([]Criterion{}, a.Acceptance...), a.Impacts...) {
		if !textOK(c.Description) {
			return fmt.Errorf("empty criterion/assessment")
		}
		if err := cited(c.Evidence); err != nil {
			return err
		}
	}
	for _, c := range a.Checks {
		if c.Category == "setup" {
			return fmt.Errorf("setup is not a verification check")
		}
	}
	for _, c := range p.Setup {
		if c.Category != "setup" {
			return fmt.Errorf("setup commands must be labelled setup")
		}
	}
	newFiles := map[string]bool{}
	for _, file := range p.NewFiles {
		if !paths[file] || newFiles[file] || evidencePaths[file] || refs[file] {
			return fmt.Errorf("new file %q must be a distinct scope path that is not frozen evidence", file)
		}
		newFiles[file] = true
	}
	ids := map[string]bool{}
	required := false
	for _, c := range append(append([]Check{}, a.Checks...), p.Setup...) {
		if !identifier.MatchString(c.ID) || ids[c.ID] || !textOK(c.Reason) {
			return fmt.Errorf("invalid or duplicate command identity: %s", c.ID)
		}
		ids[c.ID] = true
		switch c.Category {
		case "regression", "candidate", "setup":
		case "independent-acceptance":
			if !textOK(c.IndependentProvenance) {
				return fmt.Errorf("independent acceptance requires separate review provenance")
			}
		default:
			return fmt.Errorf("unknown check category: %s", c.Category)
		}
		if c.Category != "setup" && c.Required {
			required = true
		}
		if c.Dir != "." {
			if err := ValidatePath(c.Dir); err != nil {
				return err
			}
		}
		if len(c.Argv) == 0 || len(c.Argv) > 128 || c.TimeoutMS < 1 || c.TimeoutMS > 3600000 {
			return fmt.Errorf("invalid command argv or timeout")
		}
		for _, s := range c.Argv {
			if !textOK(s) {
				return fmt.Errorf("invalid command argument")
			}
		}
		if strings.ContainsAny(c.Argv[0], "\r\n\t") || (!path.IsAbs(c.Argv[0]) && strings.Contains(c.Argv[0], " ")) {
			return fmt.Errorf("command must be argv, not a shell string")
		}
		if err := cited(c.Evidence); err != nil {
			return err
		}
		definitions := []string{}
		citedNew := map[string]bool{}
		for _, id := range c.Definitions {
			if !newFiles[id] {
				definitions = append(definitions, id)
				continue
			}
			if c.Category != "candidate" || citedNew[id] {
				return fmt.Errorf("command definitions: new file %s may define only a candidate check, once", id)
			}
			citedNew[id] = true
		}
		if len(definitions) > 0 || len(citedNew) == 0 {
			if err := cited(definitions); err != nil {
				return fmt.Errorf("command definitions: %w", err)
			}
		}
		for _, id := range definitions {
			if id == "issue" {
				return fmt.Errorf("command definitions must reference frozen files")
			}
		}
	}
	if err := p.validateDefinitionRepairs(); err != nil {
		return err
	}
	if err := p.validateProtectedDeltas(paths, cited); err != nil {
		return err
	}
	if err := p.validateFormulaBaseline(); err != nil {
		return err
	}
	if err := validateCodingAllowlist(p.CodingAllowlist); err != nil {
		return err
	}
	if p.HarnessTimeoutMS < 0 || p.HarnessTimeoutMS > MaxHarnessTimeoutMS {
		return fmt.Errorf("harnessTimeoutMs must be an integer from 1 to %d", MaxHarnessTimeoutMS)
	}
	if !required {
		return fmt.Errorf("at least one required check is necessary")
	}
	if p.Revision > 1 && len(p.Artifacts) > 0 && !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(p.PausedArtifactsSHA256) {
		return fmt.Errorf("amended artifact inventory must be bound to approval")
	}
	for _, r := range p.Artifacts {
		if _, _, _, err := r.Limits(); err != nil {
			return err
		}
		if err := ValidatePath(r.Path); err != nil {
			return err
		}
		if !ids[r.CommandID] {
			return fmt.Errorf("artifact needs an approved command")
		}
		for s := range paths {
			if s == r.Path || strings.HasPrefix(s, r.Path+"/") {
				return fmt.Errorf("artifact allowance overlaps approved source")
			}
		}
	}
	for _, s := range append(append([]string{}, a.Unknowns...), in.MandatoryConstraints...) {
		if !textOK(s) {
			return fmt.Errorf("empty unknown/constraint")
		}
	}
	return nil
}

// Digest intentionally excludes the issue's update timestamp: identity/content
// changes invalidate approval, a metadata-only timestamp observation does not.
func (p Plan) Digest() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	p.Inputs.Issue.UpdatedAt = ""
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

// Zero selects the version-one bound; explicit values may only reduce it.
func (a ArtifactRoot) Limits() (int, int64, int, error) {
	files, bytes, depth := a.MaxFiles, a.MaxBytes, a.MaxDepth
	if files == 0 {
		files = 100000
	}
	if bytes == 0 {
		bytes = 1000000000
	}
	if depth == 0 {
		depth = 32
	}
	if files < 1 || files > 100000 || bytes < 1 || bytes > 1000000000 || depth < 1 || depth > 32 {
		return 0, 0, 0, fmt.Errorf("artifact bounds must be positive and within v1 limits")
	}
	return files, bytes, depth, nil
}

// ConventionalTestPath is shared with the protected-tree builder. It identifies
// preserved regression sources, never independently reviewed acceptance.
func ConventionalTestPath(file string) bool {
	lower := strings.ToLower(file)
	base := path.Base(lower)
	for _, part := range strings.Split(lower, "/") {
		switch part {
		case "test", "tests", "__tests__", "testdata", "fixtures", "spec", "specs":
			return true
		}
	}
	return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.py") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

type DefinitionOverlap struct {
	Path     string   `json:"path"`
	CheckIDs []string `json:"checkIds"`
	Kind     string   `json:"kind"`
	Detail   string   `json:"detail"`
}

// DefinitionOverlaps is an assessment, separate from structural validity. Scope
// is a proposed edit, not proof that frozen bytes have already changed.
func (p Plan) DefinitionOverlaps() []DefinitionOverlap {
	refs := map[string]string{}
	for _, e := range p.Evidence {
		refs[e.ID] = e.Path
	}
	byPath := map[string][]string{}
	tests := []string{}
	for _, c := range append(append([]Check{}, p.Analysis.Checks...), p.Setup...) {
		for _, id := range c.Definitions {
			byPath[refs[id]] = append(byPath[refs[id]], c.ID)
		}
		// Commands can invoke tests indirectly; conservatively report every check.
		if c.Category != "setup" {
			tests = append(tests, c.ID)
		}
	}
	out := []DefinitionOverlap{}
	for _, scoped := range p.Analysis.Scope {
		definitions := byPath[scoped.Path]
		if len(definitions) > 0 {
			out = append(out, DefinitionOverlap{scoped.Path, definitions, "definition", "Frozen definition may change: candidate verification requires an explicit reviewedAcceptance repair tied to a required human-supplied frozen acceptance check; retain regression definitions and protected gates."})
		}
		if ConventionalTestPath(scoped.Path) && len(tests) > 0 {
			out = append(out, DefinitionOverlap{scoped.Path, tests, "test-source", "Candidate-authored tests remain candidate evidence; regression checks also run with protected baseline test sources. This assessment does not block ordinary test edits."})
		}
	}
	return out
}

func (p Plan) validateDefinitionRepairs() error {
	inputs := map[string]bool{}
	for _, input := range p.CheckInputs {
		inputs[input.ID] = true
	}
	commands := append(append([]Check{}, p.Analysis.Checks...), p.Setup...)
	overlaps := p.DefinitionOverlaps()
	for _, c := range commands {
		// Every input argument must cite the exact frozen evidence identity, via
		// definitions or evidence. Supplying bytes establishes no review provenance.
		for _, arg := range c.Argv {
			if strings.HasPrefix(arg, "{input:") {
				id := strings.TrimSuffix(strings.TrimPrefix(arg, "{input:"), "}")
				if arg != "{input:"+id+"}" || !inputs[id] || (!containsID(c.Definitions, id) && !containsID(c.Evidence, id)) {
					return fmt.Errorf("command %s has unbound reviewed input %s", c.ID, arg)
				}
			}
		}
		if c.ReviewedAcceptance == "" {
			continue
		}
		if c.Category != "regression" {
			return fmt.Errorf("definition repair %s must retain a protected regression check", c.ID)
		}
		overlap := false
		for _, o := range overlaps {
			if o.Kind == "definition" && containsID(o.CheckIDs, c.ID) {
				overlap = true
			}
		}
		if !overlap {
			return fmt.Errorf("definition repair %s has no scoped definition overlap", c.ID)
		}
		found := false
		for _, acceptance := range p.Analysis.Checks {
			if acceptance.ID != c.ReviewedAcceptance {
				continue
			}
			if acceptance.Category != "independent-acceptance" || !acceptance.Required || !textOK(acceptance.IndependentProvenance) || acceptance.ReviewedAcceptance != "" {
				return fmt.Errorf("definition repair %s requires a separately reviewed required acceptance check", c.ID)
			}
			external := false
			for _, id := range acceptance.Definitions {
				if inputs[id] && containsID(acceptance.Argv, "{input:"+id+"}") {
					external = true
				}
			}
			if !external {
				return fmt.Errorf("definition repair %s requires a frozen human acceptance input argument", c.ID)
			}
			for _, o := range overlaps {
				if o.Kind == "definition" && containsID(o.CheckIDs, acceptance.ID) {
					return fmt.Errorf("acceptance repair itself overlaps scoped source: %s", acceptance.ID)
				}
			}
			found = true
		}
		if !found {
			return fmt.Errorf("definition repair %s names missing acceptance check %s", c.ID, c.ReviewedAcceptance)
		}
	}
	return nil
}
func containsID(ids []string, id string) bool {
	for _, value := range ids {
		if value == id {
			return true
		}
	}
	return false
}

var goPackagePath = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~+/-]{0,255}$`)

// validGoTestName accepts one exact test ID as reported by go test -json:
// Test/sub/case with no empty segment, whitespace or wildcard.
func validGoTestName(name string) bool {
	if name == "" || len(name) > 512 || strings.Contains(name, "...") || strings.ContainsAny(name, "*?\\") {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" {
			return false
		}
	}
	top := parts[0]
	return strings.HasPrefix(top, "Test") || strings.HasPrefix(top, "Fuzz") || strings.HasPrefix(top, "Example")
}

// requestsGoTestJSON reports whether a check argv asks go test for -json output,
// directly or inside a reviewed shell string. Matching needs that stream.
func requestsGoTestJSON(argv []string) bool {
	for _, arg := range argv {
		for _, token := range strings.Fields(arg) {
			switch token {
			case "-json", "--json", "-json=true", "--json=true":
				return true
			}
		}
	}
	return false
}

var failfastEnvPrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
var failfastFlag = regexp.MustCompile(`^--?(?:test\.)?failfast(?:=(.*))?$`)

// requestsGoTestFailfast reports whether a check argv asks go test to stop after
// the first failure, as -failfast or -test.failfast, with or without a boolean
// value, directly, inside a reviewed shell string or through an environment
// assignment such as GOFLAGS=-failfast. Only an explicit false value is ignored.
func requestsGoTestFailfast(argv []string) bool {
	for _, arg := range argv {
		for _, token := range strings.Fields(arg) {
			token = failfastEnvPrefix.ReplaceAllString(strings.Trim(token, "\"'`;()"), "")
			m := failfastFlag.FindStringSubmatch(strings.Trim(token, "\"'`;()"))
			if m == nil {
				continue
			}
			switch strings.ToLower(m[1]) {
			case "0", "f", "false":
				continue
			}
			return true
		}
	}
	return false
}

// RequestsGoTestJSON is requestsGoTestJSON for the verification runner, which
// streams the output of checks that ask for it.
func RequestsGoTestJSON(argv []string) bool { return requestsGoTestJSON(argv) }

// protectedTestSource mirrors what the protected view restores from the base
// commit: conventional test paths and every regression check definition.
func (p Plan) protectedTestSource(file string) bool {
	if ConventionalTestPath(file) {
		return true
	}
	ids := map[string]bool{}
	for _, e := range p.Evidence {
		if e.Path == file {
			ids[e.ID] = true
		}
	}
	for _, c := range p.Analysis.Checks {
		if c.Category != "regression" {
			continue
		}
		for _, id := range c.Definitions {
			if ids[id] {
				return true
			}
		}
	}
	return false
}

func (p Plan) validateProtectedDeltas(scope map[string]bool, cited func([]string) error) error {
	if len(p.ExpectedProtectedDeltas) == 0 {
		return nil
	}
	if len(p.ExpectedProtectedDeltas) > MaxProtectedDeltas {
		return fmt.Errorf("at most %d expected protected deltas permitted", MaxProtectedDeltas)
	}
	checks := map[string]Check{}
	for _, c := range p.Analysis.Checks {
		checks[c.ID] = c
	}
	seen := map[string]bool{}
	for _, d := range p.ExpectedProtectedDeltas {
		c, ok := checks[d.CheckID]
		if !ok || c.Category != "regression" || !c.Required {
			return fmt.Errorf("expected protected delta %s %s needs a required regression check, not %q", d.Package, d.Test, d.CheckID)
		}
		if !requestsGoTestJSON(c.Argv) {
			return fmt.Errorf("expected protected delta %s %s needs check %s to request go test -json", d.Package, d.Test, d.CheckID)
		}
		if requestsGoTestFailfast(c.Argv) {
			return fmt.Errorf("expected protected delta %s %s cannot use check %s: it requests -failfast, so tests after the first failure never run and a declared failure could mask them", d.Package, d.Test, d.CheckID)
		}
		if !goPackagePath.MatchString(d.Package) || strings.Contains(d.Package, "...") || strings.Contains(d.Package, "//") {
			return fmt.Errorf("expected protected delta needs an exact Go package import path: %q", d.Package)
		}
		if !validGoTestName(d.Test) {
			return fmt.Errorf("expected protected delta needs an exact test name without wildcards: %q", d.Test)
		}
		key := d.CheckID + "\x00" + d.Package + "\x00" + d.Test
		if seen[key] {
			return fmt.Errorf("duplicate expected protected delta: %s %s", d.Package, d.Test)
		}
		seen[key] = true
		if !scope[d.ScopePath] {
			return fmt.Errorf("expected protected delta %s %s cites a path outside the approved scope: %q", d.Package, d.Test, d.ScopePath)
		}
		if !p.protectedTestSource(d.ScopePath) {
			return fmt.Errorf("expected protected delta %s %s cites a path that is not a protected test source: %q", d.Package, d.Test, d.ScopePath)
		}
		if !textOK(d.Reason) {
			return fmt.Errorf("expected protected delta %s %s needs a reason", d.Package, d.Test)
		}
		if err := cited(d.Evidence); err != nil {
			return fmt.Errorf("expected protected delta %s %s: %w", d.Package, d.Test, err)
		}
	}
	return nil
}

// validateFormulaBaseline requires every listed baseline id to be unique and to name
// a required regression check without a reviewed-acceptance repair.
func (p Plan) validateFormulaBaseline() error {
	if len(p.FormulaBaseline) > len(p.Analysis.Checks) {
		return fmt.Errorf("formulaBaseline lists more checks than the plan contains")
	}
	checks := map[string]Check{}
	for _, c := range p.Analysis.Checks {
		checks[c.ID] = c
	}
	listed := map[string]bool{}
	for _, id := range p.FormulaBaseline {
		c, ok := checks[id]
		if listed[id] || !ok || c.Category != "regression" || !c.Required || c.ReviewedAcceptance != "" {
			return fmt.Errorf("formulaBaseline %q must be listed once and name a required regression check", id)
		}
		listed[id] = true
	}
	return nil
}

// DeltaPackageDir is the Go package directory a protected test source belongs
// to: its directory, or the parent of a testdata tree (Go ignores testdata).
func DeltaPackageDir(scopePath string) string {
	dir := pathpkg.Dir(scopePath)
	if i := strings.Index(dir+"/", "/testdata/"); i >= 0 {
		return dir[:i]
	}
	if strings.HasPrefix(dir+"/", "testdata/") {
		return "."
	}
	return dir
}

// DeltasForCheck returns the declared protected deltas of one check, in plan order.
func (p Plan) DeltasForCheck(id string) []ProtectedDelta {
	var out []ProtectedDelta
	for _, d := range p.ExpectedProtectedDeltas {
		if d.CheckID == id {
			out = append(out, d)
		}
	}
	return out
}

// IsFixturePath identifies testdata and fixtures directories, where pinned
// contract files usually live.
func IsFixturePath(file string) bool {
	for _, part := range strings.Split(strings.ToLower(file), "/") {
		if part == "testdata" || part == "fixtures" {
			return true
		}
	}
	return false
}

// FixtureScopePaths lists approved scope paths under testdata or fixtures.
func (p Plan) FixtureScopePaths() []string {
	var out []string
	for _, s := range p.Analysis.Scope {
		if IsFixturePath(s.Path) {
			out = append(out, s.Path)
		}
	}
	return out
}

// FailingTest identifies one Go test, by package import path and test name, that
// failed in a protected regression run. It is evidence for a reviewer, not a delta.
type FailingTest struct {
	Package string
	Test    string
}

// maxAdvisoryTests bounds how many failing tests the advisory names.
const maxAdvisoryTests = 20

func failingTestsSentence(seen []FailingTest) string {
	unique := map[FailingTest]bool{}
	for _, t := range seen {
		unique[t] = true
	}
	tests := make([]FailingTest, 0, len(unique))
	for t := range unique {
		tests = append(tests, t)
	}
	sort.Slice(tests, func(i, j int) bool {
		if tests[i].Package != tests[j].Package {
			return tests[i].Package < tests[j].Package
		}
		return tests[i].Test < tests[j].Test
	})
	if len(tests) == 0 {
		return ""
	}
	more := ""
	if len(tests) > maxAdvisoryTests {
		more = fmt.Sprintf(" and %d more", len(tests)-maxAdvisoryTests)
		tests = tests[:maxAdvisoryTests]
	}
	names := make([]string, len(tests))
	for i, t := range tests {
		names[i] = t.Package + " " + t.Test
	}
	return " The latest protected regression run failed these tests (package and test ID): " + strings.Join(names, "; ") + more + "."
}

// ProtectedDeltaAdvisory is a deterministic prompt for a human reviewer. It is
// returned only when scope touches fixture paths, a regression check will run in
// the protected view and no delta is declared. It never declares or approves one.
// Failing tests recorded from an earlier protected run are named in the text, so a
// reviewer can copy exact IDs; they change nothing else.
func (p Plan) ProtectedDeltaAdvisory(seen ...FailingTest) string {
	if len(p.ExpectedProtectedDeltas) > 0 {
		return ""
	}
	regression := false
	for _, c := range p.Analysis.Checks {
		if c.Category == "regression" && c.Required {
			regression = true
		}
	}
	fixtures := p.FixtureScopePaths()
	if !regression || len(fixtures) == 0 {
		return ""
	}
	return fmt.Sprintf("Protected-delta advisory (proposed for review; never auto-approved): scope touches fixture path(s) %s. The protected regression run restores base-commit test sources, so tests that pin these files may fail there and block the Molecule.%s If this change intentionally alters a pinned contract, a reviewer may amend the plan with expectedProtectedDeltas naming exact Go package and test IDs, declaring a failing parent test beside its failing subtest (an undeclared failing parent still fails the check); nothing is declared or approved automatically.", strings.Join(fixtures, ", "), failingTestsSentence(seen))
}

// ValidateDirectDeltaChecks requires every go test -json check that declares
// protected deltas to be a direct go test argv (argv[0] go, argv[1] test), so the
// Lab sees every flag. A shell, env or script wrapper can hide -failfast, which
// stops later tests from running and lets a declared failure stand in for them.
// It runs when a plan is amended or approved, not in Validate: the verification
// runner validates stored plans with Validate, which keeps every earlier plan runnable.
func (p Plan) ValidateDirectDeltaChecks() error {
	checks := map[string]Check{}
	for _, c := range p.Analysis.Checks {
		checks[c.ID] = c
	}
	for _, d := range p.ExpectedProtectedDeltas {
		c, ok := checks[d.CheckID]
		if !ok || !requestsGoTestJSON(c.Argv) {
			continue
		}
		if len(c.Argv) < 2 || c.Argv[0] != "go" || c.Argv[1] != "test" {
			return fmt.Errorf("expected protected delta %s %s needs check %s to be a direct `go test` command (argv[0] go, argv[1] test), not a shell, env or script wrapper, because a wrapper can hide flags such as -failfast that stop later tests from running", d.Package, d.Test, d.CheckID)
		}
	}
	return nil
}

// ValidateForCoding leaves overlapping proposals digestible and reviewable, but
// prevents an exact approval from starting coding under unusable frozen checks.
func (p Plan) ValidateForCoding() error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := p.ValidateDirectDeltaChecks(); err != nil {
		return err
	}
	blocked := []string{}
	for _, o := range p.DefinitionOverlaps() {
		if o.Kind != "definition" {
			continue
		}
		for _, id := range o.CheckIDs {
			for _, c := range append(append([]Check{}, p.Analysis.Checks...), p.Setup...) {
				if c.ID == id && c.ReviewedAcceptance == "" {
					blocked = append(blocked, fmt.Sprintf("%s (%s)", o.Path, id))
				}
			}
		}
	}
	if len(blocked) > 0 {
		return fmt.Errorf("frozen definition overlaps proposed scope: %s; review a scope/check amendment with separately frozen human acceptance input and reviewedAcceptance; retain protected regression gates", strings.Join(blocked, "; "))
	}
	return nil
}

// MaxEvidenceRequests bounds the evidence requests of one analysis response.
const MaxEvidenceRequests = 16

// EvidenceRequestCapMessage is the exact validator message for an over-cap response.
func EvidenceRequestCapMessage() string {
	return fmt.Sprintf("at most %d evidence requests permitted", MaxEvidenceRequests)
}

// ValidateEvidenceRequestCap rejects an over-cap response with a PolicyViolation:
// the analysis is complete and well-formed, and a corrective pass can trim it.
func ValidateEvidenceRequestCap(requests []EvidenceRequest) error {
	if len(requests) > MaxEvidenceRequests {
		return &PolicyViolation{Reason: EvidenceRequestCapMessage()}
	}
	return nil
}

func ValidateEvidenceRequests(requests []EvidenceRequest, evidence []Evidence) error {
	if err := ValidateEvidenceRequestCap(requests); err != nil {
		return err
	}
	refs := map[string]bool{"issue": true}
	for _, e := range evidence {
		refs[e.ID] = true
	}
	seen := map[string]bool{}
	for _, r := range requests {
		if err := ValidatePath(r.Path); err != nil {
			return err
		}
		key := strings.ToLower(r.Path)
		if seen[key] || !textOK(r.Reason) || len(r.Evidence) == 0 || len(r.Evidence) > 256 {
			return fmt.Errorf("invalid or duplicate evidence request: %s", r.Path)
		}
		seen[key] = true
		cited := map[string]bool{}
		for _, id := range r.Evidence {
			if !identifier.MatchString(id) || cited[id] || (evidence != nil && !refs[id]) {
				return fmt.Errorf("invalid evidence request citation: %s", id)
			}
			cited[id] = true
		}
	}
	return nil
}
