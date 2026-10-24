package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// apiError builds a Claude CLI final JSON that ended with terminal_reason api_error.
func apiError(t *testing.T, result string, extra map[string]any) string {
	t.Helper()
	v := map[string]any{"type": "result", "subtype": "success", "is_error": true, "terminal_reason": "api_error", "result": result}
	for k, e := range extra {
		v[k] = e
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestTransientProviderEveryListedMessageAndStatus: each network-class message and HTTP
// status the ticket lists classifies as transient (#190).
func TestTransientProviderEveryListedMessageAndStatus(t *testing.T) {
	for _, message := range []string{
		"API Error: Can't reach the API server — check your internet or DNS (ENOTFOUND)",
		"API Error: read ECONNRESET",
		"API Error: connect ETIMEDOUT 160.79.104.10:443",
		"API Error: getaddrinfo EAI_AGAIN api.anthropic.com",
		"API Error: socket hang up",
		"API Error: 502 Bad Gateway",
		"API Error: 503 Service Unavailable",
		"API Error: 504 Gateway Timeout",
		"API Error: 529 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}",
		"API Error: Overloaded",
	} {
		got, ok := TransientProviderFailure(apiError(t, message, nil))
		if !ok || got == "" {
			t.Fatalf("%q not transient", message)
		}
	}
	for _, status := range []int{502, 503, 504, 529} {
		if _, ok := TransientProviderFailure(apiError(t, "API Error", map[string]any{"api_error_status": status})); !ok {
			t.Fatalf("status %d not transient", status)
		}
		if _, ok := TransientProviderFailure(apiError(t, "", map[string]any{"error": map[string]any{"status": status, "message": "upstream"}})); !ok {
			t.Fatalf("nested status %d not transient", status)
		}
	}
	if _, ok := TransientProviderFailure(apiError(t, "", map[string]any{"error": map[string]any{"type": "overloaded_error"}})); !ok {
		t.Fatal("nested overloaded_error not transient")
	}
}

// TestTransientProviderRefusalsAndPermissionsAreNot: refusals, policy and permission
// errors, timeouts, budget stops and a non-network api_error are never transient, nor is
// a network-class message without terminal_reason api_error.
func TestTransientProviderRefusalsAndPermissionsAreNot(t *testing.T) {
	for name, raw := range map[string]string{
		"refusal":            `{"type":"result","is_error":true,"terminal_reason":"refusal","result":"I can't help with that (ENOTFOUND)"}`,
		"permission":         `{"type":"result","is_error":true,"terminal_reason":"permission_denied","result":"Permission denied","permission_denials":[{"tool_name":"Bash","tool_input":{"command":"curl"}}]}`,
		"policy api_error":   apiError(t, "API Error: 400 Your request violates the usage policy", nil),
		"auth api_error":     apiError(t, "API Error: 401 invalid x-api-key", nil),
		"permission api":     apiError(t, "API Error: 403 permission_error", map[string]any{"api_error_status": 403}),
		"rate limit":         apiError(t, "API Error: 429 rate_limit_error", map[string]any{"api_error_status": 429}),
		"server 500":         apiError(t, "API Error: 500 Internal server error", nil),
		"timeout":            `{"type":"result","subtype":"error_max_turns","is_error":true,"terminal_reason":"max_turns","result":"ETIMEDOUT"}`,
		"budget":             `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"terminal_reason":"budget","result":"socket hang up"}`,
		"no terminal reason": `{"type":"result","is_error":true,"result":"API Error: ENOTFOUND"}`,
		"success":            `{"type":"result","subtype":"success","is_error":false,"result":"done"}`,
		"not json":           "API Error: ENOTFOUND",
		"empty":              "",
	} {
		if message, ok := TransientProviderFailure(raw); ok {
			t.Fatalf("%s classified as transient: %q", name, message)
		}
	}
}

// TestTransientProviderClassifiesOnlyClaudeAttemptsThatRanOut: only a native Claude
// binding is classified, and the Lab's own timeout or interruption never is.
func TestTransientProviderReadsOnlyTheErrorsOwnStatus(t *testing.T) {
	for _, message := range []string{
		`API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"messages.503.content.1: unexpected tool_use_id"}}`,
		"API Error: 400 tools.504.custom.name: String should match pattern",
		"API Error: 400 prompt is too long: 503 000 tokens",
		"API Error: 401 authentication_error: connection error. invalid x-api-key",
		"API Error: 403 permission_error (ECONNRESET)",
		"API Error: 413 request timed out while uploading",
		`API Error: {"type":"error","error":{"type":"invalid_request_error","message":"overloaded tool list"}}`,
	} {
		if got, ok := TransientProviderFailure(apiError(t, message, nil)); ok {
			t.Fatalf("%q classified as transient: %q", message, got)
		}
	}
	if _, ok := TransientProviderFailure(apiError(t, "API Error: socket hang up", map[string]any{"api_error_status": 400})); ok {
		t.Fatal("a structured 4xx status classified as transient")
	}
	if _, ok := TransientProviderFailure(apiError(t, "", map[string]any{"error": map[string]any{"status": 503, "type": "permission_error"}})); ok {
		t.Fatal("a permission_error classified as transient")
	}
	for _, message := range []string{
		"API Error: 502 Bad Gateway",
		"API Error:503 upstream connect error",
		`API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		"API Error: Overloaded",
	} {
		if _, ok := TransientProviderFailure(apiError(t, message, nil)); !ok {
			t.Fatalf("%q not transient", message)
		}
	}
}

func TestTransientProviderConnectionAndTimeoutShapes(t *testing.T) {
	for _, message := range []string{
		"API Error: Connection error.",
		"API Error: Request timed out.",
		"Request timed out.",
		"API Error: connect ECONNREFUSED 127.0.0.1:443",
		"API Error: Can't reach the API server",
		"API Error: Can’t reach the API server — check your internet (ENOTFOUND)",
		"API Error: Cannot reach the API server",
		"API Error: Your computer went to sleep mid-response. Please try again.",
	} {
		if _, ok := TransientProviderFailure(apiError(t, message, nil)); !ok {
			t.Fatalf("%q not transient", message)
		}
	}
}

func TestTransientProviderClassifiesOnlyClaudeAttemptsThatRanOut(t *testing.T) {
	raw := apiError(t, "API Error: Can't reach the API server (ENOTFOUND)", nil)
	claude := []string{"/forgecell", "__adapter", "claude-code", "/claude"}
	if got := ClassifyCodingAttempt(claude, process.Result{Code: 1, RawStdout: raw}); got != TransientProviderError {
		t.Fatalf("classification = %q", got)
	}
	for name, c := range map[string]struct {
		command []string
		result  process.Result
	}{
		"custom":          {[]string{"/harness"}, process.Result{Code: 1, RawStdout: raw}},
		"codex":           {[]string{"/forgecell", "__adapter", "codex", "/codex"}, process.Result{Code: 1, RawStdout: raw}},
		"timed out":       {claude, process.Result{Code: -1, TimedOut: true, RawStdout: raw}},
		"interrupted":     {claude, process.Result{Code: -1, Interrupted: true, RawStdout: raw}},
		"parent deadline": {claude, process.Result{Code: -1, ParentDeadline: true, RawStdout: raw}},
	} {
		if got := ClassifyCodingAttempt(c.command, c.result); got != "" {
			t.Fatalf("%s classified as %q", name, got)
		}
	}
}

// TestTransientProviderReasonSurvivesCodingResultParsing: the adapter keeps the
// classification and the provider message instead of a generic failure.
func TestTransientProviderReasonSurvivesCodingResultParsing(t *testing.T) {
	raw := apiError(t, "API Error: Can't reach the API server — check your internet or DNS (ENOTFOUND)", nil)
	_, _, err := ReadCodingResult("claude-code", raw)
	if err == nil || !strings.Contains(err.Error(), TransientProviderError) || !strings.Contains(err.Error(), "ENOTFOUND") {
		t.Fatalf("error = %v", err)
	}
	// The provider script quotes its output in single quotes, so this message has none.
	raw = apiError(t, "API Error: Cannot reach the API server (ENOTFOUND)", nil)
	executable := fake(t, "cat >/dev/null\nprintf '%s' '"+raw+"'\nexit 1\n")
	r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": "molecule"}})
	if r.Coding == nil || r.Coding.Outcome != "unknown" || !strings.Contains(r.Coding.Reason, TransientProviderError) || r.Process.RawStdout != raw || !strings.Contains(r.Process.Error, TransientProviderError) || !strings.Contains(r.Process.Error, "ENOTFOUND") {
		t.Fatalf("transient evidence lost: %+v", r)
	}
	if got := ClassifyCodingAttempt([]string{"/forgecell", "__adapter", "claude-code", executable}, r.Process); got != TransientProviderError {
		t.Fatalf("classification from the executed attempt = %q", got)
	}
}
