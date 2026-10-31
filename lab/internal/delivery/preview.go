// Package delivery binds a reviewable diff to one finished Molecule and exact file scope.
package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

type Options struct {
	LabDir, MoleculeID, BaseBranch string
	Files                          []string
}
type PreviewResult struct {
	AcceptanceUnverified bool     `json:"acceptanceUnverified"`
	Digest               string   `json:"digest"`
	Tree                 string   `json:"tree"`
	Files                []string `json:"files"`
	Diff                 string   `json:"diff"`
	BaseCommit           string   `json:"baseCommit"`
	BaseBranch           string   `json:"baseBranch"`
	Repo                 string   `json:"repo"`
	MoleculeID           string   `json:"moleculeId"`
	Branch               string   `json:"branch"`
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var sha = regexp.MustCompile(`^[a-f0-9]{40}$`)
var origin = regexp.MustCompile(`(?i)^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([a-z0-9_.-]+/[a-z0-9_.-]+?)/?$`)

func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	r := process.Run(ctx, process.Options{Argv: append([]string{"git"}, args...), Dir: dir, Env: env, Stdin: []byte{}, Timeout: 30 * time.Second, MaxOutputBytes: 4_000_000})
	if !r.OK {
		return "", fmt.Errorf("git %s failed: %s %s", args[0], r.Error, r.Stderr)
	}
	return r.Stdout, nil
}
func validated(ctx context.Context, o Options) (molecule.Record, error) {
	return validatedHead(ctx, o, "")
}
func validatedHead(ctx context.Context, o Options, expected string) (molecule.Record, error) {
	var r molecule.Record
	if !safeID.MatchString(o.MoleculeID) {
		return r, fmt.Errorf("invalid Molecule id")
	}
	file := filepath.Join(o.LabDir, "ledgers", o.MoleculeID+".json")
	stat, err := os.Lstat(file)
	if err != nil {
		return r, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 8_000_000 {
		return r, fmt.Errorf("invalid ledger file")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if r.ID != o.MoleculeID || r.Kind != "molecule" || r.SchemaVersion != "v0" || !r.FormulaApproved || r.FinishedAt == "" || (r.Status != "waiting" && r.Status != "recorded") {
		return r, fmt.Errorf("delivery requires a finished approved Molecule")
	}
	ran := molecule.CompletedCoding(r)
	if r.Readiness != nil && len(r.Readiness.Plans) > 0 && r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan.Continuation == "adopt-failed-tree" && !molecule.FailedTreeAdopted(r) {
		return r, fmt.Errorf("invalid failed-tree adoption evidence")
	}
	if molecule.FailedTreeAdopted(r) {
		approved := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan.Inputs.Issue
		if approved.Repository != r.Issue.Repo || approved.Number != r.Issue.Number || approved.Key != r.Issue.Key || approved.URL != r.Issue.URL || approved.Title != r.Issue.Title || approved.Body != r.Issue.Body || approved.State != r.Issue.State {
			return r, fmt.Errorf("adopted failed-tree issue identity changed")
		}
	}
	ran = ran || molecule.FailedTreeAdopted(r)
	w := r.Workspace
	if !ran || w.Repo == "" || !strings.EqualFold(w.Repo, r.Issue.Repo) || !sha.MatchString(w.BaseCommit) || !strings.HasPrefix(w.Branch, "forgecell/") {
		return r, fmt.Errorf("invalid isolated Molecule workspace")
	}
	// The recorded checkout must still be a separate worktree, never the source checkout.
	path, err := filepath.EvalSymlinks(w.Path)
	if err != nil {
		return r, err
	}
	source, err := filepath.EvalSymlinks(w.SourceRoot)
	if err != nil {
		return r, err
	}
	if path == source {
		return r, fmt.Errorf("delivery cannot operate in the source checkout")
	}
	root, err := git(ctx, path, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return r, err
	}
	resolved, err := filepath.EvalSymlinks(strings.TrimSpace(root))
	if err != nil || resolved != path {
		return r, fmt.Errorf("workspace root changed")
	}
	for _, args := range [][]string{{"remote", "get-url", "--all", "origin"}, {"remote", "get-url", "--push", "--all", "origin"}} {
		urls, err := git(ctx, path, nil, args...)
		if err != nil {
			return r, err
		}
		for _, url := range strings.Split(strings.TrimSpace(urls), "\n") {
			m := origin.FindStringSubmatch(url)
			if m == nil || !strings.EqualFold(strings.TrimSuffix(strings.ToLower(m[1]), ".git"), w.Repo) {
				return r, fmt.Errorf("Git origin or push destination differs from intake")
			}
		}
	}
	branch, err := git(ctx, path, nil, "branch", "--show-current")
	if err != nil || strings.TrimSpace(branch) != w.Branch {
		return r, fmt.Errorf("workspace branch changed")
	}
	if expected == "" {
		expected = w.BaseCommit
	}
	head, err := git(ctx, path, nil, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != expected {
		return r, fmt.Errorf("workspace HEAD changed; review required")
	}
	if o.BaseBranch == "" || strings.HasPrefix(o.BaseBranch, "-") || o.BaseBranch == w.Branch {
		return r, fmt.Errorf("invalid PR base branch")
	}
	if _, err = git(ctx, path, nil, "check-ref-format", "refs/heads/"+o.BaseBranch); err != nil {
		return r, err
	}
	if err = validateBase(ctx, path, w.BaseCommit, o.BaseBranch); err != nil {
		return r, err
	}
	if err = validateReadiness(ctx, o, r, expectedHeadForReadiness(expected, w.BaseCommit)); err != nil {
		return r, err
	}
	if molecule.FailedTreeAdopted(r) && expected == w.BaseCommit {
		plan := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
		allowed := []string{}
		for _, s := range plan.Analysis.Scope {
			allowed = append(allowed, s.Path)
		}
		captured, e := verification.Capture(ctx, w.Path, w.BaseCommit, allowed, plan.Artifacts)
		if e != nil {
			return r, e
		}
		raw, _ := json.Marshal(captured.Artifacts)
		if captured.Tree != plan.PausedTree || fmt.Sprintf("%x", sha256.Sum256(raw)) != plan.PausedArtifactsSHA256 {
			return r, fmt.Errorf("adopted failed-tree source or artifacts changed before delivery")
		}
	}
	return r, nil
}
func expectedHeadForReadiness(expected, base string) string {
	if expected == base {
		return ""
	}
	return expected
}
func validateBase(ctx context.Context, path, base, branch string) error {
	if _, err := git(ctx, path, nil, "merge-base", "--is-ancestor", base, "refs/remotes/origin/"+branch); err != nil {
		return fmt.Errorf("recorded Molecule base must be an ancestor of the PR base; fetch origin and review the target history")
	}
	return nil
}

// DemoFormulaID and DemoSourceKind identify the Lab forgecell demo creates. Its
// intake and origin name a GitHub repository that is never contacted, and that
// nobody may own, so commands that reach GitHub refuse such a Lab.
const (
	DemoFormulaID  = "forgecell-demo"
	DemoSourceKind = "demo"
)

// ErrDemoLab is the refusal for a command that would contact GitHub in a demo Lab.
var ErrDemoLab = errors.New("this is a forgecell demo Lab; it never contacts GitHub. Inspect it with forgecell ledger ID --lab DIR, or run forgecell init in a Lab of your own repository")

// maxLabConfigBytes bounds the lab.json read to identify a demo Lab.
const maxLabConfigBytes = 1 << 20

// IsDemoLab reports whether lab's active Formula activation is the demo's. It
// reads what formula.Load reads, following a symbolic link the same way. An
// absent lab.json, or one that is not JSON, is not a demo Lab: formula.Load
// refuses that configuration itself, so the command reports its own error. A
// lab.json that is present but cannot be read as a bounded regular file is an
// error, so callers fail closed.
func IsDemoLab(lab string) (bool, error) {
	f, err := os.Open(filepath.Join(lab, "lab.json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("the Lab configuration could not be checked for a demo Lab")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxLabConfigBytes {
		return false, fmt.Errorf("the Lab configuration is not a bounded regular file, so it could not be checked for a demo Lab")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxLabConfigBytes+1))
	if err != nil || len(raw) > maxLabConfigBytes {
		return false, fmt.Errorf("the Lab configuration could not be read within %d bytes to check for a demo Lab", maxLabConfigBytes)
	}
	var cfg struct {
		Active   string `json:"activeFormulaId"`
		Approval struct {
			FormulaID  string `json:"formulaId"`
			SourceKind string `json:"sourceKind"`
		} `json:"currentApproval"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return false, nil
	}
	return cfg.Active == DemoFormulaID || cfg.Approval.FormulaID == DemoFormulaID || cfg.Approval.SourceKind == DemoSourceKind, nil
}

// RefuseDemoLab returns ErrDemoLab for a demo Lab, and an error when lab cannot
// be checked. It starts no process.
func RefuseDemoLab(lab string) error {
	demo, err := IsDemoLab(lab)
	if err != nil {
		return err
	}
	if demo {
		return ErrDemoLab
	}
	return nil
}

// Preview binds the diff of a finished Molecule to its exact file scope. It
// refuses a demo Lab before any git process starts.
func Preview(ctx context.Context, o Options) (PreviewResult, error) {
	absolute, err := filepath.Abs(o.LabDir)
	if err != nil {
		return PreviewResult{}, err
	}
	if err = RefuseDemoLab(absolute); err != nil {
		return PreviewResult{}, err
	}
	o.LabDir = absolute
	return preview(ctx, o)
}

// PreviewDemo is Preview for forgecell demo's own Lab, and only for it: it
// refuses any other Lab. The preview is local; Publish still refuses a demo Lab.
func PreviewDemo(ctx context.Context, o Options) (PreviewResult, error) {
	absolute, err := filepath.Abs(o.LabDir)
	if err != nil {
		return PreviewResult{}, err
	}
	if demo, err := IsDemoLab(absolute); err != nil {
		return PreviewResult{}, err
	} else if !demo {
		return PreviewResult{}, fmt.Errorf("the demo preview runs only in a forgecell demo Lab")
	}
	o.LabDir = absolute
	return preview(ctx, o)
}

func preview(ctx context.Context, o Options) (PreviewResult, error) {
	var p PreviewResult
	r, err := validated(ctx, o)
	if err != nil {
		return p, err
	}
	files := map[string]bool{}
	for _, f := range o.Files {
		if f == "" || filepath.IsAbs(f) || strings.ContainsAny(f, "\x00\\") || filepath.ToSlash(filepath.Clean(f)) != f {
			return p, fmt.Errorf("provide exact repository-relative file paths")
		}
		for _, part := range strings.Split(f, "/") {
			if part == ".." || part == "." || strings.EqualFold(part, ".git") {
				return p, fmt.Errorf("invalid file scope")
			}
		}
		files[f] = true
	}
	if len(files) == 0 {
		return p, fmt.Errorf("provide explicit file scope")
	}
	approved := map[string]bool{}
	plan := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
	for _, s := range plan.Analysis.Scope {
		approved[s.Path] = true
	}
	for f := range files {
		if !approved[f] {
			return p, fmt.Errorf("file was not approved before coding: %s", f)
		}
		p.Files = append(p.Files, f)
	}
	sort.Strings(p.Files)
	captured, err := verification.Capture(ctx, r.Workspace.Path, r.Workspace.BaseCommit, p.Files, plan.Artifacts)
	if err != nil {
		return p, err
	}
	if len(captured.Violations) > 0 || len(captured.Changes) == 0 {
		return p, fmt.Errorf("delivery has no changes or exceeds explicitly reviewed scope: [%s]", molecule.BoundedViolations(captured.Violations))
	}
	p.Tree = captured.Tree
	if p.Tree != r.Verification.SourceTree {
		return p, fmt.Errorf("delivery tree differs from verified source")
	}
	p.AcceptanceUnverified = !r.Verification.IndependentAcceptanceVerified
	p.Diff, err = git(ctx, r.Workspace.Path, nil, "diff", "--binary", "--no-ext-diff", "--no-textconv", r.Workspace.BaseCommit, p.Tree, "--")
	if err != nil {
		return p, err
	}
	p.BaseCommit = r.Workspace.BaseCommit
	p.BaseBranch = o.BaseBranch
	p.Repo = r.Workspace.Repo
	p.MoleculeID = r.ID
	p.Branch = r.Workspace.Branch
	identity := p
	identity.Diff = ""
	raw, _ := json.Marshal(identity)
	sum := sha256.Sum256(raw)
	p.Digest = hex.EncodeToString(sum[:])
	return p, nil
}
