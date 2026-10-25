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
	"3": "e6b8d50990a38664b0fa8c5248a4f1ed734ad4e20c208b7fc496fb05f1c5bf41",
	"4": "1fc9b2bc45c047ebe028ea02753265786e8d43f83bd6b6bda3948bbf5f376208",
	"5": "7be0c738aec6dea76f8a4d64dd61f4bb8c4d0326533bb3d38c458e37fb80435f",
	"6": "65f4a5f18b8bcdcd538ab3ac5ce44c26de3fd62ebd07c8577f3413f14fc0e2fb",
}

func adapterContractDigest(t *testing.T) string {
	t.Helper()
	h := sha256.New()
	allowlist := []string{"Read", "Glob", "Grep", "Edit", "Write", "Bash(git status)", "Bash(git diff)", "Bash(go -C lab test ./...)"}
	coding := map[string]any{"kind": "molecule", "codingAllowlist": allowlist}
	servers := []MCPServer{{Name: "graphify", Command: "/priv/graphify", Args: []string{"mcp", "--graph", "/cache/graph.json"}, Tools: []string{"query_graph"}}}
	codingWithExtensions := map[string]any{"kind": "molecule", "codingAllowlist": allowlist, "mcpServers": servers}
	analysisWithExtensions := map[string]any{"kind": "ticket-analysis", "mcpServers": servers}
	codingWithModel := map[string]any{"kind": "molecule", "codingAllowlist": allowlist, "harnessModel": "model-fixture"}
	for _, id := range []string{"codex", "claude-code", "cursor"} {
		for _, c := range []struct {
			label   string
			request map[string]any
		}{
			{"molecule", coding}, {"ticket-analysis", map[string]any{"kind": "ticket-analysis"}}, {"formula-improvement", map[string]any{"kind": "formula-improvement"}},
			{"molecule+extensions", codingWithExtensions}, {"ticket-analysis+extensions", analysisWithExtensions},
			{"molecule+model", codingWithModel}, {"ticket-analysis+model", map[string]any{"kind": "ticket-analysis", "harnessModel": "model-fixture"}},
			{"formula-improvement+model", map[string]any{"kind": "formula-improvement", "harnessModel": "model-fixture"}},
			{"review", map[string]any{"kind": ReviewKind}}, {"review+extensions", map[string]any{"kind": ReviewKind, "mcpServers": servers}},
			{"review+model", map[string]any{"kind": ReviewKind, "harnessModel": "model-fixture"}},
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
