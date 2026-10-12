//go:build unix

package harness

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// within fails the test when f has not returned in time, and then unblocks it by
// opening the FIFO's write end so a regression does not leak a stuck goroutine.
func within(t *testing.T, fifo string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
		t.Fatal("identity reader blocked on a FIFO")
	}
}

func TestReadManifestRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "manifest.json")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	within(t, fifo, func() { m = ReadManifest(dir) })
	if m.Found || m.Version != "unknown" || m.Protocol != "unknown" || m.Digest != "" {
		t.Fatalf("a FIFO manifest was read: %+v", m)
	}
}

func TestManagedTargetRejectsFIFOWithoutBlocking(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(bin, "forgecell")
	if err := syscall.Mkfifo(fifo, 0700); err != nil {
		t.Fatal(err)
	}
	within(t, fifo, func() {
		if target, ok := managedTarget(fifo); ok || target != "" {
			t.Errorf("a FIFO was treated as a managed launcher: %q", target)
		}
	})
	within(t, fifo, func() {
		if id := ResolveLauncher(fifo); id.Managed || id.Runnable() || id.Manifest.Found {
			t.Errorf("a FIFO launcher resolved: %+v", id)
		}
	})
}
