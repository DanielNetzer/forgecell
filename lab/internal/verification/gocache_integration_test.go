package verification

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// depSource is dep/dep.go with a nonce that Value reads, so the nonce is compiled into
// every test binary that imports dep: a new nonce changes those binaries' cache keys,
// unlike a comment or an unused constant.
const depSource = "package dep\n\nvar nonce = %d\n\nfunc Value() int {\n\tif nonce < 0 {\n\t\treturn 0\n\t}\n\treturn 2\n}\n"

// goCacheFixture commits a small module at moduleDir ("" for the repository root): stable
// (no dependencies), dep, and user, which imports dep. dep embeds a per-run nonce, and a
// hidden acceptance script runs stable with -count=1. scope adds approved paths beside
// dep/dep.go.
func goCacheFixture(t *testing.T, moduleDir string, scope ...string) (string, string, readiness.Plan) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir, _ := sourceFixture(t)
	module := map[string]string{
		"go.mod":                "module example.test/cache\n\ngo 1.22\n",
		"stable/stable.go":      "package stable\n\nfunc Value() int { return 1 }\n",
		"stable/stable_test.go": "package stable\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) {\n\tif Value() != 1 {\n\t\tt.Fatal(Value())\n\t}\n}\n",
		"dep/dep.go":            fmt.Sprintf(depSource, time.Now().UnixNano()),
		"user/user.go":          "package user\n\nimport \"example.test/cache/dep\"\n\nfunc Value() int { return dep.Value() }\n",
		"user/user_test.go":     "package user\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) {\n\tif Value() != 2 {\n\t\tt.Fatal(Value())\n\t}\n}\n",
	}
	checkDir := "."
	files := map[string]string{"acceptance.sh": "#!/bin/sh\nset -eu\ncd ./" + moduleDir + "\ngo test -count=1 ./stable\n", "docs/notes.md": "notes\n"}
	if moduleDir != "" {
		checkDir = moduleDir
		moduleDir += "/"
	}
	for file, data := range module {
		files[moduleDir+file] = data
	}
	for file, data := range files {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(file)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "module")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	accept, _ := os.ReadFile(filepath.Join(dir, "acceptance.sh"))
	p.Evidence = append(p.Evidence, readiness.Evidence{ID: "accept", Path: "acceptance.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256(accept))})
	p.Analysis.Scope = []readiness.ScopedPath{{Path: moduleDir + "dep/dep.go", Reason: "Implementation", Evidence: []string{"issue"}}}
	for _, path := range scope {
		p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: path, Reason: "Implementation", Evidence: []string{"issue"}})
	}
	p.Analysis.Checks = []readiness.Check{
		{ID: "acceptance", Category: "independent-acceptance", IndependentProvenance: "Reviewed hidden acceptance", Dir: ".", Argv: []string{"/bin/sh", "acceptance.sh"}, TimeoutMS: 600000, Required: true, Reason: "Hidden acceptance", Evidence: []string{"accept"}, Definitions: []string{"accept"}},
		{ID: "regression", Category: "regression", Dir: checkDir, Argv: []string{"go", "test", "./..."}, TimeoutMS: 600000, Required: true, Reason: "Regression suite", Evidence: []string{"check"}, Definitions: []string{"check"}},
	}
	return dir, base, p
}

// privateCacheContext gives the views an empty build cache of their own, beside the
// shared module cache, so entries from earlier runs on the machine cannot change the
// outcome, and a stable TMPDIR.
func privateCacheContext(t *testing.T) context.Context {
	t.Helper()
	shared, ok := resources.ResolveGoCache(context.Background())
	if !ok {
		t.Skip("go cache unavailable")
	}
	private := resources.GoCache{ModCache: shared.ModCache, BuildCache: t.TempDir()}
	return resources.WithCheckTempDir(resources.WithGoCache(context.Background(), private), t.TempDir())
}

func captureDep(t *testing.T, dir, base, value string) CaptureResult {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "dep", "dep.go"))
	if err != nil {
		t.Fatal(err)
	}
	changed := string(body) + "\n"
	if value != "2" {
		// user_test still expects 2, so keep the value but compile in a new nonce, unique to
		// this run: a comment or a constant-folded expression would leave the test binary,
		// and so its cache key, unchanged.
		changed = fmt.Sprintf(depSource, time.Now().UnixNano())
		if changed == string(body) {
			t.Fatal("dependency change produced the same source")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "dep", "dep.go"), []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Capture(context.Background(), dir, base, []string{"dep/dep.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func checkByID(t *testing.T, r Result, id string) Observation {
	t.Helper()
	for _, o := range r.Checks {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("no %s check in %+v", id, r)
	return Observation{}
}

func cachedLine(stdout, pkg string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "ok" && f[1] == pkg && strings.Contains(line, "(cached)") {
			return true
		}
	}
	return false
}

// TestSharedGoCacheServesUnchangedPackagesAcrossCheckouts runs verification in checkouts
// of the same tree at different paths sharing one GOCACHE. With -trimpath the second
// regression go test reports every package (cached); after dep changes, stable stays
// (cached) while user, which imports dep, re-runs. Hidden acceptance (-count=1) is never
// cached, and each record states its cache state and counts. The test uses its own empty
// GOCACHE, so entries from earlier runs on the machine cannot change the outcome.
func TestSharedGoCacheServesUnchangedPackagesAcrossCheckouts(t *testing.T) {
	dir, base, p := goCacheFixture(t, "")
	ctx := privateCacheContext(t)
	first := captureDep(t, dir, base, "2")
	root := t.TempDir()
	for i, dest := range []string{"one/verification", "two/elsewhere/verification"} {
		v := Run(ctx, dir, filepath.Join(root, dest), p, first)
		if !v.RequiredChecksPassed {
			t.Fatalf("checkout %d: %+v", i, v)
		}
		if i == 0 {
			continue
		}
		regression := checkByID(t, v, "regression")
		if regression.Cache != resources.CacheEnabled || !cachedLine(regression.Result.Stdout, "example.test/cache/stable") || !cachedLine(regression.Result.Stdout, "example.test/cache/user") {
			t.Fatalf("second checkout of the same tree was not served from the cache: %s %q", regression.Cache, regression.Result.Stdout)
		}
		// The view total also counts the fresh acceptance run of stable.
		if regression.Packages == nil || regression.Packages.Cached != 2 || regression.Packages.Run != 0 || v.CacheTotals() != (PackageCounts{Cached: 2, Run: 1}) {
			t.Fatalf("cached count not recorded: %+v %+v", regression.Packages, v.CacheTotals())
		}
		// Independent acceptance records "disabled (fresh)" whatever its argv: its GOFLAGS
		// -count=1 keeps it fresh.
		acceptance := checkByID(t, v, "acceptance")
		if acceptance.Cache != resources.CacheFresh || strings.Contains(acceptance.Result.Stdout, "(cached)") || acceptance.Packages == nil || acceptance.Packages.Run != 1 {
			t.Fatalf("hidden acceptance must run fresh: %s %+v %q", acceptance.Cache, acceptance.Packages, acceptance.Result.Stdout)
		}
	}
	second := captureDep(t, dir, base, "changed")
	v := Run(ctx, dir, filepath.Join(root, "three", "verification"), p, second)
	if !v.RequiredChecksPassed {
		t.Fatalf("%+v", v)
	}
	regression := checkByID(t, v, "regression")
	if !cachedLine(regression.Result.Stdout, "example.test/cache/stable") || cachedLine(regression.Result.Stdout, "example.test/cache/user") {
		t.Fatalf("a changed dependency must re-run only its importers: %q", regression.Result.Stdout)
	}
	if regression.Packages == nil || *regression.Packages != (PackageCounts{Cached: 1, Run: 1}) {
		t.Fatalf("counts: %+v", regression.Packages)
	}
}

// TestDocsOnlyChangeIsNeverServedFromCache: with the module under lab/, a change to
// docs/ lies outside every go.mod root, which Go's cache does not track, so even the
// second checkout of the same tree re-runs every package and records why.
func TestDocsOnlyChangeIsNeverServedFromCache(t *testing.T) {
	dir, base, p := goCacheFixture(t, "lab", "docs/notes.md")
	ctx := privateCacheContext(t)
	if err := os.WriteFile(filepath.Join(dir, "docs", "notes.md"), []byte("changed notes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Capture(context.Background(), dir, base, []string{"docs/notes.md"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for i, dest := range []string{"one/verification", "two/verification"} {
		v := Run(ctx, dir, filepath.Join(root, dest), p, c)
		if !v.RequiredChecksPassed {
			t.Fatalf("checkout %d: %+v", i, v)
		}
		regression := checkByID(t, v, "regression")
		if regression.Cache != resources.CacheFreshNonModule || strings.Contains(regression.Result.Stdout, "(cached)") {
			t.Fatalf("checkout %d: a docs-only change was served from the cache: %s %q", i, regression.Cache, regression.Result.Stdout)
		}
		if regression.Packages == nil || *regression.Packages != (PackageCounts{Run: 2}) {
			t.Fatalf("checkout %d counts: %+v", i, regression.Packages)
		}
	}
}
