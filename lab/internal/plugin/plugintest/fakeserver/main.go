// Command fakeserver is an MCP stdio server for tests: it serves one plugin
// contract and misbehaves on request. It never touches the network and never
// sleeps; the slow mode simply never answers.
//
// Usage: fakeserver --kind tracker|forge|reviewer --mode MODE, where MODE is
// good, slow, crash, oversized, bad-framing, wrong-contract, server-request,
// missing-tool, bad-input-crash, wrong-id, wrong-protocol, dirty-contract,
// zero-width-contract, silent, silent-list, grandchild, locked-home or unmark.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// readyMarker is written to stderr once the server is serving.
const readyMarker = "fake-mcp-ready"

// grandchildMarker is written to stderr once the grandchild mode's background
// child is running.
const grandchildMarker = "fake-grandchild-ready"

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
	grandchild := flag.Bool("grandchild", false, "run as the grandchild mode's background child")
	flag.Parse()
	if *grandchild {
		holdStderr()
	}
	c, ok := contract.Lookup(*kind)
	if !ok {
		fmt.Fprintln(os.Stderr, "fake: unknown kind")
		os.Exit(2)
	}
	s := &server{mode: *mode, contract: c, in: bufio.NewReaderSize(os.Stdin, 1<<20), out: bufio.NewWriter(os.Stdout)}
	reportEnvironment()
	fmt.Fprintln(os.Stderr, "debug: "+plantedSecret)
	if err := s.prepare(); err != nil {
		fmt.Fprintln(os.Stderr, "fake: preparing the mode failed")
		os.Exit(7)
	}
	fmt.Fprintln(os.Stderr, readyMarker)
	s.serve()
}

// prepare sets up the modes that act before serving: grandchild leaves a
// background child in the process group holding stderr; locked-home leaves a
// read-only directory tree and a symlink to FAKE_OUTSIDE (when set) in HOME;
// unmark removes the launcher's run marker beside HOME.
func (s *server) prepare() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	switch s.mode {
	case "grandchild":
		return spawnGrandchild()
	case "locked-home":
		sub := filepath.Join(home, "locked", "sub")
		if err := os.MkdirAll(sub, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(sub, "file"), []byte("x"), 0o600); err != nil {
			return err
		}
		if outside := os.Getenv("FAKE_OUTSIDE"); outside != "" {
			if err := os.Symlink(outside, filepath.Join(home, "outside")); err != nil {
				return err
			}
		}
		if err := os.Chmod(sub, 0); err != nil {
			return err
		}
		return os.Chmod(filepath.Dir(sub), 0o500)
	case "unmark":
		return os.Remove(filepath.Join(filepath.Dir(home), ".forgecell-plugin-run"))
	}
	return nil
}

// spawnGrandchild starts this executable again as a background child that
// inherits stderr, waits until it runs and leaves it running.
func spawnGrandchild() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "--grandchild")
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "ready\n" {
		return fmt.Errorf("the grandchild did not start")
	}
	fmt.Fprintf(os.Stderr, "%s pid=%d\n", grandchildMarker, cmd.Process.Pid)
	return nil
}

// holdStderr is the grandchild: it reports readiness on stdout, then holds the
// inherited stderr open forever.
func holdStderr() {
	fmt.Fprintln(os.Stdout, "ready")
	os.Stdout.Close()
	blockForever()
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
	case "silent":
		return
	}
	declared := s.contract.ID
	switch s.mode {
	case "wrong-contract":
		declared = strings.TrimSuffix(declared, "/v1") + "/v2"
	case "dirty-contract":
		declared = strings.TrimSuffix(declared, "/v1") + "/v2\xe2\x80\xae\x07\xe2\x80\xa8x"
	case "zero-width-contract":
		declared += "​"
	}
	protocol := mcp.ProtocolVersion
	if s.mode == "wrong-protocol" {
		protocol = "2024-11-05"
	}
	s.reply(id, map[string]any{
		"protocolVersion": protocol,
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
	if s.mode == "silent-list" {
		return
	}
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
