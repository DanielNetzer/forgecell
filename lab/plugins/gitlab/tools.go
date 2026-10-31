package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/plugins/internal/mcpserver"
)

const (
	// maxTitleRunes is GitLab's merge request title limit.
	maxTitleRunes = 255
	// maxChanges and maxChecks are the forge_find_change and forge_checks
	// output bounds; one check is the pipeline aggregate.
	maxChanges = 20
	maxChecks  = 200
	// maxNameRunes, maxAuthorRunes, maxBodyRunes and maxPathRunes are the
	// contract bounds of a check name and of a comment's author, body and
	// path.
	maxNameRunes   = 200
	maxAuthorRunes = 100
	maxBodyRunes   = 65536
	maxPathRunes   = 4096
	maxLine        = 10_000_000
	maxReviews     = 100
	// pipelineCheck names the aggregate check and the required check a
	// pipeline-gated project reports.
	pipelineCheck = "pipeline"
	truncatedMark = "\n[truncated]"
	// lineCutMark marks a cut single-line value, such as a name or an author.
	lineCutMark = "…"
)

var (
	refPattern     = regexp.MustCompile(`^![1-9][0-9]{0,9}$`)
	numericProject = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	pathProject    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)+$`)
	commitPattern  = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	// draftPrefix matches the title prefixes GitLab already reads as a draft.
	draftPrefix = regexp.MustCompile(`(?i)^\s*(draft:|\[draft\]|\(draft\))`)
	// botUser matches GitLab's project and group access token bot accounts.
	botUser = regexp.MustCompile(`^(project|group)_[0-9]+_bot(_[0-9a-f]+)?$`)
)

// forge serves the forge contract tools against one GitLab. configErr, when
// set, fails every call before any request.
type forge struct {
	c         *client
	configErr error
}

func newForge(lookup func(string) (string, bool)) *forge {
	cfg, err := loadConfig(lookup)
	if err != nil {
		return &forge{configErr: err}
	}
	return &forge{c: newClient(cfg)}
}

// bind returns the handler for one contract tool: it refuses arguments the
// contract input schema refuses, fails while the configuration is invalid and
// bounds the call by the tool's contract timeout. A contract tool with no
// handler is a programming error.
func (f *forge) bind(t *contract.Tool) mcpserver.Handler {
	handlers := map[string]func(context.Context, json.RawMessage) (any, error){
		"forge_describe":      f.describe,
		"forge_policy":        f.policy,
		"forge_find_change":   f.findChange,
		"forge_publish_draft": f.publishDraft,
		"forge_change":        f.change,
		"forge_checks":        f.checks,
		"forge_comments":      f.comments,
	}
	h := handlers[t.Name]
	if h == nil {
		panic("gitlab: no handler for " + t.Name)
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		if err := t.ValidateInputJSON(args); err != nil {
			return nil, mcpserver.Fail(mcpserver.Failed, "the arguments do not match the input schema")
		}
		if f.configErr != nil {
			return nil, mcpserver.Fail(mcpserver.Failed, f.configErr.Error())
		}
		ctx, cancel := context.WithTimeout(ctx, t.Timeout())
		defer cancel()
		return h(ctx, args)
	}
}

// decodeArgs decodes schema-checked arguments into the tool's input struct.
func decodeArgs(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return mcpserver.Fail(mcpserver.Failed, "the arguments do not match the input schema")
	}
	return nil
}

// projectPath returns the escaped /projects/:id prefix for a numeric id or a
// namespace/project path. Every path segment starts with a letter, digit or
// underscore, so . and .. never reach GitLab.
func projectPath(project string) (string, error) {
	if !numericProject.MatchString(project) && !pathProject.MatchString(project) {
		return "", mcpserver.Fail(mcpserver.Failed, "the project must be a numeric id or a namespace/project path")
	}
	return "/projects/" + url.PathEscape(project), nil
}

// validBranch applies git's ref name rules to a branch name.
func validBranch(b string) bool {
	if b == "" || len(b) > 255 || b == "@" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") ||
		strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "@{") ||
		strings.ContainsAny(b, "~^:?*[\\ ") {
		return false
	}
	for _, r := range b {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			return false
		}
	}
	for _, part := range strings.Split(b, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func branchArg(b string) error {
	if !validBranch(b) {
		return mcpserver.Fail(mcpserver.Failed, "the branch name is not a valid git branch name")
	}
	return nil
}

// parseRef turns a !iid ref into the merge request iid; any other ref is
// refused before a request is made.
func parseRef(ref string) (int64, error) {
	if !refPattern.MatchString(ref) {
		return 0, mcpserver.Fail(mcpserver.Failed, "the ref must be !<merge request iid>")
	}
	iid, err := strconv.ParseInt(ref[1:], 10, 64)
	if err != nil {
		return 0, mcpserver.Fail(mcpserver.Failed, "the ref must be !<merge request iid>")
	}
	return iid, nil
}

func formatRef(iid int64) (string, bool) {
	ref := "!" + strconv.FormatInt(iid, 10)
	return ref, refPattern.MatchString(ref)
}

// validURL accepts an absolute http or https URL without credentials, as the
// contract's uri format does.
func validURL(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > 2048 || strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r)
	}) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

// cut bounds s to limit runes, marking a cut so it is never silent.
func cut(s string, limit int) string {
	return cutWith(s, limit, truncatedMark)
}

// cutLine bounds a single-line value to limit runes with a single-line mark.
func cutLine(s string, limit int) string {
	return cutWith(s, limit, lineCutMark)
}

func cutWith(s string, limit int, mark string) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	keep := limit - utf8.RuneCountInString(mark)
	runes := []rune(s)
	return string(runes[:keep]) + mark
}

// isBot reports whether a note author is a bot: GitLab says so, or the
// username is a project or group token bot, the ghost user, or ends in -bot
// or _bot.
func isBot(username string, bot bool) bool {
	return bot || botUser.MatchString(username) || username == "ghost" ||
		strings.HasSuffix(username, "-bot") || strings.HasSuffix(username, "_bot")
}

// validNotePath accepts a diff path the contract's identifier format accepts;
// any other path is left out of the comment.
func validNotePath(p string) bool {
	return p != "" && utf8.RuneCountInString(p) <= maxPathRunes && strings.TrimSpace(p) == p &&
		!strings.ContainsFunc(p, func(r rune) bool {
			return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029
		})
}

func malformed(what string) error {
	return mcpserver.Fail(mcpserver.Failed, "GitLab sent "+what)
}

// The GitLab REST v4 shapes the tools read. Only the fields used are decoded.
type glProject struct {
	ID                               int64   `json:"id"`
	PathWithNamespace                string  `json:"path_with_namespace"`
	DefaultBranch                    *string `json:"default_branch"`
	WebURL                           string  `json:"web_url"`
	OnlyAllowMergeIfPipelineSucceeds bool    `json:"only_allow_merge_if_pipeline_succeeds"`
	ApprovalsBeforeMerge             *int64  `json:"approvals_before_merge"`
}

type glProtectedBranch struct {
	Name string `json:"name"`
}

type glMergeRequest struct {
	IID             int64   `json:"iid"`
	State           string  `json:"state"`
	Draft           bool    `json:"draft"`
	SHA             string  `json:"sha"`
	WebURL          string  `json:"web_url"`
	MergedAt        *string `json:"merged_at"`
	MergeCommitSHA  *string `json:"merge_commit_sha"`
	SourceBranch    string  `json:"source_branch"`
	TargetBranch    string  `json:"target_branch"`
	SourceProjectID int64   `json:"source_project_id"`
	TargetProjectID int64   `json:"target_project_id"`
}

type glPipeline struct {
	ID     int64  `json:"id"`
	SHA    string `json:"sha"`
	Status string `json:"status"`
	WebURL string `json:"web_url"`
}

type glJob struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	WebURL       string `json:"web_url"`
	AllowFailure bool   `json:"allow_failure"`
}

type glNote struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	System    bool   `json:"system"`
	CreatedAt string `json:"created_at"`
	Author    struct {
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	} `json:"author"`
	Position *struct {
		NewPath *string `json:"new_path"`
		NewLine *int64  `json:"new_line"`
	} `json:"position"`
}

func (f *forge) project(ctx context.Context, path string) (glProject, error) {
	var p glProject
	if err := f.c.get(ctx, path, nil, &p); err != nil {
		return glProject{}, f.c.fail(err)
	}
	return p, nil
}

// changeState maps a merge request state; locked is a transient open state.
func changeState(s string) (string, bool) {
	switch s {
	case "opened", "locked":
		return "open", true
	case "closed":
		return "closed", true
	case "merged":
		return "merged", true
	}
	return "", false
}

func (f *forge) describe(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		Project string `json:"project"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	path, err := projectPath(in.Project)
	if err != nil {
		return nil, err
	}
	p, err := f.project(ctx, path)
	if err != nil {
		return nil, err
	}
	identity := f.c.host + "/" + p.PathWithNamespace
	switch {
	case !pathProject.MatchString(p.PathWithNamespace) || utf8.RuneCountInString(identity) > 300:
		return nil, malformed("a malformed project path")
	case p.DefaultBranch == nil || !validBranch(*p.DefaultBranch):
		return nil, mcpserver.Fail(mcpserver.Failed, "the GitLab project has no valid default branch")
	case !validURL(p.WebURL):
		return nil, malformed("a malformed project URL")
	}
	return contract.ForgeDescription{
		Identity:      identity,
		Host:          f.c.host,
		DisplayName:   "GitLab",
		ChangeNoun:    "merge request",
		DefaultBranch: *p.DefaultBranch,
		CIKind:        "gitlab-ci",
		WebURL:        p.WebURL,
	}, nil
}

func (f *forge) policy(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		Project string `json:"project"`
		Branch  string `json:"branch"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	path, err := projectPath(in.Project)
	if err != nil {
		return nil, err
	}
	if err := branchArg(in.Branch); err != nil {
		return nil, err
	}
	// The project is read first, so a missing project fails instead of reading
	// as an unprotected branch.
	p, err := f.project(ctx, path)
	if err != nil {
		return nil, err
	}
	protected := true
	var pb glProtectedBranch
	if err := f.c.get(ctx, path+"/protected_branches/"+url.PathEscape(in.Branch), nil, &pb); err != nil {
		if !isStatus(err, http.StatusNotFound) {
			return nil, f.c.fail(err)
		}
		protected = false
	}
	out := contract.ForgePolicy{RequiredChecks: []string{}, Protected: protected}
	if p.OnlyAllowMergeIfPipelineSucceeds {
		out.RequiredChecks = []string{pipelineCheck}
	}
	if p.ApprovalsBeforeMerge != nil {
		if *p.ApprovalsBeforeMerge < 0 || *p.ApprovalsBeforeMerge > maxReviews {
			return nil, malformed("an approval count outside 0..100")
		}
		out.RequiredReviews = int(*p.ApprovalsBeforeMerge)
	}
	return out, nil
}

func (f *forge) findChange(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		Project    string `json:"project"`
		HeadBranch string `json:"headBranch"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	path, err := projectPath(in.Project)
	if err != nil {
		return nil, err
	}
	if err := branchArg(in.HeadBranch); err != nil {
		return nil, err
	}
	mrs, more, err := listPages[glMergeRequest](ctx, f.c, path+"/merge_requests", url.Values{"source_branch": {in.HeadBranch}, "state": {"opened"}})
	if err != nil {
		return nil, f.c.fail(err)
	}
	if more {
		return nil, mcpserver.Fail(mcpserver.Failed, "GitLab lists more open merge requests than five pages hold")
	}
	out := contract.ForgeChanges{Changes: []contract.ForgeChangeRef{}}
	for _, mr := range mrs {
		// A merge request from a fork can share the branch name; only one from
		// the project itself is the engine's change.
		if mr.SourceProjectID != mr.TargetProjectID {
			continue
		}
		ref, ok := formatRef(mr.IID)
		state, known := changeState(mr.State)
		if !ok || !known || !validURL(mr.WebURL) || !commitPattern.MatchString(mr.SHA) {
			return nil, malformed("a malformed merge request")
		}
		if len(out.Changes) == maxChanges {
			return nil, mcpserver.Fail(mcpserver.Failed, "more than 20 open merge requests share the head branch")
		}
		out.Changes = append(out.Changes, contract.ForgeChangeRef{Ref: ref, URL: mr.WebURL, State: state, Draft: mr.Draft, HeadCommit: mr.SHA})
	}
	return out, nil
}

// draftTitle trims the title and prefixes Draft: unless it already reads as a
// draft.
func draftTitle(title string) string {
	title = strings.TrimSpace(title)
	if draftPrefix.MatchString(title) {
		return title
	}
	return "Draft: " + title
}

// neutralizeQuickActions puts a backslash before the slash of every line whose
// first non-whitespace character is /, so GitLab runs no quick action such as
// /merge or /ready from the body.
func neutralizeQuickActions(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		rest := strings.TrimLeftFunc(line, unicode.IsSpace)
		if strings.HasPrefix(rest, "/") {
			at := len(line) - len(rest)
			lines[i] = line[:at] + `\` + rest
		}
	}
	return strings.Join(lines, "\n")
}

func (f *forge) publishDraft(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		Project    string `json:"project"`
		HeadBranch string `json:"headBranch"`
		Base       string `json:"base"`
		Title      string `json:"title"`
		Body       string `json:"body"`
		Commit     string `json:"commit"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	path, err := projectPath(in.Project)
	if err != nil {
		return nil, err
	}
	if err := branchArg(in.HeadBranch); err != nil {
		return nil, err
	}
	if err := branchArg(in.Base); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, mcpserver.Fail(mcpserver.Failed, "the draft title is blank")
	}
	title := draftTitle(in.Title)
	if utf8.RuneCountInString(title) > maxTitleRunes {
		return nil, mcpserver.Fail(mcpserver.Failed, "the draft title exceeds GitLab's 255-character limit")
	}
	body := map[string]string{"source_branch": in.HeadBranch, "target_branch": in.Base, "title": title, "description": neutralizeQuickActions(in.Body)}
	var mr glMergeRequest
	if _, err := f.c.do(ctx, http.MethodPost, path+"/merge_requests", nil, body, &mr); err != nil {
		// A status answer means GitLab refused the request. Any other failure
		// may follow a create whose answer was lost.
		var refused *statusError
		if errors.As(err, &refused) {
			return nil, f.c.fail(err)
		}
		return f.recoverDraft(ctx, path, in.HeadBranch, in.Base, in.Commit, f.c.fail(err))
	}
	ref, ok := formatRef(mr.IID)
	if !ok || !validURL(mr.WebURL) || !commitPattern.MatchString(mr.SHA) {
		return f.recoverDraft(ctx, path, in.HeadBranch, in.Base, in.Commit, malformed("a malformed merge request"))
	}
	if !mr.Draft {
		return f.recoverDraft(ctx, path, in.HeadBranch, in.Base, in.Commit, mcpserver.Fail(mcpserver.Failed, "GitLab did not open the merge request as a draft"))
	}
	return contract.ForgePublished{Ref: ref, URL: mr.WebURL, Draft: true, HeadCommit: mr.SHA}, nil
}

// recoverDraft runs after a create whose answer is missing or unusable, so a
// change GitLab made is never left unreported. It reads the open merge
// requests on the head branch once and returns the one draft at the approved
// commit when it is the only open merge request there from the project
// itself; otherwise it returns orig.
func (f *forge) recoverDraft(ctx context.Context, path, head, base, commit string, orig error) (any, error) {
	var mrs []glMergeRequest
	header, err := f.c.do(ctx, http.MethodGet, path+"/merge_requests", url.Values{
		"source_branch": {head}, "state": {"opened"}, "per_page": {strconv.Itoa(maxChanges)}, "page": {"1"},
	}, nil, &mrs)
	if err != nil || len(mrs) > maxChanges || strings.TrimSpace(header.Get("X-Next-Page")) != "" {
		return nil, orig
	}
	var own []glMergeRequest
	for _, mr := range mrs {
		if mr.SourceProjectID == mr.TargetProjectID {
			own = append(own, mr)
		}
	}
	if len(own) != 1 {
		return nil, orig
	}
	mr := own[0]
	ref, ok := formatRef(mr.IID)
	if !ok || mr.State != "opened" || !mr.Draft || mr.SHA != commit || mr.SourceBranch != head || mr.TargetBranch != base || !validURL(mr.WebURL) {
		return nil, orig
	}
	return contract.ForgePublished{Ref: ref, URL: mr.WebURL, Draft: true, HeadCommit: mr.SHA}, nil
}

func (f *forge) mergeRequest(ctx context.Context, project, ref string) (string, int64, glMergeRequest, error) {
	path, err := projectPath(project)
	if err != nil {
		return "", 0, glMergeRequest{}, err
	}
	iid, err := parseRef(ref)
	if err != nil {
		return "", 0, glMergeRequest{}, err
	}
	var mr glMergeRequest
	if err := f.c.get(ctx, path+"/merge_requests/"+strconv.FormatInt(iid, 10), nil, &mr); err != nil {
		return "", 0, glMergeRequest{}, f.c.fail(err)
	}
	return path, iid, mr, nil
}

func (f *forge) change(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		Project string `json:"project"`
		Ref     string `json:"ref"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	_, _, mr, err := f.mergeRequest(ctx, in.Project, in.Ref)
	if err != nil {
		return nil, err
	}
	state, known := changeState(mr.State)
	if !known || !validURL(mr.WebURL) || !commitPattern.MatchString(mr.SHA) {
		return nil, malformed("a malformed merge request")
	}
	out := contract.ForgeChange{State: state, Draft: mr.Draft, HeadCommit: mr.SHA, URL: mr.WebURL}
	// Merge details are reported only for a merged change.
	if state == "merged" {
		if mr.MergedAt != nil {
			at, err := time.Parse(time.RFC3339, *mr.MergedAt)
			if err != nil {
				return nil, malformed("a malformed merge time")
			}
			s := at.UTC().Format(time.RFC3339Nano)
			out.MergedAt = &s
		}
		if mr.MergeCommitSHA != nil && commitPattern.MatchString(*mr.MergeCommitSHA) {
			s := *mr.MergeCommitSHA
			out.MergeCommit = &s
		}
	}
	return out, nil
}

// jobStatus maps a GitLab job or pipeline status to a contract status and
// conclusion. Only success, failed, canceled and skipped are completed;
// manual and every unknown status stay queued, so they never count as green.
func jobStatus(s string) (status, conclusion string) {
	switch s {
	case "success":
		return "completed", "success"
	case "failed":
		return "completed", "failure"
	case "canceled":
		return "completed", "cancelled"
	case "skipped":
		return "completed", "skipped"
	case "running":
		return "in_progress", ""
	}
	return "queued", ""
}

// terminal reports whether a pipeline status is final.
func terminal(s string) bool {
	status, _ := jobStatus(s)
	return status == "completed"
}

func checkURL(s string) *string {
	if !validURL(s) {
		return nil
	}
	return &s
}

func (f *forge) checks(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		Project string `json:"project"`
		Ref     string `json:"ref"`
		Commit  string `json:"commit"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	path, err := projectPath(in.Project)
	if err != nil {
		return nil, err
	}
	iid, err := parseRef(in.Ref)
	if err != nil {
		return nil, err
	}
	pipelines, morePipelines, err := listPages[glPipeline](ctx, f.c, path+"/merge_requests/"+strconv.FormatInt(iid, 10)+"/pipelines", nil)
	if err != nil {
		return nil, f.c.fail(err)
	}
	var newest *glPipeline
	for i := range pipelines {
		p := &pipelines[i]
		if p.SHA == in.Commit && (newest == nil || p.ID > newest.ID) {
			newest = p
		}
	}
	out := contract.ForgeChecks{Checks: []contract.ForgeCheck{}}
	if newest == nil {
		return out, nil
	}
	if newest.ID < 1 {
		return nil, malformed("a malformed pipeline")
	}
	jobs, moreJobs, err := listPages[glJob](ctx, f.c, path+"/pipelines/"+strconv.FormatInt(newest.ID, 10)+"/jobs", nil)
	if err != nil {
		return nil, f.c.fail(err)
	}
	slices.SortStableFunc(jobs, func(a, b glJob) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	cutJobs := false
	for _, j := range jobs {
		if len(out.Checks) == maxChecks-1 {
			cutJobs = true
			break
		}
		name := strings.TrimSpace(j.Name)
		if name == "" {
			name = "job " + strconv.FormatInt(j.ID, 10)
		}
		// A job named like the aggregate must never stand in for it.
		if name == pipelineCheck {
			name = "pipeline (job)"
		}
		status, conclusion := jobStatus(j.Status)
		// A job allowed to fail does not fail the pipeline, so its failure is
		// neutral.
		if j.AllowFailure && conclusion == "failure" {
			conclusion = "neutral"
		}
		out.Checks = append(out.Checks, contract.ForgeCheck{Name: cutLine(name, maxNameRunes), Status: status, Conclusion: conclusion, URL: checkURL(j.WebURL)})
	}
	status, conclusion := jobStatus(newest.Status)
	out.Checks = append(out.Checks, contract.ForgeCheck{Name: pipelineCheck, Status: status, Conclusion: conclusion, URL: checkURL(newest.WebURL)})
	// A listing cut at a bound may hide a newer pipeline or more jobs, so it is
	// never complete.
	out.Complete = terminal(newest.Status) && !morePipelines && !moreJobs && !cutJobs
	return out, nil
}

func (f *forge) comments(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		Project string `json:"project"`
		Ref     string `json:"ref"`
		Since   string `json:"since"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	path, err := projectPath(in.Project)
	if err != nil {
		return nil, err
	}
	iid, err := parseRef(in.Ref)
	if err != nil {
		return nil, err
	}
	var since time.Time
	if in.Since != "" {
		if since, err = time.Parse(time.RFC3339, in.Since); err != nil {
			return nil, mcpserver.Fail(mcpserver.Failed, "since must be an RFC 3339 date-time")
		}
	}
	notes, more, err := listPagesOf[glNote](ctx, f.c, path+"/merge_requests/"+strconv.FormatInt(iid, 10)+"/notes", url.Values{"sort": {"asc"}, "order_by": {"created_at"}}, notePerPage, maxNotePages)
	if err != nil {
		return nil, f.c.fail(err)
	}
	if more {
		return nil, mcpserver.Fail(mcpserver.Failed, "the merge request has more than 500 notes")
	}
	out := contract.ForgeComments{Comments: []contract.ForgeComment{}}
	for _, n := range notes {
		if n.System {
			continue
		}
		created, err := time.Parse(time.RFC3339, n.CreatedAt)
		if err != nil || n.ID < 1 {
			return nil, malformed("a malformed note")
		}
		if in.Since != "" && created.Before(since) {
			continue
		}
		c := contract.ForgeComment{
			ID:        strconv.FormatInt(n.ID, 10),
			Author:    cutLine(n.Author.Username, maxAuthorRunes),
			IsBot:     isBot(n.Author.Username, n.Author.Bot),
			Body:      cut(n.Body, maxBodyRunes),
			CreatedAt: created.UTC().Format(time.RFC3339Nano),
		}
		if n.Position != nil {
			if p := n.Position.NewPath; p != nil && validNotePath(*p) {
				notePath := *p
				c.Path = &notePath
			}
			if l := n.Position.NewLine; l != nil && *l >= 1 && *l <= maxLine {
				line := *l
				c.Line = &line
			}
		}
		out.Comments = append(out.Comments, c)
	}
	return out, nil
}
