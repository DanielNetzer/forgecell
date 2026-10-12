package harness

import (
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"reflect"
	"strings"
	"testing"
)

const goldenGraphifyConfig = `{"mcpServers":{"graphify":{"args":["mcp","--graph","/cache/graph.json"],"command":"/priv/graphify","env":{}}}}`

const goldenCodingAllowlist = "Read,Glob,Grep,Edit,Write,Bash(git status),Bash(git diff),Bash(go -C lab test ./...)"

func goldenServer() MCPServer {
	return MCPServer{Name: "graphify", Command: "/priv/graphify", Args: []string{"mcp", "--graph", "/cache/graph.json"}, Tools: []string{"query_graph"}}
}

func codingRequest(extra map[string]any) map[string]any {
	request := map[string]any{"kind": "molecule", "codingAllowlist": []string{"Read", "Glob", "Grep", "Edit", "Write", "Bash(git status)", "Bash(git diff)", "Bash(go -C lab test ./...)"}}
	for k, v := range extra {
		request[k] = v
	}
	return request
}

func build(t *testing.T, id string, request map[string]any) Invocation {
	t.Helper()
	call, err := BuildInvocation(id, request, "/fixture/result")
	if err != nil {
		t.Fatalf("%s %v: %v", id, request["kind"], err)
	}
	return call
}

func TestCodingInvocationAddsPinnedMCPServers(t *testing.T) {
	want := []string{"--print", "--output-format", "json", "--no-session-persistence", "--permission-mode", "dontAsk",
		"--setting-sources", "user", "--strict-mcp-config", "--mcp-config", goldenGraphifyConfig,
		"--disallowedTools", "Edit(/.claude/**)", "--json-schema", CodingOutcomeSchema(),
		"--allowedTools", goldenCodingAllowlist + ",mcp__graphify__query_graph"}
	got := build(t, "claude-code", codingRequest(map[string]any{"mcpServers": []MCPServer{goldenServer()}}))
	if !reflect.DeepEqual(got.Args, want) {
		t.Fatalf("coding argv with an extension changed:\n%q\nwant\n%q", got.Args, want)
	}

	// The request travels as JSON, so the decoded form must give the same argv.
	var decoded map[string]any
	raw := `{"kind":"molecule","codingAllowlist":["Read","Glob","Grep","Edit","Write","Bash(git status)","Bash(git diff)","Bash(go -C lab test ./...)"],"mcpServers":[{"name":"graphify","command":"/priv/graphify","args":["mcp","--graph","/cache/graph.json"],"tools":["query_graph"]}]}`
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	if viaJSON := build(t, "claude-code", decoded); !reflect.DeepEqual(viaJSON, got) {
		t.Fatalf("JSON-decoded servers gave a different invocation:\n%q\n%q", viaJSON.Args, got.Args)
	}
}

func TestAnalysisInvocationKeepsToolsDisabled(t *testing.T) {
	schema := readiness.AnalysisSchema()
	head := []string{"--print", "--output-format", "json", "--no-session-persistence", "--permission-mode", "dontAsk"}

	withoutSafeMode := append(append([]string{}, head...), "--tools", "", "--strict-mcp-config", "--mcp-config", goldenGraphifyConfig,
		"--setting-sources", "user", "--allowedTools", "mcp__graphify__query_graph", "--json-schema", schema,
		"--no-chrome", "--disable-slash-commands")
	got := build(t, "claude-code", map[string]any{"kind": "ticket-analysis", "mcpServers": []MCPServer{goldenServer()}})
	if !reflect.DeepEqual(got.Args, withoutSafeMode) {
		t.Fatalf("analysis argv with an extension changed:\n%q\nwant\n%q", got.Args, withoutSafeMode)
	}
	for _, forbidden := range []string{"--safe-mode", "--disallowedTools"} {
		if argIndex(got.Args, forbidden) >= 0 {
			t.Fatalf("analysis with an extension carries %s", forbidden)
		}
	}

	safe := append(append([]string{}, head...), "--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--json-schema", schema,
		"--safe-mode", "--no-chrome", "--disable-slash-commands")
	for name, request := range map[string]map[string]any{
		"absent": {"kind": "ticket-analysis"},
		"empty":  {"kind": "ticket-analysis", "mcpServers": []MCPServer{}},
	} {
		plain := build(t, "claude-code", request)
		if !reflect.DeepEqual(plain.Args, safe) {
			t.Fatalf("%s: analysis argv without extensions changed:\n%q\nwant\n%q", name, plain.Args, safe)
		}
		if argIndex(plain.Args, "--setting-sources") >= 0 || argIndex(plain.Args, "--allowedTools") >= 0 {
			t.Fatalf("%s: analysis without extensions gained flags: %q", name, plain.Args)
		}
	}
}

func TestInvocationWithoutExtensionsIsUnchanged(t *testing.T) {
	for _, id := range []string{"codex", "claude-code", "cursor"} {
		for _, base := range []map[string]any{codingRequest(nil), {"kind": "ticket-analysis"}, {"kind": "formula-improvement"}} {
			plain, plainErr := BuildInvocation(id, base, "/fixture/result")
			for name, empty := range map[string]any{"nil": nil, "typed nil": []MCPServer(nil), "empty": []MCPServer{}, "decoded empty": []any{}} {
				request := map[string]any{"mcpServers": empty}
				for k, v := range base {
					request[k] = v
				}
				got, err := BuildInvocation(id, request, "/fixture/result")
				if (err == nil) != (plainErr == nil) || (err != nil && err.Error() != plainErr.Error()) || !reflect.DeepEqual(got, plain) {
					t.Fatalf("%s %v with %s mcpServers differs from a request without the key:\n%+v %v\n%+v %v", id, base["kind"], name, got, err, plain, plainErr)
				}
			}
		}
	}
}

func TestOtherAdaptersRejectExtensions(t *testing.T) {
	var decoded []any
	if err := json.Unmarshal([]byte(`[{"name":"graphify","command":"/priv/graphify","args":[],"tools":["query_graph"]}]`), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"codex", "cursor"} {
		for _, kind := range []string{"molecule", "ticket-analysis", "formula-improvement"} {
			for name, servers := range map[string]any{"typed": []MCPServer{goldenServer()}, "decoded": decoded} {
				call, err := BuildInvocation(id, map[string]any{"kind": kind, "mcpServers": servers}, "/fixture/result")
				if err == nil || err.Error() != "extensions are not supported for "+id+" in v1" || call.Args != nil || call.Input != "" {
					t.Fatalf("%s %s (%s): %+v %v", id, kind, name, call, err)
				}
			}
		}
	}
	if got := AdapterCapabilities("claude-code").Extensions.State; got != "supported" {
		t.Fatalf("capability probing changed: %s", got)
	}
}

func TestFormulaImprovementRejectsExtensions(t *testing.T) {
	call, err := BuildInvocation("claude-code", map[string]any{"kind": "formula-improvement", "mcpServers": []MCPServer{goldenServer()}}, "/fixture/result")
	if err == nil || call.Args != nil || call.Input != "" {
		t.Fatalf("meta received extensions: %+v %v", call, err)
	}
}

func TestExtensionInvocationIsDeterministic(t *testing.T) {
	alpha := MCPServer{Name: "alpha", Command: "/priv/alpha", Args: []string{"--b", "--a"}, Tools: []string{"zeta", "beta", "alpha_one"}}
	beta := MCPServer{Name: "beta-2", Command: "/priv/beta", Tools: []string{"read"}}
	config := `{"mcpServers":{"alpha":{"args":["--b","--a"],"command":"/priv/alpha","env":{}},"beta-2":{"args":[],"command":"/priv/beta","env":{}}}}`
	tools := "mcp__alpha__alpha_one,mcp__alpha__beta,mcp__alpha__zeta,mcp__beta-2__read"

	inputs := [][]MCPServer{{alpha, beta}, {beta, alpha}}
	reversed := alpha
	reversed.Tools = []string{"alpha_one", "beta", "zeta"}
	inputs = append(inputs, []MCPServer{beta, reversed})
	for _, kind := range []string{"molecule", "ticket-analysis"} {
		var first Invocation
		for i, servers := range inputs {
			request := map[string]any{"kind": kind, "mcpServers": servers}
			if kind == "molecule" {
				request = codingRequest(map[string]any{"mcpServers": servers})
			}
			call := build(t, "claude-code", request)
			if i == 0 {
				first = call
				if got := flagValue(t, call.Args, "--mcp-config"); got != config {
					t.Fatalf("%s config %s", kind, got)
				}
				if got := flagValue(t, call.Args, "--allowedTools"); !strings.HasSuffix(got, tools) {
					t.Fatalf("%s tools %s", kind, got)
				}
			}
			if !reflect.DeepEqual(call, first) {
				t.Fatalf("%s permuted input %d changed the invocation:\n%q\n%q", kind, i, call.Args, first.Args)
			}
		}
	}
	if !reflect.DeepEqual(alpha.Tools, []string{"zeta", "beta", "alpha_one"}) || !reflect.DeepEqual(inputs[0][0].Tools, alpha.Tools) {
		t.Fatalf("the caller's tool order was modified: %q", alpha.Tools)
	}
}

func TestExtensionServersFailClosed(t *testing.T) {
	good := func(mutate func(*MCPServer)) []MCPServer {
		s := goldenServer()
		mutate(&s)
		return []MCPServer{s}
	}
	long := func(n int) string { return strings.Repeat("a", n) }
	entry := func(fields string) []any {
		var decoded []any
		if err := json.Unmarshal([]byte("["+fields+"]"), &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	// A JSON string escape, assembled in pieces so the source holds no raw control byte.
	nulEscape := "\\" + "u0000"
	cases := map[string]any{
		"empty name":           good(func(s *MCPServer) { s.Name = "" }),
		"uppercase name":       good(func(s *MCPServer) { s.Name = "Graphify" }),
		"digit-leading name":   good(func(s *MCPServer) { s.Name = "1graph" }),
		"underscore name":      good(func(s *MCPServer) { s.Name = "graph_ify" }),
		"separator name":       good(func(s *MCPServer) { s.Name = "a__b" }),
		"overlong name":        good(func(s *MCPServer) { s.Name = long(33) }),
		"empty command":        good(func(s *MCPServer) { s.Command = "" }),
		"blank command":        good(func(s *MCPServer) { s.Command = "  " }),
		"bare command":         good(func(s *MCPServer) { s.Command = "graphify" }),
		"dot-relative command": good(func(s *MCPServer) { s.Command = "./x" }),
		"unclean command":      good(func(s *MCPServer) { s.Command = "/priv/../graphify" }),
		"trailing slash":       good(func(s *MCPServer) { s.Command = "/priv/graphify/" }),
		"NUL in command":       good(func(s *MCPServer) { s.Command = "/priv/graph" + string(rune(0)) + "ify" }),
		"newline in command":   good(func(s *MCPServer) { s.Command = "/priv/graph\nify" }),
		"NUL in arg":           good(func(s *MCPServer) { s.Args = []string{"mcp", "--graph" + string(rune(0)) + "/x"} }),
		"control char in arg":  good(func(s *MCPServer) { s.Args = []string{"mcp" + string(rune(27)) + "[31m"} }),
		"newline in arg":       good(func(s *MCPServer) { s.Args = []string{"a\nb"} }),
		"DEL in arg":           good(func(s *MCPServer) { s.Args = []string{"a" + string(rune(127)) + "b"} }),
		"no tools":             good(func(s *MCPServer) { s.Tools = nil }),
		"empty tool list":      good(func(s *MCPServer) { s.Tools = []string{} }),
		"empty tool":           good(func(s *MCPServer) { s.Tools = []string{""} }),
		"comma tool":           good(func(s *MCPServer) { s.Tools = []string{"a,b"} }),
		"spaced tool":          good(func(s *MCPServer) { s.Tools = []string{"query graph"} }),
		"wildcard tool":        good(func(s *MCPServer) { s.Tools = []string{"*"} }),
		"overlong tool":        good(func(s *MCPServer) { s.Tools = []string{long(65)} }),
		"duplicate tool":       good(func(s *MCPServer) { s.Tools = []string{"a", "b", "a"} }),
		"combined name too long": good(func(s *MCPServer) {
			s.Tools = []string{long(50)}
		}),
		"combined name too long, long server": good(func(s *MCPServer) {
			s.Name = long(32)
			s.Tools = []string{long(26)}
		}),
		"duplicate server":     []MCPServer{goldenServer(), goldenServer()},
		"string":               "graphify",
		"number":               7,
		"map":                  map[string]any{"name": "graphify"},
		"string list":          []string{"graphify"},
		"decoded string entry": []any{"graphify"},
		"decoded null entry":   []any{nil},
		"decoded unknown key":  entry(`{"name":"graphify","command":"/x","tools":["t"],"env":{"K":"v"}}`),
		"decoded name type":    entry(`{"name":7,"command":"/x","tools":["t"]}`),
		"decoded args type":    entry(`{"name":"graphify","command":"/x","args":"mcp","tools":["t"]}`),
		"decoded tools type":   entry(`{"name":"graphify","command":"/x","tools":[7]}`),
		"decoded relative":     entry(`{"name":"graphify","command":"graphify","tools":["t"]}`),
		"decoded NUL in arg":   entry(`{"name":"graphify","command":"/x","args":["a` + nulEscape + `b"],"tools":["t"]}`),
	}
	for name, servers := range cases {
		for _, request := range []map[string]any{
			codingRequest(map[string]any{"mcpServers": servers}),
			{"kind": "ticket-analysis", "mcpServers": servers},
		} {
			if call, err := BuildInvocation("claude-code", request, "/fixture/result"); err == nil || call.Args != nil || call.Input != "" {
				t.Fatalf("%s (%v) was accepted: %+v", name, request["kind"], call)
			}
		}
	}
}

func TestExtensionToolNameAtTheLimitIsAccepted(t *testing.T) {
	// "mcp__" + "graphify" + "__" is 15 bytes, leaving 49 for the tool.
	tool := strings.Repeat("t", 49)
	server := goldenServer()
	server.Tools = []string{tool}
	got := build(t, "claude-code", map[string]any{"kind": "ticket-analysis", "mcpServers": []MCPServer{server}})
	if allowed := flagValue(t, got.Args, "--allowedTools"); allowed != "mcp__graphify__"+tool || len(allowed) != 64 {
		t.Fatalf("allowedTools %q", allowed)
	}
}

func TestExtensionServersStayOutOfThePrompt(t *testing.T) {
	plain := build(t, "claude-code", codingRequest(nil))
	request := codingRequest(map[string]any{"mcpServers": []MCPServer{goldenServer()}})
	with := build(t, "claude-code", request)
	if with.Input != plain.Input {
		t.Fatalf("private server paths reached the model-visible prompt:\n%s", with.Input)
	}
	for _, private := range []string{"mcpServers", "/priv/graphify", "/cache/graph.json"} {
		if strings.Contains(with.Input, private) {
			t.Fatalf("prompt contains %q", private)
		}
	}
	if _, ok := request["mcpServers"]; !ok {
		t.Fatal("BuildInvocation modified the caller's request")
	}
}

func argIndex(args []string, flag string) int {
	for i, arg := range args {
		if arg == flag {
			return i
		}
	}
	return -1
}

func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	i := argIndex(args, flag)
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no %s value in %q", flag, args)
	}
	return args[i+1]
}
