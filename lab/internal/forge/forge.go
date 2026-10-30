// Package forge is the engine's view of a code host: policy, change requests and
// CI evidence. It mirrors the v1 forge contract; GitHub is the built-in.
package forge

import (
	"context"
	"errors"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// ErrUnsupported reports an operation this forge does not provide in v1.
var ErrUnsupported = errors.New("forge operation unsupported")

// Description is the static identity and wording of a forge.
type Description struct {
	Identity      string `json:"identity"`
	Host          string `json:"host"`
	DisplayName   string `json:"displayName"`
	ChangeNoun    string `json:"changeNoun"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	CIKind        string `json:"ciKind"`
	WebURL        string `json:"webURL,omitempty"`
}

// Comment is one review comment on a change request.
type Comment struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	IsBot     bool   `json:"isBot"`
	Body      string `json:"body"`
	Path      string `json:"path,omitempty"`
	Line      int    `json:"line,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// Forge is what the engine calls on a code host. FindChange and Change return the
// raw JSON observation so the caller keeps decoding and validation.
type Forge interface {
	Describe(ctx context.Context, project string) (Description, error)
	Policy(ctx context.Context, repo, branch, dir string) readiness.RemotePolicy
	FindChange(ctx context.Context, repo, headBranch, dir string) (string, error)
	PublishDraft(ctx context.Context, repo, base, head, title, bodyFile, dir string) (string, error)
	Change(ctx context.Context, repo, url, dir string) (string, error)
	Checks(ctx context.Context, o checks.Options) (checks.Snapshot, error)
	Comments(ctx context.Context, repo, ref, dir string) ([]Comment, error)
}
