package formula

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func modelFormula(harness, harnesses string) string {
	return "kind: formula\nid: model\nharness:\n  binding: default\n  command: [/bin/default]\n  timeoutMs: 1000\n" + harness + harnesses + rolesAtoms
}

func TestBindingModelField(t *testing.T) {
	good := "  model: model-fixture\n"
	roles := "harnesses:\n  analysis: {binding: a, command: [/bin/a], model: analysis-model}\n  coding: {binding: c, command: [/bin/c]}\n  meta: {binding: m, command: [/bin/m], model: meta-model}\n"
	f, err := Parse([]byte(modelFormula(good, roles)))
	if err != nil {
		t.Fatal(err)
	}
	if f.Harness.Model != "model-fixture" || f.Harnesses["analysis"].Model != "analysis-model" || f.Harnesses["meta"].Model != "meta-model" {
		t.Fatalf("%+v %+v", f.Harness, f.Harnesses)
	}
	// An override never inherits the default binding's model.
	if b, override := f.RoleBinding(CodingHarness); !override || b.Model != "" {
		t.Fatalf("the coding override inherited a model: %+v", b)
	}
	unoverridden, err := Parse([]byte(modelFormula(good, "")))
	if err != nil {
		t.Fatal(err)
	}
	if b, override := unoverridden.RoleBinding(CodingHarness); override || b.Model != "model-fixture" {
		t.Fatalf("a role without an override must use the default binding and its model: %+v", b)
	}

	long := strings.Repeat("m", MaxModelLength)
	if _, err := Parse([]byte(modelFormula("  model: "+long+"\n", ""))); err != nil {
		t.Fatalf("a model at the length limit was refused: %v", err)
	}
	for name, field := range map[string]string{
		"empty":        `model: ""`,
		"blank":        `model: "   "`,
		"leading dash": `model: --model`,
		"space":        `model: "a b"`,
		"tab":          "model: \"a\\tb\"",
		"newline":      "model: \"a\\nb\"",
		"NUL":          "model: \"a\\u0000b\"",
		"too long":     "model: " + long + "m",
		"number":       `model: 7`,
		"null":         `model: null`,
		"list":         `model: [a]`,
		"mapping":      `model: {name: a}`,
		"boolean":      `model: true`,
	} {
		t.Run(name+" default", func(t *testing.T) {
			if _, err := Parse([]byte(modelFormula("  "+field+"\n", ""))); err == nil {
				t.Fatalf("%s accepted", field)
			}
		})
		t.Run(name+" role", func(t *testing.T) {
			if _, err := Parse([]byte(modelFormula("", "harnesses:\n  coding: {binding: c, command: [/bin/c], "+field+"}\n"))); err == nil {
				t.Fatalf("%s accepted on a role", field)
			}
		})
	}
}

func TestBindingModelAbsentLeavesTheFormulaUnchanged(t *testing.T) {
	const yaml = "kind: formula\nid: plain\nharness:\n  binding: default\n  command: [/bin/default]\n  timeoutMs: 1000\n  instructions: guide\nharnesses:\n  coding: {binding: c, command: [/bin/c]}\n" + rolesAtoms
	f, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if f.YAML != yaml || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(yaml))) {
		t.Fatal("parsing changed the Formula bytes or SHA")
	}
	for name, b := range map[string]Binding{"default": f.Harness, "role": f.Harnesses["coding"]} {
		encoded, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "model") {
			t.Fatalf("%s: an unpinned Binding encodes a model key: %s", name, encoded)
		}
	}
	pinned := f.Harness
	pinned.Model = "model-fixture"
	before, _ := json.Marshal(f.Harness)
	after, _ := json.Marshal(pinned)
	if string(before) == string(after) {
		t.Fatal("pinning a model leaves the Binding JSON unchanged")
	}
}
