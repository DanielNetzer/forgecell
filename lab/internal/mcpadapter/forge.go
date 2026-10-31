package mcpadapter

import (
	"context"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// maxProjectRunes bounds the forge project, as the contract does.
const maxProjectRunes = 200

// Forge serves forge.Changes from a forge plugin. Every call carries {project},
// the Formula's forge.project. It only creates and reads change requests: it
// never runs git, and the engine pushes the branch itself.
type Forge struct {
	opts    Options
	project string
}

var _ forge.Changes = (*Forge)(nil)

// NewForge returns a Forge for project, which must be non-empty text of at most
// 200 characters with no control or format characters.
func NewForge(opts Options, project string) (*Forge, error) {
	if !cleanText(project, maxProjectRunes) {
		return nil, refuse("", stepOptions, mcp.SchemaInvalid, "the forge project is empty, too long or holds control or format characters")
	}
	return &Forge{opts: opts, project: project}, nil
}

// Describe returns the forge's identity and wording.
func (f *Forge) Describe(ctx context.Context) (contract.ForgeDescription, error) {
	return forgeCall[contract.ForgeDescription](ctx, f, "forge_describe", nil)
}

// Policy returns the protection policy of branch.
func (f *Forge) Policy(ctx context.Context, branch string) (contract.ForgePolicy, error) {
	return forgeCall[contract.ForgePolicy](ctx, f, "forge_policy", map[string]any{"branch": branch})
}

// FindChange returns the change requests whose head is headBranch.
func (f *Forge) FindChange(ctx context.Context, headBranch string) (contract.ForgeChanges, error) {
	return forgeCall[contract.ForgeChanges](ctx, f, "forge_find_change", map[string]any{"headBranch": headBranch})
}

// PublishDraft opens a draft change request; it is the only write tool. The
// adapter has no gate of its own: callers may invoke it only after the publish
// approval, with the approved head, base, title, body and commit.
//
// A change the plugin reports as published is never dropped. When its head is
// not in.Commit, PublishDraft returns the decoded answer, so the caller can
// find the change by its ref and url, together with a schema-invalid refusal
// that the call's record also carries. When the call succeeded and only the
// plugin's cleanup failed, PublishDraft returns the answer with a nil error;
// the failure is in the call's record (Cleanup and CleanupDiagnostics).
func (f *Forge) PublishDraft(ctx context.Context, in forge.PublishInput) (contract.ForgePublished, error) {
	const tool = "forge_publish_draft"
	args := f.args(map[string]any{"headBranch": in.HeadBranch, "base": in.Base, "title": in.Title, "body": in.Body, "commit": in.Commit})
	var out contract.ForgePublished
	err := run(ctx, f.opts, "forge", func(s *session) error {
		published, err := callAs[contract.ForgePublished](ctx, s, tool, args)
		if err != nil {
			return err
		}
		out = published
		if published.HeadCommit != in.Commit {
			return s.refuseAnswer(tool, mcp.SchemaInvalid, "the published change's head is not the approved commit")
		}
		return nil
	})
	if cleanupOnly(err) {
		return out, nil
	}
	return out, err
}

// Change returns the state of one change request.
func (f *Forge) Change(ctx context.Context, ref string) (contract.ForgeChange, error) {
	return forgeCall[contract.ForgeChange](ctx, f, "forge_change", map[string]any{"ref": ref})
}

// Checks returns the CI checks on commit of a change request.
func (f *Forge) Checks(ctx context.Context, ref, commit string) (contract.ForgeChecks, error) {
	return forgeCall[contract.ForgeChecks](ctx, f, "forge_checks", map[string]any{"ref": ref, "commit": commit})
}

// Comments returns a change request's comments, those after since when it is
// not empty.
func (f *Forge) Comments(ctx context.Context, ref, since string) (contract.ForgeComments, error) {
	args := map[string]any{"ref": ref}
	if since != "" {
		args["since"] = since
	}
	return forgeCall[contract.ForgeComments](ctx, f, "forge_comments", args)
}

// args adds {project} to a tool's own arguments.
func (f *Forge) args(own map[string]any) map[string]any {
	full := map[string]any{"project": f.project}
	for k, v := range own {
		full[k] = v
	}
	return full
}

// forgeCall runs one operation of a single call. A cleanup failure after a
// successful call returns the output with the shutdown error.
func forgeCall[T any](ctx context.Context, f *Forge, tool string, own map[string]any) (T, error) {
	args := f.args(own)
	var out T
	err := run(ctx, f.opts, "forge", func(s *session) error {
		v, err := callAs[T](ctx, s, tool, args)
		out = v
		return err
	})
	return out, err
}
