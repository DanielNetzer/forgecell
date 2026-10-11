package verification

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// V1 protects conventional test suites plus every explicitly cited checker.
// This is a preserved regression view, not independent behavioral acceptance.

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
		if protected[file] || readiness.ConventionalTestPath(file) {
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
			// Repair authorizes only the candidate view. Protected definitions
			// always retain their original hashes and regression provenance.
			check.ReviewedAcceptance = ""
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
		if candidate.RequiredChecksPassed {
			for _, d := range p.ExpectedProtectedDeltas {
				candidate.Warnings = append(candidate.Warnings, fmt.Sprintf("declared protected delta was not exercised, no protected regression run was needed: %s %s (check %s)", d.Package, d.Test, d.CheckID))
			}
		}
		return candidate
	}
	plan := p
	plan.Analysis.Checks = regression
	copy := c
	copy.Tree = tree
	preserved := runViewDeltas(ctx, source, destination+"-protected", plan, copy, observe, true)
	candidate.Protected = &preserved
	if !preserved.RequiredChecksPassed {
		candidate.RequiredChecksPassed = false
		candidate.IndependentAcceptanceVerified = false
		candidate.Error = "protected regression did not pass: " + preserved.Error
		return candidate
	}
	if candidate.RequiredChecksPassed {
		candidate.AcceptedDeltas = preserved.AcceptedDeltas
		candidate.Warnings = append(candidate.Warnings, preserved.Warnings...)
	}
	return candidate
}

// TestID identifies one Go test by package import path and test name, as
// reported by go test -json. Subtests are Test/sub/case.
type TestID struct {
	Package string `json:"package"`
	Test    string `json:"test"`
}

// DeltaEvaluation is the outcome of matching a protected regression check's
// failing set against its declared deltas.
type DeltaEvaluation struct {
	Failed   []TestID
	Accepted []readiness.ProtectedDelta
	Warnings []string
}

type goTestEvent struct {
	Action     string
	Package    string
	Test       string
	ImportPath string
	Output     string
}

// abortMarkers identify a package run that stopped early. Tests after the
// abort never start and leave no event, so a declared failing test must not
// be allowed to stand in for them.
var abortMarkers = []string{"panic: ", "fatal error: ", "test timed out after", "[build failed]", "[setup failed]"}

// parseFailedTests reads go test -json output. It fails closed: every non-blank
// line must be a JSON event, a build failure or a failed package without any
// failing test is an error, and an empty stream is an error.
func parseFailedTests(stdout string) ([]TestID, error) {
	failed := map[TestID]bool{}
	failedPackages := map[string]bool{}
	started := map[TestID]bool{}
	aborted := map[string]string{}
	events := 0
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev goTestEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Action == "" {
			return nil, fmt.Errorf("protected regression output is not go test -json; no delta can be accepted")
		}
		events++
		if ev.Action == "output" {
			for _, marker := range abortMarkers {
				if strings.Contains(ev.Output, marker) {
					aborted[ev.Package] = strings.TrimSpace(marker)
				}
			}
		}
		key := TestID{Package: ev.Package, Test: ev.Test}
		switch ev.Action {
		case "run":
			if ev.Test != "" {
				started[key] = true
			}
		case "pass", "skip":
			delete(started, key)
		}
		switch ev.Action {
		case "build-fail":
			return nil, fmt.Errorf("build failure in %q; no delta can be accepted", ev.ImportPath)
		case "fail":
			if ev.Test != "" {
				failed[key] = true
				delete(started, key)
			} else {
				failedPackages[ev.Package] = true
			}
		}
	}
	if events == 0 {
		return nil, fmt.Errorf("protected regression produced no go test -json events; no delta can be accepted")
	}
	for pkg, marker := range aborted {
		if !failedPackages[pkg] {
			continue
		}
		return nil, fmt.Errorf("package %q stopped early (%q); tests after the abort never ran, so no delta can be accepted", pkg, marker)
	}
	for id := range started {
		return nil, fmt.Errorf("test %s in %q started but never finished; no delta can be accepted", id.Test, id.Package)
	}
	for pkg := range failedPackages {
		found := false
		for id := range failed {
			if id.Package == pkg {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("package %q failed without a failing test (build, setup or panic failure); no delta can be accepted", pkg)
		}
	}
	out := make([]TestID, 0, len(failed))
	for id := range failed {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Package != out[j].Package {
			return out[i].Package < out[j].Package
		}
		return out[i].Test < out[j].Test
	})
	return out, nil
}

// uncoveredFailures returns failing tests that no declared delta accounts for. A
// failing ancestor of a declared failing subtest is accounted for, because go test
// marks every parent failed; a parent that also fails for its own reason cannot be
// told apart from that.
func uncoveredFailures(declared []readiness.ProtectedDelta, failed []TestID) []TestID {
	failedSet := map[TestID]bool{}
	for _, id := range failed {
		failedSet[id] = true
	}
	declaredSet := map[TestID]bool{}
	declaredFailing := []TestID{}
	for _, d := range declared {
		id := TestID{Package: d.Package, Test: d.Test}
		declaredSet[id] = true
		if failedSet[id] {
			declaredFailing = append(declaredFailing, id)
		}
	}
	var out []TestID
	for _, id := range failed {
		if declaredSet[id] {
			continue
		}
		covered := false
		for _, d := range declaredFailing {
			if d.Package == id.Package && strings.HasPrefix(d.Test, id.Test+"/") {
				covered = true
			}
		}
		if !covered {
			out = append(out, id)
		}
	}
	return out
}

// EvaluateProtectedDeltas decides whether one protected regression check result is
// acceptable under its declared deltas. A passing check is acceptable and yields a
// warning per declared delta, which by definition did not fail. A failing check is
// acceptable only when it exited normally with a failing status, its complete
// go test -json stream parses, and every failing test is declared.
func EvaluateProtectedDeltas(declared []readiness.ProtectedDelta, r process.Result) (DeltaEvaluation, error) {
	var e DeltaEvaluation
	if r.TimedOut || r.Interrupted || r.Overflow || !r.Reconciled {
		return e, fmt.Errorf("protected regression output is incomplete (timed out, interrupted, over the capture limit or unreconciled); no delta can be accepted")
	}
	if !r.OK && r.Code <= 0 {
		return e, fmt.Errorf("protected regression did not exit with a failing status; no delta can be accepted")
	}
	var failed []TestID
	if !r.OK {
		var err error
		if failed, err = parseFailedTests(r.Stdout); err != nil {
			return e, err
		}
		if len(failed) == 0 {
			return e, fmt.Errorf("protected regression exited non-zero without any failing test; no delta can be accepted")
		}
		if uncovered := uncoveredFailures(declared, failed); len(uncovered) > 0 {
			names := make([]string, 0, len(uncovered))
			for _, id := range uncovered {
				names = append(names, id.Package+" "+id.Test)
			}
			return e, fmt.Errorf("undeclared protected regression failures: %s", strings.Join(names, "; "))
		}
	}
	failedSet := map[TestID]bool{}
	for _, id := range failed {
		failedSet[id] = true
	}
	e.Failed = failed
	for _, d := range declared {
		if failedSet[TestID{Package: d.Package, Test: d.Test}] {
			e.Accepted = append(e.Accepted, d)
		} else {
			e.Warnings = append(e.Warnings, fmt.Sprintf("declared protected delta did not fail: %s %s (check %s)", d.Package, d.Test, d.CheckID))
		}
	}
	return e, nil
}

// acceptedProtectedFailure re-checks a recorded failing protected observation
// against the approved plan. The compact ledger projection drops captured streams,
// so the recorded failing set is checked for coverage; whenever the stream is still
// present it is re-parsed and must yield the same failing set.
func acceptedProtectedFailure(p readiness.Plan, check readiness.Check, obs Observation) bool {
	if check.Category != "regression" || !check.Required || p.Validate() != nil {
		return false
	}
	declared := p.DeltasForCheck(check.ID)
	r := obs.Result
	if len(declared) == 0 || len(obs.FailedTests) == 0 || r.TimedOut || r.Interrupted || r.Overflow || !r.Reconciled || r.Code <= 0 {
		return false
	}
	if r.Stdout != "" {
		e, err := EvaluateProtectedDeltas(declared, r)
		if err != nil || len(e.Failed) != len(obs.FailedTests) {
			return false
		}
		recorded := map[TestID]bool{}
		for _, id := range obs.FailedTests {
			recorded[id] = true
		}
		for _, id := range e.Failed {
			if !recorded[id] {
				return false
			}
		}
		return true
	}
	return len(uncoveredFailures(declared, obs.FailedTests)) == 0
}

// verifyDeltaPackages binds each declared delta to the package whose test
// source its scope path edits, using the workspace's own go.mod to resolve the
// import path. A delta can therefore never excuse an unrelated (for example
// security) test elsewhere in the module.
func verifyDeltaPackages(workspace string, declared []readiness.ProtectedDelta) error {
	for _, d := range declared {
		dir := filepath.Join(workspace, filepath.FromSlash(readiness.DeltaPackageDir(d.ScopePath)))
		root, module := dir, ""
		for {
			raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
			if err == nil {
				for _, line := range strings.Split(string(raw), "\n") {
					if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
						module = strings.Trim(f[1], "\"")
					}
				}
				break
			}
			if root == workspace || len(root) <= len(workspace) {
				break
			}
			root = filepath.Dir(root)
		}
		if module == "" {
			return fmt.Errorf("expected protected delta %s %s: no go.mod encloses %q; no delta can be accepted", d.Package, d.Test, d.ScopePath)
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		want := module
		if rel != "." {
			want = module + "/" + filepath.ToSlash(rel)
		}
		if d.Package != want {
			return fmt.Errorf("expected protected delta %s %s is not in the package of its scope path %q (%s); no delta can be accepted", d.Package, d.Test, d.ScopePath, want)
		}
	}
	return nil
}
