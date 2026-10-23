package resources

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeOwner(t *testing.T, dir string, pid int) {
	t.Helper()
	raw, _ := json.Marshal(owner{PID: pid, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err := os.WriteFile(filepath.Join(dir, OwnerFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// deadPID returns the pid of a process that has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// readOnlyModuleTree builds what Go's module cache leaves under a private HOME: files
// and directories without write permission.
func readOnlyModuleTree(t *testing.T, root string) {
	t.Helper()
	mod := mkdir(t, filepath.Join(root, "go", "pkg", "mod", "example.com", "m@v1.0.0"))
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module example.com/m\n"), 0444); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{mod, filepath.Dir(mod)} {
		if err := os.Chmod(dir, 0555); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveTreeRemovesReadOnlyModuleCache(t *testing.T) {
	home := mkdir(t, filepath.Join(t.TempDir(), "forgecell-check-home-1"))
	readOnlyModuleTree(t, home)
	if os.Getuid() != 0 {
		if err := os.RemoveAll(home); err == nil {
			t.Fatal("fixture should defeat a plain os.RemoveAll")
		}
	}
	if err := RemoveTree(home); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		t.Fatalf("tree still exists: %v", err)
	}
	if err := RemoveTree(home); err != nil {
		t.Fatalf("a missing tree is not an error: %v", err)
	}
}

func TestRemoveTreeNeverFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := mkdir(t, filepath.Join(root, "outside"))
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0444); err != nil {
		t.Fatal(err)
	}
	tree := mkdir(t, filepath.Join(root, "forgecell-tree"))
	if err := os.Symlink(outside, filepath.Join(tree, "link")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(tree); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(outside, "keep"))
	if err != nil || info.Mode().Perm() != 0444 {
		t.Fatalf("symlink target was touched: %v %v", info, err)
	}
}

func TestMkdirTempWritesOwnerAndRegisters(t *testing.T) {
	dir, err := MkdirTemp(t.TempDir(), "forgecell-test-")
	if err != nil {
		t.Fatal(err)
	}
	o, ok := readOwner(dir)
	if !ok || o.PID != os.Getpid() || o.StartedAt == "" {
		t.Fatalf("owner file = %+v, %v", o, ok)
	}
	registered := false
	for _, d := range Registered() {
		registered = registered || d == dir
	}
	if !registered {
		t.Fatal("temp directory was not registered")
	}
	readOnlyModuleTree(t, dir)
	if err := CleanupAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("CleanupAll left the directory")
	}
	if len(Registered()) != 0 {
		t.Fatalf("registry not emptied: %v", Registered())
	}
}

func TestSweepRemovesOnlyDeadOwnerForgecellDirsOfCurrentUser(t *testing.T) {
	tmp := t.TempDir()
	now := time.Now()
	dead := mkdir(t, filepath.Join(tmp, "forgecell-check-home-dead"))
	writeOwner(t, dead, deadPID(t))
	readOnlyModuleTree(t, dead)

	live := exec.Command("sleep", "60")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Process.Kill(); _ = live.Wait() }()
	alive := mkdir(t, filepath.Join(tmp, "forgecell-check-home-live"))
	writeOwner(t, alive, live.Process.Pid)

	oldNoOwner := mkdir(t, filepath.Join(tmp, "forgecell-capture-old"))
	newNoOwner := mkdir(t, filepath.Join(tmp, "forgecell-capture-new"))
	if err := os.Chtimes(oldNoOwner, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	foreign := mkdir(t, filepath.Join(tmp, "forgecell-check-home-foreign"))
	writeOwner(t, foreign, deadPID(t))
	other := mkdir(t, filepath.Join(tmp, "other-tool-dir"))
	writeOwner(t, other, deadPID(t))
	if err := os.Chtimes(other, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(tmp, "forgecell-file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	target := mkdir(t, filepath.Join(t.TempDir(), "target"))
	writeOwner(t, target, deadPID(t))
	link := filepath.Join(tmp, "forgecell-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	mine, err := MkdirTemp(tmp, "forgecell-mine-")
	if err != nil {
		t.Fatal(err)
	}
	defer Remove(mine)

	saved := ownedByUser
	ownedByUser = func(path string, info os.FileInfo) bool {
		return filepath.Base(path) != "forgecell-check-home-foreign" && saved(path, info)
	}
	defer func() { ownedByUser = saved }()

	removed, err := SweepStale(tmp, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{dead: true, oldNoOwner: true}
	if len(removed) != len(want) {
		t.Fatalf("removed %v, want exactly %v", removed, want)
	}
	for _, path := range removed {
		resolved, _ := filepath.Abs(path)
		if !want[resolved] {
			t.Fatalf("removed %s, which is not a dead-owner Forgecell directory", path)
		}
	}
	for path := range want {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived the sweep", path)
		}
	}
	for _, path := range []string{alive, newNoOwner, foreign, other, file, link, target, mine} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s must be kept: %v", path, err)
		}
	}
}

// TestSignalCleanupHelper is the child process of the SIGINT tests; it does nothing
// unless the parent test starts it.
func TestSignalCleanupHelper(t *testing.T) {
	mode := os.Getenv("FORGECELL_SIGNAL_HELPER")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	os.Exit(RunWithCleanup(func(ctx context.Context) int {
		dir, err := MkdirTemp(os.Getenv("FORGECELL_SIGNAL_TMP"), "forgecell-check-home-")
		if err != nil {
			fmt.Println("error", err)
			return 1
		}
		readOnlyModuleTree(t, dir)
		fmt.Println(dir)
		if mode == "stuck" {
			// A run that ignores cancellation: only the second signal ends it.
			select {}
		}
		<-ctx.Done()
		return 130
	}, os.Stderr))
}

func runSignalHelper(t *testing.T, mode string, signals int) {
	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalCleanupHelper$")
	cmd.Env = append(os.Environ(), "FORGECELL_SIGNAL_HELPER="+mode, "FORGECELL_SIGNAL_TMP="+tmp)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("helper output: %v", err)
	}
	dir := strings.TrimSpace(line)
	if _, err := os.Stat(filepath.Join(dir, OwnerFile)); err != nil {
		t.Fatalf("helper temp dir %q has no owner file: %v", dir, err)
	}
	for i := 0; i < signals; i++ {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("helper did not exit after SIGINT")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("SIGINT left the registered temp directory %s behind", dir)
	}
	entries, _ := os.ReadDir(tmp)
	if len(entries) != 0 {
		t.Fatalf("temp parent still holds %d entries", len(entries))
	}
}

func TestSIGINTRemovesRegisteredTempDirs(t *testing.T) {
	runSignalHelper(t, "cancel", 1)
}

func TestSecondSIGINTRemovesRegisteredTempDirsOfAStuckRun(t *testing.T) {
	runSignalHelper(t, "stuck", 2)
}
