package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

var versionCore = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)(?:-[A-Za-z0-9.-]+)?$`)
var shortRev = regexp.MustCompile(`^[0-9a-f]{4,64}$`)
var protocolPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// Ignored files the compiler still reads: an ignore rule hides them from git, not from go build.
var buildSource = regexp.MustCompile(`(^|/)(go\.mod|go\.sum|[^/]+\.(go|s|syso))$`)

var buildEnvKeep = strings.Fields("PATH HOME GOCACHE GOMODCACHE TMPDIR HTTP_PROXY HTTPS_PROXY NO_PROXY ALL_PROXY http_proxy https_proxy no_proxy all_proxy GOPROXY GONOPROXY")

// BuildFunc compiles the checkout's Forgecell command into output, stamping version.
type BuildFunc func(ctx context.Context, checkout, version, output string) error

type DevOptions struct {
	Home, Bin, Checkout string
	RunningVersion      string
	Activate            bool
	AllowDirty          bool

	// Build defaults to the local Go toolchain.
	Build BuildFunc
}

type DevRelease struct {
	Version        string `json:"version"`
	Path           string `json:"path"`
	ManifestSHA256 string `json:"manifestSha256"`
	Activated      bool   `json:"-"`
}

// devVersion is the running build's major.minor.patch (0.0.0 when it is not a release
// version) followed by -dev.<rev>. A dirty tree adds .dirty.<first 8 hex of its content
// digest>, so different uncommitted contents never share a release directory.
func devVersion(running, rev, dirtyDigest string) string {
	base := "0.0.0"
	if m := versionCore.FindStringSubmatch(running); m != nil {
		base = m[1]
	}
	version := base + "-dev." + rev
	if dirtyDigest != "" {
		version += ".dirty." + dirtyDigest[:min(8, len(dirtyDigest))]
	}
	return version
}

// gitEnv drops variables that would point git at a repository other than the checkout.
func gitEnv() []string {
	drop := []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_OBJECT_DIRECTORY=", "GIT_ALTERNATE_OBJECT_DIRECTORIES=", "GIT_COMMON_DIR=", "GIT_PREFIX=", "GIT_OPTIONAL_LOCKS="}
	var env []string
next:
	for _, kv := range os.Environ() {
		for _, d := range drop {
			if strings.HasPrefix(kv, d) {
				continue next
			}
		}
		env = append(env, kv)
	}
	return append(env, "GIT_OPTIONAL_LOCKS=0")
}

// runGit reads the checkout without running its configured fsmonitor or hooks and without
// refreshing the index; it returns stdout untrimmed.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	argv := append([]string{"git", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "--no-optional-locks"}, args...)
	r := process.Run(ctx, process.Options{Argv: argv, Dir: dir, Env: gitEnv(), Stdin: []byte{}, Timeout: 30 * time.Second, MaxOutputBytes: 10_000_000})
	if !r.OK {
		return "", fmt.Errorf("%s", firstLine(r.Stderr, r.Error))
	}
	return r.Stdout, nil
}

func firstLine(texts ...string) string {
	for _, text := range texts {
		if text = strings.TrimSpace(text); text != "" {
			if len(text) > 500 {
				text = text[:500]
			}
			return text
		}
	}
	return "unknown failure"
}

// checkout is what a dev build is a build of: the commit plus every path that makes the
// tree differ from it. digest is empty exactly when paths is.
type checkout struct {
	rev, digest string
	paths       []string
}

func walkFiles(root, rel string, keep func(string) bool) ([]string, error) {
	var found []string
	err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(rel, "/"))), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		r, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if r = filepath.ToSlash(r); keep(r) {
			found = append(found, r)
		}
		return nil
	})
	return found, err
}

func digestPaths(root string, paths []string) (string, error) {
	h := sha256.New()
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		st, err := os.Lstat(full)
		switch {
		case os.IsNotExist(err):
			fmt.Fprintf(h, "%s\x00deleted\x00", p)
		case err != nil:
			return "", err
		case st.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "%s\x00link\x00%s\x00", p, target)
		case st.Mode().IsRegular():
			f, err := os.Open(full)
			if err != nil {
				return "", err
			}
			content := sha256.New()
			_, err = io.Copy(content, f)
			f.Close()
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "%s\x00file\x00%o\x00%x\x00", p, st.Mode()&0o111, content.Sum(nil))
		default:
			fmt.Fprintf(h, "%s\x00other\x00", p)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func inspectCheckout(ctx context.Context, root string) (c checkout, err error) {
	top, err := runGit(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return c, fmt.Errorf("checkout is not a git repository: %s", root)
	}
	if real, e := filepath.EvalSymlinks(strings.TrimSpace(top)); e != nil || real != root {
		return c, fmt.Errorf("checkout is not the root of a git repository: %s", root)
	}
	rev, err := runGit(ctx, root, "rev-parse", "--short", "HEAD")
	if c.rev = strings.TrimSpace(rev); err != nil || !shortRev.MatchString(c.rev) {
		return c, fmt.Errorf("checkout has no commit to build: %s", root)
	}
	status, err := runGit(ctx, root, "status", "--porcelain=v1", "-z", "--ignored=matching", "--untracked-files=all", "--no-renames")
	if err != nil {
		return c, fmt.Errorf("cannot read checkout status: %w", err)
	}
	seen := map[string]bool{}
	for _, entry := range strings.Split(status, "\x00") {
		if len(entry) < 4 {
			continue
		}
		path := entry[3:]
		keep := func(string) bool { return true }
		if entry[:2] == "!!" {
			if !strings.HasPrefix(path, "lab/") {
				continue
			}
			keep = buildSource.MatchString
		}
		files := []string{path}
		if strings.HasSuffix(path, "/") {
			if files, err = walkFiles(root, path, keep); err != nil {
				return c, fmt.Errorf("cannot read checkout status: %w", err)
			}
		} else if !keep(path) {
			continue
		}
		for _, f := range files {
			if !seen[f] {
				seen[f] = true
				c.paths = append(c.paths, f)
			}
		}
	}
	if len(c.paths) == 0 {
		return c, nil
	}
	sort.Strings(c.paths)
	if c.digest, err = digestPaths(root, c.paths); err != nil {
		return c, fmt.Errorf("cannot read checkout state: %w", err)
	}
	return c, nil
}

// parseCommitTime reads git log --format=%ct output: a positive Unix time no later
// than year 3000, or 0 (absent) for anything else.
func parseCommitTime(out string) int64 {
	text := strings.TrimSpace(out)
	if text == "" || len(text) > 12 || strings.Trim(text, "0123456789") != "" {
		return 0
	}
	at, err := strconv.ParseInt(text, 10, 64)
	if err != nil || at <= 0 || at > 32503680000 {
		return 0
	}
	return at
}

// headCommitTime is the committer time of the checkout's HEAD, which the build
// cannot edit (never a file mtime), or 0 when git cannot report it. Tests
// replace it.
var headCommitTime = func(ctx context.Context, root string) int64 {
	out, err := runGit(ctx, root, "log", "-1", "--format=%ct", "HEAD")
	if err != nil {
		return 0
	}
	return parseCommitTime(out)
}

// recordDevBuild writes formula.DevBuildFile beside a dev build's manifest, so a
// Formula rebind can order two dev builds of one version by commit time. A
// missing time is recorded as absent: no file is written, and an existing record
// is left alone. The record replaces any earlier one atomically.
func recordDevBuild(dir, version string, commitTime int64) error {
	if commitTime <= 0 {
		return nil
	}
	raw, err := json.Marshal(formula.DevBuild{Version: version, CommitTime: commitTime})
	if err != nil {
		return err
	}
	path := filepath.Join(dir, formula.DevBuildFile)
	if st, e := os.Lstat(path); e == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("refusing unmanaged dev build record: %s", path)
	}
	tmp, err := os.CreateTemp(dir, ".dev-build-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Chmod(0600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

// buildEnv is the only environment go build sees: nothing ambient (GOFLAGS such as
// -toolexec, GOEXPERIMENT, credentials) reaches the compiler of an install.
func buildEnv(environ []string) []string {
	var env []string
	for _, kv := range environ {
		if key, _, _ := strings.Cut(kv, "="); slices.Contains(buildEnvKeep, key) {
			env = append(env, kv)
		}
	}
	return append(env, "GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local", "GOTELEMETRY=off", "CGO_ENABLED=0", "GOEXPERIMENT=", "GOENV=off")
}

func goBuild(ctx context.Context, root, version, output string) error {
	r := process.Run(ctx, process.Options{
		Argv:           []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-X main.version=" + version, "-o", output, "./cmd/forgecell"},
		Dir:            filepath.Join(root, "lab"),
		Env:            buildEnv(os.Environ()),
		Stdin:          []byte{},
		Timeout:        10 * time.Minute,
		MaxOutputBytes: 1_000_000,
	})
	if !r.OK {
		return fmt.Errorf("go build failed: %s", firstLine(r.Stderr, r.Error))
	}
	return nil
}

// builtProtocol asks the built executable for its own adapter protocol; the running
// binary's constant says nothing about a different checkout.
func builtProtocol(ctx context.Context, exe string) (string, error) {
	r := process.Run(ctx, process.Options{Argv: []string{exe, "--protocol"}, Env: []string{}, Stdin: []byte{}, Timeout: ProbeTimeout, MaxOutputBytes: 1000})
	protocol := strings.TrimSpace(r.Stdout)
	if !r.OK || !protocolPattern.MatchString(protocol) {
		return "", fmt.Errorf("built executable does not report its adapter protocol (forgecell --protocol); the checkout may predate dev install support")
	}
	return protocol, nil
}

// DevInstall builds a checkout into releases/<major.minor.patch>-dev.<rev> and stages it, or with
// Activate also switches current to it through the ordinary Install path.
func DevInstall(ctx context.Context, o DevOptions) (r DevRelease, err error) {
	if o.Checkout == "" {
		o.Checkout = "."
	}
	root, err := filepath.Abs(o.Checkout)
	if err != nil {
		return r, err
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return r, err
	}
	if st, e := os.Stat(filepath.Join(root, "lab", "cmd", "forgecell")); e != nil || !st.IsDir() {
		return r, fmt.Errorf("checkout has no lab/cmd/forgecell: %s", root)
	}
	before, err := inspectCheckout(ctx, root)
	if err != nil {
		return r, err
	}
	if len(before.paths) > 0 && !o.AllowDirty {
		shown := before.paths[:min(3, len(before.paths))]
		return r, fmt.Errorf("checkout has uncommitted, untracked or ignored Go source changes (%s); commit them or pass --allow-dirty (the version then carries .dirty and a content hash)", strings.Join(shown, ", "))
	}
	version := devVersion(o.RunningVersion, before.rev, before.digest)
	if !versionPattern.MatchString(version) {
		return r, fmt.Errorf("invalid dev version: %s", version)
	}
	work, err := resources.MkdirTemp("", "forgecell-dev-")
	if err != nil {
		return r, err
	}
	defer func() { _ = resources.Remove(work) }()
	built := filepath.Join(work, "forgecell")
	build := o.Build
	if build == nil {
		build = goBuild
	}
	if err = build(ctx, root, version, built); err != nil {
		return r, err
	}
	protocol, err := builtProtocol(ctx, built)
	if err != nil {
		return r, err
	}
	after, err := inspectCheckout(ctx, root)
	if err != nil {
		return r, err
	}
	if after.rev != before.rev || after.digest != before.digest {
		return r, fmt.Errorf("checkout changed while it was being built; nothing was installed")
	}
	release := Options{Home: o.Home, Bin: o.Bin, Source: built, Version: version, Protocol: protocol}
	var dir string
	if o.Activate {
		installed, err := Install(ctx, release)
		if err != nil {
			return r, err
		}
		dir = filepath.Join(installed.Home, "releases", version)
	} else if dir, err = Stage(ctx, release); err != nil {
		return r, err
	}
	meta := harness.ReadManifest(dir)
	if !meta.Found {
		return r, fmt.Errorf("staged release has no readable manifest: %s", dir)
	}
	if err = recordDevBuild(dir, version, headCommitTime(ctx, root)); err != nil {
		return r, err
	}
	return DevRelease{Version: version, Path: filepath.Join(dir, "forgecell"), ManifestSHA256: meta.Digest, Activated: o.Activate}, nil
}
