package formula

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHarnessTimeoutMaximumParsing(t *testing.T) {
	f, err := Parse([]byte(strings.Replace(recipe, "timeoutMs: 1000", "timeoutMs: 1000\n  maxTimeoutMs: 5000", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if f.Harness.MaxTimeoutMS != 5000 || f.Harness.TimeoutCeilingMS() != 5000 {
		t.Fatalf("declared maximum lost: %+v", f.Harness)
	}
	plain, err := Parse([]byte(recipe))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Harness.MaxTimeoutMS != 0 || plain.Harness.TimeoutCeilingMS() != 1000 {
		t.Fatalf("a Formula without maxTimeoutMs must gain no extension: %+v", plain.Harness)
	}
}

func TestHarnessTimeoutMaximumRejectsInvalidValues(t *testing.T) {
	for name, line := range map[string]string{
		"zero":          "maxTimeoutMs: 0",
		"negative":      "maxTimeoutMs: -1",
		"over cap":      "maxTimeoutMs: 3600001",
		"string":        `maxTimeoutMs: "5000"`,
		"fraction":      "maxTimeoutMs: 5000.5",
		"below timeout": "maxTimeoutMs: 999",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(strings.Replace(recipe, "timeoutMs: 1000", "timeoutMs: 1000\n  "+line, 1))); err == nil {
				t.Fatalf("accepted %s", line)
			}
		})
	}
	// An unset timeoutMs is zero, so any positive declared maximum is not below it.
	if _, err := Parse([]byte(strings.Replace(recipe, "  timeoutMs: 1000\n", "  maxTimeoutMs: 5000\n", 1))); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessTimeoutCheckNamesBothValues(t *testing.T) {
	b := Binding{TimeoutMS: 1000, MaxTimeoutMS: 5000}
	for _, ok := range []int{0, 1, 1000, 5000} {
		if err := b.CheckTimeout(ok); err != nil {
			t.Fatalf("%d: %v", ok, err)
		}
	}
	err := b.CheckTimeout(5001)
	if err == nil || !strings.Contains(err.Error(), "5001") || !strings.Contains(err.Error(), "5000") {
		t.Fatalf("over-ceiling timeout must name both values: %v", err)
	}
	if err = (Binding{TimeoutMS: 1000}).CheckTimeout(1001); err == nil {
		t.Fatal("a Formula without maxTimeoutMs must not allow an extension")
	}
	if err = (Binding{}).CheckTimeout(1); err == nil {
		t.Fatal("an unset Formula timeout has no ceiling to extend")
	}
}

// bindingHash JSON-marshals Binding, so an untagged new field would change every
// existing approval's BindingSHA256.
func TestHarnessTimeoutLeavesBindingEncodingUnchanged(t *testing.T) {
	b := Binding{Binding: "codex", Command: []string{"forgecell"}, TimeoutMS: 1000, Instructions: "work"}
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(struct {
		Binding      string
		Command      []string
		TimeoutMS    int
		Instructions string
	}{"codex", []string{"forgecell"}, 1000, "work"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(legacy) {
		t.Fatalf("binding encoding changed:\n%s\n%s", got, legacy)
	}
	b.MaxTimeoutMS = 5000
	if raw, _ := json.Marshal(b); !strings.Contains(string(raw), `"maxTimeoutMs":5000`) {
		t.Fatalf("declared maximum is not bound into the encoding: %s", raw)
	}
}
