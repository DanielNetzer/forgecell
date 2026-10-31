// Package tracker is the engine's view of an Issue tracker: the Go interface
// that mirrors the v1 tracker contract, and the GitHub built-in behind it.
//
// tracker may import intake, forgesync and process; it never imports molecule,
// delivery or ui.
package tracker

import (
	"context"

	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
)

// Tracker lists, reads and checks the state of tickets in one repository.
type Tracker interface {
	Describe(ctx context.Context) (Description, error)
	List(ctx context.Context, repo string, sel intake.Selector) (intake.OpenIssues, error)
	Get(ctx context.Context, repo string, number int64, dir string) (Ticket, error)
	States(ctx context.Context, repo string, numbers []int64) (map[int64]forgesync.Issue, bool, error)
}

// Description is what a tracker says about itself: the name shown to the
// operator, how its ticket keys look, and the selectors it accepts.
type Description struct {
	DisplayName string
	KeyPattern  string
	Selectors   []string
}

// Ticket is one ticket as read from the tracker. Its fields and JSON names
// mirror molecule.Issue, so decoding a tracker answer is identical to decoding
// one into an Issue.
type Ticket struct {
	UpdatedAt string `json:"updatedAt,omitempty"`
	Number    int64  `json:"number"`
	Repo      string `json:"repo"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	URL       string `json:"url"`
	State     string `json:"state"`
}
