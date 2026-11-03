package conformance

import (
	"encoding/json"
	"fmt"
	"io"
)

// Status is one check's outcome. A skipped check did not run because an earlier
// failure ended the session; it does not pass.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check ids, in the order the kit runs them.
const (
	CheckStart         = "start"
	CheckInitialize    = "initialize"
	CheckToolsList     = "tools-list"
	CheckDescribe      = "describe"
	CheckDeterministic = "describe-deterministic"
	CheckMalformed     = "malformed-input"
	CheckOutputBound   = "output-bound"
	CheckTimeoutBound  = "timeout-bound"
)

var checkOrder = []string{CheckStart, CheckInitialize, CheckToolsList, CheckDescribe, CheckDeterministic, CheckMalformed, CheckOutputBound, CheckTimeoutBound}

// Live check ids, appended after the contract ones in live mode only.
const (
	CheckLiveList     = "live-list"
	CheckLiveGet      = "live-get"
	CheckLiveStates   = "live-states"
	CheckLiveDescribe = "live-describe"
	CheckLivePolicy   = "live-policy"
	CheckLiveChange   = "live-change"
	CheckLivePublish  = "live-publish"
	CheckLiveFindings = "live-findings"
)

// summaries say what each check verifies; they are fixed text.
var summaries = map[string]string{
	CheckStart:         "the manifest decodes, every pinned file matches its digest and the server starts",
	CheckInitialize:    "initialize succeeds and declares the manifest's contract",
	CheckToolsList:     "tools/list covers every required contract tool with a compatible input schema",
	CheckDescribe:      "the describe tool's output decodes strictly",
	CheckDeterministic: "a second describe call returns the same output",
	CheckMalformed:     "malformed input to every read tool returns an MCP error and the server keeps serving",
	CheckOutputBound:   "every response fit its tool's output bound",
	CheckTimeoutBound:  "every call answered within its tool's timeout",
	CheckLiveList:      "tracker_list pages with unique keys matching keyPattern and no repeated cursor",
	CheckLiveGet:       "tracker_get returns each listed key with a described state",
	CheckLiveStates:    "tracker_states agrees with tracker_get for the listed keys",
	CheckLiveDescribe:  "forge_describe succeeds for the target project",
	CheckLivePolicy:    "forge_policy succeeds for the target's default branch",
	CheckLiveChange:    "forge_change, forge_checks and forge_comments decode for the named change",
	CheckLivePublish:   "forge_publish_draft opens a draft at the commit that forge_change and forge_find_change report",
	CheckLiveFindings:  "reviewer_findings decodes and two calls return the same findings",
}

// Result is one check in a report. Reason is a fixed reason, empty on pass.
type Result struct {
	ID      string `json:"id"`
	Status  Status `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Summary string `json:"summary"`
}

// Report is the outcome of one contract-mode or live run. It holds only fixed
// text, check ids, the plugin name, the manifest digest and a live run's
// cleaned target: never local paths, environment values, config values or
// plugin stderr.
type Report struct {
	SchemaVersion  string `json:"schemaVersion"`
	Mode           string `json:"mode"`
	Manifest       string `json:"manifest"`
	Plugin         string `json:"plugin,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Contract       string `json:"contract,omitempty"`
	ManifestSHA256 string `json:"manifestSha256,omitempty"`
	// Target is the cleaned --target of a live run, empty in contract mode.
	Target string   `json:"target,omitempty"`
	Passed bool     `json:"passed"`
	Checks []Result `json:"checks"`
	// Cleanup is the fixed reason "run-directory-left" when the plugin's
	// private run directory could not be removed after the run; the report
	// then fails.
	Cleanup string `json:"cleanup,omitempty"`
}

// cleanupLeft is Report.Cleanup when the run directory was left in place.
const cleanupLeft = "run-directory-left"

// Failed returns the first check that did not pass, if any.
func (r Report) Failed() (Result, bool) {
	for _, c := range r.Checks {
		if c.Status != Pass {
			return c, true
		}
	}
	return Result{}, false
}

// WriteText renders the report as a pass/fail list.
func (r Report) WriteText(w io.Writer) error {
	name := r.Plugin
	if name == "" {
		name = "(unreadable manifest)"
	}
	fmt.Fprintf(w, "Plugin %s · manifest %s\n", name, r.Manifest)
	if r.Kind != "" {
		fmt.Fprintf(w, "Kind %s · contract %s · manifest SHA-256 %s\n", r.Kind, r.Contract, r.ManifestSHA256)
	}
	passed := 0
	for _, c := range r.Checks {
		line := fmt.Sprintf("%-4s %s", map[Status]string{Pass: "PASS", Fail: "FAIL", Skip: "SKIP"}[c.Status], c.ID)
		if c.Reason != "" {
			line += ": " + c.Reason
		}
		fmt.Fprintf(w, "%s — %s\n", line, c.Summary)
		if c.Status == Pass {
			passed++
		}
	}
	if r.Cleanup != "" {
		fmt.Fprintf(w, "FAIL cleanup: %s — the plugin's private run directory could not be removed\n", r.Cleanup)
	}
	verdict := "PASS"
	if !r.Passed {
		verdict = "FAIL"
	}
	if r.Mode == modeLive {
		_, err := fmt.Fprintf(w, "Result: %s (%d of %d checks passed; live mode, target %s)\n", verdict, passed, len(r.Checks), r.Target)
		return err
	}
	_, err := fmt.Fprintf(w, "Result: %s (%d of %d checks passed; contract mode, offline)\n", verdict, passed, len(r.Checks))
	return err
}

// WriteJSON renders the report as one JSON document.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
