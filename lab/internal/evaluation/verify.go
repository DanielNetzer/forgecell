package evaluation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// Verification is separate from coding: no ignored files or installed dependencies are reused.
type Verification struct {
	Workspace  string        `json:"workspace"`
	SourceTree string        `json:"sourceTree"`
	Setup      []Observation `json:"setup"`
	Checks     []Observation `json:"checks"`
	Correct    bool          `json:"correct"`
	Error      string        `json:"error,omitempty"`
}

func verify(ctx context.Context, source, destination, acceptance string, p Plan, a Attempt) Verification {
	v := Verification{Workspace: destination, SourceTree: a.Tree, Setup: []Observation{}, Checks: []Observation{}}
	fail := func(err error) Verification { v.Error = err.Error(); return v }
	if !a.ScopeOK || !commitPattern.MatchString(a.Tree) || (a.Molecule.Status != "waiting" && a.Molecule.Status != "recorded") {
		return fail(fmt.Errorf("run did not finish within scope at an acceptable gate"))
	}
	executed := false
	for _, atom := range a.Molecule.Atoms {
		if atom.Status == "failed" || atom.Status == "blocked" {
			return fail(fmt.Errorf("run contains failed or blocked Atoms"))
		}
		if atom.Type == "harness" && atom.Status == "done" {
			executed = true
		}
	}
	if !executed {
		return fail(fmt.Errorf("no successful harness execution"))
	}
	allowed := map[string]bool{}
	for _, f := range p.AllowedFiles {
		allowed[f] = true
	}
	changes := gitResult(ctx, source, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", p.BaseCommit, a.Tree)
	if !changes.OK {
		return fail(fmt.Errorf("cannot inspect captured tree scope"))
	}
	for _, file := range strings.Split(changes.Stdout, "\x00") {
		if file != "" && !allowed[file] {
			return fail(fmt.Errorf("captured tree contains out-of-scope changes: %s", file))
		}
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return fail(err)
	}
	if r := gitResult(ctx, source, "worktree", "add", "--detach", destination, p.BaseCommit); !r.OK {
		return fail(fmt.Errorf("cannot create independent verification checkout: %s", r.Stderr))
	}
	protected := map[string]bool{}
	for _, f := range p.OriginalCheckFiles {
		protected[f] = true
	}
	paths := []string{}
	for _, f := range p.AllowedFiles {
		if !protected[f] {
			paths = append(paths, ":(literal)"+f)
		}
	}
	if len(paths) == 0 {
		return fail(fmt.Errorf("no source paths remain for independent verification"))
	}
	// A source fix may not redirect verification to arbitrary files or submodules.
	allPaths := []string{}
	for _, f := range p.AllowedFiles {
		allPaths = append(allPaths, ":(literal)"+f)
	}
	entries := gitResult(ctx, source, append([]string{"ls-tree", "-r", "-z", a.Tree, "--"}, allPaths...)...)
	if !entries.OK {
		return fail(fmt.Errorf("cannot inspect captured file types"))
	}
	for _, entry := range strings.Split(entries.Stdout, "\x00") {
		if entry != "" && !strings.HasPrefix(entry, "100644 ") && !strings.HasPrefix(entry, "100755 ") {
			return fail(fmt.Errorf("verification accepts regular source files only"))
		}
	}
	diff := gitResult(ctx, source, append([]string{"diff", "--binary", "--no-ext-diff", "--no-textconv", p.BaseCommit, a.Tree, "--"}, paths...)...)
	if !diff.OK {
		return fail(fmt.Errorf("cannot reconstruct captured changes"))
	}
	if diff.Stdout != "" {
		r := process.Run(ctx, process.Options{Argv: []string{"git", "apply", "--binary", "--index", "-"}, Dir: destination, Stdin: []byte(diff.Stdout), Timeout: 30 * time.Second})
		if !r.OK {
			return fail(fmt.Errorf("cannot apply captured source: %s", r.Stderr))
		}
	}
	before := gitResult(ctx, destination, "write-tree")
	if !before.OK {
		return fail(fmt.Errorf("cannot snapshot verification source"))
	}
	for _, c := range p.Setup {
		r := observe(ctx, c, destination, acceptance)
		v.Setup = append(v.Setup, r)
		if !r.Result.OK {
			return fail(fmt.Errorf("verification setup %s failed", c.Name))
		}
	}
	for _, c := range p.Checks {
		raw, err := bounded(acceptance)
		if err != nil || Hash(raw) != p.AcceptanceSHA256 {
			return fail(fmt.Errorf("independent checker changed"))
		}
		v.Checks = append(v.Checks, observe(ctx, c, destination, acceptance))
	}
	// Verification may build ignored outputs, but must not rewrite tracked source/tests.
	clean := gitResult(ctx, destination, "diff", "--exit-code")
	if !clean.OK {
		return fail(fmt.Errorf("verification modified tracked content"))
	}
	after := gitResult(ctx, destination, "write-tree")
	if !after.OK || after.Stdout != before.Stdout {
		return fail(fmt.Errorf("verification modified staged content"))
	}
	raw, err := bounded(acceptance)
	if err != nil || Hash(raw) != p.AcceptanceSHA256 {
		return fail(fmt.Errorf("independent checker changed"))
	}
	v.Correct = len(v.Checks) == len(p.Checks)
	for _, r := range v.Checks {
		if !r.Result.OK {
			v.Correct = false
		}
	}
	return v
}
