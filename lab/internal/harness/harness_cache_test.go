package harness

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// codexEchoFake is a fake Codex provider that records its argv and the Go cache entries
// of its environment in the working directory, then writes a completed outcome.
const codexEchoFake = `printf '%s\n' "$@" > argv.txt
env | grep -E '^(GOCACHE|GOTMPDIR|GOMODCACHE)=' | sort > env.txt
out=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then shift; out="$1"; fi
  shift
done
cat >/dev/null
printf '{"schemaVersion":"v1","outcome":"completed","reason":"implemented","paths":[]}' > "$out"
`

func runCodexEcho(t *testing.T, request map[string]any, env []string) (argv []string, goEnv []string) {
	t.Helper()
	dir := t.TempDir()
	r := Execute(context.Background(), ExecuteOptions{ID: "codex", Executable: fake(t, codexEchoFake), Dir: dir, Env: env, Request: request, Timeout: executeBackstop})
	if !r.Process.OK || r.Coding == nil || r.Coding.Outcome != "completed" {
		t.Fatalf("%+v", r)
	}
	rawArgv, err := os.ReadFile(filepath.Join(dir, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	rawEnv, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(rawArgv), "\n"), "\n"), strings.Fields(string(rawEnv))
}

// withoutOutputPaths drops the per-run output file paths, which differ between runs.
func withoutOutputPaths(argv []string) []string {
	out := []string{}
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--output-last-message" || argv[i] == "--output-schema" {
			i++
			continue
		}
		out = append(out, argv[i])
	}
	return out
}

func TestHarnessCacheReachesCodexCodingInvocation(t *testing.T) {
	lab := t.TempDir()
	root := filepath.Join(lab, "caches", "harness")
	base := []string{"PATH=" + os.Getenv("PATH"), "GOCACHE=/user/go-build", "GOTMPDIR=/user/gotmp", "GOMODCACHE=/user/mod"}
	request := map[string]any{"kind": "molecule", "codingAllowlist": []string{"Read"}}
	plainArgv, plainEnv := runCodexEcho(t, request, base)
	if !reflect.DeepEqual(plainEnv, []string{"GOCACHE=/user/go-build", "GOMODCACHE=/user/mod", "GOTMPDIR=/user/gotmp"}) {
		t.Fatalf("a coding run without the cache changed the environment: %q", plainEnv)
	}
	if slices.Contains(plainArgv, "--add-dir") || slices.ContainsFunc(plainArgv, func(a string) bool { return strings.HasPrefix(a, "shell_environment_policy.") }) {
		t.Fatalf("a coding run without the cache got --add-dir or a policy override: %q", plainArgv)
	}
	cachedArgv, cachedEnv := runCodexEcho(t, WithHarnessCache(request, root), base)
	want := []string{"GOCACHE=" + filepath.Join(root, "go-build"), "GOMODCACHE=/user/mod", "GOTMPDIR=" + filepath.Join(root, "gotmp")}
	if !reflect.DeepEqual(cachedEnv, want) {
		t.Fatalf("cache environment %q, want %q", cachedEnv, want)
	}
	i := slices.Index(cachedArgv, "--add-dir")
	if i < 0 || i+1 >= len(cachedArgv) || cachedArgv[i+1] != root || slices.Index(cachedArgv[i+1:], "--add-dir") >= 0 {
		t.Fatalf("argv must name exactly one --add-dir %s: %q", root, cachedArgv)
	}
	if cachedArgv[len(cachedArgv)-1] != "-" || cachedArgv[slices.Index(cachedArgv, "--sandbox")+1] != "workspace-write" {
		t.Fatalf("sandbox or stdin argument changed: %q", cachedArgv)
	}
	overrides := []string{
		"-c", `shell_environment_policy.set.GOCACHE="` + filepath.Join(root, "go-build") + `"`,
		"-c", `shell_environment_policy.set.GOTMPDIR="` + filepath.Join(root, "gotmp") + `"`,
	}
	if i+2+len(overrides) > len(cachedArgv) || !reflect.DeepEqual(cachedArgv[i+2:i+2+len(overrides)], overrides) {
		t.Fatalf("argv lacks the shell_environment_policy overrides %q: %q", overrides, cachedArgv)
	}
	stripped := slices.Delete(slices.Clone(cachedArgv), i, i+2+len(overrides))
	if !reflect.DeepEqual(withoutOutputPaths(stripped), withoutOutputPaths(plainArgv)) {
		t.Fatalf("argv differs beyond --add-dir and the overrides:\n%q\n%q", stripped, plainArgv)
	}
}

func TestHarnessCacheOverridesQuoteTheRoot(t *testing.T) {
	for _, c := range []struct{ root, gocache, gotmpdir string }{
		{`/lab/a"b`, `"/lab/a\"b/go-build"`, `"/lab/a\"b/gotmp"`},
		{`/lab/a\b`, `"/lab/a\\b/go-build"`, `"/lab/a\\b/gotmp"`},
		{`/lab/a\"b`, `"/lab/a\\\"b/go-build"`, `"/lab/a\\\"b/gotmp"`},
	} {
		call, err := BuildInvocation("codex", WithHarnessCache(map[string]any{"kind": "molecule"}, c.root), "/fixture/result")
		if err != nil {
			t.Fatalf("%q: %v", c.root, err)
		}
		for _, want := range []string{"shell_environment_policy.set.GOCACHE=" + c.gocache, "shell_environment_policy.set.GOTMPDIR=" + c.gotmpdir} {
			if i := slices.Index(call.Args, want); i < 1 || call.Args[i-1] != "-c" {
				t.Fatalf("%q: argv lacks -c %s: %q", c.root, want, call.Args)
			}
		}
	}
}

func TestHarnessCacheOverridesRefuseANewline(t *testing.T) {
	for _, s := range []string{"/lab/a\nb", "/lab/a\rb"} {
		if _, err := tomlBasicString(s); err == nil {
			t.Fatalf("quoted %q", s)
		}
		if _, err := codexHarnessCacheOverrides(s); err == nil {
			t.Fatalf("built overrides for %q", s)
		}
		if _, err := BuildInvocation("codex", WithHarnessCache(map[string]any{"kind": "molecule"}, s), "/fixture/result"); err == nil {
			t.Fatalf("built an invocation for %q", s)
		}
	}
	if got, err := tomlBasicString(`/lab/plain`); err != nil || got != `"/lab/plain"` {
		t.Fatalf("plain path quoted as %q, %v", got, err)
	}
}

func TestHarnessCacheDefaultsToTheProcessEnvironment(t *testing.T) {
	t.Setenv("GOCACHE", "/user/go-build")
	root := filepath.Join(t.TempDir(), "caches", "harness")
	_, env := runCodexEcho(t, WithHarnessCache(map[string]any{"kind": "molecule"}, root), nil)
	if !slices.Contains(env, "GOCACHE="+filepath.Join(root, "go-build")) || !slices.Contains(env, "GOTMPDIR="+filepath.Join(root, "gotmp")) {
		t.Fatalf("inherited environment kept the user's cache: %q", env)
	}
}

func TestHarnessCacheRefusedOutsideCodexCoding(t *testing.T) {
	root := "/fixture/lab/caches/harness"
	for _, c := range []struct {
		id, kind string
	}{
		{"codex", "ticket-analysis"}, {"codex", ReviewKind}, {"codex", "formula-improvement"},
		{"claude-code", "molecule"}, {"claude-code", "ticket-analysis"}, {"claude-code", ReviewKind}, {"claude-code", "formula-improvement"},
		{"cursor", "molecule"}, {"cursor", "formula-improvement"},
	} {
		request := WithHarnessCache(map[string]any{"kind": c.kind, "codingAllowlist": []string{"Read"}}, root)
		if _, err := BuildInvocation(c.id, request, "/fixture/result"); err == nil || !strings.Contains(err.Error(), "harness Go cache") {
			t.Fatalf("%s/%s accepted a harness cache: %v", c.id, c.kind, err)
		}
	}
	for _, bad := range []any{"relative/harness", "/lab/../harness", "", 7, "/lab/harness\n"} {
		request := map[string]any{"kind": "molecule", harnessCacheRequestKey: bad}
		if _, err := BuildInvocation("codex", request, "/fixture/result"); err == nil {
			t.Fatalf("invalid cache path %q accepted", bad)
		}
	}
}

func TestHarnessCacheStaysOutOfThePrompt(t *testing.T) {
	root := "/fixture/lab/caches/harness"
	call, err := BuildInvocation("codex", WithHarnessCache(map[string]any{"kind": "molecule"}, root), "/fixture/result")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(call.Input, root) || strings.Contains(call.Input, harnessCacheRequestKey) {
		t.Fatalf("harness cache entered the model-visible prompt: %s", call.Input)
	}
	plain, err := BuildInvocation("codex", map[string]any{"kind": "molecule"}, "/fixture/result")
	if err != nil || plain.Input != call.Input {
		t.Fatalf("prompt changed with the cache: %v", err)
	}
	original := map[string]any{"kind": "molecule"}
	if WithHarnessCache(original, root); len(original) != 1 {
		t.Fatal("WithHarnessCache modified the caller's request")
	}
}
