// Package demo runs one complete Molecule against a built-in sample repository with
// a deterministic stub harness: no credentials, no model calls, no network and
// nothing published. Every stage runs the real Molecule, policy, verification,
// review and delivery-preview code; only the harness, the reviewer and the GitHub
// reads are stubbed.
package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// Banner is the first line of every demo run.
const Banner = "DEMO — stub harness, no model calls, nothing is published"

// StageHeadings are printed in this order, each as "== HEADING ==".
var StageHeadings = []string{
	"1/6 Issue intake and plan",
	"2/6 Scope approval (auto-approved inside the demo)",
	"3/6 Coding (stub harness)",
	"4/6 Independent checks and the protected regression",
	"5/6 Review (stub reviewer)",
	"6/6 Delivery preview (local only; nothing is pushed or opened)",
}

const (
	// formulaID and the activation's SourceKind mark the Lab as a demo Lab, which
	// deliver, run, loop, issues and checks refuse.
	formulaID = delivery.DemoFormulaID
	// demoTimeout bounds the whole run; every stage also keeps its own bound.
	demoTimeout = 20 * time.Minute
	// maxLedgerBytes bounds the ledger the demo reads back for display.
	maxLedgerBytes = 8_000_000
	// maxDiffDisplayBytes bounds the diff printed in the preview. The digest still
	// covers the whole diff; only the display is cut, and says so.
	maxDiffDisplayBytes = 64 << 10
	lineRunes           = 400
	sampleRepoName      = "sample-repo"
)

// stageAtoms maps each stage to the Atom id of the demo Formula that runs it.
var stageAtoms = []string{"intake", "scope", "coding", "checks", "review"}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Options configures one demo run.
type Options struct {
	// Keep, when set, is the directory the Lab is left in: absent, or an empty
	// directory of the current user that is not a symbolic link. Empty means a
	// temporary Lab that is removed when the run ends.
	Keep string
	// StubCommand is the coding binding's argv. Empty means this executable with
	// StubArg, which serves the stub harness through the custom-command protocol.
	StubCommand []string
	Out         io.Writer
}

// Run runs the demo and prints each stage as it completes. A returned error has
// no absolute local path in it.
func Run(ctx context.Context, o Options) (err error) {
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	s := &session{out: out}
	defer func() {
		if err != nil {
			err = errors.New(s.clean(err.Error()))
		}
	}()
	fmt.Fprintln(out, Banner)
	if err = preflight(); err != nil {
		return err
	}
	command, err := stubCommand(o.StubCommand)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, demoTimeout)
	defer cancel()
	s.ctx = ctx

	var root string
	temporary := o.Keep == ""
	if temporary {
		if root, err = resources.MkdirTemp("", resources.TempPrefix+"demo-"); err != nil {
			return fmt.Errorf("the temporary demo directory could not be created: %w", err)
		}
		s.hide(root, "<demo>")
		s.lab = filepath.Join(root, "lab")
		defer func() {
			// The Lab's check TMPDIR lives outside the Lab, so it goes first, while
			// the Lab path it records still resolves.
			cleanupErr := errors.Join(removeLabCheckTemp(s.lab), resources.Remove(root))
			if cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("the temporary demo Lab was not fully removed: %w", cleanupErr))
				return
			}
			fmt.Fprintln(out, "The temporary demo Lab was removed. Run forgecell demo --keep DIR to keep it for forgecell ledger.")
		}()
		if err = os.Mkdir(s.lab, 0o700); err != nil {
			return err
		}
	} else {
		if s.lab, err = prepareKeepDir(o.Keep); err != nil {
			return err
		}
		s.hide(s.lab, o.Keep)
	}
	fmt.Fprintf(out, "Sample repository: %s (built in; its GitHub origin is recorded but never contacted)\n", Repository)
	fmt.Fprintf(out, "Issue #%d: %s\n", IssueNumber, issueTitle)

	source := filepath.Join(s.lab, sampleRepoName)
	if err = os.Mkdir(source, 0o700); err != nil {
		return err
	}
	base, err := initSampleRepository(ctx, source)
	if err != nil {
		return err
	}
	yaml, err := demoFormula(command)
	if err != nil {
		return err
	}
	if err = activateFormula(s.lab, yaml); err != nil {
		return err
	}
	template := molecule.Options{LabDir: s.lab, SourceRoot: source, FormulaID: formulaID, Base: base, TargetBranch: baseBranch,
		ReadIssue: readFixtureIssue, Analyze: cannedAnalysis, PrepareWorkspace: s.codingStarts, InvokeReview: s.reviewStarts}

	s.begin(1, "Reading the built-in issue and running intake with the canned analysis (no model call).")
	intakeOptions := template
	intakeOptions.Issue = strconv.Itoa(IssueNumber)
	r, runErr := molecule.Run(ctx, intakeOptions)
	if r.ID == "" {
		if s.interrupted() {
			return fmt.Errorf("the demo was interrupted during stage %s; nothing was published", StageHeadings[0])
		}
		return fmt.Errorf("intake could not start: %v", runErr)
	}
	s.id = r.ID
	if r.Status != "waiting" || r.Readiness == nil || r.Readiness.Phase != "scope-waiting" || len(r.Readiness.Plans) != 1 {
		return s.stopped(r, fmt.Errorf("intake did not reach the scope gate (%v)", runErr))
	}
	s.planDigest = r.Readiness.Plans[0].Digest
	s.finish(1, r)

	eval := molecule.EvaluateGatePolicy(r, formula.ScopeApproval)
	if eval.Outcome != molecule.PolicyApprove {
		s.begin(2, "")
		return fmt.Errorf("the demo scope policy did not approve the plan (%s): %s: %s", eval.Outcome, eval.Reason, eval.Detail)
	}
	s.begin(2, fmt.Sprintf("DEMO: the demo Formula's scope gate declares policy %s, and its evaluation approves exactly plan %s. In a real Lab a human approves this digest unless the Formula declares the policy.", eval.Policy, eval.PlanDigest))
	report, loopErr := molecule.RunLoop(ctx, molecule.LoopOptions{Template: template, MaxConcurrent: 1, Once: true, ListIssues: listFixtureIssues})
	if r, err = s.read(); err != nil {
		return err
	}
	if loopErr != nil {
		return s.stopped(r, fmt.Errorf("the Lab loop failed: %w", loopErr))
	}
	if len(report.Passes) != 1 || len(report.Passes[0].Items) != 1 || !report.Passes[0].Items[0].Approved {
		reason := "the loop reported no approval"
		if len(report.Passes) == 1 && len(report.Passes[0].Items) == 1 {
			reason = report.Passes[0].Items[0].Reason
		}
		return s.stopped(r, fmt.Errorf("the scope policy approval was not applied: %s", reason))
	}
	if s.failure != nil {
		return s.stopped(r, s.failure)
	}
	if r.Status != "waiting" || r.Readiness == nil || r.Readiness.Phase != "review-waiting" {
		return s.stopped(r, fmt.Errorf("the Molecule did not reach the human review gate"))
	}
	s.finish(5, r)
	s.begin(6, "Computing the delivery preview from the verified tree; no commit, push or PR is made.")
	if err = s.preview(ctx, r); err != nil {
		return err
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "DEMO complete: Molecule %s is waiting at the human review gate. Nothing was committed, pushed, opened or published.\n", r.ID)
	if !temporary {
		fmt.Fprintf(out, "The Lab is kept. Inspect the full Molecule with: forgecell ledger %s --lab %s\n", r.ID, formula.ShellWord(o.Keep))
	}
	return nil
}

// preflight refuses to start when a tool the demo needs is missing. The demo
// never needs gh, a provider CLI or network access: its Formula binds only the
// stub, and the issue, analysis and review are served in process.
func preflight() error {
	for _, tool := range []string{"git", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("the demo needs %s on PATH and was not started; it never needs gh, a provider CLI or network access", tool)
		}
	}
	return nil
}

// stubCommand resolves the coding binding's argv. It must run a regular file by
// absolute path and must not be a native adapter, so no provider can start.
func stubCommand(command []string) ([]string, error) {
	if len(command) == 0 {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("the forgecell executable could not be located for the stub harness: %w", err)
		}
		command = []string{exe, StubArg}
	}
	if len(command) > 8 || (len(command) == 4 && command[1] == "__adapter") {
		return nil, fmt.Errorf("the demo binds only the stub harness, never a native adapter")
	}
	for _, arg := range command {
		if strings.TrimSpace(arg) == "" || strings.ContainsRune(arg, 0) {
			return nil, fmt.Errorf("the stub harness command is invalid")
		}
	}
	if !filepath.IsAbs(command[0]) {
		return nil, fmt.Errorf("the stub harness executable must be an absolute path")
	}
	if info, err := os.Stat(command[0]); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("the stub harness executable is not a regular file")
	}
	return command, nil
}

// demoFormula is the Formula the demo approves in its own Lab: the standard
// Molecule shape, the stub as coding binding, a review Atom and a scope gate that
// declares the auto-when-ready policy. The review role is never executed: the
// review runs in process, and the real invoker refuses a custom review binding.
func demoFormula(command []string) (string, error) {
	coding, err := json.Marshal(command)
	if err != nil {
		return "", err
	}
	review, err := json.Marshal([]string{command[0], StubArg, "review"})
	if err != nil {
		return "", err
	}
	return "schemaVersion: v0\nkind: formula\nid: " + formulaID + "\nname: Forgecell demo (stub harness, no model calls)\n" +
		"intake: {source: github-issues, repo: " + Repository + "}\n" +
		"harness:\n  binding: demo-stub\n  command: " + string(coding) + "\n  timeoutMs: 120000\n" +
		"harnesses:\n  review: {binding: demo-stub-review, command: " + string(review) + ", timeoutMs: 60000}\n" +
		"atoms:\n" +
		"  - {id: intake, type: intake}\n" +
		"  - {id: scope, type: gate, purpose: scope, mode: policy, policy: " + formula.PolicyAutoWhenReady + "}\n" +
		"  - {id: coding, type: harness}\n" +
		"  - {id: checks, type: check}\n" +
		"  - {id: review, type: review}\n" +
		"  - {id: human-review, type: gate, purpose: review}\n" +
		"  - {id: publication, type: ship}\n" +
		"  - {id: documentation, type: document}\n", nil
}

// activateFormula records the exact-byte activation of the demo Formula in the new
// Lab, the state a completed init --approve leaves. The decision names the demo as
// its source, never a human reviewer.
func activateFormula(lab, yaml string) error {
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		return err
	}
	if err = f.ValidateReadiness(); err != nil {
		return err
	}
	a := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: "demo-built-in-formula", SourceKind: delivery.DemoSourceKind, SourceID: "forgecell demo built-in Formula"}
	if err = formula.BeginActivation(lab, yaml, a, "absent", "absent"); err != nil {
		return err
	}
	if err = formula.ApplyActivation(lab, a); err != nil {
		return err
	}
	return formula.CompleteActivation(lab, a)
}

// prepareKeepDir returns the absolute Lab directory for --keep DIR. DIR must be
// absent (it is created at 0700) or an empty directory of the current user; a
// symbolic link, a non-directory, another user's directory or a directory with
// any entry is refused. The opened directory must be the one inspected.
func prepareKeepDir(dir string) (string, error) {
	refuse := func(why string) (string, error) {
		return "", fmt.Errorf("--keep %s refused: %s; give an absent path or an empty directory you own", formula.ShellWord(dir), why)
	}
	if strings.TrimSpace(dir) == "" || strings.ContainsRune(dir, 0) {
		return refuse("it is not a directory path")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return refuse("it cannot be made absolute")
	}
	info, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		if err = os.Mkdir(abs, 0o700); err != nil {
			return refuse("it could not be created (" + pathErrorCause(err) + ")")
		}
		info, err = os.Lstat(abs)
	}
	if err != nil {
		return refuse("it cannot be inspected (" + pathErrorCause(err) + ")")
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return refuse("it is a symbolic link")
	case !info.IsDir():
		return refuse("it is not a directory")
	case !resources.OwnedByUser(abs, info):
		return refuse("it belongs to another user")
	}
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return refuse("it cannot be opened (" + pathErrorCause(err) + ")")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return refuse("it changed while it was checked")
	}
	names, err := f.Readdirnames(1)
	if err != nil && err != io.EOF {
		return refuse("it cannot be listed (" + pathErrorCause(err) + ")")
	}
	if len(names) > 0 {
		return refuse("it is not empty")
	}
	return abs, nil
}

// pathErrorCause drops the path from a file-system error.
func pathErrorCause(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// removeLabCheckTemp removes the temporary Lab's stable check TMPDIR, which sits
// outside the Lab under the Lab temp root. Only a real directory of the user that
// holds the Forgecell marker and names this Lab is removed; anything else is left
// for the orphan sweep.
func removeLabCheckTemp(lab string) error {
	dir, err := resources.LabTempPath(lab)
	if err != nil {
		return nil
	}
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !resources.OwnedByUser(dir, info) {
		return fmt.Errorf("the demo Lab's check TMPDIR is not a directory Forgecell created; it was left in place")
	}
	if marker, err := os.Lstat(filepath.Join(dir, resources.LabTempMarker)); err != nil || !marker.Mode().IsRegular() {
		return fmt.Errorf("the demo Lab's check TMPDIR has no Forgecell marker; it was left in place")
	}
	recorded, _, err := readBoundedRegular(dir, resources.LabTempLabFile, 4096)
	if err != nil {
		return fmt.Errorf("the demo Lab's check TMPDIR does not name its Lab; it was left in place")
	}
	resolved, err := filepath.EvalSymlinks(lab)
	if err != nil || strings.TrimSpace(string(recorded)) != resolved {
		return fmt.Errorf("the demo Lab's check TMPDIR names another Lab; it was left in place")
	}
	return resources.RemoveTree(dir)
}

// session prints the stages of one run. Stages print in order: a heading when the
// stage starts and its details once the ledger records them.
type session struct {
	// ctx is the run's context; once it is canceled (SIGINT or SIGTERM) a stop is
	// reported as an interruption, not as a stage that failed.
	ctx        context.Context
	out        io.Writer
	lab, id    string
	planDigest string
	headed     int
	detailed   int
	// failure is a hook's refusal; the Molecule then stopped at that stage.
	failure error
	hidden  []replacement
}

type replacement struct{ path, label string }

// hide makes clean replace path (and its resolved form) with label.
func (s *session) hide(path, label string) {
	for _, p := range []string{path, resolvedPath(path)} {
		if p != "" && p != string(filepath.Separator) {
			s.hidden = append(s.hidden, replacement{p, label})
		}
	}
	sort.SliceStable(s.hidden, func(i, j int) bool { return len(s.hidden[i].path) > len(s.hidden[j].path) })
}

func resolvedPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return resolved
}

// clean removes the demo's own directories, the temp root and the home from text
// printed to the terminal and makes it one bounded line of display-safe text.
func (s *session) clean(text string) string {
	for _, r := range s.hidden {
		text = strings.ReplaceAll(text, r.path, r.label)
	}
	for _, generic := range []struct{ path, label string }{{os.TempDir(), "$TMPDIR"}, {resolvedPath(os.TempDir()), "$TMPDIR"}, {homeDir(), "~"}} {
		if len(generic.path) > 1 {
			text = strings.ReplaceAll(text, strings.TrimSuffix(generic.path, string(filepath.Separator)), generic.label)
		}
	}
	return harness.ReviewDisplayText(text, 2000)
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// line prints one cleaned line, keeping its leading indentation.
func (s *session) line(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	rest := strings.TrimLeft(text, " ")
	fmt.Fprintln(s.out, text[:len(text)-len(rest)]+s.clean(rest))
}

// begin prints the heading of every stage up to stage that has none yet, then note.
func (s *session) begin(stage int, note string) {
	for s.headed < stage {
		s.headed++
		fmt.Fprintf(s.out, "\n== %s ==\n", StageHeadings[s.headed-1])
	}
	if note != "" {
		s.line("%s", note)
	}
}

// finish prints the details of every stage up to stage not yet detailed, from r.
func (s *session) finish(stage int, r molecule.Record) {
	for s.detailed < stage {
		s.detailed++
		s.begin(s.detailed, "")
		s.details(s.detailed, r)
	}
}

func (s *session) read() (molecule.Record, error) {
	var r molecule.Record
	if !safeID.MatchString(s.id) {
		return r, fmt.Errorf("invalid Molecule id")
	}
	raw, _, err := readBoundedRegular(filepath.Join(s.lab, "ledgers"), s.id+".json", maxLedgerBytes)
	if err != nil {
		return r, fmt.Errorf("the demo ledger could not be read: %w", err)
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("the demo ledger is not valid JSON: %w", err)
	}
	if r.ID != s.id {
		return r, fmt.Errorf("the demo ledger names another Molecule")
	}
	return r, nil
}

// codingStarts is the demo's Options.PrepareWorkspace. The approval has been
// saved by then, so it prints stage 2 from the ledger and refuses to start coding
// unless the recorded policy decision binds the plan digest stage 1 printed.
func (s *session) codingStarts(_ context.Context, _ molecule.Workspace) error {
	r, err := s.read()
	if err != nil {
		s.failure = err
		return err
	}
	s.finish(2, r)
	if n := len(r.PolicyDecisions); n == 0 || r.PolicyDecisions[n-1].PlanDigest != s.planDigest {
		s.failure = fmt.Errorf("the recorded scope approval does not bind the plan digest shown in stage 1; coding was not started")
		return s.failure
	}
	s.begin(3, "Running the stub harness through the custom-command adapter protocol: the coding request on stdin, a v1 coding outcome on stdout.")
	return nil
}

// reviewStarts is the demo's Options.InvokeReview. Verification has been saved by
// then, so it prints stages 3 and 4 from the ledger before the canned review.
func (s *session) reviewStarts(ctx context.Context, b formula.Binding, dir string, timeout time.Duration, request map[string]any, withheld []string) process.Result {
	r, err := s.read()
	if err != nil {
		s.failure = err
		return process.Result{Code: -1, Reconciled: true, Error: "the demo could not read its ledger before the review"}
	}
	s.finish(4, r)
	s.begin(5, "Running the stub reviewer: a canned verdict checked by the real review validator.")
	return cannedReview(ctx, b, dir, timeout, request, withheld)
}

// stopped prints what the ledger recorded up to the stage that stopped and
// returns cause with that stage named, or the interruption when the run's
// context was canceled.
func (s *session) stopped(r molecule.Record, cause error) error {
	stage := stoppedStage(r)
	s.finish(stage, r)
	if s.interrupted() {
		return fmt.Errorf("the demo was interrupted during stage %s; nothing was published", StageHeadings[stage-1])
	}
	return fmt.Errorf("the demo Molecule stopped at stage %s: %w", StageHeadings[stage-1], cause)
}

// interrupted reports whether the run was canceled, as by SIGINT or SIGTERM. The
// demo's own deadline is not an interruption.
func (s *session) interrupted() bool {
	return s.ctx != nil && errors.Is(s.ctx.Err(), context.Canceled)
}

// stoppedStage is the stage of the first Atom that failed or blocked, or of the
// first that still waits or never started.
func stoppedStage(r molecule.Record) int {
	status := map[string]string{}
	for _, a := range r.Atoms {
		status[a.ID] = a.Status
	}
	for i, id := range stageAtoms {
		if st := status[id]; st == "failed" || st == "blocked" {
			return i + 1
		}
	}
	for i, id := range stageAtoms {
		if st := status[id]; st != "done" {
			return i + 1
		}
	}
	return len(stageAtoms)
}

func atomOf(r molecule.Record, id string) (molecule.Atom, bool) {
	for _, a := range r.Atoms {
		if a.ID == id {
			return a, true
		}
	}
	return molecule.Atom{}, false
}

func (s *session) details(stage int, r molecule.Record) {
	if a, ok := atomOf(r, stageAtoms[stage-1]); ok {
		s.line("Atom %s: %s — %s", a.ID, a.Status, harness.ReviewDisplayText(a.Detail, lineRunes))
	}
	switch stage {
	case 1:
		s.intakeDetails(r)
	case 2:
		s.approvalDetails(r)
	case 3:
		s.codingDetails(r)
	case 4:
		s.checkDetails(r)
	case 5:
		s.reviewDetails(r)
	}
}

func (s *session) intakeDetails(r molecule.Record) {
	s.line("Molecule: %s", r.ID)
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		s.line("No plan was recorded.")
		return
	}
	proposal := r.Readiness.Plans[len(r.Readiness.Plans)-1]
	a := proposal.Plan.Analysis
	s.line("Plan digest: %s", proposal.Digest)
	s.line("Scope:")
	for _, p := range a.Scope {
		s.line("  - %s: %s", p.Path, harness.ReviewDisplayText(p.Reason, lineRunes))
	}
	s.line("Acceptance:")
	for _, c := range a.Acceptance {
		s.line("  - %s", harness.ReviewDisplayText(c.Description, lineRunes))
	}
	s.line("Checks:")
	for _, c := range a.Checks {
		required := "optional"
		if c.Required {
			required = "required"
		}
		s.line("  - %s [%s, %s]: %s", c.ID, c.Category, required, harness.ReviewDisplayText(strings.Join(c.Argv, " "), lineRunes))
	}
	s.line("Coding allowlist: %d entries derived from the checks and the Formula.", len(proposal.Plan.CodingAllowlist))
}

func (s *session) approvalDetails(r molecule.Record) {
	if len(r.PolicyDecisions) == 0 {
		s.line("No policy decision was recorded; no scope approval was applied.")
		return
	}
	d := r.PolicyDecisions[len(r.PolicyDecisions)-1]
	s.line("Approved by: %s (a policy, never a human)", d.ApprovedBy)
	s.line("Gate %s, plan digest %s, %d evidence pointer(s) recorded.", d.Gate, d.PlanDigest, len(d.Evidence))
	if d.PlanDigest == s.planDigest {
		s.line("The approval binds exactly the plan digest shown in stage 1.")
	}
}

func (s *session) codingDetails(r molecule.Record) {
	if len(r.HarnessAttempts) == 0 {
		s.line("No coding attempt was recorded.")
		return
	}
	a := r.HarnessAttempts[len(r.HarnessAttempts)-1]
	if a.Coding != nil {
		s.line("Coding outcome: %s: %s", a.Coding.Outcome, harness.ReviewDisplayText(a.Coding.Reason, lineRunes))
	}
	s.line("Process: exit %d, reconciled %t.", a.Result.Code, a.Result.Reconciled)
	if a.Capture != nil {
		s.line("Captured tree %s; changed paths: %s; scope violations: %d.", a.Capture.Tree, molecule.BoundedViolations(a.Capture.Changes), len(a.Capture.Violations))
	}
}

func (s *session) checkDetails(r molecule.Record) {
	v := r.Verification
	if v == nil {
		s.line("No verification result was recorded.")
		return
	}
	s.line("Candidate view (the captured tree in a fresh checkout):")
	for _, c := range v.Checks {
		s.line("  - %s [%s]: %s", c.ID, c.Category, passed(c.Result.OK))
	}
	if v.Protected != nil {
		s.line("Protected regression view (tests restored to the base commit):")
		for _, c := range v.Protected.Checks {
			s.line("  - %s [%s]: %s", c.ID, c.Category, passed(c.Result.OK))
		}
	} else {
		s.line("Protected regression view: not run.")
	}
	s.line("Required checks passed: %t. Independent acceptance verified: %t.", v.RequiredChecksPassed, v.IndependentAcceptanceVerified)
}

func passed(ok bool) string {
	if ok {
		return "passed"
	}
	return "FAILED"
}

func (s *session) reviewDetails(r molecule.Record) {
	if len(r.ReviewAttempts) == 0 {
		s.line("No review pass was recorded.")
		return
	}
	a := r.ReviewAttempts[len(r.ReviewAttempts)-1]
	s.line("Review %s: %d high, %d medium, %d low; it reviewed %d of %d changed file(s). Evidence only, never an approval.", a.Status, a.High, a.Medium, a.Low, a.ReviewedFiles, a.ChangedFiles)
	for _, f := range a.Findings {
		s.line("  - [%s] %s:%d %s", f.Severity, harness.ReviewDisplayText(f.File, 120), f.Line, harness.ReviewDisplayText(f.Claim, lineRunes))
	}
	for _, v := range a.VerifiedSound {
		s.line("  sound: %s", harness.ReviewDisplayText(v, lineRunes))
	}
	if a.Reason != "" {
		s.line("Reason: %s", harness.ReviewDisplayText(a.Reason, lineRunes))
	}
}

// preview runs the real delivery preview over the approved scope and prints the
// diff and the PR body that deliver --approve would open. It runs no gh and no
// push; Publish alone does.
func (s *session) preview(ctx context.Context, r molecule.Record) error {
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		return fmt.Errorf("the Molecule has no approved plan to preview")
	}
	files := []string{}
	for _, p := range r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan.Analysis.Scope {
		files = append(files, p.Path)
	}
	p, err := delivery.PreviewDemo(ctx, delivery.Options{LabDir: s.lab, MoleculeID: r.ID, BaseBranch: baseBranch, Files: files})
	if err != nil {
		if s.interrupted() {
			return fmt.Errorf("the demo was interrupted during stage %s; nothing was published", StageHeadings[5])
		}
		return fmt.Errorf("the delivery preview was refused: %w", err)
	}
	s.line("Preview digest: %s", p.Digest)
	s.line("Draft PR that deliver --approve would open: %s, head %s, base %s, files %s.", p.Repo, p.Branch, p.BaseBranch, strings.Join(p.Files, ", "))
	if p.AcceptanceUnverified {
		s.line("Independent acceptance is unverified, so a real delivery would also need --accept-unverified after human review.")
	}
	fmt.Fprintln(s.out, "\nDiff:")
	fmt.Fprintln(s.out, displayBlock(p.Diff, maxDiffDisplayBytes))
	fmt.Fprintf(s.out, "PR title: %s\n", s.clean(issueTitle))
	fmt.Fprintln(s.out, "PR body:")
	fmt.Fprint(s.out, displayBlock(prBody(r, p), maxDiffDisplayBytes))
	return nil
}

// prBody is the body Publish writes for a draft PR. The delivery commit exists
// only after an approved delivery, so the preview names it as such.
func prBody(r molecule.Record, p delivery.PreviewResult) string {
	return fmt.Sprintf("Implements issue #%d.\n\nMolecule: `%s`\nFormula: `%s`\nBase: `%s`\nDelivery: `%s`\n\nAwaiting exact-commit CI and human review. Harness-reported checks are not independent CI evidence. No merge, deployment or issue closure is authorized.\n", r.Issue.Number, r.ID, r.FormulaID, p.BaseCommit, "(the commit an approved delivery creates)")
}

// displayBlock is text made safe for a terminal: valid UTF-8, no control or
// direction characters other than newline and tab, and at most limit bytes, with
// a notice when it was cut.
func displayBlock(text string, limit int) string {
	cut := len(text) > limit
	if cut {
		text = text[:limit]
	}
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == unicode.ReplacementChar {
			return '?'
		}
		return r
	}, strings.ToValidUTF8(text, "?"))
	if cut {
		text += fmt.Sprintf("\n[display cut at %d bytes; the preview digest covers the whole diff]\n", limit)
	}
	return text
}
