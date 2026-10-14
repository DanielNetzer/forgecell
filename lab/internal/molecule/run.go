// Package molecule executes approved Formulas and records each Atom's outcome.
package molecule

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Issue struct {
	UpdatedAt string `json:"updatedAt,omitempty"`
	Number    int64  `json:"number"`
	Repo      string `json:"repo"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	URL       string `json:"url"`
	State     string `json:"state"`
}
type Workspace struct {
	SourceRoot string `json:"sourceRoot"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseCommit string `json:"baseCommit"`
	Repo       string `json:"repo"`
}
type Snapshot struct {
	YAML   string `json:"yaml"`
	SHA256 string `json:"sha256"`
}

// AtomProvenance keeps declarations separate from observations. Empty fields
// mean unknown; a successful process does not establish an external outcome.
type AtomProvenance struct {
	DeclaredSource    string       `json:"declaredSource,omitempty"`
	DeclaredBinding   string       `json:"declaredBinding,omitempty"`
	DeclaredWorkflows []string     `json:"declaredWorkflows,omitempty"`
	DeclaredPurpose   string       `json:"declaredPurpose,omitempty"`
	PlannedAction     string       `json:"plannedAction,omitempty"`
	Execution         string       `json:"execution,omitempty"`
	SkipReason        string       `json:"skipReason,omitempty"`
	Actions           []AtomAction `json:"actions,omitempty"`
}
type AtomAction struct {
	AttemptID      string           `json:"attemptId"`
	PlanDigest     string           `json:"planDigest,omitempty"`
	ResolvedSource string           `json:"resolvedSource,omitempty"`
	PlannedAction  string           `json:"plannedAction"`
	StartedAt      string           `json:"startedAt"`
	Observed       *AtomObservation `json:"observed,omitempty"`
	// ExecutedBinding is the Formula binding id that a harness-bound action ran
	// under; empty for actions that are not harness-bound and for older ledgers.
	ExecutedBinding string `json:"executedBinding,omitempty"`
}
type AtomObservation struct {
	Action         string               `json:"action"`
	Result         string               `json:"result"`
	FinishedAt     string               `json:"finishedAt"`
	Evidence       []ProvenanceEvidence `json:"evidence,omitempty"`
	ExternalAction string               `json:"externalAction,omitempty"`
	ExternalResult string               `json:"externalResult,omitempty"`
}

// Evidence references ledger data or immutable files, never process streams.
type ProvenanceEvidence struct {
	Pointer string `json:"pointer,omitempty"`
	Path    string `json:"path,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}
type Atom struct {
	Provenance *AtomProvenance `json:"provenance,omitempty"`
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Status     string          `json:"status"`
	Detail     string          `json:"detail"`
	StartedAt  string          `json:"startedAt,omitempty"`
	FinishedAt string          `json:"finishedAt,omitempty"`
	ExitCode   *int            `json:"exitCode,omitempty"`
	ElapsedMS  int64           `json:"elapsedMs,omitempty"`
}
type HarnessAttempt struct {
	Forgecell       *harness.Provenance         `json:"forgecell,omitempty"`
	Coding          *harness.CodingOutcome      `json:"coding,omitempty"`
	CodingAllowlist []string                    `json:"codingAllowlist,omitempty"`
	ID              string                      `json:"id"`
	PlanDigest      string                      `json:"planDigest"`
	Result          process.Result              `json:"result"`
	CaptureEvidence []VerificationFile          `json:"captureEvidence,omitempty"`
	Evidence        *VerificationFile           `json:"evidence,omitempty"`
	Capture         *verification.CaptureResult `json:"capture,omitempty"`
	CaptureError    string                      `json:"captureError,omitempty"`
	Recovered       bool                        `json:"recovered,omitempty"`
	// Model is the pinned model, or "model unpinned"; Isolation is the isolation mode
	// of the invocation built for the attempt. Both are absent in earlier ledgers,
	// which read as unknown rather than as unpinned or isolated.
	Model     string `json:"model,omitempty"`
	Isolation string `json:"isolation,omitempty"`
	// Usage records the effective model, tokens and reported cost parsed from the
	// Claude CLI final JSON (#149). It is additive and omitempty: a Codex, Cursor or
	// custom-command attempt, a Claude run that emitted none, and every earlier ledger
	// carry no Usage, which reads as "unavailable" rather than as zero.
	Usage *harness.Usage `json:"usage,omitempty"`
}

// RecordInvocation records the model and isolation mode that the invocation for
// request will carry, derived from the same builder the adapter runs. An
// invocation that cannot be built leaves both fields empty; the run then fails the
// same way when it starts.
func (a *HarnessAttempt) RecordInvocation(command []string, request map[string]any) {
	if model, isolation, err := harness.InvocationProvenance(command, request); err == nil {
		a.Model, a.Isolation = model, isolation
	}
}

type AnalysisAttempt struct {
	Ordinal        int                 `json:"ordinal"`
	SnapshotSHA256 string              `json:"snapshotSha256"`
	StartedAt      string              `json:"startedAt"`
	FinishedAt     string              `json:"finishedAt,omitempty"`
	Status         string              `json:"status"`
	Response       *readiness.Analysis `json:"response,omitempty"`
	Error          string              `json:"error,omitempty"`
	// Diagnostics describes the process that ran for this attempt. It is absent when
	// no process result was observed (supplied analysis function, or an earlier
	// ledger), which reads as unknown rather than as a clean run.
	Diagnostics *process.Diagnostics `json:"diagnostics,omitempty"`
	// Usage records the effective model, tokens and reported cost parsed from the
	// Claude CLI final JSON (#149); additive and omitempty. It is absent for an
	// analysis wrapper, a supplied analysis function, a non-Claude binding, a Claude
	// run that emitted none, and every earlier ledger, all of which read as
	// "unavailable" rather than as zero.
	Usage *harness.Usage `json:"usage,omitempty"`
}

// PolicyEvidence points at the ledger value a policy decision relied on.
// Pointer is an RFC 6901 JSON pointer into the marshalled Record.
type PolicyEvidence struct {
	Pointer  string `json:"pointer"`
	Observed string `json:"observed"`
}

// PolicyDecision records an automatic gate decision. ApprovedBy names the policy
// and the exact Formula bytes it came from; it is never a human identity.
type PolicyDecision struct {
	Policy        string           `json:"policy"`
	ApprovedBy    string           `json:"approvedBy"`
	Gate          string           `json:"gate"`
	PlanDigest    string           `json:"planDigest"`
	FormulaSHA256 string           `json:"formulaSha256"`
	At            string           `json:"at"`
	Evidence      []PolicyEvidence `json:"evidence"`
}
type Record struct {
	PolicyDecisions               []PolicyDecision  `json:"policyDecisions,omitempty"`
	ClarificationPredecessors     []string          `json:"clarificationPredecessors,omitempty"`
	ClarificationHistoryTruncated bool              `json:"clarificationHistoryTruncated,omitempty"`
	CheckoutStatus                string            `json:"checkoutStatus,omitempty"`
	AnalysisAttempts              []AnalysisAttempt `json:"analysisAttempts,omitempty"`
	HarnessAttempts               []HarnessAttempt  `json:"harnessAttempts,omitempty"`
	Extensions                    []ExtensionRecord `json:"extensions,omitempty"`
	// AdapterProtocol is the adapter protocol the intake fingerprinted the coding
	// binding under. A ledger written before it was recorded has none, which means an
	// earlier protocol than the one that introduced the field.
	AdapterProtocol string `json:"adapterProtocol,omitempty"`
	// ReviewAttempts is additive: a Lab older than the review Atom decodes it but drops
	// it on the next save. See ReviewAttempt.
	ReviewAttempts []ReviewAttempt `json:"reviewAttempts,omitempty"`

	VerificationAttempts []VerificationAttempt         `json:"verificationAttempts,omitempty"`
	Readiness            *readiness.State              `json:"readiness,omitempty"`
	Analysis             *readiness.Analysis           `json:"analysis,omitempty"`
	RemotePolicy         *readiness.RemotePolicy       `json:"remotePolicy,omitempty"`
	RepositoryEvidence   *readiness.RepositorySnapshot `json:"repositoryEvidence,omitempty"`
	Captures             []verification.CaptureResult  `json:"captures,omitempty"`
	Capture              *verification.CaptureResult   `json:"capture,omitempty"`
	Recovery             *verification.CaptureResult   `json:"recovery,omitempty"`
	Verification         *verification.Result          `json:"verification,omitempty"`

	SchemaVersion   string    `json:"schemaVersion"`
	Kind            string    `json:"kind"`
	ID              string    `json:"id"`
	Mode            string    `json:"mode"`
	Status          string    `json:"status"`
	FormulaID       string    `json:"formulaId"`
	FormulaApproved bool      `json:"formulaApproved"`
	FormulaSnapshot Snapshot  `json:"formulaSnapshot"`
	StartedAt       string    `json:"startedAt"`
	FinishedAt      string    `json:"finishedAt"`
	LabDir          string    `json:"labDir"`
	Workspace       Workspace `json:"workspace"`
	Issue           Issue     `json:"issue"`
	Atoms           []Atom    `json:"atoms"`
	Notes           []string  `json:"notes"`
}
type Options struct {
	MandatoryConstraints []string
	// Explicit reviewed inputs for controlled evaluations; still require the same
	// persisted plan digest and approval transition as every interactive run.
	CheckInputs []readiness.FileEvidence
	Checks      []readiness.Check
	Policy      *readiness.ExecutionPolicy
	Setup       []readiness.Check
	Artifacts   []readiness.ArtifactRoot

	LabDir, SourceRoot, Issue, FormulaID, Base string
	Approve, TargetBranch                      string
	Analyze                                    func(context.Context, formula.Binding, Issue, readiness.RepositorySnapshot) (readiness.Analysis, error)
	ReadIssue                                  func(context.Context, string, int64, string) (Issue, error)
	PrepareWorkspace                           func(context.Context, Workspace) error
	// InvokeReview and ReviewDiff replace the review process and the diff read, so
	// tests never start a provider. InvokeReview receives the resolved binding, the
	// directory for project-configuration checks, the timeout, the request and the
	// environment names the review must never see.
	InvokeReview func(context.Context, formula.Binding, string, time.Duration, map[string]any, []string) process.Result
	ReviewDiff   func(context.Context, Workspace, string, string) (string, error)
}

func stamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func token() (string, error) {
	var b [12]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func git(ctx context.Context, cwd string, args ...string) (string, error) {
	r := process.Run(ctx, process.Options{Argv: append([]string{"git"}, args...), Dir: cwd, Stdin: []byte{}, Timeout: 20 * time.Second})
	if !r.OK {
		return "", fmt.Errorf("git failed: %s %s", r.Error, r.Stderr)
	}
	return strings.TrimSpace(r.Stdout), nil
}

var originPattern = regexp.MustCompile(`(?i)^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([a-z0-9_.-]+/[a-z0-9_.-]+?)/?$`)

func resolveSource(ctx context.Context, o Options, repo string) (Workspace, error) {
	w := Workspace{Repo: repo}
	root, err := git(ctx, o.SourceRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return w, err
	}
	w.SourceRoot = root
	origin, err := git(ctx, root, "remote", "get-url", "origin")
	if err != nil {
		return w, err
	}
	m := originPattern.FindStringSubmatch(origin)
	if m == nil || !strings.EqualFold(strings.TrimSuffix(strings.ToLower(m[1]), ".git"), strings.ToLower(repo)) {
		return w, fmt.Errorf("Git origin does not match the issue repository")
	}
	base := o.Base
	if base == "" {
		base = "HEAD"
	}
	if strings.HasPrefix(base, "-") {
		return w, fmt.Errorf("invalid base commit")
	}
	w.BaseCommit, err = git(ctx, root, "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		return w, err
	}
	return w, nil
}

func planWorkspace(o Options, w Workspace, number int64) (Workspace, error) {
	nonce, err := token()
	if err != nil {
		return w, err
	}
	name := fmt.Sprintf("issue-%d-%s", number, nonce)
	w.Branch = "forgecell/" + name
	w.Path = filepath.Join(o.LabDir, "workspaces", name)
	return w, nil
}

func allocateWorkspace(ctx context.Context, w Workspace) error {
	if err := os.MkdirAll(filepath.Dir(w.Path), 0700); err != nil {
		return err
	}
	_, err := git(ctx, w.SourceRoot, "worktree", "add", "-b", w.Branch, w.Path, w.BaseCommit)
	return err
}
func readIssue(ctx context.Context, repo string, number int64, cwd string) (Issue, error) {
	r := process.Run(ctx, process.Options{Argv: []string{"gh", "issue", "view", fmt.Sprint(number), "--repo", repo, "--json", "number,title,body,state,url,updatedAt"}, Dir: cwd, Stdin: []byte{}, Timeout: 20 * time.Second})
	if !r.OK {
		return Issue{}, fmt.Errorf("GitHub intake failed: %s %s", r.Error, r.Stderr)
	}
	var issue Issue
	if err := json.Unmarshal([]byte(r.Stdout), &issue); err != nil {
		return issue, err
	}
	issue.Repo = repo
	return issue, nil
}

// Generated bindings invoke the adapter in-process so a single managed process group
// owns the provider and its descendants. An outer wrapper must not orphan that group.
func invokeHarness(ctx context.Context, command []string, dir string, timeout time.Duration, request map[string]any) process.Result {
	return invokeHarnessWith(ctx, nil, command, dir, timeout, request)
}

// invokeHarnessWith runs an attempt that already resolved its saved launcher, so the
// identity that gates the run is the one the attempt records.
func invokeHarnessWith(ctx context.Context, id *harness.LauncherIdentity, command []string, dir string, timeout time.Duration, request map[string]any) process.Result {
	return harness.InvokeBindingWith(ctx, id, command, dir, timeout, request)
}

func createWorkspace(ctx context.Context, o Options, repo string, number int64) (Workspace, error) {
	w, err := resolveSource(ctx, o, repo)
	if err != nil {
		return w, err
	}
	w, err = planWorkspace(o, w, number)
	if err != nil {
		return w, err
	}
	return w, allocateWorkspace(ctx, w)
}
