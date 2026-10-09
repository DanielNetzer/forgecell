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
type Snapshot struct {
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

func Collect(ctx context.Context, o Options) (Snapshot, error) {
	r := Snapshot{Commit: o.Commit, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: "unknown", Checks: []Check{}}
	if !repository.MatchString(o.Repo) || strings.HasSuffix(o.Repo, "/.") || strings.HasSuffix(o.Repo, "/..") || o.PR < 1 || !commit.MatchString(o.Commit) {
		return r, fmt.Errorf("checks require a repository, positive PR number and exact commit SHA")
	}
	if len(o.Required) == 0 {
		return r, fmt.Errorf("name the required checks explicitly")
	}
	seen := map[string]bool{}
	for _, name := range o.Required {
		if strings.TrimSpace(name) == "" {
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
	response := runner(ctx, process.Options{Argv: []string{"gh", "pr", "view", fmt.Sprint(o.PR), "--repo", o.Repo, "--json", "headRefOid,statusCheckRollup"}, Dir: o.Dir, Stdin: []byte{}, Timeout: 30 * time.Second})
	if !response.OK {
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
	if data.Head != o.Commit {
		r.Status = "stale"
		return r, nil
	}
	if data.Entries == nil {
		r.Detail = "Check data unavailable."
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
				return r, nil
			}
		case "StatusContext":
			c = Check{e.Context, "COMPLETED", e.State, e.TargetURL}
			switch e.State {
			case "PENDING", "EXPECTED":
				c.Status = "IN_PROGRESS"
			case "SUCCESS", "FAILURE", "ERROR":
			default:
				r.Detail = "Unrecognized commit status."
				return r, nil
			}
		default:
			r.Detail = "Unrecognized check data."
			return r, nil
		}
		if c.Name == "" {
			r.Detail = "Check name missing."
			return r, nil
		}
		r.Checks = append(r.Checks, c)
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
