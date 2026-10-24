package resources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OwnerFile is written into every Forgecell temp directory: the creating process's pid
// and start time, so a later sweep can tell a live directory from a leaked one.
const OwnerFile = ".forgecell-owner"

// TempPrefix starts the name of every Forgecell temp directory under TMPDIR. The sweep
// considers nothing else.
const TempPrefix = "forgecell-"

// StaleAge is how old a forgecell-* directory without an owner file must be before the
// sweep treats it as leaked.
const StaleAge = 24 * time.Hour

// maxOwnerBytes bounds the owner file a sweep reads.
const maxOwnerBytes = 4096

type owner struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
}

var processStart = time.Now()

// ownedByUser is ownedByCurrentUser; a variable only so tests can present a directory
// as another user's.
var ownedByUser = ownedByCurrentUser

// registry holds every temp directory this process created and has not yet removed.
var registry = struct {
	sync.Mutex
	dirs map[string]bool
}{dirs: map[string]bool{}}

// MkdirTemp creates a temp directory as os.MkdirTemp(dir, pattern) does, writes its
// owner file and registers it, so it is removed on normal exit and on SIGINT or SIGTERM
// even when the caller's own cleanup never runs. The returned path is absolute.
func MkdirTemp(dir, pattern string) (string, error) {
	path, err := os.MkdirTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	if abs, e := filepath.Abs(path); e == nil {
		path = abs
	}
	raw, _ := json.Marshal(owner{PID: os.Getpid(), StartedAt: processStart.UTC().Format(time.RFC3339Nano)})
	if err := os.WriteFile(filepath.Join(path, OwnerFile), append(raw, '\n'), 0600); err != nil {
		return "", errors.Join(err, RemoveTree(path))
	}
	registry.Lock()
	registry.dirs[path] = true
	registry.Unlock()
	return path, nil
}

// LabTempMarker is written into a Lab's check TMPDIR when LabTempDir creates it. Pruning
// touches a check TMPDIR only when the marker is there, so a directory Forgecell did not
// create is never emptied.
const LabTempMarker = ".forgecell-check-tmp"

// LabTempPrefix starts the name of every Lab's check TMPDIR under the system temp dir.
const LabTempPrefix = TempPrefix + "lab-"

// LabTempLabFile, inside a Lab's check TMPDIR, records the Lab path it belongs to.
const LabTempLabFile = "lab"

// LegacyLabTempName is the check TMPDIR <lab>/tmp that Forgecell used before it moved
// under the system temp dir. It is never pruned or removed automatically.
const LegacyLabTempName = "tmp"

// LabTempRootEnv names the environment variable that, set to a usable directory outside the
// home, is the root of every Lab's check TMPDIR ahead of $TMPDIR.
const LabTempRootEnv = "FORGECELL_LAB_TMP_ROOT"

// labTempRoot is selectLabTempRoot; a variable only so tests can supply another root.
var labTempRoot = selectLabTempRoot

// defaultLabTempRoot is systemDefaultLabTempRoot; a variable only so tests can supply
// another default.
var defaultLabTempRoot = systemDefaultLabTempRoot

// getconfTimeout bounds the one getconf call that finds the per-user temp dir on macOS.
const getconfTimeout = 2 * time.Second

// darwinUserTemp caches the per-user temp dir getconf reported on macOS; only a success is
// cached, so a failed call is tried again next time.
var darwinUserTemp struct {
	sync.Mutex
	dir string
}

// selectLabTempRoot returns the root every Lab's check TMPDIR lives under, the first of:
// $FORGECELL_LAB_TMP_ROOT, then $TMPDIR, each only when set, usable and not inside the home
// (os.UserHomeDir, which is the private HOME inside a check view), then the per-user default.
// Following the environment keeps tests and nested runs inside a check view under that
// view's TMPDIR, so they never leak directories into the user's real temp dir, and keeps a
// user's disk-backed TMPDIR. The note says which set root was passed over, and why.
func selectLabTempRoot() (string, string) {
	notes := []string{}
	for _, name := range []string{LabTempRootEnv, "TMPDIR"} {
		dir := os.Getenv(name)
		if dir == "" {
			continue
		}
		if _, err := checkLabTempRoot(dir); err != nil {
			notes = append(notes, fmt.Sprintf("$%s is not used for the Lab's check TMPDIR: %v.", name, err))
			continue
		}
		return dir, strings.Join(notes, " ")
	}
	dir, note := defaultLabTempRoot()
	if note != "" {
		notes = append(notes, note)
	}
	return dir, strings.Join(notes, " ")
}

// systemDefaultLabTempRoot is the per-user default root, which does not follow $TMPDIR: on
// macOS the per-user temp dir (confstr _CS_DARWIN_USER_TEMP_DIR, read through getconf), on
// Linux /tmp, elsewhere os.TempDir(). When getconf fails on macOS it falls back to
// os.TempDir() and returns a note saying so.
func systemDefaultLabTempRoot() (string, string) {
	switch runtime.GOOS {
	case "linux":
		return "/tmp", ""
	case "darwin":
		dir, err := darwinUserTempDir()
		if err == nil {
			return dir, ""
		}
		return os.TempDir(), fmt.Sprintf("The per-user temp dir is unknown (%v); the Lab's check TMPDIR root is %s instead.", err, os.TempDir())
	}
	return os.TempDir(), ""
}

// darwinUserTempDir runs getconf DARWIN_USER_TEMP_DIR, at most getconfTimeout, and caches
// the directory it prints once it succeeds.
func darwinUserTempDir() (string, error) {
	darwinUserTemp.Lock()
	defer darwinUserTemp.Unlock()
	if darwinUserTemp.dir != "" {
		return darwinUserTemp.dir, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), getconfTimeout)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "/usr/bin/getconf", "DARWIN_USER_TEMP_DIR").Output()
	if err != nil {
		return "", fmt.Errorf("getconf DARWIN_USER_TEMP_DIR: %w", err)
	}
	dir := strings.TrimSpace(string(raw))
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("getconf DARWIN_USER_TEMP_DIR printed %q", dir)
	}
	darwinUserTemp.dir = dir
	return dir, nil
}

// DefaultLabTempRoot returns the per-user default root of the Lab check TMPDIRs, and a note
// when it is not the fixed one.
func DefaultLabTempRoot() (string, string) {
	return defaultLabTempRoot()
}

// LabTempRootNote returns why the Lab's check TMPDIR root is not the first one it would be
// (a set $FORGECELL_LAB_TMP_ROOT or $TMPDIR was passed over, or the macOS per-user temp dir
// is unknown), or "" when it is.
func LabTempRootNote() string {
	_, note := labTempRoot()
	return note
}

// checkLabTempRoot returns base resolved and absolute when it is an existing directory that
// does not lie inside the home (os.UserHomeDir).
func checkLabTempRoot(base string) (string, error) {
	root, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("temp root: %w", err)
	}
	if root, err = filepath.Abs(root); err != nil {
		return "", err
	}
	if info, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("temp root: %w", err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("temp root %s is not a directory", root)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			home = resolved
		}
		if insideHome(home, root) {
			return "", fmt.Errorf("temp root %s lies inside the home %s; refusing it for the Lab's check TMPDIR", root, home)
		}
	}
	return root, nil
}

// insideHome reports whether path is home or lies under it. A home of "/" holds nothing
// here, since every path would lie inside it. On macOS, whose file systems ignore case by
// default, case is ignored.
func insideHome(home, path string) bool {
	home = filepath.Clean(home)
	if !filepath.IsAbs(home) || home == string(filepath.Separator) {
		return false
	}
	if runtime.GOOS == "darwin" {
		home, path = strings.ToLower(home), strings.ToLower(path)
	}
	rel, err := filepath.Rel(home, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// LabTempRoots returns the resolved, distinct roots a Lab's check TMPDIR may live under:
// the current one (LabTempPath's) and the per-user default, which differ when
// $FORGECELL_LAB_TMP_ROOT or $TMPDIR chose another. A root that is unusable is left out.
func LabTempRoots() []string {
	current, _ := labTempRoot()
	def, _ := defaultLabTempRoot()
	roots := []string{}
	for _, base := range []string{current, def} {
		root, err := checkLabTempRoot(base)
		if err != nil || slices.Contains(roots, root) {
			continue
		}
		roots = append(roots, root)
	}
	return roots
}

// LabTempPath returns, without creating anything, the stable per-Lab check TMPDIR
// <resolved temp root>/forgecell-lab-<first 16 hex of sha256(resolved lab)>, the root chosen
// as selectLabTempRoot says. It sits outside the Lab, which usually lives under the user's
// home, so tests whose TMPDIR is there never walk up into the real home while their HOME is
// private; a root that itself lies inside the home (os.UserHomeDir) is refused. The same Lab
// always gets the same path in the same environment, which keeps tests that read TMPDIR
// cacheable across views.
func LabTempPath(lab string) (string, error) {
	base, _ := labTempRoot()
	root, err := checkLabTempRoot(base)
	if err != nil {
		return "", err
	}
	resolved, err := resolvedLab(lab)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(resolved))
	return filepath.Join(root, LabTempPrefix+hex.EncodeToString(sum[:])[:16]), nil
}

// resolvedLab is lab made absolute with its symbolic links resolved; a Lab that does not
// exist yet is only made absolute.
func resolvedLab(lab string) (string, error) {
	abs, err := filepath.Abs(lab)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return abs, nil
}

// LabTempDir creates, or reuses, the stable per-Lab check TMPDIR at LabTempPath at 0700
// and returns its absolute path. Concurrent runs of the Lab share it, their files kept apart
// in the unique subdirectories Go and mktemp create under it, so unlike MkdirTemp it is never
// registered for removal. Creating it writes LabTempMarker, then LabTempLabFile; reusing a
// directory of the user (the forgecell-lab-* name is Forgecell's) makes it 0700 and writes
// whichever of them is missing, in that order, so a lost marker never silently stops the
// prune. The temp root may be shared (/tmp on Linux), so a symbolic link, a non-directory
// or another user's directory at that path is refused.
func LabTempDir(lab string) (string, error) {
	dir, err := LabTempPath(lab)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("lab temp directory is a symbolic link: %s", dir)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("lab temp directory is not a directory: %s", dir)
	}
	if !ownedByUser(dir, info) {
		return "", fmt.Errorf("lab temp directory belongs to another user: %s", dir)
	}
	if info.Mode().Perm() != 0700 {
		if err := os.Chmod(dir, 0700); err != nil {
			return "", err
		}
	}
	resolved, err := resolvedLab(lab)
	if err != nil {
		return "", err
	}
	for _, f := range []struct{ name, content string }{
		{LabTempMarker, "Forgecell check TMPDIR; entries older than 24h are pruned.\n"},
		{LabTempLabFile, resolved + "\n"},
	} {
		path := filepath.Join(dir, f.name)
		if _, err := os.Lstat(path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.WriteFile(path, []byte(f.content), 0600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// OwnedByUser reports whether info, the Lstat of path, belongs to the current user, as the
// Lab temp checks decide it.
func OwnedByUser(path string, info os.FileInfo) bool {
	return ownedByUser(path, info)
}

// LegacyLabTemp reports whether <lab>/tmp is a check TMPDIR an earlier Forgecell created
// there: a real directory of the user holding LabTempMarker. It returns the path either
// way. A symbolic link or non-directory at that path is refused.
func LegacyLabTemp(lab string) (string, bool, error) {
	dir, err := filepath.Abs(filepath.Join(lab, LegacyLabTempName))
	if err != nil {
		return "", false, err
	}
	owned, err := ownedLabTemp(dir)
	if err != nil || !owned {
		return dir, false, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return dir, false, err
	}
	return dir, ownedByUser(dir, info), nil
}

// ownedLabTemp reports whether dir is a Lab check TMPDIR that LabTempDir created: a real
// directory (Lstat, never followed) holding a regular LabTempMarker. A missing dir, or one
// without the marker, is not owned; a symbolic link or non-directory is refused.
func ownedLabTemp(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("refusing to prune a symbolic link: %s", dir)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("lab temp directory is not a directory: %s", dir)
	}
	marker, err := os.Lstat(filepath.Join(dir, LabTempMarker))
	return err == nil && marker.Mode().IsRegular(), nil
}

// LabGoTempName is the stable GOTMPDIR every view of a Lab shares, directly under its
// check TMPDIR. The prune treats it as a container: it is kept, and its own children are
// pruned instead.
const LabGoTempName = "gotmp"

// StaleLabTempEntries lists the entries directly under dir, a Lab's stable check TMPDIR,
// that belong to the current user and were last modified more than StaleAge before now:
// what tests and killed runs leaked. The shared GOTMPDIR dir/LabGoTempName is never
// listed itself, whatever its age; when it is a real directory of the user, its direct
// children (go-build* work directories and test temp directories) are listed by the same
// rules. Only a dir LabTempDir created (it holds LabTempMarker, which is never listed, as
// LabTempLabFile is not) has
// entries; a missing dir or one without the marker has none, and a symbolic link is
// refused.
func StaleLabTempEntries(dir string, now time.Time) ([]string, error) {
	owned, err := ownedLabTemp(dir)
	if err != nil || !owned {
		return nil, err
	}
	stale, err := staleChildren(dir, now, func(name string) bool {
		return name == LabTempMarker || name == LabTempLabFile || name == LabGoTempName
	})
	if err != nil {
		return nil, err
	}
	gotmp := filepath.Join(dir, LabGoTempName)
	info, err := os.Lstat(gotmp)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByUser(gotmp, info) {
		return stale, nil
	}
	children, err := staleChildren(gotmp, now, func(string) bool { return false })
	if err != nil {
		return nil, err
	}
	return append(stale, children...), nil
}

// staleChildren lists the entries directly under dir, except those skip names, that
// belong to the current user and were last modified more than StaleAge before now.
// Entries are inspected with Lstat, so a symbolic link is listed as itself, never followed.
func staleChildren(dir string, now time.Time, skip func(name string) bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	stale := []string{}
	for _, e := range entries {
		if skip(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := os.Lstat(path)
		if err != nil || !ownedByUser(path, info) || now.Sub(info.ModTime()) <= StaleAge {
			continue
		}
		stale = append(stale, path)
	}
	return stale, nil
}

// PruneLabTemp removes, with RemoveTree, every entry StaleLabTempEntries lists under dir
// at now. It returns the removed paths and every failure.
func PruneLabTemp(dir string, now time.Time) ([]string, error) {
	stale, err := StaleLabTempEntries(dir, now)
	if err != nil {
		return nil, err
	}
	removed := []string{}
	var problems []error
	for _, path := range stale {
		if err := RemoveTree(path); err != nil {
			problems = append(problems, err)
			continue
		}
		removed = append(removed, path)
	}
	return removed, errors.Join(problems...)
}

// OrphanLabTemp reports why path is an orphaned Lab check TMPDIR, and whether it is one: a
// real directory of the user named forgecell-lab-* that holds LabTempMarker, was last
// modified more than StaleAge before now, and whose LabTempLabFile is missing or names a
// path that no longer exists. A live Lab's directory is never an orphan.
func OrphanLabTemp(path string, now time.Time) (string, bool) {
	if !strings.HasPrefix(filepath.Base(path), LabTempPrefix) {
		return "", false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByUser(path, info) || now.Sub(info.ModTime()) <= StaleAge {
		return "", false
	}
	if owned, _ := ownedLabTemp(path); !owned {
		return "", false
	}
	file := filepath.Join(path, LabTempLabFile)
	fi, err := os.Lstat(file)
	if os.IsNotExist(err) {
		return "no lab file and older than 24h", true
	}
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxOwnerBytes {
		return "", false
	}
	raw, err := os.ReadFile(file)
	lab := strings.TrimSpace(string(raw))
	if err != nil || !filepath.IsAbs(lab) {
		return "", false
	}
	if _, err := os.Lstat(lab); !os.IsNotExist(err) {
		return "", false
	}
	return fmt.Sprintf("its Lab %s no longer exists and it is older than 24h", lab), true
}

// OrphanLabTemps lists the orphaned Lab check TMPDIRs (OrphanLabTemp) directly under root.
func OrphanLabTemps(root string, now time.Time) ([]StaleDir, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	orphans := []StaleDir{}
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		if reason, ok := OrphanLabTemp(path, now); ok {
			orphans = append(orphans, StaleDir{Path: path, Reason: reason})
		}
	}
	return orphans, nil
}

// SweepOrphanLabTemps removes, with RemoveTree, every directory OrphanLabTemps lists under
// root at now. It returns the removed paths and every failure.
func SweepOrphanLabTemps(root string, now time.Time) ([]string, error) {
	orphans, err := OrphanLabTemps(root, now)
	if err != nil {
		return nil, err
	}
	removed := []string{}
	var problems []error
	for _, o := range orphans {
		if err := RemoveTree(o.Path); err != nil {
			problems = append(problems, err)
			continue
		}
		removed = append(removed, o.Path)
	}
	return removed, errors.Join(problems...)
}

// Remove removes a directory MkdirTemp created and unregisters it. A directory that could
// not be removed stays registered, so exit cleanup tries again.
func Remove(path string) error {
	if err := RemoveTree(path); err != nil {
		return err
	}
	registry.Lock()
	delete(registry.dirs, path)
	registry.Unlock()
	return nil
}

// Registered returns the temp directories this process still owns, sorted.
func Registered() []string {
	registry.Lock()
	defer registry.Unlock()
	dirs := make([]string, 0, len(registry.dirs))
	for dir := range registry.dirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// CleanupAll removes every registered temp directory and reports each failure.
func CleanupAll() error {
	var problems []error
	for _, dir := range Registered() {
		if err := Remove(dir); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

// RemoveTree is the one removal helper for Forgecell temp and workspace directories. It
// makes the tree writable first (directories u+rwx, files u+w), because Go writes its
// module cache read-only and a plain os.RemoveAll then fails with "permission denied",
// and then removes it. Symbolic links are removed, never followed. A missing path is not
// an error; any other failure is returned.
func RemoveTree(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	makeWritable(path)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("cannot remove %s: %w", path, err)
	}
	return nil
}

func makeWritable(path string) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	perm := info.Mode().Perm()
	if info.IsDir() {
		if perm&0700 != 0700 {
			_ = os.Chmod(path, perm|0700)
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return
		}
		for _, e := range entries {
			makeWritable(filepath.Join(path, e.Name()))
		}
		return
	}
	if info.Mode().IsRegular() && perm&0200 == 0 {
		_ = os.Chmod(path, perm|0200)
	}
}

// StaleDir is a leaked Forgecell temp directory.
type StaleDir struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// StaleTempDirs lists the forgecell-* directories directly under tmp that belong to the
// current user and were leaked: their owner pid is dead, or they have no readable owner
// file and are older than StaleAge. A Lab check TMPDIR (it holds LabTempMarker) is listed
// only when it is orphaned (OrphanLabTemp). Directories this process registered, live
// owners, other users' directories, symbolic links and every other name are never listed.
func StaleTempDirs(tmp string, now time.Time) ([]StaleDir, error) {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return nil, err
	}
	registry.Lock()
	mine := map[string]bool{}
	for dir := range registry.dirs {
		mine[dir] = true
	}
	registry.Unlock()
	stale := []StaleDir{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), TempPrefix) {
			continue
		}
		path := filepath.Join(tmp, e.Name())
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByUser(path, info) || mine[path] {
			continue
		}
		// A Lab's stable check TMPDIR is shared and kept, and only its old entries are
		// pruned, until its Lab is gone.
		if labTemp, _ := ownedLabTemp(path); labTemp {
			if reason, ok := OrphanLabTemp(path, now); ok {
				stale = append(stale, StaleDir{Path: path, Reason: reason})
			}
			continue
		}
		if o, ok := readOwner(path); ok {
			if o.PID == os.Getpid() || pidAlive(o.PID) {
				continue
			}
			stale = append(stale, StaleDir{Path: path, Reason: fmt.Sprintf("owner pid %d is not running", o.PID)})
			continue
		}
		if now.Sub(info.ModTime()) > StaleAge {
			stale = append(stale, StaleDir{Path: path, Reason: "no owner file and older than 24h"})
		}
	}
	return stale, nil
}

func readOwner(dir string) (owner, bool) {
	var o owner
	file := filepath.Join(dir, OwnerFile)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxOwnerBytes {
		return o, false
	}
	raw, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(raw, &o) != nil || o.PID <= 0 {
		return o, false
	}
	return o, true
}

// SweepStale removes every directory StaleTempDirs lists, with RemoveTree. It returns
// the removed paths and every failure.
func SweepStale(tmp string, now time.Time) ([]string, error) {
	stale, err := StaleTempDirs(tmp, now)
	if err != nil {
		return nil, err
	}
	removed := []string{}
	var problems []error
	for _, s := range stale {
		if err := RemoveTree(s.Path); err != nil {
			problems = append(problems, err)
			continue
		}
		removed = append(removed, s.Path)
	}
	return removed, errors.Join(problems...)
}

// RunWithCleanup runs run under a context that the first SIGINT or SIGTERM cancels, and
// removes every registered temp directory when run returns. A second signal ends the
// process at once, as before, but only after removing the registry; a third signal
// during that removal ends it without waiting. stderr receives
// cleanup failures.
func RunWithCleanup(run func(ctx context.Context) int, stderr io.Writer) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		first := <-signals
		cancel()
		<-signals
		// Restore default handling first, so a third signal ends the process at once
		// even when removing a large registered tree is slow.
		signal.Reset(os.Interrupt, syscall.SIGTERM)
		reportCleanup(stderr, CleanupAll())
		os.Exit(signalExitCode(first))
	}()
	code := run(ctx)
	reportCleanup(stderr, CleanupAll())
	return code
}

func reportCleanup(stderr io.Writer, err error) {
	if err != nil && stderr != nil {
		fmt.Fprintln(stderr, "forgecell: temp cleanup:", err)
	}
}

func signalExitCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}

// IsolateLabTempRoot points LabTempRootEnv at a fresh directory for the rest of the
// process and returns a cleanup that removes it. Test binaries call it from TestMain so
// the Lab temp directories their fixture Labs create never land in the user's real
// temp root, where only the 24-hour sweep would reclaim them.
func IsolateLabTempRoot(prefix string) (func(), error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return nil, err
	}
	if err := os.Setenv(LabTempRootEnv, dir); err != nil {
		_ = RemoveTree(dir)
		return nil, err
	}
	return func() { _ = RemoveTree(dir) }, nil
}
