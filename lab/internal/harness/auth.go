package harness

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// HarnessNotAuthenticated is the typed failure class of an attempt whose provider
// CLI reported, in its structured output, that it is not authenticated (#244).
const HarnessNotAuthenticated = "harness-not-authenticated"

// loginCommands is the fixed per-adapter login command table. Nothing from provider
// output ever reaches the hint built from it.
var loginCommands = map[string]string{"claude-code": "claude auth login", "codex": "codex login", "cursor": "agent login"}

const notAuthenticatedPrefix = "harness not authenticated: run "

// genericNotAuthenticated is the hint for an adapter without a table entry.
const genericNotAuthenticated = "harness not authenticated: run the harness login command"

// LoginCommand is the adapter's own login command, or "" for an unknown adapter.
func LoginCommand(id string) string { return loginCommands[id] }

// NotAuthenticatedHint is the fixed text every surface shows for an attempt of
// adapter id classified HarnessNotAuthenticated.
func NotAuthenticatedHint(id string) string {
	if login := LoginCommand(id); login != "" {
		return notAuthenticatedPrefix + "`" + login + "`"
	}
	return genericNotAuthenticated
}

// IsNotAuthenticatedHint reports whether s is exactly one of the fixed hints, so a
// surface reading it back from an untrusted ledger shows only table text.
func IsNotAuthenticatedHint(s string) bool {
	if s == genericNotAuthenticated {
		return true
	}
	for id := range loginCommands {
		if s == NotAuthenticatedHint(id) {
			return true
		}
	}
	return false
}

// maxAuthClassifyBytes bounds the provider output the classifier parses; larger
// output is never classified and keeps today's handling.
const maxAuthClassifyBytes = 1_000_000

// authPhrase matches the authentication messages the Claude CLI and the Anthropic
// API report. It is only consulted on a structured error result, never on free text.
var authPhrase = regexp.MustCompile(`(?i)authentication_error|failed to authenticate|oauth (?:session|token)[^.;]{0,40}(?:expired|invalid|revoked)|invalid api key|not logged in|please run /login|^API Error:\s*401\b|HTTP status 401\b`)

// ClassifyAuthFailure reports whether raw, the final output of adapter id, is a
// structured provider authentication failure. For Claude Code that is a JSON object
// with is_error true or terminal_reason "api_error", whose error message, type or
// status names an authentication failure. Codex writes only the final message and
// Cursor emits no structured authentication signal, so both are explicitly not
// detected. Non-JSON or oversized output is never classified.
func ClassifyAuthFailure(id, raw string) bool {
	if id != "claude-code" || len(raw) > maxAuthClassifyBytes {
		return false
	}
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &value) != nil || value == nil {
		return false
	}
	var isError bool
	_ = json.Unmarshal(value["is_error"], &isError)
	var reason string
	_ = json.Unmarshal(value["terminal_reason"], &reason)
	if !isError && reason != "api_error" {
		return false
	}
	var e providerError
	for _, key := range []string{"result", "error", "message", "api_error"} {
		e.collect(value[key])
	}
	e.statuses(value)
	for _, status := range e.codes {
		if status == 401 {
			return true
		}
	}
	for _, text := range e.texts {
		if authPhrase.MatchString(text) {
			return true
		}
	}
	return false
}

// bindingAdapter is the adapter id of a native `launcher __adapter ID PROVIDER`
// binding, or "" for a custom command.
func bindingAdapter(command []string) string {
	if len(command) == 4 && command[1] == "__adapter" {
		return command[2]
	}
	return ""
}

// ClassifyAuthAttempt classifies one finished attempt of a saved binding from its
// raw provider stdout. It returns the fixed hint and true for a recognized
// authentication failure; a custom command, a timeout or an interrupt never is one.
func ClassifyAuthAttempt(command []string, result process.Result) (string, bool) {
	id := bindingAdapter(command)
	if id == "" || result.TimedOut || result.Interrupted || result.ParentDeadline {
		return "", false
	}
	if !ClassifyAuthFailure(id, result.RawStdout) {
		return "", false
	}
	return NotAuthenticatedHint(id), true
}
