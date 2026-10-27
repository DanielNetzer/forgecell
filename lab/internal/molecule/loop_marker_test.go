package molecule

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func markerSelector(t *testing.T, s string) intake.Selector {
	t.Helper()
	sel, err := intake.ParseSelector(s)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func heldMarker(t *testing.T, lab, starter string, watch time.Duration) *LoopMarker {
	t.Helper()
	m := NewLoopMarker(starter, watch)
	if err := m.Acquire(lab, markerSelector(t, "milestone:v1"), 3); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Release)
	return m
}

// Serial: helper-process entry point that reads its environment.
func TestLoopMarkerHolderHelper(t *testing.T) {
	lab := os.Getenv("LOOP_MARKER_LAB")
	if lab == "" {
		t.Skip("helper process for TestLoopMarkerIsVisibleFromAnotherProcessAndStaleOnceItsHolderIsKilled")
	}
	m := NewLoopMarker(LoopStarterCLI, 2*time.Minute)
	if err := m.Acquire(lab, intake.Selector{Kind: "milestone", Value: "v1"}, 2); err != nil {
		fmt.Println("error:", err)
		os.Exit(3)
	}
	m.Track("mol-7-helper")
	fmt.Println("held")
	time.Sleep(time.Minute)
}

// Serial: launches TestLoopMarkerHolderHelper as a helper process.
func TestLoopMarkerIsVisibleFromAnotherProcessAndStaleOnceItsHolderIsKilled(t *testing.T) {
	lab := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLoopMarkerHolderHelper$")
	cmd.Env = append(os.Environ(), "LOOP_MARKER_LAB="+lab)
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
	if err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("the helper did not take the marker: %q %v", line, err)
	}
	got := ReadLoopStatus(lab)
	if !got.Running || got.State != LoopMarkerRunning || !got.Held || got.Status == nil {
		t.Fatalf("a running loop in another process is not visible: %+v", got)
	}
	s := got.Status
	if s.Starter != LoopStarterCLI || s.Selector != "milestone:v1" || s.MaxConcurrent != 2 || s.WatchIntervalSeconds != 120 || s.StartedAt.IsZero() || len(s.InFlight) != 1 || s.InFlight[0] != "mol-7-helper" {
		t.Fatalf("status %+v", s)
	}
	err = NewLoopMarker(LoopStarterUI, 0).Acquire(lab, intake.Selector{Kind: intake.SelectOldest}, 1)
	if !errors.Is(err, ErrLoopRunning) || !strings.Contains(err.Error(), "terminal") || strings.Contains(err.Error(), lab) {
		t.Fatalf("a second loop was not refused with a clear reason: %v", err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if got = ReadLoopStatus(lab); got.Running || got.Held || got.State != LoopMarkerStale || got.Status != nil {
		t.Fatalf("a killed loop is still reported: %+v", got)
	}
	m := NewLoopMarker(LoopStarterUI, 0)
	if err = m.Acquire(lab, intake.Selector{Kind: intake.SelectOldest}, 1); err != nil {
		t.Fatalf("the marker outlived its killed holder: %v", err)
	}
	if got = ReadLoopStatus(lab); !got.Running || got.Status.Starter != LoopStarterUI || got.Status.Token != m.Token() {
		t.Fatalf("the new loop's status replaced the stale one: %+v", got)
	}
	m.Release()
	if got = ReadLoopStatus(lab); got.State != LoopMarkerNotRunning {
		t.Fatalf("a released marker left %+v", got)
	}
	if _, err = os.Lstat(filepath.Join(lab, loopStatusFile)); !os.IsNotExist(err) {
		t.Fatalf("the loop's own status was not removed: %v", err)
	}
}

func TestLoopMarkerRefusesASecondRunLoopBeforeListing(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	analyses := countAnalysis(&o)
	held := heldMarker(t, o.LabDir, LoopStarterUI, 0)
	listed := 0
	list := func(context.Context, string) (intake.OpenIssues, error) {
		listed++
		return intake.OpenIssues{Issues: []intake.OpenIssue{loopOpen(12, 0)}}, nil
	}
	for name, marker := range map[string]*LoopMarker{"without a marker": nil, "with its own marker": NewLoopMarker(LoopStarterCLI, 0)} {
		_, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: list, Marker: marker})
		if !errors.Is(err, ErrLoopRunning) || !strings.Contains(err.Error(), "Lab UI") || IsTransientLoopError(err) {
			t.Fatalf("%s: a second loop was not refused: %v", name, err)
		}
		if marker.Token() != "" {
			t.Fatalf("%s: a refused loop holds a marker", name)
		}
	}
	if listed != 0 || len(analyses()) != 0 || len(loopRecords(t, o.LabDir)) != 0 {
		t.Fatalf("a refused loop listed %d times, analysed %v or wrote a ledger", listed, analyses())
	}
	// The holder's own loop runs, records its pass and keeps the marker.
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: list, Marker: held})
	if err != nil || listed != 1 || !loopItem(t, report.Passes[0], 12).Started {
		t.Fatalf("the holder's loop: %v, listed %d", err, listed)
	}
	got := ReadLoopStatus(o.LabDir)
	if !got.Running || got.Status.Passes != 1 || got.Status.LastPassAt.IsZero() || len(got.Status.InFlight) != 0 || got.Status.Token != held.Token() {
		t.Fatalf("status after a pass %+v", got)
	}
	held.Release()
	if got = ReadLoopStatus(o.LabDir); got.Running || got.State != LoopMarkerNotRunning {
		t.Fatalf("after release %+v", got)
	}
}

func TestLoopStatusCarriesNoPathsOrEnvAndIsBounded(t *testing.T) {
	lab := t.TempDir()
	t.Setenv("FORGECELL_SECRET_VALUE", "sk-test-secret-value")
	m := NewLoopMarker(LoopStarterCLI, 5*time.Minute)
	long := "label:" + strings.Repeat("x", 300) + "\x1b[31m"
	if err := m.Acquire(lab, intake.Selector{Kind: "label", Value: long[len("label:"):]}, 4); err != nil {
		t.Fatal(err)
	}
	defer m.Release()
	for i := range MaxLoopStatusInFlight + 10 {
		m.Track(fmt.Sprintf("mol-%d-a", i))
	}
	m.Track("../escape")
	raw, err := os.ReadFile(filepath.Join(lab, loopStatusFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{lab, os.Getenv("HOME"), os.Getenv("PATH"), "sk-test-secret-value", "\x1b", "escape"} {
		if leak != "" && strings.Contains(string(raw), leak) {
			t.Fatalf("the status carries %q: %s", leak, raw)
		}
	}
	var keys map[string]any
	if err = json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for key := range keys {
		switch key {
		case "schemaVersion", "token", "starter", "startedAt", "selector", "selectorTruncated", "maxConcurrent", "watchIntervalSeconds", "passes", "lastPassAt", "inFlight", "inFlightTruncated", "intake":
		default:
			t.Fatalf("unexpected status field %q", key)
		}
	}
	got := ReadLoopStatus(lab)
	if !got.Running || !got.Status.SelectorTruncated || len([]rune(got.Status.Selector)) > maxLoopSelectorRunes || len(got.Status.InFlight) != MaxLoopStatusInFlight || !got.Status.InFlightTruncated || got.Status.WatchIntervalSeconds != 300 {
		t.Fatalf("status not bounded: %+v", got.Status)
	}
	served, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(served), got.Status.Token) || strings.Contains(string(served), lab) {
		t.Fatalf("the read model serves the token or a path: %s", served)
	}
	for i := range MaxLoopStatusInFlight + 10 {
		m.Untrack(fmt.Sprintf("mol-%d-a", i))
	}
	if got = ReadLoopStatus(lab); len(got.Status.InFlight) != 0 || got.Status.InFlightTruncated {
		t.Fatalf("untracked Molecules remain: %+v", got.Status)
	}
}

func TestLoopStatusFailsSafeOnHostileFiles(t *testing.T) {
	lab := t.TempDir()
	status := filepath.Join(lab, loopStatusFile)
	// Nothing at all: not running, and the read creates nothing.
	if got := ReadLoopStatus(lab); got.State != LoopMarkerNotRunning || got.Running || got.Held {
		t.Fatalf("empty Lab %+v", got)
	}
	if _, err := os.Lstat(filepath.Join(lab, "ledgers")); !os.IsNotExist(err) {
		t.Fatal("ReadLoopStatus created the ledgers directory")
	}
	// A status file whose lock is not held is stale, however convincing.
	m := NewLoopMarker(LoopStarterCLI, 0)
	if err := m.Acquire(lab, intake.Selector{Kind: intake.SelectOldest}, 1); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(status)
	if err != nil {
		t.Fatal(err)
	}
	token := m.Token()
	m.Release()
	if err = os.WriteFile(status, good, 0600); err != nil {
		t.Fatal(err)
	}
	if got := ReadLoopStatus(lab); got.Running || got.State != LoopMarkerStale {
		t.Fatalf("an unheld status was reported %+v", got)
	}
	// While a loop holds the marker, a status it cannot verify is never running.
	held := heldMarker(t, lab, LoopStarterCLI, 0)
	foreign := strings.Replace(string(good), token, strings.Repeat("a", 32), 1)
	elsewhere := filepath.Join(t.TempDir(), "status.json")
	if err = os.WriteFile(elsewhere, good, 0600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() error{
		"another loop's token": func() error { return os.WriteFile(status, []byte(foreign), 0600) },
		"malformed":            func() error { return os.WriteFile(status, []byte(`{"schemaVersion":`), 0600) },
		"unknown field": func() error {
			return os.WriteFile(status, []byte(strings.Replace(string(good), `{`, `{"path":"/Users/x",`, 1)), 0600)
		},
		"trailing data": func() error { return os.WriteFile(status, append(append([]byte{}, good...), []byte(`{}`)...), 0600) },
		"oversized":     func() error { return os.WriteFile(status, []byte(strings.Repeat(" ", maxLoopStatusBytes+1)), 0600) },
		"bad in-flight id": func() error {
			return os.WriteFile(status, []byte(strings.Replace(string(good), `"inFlight":[]`, `"inFlight":["../x"]`, 1)), 0600)
		},
		"symlink":   func() error { return os.Symlink(elsewhere, status) },
		"directory": func() error { return os.Mkdir(status, 0700) },
		"missing":   func() error { return nil },
	}
	for name, setup := range cases {
		os.RemoveAll(status)
		if err = setup(); err != nil {
			t.Fatal(name, err)
		}
		if got := ReadLoopStatus(lab); got.Running || got.Status != nil || got.State != LoopMarkerUnreadable || !got.Held {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	os.RemoveAll(status)
	// A second loop is still refused: the lock alone decides.
	if err = NewLoopMarker(LoopStarterUI, 0).Acquire(lab, intake.Selector{Kind: intake.SelectOldest}, 1); !errors.Is(err, ErrLoopRunning) {
		t.Fatalf("a held marker with an unreadable status was granted: %v", err)
	}
	held.Release()
	// A symlinked lock file is refused, never followed.
	lock := filepath.Join(lab, "ledgers", loopMarkerFile)
	os.Remove(lock)
	if err = os.Symlink(elsewhere, lock); err != nil {
		t.Fatal(err)
	}
	if got := ReadLoopStatus(lab); got.Running || got.State != LoopMarkerUnreadable {
		t.Fatalf("symlinked lock %+v", got)
	}
	if err = NewLoopMarker(LoopStarterUI, 0).Acquire(lab, intake.Selector{Kind: intake.SelectOldest}, 1); err == nil || strings.Contains(err.Error(), lab) {
		t.Fatalf("a symlinked lock was taken or its error names the Lab: %v", err)
	}
	// A missing Lab is never created.
	missing := filepath.Join(t.TempDir(), "absent")
	if err = NewLoopMarker(LoopStarterCLI, 0).Acquire(missing, intake.Selector{Kind: intake.SelectOldest}, 1); err == nil {
		t.Fatal("a marker was taken in a missing Lab")
	}
	if _, err = os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatal("Acquire created the Lab directory")
	}
}

// gatedAnalysis holds the first analysis at a loopGate.
func gatedAnalysis(o *Options) *loopGate {
	gate := newLoopGate()
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		if err := gate.hold(ctx); err != nil {
			return readiness.Analysis{}, err
		}
		return analyze(ctx, b, i, s)
	}
	return gate
}

// While Run is held in its analysis the loop status lists the Issue in flight by
// number; once Run names the Molecule, the Molecule replaces it.
func TestLoopStatusListsIntakeInFlightThenItsMolecule(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	entered, proceed := filepath.Join(dir, "entered"), filepath.Join(dir, "proceed")
	for _, p := range []string{entered, proceed} {
		if err := syscall.Mkfifo(p, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The coding harness, which runs after the policy approval, signals and waits.
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "echo coding > '"+entered+"'\nread go < '"+proceed+"'\n")
	gate := gatedAnalysis(&o)
	marker := NewLoopMarker(LoopStarterCLI, 0)
	defer marker.Release()
	first := loopBackground(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: loopFixedList(loopOpen(12, 0)), Marker: marker})
	gate.waitEntered(t)
	got := ReadLoopStatus(o.LabDir)
	if !got.Running || len(got.Status.Intake) != 1 || got.Status.Intake[0] != (LoopIntake{Issue: 12, Phase: LoopIntakePlanning}) || len(got.Status.InFlight) != 0 {
		t.Fatalf("intake in flight is not listed by Issue: %+v", got.Status)
	}
	gate.open()
	if raw, err := os.ReadFile(entered); err != nil || strings.TrimSpace(string(raw)) != "coding" {
		t.Fatalf("the harness did not start: %q %v", raw, err)
	}
	records := loopRecords(t, o.LabDir)[12]
	if len(records) != 1 {
		t.Fatalf("Molecules %d", len(records))
	}
	got = ReadLoopStatus(o.LabDir)
	if !got.Running || len(got.Status.Intake) != 0 || len(got.Status.InFlight) != 1 || got.Status.InFlight[0] != records[0].ID {
		t.Fatalf("the Molecule did not replace its Issue: %+v", got.Status)
	}
	if err := os.WriteFile(proceed, []byte("go\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if item := loopItem(t, loopWait(t, first).Passes[0], 12); !item.Started || !item.Approved || item.Failed {
		t.Fatalf("%+v", item)
	}
	if got = ReadLoopStatus(o.LabDir); !got.Running || len(got.Status.Intake) != 0 || len(got.Status.InFlight) != 0 {
		t.Fatalf("finished work is still listed: %+v", got.Status)
	}
}

// A panic in intake, recovered by the caller, never leaves the Issue listed.
func TestLoopStatusIntakeIsUntrackedWhenRunPanics(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	o.Analyze = func(context.Context, formula.Binding, Issue, readiness.RepositorySnapshot) (readiness.Analysis, error) {
		if got := ReadLoopStatus(o.LabDir); !got.Running || len(got.Status.Intake) != 1 {
			t.Errorf("intake was not listed: %+v", got)
		}
		panic("analysis panicked")
	}
	marker := heldMarker(t, o.LabDir, LoopStarterCLI, 0)
	loaded, err := loadLoopFormula(o.LabDir, o.FormulaID)
	if err != nil {
		t.Fatal(err)
	}
	l := &loopRun{work: context.Background(), o: LoopOptions{Template: o}, lab: o.LabDir, repo: loaded.Formula.Intake.Repo, loaded: loaded, marker: marker}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the analysis did not panic")
			}
		}()
		l.start(context.Background(), LoopItem{Repository: "acme/widgets", Issue: 12})
	}()
	if got := ReadLoopStatus(o.LabDir); !got.Running || len(got.Status.Intake) != 0 || len(got.Status.InFlight) != 0 {
		t.Fatalf("a panicked intake left an entry: %+v", got.Status)
	}
}

func TestLoopStatusIntakeIsBoundedAndValidated(t *testing.T) {
	lab := t.TempDir()
	m := heldMarker(t, lab, LoopStarterCLI, 0)
	m.TrackIntake(0)
	m.TrackIntake(-4)
	for i := range MaxLoopStatusInFlight + 3 {
		m.TrackIntake(int64(i + 1))
	}
	got := ReadLoopStatus(lab)
	if !got.Running || len(got.Status.Intake) != MaxLoopStatusInFlight || !got.Status.InFlightTruncated || got.Status.Intake[0].Issue != 1 {
		t.Fatalf("intake not bounded: %+v", got.Status)
	}
	for i := range MaxLoopStatusInFlight + 3 {
		m.UntrackIntake(int64(i + 1))
	}
	if got = ReadLoopStatus(lab); len(got.Status.Intake) != 0 || got.Status.InFlightTruncated {
		t.Fatalf("untracked intakes remain: %+v", got.Status)
	}
	// A status naming an intake in another phase, or an invalid Issue, is unreadable.
	m.TrackIntake(5)
	status := filepath.Join(lab, loopStatusFile)
	good, err := os.ReadFile(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"phase":"coding"`, `"phase":"planning","x":1`} {
		if err = os.WriteFile(status, []byte(strings.Replace(string(good), `"phase":"planning"`, bad, 1)), 0600); err != nil {
			t.Fatal(err)
		}
		if got = ReadLoopStatus(lab); got.Running || got.State != LoopMarkerUnreadable {
			t.Fatalf("%s: %+v", bad, got)
		}
	}
	if err = os.WriteFile(status, []byte(strings.Replace(string(good), `"issue":5`, `"issue":0`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if got = ReadLoopStatus(lab); got.Running {
		t.Fatalf("an invalid Issue was served: %+v", got)
	}
}

func TestIssueLockConcurrentRunThenLoopYieldOneMolecule(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	analyses := countAnalysis(&o)
	gate := gatedAnalysis(&o)
	run, ctx := make(chan error, 1), loopBounded(t)
	go func() {
		ro := o
		ro.Issue = "12"
		_, err := RunIntake(ctx, ro)
		run <- err
	}()
	gate.waitEntered(t)
	report, err := RunLoop(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: loopFixedList(loopOpen(12, 0))})
	if err != nil {
		t.Fatal(err)
	}
	if item := loopItem(t, report.Passes[0], 12); item.Started || item.Approved || item.Failed {
		t.Fatalf("the loop started a Molecule beside a run: %+v", item)
	} else if item.State == LoopLocked && !strings.Contains(item.Reason, "forgecell run") {
		t.Fatalf("the loop's report does not name the run: %+v", item)
	}
	gate.open()
	if err = <-run; err != nil {
		t.Fatal(err)
	}
	if got := analyses(); len(got) != 1 || len(loopRecords(t, o.LabDir)[12]) != 1 {
		t.Fatalf("analyses %v, Molecules %d", got, len(loopRecords(t, o.LabDir)[12]))
	}
}

func TestIssueLockConcurrentLoopThenRunIsRefusedNamingTheLoop(t *testing.T) {
	t.Parallel()
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	analyses := countAnalysis(&o)
	gate := gatedAnalysis(&o)
	first := loopBackground(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: loopFixedList(loopOpen(12, 0))})
	gate.waitEntered(t)
	ro := o
	ro.Issue = "12"
	_, err := RunIntake(loopBounded(t), ro)
	if !errors.Is(err, ErrIssueBusy) || !strings.Contains(err.Error(), "running loop") || !strings.Contains(err.Error(), "Issue #12") || strings.Contains(err.Error(), o.LabDir) {
		t.Fatalf("the run was not refused with a plain reason naming the loop: %v", err)
	}
	gate.open()
	if item := loopItem(t, loopWait(t, first).Passes[0], 12); !item.Started || item.Failed {
		t.Fatalf("%+v", item)
	}
	if got := analyses(); len(got) != 1 || len(loopRecords(t, o.LabDir)[12]) != 1 {
		t.Fatalf("analyses %v, Molecules %d", got, len(loopRecords(t, o.LabDir)[12]))
	}
	// Once the loop's intake is done the lock is free; RunIntake refuses nothing
	// it should not and runs the same checks as Run.
	if _, err = RunIntake(context.Background(), Options{LabDir: t.TempDir(), Issue: "12"}); err == nil || errors.Is(err, ErrIssueBusy) || !strings.Contains(err.Error(), "approve a Formula") {
		t.Fatalf("an unapproved Lab: %v", err)
	}
}
