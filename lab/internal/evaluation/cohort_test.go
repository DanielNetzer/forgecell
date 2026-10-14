package evaluation

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// completedHarness writes the in-scope change and returns a completed coding outcome.
const completedHarness = "#!/bin/sh\ncat >/dev/null\nprintf changed > value.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"done\",\"paths\":[]}'\n"

// failingHarness exits non-zero without a usable outcome: a recorded failure.
const failingHarness = "#!/bin/sh\ncat >/dev/null\nprintf boom >&2\nexit 7\n"

// stuckHarness never finishes, so a small Formula timeout always fires on it.
const stuckHarness = "#!/bin/sh\ncat >/dev/null\nsleep 60\n"

type variantSpec struct {
	name      string
	body      string
	timeoutMS int
}

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }

// cohortFixture builds a git source repo and a reviewed cohort of the given variants,
// tasks and repetitions. The returned options still require an approval digest.
func cohortFixture(t *testing.T, variants []variantSpec, tasks, reps int, primary string, stop *CohortStop) CohortOptions {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repo
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %s %v", args, b, e)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-q")
	git("config", "user.email", "test@example.test")
	git("config", "user.name", "Test")
	git("remote", "add", "origin", "https://github.com/owner/repo.git")
	os.WriteFile(filepath.Join(repo, "value.txt"), []byte("before"), 0600)
	git("add", ".")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")

	cohortDir := filepath.Join(root, "cohort")
	if err := os.Mkdir(cohortDir, 0700); err != nil {
		t.Fatal(err)
	}
	var cv []CohortVariant
	for _, v := range variants {
		script := filepath.Join(cohortDir, v.name+"-harness.sh")
		if err := os.WriteFile(script, []byte(v.body), 0700); err != nil {
			t.Fatal(err)
		}
		command, _ := json.Marshal([]string{script})
		timeout := ""
		if v.timeoutMS > 0 {
			timeout = "  timeoutMs: " + itoa(v.timeoutMS) + "\n"
		}
		yaml := "schemaVersion: v0\nkind: formula\nid: sample\nintake: {source: github-issues, repo: owner/repo}\nharness:\n  binding: custom\n  command: " + string(command) + "\n" + timeout +
			"atoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"
		if err := os.WriteFile(filepath.Join(cohortDir, v.name+".yaml"), []byte(yaml), 0600); err != nil {
			t.Fatal(err)
		}
		cv = append(cv, CohortVariant{Name: v.name, Formula: v.name + ".yaml", FormulaSHA256: Hash([]byte(yaml))})
	}

	var ct []CohortTask
	for i := 0; i < tasks; i++ {
		name := "task-" + string(rune('a'+i))
		number := 100 + i
		issue := []byte(`{"number":` + itoa(number) + `,"repo":"owner/repo","title":"Change value","body":"Set value to changed","url":"https://github.com/owner/repo/issues/` + itoa(number) + `","state":"OPEN"}`)
		acceptance := []byte("#!/bin/sh\ntest \"$(cat value.txt)\" = changed\n")
		os.WriteFile(filepath.Join(cohortDir, name+"-issue.json"), issue, 0600)
		os.WriteFile(filepath.Join(cohortDir, name+"-acceptance.sh"), acceptance, 0600)
		p := Plan{ReadinessVersion: 1, TargetBranch: "main", SchemaVersion: 1, Task: name + "-issue.json", TaskSHA256: Hash(issue), Acceptance: name + "-acceptance.sh", AcceptanceSHA256: Hash(acceptance), BaseCommit: base, AllowedFiles: []string{"value.txt"}, RequireInitialFailure: true, Checks: []Command{{Name: "acceptance", Dir: ".", Argv: []string{"/bin/sh", "{acceptance}"}, TimeoutMS: 5000}}}
		planRaw, _ := json.Marshal(p)
		os.WriteFile(filepath.Join(cohortDir, name+"-plan.json"), planRaw, 0600)
		ct = append(ct, CohortTask{Name: name, Plan: name + "-plan.json", PlanSHA256: Hash(planRaw)})
	}

	cohort := CohortFile{SchemaVersion: 1, Hypothesis: "candidate is at least as good", PrimaryOutcome: primary, Variants: cv, Tasks: ct, Repetitions: reps, Stop: stop}
	cohortRaw, _ := json.Marshal(cohort)
	cohortPath := filepath.Join(cohortDir, "cohort.json")
	os.WriteFile(cohortPath, cohortRaw, 0600)
	return CohortOptions{Cohort: cohortPath, SourceRoot: repo, Output: filepath.Join(root, "results")}
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// approveCohort computes the cohort activation digest the way RunCohort does.
func approveCohort(t *testing.T, o *CohortOptions) {
	t.Helper()
	cohortPath, _ := filepath.Abs(o.Cohort)
	raw, c, err := readCohort(cohortPath)
	if err != nil {
		t.Fatal(err)
	}
	cohortDir := filepath.Dir(cohortPath)
	var variants [2]formula.Formula
	for i, v := range c.Variants {
		b, e := frozen(cohortDir, v.Formula, v.FormulaSHA256)
		if e != nil {
			t.Fatal(e)
		}
		f, e := formula.Parse(b)
		if e != nil {
			t.Fatal(e)
		}
		variants[i] = f
	}
	var planHashes []string
	for _, tk := range c.Tasks {
		planHashes = append(planHashes, tk.PlanSHA256)
	}
	source, _ := filepath.Abs(o.SourceRoot)
	output, _ := filepath.Abs(o.Output)
	o.Approve = CohortDigest(raw, variants, planHashes, source, output)
}

func TestCohortTwoByTwoByTwoReportShape(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 2, 2, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "complete" || len(r.Attempts) != 8 {
		t.Fatalf("status=%s attempts=%d", r.Status, len(r.Attempts))
	}
	// Interleaving: for the first task, A, B, A, B across the two repetitions.
	order := []struct {
		variant string
		rep     int
	}{{"base", 1}, {"cand", 1}, {"base", 2}, {"cand", 2}}
	for i, want := range order {
		a := r.Attempts[i]
		if a.Task != "task-a" || a.Variant != want.variant || a.Repetition != want.rep {
			t.Fatalf("attempt %d = %s/%s/rep-%d", i, a.Task, a.Variant, a.Repetition)
		}
	}
	if len(r.Variants) != 2 {
		t.Fatalf("variants=%d", len(r.Variants))
	}
	for _, v := range r.Variants {
		if v.Attempts != 4 || v.HiddenAcceptancePass != 4 || v.Completions != 4 || v.RequiredChecksPass != 4 {
			t.Fatalf("variant %s: %+v", v.Name, v)
		}
		if v.N.Tasks != 2 || v.N.Repetitions != 2 || v.N.Attempts != 4 {
			t.Fatalf("variant %s missing n: %+v", v.Name, v.N)
		}
		if v.Timeouts != 0 || v.Failures != 0 || v.ScopeViolations != 0 {
			t.Fatalf("variant %s unexpected adverse counts: %+v", v.Name, v)
		}
	}
	if len(r.PerTask) != 2 {
		t.Fatalf("per-task rows=%d", len(r.PerTask))
	}
	for _, to := range r.PerTask {
		if len(to.Variants) != 2 {
			t.Fatalf("task %s cells=%d", to.Task, len(to.Variants))
		}
		for _, cell := range to.Variants {
			if cell.Attempts != 2 || cell.HiddenAcceptancePass != 2 {
				t.Fatalf("task %s cell %s: %+v", to.Task, cell.Variant, cell)
			}
		}
	}
	md, err := os.ReadFile(filepath.Join(o.Output, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"No significance", "Per-task outcomes", "n=4 attempts over 2 task(s)", "Hidden-acceptance pass"} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("report.md missing %q", want)
		}
	}
	// The source checkout is never modified.
	if raw, _ := os.ReadFile(filepath.Join(o.SourceRoot, "value.txt")); string(raw) != "before" {
		t.Fatal("source checkout changed")
	}
}

func TestCohortRecordsFailureAndContinues(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: failingHarness}, {name: "cand", body: completedHarness}}, 1, 1, "requiredChecksPassRate", nil)
	approveCohort(t, &o)
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "complete" || len(r.Attempts) != 2 {
		t.Fatalf("status=%s attempts=%d", r.Status, len(r.Attempts))
	}
	var base, cand VariantSummary
	for _, v := range r.Variants {
		if v.Name == "base" {
			base = v
		} else {
			cand = v
		}
	}
	if base.HiddenAcceptancePass != 0 || base.Failures != 1 {
		t.Fatalf("failing base recorded as success: %+v", base)
	}
	if cand.HiddenAcceptancePass != 1 || cand.Failures != 0 {
		t.Fatalf("candidate did not run after the failure: %+v", cand)
	}
}

func TestCohortRecordsTimeoutAndContinues(t *testing.T) {
	// Variant A times out; variant B runs afterwards, proving the run continues.
	o := cohortFixture(t, []variantSpec{{name: "slow", body: stuckHarness, timeoutMS: 3000}, {name: "fast", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "complete" || len(r.Attempts) != 2 {
		t.Fatalf("status=%s attempts=%d", r.Status, len(r.Attempts))
	}
	var slow, fast VariantSummary
	for _, v := range r.Variants {
		if v.Name == "slow" {
			slow = v
		} else {
			fast = v
		}
	}
	if slow.Timeouts != 1 || slow.HiddenAcceptancePass != 0 {
		t.Fatalf("slow variant timeout not recorded: %+v", slow)
	}
	if fast.HiddenAcceptancePass != 1 {
		t.Fatalf("fast variant did not run after the timeout: %+v", fast)
	}
}

func TestCohortStopsOnMaxAttempts(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 2, 2, "hiddenAcceptancePassRate", &CohortStop{MaxAttempts: ip(3)})
	approveCohort(t, &o)
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "stopped" || len(r.Attempts) != 3 || !strings.Contains(r.Stopped, "maxAttempts") {
		t.Fatalf("stop rule not honoured: status=%s attempts=%d stopped=%q", r.Status, len(r.Attempts), r.Stopped)
	}
}

func TestCohortCostCapNeverTriggersWithoutReportedCost(t *testing.T) {
	// A custom harness reports no cost, so maxCostUSD never triggers.
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 1, "costPerAcceptedOutcome", &CohortStop{MaxCostUSD: fp(0.01)})
	approveCohort(t, &o)
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "complete" || len(r.Attempts) != 2 {
		t.Fatalf("cost cap triggered without reported cost: status=%s attempts=%d", r.Status, len(r.Attempts))
	}
	for _, v := range r.Variants {
		if v.ReportedCostUSD != nil || v.CostCoverage != "0 of 1" {
			t.Fatalf("variant %s reported cost without usage: %+v", v.Name, v)
		}
	}
}

// TestCohortStopRuleOnReportedCost exercises the maxCostUSD and maxAttempts stop logic
// directly with synthesized usage, since a custom harness can never report #149 cost.
func TestCohortStopRuleOnReportedCost(t *testing.T) {
	withCost := func(cost float64) CohortAttempt {
		return CohortAttempt{Attempt: Attempt{Molecule: molecule.Record{HarnessAttempts: []molecule.HarnessAttempt{{Usage: &harness.Usage{TotalCostUSD: fp(cost), CostBasis: "list", EffectiveModels: []string{"claude-opus-4-8"}}}}}}}
	}
	attempts := []CohortAttempt{withCost(0.04), withCost(0.04)}
	if got := reportedCost(attempts[0].Attempt); got == nil || *got != 0.04 {
		t.Fatalf("reportedCost = %v", got)
	}
	if s := cohortStop(&CohortStop{MaxCostUSD: fp(0.10)}, attempts); s != "" {
		t.Fatalf("stopped below the cost cap: %q", s)
	}
	if s := cohortStop(&CohortStop{MaxCostUSD: fp(0.08)}, attempts); s == "" || !strings.Contains(s, "maxCostUSD") {
		t.Fatalf("cost cap did not trigger at the threshold: %q", s)
	}
	// With no reported cost, the cap never triggers regardless of the attempt count.
	none := []CohortAttempt{{}, {}}
	if s := cohortStop(&CohortStop{MaxCostUSD: fp(0.01)}, none); s != "" {
		t.Fatalf("cost cap triggered without reported cost: %q", s)
	}
	if s := cohortStop(&CohortStop{MaxAttempts: ip(2)}, none); !strings.Contains(s, "maxAttempts") {
		t.Fatalf("maxAttempts did not trigger: %q", s)
	}
}

func TestCohortResumesAndSkipsRecordedAttempts(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	first, err := RunCohort(context.Background(), o)
	if err != nil || len(first.Attempts) != 2 {
		t.Fatalf("%+v %v", first, err)
	}
	// Record the candidate attempt's identity, then discard the base attempt directory
	// so only it must be re-run on resume.
	candStart := ""
	for _, a := range first.Attempts {
		if a.Variant == "cand" {
			candStart = a.StartedAt
		}
	}
	baseDir := filepath.Join(o.Output, "attempts", "task-a", "base", "rep-1")
	if err := os.RemoveAll(baseDir); err != nil {
		t.Fatal(err)
	}
	second, err := RunCohort(context.Background(), o)
	if err != nil || second.Status != "complete" || len(second.Attempts) != 2 {
		t.Fatalf("resume failed: %+v %v", second, err)
	}
	var baseStart2, candStart2 string
	for _, a := range second.Attempts {
		if a.Variant == "base" {
			baseStart2 = a.StartedAt
		} else {
			candStart2 = a.StartedAt
		}
	}
	if candStart2 != candStart {
		t.Fatal("resume re-ran an already recorded attempt")
	}
	if baseStart2 == "" {
		t.Fatal("resume did not re-run the discarded attempt")
	}
}

func TestCohortRefusesMismatchedDigest(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	// Pre-create the output with a different recorded activation digest.
	abs, _ := filepath.Abs(o.Output)
	if err := os.MkdirAll(abs, 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(abs, "report.json"), []byte(`{"schemaVersion":1,"activationDigest":"`+strings.Repeat("a", 64)+`"}`), 0600)
	if _, err := RunCohort(context.Background(), o); err == nil || !strings.Contains(err.Error(), "different activation digest") {
		t.Fatalf("mismatched digest accepted: %v", err)
	}
}

func TestCohortRequiresApproval(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	if _, err := RunCohort(context.Background(), o); err == nil || !strings.Contains(err.Error(), "fresh exact cohort evaluation approval required") {
		t.Fatalf("executed without approval: %v", err)
	}
	// Nothing is created before approval.
	if _, err := os.Stat(o.Output); !os.IsNotExist(err) {
		t.Fatal("output created before approval")
	}
}

func TestCohortRejectsInvalidFile(t *testing.T) {
	cases := map[string]func(c *CohortFile){
		"schema":      func(c *CohortFile) { c.SchemaVersion = 2 },
		"outcome":     func(c *CohortFile) { c.PrimaryOutcome = "nope" },
		"variants":    func(c *CohortFile) { c.Variants = c.Variants[:1] },
		"reps":        func(c *CohortFile) { c.Repetitions = 0 },
		"tasks":       func(c *CohortFile) { c.Tasks = nil },
		"variantName": func(c *CohortFile) { c.Variants[0].Name = "Bad_Name" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := CohortFile{SchemaVersion: 1, PrimaryOutcome: "hiddenAcceptancePassRate", Repetitions: 1,
				Variants: []CohortVariant{{Name: "base", Formula: "a.yaml", FormulaSHA256: strings.Repeat("a", 64)}, {Name: "cand", Formula: "b.yaml", FormulaSHA256: strings.Repeat("b", 64)}},
				Tasks:    []CohortTask{{Name: "task-a", Plan: "p.json", PlanSHA256: strings.Repeat("c", 64)}}}
			mutate(&c)
			if err := validateCohort(c); err == nil {
				t.Fatalf("invalid cohort accepted for %s", name)
			}
		})
	}
}

func TestCohortRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cohort.json")
	os.WriteFile(path, []byte(`{"schemaVersion":1,"surprise":true}`), 0600)
	if _, _, err := readCohort(path); err == nil {
		t.Fatal("unknown field accepted")
	}
}
