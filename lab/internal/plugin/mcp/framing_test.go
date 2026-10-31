package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func reasonIs(t *testing.T, err error, want Reason) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Reason != want {
		t.Fatalf("error = %v, want reason %s", err, want)
	}
}

func TestReadAcceptsExactlyTheBound(t *testing.T) {
	line := strings.Repeat("a", 100)
	got, err := newLineReader(strings.NewReader(line + "\n")).read(100)
	if err != nil || string(got) != line {
		t.Fatalf("exact-cap message: %q, %v", got, err)
	}
	_, err = newLineReader(strings.NewReader(line + "a\n")).read(100)
	reasonIs(t, err, OutputTooLarge)
	// The bound covers every byte but the newline, a carriage return included.
	got, err = newLineReader(strings.NewReader("abc\r\n")).read(4)
	if err != nil || string(got) != "abc" {
		t.Fatalf("CRLF message: %q, %v", got, err)
	}
}

// countingReader fails the test if the reader consumes far past the bound.
type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

func TestReadStopsAtTheBoundWithoutBufferingTheRest(t *testing.T) {
	src := &countingReader{r: strings.NewReader(strings.Repeat("x", 8<<20) + "\n")}
	_, err := newLineReader(src).read(1 << 20)
	reasonIs(t, err, OutputTooLarge)
	if src.read > (1<<20)+(128<<10) {
		t.Fatalf("read %d bytes for a 1 MiB bound", src.read)
	}
}

func TestReadAtEndOfInputFails(t *testing.T) {
	_, err := newLineReader(strings.NewReader(`{"jsonrpc":"2.0"`)).read(1000)
	reasonIs(t, err, Failed)
	_, err = newLineReader(strings.NewReader("")).read(1000)
	reasonIs(t, err, Failed)
}

func TestParseRefusesMalformedMessages(t *testing.T) {
	for name, raw := range map[string]string{
		"invalid utf8":    "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"a\":\"\xff\"}}",
		"duplicate key":   `{"jsonrpc":"2.0","id":1,"id":2,"result":{}}`,
		"batch":           `[{"jsonrpc":"2.0","id":1,"result":{}}]`,
		"not json":        `{not json`,
		"trailing":        `{"jsonrpc":"2.0","id":1,"result":{}} {}`,
		"wrong version":   `{"jsonrpc":"1.0","id":1,"result":{}}`,
		"no version":      `{"id":1,"result":{}}`,
		"both":            `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"x"}}`,
		"neither":         `{"jsonrpc":"2.0","id":1}`,
		"method type":     `{"jsonrpc":"2.0","method":5}`,
		"bad error":       `{"jsonrpc":"2.0","id":1,"error":{"code":"x","message":"y"}}`,
		"case variant id": `{"jsonrpc":"2.0","ID":1,"Result":{}}`,
	} {
		if _, err := parse([]byte(raw)); err == nil {
			t.Errorf("%s: %s was accepted", name, raw)
		} else {
			reasonIs(t, err, SchemaInvalid)
		}
	}
	m, err := parse([]byte(`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`))
	if err != nil || string(m.id) != "7" || !m.isResponse() {
		t.Fatalf("good response: %+v, %v", m, err)
	}
	m, err = parse([]byte(`{"jsonrpc":"2.0","id":"s1","method":"sampling/createMessage"}`))
	if err != nil || !m.isRequest() {
		t.Fatalf("server request: %+v, %v", m, err)
	}
}

func TestEncodeIsOneBoundedLine(t *testing.T) {
	line, err := encode(map[string]string{"text": "a\nb c"}, 100)
	if err != nil || bytes.Count(line, []byte("\n")) != 1 || line[len(line)-1] != '\n' {
		t.Fatalf("encode = %q, %v", line, err)
	}
	_, err = encode(strings.Repeat("a", 200), 100)
	reasonIs(t, err, OutputTooLarge)
}

func TestEchoIDIsBounded(t *testing.T) {
	for raw, want := range map[string]string{
		`"s1"`:                               `"s1"`,
		`12`:                                 `12`,
		`{"x":1}`:                            `null`,
		`1.5`:                                `null`,
		`"` + strings.Repeat("a", 300) + `"`: `null`,
	} {
		if got := string(echoID(json.RawMessage(raw))); got != want {
			t.Errorf("echoID(%s) = %s, want %s", raw, got, want)
		}
	}
}

// scripted is an in-memory server: it answers each request line with the next
// scripted reply lines.
func scripted(t *testing.T, replies ...string) *Client {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	t.Cleanup(func() { serverIn.Close(); clientIn.Close() })
	go func() {
		buf := make([]byte, 1<<16)
		for _, reply := range replies {
			if _, err := serverIn.Read(buf); err != nil {
				return
			}
			if _, err := io.WriteString(serverOut, reply); err != nil {
				return
			}
		}
		io.Copy(io.Discard, serverIn)
	}()
	return NewClient(clientIn, clientOut, 0)
}

func TestClientRejectsUnknownAndStaleResponseIDs(t *testing.T) {
	c := scripted(t, `{"jsonrpc":"2.0","id":9,"result":{"protocolVersion":"x"}}`+"\n")
	_, err := c.Initialize(context.Background())
	reasonIs(t, err, SchemaInvalid)
	_, err = c.ListTools(context.Background())
	reasonIs(t, err, Failed)
}

func TestClientIgnoresABoundedNumberOfNotifications(t *testing.T) {
	note := `{"jsonrpc":"2.0","method":"notifications/progress","params":{}}` + "\n"
	c := scripted(t, strings.Repeat(note, maxIgnored)+`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"`+ProtocolVersion+`"}}`+"\n")
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("notifications within the bound: %v", err)
	}
	c = scripted(t, strings.Repeat(note, maxIgnored+1)+`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"`+ProtocolVersion+`"}}`+"\n")
	_, err := c.Initialize(context.Background())
	reasonIs(t, err, OutputTooLarge)
}

func TestClientMapsAnExpiredContextToTimeout(t *testing.T) {
	c := scripted(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Initialize(ctx)
	reasonIs(t, err, Timeout)
}

func TestErrorTextNeverCarriesDiagnostics(t *testing.T) {
	e := Fail(Failed, "forge_describe", "token=ghp_abcdefghijklmnopqrstuvwxyz /Users/someone/secret")
	if strings.Contains(e.Error(), "ghp_") || strings.Contains(e.Error(), "/Users") {
		t.Fatalf("Error() leaks detail: %q", e.Error())
	}
	if strings.Contains(e.Diagnostics, "ghp_abc") {
		t.Fatalf("diagnostics are not redacted: %q", e.Diagnostics)
	}
	if e.Error() != "plugin forge_describe: failed" {
		t.Fatalf("Error() = %q", e.Error())
	}
	long := Diagnose(strings.Repeat("a", maxDiagnosticBytes+100))
	if !strings.HasSuffix(long, "[truncated]") || len(long) > maxDiagnosticBytes+32 {
		t.Fatalf("diagnostics are not bounded: %d bytes", len(long))
	}
	for _, r := range Reasons {
		if !Known(r) {
			t.Fatalf("%s is not known", r)
		}
	}
	if Known("other") || ReasonOf(errors.New("x")) != Failed || ReasonOf(e) != Failed {
		t.Fatal("unknown reasons are accepted")
	}
}
