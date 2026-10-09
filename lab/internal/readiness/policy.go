package readiness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"net/url"
	"strings"
	"time"
)

type PolicyObservation struct {
	Source      string          `json:"source"`
	RetrievedAt string          `json:"retrievedAt"`
	Status      string          `json:"status"`
	SHA256      string          `json:"sha256,omitempty"`
	Content     json.RawMessage `json:"content,omitempty"`
	Unknown     string          `json:"unknown,omitempty"`
}
type RemotePolicy struct {
	Rules      PolicyObservation `json:"rulesets"`
	Protection PolicyObservation `json:"branchProtection"`
}

// A successful read is an observation, not proof of compliance. An unreadable
// policy (including HTTP 404) remains unknown, never an empty protection policy.
func ReadRemotePolicy(ctx context.Context, repo, branch, dir string, runner func(context.Context, process.Options) process.Result) RemotePolicy {
	if runner == nil {
		runner = process.Run
	}
	read := func(endpoint string, array bool) PolicyObservation {
		o := PolicyObservation{Source: "https://api.github.com/" + endpoint, RetrievedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: "unknown"}
		r := runner(ctx, process.Options{Argv: []string{"gh", "api", "--method", "GET", endpoint}, Dir: dir, Stdin: []byte{}, Timeout: 15 * time.Second, MaxOutputBytes: 256000})
		if !r.OK || r.Overflow || r.TimedOut || r.Interrupted {
			o.Unknown = "Policy unavailable, unauthorized, timed out or exceeded bounds. No absence of protection is inferred."
			return o
		}
		var data any
		if json.Unmarshal([]byte(r.Stdout), &data) != nil {
			o.Unknown = "Invalid policy response"
			return o
		}
		if array {
			if list, ok := data.([]any); !ok || len(list) >= 100 {
				o.Unknown = "Expected a complete bounded rule array (100 or more rules requires explicit pagination support)"
				return o
			}
		} else {
			if _, ok := data.(map[string]any); !ok {
				o.Unknown = "Expected branch protection object"
				return o
			}
		}
		raw, _ := json.Marshal(data)
		o.Content = raw
		o.Status = "observed"
		o.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
		return o
	}
	prefix := "repos/" + repo
	return RemotePolicy{Rules: read(prefix+"/rules/branches/"+url.PathEscape(branch)+"?per_page=100", true), Protection: read(prefix+"/branches/"+url.PathEscape(branch)+"/protection", false)}
}

// Pinning a reviewed observation is the only v1 remote-policy requirement.
// It is deliberately distinct from claiming that all rules have been satisfied.
func (p RemotePolicy) ValidateRequirements(requirements []string) error {
	for _, requirement := range requirements {
		kind, digest, ok := strings.Cut(requirement, ":")
		if !ok || len(digest) != 64 {
			return fmt.Errorf("unsupported mandatory constraint: %s", requirement)
		}
		var o PolicyObservation
		switch kind {
		case "github-ruleset-sha256":
			o = p.Rules
		case "github-protection-sha256":
			o = p.Protection
		default:
			return fmt.Errorf("unsupported mandatory constraint: %s", kind)
		}
		if o.Status != "observed" || o.SHA256 != digest {
			return fmt.Errorf("configured publication requirement cannot be verified or changed: %s", kind)
		}
	}
	return nil
}
