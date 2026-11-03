package scaffold

import (
	"encoding/json"
	"errors"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

// describeFixture is the skeleton's *_describe answer for a kind: neutral values
// that every contract's describe output schema accepts.
func describeFixture(kind, name string) (map[string]any, error) {
	switch kind {
	case "tracker":
		return map[string]any{
			"displayName": name,
			"keyPattern":  "^[A-Z][A-Z0-9]*-[0-9]+$",
			"states":      []string{"open", "closed"},
			"selectors":   []string{"oldest"},
		}, nil
	case "forge":
		// The forge contract requires displayName too, so it carries the name as
		// the tracker fixture does.
		return map[string]any{
			"identity":      "example.com/example/project",
			"host":          "example.com",
			"displayName":   name,
			"changeNoun":    "change request",
			"defaultBranch": "main",
			"ciKind":        "none",
			"webURL":        "https://example.com/example/project",
		}, nil
	case "reviewer":
		return map[string]any{
			"displayName": name,
			"severities":  []string{"high", "medium", "low", "info"},
		}, nil
	}
	return nil, errors.New("the kind must be tracker, forge or reviewer")
}

// fixtureJSON renders fixture.json: the describe value under the describe
// tool's name and an empty object for every other contract tool. The describe
// value is decoded under the contract before it is written, so a skeleton never
// starts with an answer its own contract refuses.
func fixtureJSON(c *contract.Contract, name string) ([]byte, error) {
	describe, err := describeFixture(c.Kind(), name)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(describe)
	if err != nil {
		return nil, err
	}
	if _, err := c.Describe().Decode(raw); err != nil {
		return nil, errors.New("the describe fixture does not match its contract")
	}
	out := map[string]any{}
	for _, t := range c.Tools {
		out[t.Name] = map[string]any{}
	}
	out[c.Describe().Name] = json.RawMessage(raw)
	return marshal(out)
}

// toolEntry is one row of tools.json.
type toolEntry struct {
	Name        string          `json:"name"`
	Access      contract.Access `json:"access"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// toolsJSON renders tools.json from the contract as it is now: every tool's
// name, access and input schema, in contract order.
func toolsJSON(c *contract.Contract) ([]byte, error) {
	entries := make([]toolEntry, 0, len(c.Tools))
	for i := range c.Tools {
		t := &c.Tools[i]
		entries = append(entries, toolEntry{Name: t.Name, Access: t.Access, InputSchema: t.InputSchema()})
	}
	return marshal(map[string]any{"tools": entries})
}

// marshal renders indented JSON with a trailing newline.
func marshal(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
