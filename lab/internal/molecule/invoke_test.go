package molecule

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBoundAdapterTimeoutStopsProviderDescendants(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	provider := filepath.Join(dir, "provider")
	script := "#!/bin/sh\n(printf started > started; sleep 2.5; printf late > orphan) &\nwait\n"
	if err = os.WriteFile(provider, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	// Wait for the descendant to exist before expiring the parent deadline.
	// Startup scheduling under a parallel suite is not the behavior under test;
	// process.Run separately tests its wall-clock timeout. This exercises the
	// native adapter's shared process group after a deadline has actually expired.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	done := make(chan process.Result, 1)
	go func() {
		done <- invokeHarness(ctx, []string{exe, "__adapter", "claude-code", provider}, dir, 10*time.Second, map[string]any{"kind": "molecule", "issue": map[string]any{"number": 9, "title": "fixture"}})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(filepath.Join(dir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel(context.Canceled)
			<-done
			t.Fatal("provider never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel(context.DeadlineExceeded)
	r := <-done
	if r.OK || !r.TimedOut {
		t.Fatalf("expected provider deadline: %+v", r)
	}
	time.Sleep(2700 * time.Millisecond)
	if _, err = os.Stat(filepath.Join(dir, "orphan")); !os.IsNotExist(err) {
		t.Fatal("provider descendant survived timeout")
	}
}

func TestNativeBindingRetainsBlockedContractAndTransport(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	provider := filepath.Join(dir, "provider")
	report := `{"schemaVersion":"v1","outcome":"blocked","reason":"cache denied","paths":[]}`
	if err = os.WriteFile(provider, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"structured_output\":"+report+"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result := invokeHarness(context.Background(), []string{exe, "__adapter", "claude-code", provider}, dir, time.Second, map[string]any{"kind": "molecule"})
	if !result.OK || result.Code != 0 || !result.Reconciled || result.Stdout != report || result.RawStdout == "" {
		t.Fatalf("native evidence lost: %+v", result)
	}
}

var codingBase = []string{"Read", "Glob", "Grep", "Edit", "Write", "Bash(git status)", "Bash(git diff)"}

// echoedAllowedTools returns the --allowedTools value a fake claude recorded.
func echoedAllowedTools(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	for i, arg := range args {
		if arg == "--allowedTools" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("no --allowedTools in %q", args)
	return ""
}

func TestNativeBindingPassesAllowlistToClaudeArgv(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	provider := filepath.Join(dir, "provider")
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\ncat >/dev/null\nprintf '%%s' '{\"structured_output\":{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"ok\",\"paths\":[]}}'\n", filepath.Join(dir, "argv.txt"))
	if err = os.WriteFile(provider, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	list := append(append([]string{}, codingBase...), "Bash(go -C lab test ./...)")
	result := invokeHarness(context.Background(), []string{exe, "__adapter", "claude-code", provider}, dir, 10*time.Second, map[string]any{"kind": "molecule", "codingAllowlist": list})
	if !result.OK {
		t.Fatalf("%+v", result)
	}
	if got := echoedAllowedTools(t, filepath.Join(dir, "argv.txt")); got != strings.Join(list, ",") {
		t.Fatalf("provider received %q", got)
	}
}

// claudeAllowlistFixture binds the approved Formula to a fake claude executable
// that echoes its argv outside the checkout, and declares a Go component in lab/.
func claudeAllowlistFixture(t *testing.T) (Options, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	o, repo := fixture(t)
	root := filepath.Dir(repo)
	provider := filepath.Join(root, "claude")
	argv := filepath.Join(root, "argv.txt")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\ncat >/dev/null\nprintf changed > code.txt\nprintf '%%s' '{\"structured_output\":{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"implemented\",\"paths\":[]}}'\n", argv)
	if err = os.WriteFile(provider, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal([]string{filepath.Join(root, "harness")})
	command, _ := json.Marshal([]string{exe, "__adapter", "claude-code", provider})
	recipe := strings.Replace(string(raw), string(old), string(command), 1)
	recipe = strings.Replace(recipe, "binding: custom", "binding: claude-code", 1)
	recipe = strings.Replace(recipe, "\natoms:", "\nrepositoryContext:\n  components:\n    - {path: lab, manifest: lab/go.mod, suggestedChecks: [[go, test, ./...], [go, vet, ./...]]}\natoms:", 1)
	if !strings.Contains(recipe, string(command)) || !strings.Contains(recipe, "lab/go.mod") {
		t.Fatal("fixture Formula was not rewritten")
	}
	if err = os.WriteFile(file, []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
	approveFixture(t, o.LabDir, recipe, "claude-allowlist")
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, e := analyze(ctx, b, i, s)
		a.Checks = append(a.Checks, readiness.Check{ID: "extra", Category: "candidate", Argv: []string{"echo", "ok"}, Dir: ".", TimeoutMS: 5000, Reason: "Optional fixture check", Definitions: a.Checks[0].Definitions, Evidence: a.Checks[0].Evidence})
		return a, e
	}
	return o, argv
}

func TestHarnessAttemptRecordsTheExactAllowlistTheProviderReceived(t *testing.T) {
	o, argv := claudeAllowlistFixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]string{}, codingBase...), "Bash(echo ok)", "Bash(go -C lab test ./...)", "Bash(go -C lab vet ./...)")
	plan := r.Readiness.Plans[0].Plan
	if !reflect.DeepEqual(plan.CodingAllowlist, want) {
		t.Fatalf("approved plan allowlist %q", plan.CodingAllowlist)
	}
	if len(r.HarnessAttempts) != 1 || !reflect.DeepEqual(r.HarnessAttempts[0].CodingAllowlist, want) {
		t.Fatalf("attempt did not record the allowlist: %+v", r.HarnessAttempts)
	}
	if got := echoedAllowedTools(t, argv); got != strings.Join(r.HarnessAttempts[0].CodingAllowlist, ",") {
		t.Fatalf("recorded allowlist differs from provider argv %q", got)
	}
	data, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Record
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.HarnessAttempts[0].CodingAllowlist, want) {
		t.Fatalf("persisted ledger lost the allowlist: %+v", saved.HarnessAttempts[0])
	}
	if r.Status != "waiting" || r.Verification == nil || !r.Verification.RequiredChecksPassed {
		t.Fatalf("%+v", r)
	}
}

func TestLedgerWithoutAllowlistMeansUnrecordedNotEmpty(t *testing.T) {
	raw, err := json.Marshal(HarnessAttempt{ID: "legacy", PlanDigest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "codingAllowlist") {
		t.Fatalf("absent allowlist serialized as a value: %s", raw)
	}
	var restored HarnessAttempt
	if err = json.Unmarshal([]byte(`{"id":"legacy","planDigest":"d","result":{}}`), &restored); err != nil || restored.CodingAllowlist != nil {
		t.Fatalf("historical attempt gained an allowlist: %+v %v", restored, err)
	}
}

func TestApprovedAllowlistMustMatchRederivation(t *testing.T) {
	o, argv := claudeAllowlistFixture(t)
	r, err := Run(context.Background(), o)
	if err != nil || r.Readiness == nil {
		t.Fatalf("%+v %v", r, err)
	}
	old := r.Readiness.Plans[0]
	next := old.Plan
	next.CodingAllowlist = append(append([]string{}, old.Plan.CodingAllowlist...), "Bash(make test)")
	amended, err := Amend(context.Background(), r.LabDir, r.ID, old.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = amended.Readiness.Plans[1].Digest
	if _, err = Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("a plan allowlist that differs from its derivation reached coding: %v", err)
	}
	if _, err = os.Stat(argv); !os.IsNotExist(err) {
		t.Fatal("provider started under an unreviewed allowlist")
	}
	saved, err := findPending(o.LabDir, o.Approve)
	if err != nil || len(saved.HarnessAttempts) != 0 || saved.Readiness.Phase != "scope-waiting" {
		t.Fatalf("refusal consumed the approval: %+v %v", saved.Readiness, err)
	}
}

func TestPlanWithoutStoredAllowlistGetsOnlyTheBaseList(t *testing.T) {
	o, argv := claudeAllowlistFixture(t)
	r, err := Run(context.Background(), o)
	if err != nil || r.Readiness == nil {
		t.Fatalf("%+v %v", r, err)
	}
	old := r.Readiness.Plans[0]
	next := old.Plan
	next.CodingAllowlist = nil
	amended, err := Amend(context.Background(), r.LabDir, r.ID, old.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = amended.Readiness.Plans[1].Digest
	done, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(done.HarnessAttempts) != 1 || !reflect.DeepEqual(done.HarnessAttempts[0].CodingAllowlist, codingBase) {
		t.Fatalf("legacy plan did not fail closed to the base list: %+v", done.HarnessAttempts)
	}
	if got := echoedAllowedTools(t, argv); got != strings.Join(codingBase, ",") {
		t.Fatalf("provider received %q", got)
	}
}
