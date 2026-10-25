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

// executeBackstop bounds a provider run that is expected to finish, or to be
// expired by a marker, on its own. It is not under test: it is far above any
// start-up delay on a loaded machine and still ends a real hang.
const executeBackstop = 2 * time.Minute

func fake(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake provider")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}

// expireOnceExists returns a context that is cancelled with
// context.DeadlineExceeded once path exists. process.Run reports that cause as
// its own timeout, so a test reaches a timeout as soon as the fixture has
// provably started instead of waiting a real deadline out. A fixture that
// never starts is reported as an interruption, which no timeout check accepts.
func expireOnceExists(t *testing.T, path string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(context.Canceled) })
	go func() {
		for deadline := time.Now().Add(60 * time.Second); ctx.Err() == nil; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(path); err == nil {
				cancel(context.DeadlineExceeded)
				return
			}
			if time.Now().After(deadline) {
				cancel(context.Canceled)
				return
			}
		}
	}()
	return ctx
}

func TestProviderExecution(t *testing.T) {
	dir := t.TempDir()
	executable := fake(t, `cat > request.txt
printf '{"structured_output":{"schemaVersion":"v1","outcome":"completed","reason":"implemented","paths":[]},"usage":{"input_tokens":10}}'
`)
	r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: dir, Request: map[string]any{"kind": "molecule"}, Timeout: executeBackstop})
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
func TestExecuteAllowlistReachesClaudeArgv(t *testing.T) {
	for _, c := range allowlistCases() {
		t.Run(c.name, func(t *testing.T) {
			list, err := DeriveCodingAllowlist(contextFormula(t, c.components...), c.checks)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			executable := fake(t, `printf '%s\n' "$@" > argv.txt
cat > /dev/null
printf '{"structured_output":{"schemaVersion":"v1","outcome":"completed","reason":"implemented","paths":[]}}'
`)
			r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: dir, Request: map[string]any{"kind": "molecule", "codingAllowlist": list}, Timeout: executeBackstop})
			if !r.Process.OK || r.Coding == nil || r.Coding.Outcome != "completed" {
				t.Fatalf("%+v", r)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "argv.txt"))
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			if got := allowedTools(t, args); got != strings.Join(c.want, ",") {
				t.Fatalf("fake claude received %q, want %q", got, strings.Join(c.want, ","))
			}
			if !strings.Contains(strings.Join(args, " "), "--permission-mode dontAsk") {
				t.Fatal("dontAsk permission mode lost")
			}
		})
	}
}

func TestExecuteRefusesUnsafeAllowlistBeforeStartingProvider(t *testing.T) {
	for _, entry := range []string{"Bash(git push)", "Bash(sh -c x)", "Bash(*)", "Bash"} {
		dir := t.TempDir()
		executable := fake(t, "touch started\ncat > /dev/null\n")
		r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: dir, Request: map[string]any{"kind": "molecule", "codingAllowlist": []string{entry}}, Timeout: executeBackstop})
		if r.Process.OK || r.Coding != nil || r.Process.Error == "" {
			t.Fatalf("%s: %+v", entry, r)
		}
		if _, err := os.Stat(filepath.Join(dir, "started")); !os.IsNotExist(err) {
			t.Fatalf("%s: provider started under an unsafe allowlist", entry)
		}
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
	r := Execute(context.Background(), ExecuteOptions{ID: "codex", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": "molecule"}, Timeout: executeBackstop})
	if !r.Process.OK || r.Coding == nil || r.Coding.Outcome != "completed" {
		t.Fatalf("%+v", r)
	}
}
func TestProviderFailureCannotBecomeSuccess(t *testing.T) {
	for _, body := range []string{"exit 7", `printf '{"result":"done","permission_denials":[{}]}'`, "printf 'invalid'"} {
		r := Execute(context.Background(), ExecuteOptions{ID: "cursor", Executable: fake(t, body), Dir: t.TempDir(), Request: map[string]any{"kind": "molecule"}, Timeout: executeBackstop})
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
	if marker := os.Getenv("FORGECELL_STUCK_MARKER"); marker != "" {
		os.WriteFile(marker, nil, 0600)
	}
	// Sleep past expireOnceExists' backstop so only the expiry can end the run.
	time.Sleep(2 * time.Minute)
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
		// The helper prints its result, creates its marker and then sleeps past
		// every backstop. The deadline expires once the marker exists, so the run
		// neither waits the timeout out nor depends on how fast the helper starts
		// under load; the own timeout is only a backstop.
		{"stuck-after-result", executeBackstop, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			marker := filepath.Join(t.TempDir(), "printed")
			if tc.started {
				ctx = expireOnceExists(t, marker)
			}
			r := Execute(ctx, ExecuteOptions{ID: "claude-code", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": "molecule"}, Env: append(os.Environ(), "FORGECELL_STUCK_PROVIDER=1", "FORGECELL_PROVIDER_TEST_BINARY="+binary, "FORGECELL_STUCK_MARKER="+marker), Timeout: tc.timeout})
			if !r.Process.TimedOut || r.Process.OK || r.Process.Interrupted || r.Process.Overflow || r.Process.Error != context.DeadlineExceeded.Error() {
				t.Fatalf("timeout did not fail closed: %+v", r)
			}
			if tc.started && (r.Coding == nil || r.Coding.Outcome != "completed" || !strings.Contains(r.Raw, "implemented") || r.Process.Stderr != "provider is stuck\n" || !r.Process.Reconciled) {
				t.Fatalf("stuck fixture did not return its completed-looking result and stop: %+v", r)
			}
		})
	}
}
