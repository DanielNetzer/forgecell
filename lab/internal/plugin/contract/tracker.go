package contract

import (
	"errors"
	"regexp"
)

// The forgecell.tracker/v1 outputs (design §4.1). Fields tagged text are display
// text, sanitized after decoding; the rest are identifiers, enums or
// timestamps the schema restricts.

// TrackerDescription is the output of tracker_describe.
type TrackerDescription struct {
	DisplayName  string   `json:"displayName" text:"100"`
	KeyPattern   string   `json:"keyPattern"`
	States       []string `json:"states" text:"64"`
	Selectors    []string `json:"selectors"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// check requires keyPattern to be a regular expression Go can run in linear time.
func (d *TrackerDescription) check() error {
	if _, err := regexp.Compile(d.KeyPattern); err != nil {
		return errors.New("keyPattern is not a valid regular expression")
	}
	if d.DisplayName == "" {
		return errors.New("displayName is empty after sanitizing")
	}
	return nil
}

// TrackerItem is one listed Issue.
type TrackerItem struct {
	Key       string   `json:"key"`
	Title     string   `json:"title" text:"300"`
	Labels    []string `json:"labels" text:"100"`
	Milestone *string  `json:"milestone,omitempty" text:"200"`
	UpdatedAt string   `json:"updatedAt"`
	State     string   `json:"state"`
}

// TrackerPage is the output of tracker_list.
type TrackerPage struct {
	Items      []TrackerItem `json:"items"`
	NextCursor *string       `json:"nextCursor,omitempty"`
	Total      *int64        `json:"total,omitempty"`
}

// TrackerIssue is the output of tracker_get, frozen as intake evidence.
type TrackerIssue struct {
	Key         string   `json:"key"`
	Title       string   `json:"title" text:"300"`
	Body        string   `json:"body" text:"65536,multiline"`
	State       string   `json:"state"`
	StateReason *string  `json:"stateReason,omitempty" text:"64"`
	URL         string   `json:"url"`
	UpdatedAt   string   `json:"updatedAt"`
	Labels      []string `json:"labels" text:"100"`
	Milestone   *string  `json:"milestone,omitempty" text:"200"`
}

// TrackerState is the state of one Issue in tracker_states.
type TrackerState struct {
	State       string  `json:"state"`
	StateReason *string `json:"stateReason,omitempty" text:"64"`
	ClosedAt    *string `json:"closedAt,omitempty"`
	ReopenedAt  *string `json:"reopenedAt,omitempty"`
}

// TrackerStates is the output of tracker_states, keyed by Issue key.
type TrackerStates struct {
	States map[string]TrackerState `json:"states"`
}
