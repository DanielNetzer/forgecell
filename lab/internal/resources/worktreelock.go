package resources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// WorktreeLockFile is created in a repository's git common dir and flocked while
// Forgecell adds, removes or prunes one of its worktrees.
const WorktreeLockFile = "forgecell-worktree.lock"

// worktreeLocks holds one single-slot semaphore per resolved git common dir. flock is
// per open file description, so two goroutines of one process would not exclude each
// other through it alone.
var worktreeLocks struct {
	sync.Mutex
	byDir map[string]chan struct{}
}

// worktreeLockPoll is how often a held cross-process lock is retried.
const worktreeLockPoll = 20 * time.Millisecond

// LockWorktrees serializes `git worktree add/remove/prune` on the repository at source,
// within this process and across processes: git's worktree metadata updates are not safe
// concurrently on one repository. Hold it only around the worktree metadata operation,
// never around the checks that run inside a worktree. The returned release is idempotent.
func LockWorktrees(ctx context.Context, source string) (func(), error) {
	dir, err := gitCommonDir(ctx, source)
	if err != nil {
		return nil, err
	}
	worktreeLocks.Lock()
	if worktreeLocks.byDir == nil {
		worktreeLocks.byDir = map[string]chan struct{}{}
	}
	slot, ok := worktreeLocks.byDir[dir]
	if !ok {
		slot = make(chan struct{}, 1)
		worktreeLocks.byDir[dir] = slot
	}
	worktreeLocks.Unlock()
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	file, err := flockWorktrees(ctx, filepath.Join(dir, WorktreeLockFile))
	if err != nil {
		<-slot
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// As in the ledger lock, the descriptor is closed under ForkLock so a child
			// forked concurrently never inherits the lock.
			syscall.ForkLock.RLock()
			syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			file.Close()
			syscall.ForkLock.RUnlock()
			<-slot
		})
	}, nil
}

// flockWorktrees opens path and takes an exclusive flock on it, retrying while another
// process holds it until ctx ends.
func flockWorktrees(ctx context.Context, path string) (*os.File, error) {
	syscall.ForkLock.RLock()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(worktreeLockPoll):
		}
	}
}

// gitCommonDir resolves the git common dir shared by source and all of its worktrees,
// with symlinks evaluated, so every path to one repository keys the same lock.
func gitCommonDir(ctx context.Context, source string) (string, error) {
	r := process.Run(ctx, process.Options{
		Argv:           []string{"git", "rev-parse", "--git-common-dir"},
		Dir:            source,
		Env:            withoutGitEnv(os.Environ()),
		Stdin:          []byte{},
		Timeout:        30 * time.Second,
		MaxOutputBytes: 1_000_000,
	})
	if !r.OK {
		return "", fmt.Errorf("cannot resolve git common dir of %s: %s %s", source, r.Error, strings.TrimSpace(r.Stderr))
	}
	dir := strings.TrimSpace(r.Stdout)
	if dir == "" {
		return "", fmt.Errorf("cannot resolve git common dir of %s", source)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(source, dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

// withoutGitEnv drops GIT_* variables, so an inherited GIT_DIR or GIT_COMMON_DIR cannot
// point the lookup at another repository.
func withoutGitEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}
