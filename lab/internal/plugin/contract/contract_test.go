package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContractsLoadWithTheirIDs(t *testing.T) {
	want := map[string]int{"tracker": 4, "forge": 7, "reviewer": 2}
	for _, kind := range Kinds {
		c, ok := Lookup(kind)
		if !ok || c.ID != "forgecell."+kind+"/v1" || c.ID != ID(kind) || c.Kind() != kind {
			t.Fatalf("contract %s: %+v", kind, c)
		}
		if len(c.Tools) != want[kind] {
			t.Fatalf("contract %s has %d tools, want %d", kind, len(c.Tools), want[kind])
		}
		if d := c.Describe(); d == nil || d.Name != kind+"_describe" {
			t.Fatalf("contract %s has no describe tool", kind)
		}
	}
	if _, ok := Lookup("memory"); ok {
		t.Fatal("an unknown kind has a contract")
	}
}

func TestOnlyPublishDraftWrites(t *testing.T) {
	for _, kind := range Kinds {
		c, _ := Lookup(kind)
		for _, tool := range c.Tools {
			if tool.Access != Read && tool.Access != Write {
				t.Fatalf("%s has access %q", tool.Name, tool.Access)
			}
			if (tool.Access == Write) != (tool.Name == "forge_publish_draft") {
				t.Fatalf("%s has access %s", tool.Name, tool.Access)
			}
			if !tool.Required || tool.Timeout() <= 0 || tool.MessageBytes() <= tool.MaxOutputBytes {
				t.Fatalf("%s bounds: %+v", tool.Name, tool)
			}
		}
	}
}

// walk visits every schema node with its path.
func walk(path string, s *Schema, visit func(string, *Schema)) {
	visit(path, s)
	for name, p := range s.Properties {
		walk(path+"."+name, p, visit)
	}
	if s.Items != nil {
		walk(path+"[]", s.Items, visit)
	}
	if s.values != nil {
		walk(path+"{}", s.values, visit)
	}
}

func TestSchemasAreStrictEverywhere(t *testing.T) {
	opaque := map[string]bool{"config": true, "forge": true}
	for _, kind := range Kinds {
		c, _ := Lookup(kind)
		for _, tool := range c.Tools {
			for label, root := range map[string]*Schema{"input": tool.Input, "output": tool.Output} {
				walk(tool.Name+"."+label, root, func(path string, s *Schema) {
					if s.Type != "object" {
						return
					}
					name := path[strings.LastIndex(path, ".")+1:]
					switch {
					case s.closed, s.values != nil:
					case s.open && label == "input" && opaque[name]:
					default:
						t.Errorf("%s is not a closed object", path)
					}
				})
			}
		}
	}
}

func TestPrepareRefusesLooseSchemas(t *testing.T) {
	for name, raw := range map[string]string{
		"unbounded string":   `{"type":"string"}`,
		"unbounded array":    `{"type":"array","items":{"type":"boolean"}}`,
		"implicit open":      `{"type":"object","properties":{}}`,
		"open with props":    `{"type":"object","additionalProperties":true,"maxProperties":1,"properties":{}}`,
		"unbounded map":      `{"type":"object","additionalProperties":{"type":"boolean"}}`,
		"undeclared require": `{"type":"object","additionalProperties":false,"required":["x"]}`,
		"unknown format":     `{"type":"string","maxLength":1,"format":"email"}`,
		"bad pattern":        `{"type":"string","maxLength":1,"pattern":"("}`,
		"unbounded integer":  `{"type":"integer","minimum":0}`,
		"unknown type":       `{"type":"null"}`,
	} {
		var s Schema
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatal(err)
		}
		if err := s.prepare("$"); err == nil {
			t.Errorf("%s: %s was accepted", name, raw)
		}
	}
}

func TestAdvertisedInputSchemaCompatibility(t *testing.T) {
	c, _ := Lookup("forge")
	tool, _ := c.Tool("forge_checks")
	if err := tool.CheckAdvertised(tool.InputSchema()); err != nil {
		t.Fatalf("the contract's own schema is incompatible: %v", err)
	}
	accepted := []string{
		`{"type":"object","properties":{"project":{"type":"string"},"ref":{"type":["string","null"]},"commit":{"type":"string"}},"required":["project"]}`,
		`{"type":"object","properties":{"project":{"type":"string"},"ref":{"type":"string"},"commit":{"type":"string"},"verbose":{"type":"boolean"}}}`,
	}
	for _, raw := range accepted {
		if err := tool.CheckAdvertised(json.RawMessage(raw)); err != nil {
			t.Errorf("%s was refused: %v", raw, err)
		}
	}
	refused := map[string]string{
		"missing required": `{"type":"object","properties":{"project":{"type":"string"},"ref":{"type":"string"}}}`,
		"type change":      `{"type":"object","properties":{"project":{"type":"string"},"ref":{"type":"integer"},"commit":{"type":"string"}}}`,
		"extra required":   `{"type":"object","properties":{"project":{"type":"string"},"ref":{"type":"string"},"commit":{"type":"string"},"token":{"type":"string"}},"required":["token"]}`,
		"not an object":    `{"type":"array"}`,
		"malformed":        `{"type":`,
		"huge":             `{"type":"object","description":"` + strings.Repeat("a", maxAdvertisedSchemaBytes) + `"}`,
	}
	for name, raw := range refused {
		if err := tool.CheckAdvertised(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}
	list, _ := Lookup("tracker")
	listTool, _ := list.Tool("tracker_list")
	numberLimit := `{"type":"object","properties":{"config":{"type":"object"},"selector":{"type":"string"},"limit":{"type":"number"}}}`
	if err := listTool.CheckAdvertised(json.RawMessage(numberLimit)); err != nil {
		t.Fatalf("a number accepts the contract's integer: %v", err)
	}
}

func TestValidateInput(t *testing.T) {
	c, _ := Lookup("tracker")
	tool, _ := c.Tool("tracker_list")
	good := map[string]any{"config": map[string]any{"jql": "x"}, "selector": "label:x", "limit": 10}
	if err := tool.ValidateInput(good); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string]any{
		"missing":   map[string]any{"config": map[string]any{}, "selector": "x"},
		"over":      map[string]any{"config": map[string]any{}, "selector": "x", "limit": 101},
		"unknown":   map[string]any{"config": map[string]any{}, "selector": "x", "limit": 1, "x": 1},
		"not obj":   []any{1},
		"wrongtype": map[string]any{"config": "x", "selector": "x", "limit": 1},
	} {
		if err := tool.ValidateInput(args); err == nil {
			t.Errorf("%s: %v was accepted", name, args)
		}
	}
	if err := tool.ValidateInputJSON([]byte(`{"config":{},"selector":"x","limit":1,"limit":2}`)); err == nil {
		t.Fatal("duplicate keys were accepted")
	}
}
