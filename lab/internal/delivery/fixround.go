package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// Bounds of one fix-round push.
const (
	// maxFixRoundTitleBytes bounds a fix-round commit title.
	maxFixRoundTitleBytes = 200
	// maxFixRoundFiles bounds the files one fix-round commit changes.
	maxFixRoundFiles = 1000
	// fixRoundRemoteOutputBytes bounds the output of one push or forge read.
	fixRoundRemoteOutputBytes = 1_000_000
)

// ErrFixRoundBranchMoved refuses a push the remote rejected as not a fast
// forward: someone else moved the change request's branch.
var ErrFixRoundBranchMoved = errors.New("the change request's branch moved; collect again")

// FixRoundPushOptions configure PushFixRound and PreviewFixRound. Approve is
// the exact FixRoundPreview digest; Confirm is the explicit push confirmation
// an unverified acceptance or a review that is not green requires; Title
// overrides the default commit message. Runner runs git push and, for GitHub,
// gh. Changes, when set, re-reads the change through a plugin forge that
// Plugin names; nil keeps GitHub. Now defaults to time.Now.
type FixRoundPushOptions struct {
	LabDir, MoleculeID string
	Approve, Title     string
	Confirm            bool
	Runner             func(context.Context, process.Options) process.Result
	Changes            forge.Changes
	Plugin             PluginForge
	Now                func() time.Time
}

// ReadFixRoundReceipt reads a delivery receipt a fix round may build on: a
// bounded regular file, never a symlink, for this Molecule, published at an
// exact commit with a valid fix-round history.
func ReadFixRoundReceipt(lab, id string) (Receipt, error) {
	f, found, err := openReceipt(lab, id)
	if f == nil {
		if err == nil && !found {
			err = fmt.Errorf("no delivery receipt exists for this Molecule, so it has no change request to fix")
		}
		return Receipt{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxReceiptBytes+1))
	if err != nil || len(raw) > maxReceiptBytes {
		return Receipt{}, fmt.Errorf("delivery receipt is not a bounded regular file")
	}
	var r Receipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return Receipt{}, fmt.Errorf("malformed delivery receipt")
	}
	if err := checkReceipt(id, r.Preview.MoleculeID, r.State); err != nil {
		return Receipt{}, err
	}
	if r.State != "published" || !fixRoundSHA.MatchString(r.Commit) {
		return Receipt{}, fmt.Errorf("the delivery is %s, not published; a fix round builds only on a published change request", r.State)
	}
	if err := checkFixRounds(r); err != nil {
		return Receipt{}, fmt.Errorf("delivery receipt fix rounds are invalid: %w", err)
	}
	return r, nil
}

// DefaultFixRoundTitle is the default fix-round commit message:
// fix: address reviewer findings, round N (#<issue or key>). A key that is not
// a valid tracker key is left out.
func DefaultFixRoundTitle(round int, issue molecule.Issue) string {
	title := fmt.Sprintf("fix: address reviewer findings, round %d", round)
	switch {
	case issue.IsKeyed():
		if intake.ValidateIssueKey(issue.Key) == nil && fixRoundTitleText(issue.Key) {
			title += " (#" + issue.Key + ")"
		}
	case issue.Number > 0:
		title += fmt.Sprintf(" (#%d)", issue.Number)
	}
	return title
}

// fixRoundTitleText reports one bounded, trimmed line of valid UTF-8 without
// control or format characters.
func fixRoundTitleText(s string) bool {
	if s == "" || strings.TrimSpace(s) != s || len(s) > maxFixRoundTitleBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Bidi_Control) {
			return false
		}
	}
	return true
}

// PushFixRound pushes the approved fix round of a published delivery to the
// same change request's branch, never by force, and records it on the
// receipt. Without Approve it returns *FixRoundApprovalRequired with the
// preview to approve; with it, the preview digest must match exactly. Every
// refusal that needs no process comes before any git or forge call: a demo
// Lab, an unpublished receipt, a latest plan that is not the next fix round
// on the current head, at most MaxFixRounds rounds, a verification that did
// not pass and a missing Confirm. Every check that does not need the new
// commit comes before the local commit: the origin URLs, the base ancestry,
// the plugin's host, and the change request read through the forge, which
// must be open on the receipt's head, URL and branch, with that branch still
// on the remote at the head. The commit is made in the Molecule workspace
// from the verified tree on the head and checked; a push the remote rejects
// refuses with ErrFixRoundBranchMoved. The change is re-read through the forge
// and its head must be the pushed commit, otherwise nothing is recorded. It
// never merges, marks the change ready, recreates a branch, or reads CI.
func PushFixRound(ctx context.Context, o FixRoundPushOptions) (Receipt, error) {
	abs, err := filepath.Abs(o.LabDir)
	if err != nil {
		return Receipt{}, fmt.Errorf("the Lab directory cannot be resolved")
	}
	o.LabDir = abs
	if err = RefuseDemoLab(o.LabDir); err != nil {
		return Receipt{}, err
	}
	if o.Approve == "" {
		st, err := loadFixRound(ctx, o, false)
		if err != nil {
			return Receipt{}, err
		}
		return Receipt{}, &FixRoundApprovalRequired{Preview: st.preview}
	}
	lock := filepath.Join(o.LabDir, ".delivery-lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		return Receipt{}, fmt.Errorf("delivery/fix round conflict: another delivery operation holds the lock")
	}
	locked := true
	unlock := func() {
		if locked {
			os.Remove(lock)
			locked = false
		}
	}
	defer unlock()
	st, err := loadFixRound(ctx, o, true)
	if err != nil {
		return Receipt{}, err
	}
	if st.preview.Digest != o.Approve {
		return Receipt{}, fmt.Errorf("approval is stale: the fix round push changed; review the new preview")
	}
	if err = fixRoundLocalPreflight(ctx, st); err != nil {
		return Receipt{}, err
	}
	runner := o.Runner
	if runner == nil {
		runner = process.Run
	}
	if o.Changes != nil {
		d, err := o.Changes.Describe(ctx)
		if err != nil {
			return Receipt{}, pluginError("describe the forge", err)
		}
		if d.Host != st.receipt.ForgeHost {
			return Receipt{}, fmt.Errorf("the forge plugin's host differs from the host recorded for this delivery")
		}
	}
	// An interrupted push may already have moved the branch to its own commit,
	// which this push reuses.
	heads := []string{st.head}
	if st.committed != "" {
		heads = append(heads, st.committed)
	}
	if _, reason := readFixRoundChange(ctx, o, runner, st, heads); reason != "" {
		return Receipt{}, fmt.Errorf("the change request cannot take a fix round (%s); nothing was committed or pushed", reason)
	}
	if err = fixRoundRemoteBranch(ctx, runner, st.workspace, st.preview.Branch, heads); err != nil {
		return Receipt{}, err
	}
	pushed, err := commitFixRound(ctx, st)
	if err != nil {
		return Receipt{}, err
	}
	// The full delivery evidence binds the commit before anything leaves the Lab.
	if _, err = validatedHead(ctx, Options{LabDir: o.LabDir, MoleculeID: o.MoleculeID, BaseBranch: st.receipt.Preview.BaseBranch}, pushed); err != nil {
		return Receipt{}, err
	}
	if err = pushFixRoundCommit(ctx, runner, st.workspace, pushed, st.preview.Branch); err != nil {
		return Receipt{}, err
	}
	observed, reason := readFixRoundChange(ctx, o, runner, st, []string{pushed})
	if reason != "" {
		return Receipt{}, fmt.Errorf("the change request re-read after the push does not confirm the fix round (%s); nothing was recorded", reason)
	}
	unlock()
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	fr := FixRound{Round: st.preview.Round, Parent: st.head, Commit: pushed, Tree: st.preview.Tree, PlanDigest: st.preview.PlanDigest, VerificationTree: st.preview.Tree, Findings: append([]string{}, st.preview.Findings...), PushedAt: now().UTC().Format(time.RFC3339Nano), ObservedHead: observed}
	r, err := AppendFixRound(Options{LabDir: o.LabDir, MoleculeID: o.MoleculeID}, fr)
	if err != nil {
		return r, err
	}
	if o.Changes == nil {
		return r, nil
	}
	return recordFixRoundCalls(o, r, pushed)
}

// fixRoundLocalPreflight checks, with local git only and before any commit,
// what the delivery evidence check after the commit checks without needing
// the new commit: every origin and push URL is the intake's repository or
// forge identity, and the recorded base is an ancestor of the change
// request's base branch.
func fixRoundLocalPreflight(ctx context.Context, st fixRoundState) error {
	w := st.record.Workspace
	identity, err := snapshotForgeIdentity(st.record)
	if err != nil {
		return err
	}
	for _, args := range [][]string{{"remote", "get-url", "--all", "origin"}, {"remote", "get-url", "--push", "--all", "origin"}} {
		urls, err := git(ctx, st.workspace, nil, args...)
		if err != nil {
			return fmt.Errorf("the workspace's Git origin cannot be read")
		}
		for _, url := range strings.Split(strings.TrimSpace(urls), "\n") {
			if identity != "" {
				if !molecule.OriginHasIdentity(url, identity) {
					return fmt.Errorf("Git origin or push destination differs from intake")
				}
				continue
			}
			m := origin.FindStringSubmatch(url)
			if m == nil || !strings.EqualFold(strings.TrimSuffix(strings.ToLower(m[1]), ".git"), w.Repo) {
				return fmt.Errorf("Git origin or push destination differs from intake")
			}
		}
	}
	base := st.receipt.Preview.BaseBranch
	if base == "" || strings.HasPrefix(base, "-") || base == w.Branch {
		return fmt.Errorf("invalid PR base branch")
	}
	if _, err = git(ctx, st.workspace, nil, "check-ref-format", "refs/heads/"+base); err != nil {
		return fmt.Errorf("invalid PR base branch")
	}
	return validateBase(ctx, st.workspace, w.BaseCommit, base)
}

// validFixRoundBranch reports a branch a fix round may name in a git argv.
func validFixRoundBranch(branch string) bool {
	return branch != "" && !strings.HasPrefix(branch, "-") && !strings.HasPrefix(branch, "+") && !strings.ContainsAny(branch, " \t\n\x00:")
}

// fixRoundRemoteBranch requires the change request's branch to exist on
// origin at one of heads: a deleted branch is never recreated, and a moved
// one is ErrFixRoundBranchMoved. It reads the remote and writes nothing.
func fixRoundRemoteBranch(ctx context.Context, runner func(context.Context, process.Options) process.Result, dir, branch string, heads []string) error {
	if !validFixRoundBranch(branch) {
		return fmt.Errorf("invalid fix round push target")
	}
	ref := "refs/heads/" + branch
	p := runner(ctx, process.Options{Argv: []string{"git", "ls-remote", "origin", ref}, Dir: dir, Stdin: []byte{}, Timeout: 60 * time.Second, MaxOutputBytes: fixRoundRemoteOutputBytes})
	if !p.OK || p.TimedOut || p.Interrupted || p.Overflow {
		return fmt.Errorf("the change request's branch could not be read on the remote; nothing was committed or pushed")
	}
	found := ""
	for _, line := range strings.Split(p.Stdout, "\n") {
		commit, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || name != ref {
			continue
		}
		if found != "" {
			return fmt.Errorf("the remote reports the change request's branch more than once; nothing was committed or pushed")
		}
		found = commit
	}
	if found == "" {
		return fmt.Errorf("the change request's branch no longer exists on the remote, and a fix round never recreates it; nothing was committed or pushed")
	}
	for _, h := range heads {
		if found == h {
			return nil
		}
	}
	return ErrFixRoundBranchMoved
}

// commitFixRound commits the verified tree on the head in the workspace, or
// reuses an interrupted push's identical commit, and checks it.
func commitFixRound(ctx context.Context, st fixRoundState) (string, error) {
	p := st.preview
	if st.committed != "" {
		return st.committed, nil
	}
	if _, err := git(ctx, st.workspace, nil, "read-tree", p.Tree); err != nil {
		return "", fmt.Errorf("the verified tree could not be staged; no commit was made")
	}
	tree, err := git(ctx, st.workspace, nil, "write-tree")
	if err != nil || strings.TrimSpace(tree) != p.Tree {
		return "", fmt.Errorf("content changed during staging; no commit was made")
	}
	if _, err = git(ctx, st.workspace, nil, "commit", "-m", p.Title); err != nil {
		return "", fmt.Errorf("the fix-round commit failed")
	}
	raw, err := git(ctx, st.workspace, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("the fix-round commit cannot be read")
	}
	commit := strings.TrimSpace(raw)
	// Detects a commit hook's change and a commit made on another parent.
	if err = verifyFixRoundCommit(ctx, st.workspace, commit, p.Parent, p.Tree, p.Title); err != nil {
		return "", fmt.Errorf("%w; no push was performed", err)
	}
	return commit, nil
}

// pushFixRoundCommit pushes commit to the branch, never by force. A rejected
// push is ErrFixRoundBranchMoved; any other failure is uncertain and recorded
// nowhere, so a retry reuses the commit.
func pushFixRoundCommit(ctx context.Context, runner func(context.Context, process.Options) process.Result, dir, commit, branch string) error {
	if !fixRoundSHA.MatchString(commit) || !validFixRoundBranch(branch) {
		return fmt.Errorf("invalid fix round push target")
	}
	argv := []string{"git", "push", "origin", commit + ":refs/heads/" + branch}
	p := runner(ctx, process.Options{Argv: argv, Dir: dir, Stdin: []byte{}, Timeout: 60 * time.Second, MaxOutputBytes: fixRoundRemoteOutputBytes})
	if p.OK && !p.TimedOut && !p.Interrupted && !p.Overflow {
		return nil
	}
	if !p.TimedOut && !p.Interrupted && !p.Overflow {
		for _, rejected := range []string{"non-fast-forward", "[rejected]", "fetch first", "stale info"} {
			if strings.Contains(p.Stderr, rejected) || strings.Contains(p.Stdout, rejected) {
				return ErrFixRoundBranchMoved
			}
		}
	}
	return fmt.Errorf("git push of the fix round failed or its outcome is uncertain; nothing was recorded, and a retry reuses the commit")
}

// readFixRoundChange reads the change request through its forge: it must be
// the receipt's open change on the same repository, URL and branch, with one
// of heads as its head. It returns the observed head, or the reason it is not.
func readFixRoundChange(ctx context.Context, o FixRoundPushOptions, runner func(context.Context, process.Options) process.Result, st fixRoundState, heads []string) (string, string) {
	r := st.receipt
	expected := func(head string) bool {
		for _, h := range heads {
			if head == h {
				return true
			}
		}
		return false
	}
	if o.Changes != nil {
		noun := changeNoun(r.ChangeNoun)
		c, err := o.Changes.Change(ctx, r.Ref)
		if err != nil {
			return "", pluginError("read the "+noun, err).Error()
		}
		if c.URL != r.URL || !validPluginURL(r.ForgeHost, c.URL) {
			return "", "target project or " + noun + " URL mismatch"
		}
		if state := pluginChangeState(c.State); state != "OPEN" {
			return "", noun + " is " + state
		}
		if !expected(c.HeadCommit) {
			return "", "head commit missing or moved"
		}
		return c.HeadCommit, ""
	}
	bounded := func(ctx context.Context, p process.Options) process.Result {
		if p.MaxOutputBytes == 0 || p.MaxOutputBytes > fixRoundRemoteOutputBytes {
			p.MaxOutputBytes = fixRoundRemoteOutputBytes
		}
		return runner(ctx, p)
	}
	raw, err := forge.GitHub{Run: bounded}.Change(ctx, r.Preview.Repo, r.URL, st.workspace)
	if err != nil {
		return "", "the pull request could not be read"
	}
	if strings.TrimSpace(raw) == "null" {
		return "", "PR not found"
	}
	var pr remotePR
	if json.Unmarshal([]byte(raw), &pr) != nil {
		return "", "malformed PR observation"
	}
	probe := r
	probe.Commit = pr.Head
	if reason := pr.mismatch(probe); reason != "" {
		return "", reason
	}
	if pr.State != "OPEN" {
		return "", "PR is " + pr.observedState()
	}
	if !expected(pr.Head) {
		return "", "head commit missing or moved"
	}
	return pr.Head, ""
}

// recordFixRoundCalls keeps the plugin forge's calls of the push on the
// receipt, under the delivery lock, once its fix round is recorded.
func recordFixRoundCalls(o FixRoundPushOptions, recorded Receipt, pushed string) (Receipt, error) {
	lock := filepath.Join(o.LabDir, ".delivery-lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return recorded, fmt.Errorf("fix round %d is recorded, but its plugin calls were not: another delivery operation holds the lock", len(recorded.FixRounds))
	}
	defer os.Remove(lock)
	r, err := ReadFixRoundReceipt(o.LabDir, o.MoleculeID)
	if err != nil {
		return recorded, err
	}
	if head, _ := ReceiptHead(r); head != pushed {
		return recorded, fmt.Errorf("the receipt's head moved after the fix round was recorded; its plugin calls were not recorded")
	}
	if !absorbReceiptCalls(&r, o.Plugin.Calls) {
		return r, nil
	}
	return r, writeReceipt(Options{LabDir: o.LabDir, MoleculeID: o.MoleculeID}, r)
}
