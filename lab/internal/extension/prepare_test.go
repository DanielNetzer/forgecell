package extension

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
)

// cacheEnv is a Lab, a source repository and a place for fake prepare tools. Every
// tool is a shell script that leaves evidence in a directory outside the cache.
type cacheEnv struct {
	t           *testing.T
	fx          *fixture
	name        string
	manifestSHA string
	lab         string
	repo        string
	records     string
	tmp         string
	tools       int
}

func newCacheEnv(t *testing.T) *cacheEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	fx := newFixture(t)
	e := &cacheEnv{t: t, fx: fx, name: "graphify", manifestSHA: sum([]byte("manifest")), lab: fx.lab}
	e.repo = filepath.Join(fx.root, "repo")
	e.records = filepath.Join(fx.root, "records")
	e.tmp = filepath.Join(fx.root, "tmp")
	for _, dir := range []string{e.repo, e.records, e.tmp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Completed entries are read-only; give write permission back before the
	// temporary directory is removed.
	t.Cleanup(func() { makeWritable(fx.root) })
	t.Setenv("TMPDIR", e.tmp)
	e.git("init", "-q")
	return e
}

func gitlessEnvironment() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

func (e *cacheEnv) git(args ...string) string {
	e.t.Helper()
	argv := []string{"-C", e.repo, "-c", "user.name=forgecell", "-c", "user.email=forgecell@example.com", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
	cmd := exec.Command("git", append(argv, args...)...)
	cmd.Env = gitlessEnvironment()
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *cacheEnv) commitFile(rel, content string) string {
	e.t.Helper()
	writeFile(e.t, filepath.Join(e.repo, rel), content, 0o644)
	e.git("add", rel)
	e.git("commit", "-q", "-m", "add "+rel)
	return e.git("rev-parse", "HEAD")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// rec is the shell-quoted path of an evidence file.
func (e *cacheEnv) rec(name string) string {
	return shellQuote(filepath.Join(e.records, name))
}

func (e *cacheEnv) read(name string) string {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.records, name))
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return string(raw)
}

func (e *cacheEnv) runs() int {
	e.t.Helper()
	return strings.Count(e.read("runs"), "run\n")
}

// tool pins a fake executable. The body runs under /bin/sh with the manifest's
// substituted arguments as $1, $2 and so on.
func (e *cacheEnv) tool(body string) pinned.File {
	e.t.Helper()
	e.tools++
	path := writeFile(e.t, filepath.Join(e.fx.root, "fake-tools", fmt.Sprint(e.tools), "tool"), "#!/bin/sh\n"+body+"\n", 0o700)
	raw, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatal(err)
	}
	return pinned.File{Path: path, SHA256: sum(raw)}
}

func (e *cacheEnv) manifestWith(timeoutMS int, body string, args ...string) Manifest {
	e.t.Helper()
	m := e.fx.manifest(e.t)
	m.Name = e.name
	m.Prepare = &Prepare{Command: Command{Executable: e.tool(body), Args: args}, TimeoutMS: timeoutMS, Network: "none"}
	return m
}

// scripted is a prepare tool that receives the snapshot as $1 and the cache as $2.
func (e *cacheEnv) scripted(body string) Manifest {
	e.t.Helper()
	return e.manifestWith(60000, "echo run >> "+e.rec("runs")+"\n"+body, "{{SOURCE}}", "{{CACHE}}")
}

func (e *cacheEnv) prepareCtx(ctx context.Context, m Manifest, commit string) CacheResult {
	e.t.Helper()
	return PrepareCache(ctx, e.lab, e.name, e.manifestSHA, m, e.repo, commit)
}

func (e *cacheEnv) prepare(m Manifest, commit string) CacheResult {
	e.t.Helper()
	return e.prepareCtx(context.Background(), m, commit)
}

func (e *cacheEnv) keyParent() string {
	return filepath.Join(e.lab, "extensions", e.name, e.manifestSHA)
}

func (e *cacheEnv) entryDir(commit string) string {
	return filepath.Join(e.keyParent(), commit)
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// requireOnlyEntries checks that the key directory holds exactly the given
// completed entries: no partial directory, no lock, and no temporary directory
// left behind in TMPDIR.
func (e *cacheEnv) requireOnlyEntries(commits ...string) {
	e.t.Helper()
	want := slices.Clone(commits)
	slices.Sort(want)
	// Lease files record callers that have not released their result; they are
	// not entries.
	got := slices.DeleteFunc(listNames(e.t, e.keyParent()), func(n string) bool { return strings.Contains(n, leaseInfix) })
	if !slices.Equal(got, want) {
		e.t.Fatalf("cache directory holds %v, want exactly %v", got, want)
	}
	if left := listNames(e.t, e.tmp); len(left) != 0 {
		e.t.Fatalf("temporary directories were left behind: %v", left)
	}
}

func (e *cacheEnv) requireUnavailable(res CacheResult, reasonContains string) {
	e.t.Helper()
	if res.Outcome != CacheUnavailable {
		e.t.Fatalf("outcome = %q, want unavailable (reason %q)", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, reasonContains) {
		e.t.Fatalf("reason %q should mention %q", res.Reason, reasonContains)
	}
	if res.Dir != "" || res.TreeSHA256 != "" {
		e.t.Fatalf("an unavailable result must carry no directory or digest: %+v", res)
	}
	e.requireOnlyEntries()
}

func TestCacheKeyIsDeterministicAndDistinct(t *testing.T) {
	sha := sum([]byte("manifest"))
	other := sum([]byte("other"))
	commit := strings.Repeat("a", 40)
	key := CacheKey("graphify", sha, commit)
	if key != CacheKey("graphify", sha, commit) {
		t.Fatal("the same parts must give the same key")
	}
	for label, got := range map[string]string{
		"name":     CacheKey("other", sha, commit),
		"manifest": CacheKey("graphify", other, commit),
		"commit":   CacheKey("graphify", sha, strings.Repeat("b", 40)),
	} {
		if got == key {
			t.Fatalf("a different %s must give a different key", label)
		}
	}
}

func TestPrepareCacheBuildsOnceThenReuses(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	m := e.scripted(`printf '%s' '{"nodes":1}' > "$2/graph.json"`)

	first := e.prepare(m, commit)
	if first.Outcome != CacheBuilt {
		t.Fatalf("first outcome = %q (%s), want built", first.Outcome, first.Reason)
	}
	if first.Key != CacheKey(e.name, e.manifestSHA, commit) || first.Dir != e.entryDir(commit) {
		t.Fatalf("key %q and directory %q do not follow <Lab>/extensions/<name>/<manifest-sha>/<base-commit>", first.Key, first.Dir)
	}
	if !hex64.MatchString(first.TreeSHA256) || first.Code != 0 || first.TimedOut {
		t.Fatalf("a built entry needs a tree digest and a clean process result: %+v", first)
	}
	if got, err := os.ReadFile(filepath.Join(first.Dir, "graph.json")); err != nil || string(got) != `{"nodes":1}` {
		t.Fatalf("graph.json = %q, %v", got, err)
	}
	marker, err := os.ReadFile(filepath.Join(first.Dir, ".complete"))
	if err != nil || strings.TrimSpace(string(marker)) != first.TreeSHA256 {
		t.Fatalf("the completion marker %q must record the tree digest %s (%v)", marker, first.TreeSHA256, err)
	}

	second := e.prepare(m, commit)
	if second.Outcome != CacheReused || second.TreeSHA256 != first.TreeSHA256 || second.Dir != first.Dir || second.Key != first.Key {
		t.Fatalf("second call = %+v, want a reuse of %+v", second, first)
	}
	if e.runs() != 1 {
		t.Fatalf("prepare ran %d times, want 1", e.runs())
	}
	e.requireOnlyEntries(commit)
}

func TestPrepareCacheSnapshotIsReadOnlyWithoutGit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newCacheEnv(t)
	e.commitFile("sub/b.txt", "nested")
	commit := e.commitFile("a.txt", "committed")
	writeFile(t, filepath.Join(e.repo, "a.txt"), "uncommitted edit", 0o644)
	writeFile(t, filepath.Join(e.repo, "untracked.txt"), "never added", 0o644)

	body := fmt.Sprintf(`echo "$1" > %[1]s
( cd "$1" && find . | sort ) > %[2]s
cat "$1/a.txt" > %[3]s
if ( echo x > "$1/x" ) 2>/dev/null; then echo wrote > %[4]s; else echo blocked > %[4]s; fi
if [ -e "$1/.git" ]; then echo present > %[5]s; else echo absent > %[5]s; fi
printf '%%s' data > "$2/out"`, e.rec("source"), e.rec("listing"), e.rec("a"), e.rec("write"), e.rec("git"))
	res := e.prepare(e.scripted(body), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
	}
	if got, want := e.read("listing"), ".\n./a.txt\n./sub\n./sub/b.txt\n"; got != want {
		t.Fatalf("snapshot listing = %q, want exactly the tracked files %q", got, want)
	}
	if got := e.read("a"); got != "committed" {
		t.Fatalf("the snapshot holds %q, want the committed bytes, not the working tree", got)
	}
	if got := e.read("write"); got != "blocked\n" {
		t.Fatalf("writing into the snapshot gave %q, want blocked", got)
	}
	if got := e.read("git"); got != "absent\n" {
		t.Fatalf("the snapshot has .git: %q", got)
	}
	source := strings.TrimSpace(e.read("source"))
	if strings.HasPrefix(source, e.repo) || !strings.HasPrefix(source, e.tmp+string(filepath.Separator)) {
		t.Fatalf("the snapshot %q must be a private temporary directory outside the repository", source)
	}
	if _, err := os.Lstat(source); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the snapshot %s should be removed after the build (%v)", source, err)
	}
	e.requireOnlyEntries(commit)
}

func TestPrepareCacheEnvironmentIsMinimal(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	t.Setenv("GH_TOKEN", "secret-token")
	t.Setenv("PYTHONPATH", "/ambient/python")
	ambientHome := filepath.Join(e.fx.root, "ambient-home")
	if err := os.MkdirAll(ambientHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", ambientHome)
	body := fmt.Sprintf(`env > %s
echo "$0" > %s
pwd > %s
printf '%%s' data > "$2/out"`, e.rec("env"), e.rec("argv0"), e.rec("cwd"))
	res := e.prepare(e.scripted(body), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
	}
	env := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(e.read("env")), "\n") {
		key, value, _ := strings.Cut(line, "=")
		env[key] = value
	}
	// A shell adds PWD, SHLVL, OLDPWD and _ on its own; nothing else may appear.
	allowed := []string{"PATH", "HOME", "TMPDIR", "LANG", "PWD", "SHLVL", "OLDPWD", "_"}
	for key := range env {
		if !slices.Contains(allowed, key) {
			t.Fatalf("variable %s leaked into the prepare environment: %v", key, env)
		}
	}
	if env["PATH"] != "/usr/bin:/bin" || env["LANG"] != "C.UTF-8" {
		t.Fatalf("PATH and LANG = %q, %q", env["PATH"], env["LANG"])
	}
	for _, key := range []string{"HOME", "TMPDIR"} {
		if !strings.HasPrefix(env[key], e.tmp+string(filepath.Separator)) {
			t.Fatalf("%s = %q, want a private directory under %s", key, env[key], e.tmp)
		}
		if _, err := os.Lstat(env[key]); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s %s should be removed after the build (%v)", key, env[key], err)
		}
	}
	cwd := strings.TrimSpace(e.read("cwd"))
	if !strings.HasPrefix(cwd, e.tmp+string(filepath.Separator)) {
		t.Fatalf("working directory = %q, want a private temporary directory", cwd)
	}
	e.requireOnlyEntries(commit)
}

func TestPrepareCacheRunsPrivateCopiesThroughTheInterpreter(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	m := e.scripted(fmt.Sprintf(`echo "$0" > %s
printf '%%s' data > "$2/out"`, e.rec("exec0")))
	interpreter := e.tool(fmt.Sprintf(`echo "$0" > %s
echo interp >> %s
exec "$@"`, e.rec("interp0"), e.rec("order")))
	m.Prepare.Interpreter = &interpreter
	exe := m.Prepare.Executable

	res := e.prepare(m, commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
	}
	if got := e.read("order"); got != "interp\n" {
		t.Fatalf("the interpreter did not run first: %q", got)
	}
	for name, pin := range map[string]pinned.File{"exec0": exe, "interp0": interpreter} {
		got := strings.TrimSpace(e.read(name))
		if got == pin.Path || strings.HasPrefix(got, e.fx.root+string(filepath.Separator)+"fake-tools") {
			t.Fatalf("%s ran %q, want a private copy and not the manifest path %s", name, got, pin.Path)
		}
		if !strings.HasPrefix(got, e.tmp+string(filepath.Separator)) {
			t.Fatalf("%s ran %q, want a copy under the private temporary directory", name, got)
		}
		if _, err := os.Lstat(got); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the copy %s should be removed after the build (%v)", got, err)
		}
	}
	e.requireOnlyEntries(commit)
}

func TestPrepareCacheRefusesAnExecutableChangedAfterPinning(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	m := e.scripted(`printf '%s' data > "$2/out"`)
	writeFile(t, m.Prepare.Executable.Path, "#!/bin/sh\necho swapped >> "+e.rec("runs")+"\n", 0o700)

	res := e.prepare(m, commit)
	e.requireUnavailable(res, m.Prepare.Executable.Path)
	if got := e.read("runs"); got != "" {
		t.Fatalf("a swapped executable must never run: %q", got)
	}
}

func TestPrepareCacheTimeoutLeavesNoPartialEntry(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	// The cache path is renamed into place, so a marker proves the process started,
	// and no marker means that the timeout beat a process that was slow to start.
	m := e.manifestWith(3000, fmt.Sprintf("echo run >> %s\necho \"$2\" > %s && mv %s %s\nprintf x > \"$2/partial\"\nsleep 30",
		e.rec("runs"), e.rec("cache.tmp"), e.rec("cache.tmp"), e.rec("cache")), "{{SOURCE}}", "{{CACHE}}")

	started := time.Now()
	res := e.prepare(m, commit)
	e.requireUnavailable(res, "timed out")
	if !res.TimedOut || res.ElapsedMS <= 0 || res.Code == 0 {
		t.Fatalf("timeout result = %+v, want TimedOut with the process elapsed time and a non-zero code", res)
	}
	if time.Since(started) > 20*time.Second {
		t.Fatal("the hung prepare process was not stopped")
	}
	cache := strings.TrimSpace(e.read("cache"))
	if runs := e.runs(); runs > 1 || (cache != "" && runs != 1) {
		t.Fatalf("prepare ran %d times, with the cache path recorded as %q", runs, cache)
	}
	if cache != "" {
		if _, err := os.Lstat(cache); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the partial cache directory was left behind (%v)", err)
		}
	}
}

func TestPrepareCacheInterruptedContextIsUnavailable(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	// The process sleeps far longer than any start-up delay and stays within its own
	// timeout, so only the cancellation below can end it, and it ends interrupted.
	m := e.manifestWith(maxPrepareTimeMS, "echo run >> "+e.rec("runs")+"\nsleep 300", "{{SOURCE}}", "{{CACHE}}")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel once the prepare process has recorded its start. There is no deadline:
	// the watcher stops only when the marker appears or the prepare has returned.
	// The shell creates the file before it writes the line, so wait for the line
	// itself rather than for the file to exist.
	done, started := make(chan struct{}), make(chan bool, 1)
	go func() {
		for {
			if raw, err := os.ReadFile(filepath.Join(e.records, "runs")); err == nil && strings.Contains(string(raw), "run\n") {
				started <- true
				cancel()
				return
			}
			select {
			case <-done:
				started <- false
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	res := e.prepareCtx(ctx, m, commit)
	close(done)
	if !<-started {
		t.Fatalf("the prepare process never recorded its start: %+v", res)
	}
	e.requireUnavailable(res, "interrupted")
	if res.TimedOut {
		t.Fatalf("a cancelled context is not a prepare timeout: %+v", res)
	}
	if e.runs() != 1 {
		t.Fatalf("prepare was interrupted before it started (%d runs)", e.runs())
	}
}

func TestPrepareCacheNonZeroExitIsUnavailable(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	res := e.prepare(e.scripted(`printf x > "$2/partial"; exit 3`), commit)
	e.requireUnavailable(res, "code 3")
	if res.Code != 3 || res.TimedOut {
		t.Fatalf("result = %+v, want exit code 3", res)
	}
}

func TestPrepareCacheOversizeOutputIsUnavailable(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	// One byte past the limit, as a sparse file. Either the in-build watcher stops
	// the process or the post-build check refuses its output, depending on whether
	// a poll lands before the process exits, so the exit code is not asserted. Each
	// path has its own deterministic test.
	body := `dd if=/dev/zero of="$2/index.bin" bs=1 count=1 seek=1073741824 2>/dev/null
chmod 000 "$2/index.bin"`
	res := e.prepare(e.scripted(body), commit)
	e.requireUnavailable(res, "exceeds")
	if res.TimedOut {
		t.Fatalf("an oversize output is not a prepare timeout: %+v", res)
	}
}

func TestPrepareCacheFinishRefusesOversizeOutputBeforeHashing(t *testing.T) {
	unixOnly(t)
	lowerLimits(t, 16, 1000)
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	partial := filepath.Join(e.keyParent(), partialPrefix+"test")
	// One byte past the limit. The file is unreadable, so only a size check that
	// runs before hashing can name the limit.
	file := writeFile(t, filepath.Join(partial, "index.bin"), strings.Repeat("x", 17), 0o600)
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	b := &cacheBuild{key: CacheKey(e.name, e.manifestSHA, commit), dir: e.entryDir(commit), partial: partial}

	res := b.finish()
	removeTree(partial)
	e.requireUnavailable(res, "exceeds")
}

func TestCheckTreeLimitsAreExclusiveAndRefuseLinks(t *testing.T) {
	unixOnly(t)
	lowerLimits(t, 10, 3)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sub", "a"), "0123456789", 0o600)
	writeFile(t, filepath.Join(root, "empty"), "", 0o600)
	if err := checkTree(root, true); err != nil {
		t.Fatalf("exactly the limits must be accepted: %v", err)
	}
	writeFile(t, filepath.Join(root, "empty"), "x", 0o600)
	if err := checkTree(root, true); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("one byte over the limit must be refused: %v", err)
	}
	writeFile(t, filepath.Join(root, "empty"), "", 0o600)
	writeFile(t, filepath.Join(root, "more"), "", 0o600)
	if err := checkTree(root, true); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("one entry over the limit must be refused: %v", err)
	}
	for _, name := range []string{"more", "empty"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := checkTree(root, true); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("a symlink must be refused: %v", err)
	}
	if err := checkTree(root, false); err != nil {
		t.Fatalf("measuring a tree that is still being written must not fail on a link: %v", err)
	}
}

func TestPrepareCacheRefusesUnsafeOutput(t *testing.T) {
	for name, tc := range map[string]struct{ body, reason string }{
		"symlink":       {`ln -s /etc "$2/link"`, "link"},
		"reserved name": {`printf x > "$2/.complete"`, ".complete"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newCacheEnv(t)
			commit := e.commitFile("a.txt", "committed")
			e.requireUnavailable(e.prepare(e.scripted(tc.body), commit), tc.reason)
		})
	}
}

func TestPrepareCacheMissingBaseCommitIsUnavailable(t *testing.T) {
	e := newCacheEnv(t)
	e.commitFile("a.txt", "committed")
	missing := "0123456789abcdef0123456789abcdef01234567"
	res := e.prepare(e.scripted(`printf x > "$2/out"`), missing)
	e.requireUnavailable(res, missing)
	if res.Code != -1 {
		t.Fatalf("no prepare process ran, so Code should be -1, got %d", res.Code)
	}
	if e.runs() != 0 {
		t.Fatal("prepare must not run without a snapshot")
	}
}

func TestPrepareCacheIgnoresAmbientGitEnvironment(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	elsewhere := filepath.Join(e.fx.root, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", elsewhere, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Setenv("GIT_DIR", filepath.Join(elsewhere, ".git"))
	t.Setenv("GIT_WORK_TREE", elsewhere)

	res := e.prepare(e.scripted(`printf x > "$2/out"`), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("an ambient GIT_DIR redirected the snapshot: %q (%s)", res.Outcome, res.Reason)
	}
}

func TestPrepareCacheRejectsUnsafeInputs(t *testing.T) {
	// Each case edits one of: Lab directory, name, manifest digest, repository, base commit.
	cases := map[string]func(e *cacheEnv, a *[5]string){
		"relative Lab":       func(e *cacheEnv, a *[5]string) { a[0] = "lab" },
		"unclean Lab":        func(e *cacheEnv, a *[5]string) { a[0] = e.lab + "/../lab" },
		"traversing name":    func(e *cacheEnv, a *[5]string) { a[1] = "../escape" },
		"uppercase name":     func(e *cacheEnv, a *[5]string) { a[1] = "Graphify" },
		"empty name":         func(e *cacheEnv, a *[5]string) { a[1] = "" },
		"traversing digest":  func(e *cacheEnv, a *[5]string) { a[2] = "../../escape" },
		"short digest":       func(e *cacheEnv, a *[5]string) { a[2] = "abc" },
		"relative repo":      func(e *cacheEnv, a *[5]string) { a[3] = "." },
		"option as commit":   func(e *cacheEnv, a *[5]string) { a[4] = "--output=" + filepath.Join(e.fx.root, "escape") },
		"branch as commit":   func(e *cacheEnv, a *[5]string) { a[4] = "main" },
		"traversing commit":  func(e *cacheEnv, a *[5]string) { a[4] = "../../escape" },
		"short commit":       func(e *cacheEnv, a *[5]string) { a[4] = a[4][:12] },
		"uppercase commit":   func(e *cacheEnv, a *[5]string) { a[4] = strings.ToUpper(a[4]) },
		"revision expansion": func(e *cacheEnv, a *[5]string) { a[4] += "^{tree}" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newCacheEnv(t)
			commit := e.commitFile("a.txt", "committed")
			m := e.scripted(`printf x > "$2/out"`)
			a := [5]string{e.lab, e.name, e.manifestSHA, e.repo, commit}
			mutate(e, &a)
			res := PrepareCache(context.Background(), a[0], a[1], a[2], m, a[3], a[4])
			if res.Outcome != CacheUnavailable || res.Reason == "" {
				t.Fatalf("outcome = %q, want unavailable", res.Outcome)
			}
			if e.runs() != 0 {
				t.Fatal("prepare ran for an unsafe request")
			}
			for _, escape := range []string{filepath.Join(e.fx.root, "escape"), filepath.Join(e.lab, "extensions", "escape"), filepath.Join(e.lab, "escape")} {
				if _, err := os.Lstat(escape); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("%s was created (%v)", escape, err)
				}
			}
			if _, err := os.Lstat(filepath.Join(e.lab, "extensions")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("an unsafe request created cache directories (%v)", err)
			}
		})
	}
}

func TestPrepareCacheRejectsAnInvalidManifest(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	m := e.scripted(`printf x > "$2/out"`)
	m.Prepare.Args = []string{"mid{{CACHE}}dle"}
	res := e.prepare(m, commit)
	if res.Outcome != CacheUnavailable || e.runs() != 0 {
		t.Fatalf("a manifest that fails validation must not be prepared: %+v", res)
	}
}

func TestConcurrentPrepareCacheSameKey(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	m := e.scripted(`sleep 1
printf '%s' '{"nodes":1}' > "$2/graph.json"`)

	const callers = 4
	results := make([]CacheResult, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			results[i] = PrepareCache(ctx, e.lab, e.name, e.manifestSHA, m, e.repo, commit)
		}()
	}
	wg.Wait()

	built, reused := 0, 0
	for _, res := range results {
		switch res.Outcome {
		case CacheBuilt:
			built++
		case CacheReused:
			reused++
		default:
			t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
		}
		if res.TreeSHA256 != results[0].TreeSHA256 || res.Dir != results[0].Dir {
			t.Fatalf("callers disagree: %+v vs %+v", res, results[0])
		}
	}
	if built != 1 || reused != callers-1 {
		t.Fatalf("built %d and reused %d, want 1 and %d", built, reused, callers-1)
	}
	if e.runs() != 1 {
		t.Fatalf("prepare ran %d times, want exactly once", e.runs())
	}
	e.requireOnlyEntries(commit)
}

func (e *cacheEnv) holdLock(commit string, owner string) string {
	e.t.Helper()
	lock := e.entryDir(commit) + ".lock"
	if err := os.MkdirAll(lock, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if owner != "" {
		writeFile(e.t, filepath.Join(lock, "owner"), owner, 0o600)
	}
	return lock
}

func TestPrepareCacheWaitsForAHeldLockUntilTheContextEnds(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	// The owner file is a FIFO, so the wait's read of it blocks until this test
	// opens the other end: that open is the readiness signal that prepare is
	// waiting on the held lock. The context ends while the read is held, and the
	// live owner is written only then, so the wait gives up on its next check
	// instead of after a fixed deadline.
	lock := e.holdLock(commit, "")
	owner := filepath.Join(lock, "owner")
	if err := syscall.Mkfifo(owner, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan CacheResult, 1)
	go func() { done <- e.prepareCtx(ctx, e.scripted(`printf x > "$2/out"`), commit) }()
	var pipe *os.File
	for deadline := time.Now().Add(30 * time.Second); pipe == nil; time.Sleep(10 * time.Millisecond) {
		f, err := os.OpenFile(owner, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			pipe = f
		} else if !errors.Is(err, syscall.ENXIO) || time.Now().After(deadline) {
			t.Fatalf("prepare never read the held lock's owner: %v", err)
		}
	}
	cancel()
	if _, err := pipe.WriteString(fmt.Sprint(os.Getpid())); err != nil {
		t.Fatal(err)
	}
	pipe.Close()
	var res CacheResult
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("prepare kept waiting after its context ended")
	}
	if res.Outcome != CacheUnavailable || !strings.Contains(res.Reason, "waiting") {
		t.Fatalf("result = %+v, want unavailable after waiting for the lock", res)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a held lock must be left alone: %v", err)
	}
	if e.runs() != 0 {
		t.Fatal("prepare ran without the lock")
	}
}

func TestPrepareCacheBuildsAfterTheHeldLockIsReleased(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	lock := e.holdLock(commit, fmt.Sprint(os.Getpid()))
	time.AfterFunc(300*time.Millisecond, func() {
		_ = os.Remove(filepath.Join(lock, "owner"))
		_ = os.Remove(lock)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	res := e.prepareCtx(ctx, e.scripted(`printf x > "$2/out"`), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s), want built once the lock was released", res.Outcome, res.Reason)
	}
	e.requireOnlyEntries(commit)
}

func TestPrepareCacheReportsAStaleLockWithoutStealingIt(t *testing.T) {
	exited := exec.Command("sh", "-c", "exit 0")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	for name, setup := range map[string]func(e *cacheEnv, commit string) string{
		"dead owner": func(e *cacheEnv, commit string) string { return e.holdLock(commit, fmt.Sprint(exited.Process.Pid)) },
		"old ownerless lock": func(e *cacheEnv, commit string) string {
			lock := e.holdLock(commit, "")
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(lock, old, old); err != nil {
				e.t.Fatal(err)
			}
			return lock
		},
		"garbled owner": func(e *cacheEnv, commit string) string { return e.holdLock(commit, "not a pid") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newCacheEnv(t)
			commit := e.commitFile("a.txt", "committed")
			lock := setup(e, commit)
			started := time.Now()
			res := e.prepare(e.scripted(`printf x > "$2/out"`), commit)
			if res.Outcome != CacheUnavailable || !strings.Contains(res.Reason, "lock") || !strings.Contains(res.Reason, lock) {
				t.Fatalf("result = %+v, want a report that names the lock %s", res, lock)
			}
			if time.Since(started) > 10*time.Second {
				t.Fatal("a stale lock must be reported, not waited on")
			}
			if _, err := os.Stat(lock); err != nil {
				t.Fatalf("a stale lock must be reported, never removed: %v", err)
			}
			if e.runs() != 0 {
				t.Fatal("prepare ran under a lock it did not own")
			}
		})
	}
}

func TestCompletedEntryIsReadOnly(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	res := e.prepare(e.scripted(`mkdir -p "$2/sub/deep"
printf a > "$2/graph.json"
printf b > "$2/sub/deep/b.json"
chmod 755 "$2/graph.json"`), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
	}
	for _, want := range []struct {
		rel  string
		mode os.FileMode
	}{
		{"", 0o500},
		{"sub", 0o500},
		{"sub/deep", 0o500},
		{"graph.json", 0o400},
		{"sub/deep/b.json", 0o400},
		{".complete", 0o400},
	} {
		info, err := os.Lstat(filepath.Join(res.Dir, filepath.FromSlash(want.rel)))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want.mode {
			t.Errorf("%q has mode %o, want %o", want.rel, got, want.mode)
		}
	}
	if os.Geteuid() != 0 {
		if err := os.WriteFile(filepath.Join(res.Dir, "new"), []byte("x"), 0o600); err == nil {
			t.Fatal("a completed entry accepted a new file")
		}
		if err := os.WriteFile(filepath.Join(res.Dir, "graph.json"), []byte("x"), 0o600); err == nil {
			t.Fatal("a completed entry accepted a change to a file")
		}
	}
}

func TestTamperedEntryIsRebuilt(t *testing.T) {
	tampers := map[string]func(t *testing.T, dir string){
		"changed byte": func(t *testing.T, dir string) {
			file := filepath.Join(dir, "graph.json")
			if err := os.Chmod(file, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(`{"nodes":2}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"added file": func(t *testing.T, dir string) {
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, "extra.json"), "x", 0o600)
		},
		"removed file": func(t *testing.T, dir string) {
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, "graph.json")); err != nil {
				t.Fatal(err)
			}
		},
		"missing marker": func(t *testing.T, dir string) {
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, ".complete")); err != nil {
				t.Fatal(err)
			}
		},
		"garbled marker": func(t *testing.T, dir string) {
			file := filepath.Join(dir, ".complete")
			if err := os.Chmod(file, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("not a digest"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"nested file changed": func(t *testing.T, dir string) {
			file := filepath.Join(dir, "sub", "n.json")
			if err := os.Chmod(file, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"entry replaced by a link": func(t *testing.T, dir string) {
			elsewhere := filepath.Join(t.TempDir(), "moved")
			t.Cleanup(func() { makeWritable(elsewhere) })
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(dir, elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, dir); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, tamper := range tampers {
		t.Run(name, func(t *testing.T) {
			e := newCacheEnv(t)
			commit := e.commitFile("a.txt", "committed")
			m := e.scripted(`mkdir -p "$2/sub"
printf '%s' '{"nodes":1}' > "$2/graph.json"
printf n > "$2/sub/n.json"`)
			first := e.prepare(m, commit)
			if first.Outcome != CacheBuilt {
				t.Fatalf("outcome = %q (%s)", first.Outcome, first.Reason)
			}
			tamper(t, first.Dir)

			again := e.prepare(m, commit)
			if again.Outcome != CacheBuilt {
				t.Fatalf("a tampered entry reported %q (%s), want built", again.Outcome, again.Reason)
			}
			if again.TreeSHA256 != first.TreeSHA256 {
				t.Fatalf("the rebuilt tree digest %s differs from %s", again.TreeSHA256, first.TreeSHA256)
			}
			if e.runs() != 2 {
				t.Fatalf("prepare ran %d times, want 2", e.runs())
			}
			if got, err := os.ReadFile(filepath.Join(again.Dir, "graph.json")); err != nil || string(got) != `{"nodes":1}` {
				t.Fatalf("graph.json = %q, %v", got, err)
			}
			if info, err := os.Lstat(again.Dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o500 {
				t.Fatalf("the rebuilt entry must be a read-only directory: %v %v", info, err)
			}
			if third := e.prepare(m, commit); third.Outcome != CacheReused || e.runs() != 2 {
				t.Fatalf("a rebuilt entry must be reusable: %+v", third)
			}
			e.requireOnlyEntries(commit)
		})
	}
}

func TestFailedRebuildOfATamperedEntryLeavesNoEntry(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	flaky := e.scripted(`if [ -e ` + e.rec("fail") + ` ]; then exit 9; fi
printf x > "$2/graph.json"`)
	first := e.prepare(flaky, commit)
	if first.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", first.Outcome, first.Reason)
	}
	if err := os.Chmod(filepath.Join(first.Dir, "graph.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(first.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.Dir, "graph.json"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(e.records, "fail"), "", 0o600)

	e.requireUnavailable(e.prepare(flaky, commit), "code 9")
}

func TestPrepareCacheWithoutPrepareCommandUsesEmptyCache(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	m := e.fx.manifest(t)
	m.Prepare = nil

	first := e.prepare(m, commit)
	if first.Outcome != CacheBuilt || first.Code != 0 {
		t.Fatalf("outcome = %q (%s)", first.Outcome, first.Reason)
	}
	if first.TreeSHA256 != sum(nil) {
		t.Fatalf("an empty tree digests to %s, got %s", sum(nil), first.TreeSHA256)
	}
	if got := listNames(t, first.Dir); !slices.Equal(got, []string{".complete"}) {
		t.Fatalf("the cache holds %v, want only the completion marker", got)
	}
	if info, err := os.Lstat(first.Dir); err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("the empty cache must be read-only: %v %v", info, err)
	}
	if second := e.prepare(m, commit); second.Outcome != CacheReused || second.TreeSHA256 != first.TreeSHA256 {
		t.Fatalf("second call = %+v", second)
	}
	e.requireOnlyEntries(commit)
}

// entriesFor builds one entry per commit, oldest first, and stamps each completion
// marker with an increasing time.
func (e *cacheEnv) entriesFor(m Manifest, n int) []string {
	e.t.Helper()
	commits := []string{}
	base := time.Now().Add(-10 * time.Hour)
	for i := range n {
		commit := e.commitFile(fmt.Sprintf("f%d.txt", i), fmt.Sprint(i))
		res := e.prepare(m, commit)
		if res.Outcome != CacheBuilt {
			e.t.Fatalf("entry %d: %q (%s)", i, res.Outcome, res.Reason)
		}
		res.Release()
		stamp := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(filepath.Join(e.entryDir(commit), ".complete"), stamp, stamp); err != nil {
			e.t.Fatal(err)
		}
		commits = append(commits, commit)
	}
	return commits
}

func (e *cacheEnv) key(commit string) string {
	return CacheKey(e.name, e.manifestSHA, commit)
}

func TestPruneKeepsReferencedEntries(t *testing.T) {
	e := newCacheEnv(t)
	commits := e.entriesFor(e.scripted(`printf x > "$2/graph.json"`), 3)
	oldest, middle, newest := commits[0], commits[1], commits[2]

	removed, err := Prune(e.lab, e.name, 1, map[string]bool{e.key(oldest): true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{e.key(middle)}) {
		t.Fatalf("removed %v, want only the unreferenced older entry %s", removed, e.key(middle))
	}
	e.requireOnlyEntries(oldest, newest)

	removed, err = Prune(e.lab, e.name, 0, map[string]bool{e.key(oldest): true, e.key(newest): true})
	if err != nil || len(removed) != 0 {
		t.Fatalf("a referenced entry must never be removed, got %v, %v", removed, err)
	}
	e.requireOnlyEntries(oldest, newest)

	removed, err = Prune(e.lab, e.name, 0, nil)
	want := []string{e.key(oldest), e.key(newest)}
	slices.Sort(want)
	if err != nil || !slices.Equal(removed, want) {
		t.Fatalf("removed %v, %v, want %v", removed, err, want)
	}
	e.requireOnlyEntries()
}

func TestPruneKeepsTheNewestEntries(t *testing.T) {
	e := newCacheEnv(t)
	commits := e.entriesFor(e.scripted(`printf x > "$2/graph.json"`), 4)

	removed, err := Prune(e.lab, e.name, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{e.key(commits[0]), e.key(commits[1])}
	slices.Sort(want)
	if !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want the two oldest %v", removed, want)
	}
	e.requireOnlyEntries(commits[2], commits[3])
}

func TestPruneSkipsLockedPartialAndIncompleteEntries(t *testing.T) {
	e := newCacheEnv(t)
	commits := e.entriesFor(e.scripted(`printf x > "$2/graph.json"`), 2)
	locked, free := commits[0], commits[1]
	lock := e.holdLock(locked, fmt.Sprint(os.Getpid()))
	partial := filepath.Join(e.keyParent(), ".partial-0123abcd")
	incomplete := e.entryDir(strings.Repeat("c", 40))
	stray := filepath.Join(e.keyParent(), "stray")
	for _, dir := range []string{partial, incomplete, stray} {
		writeFile(t, filepath.Join(dir, "file"), "x", 0o600)
	}

	removed, err := Prune(e.lab, e.name, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{e.key(free)}) {
		t.Fatalf("removed %v, want only the unlocked complete entry", removed)
	}
	for _, kept := range []string{e.entryDir(locked), lock, partial, incomplete, stray} {
		if _, err := os.Lstat(kept); err != nil {
			t.Fatalf("%s must be left alone: %v", kept, err)
		}
	}

	if err := os.RemoveAll(lock); err != nil {
		t.Fatal(err)
	}
	removed, err = Prune(e.lab, e.name, 0, nil)
	if err != nil || !slices.Equal(removed, []string{e.key(locked)}) {
		t.Fatalf("after the lock was released removed %v, %v", removed, err)
	}
	if listing := listNames(t, e.keyParent()); !slices.Equal(listing, []string{filepath.Base(partial), filepath.Base(incomplete), "stray"}) {
		t.Fatalf("leftover entries = %v", listing)
	}
}

func TestPruneLeavesOtherExtensionsAlone(t *testing.T) {
	e := newCacheEnv(t)
	commits := e.entriesFor(e.scripted(`printf x > "$2/graph.json"`), 1)
	other := filepath.Join(e.lab, "extensions", "other", e.manifestSHA, commits[0])
	writeFile(t, filepath.Join(other, ".complete"), "x", 0o400)

	removed, err := Prune(e.lab, "other", 0, nil)
	if err != nil || !slices.Equal(removed, []string{CacheKey("other", e.manifestSHA, commits[0])}) {
		t.Fatalf("removed %v, %v", removed, err)
	}
	e.requireOnlyEntries(commits[0])
}

func TestPruneValidatesItsArguments(t *testing.T) {
	e := newCacheEnv(t)
	for name, call := range map[string]func() ([]string, error){
		"relative Lab":  func() ([]string, error) { return Prune("lab", e.name, 1, nil) },
		"bad name":      func() ([]string, error) { return Prune(e.lab, "../x", 1, nil) },
		"negative keep": func() ([]string, error) { return Prune(e.lab, e.name, -1, nil) },
	} {
		if removed, err := call(); err == nil || len(removed) != 0 {
			t.Fatalf("%s: removed %v, %v, want an error", name, removed, err)
		}
	}
	if removed, err := Prune(e.lab, e.name, 1, nil); err != nil || len(removed) != 0 {
		t.Fatalf("an extension with no cache has nothing to prune: %v, %v", removed, err)
	}
}

func lowerLimits(t *testing.T, bytes int64, entries int) {
	t.Helper()
	oldBytes, oldEntries := cacheByteLimit, cacheEntryLimit
	cacheByteLimit, cacheEntryLimit = bytes, entries
	t.Cleanup(func() { cacheByteLimit, cacheEntryLimit = oldBytes, oldEntries })
}

const snapshotDump = `( cd "$1" && find . -type f | sort | while read -r f; do echo "== $f"; cat "$f"; done )`

const hostileAttributes = "* export-ignore\n* export-subst\n* filter=x\n* ident\n* text eol=crlf\n"

func TestPrepareCacheSnapshotIgnoresLocalConfigurationAndAttributes(t *testing.T) {
	const want = "== ./a.txt\nid: $Format:%H$ $Id$\nline\n== ./sub/b.txt\nlower\n"
	setups := map[string]func(e *cacheEnv, marker string){
		"clean repository": nil,
		"repository attributes, filter and line endings": func(e *cacheEnv, marker string) {
			writeFile(e.t, filepath.Join(e.repo, ".git", "info", "attributes"), hostileAttributes, 0o644)
			e.git("config", "filter.x.smudge", "touch "+shellQuote(marker)+"; tr a-z A-Z")
			e.git("config", "core.autocrlf", "true")
		},
		"global configuration": func(e *cacheEnv, marker string) {
			home := filepath.Join(e.fx.root, "ambient-home")
			attributes := writeFile(e.t, filepath.Join(home, "attributes"), hostileAttributes, 0o644)
			config := fmt.Sprintf("[core]\n\tautocrlf = true\n\tattributesFile = %s\n[filter \"x\"]\n\tsmudge = \"touch %s; tr a-z A-Z\"\n", attributes, shellQuote(marker))
			writeFile(e.t, filepath.Join(home, ".gitconfig"), config, 0o644)
			e.t.Setenv("HOME", home)
		},
	}
	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			e := newCacheEnv(t)
			e.commitFile("sub/b.txt", "lower\n")
			commit := e.commitFile("a.txt", "id: $Format:%H$ $Id$\nline\n")
			marker := filepath.Join(e.records, "smudge-ran")
			if setup != nil {
				setup(e, marker)
			}
			res := e.prepare(e.scripted(snapshotDump+" > "+e.rec("dump")+"\nprintf x > \"$2/out\""), commit)
			if res.Outcome != CacheBuilt {
				t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
			}
			if got := e.read("dump"); got != want {
				t.Fatalf("snapshot = %q, want the plain committed tree %q", got, want)
			}
			if _, err := os.Lstat(marker); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a smudge filter from configuration ran against the snapshot (%v)", err)
			}
		})
	}
}

func TestPrepareCacheRunsGitWithAMinimalEnvironment(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(e.fx.root, "wrapper")
	script := "#!/bin/sh\nenv >> " + e.rec("gitenv") + "\necho --- >> " + e.rec("gitenv") + "\nexec " + shellQuote(realGit) + " \"$@\"\n"
	writeFile(t, filepath.Join(wrapper, "git"), script, 0o700)
	ambientHome := filepath.Join(e.fx.root, "ambient-home")
	if err := os.MkdirAll(ambientHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", ambientHome)
	for _, key := range []string{"GH_TOKEN", "SSH_AUTH_SOCK", "PYTHONPATH", "GIT_SSH_COMMAND", "GIT_AUTHOR_NAME"} {
		t.Setenv(key, "ambient-"+key)
	}

	res := e.prepare(e.scripted(`printf x > "$2/out"`), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
	}
	log := e.read("gitenv")
	if got := strings.Count(log, "---\n"); got < 3 {
		t.Fatalf("git ran %d times, want at least a lookup, a commit check and an archive", got)
	}
	allowed := []string{"PATH", "HOME", "TMPDIR", "LC_ALL", "PWD", "OLDPWD", "SHLVL", "_", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_ATTR_NOSYSTEM", "GIT_TERMINAL_PROMPT"}
	seen := map[string]string{}
	for _, line := range strings.Split(log, "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if !slices.Contains(allowed, key) {
			t.Fatalf("variable %s leaked into git's environment: %q", key, line)
		}
		seen[key] = value
	}
	if !strings.HasPrefix(seen["HOME"], e.tmp+string(filepath.Separator)) {
		t.Fatalf("git's HOME = %q, want a private directory under %s", seen["HOME"], e.tmp)
	}
	if seen["GIT_CONFIG_NOSYSTEM"] != "1" || seen["GIT_CONFIG_GLOBAL"] != "/dev/null" {
		t.Fatalf("git must ignore system and global configuration: %v", seen)
	}
}

const growthSteps = 400

func TestPrepareCacheStopsAnOutputThatKeepsGrowing(t *testing.T) {
	cases := []struct {
		name    string
		bytes   int64
		entries int
		step    string
		reason  string
	}{
		{name: "bytes", bytes: 4 << 20, entries: 1000, step: `dd if=/dev/zero bs=1048576 count=1 >> "$2/grow" 2>/dev/null`, reason: "exceeds"},
		{name: "entries", bytes: 1 << 30, entries: 20, step: `: > "$2/f$i"`, reason: "entries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lowerLimits(t, tc.bytes, tc.entries)
			e := newCacheEnv(t)
			commit := e.commitFile("a.txt", "committed")
			// A bounded loop, so that a missing guard shows up as full progress. The
			// step marker is renamed into place, so a kill never leaves it half written.
			m := e.scripted(fmt.Sprintf("i=0\nwhile [ $i -lt %d ]; do\n%s\necho $i > %s && mv %s %s\ni=$((i+1))\nsleep 0.01\ndone",
				growthSteps, tc.step, e.rec("progress.tmp"), e.rec("progress.tmp"), e.rec("progress")))

			res := e.prepare(m, commit)
			e.requireUnavailable(res, tc.reason)
			if res.TimedOut || res.ElapsedMS > 30000 {
				t.Fatalf("the output was not stopped early: %+v", res)
			}
			// No marker means the process was stopped before it recorded a step.
			progress := 0
			if raw := strings.TrimSpace(e.read("progress")); raw != "" {
				var err error
				if progress, err = strconv.Atoi(raw); err != nil {
					t.Fatalf("the step marker %q is not a number: %v", raw, err)
				}
			}
			if progress >= growthSteps*3/4 {
				t.Fatalf("the prepare process kept running to step %d of %d, want it stopped as soon as the limit was passed", progress, growthSteps)
			}
		})
	}
}

func TestPrepareCacheOversizeSnapshotIsUnavailable(t *testing.T) {
	lowerLimits(t, 32<<10, 1000)
	e := newCacheEnv(t)
	commit := e.commitFile("big.txt", strings.Repeat("x", 64<<10))
	res := e.prepare(e.scripted(`printf x > "$2/out"`), commit)
	e.requireUnavailable(res, "snapshot")
	if !strings.Contains(res.Reason, "exceeds") || e.runs() != 0 {
		t.Fatalf("an oversize snapshot must stop before prepare runs: %+v, %d runs", res, e.runs())
	}
}

func TestPrepareCacheWorksWithARelativeTMPDIR(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	t.Chdir(e.fx.root)
	if err := os.Mkdir("relative-tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", "relative-tmp")
	body := fmt.Sprintf("echo \"$1\" > %s\necho \"$HOME\" > %s\necho \"$TMPDIR\" > %s\npwd > %s\necho \"$0\" > %s\nprintf x > \"$2/out\"", e.rec("source"), e.rec("home"), e.rec("tmpdir"), e.rec("cwd"), e.rec("argv0"))

	res := e.prepare(e.scripted(body), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
	}
	root := filepath.Join(e.fx.root, "relative-tmp")
	for _, name := range []string{"source", "home", "tmpdir", "cwd", "argv0"} {
		if got := strings.TrimSpace(e.read(name)); !strings.HasPrefix(got, root+string(filepath.Separator)) {
			t.Fatalf("%s = %q, want an absolute path below %s", name, got, root)
		}
	}
	if left := listNames(t, root); len(left) != 0 {
		t.Fatalf("temporary directories were left behind: %v", left)
	}
}

func TestPrepareCacheSourceRepositoryKinds(t *testing.T) {
	kinds := map[string]func(e *cacheEnv) string{
		"bare clone": func(e *cacheEnv) string {
			dir := filepath.Join(e.fx.root, "bare.git")
			e.git("clone", "-q", "--bare", e.repo, dir)
			return dir
		},
		"linked worktree": func(e *cacheEnv) string {
			dir := filepath.Join(e.fx.root, "linked")
			e.git("worktree", "add", "-q", "--detach", dir)
			return dir
		},
	}
	for name, source := range kinds {
		t.Run(name, func(t *testing.T) {
			e := newCacheEnv(t)
			commit := e.commitFile("a.txt", "committed")
			repo := source(e)
			m := e.scripted("cat \"$1/a.txt\" > " + e.rec("a") + "\nprintf x > \"$2/out\"")

			res := PrepareCache(context.Background(), e.lab, e.name, e.manifestSHA, m, repo, commit)
			if res.Outcome != CacheBuilt {
				t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
			}
			if got := e.read("a"); got != "committed" {
				t.Fatalf("the snapshot holds %q, want the committed bytes", got)
			}
			e.requireOnlyEntries(commit)
		})
	}
}

func TestPrepareCacheSupportsSHA256Repositories(t *testing.T) {
	e := newCacheEnv(t)
	e.repo = filepath.Join(e.fx.root, "repo-sha256")
	if err := os.MkdirAll(e.repo, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", e.repo, "init", "-q", "--object-format=sha256")
	cmd.Env = gitlessEnvironment()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this git cannot create a SHA-256 repository: %v\n%s", err, out)
	}
	commit := e.commitFile("a.txt", "committed")
	if len(commit) != 64 {
		t.Skipf("this git made a %d-character commit id, want 64", len(commit))
	}

	res := e.prepare(e.scripted("cat \"$1/a.txt\" > "+e.rec("a")+"\nprintf x > \"$2/out\""), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
	}
	if got := e.read("a"); got != "committed" {
		t.Fatalf("the snapshot holds %q, want the committed bytes", got)
	}
}

func regularEntry(name, body string) tarEntry {
	return tarEntry{header: tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644}, body: body}
}

func TestExtractSnapshotEnforcesTheCacheLimits(t *testing.T) {
	unixOnly(t)
	lowerLimits(t, 10, 3)
	cases := []struct {
		name    string
		entries []tarEntry
		reason  string
	}{
		{name: "exactly the limits", entries: []tarEntry{regularEntry("a", "01234"), regularEntry("b", "56789"), regularEntry("c", "")}},
		{name: "one byte too many", entries: []tarEntry{regularEntry("a", "01234"), regularEntry("b", "567890")}, reason: "exceeds"},
		{name: "one entry too many", entries: []tarEntry{regularEntry("a", ""), regularEntry("b", ""), regularEntry("c", ""), regularEntry("d", "")}, reason: "entries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := writeTar(t, tc.entries)
			dst := filepath.Join(t.TempDir(), "source")
			if err := os.Mkdir(dst, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { makeWritable(dst) })
			err := extractSnapshot(archive, dst)
			switch {
			case tc.reason == "" && err != nil:
				t.Fatalf("a snapshot at the limits was refused: %v", err)
			case tc.reason != "" && (err == nil || !strings.Contains(err.Error(), tc.reason)):
				t.Fatalf("error = %v, want one that mentions %q", err, tc.reason)
			}
		})
	}
}

type tarEntry struct {
	header tar.Header
	body   string
}

func writeTar(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		header := entry.header
		header.Size = int64(len(entry.body))
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractSnapshotKeepsFilesAndSkipsLinks(t *testing.T) {
	unixOnly(t)
	archive := writeTar(t, []tarEntry{
		{header: tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "abc"}, Format: tar.FormatPAX}},
		{header: tar.Header{Typeflag: tar.TypeDir, Name: "d/", Mode: 0o755}},
		{header: tar.Header{Typeflag: tar.TypeReg, Name: "d/run.sh", Mode: 0o755}, body: "#!/bin/sh\n"},
		{header: tar.Header{Typeflag: tar.TypeReg, Name: "plain.txt", Mode: 0o644}, body: "text"},
		{header: tar.Header{Typeflag: tar.TypeReg, Name: "undeclared/dir/file.txt", Mode: 0o644}, body: "deep"},
		{header: tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc"}},
		{header: tar.Header{Typeflag: tar.TypeLink, Name: "hard", Linkname: "plain.txt"}},
	})
	dst := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeWritable(dst) })
	if err := extractSnapshot(archive, dst); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]os.FileMode{"d": 0o500, "d/run.sh": 0o500, "plain.txt": 0o400, "undeclared/dir/file.txt": 0o400, "undeclared/dir": 0o500, ".": 0o500} {
		info, err := os.Lstat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %o, want %o", rel, info.Mode().Perm(), want)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dst, "plain.txt")); err != nil || string(got) != "text" {
		t.Fatalf("plain.txt = %q, %v", got, err)
	}
	for _, skipped := range []string{"link", "hard", "pax_global_header"} {
		if _, err := os.Lstat(filepath.Join(dst, skipped)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s must not be materialised (%v)", skipped, err)
		}
	}
}

func TestExtractSnapshotRefusesUnsafeNames(t *testing.T) {
	unixOnly(t)
	for _, name := range []string{"../evil", "a/../../evil", "/abs/evil", ".git/config", "sub/.git/hooks/pre-commit", "sub/.GIT/config", ".."} {
		t.Run(name, func(t *testing.T) {
			archive := writeTar(t, []tarEntry{{header: tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644}, body: "x"}})
			outer := t.TempDir()
			dst := filepath.Join(outer, "source")
			if err := os.Mkdir(dst, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := extractSnapshot(archive, dst); err == nil {
				t.Fatalf("%q was extracted", name)
			}
			if names := listNames(t, outer); !slices.Equal(names, []string{"source"}) {
				t.Fatalf("extraction wrote outside the snapshot directory: %v", names)
			}
		})
	}
}

func leaseFiles(dir string) []string {
	names, _ := os.ReadDir(filepath.Dir(dir))
	var found []string
	for _, n := range names {
		if strings.HasPrefix(n.Name(), filepath.Base(dir)+leaseInfix) {
			found = append(found, filepath.Join(filepath.Dir(dir), n.Name()))
		}
	}
	return found
}

func leasedFor(dir string) int { return len(leaseFiles(dir)) }

func exitedPID(t *testing.T) int {
	t.Helper()
	exited := exec.Command("sh", "-c", "exit 0")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	return exited.Process.Pid
}

func TestPruneLeavesALeasedEntryUntilItIsReleased(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	m := e.scripted(`printf x > "$2/graph.json"`)

	built := e.prepare(m, commit)
	if built.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", built.Outcome, built.Reason)
	}
	removed, err := Prune(e.lab, e.name, 0, nil)
	if err != nil || len(removed) != 0 {
		t.Fatalf("a built entry is leased to its caller, got removed %v, %v", removed, err)
	}
	if _, err := verifyEntry(built.Dir); err != nil {
		t.Fatalf("the returned directory must still verify after Prune ran: %v", err)
	}

	reused := e.prepare(m, commit)
	if reused.Outcome != CacheReused {
		t.Fatalf("outcome = %q (%s)", reused.Outcome, reused.Reason)
	}
	built.Release()
	if removed, err = Prune(e.lab, e.name, 0, nil); err != nil || len(removed) != 0 {
		t.Fatalf("a reused entry is leased to its caller too, got removed %v, %v", removed, err)
	}
	reused.Release()
	reused.Release()
	built.Release()
	if n := leasedFor(e.entryDir(commit)); n != 0 {
		t.Fatalf("%d leases remain after every result was released", n)
	}
	removed, err = Prune(e.lab, e.name, 0, nil)
	if err != nil || !slices.Equal(removed, []string{e.key(commit)}) {
		t.Fatalf("after release the entry is removable, got %v, %v", removed, err)
	}
}

func TestUnavailableResultHoldsNoLease(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	res := e.prepare(e.scripted(`exit 3`), commit)
	e.requireUnavailable(res, "code 3")
	res.Release()
	if n := leasedFor(e.entryDir(commit)); n != 0 {
		t.Fatalf("an unavailable result holds %d leases", n)
	}
}

func TestPruneRacingPrepareNeverRemovesALeasedEntry(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	m := e.scripted(`printf x > "$2/graph.json"`)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = Prune(e.lab, e.name, 0, nil)
				time.Sleep(time.Millisecond)
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()
	for i := range 5 {
		res := e.prepare(m, commit)
		if res.Outcome == CacheUnavailable {
			t.Fatalf("round %d: %s", i, res.Reason)
		}
		if _, err := verifyEntry(res.Dir); err != nil {
			t.Fatalf("round %d: a leased entry was removed or changed under its caller: %v", i, err)
		}
		res.Release()
	}
}

func TestPrepareCacheRecordsItsBuilderOutsideTheEntry(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	res := e.prepare(e.scripted(`cp "$2/.builder" `+e.rec("builder")+`; printf x > "$2/graph.json"`), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
	}
	pid, _, err := parseOwner(e.read("builder"))
	if err != nil || pid != os.Getpid() {
		t.Fatalf("the build directory recorded builder %q (%v), want this process %d", e.read("builder"), err, os.Getpid())
	}
	if got := listNames(t, res.Dir); !slices.Equal(got, []string{".complete", "graph.json"}) {
		t.Fatalf("the builder record must not be part of the entry: %v", got)
	}
}

func TestPrepareCacheGivesHardLinkedOutputItsOwnCopy(t *testing.T) {
	unixOnly(t)
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	outside := writeFile(t, filepath.Join(e.fx.root, "outside.txt"), "precious", 0o640)
	res := e.prepare(e.scripted(`ln `+shellQuote(outside)+` "$2/linked"; mkdir "$2/sub"; ln "$2/linked" "$2/sub/again"`), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s), want a hard link to be de-linked, not refused", res.Outcome, res.Reason)
	}
	defer res.Release()
	before, err := os.Lstat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := pinned.LinkCount(before); n != 1 || before.Mode().Perm() != 0o640 {
		t.Fatalf("the file outside the cache has %d links and mode %v, want it untouched", n, before.Mode().Perm())
	}
	for _, rel := range []string{"linked", "sub/again"} {
		entry := filepath.Join(res.Dir, filepath.FromSlash(rel))
		info, err := os.Lstat(entry)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := pinned.LinkCount(info); n != 1 || os.SameFile(info, before) {
			t.Fatalf("%s still shares its inode (%d links)", rel, n)
		}
		if info.Mode().Perm() != 0o400 {
			t.Fatalf("%s has mode %v, want read-only", rel, info.Mode().Perm())
		}
		if got, err := os.ReadFile(entry); err != nil || string(got) != "precious" {
			t.Fatalf("%s holds %q, %v", rel, got, err)
		}
	}
	if _, err := verifyEntry(res.Dir); err != nil {
		t.Fatalf("a de-linked entry must verify: %v", err)
	}
	again := e.prepare(e.scripted(`ln `+shellQuote(outside)+` "$2/linked"; mkdir "$2/sub"; ln "$2/linked" "$2/sub/again"`), commit)
	if again.Outcome != CacheReused {
		t.Fatalf("outcome = %q (%s), want the de-linked entry reused", again.Outcome, again.Reason)
	}
	again.Release()
}

func TestPrepareCacheRefusesAnEntryWhoseFileGainedAHardLink(t *testing.T) {
	unixOnly(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	m := e.scripted(`printf x > "$2/graph.json"`)
	first := e.prepare(m, commit)
	if first.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", first.Outcome, first.Reason)
	}
	first.Release()
	if err := os.Link(filepath.Join(first.Dir, "graph.json"), filepath.Join(e.fx.root, "alias")); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}
	if _, err := verifyEntry(first.Dir); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("a completed entry whose file gained another name must not verify: %v", err)
	}
}

func TestHashTreeOfAnEmptyTreeIsTheDigestOfNothing(t *testing.T) {
	for _, locked := range []bool{false, true} {
		got, err := hashTree(t.TempDir(), locked)
		if err != nil || got != sum(nil) {
			t.Fatalf("locked=%t: %q, %v", locked, got, err)
		}
	}
}

func TestPrepareCacheRebuildsAfterAModeOnlyChange(t *testing.T) {
	unixOnly(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	for name, change := range map[string]func(dir string) error{
		"file":      func(dir string) error { return os.Chmod(filepath.Join(dir, "graph.json"), 0o600) },
		"directory": func(dir string) error { return os.Chmod(filepath.Join(dir, "sub"), 0o700) },
		"root":      func(dir string) error { return os.Chmod(dir, 0o700) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newCacheEnv(t)
			commit := e.commitFile("a.txt", "committed")
			m := e.scripted(`mkdir "$2/sub"; printf x > "$2/graph.json"; printf y > "$2/sub/inner.json"`)
			first := e.prepare(m, commit)
			if first.Outcome != CacheBuilt {
				t.Fatalf("outcome = %q (%s)", first.Outcome, first.Reason)
			}
			first.Release()
			if err := change(first.Dir); err != nil {
				t.Fatal(err)
			}
			again := e.prepare(m, commit)
			if again.Outcome != CacheBuilt || e.runs() != 2 {
				t.Fatalf("a mode-only change must make the entry rebuild: %+v after %d runs", again, e.runs())
			}
			if again.TreeSHA256 != first.TreeSHA256 {
				t.Fatalf("the same output must give the same digest: %s vs %s", again.TreeSHA256, first.TreeSHA256)
			}
		})
	}
}

func TestLockDownNeverFollowsALinkPlantedAfterTheCheck(t *testing.T) {
	unixOnly(t)
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	outside := writeFile(t, filepath.Join(e.fx.root, "outside.txt"), "precious", 0o644)
	testHookBeforeChmod = func(p string) {
		if strings.Contains(p, partialPrefix) && filepath.Base(p) == "target.txt" {
			_ = os.Chmod(filepath.Dir(p), 0o700)
			_ = os.Remove(p)
			_ = os.Symlink(outside, p)
		}
	}
	t.Cleanup(func() { testHookBeforeChmod = nil })

	res := e.prepare(e.scripted(`printf x > "$2/target.txt"`), commit)
	e.requireUnavailable(res, "read-only")
	info, err := os.Stat(outside)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("the file behind the planted link changed: %v, %v", info, err)
	}
}

func TestPrepareCacheWorksThroughASymlinkedLab(t *testing.T) {
	unixOnly(t)
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	alias := filepath.Join(e.fx.root, "lab-alias")
	if err := os.Symlink(e.lab, alias); err != nil {
		t.Fatal(err)
	}
	res := PrepareCache(context.Background(), alias, e.name, e.manifestSHA, e.scripted(`printf x > "$2/graph.json"`), e.repo, commit)
	if res.Outcome != CacheBuilt || res.Dir != e.entryDir(commit) {
		t.Fatalf("a Lab reached through a link must work and report the resolved directory: %+v", res)
	}
	res.Release()
	removed, err := Prune(alias, e.name, 0, nil)
	if err != nil || !slices.Equal(removed, []string{e.key(commit)}) {
		t.Fatalf("Prune through the link: %v, %v", removed, err)
	}
}

func TestPrepareCacheRefusesSymlinkedCacheComponents(t *testing.T) {
	unixOnly(t)
	for name, link := range map[string]func(e *cacheEnv) string{
		"extension name": func(e *cacheEnv) string { return filepath.Join(e.lab, "extensions", e.name) },
		"manifest digest": func(e *cacheEnv) string {
			if err := os.MkdirAll(filepath.Join(e.lab, "extensions", e.name), 0o700); err != nil {
				e.t.Fatal(err)
			}
			return e.keyParent()
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newCacheEnv(t)
			commit := e.commitFile("a.txt", "committed")
			outside := filepath.Join(e.fx.root, "outside")
			if err := os.MkdirAll(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			at := link(e)
			if err := os.MkdirAll(filepath.Dir(at), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, at); err != nil {
				t.Fatal(err)
			}
			res := e.prepare(e.scripted(`printf x > "$2/graph.json"`), commit)
			if res.Outcome != CacheUnavailable || !strings.Contains(res.Reason, "symbolic link") {
				t.Fatalf("result = %+v, want a refusal that names the link", res)
			}
			if got := listNames(t, outside); len(got) != 0 {
				t.Fatalf("something was written through the link: %v", got)
			}
			if e.runs() != 0 {
				t.Fatal("prepare ran")
			}
			if _, err := Prune(e.lab, e.name, 0, nil); name == "extension name" && err == nil {
				t.Fatal("Prune must refuse a symlinked extension directory")
			}
		})
	}
}

func TestCaseOnlyCollisionsInASnapshotAreRefused(t *testing.T) {
	unixOnly(t)
	for name, entries := range map[string][]tarEntry{
		"files":       {regularEntry("README", "a"), regularEntry("readme", "b")},
		"directories": {regularEntry("Docs/a.txt", "a"), regularEntry("docs/b.txt", "b")},
	} {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "source")
			if err := os.Mkdir(dst, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { makeWritable(dst) })
			err := extractSnapshot(writeTar(t, entries), dst)
			if err == nil || !strings.Contains(err.Error(), "differ only by case") {
				t.Fatalf("expected a case collision, got %v", err)
			}
		})
	}
	dst := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeWritable(dst) })
	archive := writeTar(t, []tarEntry{
		{header: tar.Header{Typeflag: tar.TypeDir, Name: "d/", Mode: 0o755}},
		regularEntry("d/a", "a"), regularEntry("d/b", "b"), regularEntry("D2/a", "c"),
	})
	if err := extractSnapshot(archive, dst); err != nil {
		t.Fatalf("a directory entry followed by its files is not a collision: %v", err)
	}
}

func TestSnapshotNamesTheLinksItLeavesOut(t *testing.T) {
	unixOnly(t)
	var entries []tarEntry
	for i := range 7 {
		entries = append(entries, tarEntry{header: tar.Header{Typeflag: tar.TypeSymlink, Name: fmt.Sprintf("link%d", i), Linkname: "/etc"}})
	}
	entries = append(entries, tarEntry{header: tar.Header{Typeflag: tar.TypeLink, Name: "hard", Linkname: "plain"}}, regularEntry("plain", "x"))
	dst := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeWritable(dst) })
	dropped, err := extractSnapshotNoted(writeTar(t, entries), dst)
	if err != nil || len(dropped) != 8 || dropped[0] != "link0" || dropped[7] != "hard" {
		t.Fatalf("dropped = %v, %v", dropped, err)
	}
	note := describeDropped(dropped)
	if !strings.Contains(note, "8 tracked") || !strings.Contains(note, "link4") || strings.Contains(note, "link5") || !strings.Contains(note, "and 3 more") {
		t.Fatalf("the note must name a bounded number of links: %q", note)
	}
	if describeDropped(nil) != "" {
		t.Fatal("no dropped links, no note")
	}
}

func TestPrepareCacheReportsTrackedSymlinksItDropped(t *testing.T) {
	unixOnly(t)
	e := newCacheEnv(t)
	e.commitFile("a.txt", "committed")
	if err := os.Symlink("a.txt", filepath.Join(e.repo, "alias")); err != nil {
		t.Fatal(err)
	}
	e.git("add", "alias")
	e.git("commit", "-q", "-m", "add a link")
	commit := e.git("rev-parse", "HEAD")

	res := e.prepare(e.scripted(`printf x > "$2/graph.json"`), commit)
	if res.Outcome != CacheBuilt || !strings.Contains(res.Note, "alias") {
		t.Fatalf("a built entry must say what the snapshot left out: %+v", res)
	}
}

func fakeGit(t *testing.T, e *cacheEnv, body string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(e.fx.root, "fake-git")
	writeFile(t, filepath.Join(wrapper, "git"), "#!/bin/sh\n"+body+"\nexec "+shellQuote(realGit)+" \"$@\"\n", 0o700)
	t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPrepareCacheDoesNotCallAGitErrorAMissingCommit(t *testing.T) {
	unixOnly(t)
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	fakeGit(t, e, `case " $* " in *" cat-file "*) echo "fatal: detected dubious ownership in repository at '/x'" >&2; exit 128;; esac`)
	res := e.prepare(e.scripted(`printf x > "$2/graph.json"`), commit)
	e.requireUnavailable(res, "dubious ownership")
	if strings.Contains(res.Reason, "is not available in the source repository") || !strings.Contains(res.Reason, "git error") {
		t.Fatalf("a git failure was mislabelled: %q", res.Reason)
	}
}

func TestPrepareCacheStillCallsASilentGitFailureAMissingCommit(t *testing.T) {
	unixOnly(t)
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	// git cat-file -e exits 1 without a message for a missing object.
	fakeGit(t, e, `case " $* " in *" cat-file "*) exit 1;; esac`)
	res := e.prepare(e.scripted(`printf x > "$2/graph.json"`), commit)
	e.requireUnavailable(res, "is not available in the source repository")
}

func TestPrepareCacheReportsACallerDeadlineAsSuch(t *testing.T) {
	unixOnly(t)
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := e.prepareCtx(ctx, e.scripted("sleep 30"), commit)
	e.requireUnavailable(res, "caller's deadline")
	if !res.ParentDeadline || res.TimedOut {
		t.Fatalf("a caller deadline is not a prepare timeout: %+v", res)
	}
}

func TestPrepareCacheDoesNotCallACallerDeadlineAMissingCommit(t *testing.T) {
	unixOnly(t)
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	fakeGit(t, e, `case " $* " in *" cat-file "*) sleep 30;; esac`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := e.prepareCtx(ctx, e.scripted(`printf x > "$2/graph.json"`), commit)
	e.requireUnavailable(res, "caller's deadline")
	if strings.Contains(res.Reason, "not available in the source repository") || !res.ParentDeadline {
		t.Fatalf("result = %+v", res)
	}
}

func TestLockOwnerRecordsTheProcessStart(t *testing.T) {
	unixOnly(t)
	lock := filepath.Join(t.TempDir(), "entry.lock")
	release, err := tryLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	raw, err := os.ReadFile(filepath.Join(lock, lockOwnerName))
	if err != nil {
		t.Fatal(err)
	}
	pid, start, err := parseOwner(string(raw))
	if err != nil || pid != os.Getpid() {
		t.Fatalf("owner record %q: %d, %v", raw, pid, err)
	}
	if _, startErr := processStart(os.Getpid()); startErr == nil && start == "" {
		t.Fatal("the process start time was available but not recorded")
	}
	if reason := staleLock(lock); reason != "" {
		t.Fatalf("a lock held by this process is not stale: %s", reason)
	}
}

func TestStaleLockReportsAReusedProcessID(t *testing.T) {
	unixOnly(t)
	now, err := processStart(os.Getpid())
	if err != nil {
		t.Skipf("no process start time on this host: %v", err)
	}
	scheme, _, _ := strings.Cut(now, ":")
	lock := filepath.Join(t.TempDir(), "entry.lock")
	writeFile(t, filepath.Join(lock, lockOwnerName), fmt.Sprintf("%d\n%s:started-long-ago", os.Getpid(), scheme), 0o600)
	reason := staleLock(lock)
	if !strings.Contains(reason, "stale") || !strings.Contains(reason, "different process") || !strings.Contains(reason, lock) {
		t.Fatalf("a live process id with another start time must be reported: %q", reason)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a stale lock is reported, never removed: %v", err)
	}
	writeFile(t, filepath.Join(lock, lockOwnerName), fmt.Sprint(os.Getpid()), 0o600)
	if reason := staleLock(lock); reason != "" {
		t.Fatalf("a legacy owner record with a live pid keeps its behaviour: %q", reason)
	}
}

func TestAcquireLockGivesUpAfterTheDefaultWaitWithoutADeadline(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	e.holdLock(commit, fmt.Sprint(os.Getpid()))
	old := lockMaxWait
	lockMaxWait = 500 * time.Millisecond
	t.Cleanup(func() { lockMaxWait = old })

	started := time.Now()
	res := e.prepareCtx(context.Background(), e.scripted(`printf x > "$2/out"`), commit)
	if res.Outcome != CacheUnavailable || !strings.Contains(res.Reason, "gave up waiting") {
		t.Fatalf("result = %+v, want a give-up after the maximum wait", res)
	}
	if time.Since(started) > 10*time.Second {
		t.Fatal("the wait was not bounded")
	}
	if lockMaxWait < 0 || old < time.Duration(maxPrepareTimeMS)*time.Millisecond+snapshotTimeout {
		t.Fatalf("the default wait %s is shorter than the longest prepare plus the snapshot", old)
	}
}

func TestStaleLocksListsOnlyLeftoverLocks(t *testing.T) {
	e := newCacheEnv(t)
	commits := []string{strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)}
	dead := e.holdLock(commits[0], fmt.Sprint(exitedPID(t)))
	e.holdLock(commits[1], fmt.Sprint(os.Getpid()))
	ownerless := e.holdLock(commits[2], "")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(ownerless, old, old); err != nil {
		t.Fatal(err)
	}
	found := StaleLocks(e.lab)
	var paths []string
	for _, s := range found {
		paths = append(paths, s.Path)
		if s.Reason == "" {
			t.Fatalf("a stale lock needs a reason: %+v", s)
		}
	}
	if !slices.Equal(paths, []string{dead, ownerless}) {
		t.Fatalf("stale locks = %v, want %v", paths, []string{dead, ownerless})
	}
	for _, lock := range []string{dead, ownerless} {
		if _, err := os.Stat(lock); err != nil {
			t.Fatalf("listing must not remove %s: %v", lock, err)
		}
	}
	if got := StaleLocks(filepath.Join(e.fx.root, "missing")); len(got) != 0 {
		t.Fatalf("a Lab without a cache has no stale locks: %v", got)
	}
}

func TestPruneReportsALockWhoseOwnerIsGone(t *testing.T) {
	e := newCacheEnv(t)
	commits := e.entriesFor(e.scripted(`printf x > "$2/graph.json"`), 1)
	lock := e.holdLock(commits[0], fmt.Sprint(exitedPID(t)))
	removed, err := Prune(e.lab, e.name, 0, nil)
	if len(removed) != 0 || err == nil || !strings.Contains(err.Error(), "stale") || !strings.Contains(err.Error(), lock) {
		t.Fatalf("a dead owner's lock must be reported, not silently skipped: %v, %v", removed, err)
	}
}

func TestPruneBetweenVerifyAndReturnCannotRemoveTheEntry(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	m := e.scripted(`printf x > "$2/graph.json"`)
	first := e.prepare(m, commit)
	if first.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", first.Outcome, first.Reason)
	}
	first.Release()

	for name, run := range map[string]func() CacheResult{
		"reuse": func() CacheResult { return e.prepare(m, commit) },
		"build": func() CacheResult {
			if err := removeEntry(e.entryDir(commit)); err != nil {
				t.Fatal(err)
			}
			return e.prepare(m, commit)
		},
	} {
		var removed []string
		var pruneErr error
		hooked := false
		testHookAfterVerify = func(dir string) {
			hooked = true
			removed, pruneErr = Prune(e.lab, e.name, 0, nil)
		}
		res := run()
		testHookAfterVerify = nil
		if !hooked || res.Outcome == CacheUnavailable {
			t.Fatalf("%s: hooked=%t, result %+v", name, hooked, res)
		}
		if len(removed) != 0 || pruneErr != nil {
			t.Fatalf("%s: Prune between verify and return removed %v (%v)", name, removed, pruneErr)
		}
		if _, err := verifyEntry(res.Dir); err != nil {
			t.Fatalf("%s: the returned entry no longer verifies: %v", name, err)
		}
		res.Release()
	}
}

func TestAPanicWhileLeasingReleasesTheLeaseAndTheLock(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	testHookAfterVerify = func(string) { panic("boom") }
	t.Cleanup(func() { testHookAfterVerify = nil })
	func() {
		defer func() { _ = recover() }()
		e.prepare(e.scripted(`printf x > "$2/graph.json"`), commit)
	}()
	if n := leasedFor(e.entryDir(commit)); n != 0 {
		t.Fatalf("a panic leaked %d lease files", n)
	}
	if _, err := os.Lstat(e.entryDir(commit) + lockSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a panic leaked the entry lock: %v", err)
	}
}

func TestHelperProcessHoldsALease(t *testing.T) {
	dir := os.Getenv("FORGECELL_TEST_LEASE_DIR")
	if dir == "" {
		t.Skip("only runs as a helper of another test")
	}
	if _, err := takeLease(dir); err != nil {
		fmt.Println("LEASE-FAILED", err)
		os.Exit(2)
	}
	fmt.Println("LEASE-READY")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(0)
}

func TestPruneHonoursALeaseHeldByAnotherProcess(t *testing.T) {
	unixOnly(t)
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	res := e.prepare(e.scripted(`printf x > "$2/graph.json"`), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
	}
	res.Release()
	dir := e.entryDir(commit)

	helper := exec.Command(os.Args[0], "-test.run=^TestHelperProcessHoldsALease$")
	helper.Env = append(os.Environ(), "FORGECELL_TEST_LEASE_DIR="+dir)
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	}()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "LEASE-READY" {
		t.Fatalf("the helper did not take its lease: %q, %v", line, err)
	}

	if removed, err := Prune(e.lab, e.name, 0, nil); err != nil || len(removed) != 0 {
		t.Fatalf("a lease held by another running process was ignored: removed %v, %v", removed, err)
	}
	if n := leasedFor(dir); n != 1 {
		t.Fatalf("%d lease files, want the live one", n)
	}

	// A killed process cannot release; its lease is reaped and no longer protects.
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	_ = helper.Wait()
	removed, err := Prune(e.lab, e.name, 0, nil)
	if err != nil || !slices.Equal(removed, []string{e.key(commit)}) {
		t.Fatalf("the lease of a dead process still protected the entry: removed %v, %v", removed, err)
	}
	if n := leasedFor(dir); n != 0 {
		t.Fatalf("%d lease files remain after the owner died", n)
	}
}

func TestLeaseFileNamesTheProcessAndSitsBesideTheEntry(t *testing.T) {
	e := newCacheEnv(t)
	commit := e.commitFile("a.txt", "committed")
	res := e.prepare(e.scripted(`printf x > "$2/graph.json"`), commit)
	if res.Outcome != CacheBuilt {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason)
	}
	files := leaseFiles(res.Dir)
	if len(files) != 1 || !strings.Contains(filepath.Base(files[0]), "."+strconv.Itoa(os.Getpid())+".") {
		t.Fatalf("lease files = %v", files)
	}
	pid, _, err := parseOwner(readFileString(t, files[0]))
	if err != nil || pid != os.Getpid() {
		t.Fatalf("lease record: %d, %v", pid, err)
	}
	if got := listNames(t, res.Dir); !slices.Equal(got, []string{".complete", "graph.json"}) {
		t.Fatalf("the lease must not be part of the entry: %v", got)
	}
	res.Release()
	if n := leasedFor(res.Dir); n != 0 {
		t.Fatalf("%d lease files after Release", n)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestStartTimesFromDifferentSchemesAreNotCompared(t *testing.T) {
	if !sameStartScheme("proc:1", "proc:2") || !sameStartScheme("ps:a", "ps:b") {
		t.Fatal("tokens of one scheme compare")
	}
	if sameStartScheme("proc:1", "ps:Mon") {
		t.Fatal("tokens of different schemes must not compare")
	}
	unixOnly(t)
	now, err := processStart(os.Getpid())
	if err != nil {
		t.Skipf("no process start time on this host: %v", err)
	}
	other := "ps:"
	if strings.HasPrefix(now, "ps:") {
		other = "proc:"
	}
	if gone := ownerGone(os.Getpid(), other+"unrelated"); gone != "" {
		t.Fatalf("an owner whose start time cannot be compared is assumed alive: %q", gone)
	}
}

func TestOwnerWatchAsksLivenessEveryPollButStartTimeRarely(t *testing.T) {
	unixOnly(t)
	var w ownerWatch
	if got := w.gone(os.Getpid(), ""); got != "" {
		t.Fatalf("this process is alive: %q", got)
	}
	w.verdict = "cached verdict"
	if got := w.gone(os.Getpid(), ""); got != "cached verdict" {
		t.Fatalf("a recent verdict is reused: %q", got)
	}
	dead := exitedPID(t)
	w = ownerWatch{pid: dead, verdict: "", at: time.Now()}
	if got := w.gone(dead, ""); got != "has exited" {
		t.Fatalf("a process that exited is noticed on every poll: %q", got)
	}
	var none *ownerWatch
	if got := none.gone(dead, ""); got != "has exited" {
		t.Fatalf("a nil watch checks directly: %q", got)
	}
}

func TestPruneReapsPartialsWhoseBuilderIsGone(t *testing.T) {
	e := newCacheEnv(t)
	dead := filepath.Join(e.keyParent(), partialPrefix+"dead")
	live := filepath.Join(e.keyParent(), partialPrefix+"live")
	ownerless := filepath.Join(e.keyParent(), partialPrefix+"0123abcd")
	writeFile(t, filepath.Join(dead, "graph.json"), "x", 0o600)
	writeFile(t, filepath.Join(dead, builderName), fmt.Sprint(exitedPID(t)), 0o600)
	writeFile(t, filepath.Join(live, "graph.json"), "x", 0o600)
	writeFile(t, filepath.Join(live, builderName), string(ownerRecord()), 0o600)
	writeFile(t, filepath.Join(ownerless, "graph.json"), "x", 0o600)
	if err := os.Chmod(filepath.Join(dead, "graph.json"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dead, 0o500); err != nil {
		t.Fatal(err)
	}

	for _, keep := range []int{5, 0} {
		if _, err := Prune(e.lab, e.name, keep, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(dead); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("keep=%d: a build whose process is gone must be removed (%v)", keep, err)
		}
		for _, kept := range []string{live, ownerless} {
			if _, err := os.Lstat(kept); err != nil {
				t.Fatalf("keep=%d: %s must be left alone: %v", keep, kept, err)
			}
		}
	}
}
