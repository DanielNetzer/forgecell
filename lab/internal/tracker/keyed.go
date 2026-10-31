package tracker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

// Bounds of a Keyed call, as the v1 tracker contract sets them.
const (
	MaxListLimit = 100
	MaxStateKeys = 50
	// maxSelectorBytes is the contract's selector bound.
	maxSelectorBytes = 200
)

// GitHubKeyPattern is the anchored form of the GitHub built-in's #N keys.
const GitHubKeyPattern = `^#[1-9][0-9]{0,18}$`

// Keyed is the key- and contract-shaped view of a tracker: it mirrors the v1
// tracker contract, so the GitHub built-in and a plugin tracker can be used
// alike. Keys are opaque strings matching the tracker's keyPattern.
type Keyed interface {
	Describe(ctx context.Context) (contract.TrackerDescription, error)
	// List reads one page of the Issues a selector admits; limit is 1 to
	// MaxListLimit and an empty cursor is the first page.
	List(ctx context.Context, selector, cursor string, limit int) (contract.TrackerPage, error)
	Get(ctx context.Context, key string) (contract.TrackerIssue, error)
	// States reads the state of at most MaxStateKeys Issues; an Issue that was
	// not found is absent from the result.
	States(ctx context.Context, keys []string) (contract.TrackerStates, error)
}

// KeyedGitHub serves Keyed from the GitHub built-in for one repository: the
// key #N is Issue number N. Dir is the working directory of the gh calls.
type KeyedGitHub struct {
	GitHub GitHub
	Repo   string
	Dir    string
}

var _ Keyed = KeyedGitHub{}

// ParseGitHubKey returns N for the key #N, where N is a positive decimal number
// without a leading zero or sign.
func ParseGitHubKey(key string) (int64, error) {
	digits, ok := strings.CutPrefix(key, "#")
	if !ok || digits == "" || len(digits) > 19 || digits[0] == '0' {
		return 0, errors.New("a GitHub Issue key must be #N with N a positive number")
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, errors.New("a GitHub Issue key must be #N with N a positive number")
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n < 1 {
		return 0, errors.New("a GitHub Issue key must be #N with N a positive number")
	}
	return n, nil
}

// GitHubKey returns the key of Issue number n.
func GitHubKey(n int64) string { return "#" + strconv.FormatInt(n, 10) }

// Describe is static.
func (KeyedGitHub) Describe(context.Context) (contract.TrackerDescription, error) {
	return contract.TrackerDescription{
		DisplayName: "GitHub Issues",
		KeyPattern:  GitHubKeyPattern,
		States:      []string{"open", "closed"},
		Selectors:   []string{intake.SelectOldest, intake.SelectLabel + ":", intake.SelectMilestone + ":"},
	}, nil
}

// List reads the selected open Issues, oldest first, with
// intake.ReadSelectedOpenIssues (which runs gh itself and does not consult
// Run), and returns the page after cursor. The cursor names the last Issue of
// the previous page by its place in the listing's order (creation time, then
// number), not by an offset, so an Issue closed between page reads moves no
// other Issue past the cursor. The selector must be oldest, label:NAME or
// milestone:TITLE. GitHub's listing reads creation times only, so updatedAt
// holds the creation time. A page that reaches the end of a truncated listing
// is refused, so a bound is never mistaken for the end.
func (k KeyedGitHub) List(ctx context.Context, selector, cursor string, limit int) (contract.TrackerPage, error) {
	if limit < 1 || limit > MaxListLimit {
		return contract.TrackerPage{}, fmt.Errorf("the list limit must be between 1 and %d", MaxListLimit)
	}
	sel, err := parseGitHubSelector(selector)
	if err != nil {
		return contract.TrackerPage{}, err
	}
	var after listCursor
	if cursor != "" {
		if after, err = parseListCursor(cursor); err != nil {
			return contract.TrackerPage{}, err
		}
	}
	listing, err := k.GitHub.List(ctx, k.Repo, sel)
	if err != nil {
		return contract.TrackerPage{}, err
	}
	offset := 0
	if cursor != "" {
		for offset < len(listing.Issues) && !after.before(listing.Issues[offset]) {
			offset++
		}
	}
	end := min(offset+limit, len(listing.Issues))
	if end == len(listing.Issues) && listing.Truncated {
		return contract.TrackerPage{}, errors.New("the GitHub listing is incomplete past this page, so it cannot be read to its end")
	}
	page := contract.TrackerPage{Items: []contract.TrackerItem{}}
	for _, issue := range listing.Issues[offset:end] {
		item := contract.TrackerItem{Key: GitHubKey(issue.Number), Title: issue.Title, Labels: append([]string{}, issue.Labels...), UpdatedAt: issue.CreatedAt.UTC().Format(time.RFC3339), State: "open"}
		if issue.Milestone != "" {
			milestone := issue.Milestone
			item.Milestone = &milestone
		}
		page.Items = append(page.Items, item)
	}
	if end < len(listing.Issues) {
		next := cursorAt(listing.Issues[end-1])
		page.NextCursor = &next
	}
	if !listing.Truncated {
		total := int64(len(listing.Issues))
		page.Total = &total
	}
	return page, nil
}

// parseGitHubSelector admits exactly oldest, label:NAME and milestone:TITLE.
func parseGitHubSelector(selector string) (intake.Selector, error) {
	refuse := errors.New("the GitHub selector must be oldest, label:NAME or milestone:TITLE")
	if selector == "" || len(selector) > maxSelectorBytes {
		return intake.Selector{}, refuse
	}
	for _, r := range selector {
		if r < 0x20 || r == 0x7f {
			return intake.Selector{}, refuse
		}
	}
	sel, err := intake.ParseSelector(selector)
	if err != nil || sel.String() != selector {
		return intake.Selector{}, refuse
	}
	return sel, nil
}

// listCursor is the last Issue of a page: its number and creation time, the
// listing's sort key.
type listCursor struct {
	number  int64
	created int64 // Unix nanoseconds
}

// cursorAt writes the cursor after issue: "N.T", N its number and T its
// creation time in Unix nanoseconds.
func cursorAt(issue intake.OpenIssue) string {
	return strconv.FormatInt(issue.Number, 10) + "." + strconv.FormatInt(issue.CreatedAt.UnixNano(), 10)
}

// before reports whether issue sorts after the cursor in the listing's order,
// creation time and then number.
func (c listCursor) before(issue intake.OpenIssue) bool {
	created := issue.CreatedAt.UnixNano()
	if created != c.created {
		return created > c.created
	}
	return issue.Number > c.number
}

// parseListCursor reads a cursor KeyedGitHub.List wrote: two decimal numbers
// without a sign or leading zero, the first positive.
func parseListCursor(cursor string) (listCursor, error) {
	refuse := errors.New("the list cursor is not one this tracker wrote")
	number, created, ok := strings.Cut(cursor, ".")
	if !ok {
		return listCursor{}, refuse
	}
	n, err := parseDecimal(number)
	if err != nil || n < 1 {
		return listCursor{}, refuse
	}
	t, err := parseDecimal(created)
	if err != nil {
		return listCursor{}, refuse
	}
	return listCursor{number: n, created: t}, nil
}

// parseDecimal reads a non-negative int64 without a sign or leading zero.
func parseDecimal(s string) (int64, error) {
	if s == "" || len(s) > 19 || (len(s) > 1 && s[0] == '0') {
		return 0, strconv.ErrSyntax
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, strconv.ErrSyntax
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// Get reads Issue #N with GitHub.Get and requires the answer to be that Issue,
// open or closed.
func (k KeyedGitHub) Get(ctx context.Context, key string) (contract.TrackerIssue, error) {
	n, err := ParseGitHubKey(key)
	if err != nil {
		return contract.TrackerIssue{}, err
	}
	t, err := k.GitHub.Get(ctx, k.Repo, n, k.Dir)
	if err != nil {
		return contract.TrackerIssue{}, err
	}
	if t.Number != n {
		return contract.TrackerIssue{}, errors.New("GitHub answered with a different Issue")
	}
	state, err := githubState(t.State)
	if err != nil {
		return contract.TrackerIssue{}, err
	}
	return contract.TrackerIssue{Key: key, Title: t.Title, Body: t.Body, State: state, URL: t.URL, UpdatedAt: t.UpdatedAt, Labels: []string{}}, nil
}

// States reads the state of Issues #N with GitHub.States. A read that was
// truncated, or an Issue GitHub could not read, is refused; an Issue that was
// not found is left out.
func (k KeyedGitHub) States(ctx context.Context, keys []string) (contract.TrackerStates, error) {
	if len(keys) > MaxStateKeys {
		return contract.TrackerStates{}, fmt.Errorf("at most %d Issue keys can be read at once", MaxStateKeys)
	}
	numbers := make([]int64, 0, len(keys))
	seen := map[int64]bool{}
	for _, key := range keys {
		n, err := ParseGitHubKey(key)
		if err != nil {
			return contract.TrackerStates{}, err
		}
		if seen[n] {
			return contract.TrackerStates{}, errors.New("an Issue key is listed twice")
		}
		seen[n] = true
		numbers = append(numbers, n)
	}
	out := contract.TrackerStates{States: map[string]contract.TrackerState{}}
	if len(numbers) == 0 {
		return out, nil
	}
	issues, truncated, err := k.GitHub.States(ctx, k.Repo, numbers)
	if err != nil {
		return contract.TrackerStates{}, err
	}
	if truncated {
		return contract.TrackerStates{}, errors.New("GitHub read the state of fewer Issues than were asked for")
	}
	for n, issue := range issues {
		if !seen[n] {
			return contract.TrackerStates{}, errors.New("GitHub answered for an Issue that was not asked for")
		}
		if !issue.Found {
			if issue.Unavailable == forgesync.UnavailableNotFound {
				continue
			}
			return contract.TrackerStates{}, errors.New("GitHub could not read the state of an Issue")
		}
		state, err := githubState(issue.State)
		if err != nil {
			return contract.TrackerStates{}, err
		}
		out.States[GitHubKey(n)] = contract.TrackerState{State: state, StateReason: optional(issue.StateReason), ClosedAt: optional(issue.ClosedAt), ReopenedAt: optional(issue.ReopenedAt)}
	}
	return out, nil
}

// githubState maps GitHub's OPEN and CLOSED to the contract's open and closed.
func githubState(state string) (string, error) {
	switch state {
	case "OPEN", "open":
		return "open", nil
	case "CLOSED", "closed":
		return "closed", nil
	}
	return "", errors.New("GitHub reported an Issue state other than open or closed")
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
