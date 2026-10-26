package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Summary is one Molecule in GET /api/molecules.
type Summary struct {
	ID          string `json:"id"`
	Repository  string `json:"repository"`
	Number      int64  `json:"number"`
	Title       string `json:"title"`
	FormulaID   string `json:"formulaId"`
	Status      string `json:"status"`
	Phase       string `json:"phase"`
	CurrentAtom string `json:"currentAtom"`
	StartedAt   string `json:"startedAt"`
	FinishedAt  string `json:"finishedAt"`
	Lane        string `json:"lane"`
	Activity    string `json:"activity,omitempty"`
	Delivery    string `json:"delivery,omitempty"`
}

// MoleculeList is GET /api/molecules. Counts are taken from Molecules, so they
// always reconcile with it, and lane counts sum to All.
type MoleculeList struct {
	Sample      bool           `json:"sample"`
	ETag        string         `json:"etag"`
	Complete    bool           `json:"complete"`
	Truncated   bool           `json:"truncated"`
	Diagnostics []string       `json:"diagnostics"`
	Counts      map[string]int `json:"counts"`
	Molecules   []Summary      `json:"molecules"`
}

// Detail is GET /api/molecules/{id}: a projection of molecule.Record. It never
// carries the Lab directory, workspace or source paths, credentials, environment
// or HTML; absolute local paths inside free text are replaced by labels such as
// <lab> or ~, or by "[local path]".
type Detail struct {
	Sample      bool           `json:"sample"`
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	Phase       string         `json:"phase"`
	Lane        string         `json:"lane"`
	Activity    string         `json:"activity,omitempty"`
	Mode        string         `json:"mode"`
	FormulaID   string         `json:"formulaId"`
	StartedAt   string         `json:"startedAt"`
	FinishedAt  string         `json:"finishedAt"`
	CurrentAtom string         `json:"currentAtom"`
	Issue       IssueView      `json:"issue"`
	Workspace   WorkspaceView  `json:"workspace"`
	Formula     FormulaView    `json:"formula"`
	Readiness   *ReadinessView `json:"readiness"`
	Atoms       []AtomView     `json:"atoms"`
	Attempts    []AttemptView  `json:"attempts"`
	Delivery    *DeliveryView  `json:"delivery"`
	Gate        GateView       `json:"gate"`
	Next        NextView       `json:"next"`
	Diagnostics []string       `json:"diagnostics"`
}

// Gate states. GATES AND DECISIONS and NEXT ACTION both render the one GateView
// of a Detail, so they cannot disagree about whether a gate is open.
const (
	// GateScope: the exact plan Digest waits for scope approval.
	GateScope = "scope"
	// GateDelivery: the finished Molecule may be previewed and published as a draft.
	GateDelivery = "delivery"
	// GatePublished: the review gate was resolved by draft publication.
	GatePublished = "published"
	// GateNone: no human gate this UI performs is open.
	GateNone = "none"
)

// GateView is the selected Molecule's gate state. Receipt, Lifecycle and URL are
// set for GatePublished; URL only when it is the Issue repository's pull request.
type GateView struct {
	State     string `json:"state"`
	Digest    string `json:"digest,omitempty"`
	Receipt   string `json:"receipt,omitempty"`
	Lifecycle string `json:"lifecycle,omitempty"`
	URL       string `json:"url,omitempty"`
}

// NextView is the NEXT ACTION title and copy, derived from the lane and the
// GateView: it names Gates and decisions only while a gate there is open.
type NextView struct {
	Title string `json:"title"`
	Copy  string `json:"copy"`
}

// gateState is the one gate state of a record in lane: a pending scope approval
// of a Molecule waiting for a decision first, then a published receipt, then an
// unpublished finished Molecule. A running or blocked Molecule has no open gate.
func gateState(r molecule.Record, lane string, d *DeliveryState, view *DeliveryView) GateView {
	if digest, ok := pendingDigest(r); ok && lane == LaneDecision && sha256Hex.MatchString(digest) {
		return GateView{State: GateScope, Digest: digest}
	}
	if d.Published() {
		g := GateView{State: GatePublished, Receipt: stateText(d.State), Lifecycle: stateText(d.Lifecycle)}
		if view != nil {
			g.URL = view.URL
		}
		return g
	}
	if (lane == LaneDecision || lane == LaneOther) && r.FinishedAt != "" && (r.Status == "waiting" || r.Status == "recorded") {
		return GateView{State: GateDelivery}
	}
	return GateView{State: GateNone}
}

// NextPublished is NEXT ACTION for a published draft.
const NextPublished = "Draft published · merge on GitHub; merge and production are not verified by Forgecell."

// nextAction is NEXT ACTION for a Molecule in lane with gate g.
func nextAction(lane, activity, status, phase, current string, g GateView, delivery *DeliveryView) NextView {
	at := ""
	if current = stateText(current); current != "" {
		at = " · " + current
	}
	recorded := func(s string) string {
		if s = stateText(s); s != "" {
			return s
		}
		return "Not recorded"
	}
	label := ""
	if delivery != nil {
		label = delivery.Label + "."
	}
	switch lane {
	case LaneDecision:
		if g.State == GateScope || g.State == GateDelivery {
			return NextView{"Needs your decision" + at, "This Molecule waits at a human gate (phase " + recorded(phase) + "). Review it and decide under Gates and decisions, or with the forgecell CLI. The engine checks every decision again."}
		}
		return NextView{"Waiting at a human gate" + at, "This Molecule waits at a human gate (phase " + recorded(phase) + ") that this Lab UI does not perform. Inspect it with forgecell ledger and decide with the forgecell CLI. The engine checks every decision again."}
	case LaneRunning:
		if activity == ActivityQueued {
			return NextView{"Queued for checks" + at, "Another Molecule in this Lab holds the verification slot. Checks start, and their time budgets begin, when the slot is free. This view updates from the ledger."}
		}
		return NextView{"Running" + at, "The Lab is working within the approved scope. This view updates from the ledger."}
	case LaneBlocked:
		return NextView{"Blocked" + at, strings.TrimSpace("Status " + recorded(status) + ". Failure evidence stays in the ledger; a failed or blocked run is not a delivery. " + label)}
	case LaneFinished:
		return NextView{"Draft published", NextPublished}
	}
	if g.State == GateDelivery {
		return NextView{"Ready for a draft PR" + at, strings.TrimSpace("This Molecule finished with status " + recorded(status) + ". Preview and publish a draft PR under Gates and decisions, or with the forgecell CLI. " + label)}
	}
	if label == "" {
		label = "No published delivery receipt. This Molecule is in no lane; it appears under All and Other."
	}
	return NextView{"Recorded · " + recorded(status), label}
}

type IssueView struct {
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	Title      string `json:"title"`
	// Body is the recorded Issue text, served as plain text and never rendered.
	Body      string `json:"body"`
	State     string `json:"state"`
	UpdatedAt string `json:"updatedAt"`
	// URL is set only when the recorded URL names this exact Issue on GitHub.
	URL string `json:"url,omitempty"`
}

type WorkspaceView struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	BaseCommit string `json:"baseCommit"`
}

type FormulaView struct {
	SHA256   string `json:"sha256"`
	Approved bool   `json:"approved"`
	// YAML is the saved snapshot, included only with ?yaml=1.
	YAML string `json:"yaml,omitempty"`
}

type ReadinessView struct {
	Phase  string      `json:"phase"`
	Plans  []PlanView  `json:"plans"`
	Events []EventView `json:"events"`
}

type PlanView struct {
	Revision     int    `json:"revision"`
	Digest       string `json:"digest"`
	ParentDigest string `json:"parentDigest"`
}

type EventView struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	At     string `json:"at"`
	Detail string `json:"detail"`
	Tree   string `json:"tree,omitempty"`
}

type AtomView struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Status     string          `json:"status"`
	Detail     string          `json:"detail"`
	StartedAt  string          `json:"startedAt,omitempty"`
	FinishedAt string          `json:"finishedAt,omitempty"`
	ElapsedMS  int64           `json:"elapsedMs,omitempty"`
	ExitCode   *int            `json:"exitCode,omitempty"`
	Provenance *ProvenanceView `json:"provenance,omitempty"`
}

type ProvenanceView struct {
	DeclaredSource    string       `json:"declaredSource,omitempty"`
	DeclaredBinding   string       `json:"declaredBinding,omitempty"`
	DeclaredWorkflows []string     `json:"declaredWorkflows,omitempty"`
	DeclaredPurpose   string       `json:"declaredPurpose,omitempty"`
	PlannedAction     string       `json:"plannedAction,omitempty"`
	Execution         string       `json:"execution,omitempty"`
	SkipReason        string       `json:"skipReason,omitempty"`
	Actions           []ActionView `json:"actions"`
}

type ActionView struct {
	AttemptID       string           `json:"attemptId"`
	PlanDigest      string           `json:"planDigest,omitempty"`
	ResolvedSource  string           `json:"resolvedSource,omitempty"`
	PlannedAction   string           `json:"plannedAction"`
	StartedAt       string           `json:"startedAt"`
	ExecutedBinding string           `json:"executedBinding,omitempty"`
	Observed        *ObservationView `json:"observed,omitempty"`
}

type ObservationView struct {
	Action         string         `json:"action"`
	Result         string         `json:"result"`
	FinishedAt     string         `json:"finishedAt"`
	ExternalAction string         `json:"externalAction,omitempty"`
	ExternalResult string         `json:"externalResult,omitempty"`
	Evidence       []EvidenceView `json:"evidence"`
}

// EvidenceView names evidence by a Lab-relative path. A path outside the Lab is
// withheld and says so. Viewable says the evidence endpoint may serve the path,
// subject to its SHA-256 and file checks when it is read.
type EvidenceView struct {
	Pointer  string `json:"pointer,omitempty"`
	Path     string `json:"path,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Note     string `json:"note,omitempty"`
	Viewable bool   `json:"viewable,omitempty"`
}

// AttemptView is one analysis, coding or review harness attempt with its model
// and usage exactly as recorded; a missing usage means unavailable, not zero.
type AttemptView struct {
	Kind       string         `json:"kind"`
	ID         string         `json:"id"`
	Status     string         `json:"status,omitempty"`
	StartedAt  string         `json:"startedAt,omitempty"`
	FinishedAt string         `json:"finishedAt,omitempty"`
	Model      string         `json:"model,omitempty"`
	Binding    string         `json:"binding,omitempty"`
	Usage      *harness.Usage `json:"usage,omitempty"`
}

// DeliveryView is the receipt state, reported exactly. Label is "Draft
// published" only for a published receipt, and never says merged.
type DeliveryView struct {
	State     string `json:"state"`
	Lifecycle string `json:"lifecycle,omitempty"`
	Published bool   `json:"published"`
	Label     string `json:"label"`
	// URL is set only for a GitHub pull request URL of the Issue's repository.
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

// receiptReader reads the displayed part of one Molecule's delivery receipt;
// tests replace it.
type receiptReader func(lab, id string) (delivery.ReceiptStatus, bool, error)

// loadDelivery reads a receipt for display. An unreadable receipt is reported,
// never treated as published.
func loadDelivery(read receiptReader, lab string, r molecule.Record) (*DeliveryState, *DeliveryView) {
	receipt, found, err := read(lab, r.ID)
	if !found {
		return nil, nil
	}
	if err != nil {
		return &DeliveryState{State: "unreadable"}, &DeliveryView{State: "unreadable", Label: "Delivery receipt unreadable", Error: "the receipt could not be read safely"}
	}
	d := &DeliveryState{State: receipt.State, Lifecycle: receipt.Lifecycle}
	view := &DeliveryView{State: d.State, Lifecycle: d.Lifecycle, Published: d.Published(), Label: DeliveryLabel(d)}
	if pullURL(receipt.URL, r.Issue.Repo) {
		view.URL = receipt.URL
	}
	return d, view
}

var pullPattern = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/pull/[1-9][0-9]{0,15}$`)

func pullURL(raw, repo string) bool {
	m := pullPattern.FindStringSubmatch(raw)
	return m != nil && strings.EqualFold(m[1], repo)
}

// issueURL returns the canonical URL of the recorded Issue when the recorded URL
// names exactly that Issue, and "" otherwise.
func issueURL(issue molecule.Issue) string {
	if !strings.HasPrefix(strings.ToLower(issue.URL), "https://github.com/") {
		return ""
	}
	ref, err := intake.ParseIssueRef(issue.URL)
	if err == nil {
		ref, err = intake.BindRepository(ref, issue.Repo)
	}
	if err != nil || ref.Number != issue.Number {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s/issues/%d", ref.Owner, ref.Repo, ref.Number)
}

// redactor replaces the record's local directories, and the user's home, in free
// text with labels, then replaces any other absolute path with "[local path]",
// so a projection never carries an absolute local path.
type redactor struct{ entries []redaction }

type redaction struct{ path, label string }

func newRedactor(lab string, r molecule.Record) redactor {
	var entries []redaction
	add := func(path, label string) {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return
		}
		entries = append(entries, redaction{filepath.Clean(path), label})
		if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != filepath.Clean(path) {
			entries = append(entries, redaction{resolved, label})
		}
	}
	add(r.Workspace.Path, "<workspace>")
	add(r.Workspace.SourceRoot, "<source>")
	add(r.LabDir, "<lab>")
	if abs, err := filepath.Abs(lab); err == nil {
		add(abs, "<lab>")
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home, "~")
	}
	// Longest first, so a workspace inside the Lab keeps its own label.
	sort.SliceStable(entries, func(i, j int) bool { return len(entries[i].path) > len(entries[j].path) })
	return redactor{entries: entries}
}

// pathByte reports a byte that continues a path name, so a directory matches
// only at a name boundary: home /Users/dan never rewrites /Users/danielle.
func pathByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("._-~@+%", b) >= 0
}

// localPath is an absolute path, or a file:// URL, under a real local root, in
// free text: it starts the text or follows a separator, quote or |, >, so URLs,
// a/b pairs and repo-relative text such as /api/molecules are not paths.
var localPath = regexp.MustCompile("(^|[\\s\"'`(\\[=:,|>“”‘’])((?:file://)?/(?:Users|home|root|private|var|tmp|Volumes|opt|etc|mnt|srv|System|Library)(?:/[^\\s\"'`<>()\\[\\]{},;|“”‘’]*)?)")

// pathEnd reports a character after which a root name such as /var has ended.
func pathEnd(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-~@+%", r))
}

func (x redactor) text(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		matched := false
		for _, e := range x.entries {
			if strings.HasPrefix(s[i:], e.path) && (i+len(e.path) == len(s) || !pathByte(s[i+len(e.path)])) {
				b.WriteString(e.label)
				i += len(e.path)
				matched = true
				break
			}
		}
		if !matched {
			b.WriteByte(s[i])
			i++
		}
	}
	return scrubPaths(b.String())
}

// scrubPaths replaces each remaining local path with "[local path]", keeping
// sentence punctuation that ends it.
func scrubPaths(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range localPath.FindAllStringSubmatchIndex(s, -1) {
		start, end := m[4], m[5]
		// A bare root must end there: /variable is not /var.
		if r, _ := utf8.DecodeRuneInString(s[end:]); end < len(s) && !pathEnd(r) {
			continue
		}
		path := s[start:end]
		trimmed := strings.TrimRight(path, ".:")
		b.WriteString(s[last:start])
		b.WriteString("[local path]")
		b.WriteString(path[len(trimmed):])
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func (x redactor) texts(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = x.text(v)
	}
	return out
}

// evidence keeps a Lab-relative evidence path; an absolute path inside the Lab is
// made relative and any other path is withheld.
func evidence(lab string, r molecule.Record, e molecule.ProvenanceEvidence) EvidenceView {
	v := EvidenceView{Pointer: e.Pointer, SHA256: e.SHA256}
	switch path := e.Path; {
	case path == "":
	case !filepath.IsAbs(path):
		if filepath.IsLocal(path) {
			v.Path = filepath.ToSlash(filepath.Clean(path))
		} else {
			v.Note = "path outside the Lab withheld"
		}
	default:
		for _, root := range []string{r.LabDir, lab} {
			abs, err := filepath.Abs(root)
			if root == "" || err != nil {
				continue
			}
			if rel, err := filepath.Rel(abs, path); err == nil && filepath.IsLocal(rel) {
				v.Path = filepath.ToSlash(rel)
				break
			}
		}
		if v.Path == "" {
			v.Note = "path outside the Lab withheld"
		}
	}
	return v
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// evidenceAllowlist maps each Lab-relative file the record lists as evidence
// with a SHA-256 to its recorded digests: Atom observation evidence, harness
// result and capture files, and verification observations and results. Paths
// outside the Lab, pointer-only references and malformed digests are left out,
// so the evidence endpoint serves nothing else.
func evidenceAllowlist(lab string, r molecule.Record) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	add := func(path, sum string) {
		if !sha256Hex.MatchString(sum) {
			return
		}
		v := evidence(lab, r, molecule.ProvenanceEvidence{Path: path, SHA256: sum})
		if v.Path == "" {
			return
		}
		if out[v.Path] == nil {
			out[v.Path] = map[string]bool{}
		}
		out[v.Path][sum] = true
	}
	files := func(refs ...molecule.VerificationFile) {
		for _, f := range refs {
			add(f.Path, f.SHA256)
		}
	}
	for _, a := range r.Atoms {
		if a.Provenance == nil {
			continue
		}
		for _, act := range a.Provenance.Actions {
			if act.Observed == nil {
				continue
			}
			for _, e := range act.Observed.Evidence {
				add(e.Path, e.SHA256)
			}
		}
	}
	for _, a := range r.HarnessAttempts {
		if a.Evidence != nil {
			files(*a.Evidence)
		}
		files(a.CaptureEvidence...)
	}
	for _, a := range r.VerificationAttempts {
		files(a.Observations...)
		if a.Result != nil {
			files(*a.Result)
		}
	}
	return out
}

// currentAtom is the first Atom still active or waiting, or "" when none is.
func currentAtom(r molecule.Record) string {
	for _, a := range r.Atoms {
		if a.Status == "active" || a.Status == "running" || a.Status == "waiting" {
			return a.ID
		}
	}
	return ""
}

func phase(r molecule.Record) string {
	if r.Readiness == nil {
		return ""
	}
	return r.Readiness.Phase
}

func summarize(lab string, r molecule.Record, read receiptReader) Summary {
	x := newRedactor(lab, r)
	d, view := loadDelivery(read, lab, r)
	s := Summary{ID: r.ID, Repository: r.Issue.Repo, Number: r.Issue.Number, Title: x.text(r.Issue.Title), FormulaID: r.FormulaID, Status: r.Status, Phase: phase(r), CurrentAtom: currentAtom(r), StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Lane: Lane(r.Status, phase(r), d)}
	s.Activity = Activity(s.Lane, molecule.QueuedForChecks(r))
	if view != nil {
		s.Delivery = view.Label
	}
	return s
}

func project(lab string, r molecule.Record, read receiptReader, withYAML bool) Detail {
	x := newRedactor(lab, r)
	allowed := evidenceAllowlist(lab, r)
	d, view := loadDelivery(read, lab, r)
	out := Detail{ID: r.ID, Status: r.Status, Phase: phase(r), Lane: Lane(r.Status, phase(r), d), Mode: r.Mode, FormulaID: r.FormulaID, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, CurrentAtom: currentAtom(r), Delivery: view, Diagnostics: []string{}, Atoms: []AtomView{}, Attempts: []AttemptView{}}
	out.Activity = Activity(out.Lane, molecule.QueuedForChecks(r))
	out.Gate = gateState(r, out.Lane, d, view)
	out.Next = nextAction(out.Lane, out.Activity, r.Status, out.Phase, out.CurrentAtom, out.Gate, view)
	out.Issue = IssueView{Repository: r.Issue.Repo, Number: r.Issue.Number, Title: x.text(r.Issue.Title), Body: x.text(r.Issue.Body), State: r.Issue.State, UpdatedAt: r.Issue.UpdatedAt, URL: issueURL(r.Issue)}
	if r.Issue.URL != "" && out.Issue.URL == "" {
		out.Diagnostics = append(out.Diagnostics, "recorded Issue URL does not name this Issue on GitHub; no link is shown")
	}
	out.Workspace = WorkspaceView{Repository: r.Workspace.Repo, Branch: r.Workspace.Branch, BaseCommit: r.Workspace.BaseCommit}
	out.Formula = FormulaView{SHA256: r.FormulaSnapshot.SHA256, Approved: r.FormulaApproved}
	if withYAML {
		out.Formula.YAML = r.FormulaSnapshot.YAML
		if sum := sha256.Sum256([]byte(r.FormulaSnapshot.YAML)); hex.EncodeToString(sum[:]) != r.FormulaSnapshot.SHA256 {
			out.Diagnostics = append(out.Diagnostics, "saved Formula YAML does not match its recorded SHA-256")
		}
	}
	if s := r.Readiness; s != nil {
		rv := &ReadinessView{Phase: s.Phase, Plans: []PlanView{}, Events: []EventView{}}
		for _, p := range s.Plans {
			rv.Plans = append(rv.Plans, PlanView{Revision: p.Plan.Revision, Digest: p.Digest, ParentDigest: p.Plan.ParentDigest})
		}
		for _, e := range s.Events {
			rv.Events = append(rv.Events, EventView{Kind: e.Kind, Digest: e.Digest, At: e.At, Detail: x.text(e.Detail), Tree: e.Tree})
		}
		out.Readiness = rv
	}
	for _, a := range r.Atoms {
		av := AtomView{ID: a.ID, Type: a.Type, Status: a.Status, Detail: x.text(a.Detail), StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, ElapsedMS: a.ElapsedMS, ExitCode: a.ExitCode}
		if p := a.Provenance; p != nil {
			pv := &ProvenanceView{DeclaredSource: x.text(p.DeclaredSource), DeclaredBinding: p.DeclaredBinding, DeclaredWorkflows: x.texts(p.DeclaredWorkflows), DeclaredPurpose: x.text(p.DeclaredPurpose), PlannedAction: x.text(p.PlannedAction), Execution: p.Execution, SkipReason: x.text(p.SkipReason), Actions: []ActionView{}}
			for _, act := range p.Actions {
				ac := ActionView{AttemptID: act.AttemptID, PlanDigest: act.PlanDigest, ResolvedSource: x.text(act.ResolvedSource), PlannedAction: x.text(act.PlannedAction), StartedAt: act.StartedAt, ExecutedBinding: act.ExecutedBinding}
				if o := act.Observed; o != nil {
					ov := &ObservationView{Action: x.text(o.Action), Result: x.text(o.Result), FinishedAt: o.FinishedAt, ExternalAction: x.text(o.ExternalAction), ExternalResult: x.text(o.ExternalResult), Evidence: []EvidenceView{}}
					for _, e := range o.Evidence {
						ev := evidence(lab, r, e)
						ev.Viewable = ev.Path != "" && allowed[ev.Path][ev.SHA256]
						ov.Evidence = append(ov.Evidence, ev)
					}
					ac.Observed = ov
				}
				pv.Actions = append(pv.Actions, ac)
			}
			av.Provenance = pv
		}
		out.Atoms = append(out.Atoms, av)
	}
	for _, a := range r.AnalysisAttempts {
		out.Attempts = append(out.Attempts, AttemptView{Kind: "analysis", ID: fmt.Sprintf("analysis-%d", a.Ordinal), Status: a.Status, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, Usage: a.Usage})
	}
	for _, a := range r.HarnessAttempts {
		status := ""
		if a.Coding != nil {
			status = a.Coding.Outcome
		}
		out.Attempts = append(out.Attempts, AttemptView{Kind: "coding", ID: a.ID, Status: status, Model: a.Model, Usage: a.Usage})
	}
	for _, a := range r.ReviewAttempts {
		out.Attempts = append(out.Attempts, AttemptView{Kind: "review", ID: fmt.Sprintf("review-%d", a.Ordinal), Status: a.Status, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, Binding: a.Binding, Usage: a.Usage})
	}
	return out
}

// ledgerETag fingerprints the names, sizes and modification times of the Lab's
// ledgers and delivery receipts, reading no file contents. It is bounded by the
// shared raw-entry cap; a directory beyond it, or an entry modified within
// racyWindow, changes the tag on every call rather than hiding a change.
func ledgerETag(lab string) string {
	h := sha256.New()
	for _, sub := range []string{"ledgers", "deliveries"} {
		fmt.Fprintf(h, "%s\x00", sub)
		dir := filepath.Join(lab, sub)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() {
			fmt.Fprintf(h, "absent\x00")
			continue
		}
		f, err := os.Open(dir)
		if err != nil {
			fmt.Fprintf(h, "unreadable\x00")
			continue
		}
		var entries []os.FileInfo
		for len(entries) <= maxETagEntries {
			batch, err := f.Readdir(512)
			entries = append(entries, batch...)
			if err == io.EOF || len(batch) == 0 {
				break
			}
			if err != nil {
				fmt.Fprintf(h, "unreadable\x00")
				break
			}
		}
		f.Close()
		if len(entries) > maxETagEntries {
			fmt.Fprintf(h, "truncated %d\x00", time.Now().UnixNano())
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\x00", e.Name(), e.Size(), e.ModTime().UnixNano(), e.Mode())
			// A coarse clock can give a rewrite of the same size the same stamp, so
			// while any entry is that fresh the tag changes on every call.
			if time.Since(e.ModTime()) < racyWindow {
				fmt.Fprintf(h, "racy %d\x00", time.Now().UnixNano())
			}
		}
	}
	return `"` + hex.EncodeToString(h.Sum(nil))[:32] + `"`
}

const maxETagEntries = 65536
