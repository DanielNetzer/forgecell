package harness

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const pinnedModel = "model-fixture-1"

func TestClaudeCodingIsolationFlags(t *testing.T) {
	got := build(t, "claude-code", codingRequest(nil))
	if value := flagValue(t, got.Args, "--setting-sources"); value != "project" {
		t.Fatalf("coding loads %q settings, want only project", value)
	}
	if slices.Contains(got.Args, "user") || slices.Contains(got.Args, "--bare") || slices.Contains(got.Args, "--settings") || slices.Contains(got.Args, "--model") {
		t.Fatalf("coding argv gained or kept a flag the contract excludes: %q", got.Args)
	}
	if !slices.Contains(got.Args, "--strict-mcp-config") || flagValue(t, got.Args, "--mcp-config") != `{"mcpServers":{}}` {
		t.Fatalf("project MCP servers are no longer excluded: %q", got.Args)
	}
	if flagValue(t, got.Args, "--disallowedTools") != "Edit(/.claude/**)" {
		t.Fatalf("the .claude edit deny was lost: %q", got.Args)
	}
	if flagValue(t, got.Args, "--permission-mode") != "dontAsk" {
		t.Fatalf("dontAsk permission mode lost: %q", got.Args)
	}
}

func TestClaudeModelPinFlag(t *testing.T) {
	requests := map[string]map[string]any{
		"molecule":            codingRequest(nil),
		"ticket-analysis":     {"kind": "ticket-analysis"},
		"formula-improvement": {"kind": "formula-improvement"},
		"review":              {"kind": ReviewKind},
	}
	for kind, base := range requests {
		t.Run(kind, func(t *testing.T) {
			plain := build(t, "claude-code", base)
			pinned := map[string]any{modelRequestKey: pinnedModel}
			for k, v := range base {
				pinned[k] = v
			}
			got := build(t, "claude-code", pinned)
			if n := strings.Count(strings.Join(got.Args, "\x00"), "\x00--model\x00"); n != 1 {
				t.Fatalf("%d --model flags in %q", n, got.Args)
			}
			if i := argIndex(got.Args, "--model"); i != 6 || got.Args[i+1] != pinnedModel {
				t.Fatalf("--model is not at its fixed position after --permission-mode dontAsk: %q", got.Args)
			}
			without := slices.Delete(slices.Clone(got.Args), 6, 8)
			if !reflect.DeepEqual(without, plain.Args) {
				t.Fatalf("pinning a model changed more than --model:\n%q\n%q", without, plain.Args)
			}
			if got.Input != plain.Input || strings.Contains(got.Input, pinnedModel) || strings.Contains(got.Input, modelRequestKey) {
				t.Fatalf("the pinned model reached the model-visible prompt:\n%s", got.Input)
			}
			if pinned[modelRequestKey] != pinnedModel {
				t.Fatal("BuildInvocation modified the caller's request")
			}
		})
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(`{"kind":"molecule","harnessModel":"`+pinnedModel+`"}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if viaJSON := build(t, "claude-code", decoded); flagValue(t, viaJSON.Args, "--model") != pinnedModel {
		t.Fatalf("a JSON-decoded request lost the model: %q", viaJSON.Args)
	}
	if with := WithModel(codingRequest(nil), pinnedModel); with[modelRequestKey] != pinnedModel {
		t.Fatalf("WithModel did not attach the model: %v", with)
	}
	base := codingRequest(nil)
	if same := WithModel(base, ""); !reflect.DeepEqual(same, base) {
		t.Fatalf("WithModel changed an unpinned request: %v", same)
	}
	if _, leaked := base[modelRequestKey]; leaked {
		t.Fatal("WithModel modified the caller's request")
	}

	for name, value := range map[string]any{
		"empty":         "",
		"blank":         "  ",
		"leading dash":  "--dangerously-skip-permissions",
		"space":         "a b",
		"newline":       "a\nb",
		"NUL":           "a\x00b",
		"too long":      strings.Repeat("m", 129),
		"number":        7,
		"nil":           nil,
		"string list":   []string{pinnedModel},
		"invalid UTF-8": "a\xffb",
	} {
		call, err := BuildInvocation("claude-code", codingRequest(map[string]any{modelRequestKey: value}), "/fixture/result")
		if err == nil || call.Args != nil || call.Input != "" {
			t.Fatalf("%s model was accepted: %+v", name, call)
		}
	}
	if _, err := BuildInvocation("claude-code", codingRequest(map[string]any{modelRequestKey: strings.Repeat("m", 128)}), "/fixture/result"); err != nil {
		t.Fatalf("a model at the length limit was refused: %v", err)
	}

	for _, id := range []string{"codex", "cursor"} {
		for kind, base := range requests {
			request := map[string]any{modelRequestKey: pinnedModel}
			for k, v := range base {
				request[k] = v
			}
			call, err := BuildInvocation(id, request, "/fixture/result")
			if err == nil || call.Args != nil || call.Input != "" {
				t.Fatalf("%s %s accepted a pinned model: %+v", id, kind, call)
			}
		}
		if _, err := BuildInvocation(id, codingRequest(map[string]any{modelRequestKey: pinnedModel}), "/fixture/result"); err == nil || err.Error() != "a pinned model is not supported for "+id+" in v1" {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestClaudeAnalysisAndMetaSettingSources(t *testing.T) {
	meta := build(t, "claude-code", map[string]any{"kind": "formula-improvement"})
	if flagValue(t, meta.Args, "--setting-sources") != "project" || flagValue(t, meta.Args, "--tools") != "" || !slices.Contains(meta.Args, "--strict-mcp-config") || flagValue(t, meta.Args, "--mcp-config") != `{"mcpServers":{}}` {
		t.Fatalf("meta argv: %q", meta.Args)
	}
	if slices.Contains(meta.Args, "--safe-mode") || slices.Contains(meta.Args, "--allowedTools") {
		t.Fatalf("meta argv gained flags: %q", meta.Args)
	}
	if i, j := argIndex(meta.Args, "--mcp-config"), argIndex(meta.Args, "--setting-sources"); j != i+2 {
		t.Fatalf("--setting-sources is not directly after the MCP config: %q", meta.Args)
	}

	withExtensions := build(t, "claude-code", map[string]any{"kind": "ticket-analysis", "mcpServers": []MCPServer{goldenServer()}})
	if flagValue(t, withExtensions.Args, "--setting-sources") != "project" || slices.Contains(withExtensions.Args, "--safe-mode") || slices.Contains(withExtensions.Args, "user") {
		t.Fatalf("analysis with extensions: %q", withExtensions.Args)
	}

	for _, kind := range []string{"ticket-analysis", ReviewKind} {
		safe := build(t, "claude-code", map[string]any{"kind": kind})
		if !slices.Contains(safe.Args, "--safe-mode") || slices.Contains(safe.Args, "--setting-sources") || slices.Contains(safe.Args, "--model") {
			t.Fatalf("%s without extensions changed: %q", kind, safe.Args)
		}
	}
}

func TestFakeProviderReceivesIsolationFlags(t *testing.T) {
	for _, tc := range []struct {
		kind, output string
	}{
		{"molecule", `{"structured_output":{"schemaVersion":"v1","outcome":"completed","reason":"implemented","paths":[]}}`},
		{"formula-improvement", `{"structured_output":{"summary":"s"}}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dir := t.TempDir()
			provider := fake(t, `printf '%s\n' "$@" > argv.txt
cat > stdin.txt
printf '`+tc.output+`'
`)
			request := map[string]any{"kind": tc.kind, modelRequestKey: pinnedModel}
			result := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: provider, Dir: dir, Request: request, Timeout: 30 * time.Second})
			if !result.Process.OK {
				t.Fatalf("%+v", result)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "argv.txt"))
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			if flagValue(t, args, "--setting-sources") != "project" || flagValue(t, args, "--model") != pinnedModel || slices.Contains(args, "user") {
				t.Fatalf("the provider received %q", args)
			}
			if stdin, err := os.ReadFile(filepath.Join(dir, "stdin.txt")); err != nil || strings.Contains(string(stdin), pinnedModel) {
				t.Fatalf("the model reached the provider's prompt: %v", err)
			}
		})
	}

	dir := t.TempDir()
	provider := fake(t, "touch started\ncat > /dev/null\n")
	result := Execute(context.Background(), ExecuteOptions{ID: "codex", Executable: provider, Dir: dir, Request: map[string]any{"kind": "molecule", modelRequestKey: pinnedModel}, Timeout: 30 * time.Second})
	if result.Process.OK || result.Process.Error != "a pinned model is not supported for codex in v1" {
		t.Fatalf("%+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "started")); !os.IsNotExist(err) {
		t.Fatal("a provider started with a model its adapter cannot pin")
	}
}

func claudeBinding(t *testing.T) []string {
	t.Helper()
	provider := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(provider, []byte("v1"), 0700); err != nil {
		t.Fatal(err)
	}
	launcher, _ := fakeBuild(t, "1.0.0", AdapterProtocolVersion)
	return []string{launcher, "__adapter", "claude-code", provider}
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// gitFixture commits files to a new repository and returns its root and commit.
func gitFixture(t *testing.T, files map[string]string) (root, commit string) {
	t.Helper()
	root = t.TempDir()
	testGit(t, root, "init", "-q")
	for name, content := range files {
		writeFile(t, root, name, content)
	}
	// -f: a global gitignore commonly lists .claude/settings.local.json.
	testGit(t, root, "add", "-f", "-A")
	testGit(t, root, "commit", "-qm", "base")
	return root, testGit(t, root, "rev-parse", "HEAD")
}

func commitFile(t *testing.T, root, name, content string) string {
	t.Helper()
	writeFile(t, root, name, content)
	testGit(t, root, "add", "-f", "-A")
	testGit(t, root, "commit", "-qm", "change "+name)
	return testGit(t, root, "rev-parse", "HEAD")
}

func TestFingerprintExcludesUserLevelClaudeConfig(t *testing.T) {
	home, configDir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	command := claudeBinding(t)
	root, commit := gitFixture(t, map[string]string{"code.txt": "x"})
	project := FingerprintInputs{ProjectRoot: root, BaseCommit: commit}
	before, err := FingerprintWith(command, project)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Fingerprint(command)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{
		filepath.Join(configDir, "settings.json"),
		filepath.Join(configDir, "settings.local.json"),
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".claude", "plugins", "installed_plugins.json"),
		filepath.Join(home, ".claude", "CLAUDE.md"),
	} {
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"rewrite-output"}]}]}}`), 0600); err != nil {
			t.Fatal(err)
		}
		if after, err := FingerprintWith(command, project); err != nil || after != before {
			t.Fatalf("editing %s changed the fingerprint of an isolated run: %v", file, err)
		}
		if after, err := Fingerprint(command); err != nil || after != plain {
			t.Fatalf("editing %s changed the plain fingerprint: %v", file, err)
		}
	}
}

func TestFingerprintTracksProjectClaudeSettingsAndModel(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	command := claudeBinding(t)
	root, first := gitFixture(t, map[string]string{"code.txt": "x"})
	fingerprintOf := func(in FingerprintInputs) string {
		t.Helper()
		value, err := FingerprintWith(command, in)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	at := func(commit string) string {
		t.Helper()
		return fingerprintOf(FingerprintInputs{ProjectRoot: root, BaseCommit: commit})
	}
	absent := at(first)
	if absent == fingerprintOf(FingerprintInputs{}) {
		t.Fatal("a project root with no settings is indistinguishable from no project root")
	}
	seen := map[string]string{"absent": absent}
	commits := map[string]string{}
	step := func(name, file, content string) {
		t.Helper()
		commits[name] = commitFile(t, root, ".claude/"+file, content)
		value := at(commits[name])
		for other, previous := range seen {
			if previous == value {
				t.Fatalf("%s is indistinguishable from %s", name, other)
			}
		}
		seen[name] = value
	}
	step("project settings", "settings.json", `{"permissions":{"allow":["Bash(ls)"]}}`)
	step("edited project settings", "settings.json", `{"permissions":{"allow":["Bash(ls)","Bash(pwd)"]}}`)
	step("local settings", "settings.local.json", `{"hooks":{}}`)
	step("edited local settings", "settings.local.json", `{"hooks":{"Stop":[]}}`)

	// The run loads what its workspace holds at the base commit, so neither the
	// source checkout's working tree nor the commit it happens to be at matters.
	settled := commits["edited local settings"]
	unrelated := commitFile(t, root, "code.txt", "changed")
	if at(unrelated) != seen["edited local settings"] {
		t.Fatal("a commit that does not touch the project settings changed the fingerprint")
	}
	writeFile(t, root, ".claude/settings.json", `{"hooks":{"PreToolUse":[]}}`)
	writeFile(t, root, ".claude/settings.local.json", "dirty")
	writeFile(t, root, ".claude/untracked.json", "{}")
	if at(unrelated) != seen["edited local settings"] || at(settled) != seen["edited local settings"] {
		t.Fatal("uncommitted edits in the source checkout reached the fingerprint")
	}
	if at(first) != absent || at(commits["project settings"]) != seen["project settings"] {
		t.Fatal("a base commit other than HEAD did not give the settings committed there")
	}

	for name, in := range map[string]FingerprintInputs{
		"no commit":        {ProjectRoot: root},
		"symbolic":         {ProjectRoot: root, BaseCommit: "HEAD"},
		"option":           {ProjectRoot: root, BaseCommit: "--all"},
		"abbreviated":      {ProjectRoot: root, BaseCommit: first[:12]},
		"unknown commit":   {ProjectRoot: root, BaseCommit: strings.Repeat("0", 40)},
		"not a repository": {ProjectRoot: t.TempDir(), BaseCommit: first},
	} {
		if value, err := FingerprintWith(command, in); err == nil {
			t.Fatalf("%s fingerprinted as %s", name, value)
		}
	}

	pinned := fingerprintOf(FingerprintInputs{ProjectRoot: root, BaseCommit: settled, Model: pinnedModel})
	other := fingerprintOf(FingerprintInputs{ProjectRoot: root, BaseCommit: settled, Model: pinnedModel + "-2"})
	if base := at(settled); base == pinned || pinned == other || base == other {
		t.Fatal("the pinned model is invisible to the fingerprint")
	}
	if again := fingerprintOf(FingerprintInputs{ProjectRoot: root, BaseCommit: settled, Model: pinnedModel}); again != pinned {
		t.Fatal("the fingerprint is not deterministic")
	}

	provider := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(provider, []byte("v1"), 0700); err != nil {
		t.Fatal(err)
	}
	codex := []string{command[0], "__adapter", "codex", provider}
	plain, err := Fingerprint(codex)
	if err != nil {
		t.Fatal(err)
	}
	if with, err := FingerprintWith(codex, FingerprintInputs{ProjectRoot: root, BaseCommit: settled, Model: pinnedModel}); err != nil || with != plain {
		t.Fatalf("project settings or a model changed a Codex fingerprint: %v", err)
	}

	inputs := FingerprintInputs{ProjectRoot: root, BaseCommit: settled, Model: pinnedModel}
	bumped, err := fingerprintWith(command, "not-"+AdapterProtocolVersion, inputs)
	if err != nil || bumped == pinned {
		t.Fatal("protocol change invisible")
	}
	if err := os.WriteFile(command[3], []byte("v2"), 0700); err != nil {
		t.Fatal(err)
	}
	if drifted := fingerprintOf(inputs); drifted == pinned {
		t.Fatal("provider drift invisible")
	}
}

func claudePaths(t *testing.T, files ...string) map[string]string {
	t.Helper()
	paths := map[string]string{}
	for _, name := range files {
		paths[name] = "{}"
	}
	return paths
}

func TestClaudeProjectConfigListsOnlyWhatChangesBehavior(t *testing.T) {
	want := []string{".claude/agents/reviewer.md", ".claude/hooks/pre.sh", ".claude/settings.json", ".claude/settings.local.json", ".claude/settings.team.json"}
	root, commit := gitFixture(t, claudePaths(t, append(slices.Clone(want), ".claude/commands/build.md", ".claude/skills/x/SKILL.md", ".claude/notes.json", "settings.json", "docs/.claude/settings.json", "hooks/pre.sh", "CLAUDE.md")...))
	got, err := ClaudeProjectConfigAtCommit(root, commit)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%q %v", got, err)
	}
	dir := t.TempDir()
	for name := range claudePaths(t, append(slices.Clone(want), ".claude/commands/build.md", "settings.json", "hooks/pre.sh")...) {
		writeFile(t, dir, name, "{}")
	}
	if onDisk, err := ClaudeProjectConfigInDir(dir); err != nil || !reflect.DeepEqual(onDisk, want) {
		t.Fatalf("%q %v", onDisk, err)
	}

	clean, cleanCommit := gitFixture(t, map[string]string{"code.txt": "x", ".claude/commands/build.md": "x"})
	if none, err := ClaudeProjectConfigAtCommit(clean, cleanCommit); err != nil || len(none) != 0 {
		t.Fatalf("%q %v", none, err)
	}
	if none, err := ClaudeProjectConfigInDir(t.TempDir()); err != nil || len(none) != 0 {
		t.Fatalf("%q %v", none, err)
	}
	standIn := t.TempDir()
	writeFile(t, standIn, ".claude", "a file standing in for the directory")
	if got, err := ClaudeProjectConfigInDir(standIn); err != nil || !reflect.DeepEqual(got, []string{".claude"}) {
		t.Fatalf("%q %v", got, err)
	}
	linked := t.TempDir()
	if err := os.MkdirAll(filepath.Join(linked, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(linked, ".claude", "hooks")); err != nil {
		t.Fatal(err)
	}
	if got, err := ClaudeProjectConfigInDir(linked); err != nil || !reflect.DeepEqual(got, []string{".claude/hooks"}) {
		t.Fatalf("a symlink standing in for the hooks directory was missed: %q %v", got, err)
	}

	for _, bad := range []string{"", "HEAD", "-x", commit[:10]} {
		if _, err := ClaudeProjectConfigAtCommit(root, bad); err == nil {
			t.Fatalf("commit %q was accepted", bad)
		}
		if _, err := ClaudeTreeChanges(root, bad); err == nil {
			t.Fatalf("commit %q was accepted for the tree comparison", bad)
		}
	}
}

func TestClaudeTreeChangesFindsEveryDeviationFromTheBaseCommit(t *testing.T) {
	root, commit := gitFixture(t, map[string]string{
		"code.txt":                  "x",
		".gitignore":                ".claude/settings.local.json\n.claude/scratch/\n",
		".claude/commands/c.md":     "command\n",
		".claude/commands/d.md":     "other\n",
		".claude/notes.txt":         "notes\n",
		".claude/skills/a/SKILL.md": "skill\n",
	})
	if err := os.Symlink("commands/c.md", filepath.Join(root, ".claude", "alias.md")); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "add", "-A")
	testGit(t, root, "commit", "-qm", "symlink")
	commit = testGit(t, root, "rev-parse", "HEAD")
	// A linked work tree is what a coding run gets.
	dir := filepath.Join(t.TempDir(), "workspace")
	testGit(t, root, "worktree", "add", "-q", "-b", "run", dir, commit)

	changes := func() []string {
		t.Helper()
		got, err := ClaudeTreeChanges(dir, commit)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	expect := func(name string, want ...string) {
		t.Helper()
		if got := changes(); !slices.Equal(got, want) {
			mode := "absent"
			if info, err := os.Lstat(filepath.Join(dir, ".claude", "alias.md")); err == nil {
				mode = info.Mode().String()
			}
			t.Fatalf("%s: got %q, want %q (alias.md is %s; base tree: %s)", name, got, want, mode, testGit(t, dir, "ls-tree", "-r", commit, "--", ".claude"))
		}
	}
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	expect("a fresh checkout")

	writeFile(t, dir, "code.txt", "edited outside .claude")
	writeFile(t, dir, "docs/.claude/settings.json", "{}")
	expect("edits outside the project .claude")
	writeFile(t, dir, ".claude/commands/c.md", read(".claude/commands/c.md"))
	expect("rewriting a file with the same bytes")

	writeFile(t, dir, ".claude/commands/c.md", "command\nextra\n")
	expect("an edit", ".claude/commands/c.md")
	writeFile(t, dir, ".claude/commands/c.md", "command\n")
	expect("a restored edit")

	writeFile(t, dir, ".claude/settings.json", "{}")
	expect("a created file", ".claude/settings.json")
	writeFile(t, dir, ".claude/settings.local.json", "{}")
	writeFile(t, dir, ".claude/scratch/x", "{}")
	if status := testGit(t, dir, "status", "--porcelain", "--", ".claude"); strings.Contains(status, "settings.local.json") || strings.Contains(status, "scratch") {
		t.Fatalf("the fixture's ignored files are visible to git status, so the test proves nothing: %q", status)
	}
	expect("gitignored files that git status cannot see", ".claude/scratch/x", ".claude/settings.json", ".claude/settings.local.json")
	for _, name := range []string{".claude/settings.json", ".claude/settings.local.json", ".claude/scratch/x"} {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	expect("removed new files")

	writeFile(t, dir, ".claude/hooks/pre.sh", "#!/bin/sh\n")
	expect("a new hook", ".claude/hooks/pre.sh")
	if err := os.RemoveAll(filepath.Join(dir, ".claude", "hooks")); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(dir, ".claude", "notes.txt")); err != nil {
		t.Fatal(err)
	}
	expect("a deleted file", ".claude/notes.txt")
	writeFile(t, dir, ".claude/notes.txt", "notes\n")

	if err := os.Remove(filepath.Join(dir, ".claude", "alias.md")); err != nil {
		t.Fatal(err)
	}
	expect("a deleted symlink", ".claude/alias.md")
	if err := os.Symlink("commands/d.md", filepath.Join(dir, ".claude", "alias.md")); err != nil {
		t.Fatal(err)
	}
	expect("a retargeted symlink", ".claude/alias.md")
	if err := os.Remove(filepath.Join(dir, ".claude", "alias.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, ".claude/alias.md", "now a file")
	expect("a symlink replaced by a file", ".claude/alias.md")
	if err := os.Remove(filepath.Join(dir, ".claude", "alias.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("commands/c.md", filepath.Join(dir, ".claude", "alias.md")); err != nil {
		t.Fatal(err)
	}
	expect("a restored symlink")

	if err := os.Symlink("commands/c.md", filepath.Join(dir, ".claude", "new-alias.md")); err != nil {
		t.Fatal(err)
	}
	expect("a new symlink", ".claude/new-alias.md")
	if err := os.Remove(filepath.Join(dir, ".claude", "new-alias.md")); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(filepath.Join(dir, ".claude")); err != nil {
		t.Fatal(err)
	}
	expect("a removed .claude", ".claude/alias.md", ".claude/commands/c.md", ".claude/commands/d.md", ".claude/notes.txt", ".claude/skills/a/SKILL.md")

	bare, bareCommit := gitFixture(t, map[string]string{"code.txt": "x"})
	if got, err := ClaudeTreeChanges(bare, bareCommit); err != nil || len(got) != 0 {
		t.Fatalf("%q %v", got, err)
	}
	writeFile(t, bare, ".claude/settings.json", "{}")
	if got, err := ClaudeTreeChanges(bare, bareCommit); err != nil || !slices.Equal(got, []string{".claude/settings.json"}) {
		t.Fatalf("%q %v", got, err)
	}
}

func TestAdapterIsolationReport(t *testing.T) {
	want := map[string]string{"claude-code": IsolationPartial, "codex": IsolationPartial, "cursor": IsolationUnknown, "other": IsolationUnknown, "": IsolationUnknown}
	for id, state := range want {
		got := AdapterIsolation(id)
		if got.State != state || strings.TrimSpace(got.Reason) == "" {
			t.Fatalf("%q: %+v", id, got)
		}
	}
	claude := AdapterIsolation("claude-code").Reason
	for _, mention := range []string{"--setting-sources project", "--safe-mode", "User settings, plugins and hooks are excluded", "Not verified, hence partial", "managed (enterprise) policy settings", "user-level CLAUDE.md", "Project .claude/settings.json", "fingerprinted from the base commit", "is refused", "scope violation"} {
		if !strings.Contains(claude, mention) {
			t.Fatalf("the claude-code reason omits %q: %s", mention, claude)
		}
	}
	native := []string{"/forgecell", "__adapter", "claude-code", "/claude"}
	if got := BindingIsolation(native); got != AdapterIsolation("claude-code") {
		t.Fatalf("%+v", got)
	}
	for _, command := range [][]string{{"/custom", "--run"}, {"/custom"}, nil, {"/forgecell", "__adapter", "claude-code"}} {
		if got := BindingIsolation(command); got.State != IsolationUnknown || strings.TrimSpace(got.Reason) == "" {
			t.Fatalf("%q: %+v", command, got)
		}
	}
	if encoded, _ := json.Marshal(AdapterIsolation("codex")); !strings.HasPrefix(string(encoded), `{"state":"partial","reason":"`) {
		t.Fatalf("doctor JSON shape changed: %s", encoded)
	}

	for _, tc := range []struct {
		name      string
		command   []string
		request   map[string]any
		model     string
		isolation string
		refused   bool
	}{
		{"claude coding", native, codingRequest(nil), ModelUnpinned, "claude-code:setting-sources=project", false},
		{"claude coding pinned", native, WithModel(codingRequest(nil), pinnedModel), pinnedModel, "claude-code:setting-sources=project", false},
		{"claude meta", native, map[string]any{"kind": "formula-improvement"}, ModelUnpinned, "claude-code:setting-sources=project", false},
		{"claude analysis", native, map[string]any{"kind": "ticket-analysis"}, ModelUnpinned, "claude-code:safe-mode", false},
		{"claude analysis with extensions", native, map[string]any{"kind": "ticket-analysis", "mcpServers": []MCPServer{goldenServer()}}, ModelUnpinned, "claude-code:setting-sources=project", false},
		{"claude review", native, map[string]any{"kind": ReviewKind, modelRequestKey: pinnedModel}, pinnedModel, "claude-code:safe-mode", false},
		{"codex coding", []string{"/forgecell", "__adapter", "codex", "/codex"}, codingRequest(nil), ModelUnpinned, "codex:user-config", false},
		{"codex analysis", []string{"/forgecell", "__adapter", "codex", "/codex"}, map[string]any{"kind": "ticket-analysis"}, ModelUnpinned, "codex:strict-config", false},
		{"cursor coding", []string{"/forgecell", "__adapter", "cursor", "/agent"}, codingRequest(nil), ModelUnpinned, "cursor:unverified", false},
		{"custom", []string{"/custom"}, codingRequest(nil), ModelUnpinned, "custom:unknown", false},
		{"custom pinned", []string{"/custom"}, WithModel(codingRequest(nil), pinnedModel), pinnedModel + " (passed to a custom command, not enforced)", "custom:unknown", false},
		{"codex pinned", []string{"/forgecell", "__adapter", "codex", "/codex"}, WithModel(codingRequest(nil), pinnedModel), "", "", true},
		{"invalid model", native, codingRequest(map[string]any{modelRequestKey: "-x"}), "", "", true},
		{"unbuildable request", native, map[string]any{"kind": "other"}, "", "", true},
	} {
		model, isolation, err := InvocationProvenance(tc.command, tc.request)
		if (err != nil) != tc.refused || model != tc.model || isolation != tc.isolation {
			t.Fatalf("%s: model %q isolation %q err %v", tc.name, model, isolation, err)
		}
	}
}
