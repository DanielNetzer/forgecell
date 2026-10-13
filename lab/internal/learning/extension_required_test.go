package learning

import (
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

func extensionFormula(required bool) formula.Formula {
	return formula.Formula{Extensions: map[string]formula.ExtensionRef{
		"graph": {Manifest: "extensions/graph.json", SHA256: strings.Repeat("ab", 32), Required: required},
	}}
}

func TestLearnCannotDetachRequiredExtension(t *testing.T) {
	attach := func(required bool, coding, analysis []string) formula.Formula {
		f := extensionFormula(required)
		f.Harness.Extensions = coding
		f.Harnesses = map[string]formula.Binding{string(formula.AnalysisHarness): {Extensions: analysis}}
		return f
	}
	g := []string{"graph"}
	for _, c := range []struct {
		name            string
		before, after   formula.Formula
		refusedFromRole string
	}{
		{"detach from coding", attach(true, g, nil), attach(true, nil, nil), "coding"},
		{"detach from analysis", attach(true, nil, g), attach(true, nil, nil), "analysis"},
		{"detach from coding while analysis keeps it", attach(true, g, g), attach(true, nil, g), "coding"},
		{"optional extension may detach", attach(false, g, g), attach(false, nil, nil), ""},
		{"attaching more is allowed", attach(true, g, nil), attach(true, g, g), ""},
	} {
		err := checkExtensionDeclarations(c.before, c.after)
		if c.refusedFromRole == "" {
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), `"graph"`) || !strings.Contains(err.Error(), c.refusedFromRole) {
			t.Fatalf("%s: got %v", c.name, err)
		}
	}
}

func TestLearnCannotLowerRequiredExtension(t *testing.T) {
	for _, c := range []struct {
		name          string
		before, after bool
		refused       bool
	}{
		{"true to false", true, false, true},
		{"true to true", true, true, false},
		{"false to false", false, false, false},
		{"false to true", false, true, false},
	} {
		err := checkExtensionDeclarations(extensionFormula(c.before), extensionFormula(c.after))
		if c.refused {
			if err == nil || !strings.Contains(err.Error(), `"graph"`) || !strings.Contains(err.Error(), "required") {
				t.Fatalf("%s: got %v", c.name, err)
			}
		} else if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}
