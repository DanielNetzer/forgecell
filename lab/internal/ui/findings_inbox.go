package ui

import (
	"fmt"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// The findings Inbox overlay, and the ledger mirror of the engine's fix-round
// push gates that it, the findings view and the push route read.

// fixRoundPendingOf is the latest plan when it is the receipt's next fix round
// on its current head, not yet recorded on the receipt.
func fixRoundPendingOf(r molecule.Record, receipt delivery.Receipt) (readiness.Proposal, bool) {
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		return readiness.Proposal{}, false
	}
	latest := r.Readiness.Plans[len(r.Readiness.Plans)-1]
	ref := latest.Plan.FixRound
	head, tree := delivery.ReceiptHead(receipt)
	if ref == nil || ref.Round != len(receipt.FixRounds)+1 || ref.Head != head || ref.HeadTree != tree {
		return readiness.Proposal{}, false
	}
	return latest, true
}

// fixRoundLedgerVerified is the ledger half of the engine's push refusals: a
// passing verification of the plan digest that waits at the review gate. It
// reads no workspace; the push preview does that.
func fixRoundLedgerVerified(r molecule.Record, digest string) bool {
	v := r.Verification
	if v == nil || !v.RequiredChecksPassed || v.Error != "" || !fixRoundObject.MatchString(v.SourceTree) || r.Readiness == nil || r.Readiness.Phase != "review-waiting" {
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

// fixRoundLedgerNeedsConfirm is the preview's NeedsConfirm from the ledger
// alone: independent acceptance unverified, or no green review Atom pass of
// the plan on the verified tree.
func fixRoundLedgerNeedsConfirm(r molecule.Record, digest string) bool {
	if r.Verification == nil || !r.Verification.IndependentAcceptanceVerified {
		return true
	}
	atom := false
	for _, a := range r.Atoms {
		if a.Type == "review" {
			atom = true
		}
	}
	if !atom {
		return true
	}
	for i := len(r.ReviewAttempts) - 1; i >= 0; i-- {
		if a := r.ReviewAttempts[i]; a.PlanDigest == digest {
			return a.Status != molecule.ReviewNoHighFindings || a.Tree != r.Verification.SourceTree
		}
	}
	return true
}

// fixRoundLedgerState is a pending fix round's state from the ledger.
func fixRoundLedgerState(r molecule.Record, digest string) string {
	switch {
	case r.Status == "blocked" || r.Status == "failed":
		return fixRoundBlocked
	case phase(r) == "scope-waiting":
		return fixRoundWaitingApproval
	case fixRoundLedgerVerified(r, digest):
		return fixRoundVerified
	}
	return fixRoundRunning
}

// findingsLive reports a published receipt whose change request is not
// observed merged or closed.
func findingsLive(receipt delivery.Receipt) bool {
	return receipt.Lifecycle == nil || receipt.Lifecycle.State == "" || receipt.Lifecycle.State == "OPEN"
}

// inboxFindingsOverlay sets a published Molecule's Inbox entry to one of the
// two findings decisions: a verified fix round that waits for an explicit push
// confirmation, else open reviewer findings to triage when none of them is
// pending in a fix round. It replaces only an In-progress, Recently recorded
// or empty entry, never a scope, publish or blocked card, and reads no
// workspace and starts no process.
func inboxFindingsOverlay(lab string, r molecule.Record, v *InboxView) {
	if v == nil || r.Status == molecule.StatusRetired || (v.Kind != "" && v.Kind != InboxProgress && v.Kind != InboxRecorded) {
		return
	}
	f, ok := findingsSnapshot(r)
	if !ok {
		return
	}
	receipt, err := delivery.ReadFixRoundReceipt(lab, r.ID)
	if err != nil || !findingsLive(receipt) {
		return
	}
	set := func(kind, title, summary, action string) {
		*v = InboxView{Kind: kind, Title: title, Summary: summary, Action: action, Step: v.Step, StepLabel: v.StepLabel, StepSince: v.StepSince, StepNote: v.StepNote}
	}
	pending, isPending := fixRoundPendingOf(r, receipt)
	if isPending && fixRoundLedgerState(r, pending.Digest) == fixRoundVerified && fixRoundLedgerNeedsConfirm(r, pending.Digest) {
		set(InboxFixRound, "Fix round ready to push", fmt.Sprintf("Fix round %d is verified and waits for your push confirmation.", pending.Plan.FixRound.Round), "Review & push")
		return
	}
	record, err := delivery.ReadFindings(lab, r.ID)
	if err != nil {
		return
	}
	head, _ := delivery.ReceiptHead(receipt)
	summary := delivery.Summarize(record, findingsAutoFix(f))
	if summary.Round < 0 || summary.Commit != head {
		return
	}
	inFixRound := map[string]bool{}
	if isPending {
		for _, fr := range pending.Plan.FixRound.Findings {
			inFixRound[fr.Fingerprint] = true
		}
	}
	open := 0
	for _, g := range summary.Open {
		for _, sf := range g.Findings {
			if inFixRound[sf.Fingerprint] {
				return
			}
			open++
		}
	}
	if open == 0 {
		return
	}
	set(InboxFindings, "Reviewer findings to triage", findingsCounted(count(open), "open reviewer finding", "open reviewer findings")+" on the draft.", "Review findings")
}

// findingsCounted is n with its singular or plural noun.
func findingsCounted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
