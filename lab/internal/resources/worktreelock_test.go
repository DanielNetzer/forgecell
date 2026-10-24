package resources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func worktreeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "base"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestLockWorktreesSerializesConcurrentAddsOnOneRepository(t *testing.T) {
	t.Parallel()
	repo := worktreeRepo(t)
	parent := t.TempDir()
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := LockWorktrees(context.Background(), repo)
			if err != nil {
				errs <- err
				return
			}
			defer release()
			path := filepath.Join(parent, fmt.Sprintf("wt-%d", i))
			if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", fmt.Sprintf("b%d", i), path, "HEAD").CombinedOutput(); err != nil {
				errs <- fmt.Errorf("worktree add %d: %v\n%s", i, err, out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(out), "worktree "); got != n+1 {
		t.Fatalf("registered worktrees = %d, want %d:\n%s", got, n+1, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", WorktreeLockFile)); err != nil {
		t.Fatalf("cross-process lock file: %v", err)
	}
}

func TestLockWorktreesIsPerRepositoryAndSharedByItsWorktrees(t *testing.T) {
	t.Parallel()
	a, b := worktreeRepo(t), worktreeRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	if out, err := exec.Command("git", "-C", a, "worktree", "add", "-q", "--detach", linked, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	release, err := LockWorktrees(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// Another repository is not held up.
	other, err := LockWorktrees(context.Background(), b)
	if err != nil {
		t.Fatalf("lock on another repository: %v", err)
	}
	other()

	// A linked worktree shares the source's common dir, so it waits for the same lock.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if again, err := LockWorktrees(ctx, linked); err == nil {
		again()
		t.Fatal("lock through a linked worktree was granted while the repository lock was held")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock through a linked worktree: %v, want a wait that ends at the deadline", err)
	}

	release()
	release() // idempotent
	after, err := LockWorktrees(context.Background(), linked)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	after()
}
