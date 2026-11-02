// Package reviewer is the engine's view of an outside review agent: it mirrors
// the v1 reviewer contract (design §4.3). Findings are untrusted evidence
// against one pushed commit: they never approve, push or change a receipt.
package reviewer

import (
	"context"
	"slices"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
)

// The capabilities a reviewer may describe. The engine hands a reviewer the
// change request's comments only when it lists CapabilityComments, and its
// checks only when it lists CapabilityChecks.
const (
	CapabilityComments = "forge-comments"
	CapabilityChecks   = "forge-checks"
)

// Statuses of a findings answer: pending means the reviewer has not finished
// with the commit.
const (
	StatusPending  = "pending"
	StatusComplete = "complete"
)

// Severities, most severe first.
var Severities = []string{"high", "medium", "low", "info"}

// Description is the output of reviewer_describe.
type Description struct {
	DisplayName  string   `json:"displayName"`
	Severities   []string `json:"severities"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Has reports whether the description lists capability exactly.
func (d Description) Has(capability string) bool {
	return slices.Contains(d.Capabilities, capability)
}

// Check is one CI check on the commit, in the forge_checks item shape: status
// is queued, in_progress or completed, and conclusion is empty until it
// completes.
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url,omitempty"`
}

// Request is one reviewer_findings call: the change request's ref, the exact
// pushed commit, a JSON object describing the forge, and the comments and
// checks the engine read for the reviewer. Comments and Checks reach the
// reviewer only when its description lists the matching capability.
type Request struct {
	Ref      string
	Commit   string
	Forge    map[string]any
	Comments []forge.Comment
	Checks   []Check
}

// Finding is one decoded, sanitized finding. ID is the plugin's own id, kept
// for display only. Path is a clean repository-relative path or empty; Note
// says why the plugin's path was cleared, when it was. Line is 0 when the
// plugin named none.
type Finding struct {
	ID       string
	Severity string
	Path     string
	Line     int64
	Title    string
	Body     string
	Resolved bool
	Note     string
}

// Result is the output of reviewer_findings. InputDropped counts the comments
// the engine left out of the request to keep it within the message bound, so a
// reviewer that saw only part of its input is never taken as complete.
type Result struct {
	Status       string
	Findings     []Finding
	InputDropped int
}

// Reviewer is one reviewer plugin.
type Reviewer interface {
	Describe(ctx context.Context) (Description, error)
	Findings(ctx context.Context, req Request) (Result, error)
}
