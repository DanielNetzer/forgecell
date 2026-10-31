// Command fakeserver is an MCP stdio server for tests: it serves one plugin
// contract and misbehaves on request. It never touches the network and never
// sleeps; the slow mode simply never answers.
//
// Usage: fakeserver --kind tracker|forge|reviewer --mode MODE, where MODE is
// good, slow, crash, oversized, bad-framing, wrong-contract, server-request,
// missing-tool, bad-input-crash or wrong-id.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// readyMarker is written to stderr once the server is serving.
const readyMarker = "fake-mcp-ready"

// plantedSecret is written to stderr so tests can check that diagnostics
// redact it and errors never carry it.
const plantedSecret = "Authorization: Bearer fakefakefakefake0123456789"

type server struct {
	mode     string
	contract *contract.Contract
	in       *bufio.Reader
	out      *bufio.Writer
}

func main() {
	kind := flag.String("kind", "", "contract kind")
	mode := flag.String("mode", "good", "behaviour")
	flag.Parse()
	c, ok := contract.Lookup(*kind)
	if !ok {
		fmt.Fprintln(os.Stderr, "fake: unknown kind")
		os.Exit(2)
	}
	s := &server{mode: *mode, contract: c, in: bufio.NewReaderSize(os.Stdin, 1<<20), out: bufio.NewWriter(os.Stdout)}
	reportEnvironment()
	fmt.Fprintln(os.Stderr, "debug: "+plantedSecret)
	fmt.Fprintln(os.Stderr, readyMarker)
	s.serve()
}

// reportEnvironment writes the environment's names (never values) and whether
// HOME is the working directory, for launch tests.
func reportEnvironment() {
	var names []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintln(os.Stderr, "fake-env: "+strings.Join(names, ","))
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	info, err := os.Stat(home)
	fmt.Fprintf(os.Stderr, "fake-home: dir=%t cwd=%t\n", err == nil && info.IsDir(), home != "" && home == cwd)
}

func (s *server) serve() {
	for {
		line, err := s.in.ReadBytes('\n')
		if err != nil {
			if s.mode == "slow" {
				blockForever()
			}
			fmt.Fprintln(os.Stderr, "fake: stdin closed")
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			s.initialize(req.ID)
		case "tools/list":
			s.list(req.ID)
		case "tools/call":
			s.call(req.ID, req.Params)
		default:
			if req.ID != nil {
				s.fail(req.ID, -32601, "unknown method")
			}
		}
	}
}

func (s *server) write(v any) {
	raw, _ := json.Marshal(v)
	s.out.Write(append(raw, '\n'))
	s.out.Flush()
}

func (s *server) reply(id json.RawMessage, result any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *server) fail(id json.RawMessage, code int, message string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (s *server) initialize(id json.RawMessage) {
	switch s.mode {
	case "bad-framing":
		s.out.WriteString("{not json\n")
		s.out.Flush()
		return
	case "server-request":
		s.write(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "method": "sampling/createMessage", "params": map[string]any{}})
		s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "info"}})
		if !s.refusedServerRequest() {
			s.fail(id, -32000, "the client acted on a server request")
			return
		}
	}
	declared := s.contract.ID
	if s.mode == "wrong-contract" {
		declared = strings.TrimSuffix(declared, "/v1") + "/v2"
	}
	s.reply(id, map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}, "experimental": map[string]any{"forgecell": map[string]any{"contract": declared}}},
		"serverInfo":      map[string]any{"name": "fake", "version": "1"},
	})
}

// refusedServerRequest reads the client's next message and reports whether it
// is a JSON-RPC error answering srv-1.
func (s *server) refusedServerRequest() bool {
	line, err := s.in.ReadBytes('\n')
	if err != nil {
		return false
	}
	var answer struct {
		ID     string              `json:"id"`
		Error  *struct{ Code int } `json:"error"`
		Result json.RawMessage     `json:"result"`
	}
	ok := json.Unmarshal(line, &answer) == nil && answer.ID == "srv-1" && answer.Error != nil && answer.Result == nil
	fmt.Fprintf(os.Stderr, "fake: server request refused=%t\n", ok)
	return ok
}

func (s *server) list(id json.RawMessage) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "debug"}})
	tools := []map[string]any{}
	for i, t := range s.contract.Tools {
		if s.mode == "missing-tool" && i == len(s.contract.Tools)-1 {
			continue
		}
		tools = append(tools, map[string]any{"name": t.Name, "description": "fake " + t.Name, "inputSchema": t.InputSchema()})
	}
	if s.mode == "wrong-id" {
		id = json.RawMessage("4242")
	}
	s.reply(id, map[string]any{"tools": tools})
}

// blockForever parks the server on a pipe nobody writes, which the Go runtime
// does not report as a deadlock.
var held *os.File

func blockForever() {
	r, w, err := os.Pipe()
	if err != nil {
		os.Exit(5)
	}
	held = w
	buf := make([]byte, 1)
	r.Read(buf)
	os.Exit(5)
}
