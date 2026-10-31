package mcpserver

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

// JSON-RPC 2.0 error codes the server answers with.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

const (
	// DefaultMaxMessageBytes bounds one incoming message, excluding its newline.
	DefaultMaxMessageBytes = 1 << 20
	// maxConfigurableBytes is the largest incoming bound SetMaxMessageBytes
	// accepts.
	maxConfigurableBytes = 16 << 20
	// maxEchoedIDBytes bounds a request id that is echoed back.
	maxEchoedIDBytes = 256
)

// lineReader reads newline-delimited messages without ever buffering more than
// the current bound.
type lineReader struct{ br *bufio.Reader }

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(r, 64<<10)}
}

// read returns one message of at most limit bytes, without its newline (and an
// optional carriage return before it). A longer message is drained to its
// newline without being kept and reported as tooLarge, so the next message is
// read cleanly. End of input returns io.EOF; a message cut off by the end of
// input is dropped.
func (r *lineReader) read(limit int) (line []byte, tooLarge bool, err error) {
	var buf []byte
	size := 0
	for {
		chunk, err := r.br.ReadSlice('\n')
		n := len(chunk)
		if err == nil {
			n--
		}
		size += n
		if size > limit {
			tooLarge, buf = true, nil
		}
		if !tooLarge {
			buf = append(buf, chunk...)
		}
		switch {
		case err == nil:
			if tooLarge {
				return nil, true, nil
			}
			buf = buf[:len(buf)-1]
			if n := len(buf); n > 0 && buf[n-1] == '\r' {
				buf = buf[:n-1]
			}
			return buf, false, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil, false, io.EOF
		default:
			return nil, false, err
		}
	}
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// request is one decoded incoming message. Only exactly spelled members are
// read, so a case-variant "ID" or "Method" is never mistaken for the real one.
type request struct {
	id     json.RawMessage
	method string
	params json.RawMessage
}

// isNotification reports whether the message carries no id and so gets no
// reply.
func (r request) isNotification() bool { return r.id == nil }

// parseError is a refused message with the JSON-RPC code and the id (or null)
// to answer with.
type parseError struct {
	code int
	id   json.RawMessage
	why  string
}

func (e *parseError) Error() string { return e.why }

// parse decodes one message strictly: valid UTF-8 JSON (-32700 otherwise), one
// object with no duplicate keys, jsonrpc "2.0", a string method and a string or
// integer id of bounded size (-32600 otherwise). A null id is a notification,
// as the engine's client reads it.
func parse(raw []byte) (request, error) {
	refuse := func(code int, id json.RawMessage, why string) (request, error) {
		return request{}, &parseError{code: code, id: id, why: why}
	}
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return refuse(codeParseError, nil, "a message is not valid JSON")
	}
	trimmed := bytes.TrimSpace(raw)
	if trimmed[0] != '{' {
		return refuse(codeInvalidRequest, nil, "a message is not one JSON object (batches are not supported)")
	}
	if err := pinned.RejectDuplicateKeys(trimmed); err != nil {
		return refuse(codeInvalidRequest, nil, "a message repeats a key")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &members); err != nil {
		return refuse(codeParseError, nil, "a message is malformed")
	}
	var id json.RawMessage
	if raw, ok := members["id"]; ok && !bytes.Equal(raw, []byte("null")) {
		if id = validID(raw); id == nil {
			return refuse(codeInvalidRequest, nil, "a message id is not a bounded string or integer")
		}
	}
	var version string
	if err := json.Unmarshal(members["jsonrpc"], &version); err != nil || version != "2.0" {
		return refuse(codeInvalidRequest, id, `a message is not jsonrpc "2.0"`)
	}
	rawMethod, ok := members["method"]
	if !ok {
		return refuse(codeInvalidRequest, id, "a message is not a request")
	}
	r := request{id: id, params: members["params"]}
	if err := json.Unmarshal(rawMethod, &r.method); err != nil || r.method == "" {
		return refuse(codeInvalidRequest, id, "a message method is not a string")
	}
	return r, nil
}

// validID returns id when it is a JSON string or integer of bounded size, and
// nil otherwise.
func validID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 || len(id) > maxEchoedIDBytes {
		return nil
	}
	var s string
	if json.Unmarshal(id, &s) == nil {
		return id
	}
	if _, err := strconv.ParseInt(string(id), 10, 64); err == nil {
		return id
	}
	return nil
}

// object decodes a JSON object into its exactly spelled members. Absent params
// are an empty object.
func object(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if raw == nil {
		return map[string]json.RawMessage{}, true
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &members); err != nil {
		return nil, false
	}
	return members, true
}

// encode renders one message as a single newline-terminated line.
// encoding/json escapes every control character (and U+2028, U+2029), so the
// line never contains a raw newline.
func encode(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func nullID(id json.RawMessage) json.RawMessage {
	if id == nil {
		return json.RawMessage("null")
	}
	return id
}

func resultMessage(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": nullID(id), "result": result}
}

func errorMessage(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": nullID(id), "error": rpcError{Code: code, Message: message}}
}
