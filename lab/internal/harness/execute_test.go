package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fake(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake provider")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestProviderExecution(t *testing.T) {
	dir := t.TempDir()
	executable := fake(t, `cat > request.txt
printf '{"structured_output":{"schemaVersion":"v1","outcome":"completed","reason":"implemented","paths":[]},"usage":{"input_tokens":10}}'
`)
	r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: dir, Request: map[string]any{"kind": "molecule"}, Timeout: 30 * time.Second})
	if !r.Process.OK || r.Coding == nil || r.Coding.Outcome != "completed" || !strings.Contains(r.Raw, "input_tokens") {
		t.Fatalf("%+v", r)
	}
	input, err := os.ReadFile(filepath.Join(dir, "request.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(input), "You are the coding harness") {
		t.Fatal("provider prompt was JSON quoted")
	}
}
func TestCodexOutputFile(t *testing.T) {
	executable := fake(t, `while [ "$#" -gt 0 ]; do
 if [ "$1" = "--output-last-message" ]; then shift; out="$1"; fi
 shift
done
cat >/dev/null
printf '{"schemaVersion":"v1","outcome":"completed","reason":"implemented","paths":[]}' > "$out"
`)
	r := Execute(context.Background(), ExecuteOptions{ID: "codex", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": "molecule"}, Timeout: 30 * time.Second})
	if !r.Process.OK || r.Coding == nil || r.Coding.Outcome != "completed" {
		t.Fatalf("%+v", r)
	}
}
func TestProviderFailureCannotBecomeSuccess(t *testing.T) {
	for _, body := range []string{"exit 7", `printf '{"result":"done","permission_denials":[{}]}'`, "printf 'invalid'"} {
		r := Execute(context.Background(), ExecuteOptions{ID: "cursor", Executable: fake(t, body), Dir: t.TempDir(), Request: map[string]any{"kind": "molecule"}, Timeout: 30 * time.Second})
		if r.Process.OK && (r.Coding == nil || r.Coding.Outcome != "unknown") {
			t.Fatalf("%+v", r)
		}
	}
}

// The helper emits a valid result but never exits. It has no descendants, so
// timeout cleanup does not depend on a shell reaping another process.
func TestStuckProviderHelper(t *testing.T) {
	if os.Getenv("FORGECELL_STUCK_PROVIDER") != "1" {
		return
	}
	fmt.Fprint(os.Stdout, `{"structured_output":{"schemaVersion":"v1","outcome":"completed","reason":"implemented","paths":[]}}`)
	fmt.Fprint(os.Stderr, "provider is stuck\n")
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func TestProviderTimeoutFailsClosed(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable := fake(t, `exec "$FORGECELL_PROVIDER_TEST_BINARY" -test.run=^TestStuckProviderHelper$
`)
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		started bool
	}{
		// An already expired deadline deterministically exercises the short-timeout
		// path without depending on process startup speed.
		{"expired-deadline", time.Nanosecond, false},
		{"stuck-after-result", 5 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": "molecule"}, Env: append(os.Environ(), "FORGECELL_STUCK_PROVIDER=1", "FORGECELL_PROVIDER_TEST_BINARY="+binary), Timeout: tc.timeout})
			if !r.Process.TimedOut || r.Process.OK || r.Process.Interrupted || r.Process.Overflow || r.Process.Error != context.DeadlineExceeded.Error() {
				t.Fatalf("timeout did not fail closed: %+v", r)
			}
			if tc.started && (r.Coding == nil || r.Coding.Outcome != "completed" || !strings.Contains(r.Raw, "implemented") || r.Process.Stderr != "provider is stuck\n" || !r.Process.Reconciled) {
				t.Fatalf("stuck fixture did not return its completed-looking result and stop: %+v", r)
			}
		})
	}
}
