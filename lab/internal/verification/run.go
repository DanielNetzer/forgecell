package verification

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

type Observation struct {
	Artifacts  []ArtifactInventory `json:"artifacts,omitempty"`
	ID         string              `json:"id"`
	Category   string              `json:"category"`
	SourceTree string              `json:"sourceTree"`
	Command    readiness.Check     `json:"command"`
	Result     process.Result      `json:"result"`
	// FailedTests is the observed failing set of a protected regression check with
	// declared deltas. It survives the compact ledger projection that drops captured
	// streams, so delivery can re-check coverage against the approved plan. A failing
	// protected go test -json check without deltas records its set when it parses
	// cleanly, as evidence for a reviewer; that never changes the verdict.
	FailedTests []TestID `json:"failedTests,omitempty"`
	// AcceptedDeltas are the declared deltas that failed as expected.
	AcceptedDeltas []readiness.ProtectedDelta `json:"acceptedDeltas,omitempty"`
	// Warnings name declared deltas that did not fail.
	Warnings []string `json:"warnings,omitempty"`
	// Cache states whether the check could be served from the shared Go cache
	// ("enabled") or always runs ("disabled (fresh)"). Packages counts the go test
	// packages its output reports served (cached) and run; both survive the compact
	// ledger projection that drops captured streams.
	Cache    string         `json:"cache,omitempty"`
	Packages *PackageCounts `json:"goTestPackages,omitempty"`
	// GoEnvForwarded names the cache defaults (GOPROXY, GOTOOLCHAIN) the reviewed policy
	// overrode by forwarding the caller's value.
	GoEnvForwarded []string `json:"goEnvForwarded,omitempty"`
	// Note explains a failure that Forgecell's own cache settings caused.
	Note string `json:"note,omitempty"`
	// Selection is how a check that declares select ran: the affected packages or the
	// full suite and why. Command keeps the approved argv.
	Selection *Selection `json:"selection,omitempty"`
	// JSONAdded means the protected view ran this direct go test check with -json
	// inserted after go test, because deltas are declared and its approved argv lacks
	// -json. The events were parsed as a stream and Result.Stdout keeps only a bounded
	// plain summary; Command keeps the approved argv.
	JSONAdded bool `json:"jsonAdded,omitempty"`
}

// MatchesCheckObservation binds delivery evidence to the approved command and
// view. Only valid explicit repairs permit candidate provenance or the protected
// runner's removal of the candidate-only acceptance reference. A failing
// protected regression observation matches only when the approved plan's declared
// deltas cover its entire failing set.
func MatchesCheckObservation(p readiness.Plan, check readiness.Check, obs Observation, tree string, protected bool) bool {
	if obs.ID != check.ID || obs.SourceTree != tree || obs.Result.Incomplete() {
		return false
	}
	if !obs.Result.OK && !(protected && acceptedProtectedFailure(p, check, obs)) {
		return false
	}
	expected := check
	category := check.Category
	if check.ReviewedAcceptance != "" {
		if p.Validate() != nil {
			return false
		}
		found := false
		for _, approved := range p.Analysis.Checks {
			if reflect.DeepEqual(approved, check) {
				found = true
			}
		}
		if !found {
			return false
		}
		if protected {
			expected.ReviewedAcceptance = ""
		} else if obs.Category == "candidate" {
			category = "candidate"
		}
	}
	return obs.Category == category && reflect.DeepEqual(obs.Command, expected)
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
	// CandidateError keeps the candidate view's own failure reason when a later step,
	// such as the protected regression view, replaces Error.
	CandidateError string `json:"candidateError,omitempty"`
	// AcceptedDeltas failed in the protected view exactly as the approved plan declared.
	AcceptedDeltas []readiness.ProtectedDelta `json:"acceptedProtectedDeltas,omitempty"`
	// Warnings name declared deltas that did not fail or were never exercised.
	Warnings []string `json:"warnings,omitempty"`
	// CleanupError records a temp directory that could not be removed. It never changes
	// the verdict; the directory stays registered so exit cleanup tries again.
	CleanupError string `json:"cleanupError,omitempty"`
	// Notes record how the view's environment departed from the usual one, such as a
	// stable check TMPDIR that vanished and was replaced by a private one.
	Notes []string `json:"notes,omitempty"`
}

// removeTemp removes a registered temp directory and records a failure on v.
func removeTemp(v *Result, dir string) {
	if err := resources.Remove(dir); err != nil {
		v.CleanupError = strings.TrimPrefix(strings.Join([]string{v.CleanupError, err.Error()}, "; "), "; ")
	}
}

// labGoTempDir creates, or reuses, the stable GOTMPDIR <tmp>/gotmp at 0700 under a Lab's
// resolved check TMPDIR. testing.TempDir reads GOTMPDIR and the test-result cache keys on
// it, so every view of a Lab gets the same one; Go's own go-build* work directories in it
// are unique per invocation and removed by Go. It sits directly under the Lab's check
// TMPDIR (resources.LabTempDir), whose marker-based stale prune keeps it and prunes its old
// children. As for resources.LabTempDir, a symbolic link, a non-directory or another
// user's directory at that path is refused.
func labGoTempDir(tmp string) (string, error) {
	dir := filepath.Join(tmp, resources.LabGoTempName)
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("lab GOTMPDIR is a symbolic link: %s", dir)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("lab GOTMPDIR is not a directory: %s", dir)
	}
	if !resources.OwnedByUser(dir, info) {
		return "", fmt.Errorf("lab GOTMPDIR belongs to another user: %s", dir)
	}
	if info.Mode().Perm() != 0700 {
		if err := os.Chmod(dir, 0700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// commandEnvironment builds a view's filtered environment: a private HOME, the given
// TMPDIR and the reviewed forwarded names. Every view also gets the shared Go cache from
// applyGoCache, so no go invocation falls back to a cache under its private HOME.
func commandEnvironment(p readiness.ExecutionPolicy, home, tmp string) ([]string, []string, error) {
	env := []string{"HOME=" + home, "TMPDIR=" + tmp, "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
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

// goCommandEnv adds the process-wide shared Go cache to a view's env for one command:
// GOCACHE, GOMODCACHE, GOPROXY and GOTOOLCHAIN defaults, and GOFLAGS with -trimpath (no
// -mod, so a vendored module keeps vendor mode). fresh appends -count=1. cache is false
// when go env could not resolve the cache (no go toolchain); env is then returned
// unchanged.
func goCommandEnv(ctx context.Context, env []string, fresh bool) (out, forwarded []string, cache bool) {
	c, ok := resources.ResolveGoCache(ctx)
	if !ok {
		return env, nil, false
	}
	out, forwarded = resources.ApplyGoCache(env, c, resources.GoEnvOptions{Fresh: fresh})
	return out, forwarded, true
}

// goFlagsFailfast reports whether a command env's GOFLAGS asks go test to stop after
// its first failure, which would let a declared failure stand in for later tests.
func goFlagsFailfast(env []string) bool {
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, "GOFLAGS="); ok && readiness.RequestsGoTestFailfast([]string{value}) {
			return true
		}
	}
	return false
}

// CacheStatus is the cache state a check record states, derived from the env the check
// actually ran with: "disabled (fresh)" when that env's GOFLAGS or a go argv sets -count
// (hidden acceptance, a non-module change, an explicit -count), "enabled" for any other
// go argv, and empty for a non-go argv such as gofmt, whatever its GOFLAGS. nonModule
// names why a fresh run was forced.
func CacheStatus(argv, env []string, cache, nonModule bool) string {
	if len(argv) == 0 || filepath.Base(argv[0]) != "go" {
		return ""
	}
	if !cache {
		return resources.CacheUnavailable
	}
	fresh := resources.GoFlagsCarryCount(env)
	for i, arg := range argv {
		if strings.HasPrefix(arg, "-count=") || strings.HasPrefix(arg, "--count=") || ((arg == "-count" || arg == "--count") && i+1 < len(argv)) {
			fresh = true
		}
	}
	switch {
	case fresh && nonModule:
		return resources.CacheFreshNonModule
	case fresh:
		return resources.CacheFresh
	}
	return resources.CacheEnabled
}

// AcceptanceCacheStatus is the cache state an independent-acceptance check records,
// whatever its argv: it always runs with -count=1 when the cache is in use.
func AcceptanceCacheStatus(cache bool) string {
	if !cache {
		return resources.CacheUnavailable
	}
	return resources.CacheFresh
}

type nonModuleKey struct{}

// changesOutsideModules reports whether any changed path lies outside the root of any
// go.mod module among the tree's files (or the tree has no module). Go's cache does not
// track files outside a module (docs, scripts, data a test reads with ../), so a cached
// pass of that module could be stale; with several modules (a root go.mod plus nested
// ones, or go.work modules) a change outside one of them is enough. A go.mod that Go
// itself ignores (under testdata/ or vendor/, or a directory starting with _ or .) is
// a fixture, not a module.
func changesOutsideModules(changed, files []string) bool {
	roots := []string{}
	for _, f := range files {
		if f == "go.mod" {
			roots = append(roots, "")
		} else if dir, ok := strings.CutSuffix(f, "/go.mod"); ok && !goIgnoredDir(dir) {
			roots = append(roots, dir+"/")
		}
	}
	for _, f := range changed {
		if len(roots) == 0 {
			return true
		}
		for _, root := range roots {
			if !strings.HasPrefix(f, root) {
				return true
			}
		}
	}
	return false
}

// goIgnoredDir reports whether Go ignores the slash-separated directory dir: any element
// is testdata or vendor, or starts with _ or . .
func goIgnoredDir(dir string) bool {
	for _, elem := range strings.Split(dir, "/") {
		if elem == "testdata" || elem == "vendor" || strings.HasPrefix(elem, "_") || strings.HasPrefix(elem, ".") {
			return true
		}
	}
	return false
}

// treeChangesOutsideModules is changesOutsideModules for the change from base to tree.
func treeChangesOutsideModules(ctx context.Context, source, base, tree string) (bool, error) {
	changed, err := git(ctx, source, "", nil, "diff-tree", "--no-commit-id", "--no-renames", "--name-only", "-r", "-z", base, tree, "--")
	if err != nil {
		return false, err
	}
	files, err := git(ctx, source, "", nil, "ls-tree", "-r", "--name-only", "-z", tree)
	if err != nil {
		return false, err
	}
	return changesOutsideModules(paths(changed), paths(files)), nil
}

// PackageCounts is the number of go test packages a check's output reports served from
// the cache ("ok <pkg> (cached)") and actually run ("ok <pkg> <time>" or "FAIL <pkg> ...").
type PackageCounts struct {
	Cached int `json:"cached"`
	Run    int `json:"run"`
}

var goTestPackageLine = regexp.MustCompile(`^(ok|FAIL)\s+(\S+)(?:\s+(.*))?$`)

// CountGoTestPackages reads the package summary lines of go test output, plain or -json
// (package-level output events), and returns nil when there are none.
func CountGoTestPackages(stdout string) *PackageCounts {
	counts := PackageCounts{}
	count := func(line string) {
		m := goTestPackageLine.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
		if m == nil {
			return
		}
		if m[1] == "ok" && strings.Contains(m[3], "(cached)") {
			counts.Cached++
		} else {
			counts.Run++
		}
	}
	for _, line := range strings.Split(stdout, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "{") {
			var ev struct{ Action, Test, Output string }
			if json.Unmarshal([]byte(t), &ev) == nil {
				if ev.Action == "output" && ev.Test == "" {
					count(ev.Output)
				}
				continue
			}
		}
		count(line)
	}
	if counts.Cached == 0 && counts.Run == 0 {
		return nil
	}
	return &counts
}

// CacheTotals sums the package counts of r's checks.
func (r Result) CacheTotals() PackageCounts {
	total := PackageCounts{}
	for _, o := range r.Checks {
		if o.Packages != nil {
			total.Cached += o.Packages.Cached
			total.Run += o.Packages.Run
		}
	}
	return total
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
func runViewObserved(ctx context.Context, source, destination string, p readiness.Plan, c CaptureResult, observe func(Observation) error) Result {
	return runViewDeltas(ctx, source, destination, p, c, observe, false)
}

// runViewDeltas applies the plan's declared protected deltas only when protected
// is true, which only the protected regression view requests. The candidate view
// never relaxes a failing check.
func runViewDeltas(ctx context.Context, source, destination string, p readiness.Plan, c CaptureResult, observe func(Observation) error, protected bool) (v Result) {
	v = Result{SchemaVersion: "v1", Workspace: destination, SourceTree: c.Tree, Setup: []Observation{}, Checks: []Observation{}, Containment: "Filtered environment and temporary HOME; no OS filesystem/network sandbox. Network access is unrestricted."}
	fail := func(e error) Result {
		// A hard-floor cancellation names itself, so the record says why checks stopped.
		if reason, ok := resources.Exhausted(ctx); ok {
			e = fmt.Errorf("%s: %w", reason, e)
		}
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
	// A change outside every Go module root runs every go command fresh. RunObserved
	// decides once from the candidate tree, so the protected view follows it.
	nonModule, decided := ctx.Value(nonModuleKey{}).(bool)
	if !decided {
		if nonModule, err = treeChangesOutsideModules(ctx, source, p.Inputs.BaseCommit, c.Tree); err != nil {
			return fail(err)
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
	repaired := map[string]bool{}
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
				if command.Category != "candidate" || !slices.Contains(p.NewFiles, id) {
					return fail(fmt.Errorf("command %s has missing definition %s", command.ID, id))
				}
				// A new-file definition has no frozen bytes to hash. Its absence from the
				// base commit is the only thing that makes it safe to run unhashed.
				existing, lsErr := git(ctx, source, "", nil, "ls-tree", "-z", p.Inputs.BaseCommit, "--", id)
				if lsErr != nil || existing != "" {
					return fail(fmt.Errorf("command %s cites new file %s, but the base commit already contains it", command.ID, id))
				}
				continue
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
				// Only an explicit reviewed repair permits execution of an edited
				// scoped checker. It is candidate evidence; the original hash is
				// still enforced on the separately retained protected view.
				if command.ReviewedAcceptance == "" || !allowed[e.Path] || reviewed[id].ID != "" || err != nil {
					return fail(fmt.Errorf("command definition %s changed; review a scope/check amendment before execution", e.Path))
				}
				baseline, baselineErr := git(ctx, source, "", nil, "cat-file", "blob", p.Inputs.BaseCommit+":"+e.Path)
				if baselineErr != nil || fmt.Sprintf("%x", sha256.Sum256([]byte(baseline))) != e.SHA256 {
					return fail(fmt.Errorf("protected command definition %s does not match frozen baseline", e.Path))
				}
				repaired[command.ID] = true
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
	release, err := resources.LockWorktrees(ctx, source)
	if err != nil {
		return fail(err)
	}
	_, err = git(ctx, source, "", nil, "worktree", "add", "--no-checkout", "--detach", destination, p.Inputs.BaseCommit)
	release()
	if err != nil {
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
	// Go writes the module cache under the private HOME read-only, so these are removed
	// with resources.Remove, which makes the tree writable first; they are also registered
	// so a signal still removes them. A removal failure is recorded, not ignored.
	home, err := resources.MkdirTemp("", "forgecell-check-home-")
	if err != nil {
		return fail(err)
	}
	defer removeTemp(&v, home)
	// A stable TMPDIR (the Lab's check TMPDIR under the system temp dir, outside the user's
	// home) keeps tests that read it cacheable across views; without one the private HOME
	// doubles as TMPDIR, as before.
	tmp, ok := resources.CheckTempDirFrom(ctx)
	if !ok {
		tmp = home
	} else {
		// Resolved first, so tests never see a symlinked TMPDIR when <lab> sits behind a
		// symbolic link (/var -> /private/var on macOS). One that vanished since the run
		// began (an OS temp cleaner) leaves this view with the private HOME as TMPDIR.
		resolved, err := filepath.EvalSymlinks(tmp)
		if os.IsNotExist(err) {
			v.Notes = append(v.Notes, "The Lab's check TMPDIR "+tmp+" vanished during verification; this view used a private TMPDIR, so tests that read it were not cacheable.")
			tmp, ok = home, false
		} else if err != nil {
			return fail(err)
		} else {
			tmp = resolved
		}
	}
	env, secrets, err := commandEnvironment(p.Policy, home, tmp)
	if err != nil {
		return fail(err)
	}
	if ok {
		gotmp, err := labGoTempDir(tmp)
		if err != nil {
			return fail(err)
		}
		env = append(slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "GOTMPDIR=") }), "GOTMPDIR="+gotmp)
	}
	controls, err := resources.MkdirTemp("", "forgecell-reviewed-checks-")
	if err != nil {
		return fail(err)
	}
	defer removeTemp(&v, controls)
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
	// A selecting check runs the packages its directory's selection names. The candidate
	// view computes it from the captured change against the base commit; the protected
	// view reuses that exact selection (RunObserved shares it), or runs the full suite.
	memo, _ := ctx.Value(selectionKey{}).(*selectionMemo)
	selections := map[string]Selection{}
	selectionFor := func(dir string, env []string) Selection {
		if s, ok := selections[dir]; ok {
			return s
		}
		var s Selection
		if protected {
			ok := false
			if memo != nil {
				s, ok = memo.get(dir)
			}
			if !ok {
				s = fullSuite("the candidate view recorded no selection for this check directory")
			}
		} else {
			s = computeSelection(ctx, source, p.Inputs.BaseCommit, c.Tree, destination, dir, env, p.FullSuiteTriggers)
			if memo != nil {
				memo.put(dir, s)
			}
		}
		selections[dir] = s
		return s
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
		// Hidden acceptance is fresh by construction, whatever its argv runs.
		dir := filepath.Join(destination, filepath.FromSlash(command.Dir))
		commandEnv, forwarded, cache := goCommandEnv(ctx, env, nonModule || command.Category == "independent-acceptance")
		// A cohort attempt bounds each go command's build parallelism through GOFLAGS, after
		// the cache flags so both are kept.
		commandEnv = resources.ApplyGoParallelism(ctx, commandEnv)
		var selection *Selection
		if i >= len(p.Setup) && command.Select != "" {
			s := selectionFor(command.Dir, commandEnv)
			selection = &s
			argv = s.Argv(argv)
		}
		protectedRegression := protected && i >= len(p.Setup) && command.Required && command.Category == "regression"
		// The candidate view keeps exactly its earlier behaviour: only deltas naming the
		// check lift its cap. The protected view gives a direct go test check the deltas
		// that name it and those that opted into deltaScope identity (DeltasApplyingTo),
		// unless the env the check runs with asks for -failfast, which could let a
		// declared failure mask others.
		declared := p.DeltasForCheck(command.ID)
		identity := false
		if protectedRegression {
			if readiness.IdentityDeltaCheck(command) && !goFlagsFailfast(commandEnv) {
				declared, identity = p.DeltasApplyingTo(command), true
			}
		}
		// In the protected view only, a direct go test check that lacks -json runs with
		// -json inserted after go test, so its declared deltas can be matched. The
		// approved argv, recorded as Command, is unchanged.
		jsonAdded := identity && len(declared) > 0 && !readiness.RequestsGoTestJSON(command.Argv)
		if jsonAdded {
			argv = readiness.WithGoTestJSON(argv)
		}
		options := process.Options{Argv: argv, Dir: dir, Env: commandEnv, Stdin: []byte{}, Timeout: time.Duration(command.TimeoutMS) * time.Millisecond, MaxOutputBytes: 256000}
		// A required regression check with declared deltas asks for go test -json, which
		// is read as a stream so a large suite is not cut off by the capture cap. Every
		// other check keeps the cap, and exceeding it still fails the check.
		var parser *failedTestParser
		candidateStream := false
		if i >= len(p.Setup) && command.Required && command.Category == "regression" && len(declared) > 0 && (readiness.RequestsGoTestJSON(command.Argv) || jsonAdded) {
			options.StdoutLines = &process.LineSink{Line: func([]byte) error { return nil }}
			if protected {
				parser = newFailedTestParser()
				options.StdoutLines.Line = parser.Line
			} else {
				candidateStream = true
			}
		}
		result := process.Run(ctx, options)
		if jsonAdded {
			// The stream was parsed while it ran; the retained copy is replaced by a
			// bounded plain summary, never the raw -json stream or a prefix of it.
			var failed []TestID
			if !result.OK {
				failed, _ = parser.Finish()
			}
			result.Stdout = parser.Summary(failed)
			result.RawStdout, result.Truncated = result.Stdout, false
		}
		// The no-op sink above lifts the capture cap, so only this check keeps the
		// candidate view's overflow guard: a truncated stream is not a pass.
		var truncatedErr error
		if candidateStream && result.Truncated {
			truncatedErr = fmt.Errorf("output exceeded the %d byte limit and was truncated; the candidate view cannot pass a truncated check", options.MaxOutputBytes)
			result.OK = false
			if result.Error == "" {
				result.Error = truncatedErr.Error()
			}
		}
		category := command.Category
		if repaired[command.ID] {
			category = "candidate"
		}
		cacheState := CacheStatus(argv, commandEnv, cache, nonModule)
		if command.Category == "independent-acceptance" {
			cacheState = AcceptanceCacheStatus(cache)
		}
		observation := Observation{ID: command.ID, Category: category, SourceTree: c.Tree, Command: command, Result: scrub(result, secrets), Cache: cacheState, Packages: CountGoTestPackages(result.Stdout), GoEnvForwarded: forwarded, Selection: selection, JSONAdded: jsonAdded}
		if cache && !result.OK {
			observation.Note = resources.CacheSettingsNote(observation.Result.Stdout+"\n"+observation.Result.Stderr, forwarded)
		}
		var deltaErr error
		evaluated := false
		if protectedRegression {
			// A delta applies by identity only to a check that ran its package; one that
			// names this check by CheckID always applies, as before. Which packages ran
			// is known only from a stream that can be trusted, so a failing check whose
			// stream was refused keeps every declared delta and is refused with its reason.
			applicable := declared
			trusted := false
			if parser != nil {
				_, finishErr := parser.Finish()
				trusted = result.OK || (finishErr == nil && checkProtectedResult(observation.Result) == nil)
			}
			if identity && trusted {
				applicable = nil
				for _, d := range declared {
					if d.CheckID == command.ID || parser.ran(d.Package) {
						applicable = append(applicable, d)
					}
				}
			}
			// The failing set of a cleanly parsed failing stream is recorded even when
			// it is not covered, so --accept-protected-failures can name it; it never
			// changes the verdict.
			recordStreamed := func() {
				if parser != nil && !result.OK && checkProtectedResult(observation.Result) == nil {
					if failed, err := parser.Finish(); err == nil {
						observation.FailedTests = failed
					}
				}
			}
			if len(applicable) > 0 {
				evaluated = true
				var evaluation DeltaEvaluation
				if deltaErr = verifyDeltaPackages(destination, applicable); deltaErr == nil {
					if parser != nil {
						evaluation, deltaErr = evaluateStreamedDeltas(applicable, observation.Result, parser)
					} else {
						evaluation, deltaErr = EvaluateProtectedDeltas(applicable, observation.Result)
					}
				}
				if deltaErr == nil {
					observation.FailedTests = evaluation.Failed
					observation.AcceptedDeltas = evaluation.Accepted
					observation.Warnings = evaluation.Warnings
				} else {
					recordStreamed()
				}
			} else if parser != nil {
				recordStreamed()
			} else if !result.OK && !result.Overflow && readiness.RequestsGoTestJSON(command.Argv) {
				// No delta is declared, so this changes no verdict. The failing tests are
				// recorded so a reviewer drafting expectedProtectedDeltas can name them.
				if failed, err := parseFailedTests(observation.Result.Stdout); err == nil {
					observation.FailedTests = failed
				}
			}
		}
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
		if !result.OK && (i < len(p.Setup) || command.Required) && !(evaluated && deltaErr == nil) {
			if deltaErr != nil {
				return fail(fmt.Errorf("required command %s did not pass: %w", command.ID, deltaErr))
			}
			if truncatedErr != nil {
				return fail(fmt.Errorf("required command %s did not pass: %w", command.ID, truncatedErr))
			}
			return fail(fmt.Errorf("required command %s did not pass", command.ID))
		}
		if evaluated && deltaErr == nil {
			// A delta applied by identity to several checks is listed once per
			// declaration; each observation keeps its own copy.
			for _, d := range observation.AcceptedDeltas {
				if !slices.ContainsFunc(v.AcceptedDeltas, func(seen readiness.ProtectedDelta) bool {
					return seen.CheckID == d.CheckID && seen.Package == d.Package && seen.Test == d.Test
				}) {
					v.AcceptedDeltas = append(v.AcceptedDeltas, d)
				}
			}
			for _, w := range observation.Warnings {
				if !slices.Contains(v.Warnings, w) {
					v.Warnings = append(v.Warnings, w)
				}
			}
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
