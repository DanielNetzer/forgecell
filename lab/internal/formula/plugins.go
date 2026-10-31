package formula

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"unicode/utf8"
)

// PluginRef pins a Lab-relative plugin manifest by name and exact SHA-256. Parse
// is purely syntactic: it never reads the manifest or checks its kind.
type PluginRef struct {
	Name     string `yaml:"name" json:"name"`
	Manifest string `yaml:"manifest" json:"manifest"`
	SHA256   string `yaml:"sha256" json:"sha256"`
}

// ForgeRef names the declared plugin that serves as the forge, and the project
// it delivers to. Absent, GitHub stays the forge.
type ForgeRef struct {
	Plugin  string `yaml:"plugin" json:"plugin"`
	Project string `yaml:"project" json:"project"`
}

// ReviewerRef names a declared reviewer plugin, the lowest finding severity it
// may fix on its own, and an optional configuration passed to its calls.
type ReviewerRef struct {
	Plugin  string         `yaml:"plugin" json:"plugin"`
	AutoFix string         `yaml:"autoFix" json:"autoFix"`
	Config  map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}

// Bounds of the plugin keys.
const (
	MaxPlugins          = 16
	MaxReviewers        = 4
	MaxPluginConfig     = 4 << 10
	maxForgeProject     = 256
	maxPluginConfigNest = 32
)

// DefaultIntakeSource is the built-in tracker; DefaultForge is the built-in forge
// a ship Atom's legacy `via` names when no `forge` is declared.
const (
	DefaultIntakeSource = "github-issues"
	DefaultForge        = "github"
)

// reservedPluginNames are the built-in implementations' names; a plugin named
// like one would make intake.source or ship via ambiguous.
var reservedPluginNames = []string{DefaultIntakeSource, DefaultForge}

var autoFixLevels = []string{"high", "medium", "low", "none"}

// validatePlugins checks the plugins, intake.source, intake.config, forge and
// reviewers keys, and that the legacy intake-Atom source and ship-Atom via agree
// with them. It runs on the raw mapping before the typed decode and reads no
// files. The intake block's other keys are not newly refused.
func validatePlugins(raw map[string]any) error {
	declared := map[string]bool{}
	if value, exists := raw["plugins"]; exists {
		list, ok := value.([]any)
		if !ok {
			return fmt.Errorf("plugins must be a list of {name, manifest, sha256} pins: `plugins` is a reserved top-level Formula key, so a Formula that already used it for something else must rename or remove it")
		}
		if len(list) > MaxPlugins {
			return fmt.Errorf("plugins lists %d entries; at most %d are allowed", len(list), MaxPlugins)
		}
		for i, entry := range list {
			name, err := validatePluginPin(entry)
			if err != nil {
				return fmt.Errorf("plugins[%d]: %w", i, err)
			}
			if declared[name] {
				return fmt.Errorf("plugins[%d]: plugin %q is declared twice", i, name)
			}
			declared[name] = true
		}
	}
	source := DefaultIntakeSource
	// YAML decodes a mapping with any non-string key as map[any]any, which would
	// otherwise skip the source and config checks below. An empty `intake:` is
	// null and holds neither.
	if value, exists := raw["intake"]; exists && value != nil {
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("intake must be a mapping with string keys")
		}
	}
	if intake, ok := raw["intake"].(map[string]any); ok {
		if value, exists := intake["source"]; exists {
			s, ok := value.(string)
			if !ok || (s != DefaultIntakeSource && !declared[s]) {
				return fmt.Errorf("intake.source must be %s or the name of a declared plugin", DefaultIntakeSource)
			}
			source = s
		}
		if value, exists := intake["config"]; exists {
			if err := validatePluginConfig(value); err != nil {
				return fmt.Errorf("intake.config: %w", err)
			}
		}
	}
	forge := DefaultForge
	if value, exists := raw["forge"]; exists {
		name, err := validateForge(value, declared)
		if err != nil {
			return fmt.Errorf("forge: %w", err)
		}
		forge = name
	}
	if value, exists := raw["reviewers"]; exists {
		if err := validateReviewers(value, declared); err != nil {
			return err
		}
	}
	return validateLegacyPluginKeys(raw, source, forge)
}

func validatePluginPin(entry any) (string, error) {
	m, ok := entry.(map[string]any)
	if !ok {
		return "", fmt.Errorf("must be a mapping with name, manifest and sha256")
	}
	if err := exactKeys(m, []string{"name", "manifest", "sha256"}, nil); err != nil {
		return "", err
	}
	name, ok := m["name"].(string)
	if !ok || !extensionName.MatchString(name) {
		return "", fmt.Errorf("name must match %s", extensionName)
	}
	if slices.Contains(reservedPluginNames, name) {
		return "", fmt.Errorf("name %q is reserved for the built-in implementation", name)
	}
	manifest, ok := m["manifest"].(string)
	if !ok {
		return "", fmt.Errorf("manifest must be a string")
	}
	if err := validateExtensionManifest(manifest); err != nil {
		return "", err
	}
	if digest, _ := m["sha256"].(string); !wrapperDigest.MatchString(digest) {
		return "", fmt.Errorf("sha256 must be a lowercase SHA-256")
	}
	return name, nil
}

func validateForge(value any, declared map[string]bool) (string, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("must be a mapping with plugin and project: `forge` is a reserved top-level Formula key, so a Formula that already used it for something else must rename or remove it")
	}
	if err := exactKeys(m, []string{"plugin", "project"}, nil); err != nil {
		return "", err
	}
	name, err := declaredPlugin(m["plugin"], declared)
	if err != nil {
		return "", err
	}
	project, ok := m["project"].(string)
	if !ok || project == "" || !utf8.ValidString(project) || utf8.RuneCountInString(project) > maxForgeProject {
		return "", fmt.Errorf("project must be a non-empty string of at most %d characters", maxForgeProject)
	}
	return name, nil
}

func validateReviewers(value any, declared map[string]bool) error {
	list, ok := value.([]any)
	if !ok {
		return fmt.Errorf("reviewers must be a list of {plugin, autoFix, config} entries: `reviewers` is a reserved top-level Formula key, so a Formula that already used it for something else must rename or remove it")
	}
	if len(list) > MaxReviewers {
		return fmt.Errorf("reviewers lists %d entries; at most %d are allowed", len(list), MaxReviewers)
	}
	for i, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("reviewers[%d] must be a mapping with plugin, autoFix and optional config", i)
		}
		if err := exactKeys(m, []string{"plugin", "autoFix"}, []string{"config"}); err != nil {
			return fmt.Errorf("reviewers[%d]: %w", i, err)
		}
		if _, err := declaredPlugin(m["plugin"], declared); err != nil {
			return fmt.Errorf("reviewers[%d]: %w", i, err)
		}
		if level, _ := m["autoFix"].(string); !slices.Contains(autoFixLevels, level) {
			return fmt.Errorf("reviewers[%d]: autoFix must be high, medium, low or none", i)
		}
		if value, exists := m["config"]; exists {
			if err := validatePluginConfig(value); err != nil {
				return fmt.Errorf("reviewers[%d].config: %w", i, err)
			}
		}
	}
	return nil
}

// exactKeys refuses a missing required key and any key outside required and
// optional. The message names the offending key only when it is short.
func exactKeys(m map[string]any, required, optional []string) error {
	for _, key := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(required, key) && !slices.Contains(optional, key) {
			if len(key) > 64 {
				return fmt.Errorf("an unknown key is present; use %s", keyList(required, optional))
			}
			return fmt.Errorf("unknown key %q; use %s", key, keyList(required, optional))
		}
	}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			return fmt.Errorf("%s is required", key)
		}
	}
	return nil
}

func keyList(required, optional []string) string {
	all := append(slices.Clone(required), optional...)
	s := ""
	for i, key := range all {
		switch {
		case i == 0:
		case i == len(all)-1:
			s += " and "
		default:
			s += ", "
		}
		s += key
	}
	return s
}

func declaredPlugin(value any, declared map[string]bool) (string, error) {
	name, ok := value.(string)
	if !ok || !declared[name] {
		return "", fmt.Errorf("plugin must name a plugin declared under plugins")
	}
	return name, nil
}

// validatePluginConfig requires a JSON-compatible object (string keys; strings,
// finite numbers, booleans, null, lists and objects only) of bounded depth whose
// JSON encoding is at most MaxPluginConfig bytes.
func validatePluginConfig(value any) error {
	m, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("must be a mapping")
	}
	if err := jsonCompatible(m, 0); err != nil {
		return err
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("must be JSON-compatible")
	}
	if len(encoded) > MaxPluginConfig {
		return fmt.Errorf("is %d bytes as JSON; at most %d are allowed", len(encoded), MaxPluginConfig)
	}
	return nil
}

func jsonCompatible(value any, depth int) error {
	if depth > maxPluginConfigNest {
		return fmt.Errorf("nests deeper than %d levels", maxPluginConfigNest)
	}
	switch v := value.(type) {
	case nil, bool, int, int64, uint64:
		return nil
	case string:
		if !utf8.ValidString(v) {
			return fmt.Errorf("must hold valid UTF-8 text")
		}
		return nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("must hold finite numbers only")
		}
		return nil
	case []any:
		for _, item := range v {
			if err := jsonCompatible(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for key, item := range v {
			if !utf8.ValidString(key) {
				return fmt.Errorf("must have valid UTF-8 keys")
			}
			if err := jsonCompatible(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("must be JSON-compatible: string keys, and strings, numbers, booleans, null, lists and mappings only")
}

// validateLegacyPluginKeys requires an intake Atom's `source` and a ship Atom's
// `via`, where present, to agree with the effective tracker and forge.
func validateLegacyPluginKeys(raw map[string]any, source, forge string) error {
	atoms, _ := raw["atoms"].([]any)
	for _, value := range atoms {
		m, _ := value.(map[string]any)
		switch m["type"] {
		case "intake":
			if v, exists := m["source"]; exists && v != source {
				return fmt.Errorf("an intake Atom's source must match the Formula's intake source %q", source)
			}
		case "ship":
			if v, exists := m["via"]; exists && v != forge {
				return fmt.Errorf("a ship Atom's via must match the Formula's forge %q", forge)
			}
		}
	}
	return nil
}

func (f Formula) plugin(name string) (PluginRef, bool) {
	for _, p := range f.Plugins {
		if p.Name == name {
			return p, true
		}
	}
	return PluginRef{}, false
}

// TrackerPlugin returns the plugin intake.source names, or false when intake
// uses the built-in GitHub Issues tracker.
func (f Formula) TrackerPlugin() (PluginRef, bool) {
	if f.Intake.Source == "" || f.Intake.Source == DefaultIntakeSource {
		return PluginRef{}, false
	}
	return f.plugin(f.Intake.Source)
}

// ForgePlugin returns the forge plugin and its project, or false when GitHub
// stays the forge.
func (f Formula) ForgePlugin() (PluginRef, string, bool) {
	if f.Forge == nil {
		return PluginRef{}, "", false
	}
	p, ok := f.plugin(f.Forge.Plugin)
	if !ok {
		return PluginRef{}, "", false
	}
	return p, f.Forge.Project, true
}

// Reviewers returns a copy of the declared reviewer plugins, in order.
func (f Formula) Reviewers() []ReviewerRef {
	if len(f.ReviewerPlugins) == 0 {
		return nil
	}
	out := make([]ReviewerRef, len(f.ReviewerPlugins))
	for i, r := range f.ReviewerPlugins {
		out[i] = r
		if r.Config != nil {
			out[i].Config = maps.Clone(r.Config)
		}
	}
	return out
}

// PluginRoles returns, for each declared plugin name, the roles it fills
// (tracker, forge, reviewer) in that order, without repeats.
func (f Formula) PluginRoles() map[string][]string {
	roles := map[string][]string{}
	add := func(name, role string) {
		if !slices.Contains(roles[name], role) {
			roles[name] = append(roles[name], role)
		}
	}
	if p, ok := f.TrackerPlugin(); ok {
		add(p.Name, "tracker")
	}
	if p, _, ok := f.ForgePlugin(); ok {
		add(p.Name, "forge")
	}
	for _, r := range f.ReviewerPlugins {
		add(r.Plugin, "reviewer")
	}
	return roles
}
