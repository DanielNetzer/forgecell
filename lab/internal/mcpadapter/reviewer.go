package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/reviewer"
)

// MaxReviewerRequestBytes bounds the JSON arguments of one reviewer_findings
// call, well inside the message bound. Comments past it are left out, the
// latest first, and counted in Result.InputDropped.
const MaxReviewerRequestBytes = 3 << 20

// PathClearedNote is the note a finding carries when its path was cleared.
const PathClearedNote = "the reviewer's path was not a clean repository-relative path, so it was cleared"

// Reviewer serves reviewer.Reviewer from a reviewer plugin. Every call carries
// {config}, the Formula reviewer's config. Findings reads reviewer_describe in
// the same session first, and hands the plugin comments only when that
// description lists forge-comments, and checks only for forge-checks.
type Reviewer struct {
	opts   Options
	plugin string
	config json.RawMessage
}

var _ reviewer.Reviewer = (*Reviewer)(nil)

// NewReviewer returns a Reviewer for the Formula reviewer ref, whose config
// must encode to a JSON object of at most MaxConfigBytes (nil is the empty
// object). opts names the plugin process as for the tracker and forge.
func NewReviewer(opts Options, ref formula.ReviewerRef) (*Reviewer, error) {
	var config any = map[string]any{}
	if ref.Config != nil {
		config = ref.Config
	}
	raw, err := json.Marshal(config)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || compact.Len() > MaxConfigBytes || compact.Bytes()[0] != '{' {
		return nil, refuse("", stepOptions, mcp.SchemaInvalid, "the reviewer config must be a JSON object of at most 4 KiB")
	}
	return &Reviewer{opts: opts, plugin: ref.Plugin, config: json.RawMessage(compact.Bytes())}, nil
}

// Plugin is the Formula's name for the reviewer plugin.
func (r *Reviewer) Plugin() string { return r.plugin }

// Describe returns the reviewer's description.
func (r *Reviewer) Describe(ctx context.Context) (reviewer.Description, error) {
	var out reviewer.Description
	err := run(ctx, r.opts, "reviewer", func(s *session) error {
		d, err := r.describe(ctx, s)
		out = description(d)
		return err
	})
	if cleanupOnly(err) {
		return out, nil
	}
	return out, err
}

// Findings reads the reviewer's findings for req.Commit of the change req.Ref.
// The arguments are exactly {forge, ref, commit, config}, plus comments and
// checks only as the description's capabilities allow. Each finding's path
// must be a clean repository-relative path; any other is cleared, with a note.
// When the call succeeded and only the plugin's cleanup failed, the findings
// are returned with a nil error; the call's record carries the failure.
func (r *Reviewer) Findings(ctx context.Context, req reviewer.Request) (reviewer.Result, error) {
	const tool = "reviewer_findings"
	forgeObject := req.Forge
	if forgeObject == nil {
		forgeObject = map[string]any{}
	}
	var out reviewer.Result
	err := run(ctx, r.opts, "reviewer", func(s *session) error {
		d, err := r.describe(ctx, s)
		if err != nil {
			return err
		}
		args := map[string]any{"forge": forgeObject, "ref": req.Ref, "commit": req.Commit, "config": r.config}
		if d.Has(contract.ReviewerForgeChecks) {
			args["checks"] = nonNil(req.Checks)
		}
		dropped := 0
		if d.Has(contract.ReviewerForgeComments) {
			comments, n, err := fitComments(args, nonNil(req.Comments))
			if err != nil {
				return refuse(s.manifest.Name, tool, mcp.SchemaInvalid, "the request exceeds its bound even without comments; nothing was sent")
			}
			args["comments"], dropped = comments, n
		} else if size, err := argsBytes(args); err != nil || size > MaxReviewerRequestBytes {
			return refuse(s.manifest.Name, tool, mcp.SchemaInvalid, "the request exceeds its bound; nothing was sent")
		}
		got, err := callAs[contract.ReviewerFindings](ctx, s, tool, args)
		if err != nil {
			return err
		}
		out = reviewer.Result{Status: got.Status, Findings: make([]reviewer.Finding, 0, len(got.Findings)), InputDropped: dropped}
		for _, f := range got.Findings {
			out.Findings = append(out.Findings, finding(f))
		}
		return nil
	})
	if cleanupOnly(err) {
		return out, nil
	}
	return out, err
}

func (r *Reviewer) describe(ctx context.Context, s *session) (contract.ReviewerDescription, error) {
	return callAs[contract.ReviewerDescription](ctx, s, "reviewer_describe", map[string]any{"config": r.config})
}

func description(d contract.ReviewerDescription) reviewer.Description {
	return reviewer.Description{DisplayName: d.DisplayName, Severities: slices.Clone(d.Severities), Capabilities: slices.Clone(d.Capabilities)}
}

func nonNil[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

func argsBytes(args map[string]any) (int, error) {
	raw, err := json.Marshal(args)
	return len(raw), err
}

// fitComments leaves out the latest comments until the arguments with the rest
// fit MaxReviewerRequestBytes, and returns the comments kept and the number
// left out. It fails only when the arguments do not fit even without comments.
func fitComments(args map[string]any, comments []forge.Comment) ([]forge.Comment, int, error) {
	args["comments"] = comments
	size, err := argsBytes(args)
	delete(args, "comments")
	if err != nil {
		return nil, 0, err
	}
	kept := len(comments)
	for size > MaxReviewerRequestBytes && kept > 0 {
		raw, err := json.Marshal(comments[kept-1])
		if err != nil {
			return nil, 0, err
		}
		size -= len(raw)
		if kept > 1 {
			size-- // the separating comma
		}
		kept--
	}
	if size > MaxReviewerRequestBytes {
		return nil, 0, errRequestBound
	}
	return comments[:kept], len(comments) - kept, nil
}

var errRequestBound = errors.New("the request exceeds its bound")

// finding converts one decoded finding, clearing a path that is not clean.
func finding(f contract.ReviewerFinding) reviewer.Finding {
	out := reviewer.Finding{ID: f.ID, Severity: f.Severity, Title: f.Title, Body: f.Body, Resolved: f.Resolved}
	if f.Line != nil {
		out.Line = *f.Line
	}
	if f.Path != nil {
		if CleanRepoPath(*f.Path) {
			out.Path = *f.Path
		} else {
			out.Note = PathClearedNote
		}
	}
	return out
}

// CleanRepoPath reports whether p is a clean relative path inside a
// repository: non-empty, slash-separated and already clean, with no ".."
// segment, no leading slash, no backslash and no control or format character.
func CleanRepoPath(p string) bool {
	if !cleanText(p, 4096) || p == "." || strings.HasPrefix(p, "/") || strings.ContainsRune(p, '\\') || path.Clean(p) != p {
		return false
	}
	return !slices.Contains(strings.Split(p, "/"), "..")
}
