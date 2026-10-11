package extension

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// MaxCacheBytes bounds the regular-file bytes of one cache entry, and of the
// snapshot it is built from. An entry with more is unavailable.
const MaxCacheBytes = 1 << 30

const (
	cacheRoot = "extensions"

	// markerName holds the tree digest of a completed entry. The digest excludes it.
	markerName = ".complete"

	partialPrefix = ".partial-"
	lockSuffix    = ".lock"
	lockOwnerName = "owner"

	lockPollInterval = 250 * time.Millisecond

	// limitPollInterval is how often a running build is measured against the limits.
	limitPollInterval = 100 * time.Millisecond

	// A lock directory without an owner file is a lock being taken or released
	// until it is this old.
	orphanLockGrace = 10 * time.Second

	snapshotTimeout = 10 * time.Minute

	// prepareOutputBytes bounds the stdout and stderr of the prepare process
	// together. Its output files are bounded separately by MaxCacheBytes.
	prepareOutputBytes = 4 << 20
	gitOutputBytes     = 64 << 10
)

var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)

// The limits are variables only so that tests can lower them.
var cacheByteLimit = int64(MaxCacheBytes)

var cacheEntryLimit = 100000

// CacheOutcome is "built", "reused" or "unavailable".
type CacheOutcome string

const (
	CacheBuilt       CacheOutcome = "built"
	CacheReused      CacheOutcome = "reused"
	CacheUnavailable CacheOutcome = "unavailable"
)

// CacheResult is what PrepareCache reports. Dir and TreeSHA256 are set only for a
// built or reused entry. Code, ElapsedMS and TimedOut come from the prepare
// process; Code is -1 when no prepare process ran or none exited with a status.
type CacheResult struct {
	Key        string
	Dir        string
	Outcome    CacheOutcome
	TreeSHA256 string
	Reason     string
	Code       int
	ElapsedMS  int64
	TimedOut   bool
}

// CacheKey identifies an entry as its path below <Lab>/extensions. Prune takes and
// returns the same keys.
func CacheKey(name, manifestSHA, baseCommit string) string {
	return name + "/" + manifestSHA + "/" + baseCommit
}

func unavailable(key, reason string) CacheResult {
	return CacheResult{Key: key, Outcome: CacheUnavailable, Reason: reason, Code: -1}
}

func cleanAbs(p string) bool {
	return p != "" && !strings.ContainsRune(p, 0) && filepath.IsAbs(p) && filepath.Clean(p) == p
}

func checkCacheRequest(labDir, name, manifestSHA string, m Manifest, sourceRepo, baseCommit string) error {
	switch {
	case !cleanAbs(labDir):
		return fmt.Errorf("the Lab directory %q must be a clean absolute path", labDir)
	case !namePattern.MatchString(name):
		return fmt.Errorf("extension name %q must match %s", name, namePattern)
	case !hex64.MatchString(manifestSHA):
		return fmt.Errorf("manifest SHA-256 %q must be 64 lowercase hexadecimal characters", manifestSHA)
	case !commitPattern.MatchString(baseCommit):
		return fmt.Errorf("base commit %q must be a full lowercase commit hash", baseCommit)
	}
	if err := m.validate(); err != nil {
		return err
	}
	if m.Prepare != nil && !cleanAbs(sourceRepo) {
		return fmt.Errorf("the source repository %q must be a clean absolute path", sourceRepo)
	}
	if info, err := os.Stat(labDir); err != nil || !info.IsDir() {
		return fmt.Errorf("the Lab directory %s is not a readable directory", labDir)
	}
	return nil
}

// PrepareCache returns the cache entry for the manifest at the base commit,
// building it first when it is missing or fails its digest. The index is built
// from a read-only snapshot of the base commit, never from a checkout, in a
// Lab-owned directory. Any failure makes the extension unavailable and leaves no
// partial entry behind.
func PrepareCache(ctx context.Context, labDir, name, manifestSHA string, m Manifest, sourceRepo, baseCommit string) CacheResult {
	if err := checkCacheRequest(labDir, name, manifestSHA, m, sourceRepo, baseCommit); err != nil {
		return unavailable("", err.Error())
	}
	key := CacheKey(name, manifestSHA, baseCommit)
	dir := filepath.Join(labDir, cacheRoot, name, manifestSHA, baseCommit)
	if digest, err := verifyEntry(dir); err == nil {
		return CacheResult{Key: key, Dir: dir, Outcome: CacheReused, TreeSHA256: digest}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return unavailable(key, "cannot create the cache directory: "+err.Error())
	}
	release, err := acquireLock(ctx, dir+lockSuffix)
	if err != nil {
		return unavailable(key, err.Error())
	}
	defer release()
	// Another caller may have finished the entry while this one waited.
	if digest, err := verifyEntry(dir); err == nil {
		return CacheResult{Key: key, Dir: dir, Outcome: CacheReused, TreeSHA256: digest}
	}
	if err := removeEntry(dir); err != nil {
		return unavailable(key, "cannot replace the unusable cache entry: "+err.Error())
	}
	return buildEntry(ctx, key, dir, m, sourceRepo, baseCommit)
}

type cacheBuild struct {
	key        string
	dir        string
	partial    string
	m          Manifest
	sourceRepo string
	baseCommit string
	ran        *process.Result
}

func buildEntry(ctx context.Context, key, dir string, m Manifest, sourceRepo, baseCommit string) CacheResult {
	nonce, err := newNonce()
	if err != nil {
		return unavailable(key, "cannot name a build directory: "+err.Error())
	}
	b := &cacheBuild{key: key, dir: dir, partial: filepath.Join(filepath.Dir(dir), partialPrefix+nonce), m: m, sourceRepo: sourceRepo, baseCommit: baseCommit}
	if err := os.Mkdir(b.partial, 0o700); err != nil {
		return unavailable(key, "cannot create a build directory: "+err.Error())
	}
	// After a successful rename there is nothing left to remove.
	defer removeTree(b.partial)
	if m.Prepare != nil {
		if reason := b.runPrepare(ctx); reason != "" {
			return b.fail(reason)
		}
	}
	return b.finish()
}

func (b *cacheBuild) fail(reason string) CacheResult {
	out := unavailable(b.key, reason)
	if b.ran != nil {
		out.Code, out.ElapsedMS, out.TimedOut = b.ran.Code, b.ran.ElapsedMS, b.ran.TimedOut
	}
	return out
}

// scratch is the private working area of one prepare run. Every path in it is
// absolute, so that it stays valid for a child process with another working
// directory.
type scratch struct {
	root, source, work, home, tmp, execDir, interpDir string
}

func newScratch() (*scratch, error) {
	made, err := os.MkdirTemp("", "forgecell-extension-")
	if err != nil {
		return nil, err
	}
	// A relative TMPDIR makes MkdirTemp return a relative path.
	root, err := filepath.Abs(made)
	if err != nil {
		_ = os.RemoveAll(made)
		return nil, err
	}
	s := &scratch{
		root:      root,
		source:    filepath.Join(root, "source"),
		work:      filepath.Join(root, "work"),
		home:      filepath.Join(root, "home"),
		tmp:       filepath.Join(root, "tmp"),
		execDir:   filepath.Join(root, "bin", "executable"),
		interpDir: filepath.Join(root, "bin", "interpreter"),
	}
	for _, d := range []string{s.source, s.work, s.home, s.tmp, s.execDir, s.interpDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			removeTree(root)
			return nil, err
		}
	}
	return s, nil
}

// runPrepare runs the pinned prepare step against a snapshot and returns why it
// failed, or "" when it produced output in b.partial.
func (b *cacheBuild) runPrepare(ctx context.Context) string {
	s, err := newScratch()
	if err != nil {
		return "cannot create a private temporary directory: " + err.Error()
	}
	defer removeTree(s.root)
	p := b.m.Prepare
	// Verify and copy before anything is extracted: an untrusted executable is
	// never run and never sees a snapshot.
	argv := []string{}
	if p.Interpreter != nil {
		copied, err := copyPinned(*p.Interpreter, s.interpDir)
		if err != nil {
			return "the prepare interpreter cannot be trusted: " + err.Error()
		}
		argv = append(argv, copied)
	}
	copied, err := copyPinned(p.Executable, s.execDir)
	if err != nil {
		return "the prepare executable cannot be trusted: " + err.Error()
	}
	argv = append(argv, copied)
	if reason := b.snapshot(ctx, s); reason != "" {
		return reason
	}
	for _, arg := range p.Args {
		argv = append(argv, substitute(arg, b.partial, s.source))
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + s.home, "TMPDIR=" + s.tmp, "LANG=C.UTF-8"}
	timeout := time.Duration(p.TimeoutMS) * time.Millisecond
	res, reason := runWatched(ctx, process.Options{Argv: argv, Dir: s.work, Env: env, Stdin: []byte{}, Timeout: timeout, MaxOutputBytes: prepareOutputBytes}, b.outputLimitReason)
	b.ran = &res
	if reason != "" {
		return reason
	}
	if !res.OK {
		return describeRun("prepare", res)
	}
	return ""
}

// outputLimitReason says why the output written so far is over the cache limits,
// or returns "".
func (b *cacheBuild) outputLimitReason() string {
	if err := checkTree(b.partial, false); err != nil {
		return err.Error()
	}
	return ""
}

// runWatched runs a process and stops its process group as soon as check, polled
// while it runs, returns a reason. It returns that reason, or "" when check never
// objected. A nil check only runs the process.
func runWatched(ctx context.Context, o process.Options, check func() string) (process.Result, string) {
	if check == nil {
		return process.Run(ctx, o), ""
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done := make(chan struct{})
	finished := make(chan struct{})
	reason := ""
	go func() {
		defer close(finished)
		ticker := time.NewTicker(limitPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if reason = check(); reason != "" {
					cancel(errors.New(reason))
					return
				}
			}
		}
	}()
	res := process.Run(runCtx, o)
	close(done)
	<-finished
	return res, reason
}

// copyPinned places a verified private copy of the file in dir, under its own name.
func copyPinned(f pinned.File, dir string) (string, error) {
	dst := filepath.Join(dir, filepath.Base(f.Path))
	if err := pinned.Copy(f, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func substitute(arg, cache, source string) string {
	for _, p := range []struct{ placeholder, value string }{{placeholderCache, cache}, {placeholderSource, source}} {
		if arg == p.placeholder {
			return p.value
		}
		if rest, ok := strings.CutPrefix(arg, p.placeholder+"/"); ok {
			return p.value + "/" + rest
		}
	}
	return arg
}

// snapshot writes the tracked files of the base commit into s.source. The archive
// is taken from the source repository's object database, so no checkout, index or
// working tree is read, and the result has no .git. Git reads that database
// through a private bare repository with no configuration, attributes, filters,
// hooks or replace refs of its own, so the snapshot depends only on the commit.
func (b *cacheBuild) snapshot(ctx context.Context, s *scratch) string {
	bin, err := exec.LookPath("git")
	if err == nil {
		bin, err = filepath.Abs(bin)
	}
	if err != nil {
		return "git is not available: " + err.Error()
	}
	env := gitEnvironment(s)
	git := func(scope []string, check func() string, args ...string) (process.Result, string) {
		argv := append([]string{bin, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "core.attributesFile=/dev/null", "--no-optional-locks"}, scope...)
		return runWatched(ctx, process.Options{Argv: append(argv, args...), Dir: s.work, Env: env, Stdin: []byte{}, Timeout: snapshotTimeout, MaxOutputBytes: gitOutputBytes}, check)
	}
	located, _ := git([]string{"-C", b.sourceRepo}, nil, "rev-parse", "--git-path", "objects")
	if !located.OK {
		return withDetail(describeRun("git rev-parse in the source repository", located), located.Stderr)
	}
	objects := strings.TrimSuffix(located.Stdout, "\n")
	if !filepath.IsAbs(objects) {
		objects = filepath.Join(b.sourceRepo, objects)
	}
	if info, err := os.Stat(objects); err != nil || !info.IsDir() || strings.ContainsAny(objects, "\r\n") {
		return fmt.Sprintf("the source repository has no usable object database at %q", objects)
	}
	private := filepath.Join(s.root, "objects.git")
	if err := writeIsolatedRepository(private, objects, len(b.baseCommit) == 64); err != nil {
		return "cannot set up an isolated object database: " + err.Error()
	}
	scope := []string{"--git-dir=" + private}
	if res, _ := git(scope, nil, "cat-file", "-e", b.baseCommit+"^{commit}"); !res.OK {
		if res.TimedOut || res.Interrupted || res.Activity == nil {
			return describeRun("git", res)
		}
		return withDetail(fmt.Sprintf("base commit %s is not available in the source repository (a shallow clone or rewritten history?)", b.baseCommit), res.Stderr)
	}
	archive := filepath.Join(s.root, "snapshot.tar")
	archiveReason := func() string {
		if info, err := os.Stat(archive); err == nil && info.Size() > cacheByteLimit {
			return fmt.Sprintf("the snapshot of base commit %s exceeds the %d-byte limit", b.baseCommit, cacheByteLimit)
		}
		return ""
	}
	res, reason := git(scope, archiveReason, "archive", "--format=tar", "--output="+archive, b.baseCommit)
	if reason != "" {
		return reason
	}
	if !res.OK {
		return withDetail(describeRun("git archive of base commit "+b.baseCommit, res), res.Stderr)
	}
	if reason := archiveReason(); reason != "" {
		return reason
	}
	if err := extractSnapshot(archive, s.source); err != nil {
		return fmt.Sprintf("cannot extract the snapshot of base commit %s: %v", b.baseCommit, err)
	}
	_ = os.Remove(archive)
	return ""
}

// writeIsolatedRepository creates a bare repository whose only content is the
// source's object database, borrowed as an alternate.
func writeIsolatedRepository(dir, objects string, sha256Objects bool) error {
	for _, d := range []string{"objects/info", "objects/pack", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(d)), 0o700); err != nil {
			return err
		}
	}
	config := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"
	if sha256Objects {
		config = "[core]\n\trepositoryformatversion = 1\n\tbare = true\n[extensions]\n\tobjectformat = sha256\n"
	}
	for _, f := range []struct{ name, content string }{
		{"HEAD", "ref: refs/heads/main\n"},
		{"config", config},
		{"objects/info/alternates", objects + "\n"},
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(f.name)), []byte(f.content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// gitEnvironment is all that git is given: no ambient variable, so no credential
// or GIT_* override, and no system or user configuration.
func gitEnvironment(s *scratch) []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + s.home,
		"TMPDIR=" + s.tmp,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	}
}

func describeRun(what string, res process.Result) string {
	switch {
	case res.TimedOut:
		return fmt.Sprintf("%s timed out after %d ms", what, res.ElapsedMS)
	case res.Interrupted:
		return what + " was interrupted"
	case res.Overflow:
		return fmt.Sprintf("%s wrote more than the captured output limit", what)
	case !res.Reconciled:
		return what + ": " + res.Error
	case res.Code > 0:
		return fmt.Sprintf("%s exited with code %d", what, res.Code)
	case res.Error != "":
		return what + " failed: " + res.Error
	}
	return what + " failed"
}

// withDetail appends the first line of git's own error text. It never carries the
// prepare tool's output.
func withDetail(message, stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 200 {
				line = line[:200]
			}
			return message + ": " + strings.Map(func(r rune) rune {
				if r < ' ' || r == 0x7f {
					return '?'
				}
				return r
			}, line)
		}
	}
	return message
}

func newNonce() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// finish checks the prepare output, records its digest and publishes it with one
// rename, read-only, so that a visible entry is always a complete one.
func (b *cacheBuild) finish() CacheResult {
	if _, err := os.Lstat(filepath.Join(b.partial, markerName)); err == nil {
		return b.fail("prepare output uses the reserved name " + markerName)
	}
	// The size check precedes hashing so that an oversize output is never read.
	if err := checkTree(b.partial, true); err != nil {
		return b.fail(err.Error())
	}
	digest, err := hashTree(b.partial)
	if err != nil {
		return b.fail("cannot digest the prepare output: " + err.Error())
	}
	if err := writeMarker(b.partial, digest); err != nil {
		return b.fail("cannot write the completion marker: " + err.Error())
	}
	if err := lockDown(b.partial, false); err != nil {
		return b.fail("cannot make the cache read-only: " + err.Error())
	}
	if err := os.Rename(b.partial, b.dir); err != nil {
		return b.fail("cannot publish the cache entry: " + err.Error())
	}
	out := CacheResult{Key: b.key, Dir: b.dir, Outcome: CacheBuilt, TreeSHA256: digest}
	if b.ran != nil {
		out.Code, out.ElapsedMS = b.ran.Code, b.ran.ElapsedMS
	}
	return out
}

func treeRel(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

// checkTree accepts only directories and regular files, at most cacheByteLimit
// bytes of file content and at most cacheEntryLimit entries. A symbolic link could
// point out of the cache, and a special file has no stable content to digest.
// Unless strict, it reports only the limits and skips what it cannot read, so that
// it can measure a tree that is still being written.
func checkTree(root string, strict bool) error {
	var total int64
	entries := 0
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if strict {
				return err
			}
			return nil
		}
		rel := treeRel(root, p)
		if rel == "." || rel == markerName {
			return nil
		}
		if entries++; entries > cacheEntryLimit {
			return fmt.Errorf("prepare output has more than %d entries", cacheEntryLimit)
		}
		switch {
		case d.IsDir():
			return nil
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				if strict {
					return err
				}
				return nil
			}
			if total += info.Size(); total > cacheByteLimit {
				return fmt.Errorf("prepare output exceeds the %d-byte cache limit", cacheByteLimit)
			}
		case strict:
			return fmt.Errorf("prepare output contains %s, which is a symbolic link or special file", rel)
		}
		return nil
	})
}

// hashTree digests the tree in lexical walk order: each directory by path and each
// regular file by path and content SHA-256. Quoting keeps odd file names from
// shifting the record boundaries. The completion marker is excluded.
func hashTree(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := treeRel(root, p)
		switch {
		case rel == "." || rel == markerName:
			return nil
		case d.IsDir():
			fmt.Fprintf(h, "d %q\n", rel)
		case d.Type().IsRegular():
			digest, err := fileSHA256(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "f %s %q\n", digest, rel)
		default:
			return fmt.Errorf("%s is a symbolic link or special file", rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSHA256(p string) (string, error) {
	file, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeMarker(dir, digest string) error {
	file, err := os.OpenFile(filepath.Join(dir, markerName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	_, err = file.WriteString(digest + "\n")
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func readMarker(dir string) (string, error) {
	marker := filepath.Join(dir, markerName)
	info, err := os.Lstat(marker)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 128 {
		return "", errors.New("the completion marker is not a small regular file")
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		return "", err
	}
	digest := strings.TrimSpace(string(raw))
	if !hex64.MatchString(digest) {
		return "", errors.New("the completion marker does not hold a SHA-256")
	}
	return digest, nil
}

// verifyEntry returns the tree digest of a completed entry whose content still
// matches the digest its marker recorded.
func verifyEntry(dir string) (string, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("the cache entry is not a directory")
	}
	recorded, err := readMarker(dir)
	if err != nil {
		return "", err
	}
	if err := checkTree(dir, true); err != nil {
		return "", err
	}
	digest, err := hashTree(dir)
	if err != nil {
		return "", err
	}
	if digest != recorded {
		return "", errors.New("the tree digest differs from the completion marker")
	}
	return digest, nil
}

// lockDown makes a tree read-only: directories 0500 and files 0400, or 0500 for a
// file that was executable when keepExecutable is set.
func lockDown(root string, keepExecutable bool) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o400)
		if info.IsDir() || (keepExecutable && info.Mode().Perm()&0o100 != 0) {
			mode = 0o500
		}
		return os.Chmod(p, mode)
	})
}

// makeWritable gives the owner access to every directory under root, which is what
// deleting a read-only tree needs. It never follows a symbolic link.
func makeWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
}

func removeTree(root string) {
	makeWritable(root)
	_ = os.RemoveAll(root)
}

func removeEntry(dir string) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	makeWritable(dir)
	return os.RemoveAll(dir)
}

// snapshotPath cleans a tar entry name and refuses any that could leave the
// snapshot or put a .git directory in it. It returns "" for the root.
func snapshotPath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("snapshot entry %q has an unsafe path", name)
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("snapshot entry %q leaves the snapshot", name)
	}
	for _, part := range strings.Split(clean, "/") {
		if strings.EqualFold(part, ".git") {
			return "", fmt.Errorf("snapshot entry %q is inside a .git directory", name)
		}
	}
	return clean, nil
}

// extractSnapshot unpacks the regular files and directories of a git archive into
// dst, then makes them read-only. Symbolic links, hard links and devices are not
// materialised: a link could point at a file outside the snapshot, and the
// prepare step would index it. Entries are never written through a link because
// none is ever created. The entries and the file bytes are bounded like a cache.
func extractSnapshot(archive, dst string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := tar.NewReader(file)
	var total int64
	entries := 0
	for {
		hdr, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			continue
		}
		rel, err := snapshotPath(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		if entries++; entries > cacheEntryLimit {
			return fmt.Errorf("the snapshot has more than %d entries", cacheEntryLimit)
		}
		if total += hdr.Size; total > cacheByteLimit {
			return fmt.Errorf("the snapshot exceeds the %d-byte limit", cacheByteLimit)
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := writeSnapshotFile(target, reader, hdr.Mode&0o100 != 0); err != nil {
			return err
		}
	}
	return lockDown(dst, true)
}

func writeSnapshotFile(target string, src io.Reader, executable bool) error {
	perm := os.FileMode(0o600)
	if executable {
		perm = 0o700
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, src)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

// tryLock takes the lock directory or fails with fs.ErrExist when it is held. The
// owner file records this process, like the Molecule lock.
func tryLock(lock string) (func(), error) {
	if err := os.Mkdir(lock, 0o700); err != nil {
		return nil, err
	}
	owner := filepath.Join(lock, lockOwnerName)
	if err := os.WriteFile(owner, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		_ = os.RemoveAll(lock)
		return nil, err
	}
	return func() {
		_ = os.Remove(owner)
		_ = os.Remove(lock)
	}, nil
}

// acquireLock waits for a held lock until it is released or ctx ends. A lock that
// is plainly a leftover is reported, never removed: whether anything is still
// building is for the operator to judge.
func acquireLock(ctx context.Context, lock string) (func(), error) {
	for {
		release, err := tryLock(lock)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("cannot take the cache lock %s: %w", lock, err)
		}
		if reason := staleLock(lock); reason != "" {
			return nil, errors.New(reason)
		}
		timer := time.NewTimer(lockPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("gave up waiting for the cache lock %s: %w", lock, context.Cause(ctx))
		case <-timer.C:
		}
	}
}

// staleLock explains why a lock should not be waited on, or returns "".
func staleLock(lock string) string {
	raw, err := os.ReadFile(filepath.Join(lock, lockOwnerName))
	owner := strings.TrimSpace(string(raw))
	switch {
	case err == nil && owner != "":
		pid, convErr := strconv.Atoi(owner)
		if convErr != nil || pid < 1 {
			return fmt.Sprintf("cache lock %s has an invalid owner file; inspect it and remove the lock by hand", lock)
		}
		if !processAlive(pid) {
			return fmt.Sprintf("cache lock %s is stale: its owner, process %d, has exited; remove the lock by hand once nothing is building", lock, pid)
		}
	case err == nil || errors.Is(err, fs.ErrNotExist):
		// Being taken or released. Only an old ownerless lock is a leftover.
		if info, statErr := os.Stat(lock); statErr == nil && time.Since(info.ModTime()) > orphanLockGrace {
			return fmt.Sprintf("cache lock %s has no owner and is older than %s; remove the lock by hand once nothing is building", lock, orphanLockGrace)
		}
	default:
		return fmt.Sprintf("cache lock %s cannot be inspected: %v", lock, err)
	}
	return ""
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Prune removes the completed entries of one extension beyond the keep most
// recently completed, across all manifest digests. It never removes an entry whose
// key is in referenced, an entry whose lock is held, a partial build or an
// incomplete directory. It returns the removed keys, sorted.
func Prune(labDir, name string, keep int, referenced map[string]bool) ([]string, error) {
	if !cleanAbs(labDir) {
		return nil, fmt.Errorf("the Lab directory %q must be a clean absolute path", labDir)
	}
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("extension name %q must match %s", name, namePattern)
	}
	if keep < 0 {
		return nil, fmt.Errorf("keep must not be negative, got %d", keep)
	}
	base := filepath.Join(labDir, cacheRoot, name)
	digests, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type entry struct {
		key       string
		dir       string
		completed time.Time
	}
	var found []entry
	var errs []error
	for _, d := range digests {
		if !d.IsDir() || !hex64.MatchString(d.Name()) {
			continue
		}
		commits, err := os.ReadDir(filepath.Join(base, d.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, c := range commits {
			if !c.IsDir() || !commitPattern.MatchString(c.Name()) {
				continue
			}
			dir := filepath.Join(base, d.Name(), c.Name())
			marker, err := os.Lstat(filepath.Join(dir, markerName))
			if err != nil || !marker.Mode().IsRegular() {
				continue
			}
			found = append(found, entry{key: CacheKey(name, d.Name(), c.Name()), dir: dir, completed: marker.ModTime()})
		}
	}
	slices.SortFunc(found, func(a, b entry) int {
		if c := b.completed.Compare(a.completed); c != 0 {
			return c
		}
		return strings.Compare(a.key, b.key)
	})
	removed := []string{}
	for i, e := range found {
		if i < keep || referenced[e.key] {
			continue
		}
		release, err := tryLock(e.dir + lockSuffix)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = removeEntry(e.dir)
		release()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, e.key)
	}
	slices.Sort(removed)
	return removed, errors.Join(errs...)
}
