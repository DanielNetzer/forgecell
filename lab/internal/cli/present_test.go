package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// The derived Claude coding allowlist is part of what the human approves, so
// the scope gate must show it, and must say when a legacy plan has none.
func TestScopeGateShowsCodingAllowlist(t *testing.T) {
	for name, tc := range map[string]struct {
		allowlist []string
		want      []string
	}{
		"derived": {[]string{"Read", "Bash(go -C lab test ./...)"}, []string{"Claude Code coding allowlist (approved with this plan", "  Bash(go -C lab test ./...)"}},
		"legacy":  {nil, []string{"Claude Code coding allowlist: none stored"}},
	} {
		t.Run(name, func(t *testing.T) {
			p := readiness.Plan{CodingAllowlist: tc.allowlist}
			r := molecule.Record{ID: "mol-1", Readiness: &readiness.State{Phase: "scope-waiting", Plans: []readiness.Proposal{{Plan: p, Digest: "d"}}}}
			var out bytes.Buffer
			presentRun(&out, r)
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q in:\n%s", want, out.String())
				}
			}
		})
	}
}
