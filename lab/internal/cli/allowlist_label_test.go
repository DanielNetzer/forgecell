package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// TestPlanAllowlistLabel: the plan view names the coding binding's harness. Only
// the claude-code adapter applies the coding allowlist, so Codex and Cursor get
// one line saying it is not applied instead of entries shown as if enforced.
func TestPlanAllowlistLabel(t *testing.T) {
	snapshot := func(defaultAdapter, coding string) string {
		text := "kind: formula\nid: f\nharness: {binding: " + defaultAdapter + ", command: [/forgecell, __adapter, " + defaultAdapter + ", /provider]}\n"
		if coding != "" {
			text += "harnesses:\n  coding: {binding: " + coding + ", command: [/forgecell, __adapter, " + coding + ", /provider]}\n"
		}
		return text + roleAtoms
	}
	allowlist := []string{"Read", "Bash(go -C lab test ./...)"}
	for name, tc := range map[string]struct {
		yaml       string
		allowlist  []string
		want, deny []string
	}{
		"claude-code": {snapshot("claude-code", ""), allowlist, []string{"Claude Code coding allowlist (approved with this plan", "  Bash(go -C lab test ./...)"}, []string{"not applied"}},
		"codex":       {snapshot("codex", ""), allowlist, []string{"Coding allowlist: not applied — coding is bound to Codex"}, []string{"Claude Code coding allowlist", "  Bash(go -C lab test ./...)"}},
		"cursor":      {snapshot("cursor", ""), allowlist, []string{"Coding allowlist: not applied — coding is bound to Cursor"}, []string{"Claude Code coding allowlist", "  Bash(go -C lab test ./...)"}},
		"coding role": {snapshot("claude-code", "codex"), allowlist, []string{"coding is bound to Codex"}, []string{"Claude Code coding allowlist"}},
		"claude role": {snapshot("cursor", "claude-code"), allowlist, []string{"Claude Code coding allowlist (approved with this plan", "  Read"}, []string{"not applied"}},
		"cursor none": {snapshot("cursor", ""), nil, []string{"coding is bound to Cursor"}, []string{"none stored"}},
		"custom":      {"kind: formula\nid: f\nharness: {binding: codex, command: [/custom/wrapper]}\n" + roleAtoms, allowlist, []string{"Claude Code coding allowlist (approved with this plan"}, []string{"not applied"}},
		"no snapshot": {"", allowlist, []string{"Claude Code coding allowlist (approved with this plan", "  Read"}, []string{"not applied"}},
		"unparsable":  {"harness: [", nil, []string{"Claude Code coding allowlist: none stored"}, []string{"not applied"}},
	} {
		t.Run(name, func(t *testing.T) {
			p := readiness.Plan{CodingAllowlist: tc.allowlist}
			r := molecule.Record{ID: "mol-1", FormulaSnapshot: molecule.Snapshot{YAML: tc.yaml}, Readiness: &readiness.State{Phase: "scope-waiting", Plans: []readiness.Proposal{{Plan: p, Digest: "d"}}}}
			var out bytes.Buffer
			presentRun(&out, r)
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q in:\n%s", want, out.String())
				}
			}
			for _, deny := range tc.deny {
				if strings.Contains(out.String(), deny) {
					t.Fatalf("unexpected %q in:\n%s", deny, out.String())
				}
			}
		})
	}
}
