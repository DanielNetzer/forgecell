package ui

// Lanes are the UI's Molecule filters. Each Molecule has exactly one, chosen in
// this order (operator decisions, 2026-10-24 and 2026-10-26):
//
//  1. blocked: ledger status blocked or failed, even when a receipt exists;
//  2. decision: readiness phase scope-waiting, or review-waiting without a
//     published receipt;
//  3. running: status running or allocating, or an active readiness phase;
//  4. finished: a published delivery receipt whose lifecycle is not unresolved.
//
// A published draft outranks review-waiting (2026-10-26): forgecell deliver
// publishes without moving the ledger phase, so a delivered Molecule is finished,
// not waiting for a decision. Anything else is other: it appears under All only.
// There is no paused lane, because the engine has no pause semantics.
const (
	LaneBlocked  = "blocked"
	LaneDecision = "decision"
	LaneRunning  = "running"
	LaneFinished = "finished"
	LaneOther    = "other"
)

// Lanes lists every lane in filter-bar order; their counts always sum to All.
var Lanes = []string{LaneRunning, LaneDecision, LaneBlocked, LaneFinished, LaneOther}

// activePhases are readiness phases in which the Lab is working: the scope is
// approved and coding or checks have started or are about to.
var activePhases = map[string]bool{"approved": true, "coding": true, "check-pending": true}

// ActivityQueued is the activity of a running-lane Molecule that waits for the
// Lab's verification slot: it stays in the running lane but runs no checks yet.
const ActivityQueued = "queued for checks"

// Activity labels what a Molecule in lane is doing when that is not plain
// running: ActivityQueued for one queued for checks, and "" otherwise, so an
// unknown phase or event never produces a label.
func Activity(lane string, queued bool) string {
	if lane == LaneRunning && queued {
		return ActivityQueued
	}
	return ""
}

// DeliveryState is the part of a delivery receipt that the lane depends on.
type DeliveryState struct {
	State     string
	Lifecycle string
}

// Published reports a receipt whose state is published and whose remote lifecycle
// is not unresolved. A receipt still prepared or committed, or one observed as
// CLOSED, UNKNOWN, MISSING, AMBIGUOUS or any unrecognised lifecycle, is delivery in
// progress, never published.
func (d *DeliveryState) Published() bool {
	if d == nil || d.State != "published" {
		return false
	}
	switch d.Lifecycle {
	case "", "OPEN", "MERGED":
		return true
	}
	return false
}

// Lane maps a ledger status, readiness phase and optional receipt to one lane.
func Lane(status, phase string, delivery *DeliveryState) string {
	switch {
	case status == "blocked" || status == "failed":
		return LaneBlocked
	case phase == "scope-waiting" || phase == "review-waiting" && !delivery.Published():
		return LaneDecision
	case status == "running" || status == "allocating" || activePhases[phase]:
		return LaneRunning
	case delivery.Published():
		return LaneFinished
	}
	return LaneOther
}

// DeliveryLabel is the selected Molecule's delivery line: "Draft published" for
// a published receipt, never "merged"; "Delivery in progress" with the exact
// receipt state otherwise; empty when there is no receipt.
func DeliveryLabel(d *DeliveryState) string {
	switch {
	case d == nil:
		return ""
	case d.Published():
		return "Draft published"
	case d.Lifecycle != "":
		return "Delivery in progress · receipt " + d.State + " · lifecycle " + d.Lifecycle
	}
	return "Delivery in progress · receipt " + d.State
}
