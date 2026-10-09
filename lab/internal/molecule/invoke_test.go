package molecule

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBoundAdapterTimeoutStopsProviderDescendants(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	provider := filepath.Join(dir, "provider")
	script := "#!/bin/sh\n(printf started > started; sleep 2.5; printf late > orphan) &\nwait\n"
	if err = os.WriteFile(provider, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	// Wait for the descendant to exist before expiring the parent deadline.
	// Startup scheduling under a parallel suite is not the behavior under test;
	// process.Run separately tests its wall-clock timeout. This exercises the
	// native adapter's shared process group after a deadline has actually expired.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	done := make(chan process.Result, 1)
	go func() {
		done <- invokeHarness(ctx, []string{exe, "__adapter", "claude-code", provider}, dir, 10*time.Second, map[string]any{"kind": "molecule", "issue": map[string]any{"number": 9, "title": "fixture"}})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(filepath.Join(dir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel(context.Canceled)
			<-done
			t.Fatal("provider never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel(context.DeadlineExceeded)
	r := <-done
	if r.OK || !r.TimedOut {
		t.Fatalf("expected provider deadline: %+v", r)
	}
	time.Sleep(2700 * time.Millisecond)
	if _, err = os.Stat(filepath.Join(dir, "orphan")); !os.IsNotExist(err) {
		t.Fatal("provider descendant survived timeout")
	}
}

func TestNativeBindingRetainsBlockedContractAndTransport(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	provider := filepath.Join(dir, "provider")
	report := `{"schemaVersion":"v1","outcome":"blocked","reason":"cache denied","paths":[]}`
	if err = os.WriteFile(provider, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"structured_output\":"+report+"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result := invokeHarness(context.Background(), []string{exe, "__adapter", "claude-code", provider}, dir, time.Second, map[string]any{"kind": "molecule"})
	if !result.OK || result.Code != 0 || !result.Reconciled || result.Stdout != report || result.RawStdout == "" {
		t.Fatalf("native evidence lost: %+v", result)
	}
}
