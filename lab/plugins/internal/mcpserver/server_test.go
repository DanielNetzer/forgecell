package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// lockedBuffer is a bytes.Buffer safe to share with the serving goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// session is one Serve over in-memory pipes. stdout records every byte the
// server wrote to its output.
type session struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Reader
	outR   *io.PipeReader
	stdout *lockedBuffer
	stderr *lockedBuffer
	done   chan error
	cancel context.CancelFunc
}

func serve(t *testing.T, s *Server) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ss := &session{t: t, in: inW, out: bufio.NewReader(outR), outR: outR, stdout: &lockedBuffer{}, stderr: &lockedBuffer{}, done: make(chan error, 1)}
	s.SetStderr(ss.stderr)
	ctx, cancel := context.WithCancel(context.Background())
	ss.cancel = cancel
	go func() {
		err := s.Serve(ctx, inR, io.MultiWriter(ss.stdout, outW))
		outW.Close()
		ss.done <- err
	}()
	t.Cleanup(func() {
		cancel()
		inW.Close()
		outR.Close()
	})
	return ss
}

func (ss *session) send(line string) {
	ss.t.Helper()
	if _, err := io.WriteString(ss.in, line+"\n"); err != nil {
		ss.t.Fatalf("writing a request: %v", err)
	}
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (ss *session) recv() response {
	ss.t.Helper()
	line, err := ss.out.ReadBytes('\n')
	if err != nil {
		ss.t.Fatalf("reading a response: %v", err)
	}
	var r response
	if err := json.Unmarshal(line, &r); err != nil || r.JSONRPC != "2.0" {
		ss.t.Fatalf("response %q: %v", line, err)
	}
	return r
}

func (ss *session) roundTrip(line string) response {
	ss.t.Helper()
	ss.send(line)
	return ss.recv()
}

// wantError requires a JSON-RPC error with code and id.
func (ss *session) wantError(line string, code int, id string) {
	ss.t.Helper()
	r := ss.roundTrip(line)
	if r.Error == nil || r.Error.Code != code || string(r.ID) != id || r.Result != nil {
		ss.t.Fatalf("%q: got %+v (id %s), want error %d with id %s", line, r.Error, r.ID, code, id)
	}
}

// alive requires the server to still answer a ping.
func (ss *session) alive() {
	ss.t.Helper()
	r := ss.roundTrip(`{"jsonrpc":"2.0","id":"alive","method":"ping"}`)
	if r.Error != nil || string(r.ID) != `"alive"` || string(r.Result) != "{}" {
		ss.t.Fatalf("ping: %+v %s", r.Error, r.Result)
	}
}

// close ends the input and requires Serve to return nil.
func (ss *session) close() {
	ss.t.Helper()
	ss.in.Close()
	if err := <-ss.done; err != nil {
		ss.t.Fatalf("Serve after EOF: %v", err)
	}
}

const echoSchema = `{"type":"object","additionalProperties":false,"properties":{"word":{"type":"string"}}}`

func newEchoServer() *Server {
	s := New("test-server", "1.2.3", "forgecell.forge/v1")
	s.Tool("echo", []byte(echoSchema), func(_ context.Context, args json.RawMessage) (any, error) {
		var in struct{ Word string }
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, Fail(Failed, "invalid arguments")
		}
		return map[string]any{"word": in.Word}, nil
	})
	s.Tool("fail", []byte(`{"type":"object"}`), func(_ context.Context, args json.RawMessage) (any, error) {
		var in struct{ Reason, Message string }
		json.Unmarshal(args, &in)
		return nil, Fail(Reason(in.Reason), in.Message)
	})
	return s
}

func TestInitializeDeclaresProtocolInfoAndContract(t *testing.T) {
	ss := serve(t, newEchoServer())
	r := ss.roundTrip(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct{ Name, Version string }
		Capabilities    struct {
			Tools        map[string]any `json:"tools"`
			Experimental struct {
				Forgecell struct{ Contract string }
			}
		}
	}
	if r.Error != nil || string(r.ID) != "1" || json.Unmarshal(r.Result, &result) != nil {
		t.Fatalf("initialize: %+v %s", r.Error, r.Result)
	}
	if result.ProtocolVersion != mcp.ProtocolVersion || result.ServerInfo.Name != "test-server" || result.ServerInfo.Version != "1.2.3" ||
		result.Capabilities.Experimental.Forgecell.Contract != "forgecell.forge/v1" || result.Capabilities.Tools == nil {
		t.Fatalf("initialize result: %s", r.Result)
	}
	// notifications/initialized gets no reply: the next line answers the ping.
	ss.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	ss.alive()
	ss.close()
}

func TestTheEngineClientAcceptsTheServer(t *testing.T) {
	ss := serve(t, newEchoServer())
	c := mcp.NewClient(ss.out, ss.in, 0)
	ctx := context.Background()
	info, err := c.InitializeContract(ctx, "forgecell.forge/v1")
	if err != nil || info.Name != "test-server" || info.Contract != "forgecell.forge/v1" {
		t.Fatalf("InitializeContract: %+v %v", info, err)
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "fail" || string(tools[0].InputSchema) != echoSchema {
		t.Fatalf("ListTools: %+v %v", tools, err)
	}
	out, err := c.CallTool(ctx, "echo", map[string]any{"word": "hi"}, 0)
	if err != nil || string(out) != `{"word":"hi"}` {
		t.Fatalf("CallTool: %s %v", out, err)
	}
	for _, reason := range []mcp.Reason{mcp.AuthMissing, mcp.RateLimited, mcp.Network, mcp.Failed} {
		_, err := c.CallTool(ctx, "fail", map[string]any{"reason": string(reason), "message": "no"}, 0)
		var e *mcp.Error
		if !errors.As(err, &e) || !e.Remote || e.Reason != reason {
			t.Fatalf("%s: %v", reason, err)
		}
	}
	ss.close()
}

func TestToolsListReturnsEverySchema(t *testing.T) {
	ss := serve(t, newEchoServer())
	r := ss.roundTrip(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var result struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if r.Error != nil || json.Unmarshal(r.Result, &result) != nil || len(result.Tools) != 2 {
		t.Fatalf("tools/list: %s", r.Result)
	}
	if result.Tools[0].Name != "echo" || string(result.Tools[0].InputSchema) != echoSchema || result.Tools[1].Name != "fail" || string(result.Tools[1].InputSchema) != `{"type":"object"}` {
		t.Fatalf("tools/list: %s", r.Result)
	}
	ss.close()
}

type callResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
}

func (ss *session) call(id, tool, args string) callResult {
	ss.t.Helper()
	r := ss.roundTrip(`{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`)
	var result callResult
	if r.Error != nil || string(r.ID) != id || json.Unmarshal(r.Result, &result) != nil {
		ss.t.Fatalf("tools/call %s: %+v %s", tool, r.Error, r.Result)
	}
	return result
}

// failure decodes an isError result's structured reason and message.
func failure(t *testing.T, r callResult) (string, string) {
	t.Helper()
	var s struct{ Reason, Message string }
	if !r.IsError || json.Unmarshal(r.StructuredContent, &s) != nil || len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatalf("not an error result: %+v", r)
	}
	return s.Reason, s.Message
}

func TestToolsCallReturnsStructuredContentWithATextMirror(t *testing.T) {
	ss := serve(t, newEchoServer())
	r := ss.call("3", "echo", `{"word":"hello"}`)
	if r.IsError || string(r.StructuredContent) != `{"word":"hello"}` || len(r.Content) != 1 || r.Content[0].Type != "text" || r.Content[0].Text != `{"word":"hello"}` {
		t.Fatalf("echo: %+v", r)
	}
	ss.close()
}

func TestHandlerFailuresCarryOnlyFixedReasons(t *testing.T) {
	s := newEchoServer()
	s.Tool("plain", []byte(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("upstream said: Authorization: Bearer abcdefghijklmnopqrstuvwxyz")
	})
	s.Tool("panics", []byte(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) { panic("boom") })
	s.Tool("not-object", []byte(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) { return []int{1}, nil })
	ss := serve(t, s)
	for _, c := range []struct{ reason, want string }{
		{"auth-missing", "auth-missing"},
		{"rate-limited", "rate-limited"},
		{"network", "network"},
		{"failed", "failed"},
		{"timeout", "failed"},
		{"anything-else", "failed"},
	} {
		got, msg := failure(t, ss.call("4", "fail", `{"reason":"`+c.reason+`","message":"upstream refused"}`))
		if got != c.want || msg != "upstream refused" {
			t.Fatalf("%s: reason %q message %q, want %q", c.reason, got, msg, c.want)
		}
	}
	// The message is bounded, one line and redacted.
	hostile := "line one\nline two‮ Authorization: Bearer abcdefghijklmnopqrstuvwxyz " + strings.Repeat("x", 5000)
	raw, _ := json.Marshal(map[string]string{"reason": "network", "message": hostile})
	r := ss.call("5", "fail", string(raw))
	got, msg := failure(t, r)
	if got != "network" || strings.ContainsAny(msg, "\n‮") || strings.Contains(msg, "abcdefghijklmnop") || len([]rune(msg)) > maxMessageRunes+1 || !strings.Contains(msg, "[redacted]") {
		t.Fatalf("hostile message: %q", msg)
	}
	if strings.Contains(r.Content[0].Text, "abcdefghijklmnop") || strings.Contains(r.Content[0].Text, "\n") {
		t.Fatalf("text mirror: %q", r.Content[0].Text)
	}
	// A plain error, a panic and a non-object result are failed with no message.
	for _, tool := range []string{"plain", "panics", "not-object"} {
		r := ss.call("6", tool, `{}`)
		if got, msg := failure(t, r); got != "failed" || msg != "" || strings.Contains(r.Content[0].Text, "upstream") {
			t.Fatalf("%s: %q %q %+v", tool, got, msg, r)
		}
	}
	ss.alive()
	ss.close()
	if strings.Contains(ss.stderr.String(), "abcdefghijklmnop") || !strings.Contains(ss.stderr.String(), "[redacted]") {
		t.Fatalf("stderr is not redacted:\n%s", ss.stderr)
	}
}

func TestUnknownMethodsAndBadParamsKeepServing(t *testing.T) {
	ss := serve(t, newEchoServer())
	ss.wantError(`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`, codeMethodNotFound, "7")
	ss.alive()
	// An unknown notification gets no reply.
	ss.send(`{"jsonrpc":"2.0","method":"notifications/unknown"}`)
	ss.alive()
	ss.wantError(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"missing","arguments":{}}}`, codeInvalidParams, "8")
	ss.wantError(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"arguments":{}}}`, codeInvalidParams, "9")
	ss.wantError(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"echo","arguments":[1]}}`, codeInvalidParams, "10")
	ss.wantError(`{"jsonrpc":"2.0","id":11,"method":"tools/list","params":[]}`, codeInvalidParams, "11")
	ss.alive()
	ss.close()
}

func TestMalformedMessagesKeepServing(t *testing.T) {
	ss := serve(t, newEchoServer())
	for _, c := range []struct {
		line string
		code int
		id   string
	}{
		{`{not json`, codeParseError, "null"},
		{"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\",\"x\":\"\xff\"}", codeParseError, "null"},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, codeInvalidRequest, "null"},
		{`42`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"1.0","id":1,"method":"ping"}`, codeInvalidRequest, "1"},
		{`{"id":"a","method":"ping"}`, codeInvalidRequest, `"a"`},
		{`{"jsonrpc":"2.0","id":1,"id":2,"method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":{"x":1},"method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":"` + strings.Repeat("i", 300) + `","method":"ping"}`, codeInvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":3,"method":7}`, codeInvalidRequest, "3"},
		{`{"jsonrpc":"2.0","id":4,"result":{}}`, codeInvalidRequest, "4"},
	} {
		ss.wantError(c.line, c.code, c.id)
		ss.alive()
	}
	// Blank lines are skipped without a reply.
	ss.send("")
	ss.send("   \r")
	ss.alive()
	ss.close()
}

// paddedPing is a ping request of exactly size bytes.
func paddedPing(size int) string {
	head, tail := `{"jsonrpc":"2.0","id":"big","method":"ping","params":{"pad":"`, `"}}`
	return head + strings.Repeat("p", size-len(head)-len(tail)) + tail
}

func TestOversizedMessagesAreRefusedAndServingContinues(t *testing.T) {
	ss := serve(t, newEchoServer())
	// The default bound is 1 MiB: a message at the bound is served, one byte
	// more is refused.
	if r := ss.roundTrip(paddedPing(DefaultMaxMessageBytes)); r.Error != nil || string(r.ID) != `"big"` {
		t.Fatalf("a message at the bound: %+v", r.Error)
	}
	ss.wantError(paddedPing(DefaultMaxMessageBytes+1), codeInvalidRequest, "null")
	ss.alive()
	ss.close()

	s := newEchoServer()
	s.SetMaxMessageBytes(256)
	ss = serve(t, s)
	ss.wantError(paddedPing(257), codeInvalidRequest, "null")
	ss.alive()
	if r := ss.roundTrip(paddedPing(256)); r.Error != nil {
		t.Fatalf("a message at the configured bound: %+v", r.Error)
	}
	ss.close()
	if !strings.Contains(ss.stderr.String(), "refused a message over 256 bytes") {
		t.Fatalf("stderr: %s", ss.stderr)
	}
}

func TestOversizedResponsesAreRefused(t *testing.T) {
	s := newEchoServer()
	s.maxOut = 512
	ss := serve(t, s)
	ss.wantError(`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"echo","arguments":{"word":"`+strings.Repeat("w", 600)+`"}}}`, codeInternalError, "12")
	ss.alive()
	ss.close()
}

// expired is a handler context whose deadline has already passed, so a timeout
// never depends on how fast the machine is.
func expired(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, time.Unix(0, 0))
}

func TestATimedOutHandlerFails(t *testing.T) {
	s := newEchoServer()
	// The handler sends whether its context had a deadline; the test reads it
	// only from the channel, so nothing is shared without synchronization.
	gotDeadline := make(chan bool, 1)
	s.Tool("waits", []byte(`{"type":"object"}`), func(ctx context.Context, _ json.RawMessage) (any, error) {
		_, ok := ctx.Deadline()
		gotDeadline <- ok
		<-ctx.Done()
		return map[string]any{"late": true}, nil
	})
	s.handlerContext = expired
	ss := serve(t, s)
	if reason, _ := failure(t, ss.call("13", "waits", `{}`)); reason != "failed" {
		t.Fatalf("timed-out handler: %q", reason)
	}
	if !<-gotDeadline {
		t.Fatal("the handler's context had no deadline")
	}
	ss.alive()
	ss.close()
}

func TestOversizedResultsFailAndServingContinues(t *testing.T) {
	s := newEchoServer()
	s.Tool("bounded", []byte(echoSchema), func(_ context.Context, args json.RawMessage) (any, error) {
		var in struct{ Word string }
		json.Unmarshal(args, &in)
		return map[string]any{"word": in.Word}, nil
	}, MaxOutputBytes(32))
	ss := serve(t, s)
	// {"word":"..."} is 11 bytes plus the word: 21 bytes fit, 22 do not.
	if r := ss.call("20", "bounded", `{"word":"`+strings.Repeat("a", 21)+`"}`); r.IsError {
		t.Fatalf("a result at the bound: %+v", r)
	}
	r := ss.call("21", "bounded", `{"word":"`+strings.Repeat("b", 22)+`"}`)
	if reason, msg := failure(t, r); reason != "failed" || msg != "" || strings.Contains(r.Content[0].Text, "bbbb") {
		t.Fatalf("an oversized result: %q %q %+v", reason, msg, r)
	}
	if r := ss.call("22", "bounded", `{"word":"ok"}`); r.IsError || string(r.StructuredContent) != `{"word":"ok"}` {
		t.Fatalf("the call after an oversized result: %+v", r)
	}
	ss.close()
	if !strings.Contains(ss.stderr.String(), "over its 32-byte output bound") {
		t.Fatalf("stderr: %s", ss.stderr)
	}
}

func TestASecretStraddlingTheCutIsNeverPartlyServed(t *testing.T) {
	const secretName = "MCPSERVER_TEST_STRADDLE"
	const secret = "straddling-secret-value-0099"
	t.Setenv(secretName, secret)
	s := newEchoServer()
	s.SecretEnv(secretName)
	s.Tool("leaks", []byte(`{"type":"object"}`), func(_ context.Context, args json.RawMessage) (any, error) {
		var in struct{ Message string }
		json.Unmarshal(args, &in)
		return nil, Fail(Network, in.Message)
	})
	ss := serve(t, s)
	// prefixes are every leading part of the secret long enough to matter.
	prefixes := func(text string) bool {
		for i := minSecretLength; i <= len(secret); i++ {
			if strings.Contains(text, secret[:i]) {
				return true
			}
		}
		return false
	}
	for _, at := range []int{maxMessageRunes - 10, maxMessageRunes - 1, maxDiagnosticRunes - 60} {
		msg := strings.Repeat("m", at) + secret + " tail"
		raw, _ := json.Marshal(map[string]string{"message": msg})
		r := ss.call("23", "leaks", string(raw))
		reason, got := failure(t, r)
		if reason != "network" || prefixes(got) || prefixes(r.Content[0].Text) {
			t.Fatalf("secret at %d: message %q text %q", at, got, r.Content[0].Text)
		}
		s.Log(msg)
	}
	ss.close()
	if prefixes(ss.stderr.String()) {
		t.Fatalf("stderr holds part of the secret:\n%s", ss.stderr)
	}
	// Error() never serves part of a token straddling its cut either.
	token := "ghp_abcdefghijklmnopqrstuvwxyz0123"
	if e := Fail(Failed, strings.Repeat("e", maxMessageRunes-3)+token).Error(); strings.Contains(e, "ghp_") {
		t.Fatalf("Error(): %q", e)
	}
}

func TestSanitizeIsBoundedAndFailsSafeAtTheScanCap(t *testing.T) {
	// Text padded past the scan cap with characters cleaning drops is dropped,
	// so a secret cut at the cap never moves into the served text.
	padded := strings.Repeat("​", maxScanBytes/3) + "hunter2-hunter2-hunter2"
	if got := sanitize(padded, maxMessageRunes); got != "" {
		t.Fatalf("padded past the cap: %q", got)
	}
	long := strings.Repeat("x", 2*maxScanBytes)
	if got := sanitize(long, maxMessageRunes); len([]rune(got)) != maxMessageRunes+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("long text: %d runes", len([]rune(got)))
	}
	if e := Fail(Failed, long).(*Error); len(e.message) != maxScanBytes {
		t.Fatalf("Fail kept %d bytes", len(e.message))
	}
}

func TestHandlersNeverOverlap(t *testing.T) {
	s := newEchoServer()
	release := make(chan struct{})
	s.Tool("stuck", []byte(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
		<-release
		return map[string]any{}, nil
	})
	s.handlerContext = expired
	ss := serve(t, s)
	// The stuck handler ignores its context: its call fails at the deadline and
	// later calls fail while it still runs.
	if reason, _ := failure(t, ss.call("14", "stuck", `{}`)); reason != "failed" {
		t.Fatalf("stuck: %q", reason)
	}
	if reason, _ := failure(t, ss.call("15", "echo", `{"word":"x"}`)); reason != "failed" {
		t.Fatalf("a call while a handler runs: %q", reason)
	}
	ss.alive()
	close(release)
	<-s.running
	s.handlerContext = context.WithTimeout
	if r := ss.call("16", "echo", `{"word":"x"}`); r.IsError {
		t.Fatalf("a call after the handler returned: %+v", r)
	}
	ss.close()
}

func TestEndOfInputIsACleanExit(t *testing.T) {
	ss := serve(t, newEchoServer())
	ss.close()

	// A message cut off by the end of input is dropped.
	ss = serve(t, newEchoServer())
	io.WriteString(ss.in, `{"jsonrpc":"2.0","id":1,`)
	ss.close()
	if ss.stdout.String() != "" {
		t.Fatalf("output after a cut-off message: %q", ss.stdout)
	}
}

func TestCancelingTheContextEndsServe(t *testing.T) {
	ss := serve(t, newEchoServer())
	ss.alive()
	ss.cancel()
	if err := <-ss.done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve after cancel: %v", err)
	}
}

func TestStdoutCarriesOnlyProtocolLines(t *testing.T) {
	const secretName = "MCPSERVER_TEST_SECRET"
	const secret = "s3cr3t-value-0042"
	t.Setenv(secretName, secret)
	s := newEchoServer()
	s.SecretEnv(secretName)
	s.Tool("noisy", []byte(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
		s.Log("calling upstream with " + secret)
		return nil, errors.New("upstream rejected " + secret)
	})
	ss := serve(t, s)
	ss.roundTrip(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	ss.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	ss.roundTrip(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	ss.call("3", "echo", `{"word":"ok"}`)
	ss.call("4", "noisy", `{}`)
	ss.call("5", "fail", `{"reason":"network","message":"token `+secret+`"}`)
	ss.wantError(`{not json`, codeParseError, "null")
	ss.wantError(`{"jsonrpc":"2.0","id":6,"method":"nope"}`, codeMethodNotFound, "6")
	ss.close()

	stdout := ss.stdout.String()
	if !strings.HasSuffix(stdout, "\n") || strings.Contains(stdout, secret) {
		t.Fatalf("stdout: %q", stdout)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("stdout has %d lines, want 7:\n%s", len(lines), stdout)
	}
	for _, line := range lines {
		var r response
		if err := json.Unmarshal([]byte(line), &r); err != nil || r.JSONRPC != "2.0" || r.ID == nil || (r.Result == nil) == (r.Error == nil) {
			t.Fatalf("not a protocol line: %q", line)
		}
	}
	stderr := ss.stderr.String()
	if stderr == "" || strings.Contains(stderr, secret) || !strings.Contains(stderr, "calling upstream with [redacted]") {
		t.Fatalf("stderr:\n%s", stderr)
	}
}

func TestDiagnosticsAreBounded(t *testing.T) {
	s := newEchoServer()
	stderr := &lockedBuffer{}
	s.SetStderr(stderr)
	for i := 0; i < 2000; i++ {
		s.Log(strings.Repeat("d", 1000))
	}
	out := stderr.String()
	if len(out) > maxDiagnosticBytes || !strings.HasSuffix(out, "mcpserver: diagnostics truncated\n") {
		t.Fatalf("stderr holds %d bytes", len(out))
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if len([]rune(line)) > maxDiagnosticRunes+len("mcpserver: ")+1 {
			t.Fatalf("a diagnostic line holds %d runes", len([]rune(line)))
		}
	}
}

func TestEnvReadsOnlyTheNamedVariable(t *testing.T) {
	t.Setenv("MCPSERVER_TEST_ENV", "value")
	if v, ok := Env("MCPSERVER_TEST_ENV"); !ok || v != "value" {
		t.Fatalf("Env: %q %t", v, ok)
	}
	for _, name := range []string{"", "MCPSERVER_TEST_ENV=value", "1BAD", "A B", "MCPSERVER_TEST_UNSET"} {
		if v, ok := Env(name); ok || v != "" {
			t.Fatalf("Env(%q) = %q %t", name, v, ok)
		}
	}
}

func TestRedactReplacesSecretEnvValuesAndTokens(t *testing.T) {
	t.Setenv("MCPSERVER_TEST_SECRET", "hunter2-hunter2")
	t.Setenv("MCPSERVER_TEST_SHORT", "ab")
	got := Redact("pw hunter2-hunter2 ab Authorization: Bearer abcdefghijklmnopqrstuvwxyz", "MCPSERVER_TEST_SECRET", "MCPSERVER_TEST_SHORT", "MCPSERVER_TEST_UNSET")
	if strings.Contains(got, "hunter2") || strings.Contains(got, "abcdefghijklmnop") || !strings.Contains(got, " ab ") {
		t.Fatalf("Redact: %q", got)
	}
}

func TestRegistrationRefusesProgrammingErrors(t *testing.T) {
	panics := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s did not panic", name)
			}
		}()
		f()
	}
	ok := func(context.Context, json.RawMessage) (any, error) { return map[string]any{}, nil }
	panics("a repeated tool", func() { s := newEchoServer(); s.Tool("echo", []byte(`{}`), ok) })
	panics("a bad tool name", func() { newEchoServer().Tool("bad name", []byte(`{}`), ok) })
	panics("a schema that is not an object", func() { newEchoServer().Tool("t", []byte(`[]`), ok) })
	panics("a malformed schema", func() { newEchoServer().Tool("t", []byte(`{"type":`), ok) })
	panics("a nil handler", func() { newEchoServer().Tool("t", []byte(`{}`), nil) })
	panics("a bad contract", func() { New("n", "1", "bad contract\n") })
	panics("too many tools", func() {
		s := New("n", "1", "")
		for i := 0; i <= maxTools; i++ {
			s.Tool("t"+strconv.Itoa(i), []byte(`{}`), ok)
		}
	})
}
