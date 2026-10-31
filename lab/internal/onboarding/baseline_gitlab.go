package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
)

// GitLab baseline derivation reads the committed .gitlab-ci.yml and executes
// nothing. It reuses the closed set of recognizeBaselineLine, so a pipeline can
// never inject an argument into the reviewed Formula; anything it cannot read
// with certainty is skipped.
const (
	gitlabCIFile     = ".gitlab-ci.yml"
	gitlabMaxBytes   = 256 << 10
	gitlabMaxNodes   = 200000
	gitlabCIKind     = "gitlab-ci"
	githubActionKind = "github-actions"
)

// A leading `cd <dir> && ` prefix is the only way a line names its directory.
var gitlabCdPrefix = regexp.MustCompile(`^cd ([A-Za-z0-9_.][A-Za-z0-9_./-]*) && (.+)$`)

// gitlabReserved are top-level keys that configure the pipeline rather than name a job.
var gitlabReserved = map[string]bool{
	"stages": true, "variables": true, "default": true, "include": true, "workflow": true,
	"image": true, "services": true, "cache": true, "before_script": true, "after_script": true,
}

// gitlabParse decodes exactly one YAML document. It refuses oversized input, more
// than one document and duplicate mapping keys at any level.
func gitlabParse(content string) (*yaml.Node, error) {
	if len(content) > gitlabMaxBytes {
		return nil, fmt.Errorf("%s is %d bytes, more than the %d baseline derivation reads; explicit scoping is required", gitlabCIFile, len(content), gitlabMaxBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(content)))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s is not valid YAML", gitlabCIFile)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s must hold exactly one YAML document", gitlabCIFile)
	}
	if len(root.Content) == 0 {
		return nil, nil
	}
	doc := yamlAlias(root.Content[0])
	if doc == nil || doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of pipeline keys and jobs", gitlabCIFile)
	}
	if err := gitlabUniqueKeys(&root); err != nil {
		return nil, err
	}
	return doc, nil
}

// gitlabWalk visits every node reachable from n once, following aliases, and
// reports false when the walk exceeds its node budget or visit returns false.
func gitlabWalk(n *yaml.Node, visit func(*yaml.Node) bool) bool {
	seen := map[*yaml.Node]bool{}
	stack := []*yaml.Node{n}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == nil || seen[cur] {
			continue
		}
		if seen[cur] = true; len(seen) > gitlabMaxNodes || !visit(cur) {
			return false
		}
		if cur.Kind == yaml.AliasNode {
			stack = append(stack, cur.Alias)
		}
		stack = append(stack, cur.Content...)
	}
	return true
}

func gitlabUniqueKeys(root *yaml.Node) error {
	duplicate := false
	complete := gitlabWalk(root, func(n *yaml.Node) bool {
		if n.Kind != yaml.MappingNode {
			return true
		}
		keys := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, ok := yamlScalar(n.Content[i])
			if !ok {
				continue
			}
			if keys[k] {
				duplicate = true
				return false
			}
			keys[k] = true
		}
		return true
	})
	if duplicate {
		return fmt.Errorf("%s declares a duplicate key", gitlabCIFile)
	}
	if !complete {
		return fmt.Errorf("%s has more than %d YAML nodes; explicit scoping is required", gitlabCIFile, gitlabMaxNodes)
	}
	return nil
}

// gitlabHasReference reports whether a !reference tag is reachable from n, or the
// walk could not finish.
func gitlabHasReference(n *yaml.Node) bool {
	return !gitlabWalk(n, func(n *yaml.Node) bool { return n.Tag != "!reference" })
}

// gitlabHasKey reports whether mapping n declares key or a merge key, which could
// carry key in from an anchor.
func gitlabHasKey(n *yaml.Node, key string) bool {
	return yamlField(n, key) != nil || yamlField(n, "<<") != nil
}

func gitlabScalarIs(n *yaml.Node, values ...string) bool {
	v, ok := yamlScalar(n)
	if !ok {
		return false
	}
	for _, want := range values {
		if v == want {
			return true
		}
	}
	return false
}

// gitlabMergeRequestTerms are the only conditions read as naming a merge-request
// pipeline.
var gitlabMergeRequestTerms = map[string]bool{
	"$CI_MERGE_REQUEST_IID":                        true,
	`$CI_PIPELINE_SOURCE == "merge_request_event"`: true,
	`$CI_PIPELINE_SOURCE == 'merge_request_event'`: true,
}

// gitlabMergeRequestCondition reports whether an `if` condition is exactly one or
// more merge-request terms joined by ||. Negations, comparisons, conjunctions and
// grouping never count.
func gitlabMergeRequestCondition(cond string) bool {
	if strings.ContainsAny(cond, "!()") || strings.Contains(cond, "&&") || strings.Contains(cond, "=~") {
		return false
	}
	for _, term := range strings.Split(cond, "||") {
		if !gitlabMergeRequestTerms[strings.TrimSpace(term)] {
			return false
		}
	}
	return true
}

// gitlabRulesMatchMergeRequest evaluates rules the way GitLab does, first match
// wins, and reports whether a merge-request pipeline runs on success. It fails
// safe: a rule with any key other than if, when and allow_failure, an unknown
// when, an allow_failure other than false, a `when: never` entry alone, a rule
// without an if, or a `when: never` rule before the first merge-request rule all
// report false.
func gitlabRulesMatchMergeRequest(rules *yaml.Node) bool {
	if rules == nil || rules.Kind != yaml.SequenceNode {
		return false
	}
	var parsed []*yaml.Node
	for _, item := range rules.Content {
		rule := yamlAlias(item)
		if rule == nil || rule.Kind != yaml.MappingNode {
			return false
		}
		for i := 0; i < len(rule.Content); i += 2 {
			if !gitlabScalarIs(rule.Content[i], "if", "when", "allow_failure") {
				return false
			}
		}
		if when := yamlField(rule, "when"); when != nil && !gitlabScalarIs(when, "on_success", "always", "never") {
			return false
		}
		if af := yamlField(rule, "allow_failure"); af != nil && !gitlabScalarIs(af, "false") {
			return false
		}
		if len(rule.Content) == 2 && gitlabScalarIs(yamlField(rule, "when"), "never") {
			return false
		}
		parsed = append(parsed, rule)
	}
	for _, rule := range parsed {
		cond, ok := yamlScalar(yamlField(rule, "if"))
		if !ok {
			return false
		}
		never := gitlabScalarIs(yamlField(rule, "when"), "never")
		if gitlabMergeRequestCondition(cond) {
			return !never
		}
		if never {
			return false
		}
	}
	return false
}

// gitlabPipelineRunsOnMergeRequest applies a top-level workflow: with rules, a
// merge-request pipeline exists only when those rules match one.
func gitlabPipelineRunsOnMergeRequest(doc *yaml.Node) bool {
	workflow := yamlField(doc, "workflow")
	if workflow == nil {
		return true
	}
	if workflow.Kind != yaml.MappingNode || yamlField(workflow, "<<") != nil || gitlabHasReference(workflow) {
		return false
	}
	rules := yamlField(workflow, "rules")
	return rules == nil || gitlabRulesMatchMergeRequest(rules)
}

// gitlabRunsOnMergeRequest applies the merge-request rule: no rules, only or
// except, rules whose first match names a merge-request pipeline, or only
// listing merge_requests.
func gitlabRunsOnMergeRequest(job *yaml.Node) bool {
	rules, only, except := yamlField(job, "rules"), yamlField(job, "only"), yamlField(job, "except")
	switch {
	case rules == nil && only == nil && except == nil:
		return true
	case except != nil || (rules != nil && only != nil):
		return false
	case only != nil:
		refs := only
		if only.Kind == yaml.MappingNode {
			if len(only.Content) != 2 {
				return false
			}
			refs = yamlField(only, "refs")
		}
		if refs == nil || refs.Kind != yaml.SequenceNode {
			return false
		}
		for _, item := range refs.Content {
			if gitlabScalarIs(item, "merge_requests") {
				return true
			}
		}
		return false
	}
	return gitlabRulesMatchMergeRequest(rules)
}

// gitlabJobReadable reports whether a job's script can be read line by line: it
// shares no shell with a before_script and declares nothing that changes which
// definition, pipeline or failure policy applies.
func gitlabJobReadable(job *yaml.Node) bool {
	for _, key := range []string{"extends", "include", "trigger", "before_script", "<<"} {
		if yamlField(job, key) != nil {
			return false
		}
	}
	if parallel := yamlField(job, "parallel"); parallel != nil && (parallel.Kind != yaml.ScalarNode || yamlField(parallel, "matrix") != nil) {
		return false
	}
	if n := yamlField(job, "allow_failure"); n != nil && !gitlabScalarIs(n, "false") {
		return false
	}
	if n := yamlField(job, "when"); n != nil && !gitlabScalarIs(n, "on_success", "always") {
		return false
	}
	return !gitlabHasReference(job) && gitlabRunsOnMergeRequest(job)
}

// gitlabDir validates the directory of a `cd <dir> && ` prefix relative to the
// current one, the way effectiveDir and trackedFromDir check a working directory.
func gitlabDir(cwd, value string, tracked map[string]bool) (string, bool) {
	value = strings.TrimSuffix(value, "/")
	if value == "" || path.IsAbs(value) || path.Clean(value) != value || hasParentSegment(value) {
		return "", false
	}
	dir := path.Join(cwd, value)
	if dir != "." && len(trackedFromDir(tracked, dir)) == 0 {
		return "", false
	}
	return dir, true
}

// gitlabScriptLines returns a job's script entries, stopping at the first entry that
// is not a single-line string.
func gitlabScriptLines(job *yaml.Node) []string {
	script := yamlField(job, "script")
	if script == nil {
		return nil
	}
	entries := []*yaml.Node{script}
	if script.Kind == yaml.SequenceNode {
		entries = script.Content
	}
	var lines []string
	for _, entry := range entries {
		value, ok := yamlScalar(entry)
		if !ok {
			break
		}
		value = strings.TrimSpace(value)
		if strings.ContainsAny(value, "\r\n") {
			break
		}
		lines = append(lines, value)
	}
	return lines
}

// deriveFromGitLabJob lists a job's recognized commands in order. Every line runs in
// the same shell, so the first line that is not recognized ends the job, and a
// recognized `cd` prefix moves every later line.
func deriveFromGitLabJob(name string, job *yaml.Node, tracked map[string]bool) []formula.RequiredCheck {
	where := gitlabCIFile + ", job " + name
	var out []formula.RequiredCheck
	cwd := "."
	for _, line := range gitlabScriptLines(job) {
		if m := gitlabCdPrefix.FindStringSubmatch(line); m != nil {
			dir, ok := gitlabDir(cwd, m[1], tracked)
			if !ok {
				break
			}
			cwd, line = dir, m[2]
		}
		command, ok := recognizeBaselineLine(line, trackedFromDir(tracked, cwd))
		if !ok {
			break
		}
		check := formula.RequiredCheck{
			ID:        "baseline-" + command.kind,
			Argv:      command.argv,
			Dir:       cwd,
			TimeoutMS: command.timeoutMS,
			Workflow:  gitlabCIFile,
			Reason:    fmt.Sprintf("GitLab CI runs `%s` on merge requests (%s) in %s.", command.display, where, baselineDisplayDir(cwd)),
		}
		if check.Validate() != nil {
			break
		}
		out = append(out, check)
	}
	return out
}

// DeriveGitLabBaseline derives the reviewed CI baseline from the content of a
// committed .gitlab-ci.yml. tracked holds the committed regular files. Duplicate
// commands collapse and identifiers are deterministic, as in DeriveBaseline.
func DeriveGitLabBaseline(content string, tracked map[string]bool) ([]formula.RequiredCheck, error) {
	doc, err := gitlabParse(content)
	if err != nil || doc == nil {
		return nil, err
	}
	// An include, a merge key or a shared before_script can change every job.
	if yamlField(doc, "include") != nil || yamlField(doc, "<<") != nil || yamlField(doc, "before_script") != nil || gitlabHasKey(yamlField(doc, "default"), "before_script") {
		return nil, nil
	}
	if !gitlabPipelineRunsOnMergeRequest(doc) {
		return nil, nil
	}
	var out []formula.RequiredCheck
	seenCommand := map[string]bool{}
	usedID := map[string]int{}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key := yamlAlias(doc.Content[i])
		if key == nil || key.Kind != yaml.ScalarNode || gitlabReserved[key.Value] || !baselineJobName.MatchString(key.Value) {
			continue
		}
		job := yamlAlias(doc.Content[i+1])
		if job == nil || job.Kind != yaml.MappingNode || !gitlabJobReadable(job) {
			continue
		}
		for _, check := range deriveFromGitLabJob(key.Value, job, tracked) {
			commandKey := check.Dir + "\x00" + strings.Join(check.Argv, "\x00")
			if seenCommand[commandKey] {
				continue
			}
			seenCommand[commandKey] = true
			if check.Dir != "." {
				check.ID += "-" + strings.Trim(baselineSlug.ReplaceAllString(strings.ToLower(check.Dir), "-"), "-")
			}
			base := check.ID
			if usedID[base]++; usedID[base] > 1 {
				check.ID = fmt.Sprintf("%s-%d", base, usedID[base])
			}
			if err := check.Validate(); err != nil {
				continue
			}
			out = append(out, check)
		}
	}
	if len(out) > formula.MaxRequiredChecks {
		return nil, fmt.Errorf("%s declares %d baseline checks, more than the %d a Formula may record; explicit scoping is required", gitlabCIFile, len(out), formula.MaxRequiredChecks)
	}
	return out, nil
}

// DeriveBaselineFor selects the baseline derivation for a CI kind. GitHub Actions,
// the default, reads r's workflows; GitLab CI reads the gitlabCI content the caller
// passes.
func DeriveBaselineFor(ciKind string, r Repository, tracked map[string]bool, gitlabCI string) ([]formula.RequiredCheck, error) {
	switch ciKind {
	case "", githubActionKind:
		return DeriveBaseline(r, tracked)
	case gitlabCIKind:
		return DeriveGitLabBaseline(gitlabCI, tracked)
	}
	return nil, fmt.Errorf("CI kind must be %q or %q", githubActionKind, gitlabCIKind)
}
