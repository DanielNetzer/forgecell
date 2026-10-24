package harness

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

const TransientProviderError = "transient-provider-error"

// transientNetwork matches the connection, DNS and timeout shapes the Claude CLI and the
// Anthropic SDK report when the request never got a response, and an overloaded provider.
var transientNetwork = regexp.MustCompile(`(?i)\b(?:ENOTFOUND|ECONNRESET|ECONNREFUSED|ETIMEDOUT|EAI_AGAIN)\b|socket hang up|overloaded|connection error\.|request timed out|can(?:'|’|no)?t reach the api server|went to sleep mid-response`)

// transientStatus and clientStatus read only the status the error itself reports in its
// "API Error: NNN" prefix, never a number elsewhere in the provider message.
var (
	transientStatus = regexp.MustCompile(`(?i)^API Error:\s*(?:502|503|504|529)\b`)
	clientStatus    = regexp.MustCompile(`(?i)^API Error:\s*4\d\d\b`)
)

// deterministicType names provider error types that a resume would only repeat.
var deterministicType = regexp.MustCompile(`invalid_request_error|authentication_error|permission_error`)

var transientStatusFields = []string{"api_error_status", "status", "status_code"}

func transientCode(status int) bool {
	return status == 502 || status == 503 || status == 504 || status == 529
}

// TransientProviderFailure reports whether a Claude CLI final JSON is an api_error the
// provider would likely not repeat: a 502, 503, 504 or 529 status of the error itself, an
// overloaded provider, or a connection, DNS or timeout failure. A 4xx status, or an
// invalid request, authentication or permission error, is never transient, whatever
// else its message contains.
func TransientProviderFailure(raw string) (string, bool) {
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &value) != nil || value == nil {
		return "", false
	}
	var reason string
	if json.Unmarshal(value["terminal_reason"], &reason) != nil || reason != "api_error" {
		return "", false
	}
	var e providerError
	for _, key := range []string{"result", "error", "message", "api_error"} {
		e.collect(value[key])
	}
	e.statuses(value)
	message := strings.Join(e.texts, "; ")
	if deterministicType.MatchString(message) {
		return "", false
	}
	transient := false
	for _, status := range e.codes {
		if status >= 400 && status < 500 {
			return "", false
		}
		transient = transient || transientCode(status)
	}
	for _, text := range e.texts {
		if clientStatus.MatchString(text) {
			return "", false
		}
		transient = transient || transientStatus.MatchString(text)
	}
	if transient || transientNetwork.MatchString(message) {
		return boundedText(redactSecrets(message), 300), true
	}
	return "", false
}

// providerError gathers the message texts and structured statuses of an api_error.
type providerError struct {
	texts []string
	codes []int
}

func (e *providerError) collect(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s = strings.TrimSpace(s); s != "" {
			e.texts = append(e.texts, s)
		}
		return
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return
	}
	for _, key := range []string{"message", "type", "code", "error"} {
		e.collect(object[key])
	}
	e.statuses(object)
}

func (e *providerError) statuses(object map[string]json.RawMessage) {
	for _, key := range transientStatusFields {
		var status int
		if json.Unmarshal(object[key], &status) == nil && status > 0 {
			e.codes = append(e.codes, status)
			e.texts = append(e.texts, "HTTP status "+string(object[key]))
		}
	}
}

func ClassifyCodingAttempt(command []string, result process.Result) string {
	if !ClaudeBinding(command) || result.TimedOut || result.Interrupted || result.ParentDeadline {
		return ""
	}
	if _, ok := TransientProviderFailure(result.RawStdout); ok {
		return TransientProviderError
	}
	return ""
}
