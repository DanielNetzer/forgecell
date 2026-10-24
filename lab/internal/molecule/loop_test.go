package molecule

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

type loopLister = func(context.Context, string) (intake.OpenIssues, error)

// loopFixture approves a Formula with the given gate declarations and installs a
// fake coding harness that logs when it starts and ends. It serves Issue n with
// stable content, so the live checks at approval see what intake saw.
func loopFixture(t *testing.T, scopeAtom, reviewAtom, delay string) (Options, string) {
	t.Helper()
	o, _ := fixture(t)
	if scopeAtom != defaultScopeAtom || reviewAtom != defaultReviewAtom {
		raw, err := os.ReadFile(filepath.Join(o.LabDir, "formulas", "sample.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Replace(string(raw), defaultScopeAtom, scopeAtom, 1)
		text = strings.Replace(text, defaultReviewAtom, reviewAtom, 1)
		approveFixture(t, o.LabDir, text, "policy")
	}
	// The default fixture check reads code.txt, a scope file, which auto-when-ready
	// declines because a check names it. Read a file outside the scope instead.
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, err := analyze(ctx, b, i, s)
		a.Checks[0].Argv = []string{"/bin/sh", "-c", "test -f package.json"}
		return a, err
	}
	o.ReadIssue = func(_ context.Context, _ string, number int64, _ string) (Issue, error) {
		return loopIssue(number), nil
	}
	log := filepath.Join(t.TempDir(), "harness.log")
	script := "#!/bin/sh\ncat > request.json\necho start >> '" + log + "'\n" + delay + "printf changed > code.txt\necho end >> '" + log + "'\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Implemented and tested\",\"paths\":[]}'\n"
	if err := writeStub(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return o, log
}

func loopIssue(number int64) Issue {
	return Issue{Number: number, Repo: "acme/widgets", Title: fmt.Sprintf("Change code %d", number), Body: "Write changed", URL: fmt.Sprintf("https://github.com/acme/widgets/issues/%d", number), State: "OPEN"}
}

func loopOpen(number int64, hour int, labels ...string) intake.OpenIssue {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(hour) * time.Hour)
	return intake.OpenIssue{Repository: "acme/widgets", Number: number, Title: fmt.Sprintf("Change code %d", number), URL: loopIssue(number).URL, CreatedAt: created, Labels: labels}
}

func loopFixedList(issues ...intake.OpenIssue) loopLister {
	return func(context.Context, string) (intake.OpenIssues, error) {
		return intake.OpenIssues{Issues: issues}, nil
	}
}

func loopOnce(t *testing.T, o Options, list loopLister) LoopReport {
	t.Helper()
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: list})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Passes) != 1 {
		t.Fatalf("a once run made %d passes", len(report.Passes))
	}
	return report
}

func loopItem(t *testing.T, pass LoopPass, number int64) LoopItem {
	t.Helper()
	for _, item := range pass.Items {
		if item.Issue == number {
			return item
		}
	}
	t.Fatalf("no report for Issue %d: %+v", number, pass.Items)
	return LoopItem{}
}

// loopMolecule runs intake only, leaving the Molecule at its scope gate.
func loopMolecule(t *testing.T, o Options, number int64) Record {
	t.Helper()
	o.Issue = fmt.Sprint(number)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness == nil || r.Status != "waiting" || statePhase(r) != "scope-waiting" {
		t.Fatalf("intake did not reach the scope gate: %+v", r)
	}
	return r
}

func loopRecords(t *testing.T, lab string) map[int64][]Record {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(lab, "ledgers"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64][]Record{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			r := loadRecord(t, lab, strings.TrimSuffix(e.Name(), ".json"))
			out[r.Issue.Number] = append(out[r.Issue.Number], r)
		}
	}
	return out
}

func loopLines(t *testing.T, file string) []string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func loopStarts(lines []string) int {
	n := 0
	for _, line := range lines {
		if line == "start" {
			n++
		}
	}
	return n
}

func loopPeak(lines []string) int {
	running, peak := 0, 0
	for _, line := range lines {
		switch line {
		case "start":
			running++
			peak = max(peak, running)
		case "end":
			running--
		}
	}
	return peak
}

// countAnalysis wraps Analyze and reports the Issue numbers analysed so far.
func countAnalysis(o *Options) func() []int64 {
	var mu sync.Mutex
	var seen []int64
	inner := o.Analyze
	o.Analyze = func(c context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		mu.Lock()
		seen = append(seen, i.Number)
		mu.Unlock()
		return inner(c, b, i, s)
	}
	return func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func loopRemote(number int64, created string) string {
	return fmt.Sprintf(`{"number":%d,"title":"Change code %d","state":"open","html_url":"https://github.com/acme/widgets/issues/%d","created_at":%q,"labels":[],"milestone":null}`, number, number, number, created)
}

// loopTranscripts puts a fake gh and a logging git on PATH. The fake gh serves the
// given Issues in the order given, whatever order the loop asks for.
func loopTranscripts(t *testing.T, remotes ...string) (ghLog, gitLog string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ghLog, gitLog = filepath.Join(dir, "gh.log"), filepath.Join(dir, "git.log")
	page := filepath.Join(dir, "issues.json")
	if err = os.WriteFile(page, []byte("["+strings.Join(remotes, ",")+"]"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err = os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeStub(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho \"$*\" >> '"+ghLog+"'\ncat '"+page+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeStub(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho \"$*\" >> '"+gitLog+"'\nexec '"+realGit+"' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return ghLog, gitLog
}

// Serial: loopTranscripts sets PATH with t.Setenv.
func TestLoopDrainsThreeIssuesOldestFirstWithConcurrencyTwo(t *testing.T) {
	o, harnessLog := loopFixture(t, policyScopeAtom, policyReviewAtom, "")
	ghLog, gitLog := loopTranscripts(t, loopRemote(13, "2026-01-03T00:00:00Z"), loopRemote(12, "2026-01-02T00:00:00Z"), loopRemote(11, "2026-01-01T00:00:00Z"))

	var mu sync.Mutex
	inflight, peak, endsWhenThirdBegan := 0, 0, -1
	began := make(chan int64, 3)
	proceed := map[int64]chan struct{}{11: make(chan struct{}, 1), 12: make(chan struct{}, 1), 13: make(chan struct{}, 1)}
	defer func() {
		for _, c := range proceed {
			select {
			case c <- struct{}{}:
			default:
			}
		}
	}()
	analyze := o.Analyze
	o.Analyze = func(c context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		if i.Number == 13 {
			// A slot frees only when a worker's Molecule has finished, harness included.
			raw, _ := os.ReadFile(harnessLog)
			endsWhenThirdBegan = strings.Count(string(raw), "end\n")
		}
		mu.Unlock()
		began <- i.Number
		<-proceed[i.Number]
		return analyze(c, b, i, s)
	}
	o.PrepareWorkspace = func(context.Context, Workspace) error {
		mu.Lock()
		inflight--
		mu.Unlock()
		return nil
	}

	type outcome struct {
		report LoopReport
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 2, Once: true})
		done <- outcome{report, err}
	}()
	next := func() int64 {
		t.Helper()
		select {
		case n := <-began:
			return n
		case <-time.After(30 * time.Second):
			t.Fatal("no Issue started intake")
			return 0
		}
	}
	first := []int64{next(), next()}
	slices.Sort(first)
	if first[0] != 11 || first[1] != 12 {
		t.Fatalf("intake started for %v, want the two oldest Issues 11 and 12", first)
	}
	proceed[11] <- struct{}{}
	if n := next(); n != 13 {
		t.Fatalf("the third intake was for Issue %d, want 13 once a slot freed", n)
	}
	proceed[12] <- struct{}{}
	proceed[13] <- struct{}{}
	var result outcome
	select {
	case result = <-done:
	case <-time.After(120 * time.Second):
		t.Fatal("the loop did not finish")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	mu.Lock()
	gotPeak, gotEnds := peak, endsWhenThirdBegan
	mu.Unlock()
	if gotPeak != 2 {
		t.Fatalf("peak intake concurrency %d, want 2", gotPeak)
	}
	if gotEnds < 1 {
		t.Fatalf("the third intake began after %d finished Molecules; it must wait for a slot", gotEnds)
	}

	report := result.report
	if len(report.Passes) != 1 || report.Failed() {
		t.Fatalf("%+v", report)
	}
	var order []int64
	for _, item := range report.Passes[0].Items {
		order = append(order, item.Issue)
		if !item.Started || !item.Approved || item.Failed || item.State != LoopWaiting || item.Phase != "review-waiting" {
			t.Fatalf("%+v", item)
		}
	}
	if !slices.Equal(order, []int64{11, 12, 13}) {
		t.Fatalf("the report is not oldest first: %v", order)
	}
	records := loopRecords(t, o.LabDir)
	if len(records) != 3 {
		t.Fatalf("ledgers by Issue: %v", records)
	}
	for number := int64(11); number <= 13; number++ {
		if len(records[number]) != 1 {
			t.Fatalf("Issue %d has %d ledgers", number, len(records[number]))
		}
		r := records[number][0]
		if r.Status != "waiting" || statePhase(r) != "review-waiting" || len(r.PolicyDecisions) != 1 || len(r.HarnessAttempts) != 1 {
			t.Fatalf("Issue %d: status %s phase %s decisions %d attempts %d", number, r.Status, statePhase(r), len(r.PolicyDecisions), len(r.HarnessAttempts))
		}
	}
	lines := loopLines(t, harnessLog)
	if loopStarts(lines) != 3 || loopPeak(lines) > 2 {
		t.Fatalf("harness events: %v", lines)
	}

	calls := loopLines(t, ghLog)
	if len(calls) != 1 {
		t.Fatalf("gh calls: %v", calls)
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "api --hostname github.com --method GET repos/acme/widgets/issues?") {
			t.Fatalf("unexpected gh call: %s", call)
		}
	}
	gitCalls := loopLines(t, gitLog)
	if len(gitCalls) == 0 {
		t.Fatal("the git transcript is empty")
	}
	for _, call := range gitCalls {
		for _, word := range strings.Fields(call) {
			if word == "push" || word == "merge" || word == "pull" || word == "send-pack" {
				t.Fatalf("the loop ran git %s", call)
			}
		}
	}
}

func TestLoopPolicyDecisionRecordsIdentityAndEvidence(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
	item := loopItem(t, report.Passes[0], 12)
	if !item.Started || !item.Approved || item.Failed || item.MoleculeID == "" {
		t.Fatalf("%+v", item)
	}
	r := loadRecord(t, o.LabDir, item.MoleculeID)
	if len(r.PolicyDecisions) != 1 {
		t.Fatalf("decisions: %d", len(r.PolicyDecisions))
	}
	d := r.PolicyDecisions[0]
	want := "policy:auto-when-ready@" + r.FormulaSnapshot.SHA256
	if d.ApprovedBy != want || d.Policy != "auto-when-ready" || d.Gate != "scope" || d.PlanDigest != r.Readiness.Plans[0].Digest || d.FormulaSHA256 != r.FormulaSnapshot.SHA256 || len(d.Evidence) == 0 {
		t.Fatalf("%+v", d)
	}
	marshalled, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err = json.Unmarshal(marshalled, &doc); err != nil {
		t.Fatal(err)
	}
	for _, e := range d.Evidence {
		if !pointerTarget(doc, e.Pointer) || e.Observed == "" {
			t.Fatalf("evidence does not resolve in the record: %+v", e)
		}
	}
	scope := r.Atoms[1]
	if scope.ID != "scope" || scope.Status != "done" || !strings.Contains(scope.Detail, want) {
		t.Fatalf("%+v", scope)
	}
	actions := scope.Provenance.Actions
	if len(actions) != 1 || actions[0].ResolvedSource != "readiness-policy-approval" {
		t.Fatalf("the scope approval is not recorded as a policy approval: %+v", actions)
	}
	_, md := ledgerFiles(t, r)
	if !strings.Contains(md, "Approved by: "+want) || !strings.Contains(md, "policy-approved") {
		t.Fatal("the Markdown ledger does not show a policy approval")
	}
}

func TestLoopNeverApprovesAHumanGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, scope string }{
		{"no mode", defaultScopeAtom},
		{"mode human", "{id: scope, type: gate, purpose: scope, mode: human}"},
		{"unrecognised mode", "{id: scope, type: gate, purpose: scope, mode: auto}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, harnessLog := loopFixture(t, tc.scope, defaultReviewAtom, "")
			list := loopFixedList(loopOpen(12, 0))
			report := loopOnce(t, o, list)
			item := loopItem(t, report.Passes[0], 12)
			if !item.Started || item.Approved || item.Failed || item.State != LoopWaiting || !strings.Contains(item.Reason, "gate-is-human") || !strings.Contains(item.Reason, "needs a human") {
				t.Fatalf("%+v", item)
			}
			r := loadRecord(t, o.LabDir, item.MoleculeID)
			if len(r.Analysis.Questions) != 0 {
				t.Fatal("the analysis was expected to have no questions")
			}
			assertNothingApproved(t, r)
			if report.Failed() || loopStarts(loopLines(t, harnessLog)) != 0 {
				t.Fatal("a human gate ran coding")
			}
			again := loopOnce(t, o, list)
			second := loopItem(t, again.Passes[0], 12)
			if second.Started || second.Approved || second.MoleculeID != item.MoleculeID || second.State != LoopWaiting {
				t.Fatalf("%+v", second)
			}
			assertNothingApproved(t, r)
			if len(loopRecords(t, o.LabDir)[12]) != 1 {
				t.Fatal("a second pass created another Molecule")
			}
		})
	}
}

func TestLoopQuestionAtIntakeStopsOnlyThatMolecule(t *testing.T) {
	t.Parallel()
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	analyze := o.Analyze
	o.Analyze = func(c context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		if i.Number == 12 {
			return readiness.Analysis{Summary: "Needs clarification", Questions: oneQuestion()}, nil
		}
		return analyze(c, b, i, s)
	}
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 2, Once: true, ListIssues: loopFixedList(loopOpen(11, 0), loopOpen(12, 1), loopOpen(13, 2))})
	if err != nil {
		t.Fatal(err)
	}
	asked := loopItem(t, report.Passes[0], 12)
	if !asked.Started || asked.Approved || asked.Failed || asked.State != LoopWaiting || asked.MoleculeID == "" || !strings.Contains(asked.Reason, "1 open question") {
		t.Fatalf("%+v", asked)
	}
	for _, number := range []int64{11, 13} {
		if item := loopItem(t, report.Passes[0], number); !item.Approved || item.Phase != "review-waiting" {
			t.Fatalf("Issue %d: %+v", number, item)
		}
	}
	if report.Failed() {
		t.Fatal("a question was reported as a failure")
	}
	r := loadRecord(t, o.LabDir, asked.MoleculeID)
	if r.Status != "blocked" || len(r.PolicyDecisions) != 0 || len(r.HarnessAttempts) != 0 {
		t.Fatalf("%+v", r)
	}
	if loopStarts(loopLines(t, harnessLog)) != 2 {
		t.Fatal("the question stopped more than its own Molecule")
	}
}

func TestLoopQuestionAtScopeGateIsDeclinedAndOthersStillAdvance(t *testing.T) {
	t.Parallel()
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	asked := loopMolecule(t, o, 11)
	loopMolecule(t, o, 12)
	r := loadRecord(t, o.LabDir, asked.ID)
	r.Analysis.Questions = oneQuestion()
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	report := loopOnce(t, o, loopFixedList(loopOpen(11, 0), loopOpen(12, 1)))
	item := loopItem(t, report.Passes[0], 11)
	if item.Started || item.Approved || item.Failed || item.State != LoopWaiting || item.MoleculeID != asked.ID || !strings.Contains(item.Reason, "analysis-has-questions") {
		t.Fatalf("%+v", item)
	}
	assertNothingApproved(t, loadRecord(t, o.LabDir, asked.ID))
	other := loopItem(t, report.Passes[0], 12)
	if other.Started || !other.Approved || other.Phase != "review-waiting" {
		t.Fatalf("%+v", other)
	}
	if loopStarts(loopLines(t, harnessLog)) != 1 {
		t.Fatal("the declined Molecule reached coding, or the other one did not")
	}
}

func TestLoopReportsAnApprovalThatCouldNotBeApplied(t *testing.T) {
	t.Parallel()
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	var mu sync.Mutex
	reads := 0
	o.ReadIssue = func(_ context.Context, _ string, number int64, _ string) (Issue, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		issue := loopIssue(number)
		if reads > 1 {
			issue.Body += " changed after intake"
		}
		return issue, nil
	}
	report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
	item := loopItem(t, report.Passes[0], 12)
	if !item.Started || item.Approved || item.Failed || item.State != LoopStalled || !strings.Contains(item.Reason, "not applied") {
		t.Fatalf("%+v", item)
	}
	if report.Failed() || report.NonProgress() != 1 {
		t.Fatalf("an unapplied approval must be non-progress, not a failure: failed %t, non-progress %d", report.Failed(), report.NonProgress())
	}
	assertNothingApproved(t, loadRecord(t, o.LabDir, item.MoleculeID))
	if loopStarts(loopLines(t, harnessLog)) != 0 {
		t.Fatal("a stale approval reached coding")
	}
}

func TestLoopCountsFailedAndBlockedMoleculesButNotWaiting(t *testing.T) {
	t.Parallel()
	t.Run("failed coding", func(t *testing.T) {
		o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
		if err := writeStub(filepath.Join(filepath.Dir(o.SourceRoot), "harness"), []byte("#!/bin/sh\ncat >/dev/null\nprintf failed >&2\nexit 7\n"), 0700); err != nil {
			t.Fatal(err)
		}
		report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
		item := loopItem(t, report.Passes[0], 12)
		if !item.Started || !item.Approved || !item.Failed || item.State != LoopStopped || item.Status != "failed" || !strings.Contains(item.Reason, "Harness failed") {
			t.Fatalf("%+v", item)
		}
		if !report.Failed() {
			t.Fatal("a failed Molecule was not counted")
		}
	})
	t.Run("blocked intake", func(t *testing.T) {
		o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
		o.ReadIssue = func(_ context.Context, _ string, number int64, _ string) (Issue, error) {
			issue := loopIssue(number)
			issue.State = "CLOSED"
			return issue, nil
		}
		report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
		item := loopItem(t, report.Passes[0], 12)
		if !item.Started || item.Approved || !item.Failed || item.State != LoopStopped || item.Status != "blocked" || !strings.Contains(item.Reason, "closed") {
			t.Fatalf("%+v", item)
		}
		if !report.Failed() {
			t.Fatal("a blocked Molecule was not counted")
		}
		again := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
		if second := loopItem(t, again.Passes[0], 12); second.Started || second.Failed || second.State != LoopStopped || again.Failed() {
			t.Fatalf("a Molecule stopped in an earlier run was started or counted again: %+v", second)
		}
		if len(loopRecords(t, o.LabDir)[12]) != 1 {
			t.Fatal("a stopped Molecule was started again")
		}
	})
	t.Run("intake could not start", func(t *testing.T) {
		o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
		testGit(t, o.SourceRoot, "remote", "set-url", "origin", "https://github.com/other/repo.git")
		report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
		item := loopItem(t, report.Passes[0], 12)
		if item.Started || !item.Failed || item.State != LoopStopped || item.MoleculeID != "" || !strings.Contains(item.Reason, "origin") {
			t.Fatalf("%+v", item)
		}
		if len(loopRecords(t, o.LabDir)) != 0 {
			t.Fatal("a ledger was written for an intake that never started")
		}
	})
}

func TestLoopNeverStartsAnIssueThatAlreadyHasAMolecule(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	existing := loopMolecule(t, o, 12)
	analyses := countAnalysis(&o)
	report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
	item := loopItem(t, report.Passes[0], 12)
	if item.Started || item.MoleculeID != existing.ID || item.State != LoopWaiting {
		t.Fatalf("%+v", item)
	}
	if len(analyses()) != 0 || len(loopRecords(t, o.LabDir)[12]) != 1 {
		t.Fatal("an Issue with a Molecule was started again")
	}
}

func TestLoopFailsWhenTheLedgerInventoryIsIncomplete(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, file, cause string
		prepare           func(t *testing.T, path string)
	}{
		{"malformed ledger", "broken.json", "broken.json", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated scan", "huge.json", "5000000 bytes", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, 5_000_001); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
			dir := filepath.Join(o.LabDir, "ledgers")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			tc.prepare(t, filepath.Join(dir, tc.file))
			if inventory := ReadQueueInventory(o.LabDir); inventory.Complete {
				t.Fatal("the fixture did not make the inventory incomplete")
			}
			analyses := countAnalysis(&o)
			report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 2, Once: true, ListIssues: loopFixedList(loopOpen(11, 0), loopOpen(12, 1))})
			if err == nil || !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), tc.cause) {
				t.Fatalf("the loop did not fail naming its cause: %v", err)
			}
			if len(report.Passes) != 0 || report.Failed() {
				t.Fatalf("an incomplete inventory produced a pass: %+v", report)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil || len(entries) != 1 || len(analyses()) != 0 {
				t.Fatalf("work started on an incomplete inventory: %v %v", entries, readErr)
			}
		})
	}
}

func TestLoopRestartDoesNotDuplicateCompletedWork(t *testing.T) {
	t.Parallel()
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	analyses := countAnalysis(&o)
	list := loopFixedList(loopOpen(12, 0))
	first := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
	if !first.Started || !first.Approved {
		t.Fatalf("%+v", first)
	}
	second := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
	if second.Started || second.Approved || second.Failed || second.MoleculeID != first.MoleculeID || second.Phase != "review-waiting" {
		t.Fatalf("%+v", second)
	}
	if loopStarts(loopLines(t, harnessLog)) != 1 || len(analyses()) != 1 || len(loopRecords(t, o.LabDir)[12]) != 1 {
		t.Fatalf("a restart repeated work: harness %d, analyses %v", loopStarts(loopLines(t, harnessLog)), analyses())
	}
}

func TestLoopRestartMidAttemptDoesNotReinvokeTheHarness(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"running", "allocating"} {
		t.Run(status, func(t *testing.T) {
			o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
			r := loopMolecule(t, o, 12)
			p := r.Readiness.Plans[0]
			state, err := r.Readiness.Approve(p.Digest, p.Plan.Inputs, stamp())
			if err == nil {
				state, err = state.StartAttempt(stamp())
			}
			if err != nil {
				t.Fatal(err)
			}
			r.Readiness = &state
			r.Status = status
			if err = save(r); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
			before, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			analyses := countAnalysis(&o)
			report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
			item := loopItem(t, report.Passes[0], 12)
			if item.Started || item.Approved || item.Failed || item.State != LoopActive || item.MoleculeID != r.ID || !strings.Contains(item.Reason, "forgecell recover "+r.ID) || !strings.Contains(item.Reason, "liveness is unverified") {
				t.Fatalf("%+v", item)
			}
			after, err := os.ReadFile(file)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("the ledger of an active Molecule was changed: %v", err)
			}
			if loopStarts(loopLines(t, harnessLog)) != 0 || len(analyses()) != 0 || len(loopRecords(t, o.LabDir)[12]) != 1 {
				t.Fatal("work was repeated for an active Molecule")
			}
		})
	}
}

func TestLoopSkipsAMoleculeWhoseLockIsHeld(t *testing.T) {
	t.Parallel()
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	r := loopMolecule(t, o, 12)
	list := loopFixedList(loopOpen(12, 0))
	unlock, err := lockMolecule(o.LabDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
	if item.State != LoopLocked || item.Approved || item.Failed || item.MoleculeID != r.ID {
		t.Fatalf("%+v", item)
	}
	assertNothingApproved(t, r)
	unlock()
	item = loopItem(t, loopOnce(t, o, list).Passes[0], 12)
	if !item.Approved || item.State != LoopWaiting {
		t.Fatalf("%+v", item)
	}
	if loopStarts(loopLines(t, harnessLog)) != 1 {
		t.Fatal("the Molecule was not advanced once its lock was released")
	}
	again, err := lockMolecule(o.LabDir, r.ID)
	if err != nil {
		t.Fatalf("the lock was not released after the step: %v", err)
	}
	again()
}

// loopGate blocks the first caller of hold until the test calls open, and lets
// every later caller straight through. It honours the caller's context, so a
// loop that wrongly reaches it fails the test instead of hanging it.
type loopGate struct {
	entered chan struct{}
	opened  chan struct{}
	once    sync.Once
}

func newLoopGate() *loopGate {
	return &loopGate{entered: make(chan struct{}), opened: make(chan struct{})}
}

func (g *loopGate) hold(ctx context.Context) error {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.opened:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *loopGate) open() { close(g.opened) }

func (g *loopGate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the first loop never reached the gate")
	}
}

type loopOutcome struct {
	report LoopReport
	err    error
}

func loopBackground(ctx context.Context, lo LoopOptions) <-chan loopOutcome {
	done := make(chan loopOutcome, 1)
	go func() {
		report, err := RunLoop(ctx, lo)
		done <- loopOutcome{report, err}
	}()
	return done
}

func loopWait(t *testing.T, done <-chan loopOutcome) LoopReport {
	t.Helper()
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatal(out.err)
		}
		return out.report
	case <-time.After(120 * time.Second):
		t.Fatal("the first loop did not finish")
		return LoopReport{}
	}
}

func loopBounded(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLoopTwoConcurrentLoopsInvokeTheHarnessOnce(t *testing.T) {
	t.Parallel()
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	r := loopMolecule(t, o, 12)
	list := loopFixedList(loopOpen(12, 0))
	gate := newLoopGate()
	// The approval re-reads the Issue while the Molecule lock is held and before the
	// ledger leaves scope-waiting, so a second loop still sees a Molecule it may advance.
	o.ReadIssue = func(ctx context.Context, _ string, number int64, _ string) (Issue, error) {
		return loopIssue(number), gate.hold(ctx)
	}

	first := loopBackground(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: list})
	gate.waitEntered(t)
	second, err := RunLoop(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: list})
	if err != nil {
		t.Fatal(err)
	}
	skipped := loopItem(t, second.Passes[0], 12)
	if skipped.State != LoopLocked || skipped.Approved || skipped.Started || skipped.Failed || skipped.MoleculeID != r.ID {
		t.Fatalf("the second loop was not held off by the Molecule lock: %+v", skipped)
	}
	gate.open()
	won := loopItem(t, loopWait(t, first).Passes[0], 12)
	if !won.Approved || won.Failed || won.State != LoopWaiting {
		t.Fatalf("%+v", won)
	}
	if loopStarts(loopLines(t, harnessLog)) != 1 {
		t.Fatalf("harness invocations %d", loopStarts(loopLines(t, harnessLog)))
	}
	if saved := loadRecord(t, o.LabDir, r.ID); len(saved.PolicyDecisions) != 1 || len(saved.HarnessAttempts) != 1 {
		t.Fatalf("decisions %d, attempts %d", len(saved.PolicyDecisions), len(saved.HarnessAttempts))
	}
}

func TestLoopOverlappingLoopDuringIntakeCreatesNoSecondMolecule(t *testing.T) {
	t.Parallel()
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	analyses := countAnalysis(&o)
	gate := newLoopGate()
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		if err := gate.hold(ctx); err != nil {
			return readiness.Analysis{}, err
		}
		return analyze(ctx, b, i, s)
	}
	list := loopFixedList(loopOpen(12, 0))

	first := loopBackground(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: list})
	gate.waitEntered(t)
	second, err := RunLoop(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: list})
	if err != nil {
		t.Fatal(err)
	}
	overlap := loopItem(t, second.Passes[0], 12)
	if overlap.Started || overlap.Approved || overlap.Failed || overlap.State != LoopActive || !strings.Contains(overlap.Reason, "forgecell recover") {
		t.Fatalf("the overlapping loop did not leave the running Molecule alone: %+v", overlap)
	}
	gate.open()
	started := loopItem(t, loopWait(t, first).Passes[0], 12)
	if !started.Started || !started.Approved || started.Failed {
		t.Fatalf("%+v", started)
	}
	if got := analyses(); len(got) != 1 || len(loopRecords(t, o.LabDir)[12]) != 1 || loopStarts(loopLines(t, harnessLog)) != 1 {
		t.Fatalf("analyses %v, Molecules %d, harness invocations %d", got, len(loopRecords(t, o.LabDir)[12]), loopStarts(loopLines(t, harnessLog)))
	}
}

// TestLoopStartWindowIsSerialisedPerIssue holds the first loop inside Run, after
// it took the Issue's start lock and before Run wrote any ledger. The second
// loop's inventory therefore still shows the Issue untouched, which is the
// window a pass-start inventory alone cannot close. A fake git blocks on named
// pipes to hold that point without sleeping.
// Serial: puts a fake git on PATH with t.Setenv.
func TestLoopStartWindowIsSerialisedPerIssue(t *testing.T) {
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	analyses := countAnalysis(&o)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	entered, release, gate := filepath.Join(dir, "entered"), filepath.Join(dir, "release"), filepath.Join(dir, "gate")
	for _, fifo := range []string{entered, release} {
		if err = syscall.Mkfifo(fifo, 0600); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "bin")
	if err = os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif mkdir '" + gate + "' 2>/dev/null; then\n  echo entered > '" + entered + "'\n  read line < '" + release + "'\nfi\nexec '" + realGit + "' \"$@\"\n"
	if err = writeStub(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	list := loopFixedList(loopOpen(12, 0))

	first := loopBackground(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: list})
	reached := make(chan error, 1)
	go func() {
		f, e := os.Open(entered)
		if e == nil {
			_, e = io.ReadAll(f)
			f.Close()
		}
		reached <- e
	}()
	select {
	case e := <-reached:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the first loop never reached its first git call")
	}
	if len(loopRecords(t, o.LabDir)) != 0 {
		t.Fatal("the first loop wrote a ledger before the window under test")
	}
	second, err := RunLoop(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: list})
	if err != nil {
		t.Fatal(err)
	}
	blocked := loopItem(t, second.Passes[0], 12)
	if blocked.State != LoopLocked || blocked.Started || blocked.Approved || blocked.Failed || blocked.MoleculeID != "" || !strings.Contains(blocked.Reason, "start lock") {
		t.Fatalf("the second loop was not held off by the Issue's start lock: %+v", blocked)
	}
	if len(loopRecords(t, o.LabDir)) != 0 || len(analyses()) != 0 {
		t.Fatal("the second loop created work in the window")
	}
	pipe, err := os.OpenFile(release, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pipe.WriteString("go\n"); err != nil {
		t.Fatal(err)
	}
	pipe.Close()
	won := loopItem(t, loopWait(t, first).Passes[0], 12)
	if !won.Started || !won.Approved || won.Failed {
		t.Fatalf("%+v", won)
	}
	if got := analyses(); len(got) != 1 || len(loopRecords(t, o.LabDir)[12]) != 1 || loopStarts(loopLines(t, harnessLog)) != 1 {
		t.Fatalf("analyses %v, Molecules %d, harness invocations %d", got, len(loopRecords(t, o.LabDir)[12]), loopStarts(loopLines(t, harnessLog)))
	}
	unlock, err := lockIssue(o.LabDir, "acme/widgets", 12)
	if err != nil {
		t.Fatalf("the start lock was not released after intake: %v", err)
	}
	unlock()
}

func TestLoopRereadsTheLedgersUnderTheStartLockBeforeStarting(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	loaded, err := formula.Load(o.LabDir, o.FormulaID)
	if err != nil {
		t.Fatal(err)
	}
	stale := ReadQueueInventory(o.LabDir)
	if !stale.Complete || stale.Summarize("acme/widgets", 12).State != "untouched" {
		t.Fatal("the stale inventory was expected to show the Issue untouched")
	}
	existing := loopMolecule(t, o, 12)
	analyses := countAnalysis(&o)
	l := &loopRun{o: LoopOptions{Template: o, MaxConcurrent: 1}, lab: o.LabDir, repo: "acme/widgets", loaded: loaded}
	item := l.process(context.Background(), stale, loopOpen(12, 0))
	if item.Started || item.Approved || item.Failed || item.MoleculeID != existing.ID || item.State != LoopWaiting {
		t.Fatalf("a Molecule created after the inventory was read was ignored: %+v", item)
	}
	if len(analyses()) != 0 || len(loopRecords(t, o.LabDir)[12]) != 1 {
		t.Fatal("a second Molecule was started from a stale inventory")
	}
}

func TestLoopSkipsAnIssueWhoseStartLockIsHeld(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	analyses := countAnalysis(&o)
	list := loopFixedList(loopOpen(12, 0))
	unlock, err := lockIssue(o.LabDir, "acme/widgets", 12)
	if err != nil {
		t.Fatal(err)
	}
	item := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
	if item.State != LoopLocked || item.Started || item.Failed || len(analyses()) != 0 || len(loopRecords(t, o.LabDir)) != 0 {
		t.Fatalf("%+v", item)
	}
	if _, err = lockIssue(o.LabDir, "ACME/Widgets", 12); !errors.Is(err, errLocked) {
		t.Fatalf("the lock is case sensitive: %v", err)
	}
	unlock()
	if item = loopItem(t, loopOnce(t, o, list).Passes[0], 12); !item.Started || !item.Approved {
		t.Fatalf("%+v", item)
	}
}

// Serial: helper-process entry point that reads its environment.
func TestLoopLockHolderHelper(t *testing.T) {
	lab, id := os.Getenv("LOOP_LOCK_LAB"), os.Getenv("LOOP_LOCK_ID")
	if lab == "" {
		t.Skip("helper process for TestLoopLockIsReleasedWhenItsHolderCrashes")
	}
	if _, err := lockMolecule(lab, id); err != nil {
		fmt.Println("error:", err)
		os.Exit(3)
	}
	fmt.Println("locked")
	time.Sleep(time.Minute)
}

// Serial: launches TestLoopLockHolderHelper as a helper process.
func TestLoopLockIsReleasedWhenItsHolderCrashes(t *testing.T) {
	lab := t.TempDir()
	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLoopLockHolderHelper$")
	cmd.Env = append(os.Environ(), "LOOP_LOCK_LAB="+lab, "LOOP_LOCK_ID=mol-1-crash")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("the helper did not take the lock: %q %v", line, err)
	}
	if _, err = lockMolecule(lab, "mol-1-crash"); !errors.Is(err, errLocked) {
		t.Fatalf("a held lock was granted: %v", err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	unlock, err := lockMolecule(lab, "mol-1-crash")
	if err != nil {
		t.Fatalf("the lock outlived its crashed holder: %v", err)
	}
	unlock()
}

func TestLoopSelectionRestrictsWhichIssuesStart(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	analyses := countAnalysis(&o)
	selector, err := intake.ParseSelector("label:bug")
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, Select: selector, MaxConcurrent: 1, Once: true, ListIssues: loopFixedList(loopOpen(13, 2, "BUG"), loopOpen(12, 1), loopOpen(11, 0, "Bug", "docs"))})
	if err != nil {
		t.Fatal(err)
	}
	var reported []int64
	for _, item := range report.Passes[0].Items {
		reported = append(reported, item.Issue)
	}
	seen := analyses()
	slices.Sort(seen)
	if !slices.Equal(reported, []int64{11, 13}) || !slices.Equal(seen, []int64{11, 13}) {
		t.Fatalf("reported %v, analysed %v", reported, seen)
	}
}

func TestLoopRepeatsPassesUntilNothingProgresses(t *testing.T) {
	t.Parallel()
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	calls := 0
	sets := [][]intake.OpenIssue{{loopOpen(11, 0)}, {loopOpen(11, 0), loopOpen(12, 1)}, {loopOpen(11, 0), loopOpen(12, 1)}}
	list := func(context.Context, string) (intake.OpenIssues, error) {
		set := sets[min(calls, len(sets)-1)]
		calls++
		return intake.OpenIssues{Issues: set}, nil
	}
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, ListIssues: list})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Passes) != 3 || calls != 3 || report.Failed() {
		t.Fatalf("passes %d, listings %d", len(report.Passes), calls)
	}
	if loopStarts(loopLines(t, harnessLog)) != 2 || len(loopRecords(t, o.LabDir)) != 2 {
		t.Fatal("a repeated pass duplicated work")
	}
}

func TestLoopFailuresBeforeAnyEffectAreErrors(t *testing.T) {
	t.Parallel()
	t.Run("no approved Formula", func(t *testing.T) {
		o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
		if err := os.Remove(filepath.Join(o.LabDir, "lab.json")); err != nil {
			t.Fatal(err)
		}
		listed := 0
		list := func(context.Context, string) (intake.OpenIssues, error) {
			listed++
			return intake.OpenIssues{}, nil
		}
		if _, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: list}); err == nil || listed != 0 {
			t.Fatalf("error %v, listings %d", err, listed)
		}
	})
	t.Run("listing fails", func(t *testing.T) {
		o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
		analyses := countAnalysis(&o)
		list := func(context.Context, string) (intake.OpenIssues, error) {
			return intake.OpenIssues{}, fmt.Errorf("GitHub data unavailable")
		}
		report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: list})
		if err == nil || len(report.Passes) != 0 || len(analyses()) != 0 || len(loopRecords(t, o.LabDir)) != 0 {
			t.Fatalf("error %v, passes %d", err, len(report.Passes))
		}
	})
	t.Run("max concurrent below one", func(t *testing.T) {
		o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
		for _, n := range []int{0, -1} {
			if _, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: n, Once: true, ListIssues: loopFixedList(loopOpen(12, 0))}); err == nil {
				t.Fatalf("max concurrent %d accepted", n)
			}
		}
		if len(loopRecords(t, o.LabDir)) != 0 {
			t.Fatal("work started despite invalid options")
		}
	})
	t.Run("truncated listing is reported", func(t *testing.T) {
		o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
		list := func(context.Context, string) (intake.OpenIssues, error) {
			return intake.OpenIssues{Issues: []intake.OpenIssue{loopOpen(12, 0)}, Truncated: true}, nil
		}
		report := loopOnce(t, o, list)
		if !strings.Contains(strings.Join(report.Passes[0].Notes, "\n"), "truncated") {
			t.Fatalf("%v", report.Passes[0].Notes)
		}
	})
	t.Run("cancelled before dispatch", func(t *testing.T) {
		o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
		analyses := countAnalysis(&o)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		report, err := RunLoop(ctx, LoopOptions{Template: o, MaxConcurrent: 2, Once: true, ListIssues: loopFixedList(loopOpen(11, 0), loopOpen(12, 1))})
		if err != nil || len(report.Passes) != 1 || report.Failed() {
			t.Fatalf("error %v, report %+v", err, report)
		}
		for _, item := range report.Passes[0].Items {
			if item.State != LoopInterrupted || item.Started {
				t.Fatalf("%+v", item)
			}
		}
		if len(analyses()) != 0 || len(loopRecords(t, o.LabDir)) != 0 {
			t.Fatal("work started after cancellation")
		}
	})
}
