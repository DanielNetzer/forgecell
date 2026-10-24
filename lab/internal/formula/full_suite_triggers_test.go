package formula

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestFullSuiteTriggersParseAndDefault(t *testing.T) {
	text := recipe + "fullSuiteTriggers:\n  - lab/cmd/**\n  - lab/internal/cli/*_test.go\n"
	f, err := Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"lab/cmd/**", "lab/internal/cli/*_test.go"}
	if !reflect.DeepEqual(f.FullSuiteTriggers, want) || !reflect.DeepEqual(f.ResolvedFullSuiteTriggers(), want) {
		t.Fatalf("%+v", f.FullSuiteTriggers)
	}
	if f.YAML != text || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(text))) {
		t.Fatal("snapshot changed")
	}
	// The resolved list is a copy, so a caller cannot change the Formula through it.
	f.ResolvedFullSuiteTriggers()[0] = "changed"
	if f.FullSuiteTriggers[0] != "lab/cmd/**" {
		t.Fatal("resolved triggers alias the Formula")
	}
}

func TestFullSuiteTriggersAbsentKeepsBytesSHAAndDefault(t *testing.T) {
	f, err := Parse([]byte(recipe))
	if err != nil {
		t.Fatal(err)
	}
	if f.FullSuiteTriggers != nil || f.YAML != recipe || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(recipe))) {
		t.Fatalf("%+v", f)
	}
	if got := f.ResolvedFullSuiteTriggers(); !reflect.DeepEqual(got, []string{"lab/cmd/**"}) {
		t.Fatalf("default triggers %v", got)
	}
	f.ResolvedFullSuiteTriggers()[0] = "changed"
	if DefaultFullSuiteTriggers[0] != "lab/cmd/**" {
		t.Fatal("resolved default aliases DefaultFullSuiteTriggers")
	}
}

func TestFullSuiteTriggersRejectMalformed(t *testing.T) {
	many := "fullSuiteTriggers:\n"
	for i := 0; i <= 64; i++ {
		many += fmt.Sprintf("  - lab/p%d/**\n", i)
	}
	cases := []struct{ name, text string }{
		{"scalar", "fullSuiteTriggers: lab/cmd/**\n"},
		{"empty", "fullSuiteTriggers: []\n"},
		{"non-string", "fullSuiteTriggers: [1]\n"},
		{"blank", "fullSuiteTriggers: ['']\n"},
		{"absolute", "fullSuiteTriggers: [/etc/**]\n"},
		{"traversal", "fullSuiteTriggers: [lab/../x]\n"},
		{"dot", "fullSuiteTriggers: [./lab]\n"},
		{"git", "fullSuiteTriggers: [.git/**]\n"},
		{"partial double star", "fullSuiteTriggers: [lab/**x]\n"},
		{"bad glob", "fullSuiteTriggers: ['lab/[x']\n"},
		{"backslash", "fullSuiteTriggers: ['lab\\\\cmd']\n"},
		{"duplicate", "fullSuiteTriggers: [lab/cmd/**, lab/cmd/**]\n"},
		{"too long", "fullSuiteTriggers: [" + strings.Repeat("a", 257) + "]\n"},
		{"too many", many},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(recipe + tc.text)); err == nil {
				t.Fatalf("accepted %q", tc.text)
			}
		})
	}
}

func TestRequiredCheckSelectParses(t *testing.T) {
	text := requiredChecksRecipe(func(m map[string]any) {
		m["id"], m["argv"], m["select"] = "baseline-go-test", []string{"go", "test", "./..."}, "affected-packages"
	})
	f, err := Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.RequiredChecks) != 1 || f.RequiredChecks[0].Select != "affected-packages" {
		t.Fatalf("%+v", f.RequiredChecks)
	}
	plain, err := Parse([]byte(requiredChecksRecipe(nil)))
	if err != nil || plain.RequiredChecks[0].Select != "" {
		t.Fatalf("%+v %v", plain.RequiredChecks, err)
	}
	// An explicit empty or null select is the same as an absent one.
	for _, sel := range []any{"", nil} {
		empty, err := Parse([]byte(requiredChecksRecipe(func(m map[string]any) { m["select"] = sel })))
		if err != nil || empty.RequiredChecks[0].Select != "" {
			t.Fatalf("select %v: %+v %v", sel, empty, err)
		}
	}
}

func TestRequiredCheckSelectOmittedWhenMarshalled(t *testing.T) {
	b, err := yaml.Marshal(RequiredCheck{ID: "baseline-go-test", Argv: []string{"go", "test", "./..."}, Dir: ".", TimeoutMS: 1000, Workflow: ".github/workflows/ci.yml", Reason: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "select") {
		t.Fatalf("empty select was written:\n%s", b)
	}
}

func TestRequiredCheckSelectRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		sel  any
	}{
		{"unknown value", []string{"go", "test", "./..."}, "everything"},
		{"non-string", []string{"go", "test", "./..."}, true},
		{"shell", []string{"sh", "-c", "go test ./..."}, "affected-packages"},
		{"vet", []string{"go", "vet", "./..."}, "affected-packages"},
		{"no pattern", []string{"go", "test"}, "affected-packages"},
		{"other last pattern", []string{"go", "test", "./internal/..."}, "affected-packages"},
		{"args", []string{"go", "test", "-args", "./..."}, "affected-packages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := requiredChecksRecipe(func(m map[string]any) { m["argv"], m["select"] = tc.argv, tc.sel })
			if _, err := Parse([]byte(text)); err == nil {
				t.Fatalf("accepted select %v on %v", tc.sel, tc.argv)
			}
		})
	}
}
