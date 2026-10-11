package formula

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

const gofmtLine = `test -z "$(gofmt -l internal cmd)"`

func requiredChecksRecipe(mutate func(m map[string]any)) string {
	m := map[string]any{"id": "baseline-gofmt", "argv": []string{"sh", "-c", gofmtLine}, "dir": "lab", "timeoutMs": 120000, "workflow": ".github/workflows/lab.yml", "reason": "CI runs the gofmt check on pull requests."}
	if mutate != nil {
		mutate(m)
	}
	raw, _ := json.Marshal(m)
	return recipe + "requiredChecks:\n  - " + string(raw) + "\n"
}

func TestRequiredChecksParseIntoFormula(t *testing.T) {
	text := requiredChecksRecipe(nil)
	f, err := Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	want := []RequiredCheck{{ID: "baseline-gofmt", Argv: []string{"sh", "-c", gofmtLine}, Dir: "lab", TimeoutMS: 120000, Workflow: ".github/workflows/lab.yml", Reason: "CI runs the gofmt check on pull requests."}}
	if !reflect.DeepEqual(f.RequiredChecks, want) {
		t.Fatalf("%+v", f.RequiredChecks)
	}
	if f.YAML != text || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(text))) {
		t.Fatal("snapshot changed")
	}
}

func TestRequiredChecksAbsentKeepsBytesAndSHA(t *testing.T) {
	f, err := Parse([]byte(recipe))
	if err != nil {
		t.Fatal(err)
	}
	if f.RequiredChecks != nil || f.YAML != recipe || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(recipe))) {
		t.Fatalf("%+v", f)
	}
	empty, err := Parse([]byte(recipe + "requiredChecks: []\n"))
	if err != nil || len(empty.RequiredChecks) != 0 {
		t.Fatalf("%+v %v", empty, err)
	}
}

func TestRequiredChecksRejectMalformedEntries(t *testing.T) {
	many := recipe + "requiredChecks:\n"
	for i := 0; i <= MaxRequiredChecks; i++ {
		many += fmt.Sprintf("  - {id: c%d, argv: [go, test], dir: '.', timeoutMs: 1000, workflow: .github/workflows/a.yml, reason: r}\n", i)
	}
	duplicate := requiredChecksRecipe(nil) + `  - {"id": "baseline-gofmt", "argv": ["go", "vet", "./..."], "dir": "lab", "timeoutMs": 1000, "workflow": ".github/workflows/lab.yml", "reason": "again"}` + "\n"
	cases := []struct{ name, text string }{
		{"argv string", requiredChecksRecipe(func(m map[string]any) { m["argv"] = "go test ./..." })},
		{"argv empty", requiredChecksRecipe(func(m map[string]any) { m["argv"] = []string{} })},
		{"argv non-string", requiredChecksRecipe(func(m map[string]any) { m["argv"] = []any{"go", 1} })},
		{"argv blank", requiredChecksRecipe(func(m map[string]any) { m["argv"] = []string{"go", " "} })},
		{"argv shell string", requiredChecksRecipe(func(m map[string]any) { m["argv"] = []string{"go test ./..."} })},
		{"dir parent", requiredChecksRecipe(func(m map[string]any) { m["dir"] = "../lab" })},
		{"dir absolute", requiredChecksRecipe(func(m map[string]any) { m["dir"] = "/lab" })},
		{"dir inner parent", requiredChecksRecipe(func(m map[string]any) { m["dir"] = "lab/../x" })},
		{"dir unclean", requiredChecksRecipe(func(m map[string]any) { m["dir"] = "lab//x" })},
		{"dir git", requiredChecksRecipe(func(m map[string]any) { m["dir"] = ".git" })},
		{"dir empty", requiredChecksRecipe(func(m map[string]any) { m["dir"] = "" })},
		{"timeout zero", requiredChecksRecipe(func(m map[string]any) { m["timeoutMs"] = 0 })},
		{"timeout too large", requiredChecksRecipe(func(m map[string]any) { m["timeoutMs"] = 3600001 })},
		{"timeout string", requiredChecksRecipe(func(m map[string]any) { m["timeoutMs"] = "5" })},
		{"workflow elsewhere", requiredChecksRecipe(func(m map[string]any) { m["workflow"] = ".github/other.yml" })},
		{"workflow parent", requiredChecksRecipe(func(m map[string]any) { m["workflow"] = ".github/workflows/../x.yml" })},
		{"workflow extension", requiredChecksRecipe(func(m map[string]any) { m["workflow"] = ".github/workflows/x.txt" })},
		{"workflow missing", requiredChecksRecipe(func(m map[string]any) { delete(m, "workflow") })},
		{"id spaces", requiredChecksRecipe(func(m map[string]any) { m["id"] = "bad id" })},
		{"id empty", requiredChecksRecipe(func(m map[string]any) { m["id"] = "" })},
		{"reason blank", requiredChecksRecipe(func(m map[string]any) { m["reason"] = " " })},
		{"unknown key", requiredChecksRecipe(func(m map[string]any) { m["extra"] = 1 })},
		{"not a list", recipe + "requiredChecks: {id: x}\n"},
		{"null", recipe + "requiredChecks:\n"},
		{"entry not a mapping", recipe + "requiredChecks: [x]\n"},
		{"duplicate id", duplicate},
		{"too many", many},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.text)); err == nil {
				t.Fatalf("accepted:\n%s", tt.text)
			}
		})
	}
}

func TestRequiredChecksValidateAppliesParseRules(t *testing.T) {
	good := RequiredCheck{ID: "baseline-go-test", Argv: []string{"go", "test", "./..."}, Dir: ".", TimeoutMS: 1000, Workflow: ".github/workflows/ci.yaml", Reason: "CI"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*RequiredCheck)
	}{
		{"unclean dir", func(c *RequiredCheck) { c.Dir = "a/../b" }},
		{"empty argv", func(c *RequiredCheck) { c.Argv = nil }},
		{"bad timeout", func(c *RequiredCheck) { c.TimeoutMS = 0 }},
		{"bad workflow", func(c *RequiredCheck) { c.Workflow = "ci.yml" }},
	}
	for _, tt := range mutations {
		bad := good
		tt.mutate(&bad)
		if bad.Validate() == nil {
			t.Fatalf("%s accepted", tt.name)
		}
	}
}
