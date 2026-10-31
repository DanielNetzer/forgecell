package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
)

// DefaultMaxMessageBytes bounds one JSON-RPC message, excluding its newline. A
// call may lower it, never raise it.
const DefaultMaxMessageBytes = 4 << 20

// maxEchoedIDBytes bounds a server request id that is echoed back in a refusal.
const maxEchoedIDBytes = 256

// lineReader reads newline-delimited messages without ever buffering more than
// the current bound.
type lineReader struct{ br *bufio.Reader }

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(r, 64<<10)}
}

// read returns one message of at most limit bytes, without its newline (and an
// optional carriage return before it). A longer message is OutputTooLarge and
// reading stops at the bound; end of input, even mid-message, is Failed.
func (r *lineReader) read(limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		size := len(buf) + len(chunk)
		if err == nil {
			size--
		}
		if size > limit {
			return nil, Fail(OutputTooLarge, "", "a message exceeds "+strconv.Itoa(limit)+" bytes")
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			buf = buf[:len(buf)-1]
			if n := len(buf); n > 0 && buf[n-1] == '\r' {
				buf = buf[:n-1]
			}
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(buf) > 0 {
				return nil, Fail(Failed, "", "the server closed its output mid-message")
			}
			return nil, Fail(Failed, "", "the server closed its output")
		default:
			return nil, Fail(Failed, "", "reading from the server failed: "+err.Error())
		}
	}
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// message is one decoded JSON-RPC message. Only exactly spelled members are
// read, so a case-variant "ID" or "Result" is never mistaken for the real one.
type message struct {
	id        json.RawMessage
	method    string
	hasMethod bool
	params    json.RawMessage
	result    json.RawMessage
	err       *rpcError
}

func (m message) isResponse() bool { return !m.hasMethod }
func (m message) isRequest() bool  { return m.hasMethod && m.id != nil }

// parse decodes one message strictly: valid UTF-8, one JSON object (no batch),
// no duplicate keys, jsonrpc "2.0", and the member types JSON-RPC requires.
func parse(raw []byte) (message, error) {
	invalid := func(why string) (message, error) { return message{}, Fail(SchemaInvalid, "", why) }
	if !utf8.Valid(raw) {
		return invalid("a message is not valid UTF-8")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return invalid("a message is not one JSON object (batches are not supported)")
	}
	if err := pinned.RejectDuplicateKeys(trimmed); err != nil {
		return invalid("a message is malformed or repeats a key: " + err.Error())
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var members map[string]json.RawMessage
	if err := dec.Decode(&members); err != nil {
		return invalid("a message is malformed: " + err.Error())
	}
	if _, err := dec.Token(); err != io.EOF {
		return invalid("a message has trailing data")
	}
	var version string
	if err := json.Unmarshal(members["jsonrpc"], &version); err != nil || version != "2.0" {
		return invalid(`a message is not jsonrpc "2.0"`)
	}
	m := message{params: members["params"], result: members["result"]}
	if id, ok := members["id"]; ok && !bytes.Equal(id, []byte("null")) {
		m.id = id
	}
	if raw, ok := members["method"]; ok {
		if err := json.Unmarshal(raw, &m.method); err != nil || m.method == "" {
			return invalid("a message method is not a string")
		}
		m.hasMethod = true
		return m, nil
	}
	if raw, ok := members["error"]; ok {
		var e rpcError
		if err := strictUnmarshal(raw, &e); err != nil {
			return invalid("an error response is malformed")
		}
		m.err = &e
	}
	if (m.result == nil) == (m.err == nil) {
		return invalid("a response must carry exactly one of result and error")
	}
	return m, nil
}

// strictUnmarshal decodes one JSON value into v, refusing unknown fields.
func strictUnmarshal(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// encode renders one message as a single line. encoding/json escapes every
// control character, so the line never contains a raw newline.
func encode(v any, limit int) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, Fail(SchemaInvalid, "", "a request cannot be encoded: "+err.Error())
	}
	if len(raw) > limit {
		return nil, Fail(OutputTooLarge, "", "a request exceeds "+strconv.Itoa(limit)+" bytes")
	}
	return append(raw, '\n'), nil
}

// echoID returns a server request id that is safe to send back: a JSON string or
// integer of bounded size, or null.
func echoID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 || len(id) > maxEchoedIDBytes {
		return json.RawMessage("null")
	}
	var s string
	if json.Unmarshal(id, &s) == nil {
		return id
	}
	if _, err := strconv.ParseInt(string(id), 10, 64); err == nil {
		return id
	}
	return json.RawMessage("null")
}
