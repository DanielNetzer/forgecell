package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/reviewer"
	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// forgeInputs reads the change request's comments and checks at most once per
// round, for the reviewers that need them, within their bounds. Dropped counts
// items left out by a bound; checksPartial is set when the forge itself said
// its checks list is not complete. Either makes the reviewer's input
// incomplete, and so the round.
type forgeInputs struct {
	o      FindingsOptions
	ref    string
	pr     int
	commit string

	commentsRead, checksRead bool
	comments                 []forge.Comment
	commentsDropped          int
	commentsErr              string
	checks                   []reviewer.Check
	checksDropped            int
	checksPartial            bool
	checksErr                string
}

func (in *forgeInputs) readComments(ctx context.Context) ([]forge.Comment, int, string) {
	if in.commentsRead {
		return in.comments, in.commentsDropped, in.commentsErr
	}
	in.commentsRead = true
	if in.o.Changes == nil {
		_, err := in.o.GitHub.Comments(ctx, in.o.Repo, in.ref, in.o.LabDir)
		if err == nil || errors.Is(err, forge.ErrUnsupported) {
			in.commentsErr = commentsUnsupported
		} else {
			in.commentsErr = commentsUnreadable
		}
		return nil, 0, in.commentsErr
	}
	got, err := in.o.Changes.Comments(ctx, in.ref, "")
	if err != nil {
		in.commentsErr = commentsUnreadable + " (" + string(mcp.ReasonOf(err)) + ")"
		return nil, 0, in.commentsErr
	}
	in.comments = []forge.Comment{}
	for _, c := range got.Comments {
		if len(in.comments) == maxFindingsComments {
			in.commentsDropped++
			continue
		}
		comment := forge.Comment{ID: c.ID, Author: c.Author, IsBot: c.IsBot, Body: c.Body, CreatedAt: c.CreatedAt}
		if c.Path != nil {
			comment.Path = *c.Path
		}
		if c.Line != nil {
			comment.Line = int(*c.Line)
		}
		in.comments = append(in.comments, comment)
	}
	return in.comments, in.commentsDropped, ""
}

// githubCheckStatus and githubCheckConclusion map GitHub's check data to the
// forge_checks item shape; anything else makes the checks unreadable.
var (
	githubCheckStatus     = map[string]string{"COMPLETED": "completed", "IN_PROGRESS": "in_progress", "QUEUED": "queued", "WAITING": "queued", "PENDING": "queued", "REQUESTED": "queued"}
	githubCheckConclusion = map[string]string{"SUCCESS": "success", "FAILURE": "failure", "ERROR": "failure", "STARTUP_FAILURE": "failure", "NEUTRAL": "neutral", "CANCELLED": "cancelled", "SKIPPED": "skipped", "TIMED_OUT": "timed_out", "ACTION_REQUIRED": "action_required", "STALE": "stale"}
)

// readChecks returns the checks, the count a bound dropped, whether the forge
// said its list is partial, and a fixed error.
func (in *forgeInputs) readChecks(ctx context.Context) ([]reviewer.Check, int, bool, string) {
	if in.checksRead {
		return in.checks, in.checksDropped, in.checksPartial, in.checksErr
	}
	in.checksRead = true
	in.checks = []reviewer.Check{}
	add := func(c reviewer.Check) {
		if len(in.checks) == maxFindingsChecks {
			in.checksDropped++
			return
		}
		c.Name = safetext.Text(c.Name, 200)
		if !checkURL(c.URL) {
			c.URL = ""
		}
		if c.Name == "" {
			in.checksDropped++
			return
		}
		in.checks = append(in.checks, c)
	}
	if in.o.Changes != nil {
		got, err := in.o.Changes.Checks(ctx, in.ref, in.commit)
		if err != nil {
			in.checksErr = checksUnreadable + " (" + string(mcp.ReasonOf(err)) + ")"
			return nil, 0, false, in.checksErr
		}
		for _, c := range got.Checks {
			check := reviewer.Check{Name: c.Name, Status: c.Status, Conclusion: c.Conclusion}
			if c.URL != nil {
				check.URL = *c.URL
			}
			add(check)
		}
		in.checksPartial = !got.Complete
		return in.checks, in.checksDropped, in.checksPartial, ""
	}
	snap, err := in.o.GitHub.Checks(ctx, checks.Options{Repo: in.o.Repo, PR: in.pr, Commit: in.commit, Required: []string{checksPlaceholder}, Dir: in.o.LabDir})
	switch {
	case err != nil || snap.Detail != "" || snap.Commit != in.commit:
		in.checksErr = checksUnreadable
	case snap.Status == "stale":
		in.checksErr = checksStale
	}
	if in.checksErr != "" {
		return nil, 0, false, in.checksErr
	}
	for _, c := range snap.Checks {
		status, ok := githubCheckStatus[c.Status]
		conclusion := ""
		if ok && status == "completed" {
			conclusion, ok = githubCheckConclusion[c.Conclusion]
		}
		if !ok {
			in.checks, in.checksErr = nil, checksUnreadable
			return nil, 0, false, in.checksErr
		}
		add(reviewer.Check{Name: c.Name, Status: status, Conclusion: conclusion, URL: c.URL})
	}
	return in.checks, in.checksDropped, false, ""
}

// checkURL reports a URL the forge_checks item shape accepts.
func checkURL(raw string) bool {
	if raw == "" || len(raw) > 2048 || safetext.Text(raw, 2048) != raw {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

// cutBody cuts a body to MaxFindingBodyBytes at a rune boundary.
func cutBody(body string) (string, bool) {
	if len(body) <= MaxFindingBodyBytes {
		return body, false
	}
	cut := MaxFindingBodyBytes
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut], true
}

// severityRank orders severities, info lowest.
var severityRank = map[string]int{"info": 0, "low": 1, "medium": 2, "high": 3}

// fitFindings applies the round and size bounds: the oldest rounds go first;
// a newest round still over MaxFindingsRecordBytes on its own loses its
// lowest-severity findings, the latest first within a severity, and is marked
// incomplete with each affected reviewer's truncated flag and dropped count.
func fitFindings(record *FindingsRecord) error {
	if n := len(record.Rounds) - MaxFindingsRounds; n > 0 {
		record.Rounds = record.Rounds[n:]
		record.RoundsDropped += n
	}
	size, err := findingsSize(record)
	if err != nil {
		return err
	}
	for size > MaxFindingsRecordBytes && len(record.Rounds) > 1 {
		record.Rounds = record.Rounds[1:]
		record.RoundsDropped++
		if size, err = findingsSize(record); err != nil {
			return err
		}
	}
	if size <= MaxFindingsRecordBytes {
		return nil
	}
	round := &record.Rounds[0]
	type victim struct{ reviewer, finding, rank int }
	var victims []victim
	for ri, entry := range round.Reviewers {
		for fi, f := range entry.Findings {
			victims = append(victims, victim{ri, fi, severityRank[f.Severity]})
		}
	}
	sort.SliceStable(victims, func(a, b int) bool {
		va, vb := victims[a], victims[b]
		if va.rank != vb.rank {
			return va.rank < vb.rank
		}
		if va.reviewer != vb.reviewer {
			return va.reviewer > vb.reviewer
		}
		return va.finding > vb.finding
	})
	dropped := map[[2]int]bool{}
	next := 0
	for size > MaxFindingsRecordBytes {
		if next == len(victims) {
			return fmt.Errorf("the findings round exceeds its record bound even without findings")
		}
		// Drop victims by their estimated size, then measure the record again.
		excess := size - MaxFindingsRecordBytes
		for excess > 0 && next < len(victims) {
			v := victims[next]
			next++
			raw, _ := json.Marshal(round.Reviewers[v.reviewer].Findings[v.finding])
			excess -= len(raw) + 1
			dropped[[2]int{v.reviewer, v.finding}] = true
			round.Reviewers[v.reviewer].Truncated = true
			round.Reviewers[v.reviewer].Dropped++
		}
		round.Complete = false
		trimmed := *round
		trimmed.Reviewers = slices.Clone(round.Reviewers)
		for ri := range trimmed.Reviewers {
			kept := []Finding{}
			for fi, f := range round.Reviewers[ri].Findings {
				if !dropped[[2]int{ri, fi}] {
					kept = append(kept, f)
				}
			}
			trimmed.Reviewers[ri].Findings = kept
		}
		probe := *record
		probe.Rounds = []FindingsRound{trimmed}
		if size, err = findingsSize(&probe); err != nil {
			return err
		}
		if size <= MaxFindingsRecordBytes {
			*round = trimmed
		}
	}
	return nil
}

// findingsSize is the size of the record as written, with its newline.
func findingsSize(record *FindingsRecord) (int, error) {
	raw, err := json.Marshal(record)
	return len(raw) + 1, err
}
