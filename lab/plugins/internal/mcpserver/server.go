// Package mcpserver is a small Model Context Protocol server kit for
// first-party Forgecell plugins over the stdio transport: newline-delimited
// JSON-RPC 2.0 with initialize, tools/list and tools/call, the framing the
// engine's client in internal/plugin/mcp speaks. Every incoming message is
// bounded, handlers run one at a time under their own timeout, every handler
// failure maps to one fixed reason, and nothing but protocol messages is ever
// written to stdout.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

const (
	// DefaultHandlerTimeout bounds one handler call when SetHandlerTimeout was
	// not called. It is the longest contract tool timeout.
	DefaultHandlerTimeout = 120 * time.Second
	// DefaultMaxResponseBytes bounds one outgoing message: the engine's client
	// never reads more.
	DefaultMaxResponseBytes = mcp.DefaultMaxMessageBytes
	// maxTools bounds the tools one server registers, as the client bounds
	// tools/list.
	maxTools = 128
	// maxSchemaBytes bounds one tool's input schema.
	maxSchemaBytes = 64 << 10
	// maxInfoBytes bounds the server name, version and contract id.
	maxInfoBytes = 64
	// maxDiagnosticBytes bounds everything one Server writes to stderr;
	// maxDiagnosticRunes bounds one diagnostic line.
	maxDiagnosticBytes = 64 << 10
	maxDiagnosticRunes = 300
	// truncatedLine ends the diagnostics once their bound is reached; the bound
	// always leaves room for it.
	truncatedLine = "mcpserver: diagnostics truncated\n"
)

var (
	toolPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	infoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/+-]*$`)
)

// Handler serves one tool call. args is the call's arguments object. The result
// must encode as a JSON object; an error is answered as an isError result whose
// reason is the Fail reason, or failed for any other error.
type Handler func(ctx context.Context, args json.RawMessage) (any, error)

type tool struct {
	name    string
	schema  json.RawMessage
	handler Handler
	// maxOutput bounds the handler result's structured JSON; zero leaves only
	// the response bound.
	maxOutput int
}

// ToolOption configures one tool at registration.
type ToolOption func(*tool)

// MaxOutputBytes bounds the structured JSON of the tool's results, as a
// contract tool's maxOutputBytes does. A larger result is never sent: the call
// fails with reason failed. n must be positive.
func MaxOutputBytes(n int) ToolOption {
	if n <= 0 {
		panic("mcpserver: the output bound must be positive")
	}
	return func(t *tool) { t.maxOutput = n }
}

// Server is one MCP server. Configure it before Serve; Serve runs one session.
type Server struct {
	name, version, contract string

	mu         sync.Mutex
	tools      []tool
	byName     map[string]int
	maxIn      int
	maxOut     int
	timeout    time.Duration
	stderr     io.Writer
	secretEnv  []string
	serving    bool
	diagBudget int

	// handlerContext derives one handler's context; a seam for tests.
	handlerContext func(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc)
	// running is closed when the last handler returns; nil when none ran.
	running chan struct{}
}

// New returns a server that reports name and version in initialize and
// declares contractID (for example forgecell.forge/v1) as
// capabilities.experimental.forgecell.contract. An empty contractID declares
// none. Invalid values are a programming error and panic.
func New(name, version, contractID string) *Server {
	for label, v := range map[string]string{"name": name, "version": version} {
		if len(v) > maxInfoBytes || !infoPattern.MatchString(v) {
			panic("mcpserver: invalid server " + label)
		}
	}
	if contractID != "" && (len(contractID) > maxInfoBytes || !infoPattern.MatchString(contractID)) {
		panic("mcpserver: invalid contract id")
	}
	return &Server{
		name: name, version: version, contract: contractID,
		byName:         map[string]int{},
		maxIn:          DefaultMaxMessageBytes,
		maxOut:         DefaultMaxResponseBytes,
		timeout:        DefaultHandlerTimeout,
		stderr:         os.Stderr,
		diagBudget:     maxDiagnosticBytes,
		handlerContext: context.WithTimeout,
	}
}

// Tool registers a tool with its JSON input schema, which must be a JSON object.
// Tools are listed in registration order. An invalid or repeated name, a schema
// that is not a bounded JSON object, a nil handler or more than 128 tools is a
// programming error and panics. Options such as MaxOutputBytes bound the tool's
// results.
func (s *Server) Tool(name string, inputSchemaJSON []byte, handler Handler, opts ...ToolOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.serving:
		panic("mcpserver: Tool called while serving")
	case !toolPattern.MatchString(name):
		panic("mcpserver: invalid tool name")
	case s.byName[name] != 0:
		panic("mcpserver: tool " + name + " registered twice")
	case handler == nil:
		panic("mcpserver: tool " + name + " has no handler")
	case len(s.tools) == maxTools:
		panic("mcpserver: more than 128 tools")
	case len(inputSchemaJSON) > maxSchemaBytes:
		panic("mcpserver: tool " + name + " has an oversized input schema")
	}
	if _, ok := object(inputSchemaJSON); !ok || !json.Valid(inputSchemaJSON) {
		panic("mcpserver: tool " + name + " input schema is not a JSON object")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, inputSchemaJSON); err != nil {
		panic("mcpserver: tool " + name + " input schema is not a JSON object")
	}
	t := tool{name: name, schema: compact.Bytes(), handler: handler}
	for _, opt := range opts {
		opt(&t)
	}
	s.tools = append(s.tools, t)
	s.byName[name] = len(s.tools)
}

// SetMaxMessageBytes sets the bound on one incoming message, excluding its
// newline. Zero or less keeps DefaultMaxMessageBytes; the bound is at most
// 16 MiB.
func (s *Server) SetMaxMessageBytes(n int) {
	switch {
	case n <= 0:
		n = DefaultMaxMessageBytes
	case n > maxConfigurableBytes:
		n = maxConfigurableBytes
	}
	s.configure(func() { s.maxIn = n })
}

// SetHandlerTimeout sets each handler call's timeout. Zero or less keeps
// DefaultHandlerTimeout.
func (s *Server) SetHandlerTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultHandlerTimeout
	}
	s.configure(func() { s.timeout = d })
}

// SetStderr sets where diagnostics go; the default is os.Stderr. Diagnostics
// are bounded and redacted, and never go to stdout.
func (s *Server) SetStderr(w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	s.configure(func() { s.stderr = w })
}

// SecretEnv declares env names whose values are secrets: they are redacted from
// every diagnostic and every message sent back to the client.
func (s *Server) SecretEnv(names ...string) {
	s.configure(func() { s.secretEnv = append(s.secretEnv, names...) })
}

// configure changes the configuration, which is fixed while Serve runs so the
// session reads it without locking.
func (s *Server) configure(change func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serving {
		panic("mcpserver: configured while serving")
	}
	change()
}

// Log writes one bounded, redacted diagnostic line to stderr.
func (s *Server) Log(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logLocked(msg)
}

func (s *Server) logLocked(msg string) {
	if s.diagBudget <= 0 {
		return
	}
	line := "mcpserver: " + sanitize(msg, maxDiagnosticRunes, s.secretEnv...) + "\n"
	if len(line)+len(truncatedLine) > s.diagBudget {
		line = truncatedLine
		s.diagBudget = 0
	} else {
		s.diagBudget -= len(line)
	}
	io.WriteString(s.stderr, line)
}

// frame is one message from the reader goroutine.
type frame struct {
	line     []byte
	tooLarge bool
	err      error
}

// errOutput is returned when stdout cannot be written.
var errOutput = errors.New("mcpserver: writing a response failed")

// Serve answers requests from in on out until in ends, which returns nil, or ctx
// ends, which returns ctx's error. Messages are handled in order and one handler
// runs at a time. A malformed, oversized or unknown message is answered with a
// JSON-RPC error and serving continues. Serve runs one session at a time.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.mu.Lock()
	if s.serving {
		s.mu.Unlock()
		return errors.New("mcpserver: Serve is already running")
	}
	s.serving = true
	maxIn := s.maxIn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.serving = false
		s.mu.Unlock()
	}()

	frames := make(chan frame)
	stop := make(chan struct{})
	defer close(stop)
	// The reader goroutine ends at the end of in; when ctx ends first it stops
	// at its next message, since a blocked read cannot be interrupted.
	go func() {
		r := newLineReader(in)
		for {
			line, tooLarge, err := r.read(maxIn)
			select {
			case frames <- frame{line, tooLarge, err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		var f frame
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f = <-frames:
		}
		switch {
		case errors.Is(f.err, io.EOF):
			return nil
		case f.err != nil:
			s.Log("reading a request failed")
			return errors.New("mcpserver: reading a request failed")
		}
		reply := s.handle(ctx, f)
		if reply == nil {
			continue
		}
		line, err := encode(reply)
		if err == nil && len(line)-1 > s.maxOut {
			line, err = encode(errorMessage(reply["id"].(json.RawMessage), codeInternalError, "the response exceeds the message bound"))
			s.Log("a response exceeded the message bound")
		}
		if err != nil {
			s.Log("a response cannot be encoded")
			return errOutput
		}
		if _, err := out.Write(line); err != nil {
			s.Log("writing a response failed")
			return errOutput
		}
	}
}

// handle answers one frame; nil means no reply.
func (s *Server) handle(ctx context.Context, f frame) map[string]any {
	if f.tooLarge {
		s.Log(fmt.Sprintf("refused a message over %d bytes", s.maxIn))
		return errorMessage(nil, codeInvalidRequest, "the message exceeds the size bound")
	}
	if len(bytes.TrimSpace(f.line)) == 0 {
		return nil
	}
	req, err := parse(f.line)
	if err != nil {
		var pe *parseError
		errors.As(err, &pe)
		s.Log("refused a message: " + pe.why)
		return errorMessage(pe.id, pe.code, map[int]string{codeParseError: "parse error", codeInvalidRequest: "invalid request"}[pe.code])
	}
	if req.isNotification() {
		if req.method != "notifications/initialized" {
			s.Log("ignored the notification " + req.method)
		}
		return nil
	}
	params, ok := object(req.params)
	if !ok {
		return errorMessage(req.id, codeInvalidParams, "params is not an object")
	}
	switch req.method {
	case "initialize":
		return resultMessage(req.id, s.initializeResult())
	case "ping":
		return resultMessage(req.id, map[string]any{})
	case "tools/list":
		return resultMessage(req.id, s.listResult())
	case "tools/call":
		return s.call(ctx, req.id, params)
	}
	s.Log("refused the unknown method " + req.method)
	return errorMessage(req.id, codeMethodNotFound, "method not found")
}

func (s *Server) initializeResult() map[string]any {
	capabilities := map[string]any{"tools": map[string]any{}}
	if s.contract != "" {
		capabilities["experimental"] = map[string]any{"forgecell": map[string]any{"contract": s.contract}}
	}
	return map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"capabilities":    capabilities,
		"serverInfo":      map[string]any{"name": s.name, "version": s.version},
	}
}

func (s *Server) listResult() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	tools := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, map[string]any{"name": t.name, "inputSchema": t.schema})
	}
	return map[string]any{"tools": tools}
}

// call validates a tools/call request and runs its handler.
func (s *Server) call(ctx context.Context, id json.RawMessage, params map[string]json.RawMessage) map[string]any {
	var name string
	if err := json.Unmarshal(params["name"], &name); err != nil || !toolPattern.MatchString(name) {
		return errorMessage(id, codeInvalidParams, "the tool name is invalid")
	}
	s.mu.Lock()
	index := s.byName[name]
	var t tool
	if index > 0 {
		t = s.tools[index-1]
	}
	s.mu.Unlock()
	if index == 0 {
		return errorMessage(id, codeInvalidParams, "unknown tool")
	}
	args := params["arguments"]
	if args == nil || bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
		args = json.RawMessage("{}")
	}
	if _, ok := object(args); !ok {
		return errorMessage(id, codeInvalidParams, "arguments is not an object")
	}
	return resultMessage(id, s.run(ctx, t, args))
}

type outcome struct {
	result any
	err    error
}

// run calls one handler under its timeout and renders the tools/call result. A
// handler that ignores its context and outlives its timeout keeps the server
// from starting another until it returns: later calls fail meanwhile, so
// handlers never overlap.
func (s *Server) run(ctx context.Context, t tool, args json.RawMessage) map[string]any {
	if s.running != nil {
		select {
		case <-s.running:
		default:
			s.Log("refused a call to " + t.name + " while an earlier handler still runs")
			return s.failure(Failed, "")
		}
	}
	s.mu.Lock()
	timeout := s.timeout
	s.mu.Unlock()
	hctx, cancel := s.handlerContext(ctx, timeout)
	defer cancel()
	done := make(chan outcome, 1)
	running := make(chan struct{})
	s.running = running
	go func() {
		defer close(running)
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{err: errors.New("the handler panicked")}
			}
		}()
		result, err := t.handler(hctx, args)
		done <- outcome{result, err}
	}()
	var o outcome
	select {
	case o = <-done:
	case <-hctx.Done():
	}
	if hctx.Err() != nil {
		s.Log("the handler for " + t.name + " timed out")
		return s.failure(Failed, "")
	}
	if o.err != nil {
		reason, msg := reasonOf(o.err)
		// Log the raw text: Log redacts it whole before cutting, which a cut
		// Error() string would defeat.
		detail := msg
		var e *Error
		if !errors.As(o.err, &e) {
			detail = o.err.Error()
		}
		s.Log("the handler for " + t.name + " failed with " + string(reason) + ": " + detail)
		return s.failure(reason, msg)
	}
	structured, err := json.Marshal(o.result)
	if _, ok := object(structured); err != nil || !ok {
		s.Log("the handler for " + t.name + " returned no JSON object")
		return s.failure(Failed, "")
	}
	if t.maxOutput > 0 && len(structured) > t.maxOutput {
		s.Log(fmt.Sprintf("the handler for %s returned %d bytes, over its %d-byte output bound", t.name, len(structured), t.maxOutput))
		return s.failure(Failed, "")
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(structured)}},
		"structuredContent": json.RawMessage(structured),
	}
}

// failure renders an isError result with a fixed reason and a sanitized
// message.
func (s *Server) failure(reason Reason, msg string) map[string]any {
	msg = sanitize(msg, maxMessageRunes, s.secretEnv...)
	text := string(reason)
	if msg != "" {
		text += ": " + msg
	}
	structured := map[string]any{"reason": string(reason)}
	if msg != "" {
		structured["message"] = msg
	}
	return map[string]any{
		"isError":           true,
		"content":           []any{map[string]any{"type": "text", "text": strings.TrimSpace(text)}},
		"structuredContent": structured,
	}
}
