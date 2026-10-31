package main

import (
	"encoding/json"
	"os"
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

// call answers tools/call. Arguments that fail the contract input schema get a
// JSON-RPC invalid-params error (or a crash in bad-input-crash mode); a config
// {"fail": REASON} gets an isError result naming that reason.
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
	var args struct {
		Config map[string]any `json:"config"`
	}
	json.Unmarshal(p.Arguments, &args)
	if reason, ok := args.Config["fail"].(string); ok {
		s.reply(id, map[string]any{"isError": true, "content": []any{}, "structuredContent": map[string]any{"reason": reason}})
		return
	}
	out := outputs[p.Name]
	if s.mode == "oversized" && strings.HasSuffix(p.Name, "_describe") {
		out = map[string]any{"displayName": strings.Repeat("x", 5<<20)}
	}
	text, _ := json.Marshal(out)
	s.reply(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}, "structuredContent": out})
}
