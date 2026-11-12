package ui

import (
	"context"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// The served view of GET /api/molecules/{id}/findings (findings.go) and the
// builders that fill it from the ledger, the receipt and the findings record.

// ReviewerFindingsView is GET /api/molecules/{id}/findings. A Molecule whose Formula
// declares no reviewers serves {"reviewers": false} alone.
type ReviewerFindingsView struct {
	Reviewers bool `json:"reviewers"`
	// Plugins names the snapshot's reviewer plugins in declaration order, for
	// display; the reviewer fields below keep the reviewer keys.
	Plugins []string `json:"plugins,omitempty"`
	// Published is set when the receipt is a published delivery; Head is then
	// its change request's current head. Lifecycle is "merged" or "closed" once
	// the receipt observed its change request so; a proposal is then refused.
	Published bool   `json:"published,omitempty"`
	Head      string `json:"head,omitempty"`
	Lifecycle string `json:"lifecycle,omitempty"`
	// Unreadable is set, with a fixed Error, when the findings record could not
	// be read; nothing else of it is served.
	Unreadable bool                 `json:"unreadable,omitempty"`
	Error      string               `json:"error,omitempty"`
	Rounds     []FindingsRoundView  `json:"rounds,omitempty"`
	Round      int                  `json:"round,omitempty"`
	Commit     string               `json:"commit,omitempty"`
	OnHead     bool                 `json:"onHead,omitempty"`
	Open       []FindingsOpenView   `json:"open,omitempty"`
	OpenCount  int                  `json:"openCount,omitempty"`
	Truncated  bool                 `json:"truncated,omitempty"`
	Resolved   int                  `json:"resolved,omitempty"`
	Dismissed  int                  `json:"dismissed,omitempty"`
	Dismissals []FindingsDismissal  `json:"dismissals,omitempty"`
	FixRounds  []FindingsFixRound   `json:"fixRounds,omitempty"`
	Pending    *FindingsPendingView `json:"pending,omitempty"`
	// Collecting is set while a collection runs, from this page or the
	// background pass.
	Collecting bool             `json:"collecting,omitempty"`
	Collect    *FindingsJobView `json:"collect,omitempty"`
	Push       *FindingsJobView `json:"push,omitempty"`
}

// FindingsRoundView is one reviewer's latest round.
type FindingsRoundView struct {
	Reviewer    string `json:"reviewer"`
	Plugin      string `json:"plugin"`
	Commit      string `json:"commit"`
	CollectedAt string `json:"collectedAt,omitempty"`
	Complete    bool   `json:"complete"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

// FindingsOpenView is one open finding of the latest complete round. Repeated
// is set when an earlier fix round addressed it and it came back. Body is the
// reviewer's explanation, served on the view only (findingsBody).
type FindingsOpenView struct {
	Reviewer      string `json:"reviewer"`
	Plugin        string `json:"plugin"`
	Severity      string `json:"severity"`
	Path          string `json:"path,omitempty"`
	Line          int64  `json:"line,omitempty"`
	Title         string `json:"title"`
	Body          string `json:"body,omitempty"`
	BodyTruncated bool   `json:"bodyTruncated,omitempty"`
	Fingerprint   string `json:"fingerprint"`
	AutoFix       bool   `json:"autoFix"`
	Repeated      bool   `json:"repeated"`
}

// findingsLifecycle is "merged" or "closed" for a receipt whose change request
// is no longer live, or "".
func findingsLifecycle(receipt delivery.Receipt) string {
	switch {
	case findingsLive(receipt):
		return ""
	case receipt.Lifecycle.State == "MERGED":
		return "merged"
	}
	return "closed"
}

// FindingsDismissal is one dismissed finding and the operator's reason.
type FindingsDismissal struct {
	Fingerprint string `json:"fingerprint"`
	Reviewer    string `json:"reviewer"`
	Reason      string `json:"reason"`
	At          string `json:"at,omitempty"`
}

// FindingsFixRound is one fix round the receipt records.
type FindingsFixRound struct {
	Round    int    `json:"round"`
	Commit   string `json:"commit"`
	PushedAt string `json:"pushedAt,omitempty"`
}

// FindingsPendingView is the latest plan's fix round, not yet on the receipt.
// Preview is served when the round is ready to push; PreviewError when its
// verification passed but the engine's preview refused.
type FindingsPendingView struct {
	Round        int                  `json:"round"`
	State        string               `json:"state"`
	Findings     int                  `json:"findings"`
	Preview      *FixRoundPreviewView `json:"preview,omitempty"`
	PreviewError string               `json:"previewError,omitempty"`
}

// FixRoundPreviewView is what an approval of Digest pushes. Its names are
// display text; Digest binds the engine's raw values.
type FixRoundPreviewView struct {
	Digest               string   `json:"digest"`
	Round                int      `json:"round"`
	Branch               string   `json:"branch"`
	Parent               string   `json:"parent"`
	Tree                 string   `json:"tree"`
	Title                string   `json:"title"`
	Files                []string `json:"files"`
	Findings             int      `json:"findings"`
	AcceptanceUnverified bool     `json:"acceptanceUnverified"`
	ReviewGreen          bool     `json:"reviewGreen"`
	NeedsConfirm         bool     `json:"needsConfirm"`
}

// FindingsJobView is a collection or push job of this Lab UI.
type FindingsJobView struct {
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Category string `json:"category,omitempty"`
	Error    string `json:"error,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func findingsJobOf(j jobSnapshot) *FindingsJobView {
	return &FindingsJobView{Kind: j.kind, State: j.state, Category: j.category, Error: j.message, Reason: j.reason}
}

// findingsLine is a finding's line when its served path is set and the line
// is positive, or 0.
func findingsLine(path string, line int64) int64 {
	if path == "" || line <= 0 {
		return 0
	}
	return line
}

// findingsView builds the view; only the preview of a verified fix round runs
// a process (read-only git through the engine), bounded by ctx.
func (h *handler) findingsView(ctx context.Context, r molecule.Record) ReviewerFindingsView {
	f, ok := findingsSnapshot(r)
	if !ok {
		return ReviewerFindingsView{}
	}
	lab := h.opts.LabDir
	x := newRedactor(lab, r)
	v := ReviewerFindingsView{Reviewers: true, Collecting: h.findingsBusy(r.ID)}
	for _, ref := range f.Reviewers() {
		v.Plugins = append(v.Plugins, findingsText(x, ref.Plugin, findingsMaxRuneText))
	}
	if j, ok := h.jobs.get(findingsJobKey(r.ID)); ok {
		v.Collect = findingsJobOf(j)
	}
	if j, ok := h.jobs.get(fixRoundJobKey(r.ID)); ok {
		v.Push = findingsJobOf(j)
	}
	receipt, rerr := delivery.ReadFixRoundReceipt(lab, r.ID)
	addressed := map[string]bool{}
	if rerr == nil {
		v.Published = true
		v.Head, _ = delivery.ReceiptHead(receipt)
		v.Lifecycle = findingsLifecycle(receipt)
		for _, fr := range receipt.FixRounds {
			v.FixRounds = append(v.FixRounds, FindingsFixRound{Round: fr.Round, Commit: fr.Commit, PushedAt: stepSince(fr.PushedAt)})
			for _, fp := range fr.Findings {
				addressed[fp] = true
			}
		}
	}
	record, err := delivery.ReadFindings(lab, r.ID)
	if err != nil {
		v.Unreadable, v.Error = true, findingsUnreadableTxt
	} else {
		findingsRecordView(x, f, record, addressed, &v)
	}
	if rerr == nil {
		v.Pending = h.fixRoundPendingView(ctx, x, r, receipt)
	}
	return v
}

// findingsRecordView fills the rounds, open findings and dismissals.
func findingsRecordView(x redactor, f formula.Formula, record delivery.FindingsRecord, addressed map[string]bool, v *ReviewerFindingsView) {
	for i, ref := range f.Reviewers() {
		key := delivery.ReviewerKey(i, ref.Plugin)
		for n := len(record.Rounds) - 1; n >= 0; n-- {
			round := record.Rounds[n]
			found := false
			for _, e := range round.Reviewers {
				if e.Reviewer != key {
					continue
				}
				v.Rounds = append(v.Rounds, FindingsRoundView{Reviewer: findingsText(x, key, findingsMaxRuneText), Plugin: findingsText(x, e.Plugin, findingsMaxRuneText), Commit: round.Commit, CollectedAt: stepSince(round.CollectedAt), Complete: round.Complete, Status: e.Status, Error: findingsText(x, e.Error, findingsMaxRuneText)})
				found = true
				break
			}
			if found {
				break
			}
		}
	}
	s := delivery.Summarize(record, findingsAutoFix(f))
	if s.Round >= 0 {
		v.Round, v.Commit, v.OnHead = s.Round+1, s.Commit, v.Head != "" && s.Commit == v.Head
	}
	for _, g := range s.Open {
		for _, sf := range g.Findings {
			v.OpenCount++
			if len(v.Open) == findingsMaxOpen {
				v.Truncated = true
				continue
			}
			title := findingsText(x, sf.Title, findingsMaxRuneText)
			if strings.TrimSpace(title) == "" {
				title = "(untitled)"
			}
			path := findingsPath(x, sf.Path)
			body, cut := findingsBody(x, sf.Body, sf.BodyTruncated)
			v.Open = append(v.Open, FindingsOpenView{Reviewer: findingsText(x, sf.Reviewer, findingsMaxRuneText), Plugin: findingsText(x, sf.Plugin, findingsMaxRuneText), Severity: sf.Severity, Path: path, Line: findingsLine(path, sf.Line), Title: title, Body: body, BodyTruncated: cut && body != "", Fingerprint: sf.Fingerprint, AutoFix: sf.AutoFix, Repeated: addressed[sf.Fingerprint]})
		}
	}
	v.OpenCount = count(v.OpenCount)
	v.Resolved, v.Dismissed = count(len(s.Resolved)), count(s.Dismissed)
	for _, d := range record.Dismissed {
		v.Dismissals = append(v.Dismissals, FindingsDismissal{Fingerprint: d.Fingerprint, Reviewer: findingsText(x, d.Reviewer, findingsMaxRuneText), Reason: findingsText(x, d.Reason, delivery.MaxDismissalReasonRunes), At: stepSince(d.At)})
	}
}

// fixRoundPendingView is the pending fix round, with its push preview once it
// is verified.
func (h *handler) fixRoundPendingView(ctx context.Context, x redactor, r molecule.Record, receipt delivery.Receipt) *FindingsPendingView {
	pending, ok := fixRoundPendingOf(r, receipt)
	if !ok {
		return nil
	}
	ref := pending.Plan.FixRound
	p := &FindingsPendingView{Round: ref.Round, State: fixRoundLedgerState(r, pending.Digest), Findings: count(len(ref.Findings))}
	if p.State != fixRoundVerified {
		return p
	}
	ctx, cancel := context.WithTimeout(ctx, findingsWriteTimeout)
	defer cancel()
	preview, err := h.fixRoundPreview(ctx, h.opts.LabDir, r.ID)
	if err != nil {
		p.PreviewError = fixRoundPreviewText
		return p
	}
	p.State, p.Preview = fixRoundReady, fixRoundPreviewOf(x, preview)
	return p
}

// fixRoundPreviewOf is the served preview: every file name and the branch are
// one bounded display line, as the title is. The digest is the engine's, over
// the raw values.
func fixRoundPreviewOf(x redactor, p delivery.FixRoundPreview) *FixRoundPreviewView {
	files := make([]string, 0, len(p.Files))
	for _, f := range p.Files {
		files = append(files, findingsText(x, f, findingsMaxPathBytes))
	}
	return &FixRoundPreviewView{Digest: p.Digest, Round: p.Round, Branch: findingsText(x, p.Branch, findingsMaxPathBytes), Parent: p.Parent, Tree: p.Tree, Title: findingsText(x, p.Title, maxTitleBytes), Files: files, Findings: count(len(p.Findings)), AcceptanceUnverified: p.AcceptanceUnverified, ReviewGreen: p.ReviewGreen, NeedsConfirm: p.NeedsConfirm()}
}
