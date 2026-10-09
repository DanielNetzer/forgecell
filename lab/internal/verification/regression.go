package verification

import (
	"context"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// V1 protects conventional test suites plus every explicitly cited checker.
// This is a preserved regression view, not independent behavioral acceptance.
func regressionPath(file string) bool {
	lower := strings.ToLower(file)
	base := path.Base(lower)
	for _, part := range strings.Split(lower, "/") {
		switch part {
		case "test", "tests", "__tests__", "testdata", "fixtures", "spec", "specs":
			return true
		}
	}
	return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.py") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

func ProtectedTree(ctx context.Context, source string, p readiness.Plan, candidate string) (string, error) {
	protected := map[string]bool{}
	for _, check := range p.Analysis.Checks {
		if check.Category == "regression" {
			for _, id := range check.Definitions {
				for _, e := range p.Evidence {
					if e.ID == id {
						protected[e.Path] = true
					}
				}
			}
		}
	}
	changed, err := git(ctx, source, "", nil, "diff-tree", "--no-commit-id", "--no-renames", "--name-only", "-r", "-z", p.Inputs.BaseCommit, candidate, "--")
	if err != nil {
		return "", err
	}
	files := []string{}
	for _, file := range paths(changed) {
		if protected[file] || regressionPath(file) {
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		return candidate, nil
	}
	temp, err := os.MkdirTemp("", "forgecell-regression-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	index := filepath.Join(temp, "index")
	if _, err = git(ctx, source, index, nil, "read-tree", candidate); err != nil {
		return "", err
	}
	for _, file := range files {
		raw, e := git(ctx, source, "", nil, "ls-tree", "-z", p.Inputs.BaseCommit, "--", file)
		if e != nil {
			return "", e
		}
		if raw == "" {
			_, err = git(ctx, source, index, nil, "update-index", "--force-remove", "--", file)
		} else {
			entries := paths(raw)
			if len(entries) != 1 {
				return "", fmt.Errorf("ambiguous protected test path")
			}
			header, _, ok := strings.Cut(entries[0], "\t")
			parts := strings.Fields(header)
			if !ok || len(parts) != 3 || parts[1] != "blob" {
				return "", fmt.Errorf("unsupported protected test entry")
			}
			_, err = git(ctx, source, index, nil, "update-index", "--add", "--cacheinfo", parts[0], parts[2], file)
		}
		if err != nil {
			return "", err
		}
	}
	tree, err := git(ctx, source, index, nil, "write-tree")
	return strings.TrimSpace(tree), err
}

func Run(ctx context.Context, source, destination string, p readiness.Plan, c CaptureResult) Result {
	return RunObserved(ctx, source, destination, p, c, nil)
}
func RunObserved(ctx context.Context, source, destination string, p readiness.Plan, c CaptureResult, observe func(Observation) error) Result {
	persistenceFailed := false
	if observe != nil {
		original := observe
		observe = func(o Observation) error {
			err := original(o)
			if err != nil {
				persistenceFailed = true
			}
			return err
		}
	}
	// Always retain the candidate result separately, even when old tests fail.
	candidate := runViewObserved(ctx, source, destination, p, c, observe)
	if persistenceFailed {
		return candidate
	}
	if err := p.Validate(); err != nil {
		return candidate
	}
	regression := []readiness.Check{}
	for _, check := range p.Analysis.Checks {
		if check.Category == "regression" {
			regression = append(regression, check)
		}
	}
	if len(regression) == 0 {
		return candidate
	}
	tree, err := ProtectedTree(ctx, source, p, c.Tree)
	if err != nil {
		candidate.Error = err.Error()
		candidate.RequiredChecksPassed = false
		candidate.IndependentAcceptanceVerified = false
		return candidate
	}
	if tree == c.Tree {
		return candidate
	}
	plan := p
	plan.Analysis.Checks = regression
	copy := c
	copy.Tree = tree
	preserved := runViewObserved(ctx, source, destination+"-protected", plan, copy, observe)
	candidate.Protected = &preserved
	if !preserved.RequiredChecksPassed {
		candidate.RequiredChecksPassed = false
		candidate.IndependentAcceptanceVerified = false
		candidate.Error = "protected regression did not pass: " + preserved.Error
	}
	return candidate
}
