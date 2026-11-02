package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// FixRoundPreview is exactly what a fix-round push binds: the change request's
// branch, the head it builds on, the verified tree, the files it changes, the
// approved revision, the round, the findings it answers and the commit title,
// and whether it needs an explicit confirmation. Digest covers every other
// field.
type FixRoundPreview struct {
	MoleculeID           string   `json:"moleculeId"`
	Repo                 string   `json:"repo"`
	Branch               string   `json:"branch"`
	Parent               string   `json:"parent"`
	Tree                 string   `json:"tree"`
	Files                []string `json:"files"`
	PlanDigest           string   `json:"planDigest"`
	Round                int      `json:"round"`
	Findings             []string `json:"findings"`
	Title                string   `json:"title"`
	AcceptanceUnverified bool     `json:"acceptanceUnverified"`
	ReviewGreen          bool     `json:"reviewGreen"`
	Digest               string   `json:"digest"`
}

// NeedsConfirm reports a push that waits for an explicit confirmation: the
// independent acceptance is unverified, or the review is not green on this
// tree and plan.
func (p FixRoundPreview) NeedsConfirm() bool { return p.AcceptanceUnverified || !p.ReviewGreen }

func fixRoundPreviewDigest(p FixRoundPreview) string {
	p.Digest = ""
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// FixRoundApprovalRequired is PushFixRound's refusal without an approval: the
// preview to review and the digest to approve.
type FixRoundApprovalRequired struct{ Preview FixRoundPreview }

func (e *FixRoundApprovalRequired) Error() string {
	confirm := ""
	if e.Preview.NeedsConfirm() {
		confirm = " with --confirm"
	}
	return fmt.Sprintf("review this fix round %d push, then approve exactly digest %s%s", e.Preview.Round, e.Preview.Digest, confirm)
}

// fixRoundState is everything a push binds, read and checked.
type fixRoundState struct {
	receipt   Receipt
	record    molecule.Record
	head      string
	workspace string
	// committed is the workspace HEAD when it is already the fix-round commit
	// of an interrupted push, or "" when the workspace HEAD is the head.
	committed string
	preview   FixRoundPreview
}

// PreviewFixRound returns what PushFixRound would push, refusing as it does,
// without committing, pushing or contacting a forge.
func PreviewFixRound(ctx context.Context, o FixRoundPushOptions) (FixRoundPreview, error) {
	abs, err := filepath.Abs(o.LabDir)
	if err != nil {
		return FixRoundPreview{}, fmt.Errorf("the Lab directory cannot be resolved")
	}
	o.LabDir = abs
	if err := RefuseDemoLab(o.LabDir); err != nil {
		return FixRoundPreview{}, err
	}
	o.Approve, o.Confirm = "", false
	st, err := loadFixRound(ctx, o, false)
	return st.preview, err
}

// loadFixRound reads the receipt and the Molecule and applies every refusal
// that needs no process first: an unpublished receipt, a latest plan without
// a fix round or on another head or round, a round past MaxFixRounds, a
// verification that did not pass, and, when push is set and Approve is given,
// a missing Confirm. Only then does it read the workspace with git, read-only:
// its tree must be the verified tree.
func loadFixRound(ctx context.Context, o FixRoundPushOptions, push bool) (fixRoundState, error) {
	var st fixRoundState
	if !safeID.MatchString(o.MoleculeID) {
		return st, fmt.Errorf("invalid Molecule id")
	}
	if o.Title != "" && !fixRoundTitleText(o.Title) {
		return st, fmt.Errorf("a fix round title is one trimmed line of at most %d bytes without control characters", maxFixRoundTitleBytes)
	}
	if o.Approve != "" && !fixRoundDigest.MatchString(o.Approve) {
		return st, fmt.Errorf("provide the exact fix round preview digest")
	}
	receipt, err := ReadFixRoundReceipt(o.LabDir, o.MoleculeID)
	if err != nil {
		return st, err
	}
	if push {
		if receipt.Forge == "" && o.Changes != nil {
			return st, fmt.Errorf("this delivery was published through GitHub; push its fix round with the Formula that published it")
		}
		if receipt.Forge != "" && (o.Changes == nil || receipt.Forge != o.Plugin.Name || receipt.Project != o.Plugin.Project) {
			return st, fmt.Errorf("this delivery was published through a forge plugin; push its fix round with the Formula that declares that plugin and project")
		}
	}
	record, err := molecule.ReadRecord(o.LabDir, o.MoleculeID)
	if err != nil {
		return st, fmt.Errorf("the Molecule ledger cannot be read, so its fix round is unknown")
	}
	if record.Readiness == nil || len(record.Readiness.Plans) == 0 {
		return st, fmt.Errorf("the Molecule has no readiness plan, so it has no fix round")
	}
	latest := record.Readiness.Plans[len(record.Readiness.Plans)-1]
	ref := latest.Plan.FixRound
	if ref == nil {
		return st, fmt.Errorf("the latest plan is not a fix round; propose one with forgecell findings %s --propose", o.MoleculeID)
	}
	round := len(receipt.FixRounds) + 1
	if round > MaxFixRounds {
		return st, fmt.Errorf("the change request already has %d fix rounds, so fix rounds stop", MaxFixRounds)
	}
	head, headTree := ReceiptHead(receipt)
	if ref.Head != head || ref.HeadTree != headTree {
		return st, fmt.Errorf("fix round %d was proposed on commit %s, but the change request's head is %s; propose the fix round again", ref.Round, ref.Head, head)
	}
	if ref.Round != round {
		return st, fmt.Errorf("the latest plan is fix round %d, but the change request's next fix round is %d", ref.Round, round)
	}
	if receipt.Preview.MoleculeID != record.ID || receipt.Preview.Branch != record.Workspace.Branch || !strings.EqualFold(receipt.Preview.Repo, record.Workspace.Repo) {
		return st, fmt.Errorf("the delivery receipt does not match the Molecule's workspace")
	}
	v := record.Verification
	if !fixRoundVerified(record, latest.Digest) {
		return st, fmt.Errorf("fix round %d has no passing verification of its approved plan, so it cannot be pushed", round)
	}
	acceptanceUnverified := !v.IndependentAcceptanceVerified
	reviewGreen := fixRoundReviewGreen(record, latest.Digest, v.SourceTree)
	if push && o.Approve != "" && !o.Confirm {
		if acceptanceUnverified {
			return st, fmt.Errorf("independent acceptance of fix round %d is unverified, so the push waits for an explicit confirmation: add --confirm", round)
		}
		if !reviewGreen {
			return st, fmt.Errorf("the review of fix round %d is not green on this tree and plan (or the Formula has no review Atom), so the push waits for an explicit confirmation: add --confirm", round)
		}
	}
	title := o.Title
	if title == "" {
		title = DefaultFixRoundTitle(round, record.Issue)
	}
	findings := make([]string, 0, len(ref.Findings))
	for _, f := range ref.Findings {
		findings = append(findings, f.Fingerprint)
	}
	st = fixRoundState{receipt: receipt, record: record, head: head}
	files, committed, err := fixRoundWorkspace(ctx, record, latest.Plan, head, headTree, v.SourceTree, title)
	if err != nil {
		return st, err
	}
	st.workspace, st.committed = record.Workspace.Path, committed
	st.preview = FixRoundPreview{MoleculeID: record.ID, Repo: receipt.Preview.Repo, Branch: receipt.Preview.Branch, Parent: head, Tree: v.SourceTree, Files: files, PlanDigest: latest.Digest, Round: round, Findings: findings, Title: title, AcceptanceUnverified: acceptanceUnverified, ReviewGreen: reviewGreen}
	st.preview.Digest = fixRoundPreviewDigest(st.preview)
	return st, nil
}

// fixRoundVerified reports a passing verification of the plan digest: required
// checks passed on an exact tree, the readiness waits at the review gate after
// that plan's verification, and the plan's latest verification attempt passed
// on that tree.
func fixRoundVerified(r molecule.Record, digest string) bool {
	v := r.Verification
	if v == nil || !v.RequiredChecksPassed || v.Error != "" || !fixRoundSHA.MatchString(v.SourceTree) || r.Readiness.Phase != "review-waiting" {
		return false
	}
	waiting := false
	for _, e := range r.Readiness.Events {
		if e.Digest == digest && e.Kind == readiness.VerificationReviewWaiting {
			waiting = true
		}
	}
	if !waiting {
		return false
	}
	for i := len(r.VerificationAttempts) - 1; i >= 0; i-- {
		if a := r.VerificationAttempts[i]; a.PlanDigest == digest {
			return a.Status == "passed" && a.SourceTree == v.SourceTree
		}
	}
	return false
}

// fixRoundReviewGreen reports a Molecule whose Formula runs the review Atom and
// whose latest review pass of the plan digest found no high finding on tree.
func fixRoundReviewGreen(r molecule.Record, digest, tree string) bool {
	atom := false
	for _, a := range r.Atoms {
		if a.Type == "review" {
			atom = true
		}
	}
	if !atom {
		return false
	}
	for i := len(r.ReviewAttempts) - 1; i >= 0; i-- {
		if a := r.ReviewAttempts[i]; a.PlanDigest == digest {
			return a.Status == molecule.ReviewNoHighFindings && a.Tree == tree
		}
	}
	return false
}

// fixRoundWorkspace reads the Molecule workspace, read-only. Its branch must
// be the change request's, and its HEAD the head with the verified tree in the
// working copy and no change outside the plan's scope, or an interrupted
// push's fix-round commit of exactly that tree on the head. It returns the
// changed files and that commit, if any.
func fixRoundWorkspace(ctx context.Context, r molecule.Record, p readiness.Plan, head, headTree, tree, title string) ([]string, string, error) {
	path, err := filepath.EvalSymlinks(r.Workspace.Path)
	if err != nil {
		return nil, "", fmt.Errorf("the Molecule workspace is unavailable")
	}
	if source, err := filepath.EvalSymlinks(r.Workspace.SourceRoot); err != nil || source == path {
		return nil, "", fmt.Errorf("a fix round cannot be pushed from the source checkout")
	}
	branch, err := git(ctx, path, nil, "branch", "--show-current")
	if err != nil || strings.TrimSpace(branch) != r.Workspace.Branch {
		return nil, "", fmt.Errorf("the workspace branch changed")
	}
	raw, err := git(ctx, path, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, "", fmt.Errorf("the workspace HEAD cannot be read")
	}
	current := strings.TrimSpace(raw)
	if tree == headTree {
		return nil, "", fmt.Errorf("the verified tree is the change request's head tree, so fix round has nothing to push")
	}
	scope := []string{}
	inScope := map[string]bool{}
	for _, s := range p.Analysis.Scope {
		scope = append(scope, s.Path)
		inScope[s.Path] = true
	}
	var files []string
	committed := ""
	if current == head {
		captured, err := verification.Capture(ctx, path, head, scope, p.Artifacts)
		if err != nil {
			return nil, "", fmt.Errorf("the workspace could not be captured: %w", err)
		}
		if len(captured.Violations) > 0 {
			return nil, "", fmt.Errorf("the workspace has changes outside the fix round's approved scope: [%s]", molecule.BoundedViolations(captured.Violations))
		}
		if captured.Tree != tree {
			return nil, "", fmt.Errorf("the workspace tree is not the verified tree; verify the fix round again")
		}
		files = captured.Changes
	} else {
		// An interrupted push left its commit: it is reused only when it is exactly
		// the fix-round commit this push would make.
		if err := verifyFixRoundCommit(ctx, path, current, head, tree, title); err != nil {
			return nil, "", fmt.Errorf("the workspace HEAD moved off the change request's head: %w", err)
		}
		captured, err := verification.Capture(ctx, path, current, nil, p.Artifacts)
		if err != nil || len(captured.Changes) > 0 || captured.Tree != tree {
			return nil, "", fmt.Errorf("the workspace has changes beyond its fix-round commit")
		}
		names, err := git(ctx, path, nil, "diff", "--name-only", "-z", "--no-renames", head, current, "--")
		if err != nil {
			return nil, "", fmt.Errorf("the fix-round commit's files cannot be read")
		}
		for _, f := range strings.Split(names, "\x00") {
			if f == "" {
				continue
			}
			if !inScope[f] {
				return nil, "", fmt.Errorf("the fix-round commit changes a file outside the approved scope")
			}
			files = append(files, f)
		}
		committed = current
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("fix round has no changes to push")
	}
	if len(files) > maxFixRoundFiles {
		return nil, "", fmt.Errorf("a fix round changes at most %d files", maxFixRoundFiles)
	}
	files = append([]string{}, files...)
	sort.Strings(files)
	return unique(files), committed, nil
}

// verifyFixRoundCommit requires commit to have exactly the parent head, the
// tree and the message title.
func verifyFixRoundCommit(ctx context.Context, path, commit, head, tree, title string) error {
	parents, err := git(ctx, path, nil, "rev-list", "--parents", "-n", "1", commit)
	if err != nil || strings.TrimSpace(parents) != commit+" "+head {
		return fmt.Errorf("the fix-round commit does not have exactly the change request's head as its parent")
	}
	got, err := git(ctx, path, nil, "rev-parse", commit+"^{tree}")
	if err != nil || strings.TrimSpace(got) != tree {
		return fmt.Errorf("the fix-round commit differs from the verified tree")
	}
	message, err := git(ctx, path, nil, "show", "-s", "--format=%B", commit)
	if err != nil || strings.TrimSpace(message) != title {
		return fmt.Errorf("the fix-round commit message differs from the approved title")
	}
	return nil
}
