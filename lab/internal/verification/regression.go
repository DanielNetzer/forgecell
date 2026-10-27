package verification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	// An exempt file keeps its candidate version: it is neither restored nor removed.
	exempt := map[string]bool{}
	for _, file := range p.ProtectedExemptPaths {
		exempt[file] = true
	}
	files := []string{}
	for _, file := range paths(changed) {
		if exempt[file] {
			continue
		}
		if protected[file] || readiness.ConventionalTestPath(file) {
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		return candidate, nil
	}
	temp, err := resources.MkdirTemp("", "forgecell-regression-")
	if err != nil {
		return "", err
	}
	defer func() { _ = resources.Remove(temp) }()
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
	// The candidate tree decides once whether the change leaves every Go module root, so
	// both views run fresh for it; on a git error each view decides, and fails, itself.
	// Only validated object ids reach git here; runViewDeltas refuses anything else.
	objectID := regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
	if objectID.MatchString(c.Tree) && objectID.MatchString(p.Inputs.BaseCommit) {
		if nonModule, err := treeChangesOutsideModules(ctx, source, p.Inputs.BaseCommit, c.Tree); err == nil {
			ctx = context.WithValue(ctx, nonModuleKey{}, nonModule)
		}
	}
	// The candidate view computes each selecting check's package set once from the
	// captured change against BaseCommit; the protected view runs that same set.
	ctx = withSelectionMemo(ctx)
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
		keepCandidateError(&candidate)
		candidate.Error = err.Error()
		candidate.RequiredChecksPassed = false
		candidate.IndependentAcceptanceVerified = false
		return candidate
	}
	if tree == c.Tree {
		if candidate.RequiredChecksPassed {
			for _, d := range p.ExpectedProtectedDeltas {
				candidate.Warnings = append(candidate.Warnings, fmt.Sprintf("declared protected delta was not exercised, no protected regression run was needed: %s %s (check %s); amend the plan to remove it", d.Package, d.Test, d.CheckID))
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
		keepCandidateError(&candidate)
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

// keepCandidateError records the candidate view's own failure on CandidateError
// before Error is replaced, so a candidate failure is never read as a pass.
func keepCandidateError(candidate *Result) {
	if candidate.RequiredChecksPassed || candidate.CandidateError != "" {
		return
	}
	candidate.CandidateError = candidate.Error
	if candidate.CandidateError == "" {
		candidate.CandidateError = "candidate view did not pass"
	}
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

// runtimeAbortPrefixes are what the Go runtime prints at the start of a line when
// it stops a test binary. Output attributed to a test is only an abort when a line
// starts with one; t.Log output is indented and a test may log any text.
var runtimeAbortPrefixes = []string{"panic: ", "fatal error: ", "test timed out after"}

// abortMarker returns the marker in an output event that shows its package stopped
// early. Package-level output (no test) keeps the broad match; test-attributed
// output must begin a line with a runtime prefix.
func abortMarker(test, output string) (string, bool) {
	if test == "" {
		for _, marker := range abortMarkers {
			if strings.Contains(output, marker) {
				return strings.TrimSpace(marker), true
			}
		}
		return "", false
	}
	for _, line := range strings.Split(output, "\n") {
		for _, prefix := range runtimeAbortPrefixes {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(prefix), true
			}
		}
	}
	return "", false
}

// abnormalExitMarker recognizes the whole line go test prints when its test binary
// is killed by a signal ("signal: killed") or exits with a status other than 1
// ("exit status 2" after os.Exit(2)). Every ordinary failing package also prints
// "exit status 1", so that status is deliberately not an abort marker; a test
// that calls os.Exit(1) is caught as started but never finished instead.
func abnormalExitMarker(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "signal: ") {
			return line, true
		}
		if rest, ok := strings.CutPrefix(line, "exit status "); ok {
			if n, err := strconv.Atoi(rest); err == nil && n != 1 {
				return line, true
			}
		}
	}
	return "", false
}

// maxStreamedFailures bounds the tests a streamed parse tracks as failed or
// unfinished, so the parser's memory never follows an untrusted stream's size.
const maxStreamedFailures = 200_000

// maxStreamedPackages bounds the distinct packages a streamed parse remembers having
// run, for the same reason.
const maxStreamedPackages = 100_000

// maxStreamSummaryBytes bounds the plain summary an engine-streamed check keeps as its
// stdout in place of the -json stream.
const maxStreamSummaryBytes = 64_000

// Bounds of the output tail an engine-streamed check keeps for each failing test, so a
// reviewer can see why it failed: the last maxTailLines lines within maxTailBytes per
// test, for at most maxTailedTests tests tracked at once, and maxTailSummaryBytes of
// tails in the summary. Past a bound the tail says what it dropped.
const (
	maxTailLines        = 40
	maxTailBytes        = 8 << 10
	maxTailedTests      = 256
	maxTailSummaryBytes = 128_000
)

// Bounds of the diagnostics an engine-streamed check keeps when its stream is refused
// (a build failure, a line that is not an event, an abort): the build output and the
// package-level output each within maxDiagnosticBytes, and the first maxRawHeadLines
// lines within maxRawHeadBytes plus the last maxTailLines lines within maxTailBytes of
// the raw stdout. Past a bound the diagnostics say what they dropped.
const (
	maxDiagnosticBytes = 16 << 10
	maxRawHeadLines    = 40
	maxRawHeadBytes    = 4 << 10
	maxRawLineBytes    = 512
	// maxStreamDiagnosticsBytes bounds everything Diagnostics renders, headers included.
	maxStreamDiagnosticsBytes = 2*maxDiagnosticBytes + maxRawHeadBytes + maxTailBytes + 4<<10
)

// boundedText keeps text up to max bytes and counts the lines it dropped past it.
type boundedText struct {
	b       strings.Builder
	max     int
	dropped int
}

func (t *boundedText) add(line string) {
	if t.dropped > 0 || t.b.Len()+len(line)+1 > t.max {
		t.dropped++
		return
	}
	t.b.WriteString(line)
	t.b.WriteByte('\n')
}

// testTail is the bounded last output of one test.
type testTail struct {
	lines   []string
	bytes   int
	dropped int
}

func (t *testTail) add(line string) {
	if len(line) > maxTailBytes {
		line = line[len(line)-maxTailBytes:]
	}
	t.lines = append(t.lines, line)
	t.bytes += len(line)
	for len(t.lines) > maxTailLines || t.bytes > maxTailBytes {
		t.bytes -= len(t.lines[0])
		t.lines = t.lines[1:]
		t.dropped++
	}
}

// failedTestParser reads go test -json output one line at a time. Its state holds
// only the failing and unfinished tests, the packages that ran, a bounded copy of
// their ok/FAIL summary lines and a bounded output tail of each running or failing
// test, never the stream. It fails closed: a line that is not a JSON event or a build
// failure is remembered and reported by Finish, a failed package without any failing
// test is an error, and an empty stream is an error.
type failedTestParser struct {
	failed         map[TestID]bool
	failedPackages map[string]bool
	started        map[TestID]bool
	aborted        map[string]string
	packages       map[string]bool
	tails          map[TestID]*testTail
	tailsCut       bool
	summary        strings.Builder
	summaryCut     bool
	events         int
	err            error
	// Diagnostics kept for a refused stream: build output, package-level output that
	// is not a summary line, and a bounded head and tail of the raw lines.
	buildOutput   boundedText
	packageOutput boundedText
	rawHead       boundedText
	rawHeadLines  int
	rawTail       testTail
}

func newFailedTestParser() *failedTestParser {
	return &failedTestParser{failed: map[TestID]bool{}, failedPackages: map[string]bool{}, started: map[TestID]bool{}, aborted: map[string]string{}, packages: map[string]bool{}, tails: map[TestID]*testTail{},
		buildOutput: boundedText{max: maxDiagnosticBytes}, packageOutput: boundedText{max: maxDiagnosticBytes}, rawHead: boundedText{max: maxRawHeadBytes}}
}

// keepRaw adds one raw stdout line to the head, or once the head is full to the tail,
// each line cut to maxRawLineBytes.
func (p *failedTestParser) keepRaw(line []byte) {
	text := string(line)
	if len(line) > maxRawLineBytes {
		text = string(line[:maxRawLineBytes]) + " [forgecell: line cut]"
	}
	if p.rawHeadLines < maxRawHeadLines && p.rawHead.dropped == 0 && p.rawHead.b.Len()+len(text)+1 <= p.rawHead.max {
		p.rawHead.add(text)
		p.rawHeadLines++
		return
	}
	p.rawHeadLines = maxRawHeadLines
	p.rawTail.add(text)
}

// keepDiagnostic adds the build output and the package-level output of one event; its
// ok and FAIL package lines go to the summary instead.
func (p *failedTestParser) keepDiagnostic(ev goTestEvent) {
	var into *boundedText
	switch {
	case ev.Action == "build-output":
		into = &p.buildOutput
	case ev.Action == "output" && ev.Test == "":
		into = &p.packageOutput
	default:
		return
	}
	for _, line := range strings.Split(strings.TrimSuffix(ev.Output, "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if into == &p.packageOutput {
			if goTestPackageLine.MatchString(line) {
				p.keepSummaryLine(line)
				continue
			}
			if line == "" || line == "PASS" || line == "FAIL" {
				continue
			}
		}
		if len(line) > maxRawLineBytes {
			line = line[:maxRawLineBytes] + " [forgecell: line cut]"
		}
		into.add(line)
	}
}

// Diagnostics renders what a refused stream kept, so a reviewer sees the compiler and
// package errors a plain run would show: why the stream was refused, the build output,
// the package-level output and the head and tail of the raw stdout, each bounded and
// redacted, within maxStreamDiagnosticsBytes. It never yields a failing set.
func (p *failedTestParser) Diagnostics(refused error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[forgecell: the go test -json stream was refused (%s); its diagnostics follow and no failing test can be declared from it]\n", refused)
	section := func(title string, t *boundedText) {
		if t.b.Len() == 0 && t.dropped == 0 {
			return
		}
		fmt.Fprintf(&b, "=== %s\n", title)
		for _, line := range strings.Split(strings.TrimSuffix(t.b.String(), "\n"), "\n") {
			b.WriteString("    " + line + "\n")
		}
		if t.dropped > 0 {
			fmt.Fprintf(&b, "[forgecell: %s cut at its %d byte bound, %d later line(s) dropped]\n", title, t.max, t.dropped)
		}
	}
	section("build output", &p.buildOutput)
	section("package output", &p.packageOutput)
	section("raw stdout head", &p.rawHead)
	if len(p.rawTail.lines) > 0 {
		fmt.Fprintf(&b, "=== raw stdout tail: last %d line(s)", len(p.rawTail.lines))
		if p.rawTail.dropped > 0 {
			fmt.Fprintf(&b, ", %d line(s) between head and tail dropped", p.rawTail.dropped)
		}
		b.WriteString("\n")
		for _, line := range p.rawTail.lines {
			b.WriteString("    " + line + "\n")
		}
	}
	out := harness.RedactSecrets(b.String())
	if len(out) > maxStreamDiagnosticsBytes {
		out = out[:maxStreamDiagnosticsBytes] + "\n[forgecell: diagnostics cut]\n"
	}
	return out
}

// keepTestOutput adds the lines of one output event to its test's tail. A test that
// passes or is skipped drops its tail; past maxTailedTests a new test keeps none.
func (p *failedTestParser) keepTestOutput(id TestID, output string) {
	t, ok := p.tails[id]
	if !ok {
		if len(p.tails) >= maxTailedTests {
			p.tailsCut = true
			return
		}
		t = &testTail{}
		p.tails[id] = t
	}
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		t.add(strings.TrimRight(line, "\r"))
	}
}

// ran reports whether the stream reported any event of the package.
func (p *failedTestParser) ran(pkg string) bool { return p.packages[pkg] }

// keepSummaryLine appends one package summary line within half of
// maxStreamSummaryBytes (the failing ids take the other half); past the bound it marks
// the summary cut, so a shortened summary is never silent.
func (p *failedTestParser) keepSummaryLine(line string) {
	if p.summaryCut {
		return
	}
	if p.summary.Len()+len(line)+1 > maxStreamSummaryBytes/2 {
		p.summaryCut = true
		return
	}
	p.summary.WriteString(line)
	p.summary.WriteByte('\n')
}

// Summary is the plain stdout an engine-streamed check keeps: one line per failing
// test of the complete stream, then the ok and FAIL package lines, within
// maxStreamSummaryBytes. failed is the set Finish returned, or nil when the run
// passed or the stream could not be trusted.
func (p *failedTestParser) Summary(failed []TestID) string {
	var b strings.Builder
	cut := p.summaryCut
	for _, id := range failed {
		line := "--- FAIL: " + id.Test + " (" + id.Package + ")\n"
		if b.Len()+len(line) > maxStreamSummaryBytes/2 {
			cut = true
			break
		}
		b.WriteString(line)
	}
	b.WriteString(p.summary.String())
	if cut {
		fmt.Fprintf(&b, "[forgecell: summary cut at its %d byte bound; the observation records the complete failing set]\n", maxStreamSummaryBytes)
	}
	b.WriteString(p.tailSummary())
	return b.String()
}

// tailSummary renders the kept output tail of each test that failed, then of each
// test still unfinished (a timeout or panic stops one mid-run), within
// maxTailSummaryBytes. Output of a passing run has no failing test and renders
// nothing; the whole summary is redacted with the rest of the check output.
func (p *failedTestParser) tailSummary() string {
	var b strings.Builder
	cut := p.tailsCut
	render := func(ids []TestID, state string) {
		for _, id := range ids {
			t := p.tails[id]
			if t == nil || len(t.lines) == 0 {
				continue
			}
			var section strings.Builder
			fmt.Fprintf(&section, "=== output tail of %s test %s (%s): last %d line(s)", state, id.Test, id.Package, len(t.lines))
			if t.dropped > 0 {
				fmt.Fprintf(&section, ", %d earlier line(s) dropped", t.dropped)
			}
			section.WriteString("\n")
			for _, line := range t.lines {
				section.WriteString("    " + line + "\n")
			}
			if b.Len()+section.Len() > maxTailSummaryBytes {
				cut = true
				return
			}
			b.WriteString(section.String())
		}
	}
	render(sortedTests(p.failed), "failing")
	render(sortedTests(p.started), "unfinished")
	if cut {
		fmt.Fprintf(&b, "[forgecell: output tails cut at their bounds (%d tests tracked, %d bytes shown); the observation records the complete failing set]\n", maxTailedTests, maxTailSummaryBytes)
	}
	return b.String()
}

// Line consumes one output line. It never returns an error, so a sink built on it
// cannot stop a run that exits zero; the first refusal is reported by Finish.
// After a refusal it only keeps the bounded diagnostics, decoding a line only while
// there is room for its build or package output.
func (p *failedTestParser) Line(raw []byte) error {
	line := bytes.TrimSpace(raw)
	if len(line) == 0 {
		return nil
	}
	p.keepRaw(line)
	if p.err != nil {
		if p.buildOutput.dropped == 0 || p.packageOutput.dropped == 0 {
			var ev goTestEvent
			if json.Unmarshal(line, &ev) == nil {
				p.keepDiagnostic(ev)
			}
		}
		return nil
	}
	var ev goTestEvent
	if err := json.Unmarshal(line, &ev); err != nil || ev.Action == "" {
		p.err = fmt.Errorf("protected regression output is not go test -json; no delta can be accepted")
		return nil
	}
	p.events++
	if ev.Package != "" && !p.packages[ev.Package] {
		if len(p.packages) >= maxStreamedPackages {
			p.err = fmt.Errorf("protected regression reports more than %d packages; no delta can be accepted", maxStreamedPackages)
			return nil
		}
		p.packages[ev.Package] = true
	}
	p.keepDiagnostic(ev)
	if ev.Action == "output" {
		if marker, ok := abortMarker(ev.Test, ev.Output); ok {
			p.aborted[ev.Package] = marker
		}
		if marker, ok := abnormalExitMarker(ev.Output); ok {
			p.aborted[ev.Package] = marker
		}
		if ev.Test != "" {
			p.keepTestOutput(TestID{Package: ev.Package, Test: ev.Test}, ev.Output)
		}
	}
	key := TestID{Package: ev.Package, Test: ev.Test}
	switch ev.Action {
	case "run":
		if ev.Test != "" {
			p.started[key] = true
		}
	case "pass", "skip":
		delete(p.started, key)
		delete(p.tails, key)
	}
	switch ev.Action {
	case "build-fail":
		p.err = fmt.Errorf("build failure in %q; no delta can be accepted", ev.ImportPath)
	case "fail":
		if ev.Test != "" {
			p.failed[key] = true
			delete(p.started, key)
		} else {
			p.failedPackages[ev.Package] = true
		}
	}
	if len(p.failed)+len(p.started) > maxStreamedFailures {
		p.err = fmt.Errorf("protected regression tracks more than %d failing or unfinished tests; no delta can be accepted", maxStreamedFailures)
	}
	return nil
}

func sortedTests(set map[TestID]bool) []TestID {
	out := make([]TestID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Package != out[j].Package {
			return out[i].Package < out[j].Package
		}
		return out[i].Test < out[j].Test
	})
	return out
}

func sortedPackages(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for pkg := range set {
		out = append(out, pkg)
	}
	sort.Strings(out)
	return out
}

// Finish returns the failing tests of the complete stream, or why none can be trusted.
func (p *failedTestParser) Finish() ([]TestID, error) {
	if p.err != nil {
		return nil, p.err
	}
	if p.events == 0 {
		return nil, fmt.Errorf("protected regression produced no go test -json events; no delta can be accepted")
	}
	for _, pkg := range sortedPackages(p.failedPackages) {
		if marker, ok := p.aborted[pkg]; ok {
			return nil, fmt.Errorf("package %q stopped early (%q); tests after the abort never ran, so no delta can be accepted", pkg, marker)
		}
	}
	if unfinished := sortedTests(p.started); len(unfinished) > 0 {
		return nil, fmt.Errorf("test %s in %q started but never finished; no delta can be accepted", harness.RedactSecrets(unfinished[0].Test), harness.RedactSecrets(unfinished[0].Package))
	}
	for _, pkg := range sortedPackages(p.failedPackages) {
		found := false
		for id := range p.failed {
			if id.Package == pkg {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("package %q failed without a failing test (build, setup or panic failure); no delta can be accepted", pkg)
		}
	}
	return sortedTests(p.failed), nil
}

// parseFailedTests reads a complete go test -json output held in memory.
func parseFailedTests(stdout string) ([]TestID, error) {
	p := newFailedTestParser()
	for _, line := range strings.Split(stdout, "\n") {
		p.Line([]byte(line))
	}
	return p.Finish()
}

// uncoveredFailures returns failing tests that no declared delta names. go test
// fails every parent of a failing subtest, but a parent can also fail for its own
// reason, so a parent is accounted for only when the plan declares it explicitly.
func uncoveredFailures(declared []readiness.ProtectedDelta, failed []TestID) []TestID {
	declaredSet := map[TestID]bool{}
	for _, d := range declared {
		declaredSet[TestID{Package: d.Package, Test: d.Test}] = true
	}
	var out []TestID
	for _, id := range failed {
		if !declaredSet[id] {
			out = append(out, id)
		}
	}
	return out
}

// describeUncovered names each undeclared failure and, for a parent of a declared
// failing subtest, says that the parent must be declared explicitly.
func describeUncovered(declared []readiness.ProtectedDelta, failed, uncovered []TestID) string {
	failedSet := map[TestID]bool{}
	for _, id := range failed {
		failedSet[id] = true
	}
	names := make([]string, 0, len(uncovered))
	for _, id := range uncovered {
		name := harness.RedactSecrets(id.Package + " " + id.Test)
		for _, d := range declared {
			if d.Package == id.Package && strings.HasPrefix(d.Test, id.Test+"/") && failedSet[TestID{Package: d.Package, Test: d.Test}] {
				name += " (parent of declared failing subtest " + d.Test + "; it must be declared explicitly)"
				break
			}
		}
		names = append(names, name)
	}
	return strings.Join(names, "; ")
}

// checkProtectedResult refuses a protected regression result whose process did not
// run to a normal exit, whatever its output says.
func checkProtectedResult(r process.Result) error {
	if r.Incomplete() || !r.Reconciled {
		return fmt.Errorf("protected regression output is incomplete (timed out, interrupted, over the capture limit or unreconciled); no delta can be accepted")
	}
	if !r.OK && r.Code <= 0 {
		return fmt.Errorf("protected regression did not exit with a failing status; no delta can be accepted")
	}
	return nil
}

// ProtectedFailuresForDeltas returns the failing tests of one failed protected
// regression observation, for a reviewer's delta proposal. It is the delta
// evaluator's own path: the process must have run to a normal failing exit, the
// retained stream must be complete, and the failedTestParser must accept it, so a
// timeout, panic, build failure, abort marker or truncated output is refused, never
// turned into a list of deltas. An engine-streamed observation (JSONAdded) keeps only
// a plain summary as stdout, so its recorded failing set is used: the runner records
// it only from a complete stream the parser accepted.
func ProtectedFailuresForDeltas(obs Observation) ([]TestID, error) {
	r := obs.Result
	if err := checkProtectedResult(r); err != nil {
		return nil, err
	}
	if r.OK {
		return nil, fmt.Errorf("protected regression passed; there is no failure to accept")
	}
	if obs.JSONAdded {
		if len(obs.FailedTests) == 0 {
			return nil, fmt.Errorf("protected regression with engine-added -json recorded no clean failing set (its stream was refused or had no failing test); no delta can be accepted")
		}
		return append([]TestID(nil), obs.FailedTests...), nil
	}
	if r.Truncated {
		return nil, fmt.Errorf("protected regression output was truncated and was not parsed as a stream; no delta can be accepted")
	}
	failed, err := parseFailedTests(r.Stdout)
	if err != nil {
		return nil, err
	}
	if len(failed) == 0 {
		return nil, fmt.Errorf("protected regression exited non-zero without any failing test; no delta can be accepted")
	}
	return failed, nil
}

// matchDeltas applies the declared deltas to the failing set of a failed check, or
// reports every declared delta as not failing when the check passed.
func matchDeltas(declared []readiness.ProtectedDelta, passed bool, failed []TestID) (DeltaEvaluation, error) {
	var e DeltaEvaluation
	if !passed {
		if len(failed) == 0 {
			return e, fmt.Errorf("protected regression exited non-zero without any failing test; no delta can be accepted")
		}
		if uncovered := uncoveredFailures(declared, failed); len(uncovered) > 0 {
			return e, fmt.Errorf("undeclared protected regression failures: %s", describeUncovered(declared, failed, uncovered))
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
			e.Warnings = append(e.Warnings, fmt.Sprintf("declared protected delta did not fail: %s %s (check %s); amend the plan to remove it", d.Package, d.Test, d.CheckID))
		}
	}
	return e, nil
}

// EvaluateProtectedDeltas decides whether one protected regression check result is
// acceptable under its declared deltas. A passing check is acceptable and yields a
// warning per declared delta, which by definition did not fail. A failing check is
// acceptable only when it exited normally with a failing status, its complete
// go test -json stream parses, and every failing test is declared. A retained
// stream that was truncated is not the complete stream and is refused.
func EvaluateProtectedDeltas(declared []readiness.ProtectedDelta, r process.Result) (DeltaEvaluation, error) {
	if err := checkProtectedResult(r); err != nil {
		return DeltaEvaluation{}, err
	}
	var failed []TestID
	if !r.OK {
		if r.Truncated {
			return DeltaEvaluation{}, fmt.Errorf("protected regression output was truncated and was not parsed as a stream; no delta can be accepted")
		}
		var err error
		if failed, err = parseFailedTests(r.Stdout); err != nil {
			return DeltaEvaluation{}, err
		}
	}
	return matchDeltas(declared, r.OK, failed)
}

// evaluateStreamedDeltas is EvaluateProtectedDeltas for a result whose stdout was
// fed line by line to parser while it ran, so its retained capture may be a prefix.
func evaluateStreamedDeltas(declared []readiness.ProtectedDelta, r process.Result, parser *failedTestParser) (DeltaEvaluation, error) {
	if err := checkProtectedResult(r); err != nil {
		return DeltaEvaluation{}, err
	}
	var failed []TestID
	if !r.OK {
		var err error
		if failed, err = parser.Finish(); err != nil {
			return DeltaEvaluation{}, err
		}
	}
	return matchDeltas(declared, r.OK, failed)
}

// acceptedProtectedFailure re-checks a recorded failing protected observation
// against the approved plan. The compact ledger projection drops captured streams,
// so the recorded failing set is checked for coverage; whenever the complete stream
// is still present it is re-parsed and must yield the same failing set. A streamed
// observation whose retained capture is only a prefix, or an engine-streamed one whose
// stdout is a plain summary, is judged by its recorded set. Deltas apply by test
// identity to a direct go test check, as in the protected view.
func acceptedProtectedFailure(p readiness.Plan, check readiness.Check, obs Observation) bool {
	if check.Category != "regression" || !check.Required || p.Validate() != nil {
		return false
	}
	declared := p.DeltasApplyingTo(check)
	r := obs.Result
	if len(declared) == 0 || len(obs.FailedTests) == 0 || r.Incomplete() || !r.Reconciled || r.Code <= 0 {
		return false
	}
	// Only the runner adds -json, and only to a direct go test check that lacks it.
	if obs.JSONAdded && (!readiness.IdentityDeltaCheck(check) || readiness.RequestsGoTestJSON(check.Argv)) {
		return false
	}
	if r.Stdout != "" && !r.Truncated && !obs.JSONAdded {
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
