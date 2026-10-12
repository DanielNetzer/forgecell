package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	openIssuePageSize = 30
	openIssueMaxPages = 10
)

// OpenIssue is remote metadata for the Lab loop and confers no execution approval.
type OpenIssue struct {
	Repository string
	Number     int64
	Title      string
	URL        string
	CreatedAt  time.Time
	Labels     []string
	Milestone  string
}

// OpenIssues is a bounded listing. Truncated means more open Issues may exist
// past the pages that were read; the oldest Issues are always read first.
type OpenIssues struct {
	Issues    []OpenIssue
	Truncated bool
}

const (
	SelectOldest    = "oldest"
	SelectLabel     = "label"
	SelectMilestone = "milestone"
)

// Selector chooses which open Issues are eligible for the loop.
type Selector struct {
	Kind  string
	Value string
}

func ParseSelector(input string) (Selector, error) {
	if input == SelectOldest {
		return Selector{Kind: SelectOldest}, nil
	}
	kind, value, ok := strings.Cut(input, ":")
	if ok && (kind == SelectLabel || kind == SelectMilestone) {
		if strings.TrimSpace(value) == "" {
			return Selector{}, fmt.Errorf("selection %q needs a non-empty %s name", input, kind)
		}
		return Selector{Kind: kind, Value: value}, nil
	}
	return Selector{}, fmt.Errorf("unknown selection %q: use oldest, label:NAME or milestone:TITLE", input)
}

func (s Selector) String() string {
	if s.Kind == SelectLabel || s.Kind == SelectMilestone {
		return s.Kind + ":" + s.Value
	}
	return SelectOldest
}

func (s Selector) matches(issue OpenIssue) bool {
	switch s.Kind {
	case SelectLabel:
		for _, name := range issue.Labels {
			if strings.EqualFold(name, s.Value) {
				return true
			}
		}
		return false
	case SelectMilestone:
		return issue.Milestone != "" && issue.Milestone == s.Value
	}
	return true
}

// Apply keeps the Issues this selection admits, oldest first. Ties in creation
// time are broken by Issue number, so the order never depends on the remote.
func (s Selector) Apply(issues []OpenIssue) []OpenIssue {
	kept := make([]OpenIssue, 0, len(issues))
	for _, issue := range issues {
		if s.matches(issue) {
			kept = append(kept, issue)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].CreatedAt.Equal(kept[j].CreatedAt) {
			return kept[i].Number < kept[j].Number
		}
		return kept[i].CreatedAt.Before(kept[j].CreatedAt)
	})
	return kept
}

type remoteLabel struct {
	Name string `json:"name"`
}
type remoteMilestone struct {
	Title string `json:"title"`
}
type remoteOpenIssue struct {
	Number    int64            `json:"number"`
	Title     string           `json:"title"`
	State     string           `json:"state"`
	URL       string           `json:"html_url"`
	CreatedAt string           `json:"created_at"`
	PR        json.RawMessage  `json:"pull_request"`
	Labels    []remoteLabel    `json:"labels"`
	Milestone *remoteMilestone `json:"milestone"`
}

// ReadOpenIssues reads open Issues, oldest first, without a selection. See
// ReadSelectedOpenIssues.
func ReadOpenIssues(ctx context.Context, repository string) (OpenIssues, error) {
	return ReadSelectedOpenIssues(ctx, repository, Selector{})
}

// ReadSelectedOpenIssues reads open Issues, oldest first, in at most
// openIssueMaxPages bounded read-only GETs. Pull requests are skipped. A label or
// milestone selection is part of the query, so the page cap applies to the Issues
// that match it, and reaching the cap under a selection is an error: the matches
// beyond it are unknown. An Issue that moves between pages while they are read
// appears once, at its first occurrence. Any other malformed or inconsistent page
// fails the whole read rather than yielding a guess.
func ReadSelectedOpenIssues(ctx context.Context, repository string, selector Selector) (OpenIssues, error) {
	var result OpenIssues
	bound, err := BindRepository(IssueRef{Number: 1}, repository)
	if err != nil {
		return result, err
	}
	filter := ""
	switch selector.Kind {
	case SelectLabel:
		if strings.Contains(selector.Value, ",") {
			return result, fmt.Errorf("label %q cannot be selected: GitHub's labels filter treats a comma as a separator", selector.Value)
		}
		filter = "&labels=" + url.QueryEscape(selector.Value)
	case SelectMilestone:
		number, err := readMilestoneNumber(ctx, bound, selector.Value)
		if err != nil {
			return result, err
		}
		filter = fmt.Sprintf("&milestone=%d", number)
	}
	seen := map[int64]bool{}
	for page := 1; page <= openIssueMaxPages; page++ {
		endpoint := fmt.Sprintf("repos/%s/%s/issues?state=open&sort=created&direction=asc&per_page=%d%s&page=%d", bound.Owner, bound.Repo, openIssuePageSize, filter, page)
		items, err := readPage[remoteOpenIssue](ctx, endpoint, openIssuePageSize)
		if err != nil {
			return result, err
		}
		onPage := map[int64]bool{}
		for _, item := range items {
			if len(item.PR) > 0 && string(item.PR) != "null" {
				var pr map[string]json.RawMessage
				if json.Unmarshal(item.PR, &pr) != nil || pr == nil {
					return result, fmt.Errorf("GitHub data unavailable: malformed pull request metadata")
				}
				continue
			}
			issue, err := openIssue(repository, item)
			if err != nil {
				return result, err
			}
			if onPage[issue.Number] {
				return result, fmt.Errorf("GitHub data unavailable: duplicate Issue %d within one page", issue.Number)
			}
			onPage[issue.Number] = true
			if seen[issue.Number] {
				continue
			}
			seen[issue.Number] = true
			result.Issues = append(result.Issues, issue)
		}
		if len(items) < openIssuePageSize {
			return result, nil
		}
	}
	if filter != "" {
		return result, fmt.Errorf("%s matches more than %d open Issues, so the listing is incomplete and nothing was selected; narrow the selection", selector, openIssuePageSize*openIssueMaxPages)
	}
	result.Truncated = true
	return result, nil
}

const (
	milestonePageSize = 100
	milestoneMaxPages = 5
)

type remoteMilestoneNumber struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
}

// readMilestoneNumber resolves an exact, case-sensitive milestone title to its
// number with bounded read-only GETs. Closed milestones are included because an
// open Issue can still belong to one.
func readMilestoneNumber(ctx context.Context, bound IssueRef, title string) (int64, error) {
	var found []int64
	for page := 1; page <= milestoneMaxPages; page++ {
		endpoint := fmt.Sprintf("repos/%s/%s/milestones?state=all&per_page=%d&page=%d", bound.Owner, bound.Repo, milestonePageSize, page)
		items, err := readPage[remoteMilestoneNumber](ctx, endpoint, milestonePageSize)
		if err != nil {
			return 0, err
		}
		for _, item := range items {
			if item.Number < 1 || item.Title == "" {
				return 0, fmt.Errorf("GitHub data unavailable: invalid milestone metadata")
			}
			if item.Title == title {
				found = append(found, item.Number)
			}
		}
		if len(items) < milestonePageSize {
			break
		}
		if page == milestoneMaxPages && len(found) == 0 {
			return 0, fmt.Errorf("milestone %q was not found in the first %d milestones, so it cannot be selected", title, milestonePageSize*milestoneMaxPages)
		}
	}
	switch len(found) {
	case 0:
		return 0, fmt.Errorf("milestone %q does not exist in %s/%s", title, bound.Owner, bound.Repo)
	case 1:
		return found[0], nil
	}
	return 0, fmt.Errorf("milestone %q is ambiguous: GitHub returned %d milestones with that title", title, len(found))
}

// readPage makes one bounded read-only GET and decodes a JSON array of at most
// limit items.
func readPage[T any](ctx context.Context, endpoint string, limit int) ([]T, error) {
	remote := process.Run(ctx, process.Options{Argv: []string{"gh", "api", "--hostname", "github.com", "--method", "GET", endpoint}, Stdin: []byte{}, Timeout: 20 * time.Second, MaxOutputBytes: 4_000_000})
	if remote.Overflow {
		return nil, fmt.Errorf("GitHub data unavailable: response exceeds 4000000 bytes")
	}
	if !remote.OK {
		return nil, fmt.Errorf("GitHub data unavailable: %s", remote.Error)
	}
	var items []T
	if err := json.Unmarshal([]byte(remote.Stdout), &items); err != nil || items == nil || len(items) > limit {
		return nil, fmt.Errorf("GitHub data unavailable: malformed or oversized page")
	}
	return items, nil
}

func openIssue(repository string, item remoteOpenIssue) (OpenIssue, error) {
	invalid := fmt.Errorf("GitHub data unavailable: invalid Issue identity or metadata")
	ref, err := ParseIssueRef(item.URL)
	if err == nil {
		ref, err = BindRepository(ref, repository)
	}
	if err != nil || !urlPattern.MatchString(item.URL) || ref.Number != item.Number || item.Title == "" || item.State != "open" {
		return OpenIssue{}, invalid
	}
	created, err := time.Parse(time.RFC3339, item.CreatedAt)
	if err != nil {
		return OpenIssue{}, invalid
	}
	issue := OpenIssue{Repository: repository, Number: item.Number, Title: item.Title, URL: item.URL, CreatedAt: created}
	for _, label := range item.Labels {
		issue.Labels = append(issue.Labels, label.Name)
	}
	if item.Milestone != nil {
		issue.Milestone = item.Milestone.Title
	}
	return issue, nil
}
