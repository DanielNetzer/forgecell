package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

// outputs are canned, valid outputs for every contract tool.
var outputs = map[string]map[string]any{
	"tracker_describe":    {"displayName": "Fake tracker", "keyPattern": "^FAKE-[0-9]+$", "states": []string{"open", "closed"}, "selectors": []string{"all"}},
	"tracker_list":        {"items": []any{map[string]any{"key": "FAKE-1", "title": "First", "labels": []string{}, "updatedAt": "2026-10-30T10:00:00Z", "state": "open"}}},
	"tracker_get":         {"key": "FAKE-1", "title": "First", "body": "Body", "state": "open", "url": "https://tracker.example/FAKE-1", "updatedAt": "2026-10-30T10:00:00Z", "labels": []string{}},
	"tracker_states":      {"states": map[string]any{"FAKE-1": map[string]any{"state": "open"}}},
	"forge_describe":      {"identity": "forge.example/example/project", "host": "forge.example", "displayName": "Fake forge", "changeNoun": "change request", "defaultBranch": "main", "ciKind": "none", "webURL": "https://forge.example/example/project"},
	"forge_policy":        {"requiredChecks": []string{}, "requiredReviews": 0, "protected": false},
	"forge_find_change":   {"changes": []any{}},
	"forge_publish_draft": {"ref": "!1", "url": "https://forge.example/1", "draft": true, "headCommit": commit},
	"forge_change":        {"state": "open", "draft": true, "headCommit": commit, "url": "https://forge.example/1"},
	"forge_checks":        {"checks": []any{}, "complete": true},
	"forge_comments":      {"comments": []any{}},
	"reviewer_describe":   {"displayName": "Fake reviewer", "severities": []string{"high", "low"}},
	"reviewer_findings":   {"status": "complete", "findings": []any{}},
}

// recordEnv names the file the server appends every tools/call's name and
// received arguments to, one JSON line each, when it is set; tests pass it
// through the manifest's needs.env.
const recordEnv = "FAKE_RECORD"

// scripted maps a reviewer tool to the config key whose object value the
// server answers with in place of its canned output, so a test scripts the
// description and findings through the Formula reviewer's config.
var scripted = map[string]string{"reviewer_describe": "describe", "reviewer_findings": "findings"}

// record appends one received call to the FAKE_RECORD file.
func record(name string, arguments json.RawMessage) {
	path := os.Getenv(recordEnv)
	if path == "" {
		return
	}
	line, _ := json.Marshal(map[string]any{"tool": name, "arguments": arguments})
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	f.Write(append(line, '\n'))
	f.Close()
}

// call answers tools/call. Arguments that fail the contract input schema get a
// JSON-RPC invalid-params error (or a crash in bad-input-crash mode); a config
// {"fail": REASON} gets an isError result naming that reason. A reviewer
// config {"describe": {...}} or {"findings": {...}} is that tool's output.
func (s *server) call(id, params json.RawMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &p) != nil {
		s.fail(id, -32602, "invalid params")
		return
	}
	switch s.mode {
	case "crash":
		s.out.WriteString(`{"jsonrpc":"2.0",`)
		s.out.Flush()
		os.Exit(3)
	case "slow":
		blockForever()
	}
	tool, ok := s.contract.Tool(p.Name)
	if !ok {
		s.fail(id, -32602, "unknown tool")
		return
	}
	if err := tool.ValidateInputJSON(p.Arguments); err != nil {
		if s.mode == "bad-input-crash" {
			os.Exit(4)
		}
		s.fail(id, -32602, "invalid arguments")
		return
	}
	record(p.Name, p.Arguments)
	var args struct {
		Config map[string]any `json:"config"`
	}
	json.Unmarshal(p.Arguments, &args)
	if reason, ok := args.Config["fail"].(string); ok {
		s.reply(id, map[string]any{"isError": true, "content": []any{}, "structuredContent": map[string]any{"reason": reason}})
		return
	}
	out := outputs[p.Name]
	if script, ok := args.Config[scripted[p.Name]].(map[string]any); ok && scripted[p.Name] != "" {
		out = script
	}
	if strings.HasPrefix(s.mode, "live") {
		live, reason := liveOutput(s.mode, p.Name, p.Arguments)
		if reason != "" {
			s.reply(id, map[string]any{"isError": true, "content": []any{}, "structuredContent": map[string]any{"reason": reason}})
			return
		}
		if live != nil {
			out = live
		}
	}
	if s.mode == "oversized" && strings.HasSuffix(p.Name, "_describe") {
		out = map[string]any{"displayName": strings.Repeat("x", 5<<20)}
	}
	text, _ := json.Marshal(out)
	s.reply(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}, "structuredContent": out})
}

// The live modes serve the conformance kit's live checks. live is well
// behaved: tracker_list pages through FAKE-1 to FAKE-4 with one cursor, get
// and states agree, and forge_find_change finds the published draft. Each
// other mode breaks one rule: live-repeat-key, live-repeat-cursor,
// live-bad-key, live-bad-state (a state the description omits),
// live-states-disagree, live-endless (a cursor on every page),
// live-not-draft, live-wrong-head, live-unstable-findings,
// live-rate-limited (forge_policy is rate limited),
// live-change-rate-limited (forge_change is rate limited),
// live-oversized-list (tracker_list breaks its output bound) and
// live-slow-list (tracker_list never answers). In live-config the live
// config scripts the tracker: keyPattern replaces the described one,
// onlyState is the one described state, keys
// is the one page tracker_list returns with the state itemState, getKey is
// the key tracker_get returns and statesState the state tracker_states
// returns.

// otherCommit is the head live-wrong-head reports.
const otherCommit = "fedcba9876543210fedcba9876543210fedcba98"

// findingsCalls counts reviewer_findings calls for live-unstable-findings.
var findingsCalls int

// liveOutput returns a live mode's output for a tool, nil to keep the canned
// one, or a reason the call fails with.
func liveOutput(mode, name string, raw json.RawMessage) (map[string]any, string) {
	var args struct {
		Cursor string         `json:"cursor"`
		Key    string         `json:"key"`
		Keys   []string       `json:"keys"`
		Config map[string]any `json:"config"`
	}
	json.Unmarshal(raw, &args)
	if mode == "live-config" {
		return scriptedTracker(name, args.Key, args.Keys, args.Config), ""
	}
	switch name {
	case "tracker_describe":
		if mode == "live-bad-state" {
			return map[string]any{"displayName": "Fake tracker", "keyPattern": "^FAKE-[0-9]+$", "states": []string{"open"}, "selectors": []string{"all"}}, ""
		}
	case "tracker_list":
		switch mode {
		case "live-oversized-list":
			return map[string]any{"items": []any{}, "nextCursor": strings.Repeat("x", 5<<20)}, ""
		case "live-slow-list":
			blockForever()
		}
		return livePage(mode, args.Cursor), ""
	case "tracker_get":
		state := "open"
		if mode == "live-bad-state" {
			state = "closed"
		}
		return map[string]any{"key": args.Key, "title": "Issue", "body": "Body", "state": state, "url": "https://tracker.example/" + args.Key, "updatedAt": "2026-10-30T10:00:00Z", "labels": []string{}}, ""
	case "tracker_states":
		state := "open"
		if mode == "live-states-disagree" {
			state = "closed"
		}
		states := map[string]any{}
		for _, key := range args.Keys {
			states[key] = map[string]any{"state": state}
		}
		return map[string]any{"states": states}, ""
	case "forge_policy":
		if mode == "live-rate-limited" {
			return nil, "rate-limited"
		}
	case "forge_find_change":
		return map[string]any{"changes": []any{map[string]any{"ref": "!1", "url": "https://forge.example/1", "state": "open", "draft": true, "headCommit": commit}}}, ""
	case "forge_change":
		if mode == "live-change-rate-limited" {
			return nil, "rate-limited"
		}
		change := map[string]any{"state": "open", "draft": true, "headCommit": commit, "url": "https://forge.example/1"}
		switch mode {
		case "live-not-draft":
			change["draft"] = false
		case "live-wrong-head":
			change["headCommit"] = otherCommit
		}
		return change, ""
	case "reviewer_findings":
		if mode == "live-unstable-findings" {
			findingsCalls++
			finding := map[string]any{"id": fmt.Sprintf("F-%d", findingsCalls), "severity": "low", "title": "Finding", "body": "Body", "resolved": false}
			return map[string]any{"status": "complete", "findings": []any{finding}}, ""
		}
	}
	return nil, ""
}

// scriptedTracker is live-config's output for a tracker tool, nil to keep the
// canned one.
func scriptedTracker(name, key string, keys []string, config map[string]any) map[string]any {
	text := func(field, fallback string) string {
		if s, ok := config[field].(string); ok {
			return s
		}
		return fallback
	}
	switch name {
	case "tracker_describe":
		states := []any{"open", "closed"}
		if only := text("onlyState", ""); only != "" {
			states = []any{only}
		}
		return map[string]any{"displayName": "Fake tracker", "keyPattern": text("keyPattern", "^FAKE-[0-9]+$"), "states": states, "selectors": []string{"all"}}
	case "tracker_list":
		items := []any{}
		listed, _ := config["keys"].([]any)
		for _, k := range listed {
			items = append(items, map[string]any{"key": k, "title": "Issue", "labels": []string{}, "updatedAt": "2026-10-30T10:00:00Z", "state": text("itemState", "open")})
		}
		return map[string]any{"items": items}
	case "tracker_get":
		got := text("getKey", key)
		return map[string]any{"key": got, "title": "Issue", "body": "Body", "state": "open", "url": "https://tracker.example/issue", "updatedAt": "2026-10-30T10:00:00Z", "labels": []string{}}
	case "tracker_states":
		states := map[string]any{}
		for _, k := range keys {
			states[k] = map[string]any{"state": text("statesState", "open")}
		}
		return map[string]any{"states": states}
	}
	return nil
}

// livePage is page n of tracker_list, where cursor "cN" names page N and no
// cursor names page 0. Each page holds FAKE-(2n+1) and FAKE-(2n+2).
func livePage(mode, cursor string) map[string]any {
	n := 0
	if cursor != "" {
		n, _ = strconv.Atoi(strings.TrimPrefix(cursor, "c"))
	}
	keys := []string{fmt.Sprintf("FAKE-%d", 2*n+1), fmt.Sprintf("FAKE-%d", 2*n+2)}
	next := ""
	switch {
	case mode == "live-endless":
		next = fmt.Sprintf("c%d", n+1)
	case n == 0:
		next = "c1"
	case mode == "live-repeat-key":
		keys = []string{"FAKE-2"}
	case mode == "live-repeat-cursor":
		next = "c1"
	case mode == "live-bad-key":
		keys = []string{"BAD-3"}
	}
	items := []any{}
	for _, key := range keys {
		items = append(items, map[string]any{"key": key, "title": "Issue", "labels": []string{}, "updatedAt": "2026-10-30T10:00:00Z", "state": "open"})
	}
	page := map[string]any{"items": items}
	if next != "" {
		page["nextCursor"] = next
	}
	return page
}
