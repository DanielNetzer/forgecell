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
}

// Result is one check in a report. Reason is a fixed reason, empty on pass.
type Result struct {
	ID      string `json:"id"`
	Status  Status `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Summary string `json:"summary"`
}

// Report is the outcome of one contract-mode run. It holds only fixed text,
// check ids, the plugin name and the manifest digest: never local paths,
// environment values or plugin stderr.
type Report struct {
	SchemaVersion  string   `json:"schemaVersion"`
	Mode           string   `json:"mode"`
	Manifest       string   `json:"manifest"`
	Plugin         string   `json:"plugin,omitempty"`
	Kind           string   `json:"kind,omitempty"`
	Contract       string   `json:"contract,omitempty"`
	ManifestSHA256 string   `json:"manifestSha256,omitempty"`
	Passed         bool     `json:"passed"`
	Checks         []Result `json:"checks"`
}

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
	verdict := "PASS"
	if !r.Passed {
		verdict = "FAIL"
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
