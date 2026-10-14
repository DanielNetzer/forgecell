package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// maxCohortBytes bounds the reviewed cohort file. The strict JSON decoder rejects
// unknown fields; a larger or malformed file is refused before anything is created.
const maxCohortBytes = 64 * 1024

var (
	cohortVariantName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	cohortTaskName     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	cohortHash         = regexp.MustCompile(`^[a-f0-9]{64}$`)
	canonicalFormulaID = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

// CohortVariant pins one Formula variant by a cohort-relative path and its SHA-256.
type CohortVariant struct {
	Name          string `json:"name"`
	Formula       string `json:"formula"`
	FormulaSHA256 string `json:"formulaSha256"`
}

// CohortTask pins one frozen evaluation Plan file by a cohort-relative path and SHA-256.
type CohortTask struct {
	Name       string `json:"name"`
	Plan       string `json:"plan"`
	PlanSHA256 string `json:"planSha256"`
}

// CohortStop is the optional stopping rule. A nil pointer field is "no limit".
// maxCostUSD counts only reported cost; it never triggers when no cost is reported.
type CohortStop struct {
	MaxAttempts *int     `json:"maxAttempts,omitempty"`
	MaxCostUSD  *float64 `json:"maxCostUSD,omitempty"`
}

// CohortFile is the reviewed cohort definition: strict JSON, at most 64 KiB, unknown
// fields rejected.
type CohortFile struct {
	SchemaVersion  int             `json:"schemaVersion"`
	Hypothesis     string          `json:"hypothesis"`
	PrimaryOutcome string          `json:"primaryOutcome"`
	Variants       []CohortVariant `json:"variants"`
	Tasks          []CohortTask    `json:"tasks"`
	Repetitions    int             `json:"repetitions"`
	Stop           *CohortStop     `json:"stop,omitempty"`
}

type CohortOptions struct {
	Cohort, SourceRoot, Output, Approve string
	// KeepWorkspaces retains each attempt's Lab and workspace trees instead of deleting
	// them once the record, verification observations and captured diff are saved.
	KeepWorkspaces bool
}

// readCohort reads and strictly decodes the cohort file.
func readCohort(path string) ([]byte, CohortFile, error) {
	var c CohortFile
	st, err := os.Lstat(path)
	if err != nil {
		return nil, c, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxCohortBytes {
		return nil, c, fmt.Errorf("cohort file must be a regular file of at most %d bytes", maxCohortBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, c, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return raw, c, fmt.Errorf("invalid cohort file: %w", err)
	}
	if dec.More() {
		return raw, c, fmt.Errorf("cohort file must contain exactly one JSON object")
	}
	return raw, c, nil
}

func validateCohort(c CohortFile) error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("cohort schemaVersion must be 1")
	}
	if utf8.RuneCountInString(c.Hypothesis) > 2000 {
		return fmt.Errorf("hypothesis must be at most 2000 characters")
	}
	switch c.PrimaryOutcome {
	case "hiddenAcceptancePassRate", "requiredChecksPassRate", "costPerAcceptedOutcome":
	default:
		return fmt.Errorf("primaryOutcome must be hiddenAcceptancePassRate, requiredChecksPassRate or costPerAcceptedOutcome")
	}
	if len(c.Variants) != 2 {
		return fmt.Errorf("cohort requires exactly 2 variants")
	}
	seenVariant := map[string]bool{}
	for _, v := range c.Variants {
		if !cohortVariantName.MatchString(v.Name) {
			return fmt.Errorf("variant name %q must match ^[a-z][a-z0-9-]{0,31}$", v.Name)
		}
		if seenVariant[v.Name] {
			return fmt.Errorf("duplicate variant name %q", v.Name)
		}
		seenVariant[v.Name] = true
		if !relative(v.Formula, false) {
			return fmt.Errorf("variant %q formula must be a clean cohort-relative path", v.Name)
		}
		if !cohortHash.MatchString(v.FormulaSHA256) {
			return fmt.Errorf("variant %q formulaSha256 must be a lowercase SHA-256", v.Name)
		}
	}
	// A cohort pinning the same Formula bytes for both variants compares a recipe with
	// itself; reject it before anything is frozen or executed.
	if c.Variants[0].FormulaSHA256 == c.Variants[1].FormulaSHA256 {
		return fmt.Errorf("both variants pin the same Formula bytes; the two variants must be distinct")
	}
	if len(c.Tasks) < 1 || len(c.Tasks) > 50 {
		return fmt.Errorf("cohort requires 1 to 50 tasks")
	}
	seenTask := map[string]bool{}
	for _, t := range c.Tasks {
		if !cohortTaskName.MatchString(t.Name) {
			return fmt.Errorf("task name %q must match ^[a-z0-9][a-z0-9_-]{0,63}$", t.Name)
		}
		if seenTask[t.Name] {
			return fmt.Errorf("duplicate task name %q", t.Name)
		}
		seenTask[t.Name] = true
		if !relative(t.Plan, false) {
			return fmt.Errorf("task %q plan must be a clean cohort-relative path", t.Name)
		}
		if !cohortHash.MatchString(t.PlanSHA256) {
			return fmt.Errorf("task %q planSha256 must be a lowercase SHA-256", t.Name)
		}
	}
	if c.Repetitions < 1 || c.Repetitions > 10 {
		return fmt.Errorf("repetitions must be 1 to 10")
	}
	if c.Stop != nil {
		if c.Stop.MaxAttempts != nil && *c.Stop.MaxAttempts < 1 {
			return fmt.Errorf("stop.maxAttempts must be at least 1")
		}
		if c.Stop.MaxCostUSD != nil && !(*c.Stop.MaxCostUSD > 0) {
			return fmt.Errorf("stop.maxCostUSD must be greater than 0")
		}
	}
	return nil
}

// validatePlan applies the single-task evaluation Plan invariants a cohort task reuses.
func validatePlan(p Plan) error {
	if p.SchemaVersion != 1 || !commitPattern.MatchString(p.BaseCommit) || len(p.AllowedFiles) == 0 || len(p.Checks) == 0 {
		return fmt.Errorf("task plan requires exact base, scope and independent checks")
	}
	if p.ReadinessVersion != 1 || p.TargetBranch == "" {
		return fmt.Errorf("task plan requires readinessVersion 1 and an explicit targetBranch")
	}
	if !relative(p.Task, false) || !relative(p.Acceptance, false) {
		return fmt.Errorf("task plan task and acceptance must be clean plan-relative paths")
	}
	for _, file := range p.AllowedFiles {
		if !relative(file, false) {
			return fmt.Errorf("invalid output scope in task plan")
		}
	}
	for _, file := range p.OriginalCheckFiles {
		if !relative(file, false) {
			return fmt.Errorf("invalid original check file in task plan")
		}
	}
	names := map[string]bool{}
	acceptance := 0
	for _, cmd := range append(append([]Command{}, p.Setup...), p.Checks...) {
		if cmd.Name == "" || names[cmd.Name] || !relative(cmd.Dir, true) || len(cmd.Argv) == 0 || cmd.Argv[0] == "" || cmd.TimeoutMS < 1 || cmd.TimeoutMS > 3_600_000 {
			return fmt.Errorf("invalid or duplicate task plan command")
		}
		names[cmd.Name] = true
	}
	for _, cmd := range p.Checks {
		if cmd.Name == "acceptance" {
			acceptance++
		}
	}
	if acceptance != 1 {
		return fmt.Errorf("one task plan check must be named acceptance")
	}
	return nil
}

// CohortDigest binds the new execution decision to the exact frozen cohort inputs and
// destinations, reusing the exact-approval discipline of the single-task evaluator.
func CohortDigest(cohort []byte, variants [2]formula.Formula, planHashes []string, source, output string) string {
	raw, _ := json.Marshal([]any{"evaluation-cohort-v1", Hash(cohort), variants[0].SHA256, variants[1].SHA256, planHashes, source, output})
	return Hash(raw)
}

type loadedTask struct {
	task           CohortTask
	plan           Plan
	planRaw        []byte
	issueRaw       []byte
	acceptanceRaw  []byte
	issue          molecule.Issue
	planDir        string
	acceptancePath string
}

// RunCohort compares two Formula variants over a task cohort with repetitions. Nothing
// is created or executed before the single cohort approval digest is supplied; an
// existing output with a matching digest resumes and skips attempts already recorded,
// and a mismatched digest is refused.
func RunCohort(ctx context.Context, o CohortOptions) (report CohortReport, err error) {
	if o.Cohort, err = filepath.Abs(o.Cohort); err != nil {
		return report, err
	}
	if o.Output, err = filepath.Abs(o.Output); err != nil {
		return report, err
	}
	if o.SourceRoot, err = filepath.Abs(o.SourceRoot); err != nil {
		return report, err
	}
	raw, c, err := readCohort(o.Cohort)
	if err != nil {
		return report, err
	}
	if err = validateCohort(c); err != nil {
		return report, err
	}
	cohortDir := filepath.Dir(o.Cohort)

	var variants [2]formula.Formula
	for i, v := range c.Variants {
		body, e := frozen(cohortDir, v.Formula, v.FormulaSHA256)
		if e != nil {
			return report, e
		}
		f, e := formula.Parse(body)
		if e != nil {
			return report, fmt.Errorf("variant %q: %w", v.Name, e)
		}
		if !canonicalFormulaID.MatchString(f.ID) {
			return report, fmt.Errorf("variant %q requires a canonical Formula id", v.Name)
		}
		variants[i] = f
	}
	if variants[0].Intake.Repo != variants[1].Intake.Repo {
		return report, fmt.Errorf("both variants must target the same intake repository")
	}
	repo := variants[0].Intake.Repo

	var tasks []loadedTask
	var planHashes []string
	for _, t := range c.Tasks {
		planRaw, e := frozen(cohortDir, t.Plan, t.PlanSHA256)
		if e != nil {
			return report, e
		}
		planHashes = append(planHashes, t.PlanSHA256)
		var p Plan
		if e = json.Unmarshal(planRaw, &p); e != nil {
			return report, fmt.Errorf("task %q: %w", t.Name, e)
		}
		if e = validatePlan(p); e != nil {
			return report, fmt.Errorf("task %q: %w", t.Name, e)
		}
		planDir := filepath.Dir(filepath.Join(cohortDir, t.Plan))
		issueRaw, e := frozen(planDir, p.Task, p.TaskSHA256)
		if e != nil {
			return report, e
		}
		var issue molecule.Issue
		if e = json.Unmarshal(issueRaw, &issue); e != nil {
			return report, fmt.Errorf("task %q: %w", t.Name, e)
		}
		if issue.Number < 1 || issue.Repo != repo || issue.URL != fmt.Sprintf("https://github.com/%s/issues/%d", issue.Repo, issue.Number) {
			return report, fmt.Errorf("task %q frozen issue does not match the variant intake repository", t.Name)
		}
		acceptanceRaw, e := frozen(planDir, p.Acceptance, p.AcceptanceSHA256)
		if e != nil {
			return report, e
		}
		tasks = append(tasks, loadedTask{task: t, plan: p, planRaw: planRaw, issueRaw: issueRaw, acceptanceRaw: acceptanceRaw, issue: issue, planDir: planDir})
	}

	digest := CohortDigest(raw, variants, planHashes, o.SourceRoot, o.Output)
	if o.Approve != digest {
		return report, fmt.Errorf("fresh exact cohort evaluation approval required; nothing was created or executed. Review the frozen cohort:\n%s\nVariant %s SHA-256 %s\nVariant %s SHA-256 %s\n%d task plan(s). Source %q; output %q. Review the cohort, both Formulas and every plan, then run:\nforgecell evaluate --cohort %q --source %q --out %q --approve %s", string(raw), c.Variants[0].Name, variants[0].SHA256, c.Variants[1].Name, variants[1].SHA256, len(c.Tasks), o.SourceRoot, o.Output, o.Cohort, o.SourceRoot, o.Output, digest)
	}

	variantNames := []string{c.Variants[0].Name, c.Variants[1].Name}
	variantHashes := map[string]string{c.Variants[0].Name: variants[0].SHA256, c.Variants[1].Name: variants[1].SHA256}
	taskNames := make([]string, len(tasks))
	for i, lt := range tasks {
		taskNames[i] = lt.task.Name
	}

	resume, err := prepareCohortOutput(o.Output, digest, raw, c, variants, tasks)
	if err != nil {
		return report, err
	}

	decisionAt := time.Now().UTC().Format(time.RFC3339Nano)
	base := CohortReport{
		SchemaVersion:    1,
		ActivationDigest: digest,
		Status:           "running",
		Hypothesis:       c.Hypothesis,
		PrimaryOutcome:   c.PrimaryOutcome,
		Tasks:            len(tasks),
		Repetitions:      c.Repetitions,
		StartedAt:        decisionAt,
		Notes: []string{
			"Counts and rates only. This is paired feasibility evidence, never statistical or causal proof of improvement.",
			"Every rate states its n (tasks, repetitions and the attempts computed over).",
			"Variants are interleaved per task and repetition (A then B) so time-dependent drift affects both; attempts run sequentially in isolated per-attempt Labs.",
			"Reported cost is the #149 usage figure where available (list-price estimates included and labelled); attempts without reported cost read as unreported, never zero.",
			"Amendment counts are a proxy from the readiness plan history (proposals beyond the first).",
			"Independent checks run outside the coding harness sandbox; their results are reported separately.",
		},
	}
	if resume {
		base.Notes = append(base.Notes, "Resumed an existing output with a matching activation digest; attempts already recorded were skipped.")
	}
	report = buildCohortReport(base, variantNames, variantHashes, taskNames, nil)
	if err = saveCohortReport(o.Output, report); err != nil {
		return report, err
	}

	var attempts []CohortAttempt
	// abort records status: aborted and the evidence gathered so far, so an error or a
	// cancellation leaves a durable report that never claims the run completed.
	abort := func(e error) (CohortReport, error) {
		base.Status = "aborted"
		base.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		report = buildCohortReport(base, variantNames, variantHashes, taskNames, attempts)
		if saveErr := saveCohortReport(o.Output, report); saveErr != nil && e == nil {
			e = saveErr
		}
		return report, e
	}
	stopped := ""
loop:
	for _, lt := range tasks {
		for rep := 1; rep <= c.Repetitions; rep++ {
			for vi, vname := range variantNames {
				attemptDir := filepath.Join(o.Output, "attempts", lt.task.Name, vname, fmt.Sprintf("rep-%d", rep))
				recordPath := filepath.Join(attemptDir, "attempt.json")
				if ca, done := loadAttemptRecord(recordPath, lt.task.Name, vname, rep); done {
					attempts = append(attempts, ca)
				} else {
					// Re-hash every input under <out>/inputs (the cohort file, both variant
					// Formula copies and this task's plan, issue and acceptance) against its
					// pinned hash immediately before the attempt; a mismatch refuses the run
					// rather than executing a mutated copy, on both fresh and resumed runs.
					vf, planBytes, acceptBytes, e := verifyAttemptInputs(o.Output, Hash(raw), c.Variants, c.Variants[vi], lt)
					if e != nil {
						return abort(e)
					}
					// Deregister any worktrees a prior crashed or cancelled attempt left at
					// this coordinate before reclaiming the directory, so a re-run allocates
					// cleanly. Targeted removal only; never a repo-wide prune.
					_ = removeAttemptWorktrees(ctx, o.SourceRoot, attemptDir)
					if e := os.RemoveAll(attemptDir); e != nil {
						return abort(e)
					}
					if e := os.MkdirAll(attemptDir, 0700); e != nil {
						return abort(e)
					}
					// Materialise the verified acceptance and plan bytes into a fresh
					// per-attempt read-only path and execute setup, preflight and
					// verification from it, never from the mutable <out>/inputs copy.
					verifiedDir := filepath.Join(attemptDir, "verified")
					if e := os.MkdirAll(verifiedDir, 0700); e != nil {
						return abort(e)
					}
					acceptancePath := filepath.Join(verifiedDir, "acceptance"+filepath.Ext(lt.plan.Acceptance))
					if e := os.WriteFile(acceptancePath, acceptBytes, 0400); e != nil {
						return abort(e)
					}
					if e := os.WriteFile(filepath.Join(verifiedDir, "plan.json"), planBytes, 0400); e != nil {
						return abort(e)
					}
					lt.acceptancePath = acceptancePath
					a, e := runCohortAttempt(ctx, vf, vname, lt, planBytes, acceptBytes, o.SourceRoot, filepath.Join(attemptDir, "lab"), filepath.Join(attemptDir, "verification"), digest, decisionAt)
					if e != nil {
						return abort(e)
					}
					// A cancelled attempt is discarded before it is saved, so resume
					// re-runs this coordinate instead of skipping it as done.
					if ctx.Err() != nil {
						return abort(ctx.Err())
					}
					// Capture the retained diff before the trees are reclaimed. A diff
					// failure is recorded as a changes error, never an abort.
					sha, truncated, changesErr := writeAttemptChanges(ctx, attemptDir, o.SourceRoot, lt.plan.BaseCommit, a.Tree, a.ChangedFiles)
					a.ChangesDiffSHA256 = sha
					a.ChangesTruncated = truncated
					a.ChangesError = changesErr
					ca := CohortAttempt{Task: lt.task.Name, Variant: vname, Repetition: rep, Attempt: a}
					// The record, verification observations and captured diff are saved; by
					// default reclaim the attempt's Lab and workspace trees. A cleanup error
					// is recorded on the attempt (and summarized in the report), not swallowed.
					if !o.KeepWorkspaces {
						if e := cleanupAttemptTrees(ctx, attemptDir, o.SourceRoot); e != nil {
							ca.CleanupError = e.Error()
						}
					}
					if e := saveAttemptRecord(recordPath, ca); e != nil {
						return abort(e)
					}
					attempts = append(attempts, ca)
				}
				report = buildCohortReport(base, variantNames, variantHashes, taskNames, attempts)
				if e := saveCohortReport(o.Output, report); e != nil {
					return abort(e)
				}
				if ctx.Err() != nil {
					return abort(ctx.Err())
				}
				if s := cohortStop(c.Stop, attempts); s != "" {
					stopped = s
					break loop
				}
			}
		}
	}

	base.Status = "complete"
	if stopped != "" {
		base.Status = "stopped"
		base.Stopped = stopped
		// A stop that leaves the two variants with unequal attempt counts makes their
		// rates incomparable; record the imbalance so a reader does not miss it.
		counts := map[string]int{}
		for _, a := range attempts {
			counts[a.Variant]++
		}
		if counts[variantNames[0]] != counts[variantNames[1]] {
			base.Notes = append(base.Notes, fmt.Sprintf("Imbalance: the stop left the variants with unequal attempt counts (%s=%d, %s=%d); rates computed over different n are not directly comparable.", variantNames[0], counts[variantNames[0]], variantNames[1], counts[variantNames[1]]))
		}
	}
	base.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	report = buildCohortReport(base, variantNames, variantHashes, taskNames, attempts)
	err = saveCohortReport(o.Output, report)
	return report, err
}

// verifyAttemptInputs re-hashes every pinned input under <out>/inputs for one attempt and
// returns the verified variant Formula and the plan and acceptance bytes to execute from.
// It re-hashes the cohort file and both variant Formula copies (the shared inputs) as well
// as this task's plan, issue and acceptance. A hash mismatch refuses the attempt so a
// mutated copy is never run.
func verifyAttemptInputs(output, cohortSHA256 string, allVariants []CohortVariant, v CohortVariant, lt loadedTask) (formula.Formula, []byte, []byte, error) {
	inputsDir := filepath.Join(output, "inputs")
	if _, err := frozen(inputsDir, "cohort.json", cohortSHA256); err != nil {
		return formula.Formula{}, nil, nil, err
	}
	for _, cv := range allVariants {
		if _, err := frozen(filepath.Join(inputsDir, "variants"), cv.Name+".yaml", cv.FormulaSHA256); err != nil {
			return formula.Formula{}, nil, nil, err
		}
	}
	vb, err := frozen(filepath.Join(inputsDir, "variants"), v.Name+".yaml", v.FormulaSHA256)
	if err != nil {
		return formula.Formula{}, nil, nil, err
	}
	f, err := formula.Parse(vb)
	if err != nil {
		return formula.Formula{}, nil, nil, err
	}
	taskDir := filepath.Join(inputsDir, "tasks", lt.task.Name)
	planBytes, err := frozen(taskDir, "plan.json", lt.task.PlanSHA256)
	if err != nil {
		return f, nil, nil, err
	}
	if _, err = frozen(taskDir, "issue.json", lt.plan.TaskSHA256); err != nil {
		return f, nil, nil, err
	}
	acceptBytes, err := frozen(taskDir, "acceptance"+filepath.Ext(lt.plan.Acceptance), lt.plan.AcceptanceSHA256)
	if err != nil {
		return f, nil, nil, err
	}
	return f, planBytes, acceptBytes, nil
}

// changesManifest is the retained changed-path set written beside the capped unified diff.
type changesManifest struct {
	ChangedFiles  []string `json:"changedFiles"`
	DiffTruncated bool     `json:"diffTruncated"`
}

// maxChangesDiffBytes caps the retained unified diff; a larger diff is kept as its 5 MiB
// prefix with a truncation marker so the file stays bounded.
const maxChangesDiffBytes = 5 * 1024 * 1024

// diffTruncationMarker is appended to a diff cut at the cap so a reader never mistakes a
// truncated capture for a complete one.
const diffTruncationMarker = "\n*** changes.diff truncated at 5 MiB cap ***\n"

// diffEnv returns an environment that makes git diff independent of any user or system
// configuration, so the captured diff depends only on the pinned bytes.
func diffEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, v := range os.Environ() {
		if strings.HasPrefix(v, "GIT_CONFIG_GLOBAL=") || strings.HasPrefix(v, "GIT_CONFIG_SYSTEM=") || strings.HasPrefix(v, "GIT_CONFIG_NOSYSTEM=") {
			continue
		}
		env = append(env, v)
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
}

// writeAttemptChanges records <attempt>/changes.diff (the unified diff from the base
// commit to the captured tree, capped at 5 MiB) and <attempt>/changes.json (the changed
// paths), so report and any later recheck read these retained files rather than the
// deleted workspace tree. It returns the diff's sha256, whether it was truncated, and a
// non-empty changes-error string when the diff could not be captured. A capture failure
// never aborts the cohort: the (empty or partial) files are still written.
func writeAttemptChanges(ctx context.Context, attemptDir, source, base, tree string, changed []string) (string, bool, string) {
	return writeAttemptChangesCapped(ctx, attemptDir, source, base, tree, changed, maxChangesDiffBytes)
}

func writeAttemptChangesCapped(ctx context.Context, attemptDir, source, base, tree string, changed []string, capBytes int) (string, bool, string) {
	var diff []byte
	truncated := false
	changesErr := ""
	if commitPattern.MatchString(tree) && commitPattern.MatchString(base) {
		// Fixed, configuration-independent invocation (binary-safe, no external or
		// textconv filters, explicit prefixes) so a user's git config can never change the
		// captured diff; capped so a huge diff is truncated and marked, never unbounded.
		r := process.Run(ctx, process.Options{
			Argv:           []string{"git", "-c", "diff.noprefix=false", "diff", "--binary", "--no-ext-diff", "--no-color", base, tree},
			Dir:            source,
			Env:            diffEnv(),
			Stdin:          []byte{},
			Timeout:        60 * time.Second,
			MaxOutputBytes: capBytes,
		})
		switch {
		case r.OK:
			diff = []byte(r.Stdout)
		case r.Overflow || r.Truncated:
			// The diff exceeded the cap: keep the captured prefix and mark it truncated
			// rather than treating the enforced cut-off as a capture failure.
			truncated = true
			diff = append([]byte(r.Stdout), []byte(diffTruncationMarker)...)
		default:
			changesErr = "cannot capture attempt changes diff: " + strings.TrimSpace(r.Stderr)
		}
	} else {
		changesErr = "attempt produced no captured tree to diff"
	}
	if err := os.WriteFile(filepath.Join(attemptDir, "changes.diff"), diff, 0400); err != nil && changesErr == "" {
		changesErr = "cannot write changes.diff: " + err.Error()
	}
	paths := append([]string{}, changed...)
	sort.Strings(paths)
	manifest, _ := json.MarshalIndent(changesManifest{ChangedFiles: paths, DiffTruncated: truncated}, "", "  ")
	if err := os.WriteFile(filepath.Join(attemptDir, "changes.json"), append(manifest, '\n'), 0400); err != nil && changesErr == "" {
		changesErr = "cannot write changes.json: " + err.Error()
	}
	return Hash(diff), truncated, changesErr
}

// resolvePath returns the symlink-resolved absolute path, falling back to the plain
// absolute path when the target cannot be resolved (for example it no longer exists).
func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// removeAttemptWorktrees deregisters every git worktree the shared source object store has
// registered under attemptDir and deletes each one's branch ref. It is targeted to this
// attempt's own paths and never runs a repo-wide prune, so a concurrent session's
// worktrees are never touched. A missing worktree ("not a working tree") is ignored.
func removeAttemptWorktrees(ctx context.Context, source, attemptDir string) error {
	abs := resolvePath(attemptDir)
	list := gitResult(ctx, source, "worktree", "list", "--porcelain")
	if !list.OK {
		return fmt.Errorf("cannot list worktrees: %s", strings.TrimSpace(list.Stderr))
	}
	var problems []string
	var path, branch string
	flush := func() {
		p, br := path, branch
		path, branch = "", ""
		if p == "" {
			return
		}
		// Resolve symlinks on both sides (git may store a realpath, e.g. /private/var vs
		// /var on macOS) so the attempt's own worktrees are matched, and only those.
		ap := resolvePath(p)
		if ap != abs && !strings.HasPrefix(ap, abs+string(os.PathSeparator)) {
			return
		}
		if r := gitResult(ctx, source, "worktree", "remove", "--force", p); !r.OK {
			s := strings.TrimSpace(r.Stderr)
			if !strings.Contains(s, "not a working tree") && !strings.Contains(s, "not a worktree") {
				problems = append(problems, "worktree remove "+p+": "+s)
			}
		}
		if br != "" {
			if r := gitResult(ctx, source, "branch", "-D", br); !r.OK {
				s := strings.TrimSpace(r.Stderr)
				if !strings.Contains(s, "not found") {
					problems = append(problems, "branch -D "+br+": "+s)
				}
			}
		}
	}
	for _, line := range strings.Split(list.Stdout, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "":
			flush()
		}
	}
	flush()
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// cleanupAttemptTrees deletes the attempt's Lab and verification worktrees, deregistering
// them from the shared source object store with targeted removal (never a repo-wide
// prune), and deletes the attempt's own branch refs. The retained record, changes.diff
// and changes.json are left in place. A failure is returned so the caller can record it.
func cleanupAttemptTrees(ctx context.Context, attemptDir, source string) error {
	problems := []string{}
	if e := removeAttemptWorktrees(ctx, source, attemptDir); e != nil {
		problems = append(problems, e.Error())
	}
	if e := os.RemoveAll(filepath.Join(attemptDir, "lab")); e != nil {
		problems = append(problems, e.Error())
	}
	if e := os.RemoveAll(filepath.Join(attemptDir, "verification")); e != nil {
		problems = append(problems, e.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// prepareCohortOutput creates a fresh output directory with pinned input copies, or
// resumes an existing one whose recorded activation digest matches. A mismatch is
// refused so a different cohort can never overwrite retained evidence.
func prepareCohortOutput(output, digest string, raw []byte, c CohortFile, variants [2]formula.Formula, tasks []loadedTask) (bool, error) {
	if _, err := os.Stat(output); err == nil {
		existing, e := bounded(filepath.Join(output, "report.json"))
		if e != nil {
			return false, fmt.Errorf("existing output is not a resumable cohort run: %w", e)
		}
		var prior CohortReport
		if json.Unmarshal(existing, &prior) != nil || prior.ActivationDigest == "" {
			return false, fmt.Errorf("existing output has no recorded activation digest; refusing to reuse it")
		}
		if prior.ActivationDigest != digest {
			return false, fmt.Errorf("existing output has a different activation digest; refuse to mix cohorts")
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return false, err
	}
	inputDir := filepath.Join(output, "inputs")
	if err := os.MkdirAll(filepath.Join(inputDir, "variants"), 0700); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(inputDir, "cohort.json"), raw, 0400); err != nil {
		return false, err
	}
	for i, v := range c.Variants {
		if err := os.WriteFile(filepath.Join(inputDir, "variants", v.Name+".yaml"), []byte(variants[i].YAML), 0400); err != nil {
			return false, err
		}
	}
	for _, lt := range tasks {
		td := filepath.Join(inputDir, "tasks", lt.task.Name)
		if err := os.MkdirAll(td, 0700); err != nil {
			return false, err
		}
		for name, body := range map[string][]byte{"plan.json": lt.planRaw, "issue.json": lt.issueRaw, "acceptance" + filepath.Ext(lt.plan.Acceptance): lt.acceptanceRaw} {
			if err := os.WriteFile(filepath.Join(td, name), body, 0400); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

// loadAttemptRecord reads a recorded attempt; the second return is whether it is a
// complete record for exactly this coordinate. An incomplete, invalid or foreign record
// is treated as not done, so the attempt is re-run from a clean directory.
func loadAttemptRecord(path, task, variant string, rep int) (CohortAttempt, bool) {
	raw, err := bounded(path)
	if err != nil {
		return CohortAttempt{}, false
	}
	var ca CohortAttempt
	if json.Unmarshal(raw, &ca) != nil {
		return CohortAttempt{}, false
	}
	if ca.Task != task || ca.Variant != variant || ca.Repetition != rep || ca.StartedAt == "" || ca.ElapsedMS == 0 {
		return CohortAttempt{}, false
	}
	return ca, true
}

func saveAttemptRecord(path string, ca CohortAttempt) error {
	raw, err := json.MarshalIndent(ca, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".attempt-")
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
	return os.Rename(f.Name(), path)
}

// cohortStop evaluates the stop rule after an attempt. maxCostUSD sums only reported
// cost and never triggers when nothing reported a cost.
func cohortStop(stop *CohortStop, attempts []CohortAttempt) string {
	if stop == nil {
		return ""
	}
	if stop.MaxAttempts != nil && len(attempts) >= *stop.MaxAttempts {
		return fmt.Sprintf("maxAttempts reached (%d)", *stop.MaxAttempts)
	}
	if stop.MaxCostUSD != nil {
		reported := false
		total := 0.0
		for _, a := range attempts {
			if c := reportedCost(a.Attempt); c != nil {
				reported = true
				total += *c
			}
		}
		if reported && total >= *stop.MaxCostUSD {
			return fmt.Sprintf("maxCostUSD reached ($%.6f of $%.6f reported; basis %s)", total, *stop.MaxCostUSD, costBasisLabel(attempts))
		}
	}
	return ""
}

// costBasisLabel names the cost basis the reported figures rest on: a single shared
// label, "mixed" when attempts disagree, or "unlabelled" when none recorded one.
func costBasisLabel(attempts []CohortAttempt) string {
	bases := map[string]bool{}
	for _, a := range attempts {
		for _, h := range a.Molecule.HarnessAttempts {
			if h.Usage != nil && h.Usage.CostBasis != "" {
				bases[h.Usage.CostBasis] = true
			}
		}
	}
	switch len(bases) {
	case 0:
		return "unlabelled"
	case 1:
		for b := range bases {
			return b
		}
	}
	return "mixed"
}

// runCohortAttempt activates one variant Formula in its own isolated Lab, runs the
// frozen task through the existing Molecule engine, captures the output tree and grades
// it with the existing independent verifier. A failed or timed-out run is recorded in
// the returned Attempt (never retried); only an unrecoverable infrastructure error is
// returned as a Go error.
func runCohortAttempt(ctx context.Context, f formula.Formula, variantName string, lt loadedTask, planBytes, acceptBytes []byte, source, labDir, verifyDir, digest, decisionAt string) (Attempt, error) {
	started := time.Now()
	p := lt.plan
	a := Attempt{Variant: variantName, FormulaSHA256: f.SHA256, StartedAt: started.UTC().Format(time.RFC3339Nano), Checks: []Observation{}, Setup: []Observation{}}
	if err := os.MkdirAll(filepath.Join(labDir, "formulas"), 0700); err != nil {
		return a, err
	}
	if err := os.WriteFile(filepath.Join(labDir, "formulas", f.ID+".yaml"), []byte(f.YAML), 0400); err != nil {
		return a, err
	}
	lock := filepath.Join(labDir, ".formula-write.lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return a, err
	}
	aRecord := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: "cohort-" + digest + "-" + variantName, SourceKind: "evaluation-activation", SourceID: digest}
	activateErr := func() error {
		defer os.Remove(lock)
		if e := formula.BeginActivation(labDir, f.YAML, aRecord, "absent", f.SHA256); e != nil {
			return e
		}
		if e := formula.ApplyActivation(labDir, aRecord); e != nil {
			return e
		}
		decision, _ := json.Marshal(map[string]string{"digest": digest, "approvedAt": decisionAt, "variant": variantName, "task": lt.task.Name, "formulaSha256": f.SHA256, "sourceRoot": source})
		file, e := os.OpenFile(filepath.Join(labDir, "evaluation-decision.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
		d, e := os.Open(labDir)
		if e != nil {
			return e
		}
		e = d.Sync()
		d.Close()
		if e != nil {
			return e
		}
		return formula.CompleteActivation(labDir, aRecord)
	}()
	if activateErr != nil {
		// A variant whose exact bytes cannot be activated is a recorded failure, not a
		// silent retry; the run continues with the next attempt.
		a.Error = activateErr.Error()
		a.ElapsedMS = time.Since(started).Milliseconds()
		return a, nil
	}

	runOptions := molecule.Options{
		LabDir:     labDir,
		SourceRoot: source,
		Issue:      fmt.Sprint(lt.issue.Number),
		Base:       p.BaseCommit,
		ReadIssue:  func(context.Context, string, int64, string) (molecule.Issue, error) { return lt.issue, nil },
		PrepareWorkspace: func(ctx context.Context, w molecule.Workspace) error {
			for _, cmd := range p.Setup {
				r := observe(ctx, cmd, w.Path, lt.acceptancePath)
				a.Setup = append(a.Setup, r)
				if !r.Result.OK {
					return fmt.Errorf("setup %s failed", cmd.Name)
				}
			}
			clean := gitResult(ctx, w.Path, "status", "--porcelain")
			if !clean.OK || len(clean.Stdout) != 0 {
				return fmt.Errorf("setup changed tracked or untracked repository files")
			}
			if p.RequireInitialFailure {
				for _, cmd := range p.Checks {
					if cmd.Name == "acceptance" {
						r := observe(ctx, cmd, w.Path, lt.acceptancePath)
						a.Preflight = append(a.Preflight, r)
						if r.Result.Code != 1 || r.Result.Incomplete() {
							return fmt.Errorf("original snapshot did not produce expected acceptance failure")
						}
					}
				}
			}
			return nil
		},
	}
	configureReadiness(&runOptions, p, planBytes, acceptBytes)

	var err error
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
	done := completed(a)
	if a.Molecule.Workspace.Path != "" {
		var treeErr error
		a.ChangedFiles, a.ScopeOK, a.Tree, treeErr = outputTree(ctx, a.Molecule.Workspace, p.AllowedFiles)
		if treeErr != nil {
			a.Error = treeErr.Error()
		}
	}
	if a.Error == "" {
		a.Verification = verify(ctx, source, verifyDir, lt.acceptancePath, p, a)
		a.Checks = a.Verification.Checks
		a.Correct = done && a.Verification.Correct
		if a.Verification.Error != "" {
			a.Error = a.Verification.Error
		}
	}
	// Grade the required (non-acceptance) plan checks once, with the plan in hand, and
	// persist the tri-state so a resumed report never needs the plan to aggregate it.
	a.RequiredChecks = requiredChecksTriState(p, a)
	a.ElapsedMS = time.Since(started).Milliseconds()
	return a, nil
}
