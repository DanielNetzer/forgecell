package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func TestPluginRemotePolicyObserved(t *testing.T) {
	p := PluginRemotePolicy("gitlab", contract.ForgePolicy{RequiredChecks: []string{"build", "test"}, RequiredReviews: 1, Protected: true}, nil)
	content := `{"requiredChecks":["build","test"],"requiredReviews":1,"protected":true}`
	sum := sha256.Sum256([]byte(content))
	for name, o := range map[string]readiness.PolicyObservation{"rules": p.Rules, "protection": p.Protection} {
		if o.Source != "plugin:gitlab/forge_policy" || o.Status != "observed" || string(o.Content) != content || o.SHA256 != hex.EncodeToString(sum[:]) || o.Unknown != "" || o.RetrievedAt == "" {
			t.Fatalf("%s observation = %+v", name, o)
		}
	}
	if checks, ok := PluginPolicyRequiredChecks(p); !ok || !reflect.DeepEqual(checks, []string{"build", "test"}) {
		t.Fatalf("required checks = %q, %t", checks, ok)
	}
	// No required checks is an observed empty list, never null.
	empty := PluginRemotePolicy("gitlab", contract.ForgePolicy{}, nil)
	if string(empty.Rules.Content) != `{"requiredChecks":[],"requiredReviews":0,"protected":false}` || empty.Rules.Status != "observed" {
		t.Fatalf("empty policy = %+v", empty.Rules)
	}
	// An observation pins nothing on its own: requirements are still checked.
	if err := p.ValidateRequirements(nil); err != nil {
		t.Fatal(err)
	}
	// A GitHub observation is not a plugin policy.
	if _, ok := PluginPolicyRequiredChecks(readiness.RemotePolicy{Rules: readiness.PolicyObservation{Source: "https://api.github.com/x", Status: "observed", Content: []byte("[]")}}); ok {
		t.Fatal("a GitHub observation read as a plugin policy")
	}
	tampered := p
	tampered.Rules.Content = []byte(`{"requiredChecks":[],"requiredReviews":0,"protected":false}`)
	if _, ok := PluginPolicyRequiredChecks(tampered); ok {
		t.Fatal("content that does not match its digest was read")
	}
}

func TestPluginRemotePolicyDigestIgnoresOrderAndRepeats(t *testing.T) {
	want := PluginRemotePolicy("gitlab", contract.ForgePolicy{RequiredChecks: []string{"build", "lint", "test"}, RequiredReviews: 2, Protected: true}, nil)
	for _, in := range [][]string{{"test", "build", "lint"}, {"lint", "build", "test", "build"}, {"build", "build", "lint", "test", "test"}} {
		got := PluginRemotePolicy("gitlab", contract.ForgePolicy{RequiredChecks: in, RequiredReviews: 2, Protected: true}, nil)
		if got.Rules.SHA256 != want.Rules.SHA256 || string(got.Rules.Content) != string(want.Rules.Content) || got.Protection.SHA256 != want.Rules.SHA256 {
			t.Fatalf("%q: digest %s content %s, want %s %s", in, got.Rules.SHA256, got.Rules.Content, want.Rules.SHA256, want.Rules.Content)
		}
		if checks, ok := PluginPolicyRequiredChecks(got); !ok || !reflect.DeepEqual(checks, []string{"build", "lint", "test"}) {
			t.Fatalf("%q: required checks = %q, %t", in, checks, ok)
		}
	}
	// A different set still differs.
	other := PluginRemotePolicy("gitlab", contract.ForgePolicy{RequiredChecks: []string{"build", "lint"}, RequiredReviews: 2, Protected: true}, nil)
	if other.Rules.SHA256 == want.Rules.SHA256 {
		t.Fatal("a different check set has the same digest")
	}
	// The caller's slice is not reordered.
	in := []string{"test", "build"}
	PluginRemotePolicy("gitlab", contract.ForgePolicy{RequiredChecks: in}, nil)
	if !reflect.DeepEqual(in, []string{"test", "build"}) {
		t.Fatalf("input reordered: %q", in)
	}
}

func TestPluginRemotePolicyUnknown(t *testing.T) {
	many := make([]string, maxPluginPolicyChecks+1)
	for i := range many {
		many[i] = fmt.Sprintf("check-%d", i)
	}
	cases := map[string]struct {
		policy contract.ForgePolicy
		err    error
		want   string
	}{
		"failed call":        {err: mcp.Fail(mcp.AuthMissing, "forge_policy", "Authorization: Bearer fakefakefakefake0123456789"), want: "(auth-missing)"},
		"other error":        {err: fmt.Errorf("raw server text"), want: "(failed)"},
		"too many checks":    {policy: contract.ForgePolicy{RequiredChecks: many}, want: "complete bounded"},
		"negative reviews":   {policy: contract.ForgePolicy{RequiredReviews: -1}, want: "complete bounded"},
		"too many reviews":   {policy: contract.ForgePolicy{RequiredReviews: maxPluginPolicyReviews + 1}, want: "complete bounded"},
		"blank check name":   {policy: contract.ForgePolicy{RequiredChecks: []string{" "}}, want: "Invalid required check name"},
		"oversized check":    {policy: contract.ForgePolicy{RequiredChecks: []string{strings.Repeat("x", maxPluginPolicyCheckName+1)}}, want: "Invalid required check name"},
		"invalid utf-8 name": {policy: contract.ForgePolicy{RequiredChecks: []string{"\xff"}}, want: "Invalid required check name"},
	}
	for name, c := range cases {
		p := PluginRemotePolicy("gitlab", c.policy, c.err)
		for _, o := range []readiness.PolicyObservation{p.Rules, p.Protection} {
			if o.Status != "unknown" || o.Content != nil || o.SHA256 != "" || !strings.Contains(o.Unknown, c.want) || !strings.Contains(o.Unknown, "No absence of protection is inferred") || o.Source != "plugin:gitlab/forge_policy" {
				t.Fatalf("%s: observation = %+v", name, o)
			}
			if strings.Contains(o.Unknown, "Bearer") || strings.Contains(o.Unknown, "raw server text") {
				t.Fatalf("%s: the unknown reason leaks: %q", name, o.Unknown)
			}
		}
		if _, ok := PluginPolicyRequiredChecks(p); ok {
			t.Fatalf("%s: an unknown policy has required checks", name)
		}
	}
}

func TestPluginForgeRefusesGitHubPolicyConstraints(t *testing.T) {
	observed := PluginRemotePolicy("gitlab", contract.ForgePolicy{Protected: true}, nil)
	for _, kind := range []string{"github-ruleset-sha256", "github-protection-sha256"} {
		// Even a digest equal to the plugin observation's is refused: it pins a
		// GitHub observation, which a plugin forge never makes.
		constraint := kind + ":" + observed.Rules.SHA256
		for _, err := range []error{RefuseGitHubPolicyConstraints([]string{constraint}), ValidatePluginRequirements(observed, []string{constraint})} {
			if err == nil || !strings.Contains(err.Error(), "forge plugin, which cannot verify the mandatory constraint "+kind) {
				t.Fatalf("%s: %v", kind, err)
			}
		}
	}
	if err := ValidatePluginRequirements(observed, nil); err != nil {
		t.Fatalf("no constraints: %v", err)
	}
	if err := ValidatePluginRequirements(observed, []string{"other:" + strings.Repeat("a", 64)}); err == nil || !strings.Contains(err.Error(), "unsupported mandatory constraint") {
		t.Fatalf("other constraint: %v", err)
	}
}

func TestReadPluginForgePolicyIsUnknownWhenUnselectable(t *testing.T) {
	lab := resolvedTempDir(t)
	calls := NewPluginCallLog(lab)
	undeclared := formula.Formula{Forge: &formula.ForgeRef{Plugin: "gitlab", Project: "acme/widgets"}}
	p := readPluginForgePolicy(context.Background(), lab, undeclared, "main", calls)
	if p.Rules.Status != "unknown" || p.Rules.Source != "plugin:gitlab/forge_policy" || !strings.Contains(p.Rules.Unknown, "(failed)") {
		t.Fatalf("policy = %+v", p.Rules)
	}
	if recs, _, _ := calls.take(); len(recs) != 0 {
		t.Fatalf("records = %+v", recs)
	}
	if _, err := os.Lstat(filepath.Join(lab, "plugin-runs")); !os.IsNotExist(err) {
		t.Fatalf("an unselectable forge made a run root: %v", err)
	}
	if hasPluginForge(formula.Formula{}) || !hasPluginForge(undeclared) {
		t.Fatal("hasPluginForge")
	}
}

// pluginForgeFixture is the keyed run fixture, with no numeric Issue reader
// seam, whose approved Formula delivers through the fake forge plugin.
func pluginForgeFixture(t *testing.T) Options {
	t.Helper()
	o, _ := keyedFixture(t, keyedIssueFixture())
	installPluginForge(t, o)
	return o
}

// installPluginForge approves o's Formula with a forge block naming the fake
// forge plugin.
func installPluginForge(t *testing.T, o Options) {
	t.Helper()
	server := plugintest.Server(t)
	raw := plugintest.Manifest(t, server, "forge", "good")
	if err := os.MkdirAll(filepath.Join(o.LabDir, "plugins", "fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.LabDir, "plugins", "fake", "plugin.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	path := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	recipe, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	yaml := string(recipe) + "plugins:\n  - name: gitlab\n    manifest: plugins/fake/plugin.json\n    sha256: " + hex.EncodeToString(sum[:]) + "\nforge:\n  plugin: gitlab\n  project: acme/widgets\n"
	// The activation writes the reviewed Formula itself.
	approveFixture(t, o.LabDir, yaml, "plugin-forge")
}

func TestIntakeReadsPolicyThroughThePluginForge(t *testing.T) {
	t.Parallel()
	o := pluginForgeFixture(t)
	r, err := RunKeyed(context.Background(), o, "ENG-1234")
	if err != nil {
		t.Fatalf("RunKeyed: %v", err)
	}
	if r.RemotePolicy == nil || r.RemotePolicy.Rules.Source != "plugin:gitlab/forge_policy" || r.RemotePolicy.Rules.Status != "observed" || string(r.RemotePolicy.Rules.Content) != `{"requiredChecks":[],"requiredReviews":0,"protected":false}` {
		t.Fatalf("remote policy = %+v", r.RemotePolicy)
	}
	if len(r.PluginCalls) != 1 || r.PluginCalls[0].Tool != "forge_policy" || r.PluginCalls[0].Plugin != "fake-forge" || r.PluginCalls[0].Reason != "" {
		t.Fatalf("plugin calls = %+v", r.PluginCalls)
	}
	saved, err := ReadRecord(o.LabDir, r.ID)
	if err != nil || !reflect.DeepEqual(saved.PluginCalls, r.PluginCalls) || saved.RemotePolicy == nil {
		t.Fatalf("saved = %+v, %v", saved.PluginCalls, err)
	}
}

func TestIntakeWithAnInjectedReaderStartsNoPlugin(t *testing.T) {
	t.Parallel()
	// The numeric Issue reader seam keeps the GitHub branch off live policy; with
	// a plugin forge it keeps the plugin from starting at all.
	o, _ := fixture(t)
	installPluginForge(t, o)
	if o.ReadIssue == nil {
		t.Fatal("fixture has no injected reader")
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.RemotePolicy != nil || len(r.PluginCalls) != 0 || r.PluginCallsDropped != 0 {
		t.Fatalf("policy %+v, calls %+v", r.RemotePolicy, r.PluginCalls)
	}
	if r.Readiness == nil || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("intake did not finish: status %q notes %q", r.Status, r.Notes)
	}
	// Every plugin start prepares the Lab's run root first; none was made.
	if _, err := os.Lstat(filepath.Join(o.LabDir, "plugin-runs")); !os.IsNotExist(err) {
		t.Fatalf("a plugin run root was prepared: %v", err)
	}
	saved, err := ReadRecord(o.LabDir, r.ID)
	if err != nil || len(saved.PluginCalls) != 0 || saved.RemotePolicy != nil {
		t.Fatalf("saved calls %+v policy %+v, %v", saved.PluginCalls, saved.RemotePolicy, err)
	}
}

func TestIntakeRefusesGitHubConstraintsWithAPluginForge(t *testing.T) {
	t.Parallel()
	o := pluginForgeFixture(t)
	o.MandatoryConstraints = []string{"github-ruleset-sha256:" + strings.Repeat("a", 64)}
	r, err := RunKeyed(context.Background(), o, "ENG-1234")
	if err != nil {
		t.Fatalf("RunKeyed: %v", err)
	}
	if r.Status != "blocked" || len(r.PluginCalls) != 0 || r.RemotePolicy != nil {
		t.Fatalf("status %q, calls %+v, policy %+v", r.Status, r.PluginCalls, r.RemotePolicy)
	}
	found := false
	for _, n := range r.Notes {
		found = found || strings.Contains(n, "cannot verify the mandatory constraint github-ruleset-sha256")
	}
	if !found {
		t.Fatalf("notes = %q", r.Notes)
	}
}
