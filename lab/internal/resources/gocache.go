package resources

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// GoCache is the absolute module and build cache that every Lab and evaluation go
// invocation shares, so Go's own test cache can serve unchanged packages across
// checkouts at different paths (with -trimpath).
type GoCache struct{ ModCache, BuildCache string }

// Cache states recorded on each check. Every check go invocation carries -trimpath in
// GOFLAGS: it keeps the checkout path out of the test binary, which would otherwise
// change the cache key of every package in every fresh checkout.
const (
	CacheEnabled        = "enabled"
	CacheFresh          = "disabled (fresh)"
	CacheFreshNonModule = "disabled (fresh: non-module change)"
	CacheUnavailable    = "disabled (go cache unavailable)"
)

var goCache struct {
	sync.Mutex
	value GoCache
	ok    bool
}

// LookupGoCache resolves GOMODCACHE and GOCACHE by running `go env` under the caller's
// (unscrubbed) environment, so they are known absolute paths before a check environment
// replaces HOME. It runs from os.TempDir() with GOTOOLCHAIN forced to "local", so a go.mod
// `toolchain` line can never trigger a toolchain download here. It is a variable only so
// tests can inject a fake instead of shelling out.
var LookupGoCache = func(ctx context.Context) (GoCache, bool) {
	r := process.Run(ctx, process.Options{
		Argv:           []string{"go", "env", "GOMODCACHE", "GOCACHE"},
		Dir:            os.TempDir(),
		Env:            EnvWithGotoolchainLocal(os.Environ()),
		Stdin:          []byte{},
		Timeout:        60 * time.Second,
		MaxOutputBytes: 1_000_000,
	})
	if !r.OK {
		return GoCache{}, false
	}
	return ParseGoEnvCache(r.Stdout)
}

type goCacheKey struct{}

// WithGoCache makes ResolveGoCache under ctx return c instead of the process-wide cache,
// so a test can run checks against a private cache that no earlier run has filled.
func WithGoCache(ctx context.Context, c GoCache) context.Context {
	return context.WithValue(ctx, goCacheKey{}, c)
}

// ResolveGoCache returns the process-wide shared cache, resolved once with LookupGoCache,
// unless WithGoCache set one on ctx. A failed resolution (no go toolchain) is not
// remembered, so a later call tries again.
func ResolveGoCache(ctx context.Context) (GoCache, bool) {
	if c, ok := ctx.Value(goCacheKey{}).(GoCache); ok {
		return c, true
	}
	goCache.Lock()
	defer goCache.Unlock()
	if goCache.ok {
		return goCache.value, true
	}
	c, ok := LookupGoCache(ctx)
	if ok {
		goCache.value, goCache.ok = c, true
	}
	return c, ok
}

// EnvWithGotoolchainLocal returns env with GOTOOLCHAIN forced to "local" (replacing any
// existing entry), so a go.mod `toolchain` line can never trigger a toolchain download.
func EnvWithGotoolchainLocal(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, "GOTOOLCHAIN=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, "GOTOOLCHAIN=local")
}

// ParseGoEnvCache parses the stdout of `go env GOMODCACHE GOCACHE`. ok is false unless the
// output has at least two lines whose first two values are both non-empty absolute paths;
// trailing blank lines and CRLF endings are tolerated.
func ParseGoEnvCache(stdout string) (GoCache, bool) {
	lines := strings.Split(stdout, "\n")
	if len(lines) < 2 {
		return GoCache{}, false
	}
	c := GoCache{ModCache: strings.TrimSpace(lines[0]), BuildCache: strings.TrimSpace(lines[1])}
	if c.ModCache == "" || c.BuildCache == "" || !filepath.IsAbs(c.ModCache) || !filepath.IsAbs(c.BuildCache) {
		return GoCache{}, false
	}
	return c, true
}

// GoEnvOptions adjust the cache settings for one go invocation.
type GoEnvOptions struct {
	// Fresh appends -count=1, so no test result is served from the cache.
	Fresh bool
}

// goFlagName is the name of a GOFLAGS entry: "-mod=vendor" and "--mod=vendor" are "mod".
func goFlagName(f string) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(f, "-"), "-"), "=")
	return name
}

// GoFlagsWithCache merges the cache flags into existing, the GOFLAGS a reviewed policy
// forwarded. No -mod is ever added: without one Go already uses readonly mode, or vendor
// mode for a module with a vendor directory, and a forwarded -mod is kept. -trimpath is
// added unless existing sets it, so -trimpath=false opts out. With o.Fresh, -count=1 is
// appended unless existing sets -count. Every existing flag is kept in order.
func GoFlagsWithCache(existing string, o GoEnvOptions) string {
	set := map[string]bool{}
	for _, f := range strings.Fields(existing) {
		set[goFlagName(f)] = true
	}
	flags := []string{}
	if !set["trimpath"] {
		flags = append(flags, "-trimpath")
	}
	flags = append(flags, strings.Fields(existing)...)
	if o.Fresh && !set["count"] {
		flags = append(flags, "-count=1")
	}
	return strings.Join(flags, " ")
}

// GoFlagsCarryCount reports whether env's GOFLAGS sets -count, so no test result in that
// environment comes from the cache.
func GoFlagsCarryCount(env []string) bool {
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, "GOFLAGS="); ok {
			for _, f := range strings.Fields(value) {
				if goFlagName(f) == "count" {
					return true
				}
			}
		}
	}
	return false
}

// ApplyGoCache sets the shared cache settings in env: GOCACHE and GOMODCACHE from c and
// GOFLAGS merged by GoFlagsWithCache. Existing GOCACHE and GOMODCACHE entries are
// replaced, so a forwarded caller value can never point a check at a cache under its
// private HOME. GOPROXY and GOTOOLCHAIN default to off and local; an entry already in env
// (one a reviewed policy forwarded) is kept and its name returned in forwarded. Apply
// ApplyGoParallelism afterwards; it keeps these flags.
func ApplyGoCache(env []string, c GoCache, o GoEnvOptions) (out, forwarded []string) {
	out = make([]string, 0, len(env)+5)
	existing := ""
	kept := map[string]bool{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		switch name {
		case "GOFLAGS":
			existing = value
		case "GOCACHE", "GOMODCACHE":
		case "GOPROXY", "GOTOOLCHAIN":
			if !kept[name] {
				kept[name] = true
				forwarded = append(forwarded, name)
				out = append(out, kv)
			}
		default:
			out = append(out, kv)
		}
	}
	out = append(out, "GOFLAGS="+GoFlagsWithCache(existing, o))
	if !kept["GOTOOLCHAIN"] {
		out = append(out, "GOTOOLCHAIN=local")
	}
	if !kept["GOPROXY"] {
		out = append(out, "GOPROXY=off")
	}
	return append(out, "GOMODCACHE="+c.ModCache, "GOCACHE="+c.BuildCache), forwarded
}

// CacheSettingsNote explains a failed go check whose output shows that Forgecell's own
// cache defaults (GOPROXY=off, GOTOOLCHAIN=local) stopped it, so it is not read as a
// candidate defect. It is empty for any other output, and for a setting the reviewed
// policy forwarded.
func CacheSettingsNote(output string, forwarded []string) string {
	proxy := strings.Contains(output, "module lookup disabled by GOPROXY=off") && !slices.Contains(forwarded, "GOPROXY")
	toolchain := strings.Contains(output, "GOTOOLCHAIN=local") && !slices.Contains(forwarded, "GOTOOLCHAIN")
	if !proxy && !toolchain {
		return ""
	}
	return "This failure comes from Forgecell's shared Go cache settings (GOPROXY=off, GOTOOLCHAIN=local, GOFLAGS with -trimpath), not necessarily from the candidate: warm the caller's module cache (for example `go mod download` in the module) and install the required toolchain, or forward GOPROXY or GOTOOLCHAIN in the reviewed policy."
}

type checkTempDirKey struct{}

// WithCheckTempDir makes every check environment built under ctx use dir as TMPDIR. Go
// folds a TMPDIR that a test reads (t.TempDir does) into its test cache key, so a stable
// value is what lets a cached result be reused.
func WithCheckTempDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, checkTempDirKey{}, dir)
}

// CheckTempDirFrom returns the TMPDIR WithCheckTempDir set on ctx.
func CheckTempDirFrom(ctx context.Context) (string, bool) {
	dir, ok := ctx.Value(checkTempDirKey{}).(string)
	return dir, ok && dir != ""
}
