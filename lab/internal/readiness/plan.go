// Package readiness describes reviewable ticket scope. It never invokes a model,
// grants permissions, executes commands or imports the Molecule orchestrator.
package readiness

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
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
}

var hex256 = regexp.MustCompile(`^[a-f0-9]{64}$`)
var gitHash = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

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
		if c.Category == "independent-acceptance" || c.IndependentProvenance != "" {
			return a, fmt.Errorf("analysis cannot assert independent acceptance review; retain as a candidate check for separate human review")
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
	for _, e := range p.Evidence {
		if !identifier.MatchString(e.ID) || refs[e.ID] || !hex256.MatchString(e.SHA256) {
			return fmt.Errorf("invalid or duplicate evidence identity: %s", e.ID)
		}
		if err := ValidatePath(e.Path); err != nil {
			return err
		}
		refs[e.ID] = true
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
		if err := cited(c.Definitions); err != nil {
			return fmt.Errorf("command definitions: %w", err)
		}
		for _, id := range c.Definitions {
			if id == "issue" {
				return fmt.Errorf("command definitions must reference frozen files")
			}
		}
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

func ValidateEvidenceRequests(requests []EvidenceRequest, evidence []Evidence) error {
	if len(requests) > 16 {
		return fmt.Errorf("at most 16 evidence requests permitted")
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
