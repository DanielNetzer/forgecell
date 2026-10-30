package forge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// PRFields is the gh --json field list for a pull request observation.
const PRFields = "url,state,baseRefName,headRefName,headRefOid,isDraft,headRepository,headRepositoryOwner"

const (
	ghTimeout  = 60 * time.Second
	ghMaxBytes = 1_000_000
)

// GitHub is the built-in forge. Run is the process seam; nil means process.Run.
type GitHub struct {
	Run func(context.Context, process.Options) process.Result
}

var _ Forge = GitHub{}

func (g GitHub) runner() func(context.Context, process.Options) process.Result {
	if g.Run == nil {
		return process.Run
	}
	return g.Run
}

// gh runs one bounded gh call; any uncertainty is a failure, never a partial answer.
func (g GitHub) gh(ctx context.Context, dir string, argv ...string) (string, error) {
	p := g.runner()(ctx, process.Options{Argv: argv, Dir: dir, Stdin: []byte{}, Timeout: ghTimeout, MaxOutputBytes: ghMaxBytes})
	if !p.OK || p.TimedOut || p.Interrupted || p.Overflow || len(p.Stdout)+len(p.Stderr) > ghMaxBytes {
		return "", fmt.Errorf("%s failed or uncertain: %s %s", argv[0], p.Error, p.Stderr)
	}
	return strings.TrimSpace(p.Stdout), nil
}

func (g GitHub) Describe(_ context.Context, project string) (Description, error) {
	return Description{Identity: project, Host: "github.com", DisplayName: "GitHub", ChangeNoun: "pull request", CIKind: "github-actions"}, nil
}

func (g GitHub) Policy(ctx context.Context, repo, branch, dir string) readiness.RemotePolicy {
	return readiness.ReadRemotePolicy(ctx, repo, branch, dir, g.Run)
}

func (g GitHub) FindChange(ctx context.Context, repo, headBranch, dir string) (string, error) {
	return g.gh(ctx, dir, "gh", "pr", "list", "--repo", repo, "--head", headBranch, "--state", "all", "--limit", "2", "--json", PRFields)
}

func (g GitHub) PublishDraft(ctx context.Context, repo, base, head, title, bodyFile, dir string) (string, error) {
	return g.gh(ctx, dir, "gh", "pr", "create", "--draft", "--repo", repo, "--base", base, "--head", head, "--title", title, "--body-file", bodyFile)
}

func (g GitHub) Change(ctx context.Context, repo, url, dir string) (string, error) {
	return g.gh(ctx, dir, "gh", "pr", "view", url, "--repo", repo, "--json", PRFields)
}

func (g GitHub) Checks(ctx context.Context, o checks.Options) (checks.Snapshot, error) {
	if o.Runner == nil {
		o.Runner = g.Run
	}
	return checks.Collect(ctx, o)
}

func (g GitHub) Comments(context.Context, string, string, string) ([]Comment, error) {
	return nil, ErrUnsupported
}
