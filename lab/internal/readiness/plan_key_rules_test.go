package readiness

import (
	"strings"
	"testing"
)

// TestKeyedPlanKeyRules applies intake.ValidateIssueKey's rules and requires a
// usable slug: keys the Molecule could not name itself by are refused.
func TestKeyedPlanKeyRules(t *testing.T) {
	refused := map[string]string{
		"129 runes":            strings.Repeat("A", 129),
		"129 multibyte runes":  "A" + strings.Repeat("é", 128),
		"format character":     "ENG-1​234",
		"bidi control":         "ENG-1‮234",
		"bidi isolate":         "ENG-1⁦234",
		"line separator":       "ENG-1 234",
		"paragraph separator":  "ENG-1 234",
		"delete":               "ENG-1\x7f234",
		"no usable slug":       "###",
		"non-ASCII only":       "ÉÜ-éü",
		"separators only":      "-._~",
		"huge byte length key": strings.Repeat("é", 4096),
	}
	for name, key := range refused {
		t.Run(name, func(t *testing.T) {
			p := keyedFixture()
			p.Inputs.Issue.Key = key
			if err := p.Validate(); err == nil || err.Error() != "issue must be open and match the selected repository and identity" {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
	for name, key := range map[string]string{
		"128 runes":          strings.Repeat("A", 128),
		"128 with non-ASCII": "A" + strings.Repeat("é", 127),
		"lower case":         "eng-1234",
		"one digit":          "#1",
		"dotted":             "proj.sub-12",
		"slash":              "group/project#12",
	} {
		t.Run(name, func(t *testing.T) {
			p := keyedFixture()
			p.Inputs.Issue.Key = key
			if err := p.Validate(); err != nil {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
}
