package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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
// file and are older than StaleAge. Directories this process registered, live owners,
// other users' directories, symbolic links and every other name are never listed.
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
