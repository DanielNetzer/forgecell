package verification

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// selectionPlan approves edits to scope and one selecting go test regression check
// in the fixture module, defined by the frozen mod/go.mod.
func selectionPlan(t *testing.T, repo, base string, scope []string) readiness.Plan {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, "mod", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	p := readiness.Plan{SchemaVersion: "v1", MoleculeID: "mol-select", Revision: 1,
		Inputs:   readiness.Inputs{Repository: "owner/repo", TargetBranch: "main", BaseCommit: base, FormulaSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64), BindingSHA256: strings.Repeat("c", 64), Issue: readiness.Issue{Repository: "owner/repo", Number: 1, URL: "https://github.com/owner/repo/issues/1", State: "OPEN", Title: "Update c"}},
		Evidence: []readiness.Evidence{{ID: "manifest", Path: "mod/go.mod", SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}},
		Analysis: readiness.Analysis{Summary: "Update c", Acceptance: []readiness.Criterion{{Description: "c still passes", Evidence: []string{"issue"}}},
			Checks: []readiness.Check{{ID: "go-test", Category: "regression", Dir: "mod", Argv: []string{"go", "test", "./..."}, TimeoutMS: 600000, Required: true, Reason: "Module tests", Evidence: []string{"manifest"}, Definitions: []string{"manifest"}, Select: readiness.SelectAffectedPackages}}},
		Policy: readiness.ExecutionPolicy{Environment: []string{"PATH"}, SetupNetwork: "unrestricted", CheckNetwork: "unrestricted", Containment: "filtered-environment"}}
	for _, file := range scope {
		p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: file, Reason: "Implementation", Evidence: []string{"issue"}})
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

// selectionCapture edits c and its test, so the protected view restores the test,
// and captures the change.
func selectionCapture(t *testing.T) (string, readiness.Plan, CaptureResult) {
	t.Helper()
	repo, base := affectedFixture(t)
	scope := []string{"mod/c/c.go", "mod/c/c_test.go"}
	p := selectionPlan(t, repo, base, scope)
	writeFiles(t, repo, map[string]string{
		"mod/c/c.go":      affectedModule["mod/c/c.go"] + "\nfunc C2() int { return 2 }\n",
		"mod/c/c_test.go": affectedModule["mod/c/c_test.go"] + "\nfunc TestC2(t *testing.T) {\n\tif C2() != 2 {\n\t\tt.Fatal()\n\t}\n}\n",
	})
	capture, err := Capture(context.Background(), repo, base, scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	return repo, p, capture
}

func TestSelectionProtectedViewRunsTheCandidateSelection(t *testing.T) {
	repo, p, capture := selectionCapture(t)
	original := runGoList
	defer func() { runGoList = original }()
	calls := 0
	runGoList = func(ctx context.Context, o process.Options) process.Result {
		calls++
		return original(ctx, o)
	}
	result := Run(context.Background(), repo, filepath.Join(t.TempDir(), "verification"), p, capture)
	if !result.RequiredChecksPassed || result.Protected == nil {
		t.Fatalf("error %q, protected %v", result.Error, result.Protected != nil)
	}
	want := &Selection{Mode: readiness.SelectAffectedPackages, Selected: 1, Total: 6, Packages: []string{"example.test/m/c"}}
	candidate, protected := result.Checks[0], result.Protected.Checks[0]
	if !reflect.DeepEqual(candidate.Selection, want) || !reflect.DeepEqual(protected.Selection, want) {
		t.Fatalf("candidate %+v, protected %+v", candidate.Selection, protected.Selection)
	}
	if calls != 1 {
		t.Fatalf("go list ran %d times; the protected view must reuse the candidate selection", calls)
	}
	for _, o := range []Observation{candidate, protected} {
		if !reflect.DeepEqual(o.Command.Argv, []string{"go", "test", "./..."}) {
			t.Fatalf("the record changed the approved argv: %v", o.Command.Argv)
		}
		if !strings.Contains(o.Result.Stdout, "example.test/m/c") || strings.Contains(o.Result.Stdout, "example.test/m/d") {
			t.Fatalf("%s ran other packages: %s", o.SourceTree, o.Result.Stdout)
		}
		if o.Selection.Line() != "selection: affected-packages (1 of 6 packages)" {
			t.Fatal(o.Selection.Line())
		}
		if !MatchesCheckObservation(p, p.Analysis.Checks[0], o, o.SourceTree, o.SourceTree != capture.Tree) {
			t.Fatalf("a selected observation no longer matches its approved check")
		}
	}
}

func TestSelectionProtectedViewWithoutCandidateFallsBackToFullSuite(t *testing.T) {
	repo, p, capture := selectionCapture(t)
	original := runGoList
	defer func() { runGoList = original }()
	runGoList = func(context.Context, process.Options) process.Result {
		t.Error("the protected view ran go list")
		return process.Result{}
	}
	tree, err := ProtectedTree(context.Background(), repo, p, capture.Tree)
	if err != nil || tree == capture.Tree {
		t.Fatalf("protected tree %s: %v", tree, err)
	}
	protected := capture
	protected.Tree = tree
	v := runViewDeltas(context.Background(), repo, filepath.Join(t.TempDir(), "protected"), p, protected, nil, true)
	if !v.RequiredChecksPassed || len(v.Checks) != 1 {
		t.Fatalf("%q", v.Error)
	}
	s := v.Checks[0].Selection
	if s == nil || s.Mode != SelectionFullSuite || s.Line() != "selection: full suite (reason: the candidate view recorded no selection for this check directory)" {
		t.Fatalf("%+v", s)
	}
	if !strings.Contains(v.Checks[0].Result.Stdout, "example.test/m/c") {
		t.Fatalf("the full suite did not run: %s", v.Checks[0].Result.Stdout)
	}
}
