package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

type Command struct {
	Name      string   `json:"name"`
	Dir       string   `json:"dir"`
	Argv      []string `json:"argv"`
	TimeoutMS int      `json:"timeoutMs"`
	// Select narrows a direct `go test ... ./...` check to the packages the attempt's
	// captured change affects (readiness.SelectAffectedPackages). Hidden acceptance and
	// setup never select. Empty keeps the plan bytes of earlier evaluations unchanged.
	Select string `json:"select,omitempty"`
}

// validateCommandSelect refuses a selection on setup or hidden acceptance, and any
// selection the readiness rule refuses for a regression check.
func validateCommandSelect(c Command, setup bool) error {
	if c.Select == "" {
		return nil
	}
	if setup || c.Name == "acceptance" {
		return fmt.Errorf("evaluation command %s cannot declare select; only go test regression checks may", c.Name)
	}
	return readiness.ValidateCheckSelect(readiness.Check{ID: c.Name, Category: "regression", Argv: c.Argv, Select: c.Select})
}

type Plan struct {
	ReadinessVersion   int                      `json:"readinessVersion"`
	TargetBranch       string                   `json:"targetBranch"`
	Artifacts          []readiness.ArtifactRoot `json:"artifacts"`
	SchemaVersion      int                      `json:"schemaVersion"`
	Approval           string                   `json:"approval"`
	ApprovalSHA256     string                   `json:"approvalSha256"`
	Task               string                   `json:"task"`
	TaskSHA256         string                   `json:"taskSha256"`
	Acceptance         string                   `json:"acceptance"`
	AcceptanceSHA256   string                   `json:"acceptanceSha256"`
	BaseCommit         string                   `json:"baseCommit"`
	AllowedFiles       []string                 `json:"allowedFiles"`
	OriginalCheckFiles []string                 `json:"originalCheckFiles"`
	// CandidateTestFiles lists the replayed PR's own test files, a subset of
	// AllowedFiles and OriginalCheckFiles, that hidden acceptance grades. They run in
	// their candidate version in the protected regression and independent
	// verification. Only replay sets it; a recheck never adds to it.
	CandidateTestFiles    []string  `json:"candidateTestFiles,omitempty"`
	RequireInitialFailure bool      `json:"requireInitialFailure"`
	Setup                 []Command `json:"setup"`
	Checks                []Command `json:"checks"`
}
type Observation struct {
	Command Command        `json:"command"`
	Result  process.Result `json:"result"`
	// Cache states whether the command could be served from the shared Go cache
	// ("enabled") or always runs ("disabled (fresh)", hidden acceptance included).
	Cache string `json:"cache,omitempty"`
	// Note explains a failure that Forgecell's own cache settings caused.
	Note string `json:"note,omitempty"`
}
type Attempt struct {
	Verification  Verification    `json:"verification"`
	Variant       string          `json:"variant"`
	FormulaSHA256 string          `json:"formulaSha256"`
	StartedAt     string          `json:"startedAt"`
	ElapsedMS     int64           `json:"elapsedMs"`
	HarnessMS     int64           `json:"harnessMs"`
	Molecule      molecule.Record `json:"molecule"`
	Setup         []Observation   `json:"setup"`
	Preflight     []Observation   `json:"preflight"`
	Checks        []Observation   `json:"checks"`
	ChangedFiles  []string        `json:"changedFiles"`
	ScopeOK       bool            `json:"scopeOK"`
	Tree          string          `json:"tree,omitempty"`
	Correct       bool            `json:"correct"`
	// RequiredChecks is the tri-state required-checks result: nil when the plan has no
	// non-acceptance check (n/a), otherwise true only when verification ran and every
	// non-acceptance plan check was observed passing. Computed once at attempt time from
	// the plan and persisted, so a resumed report never needs the plan to aggregate it.
	RequiredChecks *bool `json:"requiredChecks,omitempty"`
	// HiddenAcceptance is the hidden acceptance check's own result, independent of every
	// other check, from the independent verification that sets Correct (or the Molecule's
	// candidate verification when that never ran): nil when it never ran or did not
	// complete (coding incomplete, setup failed, verification errored first, or the check
	// timed out), and for an invalid attempt. It is always written, null included, so a
	// record lacking the key is known to predate it.
	HiddenAcceptance *bool `json:"hiddenAcceptance"`
	// RegressionFailedTests lists, sorted and unique, the Go tests the full streams of the
	// failed regression checks report failing, for an attempt whose hidden acceptance
	// passed but whose regression checks did not. RegressionFailedTestsTruncated marks a
	// failed check whose stored stream was itself cut, so the list may be incomplete.
	RegressionFailedTests          []string `json:"regressionFailedTests,omitempty"`
	RegressionFailedTestsTruncated bool     `json:"regressionFailedTestsTruncated,omitempty"`
	// RegressionFailures pairs each of those failed tests with its package and its three
	// re-runs at the base commit: FlakyAtBase when it both passed and failed there,
	// BrokenAtBase when it failed every run; a test beyond the re-run cap, or one whose base
	// re-runs could not decide, is Unclassified. Cohort attempts only.
	RegressionFailures []RegressionFailure `json:"regressionFailures,omitempty"`
	// CorrectExcludingBaseFlakes is Correct, or, judged from the independent verification
	// alone with every plan check observed: acceptance and every non-go-test check passed and
	// each failed go test check failed solely on tests tagged FlakyAtBase (no unattributed
	// package failure). Correct keeps its strict meaning.
	CorrectExcludingBaseFlakes bool `json:"correctExcludingBaseFlakes,omitempty"`
	// EvaluationVerification is the independent verification of a completed coding attempt
	// whose captured tree the Molecule's own fail-fast verification stopped on a failed
	// check; Verification keeps that refusal, so Correct and the required checks keep their
	// meaning, and only HiddenAcceptance and CorrectExcludingBaseFlakes read this run.
	// Cohort attempts only.
	EvaluationVerification *Verification `json:"evaluationVerification,omitempty"`
	// ChangesDiffSHA256 is the sha256 of the retained <attempt>/changes.diff captured
	// before the workspace tree is deleted; ChangesTruncated marks a diff cut at the
	// 5 MiB cap. Both are empty/false for single-task runs, which retain their trees.
	ChangesDiffSHA256 string `json:"changesDiffSha256,omitempty"`
	ChangesTruncated  bool   `json:"changesDiffTruncated,omitempty"`
	// ChangesError records why the captured diff could not be produced (a git failure or a
	// write failure). It never aborts the cohort: the attempt record and verification are
	// still saved, with an empty or partial changes.diff. CleanupError records a failure
	// to reclaim the attempt's Lab or workspace trees after its evidence was saved. Both
	// are empty for single-task runs, which neither capture a diff nor delete trees.
	ChangesError string `json:"changesError,omitempty"`
	CleanupError string `json:"cleanupError,omitempty"`
	Error        string `json:"error,omitempty"`
	// Invalid names why the attempt cannot count toward a comparison (for example
	// "model-mismatch" when the harness was not served its pinned model). An invalid
	// attempt is never correct and is excluded from every cohort rate and median.
	Invalid string `json:"invalid,omitempty"`
	// FailedCheckOutput keeps the stream tails of failed Molecule checks, read from the
	// verification history before a cohort deletes the attempt's Lab.
	FailedCheckOutput []FailedCheckOutput `json:"failedCheckOutput,omitempty"`
	// CostUSD is the reported harness cost (#149 usage), nil when no harness attempt
	// reported one.
	CostUSD       *float64 `json:"costUsd"`
	Retries       *int     `json:"observedHarnessRetries"`
	Interventions int      `json:"humanInterventions"`
}
type Report struct {
	ActivationDigest     string    `json:"activationDigest,omitempty"`
	ActivationDecisionAt string    `json:"activationDecisionAt,omitempty"`
	SchemaVersion        int       `json:"schemaVersion"`
	Status               string    `json:"status"`
	PlanSHA256           string    `json:"planSha256"`
	TaskSHA256           string    `json:"taskSha256"`
	ApprovalSHA256       string    `json:"approvalSha256"`
	ApprovalID           string    `json:"approvalId"`
	BaseCommit           string    `json:"baseCommit"`
	StartedAt            string    `json:"startedAt"`
	FinishedAt           string    `json:"finishedAt,omitempty"`
	Attempts             []Attempt `json:"attempts"`
	Notes                []string  `json:"notes"`
}
type RunOptions struct{ Inputs, SourceRoot, Output, Approve string }

var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func relative(s string, allowDot bool) bool {
	return s != "" && !filepath.IsAbs(s) && !strings.ContainsAny(s, "\x00\\") && filepath.Clean(s) == s && (allowDot || s != ".") && s != ".." && !strings.HasPrefix(s, "../")
}
func bounded(file string) ([]byte, error) { return boundedTo(file, 2_000_000) }

// boundedTo reads a regular file of at most limit bytes.
func boundedTo(file string, limit int64) ([]byte, error) {
	st, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, fmt.Errorf("input must be a bounded regular file: %s", file)
	}
	return os.ReadFile(file)
}
func frozen(dir, name, hash string) ([]byte, error) {
	if !relative(name, false) {
		return nil, fmt.Errorf("invalid frozen input path")
	}
	raw, err := bounded(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	if Hash(raw) != hash {
		return nil, fmt.Errorf("frozen input hash mismatch: %s", name)
	}
	return raw, nil
}
func saveReport(dir string, r Report) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".report-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "report.json"))
}
func observe(ctx context.Context, c Command, workspace, acceptance string) Observation {
	argv := make([]string, len(c.Argv))
	for i, v := range c.Argv {
		argv[i] = strings.ReplaceAll(strings.ReplaceAll(v, "{acceptance}", acceptance), "{workspace}", workspace)
	}
	// The private HOME can gain a read-only Go tree; resources.Remove makes it writable
	// first, and a failed removal stays registered for exit cleanup.
	home, err := resources.MkdirTemp("", "forgecell-eval-home-")
	if err != nil {
		return Observation{Command: c, Result: process.Result{Code: -1, Error: "cannot create private HOME: " + err.Error()}}
	}
	defer resources.Remove(home)
	dir := filepath.Join(workspace, c.Dir)
	// Hidden acceptance is the plan's independent acceptance check and is fresh by
	// construction, whatever its argv runs.
	env, cache := observeEnv(ctx, home, workspace, c.Name == "acceptance")
	r := process.Run(ctx, process.Options{Argv: argv, Dir: dir, Env: env, Stdin: []byte{}, Timeout: time.Duration(c.TimeoutMS) * time.Millisecond, MaxOutputBytes: 2_000_000})
	o := Observation{Command: c, Result: r, Cache: verification.CacheStatus(argv, env, cache, false)}
	if c.Name == "acceptance" {
		o.Cache = verification.AcceptanceCacheStatus(cache)
	}
	if cache && !r.OK {
		o.Note = resources.CacheSettingsNote(r.Stdout+"\n"+r.Stderr, nil)
	}
	return o
}

// observeEnv is the scrubbed environment of an evaluation command in workspace, as in the
// Molecule's views: a private HOME, the caller's PATH and LANG, a stable TMPDIR (the one
// ctx carries, else the caller's), the shared Go cache with GOPROXY=off and GOFLAGS with
// -trimpath (no -mod, so a vendored module keeps vendor mode; -count=1 when fresh), then
// the cohort's -p. It never inherits os.Environ() wholesale. cache is false when go env
// could not resolve the shared cache.
func observeEnv(ctx context.Context, home, workspace string, fresh bool) (env []string, cache bool) {
	env = []string{"HOME=" + home}
	for _, k := range []string{"PATH", "LANG"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if tmp, ok := resources.CheckTempDirFrom(ctx); ok {
		env = append(env, "TMPDIR="+tmp)
	} else if v, ok := os.LookupEnv("TMPDIR"); ok {
		env = append(env, "TMPDIR="+v)
	}
	env = append(env, "FORGECELL_EVAL_WORKSPACE="+workspace)
	if gomodcache, gocache, ok := resolveGoCache(ctx); ok {
		env, _ = resources.ApplyGoCache(env, resources.GoCache{ModCache: gomodcache, BuildCache: gocache}, resources.GoEnvOptions{Fresh: fresh})
		cache = true
	}
	// A cohort attempt bounds each go command's build parallelism through GOFLAGS.
	return resources.ApplyGoParallelism(ctx, env), cache
}
func gitResult(ctx context.Context, dir string, args ...string) process.Result {
	return process.Run(ctx, process.Options{Argv: append([]string{"git"}, args...), Dir: dir, Stdin: []byte{}, Timeout: 30 * time.Second})
}
func outputTree(ctx context.Context, w molecule.Workspace, allowed []string) ([]string, bool, string, error) {
	head := gitResult(ctx, w.Path, "rev-parse", "HEAD")
	if !head.OK || strings.TrimSpace(head.Stdout) != w.BaseCommit {
		return nil, false, "", fmt.Errorf("harness changed HEAD")
	}
	branch := gitResult(ctx, w.Path, "branch", "--show-current")
	if !branch.OK || strings.TrimSpace(branch.Stdout) != w.Branch {
		return nil, false, "", fmt.Errorf("harness changed workspace branch")
	}
	names := map[string]bool{}
	scope := map[string]bool{}
	for _, f := range allowed {
		scope[f] = true
	}
	for _, args := range [][]string{{"ls-files", "--modified", "--deleted", "--others", "--exclude-standard", "-z"}, {"diff", "--cached", "--name-only", "-z"}} {
		r := gitResult(ctx, w.Path, args...)
		if !r.OK {
			return nil, false, "", fmt.Errorf("cannot inspect output scope")
		}
		for _, f := range strings.Split(r.Stdout, "\x00") {
			if f != "" {
				names[f] = true
			}
		}
	}
	files := []string{}
	ok := true
	for f := range names {
		files = append(files, f)
		if !scope[f] {
			ok = false
		}
	}
	sort.Strings(files)
	temp, err := resources.MkdirTemp("", "forgecell-eval-index-")
	if err != nil {
		return files, false, "", err
	}
	// Only a git index lives here; a failed removal stays registered for exit cleanup.
	defer resources.Remove(temp)
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_INDEX_FILE=") {
			env = append(env, v)
		}
	}
	env = append(env, "GIT_INDEX_FILE="+filepath.Join(temp, "index"))
	run := func(args ...string) process.Result {
		return process.Run(ctx, process.Options{Argv: append([]string{"git"}, args...), Dir: w.Path, Env: env, Stdin: []byte{}, Timeout: 30 * time.Second})
	}
	if r := run("read-tree", w.BaseCommit); !r.OK {
		return files, false, "", fmt.Errorf("cannot read base tree")
	}
	if r := run("add", "-A", "--", "."); !r.OK {
		return files, false, "", fmt.Errorf("cannot snapshot output")
	}
	r := run("write-tree")
	if !r.OK {
		return files, false, "", fmt.Errorf("cannot record output tree")
	}
	return files, ok, strings.TrimSpace(r.Stdout), nil
}
func Run(ctx context.Context, o RunOptions) (report Report, err error) {
	o.Inputs, err = filepath.Abs(o.Inputs)
	if err != nil {
		return report, err
	}
	o.Output, err = filepath.Abs(o.Output)
	if err != nil {
		return report, err
	}
	o.SourceRoot, err = filepath.Abs(o.SourceRoot)
	if err != nil {
		return report, err
	}
	raw, err := bounded(o.Inputs)
	if err != nil {
		return report, err
	}
	var p Plan
	if err = json.Unmarshal(raw, &p); err != nil {
		return report, err
	}
	if p.SchemaVersion != 1 || !commitPattern.MatchString(p.BaseCommit) || len(p.AllowedFiles) == 0 || len(p.Checks) == 0 {
		return report, fmt.Errorf("plan requires exact base, scope and independent checks")
	}
	if p.ReadinessVersion != 1 || p.TargetBranch == "" {
		return report, fmt.Errorf("evaluation plan requires readinessVersion 1 and an explicit targetBranch; review the updated plan before execution")
	}
	for _, file := range p.AllowedFiles {
		if !relative(file, false) {
			return report, fmt.Errorf("invalid output scope")
		}
	}
	for _, file := range p.OriginalCheckFiles {
		if !relative(file, false) {
			return report, fmt.Errorf("invalid original check file")
		}
	}
	if err = validateCandidateTestFiles(p); err != nil {
		return report, err
	}
	acceptanceCount := 0
	names := map[string]bool{}
	for _, c := range append(append([]Command{}, p.Setup...), p.Checks...) {
		if c.Name == "" || names[c.Name] || !relative(c.Dir, true) || len(c.Argv) == 0 || c.Argv[0] == "" || c.TimeoutMS < 1 || c.TimeoutMS > 3_600_000 {
			return report, fmt.Errorf("invalid or duplicate evaluation command")
		}
		names[c.Name] = true
	}
	for i, c := range append(append([]Command{}, p.Setup...), p.Checks...) {
		if err = validateCommandSelect(c, i < len(p.Setup)); err != nil {
			return report, err
		}
	}
	for _, c := range p.Checks {
		if c.Name == "acceptance" {
			acceptanceCount++
		}
	}
	if acceptanceCount != 1 {
		return report, fmt.Errorf("one check must be named acceptance")
	}
	dir := filepath.Dir(o.Inputs)
	approvalRaw, err := frozen(dir, p.Approval, p.ApprovalSHA256)
	if err != nil {
		return report, err
	}
	pair, err := ValidateApproval(approvalRaw)
	if err != nil {
		return report, err
	}
	taskRaw, err := frozen(dir, p.Task, p.TaskSHA256)
	if err != nil {
		return report, err
	}
	var issue molecule.Issue
	if err = json.Unmarshal(taskRaw, &issue); err != nil {
		return report, err
	}
	if issue.Number < 1 || issue.Repo != pair.Baseline.Intake.Repo || issue.URL != fmt.Sprintf("https://github.com/%s/issues/%d", issue.Repo, issue.Number) {
		return report, fmt.Errorf("frozen issue does not match Formula intake")
	}
	acceptanceRaw, err := frozen(dir, p.Acceptance, p.AcceptanceSHA256)
	if err != nil {
		return report, err
	}

	activationDigest := ActivationDigest(raw, pair, o.SourceRoot, o.Output)
	if o.Approve != activationDigest {
		return report, fmt.Errorf("fresh exact evaluation approval required; historical suggestion %s is evidence only. Review frozen plan:\n%s\nBaseline %s SHA-256 %s\n%s\nCandidate %s SHA-256 %s\n%s\nSource %q; output %q. Review setup, checks, scope and both exact recipes, then evaluate --inputs %q --source %q --out %q --approve %s", pair.Approval.ID, string(raw), pair.Baseline.ID, pair.Baseline.SHA256, pair.Baseline.YAML, pair.Candidate.ID, pair.Candidate.SHA256, pair.Candidate.YAML, o.SourceRoot, o.Output, o.Inputs, o.SourceRoot, o.Output, activationDigest)
	}
	decisionAt := time.Now().UTC().Format(time.RFC3339Nano)
	if err = os.Mkdir(o.Output, 0700); err != nil {
		return report, fmt.Errorf("use a new output directory; prior attempts must be preserved: %w", err)
	}
	inputDir := filepath.Join(o.Output, "inputs")
	if err = os.Mkdir(inputDir, 0700); err != nil {
		return report, err
	}
	for name, body := range map[string][]byte{"plan.json": raw, "approval.json": approvalRaw, "issue.json": taskRaw, "acceptance" + filepath.Ext(p.Acceptance): acceptanceRaw} {
		if err = os.WriteFile(filepath.Join(inputDir, name), body, 0400); err != nil {
			return report, err
		}
	}
	acceptance := filepath.Join(inputDir, "acceptance"+filepath.Ext(p.Acceptance))
	report = Report{ActivationDigest: activationDigest, ActivationDecisionAt: decisionAt, SchemaVersion: 1, Status: "running", PlanSHA256: Hash(raw), TaskSHA256: p.TaskSHA256, ApprovalSHA256: p.ApprovalSHA256, ApprovalID: pair.Approval.ID, BaseCommit: p.BaseCommit, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Attempts: []Attempt{}, Notes: []string{"One paired trial is a feasibility result, not statistical evidence of improvement.", "Cost and harness-internal retries are unknown unless separately evidenced. Subscription access is not zero cost.", "Trigger/fallback behavior requires provider trace review; harness prose alone is not independent verification.", "Independent checks run outside the coding harness sandbox; their results are reported separately."}}
	if err = saveReport(o.Output, report); err != nil {
		return report, err
	}
	for i, f := range []formula.Formula{pair.Baseline, pair.Candidate} {
		name := []string{"baseline", "candidate"}[i]
		started := time.Now()
		a := Attempt{Variant: name, FormulaSHA256: f.SHA256, StartedAt: started.UTC().Format(time.RFC3339Nano), Checks: []Observation{}, Setup: []Observation{}}
		lab := filepath.Join(o.Output, name)
		if err = os.MkdirAll(filepath.Join(lab, "formulas"), 0700); err != nil {
			return report, err
		}
		// Load by exact ID and preserve the originally approved bytes, not a regenerated recipe.
		if !regexp.MustCompile(`^[a-z0-9_-]+$`).MatchString(f.ID) {
			return report, fmt.Errorf("evaluation requires a canonical Formula id")
		}
		if err = os.WriteFile(filepath.Join(lab, "formulas", f.ID+".yaml"), []byte(f.YAML), 0400); err != nil {
			return report, err
		}

		// Activate from this fresh execution decision, under the shared Formula lock.
		lock := filepath.Join(lab, ".formula-write.lock")
		if err = os.Mkdir(lock, 0700); err != nil {
			return report, err
		}
		aRecord := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: "evaluation-" + activationDigest + "-" + name, SourceKind: "evaluation-activation", SourceID: activationDigest}
		err = func() error {
			defer os.Remove(lock)
			if e := formula.BeginActivation(lab, f.YAML, aRecord, "absent", f.SHA256); e != nil {
				return e
			}
			if e := formula.ApplyActivation(lab, aRecord); e != nil {
				return e
			}
			// Decision provenance is durably persisted before completing Lab activation.
			decision, _ := json.Marshal(map[string]string{"digest": activationDigest, "approvedAt": decisionAt, "planSha256": Hash(raw), "baselineSha256": pair.Baseline.SHA256, "candidateSha256": pair.Candidate.SHA256, "sourceRoot": o.SourceRoot, "output": o.Output})
			file, e := os.OpenFile(filepath.Join(lab, "evaluation-decision.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			if _, e = file.Write(decision); e != nil {
				file.Close()
				return e
			}
			if e = file.Sync(); e != nil {
				file.Close()
				return e
			}
			if e = file.Close(); e != nil {
				return e
			}
			d, e := os.Open(lab)
			if e != nil {
				return e
			}
			e = d.Sync()
			d.Close()
			if e != nil {
				return e
			}
			return formula.CompleteActivation(lab, aRecord)
		}()
		if err != nil {
			return report, err
		}
		runOptions := molecule.Options{LabDir: lab, SourceRoot: o.SourceRoot, Issue: fmt.Sprint(issue.Number), Base: p.BaseCommit, ReadIssue: func(context.Context, string, int64, string) (molecule.Issue, error) { return issue, nil }, PrepareWorkspace: func(ctx context.Context, w molecule.Workspace) error {
			for _, c := range p.Setup {
				r := observe(ctx, c, w.Path, acceptance)
				a.Setup = append(a.Setup, r)
				if !r.Result.OK {
					return fmt.Errorf("setup %s failed", c.Name)
				}
			}
			clean := gitResult(ctx, w.Path, "status", "--porcelain")
			if !clean.OK || strings.TrimSpace(clean.Stdout) != "" {
				return fmt.Errorf("setup changed tracked or untracked repository files")
			}
			if p.RequireInitialFailure {
				for _, c := range p.Checks {
					if c.Name == "acceptance" {
						r := observe(ctx, c, w.Path, acceptance)
						a.Preflight = append(a.Preflight, r)
						if r.Result.Code != 1 || r.Result.Incomplete() {
							return fmt.Errorf("original snapshot did not produce expected acceptance failure")
						}
					}
				}
			}
			return nil
		}}
		configureReadiness(&runOptions, p, raw, acceptanceRaw)
		a.Molecule, err = molecule.Run(ctx, runOptions)
		if err == nil && a.Molecule.Readiness != nil {
			runOptions.Approve = a.Molecule.Readiness.Plans[0].Digest
			a.Molecule, err = molecule.Run(ctx, runOptions)
		}

		if err != nil {
			a.Error = err.Error()
			if a.Molecule.Status == "" {
				a.Molecule.Status = "blocked"
			}
		}
		for _, atom := range a.Molecule.Atoms {
			if atom.Type == "harness" {
				a.HarnessMS += atom.ElapsedMS
			}
		}
		completed := false
		for _, atom := range a.Molecule.Atoms {
			if atom.Type == "harness" && atom.Status == "done" {
				completed = true
			}
		}
		if a.Molecule.Workspace.Path != "" {
			var treeErr error
			a.ChangedFiles, a.ScopeOK, a.Tree, treeErr = outputTree(ctx, a.Molecule.Workspace, p.AllowedFiles)
			if treeErr != nil {
				a.Error = treeErr.Error()
			}
		}
		if a.Error == "" {
			a.Verification = verify(ctx, o.SourceRoot, filepath.Join(o.Output, "verification", name), acceptance, p, a)
			a.Checks = a.Verification.Checks
			a.Correct = completed && a.Verification.Correct
			if a.Verification.Error != "" {
				a.Error = a.Verification.Error
			}
		}

		a.CostUSD = reportedCost(a)
		a.ElapsedMS = time.Since(started).Milliseconds()
		report.Attempts = append(report.Attempts, a)
		if err = saveReport(o.Output, report); err != nil {
			return report, err
		}
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
	}
	report.Status = "complete"
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	err = saveReport(o.Output, report)
	return report, err
}

// Evaluation input is a human-reviewed, hashed execution plan, not model output.
// It supplies a concrete readiness proposal and then uses ordinary scope
// approval; it cannot bypass the new execution gate with a legacy Formula.
func configureReadiness(o *molecule.Options, p Plan, planBytes, acceptance []byte) {
	o.TargetBranch = p.TargetBranch
	o.CheckInputs = []readiness.FileEvidence{
		{Evidence: readiness.Evidence{ID: "evaluation-plan", Path: "evaluation-plan.json", SHA256: Hash(planBytes)}, Content: string(planBytes)},
		{Evidence: readiness.Evidence{ID: "acceptance-input", Path: "acceptance.sh", SHA256: Hash(acceptance)}, Content: string(acceptance)},
	}
	o.Artifacts = p.Artifacts
	o.ProtectedExemptPaths = protectedExemptPaths(p)
	command := func(c Command, category string) readiness.Check {
		argv := append([]string{}, c.Argv...)
		for i, arg := range argv {
			if arg == "{acceptance}" {
				argv[i] = "{input:acceptance-input}"
			}
		}
		provenance, selection := "", ""
		if category == "independent-acceptance" {
			provenance = "Reviewed frozen evaluation plan SHA-256 " + Hash(planBytes)
		} else if category == "regression" {
			// Hidden acceptance keeps its own -run filter and never selects.
			selection = c.Select
		}
		return readiness.Check{ID: c.Name, Category: category, IndependentProvenance: provenance, Argv: argv, Dir: c.Dir, TimeoutMS: c.TimeoutMS, Required: true, Definitions: []string{"evaluation-plan", "acceptance-input"}, Reason: "Frozen evaluation command", Evidence: []string{"issue", "evaluation-plan"}, Select: selection}
	}
	for _, c := range p.Setup {
		o.Setup = append(o.Setup, command(c, "setup"))
	}
	o.Analyze = func(_ context.Context, _ formula.Binding, _ molecule.Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a := readiness.Analysis{Summary: "Execute the reviewed frozen evaluation task", Acceptance: []readiness.Criterion{{Description: "Pass the frozen independently reviewed acceptance check", Evidence: []string{"issue", "evaluation-plan"}}}}
		// A scope item whose path does not exist at base needs a scope evidence ID citing a
		// manifest (go.mod/package.json/pyproject.toml) whose directory is an ancestor
		// (readiness.ValidateScope). Cite the nearest-ancestor manifest already in the
		// collected repository snapshot (the lab/go.mod that CollectForTicket collects and
		// ValidateScope reads) in addition to issue and evaluation-plan. If no ancestor
		// manifest is in the snapshot the scope item keeps only issue/evaluation-plan and the
		// attempt fails at ValidateScope as it does today.
		existing := map[string]bool{}
		for _, f := range s.Files {
			existing[f.Path] = true
		}
		for _, file := range p.AllowedFiles {
			evidence := []string{"issue", "evaluation-plan"}
			if !existing[file] {
				if id, ok := nearestManifestEvidence(file, s.Evidence); ok {
					evidence = append(evidence, id)
				}
			}
			a.Scope = append(a.Scope, readiness.ScopedPath{Path: file, Reason: "Explicit frozen evaluation scope", Evidence: evidence})
		}
		// Hidden acceptance runs first: verification is fail-fast, so a failing
		// regression check (for example a flake in the replayed historical code) must
		// not hide the acceptance result. The other checks follow in plan order.
		for _, c := range p.Checks {
			if c.Name == "acceptance" {
				a.Checks = append(a.Checks, command(c, "independent-acceptance"))
			}
		}
		for _, c := range p.Checks {
			if c.Name != "acceptance" {
				a.Checks = append(a.Checks, command(c, "regression"))
			}
		}
		return a, nil
	}
}

// validateCandidateTestFiles requires each candidate test file to be a clean, distinct,
// in-scope original check file at a conventional test path.
func validateCandidateTestFiles(p Plan) error {
	allowed := map[string]bool{}
	for _, file := range p.AllowedFiles {
		allowed[file] = true
	}
	original := map[string]bool{}
	for _, file := range p.OriginalCheckFiles {
		original[file] = true
	}
	seen := map[string]bool{}
	for _, file := range p.CandidateTestFiles {
		if !relative(file, false) || seen[file] {
			return fmt.Errorf("invalid or duplicate candidate test file")
		}
		seen[file] = true
		if !allowed[file] || !original[file] || !readiness.ConventionalTestPath(file) {
			return fmt.Errorf("candidate test file must be an in-scope original check file at a conventional test path: %q", file)
		}
	}
	return nil
}

// protectedExemptPaths returns the replayed PR's own test files, the plan's
// CandidateTestFiles. Hidden acceptance grades them with their real versions, so the
// protected view keeps the candidate's copies instead of restoring base. Every other
// original check file, including any a recheck adds, keeps its base version. Entries
// that are not in-scope conventional test original check files are never exempt.
// It is a variable only so tests can strip the exemption.
var protectedExemptPaths = func(p Plan) []string {
	allowed := map[string]bool{}
	for _, file := range p.AllowedFiles {
		allowed[file] = true
	}
	original := map[string]bool{}
	for _, file := range p.OriginalCheckFiles {
		original[file] = true
	}
	var out []string
	for _, file := range p.CandidateTestFiles {
		if allowed[file] && original[file] && readiness.ConventionalTestPath(file) {
			out = append(out, file)
			allowed[file] = false
		}
	}
	// A canonical order keeps the plan digest independent of the task's file order.
	sort.Strings(out)
	return out
}

// nearestManifestEvidence returns the evidence ID of the nearest-ancestor manifest
// (go.mod/package.json/pyproject.toml) of file among the snapshot evidence, matching the
// ancestry rule readiness.ValidateScope applies: a manifest at repository root (directory
// ".") is an ancestor of every path, otherwise its directory must be a strict ancestor.
// When several manifests qualify the deepest (longest directory) wins. ok is false when no
// manifest in the snapshot is an ancestor.
func nearestManifestEvidence(file string, evidence []readiness.FileEvidence) (string, bool) {
	bestID := ""
	bestLen := -1
	for _, e := range evidence {
		switch path.Base(e.Path) {
		case "go.mod", "package.json", "pyproject.toml":
		default:
			continue
		}
		root := path.Dir(e.Path)
		var l int
		switch {
		case root == ".":
			l = 0
		case strings.HasPrefix(file, root+"/"):
			l = len(root)
		default:
			continue
		}
		if l > bestLen {
			bestLen, bestID = l, e.ID
		}
	}
	return bestID, bestLen >= 0
}
