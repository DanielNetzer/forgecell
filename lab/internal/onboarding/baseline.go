package onboarding

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"go.yaml.in/yaml/v4"
)

// Baseline derivation reads committed workflow text and executes nothing. Only a
// closed set of exact command lines is recognized; anything else is skipped, so a
// workflow can never inject an argument into the reviewed Formula.
var (
	baselineGoTestTimeoutLine = regexp.MustCompile(`^go test -timeout ([1-9][0-9]?)m \./\.\.\.$`)
	baselineGofmtLine         = regexp.MustCompile(`^test -z "\$\(gofmt -l ((?:\.(?:/[A-Za-z0-9_][A-Za-z0-9_./-]*)?|[A-Za-z0-9_][A-Za-z0-9_./-]*)(?: (?:\.(?:/[A-Za-z0-9_][A-Za-z0-9_./-]*)?|[A-Za-z0-9_][A-Za-z0-9_./-]*))*)\)"$`)
	baselineScriptLine        = regexp.MustCompile(`^(sh|bash) ([A-Za-z0-9_][A-Za-z0-9_./-]*\.sh)$`)
	baselineJobName           = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	baselineSlug              = regexp.MustCompile(`[^a-z0-9]+`)
)

const (
	baselineMaxLineBytes = 300
	baselineGoTestMS     = 900000
	baselineGoVetMS      = 600000
	baselineQuickMS      = 120000
	// A `go test -timeout <N>m` step outlives Go's own timeout by this margin, so Go's
	// failure report is recorded rather than a bare kill.
	baselineGoTestMaxMinutes    = 55
	baselineGoTestMarginMinutes = 5
)

// baselineComplexWords start shell lines that make a multi-line block unsafe to read
// line by line: control flow, heredocs and directory changes alter what a later
// line means.
var baselineComplexWords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true, "for": true, "while": true, "until": true,
	"do": true, "done": true, "case": true, "esac": true, "function": true, "select": true, "trap": true,
	"cd": true, "pushd": true, "popd": true, "{": true, "}": true, "(": true, ")": true,
	// They change the environment, options or sourced code of every later line.
	"export": true, "set": true, "source": true, ".": true, "unset": true,
	"eval": true, "exec": true, "exit": true, "readonly": true, "declare": true,
}

// baselineAssignment matches a leading VAR=value word, which sets the environment
// of the line or of every later line.
var baselineAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func baselineComplex(lines []string) bool {
	for _, line := range lines {
		word := ""
		if fields := strings.Fields(line); len(fields) > 0 {
			word = fields[0]
		}
		if baselineComplexWords[word] || baselineAssignment.MatchString(word) || strings.Contains(line, "<<") {
			return true
		}
		for _, suffix := range []string{"\\", "&&", "||", "|", ";", "&"} {
			if strings.HasSuffix(line, suffix) {
				return true
			}
		}
	}
	return false
}

type baselineCommand struct {
	kind      string
	argv      []string
	timeoutMS int
	display   string
}

// trackedFromDir re-keys the committed files relative to dir, because a script line
// runs from the step's working directory: script S is tracked when dir/S is.
// A root directory returns tracked unchanged.
func trackedFromDir(tracked map[string]bool, dir string) map[string]bool {
	if dir == "." || dir == "" {
		return tracked
	}
	scoped := map[string]bool{}
	for file := range tracked {
		if rel, ok := strings.CutPrefix(file, path.Clean(dir)+"/"); ok {
			scoped[rel] = true
		}
	}
	return scoped
}

func hasParentSegment(p string) bool {
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// recognizeBaselineLine maps one shell line to a baseline command, or reports false.
func recognizeBaselineLine(line string, tracked map[string]bool) (baselineCommand, bool) {
	if len(line) > baselineMaxLineBytes {
		return baselineCommand{}, false
	}
	switch line {
	case "go test ./...":
		return baselineCommand{kind: "go-test", argv: []string{"go", "test", "./..."}, timeoutMS: baselineGoTestMS, display: line}, true
	case "go vet ./...":
		return baselineCommand{kind: "go-vet", argv: []string{"go", "vet", "./..."}, timeoutMS: baselineGoVetMS, display: line}, true
	}
	if m := baselineGoTestTimeoutLine.FindStringSubmatch(line); m != nil {
		minutes, err := strconv.Atoi(m[1])
		if err != nil || minutes < 1 || minutes > baselineGoTestMaxMinutes {
			return baselineCommand{}, false
		}
		return baselineCommand{kind: "go-test", argv: []string{"go", "test", "-timeout", m[1] + "m", "./..."}, timeoutMS: (minutes + baselineGoTestMarginMinutes) * 60000, display: line}, true
	}
	if m := baselineGofmtLine.FindStringSubmatch(line); m != nil {
		if hasParentSegment(m[1]) {
			return baselineCommand{}, false
		}
		return baselineCommand{kind: "gofmt", argv: []string{"sh", "-c", line}, timeoutMS: baselineQuickMS, display: line}, true
	}
	if m := baselineScriptLine.FindStringSubmatch(line); m != nil {
		script := m[2]
		name := strings.ToLower(path.Base(script))
		if hasParentSegment(script) || path.Clean(script) != script || !tracked[script] || !(strings.Contains(name, "check") || strings.Contains(name, "hygiene")) {
			return baselineCommand{}, false
		}
		return baselineCommand{kind: "script-" + strings.Trim(baselineSlug.ReplaceAllString(strings.ToLower(strings.TrimSuffix(script, ".sh")), "-"), "-"), argv: []string{m[1], script}, timeoutMS: baselineQuickMS, display: line}, true
	}
	return baselineCommand{}, false
}

func yamlAlias(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode; i++ {
		if i > 8 {
			return nil
		}
		n = n.Alias
	}
	return n
}

func yamlField(n *yaml.Node, key string) *yaml.Node {
	n = yamlAlias(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := yamlAlias(n.Content[i]); k != nil && k.Kind == yaml.ScalarNode && k.Value == key {
			return yamlAlias(n.Content[i+1])
		}
	}
	return nil
}

func yamlScalar(n *yaml.Node) (string, bool) {
	n = yamlAlias(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", false
	}
	return n.Value, true
}

// runsOnPullRequest reports whether the workflow's `on` declares a pull_request
// trigger. The key is matched by its source text, so YAML 1.1 `on: true` handling
// cannot hide it.
func runsOnPullRequest(doc *yaml.Node) bool {
	on := yamlField(doc, "on")
	if on == nil {
		return false
	}
	switch on.Kind {
	case yaml.ScalarNode:
		return on.Value == "pull_request"
	case yaml.SequenceNode:
		for _, item := range on.Content {
			if v, ok := yamlScalar(item); ok && v == "pull_request" {
				return true
			}
		}
	case yaml.MappingNode:
		return mappingHasKey(on, "pull_request")
	}
	return false
}

func mappingHasKey(n *yaml.Node, key string) bool {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if v, ok := yamlScalar(n.Content[i]); ok && v == key {
			return true
		}
	}
	return false
}

// runDefaults returns the working-directory and shell declared under defaults.run.
func runDefaults(n *yaml.Node) (dir, shell *yaml.Node) {
	run := yamlField(yamlField(n, "defaults"), "run")
	return yamlField(run, "working-directory"), yamlField(run, "shell")
}

// effectiveDir resolves a step's working directory against its job and workflow
// defaults. It reports false for any value that is not a plain repository path.
func effectiveDir(step, job, workflow *yaml.Node) (string, bool) {
	stepDir := yamlField(step, "working-directory")
	jobDir, _ := runDefaults(job)
	flowDir, _ := runDefaults(workflow)
	var node *yaml.Node
	for _, candidate := range []*yaml.Node{stepDir, jobDir, flowDir} {
		if candidate != nil {
			node = candidate
			break
		}
	}
	if node == nil {
		return ".", true
	}
	value, ok := yamlScalar(node)
	if !ok || strings.Contains(value, "${{") {
		return "", false
	}
	value = strings.TrimSuffix(value, "/")
	if value == "" {
		return ".", true
	}
	if path.IsAbs(value) {
		return "", false
	}
	return path.Clean(value), true
}

func shellIsPOSIX(step, job, workflow *yaml.Node) bool {
	_, jobShell := runDefaults(job)
	_, flowShell := runDefaults(workflow)
	for _, candidate := range []*yaml.Node{yamlField(step, "shell"), jobShell, flowShell} {
		if candidate == nil {
			continue
		}
		value, ok := yamlScalar(candidate)
		return ok && (value == "bash" || value == "sh")
	}
	return true
}

func baselineDisplayDir(dir string) string {
	if dir == "." {
		return "the repository root"
	}
	return dir
}

// deriveFromWorkflow lists the recognized commands of one pull-request workflow in
// file order.
func deriveFromWorkflow(file, content string, tracked map[string]bool) []formula.RequiredCheck {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(content), &root); err != nil || len(root.Content) == 0 {
		return nil
	}
	doc := yamlAlias(root.Content[0])
	if !runsOnPullRequest(doc) {
		return nil
	}
	jobs := yamlField(doc, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	var out []formula.RequiredCheck
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		jobName, _ := yamlScalar(jobs.Content[i])
		job := yamlAlias(jobs.Content[i+1])
		if job == nil || job.Kind != yaml.MappingNode || yamlField(job, "if") != nil {
			continue
		}
		steps := yamlField(job, "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range steps.Content {
			step := yamlAlias(item)
			if step == nil || step.Kind != yaml.MappingNode || yamlField(step, "if") != nil {
				continue
			}
			if n := yamlField(step, "continue-on-error"); n != nil {
				if v, ok := yamlScalar(n); !ok || v != "false" {
					continue
				}
			}
			run, ok := yamlScalar(yamlField(step, "run"))
			if !ok || strings.Contains(run, "${{") || !shellIsPOSIX(step, job, doc) {
				continue
			}
			dir, ok := effectiveDir(step, job, doc)
			if !ok {
				continue
			}
			var lines []string
			for _, line := range strings.Split(strings.ReplaceAll(run, "\r\n", "\n"), "\n") {
				if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
					lines = append(lines, line)
				}
			}
			if baselineComplex(lines) {
				continue
			}
			where := file
			if baselineJobName.MatchString(jobName) {
				where += ", job " + jobName
			}
			scoped := trackedFromDir(tracked, dir)
			for _, line := range lines {
				command, ok := recognizeBaselineLine(line, scoped)
				if !ok {
					continue
				}
				check := formula.RequiredCheck{
					ID:        "baseline-" + command.kind,
					Argv:      command.argv,
					Dir:       dir,
					TimeoutMS: command.timeoutMS,
					Workflow:  file,
					Reason:    fmt.Sprintf("CI runs `%s` on pull requests (%s) in %s.", command.display, where, baselineDisplayDir(dir)),
				}
				if check.Validate() == nil {
					out = append(out, check)
				}
			}
		}
	}
	return out
}

// DeriveBaseline derives the reviewed CI baseline from the repository's committed
// pull-request workflows. tracked holds the committed regular files, so a recognized
// script must exist. Duplicate commands collapse and identifiers are deterministic.
func DeriveBaseline(r Repository, tracked map[string]bool) ([]formula.RequiredCheck, error) {
	contents := map[string]string{}
	for _, e := range r.Evidence {
		contents[e.Path] = e.Content
	}
	workflows := append([]string(nil), r.Workflows...)
	sort.Strings(workflows)
	var out []formula.RequiredCheck
	seenCommand := map[string]bool{}
	usedID := map[string]int{}
	for _, file := range workflows {
		content, ok := contents[file]
		if !ok {
			continue
		}
		for _, check := range deriveFromWorkflow(file, content, tracked) {
			key := check.Dir + "\x00" + strings.Join(check.Argv, "\x00")
			if seenCommand[key] {
				continue
			}
			seenCommand[key] = true
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
		return nil, fmt.Errorf("repository workflows declare %d baseline checks, more than the %d a Formula may record; explicit scoping is required", len(out), formula.MaxRequiredChecks)
	}
	return out, nil
}

// bootstrapBaseline derives the baseline for a bootstrap Formula. It returns nothing
// when the repository has no workflows.
func bootstrapBaseline(ctx context.Context, r Repository) ([]formula.RequiredCheck, error) {
	if len(r.Workflows) == 0 {
		return nil, nil
	}
	tracked, err := committedFiles(ctx, r)
	if err != nil {
		return nil, err
	}
	return DeriveBaseline(r, tracked)
}

// committedFiles lists the committed regular files of r's revision.
func committedFiles(ctx context.Context, r Repository) (map[string]bool, error) {
	snapshot, err := readiness.Collect(ctx, r.Root, r.Revision)
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, file := range snapshot.Files {
		if file.Mode == "100644" || file.Mode == "100755" {
			tracked[file.Path] = true
		}
	}
	return tracked, nil
}

// ciKindNone is a forge's CI kind when it runs no CI Forgecell can read.
const ciKindNone = "none"

// bootstrapBaselineFor derives the baseline for a forge's CI kind. GitHub
// Actions, and an empty kind, is bootstrapBaseline unchanged. GitLab CI reads
// the root .gitlab-ci.yml the repository evidence holds, and returns nothing
// when none is committed. none returns no baseline, and any other kind is
// refused.
func bootstrapBaselineFor(ctx context.Context, r Repository, ciKind string) ([]formula.RequiredCheck, error) {
	switch ciKind {
	case "", githubActionKind:
		return bootstrapBaseline(ctx, r)
	case ciKindNone:
		return nil, nil
	case gitlabCIKind:
	default:
		return nil, fmt.Errorf("the forge's CI kind must be %s, %s or %s", githubActionKind, gitlabCIKind, ciKindNone)
	}
	if !r.GitLabCI {
		return nil, nil
	}
	content, found := "", false
	for _, e := range r.Evidence {
		if e.Path == gitlabCIFile {
			content, found = e.Content, true
		}
	}
	if !found {
		return nil, fmt.Errorf("the committed %s was not collected as evidence; no baseline derived", gitlabCIFile)
	}
	tracked, err := committedFiles(ctx, r)
	if err != nil {
		return nil, err
	}
	return DeriveBaselineFor(ciKind, r, tracked, content)
}
