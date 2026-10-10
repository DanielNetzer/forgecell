package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"path"
	"strings"
	"time"
)

type Component struct {
	Scripts              map[string]string `json:"scripts,omitempty" yaml:"scripts,omitempty"`
	DeclaredDependencies map[string]string `json:"declaredDependencies,omitempty" yaml:"declaredDependencies,omitempty"`
	Path                 string            `json:"path" yaml:"path"`
	Manifest             string            `json:"manifest" yaml:"manifest"`
	SHA256               string            `json:"sha256" yaml:"sha256"`
	Checks               [][]string        `json:"suggestedChecks" yaml:"suggestedChecks"`
}
type Repository struct {
	Evidence       []readiness.FileEvidence `json:"evidence" yaml:"evidence"`
	EvidenceSHA256 string                   `json:"evidenceSha256" yaml:"evidenceSha256"`
	Root           string                   `json:"root" yaml:"root"`
	Repo           string                   `json:"repo" yaml:"repo"`
	Revision       string                   `json:"revision" yaml:"revision"`
	DefaultBranch  string                   `json:"defaultBranch" yaml:"defaultBranch"`
	Components     []Component              `json:"components" yaml:"components"`
	Workflows      []string                 `json:"workflows" yaml:"workflows"`
	PolicyFiles    []string                 `json:"policyFiles" yaml:"policyFiles"`
	Unknowns       []string                 `json:"unknowns" yaml:"unknowns"`
}

func ReadRepository(ctx context.Context, cwd string, runner Runner) (Repository, error) {
	if runner == nil {
		runner = process.Run
	}
	var repo Repository
	call := func(args ...string) (string, error) {
		r := runner(ctx, process.Options{Argv: args, Dir: cwd, Stdin: []byte{}, Timeout: 20 * time.Second, MaxOutputBytes: 2_000_000})
		if !r.OK {
			return "", fmt.Errorf("repository discovery failed for %s: %s %s", args[0], r.Error, r.Stderr)
		}
		if len(args) > 1 && args[0] == "git" && (args[1] == "show" || args[1] == "ls-tree") {
			return r.Stdout, nil
		}
		return strings.TrimSpace(r.Stdout), nil
	}
	root, err := call("git", "rev-parse", "--show-toplevel")
	if err != nil {
		return repo, err
	}
	cwd = root
	repo.Root = root
	repo.Revision, err = call("git", "rev-parse", "HEAD")
	if err != nil {
		return repo, err
	}
	origin, err := call("git", "remote", "get-url", "origin")
	if err != nil {
		return repo, err
	}
	identity, ok := GitHubRepoFromRemote(origin)
	if !ok {
		return repo, fmt.Errorf("origin must identify a GitHub repository")
	}
	repo.Repo = identity
	raw, err := call("gh", "repo", "view", repo.Repo, "--json", "nameWithOwner,defaultBranchRef")
	if err != nil {
		return repo, err
	}
	var info struct {
		NameWithOwner    string `json:"nameWithOwner"`
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if err = json.Unmarshal([]byte(raw), &info); err != nil {
		return repo, err
	}
	if !strings.EqualFold(info.NameWithOwner, repo.Repo) {
		return repo, fmt.Errorf("GitHub repository identity mismatch")
	}
	repo.Repo = info.NameWithOwner
	repo.DefaultBranch = info.DefaultBranchRef.Name
	snapshot, err := readiness.Collect(ctx, cwd, repo.Revision)
	if err != nil {
		return repo, err
	}
	repo.Evidence = snapshot.Evidence
	repo.EvidenceSHA256, err = snapshot.Digest()
	if err != nil {
		return repo, err
	}
	tracked := map[string]bool{}
	for _, file := range snapshot.Files {
		tracked[file.Path] = true
	}
	paths := []string{}
	contents := map[string]string{}
	for _, e := range snapshot.Evidence {
		paths = append(paths, e.Path)
		contents[e.Path] = e.Content
	}
	for _, file := range paths {
		if strings.HasPrefix(file, ".github/workflows/") && (strings.HasSuffix(file, ".yml") || strings.HasSuffix(file, ".yaml")) {
			repo.Workflows = append(repo.Workflows, file)
		}
		if path.Base(file) == "CODEOWNERS" || path.Base(file) == "CONTRIBUTING.md" {
			repo.PolicyFiles = append(repo.PolicyFiles, file)
		}
		base := path.Base(file)
		if base != "package.json" && base != "go.mod" && base != "pyproject.toml" {
			continue
		}
		if strings.Contains("/"+file, "/node_modules/") || strings.Contains("/"+file, "/vendor/") {
			continue
		}
		if len(repo.Components) >= 100 {
			return repo, fmt.Errorf("repository has more than 100 component manifests; explicit scoping is required")
		}
		content := contents[file]
		component := Component{Path: path.Dir(file), Manifest: file, SHA256: hash([]byte(content)), Checks: [][]string{}}
		if base == "go.mod" {
			component.Checks = [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}}
		}
		if base == "package.json" {
			var pkg struct {
				Scripts         map[string]string `json:"scripts"`
				Dependencies    map[string]string `json:"dependencies"`
				DevDependencies map[string]string `json:"devDependencies"`
			}
			if err = json.Unmarshal([]byte(content), &pkg); err != nil {
				return repo, fmt.Errorf("invalid committed manifest %s", file)
			}
			component.Scripts = pkg.Scripts
			component.DeclaredDependencies = map[string]string{}
			for name, spec := range pkg.DevDependencies {
				component.DeclaredDependencies[name] = spec
			}
			for name, spec := range pkg.Dependencies {
				component.DeclaredDependencies[name] = spec
			}
			manager := "npm"
			for _, prefix := range []string{"", path.Dir(file) + "/"} {
				if tracked[prefix+"pnpm-lock.yaml"] {
					manager = "pnpm"
				} else if tracked[prefix+"yarn.lock"] {
					manager = "yarn"
				}
			}
			for _, script := range []string{"test", "typecheck", "build"} {
				if pkg.Scripts[script] != "" {
					component.Checks = append(component.Checks, []string{manager, "run", script})
				}
			}
		}
		repo.Components = append(repo.Components, component)
	}
	repo.Unknowns = []string{"Dependency declarations are recorded for package.json; Go/Python dependency relationships and transitive impact are not inferred.", "Suggested checks are discovered from the committed snapshot and have not been executed.", "Issue scope, acceptance criteria and blast radius require ticket-specific evidence.", "Branch protection, reviewer requirements, deployment health and rollback policy have not been verified.", "Parallel write scopes have not been established; execution remains sequential."}
	return repo, nil
}

// Compact retains provenance and process references without sampled file bodies.
func (r Repository) Compact() map[string]any {
	evidence := []map[string]string{}
	for _, e := range r.Evidence {
		evidence = append(evidence, map[string]string{"path": e.Path, "sha256": e.SHA256})
	}
	components := []Component{}
	for _, c := range r.Components {
		c.Scripts = nil
		c.DeclaredDependencies = nil
		components = append(components, c)
	}
	return map[string]any{"repo": r.Repo, "revision": r.Revision, "defaultBranch": r.DefaultBranch, "evidence": evidence, "evidenceSha256": r.EvidenceSHA256, "components": components, "workflows": r.Workflows, "policyFiles": r.PolicyFiles, "unknowns": r.Unknowns}
}
