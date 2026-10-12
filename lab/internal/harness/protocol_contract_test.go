package harness

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

// adapterContractDigests pins the exact provider invocation (argv, stdin prompt
// and instructions) for every native adapter and operation, per adapter
// protocol version. Approvals now survive rebuilds that keep the protocol
// version, so any change to how a provider is invoked, such as dropping a
// Claude isolation flag, must bump AdapterProtocolVersion and record a new digest.
var adapterContractDigests = map[string]string{
	"2": "cfd20646d39c5596c499bc60e99fa529731920ea47e0ce64f790b00539ca6c5e",
}

func adapterContractDigest(t *testing.T) string {
	t.Helper()
	h := sha256.New()
	allowlist := []string{"Read", "Glob", "Grep", "Edit", "Write", "Bash(git status)", "Bash(git diff)", "Bash(go -C lab test ./...)"}
	coding := map[string]any{"kind": "molecule", "codingAllowlist": allowlist}
	servers := []MCPServer{{Name: "graphify", Command: "/priv/graphify", Args: []string{"mcp", "--graph", "/cache/graph.json"}, Tools: []string{"query_graph"}}}
	codingWithExtensions := map[string]any{"kind": "molecule", "codingAllowlist": allowlist, "mcpServers": servers}
	analysisWithExtensions := map[string]any{"kind": "ticket-analysis", "mcpServers": servers}
	for _, id := range []string{"codex", "claude-code", "cursor"} {
		for _, c := range []struct {
			label   string
			request map[string]any
		}{
			{"molecule", coding}, {"ticket-analysis", map[string]any{"kind": "ticket-analysis"}}, {"formula-improvement", map[string]any{"kind": "formula-improvement"}},
			{"molecule+extensions", codingWithExtensions}, {"ticket-analysis+extensions", analysisWithExtensions},
		} {
			call, err := BuildInvocation(id, c.request, "/fixture/result")
			if err != nil {
				fmt.Fprintf(h, "%s/%s: unsupported: %s\n", id, c.label, err)
				continue
			}
			raw, _ := json.Marshal(call)
			fmt.Fprintf(h, "%s/%s: %s\n", id, c.label, raw)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestAdapterInvocationChangesRequireProtocolBump(t *testing.T) {
	got := adapterContractDigest(t)
	want, ok := adapterContractDigests[AdapterProtocolVersion]
	if !ok || want != got {
		t.Fatalf("provider invocation contract for adapter protocol %q is %s, pinned %q.\nIf the change is intended, bump AdapterProtocolVersion and pin the new digest: approvals survive rebuilds that keep the protocol version.", AdapterProtocolVersion, got, want)
	}
}
