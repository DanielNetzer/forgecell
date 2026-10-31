package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// GitHub is the built-in GitHub Issues tracker. It wraps the existing gh-backed
// reads without changing them. Run is the process seam; nil means process.Run.
type GitHub struct {
	Run func(context.Context, process.Options) process.Result
}

var _ Tracker = GitHub{}

func (g GitHub) run() func(context.Context, process.Options) process.Result {
	if g.Run == nil {
		return process.Run
	}
	return g.Run
}

// Describe is static.
func (GitHub) Describe(context.Context) (Description, error) {
	return Description{DisplayName: "GitHub Issues", KeyPattern: "#N", Selectors: []string{intake.SelectOldest, intake.SelectLabel + ":", intake.SelectMilestone + ":"}}, nil
}

// List reads open Issues with intake.ReadSelectedOpenIssues, which runs gh
// itself and does not consult Run.
func (GitHub) List(ctx context.Context, repo string, sel intake.Selector) (intake.OpenIssues, error) {
	return intake.ReadSelectedOpenIssues(ctx, repo, sel)
}

// Get reads one Issue with `gh issue view`. On a decode error the partly
// decoded Ticket is returned with the error, as the Molecule intake always has.
func (g GitHub) Get(ctx context.Context, repo string, number int64, dir string) (Ticket, error) {
	r := g.run()(ctx, process.Options{Argv: []string{"gh", "issue", "view", fmt.Sprint(number), "--repo", repo, "--json", "number,title,body,state,url,updatedAt"}, Dir: dir, Stdin: []byte{}, Timeout: 20 * time.Second})
	if !r.OK {
		return Ticket{}, fmt.Errorf("GitHub intake failed: %s %s", r.Error, r.Stderr)
	}
	var t Ticket
	if err := json.Unmarshal([]byte(r.Stdout), &t); err != nil {
		return t, err
	}
	t.Repo = repo
	return t, nil
}

// States reads Issue state by number with forgesync's bounded GraphQL reads.
// The bool is true when more numbers were asked for than were read.
func (g GitHub) States(ctx context.Context, repo string, numbers []int64) (map[int64]forgesync.Issue, bool, error) {
	s, err := (&forgesync.GH{Run: g.run()}).States(ctx, repo, numbers, nil)
	return s.Issues, s.Truncated, err
}
