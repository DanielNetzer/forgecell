package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"strings"
	"time"
)

// QueueIssue is remote metadata and confers no execution approval.
type QueueIssue struct {
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state"`
	URL        string `json:"url"`
}
type IssuePage struct {
	Issues       []QueueIssue `json:"issues"`
	Page         int          `json:"page"`
	PageSize     int          `json:"pageSize"`
	RemoteItems  int          `json:"remoteItems"`
	MorePossible bool         `json:"morePossible"`
	Selected     bool         `json:"selected"`
}

// ReadIssueQueue performs exactly one bounded authenticated GET. A full page,
// including a PR-only page, indicates only that another page may exist.
func ReadIssueQueue(ctx context.Context, repository string, page, size int, selection string) (IssuePage, error) {
	result := IssuePage{Issues: []QueueIssue{}, Page: page, PageSize: size, Selected: selection != ""}
	bound, err := BindRepository(IssueRef{Number: 1}, repository)
	if err != nil {
		return result, err
	}
	if page < 1 || size < 1 || size > 100 {
		return result, fmt.Errorf("page must be positive; page-size must be 1–100")
	}
	endpoint := fmt.Sprintf("repos/%s/%s/issues?state=open&per_page=%d&page=%d", bound.Owner, bound.Repo, size, page)
	var selected int64
	if selection != "" {
		ref, e := ParseIssueRef(selection)
		if e != nil {
			return result, e
		}
		ref, e = BindRepository(ref, repository)
		if e != nil {
			return result, e
		}
		selected = ref.Number
		endpoint = fmt.Sprintf("repos/%s/%s/issues/%d", bound.Owner, bound.Repo, selected)
	}
	remoteResult := process.Run(ctx, process.Options{Argv: []string{"gh", "api", "--hostname", "github.com", "--method", "GET", endpoint}, Stdin: []byte{}, Timeout: 20 * time.Second, MaxOutputBytes: 1_000_000})
	if remoteResult.Overflow {
		return result, fmt.Errorf("GitHub data unavailable: response exceeds 1000000 bytes")
	}
	if !remoteResult.OK {
		return result, fmt.Errorf("GitHub data unavailable: %s", remoteResult.Error)
	}
	type remote struct {
		Number int64           `json:"number"`
		Title  string          `json:"title"`
		State  string          `json:"state"`
		URL    string          `json:"html_url"`
		PR     json.RawMessage `json:"pull_request"`
	}
	var items []remote
	if selected > 0 {
		var item remote
		err = json.Unmarshal([]byte(remoteResult.Stdout), &item)
		if err == nil {
			items = []remote{item}
		}
	} else {
		err = json.Unmarshal([]byte(remoteResult.Stdout), &items)
	}
	if err != nil || items == nil || selected == 0 && len(items) > size {
		return result, fmt.Errorf("GitHub data unavailable: malformed or oversized page")
	}
	result.RemoteItems = len(items)
	result.MorePossible = selected == 0 && len(items) == size
	seen := map[int64]bool{}
	for _, item := range items {
		if len(item.PR) > 0 && string(item.PR) != "null" {
			var pr map[string]json.RawMessage
			if json.Unmarshal(item.PR, &pr) != nil || pr == nil {
				return result, fmt.Errorf("GitHub data unavailable: malformed pull request metadata")
			}
			if selected > 0 {
				return result, fmt.Errorf("GitHub selection is a pull request")
			}
			continue
		}
		ref, e := ParseIssueRef(item.URL)
		if e == nil {
			ref, e = BindRepository(ref, repository)
		}
		if !urlPattern.MatchString(item.URL) || e != nil || ref.Number != item.Number || selected > 0 && item.Number != selected || seen[item.Number] || item.Title == "" || item.State != "open" && item.State != "closed" {
			return result, fmt.Errorf("GitHub data unavailable: invalid Issue identity or metadata")
		}
		seen[item.Number] = true
		result.Issues = append(result.Issues, QueueIssue{repository, item.Number, item.Title, strings.ToUpper(item.State), item.URL})
	}
	return result, nil
}
