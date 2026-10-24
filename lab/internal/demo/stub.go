package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// StubArg is the hidden forgecell subcommand the demo Formula binds as its coding
// command. It speaks the existing custom-command protocol: the coding request as
// JSON on stdin, a v1 CodingOutcome as JSON on stdout. No adapter invocation
// changes, so the adapter protocol version is unchanged.
const StubArg = "__demo-harness"

// maxStubRequestBytes bounds the coding request the stub reads. A larger request
// is refused rather than truncated.
const maxStubRequestBytes = 4 << 20

// The canned change: Greeting(name) and its table test.
const (
	stubGreeting = `// Package greeting is the built-in sample module of forgecell demo.
package greeting

import "strings"

// Hello returns the greeting for the whole world.
func Hello() string {
	return "Hello, world!"
}

// Greeting returns "Hello, NAME!" for name, or Hello() when name is blank.
func Greeting(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return Hello()
	}
	return "Hello, " + name + "!"
}
`
	stubGreetingTest = `package greeting

import "testing"

func TestGreeting(t *testing.T) {
	for name, want := range map[string]string{
		"Ada":     "Hello, Ada!",
		"  Ada  ": "Hello, Ada!",
		"":        "Hello, world!",
		"   ":     "Hello, world!",
	} {
		if got := Greeting(name); got != want {
			t.Errorf("Greeting(%q) = %q, want %q", name, got, want)
		}
	}
}
`
	stubOutcome = `{"schemaVersion":"v1","outcome":"completed","reason":"Stub harness wrote the canned Greeting(name) change and its table test; no model was called. The Lab runs the approved checks independently.","paths":[]}`
	stubReview  = `{"findings":[{"severity":"low","file":"greeting.go","line":0,"claim":"Greeting trims surrounding whitespace, which the issue does not ask for.","scenario":"Greeting(\"  Ada  \") returns \"Hello, Ada!\" rather than keeping the spaces.","status":"confirmed","fix":"Keep the trim and say so in the doc comment, or drop it."}],"verifiedSound":["The change stays inside the approved scope: greeting.go and greeting_test.go.","Hello is unchanged, and the blank-name case falls back to it."],"reviewedFiles":["greeting.go","greeting_test.go"]}`
)

// scopePaths is the exact approved scope the canned change writes.
var scopePaths = []string{"greeting.go", "greeting_test.go"}

// stubRequest is the part of the coding request the stub checks. Every other
// field is ignored.
type stubRequest struct {
	Kind  string `json:"kind"`
	Issue struct {
		Repo   string `json:"repo"`
		Number int64  `json:"number"`
	} `json:"issue"`
	ApprovedPlan struct {
		Inputs struct {
			Repository string `json:"repository"`
		} `json:"inputs"`
		Analysis struct {
			Scope []struct {
				Path string `json:"path"`
			} `json:"scope"`
		} `json:"analysis"`
	} `json:"approvedPlan"`
}

// ServeStub is the deterministic coding harness of the demo. It answers only the
// demo's own coding request, in a pristine checkout of the sample, writes the
// canned change and prints a v1 CodingOutcome. Anything else is refused with a
// nonzero exit and no file written; the Lab then records an invalid outcome and
// stops the Molecule.
func ServeStub(args []string, in io.Reader, out, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "forgecell demo stub:", err)
		return 2
	}
	if len(args) != 0 {
		return fail(fmt.Errorf("the demo review runs in process; this command only serves the coding request"))
	}
	raw, err := io.ReadAll(io.LimitReader(in, maxStubRequestBytes+1))
	if err != nil {
		return fail(err)
	}
	if len(raw) > maxStubRequestBytes {
		return fail(fmt.Errorf("the coding request exceeds %d bytes", maxStubRequestBytes))
	}
	var request stubRequest
	if err = json.Unmarshal(raw, &request); err != nil {
		return fail(fmt.Errorf("the coding request is not JSON: %w", err))
	}
	if err = checkStubRequest(request); err != nil {
		return fail(err)
	}
	dir, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	if err = applyCannedChange(dir); err != nil {
		return fail(err)
	}
	if _, err = harness.DecodeCodingOutcome([]byte(stubOutcome)); err != nil {
		return fail(fmt.Errorf("the canned outcome is invalid: %w", err))
	}
	if _, err = io.WriteString(out, stubOutcome); err != nil {
		return fail(err)
	}
	return 0
}

func checkStubRequest(r stubRequest) error {
	if r.Kind != "molecule" {
		return fmt.Errorf("the stub serves only the coding request, not %q", r.Kind)
	}
	if r.Issue.Repo != Repository || r.Issue.Number != IssueNumber || r.ApprovedPlan.Inputs.Repository != Repository {
		return fmt.Errorf("the stub serves only the demo issue #%d of %s", IssueNumber, Repository)
	}
	scope := []string{}
	for _, s := range r.ApprovedPlan.Analysis.Scope {
		scope = append(scope, s.Path)
	}
	slices.Sort(scope)
	if !slices.Equal(scope, scopePaths) {
		return fmt.Errorf("the approved scope is not the demo scope %v", scopePaths)
	}
	return nil
}

// readBoundedRegular reads dir/name only when it is a regular file of at most
// limit bytes, opened without following a symbolic link and still the file that
// was inspected.
func readBoundedRegular(dir, name string, limit int64) ([]byte, os.FileInfo, error) {
	file := filepath.Join(dir, name)
	before, err := os.Lstat(file)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return nil, nil, fmt.Errorf("%s is not a bounded regular file", name)
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s changed while it was read", name)
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(raw)) > limit {
		return nil, nil, fmt.Errorf("%s is not a bounded regular file", name)
	}
	return raw, opened, nil
}

// applyCannedChange writes the canned change into dir, which must be a pristine
// checkout of the sample: go.mod and greeting.go byte-identical to the embedded
// sample and no greeting_test.go yet. Nothing is followed through a symbolic link.
func applyCannedChange(dir string) error {
	for _, f := range sampleFiles() {
		got, _, err := readBoundedRegular(dir, f.name, maxSampleFileBytes)
		if err != nil {
			return fmt.Errorf("not a pristine demo checkout: %w", err)
		}
		if !bytes.Equal(got, f.data) {
			return fmt.Errorf("not a pristine demo checkout: %s differs from the sample", f.name)
		}
	}
	// Refuse before writing anything, so a refusal leaves the checkout unchanged;
	// the exclusive create below still refuses a test file that appears meanwhile.
	if _, err := os.Lstat(filepath.Join(dir, "greeting_test.go")); !os.IsNotExist(err) {
		return fmt.Errorf("not a pristine demo checkout: greeting_test.go already exists or cannot be inspected")
	}
	_, info, err := readBoundedRegular(dir, "greeting.go", maxSampleFileBytes)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "greeting.go"), os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	if opened, e := f.Stat(); e != nil || !os.SameFile(info, opened) {
		f.Close()
		return fmt.Errorf("greeting.go changed before it was written")
	}
	if _, err = io.WriteString(f, stubGreeting); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return writeNewFile(dir, "greeting_test.go", []byte(stubGreetingTest), 0o644)
}

// cannedAnalysis is the demo's Options.Analyze: the ticket analysis a model would
// return, citing the frozen evidence IDs the real collector assigned. It fails
// when the evidence is not the sample's.
func cannedAnalysis(_ context.Context, _ formula.Binding, issue molecule.Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
	if issue.Repo != Repository || issue.Number != IssueNumber {
		return readiness.Analysis{}, fmt.Errorf("the canned analysis covers only the demo issue")
	}
	manifest, source := "", ""
	for _, e := range s.Evidence {
		switch e.Path {
		case "go.mod":
			manifest = e.ID
		case "greeting.go":
			source = e.ID
		}
	}
	if manifest == "" || source == "" {
		return readiness.Analysis{}, fmt.Errorf("the frozen evidence lacks the sample go.mod or greeting.go")
	}
	return readiness.Analysis{
		Summary: "Add Greeting(name) to the sample greeting package and cover it with a table test. Canned demo analysis; no model was called.",
		Scope: []readiness.ScopedPath{
			{Path: "greeting.go", Reason: "Add Greeting next to Hello, which it falls back to.", Evidence: []string{"issue", source}},
			{Path: "greeting_test.go", Reason: "New table test for Greeting in the root package of the sample module.", Evidence: []string{"issue", manifest}},
		},
		Acceptance: []readiness.Criterion{
			{Description: `Greeting("Ada") returns "Hello, Ada!".`, Evidence: []string{"issue"}},
			{Description: "Greeting with a blank name returns Hello().", Evidence: []string{"issue", source}},
		},
		Checks: []readiness.Check{
			{ID: "greeting-test", Category: "candidate", Argv: []string{"go", "test", "-run", "^TestGreeting$", "./..."}, Dir: ".", TimeoutMS: 300000, Required: true, Definitions: []string{manifest}, Reason: "Runs the new Greeting table test.", Evidence: []string{"issue", manifest}},
			{ID: "sample-regression", Category: "regression", Argv: []string{"go", "test", "./..."}, Dir: ".", TimeoutMS: 300000, Required: true, Definitions: []string{manifest}, Reason: "Builds and tests the sample module; the protected view reruns it with the tests restored to the base commit.", Evidence: []string{manifest}},
		},
		Impacts:   []readiness.Assessment{{Description: "Adds one exported function; Hello keeps its behavior.", Evidence: []string{"issue", source}}},
		Unknowns:  []string{},
		Questions: []readiness.Question{},
	}, nil
}

// cannedReview is the demo's Options.InvokeReview: the stub reviewer's canned
// verdict, decoded by the real review validator. It never starts a process.
func cannedReview(_ context.Context, _ formula.Binding, _ string, _ time.Duration, request map[string]any, _ []string) process.Result {
	if request["kind"] != harness.ReviewKind {
		return process.Result{Code: -1, Reconciled: true, Error: "the stub reviewer serves only the review request"}
	}
	return process.Result{OK: true, Code: 0, Reconciled: true, Stdout: stubReview}
}
