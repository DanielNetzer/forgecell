package verification

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// Cost tiers a view runs its checks in, cheapest first, so a failing round ends before
// the slow full suite: formatting and vet, then build, then targeted tests (and every
// other command), then the full suite. Setup commands always run first, in plan order.
const (
	costFormat = iota
	costBuild
	costTargeted
	costFullSuite
)

// Reasons a check of a view never ran. A not-run check never passed.
const (
	NotRunEarlierFailure   = "not run (an earlier required check failed)"
	NotRunCandidateFailure = "not run (a required candidate check failed)"
)

// errCandidateFailed cancels a protected view running beside a candidate view whose
// required check failed. It unwraps to context.Canceled, so the in-flight check is
// recorded as interrupted.
var errCandidateFailed = fmt.Errorf("a required candidate check failed: %w", context.Canceled)

// checkCost classifies a check by its approved argv.
func checkCost(c readiness.Check) int {
	argv := c.Argv
	if len(argv) == 0 {
		return costTargeted
	}
	base := filepath.Base(argv[0])
	sub := ""
	if len(argv) > 1 {
		sub = argv[1]
	}
	switch {
	case base == "gofmt", base == "go" && sub == "vet":
		return costFormat
	case base == "go" && sub == "build":
		return costBuild
	case base == "go" && sub == "test":
		for _, arg := range argv[2:] {
			if arg == "-args" || arg == "--args" {
				break
			}
			if arg == "./..." || arg == "all" || arg == "..." {
				return costFullSuite
			}
		}
		return costTargeted
	case (base == "sh" || base == "bash") && len(argv) == 3 && argv[1] == "-c" && formatOnlyScript(argv[2]):
		return costFormat
	}
	return costTargeted
}

// formatOnlyScript reports a reviewed shell string that only formats or vets.
func formatOnlyScript(script string) bool {
	return (strings.Contains(script, "gofmt") || strings.Contains(script, "go vet")) && !strings.Contains(script, "go test") && !strings.Contains(script, "go build")
}

// executionOrder returns the indices of commands in the order a view runs them: the
// setup commands (the first setup of them) in plan order, then the checks by cost tier,
// keeping plan order within a tier. The protected view keeps plan order: it runs only
// once the candidate view's cheap checks passed (or beside it, stopped by its failure),
// and --accept-protected-failures reads its run as a plan-order prefix ending at the
// check it stopped on.
func executionOrder(commands []readiness.Check, setup int, protected bool) []int {
	order := make([]int, len(commands))
	for i := range order {
		order[i] = i
	}
	if protected {
		return order
	}
	checks := order[setup:]
	sort.SliceStable(checks, func(a, b int) bool { return checkCost(commands[checks[a]]) < checkCost(commands[checks[b]]) })
	return order
}

// notRunObservation records a check the view never started. Its result is a stopped,
// failed non-run, so no reader can take it for a pass.
func notRunObservation(command readiness.Check, category, tree, reason string) Observation {
	return Observation{ID: command.ID, Category: category, SourceTree: tree, Command: command, NotRun: reason, Result: process.Result{Reconciled: true, Code: -1, Error: reason}}
}

// coveredFailedReason is the not-run reason of a targeted check whose covering baseline
// check failed: it never ran, so it neither passed nor carries the baseline's failure.
func coveredFailedReason(covering string) string {
	return "covered by " + covering + ", which failed"
}

// coveredObservation records a targeted check the protected view skipped because the
// covering baseline check ran a superset of its packages. Its result is the covering
// check's outcome without its streams or timing, and CoveredBy names that check, so it
// is never read as a pass of its own. A failing covering check makes it a not-run
// record instead, never a copy of that failure.
func coveredObservation(command readiness.Check, tree string, covering Observation) Observation {
	if !covering.Result.OK {
		o := notRunObservation(command, command.Category, tree, coveredFailedReason(covering.ID))
		o.CoveredBy = covering.ID
		return o
	}
	r := covering.Result
	r.Stdout, r.Stderr, r.RawStdout, r.ElapsedMS, r.Activity = "", "", "", 0, nil
	return Observation{ID: command.ID, Category: command.Category, SourceTree: tree, Command: command, CoveredBy: covering.ID, Result: r, Cache: covering.Cache}
}

// strictGoTestPackages returns the package patterns of a direct go test argv whose only
// flag is -timeout (and -json when allowJSON), so two such checks differ in nothing but
// their packages. Every pattern must be relative to the check directory, clean, and
// outside any directory Go ignores. Any other flag or pattern makes the argv ineligible.
// A covered check never requests -json itself: a reviewer who asked for its stream gets
// it from its own run.
func strictGoTestPackages(argv []string, allowJSON bool) ([]string, bool) {
	if !readiness.IsDirectGoTest(argv) {
		return nil, false
	}
	var pkgs []string
	for i := 2; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "-timeout" || arg == "--timeout":
			if i+1 >= len(argv) {
				return nil, false
			}
			i++
		case strings.HasPrefix(arg, "-timeout=") || strings.HasPrefix(arg, "--timeout="), allowJSON && (arg == "-json" || arg == "--json"):
		case strings.HasPrefix(arg, "-"):
			return nil, false
		default:
			if _, _, ok := splitPattern(arg); !ok {
				return nil, false
			}
			pkgs = append(pkgs, arg)
		}
	}
	return pkgs, len(pkgs) > 0
}

// splitPattern reads a relative package pattern: "." or "./dir", optionally ending in
// "/..." (".../..." for the whole module is "./..."). dir is slash-separated and "" for
// the check directory itself.
func splitPattern(pattern string) (dir string, recursive bool, ok bool) {
	switch pattern {
	case ".":
		return "", false, true
	case "./...":
		return "", true, true
	}
	rest, found := strings.CutPrefix(pattern, "./")
	if !found {
		return "", false, false
	}
	rest, recursive = strings.CutSuffix(rest, "/...")
	if rest == "" || path.Clean(rest) != rest || strings.ContainsAny(rest, `\*?[`) || strings.Contains(rest, "...") || goIgnoredDir(rest) {
		return "", false, false
	}
	for _, elem := range strings.Split(rest, "/") {
		if elem == "." || elem == ".." || elem == "" {
			return "", false, false
		}
	}
	return rest, recursive, true
}

// patternCovers reports whether every package the target pattern names is also named
// by base, both relative to the same directory.
func patternCovers(base, target string) bool {
	bdir, brec, ok := splitPattern(base)
	if !ok {
		return false
	}
	tdir, trec, ok := splitPattern(target)
	if !ok {
		return false
	}
	if !brec {
		return !trec && bdir == tdir
	}
	return bdir == "" || tdir == bdir || strings.HasPrefix(tdir, bdir+"/")
}

// coveringBaseline returns the protected baseline check among view whose packages are a
// superset of check's, so the protected view need not run check separately. It applies
// only to a required targeted direct go test regression check that is not itself a
// baseline, has no select and differs from the baseline in nothing but its packages
// (and -timeout), in a plan that declares no protected deltas, against a baseline in
// the same directory without select. Anything else runs on its own.
func coveringBaseline(p readiness.Plan, check readiness.Check, view []readiness.Check) (readiness.Check, bool) {
	if len(p.ExpectedProtectedDeltas) > 0 || slices.Contains(p.FormulaBaseline, check.ID) || !readiness.IdentityDeltaCheck(check) || check.Select != "" || check.ReviewedAcceptance != "" {
		return readiness.Check{}, false
	}
	pkgs, ok := strictGoTestPackages(check.Argv, false)
	if !ok {
		return readiness.Check{}, false
	}
	for _, b := range view {
		if b.ID == check.ID || !slices.Contains(p.FormulaBaseline, b.ID) || !readiness.IdentityDeltaCheck(b) || b.Select != "" || b.Dir != check.Dir {
			continue
		}
		basePkgs, ok := strictGoTestPackages(b.Argv, true)
		if !ok {
			continue
		}
		covered := true
		for _, pkg := range pkgs {
			if !slices.ContainsFunc(basePkgs, func(base string) bool { return patternCovers(base, pkg) }) {
				covered = false
				break
			}
		}
		if covered {
			return b, true
		}
	}
	return readiness.Check{}, false
}

// crossesNestedModule reports whether a package of check lies in a Go module nested
// below the covering baseline's root in workspace root: go test ./... skips nested
// modules, so such a package is not run by the baseline. The targeted package directory
// and every directory up to the baseline pattern's root are inspected with
// Lstat; one holding any go.mod entry, one that is not a real directory, or one that
// cannot be read counts as crossing, so coverage fails safe to running the check.
func crossesNestedModule(root string, check, baseline readiness.Check) bool {
	pkgs, ok := strictGoTestPackages(check.Argv, false)
	basePkgs, baseOK := strictGoTestPackages(baseline.Argv, true)
	if !ok || !baseOK {
		return true
	}
	checkDir := filepath.Join(root, filepath.FromSlash(check.Dir))
	nested := func(dir string) bool {
		at := filepath.Join(checkDir, filepath.FromSlash(dir))
		info, err := os.Lstat(at)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return true
		}
		_, err = os.Lstat(filepath.Join(at, "go.mod"))
		return !errors.Is(err, os.ErrNotExist)
	}
	for _, pkg := range pkgs {
		tdir, _, _ := splitPattern(pkg)
		inside := false
		for _, base := range basePkgs {
			if !patternCovers(base, pkg) {
				continue
			}
			bdir, _, _ := splitPattern(base)
			// The baseline's own root counts too, unless it is the check directory, whose
			// go.mod is the module both checks run in.
			crosses := false
			for d := tdir; d != ""; d = parentDir(d) {
				if nested(d) {
					crosses = true
					break
				}
				if d == bdir {
					break
				}
			}
			if !crosses {
				inside = true
				break
			}
		}
		if !inside {
			return true
		}
	}
	return false
}

// parentDir returns the slash-separated parent of dir, "" for a top-level one.
func parentDir(dir string) string {
	if parent := path.Dir(dir); parent != "." {
		return parent
	}
	return ""
}

// CoveringCheckID names the protected baseline check of p that covers check in the
// protected view, judged from the plan alone; the view also refuses coverage across a
// nested module, so a check may still have run on its own.
func CoveringCheckID(p readiness.Plan, check readiness.Check) (string, bool) {
	view := []readiness.Check{}
	for _, c := range p.Analysis.Checks {
		if c.Category == "regression" {
			c.ReviewedAcceptance = ""
			view = append(view, c)
		}
	}
	expected := check
	expected.ReviewedAcceptance = ""
	b, ok := coveringBaseline(p, expected, view)
	return b.ID, ok
}

// protectedCoverage maps each covering check index to the indices it covers, for the
// protected view's checks (commands[setup:]) in workspace root.
func protectedCoverage(p readiness.Plan, commands []readiness.Check, setup int, root string) (covers map[int][]int, covered map[int]bool) {
	covers, covered = map[int][]int{}, map[int]bool{}
	view := commands[setup:]
	for i := setup; i < len(commands); i++ {
		b, ok := coveringBaseline(p, commands[i], view)
		if !ok || crossesNestedModule(root, commands[i], b) {
			continue
		}
		for j := setup; j < len(commands); j++ {
			if commands[j].ID == b.ID {
				covers[j] = append(covers[j], i)
				covered[i] = true
			}
		}
	}
	return covers, covered
}

// validCoverage reports whether obs, a protected observation of check that names the
// check covering it, is backed by a covering baseline of the approved plan.
func validCoverage(p readiness.Plan, check readiness.Check, obs Observation) bool {
	id, ok := CoveringCheckID(p, check)
	return ok && id == obs.CoveredBy
}

// Bounds of the failing output an observation keeps beside its full streams: a head and
// a tail of stdout and of stderr, and the failing test names.
const (
	excerptHeadBytes   = 2 << 10
	excerptTailBytes   = 6 << 10
	maxExcerptTests    = 50
	maxExcerptTestName = 300
)

// OutputExcerpt is a failing check's bounded, redacted output: the head and tail of
// stdout and stderr, and the failing test names of a direct go test check. It survives
// the compact ledger projection that drops captured streams, so a failure is never
// recorded without its output. Local paths of the view are replaced by placeholders.
type OutputExcerpt struct {
	Stdout         string   `json:"stdout,omitempty"`
	Stderr         string   `json:"stderr,omitempty"`
	StdoutCut      bool     `json:"stdoutCut,omitempty"`
	StderrCut      bool     `json:"stderrCut,omitempty"`
	FailedTests    []string `json:"failedTests,omitempty"`
	FailedTestsCut bool     `json:"failedTestsCut,omitempty"`
}

// excerpt keeps text whole when it fits, or its head and tail with the dropped size.
func excerpt(text string) (string, bool) {
	if len(text) <= excerptHeadBytes+excerptTailBytes {
		return strings.ToValidUTF8(text, "?"), false
	}
	dropped := len(text) - excerptHeadBytes - excerptTailBytes
	out := text[:excerptHeadBytes] + fmt.Sprintf("\n[forgecell: %d byte(s) between head and tail dropped]\n", dropped) + text[len(text)-excerptTailBytes:]
	return strings.ToValidUTF8(out, "?"), true
}

var plainFailedTest = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)

// failedTestNames lists the failing tests of a check: the parsed set when there is one,
// else the "--- FAIL:" lines of plain go test output, within maxExcerptTests.
func failedTestNames(stdout string, failed []TestID) ([]string, bool) {
	names := []string{}
	add := func(name string) bool {
		if len(names) == maxExcerptTests {
			return false
		}
		if len(name) > maxExcerptTestName {
			name = name[:maxExcerptTestName] + "…"
		}
		names = append(names, strings.ToValidUTF8(name, "?"))
		return true
	}
	for _, id := range failed {
		if !add(id.Package + " " + id.Test) {
			return names, true
		}
	}
	if len(failed) > 0 {
		return names, false
	}
	for _, line := range strings.Split(stdout, "\n") {
		if m := plainFailedTest.FindStringSubmatch(line); m != nil && !slices.Contains(names, m[1]) {
			if !add(m[1]) {
				return names, true
			}
		}
	}
	return names, false
}

// localPaths replaces the view's local directories in recorded text with placeholders,
// longest first, so a nested directory is never half-replaced.
func localPaths(pairs map[string]string) *strings.Replacer {
	keys := []string{}
	for k := range pairs {
		if k != "" && k != "/" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	args := []string{}
	for _, k := range keys {
		args = append(args, k, pairs[k])
	}
	return strings.NewReplacer(args...)
}

// failureExcerpt builds the excerpt of a failing result, already scrubbed of reviewed
// credentials; it is redacted again and stripped of local paths.
func failureExcerpt(r process.Result, failed []TestID, paths *strings.Replacer, directGoTest bool) *OutputExcerpt {
	clean := func(text string) string { return harness.RedactSecrets(paths.Replace(text)) }
	e := &OutputExcerpt{}
	e.Stdout, e.StdoutCut = excerpt(clean(r.Stdout))
	e.Stderr, e.StderrCut = excerpt(clean(r.Stderr))
	if directGoTest {
		names, cut := failedTestNames(r.Stdout, failed)
		for i := range names {
			names[i] = clean(names[i])
		}
		if len(names) > 0 {
			e.FailedTests, e.FailedTestsCut = names, cut
		}
	}
	if e.Stdout == "" && e.Stderr == "" && r.Error != "" {
		e.Stderr, e.StderrCut = excerpt(clean(r.Error))
	}
	return e
}

// IsNotRun reports a recorded check that never ran.
func (o Observation) IsNotRun() bool { return o.NotRun != "" }

// candidateCancelled reports whether ctx was cancelled because the candidate view
// beside it failed.
func candidateCancelled(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errCandidateFailed)
}
