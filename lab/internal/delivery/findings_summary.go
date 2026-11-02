package delivery

import "github.com/DanielNetzer/forgecell/lab/internal/reviewer"

// SummaryFinding is one finding of a summary, with its reviewer's plugin and
// identity, and whether it is at or above that reviewer's autoFix severity.
type SummaryFinding struct {
	Plugin   string `json:"plugin"`
	Reviewer string `json:"reviewer"`
	Finding
	AutoFix bool `json:"autoFix"`
}

// SeverityGroup is the open findings of one severity.
type SeverityGroup struct {
	Severity string           `json:"severity"`
	Findings []SummaryFinding `json:"findings"`
}

// FindingsSummary is the open findings of the latest complete round, grouped
// by severity, most severe first. Round is that round's index, or -1 when no
// round is complete. Resolved holds the findings the reviewer marked resolved
// and those inferred resolved. Pending and Failed name, by reviewer identity,
// that round's reviewers without a complete answer; NewerIncomplete is set
// when a later round is incomplete, so it was not summarized. Dismissed counts
// that round's open findings left out because the operator dismissed them.
type FindingsSummary struct {
	Round           int              `json:"round"`
	Commit          string           `json:"commit,omitempty"`
	Open            []SeverityGroup  `json:"open"`
	Resolved        []SummaryFinding `json:"resolved"`
	Pending         []string         `json:"pending"`
	Failed          []string         `json:"failed"`
	NewerIncomplete bool             `json:"newerIncomplete,omitempty"`
	Dismissed       int              `json:"dismissed,omitempty"`
}

// OpenCount is the number of open findings.
func (s FindingsSummary) OpenCount() int {
	n := 0
	for _, g := range s.Open {
		n += len(g.Findings)
	}
	return n
}

// Summarize lists the open findings of the record's latest complete round.
// autoFix maps each reviewer identity (ReviewerKey: Formula index and plugin)
// to its Formula autoFix level (high, medium, low or none); a finding at or
// above it is marked. A finding of an earlier complete round on another commit
// is inferred resolved when the later round's complete answer from the same
// reviewer identity no longer holds its fingerprint. An incomplete round never
// infers resolution. A dismissed open finding is left out and counted.
func Summarize(record FindingsRecord, autoFix map[string]string) FindingsSummary {
	s := FindingsSummary{Round: -1, Open: []SeverityGroup{}, Resolved: []SummaryFinding{}, Pending: []string{}, Failed: []string{}}
	for i := len(record.Rounds) - 1; i >= 0; i-- {
		if record.Rounds[i].Complete {
			s.Round = i
			break
		}
		s.NewerIncomplete = true
	}
	if s.Round < 0 {
		return s
	}
	latest := record.Rounds[s.Round]
	s.Commit = latest.Commit
	mark := func(entry ReviewerRound, f Finding) SummaryFinding {
		level, ok := severityRank[autoFix[entry.Reviewer]]
		return SummaryFinding{Plugin: entry.Plugin, Reviewer: entry.Reviewer, Finding: f, AutoFix: ok && severityRank[f.Severity] >= level}
	}
	dismissed := record.DismissedSet()
	groups := map[string][]SummaryFinding{}
	present := map[string]bool{}
	answered := map[string]bool{}
	for _, entry := range latest.Reviewers {
		switch entry.Status {
		case FindingsPending:
			s.Pending = append(s.Pending, entry.Reviewer)
		case FindingsError:
			s.Failed = append(s.Failed, entry.Reviewer)
		case FindingsComplete:
			answered[entry.Reviewer] = !entry.Truncated
		}
		for _, f := range entry.Findings {
			present[entry.Reviewer+"\x00"+f.Fingerprint] = true
			if f.Resolved {
				s.Resolved = append(s.Resolved, mark(entry, f))
				continue
			}
			if dismissed[f.Fingerprint] {
				s.Dismissed++
				continue
			}
			groups[f.Severity] = append(groups[f.Severity], mark(entry, f))
		}
	}
	for _, severity := range reviewer.Severities {
		if len(groups[severity]) > 0 {
			s.Open = append(s.Open, SeverityGroup{Severity: severity, Findings: groups[severity]})
		}
	}
	seen := map[string]bool{}
	for i := s.Round - 1; i >= 0; i-- {
		earlier := record.Rounds[i]
		if !earlier.Complete || earlier.Commit == latest.Commit {
			continue
		}
		for _, entry := range earlier.Reviewers {
			if !answered[entry.Reviewer] {
				continue
			}
			for _, f := range entry.Findings {
				key := entry.Reviewer + "\x00" + f.Fingerprint
				if present[key] || seen[key] || f.Resolved {
					continue
				}
				seen[key] = true
				f.Resolved = true
				s.Resolved = append(s.Resolved, mark(entry, f))
			}
		}
	}
	return s
}
