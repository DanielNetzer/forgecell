package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// analysisCorrection travels in the context so Options.Analyze keeps its signature.
type analysisCorrection struct {
	ValidatorMessage string
	PreviousResponse string
}
type analysisCorrectionKey struct{}

func withAnalysisCorrection(ctx context.Context, c analysisCorrection) context.Context {
	return context.WithValue(ctx, analysisCorrectionKey{}, c)
}
func analysisCorrectionFrom(ctx context.Context) (analysisCorrection, bool) {
	c, ok := ctx.Value(analysisCorrectionKey{}).(analysisCorrection)
	return c, ok
}

const (
	// maxCorrectiveAnalyses bounds the corrective retries of one intake, one per
	// policy violation class. The same class twice stops intake.
	maxCorrectiveAnalyses = 3
	// maxAnalysisInvocations is the initial analysis, every corrective retry and the
	// single analysis that follows the supplemental evidence pass.
	maxAnalysisInvocations = 1 + maxCorrectiveAnalyses + 1
)

// analysisRequest is the ticket-analysis payload. A corrective pass adds the exact
// validator message and the rejected response; the first pass never carries one.
func analysisRequest(ctx context.Context, i Issue, s readiness.RepositorySnapshot, inputs readiness.VerificationInputs) map[string]any {
	request := map[string]any{"kind": "ticket-analysis", "issue": i, "repository": s, "humanVerification": inputs}
	if c, ok := analysisCorrectionFrom(ctx); ok {
		correction := map[string]string{"validatorMessage": c.ValidatorMessage, "previousResponse": c.PreviousResponse}
		if c.ValidatorMessage == readiness.EvidenceRequestCapMessage() {
			correction["instruction"] = fmt.Sprintf("Return the complete analysis again with at most %d evidenceRequests, ordered by importance to the ticket scope, most important first. Drop the least important requests; do not relax or reword the limit.", readiness.MaxEvidenceRequests)
		}
		if strings.HasPrefix(c.ValidatorMessage, readiness.InScopeDefinitionMessage) || strings.HasPrefix(c.ValidatorMessage, readiness.DefinitionReferenceMessage) {
			correction["instruction"] = "Return the complete analysis again. A check may be defined by an existing frozen evidence ID that this plan does not edit (for example the parent manifest or workflow), or by a scope entry that is a new file absent from the repository tree, cited in definitions by its exact scope path. A check may never be defined by an existing in-scope file or by the issue. For a new package, `go test ./path/to/package/ -run <pattern>` defined by the parent manifest is also valid. Do not relax or reword the rule."
		}
		request["correction"] = correction
	}
	return request
}

// policyRejection keeps the established failure text while exposing the typed
// validator error, but only when the preserved response really produced it.
type policyRejection struct {
	message   string
	violation error
}

func (e *policyRejection) Error() string { return e.message }
func (e *policyRejection) Unwrap() error { return e.violation }

func decodeAnalysisResult(result process.Result) (readiness.Analysis, error) {
	if result.OK {
		return readiness.DecodeAnalysis([]byte(result.Stdout))
	}
	err := fmt.Errorf("analysis failed: %s %s", result.Error, result.Stderr)
	if result.Stdout != "" && strings.HasPrefix(result.Error, harness.InvalidAnalysisPrefix) {
		a, decodeErr := readiness.DecodeAnalysis([]byte(result.Stdout))
		if readiness.IsPolicyViolation(decodeErr) && result.Error == harness.InvalidAnalysisPrefix+decodeErr.Error() {
			return a, &policyRejection{message: err.Error(), violation: decodeErr}
		}
	}
	return readiness.Analysis{}, err
}

const (
	timeoutFreeScopePaths = 8
	timeoutRoundMS        = 60000
)

// proposeHarnessTimeoutMS is a deterministic function of scope size alone, so the
// model cannot influence it. Each path past the free threshold adds an eighth of
// the Formula timeout, rounded up to a minute and clamped to the Formula maximum.
// Zero means no extension, which keeps the plan encoding and digest unchanged.
func proposeHarnessTimeoutMS(b formula.Binding, scopePaths int) int {
	ceiling := b.TimeoutCeilingMS()
	if b.TimeoutMS < 1 || ceiling <= b.TimeoutMS || scopePaths <= timeoutFreeScopePaths {
		return 0
	}
	proposed := b.TimeoutMS + (scopePaths-timeoutFreeScopePaths)*(b.TimeoutMS/8)
	proposed = (proposed + timeoutRoundMS - 1) / timeoutRoundMS * timeoutRoundMS
	if proposed = min(proposed, ceiling); proposed <= b.TimeoutMS {
		return 0
	}
	return proposed
}

// codingTimeout is the reviewed per-Molecule value when the plan carries one,
// otherwise the Formula timeout. Zero lets the process layer apply its default.
func codingTimeout(b formula.Binding, p readiness.Plan) (int, string) {
	if p.HarnessTimeoutMS > 0 {
		return p.HarnessTimeoutMS, "plan harnessTimeoutMs"
	}
	if b.TimeoutMS == 0 {
		return 0, "Formula timeoutMs unset; process default applies"
	}
	return b.TimeoutMS, "Formula timeoutMs"
}

// approvedCodingAllowlist returns the exact tool allowance for a coding attempt.
// Intake and amendment always store a derived list, so a plan without one was
// approved by an earlier Lab under the fixed Node allowance; it keeps that
// allowance (plus read-only git status/diff) rather than silently losing it. A
// stored list must equal the one rederived from the approved Formula, the
// plan's checks and scope, or the pre-formatter derivation that earlier plans
// were approved under, so rule or input drift cannot widen what coding may run.
func approvedCodingAllowlist(f formula.Formula, p readiness.Plan) ([]string, error) {
	if len(p.CodingAllowlist) == 0 {
		return harness.LegacyCodingAllowlist()
	}
	allowed, ok, err := harness.MatchCodingAllowlist(f.YAML, p.Analysis.Checks, p.Analysis.Scope, p.CodingAllowlist)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("approved coding allowlist differs from the one derived from the plan checks and Formula; refresh intake")
	}
	return allowed, nil
}
func issueInput(i Issue) readiness.Issue {
	return readiness.Issue{Repository: i.Repo, Number: i.Number, URL: i.URL, State: i.State, Title: i.Title, Body: i.Body, UpdatedAt: i.UpdatedAt}
}

// analyzeWithWrapper runs the separately approved analysis wrapper. The decision,
// including a refusal, is persisted as immutable evidence and cited in the
// ledger before any wrapper process starts. Every failure is returned as an
// error, so intake stops with no coding harness and no fallback analysis.
// b is the resolved analysis role binding, which may differ from f.Harness.
// The diagnostics are nil until a wrapper invocation returns a process result.
func analyzeWithWrapper(ctx context.Context, r *Record, f formula.Formula, b formula.Binding, request map[string]any) (readiness.Analysis, []ProvenanceEvidence, *process.Diagnostics, error) {
	ref := b.AnalysisWrapper
	ordinal := len(r.AnalysisAttempts)
	decision := harness.ApproveAnalysisWrapper(r.LabDir, ref.Manifest, ref.SHA256)
	file, err := historyFile(*r, fmt.Sprintf("analysis-wrapper-%d-decision.json", ordinal), decision)
	if err != nil {
		return readiness.Analysis{}, nil, nil, fmt.Errorf("analysis wrapper decision could not be persisted; no process was started: %w", err)
	}
	evidence := []ProvenanceEvidence{fileProvenance(file)}
	r.Notes = append(r.Notes, fmt.Sprintf("Analysis wrapper decision %q for attempt %d is recorded at %s (SHA-256 %s). Declared controls are wrapper claims, not Forgecell-verified containment.", decision.Status, ordinal, file.Path, file.SHA256))
	if err = save(*r); err != nil {
		return readiness.Analysis{}, evidence, nil, fmt.Errorf("analysis wrapper decision could not be saved to the ledger; no process was started: %w", err)
	}
	if err = decision.Err(); err != nil {
		return readiness.Analysis{}, evidence, nil, err
	}
	contract := []map[string]any{}
	for _, a := range f.Atoms {
		if a.Type == "check" {
			contract = append(contract, map[string]any{"id": a.ID, "command": a.Command, "timeoutMs": a.TimeoutMS, "source": a.Source, "binding": a.Binding, "workflows": a.Workflows})
		}
	}
	request["formulaCheckContract"] = contract
	result := harness.InvokeAnalysisWrapper(ctx, r.LabDir, decision, request, time.Duration(b.TimeoutMS)*time.Millisecond)
	a, err := decodeAnalysisResult(result)
	return a, evidence, resultDiagnostics(result), err
}

// resultDiagnostics keeps only the secret-free diagnostics of a process result.
func resultDiagnostics(result process.Result) *process.Diagnostics {
	d := result.Diagnostics()
	return &d
}

// Lookup uses approved identity and never fills historical absence.
func atomProvenance(r *Record, id string) *AtomProvenance {
	for i := range r.Atoms {
		if r.Atoms[i].ID == id {
			return r.Atoms[i].Provenance
		}
	}
	return nil
}
func startAtomAction(r *Record, id, attempt, digest, source, action string) {
	if p := atomProvenance(r, id); p != nil {
		p.Actions = append(p.Actions, AtomAction{AttemptID: attempt, PlanDigest: digest, ResolvedSource: source, PlannedAction: action, StartedAt: stamp()})
		p.Execution = "started"
		p.SkipReason = ""
	}
}
func recordExecutedBinding(r *Record, id, attempt, binding string) {
	if p := atomProvenance(r, id); p != nil {
		for i := range p.Actions {
			if p.Actions[i].AttemptID == attempt {
				p.Actions[i].ExecutedBinding = binding
				return
			}
		}
	}
}
func observeAtomAction(r *Record, id, attempt, action, result string, evidence ...ProvenanceEvidence) {
	if p := atomProvenance(r, id); p != nil {
		for i := range p.Actions {
			a := &p.Actions[i]
			if a.AttemptID == attempt {
				a.Observed = &AtomObservation{Action: action, Result: result, FinishedAt: stamp(), Evidence: evidence}
				p.Execution = "observed"
				return
			}
		}
	}
}
func fileProvenance(ref VerificationFile) ProvenanceEvidence {
	return ProvenanceEvidence{Path: ref.Path, SHA256: ref.SHA256}
}
func plannedAtomAction(a formula.Atom) string {
	switch a.Type {
	case "intake":
		return "read-and-analyze-ticket"
	case "gate":
		return "await-" + a.Purpose + "-approval"
	case "harness":
		return "invoke-coding-harness"
	case "check":
		return "run-approved-checks"
	case "review":
		return "run-adversarial-review"
	case "ship":
		return "separate-approved-publication"
	case "document":
		return "separate-approved-documentation"
	case "learn":
		return "separate-approved-learning"
	}
	return ""
}
func atom(r *Record, index int, status, detail string) {
	a := &r.Atoms[index]
	a.Status = status
	a.Detail = detail
	if status == "skipped" && a.Provenance != nil {
		a.Provenance.Execution = "skipped"
		if len(a.Provenance.Actions) > 0 {
			a.Provenance.Execution = "stopped"
		}
		a.Provenance.SkipReason = "Execution stopped before this action."
		if a.Type == "ship" || a.Type == "document" || a.Type == "learn" {
			a.Provenance.SkipReason = "Requires a separate approved action."
		}
	}
	if a.StartedAt == "" {
		a.StartedAt = stamp()
	}
	if status != "waiting" && status != "active" {
		a.FinishedAt = stamp()
	}
}
func stopIntake(r Record, reason string) (Record, error) {
	roles, roleErr := resolveExecutionRoles(r)
	if roleErr != nil {
		return r, roleErr
	}
	atom(&r, roles.atoms[formula.IntakeRole], "blocked", reason)
	r.Status = "blocked"
	r.FinishedAt = stamp()
	for _, i := range roles.after(formula.IntakeRole) {
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
		r.Atoms = append(r.Atoms, Atom{ID: a.ID, Type: a.Type, Status: "pending", Detail: "Not started", Provenance: &AtomProvenance{DeclaredSource: a.Source, DeclaredBinding: a.Binding, DeclaredWorkflows: append([]string(nil), a.Workflows...), DeclaredPurpose: a.Purpose, PlannedAction: plannedAtomAction(a)}})
	}
	roles, err := resolveExecutionRoles(r)
	if err != nil {
		return r, err
	}
	intakeSource := "github-issues"
	if o.ReadIssue != nil {
		intakeSource = "supplied-issue-reader"
	}
	startAtomAction(&r, r.Atoms[roles.atoms[formula.IntakeRole]].ID, "intake", "", intakeSource, "read-ticket")
	if err = save(r); err != nil {
		return r, err
	}
	reader := o.ReadIssue
	if reader == nil {
		reader = readIssue
	}
	issue, err := reader(ctx, w.Repo, ref.Number, w.SourceRoot)
	if err != nil {
		observeAtomAction(&r, r.Atoms[roles.atoms[formula.IntakeRole]].ID, "intake", "read-ticket", "read-failed")
		return stopIntake(r, err.Error())
	}
	if issue.Number != ref.Number || issue.Repo != w.Repo || issue.URL != fmt.Sprintf("https://github.com/%s/issues/%d", w.Repo, ref.Number) || issue.State != "OPEN" {
		observeAtomAction(&r, r.Atoms[roles.atoms[formula.IntakeRole]].ID, "intake", "read-ticket", "response-rejected")
		return stopIntake(r, "Issue is closed or does not match the requested repository and number.")
	}
	r.Issue = issue
	observeAtomAction(&r, r.Atoms[roles.atoms[formula.IntakeRole]].ID, "intake", "read-ticket", "read-completed", ProvenanceEvidence{Pointer: "/issue"})
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
	// Extensions are resolved once, before the first analysis invocation and before
	// any harness starts, so the supplemental and corrective passes reuse them and
	// coding reuses the build. A required extension that is unavailable stops intake.
	var analysisServers []harness.MCPServer
	if declared := f.RoleExtensions(formula.AnalysisHarness); len(declared) > 0 {
		if binding := roles.bindings[formula.IntakeRole]; o.Analyze != nil || binding.AnalysisWrapper != nil {
			how := "an analysis wrapper"
			if o.Analyze != nil {
				how = "a supplied analysis function"
			}
			if extErr := skipExtensions(&r, f, formula.AnalysisHarness, "analysis runs through "+how+", which cannot attach extensions"); extErr != nil {
				return stopIntake(r, extErr.Error())
			}
		} else {
			servers, cleanup, extErr := attachExtensions(ctx, &r, f, formula.AnalysisHarness)
			defer cleanup()
			if extErr != nil {
				return stopIntake(r, extErr.Error())
			}
			if analysisServers = servers; len(servers) > 0 {
				r.Notes = append(r.Notes, AnalysisWithoutSafeModeDisclosure)
			}
		}
	}
	if _, extErr := useExtensions(ctx, &r, f, formula.CodingHarness, ""); extErr != nil {
		return stopIntake(r, extErr.Error())
	}
	analysis := o.Analyze
	var wrapperEvidence []ProvenanceEvidence
	var analysisDiagnostics *process.Diagnostics
	if analysis == nil {
		analysis = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
			inputs := readiness.VerificationInputs{CheckInputs: o.CheckInputs, Checks: o.Checks, Setup: o.Setup, Artifacts: o.Artifacts, Policy: o.Policy}
			if b.AnalysisWrapper != nil {
				a, evidence, diagnostics, err := analyzeWithWrapper(ctx, &r, f, b, analysisRequest(ctx, i, s, inputs))
				wrapperEvidence, analysisDiagnostics = evidence, diagnostics
				return a, err
			}
			// Resolve configuration from the source checkout, then Execute constrains the
			// provider and moves analysis to a temporary directory before the model runs.
			request := analysisRequest(ctx, i, s, inputs)
			if len(analysisServers) > 0 {
				request["mcpServers"] = analysisServers
			}
			result := invokeHarness(ctx, b.Command, w.SourceRoot, time.Duration(b.TimeoutMS)*time.Millisecond, request)
			analysisDiagnostics = resultDiagnostics(result)
			return decodeAnalysisResult(result)
		}
	}

	// At most one supplemental evidence pass and one corrective retry per policy
	// class, up to maxCorrectiveAnalyses in all, so maxAnalysisInvocations is the
	// hard bound. Every invocation has its own ordinal and therefore its own
	// provenance action id.
	var a readiness.Analysis
	var correction *analysisCorrection
	retried, supplemented := map[string]bool{}, false
	for ordinal := 1; ordinal <= maxAnalysisInvocations; ordinal++ {
		if err = formula.Revalidate(o.LabDir, loaded); err != nil {
			return stopIntake(r, err.Error())
		}
		digest, e := snapshot.Digest()
		if e != nil {
			return stopIntake(r, e.Error())
		}
		r.RepositoryEvidence = &snapshot
		r.AnalysisAttempts = append(r.AnalysisAttempts, AnalysisAttempt{Ordinal: ordinal, SnapshotSHA256: digest, StartedAt: stamp(), Status: "started"})
		analysisID := fmt.Sprintf("analysis-%d", ordinal)
		analysisBinding := roles.bindings[formula.IntakeRole]
		analysisSource := "bound-harness:" + analysisBinding.Binding
		if analysisBinding.AnalysisWrapper != nil && o.Analyze == nil {
			analysisSource = "analysis-wrapper:" + analysisBinding.Binding + "@sha256:" + analysisBinding.AnalysisWrapper.SHA256
		}
		if o.Analyze != nil {
			analysisSource = "supplied-analysis-function"
		}
		startAtomAction(&r, r.Atoms[roles.atoms[formula.IntakeRole]].ID, analysisID, "", analysisSource, "analyze-ticket")
		recordExecutedBinding(&r, r.Atoms[roles.atoms[formula.IntakeRole]].ID, analysisID, analysisBinding.Binding)
		if err = save(r); err != nil {
			return r, err
		}
		callCtx := ctx
		if correction != nil {
			callCtx = withAnalysisCorrection(ctx, *correction)
		}
		wrapperEvidence, analysisDiagnostics = nil, nil
		a, err = analysis(callCtx, analysisBinding, issue, snapshot)
		if err == nil {
			// A supplied Analyze can return an over-cap response without an error; the
			// real harness rejects it at its boundary. Both take the same retry path.
			err = readiness.ValidateEvidenceRequestCap(a.EvidenceRequests)
		}
		if err == nil && len(a.EvidenceRequests) == 0 {
			// Intake-time rejection; Plan.ValidateForCoding stays as the run-time backstop.
			// A scope entry absent from the frozen tree may define a check; the Lab, not
			// the model, decides which entries are new.
			if err = readiness.ValidateDefinitionReferences(a); err == nil {
				err = readiness.ValidateInScopeDefinitionsForNewFiles(a, snapshot.References(), readiness.NewScopeFiles(a.Scope, snapshot.Files))
			}
		}
		attempt := &r.AnalysisAttempts[len(r.AnalysisAttempts)-1]
		attempt.FinishedAt = stamp()
		attempt.Diagnostics = analysisDiagnostics
		if err != nil {
			attempt.Status = "failed"
			attempt.Error = err.Error()
			if readiness.IsPolicyViolation(err) {
				rejected := a
				attempt.Response = &rejected
			}
		} else {
			attempt.Status = "completed"
			response := a
			attempt.Response = &response
			r.Analysis = &a
		}
		analysisResult := "analysis-completed"
		if err != nil {
			analysisResult = "analysis-failed"
		}
		observeAtomAction(&r, r.Atoms[roles.atoms[formula.IntakeRole]].ID, analysisID, "analyze-ticket", analysisResult, append([]ProvenanceEvidence{{Pointer: fmt.Sprintf("/analysisAttempts/%d", len(r.AnalysisAttempts)-1)}}, wrapperEvidence...)...)
		if e = save(r); e != nil {
			return r, e
		}
		if err != nil {
			var violation *readiness.PolicyViolation
			if !errors.As(err, &violation) {
				return stopIntake(r, err.Error())
			}
			class := readiness.PolicyClass(violation)
			if retried[class] {
				return stopIntake(r, err.Error()+" (the corrective analysis retry was also rejected; no coding harness was invoked)")
			}
			if len(retried) >= maxCorrectiveAnalyses {
				return stopIntake(r, err.Error()+fmt.Sprintf(" (the limit of %d corrective analysis retries was reached; no coding harness was invoked)", maxCorrectiveAnalyses))
			}
			previous, e := json.Marshal(a)
			if e != nil {
				return stopIntake(r, e.Error())
			}
			retried[class] = true
			correction = &analysisCorrection{ValidatorMessage: violation.Reason, PreviousResponse: string(previous)}
			r.Notes = append(r.Notes, fmt.Sprintf("Analysis attempt %d was rejected by the unchanged policy validator (%s); one corrective analysis retry receives that message and the rejected response. Rejection class %s has now used its retry (%d of %d).", ordinal, violation.Reason, class, len(retried), maxCorrectiveAnalyses))
			continue
		}
		correction = nil
		if err = formula.Revalidate(o.LabDir, loaded); err != nil {
			return stopIntake(r, err.Error())
		}
		if err = readiness.ValidateEvidenceRequests(a.EvidenceRequests, snapshot.References()); err != nil {
			return stopIntake(r, err.Error())
		}
		if len(a.EvidenceRequests) == 0 {
			break
		}
		if supplemented {
			return stopIntake(r, "Supplemental evidence pass exhausted; unresolved evidence requests remain. No coding harness was invoked.")
		}
		supplemented = true
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
	codingBinding := roles.bindings[formula.Coding]
	bindingDigest, err := bindingHash(codingBinding)
	if err != nil {
		return stopIntake(r, err.Error())
	}
	p := readiness.Plan{SchemaVersion: "v1", MoleculeID: r.ID, Revision: 1, Inputs: readiness.Inputs{Repository: w.Repo, TargetBranch: target, BaseCommit: w.BaseCommit, FormulaSHA256: f.SHA256, EvidenceSHA256: evidenceHash, BindingSHA256: bindingDigest, Issue: issueInput(issue), MandatoryConstraints: o.MandatoryConstraints}, Evidence: snapshot.References(), Analysis: a, Policy: readiness.ExecutionPolicy{Environment: []string{"PATH"}, SetupNetwork: "unrestricted", CheckNetwork: "unrestricted", Containment: "filtered-environment"}}
	p.Analysis.Checks = append(p.Analysis.Checks, o.Checks...)
	p.NewFiles = readiness.CitedNewFiles(p.Analysis.Checks, readiness.NewScopeFiles(a.Scope, snapshot.Files))
	p.HarnessTimeoutMS = proposeHarnessTimeoutMS(codingBinding, len(p.Analysis.Scope))
	if o.Policy != nil {
		p.Policy = *o.Policy
	}
	p.CheckInputs = o.CheckInputs
	p.Setup = o.Setup
	p.Artifacts = o.Artifacts
	for _, input := range o.CheckInputs {
		p.Evidence = append(p.Evidence, input.Evidence)
	}
	if err = applyFormulaBaseline(f, &p); err != nil {
		return stopIntake(r, err.Error())
	}
	if p.CodingAllowlist, err = harness.DeriveCodingAllowlist(f.YAML, p.Analysis.Checks, p.Analysis.Scope...); err != nil {
		return stopIntake(r, err.Error())
	}
	if err = snapshot.ValidateScope(a.Scope); err != nil {
		return stopIntake(r, err.Error())
	}
	state, err := readiness.NewState(p, stamp())
	if err != nil {
		return stopIntake(r, err.Error())
	}
	r.Readiness = &state
	if len(p.FormulaBaseline) > 0 {
		r.Notes = append(r.Notes, formulaBaselineNote(f))
	}
	for _, overlap := range p.DefinitionOverlaps() {
		r.Notes = append(r.Notes, fmt.Sprintf("Scope/check overlap %s [%s] checks %s: %s", overlap.Path, overlap.Kind, strings.Join(overlap.CheckIDs, ", "), overlap.Detail))
	}
	if advisory := p.ProtectedDeltaAdvisory(); advisory != "" {
		r.Notes = append(r.Notes, advisory)
	}
	if note, ok := newFilesNote(p); ok {
		r.Notes = append(r.Notes, note)
	}
	if note, ok := scopeGateSensitivePathsNote(f); ok {
		r.Notes = append(r.Notes, note)
	}
	atom(&r, roles.atoms[formula.IntakeRole], "done", a.Summary)
	atom(&r, roles.atoms[formula.ScopeApproval], "waiting", "Review exact scope, checks and process limits before approving the digest.")
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

// resume is the human approval path: o.Approve names the digest a human reviewed.
func resume(ctx context.Context, o Options, expected formula.Loaded, number int64, repo string) (Record, error) {
	return resumeApproved(ctx, o, expected, number, repo, false)
}

// resumeByPolicy is the approval transaction with the scope gate's policy as the
// approver. It runs every live check the human path runs, and records a decision
// only when the policy approves the pre-approval record and Approve then succeeds
// against the live inputs. Only the Lab loop calls it; Run and `forgecell run`
// never approve by policy.
func resumeByPolicy(ctx context.Context, o Options, expected formula.Loaded, number int64, repo string) (Record, error) {
	return resumeApproved(ctx, o, expected, number, repo, true)
}

func resumeApproved(ctx context.Context, o Options, expected formula.Loaded, number int64, repo string, byPolicy bool) (r Record, err error) {
	f := expected.Formula
	r, err = findPending(o.LabDir, o.Approve)
	if err != nil {
		return r, err
	}
	if _, err = resolveExecutionRoles(r); err != nil {
		return r, err
	}
	if !byPolicy {
		// The loop holds the Molecule flock for a policy approval; a human approval
		// takes it here, before the lock directory, so it cannot race a loop.
		unlockLoop, lockErr := lockMolecule(o.LabDir, r.ID)
		if lockErr != nil {
			return r, lockedByLoop(lockErr)
		}
		defer unlockLoop()
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
	roles, err := resolveExecutionRoles(r)
	if err != nil {
		return r, err
	}
	if r.Issue.Number != number || r.Issue.Repo != repo || r.FormulaID != f.ID || r.FormulaSnapshot.SHA256 != f.SHA256 {
		return r, fmt.Errorf("issue or Formula changed; refresh intake")
	}
	var decision PolicyDecision
	if byPolicy {
		if decision, err = scopePolicyDecision(r); err != nil {
			return r, err
		}
		if decision.PlanDigest != o.Approve {
			return r, fmt.Errorf("scope policy approved plan %s, not the requested plan %s", decision.PlanDigest, o.Approve)
		}
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
	if err = requireFormulaBaseline(f, p); err != nil {
		return r, err
	}
	codingBinding := roles.bindings[formula.Coding]
	if err = codingBinding.CheckTimeout(p.HarnessTimeoutMS); err != nil {
		return r, err
	}
	var allowlist []string
	if p.Continuation == "" {
		if allowlist, err = approvedCodingAllowlist(f, p); err != nil {
			return r, err
		}
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
	current.BindingSHA256, err = bindingHash(codingBinding)
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
	approvalSource, approvalDetail := "readiness-approval", "Exact readiness plan approved; no publication authorized."
	if byPolicy {
		decision.At = stamp()
		r.PolicyDecisions = append(slices.Clone(r.PolicyDecisions), decision)
		approvalSource, approvalDetail = "readiness-policy-approval", "Exact readiness plan approved by "+decision.ApprovedBy+"; no publication authorized."
	}
	startAtomAction(&r, r.Atoms[roles.atoms[formula.ScopeApproval]].ID, "scope-"+o.Approve, o.Approve, approvalSource, "record-exact-scope-approval")
	observeAtomAction(&r, r.Atoms[roles.atoms[formula.ScopeApproval]].ID, "scope-"+o.Approve, "record-exact-scope-approval", "approved", ProvenanceEvidence{Pointer: "/readiness/events"})
	atom(&r, roles.atoms[formula.ScopeApproval], "done", approvalDetail)
	if p.Continuation == "" {
		atom(&r, roles.atoms[formula.Coding], "active", "Coding approved scope")
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
	// The approval may come long after intake, so the extensions are verified again
	// here. A required one that is no longer usable starts no coding harness.
	codingServers, cleanupExtensions, extErr := attachExtensions(ctx, &r, f, formula.CodingHarness)
	defer cleanupExtensions()
	if extErr != nil {
		return finishFailed(r, "Extensions unavailable: "+extErr.Error())
	}
	attemptID, err := token()
	if err != nil {
		return r, err
	}
	forgecell := harness.RecordProvenance(codingBinding.Command)
	r.HarnessAttempts = append(r.HarnessAttempts, HarnessAttempt{ID: attemptID, PlanDigest: o.Approve, CodingAllowlist: allowlist, Forgecell: &forgecell})
	startAtomAction(&r, r.Atoms[roles.atoms[formula.Coding]].ID, attemptID, o.Approve, "bound-harness:"+codingBinding.Binding, "invoke-coding-harness")
	recordExecutedBinding(&r, r.Atoms[roles.atoms[formula.Coding]].ID, attemptID, codingBinding.Binding)
	timeoutMS, timeoutSource := codingTimeout(codingBinding, p)
	if p.HarnessTimeoutMS > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("Coding harness timeout: plan harnessTimeoutMs %d applies to attempt %s (Formula timeoutMs %d, maxTimeoutMs ceiling %d).", p.HarnessTimeoutMS, attemptID, codingBinding.TimeoutMS, codingBinding.TimeoutCeilingMS()))
	}
	if err = save(r); err != nil {
		return r, err
	}
	codingRequest := map[string]any{"kind": "molecule", "moleculeId": r.ID, "formulaId": f.ID, "recipeInstructions": codingBinding.Instructions, "issue": r.Issue, "approvedPlan": p, "codingAllowlist": allowlist, "codingOutcomeSchema": json.RawMessage(harness.CodingOutcomeSchema()), "scopeChangeProtocol": `If blocked by scope, return only JSON {"schemaVersion":"v1","outcome":"scope-change","reason":"why","paths":["exact/path"]}; do not write those new paths.`, "instruction": "Implement only the approved exact scope. Do not commit, push, merge, deploy or comment. Stop and report a scope-change request if the plan is insufficient. Return only JSON with schemaVersion (v1), outcome (completed, blocked, scope-change, or no-change), reason (bounded account of changes, validation, or blocker), and paths (exact requested paths for scope-change, otherwise an empty array), matching codingOutcomeSchema. No-change requires allowNoChange in the exact approved plan. Checks are executed independently by the Lab."}
	if len(codingServers) > 0 {
		codingRequest["mcpServers"] = codingServers
	}
	result := invokeHarness(ctx, codingBinding.Command, r.Workspace.Path, time.Duration(timeoutMS)*time.Millisecond, codingRequest)
	cleanupExtensions()
	r.Atoms[roles.atoms[formula.Coding]].ExitCode = &result.Code
	r.Atoms[roles.atoms[formula.Coding]].ElapsedMS = result.ElapsedMS

	a := &r.HarnessAttempts[len(r.HarnessAttempts)-1]
	coding := harness.CodingOutcomeFromProcess(result)
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
	processOutcome := "process-failed"
	if result.OK {
		processOutcome = "process-succeeded"
	}
	if result.Interrupted {
		processOutcome = "process-interrupted"
	} else if result.TimedOut {
		processOutcome = "process-timed-out"
	} else if result.ParentDeadline {
		processOutcome = "process-parent-deadline"
	}
	observeAtomAction(&r, r.Atoms[roles.atoms[formula.Coding]].ID, attemptID, "invoke-coding-harness", processOutcome, fileProvenance(ref))
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
	if provenance := atomProvenance(&r, r.Atoms[roles.atoms[formula.Coding]].ID); provenance != nil {
		for i := range provenance.Actions {
			action := &provenance.Actions[i]
			if action.AttemptID == attemptID && action.Observed != nil {
				action.Observed.Evidence = append(action.Observed.Evidence, fileProvenance(captureRef))
			}
		}
	}
	a.Capture = &capture
	r.Capture = &capture
	r.Captures = append(r.Captures, capture)
	if err = save(r); err != nil {
		return r, err
	}
	if !result.OK {
		summary := harnessOutcomeSummary(result, ref)
		if result.TimedOut || result.ParentDeadline {
			summary = harnessTimeoutSummary(timeoutGate{Molecule: r.ID, Lab: r.LabDir, Digest: o.Approve, Attempt: attemptID, TimeoutMS: timeoutMS, Source: timeoutSource, CeilingMS: codingBinding.TimeoutCeilingMS()}, result, ref, capture, captureErr)
		}
		r, err = finishFailed(r, summary)
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
			atom(&r, roles.atoms[formula.Coding], "failed", coding.Outcome+": "+coding.Reason+"; "+detail)
			return finishStopped(r, detail)
		}
		state, e := r.Readiness.FinishAttempt("scope-violation", detail, stamp())
		if e != nil {
			return r, e
		}
		state.Events[len(state.Events)-1].Tree = capture.Tree
		r.Readiness = &state
		atom(&r, roles.atoms[formula.Coding], "failed", detail)
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
		atom(&r, roles.atoms[formula.Coding], "failed", phase+": "+detail)
		return finishStopped(r, phase+": "+detail+" Retained tree: "+capture.Tree)
	}
	atom(&r, roles.atoms[formula.Coding], "done", coding.Outcome+": "+coding.Reason)
	state, err = r.Readiness.FinishAttempt("check-pending", "Coding output captured: "+capture.Tree, stamp())
	if err != nil {
		return r, err
	}
	r.Readiness = &state
	return verifyCaptured(ctx, o, r, p, capture)
}

func verifyCaptured(ctx context.Context, o Options, r Record, p readiness.Plan, capture verification.CaptureResult) (Record, error) {
	roles, roleErr := resolveExecutionRoles(r)
	if roleErr != nil {
		return r, roleErr
	}
	if err := beginVerification(&r, p, capture.Tree); err != nil {
		return r, err
	}
	checkID := r.Atoms[roles.atoms[formula.Verification]].ID
	attemptID := fmt.Sprintf("verification-%d", len(r.VerificationAttempts))
	digest := r.VerificationAttempts[len(r.VerificationAttempts)-1].PlanDigest
	startAtomAction(&r, checkID, attemptID, digest, "lab-independent-verification", "run-approved-checks")
	atom(&r, roles.atoms[formula.Verification], "active", "Running approved checks in a fresh checkout")
	if err := save(r); err != nil {
		return r, err
	}
	destination := filepath.Join(o.LabDir, "verification", fmt.Sprintf("%s-r%d", r.ID, p.Revision))
	verified := verification.RunObserved(ctx, r.Workspace.SourceRoot, destination, p, capture, func(obs verification.Observation) error {
		if err := recordObservation(&r, obs); err != nil {
			return err
		}
		refs := []ProvenanceEvidence{}
		for _, ref := range r.VerificationAttempts[len(r.VerificationAttempts)-1].Observations {
			refs = append(refs, fileProvenance(ref))
		}
		observeAtomAction(&r, checkID, attemptID, "observe-verification", "partial-observations", refs...)
		return save(r)
	})
	if err := completeVerification(&r, verified); err != nil {
		return r, err
	}
	attempt := r.VerificationAttempts[len(r.VerificationAttempts)-1]
	refs := []ProvenanceEvidence{}
	for _, ref := range attempt.Observations {
		refs = append(refs, fileProvenance(ref))
	}
	if attempt.Result != nil {
		refs = append(refs, fileProvenance(*attempt.Result))
	}
	outcome := "checks-failed"
	if verified.RequiredChecksPassed {
		outcome = "required-checks-passed"
	}
	observeAtomAction(&r, checkID, attemptID, "run-approved-checks", outcome, refs...)
	projection := compactVerification(verified)
	r.Verification = &projection
	reviewIndex, hasReview := roles.atoms[formula.Review]
	var review ReviewDecision
	if hasReview && verified.RequiredChecksPassed {
		atom(&r, roles.atoms[formula.Verification], "done", "Required checks passed on "+verified.SourceTree+". Independent acceptance verified: "+fmt.Sprint(verified.IndependentAcceptanceVerified))
		atom(&r, reviewIndex, "active", "Running adversarial review of the captured tree")
		if err := save(r); err != nil {
			return r, err
		}
		f, err := formula.Parse([]byte(r.FormulaSnapshot.YAML))
		if err != nil {
			return r, fmt.Errorf("invalid execution Formula snapshot: %w", err)
		}
		in := ReviewInputs{HarnessAttemptID: reviewAttemptKey(r, p), PlanDigest: digest, Plan: p, Capture: capture, Verified: verified}
		r, review, err = runReviewAtom(ctx, o, r, f, in)
		if err != nil {
			review = ReviewDecision{Blocked: true, Status: ReviewFailed, Detail: reviewText("Adversarial review could not run: "+err.Error(), reviewReasonRunes)}
			atom(&r, reviewIndex, "failed", review.Detail+" Delivery is blocked.")
		}
	}
	detail := verified.Error
	if review.Blocked {
		detail = review.Detail
	}
	state, err := r.Readiness.RecordVerification(verified.RequiredChecksPassed && !review.Blocked, detail, stamp())
	if err != nil {
		return r, err
	}
	r.Readiness = &state
	if !verified.RequiredChecksPassed {
		atom(&r, roles.atoms[formula.Verification], "failed", verified.Error)
		return finishStopped(r, "Required verification did not pass.")
	}
	if review.Blocked {
		return finishStopped(r, "Adversarial review blocked delivery: "+review.Detail)
	}
	if !hasReview {
		atom(&r, roles.atoms[formula.Verification], "done", "Required checks passed on "+verified.SourceTree+". Independent acceptance verified: "+fmt.Sprint(verified.IndependentAcceptanceVerified))
	}
	atom(&r, roles.atoms[formula.ReviewApproval], "waiting", "Human review required before explicit delivery approval.")
	for _, i := range roles.after(formula.ReviewApproval) {
		atom(&r, i, "skipped", "Publication is a separate approved action.")
	}
	r.Status = "waiting"
	r.FinishedAt = stamp()
	err = save(r)
	return r, err
}
func finishStopped(r Record, detail string) (Record, error) {
	roles, roleErr := resolveExecutionRoles(r)
	if roleErr != nil {
		return r, roleErr
	}
	r.Status = "blocked"
	r.FinishedAt = stamp()
	r.Notes = append(r.Notes, detail)
	for _, i := range roles.after(formula.Coding) {
		if r.Atoms[i].Status == "pending" || r.Atoms[i].Status == "active" {
			atom(&r, i, "skipped", detail)
		}
	}
	return r, save(r)
}
func finishFailed(r Record, detail string) (Record, error) {
	roles, roleErr := resolveExecutionRoles(r)
	if roleErr != nil {
		return r, roleErr
	}
	state, err := r.Readiness.FinishAttempt("failed", detail, stamp())
	if err != nil {
		return r, err
	}
	r.Readiness = &state
	atom(&r, roles.atoms[formula.Coding], "failed", detail)
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
	return fmt.Sprintf("Harness failed: exit=%d elapsedMs=%d timeout=%t interrupted=%t overflow=%t reconciled=%t; %s. Complete outcome: %s (SHA-256 %s). %s", result.Code, result.ElapsedMS, result.TimedOut, result.Interrupted, result.Overflow, result.Reconciled, message, ref.Path, ref.SHA256, gateDiagnostics(result))
}

// gateDiagnostics is the single-line ledger diagnostics sentence for a gate
// message. It comes from the same renderer as the Markdown ledger, so it carries
// only the enumerated terminal state, byte counts and millisecond offsets.
func gateDiagnostics(result process.Result) string {
	return strings.TrimSuffix(renderHarnessDiagnostics(result), "\n")
}

type timeoutGate struct {
	Molecule, Lab, Digest, Attempt, Source string
	TimeoutMS, CeilingMS                   int
}

// harnessTimeoutSummary is the scope-gate message for a timed-out coding attempt:
// what ran, what was retained, and the exact commands that keep the retained work.
// Other failures keep harnessOutcomeSummary unchanged.
func harnessTimeoutSummary(g timeoutGate, result process.Result, ref VerificationFile, capture verification.CaptureResult, captureErr error) string {
	tree := fmt.Sprintf("tree capture failed: %v", captureErr)
	review := ""
	if captureErr == nil {
		violations := "none"
		if len(capture.Violations) > 0 {
			violations = strings.Join(capture.Violations, ", ")
			review = fmt.Sprintf(" Out-of-scope writes need exact-diff review: add --accept-existing-tree %s to amend after inspecting the diff.", capture.Tree)
		}
		tree = fmt.Sprintf("retained tree %s: %d changed path(s), violations: %s", capture.Tree, len(capture.Changes), violations)
	}
	head := fmt.Sprintf("Harness timed out: elapsedMs=%d timeoutMs=%d (%s)", result.ElapsedMS, g.TimeoutMS, g.Source)
	extend := "The Formula declares no maxTimeoutMs above timeoutMs, so a longer coding timeout needs a reviewed Formula change."
	if g.CeilingMS > g.TimeoutMS {
		extend = fmt.Sprintf("To continue coding instead, omit continuation and set harnessTimeoutMs up to %d.", g.CeilingMS)
	}
	if result.ParentDeadline {
		head = fmt.Sprintf("Harness stopped by a caller deadline before its own timeout: elapsedMs=%d timeoutMs=%d (%s)", result.ElapsedMS, g.TimeoutMS, g.Source)
		extend = "The caller's deadline expired first, so a longer harnessTimeoutMs would not have helped; run again with a longer caller deadline."
	}
	return fmt.Sprintf("%s interrupted=%t overflow=%t reconciled=%t; %s. Complete outcome: %s (SHA-256 %s). %s\n"+
		"Nothing is retried automatically. To keep the retained work, run in order:\n"+
		"  recover: forgecell recover %s --plan %s --confirm-stopped --lab %q\n"+
		"  export:  forgecell ledger %s --lab %q   (copy readiness.plans[last].plan into FILE and set \"continuation\":\"adopt-failed-tree\" and \"adoptAttempt\":%q)\n"+
		"  amend:   forgecell amend %s --parent %s --plan FILE --lab %q\n"+
		"Then review and approve the new digest.%s %s",
		head, result.Interrupted, result.Overflow, result.Reconciled, tree, ref.Path, ref.SHA256, gateDiagnostics(result),
		g.Molecule, g.Digest, g.Lab, g.Molecule, g.Lab, g.Attempt, g.Molecule, g.Digest, g.Lab, review, extend)
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
