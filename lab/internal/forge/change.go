package forge

import (
	"context"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

// Changes is the contract-shaped view of a code host's change requests: it
// mirrors the v1 forge contract, beside the GitHub-shaped Forge. Refs are
// opaque change-request references the forge returned. The branch itself is
// pushed by the engine; a Changes implementation never runs git.
type Changes interface {
	Describe(ctx context.Context) (contract.ForgeDescription, error)
	Policy(ctx context.Context, branch string) (contract.ForgePolicy, error)
	FindChange(ctx context.Context, headBranch string) (contract.ForgeChanges, error)
	// PublishDraft is the only write: callers invoke it only after the publish
	// approval.
	PublishDraft(ctx context.Context, in PublishInput) (contract.ForgePublished, error)
	Change(ctx context.Context, ref string) (contract.ForgeChange, error)
	Checks(ctx context.Context, ref, commit string) (contract.ForgeChecks, error)
	// Comments reads a change request's comments; an empty since reads them all.
	Comments(ctx context.Context, ref, since string) (contract.ForgeComments, error)
}

// PublishInput is one draft change request to open: the pushed head branch,
// the base it targets, its title and body, and the exact commit the head must
// be.
type PublishInput struct {
	HeadBranch string
	Base       string
	Title      string
	Body       string
	Commit     string
}
