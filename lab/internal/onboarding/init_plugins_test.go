package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"go.yaml.in/yaml/v4"
)

// fakeDescriber answers forge_describe and tracker_describe without starting a
// plugin, and records the Formula each call was given.
type fakeDescriber struct {
	forge      contract.ForgeDescription
	tracker    contract.TrackerDescription
	forgeErr   error
	forgeCalls []formula.Formula
	tracks     []formula.Formula
}

func (d *fakeDescriber) DescribeForge(_ context.Context, _ string, f formula.Formula) (contract.ForgeDescription, error) {
	d.forgeCalls = append(d.forgeCalls, f)
	return d.forge, d.forgeErr
}

func (d *fakeDescriber) DescribeTracker(_ context.Context, _ string, f formula.Formula) (contract.TrackerDescription, error) {
	d.tracks = append(d.tracks, f)
	return d.tracker, nil
}

func gitlabDescriber() *fakeDescriber {
	return &fakeDescriber{
		forge:   contract.ForgeDescription{Identity: "gitlab.example.com/acme/backend", Host: "gitlab.example.com", DisplayName: "GitLab", ChangeNoun: "merge request", DefaultBranch: "main", CIKind: "gitlab-ci", WebURL: "https://gitlab.example.com/acme/backend"},
		tracker: contract.TrackerDescription{DisplayName: "Jira", KeyPattern: `^ABC-[0-9]+$`, States: []string{"open"}, Selectors: []string{"project:"}},
	}
}

const pluginGitLabCI = "test:\n  script:\n    - go test ./...\n    - go vet ./...\n"

type pluginFixture struct {
	root, lab, config string
	forgeSHA          string
	trackerSHA        string
	runner            Runner
	ghCalls           *int
}

func pluginGit(t *testing.T, root string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = root
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, b, err)
	}
}

func writePluginFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// newPluginFixture is a GitLab checkout whose origin is gitlab.example.com/acme/backend,
// a Lab holding a forge and a tracker manifest that pin a placeholder server
// file, and a tracker config. gh is never expected: the runner counts its calls.
func newPluginFixture(t *testing.T, gitlabCI string) pluginFixture {
	t.Helper()
	root := plugintest.TempDir(t)
	pluginGit(t, root, "init", "-q")
	pluginGit(t, root, "config", "user.name", "Test")
	pluginGit(t, root, "config", "user.email", "test@example.test")
	pluginGit(t, root, "remote", "add", "origin", "https://gitlab.example.com/acme/backend.git")
	writePluginFile(t, filepath.Join(root, "go.mod"), []byte("module example/backend\n\ngo 1.27\n"))
	if gitlabCI != "" {
		writePluginFile(t, filepath.Join(root, ".gitlab-ci.yml"), []byte(gitlabCI))
	}
	pluginGit(t, root, "add", ".")
	pluginGit(t, root, "commit", "-qm", "base")

	lab := filepath.Join(plugintest.TempDir(t), "lab")
	if err := os.Mkdir(lab, 0o700); err != nil {
		t.Fatal(err)
	}
	serverPath := filepath.Join(plugintest.TempDir(t), "server")
	body := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(serverPath, body, 0o700); err != nil {
		t.Fatal(err)
	}
	server := pinned.File{Path: serverPath, SHA256: sha(body)}
	forge := plugintest.Manifest(t, server, "forge", "ok")
	tracker := plugintest.Manifest(t, server, "tracker", "ok")
	writePluginFile(t, filepath.Join(lab, "plugins", "gitlab", "plugin.json"), forge)
	writePluginFile(t, filepath.Join(lab, "plugins", "jira", "plugin.json"), tracker)
	config := filepath.Join(plugintest.TempDir(t), "tracker.json")
	writePluginFile(t, config, []byte(`{"site": "example.atlassian.net", "project": "ABC", "limit": 50}`))

	calls := 0
	runner := func(ctx context.Context, o process.Options) process.Result {
		if o.Argv[0] == "gh" {
			calls++
			return process.Result{Error: "gh must not run"}
		}
		return process.Run(ctx, o)
	}
	return pluginFixture{root: root, lab: lab, config: config, forgeSHA: sha(forge), trackerSHA: sha(tracker), runner: runner, ghCalls: &calls}
}

func (f pluginFixture) options(d PluginDescriber) InitOptions {
	return InitOptions{
		Cwd: f.root, LabDir: f.lab, Launcher: "/installed/forgecell", Runner: f.runner,
		Candidates:  []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}},
		ForgePlugin: "plugins/gitlab/plugin.json", Project: "acme/backend",
		TrackerPlugin: "plugins/jira/plugin.json", TrackerConfig: f.config,
		Describer: d,
	}
}

func TestPluginInitProposesTheExactPluginFormula(t *testing.T) {
	f := newPluginFixture(t, pluginGitLabCI)
	d := gitlabDescriber()
	out, err := Prepare(context.Background(), f.options(d))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "pending" || out.Proposal == nil || out.Repository.Repo != "acme/backend" || *f.ghCalls != 0 {
		t.Fatalf("status %s, repo %s, gh calls %d: %+v", out.Status, out.Repository.Repo, *f.ghCalls, out)
	}
	if len(d.forgeCalls) != 1 || len(d.tracks) != 1 {
		t.Fatalf("each plugin must be described once: forge %d, tracker %d", len(d.forgeCalls), len(d.tracks))
	}
	if p, project, ok := d.forgeCalls[0].ForgePlugin(); !ok || p.SHA256 != f.forgeSHA || project != "acme/backend" {
		t.Fatalf("forge described with %+v %q %v", p, project, ok)
	}
	if p, ok := d.tracks[0].TrackerPlugin(); !ok || p.SHA256 != f.trackerSHA || d.tracks[0].Intake.Config["project"] != "ABC" {
		t.Fatalf("tracker described with %+v %v", d.tracks[0], ok)
	}

	got := decodeYAML(t, out.Proposal.YAML)
	if created, _ := got["createdAt"].(string); created == "" {
		t.Fatalf("createdAt missing: %v", got["createdAt"])
	}
	repoContext, _ := got["repositoryContext"].(map[string]any)
	if repoContext["repo"] != "acme/backend" || repoContext["revision"] != out.Repository.Revision || repoContext["defaultBranch"] != "main" {
		t.Fatalf("repositoryContext = %v", repoContext)
	}
	delete(got, "createdAt")
	delete(got, "repositoryContext")
	want := decodeYAML(t, `schemaVersion: v0
kind: formula
id: backend-fake-tracker
name: acme/backend issue workflow
source: bootstrap-assay
plugins:
  - {name: fake-forge, manifest: plugins/gitlab/plugin.json, sha256: `+f.forgeSHA+`}
  - {name: fake-tracker, manifest: plugins/jira/plugin.json, sha256: `+f.trackerSHA+`}
forge: {plugin: fake-forge, project: acme/backend, identity: gitlab.example.com/acme/backend}
intake:
  source: fake-tracker
  repo: acme/backend
  config: {site: example.atlassian.net, project: ABC, limit: 50}
harness: {binding: codex, command: [/installed/forgecell, __adapter, codex, /codex], invoke: byo, timeoutMs: 900000, maxTimeoutMs: 3600000}
lab: {dir: "`+f.lab+`", observe: false}
atoms:
  - {id: intake, type: intake, source: fake-tracker}
  - {id: scope, type: gate, purpose: scope, mode: human}
  - {id: harness, type: harness, binding: codex}
  - {id: check, type: check, workflows: [.gitlab-ci.yml]}
  - {id: gate, type: gate, purpose: review, mode: human}
  - {id: ship, type: ship, via: fake-forge}
  - {id: document, type: document, target: fake-tracker}
requiredChecks:
  - id: baseline-go-test
    argv: [go, test, ./...]
    dir: .
    timeoutMs: 900000
    workflow: .gitlab-ci.yml
    reason: "GitLab CI runs `+"`go test ./...`"+` on merge requests (.gitlab-ci.yml, job test) in the repository root."
  - id: baseline-go-vet
    argv: [go, vet, ./...]
    dir: .
    timeoutMs: 600000
    workflow: .gitlab-ci.yml
    reason: "GitLab CI runs `+"`go vet ./...`"+` on merge requests (.gitlab-ci.yml, job test) in the repository root."
`)
	if !reflect.DeepEqual(got, want) {
		gotText, _ := yaml.Marshal(got)
		wantText, _ := yaml.Marshal(want)
		t.Fatalf("proposal YAML differs:\n got:\n%s\nwant:\n%s", gotText, wantText)
	}
	if !strings.Contains(out.Note, "from .gitlab-ci.yml") || !strings.Contains(out.Note, "pinned by SHA-256 on first use") {
		t.Fatalf("note = %s", out.Note)
	}

	reviewed, err := InspectProposal(f.lab, out.Proposal.ID)
	if err != nil || reviewed.YAML != out.Proposal.YAML {
		t.Fatalf("review shows other bytes: %v", err)
	}
	if _, err = Review(f.lab, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	approved, err := formula.Load(f.lab, "")
	if err != nil || !approved.Approved || approved.Formula.YAML != out.Proposal.YAML {
		t.Fatalf("approve did not activate the reviewed bytes: %v", err)
	}
	if p, ok := approved.Formula.TrackerPlugin(); !ok || p.Name != "fake-tracker" {
		t.Fatalf("approved tracker %+v %v", p, ok)
	}
}

func TestPluginInitCIKindNoneDerivesNoBaseline(t *testing.T) {
	f := newPluginFixture(t, pluginGitLabCI)
	d := gitlabDescriber()
	d.forge.CIKind = "none"
	out, err := Prepare(context.Background(), f.options(d))
	if err != nil {
		t.Fatal(err)
	}
	got := decodeYAML(t, out.Proposal.YAML)
	if _, ok := got["requiredChecks"]; ok {
		t.Fatalf("ciKind none recorded a baseline: %v", got["requiredChecks"])
	}
	atoms := got["atoms"].([]any)
	if check := atoms[3].(map[string]any); !reflect.DeepEqual(check["workflows"], []any{}) {
		t.Fatalf("check atom cites %v", check["workflows"])
	}
}

func TestPluginInitWithoutGitLabCIFileDerivesNoBaseline(t *testing.T) {
	f := newPluginFixture(t, "")
	out, err := Prepare(context.Background(), f.options(gitlabDescriber()))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeYAML(t, out.Proposal.YAML)["requiredChecks"]; ok {
		t.Fatal("a checkout without .gitlab-ci.yml recorded a baseline")
	}
}

// labEntries lists the Lab's entries other than the fixture's own plugins.
func labEntries(t *testing.T, lab string) []string {
	t.Helper()
	entries, err := os.ReadDir(lab)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != "plugins" {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestPluginInitRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(t *testing.T, f pluginFixture, o *InitOptions, d *fakeDescriber)
		want string
	}{
		"missing project": {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) { o.Project = "" }, "always come together"},
		"missing tracker": {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) { o.TrackerPlugin = "" }, "always come together"},
		"missing config":  {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) { o.TrackerConfig = "" }, "always come together"},
		"missing forge":   {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) { o.ForgePlugin = "" }, "always come together"},
		"rebind":          {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) { o.Rebind = "codex" }, "do not combine them with --rebind"},
		"model":           {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) { o.ModelSet = true }, "do not combine them with --rebind, --model"},
		"enable review":   {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) { o.EnableReview = true }, "--enable-review"},
		"refresh":         {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) { o.RefreshBaseline = true }, "--refresh-baseline"},
		"meta":            {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) { o.MetaHarness = "codex" }, "--meta-harness"},
		"nested group": {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) {
			o.Project = "acme/platform/backend"
		}, "nested groups such as group/subgroup/repo are not supported yet"},
		"bad project": {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) { o.Project = "acme/.." }, "--project must be exactly owner/repo"},
		"traversing manifest": {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) {
			o.ForgePlugin = "../plugin.json"
		}, "--forge-plugin must be a clean Lab-relative path"},
		"reserved manifest": {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) {
			o.TrackerPlugin = "workspaces/plugin.json"
		}, "--tracker-plugin must not be under workspaces/"},
		"wrong forge kind": {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) {
			o.ForgePlugin = "plugins/jira/plugin.json"
		}, "is a tracker plugin; it must be a forge plugin"},
		"wrong tracker kind": {func(_ *testing.T, _ pluginFixture, o *InitOptions, _ *fakeDescriber) {
			o.TrackerPlugin = "plugins/gitlab/plugin.json"
		}, "is a forge plugin; it must be a tracker plugin"},
		"symlinked manifest": {func(t *testing.T, f pluginFixture, o *InitOptions, _ *fakeDescriber) {
			if err := os.Symlink(filepath.Join(f.lab, "plugins", "gitlab"), filepath.Join(f.lab, "plugins", "linked")); err != nil {
				t.Fatal(err)
			}
			o.ForgePlugin = "plugins/linked/plugin.json"
		}, "symlink"},
		"oversized config": {func(t *testing.T, f pluginFixture, o *InitOptions, _ *fakeDescriber) {
			writePluginFile(t, f.config, []byte(`{"pad": "`+strings.Repeat("x", 4<<10)+`"}`))
		}, "--tracker-config is larger than 4096 bytes"},
		"array config": {func(t *testing.T, f pluginFixture, o *InitOptions, _ *fakeDescriber) {
			writePluginFile(t, f.config, []byte(`["ABC"]`))
		}, "--tracker-config must hold a JSON object"},
		"duplicate config key": {func(t *testing.T, f pluginFixture, o *InitOptions, _ *fakeDescriber) {
			writePluginFile(t, f.config, []byte(`{"project": "A", "project": "B"}`))
		}, "--tracker-config repeats a key"},
		"trailing config": {func(t *testing.T, f pluginFixture, o *InitOptions, _ *fakeDescriber) {
			writePluginFile(t, f.config, []byte(`{"project": "A"} {}`))
		}, "--tracker-config must hold one JSON object"},
		"huge integer config": {func(t *testing.T, f pluginFixture, o *InitOptions, _ *fakeDescriber) {
			writePluginFile(t, f.config, []byte(`{"limit": 123456789012345678901234567890}`))
		}, "outside the 64-bit range"},
		"symlinked config": {func(t *testing.T, f pluginFixture, o *InitOptions, _ *fakeDescriber) {
			link := filepath.Join(filepath.Dir(f.config), "link.json")
			if err := os.Symlink(f.config, link); err != nil {
				t.Fatal(err)
			}
			o.TrackerConfig = link
		}, "not a symlink"},
		"origin mismatch": {func(t *testing.T, f pluginFixture, _ *InitOptions, _ *fakeDescriber) {
			pluginGit(t, f.root, "remote", "set-url", "origin", "git@gitlab.example.com:acme/other.git")
		}, "the origin remote resolves to gitlab.example.com/acme/other, but the forge plugin describes gitlab.example.com/acme/backend"},
		"unknown ci kind": {func(_ *testing.T, _ pluginFixture, _ *InitOptions, d *fakeDescriber) { d.forge.CIKind = "jenkins" }, "CI kind must be"},
		"invalid identity": {func(_ *testing.T, _ pluginFixture, _ *InitOptions, d *fakeDescriber) {
			d.forge.Identity = "GitLab.example.com/../x"
		}, "identity"},
		"describe failure": {func(_ *testing.T, _ pluginFixture, _ *InitOptions, d *fakeDescriber) {
			d.forgeErr = errors.New("plugin refused")
		}, "describing the forge plugin fake-forge: plugin refused"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPluginFixture(t, pluginGitLabCI)
			d := gitlabDescriber()
			o := f.options(d)
			c.edit(t, f, &o, d)
			out, err := Prepare(context.Background(), o)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q (%+v)", err, c.want, out)
			}
			if strings.Contains(err.Error(), f.root) || strings.Contains(err.Error(), filepath.Dir(f.config)) {
				t.Fatalf("refusal names a local path: %v", err)
			}
			if names := labEntries(t, f.lab); len(names) != 0 {
				t.Fatalf("a refused init wrote %v", names)
			}
			if *f.ghCalls != 0 {
				t.Fatalf("gh ran %d times", *f.ghCalls)
			}
		})
	}
}

func TestPluginInitOriginMismatchNeverStartsTheTracker(t *testing.T) {
	f := newPluginFixture(t, pluginGitLabCI)
	pluginGit(t, f.root, "remote", "set-url", "origin", "git@gitlab.example.com:acme/other.git")
	d := gitlabDescriber()
	if _, err := Prepare(context.Background(), f.options(d)); err == nil || !strings.Contains(err.Error(), "but the forge plugin describes gitlab.example.com/acme/backend") {
		t.Fatalf("err = %v", err)
	}
	if len(d.forgeCalls) != 1 || len(d.tracks) != 0 {
		t.Fatalf("an origin mismatch must stop before the tracker starts: forge %d, tracker %d", len(d.forgeCalls), len(d.tracks))
	}
}

func TestPlainBranch(t *testing.T) {
	for _, name := range []string{"main", "release/1.2", "v1.0", "9-hotfix", "feature/a_b-c"} {
		if !plainBranch(name) {
			t.Errorf("plainBranch(%q) = false", name)
		}
	}
	for _, name := range []string{"", "-main", ".main", "_main", "/main", "release//1", "release/", "release/./x", "release/../x", "..", ".", "main.lock", "release/x.lock", "main branch", strings.Repeat("a", 256)} {
		if plainBranch(name) {
			t.Errorf("plainBranch(%q) = true", name)
		}
	}
}

func TestPluginInitRefusesAnUnplainDefaultBranch(t *testing.T) {
	for _, branch := range []string{"-main", "release//1", "release/", "release/../x", "main.lock"} {
		f := newPluginFixture(t, pluginGitLabCI)
		d := gitlabDescriber()
		d.forge.DefaultBranch = branch
		if _, err := Prepare(context.Background(), f.options(d)); err == nil || !strings.Contains(err.Error(), "no plain default branch") {
			t.Fatalf("branch %q: err = %v", branch, err)
		}
		if len(d.tracks) != 0 {
			t.Fatalf("branch %q: the tracker was described", branch)
		}
	}
}

func TestPluginInitRefusesALabWithAFormula(t *testing.T) {
	f := newPluginFixture(t, pluginGitLabCI)
	first, err := Prepare(context.Background(), f.options(gitlabDescriber()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Prepare(context.Background(), f.options(gitlabDescriber())); err == nil || !strings.Contains(err.Error(), "a proposal is already pending") {
		t.Fatalf("a pending proposal was not refused: %v", err)
	}
	if _, err = Review(f.lab, first.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	d := gitlabDescriber()
	if _, err = Prepare(context.Background(), f.options(d)); err == nil || !strings.Contains(err.Error(), "the Lab already has a Formula (backend-fake-tracker); adding plugins to an existing Formula is a hand-reviewed change") {
		t.Fatalf("an approved Formula was not refused: %v", err)
	}
	if len(d.forgeCalls)+len(d.tracks) != 0 {
		t.Fatal("a refused init started a plugin")
	}
}

// TestGitHubInitUnchangedByPluginFields: the GitHub bootstrap names no plugin
// key, and its YAML keys and atoms are the GitHub set.
func TestGitHubInitUnchangedByPluginFields(t *testing.T) {
	root, runner := repoFixture(t)
	dir := filepath.Join(t.TempDir(), "lab")
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/installed/forgecell", Candidates: []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}}, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeYAML(t, out.Proposal.YAML)
	var keys []string
	for key := range doc {
		keys = append(keys, key)
	}
	want := []string{"atoms", "createdAt", "harness", "id", "intake", "kind", "lab", "name", "repositoryContext", "schemaVersion", "source"}
	if !sameSet(keys, want) {
		t.Fatalf("GitHub bootstrap keys = %v, want %v", keys, want)
	}
	if !reflect.DeepEqual(doc["intake"], map[string]any{"source": "github-issues", "repo": "acme/widgets"}) || doc["id"] != "widgets-github-issue" {
		t.Fatalf("GitHub intake %v id %v", doc["intake"], doc["id"])
	}
	atoms := doc["atoms"].([]any)
	if ship := atoms[5].(map[string]any); ship["via"] != "github" {
		t.Fatalf("ship %v", ship)
	}
	if out.Repository.GitLabCI || strings.Contains(out.Proposal.YAML, "plugins") {
		t.Fatalf("GitHub evidence changed: %+v", out.Repository)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}

func TestReadRepositoryWithRequiresTheDescribedOrigin(t *testing.T) {
	f := newPluginFixture(t, pluginGitLabCI)
	id := ForgeIdentity{Identity: "gitlab.example.com/acme/backend", DefaultBranch: "main", CIKind: "gitlab-ci"}
	r, err := ReadRepositoryWith(context.Background(), f.root, f.runner, id)
	if err != nil {
		t.Fatal(err)
	}
	if !r.GitLabCI || r.DefaultBranch != "main" || r.Repo != "" || *f.ghCalls != 0 {
		t.Fatalf("%+v gh %d", r, *f.ghCalls)
	}
	found := false
	for _, e := range r.Evidence {
		found = found || e.Path == ".gitlab-ci.yml" && e.Content == pluginGitLabCI
	}
	if !found {
		t.Fatal(".gitlab-ci.yml was not collected for gitlab-ci")
	}
	none, err := ReadRepositoryWith(context.Background(), f.root, f.runner, ForgeIdentity{Identity: id.Identity, DefaultBranch: "main", CIKind: "none"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range none.Evidence {
		if e.Path == ".gitlab-ci.yml" {
			t.Fatal(".gitlab-ci.yml was collected for CI kind none")
		}
	}
	for _, bad := range []ForgeIdentity{{Identity: "gitlab.example.com/acme/other", DefaultBranch: "main"}, {Identity: "not-a-host", DefaultBranch: "main"}, {Identity: id.Identity}} {
		if _, err = ReadRepositoryWith(context.Background(), f.root, f.runner, bad); err == nil {
			t.Fatalf("%+v was accepted", bad)
		}
	}
	pluginGit(t, f.root, "remote", "set-url", "origin", "https://github.com/acme/backend.git")
	if _, err = ReadRepositoryWith(context.Background(), f.root, f.runner, id); err == nil || !strings.Contains(err.Error(), "github.com/acme/backend") {
		t.Fatalf("a GitHub origin was accepted for a GitLab identity: %v", err)
	}
}
