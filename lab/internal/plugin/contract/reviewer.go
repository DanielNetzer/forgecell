package contract

import "errors"

// The forgecell.reviewer/v1 outputs (design §4.3). Findings are untrusted
// evidence: they never approve, and their text is sanitized after decoding.

// ReviewerDescription is the output of reviewer_describe.
type ReviewerDescription struct {
	DisplayName  string   `json:"displayName" text:"100"`
	Severities   []string `json:"severities"`
	Capabilities []string `json:"capabilities,omitempty"`
}

func (d *ReviewerDescription) check() error {
	if d.DisplayName == "" {
		return errors.New("displayName is empty after sanitizing")
	}
	return nil
}

// ReviewerFinding is one finding against a pushed commit.
type ReviewerFinding struct {
	ID       string  `json:"id"`
	Severity string  `json:"severity"`
	Path     *string `json:"path,omitempty"`
	Line     *int64  `json:"line,omitempty"`
	Title    string  `json:"title" text:"300"`
	Body     string  `json:"body" text:"65536,multiline"`
	Resolved bool    `json:"resolved"`
}

// ReviewerFindings is the output of reviewer_findings. Status pending means the
// reviewer has not finished with the commit.
type ReviewerFindings struct {
	Status   string            `json:"status"`
	Findings []ReviewerFinding `json:"findings"`
}
