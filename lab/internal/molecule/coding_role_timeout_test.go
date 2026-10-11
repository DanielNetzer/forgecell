package molecule

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// codingRoleAmendment approves a Formula whose default harness and coding role
// carry the given timeout declarations, then amends the first plan to timeoutMS.
func codingRoleAmendment(t *testing.T, defaults, coding string, timeoutMS int) error {
	t.Helper()
	o, repo := fixture(t)
	command, _ := json.Marshal([]string{filepath.Join(filepath.Dir(repo), "harness")})
	recipe := "schemaVersion: v0\nkind: formula\nid: sample\nintake: {source: github-issues, repo: acme/widgets}\nharness:\n  binding: custom\n  command: " + string(command) + "\n" + defaults +
		"harnesses:\n  coding:\n    binding: custom\n    command: " + string(command) + "\n" + coding +
		"atoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"
	approveFixture(t, o.LabDir, recipe, "coding-role")
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness == nil {
		t.Fatalf("fixture not ready: %+v", r)
	}
	next := copyPlan(t, r.Readiness.Plans[0].Plan)
	next.HarnessTimeoutMS = timeoutMS
	_, err = Amend(context.Background(), o.LabDir, r.ID, r.Readiness.Plans[0].Digest, next, "")
	return err
}

func TestAmendTimeoutCeilingFollowsTheCodingRole(t *testing.T) {
	const defaults = "  timeoutMs: 1000\n  maxTimeoutMs: 2000\n"
	for _, c := range []struct {
		name, coding string
		timeoutMS    int
		want         string
	}{
		{"within a coding ceiling above the default", "    timeoutMs: 5000\n    maxTimeoutMs: 9000\n", 8000, ""},
		{"above the coding ceiling", "    timeoutMs: 5000\n    maxTimeoutMs: 9000\n", 9001, "maximum of 9000"},
		{"a coding ceiling below the default", "    timeoutMs: 300\n    maxTimeoutMs: 500\n", 600, "maximum of 500"},
		{"an omitted coding timeout inherits the default ceiling", "", 2000, ""},
		{"an omitted coding timeout is still bounded", "", 2001, "maximum of 2000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := codingRoleAmendment(t, defaults, c.coding, c.timeoutMS)
			if c.want == "" && err != nil {
				t.Fatal(err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
		})
	}
}
