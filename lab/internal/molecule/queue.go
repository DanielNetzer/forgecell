package molecule

import (
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type QueueAttempt struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	Status     string `json:"status"`
	Phase      string `json:"phase,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
}
type QueueInventory struct {
	Complete    bool           `json:"complete"`
	Truncated   bool           `json:"truncated"`
	Diagnostics []string       `json:"diagnostics"`
	Attempts    []QueueAttempt `json:"-"`
}
type QueueSummary struct {
	Attempts          []QueueAttempt `json:"attempts"`
	State             string         `json:"state"`
	Latest            *QueueAttempt  `json:"latest,omitempty"`
	Active            int            `json:"ledgerReportedActiveAttempts"`
	Blocked           int            `json:"blockedAttempts"`
	DuplicateActive   bool           `json:"duplicateActive"`
	Partial           bool           `json:"partial"`
	OrderingUncertain bool           `json:"orderingUncertain"`
}

func (q *QueueInventory) problem(message string) {
	q.Complete = false
	q.Diagnostics = append(q.Diagnostics, message)
}
func (q *QueueInventory) truncate(message string) { q.Truncated = true; q.problem(message) }

// maxLoopLockFiles bounds how many advisory lock files the scan will step over.
const maxLoopLockFiles = 1 << 20

// readLedgerNames lists the directory without keeping the loop's advisory lock
// files, which are never removed and would otherwise crowd real ledgers out of
// the entry cap. It stops reading once more than 4096 other entries, or more
// than maxLoopLockFiles lock files, have been seen; callers apply the caps.
func readLedgerNames(dir *os.File) (names []string, locks int, err error) {
	for {
		batch, e := dir.Readdirnames(512)
		for _, name := range batch {
			if isLoopLock(name) {
				locks++
				continue
			}
			names = append(names, name)
		}
		if e == io.EOF {
			return names, locks, nil
		}
		if e != nil {
			return nil, 0, e
		}
		if len(names) > 4096 || locks > maxLoopLockFiles {
			return names, locks, nil
		}
	}
}

func isLoopLock(name string) bool { return strings.HasSuffix(name, loopLockSuffix) }

// queueHeaders lets every scan in this process, such as a loop pass and its
// re-read under an Issue lock, reuse the headers of unchanged ledgers.
var queueHeaders = ledger.NewHeaderCache()

// scanLedgers reads every ledger header in lab with the shared bounded reader.
// Companion files (.md, .lock and the loop's never-removed .loop-lock files) do
// not count toward the entry cap.
func scanLedgers(lab string, wrap func(string, io.Reader) io.Reader) ledger.Scan {
	return ledger.ScanDir(filepath.Join(lab, "ledgers"), ledger.ScanOptions{Cache: queueHeaders, Wrap: wrap})
}

// ReadQueueInventory never recovers or rewrites evidence. Counts report ledgers,
// not process liveness. Directory entries, files and bytes are bounded, and each
// ledger is streamed for its header and only validated after it, so memory stays
// bounded however large it is.
func ReadQueueInventory(lab string) QueueInventory { return readQueueInventory(lab, nil) }

func readQueueInventory(lab string, wrap func(string, io.Reader) io.Reader) QueueInventory {
	q := QueueInventory{Complete: true, Diagnostics: []string{}, Attempts: []QueueAttempt{}}
	scan := scanLedgers(lab, wrap)
	for _, f := range scan.Files {
		switch f.Kind {
		case ledger.ProblemNone:
		case ledger.ProblemOversized:
			q.truncate(f.Problem)
			continue
		default:
			q.problem(f.Problem)
			continue
		}
		name, header, p := f.Name, f.Header, f.Header.QueueProjection
		ref, e := intake.BindRepository(intake.IssueRef{Number: p.Issue.Number}, p.Issue.Repo)
		if e != nil {
			q.problem(name + ": missing or invalid historical Issue identity; unjoinable")
			continue
		}
		if p.Workspace != nil && p.Workspace.Repo != "" && !strings.EqualFold(p.Workspace.Repo, p.Issue.Repo) {
			q.problem(name + ": workspace/Issue repository identity mismatch")
			continue
		}
		if p.Issue.URL != "" {
			urlRef, urlErr := intake.ParseIssueRef(p.Issue.URL)
			if urlErr == nil {
				urlRef, urlErr = intake.BindRepository(urlRef, p.Issue.Repo)
			}
			if urlErr != nil || urlRef.Number != p.Issue.Number {
				q.problem(name + ": historical Issue URL identity mismatch")
				continue
			}
		}
		a := QueueAttempt{ID: header.ID, Repository: ref.Owner + "/" + ref.Repo, Number: ref.Number, Status: header.Status, StartedAt: p.StartedAt}
		if p.Readiness != nil {
			a.Phase = p.Readiness.Phase
		}
		q.Attempts = append(q.Attempts, a)
	}
	switch {
	case scan.Problem == "":
	case scan.Truncated:
		q.truncate(scan.Problem)
	default:
		q.problem(scan.Problem)
	}
	return q
}
func (q QueueInventory) Summarize(repository string, number int64) QueueSummary {
	s := QueueSummary{State: "unknown", Partial: !q.Complete, Attempts: []QueueAttempt{}}
	for _, a := range q.Attempts {
		if !strings.EqualFold(repository, a.Repository) || number != a.Number {
			continue
		}
		s.Attempts = append(s.Attempts, a)
		if a.Status == "running" || a.Status == "allocating" {
			s.Active++
		}
		if a.Status == "blocked" {
			s.Blocked++
		}
		if _, err := time.Parse(time.RFC3339Nano, a.StartedAt); err != nil {
			s.OrderingUncertain = true
		}
	}
	s.DuplicateActive = s.Active >= 2
	if len(s.Attempts) == 0 {
		if q.Complete {
			s.State = "untouched"
		}
		return s
	}
	s.State = "recorded"
	if !q.Complete {
		s.OrderingUncertain = true
	}
	if s.OrderingUncertain {
		return s
	}
	sort.Slice(s.Attempts, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, s.Attempts[i].StartedAt)
		b, _ := time.Parse(time.RFC3339Nano, s.Attempts[j].StartedAt)
		if a.Equal(b) {
			return s.Attempts[i].ID < s.Attempts[j].ID
		}
		return a.After(b)
	})
	s.Latest = &s.Attempts[0]
	return s
}
