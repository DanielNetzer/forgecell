// Package mcp is a minimal Model Context Protocol client over the stdio
// transport: newline-delimited JSON-RPC 2.0 with initialize, tools/list and
// tools/call. Every message is bounded, one request is outstanding at a time,
// and every failure maps to one fixed Reason.
package mcp

import (
	"errors"

	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// Reason is one fixed plugin failure. Callers show only the reason and the
// plugin or tool name, never server text.
type Reason string

const (
	NotInstalled     Reason = "not-installed"
	ManifestChanged  Reason = "manifest-changed"
	ContractMismatch Reason = "contract-mismatch"
	AuthMissing      Reason = "auth-missing"
	RateLimited      Reason = "rate-limited"
	Timeout          Reason = "timeout"
	OutputTooLarge   Reason = "output-too-large"
	SchemaInvalid    Reason = "schema-invalid"
	Network          Reason = "network"
	Failed           Reason = "failed"
)

// Reasons lists every fixed reason.
var Reasons = []Reason{NotInstalled, ManifestChanged, ContractMismatch, AuthMissing, RateLimited, Timeout, OutputTooLarge, SchemaInvalid, Network, Failed}

// maxDiagnosticBytes bounds the diagnostics an Error carries for the ledger.
const maxDiagnosticBytes = 16 << 10

// Error is a plugin failure. Error() is display-safe: the fixed reason and the
// tool name only. Diagnostics holds bounded, redacted detail for the ledger and
// is never display text.
type Error struct {
	Reason Reason
	// Tool is the contract tool or MCP method that failed, if any.
	Tool string
	// Remote is true when the server answered with a well-formed MCP error (a
	// JSON-RPC error response or a tools/call result with isError).
	Remote bool
	// Diagnostics is redacted, bounded detail for the ledger, never for display.
	Diagnostics string
}

func (e *Error) Error() string {
	if e.Tool != "" {
		return "plugin " + e.Tool + ": " + string(e.Reason)
	}
	return "plugin: " + string(e.Reason)
}

// Fail builds an Error with redacted, bounded diagnostics.
func Fail(reason Reason, tool, diagnostics string) *Error {
	return &Error{Reason: reason, Tool: tool, Diagnostics: Diagnose(diagnostics)}
}

// Diagnose makes detail safe to keep in a ledger: secrets are replaced and the
// text is cut to maxDiagnosticBytes with a secret straddling the cut replaced
// whole. Control and format characters are replaced so one entry stays one
// record.
func Diagnose(s string) string {
	clean := safetext.Text(safetext.CutRedacted(s, maxDiagnosticBytes), maxDiagnosticBytes)
	if len(s) > maxDiagnosticBytes {
		clean += " [truncated]"
	}
	return clean
}

// ReasonOf returns the fixed reason of err, or Failed for any other error.
func ReasonOf(err error) Reason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return Failed
}

// Known reports whether r is one of the fixed reasons.
func Known(r Reason) bool {
	for _, k := range Reasons {
		if k == r {
			return true
		}
	}
	return false
}
