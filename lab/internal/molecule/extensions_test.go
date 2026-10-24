package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/extension"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
)

type extCfg struct {
	Required, Analysis, Coding bool
	// Wrapper binds the analysis role to a custom harness with a pinned analysis
	// wrapper, so analysis cannot attach servers.
	Wrapper                 bool
	PrepareBody, ServerBody string
	ServerArgs              []string
	Tools                   []extension.Tool
}

type extEnv struct {
	t       *testing.T
	o       Options
	root    string
	records string
	server  string
}

const defaultPrepareBody = "echo run >> RECORDS/runs\nprintf indexed > \"$1/graph.json\"\n"
const defaultServerBody = "/usr/bin/env > RECORDS/server-env.txt\nprintf '%s\\n' \"$@\" > RECORDS/server-argv.txt\n"

// fakeClaude records its argv and prompt per role, runs the MCP server named in
// --mcp-config the way Claude Code would, and answers with a fixed outcome.
const fakeClaude = `#!/bin/sh
case "$1" in --help) echo "--safe-mode --tools --strict-mcp-config --json-schema --no-chrome --disable-slash-commands"; exit 0;; esac
kind=coding
for a in "$@"; do [ "$a" = "--tools" ] && kind=analysis; done
printf '%s\n' "$@" > RECORDS/argv-$kind.txt
/usr/bin/env > RECORDS/claude-env-$kind.txt
cat > RECORDS/stdin-$kind.txt
prev=
cfg=
for a in "$@"; do [ "$prev" = "--mcp-config" ] && cfg=$a; prev=$a; done
cmd=$(printf '%s' "$cfg" | sed -n 's/.*"command":"\([^"]*\)".*/\1/p')
srvargs=$(printf '%s' "$cfg" | sed -n 's/.*"args":\[\([^]]*\)\].*/\1/p' | sed 's/","/\
/g; s/^"//; s/"$//')
if [ -n "$cmd" ]; then
  IFS='
'
  set -f
  set -- $srvargs
  "$cmd" "$@" >/dev/null 2>&1 </dev/null
fi
if [ "$kind" = analysis ]; then printf '%s' '{"is_error":true,"result":"stop"}'; exit 0; fi
printf changed > code.txt
printf '%s' '{"structured_output":{"schemaVersion":"v1","outcome":"completed","reason":"implemented","paths":[]}}'
`

func newExtensionEnv(t *testing.T, cfg extCfg) *extEnv {
	t.Helper()
	// The pinned-file placement rules refuse a path that resolves through a
	// symlink; fixture resolves its own temp root, whatever TMPDIR or GOTMPDIR
	// the environment set.
	o, _ := claudeAllowlistFixture(t)
	root := filepath.Dir(o.SourceRoot)
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	records := filepath.Join(root, "records")
	err := os.MkdirAll(records, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	e := &extEnv{t: t, o: o, root: root, records: records}
	subst := func(s string) string { return strings.ReplaceAll(s, "RECORDS", records) }
	if err = writeStub(filepath.Join(root, "claude"), []byte(subst(fakeClaude)), 0o700); err != nil {
		t.Fatal(err)
	}
	prepare, server := cfg.PrepareBody, cfg.ServerBody
	if prepare == "" {
		prepare = defaultPrepareBody
	}
	if server == "" {
		server = defaultServerBody
	}
	prepareFile := e.tool("prepare", subst(prepare))
	e.server = e.tool("server", subst(server))
	args := cfg.ServerArgs
	if args == nil {
		args = []string{"--graph", "{{CACHE}}/graph.json"}
	}
	tools := cfg.Tools
	if tools == nil {
		tools = []extension.Tool{{Name: "query_graph", Access: "read-only"}, {Name: "annotate", Access: "read-write"}}
	}
	m := extension.Manifest{SchemaVersion: "v1", Kind: "mcp-provider", Name: "fake",
		Server:       extension.Command{Executable: e.pin(e.server), Args: args},
		Prepare:      &extension.Prepare{Command: extension.Command{Executable: e.pin(prepareFile), Args: []string{"{{CACHE}}"}}, TimeoutMS: 60000, Network: "none"},
		Tools:        tools,
		Roles:        []string{"analysis", "coding"},
		Dependencies: []extension.Dependency{},
		Limitations:  "fake index for tests"}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(o.LabDir, "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(o.LabDir, "extensions", "fake.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	file := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	recipe, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	command, _ := json.Marshal([]string{exe, "__adapter", "claude-code", filepath.Join(root, "claude")})
	block := "extensions:\n  fake: {manifest: extensions/fake.json, sha256: " + hex.EncodeToString(digest[:]) + ", required: " + map[bool]string{true: "true", false: "false"}[cfg.Required] + "}\n"
	text := string(recipe)
	if cfg.Analysis {
		block += "harnesses:\n  analysis: {binding: claude-code, command: " + string(command) + ", extensions: [fake]}\n"
	}
	if cfg.Wrapper {
		block += "harnesses:\n  analysis: {binding: custom, command: [\"/bin/custom-harness\"], analysisWrapper: {manifest: analysis-wrappers/w.json, sha256: " + strings.Repeat("a", 64) + "}, extensions: [fake]}\n"
	}
	if cfg.Coding {
		text = strings.Replace(text, "binding: claude-code\n", "binding: claude-code\n  extensions: [fake]\n", 1)
	}
	text = strings.Replace(text, "\natoms:", "\n"+block+"atoms:", 1)
	if err = os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	approveFixture(t, o.LabDir, text, "extensions")
	return e
}

func (e *extEnv) tool(name, body string) string {
	e.t.Helper()
	path := filepath.Join(e.root, "exttools", name, "file")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := writeStub(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *extEnv) pin(path string) pinned.File {
	e.t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return pinned.File{Path: path, SHA256: hex.EncodeToString(sum[:])}
}

func (e *extEnv) read(name string) string {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.records, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(raw)
}

func (e *extEnv) exists(name string) bool {
	_, err := os.Stat(filepath.Join(e.records, name))
	return err == nil
}

func (e *extEnv) builds() int {
	if !e.exists("runs") {
		return 0
	}
	return strings.Count(e.read("runs"), "run\n")
}

func (e *extEnv) argv(role string) []string {
	return strings.Split(strings.TrimSuffix(e.read("argv-"+role+".txt"), "\n"), "\n")
}

// requireAnalysisRan fails with the recorded intake outcome when the fake
// claude never recorded its analysis argv. The fake analysis always fails, so
// a blocked status alone cannot tell a run that reached the provider from one
// that stopped before it, for example on a capability probe under heavy load.
func (e *extEnv) requireAnalysisRan(r Record) {
	e.t.Helper()
	if e.exists("argv-analysis.txt") {
		return
	}
	attempts := make([]string, len(r.AnalysisAttempts))
	for i, a := range r.AnalysisAttempts {
		attempts[i] = fmt.Sprintf("#%d %s: %s (diagnostics %+v)", a.Ordinal, a.Status, a.Error, a.Diagnostics)
	}
	e.t.Fatalf("the fake claude never ran for analysis: status %s, analysis attempts %q, extensions %+v, notes %q", r.Status, attempts, r.Extensions, r.Notes)
}

// isolatedServerPath checks that a server is launched through env -i with the
// fixed minimal environment and returns the private server path it runs.
func isolatedServerPath(t *testing.T, command string, args []string, original string) string {
	t.Helper()
	if command != "/usr/bin/env" || len(args) < 6 || args[0] != "-i" || args[1] != "PATH=/usr/bin:/bin" || args[4] != "LANG=C.UTF-8" {
		t.Fatalf("the server must start through env -i with a fixed environment: %q %q", command, args)
	}
	home, tmp := strings.TrimPrefix(args[2], "HOME="), strings.TrimPrefix(args[3], "TMPDIR=")
	if !filepath.IsAbs(home) || tmp != filepath.Join(home, "tmp") || !strings.HasPrefix(args[2], "HOME=") || !strings.HasPrefix(args[3], "TMPDIR=") {
		t.Fatalf("HOME and TMPDIR must be absolute private paths: %q", args)
	}
	server := args[5]
	if !filepath.IsAbs(server) || filepath.Clean(server) != server || server == original || !strings.HasPrefix(server, home+string(filepath.Separator)) {
		t.Fatalf("the server must be an absolute private copy: %q (original %s)", server, original)
	}
	return server
}

func lineAfter(lines []string, flag string) string {
	for i, line := range lines {
		if line == flag && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	return "<absent>"
}

func (e *extEnv) formula() formula.Formula {
	e.t.Helper()
	loaded, err := formula.Load(e.o.LabDir, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return loaded.Formula
}

func (e *extEnv) record() *Record {
	head := testGit(e.t, e.o.SourceRoot, "rev-parse", "HEAD")
	return &Record{LabDir: e.o.LabDir, Workspace: Workspace{SourceRoot: e.o.SourceRoot, BaseCommit: head}}
}

func TestExtensionCodingReceivesServerAndReusesIntakeBuild(t *testing.T) {
	e := newExtensionEnv(t, extCfg{Coding: true})
	r, err := runApprovedFixture(t, e.o)
	if err != nil || r.Status != "waiting" {
		t.Fatalf("%v %+v", err, r)
	}
	lines := e.argv("coding")
	var config struct {
		Servers map[string]struct {
			Command string
			Args    []string
			Env     map[string]string
		} `json:"mcpServers"`
	}
	if err = json.Unmarshal([]byte(lineAfter(lines, "--mcp-config")), &config); err != nil {
		t.Fatalf("%v in %q", err, lines)
	}
	server, ok := config.Servers["fake"]
	if !ok || server.Env == nil || len(server.Env) != 0 {
		t.Fatalf("server config %+v", config)
	}
	private := isolatedServerPath(t, server.Command, server.Args, e.server)
	passed := server.Args[6:]
	if len(passed) != 2 || passed[0] != "--graph" || !strings.HasSuffix(passed[1], "/graph.json") || strings.Contains(passed[1], "{{") || !filepath.IsAbs(passed[1]) {
		t.Fatalf("the server arguments did not survive the env wrapper: %q (server %s)", server.Args, private)
	}
	if got := e.read("server-argv.txt"); got != strings.Join(passed, "\n")+"\n" {
		t.Fatalf("the server did not start with its arguments: %q", got)
	}
	if !strings.HasSuffix(lineAfter(lines, "--allowedTools"), ",mcp__fake__query_graph,mcp__fake__annotate") && !strings.HasSuffix(lineAfter(lines, "--allowedTools"), ",mcp__fake__annotate,mcp__fake__query_graph") {
		t.Fatalf("tools not allowlisted: %q", lineAfter(lines, "--allowedTools"))
	}
	if !slices.Contains(lines, "--strict-mcp-config") {
		t.Fatal("strict MCP dropped")
	}
	if e.builds() != 1 {
		t.Fatalf("expected one build shared by intake and coding, got %d", e.builds())
	}
	if len(r.Extensions) != 1 {
		t.Fatalf("%+v", r.Extensions)
	}
	rec := r.Extensions[0]
	if rec.Cache.Outcome != "built" || !slices.Equal(rec.Roles, []string{"coding"}) || len(rec.Tools["coding"]) != 2 || rec.Network != "none" || rec.Kind != "mcp-provider" {
		t.Fatalf("coding reuse must keep the original built outcome: %+v", rec)
	}
	if stdin := e.read("stdin-coding.txt"); strings.Contains(stdin, "forgecell-extension-servers") || strings.Contains(stdin, "mcpServers") {
		t.Fatal("private server paths reached the model-visible prompt")
	}
	data, err := os.ReadFile(filepath.Join(e.o.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "forgecell-extension-servers") || strings.Contains(string(data), "mcpServers") {
		t.Fatal("the ledger holds private server paths or MCP configuration")
	}
	// Other tests and concurrent Lab views share TMPDIR, so only this run's own private
	// directory (the forgecell-extension-servers-* ancestor of the server copy) is checked.
	own := private
	for !strings.HasPrefix(filepath.Base(own), "forgecell-extension-servers-") {
		if parent := filepath.Dir(own); parent != own {
			own = parent
			continue
		}
		t.Fatalf("the server copy %s is not under a forgecell-extension-servers-* directory", private)
	}
	if _, err := os.Lstat(own); !os.IsNotExist(err) {
		t.Fatalf("private server directory left behind: %s (%v)", own, err)
	}
}

func TestExtensionAnalysisReceivesReadOnlyServerWithoutSafeMode(t *testing.T) {
	t.Parallel()
	e := newExtensionEnv(t, extCfg{Analysis: true})
	e.o.Analyze = nil
	r, err := Run(context.Background(), e.o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "blocked" {
		t.Fatalf("the fake analysis answers with a failure: %+v", r.Atoms)
	}
	e.requireAnalysisRan(r)
	lines := e.argv("analysis")
	if lineAfter(lines, "--allowedTools") != "mcp__fake__query_graph" {
		t.Fatalf("analysis must get only the read-only tool: %q", lineAfter(lines, "--allowedTools"))
	}
	if slices.Contains(lines, "--safe-mode") || lineAfter(lines, "--setting-sources") != "project" || lineAfter(lines, "--tools") != "" || !slices.Contains(lines, "--strict-mcp-config") {
		t.Fatalf("analysis flags %q", lines)
	}
	if !strings.Contains(lineAfter(lines, "--mcp-config"), `"fake"`) {
		t.Fatalf("no server for analysis: %q", lines)
	}
	if len(r.Extensions) != 1 || !slices.Equal(r.Extensions[0].Roles, []string{"analysis"}) || len(r.Extensions[0].Tools["analysis"]) != 1 {
		t.Fatalf("%+v", r.Extensions)
	}
	if !slices.Contains(r.Notes, AnalysisWithoutSafeModeDisclosure) {
		t.Fatalf("the JSON record lacks the no-safe-mode note: %q", r.Notes)
	}
	markdown, err := os.ReadFile(filepath.Join(e.o.LabDir, "ledgers", r.ID+".md"))
	if err != nil || !strings.Contains(string(markdown), "## Extensions") || strings.Count(string(markdown), AnalysisWithoutSafeModeDisclosure) < 2 {
		t.Fatalf("the Markdown ledger lacks the extensions section and disclosure: %v", err)
	}
	if strings.Contains(e.read("stdin-analysis.txt"), "forgecell-extension-servers") {
		t.Fatal("private server paths reached the model-visible prompt")
	}
}

func TestExtensionOneBuildSharedByBothRoles(t *testing.T) {
	t.Parallel()
	e := newExtensionEnv(t, extCfg{Analysis: true, Coding: true})
	f, r := e.formula(), e.record()
	analysisServers, err := useExtensions(context.Background(), r, f, formula.AnalysisHarness, resolvedTempDir(t))
	if err != nil || len(analysisServers) != 1 || !slices.Equal(analysisServers[0].Tools, []string{"query_graph"}) {
		t.Fatalf("%v %+v", err, analysisServers)
	}
	codingDir := resolvedTempDir(t)
	codingServers, err := useExtensions(context.Background(), r, f, formula.CodingHarness, codingDir)
	if err != nil || len(codingServers) != 1 || len(codingServers[0].Tools) != 2 {
		t.Fatalf("%v %+v", err, codingServers)
	}
	private := isolatedServerPath(t, codingServers[0].Command, codingServers[0].Args, e.server)
	if !strings.HasPrefix(private, codingDir+string(filepath.Separator)) || codingServers[0].Args[2] != "HOME="+codingDir || codingServers[0].Args[3] != "TMPDIR="+filepath.Join(codingDir, "tmp") {
		t.Fatalf("the server must run from the private directory: %q", codingServers[0].Args)
	}
	if info, err := os.Stat(filepath.Join(codingDir, "tmp")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the private TMPDIR must exist and be private: %v", err)
	}
	for _, arg := range codingServers[0].Args {
		if strings.Contains(arg, "{{") {
			t.Fatalf("unresolved placeholder in %q", codingServers[0].Args)
		}
	}
	if e.builds() != 1 {
		t.Fatalf("builds %d", e.builds())
	}
	rec := r.Extensions[0]
	if rec.Cache.Outcome != "built" || !slices.Equal(rec.Roles, []string{"analysis", "coding"}) {
		t.Fatalf("%+v", rec)
	}
}

func TestExtensionUnusableForRoleIsSkippedNotFatal(t *testing.T) {
	t.Parallel()
	cases := map[string]extCfg{
		"source placeholder in server args": {Coding: true, ServerArgs: []string{"{{SOURCE}}/x"}},
		"tool entry over the bound":         {Coding: true, Tools: []extension.Tool{{Name: strings.Repeat("t", 60), Access: "read-only"}}},
		"no tools for the role":             {Coding: true, Tools: []extension.Tool{{Name: "annotate", Access: "read-write"}}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			e := newExtensionEnv(t, cfg)
			f, r := e.formula(), e.record()
			role := formula.CodingHarness
			if name == "no tools for the role" {
				f.Harnesses = map[string]formula.Binding{"analysis": {Binding: "claude-code", Command: f.Harness.Command, Extensions: []string{"fake"}}}
				role = formula.AnalysisHarness
			}
			servers, err := useExtensions(context.Background(), r, f, role, resolvedTempDir(t))
			if err != nil || len(servers) != 0 {
				t.Fatalf("an optional extension must be skipped: %v %+v", err, servers)
			}
			if r.Extensions[0].Skipped[string(role)] == "" || len(r.Extensions[0].Roles) != 0 {
				t.Fatalf("a skipped role must never be listed as attached: %+v", r.Extensions[0])
			}
			f.Extensions["fake"] = formula.ExtensionRef{Manifest: f.Extensions["fake"].Manifest, SHA256: f.Extensions["fake"].SHA256, Required: true}
			if _, err = useExtensions(context.Background(), e.record(), f, role, resolvedTempDir(t)); err == nil {
				t.Fatal("a required extension that cannot be attached must fail")
			}
		})
	}
}

func pruneAll(t *testing.T, e *extEnv) []string {
	t.Helper()
	removed, err := extension.Prune(e.o.LabDir, "fake", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return removed
}

func TestExtensionAttachHoldsTheCacheLeaseUntilCleanup(t *testing.T) {
	t.Parallel()
	e := newExtensionEnv(t, extCfg{Analysis: true, Coding: true})
	f, r := e.formula(), e.record()
	servers, cleanup, err := attachExtensions(context.Background(), r, f, formula.CodingHarness)
	if err != nil || len(servers) != 1 {
		t.Fatalf("%v %+v", err, servers)
	}
	if removed := pruneAll(t, e); len(removed) != 0 {
		t.Fatalf("Prune removed the entry of a running role: %v", removed)
	}
	cleanup()
	cleanup()
	if removed := pruneAll(t, e); len(removed) != 1 {
		t.Fatalf("after the role finished the entry must be removable, removed %v", removed)
	}
}

func TestExtensionResolveOnlyAndFailedAttachHoldNoLease(t *testing.T) {
	t.Parallel()
	e := newExtensionEnv(t, extCfg{Coding: true})
	f, r := e.formula(), e.record()
	if _, err := useExtensions(context.Background(), r, f, formula.CodingHarness, ""); err != nil {
		t.Fatal(err)
	}
	if removed := pruneAll(t, e); len(removed) != 1 {
		t.Fatalf("a resolve-only call must hold no lease, removed %v", removed)
	}

	// The cache builds, then the required server cannot be launched.
	e = newExtensionEnv(t, extCfg{Coding: true, Required: true, ServerArgs: []string{"{{SOURCE}}/x"}})
	f, r = e.formula(), e.record()
	if _, cleanup, err := attachExtensions(context.Background(), r, f, formula.CodingHarness); err == nil {
		t.Fatal("a required extension that cannot be attached must fail")
	} else {
		cleanup()
	}
	if e.builds() != 1 {
		t.Fatalf("the cache must have been built once, got %d", e.builds())
	}
	if removed := pruneAll(t, e); len(removed) != 1 {
		t.Fatalf("a failed attach must leave no lease behind, removed %v", removed)
	}
}

func TestServerArgsRefuseAnEscapingCacheSuffix(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"{{CACHE}}/../x", "{{CACHE}}//x", "{{CACHE}}/a/../../x", "{{CACHE}}/./x", "{{CACHE}}/x/", "{{CACHE}}/"} {
		if _, err := serverArgs([]string{arg}, "/cache"); err == nil {
			t.Fatalf("%q was accepted", arg)
		}
	}
	got, err := serverArgs([]string{"{{CACHE}}", "{{CACHE}}/graph.json", "plain"}, "/cache")
	if err != nil || !slices.Equal(got, []string{"/cache", "/cache/graph.json", "plain"}) {
		t.Fatalf("%v %v", got, err)
	}
}

func TestExtensionLinesShowTheDroppedLinkNote(t *testing.T) {
	t.Parallel()
	e := ExtensionRecord{Name: "fake", Kind: "mcp-provider", Cache: ExtensionCache{Outcome: "built", Code: 0, Note: "the snapshot left out 1 tracked symbolic or hard links"}}
	if lines := strings.Join(e.Lines(), "\n"); !strings.Contains(lines, "cache note: the snapshot left out 1 tracked") {
		t.Fatalf("the note is missing:\n%s", lines)
	}
	e.Cache.Note = ""
	if lines := strings.Join(e.Lines(), "\n"); strings.Contains(lines, "cache note") {
		t.Fatalf("a record without a note gained a line:\n%s", lines)
	}
}

func TestExtensionUnsupportedAdapterIsUnavailable(t *testing.T) {
	t.Parallel()
	e := newExtensionEnv(t, extCfg{Coding: true})
	f, r := e.formula(), e.record()
	f.Harness.Command = []string{"/bin/custom-harness"}
	servers, err := useExtensions(context.Background(), r, f, formula.CodingHarness, resolvedTempDir(t))
	if err != nil || len(servers) != 0 || !strings.Contains(r.Extensions[0].Skipped["coding"], "cannot attach extensions") {
		t.Fatalf("%v %+v %+v", err, servers, r.Extensions)
	}
	if e.builds() != 0 {
		t.Fatal("an extension no adapter can attach must not be prepared")
	}
}

func TestExtensionUnavailableOptionalDoesNotBlock(t *testing.T) {
	t.Parallel()
	e := newExtensionEnv(t, extCfg{Coding: true, PrepareBody: "echo run >> RECORDS/runs\nexit 1\n"})
	r, err := runApprovedFixture(t, e.o)
	if err != nil || r.Status != "waiting" {
		t.Fatalf("%v %+v", err, r)
	}
	rec := r.Extensions[0]
	if rec.Cache.Outcome != "unavailable" || !strings.Contains(rec.Cache.Reason, "prepare") || len(rec.Roles) != 0 || rec.Skipped["coding"] == "" {
		t.Fatalf("%+v", rec)
	}
	lines := e.argv("coding")
	if lineAfter(lines, "--mcp-config") != `{"mcpServers":{}}` || strings.Contains(strings.Join(lines, " "), "mcp__fake") {
		t.Fatalf("an unavailable extension must leave an empty MCP config: %q", lines)
	}
}

func TestExtensionUnavailableRequiredStopsIntake(t *testing.T) {
	t.Parallel()
	e := newExtensionEnv(t, extCfg{Required: true, Coding: true, PrepareBody: "echo run >> RECORDS/runs\nexit 1\n"})
	r, err := Run(context.Background(), e.o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "blocked" || r.Atoms[0].Status != "blocked" || r.Atoms[1].Status != "skipped" || r.Readiness != nil {
		t.Fatalf("%+v", r.Atoms)
	}
	for _, want := range []string{`required extension "fake"`, "prepare", "reviewed Formula change", "set required to false"} {
		if !strings.Contains(r.Atoms[0].Detail, want) {
			t.Fatalf("message lacks %q: %s", want, r.Atoms[0].Detail)
		}
	}
	if e.exists("argv-coding.txt") || e.exists("argv-analysis.txt") {
		t.Fatal("a harness started before the required extension was checked")
	}
	if len(r.Extensions) != 1 || r.Extensions[0].Cache.Outcome != "unavailable" || !r.Extensions[0].Required {
		t.Fatalf("the unavailable extension must stay recorded: %+v", r.Extensions)
	}
}

func TestExtensionRequiredBecomingUnavailableStartsNoCodingHarness(t *testing.T) {
	t.Parallel()
	e := newExtensionEnv(t, extCfg{Required: true, Coding: true})
	r, err := Run(context.Background(), e.o)
	if err != nil || r.Readiness == nil {
		t.Fatalf("%v %+v", err, r)
	}
	if err = writeStub(e.server, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.o.Approve = r.Readiness.Plans[0].Digest
	done, err := Run(context.Background(), e.o)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "failed" || len(done.HarnessAttempts) != 0 || !strings.Contains(done.Atoms[2].Detail, `required extension "fake"`) {
		t.Fatalf("%+v", done.Atoms)
	}
	if e.exists("argv-coding.txt") {
		t.Fatal("the coding harness started")
	}
}

func TestExtensionServerDeathStillLetsTheRoleFinish(t *testing.T) {
	t.Parallel()
	e := newExtensionEnv(t, extCfg{Coding: true, ServerBody: "exit 1\n"})
	r, err := runApprovedFixture(t, e.o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "waiting" || len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].Coding == nil || r.HarnessAttempts[0].Coding.Outcome != "completed" {
		t.Fatalf("%+v", r.HarnessAttempts)
	}
	if len(r.Extensions) != 1 || r.Extensions[0].Cache.Outcome != "built" || !slices.Equal(r.Extensions[0].Roles, []string{"coding"}) {
		t.Fatalf("the extension was available and attached: %+v", r.Extensions)
	}
}

func TestExtensionsLeaveTheWorkspaceCaptureUnchanged(t *testing.T) {
	t.Parallel()
	base, _ := claudeAllowlistFixture(t)
	want, err := runApprovedFixture(t, base)
	if err != nil || want.Capture == nil {
		t.Fatalf("%v", err)
	}
	e := newExtensionEnv(t, extCfg{Coding: true})
	got, err := runApprovedFixture(t, e.o)
	if err != nil || got.Capture == nil {
		t.Fatalf("%v", err)
	}
	if len(got.Capture.Violations) != 0 || got.Capture.Tree != want.Capture.Tree || len(got.Capture.Changes) != len(want.Capture.Changes) {
		t.Fatalf("extensions changed the capture:\n got %+v\nwant %+v", got.Capture, want.Capture)
	}
	cache := filepath.Join(e.o.LabDir, "extensions")
	if _, err = os.Stat(cache); err != nil {
		t.Fatalf("the build should have used the Lab cache: %v", err)
	}
	if rel, relErr := filepath.Rel(got.Workspace.Path, cache); relErr != nil || !strings.HasPrefix(rel, "..") {
		t.Fatalf("the cache %s lies inside the checkout", cache)
	}
	if _, err = os.Stat(filepath.Join(got.Workspace.Path, "extensions")); !os.IsNotExist(err) {
		t.Fatal("the checkout gained an extensions directory")
	}
}

var ambientSecrets = map[string]string{
	"ANTHROPIC_API_KEY":       "anthropic-secret",
	"CLAUDE_CODE_OAUTH_TOKEN": "oauth-secret",
	"GH_TOKEN":                "gh-secret",
	"GITHUB_TOKEN":            "github-secret",
	"NPM_TOKEN":               "npm-secret",
	"HTTPS_PROXY":             "http://u:p@h",
}

func setAmbientSecrets(t *testing.T) {
	t.Helper()
	for name, value := range ambientSecrets {
		t.Setenv(name, value)
	}
}

// envLines returns a recorded environment as sorted lines without the variables a
// shell adds or changes by itself.
func envLines(raw string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		name, _, _ := strings.Cut(line, "=")
		if !slices.Contains([]string{"PWD", "OLDPWD", "SHLVL", "_"}, name) {
			lines = append(lines, line)
		}
	}
	slices.Sort(lines)
	return lines
}

// runRole runs the role against the fake claude and returns what the Claude
// process and the extension server each saw as their environment.
func (e *extEnv) runRole(role string) (claude, server []string) {
	e.t.Helper()
	if role == "analysis" {
		e.o.Analyze = nil
		r, err := Run(context.Background(), e.o)
		if err != nil {
			e.t.Fatal(err)
		}
		e.requireAnalysisRan(r)
	} else if _, err := runApprovedFixture(e.t, e.o); err != nil {
		e.t.Fatal(err)
	}
	claude = envLines(e.read("claude-env-" + role + ".txt"))
	if e.exists("server-env.txt") {
		server = envLines(e.read("server-env.txt"))
	}
	return claude, server
}

func TestExtensionServerEnvironmentIsIsolatedFromTheLab(t *testing.T) {
	for _, role := range []string{"coding", "analysis"} {
		t.Run(role, func(t *testing.T) {
			setAmbientSecrets(t)
			e := newExtensionEnv(t, extCfg{Analysis: role == "analysis", Coding: role == "coding"})
			_, server := e.runRole(role)
			if len(server) == 0 {
				t.Fatal("the extension server never ran or recorded nothing")
			}
			names := map[string]string{}
			for _, line := range server {
				name, value, _ := strings.Cut(line, "=")
				names[name] = value
			}
			if names["PATH"] != "/usr/bin:/bin" || names["LANG"] != "C.UTF-8" || !filepath.IsAbs(names["HOME"]) || names["TMPDIR"] != filepath.Join(names["HOME"], "tmp") {
				t.Fatalf("the server did not get the fixed minimal environment: %q", server)
			}
			if len(names) != 4 {
				t.Fatalf("the server environment holds more than the four fixed variables: %q", server)
			}
			joined := strings.Join(server, "\n")
			for name, value := range ambientSecrets {
				if _, leaked := names[name]; leaked || strings.Contains(joined, value) {
					t.Fatalf("the extension server inherited %s", name)
				}
			}
		})
	}
}

func TestExtensionAttachingDoesNotChangeTheClaudeEnvironment(t *testing.T) {
	for _, role := range []string{"coding", "analysis"} {
		t.Run(role, func(t *testing.T) {
			setAmbientSecrets(t)
			baseline, none := newExtensionEnv(t, extCfg{}).runRole(role)
			if len(none) != 0 {
				t.Fatal("a role without extensions started a server")
			}
			with, _ := newExtensionEnv(t, extCfg{Analysis: role == "analysis", Coding: role == "coding"}).runRole(role)
			if !slices.Equal(baseline, with) {
				t.Fatalf("attaching extensions changed the Claude process environment:\n baseline %q\n with      %q", baseline, with)
			}
			if role == "coding" {
				for name, value := range ambientSecrets {
					if !slices.Contains(with, name+"="+value) {
						t.Fatalf("the Claude process lost %s", name)
					}
				}
			}
		})
	}
}

func TestExtensionUnavailableWithoutEnvBinary(t *testing.T) {
	e := newExtensionEnv(t, extCfg{Coding: true})
	f, r := e.formula(), e.record()
	missing := filepath.Join(resolvedTempDir(t), "env")
	was := serverEnvBinary
	serverEnvBinary = missing
	t.Cleanup(func() { serverEnvBinary = was })
	servers, err := useExtensions(context.Background(), r, f, formula.CodingHarness, resolvedTempDir(t))
	if err != nil || len(servers) != 0 || !strings.Contains(r.Extensions[0].Skipped["coding"], missing) || r.Extensions[0].Cache.Outcome != "unavailable" {
		t.Fatalf("an optional extension must be recorded unavailable: %v %+v %+v", err, servers, r.Extensions)
	}
	if e.builds() != 0 {
		t.Fatal("an extension that cannot be hosted must not be prepared")
	}
	f.Extensions["fake"] = formula.ExtensionRef{Manifest: f.Extensions["fake"].Manifest, SHA256: f.Extensions["fake"].SHA256, Required: true}
	if _, err = useExtensions(context.Background(), e.record(), f, formula.CodingHarness, resolvedTempDir(t)); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("a required extension must fail with the reason: %v", err)
	}
}

// analysisThroughOther runs intake with analysis bound to a path that cannot
// attach servers: a supplied analysis function or a pinned analysis wrapper.
func (e *extEnv) analysisThroughOther(wrapper bool) (Record, error) {
	e.t.Helper()
	if wrapper {
		e.o.Analyze = nil
	}
	return Run(context.Background(), e.o)
}

func TestExtensionOptionalSkippedWhenAnalysisCannotAttach(t *testing.T) {
	t.Parallel()
	for name, wrapper := range map[string]bool{"supplied analysis function": false, "analysis wrapper": true} {
		t.Run(name, func(t *testing.T) {
			e := newExtensionEnv(t, extCfg{Analysis: !wrapper, Wrapper: wrapper})
			r, err := e.analysisThroughOther(wrapper)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Extensions) != 1 {
				t.Fatalf("an optional extension skipped for analysis must still be recorded for the gate: %+v", r.Extensions)
			}
			rec := r.Extensions[0]
			reason := rec.Skipped["analysis"]
			if !strings.Contains(reason, name) || !strings.Contains(reason, "cannot attach extensions") || len(rec.Roles) != 0 || rec.Active() || rec.Network != "none" {
				t.Fatalf("%+v", rec)
			}
			if len(r.AnalysisExtensionsAttached()) != 0 || slices.Contains(r.Notes, AnalysisWithoutSafeModeDisclosure) {
				t.Fatalf("analysis must not claim extensions or the no-safe-mode disclosure: %q", r.Notes)
			}
			if e.builds() != 0 || e.exists("argv-analysis.txt") {
				t.Fatal("nothing may be prepared or started for a role that cannot attach servers")
			}
			if evidence, ok := extensionEvidence(r); !ok || !strings.HasPrefix(evidence.Observed, "0 active extensions") || !strings.Contains(evidence.Observed, "unavailable, not active: fake") {
				t.Fatalf("%+v", evidence)
			}
			markdown, err := os.ReadFile(filepath.Join(e.o.LabDir, "ledgers", r.ID+".md"))
			if err != nil || !strings.Contains(string(markdown), "## Extensions") || !strings.Contains(string(markdown), reason) || strings.Contains(string(markdown), AnalysisWithoutSafeModeDisclosure) {
				t.Fatalf("the ledger must show the skip and make no safe-mode claim: %v", err)
			}
		})
	}
}

func TestExtensionRequiredStopsIntakeWhenAnalysisCannotAttach(t *testing.T) {
	t.Parallel()
	for name, wrapper := range map[string]bool{"supplied analysis function": false, "analysis wrapper": true} {
		t.Run(name, func(t *testing.T) {
			e := newExtensionEnv(t, extCfg{Required: true, Analysis: !wrapper, Wrapper: wrapper})
			r, err := e.analysisThroughOther(wrapper)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != "blocked" || r.Atoms[0].Status != "blocked" || r.Atoms[1].Status != "skipped" || r.Readiness != nil || len(r.AnalysisAttempts) != 0 {
				t.Fatalf("intake must stop before analysis: %+v", r.Atoms)
			}
			for _, want := range []string{`required extension "fake"`, name, "native adapter harness", "reviewed Formula change", "set required to false"} {
				if !strings.Contains(r.Atoms[0].Detail, want) {
					t.Fatalf("message lacks %q: %s", want, r.Atoms[0].Detail)
				}
			}
			if e.exists("argv-analysis.txt") || e.exists("argv-coding.txt") || e.builds() != 0 {
				t.Fatal("a harness started or an extension was prepared")
			}
			if len(r.Extensions) != 1 || !r.Extensions[0].Required || r.Extensions[0].Skipped["analysis"] == "" || len(r.Extensions[0].Roles) != 0 {
				t.Fatalf("the skipped extension must stay recorded: %+v", r.Extensions)
			}
		})
	}
}

func TestExtensionHistoricalLedgerWithoutExtensionsVerifies(t *testing.T) {
	t.Parallel()
	o, _ := claudeAllowlistFixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"extensions"`) {
		t.Fatal("an extension-free record gained an extensions key")
	}
	if _, err = ledger.Decode(data); err != nil {
		t.Fatal(err)
	}
	var restored Record
	if err = json.Unmarshal(data, &restored); err != nil || restored.Extensions != nil {
		t.Fatalf("%v %+v", err, restored.Extensions)
	}
	markdown, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".md"))
	if err != nil || strings.Contains(string(markdown), "## Extensions") {
		t.Fatalf("an extension-free ledger gained a section: %v", err)
	}
}

func TestExtensionGatePolicyEvidenceListsOnlyActiveExtensions(t *testing.T) {
	t.Parallel()
	if _, ok := extensionEvidence(Record{}); ok {
		t.Fatal("evidence must be absent when no extension was recorded")
	}
	r := Record{Extensions: []ExtensionRecord{
		{Name: "graphify", Roles: []string{"analysis"}, Tools: map[string][]ExtensionTool{"analysis": {{Name: "q", Access: "read-only"}}}},
		{Name: "broken", Roles: []string{}, Skipped: map[string]string{"coding": "cache unavailable"}},
	}}
	evidence, ok := extensionEvidence(r)
	if !ok || evidence.Pointer != "/extensions" || evidence.Observed != "1 active extensions: graphify; unavailable, not active: broken" {
		t.Fatalf("%+v", evidence)
	}
}
