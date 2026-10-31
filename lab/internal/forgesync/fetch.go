package forgesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Bounds of one sync. The open-Issue listing reads at most MaxListed Issues in
// pages of listPageSize; Issue and pull request state is read in GraphQL queries
// of at most MaxBatch numbers, for at most MaxStateNumbers numbers in all.
const (
	MaxListed       = 500
	listPageSize    = 100
	MaxBatch        = 50
	MaxStateNumbers = 1000
	callTimeout     = 20 * time.Second
	maxCallBytes    = 4_000_000
	maxTitleRunes   = 300
	maxLabels       = 20
	maxLabelRunes   = 100
	maxMilestone    = 200
	maxCursorBytes  = 200
	maxSafeInteger  = 9007199254740991
)

// Fixed failure reasons. A response never carries gh's own output, which can
// name local paths, hosts or accounts.
const (
	ReasonGHMissing        = "GitHub CLI (gh) is not installed or not on PATH; install gh to check GitHub state."
	ReasonNotAuthenticated = "gh is not signed in to github.com; run gh auth login, then Refresh."
	ReasonRateLimited      = "GitHub's API rate limit was reached; wait a few minutes, then Refresh."
	ReasonOffline          = "GitHub could not be reached; check the network connection, then Refresh."
	ReasonWrongHost        = "gh could not read this repository on github.com; check gh auth status and the active Formula's repository."
	ReasonTimeout          = "GitHub did not answer in time; Refresh to try again."
	ReasonOversized        = "GitHub's answer was larger than Forgecell reads; nothing from it is shown."
	ReasonMalformed        = "GitHub's answer was malformed or incomplete; nothing from it is shown."
	ReasonFailed           = "gh could not read GitHub state; run gh auth status, then Refresh."
	ReasonRepository       = "The active Formula names no valid GitHub repository."
)

// Plain reasons for one Issue GitHub does not return.
const (
	UnavailableNotFound = "not found or transferred"
	UnavailableNoAccess = "no access"
)

// Failure is a sync that failed closed with one fixed Reason.
type Failure struct{ Reason string }

func (f *Failure) Error() string { return f.Reason }

func fail(reason string) error { return &Failure{Reason: reason} }

// ReasonOf is the fixed reason of err; an error that is not a Failure never
// shows its own text.
func ReasonOf(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Reason
	}
	return ReasonFailed
}

// Listing is the bounded open-Issue listing, oldest first. Total is GitHub's
// count of open Issues; Truncated means more exist than were read.
type Listing struct {
	Issues    []Issue
	Total     int
	Truncated bool
}

// States are the Issues and pull requests read by number. Truncated means more
// numbers were asked for than MaxStateNumbers; the rest were not read.
type States struct {
	Issues    map[int64]Issue
	PRs       map[int64]PR
	Truncated bool
}

// Fetcher reads GitHub state; tests replace it with a fake.
type Fetcher interface {
	OpenIssues(ctx context.Context, repository string) (Listing, error)
	States(ctx context.Context, repository string, issues, prs []int64) (States, error)
}

// GH is the gh-backed Fetcher. Every call is one read-only `gh api graphql` with
// a timeout and a byte cap; Run is the process seam tests replace.
type GH struct {
	Run      func(context.Context, process.Options) process.Result
	Timeout  time.Duration
	MaxBytes int
}

// NewGH returns the Fetcher that runs gh.
func NewGH() *GH { return &GH{Run: process.Run, Timeout: callTimeout, MaxBytes: maxCallBytes} }

// issueListFields reads the latest reopen as well: row 15 needs it to fall
// after the Forgecell merge. It is aliased so the closing timeline can be read
// beside it.
const issueListFields = `number state stateReason createdAt updatedAt title locked labels(first:20){nodes{name}} milestone{title} reopened:timelineItems(last:1,itemTypes:[REOPENED_EVENT]){nodes{... on ReopenedEvent{createdAt}}}`

const issueStateFields = issueListFields + ` closedAt timelineItems(last:1,itemTypes:[CLOSED_EVENT]){nodes{... on ClosedEvent{closer{__typename ... on PullRequest{number} ... on Commit{oid}}}}}`

const prFields = `number state isDraft mergedAt mergeCommit{oid} headRefOid headRef{name} reviewDecision commits(last:1){nodes{commit{statusCheckRollup{state}}}}`

const listQuery = `query($owner:String!,$name:String!,$after:String){repository(owner:$owner,name:$name){issues(states:OPEN,first:100,after:$after,orderBy:{field:CREATED_AT,direction:ASC}){totalCount pageInfo{hasNextPage endCursor} nodes{` + issueListFields + `}}}}`

type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

type gqlResponse struct {
	Data *struct {
		Repository json.RawMessage `json:"repository"`
	} `json:"data"`
	Errors []gqlError `json:"errors"`
}

type remoteIssue struct {
	Number      int64   `json:"number"`
	State       string  `json:"state"`
	StateReason *string `json:"stateReason"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
	ClosedAt    *string `json:"closedAt"`
	Title       string  `json:"title"`
	Locked      bool    `json:"locked"`
	Labels      *struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Milestone *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	Timeline *struct {
		Nodes []struct {
			Closer *struct {
				Type   string `json:"__typename"`
				Number int64  `json:"number"`
				OID    string `json:"oid"`
			} `json:"closer"`
		} `json:"nodes"`
	} `json:"timelineItems"`
	Reopened *struct {
		Nodes []struct {
			CreatedAt *string `json:"createdAt"`
		} `json:"nodes"`
	} `json:"reopened"`
}

type remotePR struct {
	Number      int64   `json:"number"`
	State       string  `json:"state"`
	IsDraft     bool    `json:"isDraft"`
	MergedAt    *string `json:"mergedAt"`
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	HeadRefOid string `json:"headRefOid"`
	HeadRef    *struct {
		Name string `json:"name"`
	} `json:"headRef"`
	ReviewDecision *string `json:"reviewDecision"`
	Commits        *struct {
		Nodes []struct {
			Commit struct {
				Rollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// bind validates the repository; its parts are passed to gh only as argv.
func bind(repository string) (owner, name string, err error) {
	ref, e := intake.BindRepository(intake.IssueRef{Number: 1}, repository)
	if e != nil {
		return "", "", fail(ReasonRepository)
	}
	return ref.Owner, ref.Repo, nil
}

// OpenIssues reads open Issues oldest first, at most MaxListed of them.
func (g *GH) OpenIssues(ctx context.Context, repository string) (Listing, error) {
	owner, name, err := bind(repository)
	if err != nil {
		return Listing{}, err
	}
	base := "https://github.com/" + owner + "/" + name
	var out Listing
	seen := map[int64]bool{}
	after := ""
	for page := 0; page < MaxListed/listPageSize; page++ {
		vars := map[string]string{"owner": owner, "name": name}
		if after != "" {
			vars["after"] = after
		}
		resp, err := g.query(ctx, listQuery, vars)
		if err != nil {
			return Listing{}, err
		}
		var repo *struct {
			Issues *struct {
				TotalCount int `json:"totalCount"`
				PageInfo   struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []json.RawMessage `json:"nodes"`
			} `json:"issues"`
		}
		if json.Unmarshal(resp.Data.Repository, &repo) != nil || repo == nil || repo.Issues == nil || len(repo.Issues.Nodes) > listPageSize || repo.Issues.TotalCount < 0 {
			return Listing{}, fail(ReasonMalformed)
		}
		out.Total = repo.Issues.TotalCount
		for _, raw := range repo.Issues.Nodes {
			var ri remoteIssue
			if json.Unmarshal(raw, &ri) != nil {
				return Listing{}, fail(ReasonMalformed)
			}
			issue, ok := issueFrom(base, ri)
			if !ok || issue.State != IssueOpen {
				return Listing{}, fail(ReasonMalformed)
			}
			// An Issue that moves between pages while they are read appears once.
			if seen[issue.Number] {
				continue
			}
			seen[issue.Number] = true
			out.Issues = append(out.Issues, issue)
		}
		info := repo.Issues.PageInfo
		if !info.HasNextPage {
			out.Truncated = out.Total > len(out.Issues)
			return out, nil
		}
		if info.EndCursor == "" || len(info.EndCursor) > maxCursorBytes || !cursorPattern.MatchString(info.EndCursor) || info.EndCursor == after {
			return Listing{}, fail(ReasonMalformed)
		}
		after = info.EndCursor
	}
	out.Truncated = true
	return out, nil
}

var cursorPattern = regexp.MustCompile(`^[A-Za-z0-9+/=_:-]+$`)

type ref struct {
	pr     bool
	number int64
}

func (r ref) alias() string {
	if r.pr {
		return fmt.Sprintf("p%d", r.number)
	}
	return fmt.Sprintf("i%d", r.number)
}

// States reads the given Issues and pull requests by number in queries of at
// most MaxBatch numbers. Invalid numbers are dropped; past MaxStateNumbers the
// newest numbers are read and Truncated is set.
func (g *GH) States(ctx context.Context, repository string, issues, prs []int64) (States, error) {
	owner, name, err := bind(repository)
	if err != nil {
		return States{}, err
	}
	base := "https://github.com/" + owner + "/" + name
	refs, truncated := wanted(issues, prs)
	out := States{Issues: map[int64]Issue{}, PRs: map[int64]PR{}, Truncated: truncated}
	for start := 0; start < len(refs); start += MaxBatch {
		batch := refs[start:min(start+MaxBatch, len(refs))]
		var q strings.Builder
		q.WriteString(`query($owner:String!,$name:String!){repository(owner:$owner,name:$name){`)
		for _, r := range batch {
			// Numbers are validated integers; nothing else is written into the query.
			if r.pr {
				fmt.Fprintf(&q, "%s:pullRequest(number:%d){%s} ", r.alias(), r.number, prFields)
			} else {
				fmt.Fprintf(&q, "%s:issue(number:%d){%s} ", r.alias(), r.number, issueStateFields)
			}
		}
		q.WriteString(`}}`)
		resp, err := g.query(ctx, q.String(), map[string]string{"owner": owner, "name": name})
		if err != nil {
			return States{}, err
		}
		var nodes map[string]json.RawMessage
		if json.Unmarshal(resp.Data.Repository, &nodes) != nil || nodes == nil {
			return States{}, fail(ReasonMalformed)
		}
		missing := missingReasons(resp.Errors)
		for _, r := range batch {
			raw, ok := nodes[r.alias()]
			if !ok {
				return States{}, fail(ReasonMalformed)
			}
			null := string(raw) == "null"
			switch {
			case r.pr && null:
				out.PRs[r.number] = PR{Number: r.number}
			case r.pr:
				var rp remotePR
				if json.Unmarshal(raw, &rp) != nil {
					return States{}, fail(ReasonMalformed)
				}
				p, ok := prFrom(base, rp)
				if !ok || p.Number != r.number {
					return States{}, fail(ReasonMalformed)
				}
				out.PRs[r.number] = p
			case null:
				reason := missing[r.alias()]
				if reason == "" {
					reason = UnavailableNotFound
				}
				out.Issues[r.number] = Issue{Number: r.number, Labels: []string{}, Unavailable: reason}
			default:
				var ri remoteIssue
				if json.Unmarshal(raw, &ri) != nil {
					return States{}, fail(ReasonMalformed)
				}
				issue, ok := issueFrom(base, ri)
				if !ok || issue.Number != r.number {
					return States{}, fail(ReasonMalformed)
				}
				out.Issues[r.number] = issue
			}
		}
	}
	return out, nil
}

// wanted is the deduplicated, valid numbers to read, newest first, capped at
// MaxStateNumbers.
func wanted(issues, prs []int64) ([]ref, bool) {
	var refs []ref
	seen := map[ref]bool{}
	add := func(pr bool, ns []int64) {
		for _, n := range ns {
			r := ref{pr, n}
			if n < 1 || n > maxSafeInteger || seen[r] {
				continue
			}
			seen[r] = true
			refs = append(refs, r)
		}
	}
	add(false, issues)
	add(true, prs)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].number != refs[j].number {
			return refs[i].number > refs[j].number
		}
		return !refs[i].pr && refs[j].pr
	})
	if len(refs) > MaxStateNumbers {
		return refs[:MaxStateNumbers], true
	}
	return refs, false
}

// missingReasons maps the aliases GitHub reports as not found or forbidden to a
// plain reason.
func missingReasons(errs []gqlError) map[string]string {
	out := map[string]string{}
	for _, e := range errs {
		if len(e.Path) < 2 {
			continue
		}
		alias, ok := e.Path[1].(string)
		if !ok {
			continue
		}
		switch e.Type {
		case "NOT_FOUND":
			out[alias] = UnavailableNotFound
		case "FORBIDDEN":
			out[alias] = UnavailableNoAccess
		}
	}
	return out
}

// query runs one bounded read-only GraphQL query. GitHub reports a missing Issue
// or pull request as a null node with a NOT_FOUND or FORBIDDEN error, and gh
// then exits non-zero; such an answer is kept when every error is of those
// kinds. Any other failure maps to one fixed reason.
func (g *GH) query(ctx context.Context, q string, vars map[string]string) (gqlResponse, error) {
	argv := []string{"gh", "api", "graphql", "--hostname", "github.com", "-f", "query=" + q}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "-f", k+"="+vars[k])
	}
	timeout, limit := g.Timeout, g.MaxBytes
	if timeout <= 0 {
		timeout = callTimeout
	}
	if limit <= 0 {
		limit = maxCallBytes
	}
	res := g.Run(ctx, process.Options{Argv: argv, Stdin: []byte{}, Timeout: timeout, MaxOutputBytes: limit})
	switch {
	case res.Overflow:
		return gqlResponse{}, fail(ReasonOversized)
	case res.TimedOut || res.ParentDeadline || res.Interrupted:
		return gqlResponse{}, fail(ReasonTimeout)
	}
	var resp gqlResponse
	parsed := json.Unmarshal([]byte(res.Stdout), &resp) == nil && resp.Data != nil
	if parsed && len(resp.Data.Repository) > 0 && string(resp.Data.Repository) != "null" && onlyMissing(resp.Errors) {
		return resp, nil
	}
	if parsed || res.OK {
		for _, e := range resp.Errors {
			if e.Type == "RATE_LIMITED" {
				return gqlResponse{}, fail(ReasonRateLimited)
			}
		}
		if parsed && (len(resp.Data.Repository) == 0 || string(resp.Data.Repository) == "null") {
			return gqlResponse{}, fail(ReasonWrongHost)
		}
		if res.OK {
			return gqlResponse{}, fail(ReasonMalformed)
		}
	}
	return gqlResponse{}, fail(classify(res))
}

func onlyMissing(errs []gqlError) bool {
	for _, e := range errs {
		if e.Type != "NOT_FOUND" && e.Type != "FORBIDDEN" || len(e.Path) < 2 {
			return false
		}
	}
	return true
}

// classify maps a failed gh run to one fixed reason from its stderr and error
// text, which are read here and never returned.
func classify(res process.Result) string {
	text := strings.ToLower(res.Stderr + "\n" + res.Error)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(text, w) {
				return true
			}
		}
		return false
	}
	switch {
	case res.Code == -1 && has("executable file not found", "no such file or directory"):
		return ReasonGHMissing
	case has("rate limit", "rate_limited", "abuse detection"):
		return ReasonRateLimited
	case has("gh auth login", "not logged in", "authentication required", "requires authentication", "bad credentials", "http 401", "auth status"):
		return ReasonNotAuthenticated
	case has("could not resolve host", "no such host", "dial tcp", "connection refused", "network is unreachable", "i/o timeout", "tls handshake", "connection reset", "timeout awaiting"):
		return ReasonOffline
	case has("could not resolve to a repository", "http 404", "not a known github host", "none of the git remotes", "unknown host", "enterprise"):
		return ReasonWrongHost
	}
	return ReasonFailed
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func validNumber(n int64) bool { return n >= 1 && n <= maxSafeInteger }

func commit(s string) string {
	if commitPattern.MatchString(s) {
		return s
	}
	return ""
}

// stamp is a valid RFC 3339 time in UTC, or "".
func stamp(s *string) string {
	if s == nil {
		return ""
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// text bounds untrusted display text: valid UTF-8, no control characters, no
// format characters (unicode.Cf, such as U+202E bidi overrides or zero-width
// joiners, which can disguise text), at most limit runes.
// The rules live in safetext so plugin output decoders share them.
func text(s string, limit int) string { return safetext.Text(s, limit) }

func oneOf(s string, values ...string) string {
	for _, v := range values {
		if s == v {
			return s
		}
	}
	return ""
}

// issueFrom validates one remote Issue. Its URLs are built from the validated
// number and the repository's fixed base, never taken from GitHub.
func issueFrom(base string, ri remoteIssue) (Issue, bool) {
	state := oneOf(ri.State, IssueOpen, IssueClosed)
	if !validNumber(ri.Number) || state == "" {
		return Issue{}, false
	}
	issue := Issue{Number: ri.Number, Found: true, State: state, StateReason: oneOf(str(ri.StateReason), ReasonCompleted, ReasonNotPlanned, ReasonReopened), Title: text(ri.Title, maxTitleRunes), Labels: []string{}, CreatedAt: stamp(&ri.CreatedAt), UpdatedAt: stamp(&ri.UpdatedAt), Locked: ri.Locked, URL: fmt.Sprintf("%s/issues/%d", base, ri.Number)}
	if ri.Labels != nil {
		for _, l := range ri.Labels.Nodes {
			if len(issue.Labels) == maxLabels {
				break
			}
			if name := text(l.Name, maxLabelRunes); name != "" {
				issue.Labels = append(issue.Labels, name)
			}
		}
	}
	if ri.Milestone != nil {
		issue.Milestone = text(ri.Milestone.Title, maxMilestone)
	}
	if ri.Reopened != nil && len(ri.Reopened.Nodes) > 0 {
		issue.ReopenedAt = stamp(ri.Reopened.Nodes[len(ri.Reopened.Nodes)-1].CreatedAt)
	}
	if state == IssueClosed {
		issue.ClosedAt = stamp(ri.ClosedAt)
		if ri.Timeline != nil && len(ri.Timeline.Nodes) > 0 {
			if c := ri.Timeline.Nodes[len(ri.Timeline.Nodes)-1].Closer; c != nil {
				switch {
				case c.Type == "PullRequest" && validNumber(c.Number):
					issue.ClosedBy = &Link{Kind: "pull", Number: c.Number, URL: fmt.Sprintf("%s/pull/%d", base, c.Number)}
				case c.Type == "Commit" && commit(c.OID) != "":
					issue.ClosedBy = &Link{Kind: "commit", Commit: c.OID, URL: base + "/commit/" + c.OID}
				}
			}
		}
	}
	return issue, true
}

func prFrom(base string, rp remotePR) (PR, bool) {
	state := oneOf(rp.State, PROpen, PRClosed, PRMerged)
	if !validNumber(rp.Number) || state == "" {
		return PR{}, false
	}
	p := PR{Number: rp.Number, Found: true, State: state, IsDraft: rp.IsDraft, HeadRefOid: commit(rp.HeadRefOid), HeadDeleted: rp.HeadRef == nil, ReviewDecision: oneOf(str(rp.ReviewDecision), "APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED"), URL: fmt.Sprintf("%s/pull/%d", base, rp.Number)}
	if state == PRMerged {
		p.MergedAt = stamp(rp.MergedAt)
		if rp.MergeCommit != nil {
			p.MergeCommit = commit(rp.MergeCommit.OID)
		}
	}
	if rp.Commits != nil && len(rp.Commits.Nodes) > 0 {
		if r := rp.Commits.Nodes[len(rp.Commits.Nodes)-1].Commit.Rollup; r != nil {
			p.Checks = oneOf(r.State, "SUCCESS", "FAILURE", "ERROR", "PENDING", "EXPECTED")
		}
	}
	return p, true
}
