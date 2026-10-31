// Package contract holds the v1 plugin contracts: for each kind, the MCP tools a
// plugin serves, each with its access level, strict input and output schemas,
// an output byte cap and a default timeout. The schemas are embedded JSON files;
// strict decoders turn each tool's output into sanitized Go values.
package contract

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// Access is a tool's effect: read tools never change remote state; the engine
// calls write tools only at the step a human gate authorizes.
type Access string

const (
	Read  Access = "read"
	Write Access = "write"
)

// Kinds lists the v1 plugin kinds.
var Kinds = []string{"tracker", "forge", "reviewer"}

// envelopeBytes is the room a tools/call response needs beyond its output.
const envelopeBytes = 64 << 10

var toolName = regexp.MustCompile(`^[a-z]+_[a-z_]{1,58}$`)

// Tool is one contract tool.
type Tool struct {
	Name           string  `json:"name"`
	Access         Access  `json:"access"`
	Required       bool    `json:"required"`
	MaxOutputBytes int     `json:"maxOutputBytes"`
	TimeoutMS      int     `json:"timeoutMs"`
	Input          *Schema `json:"input"`
	Output         *Schema `json:"output"`
}

// Contract is one kind's tool set.
type Contract struct {
	ID    string `json:"contract"`
	Tools []Tool `json:"tools"`
	kind  string
}

//go:embed tracker.v1.json forge.v1.json reviewer.v1.json
var files embed.FS

var contracts = mustLoad()

func mustLoad() map[string]*Contract {
	out := map[string]*Contract{}
	for _, kind := range Kinds {
		c, err := load(kind)
		if err != nil {
			panic(err)
		}
		out[kind] = c
	}
	return out
}

func load(kind string) (*Contract, error) {
	raw, err := files.ReadFile(kind + ".v1.json")
	if err != nil {
		return nil, err
	}
	if err := pinned.RejectDuplicateKeys(raw); err != nil {
		return nil, fmt.Errorf("contract %s: %w", kind, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Contract
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("contract %s: %w", kind, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("contract %s has trailing data", kind)
	}
	if c.ID != ID(kind) {
		return nil, fmt.Errorf("contract %s declares %q", kind, c.ID)
	}
	c.kind = kind
	seen := map[string]bool{}
	for i := range c.Tools {
		t := &c.Tools[i]
		switch {
		case !toolName.MatchString(t.Name) || !strings.HasPrefix(t.Name, kind+"_"):
			return nil, fmt.Errorf("contract %s: tool %q is misnamed", kind, t.Name)
		case seen[t.Name]:
			return nil, fmt.Errorf("contract %s: tool %s is listed twice", kind, t.Name)
		case t.Access != Read && t.Access != Write:
			return nil, fmt.Errorf("contract %s: tool %s has access %q", kind, t.Name, t.Access)
		case t.MaxOutputBytes < 1 || t.MaxOutputBytes+envelopeBytes > mcp.DefaultMaxMessageBytes:
			return nil, fmt.Errorf("contract %s: tool %s has an output cap outside the message bound", kind, t.Name)
		case t.TimeoutMS < 1 || t.TimeoutMS > 120_000:
			return nil, fmt.Errorf("contract %s: tool %s has a timeout outside 1..120000 ms", kind, t.Name)
		}
		seen[t.Name] = true
		for label, s := range map[string]*Schema{"input": t.Input, "output": t.Output} {
			if err := s.prepare(t.Name + "." + label); err != nil {
				return nil, fmt.Errorf("contract %s: %w", kind, err)
			}
			if s.Type != "object" || !s.closed {
				return nil, fmt.Errorf("contract %s: %s %s is not a closed object", kind, t.Name, label)
			}
		}
		if outputs[t.Name] == nil {
			return nil, fmt.Errorf("contract %s: tool %s has no decoder", kind, t.Name)
		}
	}
	if !seen[kind+"_describe"] {
		return nil, fmt.Errorf("contract %s has no %s_describe", kind, kind)
	}
	return &c, nil
}

// ID returns the contract id of a kind, forgecell.<kind>/v1.
func ID(kind string) string { return "forgecell." + kind + "/v1" }

// Lookup returns the contract of a kind.
func Lookup(kind string) (*Contract, bool) {
	c, ok := contracts[kind]
	return c, ok
}

// Kind returns the contract's kind.
func (c *Contract) Kind() string { return c.kind }

// Tool returns one tool of the contract.
func (c *Contract) Tool(name string) (*Tool, bool) {
	for i := range c.Tools {
		if c.Tools[i].Name == name {
			return &c.Tools[i], true
		}
	}
	return nil, false
}

// Describe returns the kind's *_describe tool.
func (c *Contract) Describe() *Tool {
	t, _ := c.Tool(c.kind + "_describe")
	return t
}

// Timeout is the tool's default per-call timeout.
func (t *Tool) Timeout() time.Duration { return time.Duration(t.TimeoutMS) * time.Millisecond }

// MessageBytes is the per-call JSON-RPC message bound for this tool: its output
// cap twice over (servers often repeat structured output as text content) plus
// the envelope, never above the client default.
func (t *Tool) MessageBytes() int {
	return min(mcp.DefaultMaxMessageBytes, 2*t.MaxOutputBytes+envelopeBytes)
}

// InputSchema returns the contract input schema as JSON, as a plugin would
// advertise it in tools/list.
func (t *Tool) InputSchema() json.RawMessage {
	raw, err := json.Marshal(t.Input)
	if err != nil {
		panic(err)
	}
	return raw
}

// ValidateInput checks arguments against the tool's input schema. The engine
// sends only arguments that pass; a plugin may use it to refuse bad input.
func (t *Tool) ValidateInput(args any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return t.ValidateInputJSON(raw)
}

// ValidateInputJSON checks raw JSON arguments against the tool's input schema.
func (t *Tool) ValidateInputJSON(raw []byte) error {
	doc, err := strictDocument(raw)
	if err != nil {
		return err
	}
	return t.Input.Validate(doc)
}

// CheckAdvertised checks a plugin's tools/list inputSchema for this tool.
func (t *Tool) CheckAdvertised(inputSchema json.RawMessage) error {
	return t.Input.Compatible(inputSchema)
}
