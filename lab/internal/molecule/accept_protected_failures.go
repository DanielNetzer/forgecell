package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// Bounds of reading the retained verification evidence and the module manifests
// that map a failing package to its in-scope test source.
const (
	maxVerificationEvidenceBytes = 64_000_000
	maxModuleManifestBytes       = 64_000
	maxManifestLookups           = 4096
)

// protectedFailureDeltas turns the latest protected-regression failure of the
// retained tree into the expected protected deltas of a successor of latest. It
// refuses, never guesses: the latest verification must be of the latest plan and
// the retained tree, every required candidate-view command must have passed, the
// protected view must have stopped on one required go test -json regression check
// whose complete stream the delta parser accepts, every failing test's package must
// have an in-scope _test.go file, and the total stays within MaxProtectedDeltas.
// Deltas of other checks are kept; the failing check's deltas become exactly the
// tests that failed.
func protectedFailureDeltas(ctx context.Context, r Record, latest readiness.Proposal) ([]readiness.ProtectedDelta, error) {
	const refuse = "--accept-protected-failures refused: "
	if len(r.VerificationAttempts) == 0 || r.Capture == nil || !reviewRevision.MatchString(r.Capture.Tree) {
		return nil, fmt.Errorf(refuse + "no verification of the retained tree is recorded")
	}
	tree := r.Capture.Tree
	a := r.VerificationAttempts[len(r.VerificationAttempts)-1]
	if a.Status != "failed" || a.Result == nil {
		return nil, fmt.Errorf(refuse+"the latest verification of tree %s did not fail", tree)
	}
	if a.SourceTree != tree {
		return nil, fmt.Errorf(refuse + "the latest verification is not of the retained tree")
	}
	if a.PlanDigest != latest.Digest {
		return nil, fmt.Errorf(refuse + "the latest verification ran under an earlier plan revision; approve and run checks on the latest plan first")
	}
	result, err := readVerificationResult(r, *a.Result)
	if err != nil {
		return nil, fmt.Errorf(refuse+"%v", err)
	}
	if result.SchemaVersion != "v1" || result.SourceTree != tree {
		return nil, fmt.Errorf(refuse + "the latest verification result does not match the retained tree")
	}
	p := latest.Plan
	if err = candidateViewPassed(p, result, tree); err != nil {
		return nil, fmt.Errorf(refuse+"%v", err)
	}
	check, obs, err := failingProtectedCheck(p, result)
	if err != nil {
		return nil, fmt.Errorf(refuse+"%v", err)
	}
	failed, err := verification.ProtectedFailuresForDeltas(obs)
	if err != nil {
		return nil, fmt.Errorf(refuse+"protected regression check %s on tree %s is not a clean go test failure: %v", check.ID, tree, err)
	}
	var kept []readiness.ProtectedDelta
	for _, d := range p.ExpectedProtectedDeltas {
		if d.CheckID != check.ID {
			kept = append(kept, d)
		}
	}
	if len(kept)+len(failed) > readiness.MaxProtectedDeltas {
		return nil, fmt.Errorf(refuse+"check %s failed %d tests in the protected regression run; at most %d expected protected deltas are permitted", check.ID, len(failed), readiness.MaxProtectedDeltas-len(kept))
	}
	for _, id := range failed {
		// A secret-shaped name is redacted in every persisted copy, so it could not be
		// shown as reviewed or matched exactly; it never becomes a plan declaration.
		if harness.RedactSecrets(id.Package) != id.Package || harness.RedactSecrets(id.Test) != id.Test {
			return nil, fmt.Errorf(refuse+"a failing test of check %s has a secret-shaped name; review the protected run instead", check.ID)
		}
	}
	files, err := inScopeTestFiles(ctx, r.Workspace.Path, tree, p.Analysis.Scope)
	if err != nil {
		return nil, fmt.Errorf(refuse+"%v", err)
	}
	reason := fmt.Sprintf("Failed by design in the protected regression run of check %s on retained tree %s; accepted with amend --accept-protected-failures.", check.ID, tree)
	deltas := kept
	for _, id := range failed {
		scoped, ok := files[id.Package]
		if !ok {
			return nil, fmt.Errorf(refuse+"failing test %s %s of check %s has no in-scope _test.go file in its package to justify it", id.Package, id.Test, check.ID)
		}
		evidence := append([]string(nil), scoped.Evidence...)
		if len(evidence) == 0 {
			evidence = []string{"issue"}
		}
		deltas = append(deltas, readiness.ProtectedDelta{CheckID: check.ID, Package: id.Package, Test: id.Test, ScopePath: scoped.Path, Reason: reason, Evidence: evidence})
	}
	return deltas, nil
}

// readVerificationResult reads one retained verification result file under the
// Molecule's own verification-history directory: a regular file, not a symbolic
// link, bounded in size and matching its recorded hash.
func readVerificationResult(r Record, ref VerificationFile) (verification.Result, error) {
	var out verification.Result
	prefix := "verification-history/" + r.ID + "/"
	if !safeLoopID(r.ID) || !strings.HasPrefix(ref.Path, prefix) || path.Clean(ref.Path) != ref.Path || strings.Contains(ref.Path, "..") || strings.Contains(strings.TrimPrefix(ref.Path, prefix), "/") {
		return out, fmt.Errorf("invalid verification evidence path")
	}
	file := filepath.Join(r.LabDir, filepath.FromSlash(ref.Path))
	// O_NONBLOCK keeps a FIFO planted at the path from blocking the open.
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return out, fmt.Errorf("verification evidence %s cannot be opened", ref.Path)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxVerificationEvidenceBytes {
		return out, fmt.Errorf("invalid verification evidence file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxVerificationEvidenceBytes+1))
	if err != nil || len(raw) > maxVerificationEvidenceBytes {
		return out, fmt.Errorf("invalid verification evidence file")
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != ref.SHA256 {
		return out, fmt.Errorf("verification history changed: %s", ref.Path)
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("verification evidence %s is not a verification result", ref.Path)
	}
	return out, nil
}

// candidateViewPassed requires the candidate view of result to have run every setup
// command and check of p on tree, with every setup and required check passing.
func candidateViewPassed(p readiness.Plan, result verification.Result, tree string) error {
	// The candidate view can fail after every check passed, such as a post-check
	// audit; its own reason survives the protected view's error.
	if result.CandidateError != "" {
		return fmt.Errorf("the candidate view failed on tree %s, so the failure is not only in the protected view", tree)
	}
	if len(result.Setup) != len(p.Setup) || len(result.Checks) != len(p.Analysis.Checks) {
		return fmt.Errorf("the candidate checks did not all run on tree %s, so the failure is not only in the protected view", tree)
	}
	for i, obs := range result.Setup {
		if obs.ID != p.Setup[i].ID || obs.SourceTree != tree || !obs.Result.OK {
			return fmt.Errorf("setup %s did not pass on tree %s; only a failure in the protected view can be accepted", p.Setup[i].ID, tree)
		}
	}
	for i, obs := range result.Checks {
		c := p.Analysis.Checks[i]
		if obs.ID != c.ID || obs.SourceTree != tree || (c.Required && !obs.Result.OK) {
			return fmt.Errorf("candidate check %s did not pass on tree %s; only a failure in the protected view can be accepted", c.ID, tree)
		}
	}
	return nil
}

// failingProtectedCheck returns the required regression check the protected view of
// result stopped on and its observation. The protected view must have failed for
// that check's own exit, not for setup, a definition change or a source mutation.
func failingProtectedCheck(p readiness.Plan, result verification.Result) (readiness.Check, verification.Observation, error) {
	v := result.Protected
	if v == nil || v.RequiredChecksPassed {
		return readiness.Check{}, verification.Observation{}, fmt.Errorf("the latest verification has no protected regression failure")
	}
	// The protected view runs the derived tree with in-scope tests restored to base;
	// one that ran the candidate tree itself is not a protected regression run.
	if v.SourceTree == result.SourceTree || !reviewRevision.MatchString(v.SourceTree) {
		return readiness.Check{}, verification.Observation{}, fmt.Errorf("the protected regression run did not run a derived protected tree")
	}
	var regression []readiness.Check
	for _, c := range p.Analysis.Checks {
		if c.Category == "regression" {
			c.ReviewedAcceptance = ""
			regression = append(regression, c)
		}
	}
	if len(v.Setup) != len(p.Setup) || len(v.Checks) == 0 || len(v.Checks) > len(regression) {
		return readiness.Check{}, verification.Observation{}, fmt.Errorf("the protected regression run did not fail on a regression check")
	}
	for _, obs := range v.Setup {
		if !obs.Result.OK {
			return readiness.Check{}, verification.Observation{}, fmt.Errorf("protected regression setup %s did not pass", obs.ID)
		}
	}
	last := len(v.Checks) - 1
	for i, obs := range v.Checks {
		c := regression[i]
		if obs.ID != c.ID || obs.SourceTree != v.SourceTree || obs.Category != "regression" || !reflect.DeepEqual(obs.Command, c) {
			return readiness.Check{}, verification.Observation{}, fmt.Errorf("protected regression observation %d does not match the approved check", i+1)
		}
		if i < last && c.Required && !obs.Result.OK {
			return readiness.Check{}, verification.Observation{}, fmt.Errorf("protected regression check %s failed before the last one recorded", c.ID)
		}
	}
	c, obs := regression[last], v.Checks[last]
	stopped := "required command " + c.ID + " did not pass"
	if !c.Required || obs.Result.OK || (v.Error != stopped && !strings.HasPrefix(v.Error, stopped+": ")) {
		return readiness.Check{}, verification.Observation{}, fmt.Errorf("the protected regression run did not stop on a failing required check")
	}
	if !readiness.RequestsGoTestJSON(c.Argv) || len(c.Argv) < 2 || c.Argv[0] != "go" || c.Argv[1] != "test" {
		return readiness.Check{}, verification.Observation{}, fmt.Errorf("protected regression check %s is not a direct go test -json command, so its failing tests cannot be declared", c.ID)
	}
	return c, obs, nil
}

// inScopeTestFiles maps each Go package import path to the lexicographically first
// in-scope _test.go file of that package directory. The import path is resolved
// from the nearest go.mod in the retained tree, the rule verification applies to a
// declared delta's scope path.
func inScopeTestFiles(ctx context.Context, workspace, tree string, scope []readiness.ScopedPath) (map[string]readiness.ScopedPath, error) {
	tests := []readiness.ScopedPath{}
	for _, s := range scope {
		if strings.HasSuffix(s.Path, "_test.go") && readiness.ConventionalTestPath(s.Path) && readiness.ValidatePath(s.Path) == nil {
			tests = append(tests, s)
		}
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].Path < tests[j].Path })
	type manifest struct {
		module string
		found  bool
	}
	modules := map[string]manifest{}
	lookups := 0
	moduleAt := func(dir string) (manifest, error) {
		if m, ok := modules[dir]; ok {
			return m, nil
		}
		if lookups++; lookups > maxManifestLookups {
			return manifest{}, fmt.Errorf("too many module manifest lookups for the in-scope test files")
		}
		module, found, err := treeModule(ctx, workspace, tree, dir)
		if err != nil {
			return manifest{}, err
		}
		modules[dir] = manifest{module, found}
		return modules[dir], nil
	}
	out := map[string]readiness.ScopedPath{}
	for _, s := range tests {
		dir := readiness.DeltaPackageDir(s.Path)
		root := dir
		for {
			m, err := moduleAt(root)
			if err != nil {
				return nil, err
			}
			// The nearest go.mod decides, as in verification; one without a module
			// line leaves the file without a package.
			if m.found {
				if m.module == "" {
					break
				}
				pkg := m.module
				if dir != root {
					rel := dir
					if root != "." {
						rel = strings.TrimPrefix(dir, root+"/")
					}
					pkg = m.module + "/" + rel
				}
				if _, seen := out[pkg]; !seen {
					out[pkg] = s
				}
				break
			}
			if root == "." {
				break
			}
			root = path.Dir(root)
		}
	}
	return out, nil
}

// treeModule returns the module path declared by dir/go.mod in tree and whether
// the tree has a go.mod there. Only a regular, bounded blob is read.
func treeModule(ctx context.Context, workspace, tree, dir string) (string, bool, error) {
	manifest := "go.mod"
	if dir != "." {
		manifest = dir + "/go.mod"
	}
	entry, err := git(ctx, workspace, "ls-tree", "-z", tree, "--", manifest)
	if err != nil {
		return "", false, fmt.Errorf("the retained tree cannot be read")
	}
	entry = strings.TrimSuffix(entry, "\x00")
	if entry == "" {
		return "", false, nil
	}
	header, name, ok := strings.Cut(entry, "\t")
	parts := strings.Fields(header)
	if !ok || name != manifest || len(parts) != 3 || parts[1] != "blob" || (parts[0] != "100644" && parts[0] != "100755") || !reviewRevision.MatchString(parts[2]) {
		return "", false, fmt.Errorf("module manifest %s is not a regular file in the retained tree", manifest)
	}
	size, err := git(ctx, workspace, "cat-file", "-s", parts[2])
	if n, convErr := strconv.Atoi(size); err != nil || convErr != nil || n > maxModuleManifestBytes {
		return "", false, fmt.Errorf("module manifest %s cannot be read within %d bytes", manifest, maxModuleManifestBytes)
	}
	raw, err := git(ctx, workspace, "cat-file", "blob", parts[2])
	if err != nil {
		return "", false, fmt.Errorf("module manifest %s cannot be read", manifest)
	}
	module := ""
	for _, line := range strings.Split(raw, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			module = strings.Trim(f[1], "\"")
		}
	}
	return module, true, nil
}
