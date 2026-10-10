// Package checks records exact-commit CI evidence. A snapshot never authorizes a merge.
package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

type Options struct {
	Repo     string
	PR       int
	Commit   string
	Required []string
	Dir      string
	Runner   func(context.Context, process.Options) process.Result
}
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url,omitempty"`
}

const MaxSnapshotBytes = 256_000
const MaxChecks = 512

type Snapshot struct {
	Repo         string   `json:"repo"`
	PR           int      `json:"pr"`
	Commit       string   `json:"commit"`
	ObservedHead string   `json:"observedHead,omitempty"`
	CheckedAt    string   `json:"checkedAt"`
	Required     []string `json:"required"`
	Status       string   `json:"status"`
	Checks       []Check  `json:"checks"`
	Detail       string   `json:"detail,omitempty"`
}

var repository = regexp.MustCompile(`^[A-Za-z0-9_-]+/[A-Za-z0-9_.-]+$`)
var commit = regexp.MustCompile(`^[a-f0-9]{40}$`)

func Collect(ctx context.Context, o Options) (r Snapshot, err error) {
	defer func() {
		raw, _ := json.Marshal(r)
		if err == nil && len(raw) > MaxSnapshotBytes {
			r.Status = "unknown"
			r.Checks = []Check{}
			r.Detail = "Snapshot exceeds bound; check evidence unavailable."
		}
	}()
	r = Snapshot{Repo: o.Repo, PR: o.PR, Commit: o.Commit, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: "unknown", Checks: []Check{}}
	if len(o.Repo) > 256 || !repository.MatchString(o.Repo) || strings.HasSuffix(o.Repo, "/.") || strings.HasSuffix(o.Repo, "/..") || o.PR < 1 || !commit.MatchString(o.Commit) {
		return r, fmt.Errorf("checks require a repository, positive PR number and exact commit SHA")
	}
	if len(o.Required) == 0 || len(o.Required) > 128 {
		return r, fmt.Errorf("name the required checks explicitly")
	}
	seen := map[string]bool{}
	for _, name := range o.Required {
		if strings.TrimSpace(name) == "" || len(name) > 256 {
			return r, fmt.Errorf("required check names cannot be blank")
		}
		if !seen[name] {
			r.Required = append(r.Required, name)
			seen[name] = true
		}
	}
	runner := o.Runner
	if runner == nil {
		runner = process.Run
	}
	response := runner(ctx, process.Options{Argv: []string{"gh", "pr", "view", fmt.Sprint(o.PR), "--repo", o.Repo, "--json", "headRefOid,statusCheckRollup"}, Dir: o.Dir, Stdin: []byte{}, Timeout: 30 * time.Second, MaxOutputBytes: 1_000_000})
	if !response.OK || response.Overflow || response.TimedOut || response.Interrupted || len(response.Stdout)+len(response.Stderr) > 1_000_000 {
		r.Detail = "GitHub check lookup failed. No passing evidence recorded."
		return r, nil
	}
	var data struct {
		Head    string `json:"headRefOid"`
		Entries []struct {
			Type       string `json:"__typename"`
			Name       string `json:"name"`
			Context    string `json:"context"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
			DetailsURL string `json:"detailsUrl"`
			TargetURL  string `json:"targetUrl"`
		} `json:"statusCheckRollup"`
	}
	if json.Unmarshal([]byte(response.Stdout), &data) != nil || !commit.MatchString(data.Head) {
		r.Detail = "Invalid GitHub check response."
		return r, nil
	}
	r.ObservedHead = data.Head

	if data.Entries == nil {
		r.Detail = "Check data unavailable."
		return r, nil
	}
	if len(data.Entries) > MaxChecks {
		r.Detail = "Check count exceeds bound."
		return r, nil
	}
	for _, e := range data.Entries {
		c := Check{}
		switch e.Type {
		case "CheckRun":
			c = Check{e.Name, e.Status, e.Conclusion, e.DetailsURL}
			switch c.Status {
			case "QUEUED", "IN_PROGRESS", "WAITING", "PENDING", "REQUESTED", "COMPLETED":
			default:
				r.Detail = "Unrecognized check status."
			}
		case "StatusContext":
			c = Check{e.Context, "COMPLETED", e.State, e.TargetURL}
			switch e.State {
			case "PENDING", "EXPECTED":
				c.Status = "IN_PROGRESS"
			case "SUCCESS", "FAILURE", "ERROR":
			default:
				r.Detail = "Unrecognized commit status."
			}
		default:
			r.Detail = "Unrecognized check data."
			c = Check{e.Name, e.Status, e.Conclusion, e.DetailsURL}
			if c.Name == "" {
				c.Name = e.Context
			}
			if c.URL == "" {
				c.URL = e.TargetURL
			}
		}
		if c.Name == "" || len(c.Name) > 256 || len(c.Status) > 64 || len(c.Conclusion) > 64 || len(c.URL) > 2048 {
			r.Detail = "Check fields missing or exceed bounds."
			return r, nil
		}
		r.Checks = append(r.Checks, c)
	}
	raw, _ := json.Marshal(r)
	if len(raw) > MaxSnapshotBytes {
		r.Checks = []Check{}
		r.Detail = "Snapshot exceeds bound."
		return r, nil
	}
	if r.Detail != "" {
		return r, nil
	}
	if data.Head != o.Commit {
		r.Status = "stale"
		return r, nil
	}
	present := map[string]bool{}
	pending, failed := false, false
	for _, c := range r.Checks {
		present[c.Name] = true
		if c.Status != "COMPLETED" {
			pending = true
		} else if c.Conclusion != "SUCCESS" {
			failed = true
		}
	}
	for _, name := range r.Required {
		if !present[name] {
			r.Status = "missing"
			return r, nil
		}
	}
	switch {
	case failed:
		r.Status = "failed"
	case pending:
		r.Status = "pending"
	default:
		r.Status = "passed"
	}
	return r, nil
}

// Validate rejects malformed or falsely passing persisted snapshots. Unknown retains
// partial evidence without promoting it; collection uncertainty cannot prove CI success.
func Validate(s Snapshot) error {
	if len(s.Repo) > 256 || !repository.MatchString(s.Repo) || strings.HasSuffix(s.Repo, "/.") || strings.HasSuffix(s.Repo, "/..") || s.PR < 1 || !commit.MatchString(s.Commit) {
		return fmt.Errorf("invalid CI identity")
	}
	if _, err := time.Parse(time.RFC3339Nano, s.CheckedAt); err != nil {
		return fmt.Errorf("invalid CI observation time")
	}
	if len(s.Required) == 0 || len(s.Required) > 128 || len(s.Checks) > MaxChecks || len(s.Detail) > 2048 {
		return fmt.Errorf("invalid CI bounds")
	}
	raw, err := json.Marshal(s)
	if err != nil || len(raw) > MaxSnapshotBytes {
		return fmt.Errorf("CI snapshot exceeds bound")
	}
	required := map[string]bool{}
	for _, name := range s.Required {
		if strings.TrimSpace(name) == "" || len(name) > 256 || required[name] {
			return fmt.Errorf("invalid CI required names")
		}
		required[name] = true
	}
	if s.ObservedHead != "" && !commit.MatchString(s.ObservedHead) {
		return fmt.Errorf("invalid observed head")
	}
	present := map[string]bool{}
	failed, pending := false, false
	for _, c := range s.Checks {
		if c.Name == "" || len(c.Name) > 256 || len(c.Status) > 64 || len(c.Conclusion) > 64 || len(c.URL) > 2048 {
			return fmt.Errorf("invalid CI check bounds")
		}
		present[c.Name] = true
		switch c.Status {
		case "COMPLETED":
			if c.Conclusion != "SUCCESS" {
				failed = true
			}
		case "QUEUED", "IN_PROGRESS", "WAITING", "PENDING", "REQUESTED":
			pending = true
		default:
			if s.Status != "unknown" {
				return fmt.Errorf("unknown CI check status")
			}
		}
	}
	if s.Status == "unknown" {
		return nil
	}
	if s.Detail != "" {
		return fmt.Errorf("CI collection uncertainty requires unknown status")
	}
	if s.ObservedHead == "" {
		return fmt.Errorf("CI status requires observed head")
	}
	expected := "passed"
	switch {
	case s.ObservedHead != s.Commit:
		expected = "stale"
	case failed:
		expected = "failed"
	case pending:
		expected = "pending"
	}
	if expected != "stale" {
		for name := range required {
			if !present[name] {
				expected = "missing"
			}
		}
	}
	if s.Status != expected {
		return fmt.Errorf("CI aggregate does not match exact evidence")
	}
	return nil
}
