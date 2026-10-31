package contract

import "errors"

// The forgecell.forge/v1 outputs (design §4.2). Commits are 40- or
// 64-character lowercase hex and states are closed enums, both enforced by the
// schema; fields tagged text are display text, sanitized after decoding.

// ForgeDescription is the output of forge_describe.
type ForgeDescription struct {
	Identity      string   `json:"identity"`
	Host          string   `json:"host"`
	DisplayName   string   `json:"displayName" text:"100"`
	ChangeNoun    string   `json:"changeNoun" text:"40"`
	DefaultBranch string   `json:"defaultBranch"`
	CIKind        string   `json:"ciKind"`
	WebURL        string   `json:"webURL"`
	Capabilities  []string `json:"capabilities,omitempty"`
}

func (d *ForgeDescription) check() error {
	if d.DisplayName == "" || d.ChangeNoun == "" {
		return errors.New("displayName or changeNoun is empty after sanitizing")
	}
	return nil
}

// ForgePolicy is the output of forge_policy.
type ForgePolicy struct {
	RequiredChecks  []string `json:"requiredChecks" text:"200"`
	RequiredReviews int      `json:"requiredReviews"`
	Protected       bool     `json:"protected"`
}

// ForgeChangeRef is one change request found for a head branch.
type ForgeChangeRef struct {
	Ref        string `json:"ref"`
	URL        string `json:"url"`
	State      string `json:"state"`
	Draft      bool   `json:"draft"`
	HeadCommit string `json:"headCommit"`
}

// ForgeChanges is the output of forge_find_change.
type ForgeChanges struct {
	Changes []ForgeChangeRef `json:"changes"`
}

// ForgePublished is the output of forge_publish_draft. Draft is always true:
// the schema refuses anything else.
type ForgePublished struct {
	Ref        string `json:"ref"`
	URL        string `json:"url"`
	Draft      bool   `json:"draft"`
	HeadCommit string `json:"headCommit"`
}

// ForgeChange is the output of forge_change.
type ForgeChange struct {
	State       string  `json:"state"`
	Draft       bool    `json:"draft"`
	HeadCommit  string  `json:"headCommit"`
	MergedAt    *string `json:"mergedAt,omitempty"`
	MergeCommit *string `json:"mergeCommit,omitempty"`
	URL         string  `json:"url"`
}

// check refuses merge details on a change that is not merged.
func (c *ForgeChange) check() error {
	if c.State != "merged" && (c.MergedAt != nil || c.MergeCommit != nil) {
		return errors.New("mergedAt or mergeCommit is set on a change that is not merged")
	}
	return nil
}

// ForgeCheck is one CI check on a commit.
type ForgeCheck struct {
	Name       string  `json:"name" text:"200"`
	Status     string  `json:"status"`
	Conclusion string  `json:"conclusion"`
	URL        *string `json:"url,omitempty"`
}

// ForgeChecks is the output of forge_checks. Complete is false while the forge
// may still add checks.
type ForgeChecks struct {
	Checks   []ForgeCheck `json:"checks"`
	Complete bool         `json:"complete"`
}

// ForgeComment is one comment on a change request.
type ForgeComment struct {
	ID        string  `json:"id"`
	Author    string  `json:"author" text:"100"`
	IsBot     bool    `json:"isBot"`
	Body      string  `json:"body" text:"65536,multiline"`
	Path      *string `json:"path,omitempty"`
	Line      *int64  `json:"line,omitempty"`
	CreatedAt string  `json:"createdAt"`
}

// ForgeComments is the output of forge_comments.
type ForgeComments struct {
	Comments []ForgeComment `json:"comments"`
}
