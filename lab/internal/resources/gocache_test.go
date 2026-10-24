package resources

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// resetGoCache clears the process-wide resolution and restores LookupGoCache after t.
func resetGoCache(t *testing.T) {
	t.Helper()
	orig := LookupGoCache
	forget := func() {
		goCache.Lock()
		goCache.value, goCache.ok = GoCache{}, false
		goCache.Unlock()
		goCacheMiss.Lock()
		goCacheMiss.at = time.Time{}
		goCacheMiss.Unlock()
	}
	forget()
	t.Cleanup(func() { LookupGoCache = orig; forget() })
}

// TestResolveGoCacheIsAbsoluteWithGOCACHEUnset resolves the real cache with GOCACHE and
// GOMODCACHE unset in the caller's environment: both are still absolute go env defaults.
func TestResolveGoCacheIsAbsoluteWithGOCACHEUnset(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	resetGoCache(t)
	t.Setenv("GOCACHE", "")
	t.Setenv("GOMODCACHE", "")
	os.Unsetenv("GOCACHE")
	os.Unsetenv("GOMODCACHE")
	c, ok := ResolveGoCache(context.Background())
	if !ok || !filepath.IsAbs(c.BuildCache) || !filepath.IsAbs(c.ModCache) {
		t.Fatalf("got %+v ok=%t", c, ok)
	}
}

// TestResolveGoCacheOncePerProcess resolves once and reuses the result, while a failed
// resolution is retried.
func TestResolveGoCacheOncePerProcess(t *testing.T) {
	resetGoCache(t)
	calls := 0
	fail := true
	LookupGoCache = func(context.Context) (GoCache, bool) {
		calls++
		if fail {
			return GoCache{}, false
		}
		return GoCache{ModCache: "/mod", BuildCache: "/build"}, true
	}
	if _, ok := ResolveGoCache(context.Background()); ok {
		t.Fatal("a failed lookup resolved")
	}
	fail = false
	for range 3 {
		if c, ok := ResolveGoCache(context.Background()); !ok || c != (GoCache{ModCache: "/mod", BuildCache: "/build"}) {
			t.Fatalf("got %+v ok=%t", c, ok)
		}
	}
	if calls != 2 {
		t.Fatalf("lookups = %d, want 2 (one failed, one resolved)", calls)
	}
	private := GoCache{ModCache: "/private/mod", BuildCache: "/private/build"}
	if c, ok := ResolveGoCache(WithGoCache(context.Background(), private)); !ok || c != private || calls != 2 {
		t.Fatalf("WithGoCache: got %+v ok=%t lookups=%d", c, ok, calls)
	}
}

// TestParseGoEnvCache: two absolute lines parse, CRLF endings and a trailing blank line are
// tolerated, and anything short of two non-empty absolute paths is rejected.
func TestParseGoEnvCache(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   GoCache
		wantOK bool
	}{
		{name: "two absolute lines", stdout: "/go/pkg/mod\n/go-build\n", want: GoCache{"/go/pkg/mod", "/go-build"}, wantOK: true},
		{name: "crlf endings", stdout: "/go/pkg/mod\r\n/go-build\r\n", want: GoCache{"/go/pkg/mod", "/go-build"}, wantOK: true},
		{name: "trailing blank line", stdout: "/go/pkg/mod\n/go-build\n\n", want: GoCache{"/go/pkg/mod", "/go-build"}, wantOK: true},
		{name: "one line only", stdout: "/go/pkg/mod\n"},
		{name: "empty value", stdout: "/go/pkg/mod\n\n"},
		{name: "relative path", stdout: "go/pkg/mod\n/go-build\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseGoEnvCache(c.stdout)
			if ok != c.wantOK || (ok && got != c.want) {
				t.Fatalf("got %+v ok=%t, want %+v ok=%t", got, ok, c.want, c.wantOK)
			}
		})
	}
}

// TestGoFlagsWithCacheKeepsOtherFlags: no -mod is ever added (a forwarded one is kept),
// -trimpath unless forwarded (so -trimpath=false opts out), -count=1 when fresh, and every
// forwarded flag is kept in order.
func TestGoFlagsWithCacheKeepsOtherFlags(t *testing.T) {
	for _, c := range []struct {
		existing string
		o        GoEnvOptions
		want     string
	}{
		{"", GoEnvOptions{}, "-trimpath"},
		{"-mod=mod", GoEnvOptions{}, "-trimpath -mod=mod"},
		{"-trimpath -race -mod=vendor", GoEnvOptions{}, "-trimpath -race -mod=vendor"},
		{"-p=4 -tags=integration", GoEnvOptions{}, "-trimpath -p=4 -tags=integration"},
		{"--mod=mod -v", GoEnvOptions{}, "-trimpath --mod=mod -v"},
		{"-trimpath=false", GoEnvOptions{}, "-trimpath=false"},
		{"", GoEnvOptions{Fresh: true}, "-trimpath -count=1"},
		{"-count=3", GoEnvOptions{Fresh: true}, "-trimpath -count=3"},
	} {
		if got := GoFlagsWithCache(c.existing, c.o); got != c.want {
			t.Errorf("GoFlagsWithCache(%q, %+v) = %q, want %q", c.existing, c.o, got, c.want)
		}
	}
	if !GoFlagsCarryCount([]string{"GOFLAGS=-trimpath -count=1"}) || GoFlagsCarryCount([]string{"GOFLAGS=-trimpath", "X=-count=1"}) {
		t.Fatal("GoFlagsCarryCount misread GOFLAGS")
	}
}

// TestTrimpathOptOutSurvivesParallelism: a reviewed policy's GOFLAGS=-trimpath=false is
// kept through the cache merge and the cohort's -p.
func TestTrimpathOptOutSurvivesParallelism(t *testing.T) {
	env, _ := ApplyGoCache([]string{"GOFLAGS=-trimpath=false"}, GoCache{ModCache: "/m", BuildCache: "/b"}, GoEnvOptions{})
	env = ApplyGoParallelism(WithGoParallelism(context.Background(), 2), env)
	if !slices.Contains(env, "GOFLAGS=-trimpath=false -p=2") {
		t.Fatalf("opt-out lost: %q", env)
	}
}

// TestApplyGoCacheReplacesCacheAndKeepsForwardedSettings: forwarded cache entries are
// replaced, forwarded GOPROXY and GOTOOLCHAIN are kept and reported, the defaults apply
// otherwise, and ApplyGoParallelism applied afterwards keeps -trimpath.
func TestApplyGoCacheReplacesCacheAndKeepsForwardedSettings(t *testing.T) {
	c := GoCache{ModCache: "/shared/mod", BuildCache: "/shared/build"}
	env := []string{"HOME=/h", "GOCACHE=/h/cache", "GOMODCACHE=/h/mod", "GOPROXY=https://proxy", "GOTOOLCHAIN=auto", "GOFLAGS=-tags=x", "PATH=/bin"}
	got, forwarded := ApplyGoCache(env, c, GoEnvOptions{})
	want := []string{"HOME=/h", "GOPROXY=https://proxy", "GOTOOLCHAIN=auto", "PATH=/bin", "GOFLAGS=-trimpath -tags=x", "GOMODCACHE=/shared/mod", "GOCACHE=/shared/build"}
	if !slices.Equal(got, want) || !slices.Equal(forwarded, []string{"GOPROXY", "GOTOOLCHAIN"}) {
		t.Fatalf("got %q forwarded %q, want %q", got, forwarded, want)
	}
	got, forwarded = ApplyGoCache([]string{"HOME=/h", "GOFLAGS=-tags=x"}, c, GoEnvOptions{})
	want = []string{"HOME=/h", "GOFLAGS=-trimpath -tags=x", "GOTOOLCHAIN=local", "GOPROXY=off", "GOMODCACHE=/shared/mod", "GOCACHE=/shared/build"}
	if !slices.Equal(got, want) || len(forwarded) != 0 {
		t.Fatalf("got %q forwarded %q, want %q", got, forwarded, want)
	}
	got = ApplyGoParallelism(WithGoParallelism(context.Background(), 2), got)
	if !slices.Contains(got, "GOFLAGS=-trimpath -tags=x -p=2") {
		t.Fatalf("parallelism dropped the cache flags: %q", got)
	}
}

func TestCacheSettingsNote(t *testing.T) {
	if note := CacheSettingsNote("go: m@v1: module lookup disabled by GOPROXY=off", nil); !strings.Contains(note, "go mod download") {
		t.Fatalf("note %q", note)
	}
	if note := CacheSettingsNote("go: go.mod requires go >= 1.99 (running go 1.22; GOTOOLCHAIN=local)", nil); note == "" {
		t.Fatal("toolchain failure has no note")
	}
	if note := CacheSettingsNote("module lookup disabled by GOPROXY=off", []string{"GOPROXY"}); note != "" {
		t.Fatalf("a forwarded GOPROXY got a note: %q", note)
	}
	if note := CacheSettingsNote("--- FAIL: TestX", nil); note != "" {
		t.Fatalf("an ordinary failure got a note: %q", note)
	}
}

// TestGoCacheDirHonoursGOCACHESetAfterResolution: GOCACHE set after the process-wide
// resolution wins over it, GOCACHE=off means none, and unset falls back to the resolution.
func TestGoCacheDirHonoursGOCACHESetAfterResolution(t *testing.T) {
	resetGoCache(t)
	LookupGoCache = func(context.Context) (GoCache, bool) {
		return GoCache{ModCache: "/resolved/mod", BuildCache: "/resolved/build"}, true
	}
	t.Setenv("GOCACHE", "")
	if got := GoCacheDir(); got != "/resolved/build" {
		t.Fatalf("unset GOCACHE: got %q, want the resolved cache", got)
	}
	explicit := t.TempDir()
	t.Setenv("GOCACHE", explicit)
	if got := GoCacheDir(); got != explicit {
		t.Fatalf("GOCACHE after resolution: got %q, want %q", got, explicit)
	}
	t.Setenv("GOCACHE", "off")
	if got := GoCacheDir(); got != "" {
		t.Fatalf("GOCACHE=off: got %q, want none", got)
	}
}

// TestGoCacheDirRemembersAFailedLookup: a failed go env resolution is not repeated on
// every sample within goCacheRetry, and is tried again once that interval has passed.
func TestGoCacheDirRemembersAFailedLookup(t *testing.T) {
	resetGoCache(t)
	calls := 0
	LookupGoCache = func(context.Context) (GoCache, bool) { calls++; return GoCache{}, false }
	t.Setenv("GOCACHE", "")
	for range 5 {
		GoCacheDir()
	}
	if calls != 1 {
		t.Fatalf("lookups = %d, want 1 within the retry interval", calls)
	}
	goCacheMiss.Lock()
	goCacheMiss.at = time.Now().Add(-2 * goCacheRetry)
	goCacheMiss.Unlock()
	GoCacheDir()
	if calls != 2 {
		t.Fatalf("lookups = %d, want 2 after the retry interval", calls)
	}
}

func TestCheckTempDirTravelsOnContext(t *testing.T) {
	if _, ok := CheckTempDirFrom(context.Background()); ok {
		t.Fatal("empty context carried a TMPDIR")
	}
	if dir, ok := CheckTempDirFrom(WithCheckTempDir(context.Background(), "/lab/tmp")); !ok || dir != "/lab/tmp" {
		t.Fatalf("got %q %t", dir, ok)
	}
}
