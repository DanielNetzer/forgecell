package verification

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

type Observation struct {
	Artifacts  []ArtifactInventory `json:"artifacts,omitempty"`
	ID         string              `json:"id"`
	Category   string              `json:"category"`
	SourceTree string              `json:"sourceTree"`
	Command    readiness.Check     `json:"command"`
	Result     process.Result      `json:"result"`
}
type Result struct {
	Protected                     *Result       `json:"protectedRegression,omitempty"`
	SchemaVersion                 string        `json:"schemaVersion"`
	Workspace                     string        `json:"workspace"`
	SourceTree                    string        `json:"sourceTree"`
	Setup                         []Observation `json:"setup"`
	Checks                        []Observation `json:"checks"`
	RequiredChecksPassed          bool          `json:"requiredChecksPassed"`
	IndependentAcceptanceVerified bool          `json:"independentAcceptanceVerified"`
	Containment                   string        `json:"containment"`
	Error                         string        `json:"error,omitempty"`
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func commandEnvironment(p readiness.ExecutionPolicy, home string) ([]string, []string, error) {
	env := []string{"HOME=" + home, "TMPDIR=" + home, "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	secrets := []string{}
	seen := map[string]bool{}
	credentials := map[string]bool{}
	for _, name := range p.CredentialNames {
		credentials[name] = true
	}
	for _, name := range p.Environment {
		if !envName.MatchString(name) || seen[name] || name == "HOME" || name == "TMPDIR" || name == "ENV" || name == "BASH_ENV" || strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
			return nil, nil, fmt.Errorf("unsupported verification environment name: %s", name)
		}
		seen[name] = true
		value, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		if name == "PATH" {
			env[2] = "PATH=" + value
		} else if name == "LANG" {
			env[3] = "LANG=" + value
		} else {
			env = append(env, name+"="+value)
		}
		if credentials[name] && value != "" {
			secrets = append(secrets, value)
		}
	}
	for name := range credentials {
		if !seen[name] {
			return nil, nil, fmt.Errorf("credential forwarding must also be in the reviewed environment: %s", name)
		}
	}
	return env, secrets, nil
}
func scrub(r process.Result, secrets []string) process.Result {
	for _, secret := range secrets {
		r.Stdout = strings.ReplaceAll(r.Stdout, secret, "[redacted]")
		r.Stderr = strings.ReplaceAll(r.Stderr, secret, "[redacted]")
		r.Error = strings.ReplaceAll(r.Error, secret, "[redacted]")
	}
	return r
}

// Run verifies a captured tree in a new checkout. The caller still owns human
// approval and persistence. Passing commands are evidence, not a correctness oracle.
func runView(ctx context.Context, source, destination string, p readiness.Plan, c CaptureResult) Result {
	return runViewObserved(ctx, source, destination, p, c, nil)
}
func runViewObserved(ctx context.Context, source, destination string, p readiness.Plan, c CaptureResult, observe func(Observation) error) (v Result) {
	v = Result{SchemaVersion: "v1", Workspace: destination, SourceTree: c.Tree, Setup: []Observation{}, Checks: []Observation{}, Containment: "Filtered environment and temporary HOME; no OS filesystem/network sandbox. Network access is unrestricted."}
	fail := func(e error) Result {
		v.Error = e.Error()
		v.RequiredChecksPassed = false
		v.IndependentAcceptanceVerified = false
		return v
	}
	if err := p.Validate(); err != nil {
		return fail(err)
	}
	if !regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`).MatchString(c.Tree) {
		return fail(fmt.Errorf("invalid captured tree identity"))
	}
	if len(c.Violations) > 0 || c.Tree == "" || len(p.Analysis.Checks) == 0 {
		return fail(fmt.Errorf("captured source must be within scope and have required checks"))
	}
	if p.Policy.Containment != "filtered-environment" || p.Policy.SetupNetwork != "unrestricted" || p.Policy.CheckNetwork != "unrestricted" {
		return fail(fmt.Errorf("requested execution containment/network policy is not implemented; no commands executed"))
	}
	allowed := map[string]bool{}
	allowedList := []string{}
	for _, s := range p.Analysis.Scope {
		if err := readiness.ValidatePath(s.Path); err != nil {
			return fail(err)
		}
		allowed[s.Path] = true
		allowedList = append(allowedList, s.Path)
	}
	changed, err := git(ctx, source, "", nil, "diff-tree", "--no-commit-id", "--no-renames", "--name-only", "-r", "-z", p.Inputs.BaseCommit, c.Tree, "--")
	if err != nil {
		return fail(err)
	}
	for _, file := range paths(changed) {
		if !allowed[file] {
			return fail(fmt.Errorf("captured tree includes unapproved path: %s", file))
		}
	}
	refs := map[string]readiness.Evidence{}
	for _, e := range p.Evidence {
		refs[e.ID] = e
	}
	reviewed := map[string]readiness.FileEvidence{}
	for _, input := range p.CheckInputs {
		reviewed[input.ID] = input
	}
	commands := append(append([]readiness.Check{}, p.Setup...), p.Analysis.Checks...)
	required := false
	for _, command := range commands {
		if command.Required && command.Category != "setup" {
			required = true
		}
		if command.Dir != "." {
			if err := readiness.ValidatePath(command.Dir); err != nil {
				return fail(err)
			}
		}
		if len(command.Argv) == 0 || command.TimeoutMS < 1 || command.TimeoutMS > 3600000 || len(command.Definitions) == 0 {
			return fail(fmt.Errorf("command %s has invalid execution parameters", command.ID))
		}
		for _, id := range command.Definitions {
			e, ok := refs[id]
			if !ok {
				return fail(fmt.Errorf("command %s has missing definition %s", command.ID, id))
			}
			if err := readiness.ValidatePath(e.Path); err != nil {
				return fail(err)
			}
			content := ""
			var err error
			if input, ok := reviewed[id]; ok {
				content = input.Content
			} else {
				content, err = git(ctx, source, "", nil, "cat-file", "blob", c.Tree+":"+e.Path)
			}
			if err != nil || fmt.Sprintf("%x", sha256.Sum256([]byte(content))) != e.SHA256 {
				return fail(fmt.Errorf("command definition %s changed; review a scope/check amendment before execution", e.Path))
			}
		}
	}
	if !required {
		return fail(fmt.Errorf("no required verification checks"))
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return fail(fmt.Errorf("verification destination must not exist"))
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return fail(err)
	}
	// --no-checkout avoids smudge filters. Materialize blobs ourselves, after mode
	// and path validation, rather than running candidate-controlled Git filters.
	if _, err := git(ctx, source, "", nil, "worktree", "add", "--no-checkout", "--detach", destination, p.Inputs.BaseCommit); err != nil {
		return fail(err)
	}
	entries, err := git(ctx, source, "", nil, "ls-tree", "-r", "-z", c.Tree)
	if err != nil {
		return fail(err)
	}
	for _, entry := range paths(entries) {
		header, file, ok := strings.Cut(entry, "\t")
		parts := strings.Fields(header)
		if !ok || len(parts) != 3 || (parts[0] != "100644" && parts[0] != "100755") {
			return fail(fmt.Errorf("verification tree must contain regular files only"))
		}
		if err := readiness.ValidatePath(file); err != nil {
			return fail(err)
		}
		data, err := git(ctx, source, "", nil, "cat-file", "blob", parts[2])
		if err != nil {
			return fail(err)
		}
		target := filepath.Join(destination, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return fail(err)
		}
		mode := os.FileMode(0600)
		if parts[0] == "100755" {
			mode = 0700
		}
		if err := os.WriteFile(target, []byte(data), mode); err != nil {
			return fail(err)
		}
	}
	if _, err := git(ctx, destination, "", nil, "read-tree", c.Tree); err != nil {
		return fail(err)
	}
	home, err := os.MkdirTemp("", "forgecell-check-home-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(home)
	env, secrets, err := commandEnvironment(p.Policy, home)
	if err != nil {
		return fail(err)
	}
	controls, err := os.MkdirTemp("", "forgecell-reviewed-checks-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(controls)
	inputPaths := map[string]string{}
	for id, input := range reviewed {
		file := filepath.Join(controls, id)
		if err := os.WriteFile(file, []byte(input.Content), 0400); err != nil {
			return fail(err)
		}
		inputPaths[id] = file
	}
	checkInputs := func() error {
		for id, file := range inputPaths {
			info, err := os.Lstat(file)
			if err != nil || !info.Mode().IsRegular() || info.Size() > 64000 {
				return fmt.Errorf("reviewed check input changed: %s", id)
			}
			data, err := os.ReadFile(file)
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != reviewed[id].SHA256 {
				return fmt.Errorf("reviewed check input changed: %s", id)
			}
		}
		return nil
	}
	for i, command := range commands {
		if err := regularAncestors(destination, filepath.ToSlash(filepath.Join(command.Dir, "placeholder"))); err != nil {
			return fail(err)
		}
		if err := checkInputs(); err != nil {
			return fail(err)
		}
		argv := append([]string{}, command.Argv...)
		for i, arg := range argv {
			if strings.HasPrefix(arg, "{input:") && strings.HasSuffix(arg, "}") {
				id := strings.TrimSuffix(strings.TrimPrefix(arg, "{input:"), "}")
				file, ok := inputPaths[id]
				if !ok {
					return fail(fmt.Errorf("unknown reviewed input %s", id))
				}
				argv[i] = file
			}
		}
		result := process.Run(ctx, process.Options{Argv: argv, Dir: filepath.Join(destination, filepath.FromSlash(command.Dir)), Env: env, Stdin: []byte{}, Timeout: time.Duration(command.TimeoutMS) * time.Millisecond, MaxOutputBytes: 256000})
		observation := Observation{ID: command.ID, Category: command.Category, SourceTree: c.Tree, Command: command, Result: scrub(result, secrets)}
		if i < len(p.Setup) {
			v.Setup = append(v.Setup, observation)
		} else {
			v.Checks = append(v.Checks, observation)
		}
		if observe != nil {
			if err := observe(observation); err != nil {
				return fail(err)
			}
		}
		if err := checkInputs(); err != nil {
			return fail(err)
		}
		// Audit after every command. Artifacts remain bounded exceptions; no source,
		// checker or index mutation can be treated as successful verification.
		after, err := Capture(ctx, destination, p.Inputs.BaseCommit, allowedList, p.Artifacts)
		if err != nil {
			return fail(err)
		}
		if i < len(p.Setup) {
			v.Setup[len(v.Setup)-1].Artifacts = after.Artifacts
		} else {
			v.Checks[len(v.Checks)-1].Artifacts = after.Artifacts
		}
		if after.Tree != c.Tree || len(after.Violations) > 0 {
			return fail(fmt.Errorf("verification command %s modified source or wrote outside artifact allowances", command.ID))
		}
		staged, err := git(ctx, destination, "", nil, "write-tree")
		if err != nil || strings.TrimSpace(staged) != c.Tree {
			return fail(fmt.Errorf("verification changed staged content"))
		}
		if !result.OK && (i < len(p.Setup) || command.Required) {
			return fail(fmt.Errorf("required command %s did not pass", command.ID))
		}
	}
	v.RequiredChecksPassed = true
	independent, allIndependent := false, true
	for _, o := range v.Checks {
		if o.Category == "independent-acceptance" {
			independent = true
			if !o.Result.OK || o.Command.IndependentProvenance == "" {
				allIndependent = false
			}
		}
	}
	v.IndependentAcceptanceVerified = independent && allIndependent
	return v
}
