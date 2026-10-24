package readiness

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSelectAcceptsDirectGoTestCheck(t *testing.T) {
	for _, category := range []string{"regression", "candidate"} {
		p := fixture()
		p.Analysis.Checks[0].Category = category
		p.Analysis.Checks[0].Argv = []string{"go", "test", "-count=1", "-run", "TestX", "./..."}
		p.Analysis.Checks[0].Select = SelectAffectedPackages
		p.FullSuiteTriggers = []string{"lab/cmd/**"}
		if err := p.Validate(); err != nil {
			t.Fatalf("%s: %v", category, err)
		}
	}
}

func TestSelectAcceptsGraphNeutralFlags(t *testing.T) {
	for _, argv := range [][]string{
		{"go", "test", "-v", "-race", "-short", "-failfast", "-json", "-cover", "-benchmem", "./..."},
		{"go", "test", "-timeout", "5m", "-p", "2", "-count", "1", "-skip", "TestSlow", "./..."},
		{"go", "test", "--run=TestX", "-covermode=atomic", "-coverprofile", "c.out", "-bench", ".", "-benchtime=1x", "./..."},
	} {
		p := fixture()
		p.Analysis.Checks[0].Argv = argv
		p.Analysis.Checks[0].Select = SelectAffectedPackages
		if err := p.Validate(); err != nil {
			t.Fatalf("%q: %v", argv, err)
		}
	}
}

// A -tags check selects packages whose tests exist only under that build tag, which
// go list without the tag cannot see, so build-affecting flags refuse selection.
func TestSelectRefusesBuildAffectingFlags(t *testing.T) {
	for _, argv := range [][]string{
		{"go", "test", "-tags", "integration", "./..."},
		{"go", "test", "-tags=integration", "./..."},
		{"go", "test", "--tags=integration", "./..."},
		{"go", "test", "-mod=mod", "./..."},
		{"go", "test", "-modfile", "alt.mod", "./..."},
		{"go", "test", "-overlay=o.json", "./..."},
		{"go", "test", "-C", "sub", "./..."},
		{"go", "test", "-coverpkg=./...", "./..."},
		{"go", "test", "-unknown", "./..."},
		{"go", "test", "-", "./..."},
		{"go", "test", "./internal/...", "./..."},
		{"go", "test", "-v", "true", "./..."},
		{"go", "test", "-run", "./..."},
	} {
		p := fixture()
		p.Analysis.Checks[0].Argv = argv
		p.Analysis.Checks[0].Select = SelectAffectedPackages
		if err := p.Validate(); err == nil {
			t.Fatalf("accepted select on %q", argv)
		}
		p.Analysis.Checks[0].Select = ""
		if err := ValidateCheckSelect(p.Analysis.Checks[0]); err != nil {
			t.Fatalf("an unselected check is refused: %v", err)
		}
	}
}

func TestSelectRefusesInvalidChecks(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Check)
	}{
		{"unknown value", func(c *Check) { c.Select = "all" }},
		{"shell wrapper", func(c *Check) { c.Argv = []string{"sh", "-c", "go test ./..."} }},
		{"env wrapper", func(c *Check) { c.Argv = []string{"env", "go", "test", "./..."} }},
		{"go vet", func(c *Check) { c.Argv = []string{"go", "vet", "./..."} }},
		{"no package pattern", func(c *Check) { c.Argv = []string{"go", "test"} }},
		{"narrow last pattern", func(c *Check) { c.Argv = []string{"go", "test", "./internal/..."} }},
		{"second ./...", func(c *Check) { c.Argv = []string{"go", "test", "./...", "./..."} }},
		{"args before pattern", func(c *Check) { c.Argv = []string{"go", "test", "-args", "./..."} }},
		{"independent acceptance", func(c *Check) {
			c.Category, c.IndependentProvenance = "independent-acceptance", "Separately reviewed"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture()
			p.Analysis.Checks[0].Select = SelectAffectedPackages
			tc.mutate(&p.Analysis.Checks[0])
			if err := p.Validate(); err == nil {
				t.Fatalf("accepted select on %+v", p.Analysis.Checks[0])
			}
		})
	}
	p := fixture()
	p.Setup = []Check{{ID: "setup", Category: "setup", Argv: []string{"go", "test", "./..."}, Dir: "lab", TimeoutMS: 1000, Definitions: []string{"manifest"}, Reason: "Setup", Evidence: []string{"manifest"}, Select: SelectAffectedPackages}}
	if err := p.Validate(); err == nil {
		t.Fatal("accepted select on a setup command")
	}
	if err := p.ValidateConfiguration(); err == nil {
		t.Fatal("configuration accepted select on a setup command")
	}
}

func TestSelectAbsentKeepsPlanDigest(t *testing.T) {
	p := fixture()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"select"`) || strings.Contains(string(raw), `"fullSuiteTriggers"`) {
		t.Fatalf("an unselected plan serializes the new fields: %s", raw)
	}
	before, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	selected := fixture()
	selected.Analysis.Checks[0].Select = SelectAffectedPackages
	after, err := selected.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("a declared selection does not join the plan digest")
	}
	triggers := fixture()
	triggers.FullSuiteTriggers = []string{"lab/cmd/**"}
	if d, err := triggers.Digest(); err != nil || d == before {
		t.Fatalf("fullSuiteTriggers do not join the plan digest: %v", err)
	}
}

func TestSelectStaysOutOfTheAnalysisContract(t *testing.T) {
	if schema := AnalysisSchema(); strings.Contains(schema, `"select"`) {
		t.Fatalf("analysis schema demands select: %s", schema)
	}
	a := Analysis{Summary: "s", Checks: []Check{{ID: "c", Category: "candidate", Argv: []string{"go", "test", "./..."}, Select: SelectAffectedPackages}}}
	raw, _ := json.Marshal(a)
	if _, err := DecodeAnalysis(raw); err == nil || IsPolicyViolation(err) {
		t.Fatalf("analysis-declared selection was not refused outright: %v", err)
	}
}

func TestSelectFullSuiteTriggerValidationAndMatching(t *testing.T) {
	if err := ValidateFullSuiteTriggers([]string{"lab/cmd/**", "lab/internal/*/testdata/**", "docs/[a-c]*.md", "**/fixtures/**"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{{""}, {"/abs"}, {"a/../b"}, {"./a"}, {"a//b"}, {".git/x"}, {"a/**b"}, {"a/[b"}, {"a\\b"}, {"a", "a"}, {"a\x01"}, {strings.Repeat("a/", 33) + "a"}} {
		if err := ValidateFullSuiteTriggers(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	too := make([]string, MaxFullSuiteTriggers+1)
	for i := range too {
		too[i] = strings.Repeat("x", i+1)
	}
	if err := ValidateFullSuiteTriggers(too); err == nil {
		t.Fatal("accepted too many triggers")
	}
	cases := []struct {
		pattern, file string
		want          bool
	}{
		{"lab/cmd/**", "lab/cmd/forgecell/main.go", true},
		{"lab/cmd/**", "lab/cmd/main.go", true},
		{"lab/cmd/**", "lab/cmdx/main.go", false},
		{"lab/cmd/**", "lab/internal/cmd.go", false},
		{"**/fixtures/**", "a/b/fixtures/c/d.txt", true},
		{"**/fixtures/**", "fixtures/x", true},
		{"lab/*/x.go", "lab/a/x.go", true},
		{"lab/*/x.go", "lab/a/b/x.go", false},
		{"lab/internal/cli/*_test.go", "lab/internal/cli/cli_test.go", true},
		{"README.md", "README.md", true},
		{"README.md", "docs/README.md", false},
	}
	for _, tc := range cases {
		if got := MatchFullSuiteTrigger(tc.pattern, tc.file); got != tc.want {
			t.Errorf("MatchFullSuiteTrigger(%q, %q) = %t", tc.pattern, tc.file, got)
		}
	}
	// Many ** segments against a deep path stay polynomial.
	deep := strings.Repeat("d/", 2000) + "f.go"
	if MatchFullSuiteTrigger(strings.Repeat("**/", 31)+"x.go", deep) {
		t.Fatal("matched a different file name")
	}
	bad := fixture()
	bad.FullSuiteTriggers = []string{"../x"}
	if bad.Validate() == nil || bad.ValidateConfiguration() == nil {
		t.Fatal("plan accepted an unsafe trigger")
	}
}
