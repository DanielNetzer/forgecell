// Package intake resolves ticket identity before a Molecule can execute.
package intake

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type IssueRef struct {
	Number int64  `json:"number"`
	Owner  string `json:"owner,omitempty"`
	Repo   string `json:"repo,omitempty"`
}

var urlPattern = regexp.MustCompile(`(?i)^https?://github\.com/([a-z0-9_.-]+)/([a-z0-9_.-]+)/issues/([0-9]+)(?:[/?#].*)?$`)
var slugPattern = regexp.MustCompile(`^([a-zA-Z0-9_.-]+)/([a-zA-Z0-9_.-]+)#([0-9]+)$`)
var numberPattern = regexp.MustCompile(`^#?([0-9]+)$`)
var repoPattern = regexp.MustCompile(`^([a-zA-Z0-9_.-]+)/([a-zA-Z0-9_.-]+)$`)

func ParseIssueRef(input string) (IssueRef, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return IssueRef{}, fmt.Errorf("issue reference is empty")
	}
	var ref IssueRef
	var digits string
	if m := urlPattern.FindStringSubmatch(raw); m != nil {
		ref.Owner, ref.Repo, digits = m[1], m[2], m[3]
	} else if m := slugPattern.FindStringSubmatch(raw); m != nil {
		ref.Owner, ref.Repo, digits = m[1], m[2], m[3]
	} else if m := numberPattern.FindStringSubmatch(raw); m != nil {
		digits = m[1]
	} else {
		return ref, fmt.Errorf("invalid issue reference: use 123, #123, owner/repo#123, or a GitHub issue URL")
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	// Preserve exact numbers in existing JSON/JavaScript readers as well as Go.
	if err != nil || n < 1 || n > 9007199254740991 {
		return IssueRef{}, fmt.Errorf("issue number must be a positive safe integer")
	}
	if ref.Owner == "." || ref.Owner == ".." || ref.Repo == "." || ref.Repo == ".." {
		return IssueRef{}, fmt.Errorf("invalid repository")
	}
	ref.Number = n
	return ref, nil
}

// BindRepository refuses accidental cross-repository execution instead of guessing.
func BindRepository(ref IssueRef, repository string) (IssueRef, error) {
	m := repoPattern.FindStringSubmatch(repository)
	if m == nil || m[1] == "." || m[1] == ".." || m[2] == "." || m[2] == ".." {
		return IssueRef{}, fmt.Errorf("invalid repository binding")
	}
	if ref.Number < 1 || ref.Number > 9007199254740991 {
		return IssueRef{}, fmt.Errorf("invalid issue number")
	}
	if ref.Owner != "" && !strings.EqualFold(ref.Owner, m[1]) || ref.Repo != "" && !strings.EqualFold(ref.Repo, m[2]) {
		return IssueRef{}, fmt.Errorf("issue repository does not match the approved Formula")
	}
	ref.Owner, ref.Repo = m[1], m[2]
	return ref, nil
}
