package extension

import (
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
)

func TestRequireExactKeysMatchesTheManifestRule(t *testing.T) {
	typ := reflect.TypeOf(Manifest{})
	for name, raw := range map[string]string{
		"case variant":     `{"Kind":"mcp-provider"}`,
		"kelvin sign":      `{"Kind":"mcp-provider"}`,
		"nested variant":   `{"server":{"Executable":{"path":"/x","sha256":"y"}}}`,
		"unknown":          `{"kind":"mcp-provider","extra":1}`,
		"in a slice":       `{"tools":[{"name":"a","ACCESS":"read-only"}]}`,
		"embedded variant": `{"prepare":{"TimeoutMS":1}}`,
	} {
		if err := RequireExactKeys([]byte(raw), typ); err == nil {
			t.Errorf("%s: %s was accepted", name, raw)
		}
	}
	if err := RequireExactKeys([]byte(`{"kind":"mcp-provider","tools":[{"name":"a","access":"read-only"}]}`), typ); err != nil {
		t.Fatalf("exact keys refused: %v", err)
	}
	if err := RequireExactKeys([]byte(`{`), typ); err == nil {
		t.Fatal("malformed JSON was accepted")
	}
	if err := RequireExactKeys([]byte(`{}`), nil); err == nil {
		t.Fatal("a nil type was accepted")
	}
}

func TestExportedPinAndCommandRules(t *testing.T) {
	digest := strings.Repeat("a", 64)
	good := pinned.File{Path: "/opt/tool", SHA256: digest}
	if err := ValidatePin("server", good); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]pinned.File{
		"relative":    {Path: "opt/tool", SHA256: digest},
		"unclean":     {Path: "/opt/../tool", SHA256: digest},
		"placeholder": {Path: "/opt/{{CACHE}}", SHA256: digest},
		"uppercase":   {Path: "/opt/tool", SHA256: strings.Repeat("A", 64)},
		"short":       {Path: "/opt/tool", SHA256: "abc"},
	} {
		if err := ValidatePin("server", f); err == nil {
			t.Errorf("%s: pin %+v was accepted", name, f)
		}
	}
	bad := pinned.File{Path: "tool", SHA256: digest}
	if err := (Command{Executable: good, Interpreter: &bad}).Validate("server"); err == nil {
		t.Fatal("a bad interpreter pin was accepted")
	}
	if err := (Command{Executable: good}).Validate("server"); err != nil {
		t.Fatal(err)
	}
}

func TestExportedNameAndArgRules(t *testing.T) {
	if !ValidName("gitlab") || ValidName("GitLab") || ValidName("1x") || ValidName(strings.Repeat("a", 33)) {
		t.Fatal("ValidName disagrees with the manifest name pattern")
	}
	if !ValidToolName("forge_describe") || ValidToolName("forge describe") || ValidToolName("") {
		t.Fatal("ValidToolName disagrees with the manifest tool pattern")
	}
	pins := []pinned.File{{Path: "/opt/tool", SHA256: strings.Repeat("a", 64)}}
	if err := ValidateArgs([]string{"serve", "{{CACHE}}/graph.json", "--flag=x"}, pins); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"nul":            {"a\x00b"},
		"placeholder":    {"{{HOME}}"},
		"escaping":       {"{{CACHE}}/../x"},
		"pinned path":    {"--config=/opt/tool"},
		"unclean pinned": {"/opt/./tool"},
	} {
		if err := ValidateArgs(args, pins); err == nil {
			t.Errorf("%s: %q was accepted", name, args)
		}
	}
}

func TestDecodeStillOwnsMCPProviderManifests(t *testing.T) {
	fx := newFixture(t)
	raw := marshal(t, fx.manifest(t))
	if _, err := Decode(raw); err != nil {
		t.Fatalf("an mcp-provider manifest is refused: %v", err)
	}
	forge := mutateJSON(t, raw, func(m map[string]any) { m["kind"] = "forge" })
	if _, err := Decode(forge); err == nil {
		t.Fatal("extension.Decode accepted a plugin kind")
	}
}
