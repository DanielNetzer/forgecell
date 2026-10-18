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

// TestCohortRefusesBareSource proves a bare --source (what replay previously wrote as
// source.git) is refused before any output is created or any attempt is recorded, with the
// exact message, rather than failing per-attempt in the molecule.
func TestCohortRefusesBareSource(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	bare := filepath.Join(t.TempDir(), "source.git")
	c := exec.Command("git", "init", "--bare", "-q", bare)
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("git init --bare: %s %v", b, e)
	}
	// A directory where `git rev-parse --show-toplevel` fails is refused the same way.
	for _, source := range []string{bare, t.TempDir()} {
		o.SourceRoot = source
		r, err := RunCohort(context.Background(), o)
		if err == nil || err.Error() != "--source must be a non-bare git checkout (replay writes <out>/source)" {
			t.Fatalf("source %s not refused with the exact message: %v", source, err)
		}
		if len(r.Attempts) != 0 {
			t.Fatalf("attempts recorded for source %s: %+v", source, r.Attempts)
		}
		// Fails before any attempt: no output directory is created.
		if _, e := os.Stat(o.Output); !os.IsNotExist(e) {
			t.Fatalf("output created for source %s: %v", source, e)
		}
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
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	// Deleting the per-attempt source must not record a spurious cleanup error.
	assertCleanCleanup(t, r)
	attemptDir := filepath.Join(o.Output, "attempts", "task-a", "base", "rep-1")
	for _, gone := range []string{"lab", "verification", "source"} {
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
	// Attempts allocate worktrees only in their own per-attempt source, which cleanup
	// deletes; --source never holds a registration pointing under the output directory.
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

// assertCleanCleanup fails when any attempt recorded a cleanupError or the report carries
// the manual-removal note.
func assertCleanCleanup(t *testing.T, r CohortReport) {
	t.Helper()
	if len(r.Attempts) == 0 {
		t.Fatal("no attempts recorded")
	}
	for _, ca := range r.Attempts {
		if ca.CleanupError != "" {
			t.Fatalf("attempt %s/%s recorded cleanupError %q", ca.Task, ca.Variant, ca.CleanupError)
		}
	}
	for _, n := range r.Notes {
		if strings.Contains(n, "may need manual removal") {
			t.Fatalf("report has a cleanup note: %q", n)
		}
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
	// Cancel once the base harness has started (it writes the marker before it
	// sleeps), not on a fixed deadline that setup under load can use up. The
	// fallback deadline is reached only on failure.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	go func() {
		defer cancel()
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if ctx.Err() != nil {
				return
			}
			if _, err := os.Stat(marker); err == nil {
				close(started)
				return
			}
		}
	}()
	r1, err := RunCohort(ctx, o)
	select {
	case <-started:
	default:
		t.Fatalf("base harness never started before the first run ended: %v", err)
	}
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

// claudeCodeCommand is a claude-code-shaped binding command, the only shape for which
// harness.AttemptUsage records usage.
var claudeCodeCommand = []string{"/launcher", "__adapter", "claude-code", "/usr/bin/claude"}

// servedUsage builds harness usage from canned Claude CLI final JSON reporting one served
// model and an optional total cost, compared against pin.
func servedUsage(t *testing.T, served, pin string, cost *float64) *harness.Usage {
	t.Helper()
	top := map[string]any{"modelUsage": map[string]any{served: map[string]any{"inputTokens": 10, "outputTokens": 5, "canonicalModel": served, "costBasis": "list"}}}
	if cost != nil {
		top["total_cost_usd"] = *cost
	}
	raw, _ := json.Marshal(top)
	u := harness.AttemptUsage(claudeCodeCommand, string(raw), pin)
	if u == nil {
		t.Fatal("expected usage from the claude-code-shaped command")
	}
	return u
}

// TestModelMismatchMarksAttemptInvalid: a harness attempt served a model other than its
// pin makes the attempt invalid and never correct; the field is omitted when empty.
func TestModelMismatchMarksAttemptInvalid(t *testing.T) {
	a := completedAttempt(nil)
	a.Correct = true
	a.Molecule.HarnessAttempts = []molecule.HarnessAttempt{{Model: "claude-opus-4-8", Usage: servedUsage(t, "claude-opus-5-5", "claude-opus-4-8", nil)}}
	finalizeUsage(&a)
	if a.Invalid != "model-mismatch" || a.Correct {
		t.Fatalf("mismatch not invalid: invalid=%q correct=%v", a.Invalid, a.Correct)
	}
	if pinned, served, ok := modelMismatch(a); !ok || pinned != "claude-opus-4-8" || served != "claude-opus-5-5" {
		t.Fatalf("mismatch detail = %q %q %v", pinned, served, ok)
	}
	raw, _ := json.Marshal(a)
	if !strings.Contains(string(raw), `"invalid":"model-mismatch"`) {
		t.Fatalf("attempt JSON lacks invalid: %s", raw)
	}
	valid, _ := json.Marshal(completedAttempt(nil))
	if strings.Contains(string(valid), `"invalid"`) {
		t.Fatalf("valid attempt JSON carries invalid: %s", valid)
	}
}

// TestAliasPinIsNotMismatch pins the existing pinMismatch behavior: an alias pin that
// resolves to exactly one model is not a mismatch, so the attempt stays valid.
func TestAliasPinIsNotMismatch(t *testing.T) {
	for _, alias := range []string{"opus", "sonnet", "haiku"} {
		a := completedAttempt(nil)
		a.Correct = true
		u := servedUsage(t, "claude-"+alias+"-5-5", alias, nil)
		if u.ModelMismatch {
			t.Fatalf("alias %s resolving to one model flagged as a mismatch", alias)
		}
		a.Molecule.HarnessAttempts = []molecule.HarnessAttempt{{Model: alias, Usage: u}}
		finalizeUsage(&a)
		if a.Invalid != "" || !a.Correct {
			t.Fatalf("alias %s attempt invalidated: invalid=%q correct=%v", alias, a.Invalid, a.Correct)
		}
	}
}

// TestAttemptCostUSDFromReportedUsage: a harness that reports total_cost_usd yields a
// matching attempt.json costUsd; with no reported cost it stays null.
func TestAttemptCostUSDFromReportedUsage(t *testing.T) {
	dir := t.TempDir()
	read := func(ca CohortAttempt) map[string]json.RawMessage {
		t.Helper()
		path := filepath.Join(dir, "attempt.json")
		if err := saveAttemptRecord(path, ca); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(path)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	}
	a := completedAttempt(nil)
	a.Molecule.HarnessAttempts = []molecule.HarnessAttempt{
		{Model: "claude-opus-5-5", Usage: servedUsage(t, "claude-opus-5-5", "claude-opus-5-5", fp(0.25))},
		{Model: "claude-opus-5-5", Usage: servedUsage(t, "claude-opus-5-5", "claude-opus-5-5", fp(0.5))},
	}
	finalizeUsage(&a)
	if a.CostUSD == nil || *a.CostUSD != 0.75 || a.Invalid != "" {
		t.Fatalf("CostUSD = %v invalid=%q, want 0.75", a.CostUSD, a.Invalid)
	}
	if got := string(read(CohortAttempt{Attempt: a})["costUsd"]); got != "0.75" {
		t.Fatalf("attempt.json costUsd = %s, want 0.75", got)
	}
	none := completedAttempt(nil)
	none.Molecule.HarnessAttempts = []molecule.HarnessAttempt{{Model: "claude-opus-5-5", Usage: servedUsage(t, "claude-opus-5-5", "claude-opus-5-5", nil)}, {}}
	finalizeUsage(&none)
	if none.CostUSD != nil {
		t.Fatalf("CostUSD = %v without a reported cost", *none.CostUSD)
	}
	if got := string(read(CohortAttempt{Attempt: none})["costUsd"]); got != "null" {
		t.Fatalf("attempt.json costUsd = %s, want null", got)
	}
}

// TestCohortModelMismatchInvalidatesAndStops runs a real cohort whose base variant is
// served a model other than its pin (injected as Claude CLI usage the custom stub
// command cannot report). Its first attempt is invalid, excluded from the rates, keeps
// its cost, and stops the run because the comparison cannot be made.
func TestCohortModelMismatchInvalidatesAndStops(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 2, 2, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	original := runCohortMolecule
	t.Cleanup(func() { runCohortMolecule = original })
	runCohortMolecule = func(ctx context.Context, opts molecule.Options) (molecule.Record, error) {
		r, err := original(ctx, opts)
		if strings.Contains(opts.LabDir, string(filepath.Separator)+"base"+string(filepath.Separator)) {
			for i := range r.HarnessAttempts {
				r.HarnessAttempts[i].Model = "claude-opus-4-8"
				r.HarnessAttempts[i].Usage = servedUsage(t, "claude-opus-5-5", "claude-opus-4-8", fp(0.5))
			}
		}
		return r, err
	}
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	want := "variant base was not served its pinned model (claude-opus-4-8 → claude-opus-5-5)"
	if r.Status != "stopped" || r.Stopped != want || len(r.Attempts) != 1 {
		t.Fatalf("status=%s stopped=%q attempts=%d", r.Status, r.Stopped, len(r.Attempts))
	}
	a := r.Attempts[0]
	if a.Variant != "base" || a.Invalid != "model-mismatch" || a.Correct || !completed(a.Attempt) {
		t.Fatalf("first attempt not invalid: variant=%s invalid=%q correct=%v", a.Variant, a.Invalid, a.Correct)
	}
	var base VariantSummary
	for _, v := range r.Variants {
		if v.Name == "base" {
			base = v
		}
	}
	if base.Attempts != 1 || base.Invalid != 1 || base.InvalidReasons["model-mismatch"] != 1 || base.Completions != 0 || base.HiddenAcceptancePass != 0 || base.N.Attempts != 0 || base.CostPerAcceptedUSD != nil {
		t.Fatalf("invalid attempt not excluded from rates: %+v", base)
	}
	if base.ReportedCostUSD == nil || *base.ReportedCostUSD != 0.5 || base.CostCoverage != "1 of 1" {
		t.Fatalf("invalid attempt's spent cost dropped: %+v", base)
	}
	notes := 0
	for _, n := range r.Notes {
		if n == "1 attempt(s) excluded as invalid; see each attempt's invalid field." {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("invalid note count = %d: %v", notes, r.Notes)
	}
	raw, err := os.ReadFile(filepath.Join(o.Output, "attempts", "task-a", "base", "rep-1", "attempt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	if string(fields["invalid"]) != `"model-mismatch"` || string(fields["costUsd"]) != "0.5" {
		t.Fatalf("attempt.json invalid=%s costUsd=%s", fields["invalid"], fields["costUsd"])
	}
	md, _ := os.ReadFile(filepath.Join(o.Output, "report.md"))
	for _, s := range []string{"Stopped early: " + want, "task-a / base / rep-1: invalid:model-mismatch; served: claude-opus-5-5; cost $0.500000"} {
		if !strings.Contains(string(md), s) {
			t.Fatalf("report.md missing %q:\n%s", s, md)
		}
	}
}

// TestCohortLaterModelMismatchDoesNotStop: only a variant's first attempt stops the run;
// a mismatch on a later attempt is recorded as invalid and the cohort completes.
func TestCohortLaterModelMismatchDoesNotStop(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: completedHarness}, {name: "cand", body: completedHarness}}, 1, 2, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	original := runCohortMolecule
	t.Cleanup(func() { runCohortMolecule = original })
	later := string(filepath.Separator) + filepath.Join("base", "rep-2") + string(filepath.Separator)
	runCohortMolecule = func(ctx context.Context, opts molecule.Options) (molecule.Record, error) {
		r, err := original(ctx, opts)
		if strings.Contains(opts.LabDir, later) {
			for i := range r.HarnessAttempts {
				r.HarnessAttempts[i].Model = "claude-opus-4-8"
				r.HarnessAttempts[i].Usage = servedUsage(t, "claude-opus-5-5", "claude-opus-4-8", nil)
			}
		}
		return r, err
	}
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "complete" || r.Stopped != "" || len(r.Attempts) != 4 {
		t.Fatalf("status=%s stopped=%q attempts=%d", r.Status, r.Stopped, len(r.Attempts))
	}
	for _, a := range r.Attempts {
		if invalid := a.Variant == "base" && a.Repetition == 2; (a.Invalid == "model-mismatch") != invalid {
			t.Fatalf("%s rep-%d invalid=%q", a.Variant, a.Repetition, a.Invalid)
		}
	}
}

// wrongValueHarness completes with a change the acceptance check rejects.
const wrongValueHarness = "#!/bin/sh\ncat >/dev/null\nprintf wrong > value.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"done\",\"paths\":[]}'\n"

// TestCohortAttemptRecordKeepsFailedCheckOutputAfterCleanup: the failed check's entry is
// read from the verification history before cleanup deletes the Lab, survives in
// attempt.json, and is left out of report.json.
func TestCohortAttemptRecordKeepsFailedCheckOutputAfterCleanup(t *testing.T) {
	o := cohortFixture(t, []variantSpec{{name: "base", body: wrongValueHarness}, {name: "cand", body: completedHarness}}, 1, 1, "hiddenAcceptancePassRate", nil)
	approveCohort(t, &o)
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	attemptDir := filepath.Join(o.Output, "attempts", "task-a", "base", "rep-1")
	if _, e := os.Stat(filepath.Join(attemptDir, "lab")); !os.IsNotExist(e) {
		t.Fatalf("lab tree not deleted: %v", e)
	}
	raw, err := os.ReadFile(filepath.Join(attemptDir, "attempt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ca CohortAttempt
	if err := json.Unmarshal(raw, &ca); err != nil {
		t.Fatal(err)
	}
	if ca.Correct || len(ca.FailedCheckOutput) == 0 {
		t.Fatalf("attempt.json lost the failed check: correct=%v %+v", ca.Correct, ca.FailedCheckOutput)
	}
	found := false
	for _, f := range ca.FailedCheckOutput {
		if f.ID == "acceptance" && f.Phase == "check" && !f.Protected && f.Code != 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failed acceptance entry: %+v", ca.FailedCheckOutput)
	}
	if r.Attempts[0].FailedCheckOutput == nil {
		t.Fatal("in-memory report attempt lost its failed-check output")
	}
	report, err := os.ReadFile(filepath.Join(o.Output, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(report), "failedCheckOutput") {
		t.Fatal("report.json carries failed-check output")
	}
	md, _ := os.ReadFile(filepath.Join(o.Output, "report.md"))
	if !strings.Contains(string(md), "  - failed check `acceptance` exit ") {
		t.Fatalf("report.md missing the failed check:\n%s", md)
	}
}
