package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

func TestMain(m *testing.M) { os.Exit(plugintest.Main(m)) }

// spawn starts the fake server directly, without the plugin launcher, and
// returns a client on its pipes.
func spawn(t *testing.T, kind, mode string) *mcp.Client {
	t.Helper()
	server := plugintest.Server(t)
	cmd := exec.Command(server.Path, "--kind", kind, "--mode", mode)
	cmd.Dir = plugintest.TempDir(t)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + cmd.Dir}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	})
	return mcp.NewClient(stdout, stdin, 0)
}

func reason(t *testing.T, err error, want mcp.Reason) *mcp.Error {
	t.Helper()
	e, ok := err.(*mcp.Error)
	if !ok || e.Reason != want {
		t.Fatalf("error = %v, want %s", err, want)
	}
	return e
}

func initialized(t *testing.T, kind, mode string) *mcp.Client {
	t.Helper()
	c := spawn(t, kind, mode)
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGoodServerSession(t *testing.T) {
	c := spawn(t, "tracker", "good")
	info, err := c.Initialize(context.Background())
	if err != nil || info.Contract != "forgecell.tracker/v1" || info.ProtocolVersion != mcp.ProtocolVersion || info.Name != "fake" {
		t.Fatalf("Initialize = %+v, %v", info, err)
	}
	if c.Server() != info {
		t.Fatal("the server info was not recorded")
	}
	tools, err := c.ListTools(context.Background())
	if err != nil || len(tools) != 4 {
		t.Fatalf("ListTools = %v, %v", tools, err)
	}
	ct, _ := contract.Lookup("tracker")
	for _, tool := range tools {
		want, ok := ct.Tool(tool.Name)
		if !ok {
			t.Fatalf("unexpected tool %s", tool.Name)
		}
		if err := want.CheckAdvertised(tool.InputSchema); err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
	}
	describe := ct.Describe()
	raw, err := c.CallTool(context.Background(), describe.Name, map[string]any{"config": map[string]any{}}, describe.MessageBytes())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := describe.Decode(raw); err != nil {
		t.Fatalf("describe output: %v", err)
	}
}

func TestCallBeforeInitializeFails(t *testing.T) {
	c := spawn(t, "tracker", "good")
	if _, err := c.ListTools(context.Background()); err == nil {
		t.Fatal("tools/list ran before initialize")
	}
	reason(t, func() error { _, err := c.CallTool(context.Background(), "bad name", nil, 0); return err }(), mcp.SchemaInvalid)
}

func TestToolErrorsAreRemoteAndMapped(t *testing.T) {
	c := initialized(t, "tracker", "good")
	args := func(fail string) map[string]any {
		return map[string]any{"config": map[string]any{"fail": fail}, "selector": "all", "limit": 1}
	}
	for fail, want := range map[string]mcp.Reason{"auth-missing": mcp.AuthMissing, "rate-limited": mcp.RateLimited, "network": mcp.Network, "timeout": mcp.Failed, "other": mcp.Failed} {
		_, err := c.CallTool(context.Background(), "tracker_list", args(fail), 0)
		if e := reason(t, err, want); !e.Remote {
			t.Fatalf("%s: not marked remote", fail)
		}
	}
	_, err := c.CallTool(context.Background(), "tracker_list", map[string]any{"limit": "ten"}, 0)
	if e := reason(t, err, mcp.Failed); !e.Remote {
		t.Fatal("a JSON-RPC error is not marked remote")
	}
	if _, err := c.CallTool(context.Background(), "tracker_states", map[string]any{"config": map[string]any{}, "keys": []string{"FAKE-1"}}, 0); err != nil {
		t.Fatalf("the session did not survive remote errors: %v", err)
	}
}

func TestSlowServerTimesOutAndEndsTheSession(t *testing.T) {
	c := initialized(t, "forge", "slow")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.CallTool(ctx, "forge_describe", map[string]any{"project": "p"}, 0)
	reason(t, err, mcp.Timeout)
	_, err = c.CallTool(context.Background(), "forge_describe", map[string]any{"project": "p"}, 0)
	reason(t, err, mcp.Failed)
}

func TestCrashMidCallFails(t *testing.T) {
	c := initialized(t, "reviewer", "crash")
	_, err := c.CallTool(context.Background(), "reviewer_describe", map[string]any{"config": map[string]any{}}, 0)
	if e := reason(t, err, mcp.Failed); e.Remote {
		t.Fatal("a crash is marked remote")
	}
}

func TestOversizedResponseIsRefused(t *testing.T) {
	c := initialized(t, "forge", "oversized")
	_, err := c.CallTool(context.Background(), "forge_describe", map[string]any{"project": "p"}, 1<<20)
	reason(t, err, mcp.OutputTooLarge)
}

func TestPerCallBoundOnlyLowers(t *testing.T) {
	c := initialized(t, "forge", "good")
	_, err := c.CallTool(context.Background(), "forge_policy", map[string]any{"project": "p", "branch": "main"}, 16)
	reason(t, err, mcp.OutputTooLarge)
	c = initialized(t, "forge", "good")
	if _, err := c.CallTool(context.Background(), "forge_policy", map[string]any{"project": "p", "branch": "main"}, 1<<30); err != nil {
		t.Fatalf("a bound above the default: %v", err)
	}
}

func TestMalformedFramingIsSchemaInvalid(t *testing.T) {
	c := spawn(t, "tracker", "bad-framing")
	_, err := c.Initialize(context.Background())
	reason(t, err, mcp.SchemaInvalid)
}

func TestWrongContractIsRecordedForTheCaller(t *testing.T) {
	c := spawn(t, "tracker", "wrong-contract")
	info, err := c.Initialize(context.Background())
	if err != nil || info.Contract != "forgecell.tracker/v2" {
		t.Fatalf("Initialize = %+v, %v", info, err)
	}
}

func TestAnotherProtocolVersionIsRefused(t *testing.T) {
	c := spawn(t, "tracker", "wrong-protocol")
	info, err := c.Initialize(context.Background())
	reason(t, err, mcp.ContractMismatch)
	if info != (mcp.ServerInfo{}) || c.Server() != (mcp.ServerInfo{}) {
		t.Fatalf("a refused server was recorded: %+v", info)
	}
	if _, err := c.ListTools(context.Background()); err == nil {
		t.Fatal("tools/list ran after a refused initialize")
	}
}

func TestDeclaredContractIsSanitized(t *testing.T) {
	c := spawn(t, "tracker", "dirty-contract")
	info, err := c.Initialize(context.Background())
	if err != nil || info.Contract != "forgecell.tracker/v2  x" {
		t.Fatalf("Initialize = %q, %v", info.Contract, err)
	}
}

func TestInitializeContractComparesTheRawContract(t *testing.T) {
	c := spawn(t, "tracker", "zero-width-contract")
	info, err := c.InitializeContract(context.Background(), "forgecell.tracker/v1")
	reason(t, err, mcp.ContractMismatch)
	if info.Contract != "forgecell.tracker/v1" || c.Server() != (mcp.ServerInfo{}) {
		t.Fatalf("Initialize = %+v, recorded %+v", info, c.Server())
	}
	if _, err := c.ListTools(context.Background()); err == nil {
		t.Fatal("tools/list ran after a refused initialize")
	}
}

func TestInitializeContractAcceptsTheExactContract(t *testing.T) {
	c := spawn(t, "tracker", "good")
	if info, err := c.InitializeContract(context.Background(), "forgecell.tracker/v1"); err != nil || info.Contract != "forgecell.tracker/v1" {
		t.Fatalf("InitializeContract = %+v, %v", info, err)
	}
}

func TestInitializeHonoursTheCallerDeadline(t *testing.T) {
	// The silent server never answers, so any deadline gives the same outcome.
	c := spawn(t, "tracker", "silent")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Initialize(ctx)
	reason(t, err, mcp.Timeout)
}

func TestServerRequestIsRefusedNotActedOn(t *testing.T) {
	// The fake answers initialize only if the client replied to its
	// sampling/createMessage request with a JSON-RPC error.
	c := spawn(t, "forge", "server-request")
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize with a server request: %v", err)
	}
}

func TestUnknownResponseIDIsSchemaInvalid(t *testing.T) {
	c := initialized(t, "reviewer", "wrong-id")
	_, err := c.ListTools(context.Background())
	reason(t, err, mcp.SchemaInvalid)
}

func TestMissingToolIsVisibleToTheCaller(t *testing.T) {
	c := initialized(t, "reviewer", "missing-tool")
	tools, err := c.ListTools(context.Background())
	if err != nil || len(tools) != 1 {
		t.Fatalf("ListTools = %v, %v", tools, err)
	}
	var schema map[string]any
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil || schema["type"] != "object" {
		t.Fatalf("inputSchema = %s", tools[0].InputSchema)
	}
}
