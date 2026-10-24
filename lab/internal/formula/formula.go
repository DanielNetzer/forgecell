// Package formula validates durable recipes and retains their exact approved bytes.
package formula

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go.yaml.in/yaml/v4"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Binding struct {
	Binding      string   `yaml:"binding"`
	Command      []string `yaml:"command"`
	TimeoutMS    int      `yaml:"timeoutMs"`
	Instructions string   `yaml:"instructions"`
	// MaxTimeoutMS bounds a reviewed per-Molecule coding timeout. The Binding is
	// JSON-hashed untagged, so omitempty keeps existing binding hashes unchanged.
	MaxTimeoutMS int `yaml:"maxTimeoutMs" json:"maxTimeoutMs,omitempty"`
	// AnalysisWrapper separately pins a read-only ticket-analysis wrapper beside
	// the coding command. A nil pointer is omitted, so existing binding hashes
	// and historical Formulas are unchanged.
	AnalysisWrapper *AnalysisWrapperRef `yaml:"analysisWrapper" json:"analysisWrapper,omitempty"`
	// Extensions names declared Formula extensions this binding attaches. omitempty
	// keeps binding hashes of extension-free Formulas unchanged.
	Extensions []string `yaml:"extensions" json:"extensions,omitempty"`
	// Model pins the provider model of a native Claude binding. A role override
	// never inherits the default binding's model, because a model name is only
	// meaningful for the provider that binding names. omitempty keeps the Formula
	// bytes, SHA and Binding JSON of unpinned Formulas unchanged.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
}

// AnalysisWrapperRef names a Lab-relative manifest and its exact SHA-256. The
// reference is covered by the exact-byte Formula approval.
type AnalysisWrapperRef struct {
	Manifest string `yaml:"manifest" json:"manifest"`
	SHA256   string `yaml:"sha256" json:"sha256"`
}

var wrapperDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validateAnalysisWrapper(binding map[string]any, value any) error {
	m, ok := value.(map[string]any)
	if !ok || len(m) != 2 {
		return fmt.Errorf("analysisWrapper must be a mapping with exactly manifest and sha256")
	}
	manifest, _ := m["manifest"].(string)
	digest, _ := m["sha256"].(string)
	if manifest == "" || len(manifest) > 512 || strings.ContainsAny(manifest, "\\\x00") || !filepath.IsLocal(manifest) || filepath.ToSlash(filepath.Clean(manifest)) != manifest {
		return fmt.Errorf("analysisWrapper manifest must be a clean Lab-relative path")
	}
	if !wrapperDigest.MatchString(digest) {
		return fmt.Errorf("analysisWrapper sha256 must be a lowercase SHA-256")
	}
	command, _ := binding["command"].([]any)
	if len(command) == 0 {
		return fmt.Errorf("analysisWrapper requires an existing coding command in the same binding")
	}
	if len(command) == 4 && command[1] == "__adapter" {
		return fmt.Errorf("analysisWrapper is refused on a native adapter binding; native analysis is never substituted")
	}
	return nil
}

const maxFormulaTimeoutMS = 3600000

// TimeoutCeilingMS is the largest coding timeout a plan may carry: the declared
// maximum, or the Formula timeout itself when no extension is granted.
func (b Binding) TimeoutCeilingMS() int {
	if b.MaxTimeoutMS > 0 {
		return max(b.MaxTimeoutMS, b.TimeoutMS)
	}
	return b.TimeoutMS
}

// CheckTimeout validates a plan's per-Molecule timeout; zero means "use the Formula timeout".
func (b Binding) CheckTimeout(ms int) error {
	if ms == 0 || ms <= b.TimeoutCeilingMS() {
		return nil
	}
	return fmt.Errorf("harnessTimeoutMs %d exceeds the Formula maximum of %d ms (timeoutMs %d, maxTimeoutMs %d)", ms, b.TimeoutCeilingMS(), b.TimeoutMS, b.MaxTimeoutMS)
}

type Atom struct {
	Purpose   string   `yaml:"purpose"`
	ID        string   `yaml:"id"`
	Type      string   `yaml:"type"`
	Command   []string `yaml:"command"`
	TimeoutMS int      `yaml:"timeoutMs"`
	Source    string   `yaml:"source"`
	Binding   string   `yaml:"binding"`
	Workflows []string `yaml:"workflows"`
	// Mode and Policy are filled from the raw mapping by Parse, so an existing
	// Formula with an unusual `mode` value still decodes. See gate_policy.go.
	Mode   string `yaml:"-"`
	Policy string `yaml:"-"`
}
type Intake struct {
	Source string `yaml:"source"`
	Repo   string `yaml:"repo"`
}
type Formula struct {
	SchemaVersion string  `yaml:"schemaVersion"`
	Kind          string  `yaml:"kind"`
	ID            string  `yaml:"id"`
	Name          string  `yaml:"name"`
	Harness       Binding `yaml:"harness"`
	// Harnesses holds optional per-role bindings; a role not declared here uses Harness.
	Harnesses map[string]Binding `yaml:"harnesses" json:"harnesses,omitempty"`
	Intake    Intake             `yaml:"intake"`
	Atoms     []Atom             `yaml:"atoms"`
	// RequiredChecks is the reviewed CI baseline every plan must include. Absent
	// keeps the Formula bytes, SHA and behaviour of earlier Formulas unchanged.
	RequiredChecks []RequiredCheck `yaml:"requiredChecks" json:"requiredChecks,omitempty"`
	// Extensions declares pinned extension manifests by name; roles bind them by name.
	// Absent keeps the Formula bytes, SHA and behaviour of earlier Formulas unchanged.
	Extensions map[string]ExtensionRef `yaml:"extensions" json:"extensions,omitempty"`
	// Learning holds optional learning settings. Absent keeps the Formula bytes,
	// SHA and behaviour of earlier Formulas unchanged.
	Learning *LearningSettings `yaml:"learning" json:"learning,omitempty"`
	YAML     string            `yaml:"-"`
	SHA256   string            `yaml:"-"`
}
type Loaded struct {
	Formula  Formula
	Approved bool
	Approval Approval
	File     string
}

func validateCommand(m map[string]any) error {
	if value, ok := m["command"]; ok {
		args, ok := value.([]any)
		if !ok || len(args) == 0 {
			return fmt.Errorf("command must be a non-empty argv array")
		}
		for _, arg := range args {
			s, ok := arg.(string)
			if !ok || strings.TrimSpace(s) == "" || strings.ContainsRune(s, 0) {
				return fmt.Errorf("command argv must contain non-empty strings")
			}
		}
	}
	if value, ok := m["timeoutMs"]; ok {
		n, ok := value.(int)
		if !ok || n < 1 || n > 3600000 {
			return fmt.Errorf("timeoutMs must be an integer from 1 to 3600000")
		}
	}
	return nil
}

// MaxModelLength bounds a pinned model identifier.
const MaxModelLength = 128

// ValidateModel checks a pinned model identifier. It is passed to the provider as
// one argv value, so it must be non-blank, short, free of whitespace and control
// characters, and must not look like a flag.
func ValidateModel(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("model must not be empty or blank")
	}
	if len(s) > MaxModelLength {
		return fmt.Errorf("model must be at most %d bytes", MaxModelLength)
	}
	if !utf8.ValidString(s) || strings.HasPrefix(s, "-") || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("model must not start with '-' or contain whitespace or control characters")
	}
	return nil
}

// validateBindingMapping holds the rules shared by `harness` and every `harnesses` role.
func validateBindingMapping(m map[string]any) error {
	if _, exists := m["policy"]; exists {
		return fmt.Errorf("a binding cannot declare a policy; binding changes always require human review")
	}
	if err := validateCommand(m); err != nil {
		return err
	}
	if value, ok := m["model"]; ok {
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("model must be a string")
		}
		if err := ValidateModel(s); err != nil {
			return err
		}
	}
	if value, ok := m["maxTimeoutMs"]; ok {
		n, ok := value.(int)
		if !ok || n < 1 || n > maxFormulaTimeoutMS {
			return fmt.Errorf("maxTimeoutMs must be an integer from 1 to %d", maxFormulaTimeoutMS)
		}
		if timeout, _ := m["timeoutMs"].(int); n < timeout {
			return fmt.Errorf("maxTimeoutMs must not be below timeoutMs")
		}
	}
	if value, ok := m["instructions"]; ok {
		s, ok := value.(string)
		if !ok || strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > 8000 {
			return fmt.Errorf("instructions must be non-empty bounded text")
		}
	}
	return nil
}

// HarnessRole names a harness-bound activity that may carry its own binding.
type HarnessRole string

const (
	AnalysisHarness HarnessRole = "analysis"
	CodingHarness   HarnessRole = "coding"
	MetaHarness     HarnessRole = "meta"
)

// HarnessRoles lists the roles a Formula may bind, in display order.
var HarnessRoles = []HarnessRole{AnalysisHarness, CodingHarness, MetaHarness}

// ReviewHarness binds the read-only adversarial review. It is deliberately not in
// HarnessRoles: it has no default, so it never resolves to the coding binding, and
// a Formula without a review Atom has no review role at all.
const ReviewHarness HarnessRole = "review"

func validateHarnessRoles(value any) error {
	roles, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("harnesses must be a mapping of role to binding")
	}
	for name, v := range roles {
		known := name == string(ReviewHarness)
		for _, role := range HarnessRoles {
			known = known || name == string(role)
		}
		if !known {
			return fmt.Errorf("harnesses.%s: unknown role; use analysis, coding, meta or review", name)
		}
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("harnesses.%s must be a mapping", name)
		}
		if err := validateBindingMapping(m); err != nil {
			return fmt.Errorf("harnesses.%s: %w", name, err)
		}
		if value, ok := m["analysisWrapper"]; ok {
			if name != "analysis" {
				return fmt.Errorf("harnesses.%s: analysisWrapper is only valid on the analysis role", name)
			}
			if err := validateAnalysisWrapper(m, value); err != nil {
				return fmt.Errorf("harnesses.%s: %w", name, err)
			}
		}
		if id, ok := m["binding"].(string); !ok || strings.TrimSpace(id) == "" {
			return fmt.Errorf("harnesses.%s: binding must be a non-empty string", name)
		}
		if _, ok := m["command"]; !ok {
			return fmt.Errorf("harnesses.%s: command must be a non-empty argv array", name)
		}
	}
	return nil
}

// RoleBinding resolves a role to its declared binding, or to the default `harness`.
// Every override inherits the default timeout it omits, so no role silently falls
// back to the process default; a coding override also inherits the approved
// instructions and maximum. The parsed Harnesses and the Formula bytes stay as approved.
func (f Formula) RoleBinding(role HarnessRole) (Binding, bool) {
	b, ok := f.Harnesses[string(role)]
	if !ok {
		if role == ReviewHarness {
			// The coding harness must not review its own change, so review has no default.
			return Binding{}, false
		}
		return f.Harness, false
	}
	if role == CodingHarness && b.Instructions == "" {
		b.Instructions = f.Harness.Instructions
	}
	if b.TimeoutMS == 0 {
		b.TimeoutMS = f.Harness.TimeoutMS
		if b.MaxTimeoutMS > 0 {
			b.TimeoutMS = min(b.TimeoutMS, b.MaxTimeoutMS)
		}
		if role == CodingHarness && b.MaxTimeoutMS == 0 {
			b.MaxTimeoutMS = f.Harness.MaxTimeoutMS
		}
	}
	return b, true
}

// processDefaultTimeoutMS mirrors the 10-minute timeout the process runner
// applies when a binding carries none.
const processDefaultTimeoutMS = 600000

// EffectiveTimeoutMS is the timeout an execution applies: the declared timeoutMs,
// or, when none is declared and maxTimeoutMs is below the process default, that
// maximum, so a run cannot outlast it. Zero still lets the process layer apply its
// default. The binding itself keeps what the Formula declares, so a binding hash
// computed before this clamp existed is unchanged.
func (b Binding) EffectiveTimeoutMS() int {
	if b.TimeoutMS == 0 && b.MaxTimeoutMS > 0 && b.MaxTimeoutMS < processDefaultTimeoutMS {
		return b.MaxTimeoutMS
	}
	return b.TimeoutMS
}

// ExecutionBinding resolves the binding that runs an execution role: intake
// analysis, coding or learning (meta). Other execution roles are not harness-bound.
func (f Formula) ExecutionBinding(role ExecutionRole) (Binding, bool) {
	switch role {
	case IntakeRole:
		b, _ := f.RoleBinding(AnalysisHarness)
		return b, true
	case Coding:
		b, _ := f.RoleBinding(CodingHarness)
		return b, true
	case Learning:
		b, _ := f.RoleBinding(MetaHarness)
		return b, true
	}
	return Binding{}, false
}

// ReviewAtom returns the Formula's review Atom, found by type. More than one is an
// error. A Formula without one has no review.
func (f Formula) ReviewAtom() (Atom, bool, error) {
	var found []Atom
	for _, a := range f.Atoms {
		if a.Type == "review" {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 0:
		return Atom{}, false, nil
	case 1:
		return found[0], true, nil
	}
	return Atom{}, false, fmt.Errorf("a Formula can have only one review Atom; found %d", len(found))
}

// ValidateReviewAtom checks the review Atom and the harnesses.review role against
// each other. The Atom takes its binding from harnesses.review or the meta binding,
// so it cannot carry a command; a role without the Atom is refused. A Formula with
// neither passes.
func (f Formula) ValidateReviewAtom() error {
	atom, present, err := f.ReviewAtom()
	if err != nil {
		return err
	}
	role, declared := f.Harnesses[string(ReviewHarness)]
	if declared && !present {
		return fmt.Errorf("harnesses.review: the Formula has no review Atom; add the review Atom or remove the role binding")
	}
	if !present {
		return nil
	}
	if len(atom.Command) > 0 {
		return fmt.Errorf("review Atom %s cannot declare a command; bind harnesses.review or the meta binding instead", atom.ID)
	}
	if declared && atom.Binding != "" && atom.Binding != role.Binding {
		return fmt.Errorf("harnesses.review binding %q conflicts with Atom %s binding %q", role.Binding, atom.ID, atom.Binding)
	}
	return nil
}

// ValidateHarnessRoles checks that each declared role has its Atom and does not
// contradict that Atom's own binding. A Formula without `harnesses` always passes.
func (f Formula) ValidateHarnessRoles() error {
	if err := f.ValidateReviewAtom(); err != nil {
		return err
	}
	if len(f.Harnesses) == 0 {
		return nil
	}
	atoms, err := f.ExecutionRoles()
	if err != nil {
		return err
	}
	for _, role := range HarnessRoles {
		b, declared := f.Harnesses[string(role)]
		if !declared {
			continue
		}
		var atom Atom
		var present bool
		switch role {
		case AnalysisHarness:
			atom, present = atoms[IntakeRole]
		case CodingHarness:
			atom, present = atoms[Coding]
		case MetaHarness:
			atom, present = atoms[Learning]
		}
		if !present {
			return fmt.Errorf("harnesses.%s: the Formula has no Atom for this role; add the learn Atom or remove the role binding", role)
		}
		if atom.Binding != "" && atom.Binding != b.Binding {
			return fmt.Errorf("harnesses.%s binding %q conflicts with Atom %s binding %q", role, b.Binding, atom.ID, atom.Binding)
		}
		if role == MetaHarness && len(atom.Command) > 0 && !slices.Equal(atom.Command, b.Command) {
			return fmt.Errorf("harnesses.meta command conflicts with the command of learn Atom %s", atom.ID)
		}
	}
	return nil
}

func Parse(data []byte) (Formula, error) {
	var f Formula
	if len(data) > 1_000_000 {
		return f, fmt.Errorf("Formula exceeds 1 MB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return f, err
	}
	if raw == nil {
		return f, fmt.Errorf("Formula must be a mapping")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return f, fmt.Errorf("Formula must contain exactly one YAML document")
	}
	if raw["kind"] != "formula" {
		return f, fmt.Errorf("kind must be formula")
	}
	if v, ok := raw["schemaVersion"]; ok && v != "v0" {
		return f, fmt.Errorf("unsupported Formula schema")
	}
	id, ok := raw["id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return f, fmt.Errorf("Formula id is required")
	}
	atoms, ok := raw["atoms"].([]any)
	if !ok || len(atoms) == 0 {
		return f, fmt.Errorf("Formula needs at least one Atom")
	}
	for _, value := range atoms {
		m, ok := value.(map[string]any)
		if !ok {
			return f, fmt.Errorf("Atom must be a mapping")
		}
		if err := validateCommand(m); err != nil {
			return f, err
		}
		typ, ok := m["type"].(string)
		if !ok || strings.TrimSpace(typ) == "" {
			return f, fmt.Errorf("Atom type is required")
		}
		if err := validateGatePolicy(m); err != nil {
			return f, err
		}
	}
	if _, exists := raw["policy"]; exists {
		return f, fmt.Errorf("a Formula cannot declare a policy; Formula changes always require human review")
	}
	if value, exists := raw["harness"]; exists {
		m, ok := value.(map[string]any)
		if !ok {
			return f, fmt.Errorf("harness must be a mapping")
		}
		if err := validateBindingMapping(m); err != nil {
			return f, err
		}
		if value, ok := m["analysisWrapper"]; ok {
			if err := validateAnalysisWrapper(m, value); err != nil {
				return f, err
			}
		}
	}
	if value, exists := raw["harnesses"]; exists {
		if err := validateHarnessRoles(value); err != nil {
			return f, err
		}
	}
	if err := validateExtensions(raw); err != nil {
		return f, err
	}
	if value, exists := raw["requiredChecks"]; exists {
		if err := validateRequiredChecks(value); err != nil {
			return f, err
		}
	}
	if value, exists := raw["learning"]; exists {
		if err := validateLearning(value); err != nil {
			return f, err
		}
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return f, err
	}
	f.ID = strings.TrimSpace(f.ID)
	if f.Name == "" {
		f.Name = f.ID
	}
	if f.SchemaVersion == "" {
		f.SchemaVersion = "v0"
	}
	if f.Harness.Binding == "" {
		f.Harness.Binding = "unbound"
	}
	if f.Intake.Source == "" {
		f.Intake.Source = "github-issues"
	}
	seen := map[string]bool{}
	for i := range f.Atoms {
		a := &f.Atoms[i]
		if i < len(atoms) {
			if m, ok := atoms[i].(map[string]any); ok {
				a.Mode, _ = m["mode"].(string)
				a.Policy, _ = m["policy"].(string)
			}
		}
		a.Type = strings.TrimSpace(a.Type)
		a.ID = strings.TrimSpace(a.ID)
		if a.ID == "" {
			a.ID = fmt.Sprintf("%s-%d", a.Type, i+1)
		}
		if seen[a.ID] {
			return Formula{}, fmt.Errorf("duplicate Atom id: %s", a.ID)
		}
		seen[a.ID] = true
	}
	f.YAML = string(data)
	f.SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
	return f, nil
}

// releaseLauncher matches an installed release launcher: an absolute path that ends
// in /releases/<version>/forgecell. Its characters are limited so that the value
// appears literally in its YAML line in plain, single- or double-quoted style.
var releaseLauncher = regexp.MustCompile(`^(?:/[A-Za-z0-9._@+~ -]+)*/releases/[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?/forgecell$`)

const maxLauncherPathBytes = 4096

// IsReleaseLauncher reports whether p names a release launcher by a clean
// absolute path, so a "." or ".." segment can never satisfy it.
func IsReleaseLauncher(p string) bool {
	return len(p) <= maxLauncherPathBytes && filepath.IsAbs(p) && filepath.Clean(p) == p && releaseLauncher.MatchString(p)
}

// errNotLauncherOnly is deliberately uninformative: a caller reports its own
// refusal, and no Formula text is echoed.
var errNotLauncherOnly = fmt.Errorf("Formula change is not launcher-only")

// LauncherOnlyChange returns nil only when previous and next are both valid
// Formulas that differ solely in the launcher (argv[0]) of native
// [launcher, "__adapter", adapter, provider] commands, with the old and the new
// value both release launchers. Every other byte, including line structure,
// must be identical. Identical Formulas are refused: there is nothing to carry.
// Input is bounded by Parse's 1 MB limit and anchors or aliases are refused, so
// one line can never stand for several launcher positions.
func LauncherOnlyChange(previous, next []byte) error {
	before, err := Parse(previous)
	if err != nil {
		return errNotLauncherOnly
	}
	after, err := Parse(next)
	if err != nil || before.ID != after.ID || before.SHA256 == after.SHA256 {
		return errNotLauncherOnly
	}
	oldSites, err := launcherSites(previous)
	if err != nil {
		return err
	}
	newSites, err := launcherSites(next)
	if err != nil || len(oldSites) != len(newSites) {
		return errNotLauncherOnly
	}
	changed := map[int][2]string{}
	for i := range oldSites {
		o, n := oldSites[i], newSites[i]
		if o.Line != n.Line || o.Column != n.Column || o.Style != n.Style {
			return errNotLauncherOnly
		}
		if o.Value == n.Value {
			continue
		}
		if _, twice := changed[o.Line]; twice || !IsReleaseLauncher(o.Value) || !IsReleaseLauncher(n.Value) {
			return errNotLauncherOnly
		}
		changed[o.Line] = [2]string{o.Value, n.Value}
	}
	oldLines, newLines := bytes.Split(previous, []byte("\n")), bytes.Split(next, []byte("\n"))
	if len(oldLines) != len(newLines) {
		return errNotLauncherOnly
	}
	consumed := 0
	for i := range oldLines {
		if bytes.Equal(oldLines[i], newLines[i]) {
			continue
		}
		pair, ok := changed[i+1]
		if !ok || !sameExceptLauncher(string(oldLines[i]), string(newLines[i]), pair[0], pair[1]) {
			return errNotLauncherOnly
		}
		consumed++
	}
	if consumed != len(changed) {
		return errNotLauncherOnly
	}
	return nil
}

// sameExceptLauncher requires each launcher to occur exactly once in its line and
// the bytes around it to be identical.
func sameExceptLauncher(oldLine, newLine, oldValue, newValue string) bool {
	if strings.Count(oldLine, oldValue) != 1 || strings.Count(newLine, newValue) != 1 {
		return false
	}
	i, j := strings.Index(oldLine, oldValue), strings.Index(newLine, newValue)
	return oldLine[:i] == newLine[:j] && oldLine[i+len(oldValue):] == newLine[j+len(newValue):]
}

// launcherSites returns, in document order, the argv[0] node of every native
// command held by harness, harnesses.<role> or an Atom.
func launcherSites(data []byte) ([]*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errNotLauncherOnly
	}
	if hasAnchors(&doc, 0) {
		return nil, errNotLauncherOnly
	}
	root := doc.Content[0]
	var sites []*yaml.Node
	add := func(holder *yaml.Node) {
		args := mappingValue(holder, "command")
		if args != nil && args.Kind == yaml.SequenceNode && len(args.Content) == 4 && args.Content[0].Kind == yaml.ScalarNode && args.Content[1].Kind == yaml.ScalarNode && args.Content[1].Value == "__adapter" {
			sites = append(sites, args.Content[0])
		}
	}
	add(mappingValue(root, "harness"))
	if roles := mappingValue(root, "harnesses"); roles != nil && roles.Kind == yaml.MappingNode {
		for i := 1; i < len(roles.Content); i += 2 {
			add(roles.Content[i])
		}
	}
	if atoms := mappingValue(root, "atoms"); atoms != nil && atoms.Kind == yaml.SequenceNode {
		for _, atom := range atoms.Content {
			add(atom)
		}
	}
	return sites, nil
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Kind == yaml.ScalarNode && m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// hasAnchors reports an anchor, an alias or nesting deeper than any Formula needs;
// each fails the comparison safe.
func hasAnchors(n *yaml.Node, depth int) bool {
	if depth > 64 || n.Anchor != "" || n.Kind == yaml.AliasNode {
		return true
	}
	for _, c := range n.Content {
		if hasAnchors(c, depth+1) {
			return true
		}
	}
	return false
}

var unsafeFilename = regexp.MustCompile(`[^a-z0-9._-]+`)

func Inspect(labDir, id string) (Loaded, error) {
	if id == "" {
		data, err := os.ReadFile(filepath.Join(labDir, "lab.json"))
		if os.IsNotExist(err) {
			return Loaded{}, nil
		}
		if err != nil {
			return Loaded{}, err
		}
		var cfg struct {
			SchemaVersion   string `json:"schemaVersion"`
			ActiveFormulaID string `json:"activeFormulaId"`
		}
		if err = json.Unmarshal(data, &cfg); err != nil {
			return Loaded{}, fmt.Errorf("invalid Lab configuration: %w", err)
		}
		if cfg.SchemaVersion != "v0" || cfg.ActiveFormulaID == "" {
			return Loaded{}, fmt.Errorf("invalid Lab configuration")
		}
		id = cfg.ActiveFormulaID
	}
	if strings.ContainsAny(id, "/\\\x00") {
		return Loaded{}, fmt.Errorf("invalid Formula id")
	}
	name := strings.Trim(unsafeFilename.ReplaceAllString(strings.ToLower(id), "-"), "-")
	if name == "" || name == "." || name == ".." {
		return Loaded{}, fmt.Errorf("invalid Formula id")
	}
	file := filepath.Join(labDir, "formulas", name+".yaml")
	info, err := os.Lstat(file)
	if err != nil {
		return Loaded{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1_000_000 {
		return Loaded{}, fmt.Errorf("Formula must be a bounded regular file")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return Loaded{}, err
	}
	f, err := Parse(data)
	if err != nil {
		return Loaded{}, err
	}
	if f.ID != id {
		return Loaded{}, fmt.Errorf("Formula id does not match selected recipe")
	}
	return Loaded{Formula: f, File: file}, nil
}

// Approval identifies one human activation, even when two decisions approve identical bytes.
type Approval struct {
	FormulaID  string `json:"formulaId"`
	SHA256     string `json:"sha256"`
	DecisionID string `json:"decisionId"`
	SourceKind string `json:"sourceKind"`
	SourceID   string `json:"sourceId"`
}
type activation struct {
	ActiveIDBefore   string   `json:"activeIdBefore,omitempty"`
	ActiveHashBefore string   `json:"activeHashBefore,omitempty"`
	Approval         Approval `json:"approval"`
	YAML             string   `json:"yaml"`
	ConfigBefore     string   `json:"configBefore"`
	TargetBefore     string   `json:"targetBefore"`
	Config           string   `json:"config"`
	Complete         bool     `json:"complete"`
}

func digest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func FileHash(path string) (string, error) {
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return "absent", nil
	}
	if e != nil {
		return "", e
	}
	return digest(b), nil
}
func approvalError(dir string, e error) error {
	return fmt.Errorf("Formula execution approval rejected: %v; review current YAML with forgecell init --lab %s, then init --lab %s --approve PROPOSAL_ID", e, ShellWord(dir), ShellWord(dir))
}

// ShellWord quotes s as one literal word for a POSIX shell, for printed commands.
func ShellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func decisionFile(dir string, a Approval) (string, error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`).MatchString(a.DecisionID) || a.SourceID == "" || a.SourceKind == "" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(a.SHA256) {
		return "", fmt.Errorf("invalid approval identity")
	}
	return filepath.Join(dir, "formula-decisions", a.DecisionID+".json"), nil
}
func readActivation(dir string, a Approval) (activation, error) {
	var v activation
	path, e := decisionFile(dir, a)
	if e != nil {
		return v, e
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return v, e
	}
	if e = json.Unmarshal(b, &v); e != nil {
		return v, e
	}
	f, e := Parse([]byte(v.YAML))
	if e != nil || v.Approval != a || f.ID != a.FormulaID || f.SHA256 != a.SHA256 {
		return v, fmt.Errorf("activation intent changed")
	}
	var cfg struct {
		Active   string   `json:"activeFormulaId"`
		Approval Approval `json:"currentApproval"`
	}
	if json.Unmarshal([]byte(v.Config), &cfg) != nil || cfg.Active != a.FormulaID || cfg.Approval != a {
		return v, fmt.Errorf("activation configuration changed")
	}
	return v, nil
}

// Load is the execution boundary. Inspect remains available for historical/legacy review.
func Load(dir, id string) (Loaded, error) {
	if _, e := os.Lstat(filepath.Join(dir, ".formula-write.lock")); e == nil {
		return Loaded{}, approvalError(dir, fmt.Errorf("Formula activation is locked; inspect interrupted approval and resume its exact decision"))
	} else if !os.IsNotExist(e) {
		return Loaded{}, approvalError(dir, e)
	}
	return loadCurrent(dir, id, "")
}

// LoadForReview verifies the current decision while the caller owns the shared write lock.
func LoadForReview(dir, id, decision string) (Loaded, error) { return loadCurrent(dir, id, decision) }
func loadCurrent(dir, id, reviewingDecision string) (Loaded, error) {
	loaded, e := Inspect(dir, id)
	if e != nil {
		return loaded, approvalError(dir, e)
	}
	if loaded.Formula.ID == "" {
		return loaded, nil
	}
	b, e := os.ReadFile(filepath.Join(dir, "lab.json"))
	if e != nil {
		return loaded, approvalError(dir, e)
	}
	var cfg struct {
		Schema   string   `json:"schemaVersion"`
		Active   string   `json:"activeFormulaId"`
		Approval Approval `json:"currentApproval"`
	}
	if e = json.Unmarshal(b, &cfg); e != nil {
		return loaded, approvalError(dir, e)
	}
	a := cfg.Approval
	if cfg.Schema != "v0" || cfg.Active != loaded.Formula.ID || a.FormulaID != cfg.Active || a.SHA256 != loaded.Formula.SHA256 {
		return loaded, approvalError(dir, fmt.Errorf("missing, stale or non-current exact-byte approval"))
	}
	v, e := readActivation(dir, a)
	if e != nil {
		return loaded, approvalError(dir, e)
	}
	if !v.Complete || v.Config != string(b) {
		return loaded, approvalError(dir, fmt.Errorf("activation decision is incomplete or stale"))
	}

	{
		entries, e := os.ReadDir(filepath.Join(dir, "formula-decisions"))
		if e != nil {
			return loaded, approvalError(dir, e)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			data, e := os.ReadFile(filepath.Join(dir, "formula-decisions", entry.Name()))
			if e != nil {
				return loaded, approvalError(dir, e)
			}
			var pending activation
			if json.Unmarshal(data, &pending) == nil && !pending.Complete && pending.Approval.DecisionID != reviewingDecision && (pending.ConfigBefore == digest(b) || pending.Config == string(b) || pending.Approval == a) {
				return loaded, approvalError(dir, fmt.Errorf("unfinished activation intent; resume its exact approve decision before execution"))
			}
		}
	}
	loaded.Approved = true
	loaded.Approval = a
	return loaded, nil
}

// Revalidate checks both bytes and decision freshness immediately before invocation.
func Revalidate(dir string, expected Loaded) error {
	current, e := Load(dir, expected.Formula.ID)
	if e != nil {
		return e
	}
	if !current.Approved || current.Approval != expected.Approval {
		return approvalError(dir, fmt.Errorf("approval identity changed"))
	}
	return nil
}
func durable(path string, b []byte) error {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".approval-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func saveActivation(dir string, v activation) error {
	path, e := decisionFile(dir, v.Approval)
	if e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return durable(path, append(b, '\n'))
}

// BeginActivation is called under .formula-write.lock, before replacing any bytes.
func BeginActivation(dir, yaml string, a Approval, configBefore, targetBefore string) error {
	f, e := Parse([]byte(yaml))
	if e != nil {
		return e
	}
	if a.FormulaID != f.ID || a.SHA256 != f.SHA256 {
		return fmt.Errorf("approval bytes differ")
	}
	path, e := decisionFile(dir, a)
	if e != nil {
		return e
	}
	if _, e = os.Stat(path); e == nil {
		v, e := readActivation(dir, a)
		if e != nil {
			return e
		}
		if v.YAML != yaml || v.ConfigBefore != configBefore || v.TargetBefore != targetBefore {
			return fmt.Errorf("conflicting activation intent")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	actual, e := FileHash(filepath.Join(dir, "lab.json"))
	if e != nil {
		return e
	}
	if actual != configBefore {
		return fmt.Errorf("configuration changed; stale activation refused")
	}
	inspected, e := Inspect(dir, f.ID)
	if e == nil && inspected.Formula.SHA256 != targetBefore {
		return fmt.Errorf("target changed; stale activation refused")
	}
	if e != nil && !(os.IsNotExist(e) && targetBefore == "absent") {
		return e
	}
	cfg := map[string]any{}
	if configBefore != "absent" {
		b, e := os.ReadFile(filepath.Join(dir, "lab.json"))
		if e != nil {
			return e
		}
		if e = json.Unmarshal(b, &cfg); e != nil {
			return e
		}
	}
	activeID, activeHash := "", ""
	if configBefore != "absent" {
		active, e := Inspect(dir, "")
		if e != nil {
			return e
		}
		activeID, activeHash = active.Formula.ID, active.Formula.SHA256
	}
	cfg["schemaVersion"] = "v0"
	cfg["activeFormulaId"] = f.ID
	cfg["currentApproval"] = a
	b, e := json.MarshalIndent(cfg, "", "  ")
	if e != nil {
		return e
	}
	return saveActivation(dir, activation{ActiveIDBefore: activeID, ActiveHashBefore: activeHash, Approval: a, YAML: yaml, ConfigBefore: configBefore, TargetBefore: targetBefore, Config: string(append(b, '\n'))})
}

// verifyOriginalActive permits replacement of the transaction's target, but a
// changed-ID activation never writes the original recipe. That snapshot must
// remain intact throughout recovery, even after lab.json has been replaced.
func verifyOriginalActive(dir string, v activation) error {
	if v.ActiveIDBefore == "" {
		if v.ConfigBefore != "absent" {
			return fmt.Errorf("activation lacks original Formula snapshot; review a new proposal")
		}
		return nil
	}
	if v.ActiveIDBefore == v.Approval.FormulaID {
		return nil
	}
	active, e := Inspect(dir, v.ActiveIDBefore)
	if e != nil || active.Formula.SHA256 != v.ActiveHashBefore {
		return fmt.Errorf("original active Formula changed; interrupted activation refused; review a new proposal")
	}
	return nil
}

// ApplyActivation retries only the saved transition; it cannot overwrite a newer approval.
func ApplyActivation(dir string, a Approval) error {
	v, e := readActivation(dir, a)
	if e != nil {
		return e
	}
	cfg, e := FileHash(filepath.Join(dir, "lab.json"))
	if e != nil {
		return e
	}
	if cfg != v.ConfigBefore && cfg != digest([]byte(v.Config)) {
		return fmt.Errorf("newer configuration conflicts with activation")
	}
	loaded, e := Inspect(dir, a.FormulaID)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	actual := "absent"
	if e == nil {
		actual = loaded.Formula.SHA256
	}
	if actual != v.TargetBefore && actual != a.SHA256 {
		return fmt.Errorf("recipe conflicts with interrupted activation")
	}
	if v.Complete {
		if cfg != digest([]byte(v.Config)) || actual != a.SHA256 {
			return fmt.Errorf("completed activation is stale")
		}
		return nil
	}
	if e = verifyOriginalActive(dir, v); e != nil {
		return e
	}
	name := strings.Trim(unsafeFilename.ReplaceAllString(strings.ToLower(a.FormulaID), "-"), "-")
	if e = durable(filepath.Join(dir, "formulas", name+".yaml"), []byte(v.YAML)); e != nil {
		return e
	}
	return durable(filepath.Join(dir, "lab.json"), []byte(v.Config))
}
func CompleteActivation(dir string, a Approval) error {
	v, e := readActivation(dir, a)
	if e != nil {
		return e
	}
	cfg, e := FileHash(filepath.Join(dir, "lab.json"))
	if e != nil {
		return e
	}
	f, e := Inspect(dir, a.FormulaID)
	if e != nil {
		return e
	}
	if cfg != digest([]byte(v.Config)) || f.Formula.SHA256 != a.SHA256 {
		return fmt.Errorf("activation changed before completion")
	}
	if !v.Complete {
		if e = verifyOriginalActive(dir, v); e != nil {
			return e
		}
	}
	v.Complete = true
	return saveActivation(dir, v)
}

// ActivationStarted distinguishes an untouched proposal from a persisted approval intent.
func ActivationStarted(dir string, a Approval) (bool, error) {
	path, e := decisionFile(dir, a)
	if e != nil {
		return false, e
	}
	if _, e = os.Lstat(path); os.IsNotExist(e) {
		return false, nil
	} else if e != nil {
		return false, e
	}
	_, e = readActivation(dir, a)
	return true, e
}
