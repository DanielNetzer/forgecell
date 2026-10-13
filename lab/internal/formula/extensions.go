package formula

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// ExtensionRef names a Lab-relative extension manifest and its exact SHA-256.
// Parse is purely syntactic: it never reads, hashes or verifies the manifest.
type ExtensionRef struct {
	Manifest string `yaml:"manifest" json:"manifest"`
	SHA256   string `yaml:"sha256" json:"sha256"`
	Required bool   `yaml:"required" json:"required,omitempty"`
}

var extensionName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// extensionReservedRoots are Lab directories a manifest must never be read from.
var extensionReservedRoots = []string{"workspaces", "verification", "ledgers"}

func validateExtensionManifest(manifest string) error {
	if manifest == "." {
		return fmt.Errorf("manifest must name a file, not the Lab root")
	}
	if manifest == "" || len(manifest) > 512 || strings.ContainsAny(manifest, "\\\x00") || !filepath.IsLocal(manifest) || filepath.ToSlash(filepath.Clean(manifest)) != manifest {
		return fmt.Errorf("manifest must be a clean Lab-relative path")
	}
	root, _, _ := strings.Cut(manifest, "/")
	for _, reserved := range extensionReservedRoots {
		if strings.EqualFold(root, reserved) {
			return fmt.Errorf("manifest must not be under %s/", reserved)
		}
	}
	return nil
}

func validateExtensionDeclaration(entry any) error {
	m, ok := entry.(map[string]any)
	if !ok {
		return fmt.Errorf("must be a mapping with manifest, sha256 and optional required")
	}
	for key := range m {
		if key != "manifest" && key != "sha256" && key != "required" {
			return fmt.Errorf("unknown key %q; use manifest, sha256 and required", key)
		}
	}
	manifest, ok := m["manifest"].(string)
	if !ok {
		return fmt.Errorf("manifest must be a string")
	}
	if err := validateExtensionManifest(manifest); err != nil {
		return err
	}
	digest, _ := m["sha256"].(string)
	if !wrapperDigest.MatchString(digest) {
		return fmt.Errorf("sha256 must be a lowercase SHA-256")
	}
	if value, ok := m["required"]; ok {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("required must be a boolean")
		}
	}
	return nil
}

func validateBindingExtensions(where string, binding map[string]any, declared map[string]any) error {
	value, ok := binding["extensions"]
	if !ok {
		return nil
	}
	names, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s.extensions must be a list of extension names", where)
	}
	seen := map[string]bool{}
	for _, item := range names {
		name, ok := item.(string)
		if !ok {
			return fmt.Errorf("%s.extensions must contain only strings", where)
		}
		if _, ok := declared[name]; !ok {
			return fmt.Errorf("%s.extensions references undeclared extension %q", where, name)
		}
		if seen[name] {
			return fmt.Errorf("%s.extensions repeats extension %q", where, name)
		}
		seen[name] = true
	}
	return nil
}

// validateExtensions checks the top-level declarations and every role reference.
// It runs after the harness and harnesses mappings are known to be well-formed.
func validateExtensions(raw map[string]any) error {
	var declared map[string]any
	if value, exists := raw["extensions"]; exists {
		var ok bool
		if declared, ok = value.(map[string]any); !ok {
			return fmt.Errorf("extensions must be a mapping of name to declaration: `extensions` is a reserved top-level Formula key, so a Formula that already used it for something else must rename or remove it")
		}
		for name, entry := range declared {
			if !extensionName.MatchString(name) {
				return fmt.Errorf("extensions.%s: name must match %s", name, extensionName)
			}
			if err := validateExtensionDeclaration(entry); err != nil {
				return fmt.Errorf("extensions.%s: %w", name, err)
			}
		}
	}
	if harness, ok := raw["harness"].(map[string]any); ok {
		if err := validateBindingExtensions("harness", harness, declared); err != nil {
			return err
		}
	}
	roles, _ := raw["harnesses"].(map[string]any)
	for name, value := range roles {
		role, _ := value.(map[string]any)
		if _, has := role["extensions"]; has && name != string(AnalysisHarness) && name != string(CodingHarness) {
			return fmt.Errorf("harnesses.%s.extensions is not supported; v1 binds extensions to the analysis and coding roles only", name)
		}
		if err := validateBindingExtensions("harnesses."+name, role, declared); err != nil {
			return err
		}
	}
	return nil
}

// RoleExtensions resolves the extension names a role binds, in declared order.
// A declared coding override replaces the default `harness` list rather than
// inheriting it; analysis uses only its own role list and meta binds none.
func (f Formula) RoleExtensions(role HarnessRole) []string {
	switch role {
	case CodingHarness:
		if b, declared := f.Harnesses[string(role)]; declared {
			return slices.Clone(b.Extensions)
		}
		return slices.Clone(f.Harness.Extensions)
	case AnalysisHarness:
		return slices.Clone(f.Harnesses[string(role)].Extensions)
	}
	return nil
}
