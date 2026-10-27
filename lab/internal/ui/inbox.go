package ui

import (
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// Inbox kinds. Each listed Molecule is one Inbox entry at most: a Needs-you card
// (scope, publish, blocked), an In-progress row (progress) or a Recently recorded
// link (recorded). Other and retired Molecules have none.
const (
	InboxScope    = "scope"
	InboxPublish  = "publish"
	InboxBlocked  = "blocked"
	InboxProgress = "progress"
	InboxRecorded = "recorded"
)

// The seven progress steps, in order. Steps before Step are complete; Step is the
// current one.
const (
	StepPlan = iota
	StepApproval
	StepCoding
	StepChecks
	StepReview
	StepDraft
	StepMerged
)

// StepLabels are the plain-words names of the progress steps.
var StepLabels = []string{"Plan", "Your approval", "Coding", "Checks", "Review", "Draft PR", "Merged"}

// maxInboxSummaryRunes bounds the plan's one-line summary on a scope card.
const maxInboxSummaryRunes = 160

// maxInboxCount bounds every count the Inbox serves; a larger recorded list is
// shown at the bound rather than read further.
const maxInboxCount = 9999

// InboxView is a Molecule's Inbox entry on GET /api/molecules: plain words and
// bounded counts derived from the ledger and the receipt, so the browser never
// maps phase codes, ids or digests to text. It holds plain values only, so a
// Summary stays comparable. Each group of counts is served only with its flag:
// Planned for Files and Checks, Verified for Passed of Ran, Captured for
// Changed; without the flag it was not recorded, which is never zero.
// StepSince is the current attempt's start as RFC 3339 UTC, for the browser's
// elapsed time only; it is data, never shown.
type InboxView struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	Action  string `json:"action"`
	// Files and Checks are the latest plan's scoped files and checks; Passed of
	// Ran are its recorded check results; Changed is the captured change's files.
	Planned   bool   `json:"planned,omitempty"`
	Files     int    `json:"files,omitempty"`
	Checks    int    `json:"checks,omitempty"`
	Verified  bool   `json:"verified,omitempty"`
	Passed    int    `json:"passed,omitempty"`
	Ran       int    `json:"ran,omitempty"`
	Captured  bool   `json:"captured,omitempty"`
	Changed   int    `json:"changed,omitempty"`
	Step      int    `json:"step"`
	StepLabel string `json:"stepLabel"`
	StepSince string `json:"stepSince,omitempty"`
	// StepNote is a fixed rerun note of the current attempt, "rerun" or "rerun
	// after a plan revision", or empty.
	StepNote string `json:"stepNote,omitempty"`
	Queued   bool   `json:"queued,omitempty"`
	Merged   bool   `json:"merged,omitempty"`
}

// inboxView maps one Molecule's lane, phase, Atoms and receipt to its Inbox
// entry, or the zero InboxView for a lane the Inbox does not list.
func inboxView(x redactor, r molecule.Record, lane, activity string, d *DeliveryState) InboxView {
	// A published receipt is recorded, whatever the lane: a Molecule still running
	// after its draft PR is never an In-progress row titled Draft published.
	if lane == LaneRunning && d.Published() {
		lane = LaneFinished
	}
	step, label, since, note := stepProgress(r, lane, activity, d)
	v := InboxView{Step: step, StepLabel: label, StepSince: since, StepNote: note}
	switch lane {
	case LaneDecision:
		if phase(r) == "scope-waiting" {
			v.Kind, v.Title, v.Action = InboxScope, "Plan ready for your approval", "Review plan"
			if r.Readiness != nil && len(r.Readiness.Plans) > 0 {
				a := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan.Analysis
				v.Summary = inboxSummary(x, a.Summary)
				v.Planned, v.Files, v.Checks = true, count(len(a.Scope)), count(len(a.Checks))
			}
		} else {
			v.Kind, v.Title, v.Action = InboxPublish, "Ready to publish", "Review & publish"
			if vr := r.Verification; vr != nil {
				passed := 0
				for _, c := range vr.Checks {
					if c.Result.OK {
						passed++
					}
				}
				v.Verified, v.Passed, v.Ran = true, count(passed), count(len(vr.Checks))
			}
			if r.Capture != nil {
				v.Captured, v.Changed = true, count(len(r.Capture.Changes))
			}
		}
	case LaneBlocked:
		v.Kind, v.Title, v.Action = InboxBlocked, "Blocked", "Open"
		v.Summary = blockedReason(r)
	case LaneRunning:
		v.Kind, v.Title, v.Action = InboxProgress, label, "Watch"
		v.Queued = activity == ActivityQueued
	case LaneFinished:
		v.Kind, v.Title, v.Action = InboxRecorded, "Draft published", "Open"
		if d != nil && d.Lifecycle == "MERGED" {
			v.Title, v.Merged = "Merged", true
		}
	default:
		return InboxView{}
	}
	return v
}

func count(n int) int { return min(n, maxInboxCount) }

var (
	// longHex is a digest-like run of hex digits; the Inbox shows its short form.
	longHex = regexp.MustCompile(`\b[0-9a-fA-F]{40,}\b`)
	// isoTime is an ISO 8601 timestamp; the Inbox keeps only its date.
	isoTime = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?`)
	// rawID is a Molecule id (mol-<number>-<hex>) or an assay id (assay-<hex>),
	// which the Inbox never shows. Only real ids match, so ordinary words such as
	// "assay-based" or "mol-file" are kept.
	rawID = regexp.MustCompile(`\b(?:mol-(?:\d+-)?|assay-)[0-9a-f]{6,}\b`)
)

// inboxSummary is untrusted plan text made one bounded line: redacted, every
// control and line break removed, digests shortened, timestamps cut to their
// date, ids replaced, and cut with an ellipsis.
func inboxSummary(x redactor, s string) string {
	s = harness.ReviewDisplayText(redactSecrets(x.text(s)), 4*maxInboxSummaryRunes)
	s = rawID.ReplaceAllStringFunc(s, func(id string) string {
		if id[0] == 'a' {
			return "an assay"
		}
		return "a Molecule"
	})
	s = longHex.ReplaceAllStringFunc(s, func(h string) string { return h[:7] + "…" })
	s = isoTime.ReplaceAllString(s, "$1")
	if utf8.RuneCountInString(s) > maxInboxSummaryRunes {
		s = string([]rune(s)[:maxInboxSummaryRunes-1]) + "…"
	}
	return s
}

// blockedReason is the blocking reason in fixed plain words: the login hint for
// an unauthenticated harness (#244), otherwise the step whose Atom failed. Ledger
// text never reaches it.
func blockedReason(r molecule.Record) string {
	if hint, ok := molecule.NotAuthenticated(r); ok {
		return harness.ReviewDisplayText(hint, maxReasonRunes)
	}
	coded := false
	for _, a := range r.Atoms {
		s := atomStep(a.Type, coded)
		coded = coded || a.Type == "harness"
		if a.Status != "failed" && a.Status != "blocked" {
			continue
		}
		switch s {
		case StepPlan:
			return "Planning stopped before a plan was ready."
		case StepApproval:
			return "The plan approval did not go through."
		case StepCoding:
			return "Coding stopped before the change was finished."
		case StepChecks:
			return "Required checks did not pass."
		case StepReview:
			return "Review found a problem that blocks this change."
		case StepDraft:
			return "The draft PR could not be published."
		}
	}
	if r.Status == "failed" {
		return "This Molecule failed before it finished."
	}
	return "This Molecule stopped before it finished."
}

// atomStep maps an Atom type to its progress step: a gate before the harness Atom
// is your approval of the plan, one after it is the review gate. Documentation and
// learning Atoms, and unknown types, have no step of their own (-1).
func atomStep(typ string, coded bool) int {
	switch typ {
	case "intake":
		return StepPlan
	case "gate":
		if coded {
			return StepReview
		}
		return StepApproval
	case "harness":
		return StepCoding
	case "check":
		return StepChecks
	case "review":
		return StepReview
	case "ship":
		return StepDraft
	}
	return -1
}

// phaseSteps is the fallback step of a readiness phase when no Atom is current.
var phaseSteps = map[string]int{"scope-waiting": StepApproval, "approved": StepCoding, "coding": StepCoding, "check-pending": StepChecks, "review-waiting": StepReview}

// Rerun notes of the current attempt.
const (
	rerunNote         = "rerun"
	rerunRevisionNote = "rerun after a plan revision"
)

// progress is a Molecule's current step, its plain label and when the current
// attempt of that step started; stepProgress also gives the rerun note.
func progress(r molecule.Record, lane, activity string, d *DeliveryState) (int, string, string) {
	step, label, since, _ := stepProgress(r, lane, activity, d)
	return step, label, since
}

// stepProgress is a Molecule's current step, its plain label, when the current
// attempt of that step started, and a rerun note. A published receipt is the
// Draft PR step done (Merged when the receipt observed a merge); otherwise the
// first active or waiting Atom decides, then a failed one, then the readiness
// phase.
func stepProgress(r molecule.Record, lane, activity string, d *DeliveryState) (int, string, string, string) {
	if d.Published() {
		if d.Lifecycle == "MERGED" {
			return StepMerged, "Merged", "", ""
		}
		return StepMerged, "Draft published", "", ""
	}
	if activity == ActivityQueued {
		return StepChecks, "Queued for checks", "", ""
	}
	coded, failedAt := false, -1
	for _, a := range r.Atoms {
		s := atomStep(a.Type, coded)
		coded = coded || a.Type == "harness"
		switch a.Status {
		case "active", "running", "waiting":
			if s < 0 {
				s = StepDraft
			}
			since, note := attemptSince(r, a)
			return s, StepLabels[s], since, note
		case "failed", "blocked":
			if failedAt < 0 && s >= 0 {
				failedAt = s
			}
		}
	}
	if failedAt >= 0 {
		return failedAt, StepLabels[failedAt], "", ""
	}
	if s, ok := phaseSteps[phase(r)]; ok {
		return s, StepLabels[s], "", ""
	}
	return StepPlan, StepLabels[StepPlan], "", ""
}

// attemptSince is when the current attempt of a current Atom started, and its
// rerun note. Coding counts from the latest harness attempt's start recorded on
// the Atom, Checks from the latest verification attempt and Review from the
// latest review pass, never from the Atom's first start, which an earlier
// attempt set. A step with attempts but no recorded start of the latest one
// shows no duration; a step with no attempt recorded yet, and other Atoms,
// which run once, count from the Atom's own start.
func attemptSince(r molecule.Record, a molecule.Atom) (string, string) {
	var starts, digests []string
	switch a.Type {
	case "harness":
		recorded := map[string]string{}
		if a.Provenance != nil {
			for _, act := range a.Provenance.Actions {
				recorded[act.AttemptID] = act.StartedAt
			}
		}
		for _, h := range r.HarnessAttempts {
			starts, digests = append(starts, recorded[h.ID]), append(digests, h.PlanDigest)
		}
	case "check":
		for _, v := range r.VerificationAttempts {
			starts, digests = append(starts, v.StartedAt), append(digests, v.PlanDigest)
		}
	case "review":
		for _, v := range r.ReviewAttempts {
			starts, digests = append(starts, v.StartedAt), append(digests, v.PlanDigest)
		}
	default:
		return stepSince(a.StartedAt), ""
	}
	n := len(starts)
	if n == 0 {
		return stepSince(a.StartedAt), ""
	}
	note := ""
	if n > 1 {
		note = rerunNote
		if digests[n-1] != digests[n-2] {
			note = rerunRevisionNote
		}
	}
	return stepSince(starts[n-1]), note
}

// stepSince re-formats a recorded start time, or returns "" for anything that
// is not a valid RFC 3339 time, so ledger text never passes through.
func stepSince(at string) string {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
