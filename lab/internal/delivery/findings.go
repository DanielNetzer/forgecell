package delivery

import (
	"bytes"
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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/reviewer"
	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// Bounds of the findings record, deliveries/<id>.findings.json. Hitting one
// fails safe: the oldest rounds are dropped and counted, and a round that lost
// findings is marked incomplete, never silently accepted.
const (
	MaxFindingsRecordBytes = 4 << 20
	MaxFindingsPerReviewer = 500
	MaxFindingsRounds      = 20
	MaxFindingBodyBytes    = 16 << 10
	// maxFindingsCalls bounds one round's plugin call records.
	maxFindingsCalls     = 32
	maxFindingsComments  = 500
	maxFindingsChecks    = 200
	maxFindingTitleRunes = 300
	maxFindingBodyRunes  = 65536
	maxFindingIDRunes    = 100
	maxFindingLine       = 10_000_000
	maxFindingsError     = 300
	findingsVersion      = "v1"
)

// The status of one reviewer in a round: complete and pending are the
// reviewer's own answer; error means no answer was recorded.
const (
	FindingsComplete = reviewer.StatusComplete
	FindingsPending  = reviewer.StatusPending
	FindingsError    = "error"
)

// ErrNoChangeRequest refuses a receipt whose change request cannot be named.
var ErrNoChangeRequest = errors.New("the delivery receipt names no change request")

// ErrNoReviewers refuses a collection without reviewers.
var ErrNoReviewers = errors.New("this Lab's Formula declares no reviewers")

// The fixed per-reviewer errors of forge reads.
const (
	commentsUnsupported = "this forge does not serve review comments"
	commentsUnreadable  = "the change request's comments could not be read"
	checksUnreadable    = "the change request's checks could not be read"
	checksStale         = "the change request's head moved past the delivered commit, so its checks are not this commit's"
)

// checksPlaceholder is the required-check name a GitHub checks read for a
// reviewer passes: the reviewer gets the checks, never a pass or fail verdict.
const checksPlaceholder = "forgecell-reviewer-input"

var (
	githubPullNumber = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
	fingerprintHex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Finding is one recorded finding. Fingerprint identifies it across rounds;
// PluginID is the plugin's own id, for display only. Resolved is the plugin's
// own claim. Path is a clean repository-relative path or empty, and Note says
// why a path was cleared.
type Finding struct {
	Fingerprint   string `json:"fingerprint"`
	Severity      string `json:"severity"`
	Path          string `json:"path"`
	Line          int64  `json:"line"`
	Title         string `json:"title"`
	Body          string `json:"body"`
	BodyTruncated bool   `json:"bodyTruncated,omitempty"`
	PluginID      string `json:"pluginID"`
	Resolved      bool   `json:"resolved"`
	Note          string `json:"note,omitempty"`
}

// ReviewerRound is one reviewer's answer in a round. Reviewer is its identity,
// ReviewerKey of its Formula index and plugin, so the same plugin declared
// twice never shares findings; Plugin is for display. Truncated and Dropped
// count findings left out by a bound; InputDropped counts comments or checks
// the reviewer was not handed, and InputIncomplete is set when the forge said
// its checks list is partial.
type ReviewerRound struct {
	Reviewer        string    `json:"reviewer"`
	Plugin          string    `json:"plugin"`
	Status          string    `json:"status"`
	Error           string    `json:"error,omitempty"`
	Findings        []Finding `json:"findings"`
	Truncated       bool      `json:"truncated,omitempty"`
	Dropped         int       `json:"dropped,omitempty"`
	InputDropped    int       `json:"inputDropped,omitempty"`
	InputIncomplete bool      `json:"inputIncomplete,omitempty"`
}

// FindingsRound is one collection against the exact delivered commit.
// Complete is false when any reviewer's findings or input were cut by a bound
// or the forge served partial input.
type FindingsRound struct {
	Commit             string                  `json:"commit"`
	Ref                string                  `json:"ref"`
	CollectedAt        string                  `json:"collectedAt"`
	Complete           bool                    `json:"complete"`
	Reviewers          []ReviewerRound         `json:"reviewers"`
	PluginCalls        []mcpadapter.CallRecord `json:"pluginCalls,omitempty"`
	PluginCallsDropped int                     `json:"pluginCallsDropped,omitempty"`
}

// FindingsRecord is deliveries/<id>.findings.json: untrusted evidence beside
// the receipt, which it never changes. RoundsDropped counts the oldest rounds
// dropped by a bound.
type FindingsRecord struct {
	SchemaVersion string          `json:"schemaVersion"`
	MoleculeID    string          `json:"moleculeId"`
	Rounds        []FindingsRound `json:"rounds"`
	RoundsDropped int             `json:"roundsDropped,omitempty"`
}

// FindingsReviewer is one Formula reviewer: its plugin name and the reviewer.
// Its index in FindingsOptions.Reviewers is its Formula index.
type FindingsReviewer struct {
	Plugin   string
	Reviewer reviewer.Reviewer
}

// ReviewerKey is a reviewer's identity in the findings record, its Formula
// index and plugin name, such as 0:critic.
func ReviewerKey(index int, plugin string) string {
	return strconv.Itoa(index) + ":" + plugin
}

// FindingsOptions configure Collect. Repo is the Formula's intake.repo, which
// a GitHub receipt's pull request URL must name. Changes is the Lab's plugin
// forge, or nil for GitHub, whose checks GitHub reads; ForgePlugin and
// ForgeProject are that forge plugin's name and project from the Formula, and
// a plugin receipt must name exactly them. Calls is the log the adapters'
// Observer writes to; Now defaults to time.Now.
type FindingsOptions struct {
	LabDir       string
	MoleculeID   string
	Repo         string
	Reviewers    []FindingsReviewer
	Changes      forge.Changes
	ForgePlugin  string
	ForgeProject string
	GitHub       forge.GitHub
	Calls        *PluginCallLog
	Now          func() time.Time
}

// Collect appends one findings round for the published delivery. It reads the
// receipt, which must be published with a commit and a change request, calls
// every reviewer for exactly that commit, and writes the record under the
// delivery lock. A reviewer's error or pending answer is recorded for it alone
// and never stops the others. Comments and checks are read only for a reviewer
// that describes the matching capability. Nothing here approves, pushes or
// changes the receipt.
func Collect(ctx context.Context, o FindingsOptions) (FindingsRecord, error) {
	if !safeID.MatchString(o.MoleculeID) {
		return FindingsRecord{}, fmt.Errorf("invalid Molecule id")
	}
	if len(o.Reviewers) == 0 {
		return FindingsRecord{}, ErrNoReviewers
	}
	if len(o.Reviewers) > formula.MaxReviewers {
		return FindingsRecord{}, fmt.Errorf("at most %d reviewers are collected", formula.MaxReviewers)
	}
	lock := filepath.Join(o.LabDir, ".delivery-lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return FindingsRecord{}, fmt.Errorf("delivery/findings collection conflict: another delivery operation holds the lock")
	}
	defer os.Remove(lock)
	r, err := readPublishedReceipt(o.LabDir, o.MoleculeID)
	if err != nil {
		return FindingsRecord{}, err
	}
	ref, pr, forgeObject, err := findingsTarget(o, r)
	if err != nil {
		return FindingsRecord{}, err
	}
	record, err := ReadFindings(o.LabDir, o.MoleculeID)
	if err != nil {
		return FindingsRecord{}, err
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	// Reviewers read the change request's current head, the last fix round's commit.
	head, _ := ReceiptHead(r)
	round := FindingsRound{Commit: head, Ref: ref, CollectedAt: now().UTC().Format(time.RFC3339), Complete: true, Reviewers: []ReviewerRound{}}
	in := &forgeInputs{o: o, ref: ref, pr: pr, commit: head}
	for i, fr := range o.Reviewers {
		entry := collectOne(ctx, ReviewerKey(i, fr.Plugin), fr, reviewer.Request{Ref: ref, Commit: head, Forge: forgeObject}, in)
		if entry.Truncated || entry.InputDropped > 0 || entry.InputIncomplete {
			round.Complete = false
		}
		round.Reviewers = append(round.Reviewers, entry)
	}
	calls, dropped := o.Calls.take()
	if len(calls) > maxFindingsCalls {
		dropped += len(calls) - maxFindingsCalls
		calls = calls[:maxFindingsCalls]
	}
	round.PluginCalls, round.PluginCallsDropped = calls, dropped
	record.Rounds = append(record.Rounds, round)
	if err := fitFindings(&record); err != nil {
		return FindingsRecord{}, err
	}
	if err := writeFindings(o.LabDir, o.MoleculeID, record); err != nil {
		return FindingsRecord{}, err
	}
	return record, nil
}

// readPublishedReceipt reads the receipt with openReceipt's safety and
// requires it to be published at an exact commit.
func readPublishedReceipt(lab, id string) (Receipt, error) {
	f, found, err := openReceipt(lab, id)
	if f == nil {
		if err == nil && !found {
			err = fmt.Errorf("no delivery receipt exists for this Molecule")
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
	if r.State != "published" || !pluginCommit.MatchString(r.Commit) {
		return Receipt{}, fmt.Errorf("findings are collected only for a published delivery with an exact commit")
	}
	if err := checkFixRounds(r); err != nil {
		return Receipt{}, fmt.Errorf("delivery receipt fix rounds are invalid: %w", err)
	}
	return r, nil
}

// findingsTarget names the receipt's change request for the reviewers: a
// plugin forge receipt's recorded ref, or a GitHub receipt's pull request
// number. The Lab's forge must be the one the delivery was published through:
// the same plugin and project for a plugin receipt, and no plugin for GitHub.
func findingsTarget(o FindingsOptions, r Receipt) (string, int, map[string]any, error) {
	if r.Forge != "" {
		if o.Changes == nil || o.ForgePlugin == "" {
			return "", 0, nil, fmt.Errorf("this delivery was published through a forge plugin, but the Lab's Formula declares none")
		}
		if r.Forge != o.ForgePlugin || r.Project != o.ForgeProject {
			return "", 0, nil, fmt.Errorf("this delivery was published through another forge plugin or project than the Lab's Formula declares")
		}
		if !validPluginRef(r.Ref) {
			return "", 0, nil, ErrNoChangeRequest
		}
		return r.Ref, 0, map[string]any{"kind": "plugin", "plugin": r.Forge, "host": r.ForgeHost, "project": r.Project, "url": r.URL}, nil
	}
	if o.Changes != nil || o.ForgePlugin != "" {
		return "", 0, nil, fmt.Errorf("this delivery was published through GitHub, but the Lab's Formula declares a forge plugin")
	}
	ref, pr, err := GitHubPullRef(r.URL, o.Repo)
	if err != nil {
		return "", 0, nil, err
	}
	return ref, pr, map[string]any{"kind": "github", "host": "github.com", "project": o.Repo, "url": r.URL}, nil
}

// GitHubPullRef returns the pull request number of a GitHub receipt URL as the
// reviewer ref, its decimal string, and as an int. The URL must be exactly
// https://github.com/<repo>/pull/<N>, the repository matching repo ignoring
// ASCII case only and N a positive decimal of at most 10 digits; any other is
// refused.
func GitHubPullRef(raw, repo string) (string, int, error) {
	rest, ok := strings.CutPrefix(raw, "https://github.com/")
	if !ok || repo == "" {
		return "", 0, ErrNoChangeRequest
	}
	at := strings.LastIndex(rest, "/pull/")
	if at < 0 || !asciiEqualFold(rest[:at], repo) {
		return "", 0, ErrNoChangeRequest
	}
	number := rest[at+len("/pull/"):]
	if !githubPullNumber.MatchString(number) {
		return "", 0, ErrNoChangeRequest
	}
	n, err := strconv.Atoi(number)
	if err != nil {
		return "", 0, ErrNoChangeRequest
	}
	return number, n, nil
}

// asciiEqualFold reports whether a and b are equal ignoring ASCII case. Any
// non-ASCII byte refuses, so no Unicode case folding (such as the Kelvin sign
// for k) ever matches.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	lower := func(c byte) byte {
		if 'A' <= c && c <= 'Z' {
			return c + 'a' - 'A'
		}
		return c
	}
	for i := 0; i < len(a); i++ {
		if a[i] >= 0x80 || b[i] >= 0x80 || lower(a[i]) != lower(b[i]) {
			return false
		}
	}
	return true
}

// collectOne records one reviewer's answer; every failure is its own.
func collectOne(ctx context.Context, key string, fr FindingsReviewer, req reviewer.Request, in *forgeInputs) ReviewerRound {
	entry := ReviewerRound{Reviewer: key, Plugin: fr.Plugin, Status: FindingsError, Findings: []Finding{}}
	if fr.Reviewer == nil {
		entry.Error = "the reviewer is not available"
		return entry
	}
	d, err := fr.Reviewer.Describe(ctx)
	if err != nil {
		entry.Error = reviewerError(err)
		return entry
	}
	if d.Has(reviewer.CapabilityComments) {
		comments, dropped, why := in.readComments(ctx)
		if why != "" {
			entry.Error = why
			return entry
		}
		req.Comments, entry.InputDropped = comments, entry.InputDropped+dropped
	}
	if d.Has(reviewer.CapabilityChecks) {
		list, dropped, partial, why := in.readChecks(ctx)
		if why != "" {
			entry.Error = why
			entry.InputDropped = 0
			return entry
		}
		req.Checks, entry.InputDropped, entry.InputIncomplete = list, entry.InputDropped+dropped, partial
	}
	result, err := fr.Reviewer.Findings(ctx, req)
	if err != nil {
		entry.Error = reviewerError(err)
		entry.InputDropped, entry.InputIncomplete = 0, false
		return entry
	}
	if result.Status != FindingsComplete && result.Status != FindingsPending {
		entry.Error = "the reviewer answered an unknown status"
		entry.InputDropped, entry.InputIncomplete = 0, false
		return entry
	}
	entry.Status = result.Status
	entry.InputDropped += max(result.InputDropped, 0)
	for _, f := range result.Findings {
		recorded, ok := recordFinding(key, f)
		if !ok || len(entry.Findings) == MaxFindingsPerReviewer {
			entry.Truncated = true
			entry.Dropped++
			continue
		}
		entry.Findings = append(entry.Findings, recorded)
	}
	return entry
}

// reviewerError is a display-safe account of a reviewer failure: the
// adapter's fixed form, or only the fixed reason of any other error.
func reviewerError(err error) string {
	var e *mcpadapter.Error
	if errors.As(err, &e) {
		return safetext.Text(e.Error(), maxFindingsError)
	}
	return "the reviewer failed: " + string(mcp.ReasonOf(err))
}

// recordFinding sanitizes one finding for the record and fingerprints it under
// the reviewer identity key. A finding with an unknown severity is not
// recorded.
func recordFinding(key string, f reviewer.Finding) (Finding, bool) {
	if !slices.Contains(reviewer.Severities, f.Severity) {
		return Finding{}, false
	}
	out := Finding{Severity: f.Severity, Title: safetext.Text(f.Title, maxFindingTitleRunes), PluginID: safetext.Text(f.ID, maxFindingIDRunes), Resolved: f.Resolved, Note: safetext.Text(f.Note, maxFindingsError)}
	if f.Path != "" {
		if mcpadapter.CleanRepoPath(f.Path) {
			out.Path = f.Path
		} else {
			out.Note = mcpadapter.PathClearedNote
		}
	}
	if f.Line > 0 && f.Line <= maxFindingLine {
		out.Line = f.Line
	}
	out.Body, out.BodyTruncated = cutBody(safetext.Multiline(f.Body, maxFindingBodyRunes))
	out.Fingerprint = FindingFingerprint(key, out.Severity, out.Path, out.Title)
	return out, true
}

// FindingFingerprint is sha256 over the reviewer identity, severity, path and
// normalized title joined by NUL bytes, which none of them can hold, so no
// separator in a path or title can collide. The title is lower-cased with
// whitespace runs collapsed, so it is stable across the plugin's own ids and
// the title's spacing and case.
func FindingFingerprint(reviewerKey, severity, path, title string) string {
	normalized := strings.Join(strings.Fields(strings.ToLower(title)), " ")
	sum := sha256.Sum256([]byte(strings.Join([]string{reviewerKey, severity, path, normalized}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// ReadFindings returns deliveries/<id>.findings.json, or an empty record when
// none exists. It refuses an invalid id, a symlinked or non-regular record or
// deliveries directory, a record over its bound, and one that is malformed,
// names another Molecule or exceeds a bound.
func ReadFindings(lab, id string) (FindingsRecord, error) {
	empty := FindingsRecord{SchemaVersion: findingsVersion, MoleculeID: id, Rounds: []FindingsRound{}}
	if !safeID.MatchString(id) {
		return FindingsRecord{}, fmt.Errorf("invalid Molecule id")
	}
	dir := filepath.Join(lab, "deliveries")
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return empty, nil
	}
	if err != nil || !info.IsDir() {
		return FindingsRecord{}, fmt.Errorf("deliveries directory is not a regular directory")
	}
	file := filepath.Join(dir, id+".findings.json")
	info, err = os.Lstat(file)
	if os.IsNotExist(err) {
		return empty, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxFindingsRecordBytes {
		return FindingsRecord{}, fmt.Errorf("the findings record is not a bounded regular file")
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return FindingsRecord{}, fmt.Errorf("the findings record cannot be opened")
	}
	defer f.Close()
	if stat, err := f.Stat(); err != nil || !os.SameFile(info, stat) {
		return FindingsRecord{}, fmt.Errorf("opened findings record identity mismatch")
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxFindingsRecordBytes+1))
	if err != nil || len(raw) > MaxFindingsRecordBytes {
		return FindingsRecord{}, fmt.Errorf("the findings record is not a bounded regular file")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var record FindingsRecord
	if err := dec.Decode(&record); err != nil {
		return FindingsRecord{}, fmt.Errorf("malformed findings record")
	}
	if _, err := dec.Token(); err != io.EOF {
		return FindingsRecord{}, fmt.Errorf("malformed findings record")
	}
	if err := checkFindings(id, record); err != nil {
		return FindingsRecord{}, err
	}
	if record.Rounds == nil {
		record.Rounds = []FindingsRound{}
	}
	return record, nil
}

func checkFindings(id string, record FindingsRecord) error {
	bad := fmt.Errorf("the findings record exceeds a bound or is malformed")
	if record.SchemaVersion != findingsVersion || record.MoleculeID != id {
		return fmt.Errorf("findings record identity mismatch")
	}
	if len(record.Rounds) > MaxFindingsRounds || record.RoundsDropped < 0 {
		return bad
	}
	for _, round := range record.Rounds {
		if !pluginCommit.MatchString(round.Commit) || len(round.Reviewers) > formula.MaxReviewers || len(round.PluginCalls) > maxFindingsCalls || round.PluginCallsDropped < 0 {
			return bad
		}
		for i, entry := range round.Reviewers {
			if entry.Reviewer != ReviewerKey(i, entry.Plugin) {
				return bad
			}
			if entry.Status != FindingsComplete && entry.Status != FindingsPending && entry.Status != FindingsError {
				return bad
			}
			if len(entry.Findings) > MaxFindingsPerReviewer || entry.Dropped < 0 || entry.InputDropped < 0 {
				return bad
			}
			for _, f := range entry.Findings {
				if !fingerprintHex.MatchString(f.Fingerprint) || severityRank[f.Severity] == 0 && f.Severity != "info" || len(f.Body) > MaxFindingBodyBytes || (f.Path != "" && !mcpadapter.CleanRepoPath(f.Path)) {
					return bad
				}
			}
		}
	}
	return nil
}

// writeFindings writes the record beside the receipt: never through a symlink
// or onto a non-regular file, by an atomic rename of a private temporary file.
func writeFindings(lab, id string, record FindingsRecord) error {
	if !safeID.MatchString(id) {
		return fmt.Errorf("invalid Molecule id")
	}
	dir := filepath.Join(lab, "deliveries")
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("deliveries directory is not a regular directory")
	}
	file := filepath.Join(dir, id+".findings.json")
	if info, err := os.Lstat(file); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("the findings record is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("the findings record cannot be read")
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > MaxFindingsRecordBytes {
		return fmt.Errorf("the findings record exceeds its bound")
	}
	f, err := os.CreateTemp(dir, ".findings-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), file); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
