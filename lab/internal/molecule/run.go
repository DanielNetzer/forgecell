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
	Coding          *harness.CodingOutcome      `json:"coding,omitempty"`
	ID              string                      `json:"id"`
	PlanDigest      string                      `json:"planDigest"`
	Result          process.Result              `json:"result"`
	CaptureEvidence []VerificationFile          `json:"captureEvidence,omitempty"`
	Evidence        *VerificationFile           `json:"evidence,omitempty"`
	Capture         *verification.CaptureResult `json:"capture,omitempty"`
	CaptureError    string                      `json:"captureError,omitempty"`
	Recovered       bool                        `json:"recovered,omitempty"`
}
type AnalysisAttempt struct {
	Ordinal        int                 `json:"ordinal"`
	SnapshotSHA256 string              `json:"snapshotSha256"`
	StartedAt      string              `json:"startedAt"`
	FinishedAt     string              `json:"finishedAt,omitempty"`
	Status         string              `json:"status"`
	Response       *readiness.Analysis `json:"response,omitempty"`
	Error          string              `json:"error,omitempty"`
}
type Record struct {
	ClarificationPredecessors     []string          `json:"clarificationPredecessors,omitempty"`
	ClarificationHistoryTruncated bool              `json:"clarificationHistoryTruncated,omitempty"`
	CheckoutStatus                string            `json:"checkoutStatus,omitempty"`
	AnalysisAttempts              []AnalysisAttempt `json:"analysisAttempts,omitempty"`
	HarnessAttempts               []HarnessAttempt  `json:"harnessAttempts,omitempty"`

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
	return harness.InvokeBinding(ctx, command, dir, timeout, request)
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
