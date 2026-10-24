package verification

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// Selection is how a check that declares select ran: the affected packages of the
// captured change, or the full suite and why. It survives the compact ledger
// projection that drops captured streams.
type Selection struct {
	Mode string `json:"mode"`
	// Selected and Total count the module's packages that ran and that exist; both are
	// zero for a full-suite fallback decided before go list ran.
	Selected int `json:"selected,omitempty"`
	Total    int `json:"total,omitempty"`
	// Packages are the import paths that replaced ./..., sorted. Empty when every
	// package is affected, which keeps ./... itself.
	Packages []string `json:"packages,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// SelectionFullSuite is the Mode of a check that fell back to ./... .
const SelectionFullSuite = "full-suite"

// Line is the record and ledger statement of the selection.
func (s Selection) Line() string {
	if s.Mode == readiness.SelectAffectedPackages {
		return fmt.Sprintf("selection: affected-packages (%d of %d packages)", s.Selected, s.Total)
	}
	return "selection: full suite (reason: " + s.Reason + ")"
}

// Argv replaces the trailing ./... of a validated selecting argv with the selected
// packages. A full suite, or a selection of every package, keeps ./... .
func (s Selection) Argv(argv []string) []string {
	if s.Mode != readiness.SelectAffectedPackages || len(s.Packages) == 0 || len(argv) == 0 || argv[len(argv)-1] != "./..." {
		return argv
	}
	return append(append([]string{}, argv[:len(argv)-1]...), s.Packages...)
}

func fullSuite(reason string) Selection {
	return Selection{Mode: SelectionFullSuite, Reason: reason}
}

// Bounds on go list. Hitting any of them falls back to the full suite.
const (
	maxGoListBytes    = 32 << 20
	maxGoListTimeout  = 5 * time.Minute
	maxGoListEntries  = 20_000
	maxModulePackages = 2_000
	maxImportPathLen  = 512
	// maxSelectedArgvBytes keeps the selected import paths well under the exec argument
	// limit (E2BIG); a larger selection runs the full suite instead.
	maxSelectedArgvBytes = 128 << 10
)

// runGoList is the go list runner; tests replace it to inject failures.
var runGoList = process.Run

var importPathPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~+/-]*$`)

// selectionKey carries the selections the candidate view computed, so the protected
// view of the same captured change runs the identical package set.
type selectionKey struct{}

type selectionMemo struct {
	mu    sync.Mutex
	byDir map[string]Selection
}

func (m *selectionMemo) get(dir string) (Selection, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byDir[dir]
	return s, ok
}

func (m *selectionMemo) put(dir string, s Selection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byDir[dir] = s
}

func withSelectionMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, selectionKey{}, &selectionMemo{byDir: map[string]Selection{}})
}

// shownPath bounds and quotes a repository path for a selection reason.
func shownPath(file string) string {
	if len(file) > 200 {
		file = file[:200] + "…"
	}
	return fmt.Sprintf("%q", file)
}

// moduleRel returns file relative to the module directory dir ("." for the root).
func moduleRel(dir, file string) (string, bool) {
	if dir == "." {
		return file, true
	}
	return strings.CutPrefix(file, dir+"/")
}

// ignoredByGo reports whether Go ignores the module-relative directory dir.
func ignoredByGo(dir string) bool {
	return dir != "." && goIgnoredDir(dir)
}

// goPackageDirs returns the module-relative directories holding .go files that ./...
// can match.
func goPackageDirs(dir string, files []string) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		rel, ok := moduleRel(dir, f)
		if !ok || !strings.HasSuffix(rel, ".go") {
			continue
		}
		if d := path.Dir(rel); !ignoredByGo(d) {
			out[d] = true
		}
	}
	return out
}

// preselectReason applies every fallback rule that needs no go list: a change outside
// the module, under cmd/ or a fullSuiteTriggers match, to go.mod or go.sum, to testdata,
// to any non-Go file, to a Go file Go ignores, or a package added or removed. It
// returns the reason, or "" when only Go files of existing packages changed.
func preselectReason(dir string, changed, baseFiles, treeFiles, triggers []string) string {
	if len(changed) == 0 {
		return "no changed files"
	}
	sorted := append([]string{}, changed...)
	sort.Strings(sorted)
	for _, f := range sorted {
		rel, ok := moduleRel(dir, f)
		if !ok {
			return "changed file outside the module: " + shownPath(f)
		}
		elems := strings.Split(rel, "/")
		for _, e := range elems[:len(elems)-1] {
			if e == "cmd" {
				return "changed file under cmd/: " + shownPath(f)
			}
		}
		for _, t := range triggers {
			if readiness.MatchFullSuiteTrigger(t, f) {
				return "changed file matches fullSuiteTriggers " + shownPath(t) + ": " + shownPath(f)
			}
		}
		switch path.Base(rel) {
		case "go.mod", "go.sum", "go.work", "go.work.sum":
			return "module file changed: " + shownPath(f)
		}
		for _, e := range elems[:len(elems)-1] {
			if e == "testdata" {
				return "testdata file changed: " + shownPath(f)
			}
		}
		if !strings.HasSuffix(rel, ".go") {
			return "non-Go file changed: " + shownPath(f)
		}
		if ignoredByGo(path.Dir(rel)) {
			return "Go file in a directory Go ignores changed: " + shownPath(f)
		}
	}
	base, tree := goPackageDirs(dir, baseFiles), goPackageDirs(dir, treeFiles)
	if len(base) != len(tree) {
		return "the change adds or removes a package"
	}
	for d := range tree {
		if !base[d] {
			return "the change adds or removes a package"
		}
	}
	return ""
}

type goListModule struct {
	Dir  string
	Main bool
}

type goListEntry struct {
	ImportPath      string
	Dir             string
	ForTest         string
	Deps            []string
	EmbedFiles      []string
	TestEmbedFiles  []string
	XTestEmbedFiles []string
	Error           *struct{ Err string }
	DepsErrors      []json.RawMessage
	Module          *goListModule
}

// goListFields is the -json field list the selection reads.
const goListFields = "ImportPath,Dir,ForTest,Deps,EmbedFiles,TestEmbedFiles,XTestEmbedFiles,Error,DepsErrors,Module"

// decodeGoList reads go list -json output: a bounded stream of JSON objects.
func decodeGoList(stdout string) ([]goListEntry, error) {
	dec := json.NewDecoder(strings.NewReader(stdout))
	var out []goListEntry
	for {
		var e goListEntry
		err := dec.Decode(&e)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("go list output is not JSON")
		}
		if len(out) == maxGoListEntries {
			return nil, fmt.Errorf("go list returned more than %d entries", maxGoListEntries)
		}
		out = append(out, e)
	}
}

// basePath is an import path without its test-variant suffix (" [p.test]").
func basePath(importPath string) string {
	p, _, _ := strings.Cut(importPath, " ")
	return p
}

// selectFromListing applies the reverse-dependency rule to go list -test output: the
// changed packages (the directories of the changed .go files) plus every module
// package whose own, test or external-test dependencies include one of them.
func selectFromListing(dir string, changed []string, entries []goListEntry) Selection {
	module := ""
	byDir := map[string]string{}
	pkgs := map[string]bool{}
	forTest := map[string]bool{}
	for _, e := range entries {
		if e.Error != nil || len(e.DepsErrors) > 0 {
			return fullSuite("go list reported a package error")
		}
		if e.ForTest != "" {
			forTest[e.ForTest] = true
		}
	}
	isTestMain := func(e goListEntry) bool {
		trimmed, ok := strings.CutSuffix(e.ImportPath, ".test")
		return ok && e.ForTest == "" && forTest[trimmed]
	}
	// owner is the module package an entry belongs to: itself, the package a test
	// variant is built for, or the package of a test main.
	owner := func(e goListEntry) string {
		switch {
		case e.ForTest != "":
			return e.ForTest
		case isTestMain(e):
			return strings.TrimSuffix(e.ImportPath, ".test")
		}
		return basePath(e.ImportPath)
	}
	for _, e := range entries {
		if e.ForTest != "" || isTestMain(e) {
			continue
		}
		if e.Module == nil || !e.Module.Main || e.Module.Dir == "" || e.Dir == "" {
			return fullSuite("go list returned a package outside the main module")
		}
		if module == "" {
			module = e.Module.Dir
		} else if module != e.Module.Dir {
			return fullSuite("go list returned packages of more than one module")
		}
		if len(e.ImportPath) > maxImportPathLen || !importPathPattern.MatchString(e.ImportPath) || strings.Contains(e.ImportPath, "..") || pkgs[e.ImportPath] {
			return fullSuite("go list returned an unexpected import path")
		}
		rel, err := filepath.Rel(module, e.Dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fullSuite("go list returned a package directory outside the module")
		}
		pkgs[e.ImportPath] = true
		byDir[path.Join(dir, filepath.ToSlash(rel))] = e.ImportPath
	}
	if len(pkgs) == 0 {
		return fullSuite("go list returned no packages")
	}
	if len(pkgs) > maxModulePackages {
		return fullSuite(fmt.Sprintf("the module has more than %d packages", maxModulePackages))
	}
	dirOf := map[string]string{}
	for d, ip := range byDir {
		dirOf[ip] = d
	}
	changedFiles := map[string]bool{}
	for _, f := range changed {
		changedFiles[f] = true
	}
	for _, e := range entries {
		ownerDir, ok := dirOf[owner(e)]
		if !ok {
			continue
		}
		for _, files := range [][]string{e.EmbedFiles, e.TestEmbedFiles, e.XTestEmbedFiles} {
			for _, f := range files {
				if file := path.Join(ownerDir, filepath.ToSlash(f)); changedFiles[file] {
					return fullSuite("embedded file changed: " + shownPath(file))
				}
			}
		}
	}
	changedPkgs := map[string]bool{}
	sorted := append([]string{}, changed...)
	sort.Strings(sorted)
	for _, f := range sorted {
		ip, ok := byDir[path.Dir(f)]
		if !ok {
			return fullSuite("changed Go file is not in a listed package: " + shownPath(f))
		}
		changedPkgs[ip] = true
	}
	if len(changedPkgs) == 0 {
		return fullSuite("the affected set is empty while Go files changed")
	}
	affected := map[string]bool{}
	for _, e := range entries {
		o := owner(e)
		if !pkgs[o] {
			continue
		}
		hit := changedPkgs[basePath(e.ImportPath)]
		for _, dep := range e.Deps {
			if hit {
				break
			}
			hit = changedPkgs[basePath(dep)]
		}
		if hit {
			affected[o] = true
		}
	}
	if len(affected) == 0 {
		return fullSuite("the affected set is empty while Go files changed")
	}
	s := Selection{Mode: readiness.SelectAffectedPackages, Selected: len(affected), Total: len(pkgs)}
	if len(affected) < len(pkgs) {
		s.Packages = sortedPackages(affected)
		size := 0
		for _, p := range s.Packages {
			size += len(p) + 1
		}
		if size > maxSelectedArgvBytes {
			return fullSuite(fmt.Sprintf("the selected package list exceeds %d bytes of argv", maxSelectedArgvBytes))
		}
	}
	return s
}

// computeSelection decides the package set of a selecting check that runs in dir
// (repository-relative) of workspace, for the change from base to tree. go list runs
// with the check's own environment and bounded time and output; any failure, bound or
// rule it cannot prove falls back to the full suite.
func computeSelection(ctx context.Context, source, base, tree, workspace, dir string, env, triggers []string) Selection {
	changedRaw, err := git(ctx, source, "", nil, "diff-tree", "--no-commit-id", "--no-renames", "--name-only", "-r", "-z", base, tree, "--")
	if err != nil {
		return fullSuite("the changed files could not be listed")
	}
	baseRaw, err := git(ctx, source, "", nil, "ls-tree", "-r", "--name-only", "-z", base)
	if err != nil {
		return fullSuite("the base files could not be listed")
	}
	treeRaw, err := git(ctx, source, "", nil, "ls-tree", "-r", "--name-only", "-z", tree)
	if err != nil {
		return fullSuite("the candidate files could not be listed")
	}
	changed, treeFiles := paths(changedRaw), paths(treeRaw)
	if reason := preselectReason(dir, changed, paths(baseRaw), treeFiles, triggers); reason != "" {
		return fullSuite(reason)
	}
	root := false
	for _, f := range treeFiles {
		if f == path.Join(dir, "go.mod") {
			root = true
		}
	}
	if !root {
		return fullSuite("the check directory is not a Go module root")
	}
	timeout := maxGoListTimeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
	}
	if timeout <= 0 {
		return fullSuite("no time was left for go list")
	}
	r := runGoList(ctx, process.Options{Argv: []string{"go", "list", "-test", "-json=" + goListFields, "./..."}, Dir: filepath.Join(workspace, filepath.FromSlash(dir)), Env: env, Stdin: []byte{}, Timeout: timeout, MaxOutputBytes: maxGoListBytes})
	switch {
	case r.TimedOut || r.ParentDeadline:
		return fullSuite("go list timed out")
	case r.Overflow || r.Truncated:
		return fullSuite(fmt.Sprintf("go list output exceeded %d bytes", maxGoListBytes))
	case r.Interrupted:
		return fullSuite("go list was interrupted")
	case !r.OK:
		return fullSuite(fmt.Sprintf("go list failed (exit %d)", r.Code))
	}
	entries, err := decodeGoList(r.Stdout)
	if err != nil {
		return fullSuite(err.Error())
	}
	return selectFromListing(dir, changed, entries)
}
