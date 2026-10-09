package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

type Command struct {
	Name      string   `json:"name"`
	Dir       string   `json:"dir"`
	Argv      []string `json:"argv"`
	TimeoutMS int      `json:"timeoutMs"`
}
type Plan struct {
	ReadinessVersion      int                      `json:"readinessVersion"`
	TargetBranch          string                   `json:"targetBranch"`
	Artifacts             []readiness.ArtifactRoot `json:"artifacts"`
	SchemaVersion         int                      `json:"schemaVersion"`
	Approval              string                   `json:"approval"`
	ApprovalSHA256        string                   `json:"approvalSha256"`
	Task                  string                   `json:"task"`
	TaskSHA256            string                   `json:"taskSha256"`
	Acceptance            string                   `json:"acceptance"`
	AcceptanceSHA256      string                   `json:"acceptanceSha256"`
	BaseCommit            string                   `json:"baseCommit"`
	AllowedFiles          []string                 `json:"allowedFiles"`
	OriginalCheckFiles    []string                 `json:"originalCheckFiles"`
	RequireInitialFailure bool                     `json:"requireInitialFailure"`
	Setup                 []Command                `json:"setup"`
	Checks                []Command                `json:"checks"`
}
type Observation struct {
	Command Command        `json:"command"`
	Result  process.Result `json:"result"`
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
	Error         string          `json:"error,omitempty"`
	CostUSD       *float64        `json:"costUsd"`
	Retries       *int            `json:"observedHarnessRetries"`
	Interventions int             `json:"humanInterventions"`
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
func bounded(file string) ([]byte, error) {
	st, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 2_000_000 {
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
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "FORGECELL_EVAL_WORKSPACE=") {
			env = append(env, v)
		}
	}
	env = append(env, "FORGECELL_EVAL_WORKSPACE="+workspace)
	r := process.Run(ctx, process.Options{Argv: argv, Dir: filepath.Join(workspace, c.Dir), Env: env, Stdin: []byte{}, Timeout: time.Duration(c.TimeoutMS) * time.Millisecond, MaxOutputBytes: 2_000_000})
	return Observation{c, r}
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
	temp, err := os.MkdirTemp("", "forgecell-eval-index-")
	if err != nil {
		return files, false, "", err
	}
	defer os.RemoveAll(temp)
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
	acceptanceCount := 0
	names := map[string]bool{}
	for _, c := range append(append([]Command{}, p.Setup...), p.Checks...) {
		if c.Name == "" || names[c.Name] || !relative(c.Dir, true) || len(c.Argv) == 0 || c.Argv[0] == "" || c.TimeoutMS < 1 || c.TimeoutMS > 3_600_000 {
			return report, fmt.Errorf("invalid or duplicate evaluation command")
		}
		names[c.Name] = true
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
						if r.Result.Code != 1 || r.Result.TimedOut || r.Result.Interrupted || r.Result.Overflow {
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
	command := func(c Command, category string) readiness.Check {
		argv := append([]string{}, c.Argv...)
		for i, arg := range argv {
			if arg == "{acceptance}" {
				argv[i] = "{input:acceptance-input}"
			}
		}
		provenance := ""
		if category == "independent-acceptance" {
			provenance = "Reviewed frozen evaluation plan SHA-256 " + Hash(planBytes)
		}
		return readiness.Check{ID: c.Name, Category: category, IndependentProvenance: provenance, Argv: argv, Dir: c.Dir, TimeoutMS: c.TimeoutMS, Required: true, Definitions: []string{"evaluation-plan", "acceptance-input"}, Reason: "Frozen evaluation command", Evidence: []string{"issue", "evaluation-plan"}}
	}
	for _, c := range p.Setup {
		o.Setup = append(o.Setup, command(c, "setup"))
	}
	o.Analyze = func(context.Context, formula.Binding, molecule.Issue, readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a := readiness.Analysis{Summary: "Execute the reviewed frozen evaluation task", Acceptance: []readiness.Criterion{{Description: "Pass the frozen independently reviewed acceptance check", Evidence: []string{"issue", "evaluation-plan"}}}}
		for _, file := range p.AllowedFiles {
			a.Scope = append(a.Scope, readiness.ScopedPath{Path: file, Reason: "Explicit frozen evaluation scope", Evidence: []string{"issue", "evaluation-plan"}})
		}
		for _, c := range p.Checks {
			category := "regression"
			if c.Name == "acceptance" {
				category = "independent-acceptance"
			}
			a.Checks = append(a.Checks, command(c, category))
		}
		return a, nil
	}
}
