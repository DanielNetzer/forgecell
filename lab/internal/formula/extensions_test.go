package formula

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var extensionDigest = strings.Repeat("ab", 32)

func extensionDeclaration(name string) string {
	return "  " + name + ": {manifest: extensions/" + name + ".json, sha256: " + extensionDigest + "}\n"
}

// extensionRecipe assembles a Formula; defaultList is a harness-level line and
// roles a complete harnesses block, both optional.
func extensionRecipe(top, defaultList, roles string) string {
	return "kind: formula\nid: ext\n" + top + "harness:\n  binding: claude-code\n  command: [/bin/default]\n  timeoutMs: 1000\n" + defaultList + roles + rolesAtoms
}

func extensionRole(role, list string) string {
	return "harnesses:\n  " + role + ": {binding: claude-code, command: [/bin/" + role + "], extensions: " + list + "}\n"
}

func TestExtensionFreeFormulaIsUnchanged(t *testing.T) {
	data := []byte(extensionRecipe("", "", ""))
	f, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if f.YAML != string(data) || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
		t.Fatal("exact bytes or digest changed")
	}
	if f.Extensions != nil || f.Harness.Extensions != nil || f.Harnesses != nil {
		t.Fatalf("extension state appeared: %+v", f)
	}
	binding, err := json.Marshal(f.Harness)
	if err != nil {
		t.Fatal(err)
	}
	if string(binding) != `{"Binding":"claude-code","Command":["/bin/default"],"TimeoutMS":1000,"Instructions":""}` {
		t.Fatalf("binding JSON changed: %s", binding)
	}
	whole, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(whole), `"extensions"`) {
		t.Fatalf("Formula JSON changed: %s", whole)
	}
	for _, role := range HarnessRoles {
		if got := f.RoleExtensions(role); len(got) != 0 {
			t.Fatalf("%s resolved %v", role, got)
		}
	}
}

func TestExtensionDeclarationsParse(t *testing.T) {
	top := "extensions:\n  graphify: {manifest: extensions/graphify.json, sha256: " + extensionDigest + ", required: true}\n  unused: {manifest: extensions/unused.json, sha256: " + extensionDigest + "}\n"
	f, err := Parse([]byte(extensionRecipe(top, "  extensions: [graphify]\n", "")))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Extensions["graphify"]; got != (ExtensionRef{Manifest: "extensions/graphify.json", SHA256: extensionDigest, Required: true}) {
		t.Fatalf("%+v", got)
	}
	if got := f.Extensions["unused"]; got.Required || len(f.Extensions) != 2 {
		t.Fatalf("required must default to false and unreferenced declarations are accepted: %+v", f.Extensions)
	}
	if !slices.Equal(f.Harness.Extensions, []string{"graphify"}) {
		t.Fatalf("%v", f.Harness.Extensions)
	}
}

func TestExtensionDeclarationsValidate(t *testing.T) {
	entry := func(body string) string { return "extensions:\n  g: {" + body + "}\n" }
	good := "manifest: e/x.json, sha256: " + extensionDigest
	for name, top := range map[string]string{
		"not a mapping":         "extensions: [g]\n",
		"scalar":                "extensions: g\n",
		"null":                  "extensions:\n",
		"entry not a mapping":   "extensions:\n  g: e/x.json\n",
		"uppercase name":        "extensions:\n  Graph: {" + good + "}\n",
		"underscore name":       "extensions:\n  bad_name: {" + good + "}\n",
		"digit-first name":      "extensions:\n  1g: {" + good + "}\n",
		"empty name":            "extensions:\n  \"\": {" + good + "}\n",
		"over-long name":        "extensions:\n  a" + strings.Repeat("b", 32) + ": {" + good + "}\n",
		"missing manifest":      entry("sha256: " + extensionDigest),
		"missing sha256":        entry("manifest: e/x.json"),
		"non-string manifest":   entry("manifest: 5, sha256: " + extensionDigest),
		"short digest":          entry("manifest: e/x.json, sha256: abc"),
		"uppercase digest":      entry("manifest: e/x.json, sha256: " + strings.ToUpper(extensionDigest)),
		"63 character digest":   entry("manifest: e/x.json, sha256: " + extensionDigest[1:]),
		"65 character digest":   entry("manifest: e/x.json, sha256: " + extensionDigest + "a"),
		"non-hex digest":        entry("manifest: e/x.json, sha256: " + strings.Repeat("g", 64)),
		"string required":       entry(good + ", required: \"true\""),
		"numeric required":      entry(good + ", required: 1"),
		"unknown entry key":     entry(good + ", extra: 1"),
		"duplicate entry key":   entry(good + ", manifest: e/y.json"),
		"duplicate declaration": "extensions:\n  g: {" + good + "}\n  g: {" + good + "}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(extensionRecipe(top, "", ""))); err == nil {
				t.Fatal("invalid declaration accepted")
			}
		})
	}
}

func TestExtensionManifestPathMustStayInLab(t *testing.T) {
	parse := func(path string) error {
		top := "extensions:\n  g: {manifest: " + strconv.Quote(path) + ", sha256: " + extensionDigest + "}\n"
		_, err := Parse([]byte(extensionRecipe(top, "", "")))
		return err
	}
	for _, p := range []string{
		"", ".", "../x.json", "/abs/x.json", "e/../../x.json", "e/../x.json", "./e/x.json", "e//x.json", "e/", "e/./x.json",
		"e\\x.json", "e/x\x00.json", strings.Repeat("a", 513),
		"workspaces/issue-1/x.json", "verification/x.json", "ledgers/x.json", "Workspaces/x.json", "LEDGERS/x.json", "Verification/x.json",
	} {
		if err := parse(p); err == nil {
			t.Errorf("manifest path %q accepted", p)
		}
	}
	for _, p := range []string{"extensions/graphify.json", "graphify.json", "workspace/x.json", "e/workspaces/x.json", "a..b/x.json", strings.Repeat("a", 512)} {
		if err := parse(p); err != nil {
			t.Errorf("manifest path %q rejected: %v", p, err)
		}
	}
}

func TestExtensionBindingReferences(t *testing.T) {
	declared := extensionDeclaration("a") + extensionDeclaration("b")
	top := "extensions:\n" + declared
	for name, recipe := range map[string]string{
		"unknown default":          extensionRecipe(top, "  extensions: [missing]\n", ""),
		"duplicate default":        extensionRecipe(top, "  extensions: [a, a]\n", ""),
		"non-string default":       extensionRecipe(top, "  extensions: [1]\n", ""),
		"non-list default":         extensionRecipe(top, "  extensions: a\n", ""),
		"mapping default":          extensionRecipe(top, "  extensions: {a: b}\n", ""),
		"null default":             extensionRecipe(top, "  extensions:\n", ""),
		"none declared":            extensionRecipe("", "  extensions: [a]\n", ""),
		"none declared for role":   extensionRecipe("", "", extensionRole("coding", "[a]")),
		"unknown analysis":         extensionRecipe(top, "", extensionRole("analysis", "[missing]")),
		"duplicate analysis":       extensionRecipe(top, "", extensionRole("analysis", "[b, a, b]")),
		"non-string analysis":      extensionRecipe(top, "", extensionRole("analysis", "[[a]]")),
		"unknown coding override":  extensionRecipe(top, "", extensionRole("coding", "[missing]")),
		"meta role is unsupported": extensionRecipe(top, "", extensionRole("meta", "[a]")),
		"empty meta list":          extensionRecipe(top, "", extensionRole("meta", "[]")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(recipe)); err == nil {
				t.Fatal("invalid reference accepted")
			}
		})
	}
	for name, recipe := range map[string]string{
		"declared but unreferenced": extensionRecipe(top, "", ""),
		"empty list":                extensionRecipe(top, "  extensions: []\n", ""),
		"both roles":                extensionRecipe(top, "  extensions: [a, b]\n", extensionRole("analysis", "[a]")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(recipe)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtensionRoleExtensionsResolve(t *testing.T) {
	top := "extensions:\n" + extensionDeclaration("a") + extensionDeclaration("b")
	parse := func(defaultList, roles string) Formula {
		t.Helper()
		f, err := Parse([]byte(extensionRecipe(top, defaultList, roles)))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	want := func(f Formula, role HarnessRole, names ...string) {
		t.Helper()
		if got := f.RoleExtensions(role); len(got) != len(names) || (len(names) > 0 && !slices.Equal(got, names)) {
			t.Fatalf("%s = %v, want %v", role, got, names)
		}
	}

	f := parse("  extensions: [b, a]\n", "")
	want(f, CodingHarness, "b", "a")
	want(f, AnalysisHarness)
	want(f, MetaHarness)
	want(f, HarnessRole("other"))

	f = parse("  extensions: [b, a]\n", extensionRole("coding", "[a]"))
	want(f, CodingHarness, "a")
	want(f, AnalysisHarness)

	f = parse("  extensions: [a]\n", "harnesses:\n  coding: {binding: claude-code, command: [/bin/coding]}\n")
	want(f, CodingHarness)

	f = parse("  extensions: [a]\n", extensionRole("analysis", "[b]"))
	want(f, AnalysisHarness, "b")
	want(f, CodingHarness, "a")
	want(f, MetaHarness)

	got := f.RoleExtensions(AnalysisHarness)
	got[0] = "mutated"
	want(f, AnalysisHarness, "b")
	if f.Harnesses["analysis"].Extensions[0] != "b" {
		t.Fatal("RoleExtensions returned the stored slice")
	}
}
