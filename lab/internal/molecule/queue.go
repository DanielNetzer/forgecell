package molecule

import (
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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

// ReadQueueInventory never recovers or rewrites evidence. Counts report ledgers,
// not process liveness. Directory entries, files and bytes are bounded.
func ReadQueueInventory(lab string) QueueInventory {
	q := QueueInventory{Complete: true, Diagnostics: []string{}, Attempts: []QueueAttempt{}}
	path := filepath.Join(lab, "ledgers")
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return q
	}
	if err != nil {
		q.problem(fmt.Sprintf("local evidence unavailable: %v", err))
		return q
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		q.problem("ledger directory is not a regular directory")
		return q
	}
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		q.problem(fmt.Sprintf("local evidence unavailable: %v", err))
		return q
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		q.problem("ledger directory identity changed")
		return q
	}
	names, err := dir.Readdirnames(4097)
	if err != nil && err != io.EOF {
		q.problem(fmt.Sprintf("ledger directory unreadable: %v", err))
		return q
	}
	if len(names) > 4096 {
		q.truncate("directory scan truncated at 4096 entries")
		return q
	}
	sort.Strings(names)
	files := 0
	total := int64(0)
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if files >= 256 {
			q.truncate("ledger scan truncated at 256 JSON files")
			break
		}
		files++
		current, e := os.Lstat(path)
		if e != nil || !os.SameFile(opened, current) {
			q.problem("ledger directory identity changed during scan")
			break
		}
		file := filepath.Join(path, name)
		info, e := os.Lstat(file)
		if e != nil {
			q.problem(fmt.Sprintf("%s: unreadable: %v", name, e))
			continue
		}
		if !info.Mode().IsRegular() {
			q.problem(name + ": refused nonregular file or symlink")
			continue
		}
		if info.Size() > 5_000_000 {
			q.truncate(name + ": file exceeds 5000000 bytes")
			continue
		}
		f, e := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if e != nil {
			q.problem(fmt.Sprintf("%s: unreadable: %v", name, e))
			continue
		}
		stat, e := f.Stat()
		if e != nil || !stat.Mode().IsRegular() || !os.SameFile(info, stat) {
			f.Close()
			q.problem(name + ": opened file identity mismatch")
			continue
		}
		remaining := int64(20_000_000) - total
		limit := int64(5_000_000)
		if remaining < limit {
			limit = remaining
		}
		raw, e := io.ReadAll(io.LimitReader(f, limit+1))
		f.Close()
		total += int64(len(raw))
		if int64(len(raw)) > limit {
			if limit == remaining {
				q.truncate("ledger byte scan truncated at 20000000 bytes")
				break
			}
			q.truncate(name + ": file grew beyond 5000000 bytes")
			continue
		}
		if e != nil {
			q.problem(fmt.Sprintf("%s: unreadable: %v", name, e))
			continue
		}
		header, e := ledger.Decode(raw)
		if e != nil {
			q.problem(fmt.Sprintf("%s: malformed or unsupported ledger: %v", name, e))
			continue
		}
		if header.ID+".json" != name {
			q.problem(name + ": ledger identity mismatch")
			continue
		}
		p, e := header.QueueProjection()
		if e != nil {
			q.problem(fmt.Sprintf("%s: malformed historical projection: %v", name, e))
			continue
		}
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
