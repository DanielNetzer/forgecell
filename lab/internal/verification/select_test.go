package verification

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// affectedModule is a fixture module under mod/: c imports b, b imports a, d's
// external test imports a (a test-only import), e embeds data.txt and is unrelated,
// and cmd/tool is a command.
var affectedModule = map[string]string{
	"README.md":            "fixture\n",
	"mod/go.mod":           "module example.test/m\n\ngo 1.21\n",
	"mod/a/a.go":           "package a\n\nfunc A() int { return 1 }\n",
	"mod/b/b.go":           "package b\n\nimport \"example.test/m/a\"\n\nfunc B() int { return a.A() + 1 }\n",
	"mod/c/c.go":           "package c\n\nimport \"example.test/m/b\"\n\nfunc C() int { return b.B() + 1 }\n",
	"mod/c/c_test.go":      "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {\n\tif C() != 3 {\n\t\tt.Fatal(C())\n\t}\n}\n",
	"mod/d/d.go":           "package d\n\nfunc D() int { return 4 }\n",
	"mod/d/d_test.go":      "package d_test\n\nimport (\n\t\"testing\"\n\n\t\"example.test/m/a\"\n\t\"example.test/m/d\"\n)\n\nfunc TestD(t *testing.T) {\n\tif d.D() != 4 || a.A() != 1 {\n\t\tt.Fatal()\n\t}\n}\n",
	"mod/e/e.go":           "package e\n\nimport _ \"embed\"\n\n//go:embed data.txt\nvar Data string\n",
	"mod/e/data.txt":       "data\n",
	"mod/a/testdata/x.txt": "x\n",
	"mod/cmd/tool/main.go": "package main\n\nfunc main() {}\n",
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for file, data := range files {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(file)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// affectedFixture commits affectedModule and returns the repository and base commit.
func affectedFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-b", "main")
	gitTest(t, dir, "config", "user.email", "test@example.test")
	gitTest(t, dir, "config", "user.name", "Test")
	writeFiles(t, dir, affectedModule)
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "base")
	return dir, gitTest(t, dir, "rev-parse", "HEAD")
}

// goListTestEnv is a go environment whose caches and home are private temp dirs, so
// go list writes nothing into the user's real home.
func goListTestEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "GOCACHE=" + filepath.Join(home, "cache"), "GOPATH=" + filepath.Join(home, "gopath"), "GOMODCACHE=" + filepath.Join(home, "modcache"), "GOFLAGS=", "GOTOOLCHAIN=local", "GOPROXY=off", "GOWORK=off", "GOENV=off", "CGO_ENABLED=0"}
}

// selectAfter applies the edits (a nil value removes the file) to the fixture
// worktree, stages them and returns the selection of a check in dir.
func selectAfter(t *testing.T, edits map[string]*string, dir string, triggers []string) Selection {
	t.Helper()
	repo, base := affectedFixture(t)
	for file, data := range edits {
		if data == nil {
			if err := os.Remove(filepath.Join(repo, file)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		writeFiles(t, repo, map[string]string{file: *data})
	}
	gitTest(t, repo, "add", "-A")
	tree := gitTest(t, repo, "write-tree")
	return computeSelection(context.Background(), repo, base, tree, repo, dir, goListTestEnv(t), triggers)
}

func text(s string) *string { return &s }

func TestAffectedChangeInLeafSelectsOnlyIt(t *testing.T) {
	s := selectAfter(t, map[string]*string{"mod/c/c.go": text(affectedModule["mod/c/c.go"] + "\nfunc C2() int { return 0 }\n")}, "mod", nil)
	want := Selection{Mode: readiness.SelectAffectedPackages, Selected: 1, Total: 6, Packages: []string{"example.test/m/c"}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("%+v", s)
	}
	if s.Line() != "selection: affected-packages (1 of 6 packages)" {
		t.Fatal(s.Line())
	}
	if got := s.Argv([]string{"go", "test", "-json", "./..."}); !reflect.DeepEqual(got, []string{"go", "test", "-json", "example.test/m/c"}) {
		t.Fatal(got)
	}
}

func TestAffectedChangeInRootSelectsReverseDependenciesAndTestImports(t *testing.T) {
	s := selectAfter(t, map[string]*string{"mod/a/a.go": text("package a\n\nfunc A() int { return 0 + 1 }\n")}, "mod", nil)
	// b and c import a transitively; d only through its external test.
	want := []string{"example.test/m/a", "example.test/m/b", "example.test/m/c", "example.test/m/d"}
	if s.Mode != readiness.SelectAffectedPackages || s.Selected != 4 || s.Total != 6 || !reflect.DeepEqual(s.Packages, want) {
		t.Fatalf("%+v", s)
	}
}

func TestAffectedTestOnlyImportIsIncluded(t *testing.T) {
	// d's tests import a; a change to d's test file selects d, and a change in b,
	// which nothing tests through, selects b and c but not d.
	s := selectAfter(t, map[string]*string{"mod/b/b.go": text(affectedModule["mod/b/b.go"] + "\nvar X = 1\n")}, "mod", nil)
	if !reflect.DeepEqual(s.Packages, []string{"example.test/m/b", "example.test/m/c"}) {
		t.Fatalf("%+v", s)
	}
	s = selectAfter(t, map[string]*string{"mod/d/d_test.go": text(affectedModule["mod/d/d_test.go"] + "\nvar _ = 1\n")}, "mod", nil)
	if !reflect.DeepEqual(s.Packages, []string{"example.test/m/d"}) {
		t.Fatalf("%+v", s)
	}
}

func TestAffectedEveryPackageKeepsThePattern(t *testing.T) {
	s := Selection{Mode: readiness.SelectAffectedPackages, Selected: 3, Total: 3}
	if got := s.Argv([]string{"go", "test", "./..."}); !reflect.DeepEqual(got, []string{"go", "test", "./..."}) {
		t.Fatal(got)
	}
	full := fullSuite("go list failed (exit 1)")
	if full.Line() != "selection: full suite (reason: go list failed (exit 1))" || !reflect.DeepEqual(full.Argv([]string{"go", "test", "./..."}), []string{"go", "test", "./..."}) {
		t.Fatal(full.Line())
	}
}

func TestFullSuiteFallbackTriggers(t *testing.T) {
	cases := []struct {
		name     string
		edits    map[string]*string
		dir      string
		triggers []string
		reason   string
	}{
		{"go.mod", map[string]*string{"mod/go.mod": text("module example.test/m\n\ngo 1.22\n")}, "mod", nil, `module file changed: "mod/go.mod"`},
		{"go.sum", map[string]*string{"mod/go.sum": text("\n")}, "mod", nil, `module file changed: "mod/go.sum"`},
		{"testdata", map[string]*string{"mod/a/testdata/x.txt": text("y\n")}, "mod", nil, `testdata file changed: "mod/a/testdata/x.txt"`},
		{"embedded file", map[string]*string{"mod/e/data.txt": text("other\n")}, "mod", nil, `non-Go file changed: "mod/e/data.txt"`},
		{"cmd", map[string]*string{"mod/cmd/tool/main.go": text("package main\n\nfunc main() { _ = 1 }\n")}, "mod", nil, `changed file under cmd/: "mod/cmd/tool/main.go"`},
		{"fullSuiteTriggers", map[string]*string{"mod/c/c.go": text(affectedModule["mod/c/c.go"] + "\n")}, "mod", []string{"mod/c/**"}, `changed file matches fullSuiteTriggers "mod/c/**": "mod/c/c.go"`},
		{"Go file outside the module", map[string]*string{"mod/c/c.go": text(affectedModule["mod/c/c.go"] + "\n"), "lab/cmd/x.go": text("package x\n")}, "mod", []string{"lab/cmd/**"}, `changed file outside the module: "lab/cmd/x.go"`},
		{"added package", map[string]*string{"mod/f/f.go": text("package f\n")}, "mod", nil, "the change adds or removes a package"},
		{"removed package", map[string]*string{"mod/d/d.go": nil, "mod/d/d_test.go": nil}, "mod", nil, "the change adds or removes a package"},
		{"outside the module", map[string]*string{"README.md": text("changed\n")}, "mod", nil, `changed file outside the module: "README.md"`},
		{"script in the module", map[string]*string{"mod/run.sh": text("#!/bin/sh\n")}, "mod", nil, `non-Go file changed: "mod/run.sh"`},
		{"ignored directory", map[string]*string{"mod/_old/x.go": text("package old\n")}, "mod", nil, `Go file in a directory Go ignores changed: "mod/_old/x.go"`},
		{"not a module root", map[string]*string{"mod/c/c.go": text(affectedModule["mod/c/c.go"] + "\n")}, ".", nil, "the check directory is not a Go module root"},
		{"no change", map[string]*string{}, "mod", nil, "no changed files"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := selectAfter(t, tc.edits, tc.dir, tc.triggers)
			if s.Mode != SelectionFullSuite || s.Reason != tc.reason || len(s.Packages) != 0 {
				t.Fatalf("%+v, want reason %q", s, tc.reason)
			}
			if !strings.HasPrefix(s.Line(), "selection: full suite (reason: ") {
				t.Fatal(s.Line())
			}
		})
	}
}

func TestFullSuiteWhenGoListFails(t *testing.T) {
	cases := []struct {
		result process.Result
		reason string
	}{
		{process.Result{OK: false, Code: 1, Stderr: "/Users/someone/private: cannot find package"}, "go list failed (exit 1)"},
		{process.Result{TimedOut: true}, "go list timed out"},
		{process.Result{Overflow: true}, "go list output exceeded 33554432 bytes"},
		{process.Result{Interrupted: true}, "go list was interrupted"},
		{process.Result{OK: true, Stdout: "not json"}, "go list output is not JSON"},
		{process.Result{OK: true, Stdout: ""}, "go list returned no packages"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			original := runGoList
			defer func() { runGoList = original }()
			calls := 0
			runGoList = func(_ context.Context, o process.Options) process.Result {
				calls++
				if len(o.Argv) < 2 || o.Argv[0] != "go" || o.Argv[1] != "list" || o.MaxOutputBytes != maxGoListBytes || o.Timeout <= 0 || o.Timeout > maxGoListTimeout {
					t.Errorf("unbounded or unexpected go list: %+v", o)
				}
				return tc.result
			}
			s := selectAfter(t, map[string]*string{"mod/c/c.go": text(affectedModule["mod/c/c.go"] + "\n")}, "mod", nil)
			if calls != 1 || s.Mode != SelectionFullSuite || s.Reason != tc.reason {
				t.Fatalf("calls %d: %+v", calls, s)
			}
			if strings.Contains(s.Line(), "/Users/") {
				t.Fatal("a local path leaked into the selection")
			}
		})
	}
}

func TestFullSuiteFromHostileListing(t *testing.T) {
	main := &goListModule{Dir: "/w/mod", Main: true}
	pkg := func(ip, dir string) goListEntry { return goListEntry{ImportPath: ip, Dir: dir, Module: main} }
	changed := []string{"mod/a/a.go"}
	cases := []struct {
		name    string
		entries []goListEntry
		reason  string
	}{
		{"package error", []goListEntry{{ImportPath: "example.test/m/a", Dir: "/w/mod/a", Module: main, Error: &struct{ Err string }{"bad"}}}, "go list reported a package error"},
		{"other module", []goListEntry{pkg("example.test/m/a", "/w/mod/a"), {ImportPath: "x/y", Dir: "/w/y", Module: &goListModule{Dir: "/w/y", Main: true}}}, "go list returned packages of more than one module"},
		{"dependency module", []goListEntry{{ImportPath: "x/y", Dir: "/w/y", Module: &goListModule{Dir: "/w/y"}}}, "go list returned a package outside the main module"},
		{"flag-like import path", []goListEntry{pkg("-exec=evil", "/w/mod/a")}, "go list returned an unexpected import path"},
		{"duplicate import path", []goListEntry{pkg("example.test/m/a", "/w/mod/a"), pkg("example.test/m/a", "/w/mod/b")}, "go list returned an unexpected import path"},
		{"directory outside", []goListEntry{pkg("example.test/m/a", "/w/other")}, "go list returned a package directory outside the module"},
		{"unlisted package", []goListEntry{pkg("example.test/m/b", "/w/mod/b")}, `changed Go file is not in a listed package: "mod/a/a.go"`},
		{"embedded Go file", []goListEntry{{ImportPath: "example.test/m/a", Dir: "/w/mod/a", Module: main, EmbedFiles: []string{"a.go"}}}, `embedded file changed: "mod/a/a.go"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if s := selectFromListing("mod", changed, tc.entries); s.Mode != SelectionFullSuite || s.Reason != tc.reason {
				t.Fatalf("%+v, want %q", s, tc.reason)
			}
		})
	}
	many := []goListEntry{}
	for i := 0; i <= maxModulePackages; i++ {
		many = append(many, pkg(fmt.Sprintf("example.test/m/p%d", i), fmt.Sprintf("/w/mod/p%d", i)))
	}
	if s := selectFromListing("mod", changed, many); s.Mode != SelectionFullSuite {
		t.Fatalf("over-bound module accepted: %+v", s.Line())
	}
	if _, err := decodeGoList(strings.Repeat("{}", maxGoListEntries+1)); err == nil {
		t.Fatal("over-bound listing accepted")
	}
}

func TestFullSuiteWhenSelectedArgvIsTooLarge(t *testing.T) {
	main := &goListModule{Dir: "/w/mod", Main: true}
	listing := func(dependents int) []goListEntry {
		entries := []goListEntry{
			{ImportPath: "example.test/m/a", Dir: "/w/mod/a", Module: main},
			{ImportPath: "example.test/m/z", Dir: "/w/mod/z", Module: main},
		}
		for i := 0; i < dependents; i++ {
			ip := fmt.Sprintf("example.test/m/%s/p%d", strings.Repeat("x", 400), i)
			entries = append(entries, goListEntry{ImportPath: ip, Dir: fmt.Sprintf("/w/mod/p%d", i), Module: main, Deps: []string{"example.test/m/a"}})
		}
		return entries
	}
	changed := []string{"mod/a/a.go"}
	if s := selectFromListing("mod", changed, listing(10)); s.Mode != readiness.SelectAffectedPackages || s.Selected != 11 {
		t.Fatalf("a small selection fell back: %+v", s)
	}
	dependents := maxSelectedArgvBytes/400 + 1
	s := selectFromListing("mod", changed, listing(dependents))
	if s.Mode != SelectionFullSuite || !strings.Contains(s.Reason, "bytes of argv") {
		t.Fatalf("an oversized selection was not replaced by the full suite: %s", s.Line())
	}
	argv := []string{"go", "test", "./..."}
	if got := s.Argv(argv); !reflect.DeepEqual(got, argv) {
		t.Fatalf("argv = %d elements", len(got))
	}
}
