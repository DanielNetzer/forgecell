package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/plugins/internal/mcpserver"
)

const (
	displayName = "Jira"
	// maxTitleRunes, maxLabelRunes, maxMilestoneRunes and maxBodyRunes are the
	// contract bounds of an Issue's title, labels, milestone and body;
	// maxLabels bounds how many labels one Issue reports.
	maxTitleRunes     = 300
	maxLabelRunes     = 100
	maxLabels         = 20
	maxMilestoneRunes = 200
	maxBodyRunes      = 65536
	// maxJQLRunes bounds the reviewed config.jql filter.
	maxJQLRunes = 1000
	// maxStateKeys is the contract bound of tracker_states keys.
	maxStateKeys = 50
	// maxTimeRunes bounds a Jira timestamp before it is parsed.
	maxTimeRunes  = 40
	truncatedMark = "\n[truncated]"
	// lineCutMark marks a cut single-line value, such as a title or a label.
	lineCutMark = "…"

	listFields   = "summary,labels,updated,status,fixVersions"
	getFields    = "summary,description,status,labels,updated,fixVersions,resolution"
	statesFields = "status,resolution,resolutiondate,updated"
	// genericKeyPattern is tracker_describe's keyPattern without a project.
	genericKeyPattern = `^[A-Z][A-Z0-9_]{1,9}-[1-9][0-9]{0,9}$`
)

var (
	projectPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,9}$`)
	genericKey     = regexp.MustCompile(genericKeyPattern)
	// sitePattern is a lowercase dotted host name with no port, path or scheme.
	sitePattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	labelPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	filterPattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)
	// states and selectors are tracker_describe's fixed lists.
	describeStates    = []string{"open", "closed"}
	describeSelectors = []string{"oldest", "label:", "filter:"}
	// notPlanned holds the lowercase resolution names that close an Issue as
	// not planned.
	notPlanned = map[string]bool{"won't do": true, "won't fix": true, "duplicate": true, "cannot reproduce": true, "declined": true}
	// jiraTimes are the layouts Jira writes timestamps in.
	jiraTimes = []string{"2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700", time.RFC3339Nano}
)

// tracker serves the tracker contract tools against Jira Cloud. configErr,
// when set, fails every call before any request.
type tracker struct {
	c         *client
	customCA  bool
	configErr error
}

func newTracker(lookup func(string) (string, bool)) *tracker {
	cfg, err := loadConfig(lookup)
	if err != nil {
		return &tracker{configErr: err}
	}
	return &tracker{c: newClient(cfg), customCA: cfg.customCA}
}

// bind returns the handler for one contract tool: it refuses arguments the
// contract input schema refuses, fails while the configuration is invalid and
// bounds the call by the tool's contract timeout. A contract tool with no
// handler is a programming error.
func (tr *tracker) bind(t *contract.Tool) mcpserver.Handler {
	handlers := map[string]func(context.Context, json.RawMessage) (any, error){
		"tracker_describe": tr.describe,
		"tracker_list":     tr.list,
		"tracker_get":      tr.get,
		"tracker_states":   tr.states,
	}
	h := handlers[t.Name]
	if h == nil {
		panic("jira: no handler for " + t.Name)
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		if err := t.ValidateInputJSON(args); err != nil {
			return nil, mcpserver.Fail(mcpserver.Failed, "the arguments do not match the input schema")
		}
		if tr.configErr != nil {
			return nil, mcpserver.Fail(mcpserver.Failed, tr.configErr.Error())
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

// siteConfig is the reviewed intake.config object.
type siteConfig struct {
	Site    string `json:"site"`
	Project string `json:"project"`
	JQL     string `json:"jql"`
}

// parseConfig checks every config key that is present: site, project and jql
// only, each a string. required also demands site and project. Its errors are
// fixed text that never names a config value.
func (tr *tracker) parseConfig(raw json.RawMessage, required bool) (siteConfig, error) {
	var c siteConfig
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return siteConfig{}, mcpserver.Fail(mcpserver.Failed, "the config holds only site, project and jql, each a string")
		}
	}
	if required && (c.Site == "" || c.Project == "") {
		return siteConfig{}, mcpserver.Fail(mcpserver.Failed, "the config must name a Jira site and a project")
	}
	if c.Site != "" && !tr.validSite(c.Site) {
		if tr.customCA {
			return siteConfig{}, mcpserver.Fail(mcpserver.Failed, "the config site must be a lowercase dotted host name with no scheme, port or path")
		}
		return siteConfig{}, mcpserver.Fail(mcpserver.Failed, "the config site must be a lowercase host name ending in .atlassian.net with no scheme, port or path")
	}
	if c.Project != "" && !projectPattern.MatchString(c.Project) {
		return siteConfig{}, mcpserver.Fail(mcpserver.Failed, "the config project must be a Jira project key matching ^[A-Z][A-Z0-9_]{1,9}$")
	}
	if c.JQL != "" && !validJQL(c.JQL) {
		return siteConfig{}, mcpserver.Fail(mcpserver.Failed, "the config jql must be at most 1000 characters, on one line, with balanced quotes and parentheses")
	}
	return c, nil
}

// validSite accepts a lowercase dotted host name; without JIRA_CA_FILE it must
// end in .atlassian.net.
func (tr *tracker) validSite(s string) bool {
	if len(s) > 253 || !sitePattern.MatchString(s) {
		return false
	}
	return tr.customCA || (strings.HasSuffix(s, ".atlassian.net") && s != "atlassian.net")
}

// validJQL accepts a reviewed filter of at most maxJQLRunes runes with no
// control or format characters, whose quotes close and whose parentheses
// balance outside quotes, so it can never close the parentheses it is wrapped
// in.
func validJQL(s string) bool {
	if strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > maxJQLRunes || !utf8.ValidString(s) {
		return false
	}
	depth := 0
	var quote rune
	escaped := false
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			return false
		}
		switch {
		case quote != 0:
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == quote:
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return quote == 0 && depth == 0
}

// jqlString quotes s as a JQL string literal, escaping backslashes and double
// quotes.
func jqlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// keyPatternFor is tracker_describe's keyPattern for a project, or the generic
// one without.
func keyPatternFor(project string) string {
	if project == "" {
		return genericKeyPattern
	}
	return "^" + project + "-[1-9][0-9]{0,9}$"
}

// validKey reports whether key is an Issue key of the project.
func validKey(project, key string) bool {
	return genericKey.MatchString(key) && strings.HasPrefix(key, project+"-")
}

func keyArg(project, key string) error {
	if !validKey(project, key) {
		return mcpserver.Fail(mcpserver.Failed, "the key must match ^"+project+"-[1-9][0-9]{0,9}$")
	}
	return nil
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

func malformed(what string) error {
	return mcpserver.Fail(mcpserver.Failed, "Jira sent "+what)
}

// The Jira Cloud REST v3 shapes the tools read. Only the fields used are
// decoded.
type jiraNamed struct {
	Name string `json:"name"`
}

type jiraStatus struct {
	Name           string `json:"name"`
	StatusCategory struct {
		Key string `json:"key"`
	} `json:"statusCategory"`
}

type jiraFields struct {
	Summary        string          `json:"summary"`
	Description    json.RawMessage `json:"description"`
	Status         *jiraStatus     `json:"status"`
	Labels         []string        `json:"labels"`
	Updated        string          `json:"updated"`
	FixVersions    []jiraNamed     `json:"fixVersions"`
	Resolution     *jiraNamed      `json:"resolution"`
	ResolutionDate *string         `json:"resolutiondate"`
}

type jiraIssue struct {
	Key    string     `json:"key"`
	Fields jiraFields `json:"fields"`
}

// jiraTime parses a Jira timestamp into a UTC RFC 3339 date-time.
func jiraTime(s string) (string, bool) {
	if s == "" || utf8.RuneCountInString(s) > maxTimeRunes {
		return "", false
	}
	for _, layout := range jiraTimes {
		if at, err := time.Parse(layout, s); err == nil {
			return at.UTC().Format(time.RFC3339Nano), true
		}
	}
	return "", false
}

// stateOf maps a status category and resolution: done is closed, as not
// planned for the resolutions that say so and completed otherwise; every other
// category is open.
func stateOf(category string, resolution *jiraNamed) (state string, reason *string) {
	if category != "done" {
		return "open", nil
	}
	r := "completed"
	if resolution != nil {
		name := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.Name, "’", "'")))
		if notPlanned[name] {
			r = "not_planned"
		}
	}
	return "closed", &r
}

// labelsOf bounds an Issue's labels to the contract: at most 20, each cut to
// 100 runes.
func labelsOf(in []string) []string {
	out := []string{}
	for _, l := range in {
		if len(out) == maxLabels {
			break
		}
		out = append(out, cutLine(l, maxLabelRunes))
	}
	return out
}

// milestoneOf is the first fix version's name, cut to the contract bound.
func milestoneOf(versions []jiraNamed) *string {
	if len(versions) == 0 || strings.TrimSpace(versions[0].Name) == "" {
		return nil
	}
	m := cutLine(versions[0].Name, maxMilestoneRunes)
	return &m
}

func (tr *tracker) describe(_ context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		Config json.RawMessage `json:"config"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	c, err := tr.parseConfig(in.Config, false)
	if err != nil {
		return nil, err
	}
	return contract.TrackerDescription{
		DisplayName: displayName,
		KeyPattern:  keyPatternFor(c.Project),
		States:      append([]string(nil), describeStates...),
		Selectors:   append([]string(nil), describeSelectors...),
	}, nil
}

// listJQL builds the tracker_list query: the project's Issues not done, the
// reviewed filter in parentheses, then the selector, oldest first. Selector
// values are checked and quoted, so a selector never adds JQL of its own.
func listJQL(c siteConfig, selector string) (string, error) {
	jql := "project = " + jqlString(c.Project) + " AND statusCategory != Done"
	if c.JQL != "" {
		jql += " AND (" + c.JQL + ")"
	}
	switch {
	case selector == "oldest":
	case strings.HasPrefix(selector, "label:"):
		name := strings.TrimPrefix(selector, "label:")
		if !labelPattern.MatchString(name) {
			return "", mcpserver.Fail(mcpserver.Failed, "a label selector name must match ^[A-Za-z0-9_.-]{1,64}$")
		}
		jql += " AND labels = " + jqlString(name)
	case strings.HasPrefix(selector, "filter:"):
		id := strings.TrimPrefix(selector, "filter:")
		if !filterPattern.MatchString(id) {
			return "", mcpserver.Fail(mcpserver.Failed, "a filter selector must name a numeric saved filter id")
		}
		jql += " AND filter = " + id
	default:
		return "", mcpserver.Fail(mcpserver.Failed, "the selector must be oldest, label:<name> or filter:<id>")
	}
	return jql + " ORDER BY created ASC", nil
}

func (tr *tracker) list(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		Config   json.RawMessage `json:"config"`
		Selector string          `json:"selector"`
		Cursor   string          `json:"cursor"`
		Limit    int             `json:"limit"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	c, err := tr.parseConfig(in.Config, true)
	if err != nil {
		return nil, err
	}
	jql, err := listJQL(c, in.Selector)
	if err != nil {
		return nil, err
	}
	if in.Cursor != "" && !validToken(in.Cursor) {
		return nil, mcpserver.Fail(mcpserver.Failed, "the cursor is not a Jira page token")
	}
	issues, next, err := tr.c.search(ctx, c.Site, jql, listFields, in.Limit, in.Cursor)
	if err != nil {
		return nil, err
	}
	if len(issues) == 0 && next != "" {
		return nil, mcpserver.Fail(mcpserver.Failed, "Jira sent five pages with no issues")
	}
	out := contract.TrackerPage{Items: []contract.TrackerItem{}}
	for _, is := range issues {
		if !validKey(c.Project, is.Key) || is.Fields.Status == nil {
			return nil, malformed("a malformed issue")
		}
		updated, ok := jiraTime(is.Fields.Updated)
		if !ok {
			return nil, malformed("a malformed update time")
		}
		state, _ := stateOf(is.Fields.Status.StatusCategory.Key, nil)
		out.Items = append(out.Items, contract.TrackerItem{
			Key:       is.Key,
			Title:     cutLine(is.Fields.Summary, maxTitleRunes),
			Labels:    labelsOf(is.Fields.Labels),
			Milestone: milestoneOf(is.Fields.FixVersions),
			UpdatedAt: updated,
			State:     state,
		})
	}
	if next != "" {
		out.NextCursor = &next
	}
	return out, nil
}

func (tr *tracker) get(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		Config json.RawMessage `json:"config"`
		Key    string          `json:"key"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	c, err := tr.parseConfig(in.Config, true)
	if err != nil {
		return nil, err
	}
	if err := keyArg(c.Project, in.Key); err != nil {
		return nil, err
	}
	var is jiraIssue
	if err := tr.c.get(ctx, c.Site, "/issue/"+url.PathEscape(in.Key), url.Values{"fields": {getFields}}, &is); err != nil {
		return nil, err
	}
	if is.Key != in.Key {
		return nil, malformed("a different issue than the one asked for")
	}
	if is.Fields.Status == nil {
		return nil, malformed("an issue with no status")
	}
	updated, ok := jiraTime(is.Fields.Updated)
	if !ok {
		return nil, malformed("a malformed update time")
	}
	body, err := descriptionText(is.Fields.Description, maxBodyRunes)
	if err != nil {
		return nil, err
	}
	state, reason := stateOf(is.Fields.Status.StatusCategory.Key, is.Fields.Resolution)
	return contract.TrackerIssue{
		Key:         is.Key,
		Title:       cutLine(is.Fields.Summary, maxTitleRunes),
		Body:        body,
		State:       state,
		StateReason: reason,
		URL:         "https://" + c.Site + "/browse/" + is.Key,
		UpdatedAt:   updated,
		Labels:      labelsOf(is.Fields.Labels),
		Milestone:   milestoneOf(is.Fields.FixVersions),
	}, nil
}

func (tr *tracker) states(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		Config json.RawMessage `json:"config"`
		Keys   []string        `json:"keys"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	c, err := tr.parseConfig(in.Config, true)
	if err != nil {
		return nil, err
	}
	asked := map[string]bool{}
	quoted := []string{}
	for _, key := range in.Keys {
		if err := keyArg(c.Project, key); err != nil {
			return nil, err
		}
		if !asked[key] {
			asked[key] = true
			quoted = append(quoted, jqlString(key))
		}
	}
	out := contract.TrackerStates{States: map[string]contract.TrackerState{}}
	if len(quoted) == 0 {
		return out, nil
	}
	jql := "key in (" + strings.Join(quoted, ", ") + ")"
	// Pages hold up to maxStateKeys issues, so an issue Jira lists that was not
	// asked for never takes the place of one that was.
	issues, next, err := tr.c.search(ctx, c.Site, jql, statesFields, maxStateKeys, "")
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for _, is := range issues {
		if asked[is.Key] {
			found[is.Key] = true
		}
	}
	if next != "" && len(found) < len(quoted) {
		return nil, mcpserver.Fail(mcpserver.Failed, "Jira did not list the issues within five pages")
	}
	for _, is := range issues {
		// A key Jira answers with that was not asked for, such as an Issue's
		// new key after a move, is left out.
		if !asked[is.Key] {
			continue
		}
		if is.Fields.Status == nil {
			return nil, malformed("an issue with no status")
		}
		state, reason := stateOf(is.Fields.Status.StatusCategory.Key, is.Fields.Resolution)
		s := contract.TrackerState{State: state, StateReason: reason}
		if state == "closed" && is.Fields.ResolutionDate != nil {
			at, ok := jiraTime(*is.Fields.ResolutionDate)
			if !ok {
				return nil, malformed("a malformed resolution date")
			}
			s.ClosedAt = &at
		}
		out.States[is.Key] = s
	}
	return out, nil
}
