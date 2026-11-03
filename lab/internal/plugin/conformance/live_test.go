package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

const liveCommit = "0123456789abcdef0123456789abcdef01234567"

// recordedCall is one tools/call the fake server received with valid arguments.
type recordedCall struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// runLive runs live mode against the fake in mode, recording every call it
// receives, and returns the report and the recorded calls.
func runLive(t *testing.T, kind, mode string, live Live) (Report, []recordedCall) {
	t.Helper()
	server := plugintest.Server(t)
	record := filepath.Join(plugintest.TempDir(t), "calls.jsonl")
	path := plugintest.WriteManifest(t, plugintest.Manifest(t, server, kind, mode, "FAKE_RECORD"))
	root := plugintest.TempDir(t)
	opts := testOptions(root, mode)
	opts.LookupEnv = func(name string) (string, bool) {
		if name == "FAKE_RECORD" {
			return record, true
		}
		return "", false
	}
	r := RunLive(context.Background(), path, opts, live)
	if left, err := os.ReadDir(root); err != nil || len(left) != 0 {
		t.Fatalf("the run left %d entries behind (%v)", len(left), err)
	}
	return r, readCalls(t, record)
}

func readCalls(t *testing.T, path string) []recordedCall {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var calls []recordedCall
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var c recordedCall
		if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return calls
}

func countCalls(calls []recordedCall, tool string) int {
	n := 0
	for _, c := range calls {
		if c.Tool == tool {
			n++
		}
	}
	return n
}

// liveChecks returns the report's checks after the contract ones, failing the
// test unless every contract check passed and the live ids are want.
func liveChecks(t *testing.T, r Report, want ...string) []Result {
	t.Helper()
	if r.Mode != modeLive || len(r.Checks) != len(checkOrder)+len(want) {
		t.Fatalf("report = %+v", r)
	}
	for i, id := range checkOrder {
		if r.Checks[i].ID != id || r.Checks[i].Status != Pass {
			t.Fatalf("contract check %d = %+v", i, r.Checks[i])
		}
	}
	live := r.Checks[len(checkOrder):]
	for i, id := range want {
		if live[i].ID != id {
			t.Fatalf("live check %d = %+v, want %s", i, live[i], id)
		}
	}
	return live
}

func requireAll(t *testing.T, checks []Result, status Status, reason string) {
	t.Helper()
	for _, c := range checks {
		if c.Status != status || c.Reason != reason {
			t.Fatalf("%s = %+v, want %s %q", c.ID, c, status, reason)
		}
	}
}

func TestLiveTrackerPassesAgainstAWellBehavedFake(t *testing.T) {
	config := map[string]any{"board": "FAKE"}
	r, calls := runLive(t, "tracker", "live", Live{Target: "FAKE", Config: config})
	requireAll(t, liveChecks(t, r, CheckLiveList, CheckLiveGet, CheckLiveStates), Pass, "")
	if !r.Passed || r.Target != "FAKE" {
		t.Fatalf("report = %+v", r)
	}
	if countCalls(calls, "tracker_list") != 2 || countCalls(calls, "tracker_get") != 4 || countCalls(calls, "tracker_states") != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	for _, c := range calls {
		if c.Tool == "tracker_describe" {
			continue
		}
		var args struct {
			Config   map[string]any `json:"config"`
			Selector string         `json:"selector"`
			Limit    int            `json:"limit"`
		}
		if err := json.Unmarshal(c.Arguments, &args); err != nil || args.Config["board"] != "FAKE" || len(args.Config) != 1 {
			t.Fatalf("%s arguments = %s", c.Tool, c.Arguments)
		}
		if c.Tool == "tracker_list" && (args.Selector != "all" || args.Limit != liveListLimit) {
			t.Fatalf("tracker_list arguments = %s", c.Arguments)
		}
	}
}

func TestLiveTrackerFailures(t *testing.T) {
	for _, c := range []struct{ mode, check string }{
		{"live-repeat-key", CheckLiveList},
		{"live-repeat-cursor", CheckLiveList},
		{"live-bad-key", CheckLiveList},
		{"live-bad-state", CheckLiveGet},
		{"live-states-disagree", CheckLiveStates},
	} {
		r, _ := runLive(t, "tracker", c.mode, Live{Target: "FAKE"})
		live := liveChecks(t, r, CheckLiveList, CheckLiveGet, CheckLiveStates)
		failed, ok := r.Failed()
		if r.Passed || !ok || failed.ID != c.check || failed.Status != Fail || failed.Reason != "failed" {
			t.Fatalf("%s: first failure %+v, want %s", c.mode, failed, c.check)
		}
		for i, check := range live {
			if check.ID == c.check {
				requireAll(t, live[:i], Pass, "")
				requireAll(t, live[i+1:], Skip, "")
			}
		}
	}
}

func TestLiveTrackerListStopsAfterFivePages(t *testing.T) {
	r, calls := runLive(t, "tracker", "live-endless", Live{Target: "FAKE"})
	requireAll(t, liveChecks(t, r, CheckLiveList, CheckLiveGet, CheckLiveStates), Pass, "")
	if countCalls(calls, "tracker_list") != liveListPages || countCalls(calls, "tracker_get") != liveGetKeys {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestLiveCallBoundFailsTheCurrentCheckAsIncomplete(t *testing.T) {
	previous := maxLiveCalls
	// Contract mode makes 11 tracker calls; the live describe and two pages
	// reach the bound before the third page.
	maxLiveCalls = 14
	t.Cleanup(func() { maxLiveCalls = previous })
	r, calls := runLive(t, "tracker", "live-endless", Live{Target: "FAKE"})
	live := liveChecks(t, r, CheckLiveList, CheckLiveGet, CheckLiveStates)
	if r.Passed || live[0].Status != Fail || live[0].Reason != string(reasonIncomplete) {
		t.Fatalf("live-list = %+v", live[0])
	}
	requireAll(t, live[1:], Skip, "")
	if countCalls(calls, "tracker_list") != 2 || countCalls(calls, "tracker_get") != 0 {
		t.Fatalf("calls = %+v", calls)
	}
}

// liveConfig runs the live-config tracker fake scripted by config and returns
// its live checks.
func liveConfig(t *testing.T, config map[string]any) ([]Result, []recordedCall) {
	t.Helper()
	r, calls := runLive(t, "tracker", "live-config", Live{Target: "FAKE", Config: config})
	live := liveChecks(t, r, CheckLiveList, CheckLiveGet, CheckLiveStates)
	if passed := live[0].Status == Pass && live[1].Status == Pass && live[2].Status == Pass; r.Passed != passed {
		t.Fatalf("report = %+v", r)
	}
	return live, calls
}

func TestLiveKeysMatchTheAnchoredKeyPattern(t *testing.T) {
	const unanchored = "[A-Z]+-[0-9]+"
	live, _ := liveConfig(t, map[string]any{"keyPattern": unanchored, "keys": []any{"ABC-1"}})
	requireAll(t, live, Pass, "")
	live, calls := liveConfig(t, map[string]any{"keyPattern": unanchored, "keys": []any{"junk ABC-1 junk"}})
	requireAll(t, live[:1], Fail, "failed")
	requireAll(t, live[1:], Skip, "")
	if countCalls(calls, "tracker_get") != 0 {
		t.Fatalf("calls = %+v", calls)
	}
	live, _ = liveConfig(t, map[string]any{"keyPattern": unanchored, "keys": []any{"ABC-1"}, "getKey": "junk ABC-1 junk"})
	requireAll(t, live[:1], Pass, "")
	requireAll(t, live[1:2], Fail, "failed")
	// A pattern that compiles only once wrapped cannot escape the anchors.
	live, calls = liveConfig(t, map[string]any{"keyPattern": "FAKE-1)|(.*", "keys": []any{"junk"}})
	if live[0].Status != Fail || countCalls(calls, "tracker_list") != 0 {
		t.Fatalf("live-list = %+v, calls %+v", live[0], calls)
	}
	if _, err := anchoredKeyPattern("^x)|(y$"); err == nil {
		t.Fatal("a pattern that compiles only once wrapped was admitted")
	}
	for _, c := range []struct {
		pattern, key string
		ok           bool
	}{
		{unanchored, "ABC-1", true},
		{unanchored, "junk ABC-1 junk", false},
		{"^A|B$", "AB", false},
		{"^FAKE-[0-9]+$", "FAKE-12", true},
	} {
		pattern, err := anchoredKeyPattern(c.pattern)
		if err != nil || (checkKey("tracker_list", pattern, c.key) == nil) != c.ok {
			t.Fatalf("%q against %q: ok = %v, want %v (%v)", c.key, c.pattern, !c.ok, c.ok, err)
		}
	}
}

func TestLiveKeysPassTheKeyTextRules(t *testing.T) {
	for _, key := range []string{"FAKE-1‮", "FAKE-1\nFAKE-2"} {
		live, calls := liveConfig(t, map[string]any{"keyPattern": "(?s).+", "keys": []any{key}})
		if live[0].Status != Fail || countCalls(calls, "tracker_get") != 0 {
			t.Fatalf("%q: live-list = %+v, calls %+v", key, live[0], calls)
		}
	}
	// The kit's own rule, apart from the output schema the client applies.
	pattern, _ := anchoredKeyPattern("(?s).+")
	for _, key := range []string{"FAKE-1‮", "FAKE-1\nx", "‏FAKE", strings.Repeat("K", 129)} {
		if checkKey("tracker_list", pattern, key) == nil {
			t.Fatalf("%q passed the key text rules", key)
		}
	}
	if checkKey("tracker_list", pattern, strings.Repeat("K", 128)) != nil {
		t.Fatal("a 128-rune key failed")
	}
}

func TestLiveStatesAreDescribedStates(t *testing.T) {
	// The tracker describes only "open"; "closed" is a contract state it omits.
	live, _ := liveConfig(t, map[string]any{"onlyState": "open", "keys": []any{"FAKE-1"}, "itemState": "closed"})
	requireAll(t, live[:1], Fail, "failed")
	live, _ = liveConfig(t, map[string]any{"onlyState": "open", "keys": []any{"FAKE-1"}, "statesState": "closed"})
	requireAll(t, live[:2], Pass, "")
	requireAll(t, live[2:], Fail, "failed")
}

func TestLiveCallsBreakingABoundFailTheBoundChecks(t *testing.T) {
	previous := liveTimeout
	t.Cleanup(func() { liveTimeout = previous })
	for _, c := range []struct{ mode, bound, reason string }{
		{"live-oversized-list", CheckOutputBound, "output-too-large"},
		{"live-slow-list", CheckTimeoutBound, "timeout"},
	} {
		liveTimeout = 0
		if c.mode == "live-slow-list" {
			// The slow list never answers, so any timeout gives the same outcome.
			liveTimeout = 10 * time.Millisecond
		}
		r, _ := runLive(t, "tracker", c.mode, Live{Target: "FAKE"})
		byID := map[string]Result{}
		for _, check := range r.Checks {
			byID[check.ID] = check
		}
		if r.Passed || byID[CheckLiveList].Status != Fail || byID[CheckLiveList].Reason != c.reason {
			t.Fatalf("%s: live-list = %+v", c.mode, byID[CheckLiveList])
		}
		if byID[c.bound].Status != Fail || byID[c.bound].Reason != c.reason {
			t.Fatalf("%s: %s = %+v", c.mode, c.bound, byID[c.bound])
		}
	}
}

func TestLivePublishThatOpenedADraftSaysCloseItByHand(t *testing.T) {
	write := Live{Target: "example/project", Commit: liveCommit, AllowWrite: true, Head: "forgecell/check", Base: "main"}
	for _, c := range []struct {
		mode    string
		status  Status
		reason  string
		summary string
	}{
		{"live", Pass, "", summaries[CheckLivePublish]},
		{"live-not-draft", Fail, "failed", livePublishOpened},
		{"live-wrong-head", Fail, "failed", livePublishOpened},
		{"live-change-rate-limited", Skip, "rate-limited", livePublishOpened},
		// Rate limited before the publish: no draft was opened.
		{"live-rate-limited", Skip, "rate-limited", summaries[CheckLivePublish]},
	} {
		r, calls := runLive(t, "forge", c.mode, write)
		checks := liveChecks(t, r, CheckLiveDescribe, CheckLivePolicy, CheckLivePublish)
		if got := checks[2]; got.Status != c.status || got.Reason != c.reason || got.Summary != c.summary {
			t.Fatalf("%s: live-publish = %+v", c.mode, got)
		}
		var text bytes.Buffer
		if err := r.WriteText(&text); err != nil || strings.Contains(text.String(), "!1") {
			t.Fatalf("%s: text report names a ref (%v):\n%s", c.mode, err, text.String())
		}
		if c.mode == "live-change-rate-limited" && calls[len(calls)-1].Tool != "forge_change" {
			t.Fatalf("calls after the rate limit = %+v", calls)
		}
	}
}

func TestLiveForgePassesAndPublishesOneDraft(t *testing.T) {
	live := Live{Target: "example/project", Change: "!1", Commit: liveCommit, AllowWrite: true, Head: "forgecell/check", Base: "main"}
	r, calls := runLive(t, "forge", "live", live)
	requireAll(t, liveChecks(t, r, CheckLiveDescribe, CheckLivePolicy, CheckLiveChange, CheckLivePublish), Pass, "")
	if !r.Passed || countCalls(calls, "forge_publish_draft") != 1 {
		t.Fatalf("report %+v, calls %+v", r, calls)
	}
	for _, c := range calls {
		if c.Tool != "forge_publish_draft" {
			continue
		}
		var args map[string]string
		if err := json.Unmarshal(c.Arguments, &args); err != nil || args["title"] != livePublishTitle || args["body"] != livePublishBody ||
			args["headBranch"] != "forgecell/check" || args["base"] != "main" || args["commit"] != liveCommit || args["project"] != "example/project" {
			t.Fatalf("forge_publish_draft arguments = %s", c.Arguments)
		}
	}
}

func TestLiveForgeWithoutAllowWriteNeverCallsAWriteTool(t *testing.T) {
	r, calls := runLive(t, "forge", "live", Live{Target: "example/project", Change: "!1", Commit: liveCommit})
	requireAll(t, liveChecks(t, r, CheckLiveDescribe, CheckLivePolicy, CheckLiveChange), Pass, "")
	if !r.Passed || countCalls(calls, "forge_publish_draft") != 0 || countCalls(calls, "forge_checks") != 1 || countCalls(calls, "forge_comments") != 1 {
		t.Fatalf("report %+v, calls %+v", r, calls)
	}
	r, calls = runLive(t, "forge", "live", Live{Target: "example/project"})
	requireAll(t, liveChecks(t, r, CheckLiveDescribe, CheckLivePolicy), Pass, "")
	if !r.Passed || countCalls(calls, "forge_publish_draft") != 0 || countCalls(calls, "forge_change") != 0 {
		t.Fatalf("report %+v, calls %+v", r, calls)
	}
}

func TestLivePublishFailsADraftThatIsNotOne(t *testing.T) {
	for _, mode := range []string{"live-not-draft", "live-wrong-head"} {
		live := Live{Target: "example/project", Commit: liveCommit, AllowWrite: true, Head: "forgecell/check", Base: "main"}
		r, calls := runLive(t, "forge", mode, live)
		checks := liveChecks(t, r, CheckLiveDescribe, CheckLivePolicy, CheckLivePublish)
		if r.Passed || checks[2].Status != Fail || checks[2].Reason != "failed" {
			t.Fatalf("%s: live-publish = %+v", mode, checks[2])
		}
		if countCalls(calls, "forge_find_change") != 0 {
			t.Fatalf("%s: calls after the failed change = %+v", mode, calls)
		}
	}
}

func TestLiveReviewerFindings(t *testing.T) {
	live := Live{Target: "forge.example/example/project", Change: "!1", Commit: liveCommit}
	r, calls := runLive(t, "reviewer", "good", live)
	requireAll(t, liveChecks(t, r, CheckLiveFindings), Pass, "")
	if !r.Passed || countCalls(calls, "reviewer_findings") != 2 {
		t.Fatalf("report %+v, calls %+v", r, calls)
	}
	var args struct {
		Forge map[string]any `json:"forge"`
		Ref   string         `json:"ref"`
	}
	if err := json.Unmarshal(calls[len(calls)-1].Arguments, &args); err != nil || args.Forge["identity"] != live.Target || args.Ref != "!1" {
		t.Fatalf("reviewer_findings arguments = %s", calls[len(calls)-1].Arguments)
	}
	r, _ = runLive(t, "reviewer", "live-unstable-findings", live)
	if checks := liveChecks(t, r, CheckLiveFindings); r.Passed || checks[0].Status != Fail || checks[0].Reason != "failed" {
		t.Fatalf("live-findings = %+v", checks[0])
	}
	// Without a change the arguments fail the schema and nothing is called.
	r, calls = runLive(t, "reviewer", "good", Live{Target: live.Target})
	if checks := liveChecks(t, r, CheckLiveFindings); r.Passed || checks[0].Reason != "schema-invalid" || countCalls(calls, "reviewer_findings") != 0 {
		t.Fatalf("live-findings = %+v, calls %+v", checks[0], calls)
	}
}

func TestRateLimitedStopsTheRunWithNoFurtherCalls(t *testing.T) {
	live := Live{Target: "example/project", Change: "!1", Commit: liveCommit, AllowWrite: true, Head: "forgecell/check", Base: "main"}
	r, calls := runLive(t, "forge", "live-rate-limited", live)
	checks := liveChecks(t, r, CheckLiveDescribe, CheckLivePolicy, CheckLiveChange, CheckLivePublish)
	requireAll(t, checks[:1], Pass, "")
	requireAll(t, checks[1:], Skip, "rate-limited")
	if r.Passed || len(calls) == 0 || calls[len(calls)-1].Tool != "forge_policy" || countCalls(calls, "forge_policy") != 1 {
		t.Fatalf("report %+v, calls %+v", r, calls)
	}

	r, calls = runLive(t, "tracker", "live", Live{Target: "FAKE", Config: map[string]any{"fail": "rate-limited"}})
	requireAll(t, liveChecks(t, r, CheckLiveList, CheckLiveGet, CheckLiveStates), Skip, "rate-limited")
	if r.Passed || calls[len(calls)-1].Tool != "tracker_describe" || countCalls(calls, "tracker_list") != 0 {
		t.Fatalf("report %+v, calls %+v", r, calls)
	}
}

func TestLiveChecksSkipWhenContractModeFails(t *testing.T) {
	r, calls := runLive(t, "tracker", "missing-tool", Live{Target: "FAKE"})
	if failed, _ := r.Failed(); r.Passed || failed.ID != CheckToolsList {
		t.Fatalf("first failure %+v", failed)
	}
	live := r.Checks[len(checkOrder):]
	if len(live) != 3 || len(calls) != 0 {
		t.Fatalf("live checks %+v, calls %+v", live, calls)
	}
	requireAll(t, live, Skip, "")
}

func TestLiveReportsCarryNoConfigValuesOrLocalPaths(t *testing.T) {
	const secret = "config-secret-value-0451"
	r, _ := runLive(t, "tracker", "live", Live{Target: "FAKE‮\x07", Config: map[string]any{"token": secret}})
	if !r.Passed || r.Target != "FAKE" {
		t.Fatalf("report = %+v", r)
	}
	var text, js bytes.Buffer
	if err := r.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{text.String(), js.String()} {
		if strings.Contains(out, secret) || strings.Contains(out, "token") || strings.Contains(out, os.TempDir()) || strings.Contains(out, plugintest.PlantedSecret) {
			t.Fatalf("report leaks:\n%s", out)
		}
	}
	if !strings.Contains(text.String(), "PASS live-states") || !strings.Contains(text.String(), "Result: PASS (11 of 11 checks passed; live mode, target FAKE)") {
		t.Fatalf("text report:\n%s", text.String())
	}
	var decoded Report
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil || decoded.Target != "FAKE" || decoded.Mode != "live" || len(decoded.Checks) != 11 {
		t.Fatalf("JSON report = %+v, %v", decoded, err)
	}
}

// contractText and contractJSON are the contract-mode reports of the good forge
// fake as they were before live mode, with the manifest digest left open.
const contractText = `Plugin fake-forge · manifest plugin.json
Kind forge · contract forgecell.forge/v1 · manifest SHA-256 %s
PASS start — the manifest decodes, every pinned file matches its digest and the server starts
PASS initialize — initialize succeeds and declares the manifest's contract
PASS tools-list — tools/list covers every required contract tool with a compatible input schema
PASS describe — the describe tool's output decodes strictly
PASS describe-deterministic — a second describe call returns the same output
PASS malformed-input — malformed input to every read tool returns an MCP error and the server keeps serving
PASS output-bound — every response fit its tool's output bound
PASS timeout-bound — every call answered within its tool's timeout
Result: PASS (8 of 8 checks passed; contract mode, offline)
`

const contractJSON = `{
  "schemaVersion": "v1",
  "mode": "contract",
  "manifest": "plugin.json",
  "plugin": "fake-forge",
  "kind": "forge",
  "contract": "forgecell.forge/v1",
  "manifestSha256": "%s",
  "passed": true,
  "checks": [
    {
      "id": "start",
      "status": "pass",
      "summary": "the manifest decodes, every pinned file matches its digest and the server starts"
    },
    {
      "id": "initialize",
      "status": "pass",
      "summary": "initialize succeeds and declares the manifest's contract"
    },
    {
      "id": "tools-list",
      "status": "pass",
      "summary": "tools/list covers every required contract tool with a compatible input schema"
    },
    {
      "id": "describe",
      "status": "pass",
      "summary": "the describe tool's output decodes strictly"
    },
    {
      "id": "describe-deterministic",
      "status": "pass",
      "summary": "a second describe call returns the same output"
    },
    {
      "id": "malformed-input",
      "status": "pass",
      "summary": "malformed input to every read tool returns an MCP error and the server keeps serving"
    },
    {
      "id": "output-bound",
      "status": "pass",
      "summary": "every response fit its tool's output bound"
    },
    {
      "id": "timeout-bound",
      "status": "pass",
      "summary": "every call answered within its tool's timeout"
    }
  ]
}
`

func TestContractModeReportsAreByteIdentical(t *testing.T) {
	for _, mode := range []string{"good", "live"} {
		r, _ := run(t, "forge", mode)
		var text, js bytes.Buffer
		if err := r.WriteText(&text); err != nil {
			t.Fatal(err)
		}
		if err := r.WriteJSON(&js); err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf(contractText, r.ManifestSHA256); text.String() != want {
			t.Fatalf("%s: text report\n%s\nwant\n%s", mode, text.String(), want)
		}
		if want := fmt.Sprintf(contractJSON, r.ManifestSHA256); js.String() != want {
			t.Fatalf("%s: JSON report\n%s\nwant\n%s", mode, js.String(), want)
		}
	}
}
