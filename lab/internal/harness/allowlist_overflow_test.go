package harness

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func manyGoFiles(n int) []readiness.ScopedPath {
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf("lab/pkg/f%04d.go", i)
	}
	return scopeOf(paths...)
}

func TestAllowlistFormatterOverflowDropsOnlyFormatterEntries(t *testing.T) {
	checks := []readiness.Check{shellGofmtCheck("lab")}
	got, note, err := DeriveCodingAllowlistNoted(emptyFormula, checks, manyGoFiles(600)...)
	if err != nil {
		t.Fatalf("a formatter overflow must not fail the derivation: %v", err)
	}
	want := joinEntries(wantBase, []string{"Bash(gofmt -l lab/internal lab/cmd)"})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overflow list\n got %d entries %q\nwant %q", len(got), got[:min(len(got), 12)], want)
	}
	if !strings.Contains(note, "600 per-file formatter entries") || !strings.Contains(note, "512") {
		t.Fatalf("the scope gate note must say what was dropped: %q", note)
	}
	plain, err := DeriveCodingAllowlist(emptyFormula, checks, manyGoFiles(600)...)
	if err != nil || !reflect.DeepEqual(plain, got) {
		t.Fatalf("DeriveCodingAllowlist must agree: %v %v", plain, err)
	}
	if matched, ok, err := MatchCodingAllowlist(emptyFormula, checks, manyGoFiles(600), got); err != nil || !ok || !reflect.DeepEqual(matched, got) {
		t.Fatalf("a stored dropped list must still match: %v %v", ok, err)
	}
}

func TestAllowlistFormatterOverflowKeepsOtherEntriesBounded(t *testing.T) {
	checks := []readiness.Check{shellGofmtCheck("lab")}
	for i := 0; i < 600; i++ {
		checks = append(checks, readiness.Check{ID: fmt.Sprintf("c%d", i), Argv: []string{"go", "test", fmt.Sprintf("./pkg%d", i)}, Dir: "lab", Required: true})
	}
	if _, _, err := DeriveCodingAllowlistNoted(emptyFormula, checks, scopeOf("lab/a.go")...); err == nil {
		t.Fatal("overflow caused by non-formatter entries must still fail")
	}
}

func TestAllowlistFormatterOverflowWithinBoundIsUnchanged(t *testing.T) {
	checks := []readiness.Check{shellGofmtCheck("lab")}
	got, note, err := DeriveCodingAllowlistNoted(emptyFormula, checks, scopeOf("lab/a.go", "lab/b.go")...)
	if err != nil || note != "" {
		t.Fatalf("no drop expected: %q %v", note, err)
	}
	if n := countPrefix(got, "Bash(gofmt -w "); n != 2 {
		t.Fatalf("want two -w entries: %q", got)
	}
}

func TestAllowlistFormatterOverflowRootLevelChecks(t *testing.T) {
	scope := scopeOf("main.go", "internal/x.go", "README.md")
	want := joinEntries(wantBase, []string{"Bash(gofmt -l .)", "Bash(gofmt -w internal/x.go)", "Bash(gofmt -w main.go)"})
	for name, c := range map[string]readiness.Check{
		"direct": {Argv: []string{"gofmt", "-l", "."}, Dir: ".", Required: true},
		"shell":  {Argv: []string{"sh", "-c", `test -z "$(gofmt -l .)"`}, Dir: ".", Required: true},
		"empty":  {Argv: []string{"gofmt", "-l", "."}, Dir: "", Required: true},
	} {
		if got := derive(t, emptyFormula, []readiness.Check{c}, scope); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s root check\n got %q\nwant %q", name, got, want)
		}
	}
	nested := derive(t, emptyFormula, []readiness.Check{{Argv: []string{"gofmt", "-l", "."}, Dir: "lab", Required: true}}, scopeOf("lab/a.go"))
	if wantNested := joinEntries(wantBase, []string{"Bash(gofmt -l lab)", "Bash(gofmt -w lab/a.go)"}); !reflect.DeepEqual(nested, wantNested) {
		t.Fatalf("nested dot check\n got %q\nwant %q", nested, wantNested)
	}
	if got := derive(t, emptyFormula, []readiness.Check{{Argv: []string{"gofmt", "-l", ".."}, Dir: ".", Required: true}}, scope); !reflect.DeepEqual(got, wantBase) {
		t.Fatalf("a parent directory must still fail closed: %q", got)
	}
}
