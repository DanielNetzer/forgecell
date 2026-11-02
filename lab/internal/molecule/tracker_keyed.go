package molecule

import (
	"context"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/tracker"
)

// TrackerKeyPattern reads the tracker's keyPattern with Describe and returns it
// anchored with ^ and $, as intake.ParseIssueKey wants it. The adapter always
// matches keyPattern whole, so anchoring an unanchored pattern changes nothing.
func TrackerKeyPattern(ctx context.Context, t tracker.Keyed) (string, error) {
	d, err := t.Describe(ctx)
	if err != nil {
		return "", err
	}
	pattern := d.KeyPattern
	if !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") {
		pattern = "^(?:" + pattern + ")$"
	}
	return pattern, nil
}

// KeyedReader is an Options.ReadIssueKey that reads the Issue with the
// tracker's Get. The answer becomes a keyed Issue (Number 0) in the requested
// repository, its open and closed states mapped to OPEN and CLOSED; any other
// state is left empty, so RunKeyed refuses it. A failed Get keeps its bounded,
// redacted diagnostics on calls, which may be nil.
func KeyedReader(t tracker.Keyed, calls *PluginCallLog) func(ctx context.Context, repo, key, dir string) (Issue, error) {
	return func(ctx context.Context, repo, key, _ string) (Issue, error) {
		issue, err := t.Get(ctx, key)
		if err != nil {
			if calls != nil {
				calls.Fail(err)
			}
			return Issue{}, err
		}
		state := map[string]string{"open": "OPEN", "closed": "CLOSED"}[issue.State]
		return Issue{Key: issue.Key, Number: 0, Repo: repo, Title: issue.Title, Body: issue.Body, State: state, URL: issue.URL, UpdatedAt: issue.UpdatedAt}, nil
	}
}
