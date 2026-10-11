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
	"1": "dbd82aadc85eca36dad19d700ad8f4fbb71108760191b31c4824d9a82cf35643",
}

func adapterContractDigest(t *testing.T) string {
	t.Helper()
	h := sha256.New()
	coding := map[string]any{"kind": "molecule", "codingAllowlist": []string{"Read", "Glob", "Grep", "Edit", "Write", "Bash(git status)", "Bash(git diff)", "Bash(go -C lab test ./...)"}}
	for _, id := range []string{"codex", "claude-code", "cursor"} {
		for _, request := range []map[string]any{coding, {"kind": "ticket-analysis"}, {"kind": "formula-improvement"}} {
			call, err := BuildInvocation(id, request, "/fixture/result")
			if err != nil {
				fmt.Fprintf(h, "%s/%s: unsupported: %s\n", id, request["kind"], err)
				continue
			}
			raw, _ := json.Marshal(call)
			fmt.Fprintf(h, "%s/%s: %s\n", id, request["kind"], raw)
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
