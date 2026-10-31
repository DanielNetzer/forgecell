package formula

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const (
	pluginDigestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pluginDigestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pluginDigestC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// pluginRecipe declares a tracker, a forge and a reviewer plugin, and keeps the
// legacy intake source and ship via in agreement with them.
const pluginRecipe = `schemaVersion: v0
kind: formula
id: sample
intake:
  source: linear
  repo: acme/widgets
  config:
    team: ENG
    labels: [bug, p1]
    limit: 20
harness:
  binding: codex
  command: [forgecell, __adapter, codex, /bin/codex]
  timeoutMs: 1000
atoms:
  - id: intake
    type: intake
    source: linear
  - id: work
    type: harness
  - id: ship
    type: ship
    via: gitlab
plugins:
  - name: linear
    manifest: plugins/linear/plugin.json
    sha256: ` + pluginDigestA + `
  - name: gitlab
    manifest: plugins/gitlab/plugin.json
    sha256: ` + pluginDigestB + `
  - name: review-bot
    manifest: plugins/review-bot/plugin.json
    sha256: ` + pluginDigestC + `
forge:
  plugin: gitlab
  project: acme/widgets
reviewers:
  - plugin: review-bot
    autoFix: medium
    config: {strict: true}
  - plugin: review-bot
    autoFix: none
`

func TestPluginKeysAbsentKeepIdentity(t *testing.T) {
	f, err := Parse([]byte(recipe))
	if err != nil {
		t.Fatal(err)
	}
	if f.YAML != recipe || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(recipe))) {
		t.Fatal("snapshot changed")
	}
	if f.Plugins != nil || f.Forge != nil || f.ReviewerPlugins != nil || f.Intake.Config != nil || f.Intake.Source != "github-issues" {
		t.Fatalf("absent keys decoded as present: %+v", f)
	}
	if _, ok := f.TrackerPlugin(); ok {
		t.Fatal("a Formula without plugins has a tracker plugin")
	}
	if _, _, ok := f.ForgePlugin(); ok {
		t.Fatal("a Formula without forge has a forge plugin")
	}
	if f.Reviewers() != nil || len(f.PluginRoles()) != 0 {
		t.Fatal("a Formula without reviewers has reviewers")
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"plugins"`, `"forge"`, `"reviewers"`, `"config"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("JSON of an unchanged Formula gained %s: %s", key, raw)
		}
	}
	// An intake source left out still defaults, and the init-written legacy keys agree.
	legacy := strings.Replace(recipe, "  source: github-issues\n", "", 1) + "  - id: ship\n    type: ship\n    via: github\n"
	legacy = strings.Replace(legacy, "    type: intake\n", "    type: intake\n    source: github-issues\n", 1)
	if f, err = Parse([]byte(legacy)); err != nil || f.Intake.Source != "github-issues" {
		t.Fatalf("legacy keys refused: %v", err)
	}
}

func TestPluginKeysRoundTrip(t *testing.T) {
	f, err := Parse([]byte(pluginRecipe))
	if err != nil {
		t.Fatal(err)
	}
	if f.YAML != pluginRecipe || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(pluginRecipe))) {
		t.Fatal("snapshot changed")
	}
	tracker, ok := f.TrackerPlugin()
	if !ok || tracker != (PluginRef{Name: "linear", Manifest: "plugins/linear/plugin.json", SHA256: pluginDigestA}) {
		t.Fatalf("tracker %+v %v", tracker, ok)
	}
	forge, project, ok := f.ForgePlugin()
	if !ok || forge.Name != "gitlab" || forge.SHA256 != pluginDigestB || project != "acme/widgets" {
		t.Fatalf("forge %+v %q %v", forge, project, ok)
	}
	want := []ReviewerRef{{Plugin: "review-bot", AutoFix: "medium", Config: map[string]any{"strict": true}}, {Plugin: "review-bot", AutoFix: "none"}}
	if got := f.Reviewers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reviewers %#v", got)
	}
	if !reflect.DeepEqual(f.Intake.Config, map[string]any{"team": "ENG", "labels": []any{"bug", "p1"}, "limit": 20}) {
		t.Fatalf("intake config %#v", f.Intake.Config)
	}
	if roles := f.PluginRoles(); !reflect.DeepEqual(roles, map[string][]string{"linear": {"tracker"}, "gitlab": {"forge"}, "review-bot": {"reviewer"}}) {
		t.Fatalf("roles %v", roles)
	}
	// Reviewers returns a copy: changing it never changes the Formula.
	f.Reviewers()[0].Config["strict"] = false
	if f.ReviewerPlugins[0].Config["strict"] != true {
		t.Fatal("Reviewers shares its config map with the Formula")
	}
	// The same bytes parse to the same Formula.
	again, err := Parse([]byte(f.YAML))
	if err != nil || !reflect.DeepEqual(again, mustParse(t, pluginRecipe)) {
		t.Fatalf("re-parse differs: %v", err)
	}
	// A plugin may fill no role, or two.
	two := strings.Replace(pluginRecipe, "  - plugin: review-bot\n    autoFix: none\n", "  - plugin: linear\n    autoFix: none\n", 1)
	if roles := mustParse(t, two).PluginRoles(); !reflect.DeepEqual(roles["linear"], []string{"tracker", "reviewer"}) {
		t.Fatalf("roles %v", roles)
	}
}

func mustParse(t *testing.T, s string) Formula {
	t.Helper()
	f, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPluginKeysRefusals(t *testing.T) {
	replace := func(old, new string) string {
		t.Helper()
		if !strings.Contains(pluginRecipe, old) {
			t.Fatalf("fixture lacks %q", old)
		}
		return strings.Replace(pluginRecipe, old, new, 1)
	}
	plugins := func(n int) string {
		var b strings.Builder
		b.WriteString("plugins:\n")
		for i := range n {
			fmt.Fprintf(&b, "  - name: p%d\n    manifest: plugins/p%d.json\n    sha256: %s\n", i, i, pluginDigestA)
		}
		return b.String()
	}
	// withReviewers replaces the fixture's trailing reviewers list with n entries.
	withReviewers := func(n int) string {
		return pluginRecipe[:strings.Index(pluginRecipe, "reviewers:\n")] + "reviewers:\n" + strings.Repeat("  - plugin: review-bot\n    autoFix: low\n", n)
	}
	big := strings.Repeat("x", MaxPluginConfig)
	for _, c := range []struct{ name, yaml, want string }{
		{"duplicate name", replace("name: gitlab", "name: linear"), `plugin "linear" is declared twice`},
		{"bad digest", replace("sha256: "+pluginDigestB, "sha256: "+strings.ToUpper(pluginDigestB)), "plugins[1]: sha256 must be a lowercase SHA-256"},
		{"short digest", replace("sha256: "+pluginDigestB, "sha256: abc"), "sha256 must be a lowercase SHA-256"},
		{"unknown pin key", replace("    sha256: "+pluginDigestA+"\n", "    sha256: "+pluginDigestA+"\n    required: true\n"), `plugins[0]: unknown key "required"; use name, manifest and sha256`},
		{"missing pin key", replace("    manifest: plugins/linear/plugin.json\n", ""), "plugins[0]: manifest is required"},
		{"bad name", replace("name: gitlab", "name: GitLab"), "plugins[1]: name must match"},
		{"reserved name", replace("name: gitlab", "name: github"), `plugins[1]: name "github" is reserved`},
		{"reserved tracker name", replace("name: gitlab", "name: github-issues"), `name "github-issues" is reserved`},
		{"traversing manifest", replace("plugins/gitlab/plugin.json", "../gitlab/plugin.json"), "manifest must be a clean Lab-relative path"},
		{"reserved manifest root", replace("plugins/gitlab/plugin.json", "workspaces/gitlab/plugin.json"), "manifest must not be under workspaces/"},
		{"plugins not a list", strings.Replace(recipe, "atoms:", "plugins: {linear: x}\natoms:", 1), "plugins must be a list"},
		{"too many plugins", strings.Replace(recipe, "atoms:", plugins(MaxPlugins+1)+"atoms:", 1), "plugins lists 17 entries; at most 16"},
		{"undeclared intake source", replace("  source: linear\n  repo", "  source: jira\n  repo"), "intake.source must be github-issues or the name of a declared plugin"},
		{"non-string intake source", replace("  source: linear\n  repo", "  source: [linear]\n  repo"), "intake.source must be"},
		{"intake config not a mapping", replace("  config:\n    team: ENG\n    labels: [bug, p1]\n    limit: 20\n", "  config: ENG\n"), "intake.config: must be a mapping"},
		{"oversized intake config", replace("    team: ENG\n", "    team: "+big+"\n"), "bytes as JSON; at most 4096 are allowed"},
		{"non-JSON intake config key", replace("    team: ENG\n", "    team: {1: ENG}\n"), "intake.config: must be JSON-compatible"},
		{"non-finite intake config", replace("    limit: 20\n", "    limit: .inf\n"), "intake.config: must hold finite numbers only"},
		{"undeclared forge", replace("  plugin: gitlab\n  project", "  plugin: bitbucket\n  project"), "forge: plugin must name a plugin declared under plugins"},
		{"built-in forge name", replace("  plugin: gitlab\n  project", "  plugin: github\n  project"), "forge: plugin must name a plugin declared"},
		{"unknown forge key", replace("  project: acme/widgets\n", "  project: acme/widgets\n  branch: main\n"), `forge: unknown key "branch"; use plugin and project`},
		{"empty project", replace("  project: acme/widgets\n", "  project: \"\"\n"), "forge: project must be a non-empty string of at most 256"},
		{"long project", replace("  project: acme/widgets\n", "  project: "+strings.Repeat("é", 257)+"\n"), "project must be a non-empty string"},
		{"missing project", replace("  project: acme/widgets\n", ""), "forge: project is required"},
		{"forge not a mapping", strings.Replace(recipe, "atoms:", "forge: gitlab\natoms:", 1), "forge: must be a mapping"},
		{"undeclared reviewer", replace("  - plugin: review-bot\n    autoFix: none", "  - plugin: other\n    autoFix: none"), "reviewers[1]: plugin must name a plugin declared"},
		{"bad autoFix", replace("autoFix: medium", "autoFix: critical"), "reviewers[0]: autoFix must be high, medium, low or none"},
		{"missing autoFix", replace("    autoFix: none\n", ""), "reviewers[1]: autoFix is required"},
		{"unknown reviewer key", replace("    autoFix: none\n", "    autoFix: none\n    blocking: true\n"), `reviewers[1]: unknown key "blocking"; use plugin, autoFix and config`},
		{"oversized reviewer config", replace("config: {strict: true}", "config: {note: "+big+"}"), "reviewers[0].config: is 4"},
		{"too many reviewers", withReviewers(MaxReviewers + 1), "reviewers lists 5 entries; at most 4"},
		{"reviewers not a list", strings.Replace(recipe, "atoms:", "reviewers: alice\natoms:", 1), "reviewers must be a list"},
		{"legacy source disagrees", replace("    source: linear\n", "    source: github-issues\n"), `an intake Atom's source must match the Formula's intake source "linear"`},
		{"legacy via disagrees", replace("    via: gitlab\n", "    via: github\n"), `a ship Atom's via must match the Formula's forge "gitlab"`},
		{"legacy via without forge", strings.Replace(recipe, "    type: gate\n", "    type: gate\n  - id: ship\n    type: ship\n    via: gitlab\n", 1), `a ship Atom's via must match the Formula's forge "github"`},
		{"legacy source without plugins", strings.Replace(recipe, "    type: intake\n", "    type: intake\n    source: jira\n", 1), `intake source "github-issues"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
	// The bounds themselves are accepted.
	ok := strings.Replace(recipe, "atoms:", plugins(MaxPlugins)+"atoms:", 1)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("%d plugins refused: %v", MaxPlugins, err)
	}
	if _, err := Parse([]byte(withReviewers(MaxReviewers))); err != nil {
		t.Fatalf("%d reviewers refused: %v", MaxReviewers, err)
	}
}

func TestPluginKeysIntakeNeedsStringKeys(t *testing.T) {
	// One non-string key makes YAML decode intake as map[any]any; that must not
	// skip the intake.source and intake.config checks.
	big := strings.Repeat("x", MaxPluginConfig)
	for _, key := range []string{"1", "~", "true"} {
		for _, body := range []string{
			pluginRecipe,
			strings.Replace(pluginRecipe, "  source: linear\n  repo", "  source: jira\n  repo", 1),
			strings.Replace(pluginRecipe, "    team: ENG\n", "    team: "+big+"\n", 1),
		} {
			yaml := strings.Replace(body, "intake:\n", "intake:\n  "+key+": x\n", 1)
			if yaml == body {
				t.Fatal("fixture lacks an intake block")
			}
			if _, err := Parse([]byte(yaml)); err == nil || !strings.Contains(err.Error(), "intake must be a mapping with string keys") {
				t.Fatalf("key %s: got %v", key, err)
			}
		}
	}
	if _, err := Parse([]byte(pluginRecipe)); err != nil {
		t.Fatalf("the same Formula without the key: %v", err)
	}
}
