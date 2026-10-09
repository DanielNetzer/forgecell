package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

func bindingHash(b formula.Binding) (string, error) {
	fingerprint, err := harness.Fingerprint(b.Command)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(struct {
		Binding     formula.Binding
		Fingerprint string
	}{b, fingerprint})
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}
func issueInput(i Issue) readiness.Issue {
	return readiness.Issue{Repository: i.Repo, Number: i.Number, URL: i.URL, State: i.State, Title: i.Title, Body: i.Body, UpdatedAt: i.UpdatedAt}
}
func atom(r *Record, index int, status, detail string) {
	a := &r.Atoms[index]
	a.Status = status
	a.Detail = detail
	if a.StartedAt == "" {
		a.StartedAt = stamp()
	}
	if status != "waiting" && status != "active" {
		a.FinishedAt = stamp()
	}
}
func stopIntake(r Record, reason string) (Record, error) {
	atom(&r, 0, "blocked", reason)
	r.Status = "blocked"
	r.FinishedAt = stamp()
	for i := 1; i < len(r.Atoms); i++ {
		atom(&r, i, "skipped", "Intake stopped before coding.")
	}
	r.Notes = append(r.Notes, reason)
	return r, save(r)
}
func Run(ctx context.Context, o Options) (r Record, err error) {
	o.LabDir, err = filepath.Abs(o.LabDir)
	if err != nil {
		return r, err
	}
	loaded, err := formula.Load(o.LabDir, o.FormulaID)
	if err != nil {
		return r, err
	}
	if !loaded.Approved {
		return r, fmt.Errorf("approve a Formula with init before running")
	}
	f := loaded.Formula
	if err = f.ValidateReadiness(); err != nil {
		return r, err
	}
	if f.Intake.Source != "github-issues" {
		return r, fmt.Errorf("unsupported intake source")
	}
	ref, err := intake.ParseIssueRef(o.Issue)
	if err != nil {
		return r, err
	}
	ref, err = intake.BindRepository(ref, f.Intake.Repo)
	if err != nil {
		return r, err
	}
	if o.Approve != "" {
		return resume(ctx, o, loaded, ref.Number, ref.Owner+"/"+ref.Repo)
	}
	w, err := resolveSource(ctx, o, ref.Owner+"/"+ref.Repo)
	if err != nil {
		return r, err
	}
	nonce, err := token()
	if err != nil {
		return r, err
	}
	r = Record{SchemaVersion: "v0", Kind: "molecule", ID: fmt.Sprintf("mol-%d-%s", ref.Number, nonce), Mode: "ticket", Status: "running", FormulaID: f.ID, FormulaApproved: true, FormulaSnapshot: Snapshot{f.YAML, f.SHA256}, StartedAt: stamp(), LabDir: o.LabDir, Workspace: w, Issue: Issue{Number: ref.Number, Repo: w.Repo}, Notes: []string{"Intake reads the Issue body, not comments. Clarify the body and run again if blocked.", "Scope approval never authorizes publication, merge, deployment or issue comments."}}
	r.CheckoutStatus = "absent"
	r.ClarificationPredecessors, r.ClarificationHistoryTruncated, err = clarificationHistory(o.LabDir, w.Repo, ref.Number)
	if err != nil {
		return r, err
	}
	for _, a := range f.Atoms {
		r.Atoms = append(r.Atoms, Atom{ID: a.ID, Type: a.Type, Status: "pending", Detail: "Not started"})
	}
	if err = save(r); err != nil {
		return r, err
	}
	reader := o.ReadIssue
	if reader == nil {
		reader = readIssue
	}
	issue, err := reader(ctx, w.Repo, ref.Number, w.SourceRoot)
	if err != nil {
		return stopIntake(r, err.Error())
	}
	if issue.Number != ref.Number || issue.Repo != w.Repo || issue.URL != fmt.Sprintf("https://github.com/%s/issues/%d", w.Repo, ref.Number) || issue.State != "OPEN" {
		return stopIntake(r, "Issue is closed or does not match the requested repository and number.")
	}
	r.Issue = issue
	snapshot, err := readiness.CollectForTicket(ctx, w.SourceRoot, w.BaseCommit, issue.Title+"\n"+issue.Body)
	if err != nil {
		return stopIntake(r, err.Error())
	}
	r.RepositoryEvidence = &snapshot
	target := o.TargetBranch
	if target == "" {
		target, err = git(ctx, w.SourceRoot, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			return stopIntake(r, "Target branch is unknown. Run again with --target BRANCH; it is part of the scope approval.")
		}
		target = strings.TrimPrefix(target, "origin/")
	}
	if o.ReadIssue == nil {
		policy := readiness.ReadRemotePolicy(ctx, w.Repo, target, w.SourceRoot, nil)
		r.RemotePolicy = &policy
	}
	analysis := o.Analyze
	if analysis == nil {
		analysis = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
			// Resolve configuration from the source checkout, then Execute constrains the
			// provider and moves analysis to a temporary directory before the model runs.
			result := invokeHarness(ctx, b.Command, w.SourceRoot, time.Duration(b.TimeoutMS)*time.Millisecond, map[string]any{"kind": "ticket-analysis", "issue": i, "repository": s, "humanVerification": readiness.VerificationInputs{CheckInputs: o.CheckInputs, Checks: o.Checks, Setup: o.Setup, Artifacts: o.Artifacts, Policy: o.Policy}})
			if !result.OK {
				return readiness.Analysis{}, fmt.Errorf("analysis failed: %s %s", result.Error, result.Stderr)
			}
			return readiness.DecodeAnalysis([]byte(result.Stdout))
		}
	}

	var a readiness.Analysis
	for ordinal := 1; ordinal <= 2; ordinal++ {
		if err = formula.Revalidate(o.LabDir, loaded); err != nil {
			return stopIntake(r, err.Error())
		}
		digest, e := snapshot.Digest()
		if e != nil {
			return stopIntake(r, e.Error())
		}
		r.RepositoryEvidence = &snapshot
		r.AnalysisAttempts = append(r.AnalysisAttempts, AnalysisAttempt{Ordinal: ordinal, SnapshotSHA256: digest, StartedAt: stamp(), Status: "started"})
		if err = save(r); err != nil {
			return r, err
		}
		a, err = analysis(ctx, f.Harness, issue, snapshot)
		attempt := &r.AnalysisAttempts[len(r.AnalysisAttempts)-1]
		attempt.FinishedAt = stamp()
		if err != nil {
			attempt.Status = "failed"
			attempt.Error = err.Error()
		} else {
			attempt.Status = "completed"
			response := a
			attempt.Response = &response
			r.Analysis = &a
		}
		if e = save(r); e != nil {
			return r, e
		}
		if err != nil {
			return stopIntake(r, err.Error())
		}
		if err = formula.Revalidate(o.LabDir, loaded); err != nil {
			return stopIntake(r, err.Error())
		}
		if err = readiness.ValidateEvidenceRequests(a.EvidenceRequests, snapshot.References()); err != nil {
			return stopIntake(r, err.Error())
		}
		if len(a.EvidenceRequests) == 0 {
			break
		}
		if ordinal == 2 {
			return stopIntake(r, "Supplemental evidence pass exhausted; unresolved evidence requests remain. No coding harness was invoked.")
		}
		snapshot, err = readiness.CollectSupplemental(ctx, w.SourceRoot, snapshot, a.EvidenceRequests)
		if err != nil {
			return stopIntake(r, err.Error())
		}
		r.RepositoryEvidence = &snapshot
		if err = save(r); err != nil {
			return r, err
		}
	}
	if len(a.Questions) > 0 {
		return stopIntake(r, "Clarification required. No coding harness was invoked.")
	}
	evidenceHash, err := snapshot.Digest()
	if err != nil {
		return stopIntake(r, err.Error())
	}
	bindingDigest, err := bindingHash(f.Harness)
	if err != nil {
		return stopIntake(r, err.Error())
	}
	p := readiness.Plan{SchemaVersion: "v1", MoleculeID: r.ID, Revision: 1, Inputs: readiness.Inputs{Repository: w.Repo, TargetBranch: target, BaseCommit: w.BaseCommit, FormulaSHA256: f.SHA256, EvidenceSHA256: evidenceHash, BindingSHA256: bindingDigest, Issue: issueInput(issue), MandatoryConstraints: o.MandatoryConstraints}, Evidence: snapshot.References(), Analysis: a, Policy: readiness.ExecutionPolicy{Environment: []string{"PATH"}, SetupNetwork: "unrestricted", CheckNetwork: "unrestricted", Containment: "filtered-environment"}}
	p.Analysis.Checks = append(p.Analysis.Checks, o.Checks...)
	if o.Policy != nil {
		p.Policy = *o.Policy
	}
	p.CheckInputs = o.CheckInputs
	p.Setup = o.Setup
	p.Artifacts = o.Artifacts
	for _, input := range o.CheckInputs {
		p.Evidence = append(p.Evidence, input.Evidence)
	}
	if err = snapshot.ValidateScope(a.Scope); err != nil {
		return stopIntake(r, err.Error())
	}
	state, err := readiness.NewState(p, stamp())
	if err != nil {
		return stopIntake(r, err.Error())
	}
	r.Readiness = &state
	for _, overlap := range p.DefinitionOverlaps() {
		r.Notes = append(r.Notes, fmt.Sprintf("Scope/check overlap %s [%s] checks %s: %s", overlap.Path, overlap.Kind, strings.Join(overlap.CheckIDs, ", "), overlap.Detail))
	}
	atom(&r, 0, "done", a.Summary)
	atom(&r, 1, "waiting", "Review exact scope, checks and process limits before approving the digest.")
	return allocateRecord(ctx, o, r, allocateWorkspace, save)
}

// Persist the exact destination before any checkout side effect. A failed final
// save leaves the allocating record discoverable and unable to authorize coding.
func allocateRecord(ctx context.Context, o Options, r Record, allocate func(context.Context, Workspace) error, persist func(Record) error) (Record, error) {
	w, err := planWorkspace(o, r.Workspace, r.Issue.Number)
	if err != nil {
		return stopIntake(r, err.Error())
	}
	r.Workspace = w
	r.Status = "allocating"
	r.CheckoutStatus = "allocating"
	if err = persist(r); err != nil {
		return r, err
	}
	if err = allocate(ctx, w); err != nil {
		r.CheckoutStatus = "failed"
		return stopIntake(r, "Coding checkout allocation failed: "+err.Error())
	}
	r.CheckoutStatus = "available"
	r.Status = "waiting"
	if err = persist(r); err != nil {
		// JSON may have been replaced before Markdown persistence failed. Restore
		// the non-runnable allocation state while retaining its exact destination.
		r.CheckoutStatus = "allocating"
		r.Status = "allocating"
		if recoveryErr := save(r); recoveryErr != nil {
			return r, fmt.Errorf("allocation persistence failed: %w; retaining allocation state: %v", err, recoveryErr)
		}
		return r, err
	}
	return r, nil
}

func findPending(lab, digest string) (Record, error) {
	var found Record
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return found, fmt.Errorf("invalid approval digest")
	}
	entries, err := os.ReadDir(filepath.Join(lab, "ledgers"))
	if err != nil {
		return found, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return found, err
		}
		if !info.Mode().IsRegular() || info.Size() > 5_000_000 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(lab, "ledgers", entry.Name()))
		if err != nil {
			return found, err
		}
		var r Record
		if json.Unmarshal(raw, &r) != nil || r.Readiness == nil {
			continue
		}
		plans := r.Readiness.Plans
		if len(plans) == 0 || plans[len(plans)-1].Digest != digest {
			continue
		}
		if entry.Name() != r.ID+".json" || strings.ContainsAny(r.ID, "/\\") || r.LabDir != lab {
			return found, fmt.Errorf("ledger identity mismatch")
		}
		if found.ID != "" {
			return found, fmt.Errorf("ambiguous approval identity")
		}
		found = r
	}
	if found.ID == "" {
		return found, fmt.Errorf("no Molecule matches this approval digest")
	}
	if found.Workspace.Path == "" || (found.CheckoutStatus != "" && found.CheckoutStatus != "available") {
		return found, fmt.Errorf("coding checkout unavailable; refresh intake")
	}
	return found, nil
}
func resume(ctx context.Context, o Options, expected formula.Loaded, number int64, repo string) (r Record, err error) {
	f := expected.Formula
	r, err = findPending(o.LabDir, o.Approve)
	if err != nil {
		return r, err
	}
	lock := filepath.Join(o.LabDir, "ledgers", r.ID+".lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		return r, fmt.Errorf("Molecule is locked; inspect an interrupted attempt before recovery: %w", err)
	}
	defer os.Remove(lock)
	owner := filepath.Join(lock, "owner")
	if err = os.WriteFile(owner, []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
		return r, err
	}
	defer os.Remove(owner)
	r, err = findPending(o.LabDir, o.Approve)
	if err != nil {
		return r, err
	}
	if r.Issue.Number != number || r.Issue.Repo != repo || r.FormulaID != f.ID || r.FormulaSnapshot.SHA256 != f.SHA256 {
		return r, fmt.Errorf("issue or Formula changed; refresh intake")
	}
	if err = ValidateVerificationHistory(r); err != nil {
		return r, err
	}
	if err = confirmVerificationProcessesStopped(r); err != nil {
		return r, err
	}
	p := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
	if err = p.ValidateForCoding(); err != nil {
		return r, err
	}
	if err = validateRepairHistory(r.Readiness.Plans); err != nil {
		return r, err
	}
	if o.TargetBranch != "" && o.TargetBranch != p.Inputs.TargetBranch {
		return r, fmt.Errorf("target branch changed")
	}
	base := o.Base
	if base == "" {
		base = "HEAD"
	}
	if strings.HasPrefix(base, "-") {
		return r, fmt.Errorf("invalid base")
	}
	currentBase, err := git(ctx, r.Workspace.SourceRoot, "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil || currentBase != r.Workspace.BaseCommit {
		return r, fmt.Errorf("source base changed; refresh intake")
	}
	for _, dir := range []string{r.Workspace.SourceRoot, r.Workspace.Path} {
		origin, e := git(ctx, dir, "remote", "get-url", "origin")
		m := originPattern.FindStringSubmatch(origin)
		if e != nil || m == nil || !strings.EqualFold(strings.TrimSuffix(m[1], ".git"), repo) {
			return r, fmt.Errorf("repository origin changed")
		}
	}
	branch, err := git(ctx, r.Workspace.Path, "branch", "--show-current")
	if err != nil || branch != r.Workspace.Branch {
		return r, fmt.Errorf("workspace branch changed")
	}
	allowed := []string{}
	for _, item := range p.Analysis.Scope {
		allowed = append(allowed, item.Path)
	}
	captured, err := verification.Capture(ctx, r.Workspace.Path, p.Inputs.BaseCommit, allowed, p.Artifacts)
	if err != nil || len(captured.Violations) > 0 || (p.Revision == 1 && len(captured.Changes) > 0) || (p.Revision > 1 && (captured.Tree != p.PausedTree || (p.PausedArtifactsSHA256 != "" && artifactHash(captured) != p.PausedArtifactsSHA256))) {
		return r, fmt.Errorf("pending workspace changed; no coding was started")
	}
	reader := o.ReadIssue
	if reader == nil {
		reader = readIssue
	}
	issue, err := reader(ctx, repo, number, r.Workspace.Path)
	if err != nil {
		return r, err
	}
	snapshot, err := readiness.CollectForTicket(ctx, r.Workspace.SourceRoot, r.Workspace.BaseCommit, issue.Title+"\n"+issue.Body)
	if err != nil {
		return r, err
	}
	if r.RepositoryEvidence != nil {
		for _, pass := range r.RepositoryEvidence.Collection {
			if pass.Supplemental {
				snapshot, err = readiness.CollectSupplemental(ctx, r.Workspace.SourceRoot, snapshot, pass.Requests)
				if err != nil {
					return r, err
				}
			}
		}
	}
	hash, err := snapshot.Digest()
	if err != nil {
		return r, err
	}
	current := p.Inputs
	current.Issue = issueInput(issue)
	current.FormulaSHA256 = f.SHA256
	current.EvidenceSHA256 = hash
	current.BindingSHA256, err = bindingHash(f.Harness)
	if err != nil {
		return r, err
	}
	if len(p.Inputs.MandatoryConstraints) > 0 {
		policy := readiness.ReadRemotePolicy(ctx, repo, p.Inputs.TargetBranch, r.Workspace.SourceRoot, nil)
		if err = policy.ValidateRequirements(p.Inputs.MandatoryConstraints); err != nil {
			return r, err
		}
		r.RemotePolicy = &policy
	}
	if err = formula.Revalidate(o.LabDir, expected); err != nil {
		return r, err
	}
	state, err := r.Readiness.Approve(o.Approve, current, stamp())
	if err != nil {
		return r, err
	}
	if p.Continuation != "" {
		state, err = state.StartChecks(stamp())
	} else {
		state, err = state.StartAttempt(stamp())
	}
	if err != nil {
		return r, err
	}
	r.Readiness = &state
	atom(&r, 1, "done", "Exact readiness plan approved; no publication authorized.")
	if p.Continuation == "" {
		atom(&r, 2, "active", "Coding approved scope")
	}
	r.Status = "running"
	if err = save(r); err != nil {
		return r, err
	}
	if p.Continuation != "" {
		if (p.Continuation == "checks-only" && !completedCoding(r)) || (p.Continuation == "adopt-failed-tree" && !FailedTreeAdopted(r)) || r.Capture == nil || r.Capture.Tree != captured.Tree {
			return r, fmt.Errorf("missing retained completed coding output")
		}
		return verifyCaptured(ctx, o, r, p, captured)
	}
	if o.PrepareWorkspace != nil {
		if err = o.PrepareWorkspace(ctx, r.Workspace); err != nil {
			return finishFailed(r, "Preparation failed: "+err.Error())
		}
	}
	if err = formula.Revalidate(o.LabDir, expected); err != nil {
		return finishFailed(r, err.Error())
	}
	if len(r.HarnessAttempts) >= 100 {
		return finishFailed(r, "harness history capacity reached; no process started")
	}
	attemptID, err := token()
	if err != nil {
		return r, err
	}
	r.HarnessAttempts = append(r.HarnessAttempts, HarnessAttempt{ID: attemptID, PlanDigest: o.Approve})
	if err = save(r); err != nil {
		return r, err
	}
	result := invokeHarness(ctx, f.Harness.Command, r.Workspace.Path, time.Duration(f.Harness.TimeoutMS)*time.Millisecond, map[string]any{"kind": "molecule", "moleculeId": r.ID, "formulaId": f.ID, "recipeInstructions": f.Harness.Instructions, "issue": r.Issue, "approvedPlan": p, "codingOutcomeSchema": json.RawMessage(harness.CodingOutcomeSchema()), "scopeChangeProtocol": `If blocked by scope, return only JSON {"schemaVersion":"v1","outcome":"scope-change","reason":"why","paths":["exact/path"]}; do not write those new paths.`, "instruction": "Implement only the approved exact scope. Do not commit, push, merge, deploy or comment. Stop and report a scope-change request if the plan is insufficient. Return only JSON with schemaVersion (v1), outcome (completed, blocked, scope-change, or no-change), reason (bounded account of changes, validation, or blocker), and paths (exact requested paths for scope-change, otherwise an empty array), matching codingOutcomeSchema. No-change requires allowNoChange in the exact approved plan. Checks are executed independently by the Lab."})
	r.Atoms[2].ExitCode = &result.Code
	r.Atoms[2].ElapsedMS = result.ElapsedMS

	a := &r.HarnessAttempts[len(r.HarnessAttempts)-1]
	coding := harness.CodingOutcomeFromResult(result.Stdout)
	a.Coding = &coding
	ref, err := historyFile(r, "harness-"+attemptID+"-result.json", struct {
		process.Result
		AttemptID  string `json:"attemptId"`
		PlanDigest string `json:"planDigest"`
	}{result, attemptID, o.Approve})
	if err != nil {
		return r, err
	}
	a.Result = result
	a.Result.Stdout, a.Result.Stderr, a.Result.RawStdout = "", "", ""
	a.Evidence = &ref
	if err = save(r); err != nil {
		return r, err
	}
	captureCtx, cancelCapture := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	capture, captureErr := verification.Capture(captureCtx, r.Workspace.Path, p.Inputs.BaseCommit, allowed, p.Artifacts)
	cancelCapture()
	if captureErr != nil {
		a.CaptureError = captureErr.Error()
	}
	captureRef, err := historyFile(r, "harness-"+attemptID+"-capture.json", struct {
		verification.CaptureResult
		CaptureError string `json:"captureError,omitempty"`
	}{capture, a.CaptureError})
	if err != nil {
		return r, err
	}
	a.CaptureEvidence = append(a.CaptureEvidence, captureRef)
	a.Capture = &capture
	r.Capture = &capture
	r.Captures = append(r.Captures, capture)
	if err = save(r); err != nil {
		return r, err
	}
	if !result.OK {
		r, err = finishFailed(r, harnessOutcomeSummary(result, ref))
		if err != nil {
			return r, err
		}
		if len(capture.Violations) > 0 {
			r.Readiness.Phase = "scope-violation"
			r.Readiness.Events = append(r.Readiness.Events, readiness.Event{Kind: "attempt-scope-violation", Digest: o.Approve, At: stamp(), Tree: capture.Tree, Detail: "Failed harness wrote outside approved scope; original failure retained."})
		}
		return r, save(r)
	}
	if captureErr != nil || !result.Reconciled || len(capture.Violations) > 0 {
		detail := fmt.Sprintf("Scope audit failed: %v; process uncertainty: %s; observed violations: %v", captureErr, result.Uncertainty, capture.Violations)
		if coding.Outcome == "blocked" || coding.Outcome == "unknown" || coding.Outcome == "scope-change" {
			state, e := r.Readiness.FinishAttempt(coding.Outcome, coding.Reason, stamp())
			if e != nil {
				return r, e
			}
			state.Phase = "scope-violation"
			state.Events = append(state.Events, readiness.Event{Kind: "attempt-scope-violation", Digest: o.Approve, At: stamp(), Tree: capture.Tree, Detail: detail})
			r.Readiness = &state
			atom(&r, 2, "failed", coding.Outcome+": "+coding.Reason+"; "+detail)
			return finishStopped(r, detail)
		}
		state, e := r.Readiness.FinishAttempt("scope-violation", detail, stamp())
		if e != nil {
			return r, e
		}
		state.Events[len(state.Events)-1].Tree = capture.Tree
		r.Readiness = &state
		atom(&r, 2, "failed", detail)
		return finishStopped(r, detail)
	}
	// Preserve the validated report independently of capture/policy decisions.
	phase, detail := coding.Outcome, coding.Reason
	if phase == "completed" && len(capture.Changes) == 0 {
		phase = "unknown"
		detail = "Completed coding reported without captured source edits."
	}
	if phase == "no-change" && (!p.AllowNoChange || len(capture.Changes) != 0) {
		phase = "unknown"
		detail = "No-change requires exact approved permission and no source edits."
	}
	if phase != "completed" && phase != "no-change" {
		state, e := r.Readiness.FinishAttempt(phase, detail, stamp())
		if e != nil {
			return r, e
		}
		r.Readiness = &state
		atom(&r, 2, "failed", phase+": "+detail)
		return finishStopped(r, phase+": "+detail+" Retained tree: "+capture.Tree)
	}
	atom(&r, 2, "done", coding.Outcome+": "+coding.Reason)
	state, err = r.Readiness.FinishAttempt("check-pending", "Coding output captured: "+capture.Tree, stamp())
	if err != nil {
		return r, err
	}
	r.Readiness = &state
	return verifyCaptured(ctx, o, r, p, capture)
}

func verifyCaptured(ctx context.Context, o Options, r Record, p readiness.Plan, capture verification.CaptureResult) (Record, error) {
	if err := beginVerification(&r, p, capture.Tree); err != nil {
		return r, err
	}
	atom(&r, 3, "active", "Running approved checks in a fresh checkout")
	if err := save(r); err != nil {
		return r, err
	}
	destination := filepath.Join(o.LabDir, "verification", fmt.Sprintf("%s-r%d", r.ID, p.Revision))
	verified := verification.RunObserved(ctx, r.Workspace.SourceRoot, destination, p, capture, func(obs verification.Observation) error { return recordObservation(&r, obs) })
	if err := completeVerification(&r, verified); err != nil {
		return r, err
	}
	projection := compactVerification(verified)
	r.Verification = &projection
	state, err := r.Readiness.RecordVerification(verified.RequiredChecksPassed, verified.Error, stamp())
	if err != nil {
		return r, err
	}
	r.Readiness = &state
	if !verified.RequiredChecksPassed {
		atom(&r, 3, "failed", verified.Error)
		return finishStopped(r, "Required verification did not pass.")
	}
	atom(&r, 3, "done", "Required checks passed on "+verified.SourceTree+". Independent acceptance verified: "+fmt.Sprint(verified.IndependentAcceptanceVerified))
	atom(&r, 4, "waiting", "Human review required before explicit delivery approval.")
	for i := 5; i < len(r.Atoms); i++ {
		atom(&r, i, "skipped", "Publication is a separate approved action.")
	}
	r.Status = "waiting"
	r.FinishedAt = stamp()
	err = save(r)
	return r, err
}
func finishStopped(r Record, detail string) (Record, error) {
	r.Status = "blocked"
	r.FinishedAt = stamp()
	r.Notes = append(r.Notes, detail)
	for i := 3; i < len(r.Atoms); i++ {
		if r.Atoms[i].Status == "pending" || r.Atoms[i].Status == "active" {
			atom(&r, i, "skipped", detail)
		}
	}
	return r, save(r)
}
func finishFailed(r Record, detail string) (Record, error) {
	state, err := r.Readiness.FinishAttempt("failed", detail, stamp())
	if err != nil {
		return r, err
	}
	r.Readiness = &state
	atom(&r, 2, "failed", detail)
	r, err = finishStopped(r, detail)
	if err != nil {
		return r, err
	}
	r.Status = "failed"
	return r, save(r)
}

// Streams belong to immutable evidence; ledger projections must remain bounded
// even when JSON escaping expands control characters in captured output.
func harnessOutcomeSummary(result process.Result, ref VerificationFile) string {
	message := result.Error
	if len(message) > 512 {
		message = message[:512] + " (truncated; see evidence)"
	}
	return fmt.Sprintf("Harness failed: exit=%d elapsedMs=%d timeout=%t interrupted=%t overflow=%t reconciled=%t; %s. Complete outcome: %s (SHA-256 %s).", result.Code, result.ElapsedMS, result.TimedOut, result.Interrupted, result.Overflow, result.Reconciled, message, ref.Path, ref.SHA256)
}

// An explicit checker repair cannot shed earlier protected gates or rebind their
// baseline hashes. Historical plans are immutable; approval checks every link.
func validateRepairHistory(plans []readiness.Proposal) error {
	repair := false
	for i := 1; i < len(plans); i++ {
		previous, next := plans[i-1].Plan, plans[i].Plan
		for _, p := range []readiness.Plan{previous, next} {
			for _, c := range p.Analysis.Checks {
				if c.ReviewedAcceptance != "" {
					repair = true
				}
			}
		}
		if !repair {
			continue
		}
		for _, old := range previous.Analysis.Checks {
			if old.Category != "regression" || !old.Required {
				continue
			}
			retained := false
			for _, current := range next.Analysis.Checks {
				if current.ID != old.ID {
					continue
				}
				before, after := old, current
				before.ReviewedAcceptance, after.ReviewedAcceptance = "", ""
				if !reflect.DeepEqual(before, after) {
					return fmt.Errorf("definition repair must retain exact protected gate %s", old.ID)
				}
				retained = true
			}
			if !retained {
				return fmt.Errorf("definition repair removed protected gate %s", old.ID)
			}
			for _, id := range old.Definitions {
				var before, after readiness.Evidence
				for _, e := range previous.Evidence {
					if e.ID == id {
						before = e
					}
				}
				for _, e := range next.Evidence {
					if e.ID == id {
						after = e
					}
				}
				if before != after {
					return fmt.Errorf("definition repair rebound protected definition %s", id)
				}
			}
		}
	}
	return nil
}
