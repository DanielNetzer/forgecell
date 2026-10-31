package molecule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// Bounds of a plugin forge policy, as the forge contract states them; an
// answer past them is unknown, never accepted.
const (
	maxPluginPolicyChecks    = 64
	maxPluginPolicyCheckName = 200
	maxPluginPolicyReviews   = 100
)

// githubPolicyConstraints are the mandatory constraints that pin a GitHub
// policy observation; a plugin forge cannot satisfy them.
var githubPolicyConstraints = []string{"github-ruleset-sha256", "github-protection-sha256"}

// PluginPolicySource is the Source of the policy observations a forge plugin
// answers: plugin:<name>/forge_policy.
func PluginPolicySource(plugin string) string {
	return "plugin:" + plugin + "/forge_policy"
}

// PluginRemotePolicy maps a forge_policy answer, or the failure to get one, to
// the RemotePolicy the GitHub path records. Both observations carry the
// plugin's Source and the SHA-256 of the canonical JSON of the decoded policy
// (required checks, required reviews and protected), which is also their
// content. As for GitHub, a successful read is an observation, not proof of
// compliance: a failed, unbounded or malformed answer is unknown, never an
// empty policy.
func PluginRemotePolicy(plugin string, p contract.ForgePolicy, err error) readiness.RemotePolicy {
	o := readiness.PolicyObservation{Source: PluginPolicySource(plugin), RetrievedAt: stamp(), Status: "unknown"}
	if err != nil {
		o.Unknown = "Policy unavailable from the forge plugin (" + string(mcp.ReasonOf(err)) + "). No absence of protection is inferred."
		return readiness.RemotePolicy{Rules: o, Protection: o}
	}
	if len(p.RequiredChecks) > maxPluginPolicyChecks || p.RequiredReviews < 0 || p.RequiredReviews > maxPluginPolicyReviews {
		o.Unknown = "Expected a complete bounded forge policy. No absence of protection is inferred."
		return readiness.RemotePolicy{Rules: o, Protection: o}
	}
	checks := make([]string, 0, len(p.RequiredChecks))
	for _, name := range p.RequiredChecks {
		if strings.TrimSpace(name) == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxPluginPolicyCheckName {
			o.Unknown = "Invalid required check name in the forge policy. No absence of protection is inferred."
			return readiness.RemotePolicy{Rules: o, Protection: o}
		}
		checks = append(checks, name)
	}
	// The digest binds the set of required checks, not the plugin's order or
	// repeats, so equivalent policies hash the same.
	sort.Strings(checks)
	checks = slices.Compact(checks)
	raw, marshalErr := json.Marshal(contract.ForgePolicy{RequiredChecks: checks, RequiredReviews: p.RequiredReviews, Protected: p.Protected})
	if marshalErr != nil {
		o.Unknown = "Invalid forge policy. No absence of protection is inferred."
		return readiness.RemotePolicy{Rules: o, Protection: o}
	}
	o.Content = raw
	o.Status = "observed"
	o.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	return readiness.RemotePolicy{Rules: o, Protection: o}
}

// PluginPolicyRequiredChecks returns the required check names of an observed
// plugin policy, sorted and without repeats, or false when the policy is unknown or
// not a plugin observation.
func PluginPolicyRequiredChecks(p readiness.RemotePolicy) ([]string, bool) {
	if p.Rules.Status != "observed" || !strings.HasPrefix(p.Rules.Source, "plugin:") {
		return nil, false
	}
	// A persisted record may hold the content re-indented; the digest is of its
	// compact form.
	var compact bytes.Buffer
	var decoded contract.ForgePolicy
	if json.Compact(&compact, p.Rules.Content) != nil || json.Unmarshal(compact.Bytes(), &decoded) != nil || fmt.Sprintf("%x", sha256.Sum256(compact.Bytes())) != p.Rules.SHA256 {
		return nil, false
	}
	return decoded.RequiredChecks, true
}

// RefuseGitHubPolicyConstraints refuses mandatory constraints that pin a GitHub
// policy digest, which a plugin forge cannot satisfy. Pinning a plugin policy
// digest is not supported yet.
func RefuseGitHubPolicyConstraints(requirements []string) error {
	for _, requirement := range requirements {
		kind, _, _ := strings.Cut(requirement, ":")
		for _, github := range githubPolicyConstraints {
			if kind == github {
				return fmt.Errorf("the Formula delivers through a forge plugin, which cannot verify the mandatory constraint %s; remove it, since pinning a plugin forge policy is not supported yet", github)
			}
		}
	}
	return nil
}

// ValidatePluginRequirements is RemotePolicy.ValidateRequirements for a
// plugin forge: GitHub policy digest constraints are refused with a plain
// reason first.
func ValidatePluginRequirements(p readiness.RemotePolicy, requirements []string) error {
	if err := RefuseGitHubPolicyConstraints(requirements); err != nil {
		return err
	}
	return p.ValidateRequirements(requirements)
}

// hasPluginForge reports whether the Formula declares a forge block, which
// always selects a plugin forge.
func hasPluginForge(f formula.Formula) bool {
	return f.Forge != nil
}

// readPluginForgePolicy reads branch's policy through the Formula's forge
// plugin. Its calls go to calls, and the failure diagnostics of a failed
// operation to its notes. A plugin that cannot be selected or answered is an
// unknown policy, as an unreadable GitHub policy is.
func readPluginForgePolicy(ctx context.Context, labDir string, f formula.Formula, branch string, calls *PluginCallLog) readiness.RemotePolicy {
	name := ""
	if f.Forge != nil {
		name = f.Forge.Plugin
	}
	resolved, err := resolvedLabDir(labDir)
	if err != nil {
		return PluginRemotePolicy(name, contract.ForgePolicy{}, err)
	}
	changes, _, err := mcpadapter.ForgeFor(resolved, f, mcpadapter.Options{Observer: calls.Observe})
	if err != nil {
		calls.Fail(err)
		return PluginRemotePolicy(name, contract.ForgePolicy{}, err)
	}
	p, err := changes.Policy(ctx, branch)
	if err != nil {
		calls.Fail(err)
	}
	return PluginRemotePolicy(name, p, err)
}

// pluginPolicyCalls returns the Molecule's call log, or a new one for the Lab
// when the run has none (a GitHub-intake run with a plugin forge).
func pluginPolicyCalls(o Options) *PluginCallLog {
	if o.PluginCalls != nil {
		return o.PluginCalls
	}
	if resolved, err := resolvedLabDir(o.LabDir); err == nil {
		return NewPluginCallLog(resolved, o.LabDir)
	}
	return NewPluginCallLog(o.LabDir)
}

// resolvedLabDir is the Lab's absolute path with symlinks resolved, as the
// plugin adapter requires. The error names no path.
func resolvedLabDir(labDir string) (string, error) {
	abs, err := filepath.Abs(labDir)
	if err != nil {
		return "", fmt.Errorf("the Lab directory cannot be resolved")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("the Lab directory cannot be resolved")
	}
	return resolved, nil
}
