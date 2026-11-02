package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/fixround"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// maxFixRoundIncludes bounds the --include fingerprints of one proposal.
const maxFixRoundIncludes = fixround.MaxIncludes

var findingFingerprint = fixround.Fingerprint

// Seams of the fix-round commands; tests replace them.
var (
	fixRoundReadRecord = molecule.ReadRecord
	fixRoundAmend      = amendFixRoundThroughIssue
	fixRoundPreview    = delivery.PreviewFixRound
	fixRoundPush       = pushFixRoundThroughForge
)

// runFindingsPropose proposes the next fix round of a published delivery from
// the latest complete findings round on its head: the open findings at or
// above each reviewer's autoFix level, and those named by include. A finding
// an earlier fix round addressed that is still open is not proposed again
// unless included, and a dismissed one is never proposed. The selection is
// fixround.Propose's, reading and amending the ledger through this command's
// seams; the skipped findings are printed before any refusal. Findings are
// untrusted evidence: their text reaches the revision only flattened and
// bounded, and nothing here approves or pushes.
func runFindingsPropose(ctx context.Context, lab, labArg, id string, f formula.Formula, includes []string, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	p, err := fixround.Propose(ctx, lab, id, includes, fixround.Options{LookupEnv: pluginLookupEnv, Formula: &f, ReadRecord: fixRoundReadRecord, Amend: fixRoundAmend})
	for _, skip := range p.Skipped {
		if skip.Reason == fixround.SkipDismissed {
			fmt.Fprintf(out, "finding %s is dismissed; it is not proposed\n", skip.Fingerprint)
			continue
		}
		fmt.Fprintf(out, "finding %s came back after a fix; it is not proposed again\n", skip.Fingerprint)
	}
	if err != nil {
		return fail(err)
	}
	if p.None {
		fmt.Fprintln(out, "no findings to propose")
		return 0
	}
	presentFixRoundProposal(out, labArg, p.Record, p.Round, len(p.Picked))
	return 0
}

// presentFixRoundProposal prints the revision summary and the exact command
// that approves it, in the keyed form for a keyed Molecule.
func presentFixRoundProposal(out io.Writer, labArg string, r molecule.Record, round, findings int) {
	plans := r.Readiness.Plans
	latest := plans[len(plans)-1]
	p := latest.Plan
	summary := "no change"
	if len(plans) > 1 {
		summary = molecule.AmendDiffSummary(plans[len(plans)-2].Plan, p)
	}
	fmt.Fprintf(out, "Fix round %d proposed for %s: %d finding(s); revision %d: %s\n", round, r.ID, findings, p.Revision, summary)
	issueArg := fmt.Sprint(r.Issue.Number)
	if r.Issue.IsKeyed() {
		if intake.ValidateIssueKey(r.Issue.Key) != nil {
			fmt.Fprintln(out, "This Molecule's Issue key is not a valid tracker key, so no approve command is shown.")
			return
		}
		issueArg = shellWord(r.Issue.Key)
	}
	fmt.Fprintf(out, "Review the full ledger, then approve exactly this plan:\nforgecell run %s --lab %s --formula %s --base %s --target %s --approve %s\n", issueArg, shellWord(labArg), shellWord(r.FormulaID), shellWord(p.Inputs.BaseCommit), shellWord(p.Inputs.TargetBranch), latest.Digest)
}

// amendFixRoundThroughIssue records the fix round, re-reading the Issue through
// the Molecule's own reader: the tracker plugin of its approved Formula for a
// keyed Molecule, else GitHub.
func amendFixRoundThroughIssue(ctx context.Context, lab string, r molecule.Record, req molecule.FixRoundRequest) (molecule.Record, error) {
	return fixround.AmendThroughIssue(ctx, lab, r, req, pluginLookupEnv)
}

// pushFixRoundThroughForge pushes through the Molecule's forge, chosen as
// deliver chooses it: its Formula snapshot's forge plugin, else GitHub. A
// preview without an approval starts no plugin.
func pushFixRoundThroughForge(ctx context.Context, o delivery.FixRoundPushOptions) (delivery.Receipt, error) {
	return fixround.PushWith(ctx, o, fixround.Options{LookupEnv: pluginLookupEnv})
}

// runFindingsPush is findings --push: without an approval it prints the
// preview and the command that approves it; with one it pushes exactly that.
func runFindingsPush(ctx context.Context, lab, id, approve, title string, confirm bool, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	r, err := fixRoundPush(ctx, delivery.FixRoundPushOptions{LabDir: lab, MoleculeID: id, Approve: approve, Title: title, Confirm: confirm})
	var pending *delivery.FixRoundApprovalRequired
	if errors.As(err, &pending) {
		if code := encode(out, stderr, pending.Preview); code != 0 {
			return code
		}
		fmt.Fprintln(stderr, fixRoundPushCommand(id, title, pending.Preview))
		return 0
	}
	if err != nil {
		return fail(err)
	}
	fmt.Fprintln(out, fixRoundPushedLine(r))
	return 0
}

// fixRoundPushCommand is the exact command that pushes the previewed round.
func fixRoundPushCommand(id, title string, p delivery.FixRoundPreview) string {
	line := fmt.Sprintf("Review this fix round %d push, then push it with forgecell findings %s --push --approve %s", p.Round, id, p.Digest)
	if title != "" {
		line += " --title " + shellWord(flatten(title))
	}
	if p.NeedsConfirm() {
		line += " --confirm"
	}
	return line
}

// fixRoundPushedLine reports the recorded fix round.
func fixRoundPushedLine(r delivery.Receipt) string {
	if len(r.FixRounds) == 0 {
		return "No fix round was recorded."
	}
	fr := r.FixRounds[len(r.FixRounds)-1]
	return fmt.Sprintf("Fix round %d pushed to %s: commit %s is the change request's head. Nothing was merged or marked ready.", fr.Round, flatten(r.Preview.Branch), fr.Commit)
}

// fixRoundAfterRun is the run-completion hook of a fix-round plan. When the
// run's latest plan carries a fix round whose checks passed, it pushes the
// round automatically if the review is green and acceptance is verified,
// bound to the preview digest it just computed; otherwise it prints how to
// push it. A plan without a fix round prints nothing. It returns a non-zero
// exit code when the round could not be pushed.
func fixRoundAfterRun(ctx context.Context, out, stderr io.Writer, lab string, r molecule.Record) int {
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		return 0
	}
	ref := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan.FixRound
	if ref == nil || r.Readiness.Phase != "review-waiting" || r.Verification == nil || !r.Verification.RequiredChecksPassed {
		return 0
	}
	if !safeID.MatchString(r.ID) {
		return 0
	}
	o := delivery.FixRoundPushOptions{LabDir: lab, MoleculeID: r.ID}
	p, err := fixRoundPreview(ctx, o)
	if err != nil {
		fmt.Fprintf(stderr, "forgecell: fix round %d was not pushed: %v\n", ref.Round, err)
		return 1
	}
	if p.NeedsConfirm() {
		fmt.Fprintf(out, "fix round %d is ready; push it with forgecell findings %s --push --approve %s --confirm\n", p.Round, r.ID, p.Digest)
		return 0
	}
	// The scope approval of this exact revision is the push approval.
	o.Approve = p.Digest
	receipt, err := fixRoundPush(ctx, o)
	if err != nil {
		fmt.Fprintf(stderr, "forgecell: fix round %d was not pushed: %v\n", p.Round, err)
		return 1
	}
	fmt.Fprintln(out, fixRoundPushedLine(receipt))
	return 0
}
