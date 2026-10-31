package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"sync"

	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// ProtocolVersion is the MCP revision this client speaks.
const ProtocolVersion = "2025-06-18"

// Bounds of one session.
const (
	// maxIgnored bounds the notifications, blank lines and refused server
	// requests one call may skip before its response.
	maxIgnored = 64
	// ignoredBudget bounds the bytes those skipped messages may add to a call.
	ignoredBudget = 1 << 20
	maxInitBytes  = 1 << 20
	maxListBytes  = 1 << 20
	maxListPages  = 8
	maxListTools  = 128
	maxCursor     = 1024
	maxInfoRunes  = 100
)

var listedToolPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// ServerInfo is what the server reported in initialize, sanitized.
type ServerInfo struct {
	ProtocolVersion string `json:"protocolVersion"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	// Contract is capabilities.experimental.forgecell.contract, the Forgecell
	// contract the server says it serves.
	Contract string `json:"contract"`
}

// ToolInfo is one tool from tools/list.
type ToolInfo struct {
	Name        string
	InputSchema json.RawMessage
}

// Client speaks MCP over one reader and writer, usually a plugin's stdout and
// stdin. One request is outstanding at a time. A timeout, an oversized or
// malformed message, an unexpected response id or the end of the server's
// output ends the session: later calls fail without touching the pipes.
type Client struct {
	mu          sync.Mutex
	in          *lineReader
	out         io.Writer
	max         int
	next        int64
	broken      bool
	initialized bool
	server      ServerInfo
}

// NewClient returns a client that reads responses from r and writes requests to
// w. maxMessageBytes lowers DefaultMaxMessageBytes; zero keeps the default.
func NewClient(r io.Reader, w io.Writer, maxMessageBytes int) *Client {
	if maxMessageBytes <= 0 || maxMessageBytes > DefaultMaxMessageBytes {
		maxMessageBytes = DefaultMaxMessageBytes
	}
	return &Client{in: newLineReader(r), out: w, max: maxMessageBytes}
}

// Initialize runs the initialize handshake, records what the server reported and
// sends notifications/initialized.
func (c *Client) Initialize(ctx context.Context) (ServerInfo, error) {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "forgecell", "version": "1"},
	}
	raw, err := c.exchange(ctx, "initialize", "initialize", params, maxInitBytes)
	if err != nil {
		return ServerInfo{}, err
	}
	invalid := func(why string) (ServerInfo, error) { return ServerInfo{}, Fail(SchemaInvalid, "initialize", why) }
	result, err := object(raw)
	if err != nil {
		return invalid("the initialize result is not an object")
	}
	var info ServerInfo
	if err := json.Unmarshal(result["protocolVersion"], &info.ProtocolVersion); err != nil || info.ProtocolVersion == "" || len(info.ProtocolVersion) > 64 {
		return invalid("the initialize result has no protocolVersion")
	}
	info.ProtocolVersion = safetext.Text(info.ProtocolVersion, 64)
	if raw, ok := result["serverInfo"]; ok {
		si, err := object(raw)
		if err != nil {
			return invalid("serverInfo is not an object")
		}
		info.Name, _ = optionalString(si["name"])
		info.Version, _ = optionalString(si["version"])
		info.Name, info.Version = safetext.Text(info.Name, maxInfoRunes), safetext.Text(info.Version, maxInfoRunes)
	}
	contract, ok := declaredContract(result["capabilities"])
	if !ok {
		return invalid("capabilities.experimental.forgecell.contract is malformed")
	}
	info.Contract = contract
	line, err := encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, c.max)
	if err != nil {
		return ServerInfo{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.out.Write(line); err != nil {
		c.broken = true
		return ServerInfo{}, Fail(Failed, "initialize", "writing to the server failed")
	}
	c.initialized, c.server = true, info
	return info, nil
}

// declaredContract reads capabilities.experimental.forgecell.contract. An absent
// value is "" and ok; a present one of the wrong type is not ok.
func declaredContract(raw json.RawMessage) (string, bool) {
	if raw == nil {
		return "", true
	}
	caps, err := object(raw)
	if err != nil {
		return "", false
	}
	if caps["experimental"] == nil {
		return "", true
	}
	experimental, err := object(caps["experimental"])
	if err != nil {
		return "", false
	}
	if experimental["forgecell"] == nil {
		return "", true
	}
	forgecell, err := object(experimental["forgecell"])
	if err != nil {
		return "", false
	}
	contract, ok := optionalString(forgecell["contract"])
	if !ok || len(contract) > 64 {
		return "", false
	}
	return contract, true
}

// ListTools reads every page of tools/list, bounded in pages and tools.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	if err := c.requireInitialized("tools/list"); err != nil {
		return nil, err
	}
	var tools []ToolInfo
	seen := map[string]bool{}
	cursors := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		if page == maxListPages {
			return nil, Fail(OutputTooLarge, "tools/list", fmt.Sprintf("tools/list has more than %d pages", maxListPages))
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.exchange(ctx, "tools/list", "tools/list", params, maxListBytes)
		if err != nil {
			return nil, err
		}
		invalid := func(why string) ([]ToolInfo, error) { return nil, Fail(SchemaInvalid, "tools/list", why) }
		result, err := object(raw)
		if err != nil {
			return invalid("the tools/list result is not an object")
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(result["tools"], &entries); err != nil {
			return invalid("the tools/list result has no tools array")
		}
		for _, entry := range entries {
			tool, err := object(entry)
			if err != nil {
				return invalid("a listed tool is not an object")
			}
			name, ok := optionalString(tool["name"])
			if !ok || !listedToolPattern.MatchString(name) {
				return invalid("a listed tool has an invalid name")
			}
			if seen[name] {
				return invalid("a tool is listed twice")
			}
			seen[name] = true
			schema := tool["inputSchema"]
			if _, err := object(schema); err != nil {
				return invalid("a listed tool has no inputSchema object")
			}
			if len(tools) == maxListTools {
				return nil, Fail(OutputTooLarge, "tools/list", fmt.Sprintf("tools/list lists more than %d tools", maxListTools))
			}
			tools = append(tools, ToolInfo{Name: name, InputSchema: schema})
		}
		next, ok := optionalString(result["nextCursor"])
		if !ok || len(next) > maxCursor {
			return invalid("nextCursor is not a bounded string")
		}
		if next == "" {
			return tools, nil
		}
		if cursors[next] {
			return invalid("nextCursor repeats an earlier page")
		}
		cursors[next], cursor = true, next
	}
}

// CallTool calls one tool and returns its structuredContent object. maxBytes
// lowers the message bound for this call. A result with isError is a Remote
// error whose reason is auth-missing, rate-limited or network when the
// structured reason names one, and failed otherwise.
func (c *Client) CallTool(ctx context.Context, name string, args any, maxBytes int) (json.RawMessage, error) {
	if !listedToolPattern.MatchString(name) {
		return nil, Fail(SchemaInvalid, "", "the tool name is invalid")
	}
	if err := c.requireInitialized(name); err != nil {
		return nil, err
	}
	raw, err := c.exchange(ctx, name, "tools/call", map[string]any{"name": name, "arguments": args}, maxBytes)
	if err != nil {
		return nil, err
	}
	result, err := object(raw)
	if err != nil {
		return nil, Fail(SchemaInvalid, name, "the tools/call result is not an object")
	}
	isError := false
	if v, ok := result["isError"]; ok {
		if err := json.Unmarshal(v, &isError); err != nil {
			return nil, Fail(SchemaInvalid, name, "isError is not a boolean")
		}
	}
	structured := result["structuredContent"]
	if isError {
		e := Fail(remoteReason(structured), name, "the tool reported an error")
		e.Remote = true
		return nil, e
	}
	if _, err := object(structured); err != nil {
		return nil, Fail(SchemaInvalid, name, "the result carries no structuredContent object")
	}
	return structured, nil
}

// Server returns what initialize recorded.
func (c *Client) Server() ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.server
}

func (c *Client) requireInitialized(label string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized {
		return Fail(Failed, label, "the session is not initialized")
	}
	return nil
}

type outcome struct {
	result json.RawMessage
	err    error
}

// exchange sends one request and waits for its response or ctx. The round trip
// runs in its own goroutine so a server that never answers, or never reads,
// still yields Timeout; the session then ends, and closing the pipes ends the
// goroutine.
func (c *Client) exchange(ctx context.Context, label, method string, params any, limit int) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken {
		return nil, Fail(Failed, label, "the session ended after an earlier failure")
	}
	if ctx.Err() != nil {
		return nil, Fail(Timeout, label, "the deadline passed before the request was sent")
	}
	if limit <= 0 || limit > c.max {
		limit = c.max
	}
	c.next++
	id := c.next
	line, err := encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}, c.max)
	if err != nil {
		return nil, label2(err, label)
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := c.roundTrip(id, line, limit)
		done <- outcome{result, err}
	}()
	select {
	case o := <-done:
		var e *Error
		if o.err != nil && !(errors.As(o.err, &e) && e.Remote) {
			c.broken = true
		}
		return o.result, label2(o.err, label)
	case <-ctx.Done():
		c.broken = true
		return nil, Fail(Timeout, label, "no response before the deadline")
	}
}

func label2(err error, label string) error {
	var e *Error
	if errors.As(err, &e) && e.Tool == "" {
		e.Tool = label
	}
	return err
}

// roundTrip writes the request and reads until its response. Notifications are
// ignored and server requests are refused with a JSON-RPC error, both bounded in
// count and bytes. Any other response id is SchemaInvalid.
func (c *Client) roundTrip(id int64, line []byte, limit int) (json.RawMessage, error) {
	if _, err := c.out.Write(line); err != nil {
		return nil, Fail(Failed, "", "writing to the server failed")
	}
	want := strconv.FormatInt(id, 10)
	budget := limit + ignoredBudget
	ignored := 0
	skip := func() error {
		if ignored++; ignored > maxIgnored {
			return Fail(OutputTooLarge, "", fmt.Sprintf("the server sent more than %d messages before its response", maxIgnored))
		}
		return nil
	}
	for {
		raw, err := c.in.read(min(limit, budget))
		if err != nil {
			return nil, err
		}
		budget -= len(raw) + 1
		if len(bytes.TrimSpace(raw)) == 0 {
			if err := skip(); err != nil {
				return nil, err
			}
			continue
		}
		m, err := parse(raw)
		if err != nil {
			return nil, err
		}
		if m.hasMethod {
			if err := skip(); err != nil {
				return nil, err
			}
			if m.isRequest() {
				refusal, _ := encode(map[string]any{"jsonrpc": "2.0", "id": echoID(m.id), "error": map[string]any{"code": -32601, "message": "Forgecell does not serve requests"}}, c.max)
				if _, err := c.out.Write(refusal); err != nil {
					return nil, Fail(Failed, "", "writing to the server failed")
				}
			}
			continue
		}
		if string(m.id) != want {
			return nil, Fail(SchemaInvalid, "", "a response answers an unknown or earlier request")
		}
		if m.err != nil {
			e := Fail(remoteReason(m.err.Data), "", fmt.Sprintf("JSON-RPC error %d: %s", m.err.Code, m.err.Message))
			e.Remote = true
			return nil, e
		}
		return m.result, nil
	}
}

// remoteReason maps a structured {"reason": …} in an error payload to one of
// the reasons a server may name; anything else is Failed.
func remoteReason(raw json.RawMessage) Reason {
	fields, err := object(raw)
	if err != nil {
		return Failed
	}
	reason, _ := optionalString(fields["reason"])
	switch Reason(reason) {
	case AuthMissing, RateLimited, Network:
		return Reason(reason)
	}
	return Failed
}

// object decodes a JSON object into its exactly spelled members.
func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return nil, errors.New("not a JSON object")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	return members, nil
}

// optionalString decodes an absent or null member as "" and a string as itself.
func optionalString(raw json.RawMessage) (string, bool) {
	if raw == nil || bytes.Equal(raw, []byte("null")) {
		return "", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
