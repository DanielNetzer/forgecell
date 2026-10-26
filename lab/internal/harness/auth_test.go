package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// Recorded Claude Code final JSON for an expired OAuth session (2026-10-25), with a
// fake token and home path added to prove neither reaches any surface.
const (
	authFakeToken = "sk-ant-oat01-FAKEAUTHTOKEN0123456789abcdef"
	authFakeHome  = "/Users/forgecell-fake-home/.claude/.credentials.json"
	authExpired   = `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","duration_ms":312,"num_turns":1,"result":"Failed to authenticate: OAuth session expired and could not be refreshed (token ` + authFakeToken + ` at ` + authFakeHome + `)","session_id":"00000000-0000-0000-0000-000000000000","total_cost_usd":0}`
)

func authLeak(t *testing.T, surface, text string) {
	t.Helper()
	for _, secret := range []string{authFakeToken, authFakeHome, "forgecell-fake-home", "OAuth session expired"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaks %q: %s", surface, secret, text)
		}
	}
}

func TestHarnessNotAuthenticatedClassifiesStructuredClaudeAuthFailures(t *testing.T) {
	for name, raw := range map[string]string{
		"recorded oauth expiry":  authExpired,
		"invalid api key":        `{"type":"result","is_error":true,"result":"Invalid API key · Please run /login"}`,
		"authentication_error":   apiError(t, `API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, nil),
		"structured 401":         apiError(t, "API Error", map[string]any{"api_error_status": 401}),
		"nested error type":      `{"type":"result","is_error":true,"error":{"type":"authentication_error","message":"bad"}}`,
		"api_error without flag": `{"type":"result","is_error":false,"terminal_reason":"api_error","result":"Failed to authenticate: OAuth token has expired"}`,
	} {
		if !ClassifyAuthFailure("claude-code", raw) {
			t.Fatalf("%s not classified", name)
		}
	}
}

func TestHarnessNotAuthenticatedIgnoresOtherFailuresAndAdapters(t *testing.T) {
	for name, raw := range map[string]string{
		"overloaded 529":      apiError(t, `API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, nil),
		"unavailable 503":     apiError(t, "API Error: 503 Service Unavailable", nil),
		"invalid request 400": apiError(t, `API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`, nil),
		"permission 403":      apiError(t, "API Error: 403 permission_error", map[string]any{"api_error_status": 403}),
		"network":             apiError(t, "API Error: Can't reach the API server (ENOTFOUND)", nil),
		"success mentions it": `{"type":"result","subtype":"success","is_error":false,"result":"Fixed the Failed to authenticate message"}`,
		"refusal":             `{"type":"result","is_error":true,"terminal_reason":"refusal","result":"I can't help with that"}`,
		"free text":           "Failed to authenticate: OAuth session expired",
		"not an object":       `["Failed to authenticate"]`,
		"empty":               "",
		"oversized":           `{"is_error":true,"result":"Failed to authenticate` + strings.Repeat(" ", maxAuthClassifyBytes) + `"}`,
	} {
		if ClassifyAuthFailure("claude-code", raw) {
			t.Fatalf("%s misclassified as an authentication failure", name)
		}
	}
	// Codex writes only its final message and Cursor emits no structured auth signal:
	// both are explicitly not detected, even for the recorded Claude JSON.
	for _, id := range []string{"codex", "cursor", "custom"} {
		if ClassifyAuthFailure(id, authExpired) {
			t.Fatalf("%s classified", id)
		}
	}
	if _, ok := TransientProviderFailure(authExpired); ok {
		t.Fatal("an authentication failure must not be transient")
	}
}

func TestHarnessNotAuthenticatedHintTable(t *testing.T) {
	for id, want := range map[string]string{
		"claude-code": "harness not authenticated: run `claude auth login`",
		"codex":       "harness not authenticated: run `codex login`",
		"cursor":      "harness not authenticated: run `agent login`",
		"custom":      "harness not authenticated: run the harness login command",
	} {
		if got := NotAuthenticatedHint(id); got != want || !IsNotAuthenticatedHint(got) {
			t.Fatalf("%s: %q", id, got)
		}
	}
	if IsNotAuthenticatedHint("harness not authenticated: run `rm -rf /`") {
		t.Fatal("arbitrary text accepted as a hint")
	}
}

func TestHarnessNotAuthenticatedAttemptClassification(t *testing.T) {
	claude := []string{"/forgecell", "__adapter", "claude-code", "/claude"}
	if hint, ok := ClassifyAuthAttempt(claude, process.Result{RawStdout: authExpired}); !ok || hint != NotAuthenticatedHint("claude-code") {
		t.Fatalf("%q %t", hint, ok)
	}
	for name, c := range map[string]struct {
		command []string
		result  process.Result
	}{
		"custom command": {[]string{"/custom"}, process.Result{RawStdout: authExpired}},
		"timed out":      {claude, process.Result{RawStdout: authExpired, TimedOut: true}},
		"interrupted":    {claude, process.Result{RawStdout: authExpired, Interrupted: true}},
		"non-auth error": {claude, process.Result{RawStdout: apiError(t, "API Error: 529 Overloaded", nil)}},
	} {
		if _, ok := ClassifyAuthAttempt(c.command, c.result); ok {
			t.Fatalf("%s classified", name)
		}
	}
}

func TestHarnessNotAuthenticatedResultParsingNamesTheHint(t *testing.T) {
	want := NotAuthenticatedHint("claude-code")
	if _, err := ReadResult("claude-code", authExpired, true); err == nil || err.Error() != want {
		t.Fatalf("ReadResult: %v", err)
	}
	if _, _, err := ReadCodingResult("claude-code", authExpired); err == nil || err.Error() != want {
		t.Fatalf("ReadCodingResult: %v", err)
	}
	// An unrecognized failure keeps today's message exactly.
	other := `{"type":"result","is_error":true,"result":"something else"}`
	if _, err := ReadResult("claude-code", other, true); err == nil || err.Error() != "claude-code reported a failure" {
		t.Fatalf("unrecognized failure changed: %v", err)
	}
}

func TestHarnessNotAuthenticatedExecuteCodingAndAnalysis(t *testing.T) {
	want := NotAuthenticatedHint("claude-code")
	body := "case \"$*\" in\n\"--help\") echo '--safe-mode --tools --strict-mcp-config --json-schema --no-chrome --disable-slash-commands' ;;\n*) cat >/dev/null\nprintf '%s' '" + authExpired + "'\nexit 1 ;;\nesac\n"
	executable := fake(t, body)
	coding := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": "molecule"}, Timeout: executeBackstop})
	if coding.Coding == nil || coding.Coding.Outcome != "unknown" || coding.Coding.Reason != want || coding.Process.Error != want || coding.Process.RawStdout != authExpired {
		t.Fatalf("coding: %+v", coding)
	}
	authLeak(t, "coding reason", coding.Coding.Reason+coding.Process.Error)
	analysis := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": "ticket-analysis"}, Timeout: executeBackstop})
	if analysis.Process.OK || analysis.Process.Error != want {
		t.Fatalf("analysis: %+v", analysis.Process)
	}
	authLeak(t, "analysis error", analysis.Process.Error)
	// A non-auth failure keeps the bare exit status, as before.
	other := fake(t, strings.Replace(body, authExpired, apiError(t, "API Error: 529 Overloaded", nil), 1))
	if r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: other, Dir: t.TempDir(), Request: map[string]any{"kind": "ticket-analysis"}, Timeout: executeBackstop}); r.Process.Error == want || strings.Contains(r.Process.Error, "not authenticated") {
		t.Fatalf("non-auth analysis failure classified: %+v", r.Process)
	}
}
