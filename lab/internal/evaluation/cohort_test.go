package evaluation

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
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
		// The fixture plan has only the acceptance check, so required checks are n/a.
		if v.Attempts != 4 || v.HiddenAcceptancePass != 4 || v.Completions != 4 {
			t.Fatalf("variant %s: %+v", v.Name, v)
		}
		if v.RequiredChecksPass != nil || v.RequiredChecksN != 0 {
			t.Fatalf("variant %s required checks should be n/a: %+v", v.Name, v)
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

// TestCohortRejectsIdenticalFormula rejects a cohort that pins the same Formula bytes for
// both variants before anything is frozen or executed (item 6).
func TestCohortRejectsIdenticalFormula(t *testing.T) {
	c := CohortFile{SchemaVersion: 1, PrimaryOutcome: "hiddenAcceptancePassRate", Repetitions: 1,
		Variants: []CohortVariant{{Name: "base", Formula: "a.yaml", FormulaSHA256: strings.Repeat("a", 64)}, {Name: "cand", Formula: "b.yaml", FormulaSHA256: strings.Repeat("a", 64)}},
		Tasks:    []CohortTask{{Name: "task-a", Plan: "p.json", PlanSHA256: strings.Repeat("c", 64)}}}
	if err := validateCohort(c); err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("identical-Formula cohort accepted: %v", err)
	}
}

// TestCohortDigestSensitivity proves the activation digest changes when the cohort bytes,
// either Formula, any plan hash, the source or the output change (item 6).
func TestCohortDigestSensitivity(t *testing.T) {
	cohort := []byte(`{"schemaVersion":1}`)
	var v [2]formula.Formula
	v[0] = formula.Formula{SHA256: strings.Repeat("a", 64)}
	v[1] = formula.Formula{SHA256: strings.Repeat("b", 64)}
	planHashes := []string{strings.Repeat("c", 64)}
	base := CohortDigest(cohort, v, planHashes, "/src", "/out")
	cases := map[string]string{
		"cohort":  CohortDigest([]byte(`{"schemaVersion":2}`), v, planHashes, "/src", "/out"),
		"formula": CohortDigest(cohort, [2]formula.Formula{{SHA256: strings.Repeat("d", 64)}, v[1]}, planHashes, "/src", "/out"),
		"plan":    CohortDigest(cohort, v, []string{strings.Repeat("e", 64)}, "/src", "/out"),
		"source":  CohortDigest(cohort, v, planHashes, "/other", "/out"),
		"out":     CohortDigest(cohort, v, planHashes, "/src", "/other"),
	}
	for name, got := range cases {
		if got == base {
			t.Fatalf("changing %s did not change the digest", name)
		}
	}
}

// TestCohortMedianAndP90 covers the median (even/odd) and nearest-rank p90 helpers that
// feed the latency summaries (item 6).
func TestCohortMedianAndP90(t *testing.T) {
	if median(nil) != 0 || p90(nil) != 0 {
		t.Fatal("empty samples should be zero")
	}
	even := []int64{40, 10, 30, 20}
	if got := median(even); got != 25 {
		t.Fatalf("even median = %d, want 25", got)
	}
	if got := p90(even); got != 40 { // ceil(0.9*4)=4 -> rank index 3
		t.Fatalf("p90 = %d, want 40", got)
	}
	odd := []int64{30, 10, 20}
	if got := median(odd); got != 20 {
		t.Fatalf("odd median = %d, want 20", got)
	}
	if got := p90([]int64{5}); got != 5 {
		t.Fatalf("single p90 = %d, want 5", got)
	}
}

// completedAttempt is a minimal Attempt whose harness Atom finished, for grading helpers.
func completedAttempt(checks []Observation) Attempt {
	return Attempt{Molecule: molecule.Record{Atoms: []molecule.Atom{{Type: "harness", Status: "done"}}}, Checks: checks}
}

// TestRequiredChecksTriState covers the three required-checks states: n/a when the plan
// has no non-acceptance check, a failure when a required check was not observed passing,
// and a pass when every required check was observed passing (item 2).
func TestRequiredChecksTriState(t *testing.T) {
	acceptanceOnly := Plan{Checks: []Command{{Name: "acceptance"}}}
	if requiredChecksTriState(acceptanceOnly, completedAttempt(nil)) != nil {
		t.Fatal("acceptance-only plan must grade required checks as n/a")
	}
	withBuild := Plan{Checks: []Command{{Name: "acceptance"}, {Name: "build"}}}
	// Verification never ran: no observation for build, so it is a failure, not n/a.
	if rc := requiredChecksTriState(withBuild, Attempt{}); rc == nil || *rc {
		t.Fatalf("unobserved required check should be a failure: %v", rc)
	}
	// Harness done but build observed failing.
	failing := completedAttempt([]Observation{{Command: Command{Name: "build"}, Result: process.Result{OK: false}}})
	if rc := requiredChecksTriState(withBuild, failing); rc == nil || *rc {
		t.Fatalf("failing required check should be a failure: %v", rc)
	}
	// Harness done and build observed passing.
	passing := completedAttempt([]Observation{{Command: Command{Name: "build"}, Result: process.Result{OK: true}}})
	if rc := requiredChecksTriState(withBuild, passing); rc == nil || !*rc {
		t.Fatalf("observed passing required check should pass: %v", rc)
	}
}

// TestCohortRequiredChecksAggregation checks that n/a attempts are excluded from the
// required-checks n and the pass count aggregates only measurable attempts (item 2).
func TestCohortRequiredChecksAggregation(t *testing.T) {
	tr, fa := true, false
	mk := func(rc *bool) CohortAttempt {
		return CohortAttempt{Task: "task-a", Variant: "base", Attempt: Attempt{RequiredChecks: rc}}
	}
	attempts := []CohortAttempt{mk(&tr), mk(&fa), mk(nil)}
	base := CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 3, PrimaryOutcome: "requiredChecksPassRate"}
	r := buildCohortReport(base, []string{"base", "cand"}, map[string]string{"base": "x", "cand": "y"}, []string{"task-a"}, attempts)
	var v VariantSummary
	for _, s := range r.Variants {
		if s.Name == "base" {
			v = s
		}
	}
	if v.RequiredChecksN != 2 || v.RequiredChecksPass == nil || *v.RequiredChecksPass != 1 {
		t.Fatalf("aggregate excluded n/a wrongly: n=%d pass=%v", v.RequiredChecksN, v.RequiredChecksPass)
	}
	// The per-task cell carries the same measurable n.
	cell := r.PerTask[0].Variants[0]
	if cell.RequiredChecksN != 2 || cell.RequiredChecksPass == nil || *cell.RequiredChecksPass != 1 {
		t.Fatalf("cell aggregate wrong: n=%d pass=%v", cell.RequiredChecksN, cell.RequiredChecksPass)
	}
}

// TestCohortRequiredChecksNA runs a real cohort whose plan has only the acceptance check,
// so required checks are n/a everywhere, the primary outcome reads as not measurable, and
// the markdown renders n/a (item 2).
func TestCohortRequiredChecksNA(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 1, "requiredChecksPassRate", nil)
	approveCohort(t, &o)
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range r.Variants {
		if v.RequiredChecksPass != nil || v.RequiredChecksN != 0 {
			t.Fatalf("variant %s not n/a: %+v", v.Name, v)
		}
	}
	for _, to := range r.PerTask {
		for _, cell := range to.Variants {
			if cell.RequiredChecksPass != nil || cell.RequiredChecksN != 0 {
				t.Fatalf("cell %s/%s not n/a: %+v", to.Task, cell.Variant, cell)
			}
		}
	}
	note := false
	for _, n := range r.Notes {
		if strings.Contains(n, "not measurable") {
			note = true
		}
	}
	if !note {
		t.Fatalf("missing not-measurable note: %v", r.Notes)
	}
	md, _ := os.ReadFile(filepath.Join(o.Output, "report.md"))
	if !strings.Contains(string(md), "Required-checks pass: n/a") {
		t.Fatalf("report.md did not render n/a required checks:\n%s", md)
	}
}

// TestCohortCostPerAcceptedPartialCoverage proves cost per accepted outcome divides only
// by accepted attempts among the cost-reporting ones and prints both coverages (item 3).
func TestCohortCostPerAcceptedPartialCoverage(t *testing.T) {
	mk := func(cost *float64, correct bool) CohortAttempt {
		var ha []molecule.HarnessAttempt
		if cost != nil {
			ha = []molecule.HarnessAttempt{{Usage: &harness.Usage{TotalCostUSD: cost, CostBasis: "list"}}}
		}
		return CohortAttempt{Task: "task-a", Variant: "base", Attempt: Attempt{Correct: correct, Molecule: molecule.Record{HarnessAttempts: ha}}}
	}
	attempts := []CohortAttempt{mk(fp(0.02), true), mk(fp(0.04), false), mk(nil, true)}
	base := CohortReport{SchemaVersion: 1, Tasks: 1, Repetitions: 3, PrimaryOutcome: "costPerAcceptedOutcome"}
	r := buildCohortReport(base, []string{"base", "cand"}, map[string]string{"base": "x", "cand": "y"}, []string{"task-a"}, attempts)
	var v VariantSummary
	for _, s := range r.Variants {
		if s.Name == "base" {
			v = s
		}
	}
	if v.ReportedCostUSD == nil || *v.ReportedCostUSD != 0.06 {
		t.Fatalf("reported cost total = %v, want 0.06", v.ReportedCostUSD)
	}
	if v.CostCoverage != "2 of 3" {
		t.Fatalf("cost coverage = %q, want 2 of 3", v.CostCoverage)
	}
	// Two attempts reported cost; one of them was accepted, so divide 0.06 by 1.
	if v.CostPerAcceptedUSD == nil || *v.CostPerAcceptedUSD != 0.06 {
		t.Fatalf("cost per accepted = %v, want 0.06", v.CostPerAcceptedUSD)
	}
	if v.AcceptedCostCoverage != "1 of 2" {
		t.Fatalf("accepted-among-reporting coverage = %q, want 1 of 2", v.AcceptedCostCoverage)
	}
}

// TestCohortDiscardsCancelledAttempt proves a cancelled attempt is not persisted, so
// resume re-runs that coordinate, and the run is recorded as aborted (items 1 and 6).
func TestCohortDiscardsCancelledAttempt(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: stuckHarness, timeoutMS: 30000}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	r, err := RunCohort(ctx, o)
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if r.Status != "aborted" {
		t.Fatalf("status = %q, want aborted", r.Status)
	}
	rec := filepath.Join(o.Output, "attempts", "task-a", "base", "rep-1", "attempt.json")
	if _, e := os.Stat(rec); !os.IsNotExist(e) {
		t.Fatalf("cancelled attempt was persisted: %v", e)
	}
	if _, done := loadAttemptRecord(rec, "task-a", "base", 1); done {
		t.Fatal("cancelled coordinate counts as done; resume would skip it")
	}
}

// TestCohortResumeRefusesTamperedInputs proves resume re-hashes <out>/inputs before each
// executed attempt and refuses a mutated copy, writing status: aborted (item 4).
func TestCohortResumeRefusesTamperedInputs(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	if _, err := RunCohort(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	// Discard the base attempt so resume must re-run it, then tamper its copied input.
	if err := os.RemoveAll(filepath.Join(o.Output, "attempts", "task-a", "base", "rep-1")); err != nil {
		t.Fatal(err)
	}
	acc := filepath.Join(o.Output, "inputs", "tasks", "task-a", "acceptance.sh")
	if err := os.Chmod(acc, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(acc, []byte("#!/bin/sh\nexit 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := RunCohort(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tampered input accepted: %v", err)
	}
	if r.Status != "aborted" {
		t.Fatalf("status = %q, want aborted", r.Status)
	}
}

// TestCohortDeletesWorkspacesByDefault proves each attempt's Lab and verification trees
// are deleted after evidence is saved, while the record, changes.diff and changes.json
// are retained and the diff's sha256 is recorded (item 5).
func TestCohortDeletesWorkspacesByDefault(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	if _, err := RunCohort(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	attemptDir := filepath.Join(o.Output, "attempts", "task-a", "base", "rep-1")
	for _, gone := range []string{"lab", "verification"} {
		if _, e := os.Stat(filepath.Join(attemptDir, gone)); !os.IsNotExist(e) {
			t.Fatalf("%s tree not deleted: %v", gone, e)
		}
	}
	for _, kept := range []string{"attempt.json", "changes.diff", "changes.json"} {
		if _, e := os.Stat(filepath.Join(attemptDir, kept)); e != nil {
			t.Fatalf("retained %s missing: %v", kept, e)
		}
	}
	diff, err := os.ReadFile(filepath.Join(attemptDir, "changes.diff"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diff), "value.txt") {
		t.Fatalf("changes.diff missing the changed path:\n%s", diff)
	}
	var ca CohortAttempt
	recRaw, err := os.ReadFile(filepath.Join(attemptDir, "attempt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(recRaw, &ca); err != nil {
		t.Fatal(err)
	}
	if ca.ChangesDiffSHA256 != Hash(diff) {
		t.Fatalf("recorded diff sha256 %q != file hash %q", ca.ChangesDiffSHA256, Hash(diff))
	}
	var man changesManifest
	manRaw, err := os.ReadFile(filepath.Join(attemptDir, "changes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manRaw, &man); err != nil {
		t.Fatal(err)
	}
	if len(man.ChangedFiles) != 1 || man.ChangedFiles[0] != "value.txt" || man.DiffTruncated {
		t.Fatalf("changes.json = %+v", man)
	}
	// Targeted cleanup deregisters the attempt's worktrees from the shared source object
	// store; no registration may remain pointing under the output directory.
	wl := exec.Command("git", "worktree", "list", "--porcelain")
	wl.Dir = o.SourceRoot
	listed, _ := wl.CombinedOutput()
	out, _ := filepath.Abs(o.Output)
	resolved, err := filepath.EvalSymlinks(out)
	if err != nil {
		resolved = out
	}
	if strings.Contains(string(listed), out) || strings.Contains(string(listed), resolved) {
		t.Fatalf("stale worktree registration under output after cleanup:\n%s", listed)
	}
}

// TestCohortKeepWorkspacesRetainsTrees proves --keep-workspaces retains the attempt trees
// (item 5).
func TestCohortKeepWorkspacesRetainsTrees(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	o.KeepWorkspaces = true
	if _, err := RunCohort(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	attemptDir := filepath.Join(o.Output, "attempts", "task-a", "base", "rep-1")
	if _, e := os.Stat(filepath.Join(attemptDir, "lab")); e != nil {
		t.Fatalf("lab tree deleted despite --keep-workspaces: %v", e)
	}
	// Even when retained, the captured diff is still written beside the trees.
	if _, e := os.Stat(filepath.Join(attemptDir, "changes.diff")); e != nil {
		t.Fatalf("changes.diff missing with --keep-workspaces: %v", e)
	}
}

// stickyHarness sleeps (so a parent deadline cancels it) until a marker file exists, then
// completes. It drives a cancel on the first run and a clean completion on resume.
const stickyHarness = "#!/bin/sh\ncat >/dev/null\nif [ -f \"$FORGECELL_TEST_MARKER\" ]; then printf changed > value.txt; printf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"done\",\"paths\":[]}'; else : > \"$FORGECELL_TEST_MARKER\"; sleep 60; fi\n"

// tamperHarness completes the task and then mutates the frozen input named by
// $FORGECELL_TEST_TAMPER, so the next attempt's re-hash catches the mutation.
const tamperHarness = "#!/bin/sh\ncat >/dev/null\nprintf changed > value.txt\nif [ -n \"$FORGECELL_TEST_TAMPER\" ]; then chmod u+w \"$FORGECELL_TEST_TAMPER\" 2>/dev/null; printf x >> \"$FORGECELL_TEST_TAMPER\"; fi\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"done\",\"paths\":[]}'\n"

// TestCohortChangesDiffTruncatesAndRecordsError proves changes.diff capture truncates an
// oversize diff (marking it and hashing what was written) instead of aborting, and that a
// git failure is recorded as a changes error rather than aborting the cohort (item 5 HIGH).
func TestCohortChangesDiffTruncatesAndRecordsError(t *testing.T) {
	repo := t.TempDir()
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
	os.WriteFile(filepath.Join(repo, "value.txt"), []byte("before\n"), 0600)
	git("add", ".")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(repo, "big.txt"), []byte(strings.Repeat("abcdefgh\n", 4000)), 0600)
	git("add", ".")
	git("commit", "-qm", "big")
	tree := git("rev-parse", "HEAD")

	attemptDir := t.TempDir()
	sha, truncated, cerr := writeAttemptChangesCapped(context.Background(), attemptDir, repo, base, tree, []string{"big.txt"}, 256)
	if cerr != "" {
		t.Fatalf("unexpected changes error: %s", cerr)
	}
	if !truncated {
		t.Fatal("oversize diff was not marked truncated")
	}
	diff, err := os.ReadFile(filepath.Join(attemptDir, "changes.diff"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diff), "truncated") {
		t.Fatalf("truncation marker missing:\n%s", diff)
	}
	if Hash(diff) != sha {
		t.Fatalf("recorded diff sha %q != written file hash %q", sha, Hash(diff))
	}
	var man changesManifest
	manRaw, _ := os.ReadFile(filepath.Join(attemptDir, "changes.json"))
	if err := json.Unmarshal(manRaw, &man); err != nil {
		t.Fatal(err)
	}
	if !man.DiffTruncated || len(man.ChangedFiles) != 1 || man.ChangedFiles[0] != "big.txt" {
		t.Fatalf("changes.json = %+v", man)
	}
	// A git failure (an unknown tree object) records a changes error and does not abort.
	_, _, cerr2 := writeAttemptChangesCapped(context.Background(), attemptDir, repo, base, strings.Repeat("f", 40), nil, maxChangesDiffBytes)
	if cerr2 == "" || !strings.Contains(cerr2, "cannot capture") {
		t.Fatalf("git failure not recorded as a changes error: %q", cerr2)
	}
}

// TestCohortResumeAfterCancelReruns proves that a coordinate left behind by a cancelled
// attempt is re-run cleanly on resume (its stale worktree is deregistered by targeted
// removal) and that verification then succeeds (item 5 MEDIUM).
func TestCohortResumeAfterCancelReruns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("FORGECELL_TEST_MARKER", marker)
	o := cohortFixture(t, []variantSpec{{name: "base", body: stickyHarness, timeoutMS: 30000}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r1, err := RunCohort(ctx, o)
	if err == nil {
		t.Fatal("expected a cancellation error on the first run")
	}
	if r1.Status != "aborted" {
		t.Fatalf("first run status = %q, want aborted", r1.Status)
	}
	// Resume with a fresh context; the base coordinate must re-run and verify correctly.
	r2, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if r2.Status != "complete" || len(r2.Attempts) != 2 {
		t.Fatalf("resume did not complete: status=%s attempts=%d", r2.Status, len(r2.Attempts))
	}
	for _, a := range r2.Attempts {
		if a.Variant == "base" && !a.Correct {
			t.Fatalf("resumed base attempt did not verify correctly: %+v", a)
		}
	}
}

// TestCohortFreshRunTamperAborts proves that a fresh run re-hashes <out>/inputs before
// each attempt: when the first attempt mutates the frozen cohort file, the second
// attempt's re-hash refuses and the run is recorded as aborted (item 4 MEDIUM).
func TestCohortFreshRunTamperAborts(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: tamperHarness}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	t.Setenv("FORGECELL_TEST_TAMPER", filepath.Join(o.Output, "inputs", "cohort.json"))
	approveCohort(t, &o)
	r, err := RunCohort(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("mutated input accepted: %v", err)
	}
	if r.Status != "aborted" {
		t.Fatalf("status = %q, want aborted", r.Status)
	}
}

// TestCohortImbalanceNote records an imbalance note when a stop leaves the variants with
// unequal attempt counts (item 6).
func TestCohortImbalanceNote(t *testing.T) {
	// maxAttempts=3 over 2 reps: base runs twice, cand once, before the stop.
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 2, "hiddenAcceptancePassRate", &CohortStop{MaxAttempts: ip(3)})
	approveCohort(t, &o)
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "stopped" || len(r.Attempts) != 3 {
		t.Fatalf("status=%s attempts=%d", r.Status, len(r.Attempts))
	}
	found := false
	for _, n := range r.Notes {
		if strings.Contains(n, "Imbalance") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing imbalance note: %v", r.Notes)
	}
}
