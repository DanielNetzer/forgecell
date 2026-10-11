package formula

import (
	"encoding/json"
	"strings"
	"testing"
)

func wrapperRecipe(harness string) []byte {
	return []byte("schemaVersion: v0\nkind: formula\nid: wrapped\nharness:\n" + harness + "atoms:\n  - {id: intake, type: intake}\n")
}

func TestAnalysisWrapperReferenceParsesAndIsCoveredByFormulaBytes(t *testing.T) {
	digest := strings.Repeat("a", 64)
	with, err := Parse(wrapperRecipe("  binding: custom\n  command: [\"/bin/echo\"]\n  analysisWrapper: {manifest: analysis-wrappers/w.json, sha256: " + digest + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	ref := with.Harness.AnalysisWrapper
	if ref == nil || ref.Manifest != "analysis-wrappers/w.json" || ref.SHA256 != digest {
		t.Fatalf("%+v", with.Harness)
	}
	other, err := Parse(wrapperRecipe("  binding: custom\n  command: [\"/bin/echo\"]\n  analysisWrapper: {manifest: analysis-wrappers/w.json, sha256: " + strings.Repeat("b", 64) + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if with.SHA256 == other.SHA256 {
		t.Fatal("the wrapper reference must change the exact Formula approval digest")
	}
	raw, _ := json.Marshal(with.Harness)
	if !strings.Contains(string(raw), `"analysisWrapper"`) {
		t.Fatalf("the binding hash must cover the reference: %s", raw)
	}
}

func TestAnalysisWrapperAbsentReferenceKeepsHistoricalBindingHash(t *testing.T) {
	f, err := Parse(wrapperRecipe("  binding: custom\n  command: [\"/bin/echo\"]\n  timeoutMs: 1000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Harness.AnalysisWrapper != nil {
		t.Fatal("a historical binding must not gain an analysis wrapper")
	}
	raw, _ := json.Marshal(f.Harness)
	want := `{"Binding":"custom","Command":["/bin/echo"],"TimeoutMS":1000,"Instructions":""}`
	if string(raw) != want {
		t.Fatalf("binding encoding changed, which would rebind every approved plan:\n got %s\nwant %s", raw, want)
	}
}

func TestAnalysisWrapperReferenceValidation(t *testing.T) {
	digest := strings.Repeat("c", 64)
	command := "  command: [\"/bin/echo\"]\n"
	cases := [][2]string{
		{"bad digest", command + "  analysisWrapper: {manifest: w.json, sha256: abc}\n"},
		{"uppercase digest", command + "  analysisWrapper: {manifest: w.json, sha256: " + strings.ToUpper(digest) + "}\n"},
		{"absolute path", command + "  analysisWrapper: {manifest: /etc/w.json, sha256: " + digest + "}\n"},
		{"parent path", command + "  analysisWrapper: {manifest: ../w.json, sha256: " + digest + "}\n"},
		{"unclean path", command + "  analysisWrapper: {manifest: a//w.json, sha256: " + digest + "}\n"},
		{"backslash path", command + "  analysisWrapper: {manifest: 'a\\w.json', sha256: " + digest + "}\n"},
		{"empty path", command + "  analysisWrapper: {manifest: '', sha256: " + digest + "}\n"},
		{"unknown key", command + "  analysisWrapper: {manifest: w.json, sha256: " + digest + ", trust: all}\n"},
		{"missing digest", command + "  analysisWrapper: {manifest: w.json}\n"},
		{"not a mapping", command + "  analysisWrapper: w.json\n"},
		{"no coding command", "  binding: custom\n  analysisWrapper: {manifest: w.json, sha256: " + digest + "}\n"},
		{"native adapter", "  command: [\"/usr/local/bin/forgecell\", \"__adapter\", \"codex\", \"/usr/local/bin/codex\"]\n  analysisWrapper: {manifest: w.json, sha256: " + digest + "}\n"},
	}
	for _, c := range cases {
		if _, err := Parse(wrapperRecipe(c[1])); err == nil {
			t.Errorf("%s: Parse accepted the reference", c[0])
		}
	}
	if _, err := Parse(wrapperRecipe(command + "  analysisWrapper: {manifest: analysis-wrappers/w.json, sha256: " + digest + "}\n")); err != nil {
		t.Fatalf("a valid reference was rejected: %v", err)
	}
}

// A role override must not carry an unvalidated wrapper reference: per-role
// bindings (#38) and the analysis wrapper (#2) meet here.
func TestRoleOverrideAnalysisWrapperIsValidated(t *testing.T) {
	recipe := func(role, wrapper string) []byte {
		return []byte("schemaVersion: v0\nkind: formula\nid: wrapped\nharness:\n  binding: custom\n  command: [\"/bin/echo\"]\nharnesses:\n  " + role + ":\n    binding: custom\n    command: [\"/bin/echo\"]\n    analysisWrapper: " + wrapper + "\natoms:\n  - {id: intake, type: intake}\n")
	}
	good := "{manifest: analysis-wrappers/w.json, sha256: " + strings.Repeat("a", 64) + "}"
	if _, err := Parse(recipe("analysis", good)); err != nil {
		t.Fatalf("valid analysis-role wrapper rejected: %v", err)
	}
	for name, tc := range map[string][2]string{
		"bad digest":        {"analysis", "{manifest: analysis-wrappers/w.json, sha256: abc}"},
		"escaping manifest": {"analysis", "{manifest: ../w.json, sha256: " + strings.Repeat("a", 64) + "}"},
		"coding role":       {"coding", good},
	} {
		if _, err := Parse(recipe(tc[0], tc[1])); err == nil {
			t.Fatalf("%s: unvalidated role-override wrapper accepted", name)
		}
	}
}
