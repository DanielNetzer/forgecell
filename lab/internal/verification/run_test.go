package verification

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

func checkPlan(dir, base string) readiness.Plan {
	data, _ := os.ReadFile(filepath.Join(dir, "check.sh"))
	return readiness.Plan{SchemaVersion: "v1", MoleculeID: "mol-test", Revision: 1, Inputs: readiness.Inputs{Repository: "owner/repo", TargetBranch: "main", BaseCommit: base, FormulaSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64), BindingSHA256: strings.Repeat("c", 64), Issue: readiness.Issue{Repository: "owner/repo", Number: 1, URL: "https://github.com/owner/repo/issues/1", State: "OPEN", Title: "Update code"}}, Evidence: []readiness.Evidence{{ID: "check", Path: "check.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}}, Analysis: readiness.Analysis{Summary: "Update code", Acceptance: []readiness.Criterion{{Description: "Code returns changed", Evidence: []string{"issue"}}}, Scope: []readiness.ScopedPath{{Path: "code.txt", Reason: "Implementation", Evidence: []string{"issue"}}}, Checks: []readiness.Check{{ID: "regression", Category: "regression", Dir: ".", Argv: []string{"/bin/sh", "check.sh"}, TimeoutMS: 1000, Required: true, Reason: "Verify implementation", Evidence: []string{"check"}, Definitions: []string{"check"}}}}, Policy: readiness.ExecutionPolicy{Environment: []string{"PATH"}, SetupNetwork: "unrestricted", CheckNetwork: "unrestricted", Containment: "filtered-environment"}}
}
func TestFreshVerificationDoesNotReuseIgnoredFilesOrSecrets(t *testing.T) {
	dir, base := sourceFixture(t)
	// Put the checker in the reviewed baseline so candidate checks cannot invent
	// their own oracle. Existing ignored content and host credentials must not leak.
	script := "#!/bin/sh\ntest ! -e ignored.txt && test -z \"$FORGECELL_TEST_SECRET\" && test \"$(cat code.txt)\" = changed\n"
	os.WriteFile(filepath.Join(dir, "check.sh"), []byte(script), 0600)
	gitTest(t, dir, "add", "check.sh")
	gitTest(t, dir, "commit", "-m", "acceptance")
	base = gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	os.WriteFile(filepath.Join(dir, "code.txt"), []byte("changed"), 0600)
	capture, err := Capture(context.Background(), dir, base, []string{"code.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("coding checkout only"), 0600)
	t.Setenv("FORGECELL_TEST_SECRET", "not-forwarded")
	dest := filepath.Join(t.TempDir(), "verification")
	result := Run(context.Background(), dir, dest, p, capture)
	if !result.RequiredChecksPassed || result.SourceTree != capture.Tree || len(result.Checks) != 1 {
		t.Fatalf("%+v", result)
	}
	if result.IndependentAcceptanceVerified {
		t.Fatal("regression promoted to independent acceptance")
	}
	if _, err := os.Stat(filepath.Join(dest, "ignored.txt")); !os.IsNotExist(err) {
		t.Fatal("ignored coding data copied")
	}
}

// envCheck runs one verification whose check prints the Go-related environment it got.
// files are added to the base commit (go.mod for a module root, vendor/modules.txt for a
// vendored module), and the change edits code.txt. The views use a private cache.
func envCheck(t *testing.T, files map[string]string, forward []string) (map[string]string, Observation) {
	t.Helper()
	dir, _ := sourceFixture(t)
	files["check.sh"] = "#!/bin/sh\nfor k in HOME TMPDIR GOTMPDIR GOCACHE GOMODCACHE GOPROXY GOFLAGS GOTOOLCHAIN; do eval \"echo $k=\\$$k\"; done\n"
	for file, data := range files {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(file)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "env")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	p.Policy.Environment = append([]string{"PATH"}, forward...)
	os.WriteFile(filepath.Join(dir, "code.txt"), []byte("changed"), 0600)
	capture, err := Capture(context.Background(), dir, base, []string{"code.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	ctx := resources.WithGoCache(resources.WithCheckTempDir(context.Background(), tmp), resources.GoCache{ModCache: "/private/mod", BuildCache: "/private/build"})
	result := Run(resources.WithGoParallelism(ctx, 3), dir, filepath.Join(t.TempDir(), "verification"), p, capture)
	if !result.RequiredChecksPassed || len(result.Checks) != 1 {
		t.Fatalf("%+v", result)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(result.Checks[0].Result.Stdout), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	if got["TMPDIR"] != tmp || got["HOME"] == tmp || !strings.Contains(got["HOME"], "forgecell-check-home-") {
		t.Fatalf("TMPDIR must be the stable one and HOME private: %v", got)
	}
	if !strings.Contains(got["GOTMPDIR"], "forgecell-gotmp-") || strings.HasPrefix(got["GOTMPDIR"], tmp) {
		t.Fatalf("GOTMPDIR must be a per-view directory outside the stable TMPDIR: %v", got)
	}
	if _, err := os.Lstat(got["GOTMPDIR"]); !os.IsNotExist(err) {
		t.Fatalf("per-view GOTMPDIR %s was not removed: %v", got["GOTMPDIR"], err)
	}
	return got, result.Checks[0]
}

// TestCheckEnvironmentCarriesSharedGoCache asserts that a view's check environment holds
// the cache ctx resolves (never under the private HOME), GOPROXY=off, GOTOOLCHAIN=local
// and GOFLAGS with -trimpath merged before the cohort's -p, while TMPDIR is the stable one
// ctx carries, HOME stays private and Go's work directories go to a per-view GOTMPDIR. A
// forwarded -mod is kept, and a non-go argv records no cache state.
func TestCheckEnvironmentCarriesSharedGoCache(t *testing.T) {
	t.Setenv("GOCACHE", "")
	os.Unsetenv("GOCACHE")
	t.Setenv("GOFLAGS", "-mod=mod")
	got, o := envCheck(t, map[string]string{"go.mod": "module example.test/env\n\ngo 1.22\n"}, []string{"GOFLAGS"})
	if got["GOCACHE"] != "/private/build" || got["GOMODCACHE"] != "/private/mod" {
		t.Fatalf("shared cache not passed: %v", got)
	}
	if strings.HasPrefix(got["GOCACHE"], got["HOME"]) || strings.HasPrefix(got["GOMODCACHE"], got["HOME"]) {
		t.Fatalf("cache fell back under the private HOME: %v", got)
	}
	if got["GOPROXY"] != "off" || got["GOTOOLCHAIN"] != "local" || got["GOFLAGS"] != "-trimpath -mod=mod -p=3" {
		t.Fatalf("go settings: %v", got)
	}
	if o.Cache != "" || len(o.GoEnvForwarded) != 0 {
		t.Fatalf("a /bin/sh check records cache %q forwarded %v", o.Cache, o.GoEnvForwarded)
	}
}

// TestVendoredModuleIsNotForcedReadonly: no -mod is added, so a module with
// vendor/modules.txt keeps Go's default vendor mode.
func TestVendoredModuleIsNotForcedReadonly(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	got, _ := envCheck(t, map[string]string{"go.mod": "module example.test/env\n\ngo 1.22\n", "vendor/modules.txt": "# example.test/dep v1.0.0\n"}, nil)
	if got["GOFLAGS"] != "-trimpath -p=3" {
		t.Fatalf("vendored module GOFLAGS: %v", got)
	}
}

// TestPolicyForwardedGoSettingsWin: a reviewed policy that forwards GOPROXY, GOTOOLCHAIN
// and GOFLAGS=-trimpath=false keeps them, and the observation records the override.
func TestPolicyForwardedGoSettingsWin(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.example.test")
	t.Setenv("GOTOOLCHAIN", "auto")
	t.Setenv("GOFLAGS", "-trimpath=false")
	got, o := envCheck(t, map[string]string{"go.mod": "module example.test/env\n\ngo 1.22\n"}, []string{"GOPROXY", "GOTOOLCHAIN", "GOFLAGS"})
	if got["GOPROXY"] != "https://proxy.example.test" || got["GOTOOLCHAIN"] != "auto" || got["GOFLAGS"] != "-trimpath=false -p=3" {
		t.Fatalf("forwarded settings: %v", got)
	}
	if strings.Join(o.GoEnvForwarded, ",") != "GOPROXY,GOTOOLCHAIN" {
		t.Fatalf("override not recorded: %v", o.GoEnvForwarded)
	}
}

// TestNonModuleChangeRunsFresh: a change outside every go.mod root (here the tree has no
// module at all) runs every check with -count=1; the /bin/sh check itself records no
// cache state, and a go argv in that env records why it ran fresh.
func TestNonModuleChangeRunsFresh(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	got, o := envCheck(t, map[string]string{"lab/go.mod": "module example.test/env\n\ngo 1.22\n"}, nil)
	if got["GOFLAGS"] != "-trimpath -count=1 -p=3" || o.Cache != "" {
		t.Fatalf("non-module change: %v cache %q", got, o.Cache)
	}
	if s := CacheStatus([]string{"go", "test", "./..."}, []string{"GOFLAGS=" + got["GOFLAGS"]}, true, true); s != resources.CacheFreshNonModule {
		t.Fatalf("go argv in a non-module env: %q", s)
	}
	if !changesOutsideModules([]string{"docs/a.md"}, []string{"lab/go.mod", "docs/a.md"}) || changesOutsideModules([]string{"lab/x.go"}, []string{"lab/go.mod"}) || changesOutsideModules([]string{"docs/a.md"}, []string{"go.mod"}) {
		t.Fatal("changesOutsideModules misclassified a path")
	}
	if !changesOutsideModules([]string{"docs/a.md"}, nil) || changesOutsideModules(nil, nil) {
		t.Fatal("a tree without modules misclassified")
	}
}

// TestNonModuleChangeIsPerModule: with a root go.mod and a nested one, a docs-only change
// lies inside the root module but outside the nested one, so it forces fresh; so does a
// root-module file outside the nested module. Only a change inside every module root (in
// the nested module, which the root module also covers) stays cached.
func TestNonModuleChangeIsPerModule(t *testing.T) {
	files := []string{"go.mod", "main.go", "lab/go.mod", "lab/x.go", "docs/a.md"}
	if !changesOutsideModules([]string{"docs/a.md"}, files) {
		t.Fatal("docs-only change in a root+nested repo must force fresh")
	}
	if !changesOutsideModules([]string{"main.go"}, files) {
		t.Fatal("a change outside the nested module must force fresh")
	}
	if changesOutsideModules([]string{"lab/x.go"}, files) {
		t.Fatal("a change inside every module root must stay cached")
	}
	work := []string{"go.work", "a/go.mod", "a/a.go", "b/go.mod", "b/b.go"}
	if !changesOutsideModules([]string{"a/a.go"}, work) {
		t.Fatal("a go.work change outside module b must force fresh")
	}
}

// TestNonModuleChangeIgnoresFixtureModules: go.mod files Go itself ignores (under
// testdata/ or vendor/, or a directory starting with _ or .) are not module roots, so a
// change inside lab/ outside such a fixture stays cacheable.
func TestNonModuleChangeIgnoresFixtureModules(t *testing.T) {
	files := []string{"lab/go.mod", "lab/x.go", "lab/internal/x/testdata/mod/go.mod", "lab/vendor/dep/go.mod", "lab/_old/go.mod", "lab/.hidden/go.mod"}
	if changesOutsideModules([]string{"lab/x.go"}, files) {
		t.Fatal("a change inside lab/ outside an ignored fixture module must stay cacheable")
	}
	if !changesOutsideModules([]string{"docs/a.md"}, files) {
		t.Fatal("a change outside lab/ must still force fresh")
	}
}

// TestAcceptanceCacheStatusIsFresh: independent acceptance records "disabled (fresh)"
// whatever its argv, since it always runs with -count=1.
func TestAcceptanceCacheStatusIsFresh(t *testing.T) {
	if s := AcceptanceCacheStatus(true); s != resources.CacheFresh {
		t.Fatalf("acceptance with cache: %q", s)
	}
	if s := AcceptanceCacheStatus(false); s != resources.CacheUnavailable {
		t.Fatalf("acceptance without cache: %q", s)
	}
}

// TestDocsOnlyChangeInNestedModuleRepoRunsFresh: end to end, a docs-only change in a tree
// with a root go.mod and a nested lab/go.mod runs the checks with -count=1.
func TestDocsOnlyChangeInNestedModuleRepoRunsFresh(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	got, _ := envCheck(t, map[string]string{"go.mod": "module example.test/root\n\ngo 1.22\n", "lab/go.mod": "module example.test/lab\n\ngo 1.22\n"}, nil)
	if got["GOFLAGS"] != "-trimpath -count=1 -p=3" {
		t.Fatalf("root+nested docs-only change: %v", got)
	}
}

// TestCacheSettingsFailureGetsNote: a check that fails because GOPROXY=off blocked a module
// lookup is recorded with a note naming Forgecell's cache settings.
func TestCacheSettingsFailureGetsNote(t *testing.T) {
	dir, _ := sourceFixture(t)
	os.WriteFile(filepath.Join(dir, "check.sh"), []byte("#!/bin/sh\necho 'go: example.test/dep@v1.0.0: module lookup disabled by GOPROXY=off' >&2\nexit 1\n"), 0600)
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "offline")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	os.WriteFile(filepath.Join(dir, "code.txt"), []byte("changed"), 0600)
	capture, err := Capture(context.Background(), dir, base, []string{"code.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := resources.WithGoCache(context.Background(), resources.GoCache{ModCache: "/private/mod", BuildCache: "/private/build"})
	result := Run(ctx, dir, filepath.Join(t.TempDir(), "verification"), p, capture)
	if result.RequiredChecksPassed || len(result.Checks) != 1 || !strings.Contains(result.Checks[0].Note, "go mod download") || !strings.Contains(result.Checks[0].Note, "GOPROXY=off") {
		t.Fatalf("missing cache settings note: %+v", result.Checks)
	}
}

func TestCacheStatusAndPackageCounts(t *testing.T) {
	freshEnv := []string{"GOFLAGS=-trimpath -count=1"}
	cacheEnv := []string{"GOFLAGS=-trimpath"}
	for _, c := range []struct {
		argv      []string
		env       []string
		cache     bool
		nonModule bool
		want      string
	}{
		{[]string{"go", "test", "./..."}, cacheEnv, true, false, resources.CacheEnabled},
		{[]string{"/usr/local/go/bin/go", "vet", "./..."}, cacheEnv, true, false, resources.CacheEnabled},
		{[]string{"go", "test", "-count=1", "./..."}, cacheEnv, true, false, resources.CacheFresh},
		{[]string{"go", "test", "-count", "1", "./..."}, cacheEnv, true, false, resources.CacheFresh},
		// A non-go argv records no cache state, even when its GOFLAGS carries -count.
		{[]string{"/bin/sh", "acceptance.sh"}, freshEnv, true, false, ""},
		{[]string{"sh", "-c", "go test ./..."}, freshEnv, true, true, ""},
		{[]string{"go", "test", "./..."}, freshEnv, true, true, resources.CacheFreshNonModule},
		{[]string{"sh", "-c", "test -z \"$(gofmt -l .)\""}, cacheEnv, true, false, ""},
		{[]string{"go", "test", "./..."}, nil, false, false, resources.CacheUnavailable},
		{[]string{"gofmt", "-l", "."}, nil, false, false, ""},
	} {
		if got := CacheStatus(c.argv, c.env, c.cache, c.nonModule); got != c.want {
			t.Errorf("CacheStatus(%v, %v, %t, %t) = %q, want %q", c.argv, c.env, c.cache, c.nonModule, got, c.want)
		}
	}
	plain := "ok  \texample.test/a\t(cached)\nok  \texample.test/b\t0.12s\n?   \texample.test/c\t[no test files]\n--- FAIL: TestX (0.00s)\nFAIL\texample.test/d\t0.01s\nFAIL\n"
	if got := CountGoTestPackages(plain); got == nil || *got != (PackageCounts{Cached: 1, Run: 2}) {
		t.Fatalf("plain counts: %+v", got)
	}
	events := `{"Action":"output","Package":"example.test/a","Output":"ok  \texample.test/a\t(cached)\n"}` + "\n" +
		`{"Action":"output","Package":"example.test/b","Test":"TestB","Output":"ok  \tnot a package line\t(cached)\n"}` + "\n" +
		`{"Action":"output","Package":"example.test/b","Output":"ok  \texample.test/b\t0.2s\n"}` + "\n"
	if got := CountGoTestPackages(events); got == nil || *got != (PackageCounts{Cached: 1, Run: 1}) {
		t.Fatalf("json counts: %+v", got)
	}
	if got := CountGoTestPackages("all good\n"); got != nil {
		t.Fatalf("non-go output must record no counts: %+v", got)
	}
}

func TestChangedCheckDefinitionStopsBeforeExecution(t *testing.T) {
	dir, base := sourceFixture(t)
	p := checkPlan(dir, base)
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "check.sh", Reason: "Checker", Evidence: []string{"check"}})
	os.WriteFile(filepath.Join(dir, "check.sh"), []byte("touch should-not-run\n"), 0600)
	c, err := Capture(context.Background(), dir, base, []string{"check.sh", "code.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
	if v.RequiredChecksPassed || len(v.Checks) != 0 || v.Error == "" {
		t.Fatalf("changed checker accepted: %+v", v)
	}
}
func TestFailedChecksAndMutatingChecksCannotPass(t *testing.T) {
	for _, script := range []string{"exit 7\n", "printf tampered > code.txt\n"} {
		t.Run(script, func(t *testing.T) {
			dir, _ := sourceFixture(t)
			os.WriteFile(filepath.Join(dir, "check.sh"), []byte(script), 0600)
			gitTest(t, dir, "add", "check.sh")
			gitTest(t, dir, "commit", "-m", "checker")
			base := gitTest(t, dir, "rev-parse", "HEAD")
			p := checkPlan(dir, base)
			c, err := Capture(context.Background(), dir, base, []string{"code.txt"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
			if v.RequiredChecksPassed {
				t.Fatalf("invalid check passed: %+v", v)
			}
		})
	}
}

func TestWeakenedHelperCannotReplaceProtectedRegression(t *testing.T) {
	dir, _ := sourceFixture(t)
	os.Mkdir(filepath.Join(dir, "tests"), 0700)
	os.WriteFile(filepath.Join(dir, "tests", "assert.sh"), []byte("test \"$(cat code.txt)\" = base\n"), 0600)
	os.WriteFile(filepath.Join(dir, "check.sh"), []byte(". tests/assert.sh\n"), 0600)
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "baseline regression")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "tests/assert.sh", Reason: "Update test", Evidence: []string{"issue"}})
	os.WriteFile(filepath.Join(dir, "code.txt"), []byte("broken"), 0600)
	os.WriteFile(filepath.Join(dir, "tests", "assert.sh"), []byte("true\n"), 0600)
	c, err := Capture(context.Background(), dir, base, []string{"code.txt", "tests/assert.sh"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
	if v.RequiredChecksPassed || v.Protected == nil || v.Protected.RequiredChecksPassed || len(v.Checks) != 1 || !v.Checks[0].Result.OK || v.Protected.SourceTree == v.SourceTree {
		t.Fatalf("weakened test hid regression: %+v", v)
	}
}

func TestHistoryPersistenceFailureStopsProtectedExecution(t *testing.T) {
	dir, _ := sourceFixture(t)
	os.Mkdir(filepath.Join(dir, "tests"), 0700)
	os.WriteFile(filepath.Join(dir, "tests", "assert.sh"), []byte("true\n"), 0600)
	os.WriteFile(filepath.Join(dir, "check.sh"), []byte(". tests/assert.sh\n"), 0600)
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "baseline")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "tests/assert.sh", Reason: "Change test", Evidence: []string{"issue"}})
	os.WriteFile(filepath.Join(dir, "tests", "assert.sh"), []byte("echo candidate\n"), 0600)
	c, e := Capture(context.Background(), dir, base, []string{"code.txt", "tests/assert.sh"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(t.TempDir(), "verify")
	v := RunObserved(context.Background(), dir, dest, p, c, func(Observation) error { return fmt.Errorf("disk full") })
	if v.RequiredChecksPassed || v.Protected != nil {
		t.Fatal("continued after evidence persistence failure")
	}
	if _, e = os.Stat(dest + "-protected"); !os.IsNotExist(e) {
		t.Fatal("protected commands started after persistence failure")
	}
}

func repairCheckPlan(p readiness.Plan, content string) readiness.Plan {
	input := readiness.FileEvidence{Evidence: readiness.Evidence{ID: "human", Path: "external-acceptance.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}, Content: content}
	p.CheckInputs = []readiness.FileEvidence{input}
	p.Evidence = append(p.Evidence, input.Evidence)
	p.Analysis.Checks[0].ReviewedAcceptance = "human-check"
	p.Analysis.Checks = append(p.Analysis.Checks, readiness.Check{ID: "human-check", Category: "independent-acceptance", IndependentProvenance: "separate human fixture review", Dir: ".", Argv: []string{"/bin/sh", "{input:human}"}, TimeoutMS: 1000, Required: true, Definitions: []string{"human"}, Evidence: []string{"human"}, Reason: "Frozen human acceptance"})
	return p
}
func TestReviewedCheckerRepairRetainsAllCandidateAndProtectedCommands(t *testing.T) {
	// Reproduce the immutable issue22 excerpt's mechanism in an isolated repo;
	// the original ledger/result hashes are historical evidence, not verified here.
	dir, base := sourceFixture(t)
	p := checkPlan(dir, base)
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "check.sh", Reason: "Edit checker", Evidence: []string{"check"}})
	p = repairCheckPlan(p, "test -f code.txt\n")
	os.WriteFile(filepath.Join(dir, "check.sh"), []byte("printf candidate\n"), 0600)
	c, err := Capture(context.Background(), dir, base, []string{"check.sh", "code.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ValidateForCoding(); err != nil {
		t.Fatal(err)
	}
	v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
	if !v.RequiredChecksPassed || !v.IndependentAcceptanceVerified || len(v.Checks) != 2 || v.Protected == nil || !v.Protected.RequiredChecksPassed || len(v.Protected.Checks) != 1 {
		t.Fatalf("repair lost candidate/protected outcomes: %+v", v)
	}
	if v.Checks[0].Category != "candidate" || v.Checks[0].Command.Definitions[0] != "check" || v.Protected.Checks[0].Category != "regression" || v.SourceTree == v.Protected.SourceTree {
		t.Fatal("checker provenance or source identity lost", v)
	}
}
func TestExternalAcceptanceMutationCannotPass(t *testing.T) {
	for _, script := range []string{"chmod u+w \"$0\"; printf tampered > \"$0\"\n", "rm \"$0\"; ln -s /dev/null \"$0\"\n"} {
		t.Run(script, func(t *testing.T) {
			dir, base := sourceFixture(t)
			p := checkPlan(dir, base)
			p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "check.sh", Reason: "Checker", Evidence: []string{"check"}})
			p = repairCheckPlan(p, script)
			c, err := Capture(context.Background(), dir, base, []string{"code.txt", "check.sh"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
			if v.RequiredChecksPassed || v.IndependentAcceptanceVerified || !strings.Contains(v.Error, "reviewed check input changed") {
				t.Fatalf("tampered external acceptance passed: %+v", v)
			}
		})
	}
}

func TestReviewedRepairCannotBypassProtectedFailure(t *testing.T) {
	dir, _ := sourceFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte("exit 7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", "check.sh")
	gitTest(t, dir, "commit", "-m", "protected failing checker")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "check.sh", Reason: "Edit checker", Evidence: []string{"check"}})
	p = repairCheckPlan(p, "true\n")
	if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte("true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Capture(context.Background(), dir, base, []string{"check.sh", "code.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
	if v.RequiredChecksPassed || v.IndependentAcceptanceVerified || v.Protected == nil || v.Protected.RequiredChecksPassed || len(v.Checks) != 2 || !v.Checks[0].Result.OK || !v.Checks[1].Result.OK || len(v.Protected.Checks) != 1 || v.Protected.Checks[0].Result.OK {
		t.Fatalf("candidate acceptance bypassed protected failure: %+v", v)
	}
}
func TestReviewedRepairRejectsReboundBaselineHashBeforeCommands(t *testing.T) {
	dir, base := sourceFixture(t)
	p := checkPlan(dir, base)
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "check.sh", Reason: "Edit checker", Evidence: []string{"check"}})
	p = repairCheckPlan(p, "true\n")
	p.Evidence[0].SHA256 = strings.Repeat("f", 64)
	if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte("true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Capture(context.Background(), dir, base, []string{"check.sh", "code.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
	if v.RequiredChecksPassed || v.IndependentAcceptanceVerified || len(v.Checks) != 0 || v.Protected == nil || len(v.Protected.Checks) != 0 {
		t.Fatalf("rebound baseline hash executed commands: %+v", v)
	}
}
func TestExternalAcceptanceMutationStopsBeforeNextCommand(t *testing.T) {
	dir, base := sourceFixture(t)
	p := checkPlan(dir, base)
	// All external inputs, even those not used until a later check, must be intact
	// after setup; its successful exit cannot mask acceptance-input tampering.
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "check.sh", Reason: "Checker", Evidence: []string{"check"}})
	p = repairCheckPlan(p, "true\n")
	p.Setup = []readiness.Check{{ID: "mutate-input", Category: "setup", Dir: ".", Argv: []string{"/bin/sh", "-c", "chmod u+w \"$1\"; printf tampered > \"$1\"", "setup", "{input:human}"}, TimeoutMS: 1000, Definitions: []string{"human"}, Evidence: []string{"human"}, Reason: "Synthetic tampering setup"}}
	c, err := Capture(context.Background(), dir, base, []string{"code.txt", "check.sh"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
	if v.RequiredChecksPassed || v.IndependentAcceptanceVerified || len(v.Setup) != 1 || len(v.Checks) != 0 || !strings.Contains(v.Error, "reviewed check input changed") {
		t.Fatalf("continued after setup tampered with acceptance: %+v", v)
	}
}

const deltaPkg = "example.test/pkg"

func deltaLine(action, pkg, test string) string {
	if test == "" {
		return fmt.Sprintf(`printf '%%s\n' '{"Action":%q,"Package":%q}'`, action, pkg)
	}
	return fmt.Sprintf(`printf '%%s\n' '{"Action":%q,"Package":%q,"Test":%q}'`, action, pkg, test)
}

func passingBody(pkg, test string) string {
	return deltaLine("pass", pkg, test) + "\nexit 0"
}

func failingBody(pkg string, tests ...string) string {
	lines := []string{}
	for _, name := range tests {
		lines = append(lines, deltaLine("fail", pkg, name))
	}
	lines = append(lines, deltaLine("fail", pkg, ""), "exit 1")
	return strings.Join(lines, "\n")
}

func pinnedDelta(pkg, test string) readiness.ProtectedDelta {
	return readiness.ProtectedDelta{CheckID: "regression", Package: pkg, Test: test, ScopePath: "testdata/pin.txt", Reason: "The change alters the pinned fixture", Evidence: []string{"issue"}}
}

// deltaFixture commits a fake go test -json checker that reads a pinned fixture.
// The candidate rewrites the fixture, so the protected view restores the old value
// and runs protectedBody instead of candidateBody. No Go toolchain is involved.
func deltaFixture(t *testing.T, candidateBody, protectedBody string, deltas ...readiness.ProtectedDelta) (string, readiness.Plan, CaptureResult) {
	t.Helper()
	dir, _ := sourceFixture(t)
	script := "#!/bin/sh\nif [ \"$(cat testdata/pin.txt)\" = new ]; then\n" + candidateBody + "\nfi\n" + protectedBody + "\n"
	if err := os.MkdirAll(filepath.Join(dir, "testdata"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testdata", "pin.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	// Deltas are bound to the package of their scope path through go.mod.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/pkg\n\ngo 1.22\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "pinned fixture and checker")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	p.Analysis.Scope = []readiness.ScopedPath{{Path: "testdata/pin.txt", Reason: "Pinned fixture", Evidence: []string{"issue"}}}
	p.Analysis.Checks[0].Argv = []string{"/bin/sh", "check.sh", "-json"}
	p.ExpectedProtectedDeltas = deltas
	if err := os.WriteFile(filepath.Join(dir, "testdata", "pin.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Capture(context.Background(), dir, base, []string{"testdata/pin.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return dir, p, c
}

func runDelta(t *testing.T, dir string, p readiness.Plan, c CaptureResult) Result {
	t.Helper()
	return Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
}

func TestProtectedDeltaDeclaredFailurePasses(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestPinned"), pinnedDelta(deltaPkg, "TestPinned"))
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Error != "" || v.Protected == nil || !v.Protected.RequiredChecksPassed {
		t.Fatalf("declared delta did not pass: %+v", v)
	}
	if len(v.AcceptedDeltas) != 1 || v.AcceptedDeltas[0].Test != "TestPinned" || len(v.Warnings) != 0 {
		t.Fatalf("accepted delta not listed: %+v", v)
	}
	obs := v.Protected.Checks[0]
	if obs.Result.OK || len(obs.FailedTests) != 1 || len(obs.AcceptedDeltas) != 1 || v.IndependentAcceptanceVerified {
		t.Fatalf("protected observation lost the delta evidence: %+v", obs)
	}
	if !v.Checks[0].Result.OK || len(v.Checks[0].AcceptedDeltas) != 0 {
		t.Fatalf("candidate view was relaxed: %+v", v.Checks[0])
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var back Result
	if err = json.Unmarshal(raw, &back); err != nil || len(back.AcceptedDeltas) != 1 || len(back.Protected.Checks[0].FailedTests) != 1 {
		t.Fatalf("evidence did not survive the JSON projection: %v %+v", err, back)
	}
}

func TestProtectedDeltaSubtestFailuresRequireDeclaredParents(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestParent/sub/case", "TestParent/sub", "TestParent"),
		pinnedDelta(deltaPkg, "TestParent/sub/case"), pinnedDelta(deltaPkg, "TestParent/sub"), pinnedDelta(deltaPkg, "TestParent"))
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || len(v.AcceptedDeltas) != 3 {
		t.Fatalf("declared subtest and parents were not accepted: %+v", v)
	}
}

func TestProtectedDeltaUndeclaredParentIsNotMasked(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestParent/sub", "TestParent"), pinnedDelta(deltaPkg, "TestParent/sub"))
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || len(v.AcceptedDeltas) != 0 || !strings.Contains(v.Error, deltaPkg+" TestParent") || !strings.Contains(v.Error, "declared") {
		t.Fatalf("an undeclared failing parent was masked by its declared subtest: %+v", v)
	}
}

func TestProtectedDeltaFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		delta  readiness.ProtectedDelta
		wanted string
	}{
		{"undeclared test", failingBody(deltaPkg, "TestPinned", "TestOther"), pinnedDelta(deltaPkg, "TestPinned"), deltaPkg + " TestOther"},
		{"same test in another package", failingBody("other.test/pkg", "TestPinned"), pinnedDelta(deltaPkg, "TestPinned"), "other.test/pkg TestPinned"},
		{"declared parent does not cover its subtest", failingBody(deltaPkg, "TestParent/sub", "TestParent"), pinnedDelta(deltaPkg, "TestParent"), deltaPkg + " TestParent/sub"},
		{"package failure without a failing test", deltaLine("fail", deltaPkg, "") + "\nexit 1", pinnedDelta(deltaPkg, "TestPinned"), "failed without a failing test"},
		{"build failure", `printf '%s\n' '{"Action":"build-fail","ImportPath":"example.test/pkg"}'` + "\nexit 2", pinnedDelta(deltaPkg, "TestPinned"), "build failure"},
		{"unparseable output", "echo not-json\nexit 1", pinnedDelta(deltaPkg, "TestPinned"), "not go test -json"},
		{"non-zero exit without a failing test", deltaLine("pass", deltaPkg, "TestPinned") + "\nexit 1", pinnedDelta(deltaPkg, "TestPinned"), "without any failing test"},
		{"no output", "exit 1", pinnedDelta(deltaPkg, "TestPinned"), "no go test -json events"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), tc.body, tc.delta)
			v := runDelta(t, dir, p, c)
			if v.RequiredChecksPassed || v.IndependentAcceptanceVerified || v.Protected == nil || v.Protected.RequiredChecksPassed || len(v.AcceptedDeltas) != 0 {
				t.Fatalf("undeclared protected failure passed: %+v", v)
			}
			if !strings.Contains(v.Error, "protected regression did not pass") || !strings.Contains(v.Error, tc.wanted) {
				t.Fatalf("error does not name the failure %q: %q", tc.wanted, v.Error)
			}
		})
	}
}

func TestProtectedDeltaDeclaredButPassingWarns(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), passingBody(deltaPkg, "TestPinned"), pinnedDelta(deltaPkg, "TestPinned"))
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Protected == nil || !v.Protected.RequiredChecksPassed || len(v.AcceptedDeltas) != 0 {
		t.Fatalf("passing declared delta failed the check: %+v", v)
	}
	if len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], deltaPkg+" TestPinned") {
		t.Fatalf("missing warning: %+v", v.Warnings)
	}
}

func TestProtectedDeltaUndeclaredBehaviourIsUnchanged(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestPinned"))
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || v.Error != "protected regression did not pass: required command regression did not pass" || len(v.AcceptedDeltas) != 0 || len(v.Warnings) != 0 {
		t.Fatalf("no declaration changed behaviour: %+v", v)
	}
}

func TestProtectedDeltaNeverRelaxesCandidateView(t *testing.T) {
	dir, p, c := deltaFixture(t, failingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestPinned"), pinnedDelta(deltaPkg, "TestPinned"))
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || v.Checks[0].Result.OK || !strings.Contains(v.Error, "required command regression did not pass") || len(v.AcceptedDeltas) != 0 {
		t.Fatalf("candidate failure was accepted through a delta: %+v", v)
	}
}

func TestProtectedDeltaWithoutProtectedRunWarnsNotExercised(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), passingBody(deltaPkg, "TestPinned"), pinnedDelta(deltaPkg, "TestPinned"))
	// Restore the fixture so the captured tree does not differ from the base commit.
	if err := os.WriteFile(filepath.Join(dir, "testdata", "pin.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Capture(context.Background(), dir, p.Inputs.BaseCommit, []string{"testdata/pin.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Protected != nil {
		t.Fatalf("unchanged candidate should pass without a protected run: %+v", v)
	}
	if len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], "not exercised") {
		t.Fatalf("missing not-exercised warning: %+v", v)
	}
}

func TestProtectedDeltaDeliveryMatchingReevaluatesAgainstPlan(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestPinned"), pinnedDelta(deltaPkg, "TestPinned"))
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Protected == nil {
		t.Fatalf("setup: %+v", v)
	}
	check := p.Analysis.Checks[0]
	tree := v.Protected.SourceTree
	obs := v.Protected.Checks[0]
	if !MatchesCheckObservation(p, check, obs, tree, true) {
		t.Fatal("full-stream protected observation with a covered failing set was rejected")
	}
	compact := obs
	compact.Result.Stdout, compact.Result.Stderr, compact.Result.RawStdout = "", "", ""
	if !MatchesCheckObservation(p, check, compact, tree, true) {
		t.Fatal("compact ledger projection of an accepted observation was rejected")
	}
	if MatchesCheckObservation(p, check, obs, tree, false) {
		t.Fatal("candidate matching accepted a failing observation")
	}
	undeclared := p
	undeclared.ExpectedProtectedDeltas = nil
	if MatchesCheckObservation(undeclared, check, obs, tree, true) || MatchesCheckObservation(undeclared, check, compact, tree, true) {
		t.Fatal("failing observation matched a plan without deltas")
	}
	widened := compact
	widened.FailedTests = append(append([]TestID{}, compact.FailedTests...), TestID{Package: deltaPkg, Test: "TestOther"})
	if MatchesCheckObservation(p, check, widened, tree, true) {
		t.Fatal("uncovered recorded failure matched")
	}
	empty := compact
	empty.FailedTests = nil
	if MatchesCheckObservation(p, check, empty, tree, true) {
		t.Fatal("failing observation without a recorded failing set matched")
	}
	narrowed := obs
	narrowed.FailedTests = []TestID{{Package: deltaPkg, Test: "TestPinned"}}
	narrowed.Result.Stdout = strings.Replace(obs.Result.Stdout, `"Test":"TestPinned"}`, `"Test":"TestPinned"}`+"\n"+`{"Action":"fail","Package":"example.test/pkg","Test":"TestHidden"}`, 1)
	if MatchesCheckObservation(p, check, narrowed, tree, true) {
		t.Fatal("recorded failing set that hides a failure in the retained stream matched")
	}
	mutations := []func(*Observation){
		func(o *Observation) { o.Result.TimedOut = true },
		func(o *Observation) { o.Result.Interrupted = true },
		func(o *Observation) { o.Result.Overflow = true },
		func(o *Observation) { o.Result.Reconciled = false },
		func(o *Observation) { o.Result.Code = 0 },
	}
	for i, mutate := range mutations {
		bad := compact
		mutate(&bad)
		if MatchesCheckObservation(p, check, bad, tree, true) {
			t.Fatalf("incomplete observation %d matched", i)
		}
	}
}

func TestProtectedDeltaEvaluatorRejectsIncompleteResults(t *testing.T) {
	declared := []readiness.ProtectedDelta{pinnedDelta(deltaPkg, "TestPinned")}
	stream := `{"Action":"fail","Package":"example.test/pkg","Test":"TestPinned"}` + "\n" + `{"Action":"fail","Package":"example.test/pkg"}` + "\n"
	good := process.Result{Reconciled: true, Code: 1, Stdout: stream}
	if e, err := EvaluateProtectedDeltas(declared, good); err != nil || len(e.Accepted) != 1 || len(e.Warnings) != 0 {
		t.Fatalf("baseline stream was rejected: %v %+v", err, e)
	}
	bad := []process.Result{
		{Reconciled: true, Code: 1, TimedOut: true, Stdout: stream},
		{Reconciled: true, Code: 1, ParentDeadline: true, Stdout: stream},
		{Reconciled: true, Code: 1, Interrupted: true, Stdout: stream},
		{Reconciled: true, Code: 1, Overflow: true, Stdout: stream},
		{Code: 1, Stdout: stream},
		{Reconciled: true, Code: -1, Stdout: stream},
		{Reconciled: true, Code: 0, Stdout: stream},
		{Reconciled: true, Code: 1, Stdout: stream + `{"Action":"fail","Pack`},
	}
	for i, r := range bad {
		if _, err := EvaluateProtectedDeltas(declared, r); err == nil {
			t.Fatalf("incomplete result %d was accepted", i)
		}
	}
}

// A delta may only excuse a test in the package its scope path belongs to.
func TestProtectedDeltaOutsideItsScopePackageIsRefused(t *testing.T) {
	const security = "example.test/pkg/security"
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(security, "TestAuth"), pinnedDelta(security, "TestAuth"))
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || !strings.Contains(v.Error, "not in the package of its scope path") {
		t.Fatalf("delta outside its scope package accepted: %+v", v.Error)
	}
}
