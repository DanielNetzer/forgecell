package harness

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const (
	emptyFormula = "kind: formula\nid: sample\n"
	gofmtShell   = `test -z "$(gofmt -l internal cmd)"`
)

func shellGofmtCheck(dir string) readiness.Check {
	return readiness.Check{ID: "baseline-gofmt-lab", Category: "regression", Argv: []string{"sh", "-c", gofmtShell}, Dir: dir, Required: true}
}

func scopeOf(paths ...string) []readiness.ScopedPath {
	out := []readiness.ScopedPath{}
	for _, p := range paths {
		out = append(out, readiness.ScopedPath{Path: p, Reason: "Implementation", Evidence: []string{"issue"}})
	}
	return out
}

func derive(t *testing.T, formula string, checks []readiness.Check, scope []readiness.ScopedPath) []string {
	t.Helper()
	got, err := DeriveCodingAllowlist(formula, checks, scope...)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func preFormatter(t *testing.T, formula string, checks []readiness.Check) []string {
	t.Helper()
	got, err := DerivePreFormatterAllowlist(formula, checks)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func countPrefix(entries []string, prefix string) int {
	n := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry, prefix) {
			n++
		}
	}
	return n
}

func TestRequiredGofmtShellCheckAddsExactFormatterEntries(t *testing.T) {
	scope := scopeOf("lab/internal/harness/allowlist.go", "lab/internal/harness/allowlist_gofmt_test.go", "README.md", "lab/go.mod")
	checks := []readiness.Check{shellGofmtCheck("lab")}
	want := joinEntries(wantBase, []string{
		"Bash(gofmt -l lab/internal lab/cmd)",
		"Bash(gofmt -w lab/internal/harness/allowlist.go)",
		"Bash(gofmt -w lab/internal/harness/allowlist_gofmt_test.go)",
	})
	if got := derive(t, emptyFormula, checks, scope); !reflect.DeepEqual(got, want) {
		t.Fatalf("allowlist\n got %q\nwant %q", got, want)
	}
	if got := preFormatter(t, emptyFormula, checks); !reflect.DeepEqual(got, wantBase) {
		t.Fatalf("the pre-change derivation must stay without formatter entries: %q", got)
	}
	listOnly := joinEntries(wantBase, []string{"Bash(gofmt -l lab/internal lab/cmd)"})
	if got := derive(t, emptyFormula, checks, nil); !reflect.DeepEqual(got, listOnly) {
		t.Fatalf("without scope only the list entry is derived: %q", got)
	}
	got, err := DeriveCodingAllowlist(emptyFormula, checks)
	if err != nil || !reflect.DeepEqual(got, listOnly) {
		t.Fatalf("the two-argument form must still compile and derive: %q %v", got, err)
	}
	again, err := EffectiveCodingAllowlist(derive(t, emptyFormula, checks, scope))
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("the derived list is not canonical: %q %v", again, err)
	}
}

func TestRequiredDirectGofmtCheckAddsExactFormatterEntries(t *testing.T) {
	argv := []string{"gofmt", "-l", "internal", "cmd"}
	nested := []readiness.Check{{Argv: argv, Dir: "lab", Required: true}}
	want := joinEntries(wantBase, []string{"Bash(gofmt -l lab/internal lab/cmd)", "Bash(gofmt -w lab/cmd/main.go)"})
	if got := derive(t, emptyFormula, nested, scopeOf("lab/cmd/main.go", "lab/README.md")); !reflect.DeepEqual(got, want) {
		t.Fatalf("nested direct check\n got %q\nwant %q", got, want)
	}
	root := []readiness.Check{{Argv: argv, Dir: ".", Required: true}}
	want = joinEntries(wantBase, []string{"Bash(gofmt -l internal cmd)", "Bash(gofmt -w cmd/y.go)", "Bash(gofmt -w internal/x.go)"})
	if got := derive(t, emptyFormula, root, scopeOf("internal/x.go", "cmd/y.go")); !reflect.DeepEqual(got, want) {
		t.Fatalf("root direct check\n got %q\nwant %q", got, want)
	}
}

func TestFormatterEntriesIgnoreScopeOrderAndDuplicates(t *testing.T) {
	checks := []readiness.Check{shellGofmtCheck("lab")}
	first := derive(t, emptyFormula, checks, scopeOf("lab/b.go", "lab/a.go", "lab/a.go"))
	second := derive(t, emptyFormula, checks, scopeOf("lab/a.go", "lab/b.go"))
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("scope order or duplicates changed the list:\n%q\n%q", first, second)
	}
	if n := countPrefix(first, "Bash(gofmt -w "); n != 2 {
		t.Fatalf("want one -w entry per distinct .go path, got %d: %q", n, first)
	}
}

func TestFormatterEntriesRefuseOutOfScopePathsAndPatterns(t *testing.T) {
	checks := []readiness.Check{shellGofmtCheck("lab")}
	got := derive(t, emptyFormula, checks, scopeOf("lab/a.go"))
	for _, entry := range got {
		if strings.Contains(entry, "lab/b.go") || strings.Contains(entry, "*") {
			t.Fatalf("unapproved path or pattern reached the allowlist: %q", entry)
		}
	}
	if countPrefix(got, "Bash(gofmt -w lab/a.go)") != 1 || countPrefix(got, "Bash(gofmt -w ") != 1 {
		t.Fatalf("only the approved .go path may be formattable: %q", got)
	}
	for _, entry := range []string{"Bash(gofmt -w *.go)", "Bash(gofmt -l:*)", "Bash(gofmt -w lab/*.go)", "Bash(gofmt -l lab/internal lab/*)", "Bash(gofmt -w lab/[a].go)"} {
		if _, err := EffectiveCodingAllowlist([]string{entry}); err == nil {
			t.Fatalf("accepted %q", entry)
		}
	}
}

func TestOnlyRequiredChecksGrantFormatterEntries(t *testing.T) {
	scope := scopeOf("lab/a.go")
	for _, argv := range [][]string{{"sh", "-c", gofmtShell}, {"gofmt", "-l", "internal", "cmd"}, {"go", "fmt", "./..."}} {
		checks := []readiness.Check{{Argv: argv, Dir: ".", Required: false}}
		got := derive(t, emptyFormula, checks, scope)
		if want := preFormatter(t, emptyFormula, checks); !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: a candidate check changed the allowlist\n got %q\nwant %q", argv, got, want)
		}
		if countPrefix(got, "Bash(gofmt -w ") != 0 || countPrefix(got, "Bash(go fmt lab/") != 0 {
			t.Fatalf("%q: a candidate check granted a formatter: %q", argv, got)
		}
	}
}

func TestUnrecognisedGofmtShapesFailClosed(t *testing.T) {
	shapes := [][]string{
		{"sh", "-c", "gofmt -w . ; true"},
		{"sh", "-c", `test -z "$(gofmt -l internal; rm -rf x)"`},
		{"sh", "-c", `test -z "$(gofmt -l internal cmd) && true"`},
		{"sh", "-c", `test -z "$(gofmt -l $(pwd))"`},
		{"sh", "-c", "test -z \"$(gofmt -l `pwd`)\""},
		{"sh", "-c", `test -z "$(gofmt -l internal | cat)"`},
		{"sh", "-c", `test -z "$(gofmt -l /abs)"`},
		{"sh", "-c", `test -z "$(gofmt -l ../x)"`},
		{"sh", "-c", `test -z "$(gofmt -l a/../b)"`},
		{"sh", "-c", `test -z "$(gofmt -l -w .)"`},
		{"sh", "-c", `test -z "$(gofmt -l internal/*)"`},
		{"sh", "-c", `test -z "$(gofmt -l internal  cmd)"`},
		{"sh", "-c", `test -z "$(gofmt -l x/.git/y)"`},
		{"sh", "-c", `test -z "$(gofmt -l )"`},
		{"sh", "-c", `test -z "$(gofmt -l internal)"`, "extra"},
		{"sh", "-ce", gofmtShell},
		{"bash", "-c", gofmtShell},
		{"/bin/sh", "-c", gofmtShell},
		{"gofmt", "-w", "internal"},
		{"gofmt", "-l"},
		{"gofmt", "-l", "../x"},
		{"gofmt", "-l", "/abs"},
		{"gofmt", "-l", "-w"},
		{"gofmt", "-l", "a*"},
		{"gofmt", "-l", "a b"},
		{"gofmt", "-l", "x/.git/y"},
		{"gofmt", "-d", "internal"},
	}
	scope := scopeOf("lab/a.go")
	for _, argv := range shapes {
		checks := []readiness.Check{{Argv: argv, Dir: "lab", Required: true}}
		got := derive(t, emptyFormula, checks, scope)
		if !reflect.DeepEqual(got, wantBase) {
			t.Fatalf("%q reached the allowlist: %q", argv, got)
		}
	}
	for _, dir := range []string{"../escape", "/abs", "a b", "lab/../x", "lab*", "-flag", "lab/", "./lab"} {
		got := derive(t, emptyFormula, []readiness.Check{shellGofmtCheck(dir)}, scope)
		if !reflect.DeepEqual(got, wantBase) {
			t.Fatalf("directory %q reached the allowlist: %q", dir, got)
		}
	}
	got := derive(t, emptyFormula, []readiness.Check{{Argv: []string{"gofmt", "-l", "."}, Dir: ".", Required: true}}, scope)
	if !reflect.DeepEqual(got, joinEntries(wantBase, []string{"Bash(gofmt -l .)", "Bash(gofmt -w lab/a.go)"})) {
		t.Fatalf("a root-level gofmt check must list the root and format each in-scope .go file: %q", got)
	}
	for _, entry := range derive(t, emptyFormula, []readiness.Check{shellGofmtCheck("lab")}, scope) {
		if strings.HasPrefix(entry, "Bash(sh") {
			t.Fatalf("the shell itself was granted: %q", entry)
		}
	}
}

func TestFormatterEntriesAreOmittedNotEscaped(t *testing.T) {
	long := "lab/" + strings.Repeat("a", 520) + ".go"
	scope := scopeOf("lab/ok.go", "lab/internal/release/notes.go", "lab/install/x.go", "lab/a b.go", "lab/semi;colon.go", "lab/$(id).go", long, "lab/.git/x.go", "lab/not-go.txt", "lab/x.go.bak", "lab/UPPER.GO")
	got := derive(t, emptyFormula, []readiness.Check{shellGofmtCheck("lab")}, scope)
	want := joinEntries(wantBase, []string{"Bash(gofmt -l lab/internal lab/cmd)", "Bash(gofmt -w lab/ok.go)"})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unsafe scope paths must be omitted without failing derivation\n got %q\nwant %q", got, want)
	}
	for _, entry := range got {
		if len(entry) > maxAllowlistEntryBytes {
			t.Fatalf("entry exceeds the bound: %d", len(entry))
		}
	}
}

func TestRequiredGoFmtCheckAddsExactFilesOnly(t *testing.T) {
	nested := []readiness.Check{{Argv: []string{"go", "fmt", "./..."}, Dir: "lab", Required: true}}
	scope := scopeOf("lab/internal/harness/allowlist.go", "other/x.go", "lab/README.md", "labx/y.go")
	want := joinEntries(wantBase, []string{"Bash(go -C lab fmt ./...)", "Bash(go -C lab fmt internal/harness/allowlist.go)"})
	if got := derive(t, emptyFormula, nested, scope); !reflect.DeepEqual(got, want) {
		t.Fatalf("nested go fmt\n got %q\nwant %q", got, want)
	}
	root := []readiness.Check{{Argv: []string{"go", "fmt", "./..."}, Dir: ".", Required: true}}
	want = joinEntries(wantBase, []string{"Bash(go fmt ./...)", "Bash(go fmt lab/x.go)"})
	if got := derive(t, emptyFormula, root, scopeOf("lab/x.go", "lab/y.txt")); !reflect.DeepEqual(got, want) {
		t.Fatalf("root go fmt\n got %q\nwant %q", got, want)
	}
	if got := derive(t, emptyFormula, nested, scope); countPrefix(got, "Bash(gofmt") != 0 {
		t.Fatalf("a go fmt check must not grant gofmt: %q", got)
	}
	vet := []readiness.Check{{Argv: []string{"go", "vet", "./..."}, Dir: "lab", Required: true}}
	if got := derive(t, emptyFormula, vet, scope); countPrefix(got, "Bash(go -C lab fmt") != 0 {
		t.Fatalf("go fmt entries without a go fmt check: %q", got)
	}
	if got := derive(t, emptyFormula, []readiness.Check{shellGofmtCheck("lab")}, scope); countPrefix(got, "Bash(go ") != 0 {
		t.Fatalf("a gofmt check must not grant go fmt: %q", got)
	}
}

func TestPlansWithoutGofmtCheckDeriveIdenticalAllowlists(t *testing.T) {
	scope := scopeOf("lab/a.go", "lab/internal/b.go", "web/x.js", "README.md")
	for _, c := range allowlistCases() {
		t.Run(c.name, func(t *testing.T) {
			formula := contextFormula(t, c.components...)
			got := derive(t, formula, c.checks, scope)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("scope changed an allowlist with no gofmt check\n got %q\nwant %q", got, c.want)
			}
			if !reflect.DeepEqual(got, preFormatter(t, formula, c.checks)) {
				t.Fatalf("pre-change derivation differs")
			}
		})
	}
	required := []readiness.Check{{Argv: []string{"go", "vet", "./..."}, Dir: "lab", Required: true}, {Argv: []string{"go", "test", "./..."}, Dir: "lab", Required: true}}
	want := joinEntries(wantBase, []string{"Bash(go -C lab test ./...)", "Bash(go -C lab vet ./...)"})
	if got := derive(t, emptyFormula, required, scope); !reflect.DeepEqual(got, want) {
		t.Fatalf("required non-formatter checks changed: %q", got)
	}
}

func TestMatchCodingAllowlistAcceptsOnlyCurrentOrPreFormatterLists(t *testing.T) {
	scope := scopeOf("lab/a.go")
	checks := []readiness.Check{shellGofmtCheck("lab")}
	current := derive(t, emptyFormula, checks, scope)
	legacy := preFormatter(t, emptyFormula, checks)
	for name, stored := range map[string][]string{"current": current, "pre-formatter": legacy} {
		got, ok, err := MatchCodingAllowlist(emptyFormula, checks, scope, stored)
		if err != nil || !ok || !reflect.DeepEqual(got, stored) {
			t.Fatalf("%s: %q %v %v", name, got, ok, err)
		}
	}
	outOfScope := append(append([]string{}, current...), "Bash(gofmt -w lab/other.go)")
	dropped := append([]string{}, current[:len(current)-1]...)
	wildcard := append(append([]string{}, current...), "Bash(gofmt -w *.go)")
	for name, stored := range map[string][]string{"out of scope": outOfScope, "dropped": dropped, "wildcard": wildcard, "absent": nil, "extra": append(append([]string{}, legacy...), "Bash(go test ./x)")} {
		got, ok, err := MatchCodingAllowlist(emptyFormula, checks, scope, stored)
		if err != nil || ok || got != nil {
			t.Fatalf("%s was accepted: %q %v %v", name, got, ok, err)
		}
	}
	other := scopeOf("lab/b.go")
	if _, ok, err := MatchCodingAllowlist(emptyFormula, checks, other, current); err != nil || ok {
		t.Fatalf("a list derived for a different scope was accepted: %v %v", ok, err)
	}
	plain := []readiness.Check{{Argv: []string{"go", "test", "./..."}, Dir: "lab", Required: true}}
	plainList := derive(t, emptyFormula, plain, scope)
	if _, ok, err := MatchCodingAllowlist(emptyFormula, plain, scope, plainList); err != nil || !ok {
		t.Fatalf("a plan without a gofmt check must match its derivation: %v %v", ok, err)
	}
	if _, ok, err := MatchCodingAllowlist(emptyFormula, plain, scope, append(append([]string{}, plainList...), "Bash(gofmt -w lab/a.go)")); err != nil || ok {
		t.Fatalf("a formatter entry without a gofmt check was accepted: %v %v", ok, err)
	}
	if _, _, err := MatchCodingAllowlist("repositoryContext: {components: nope}", checks, scope, current); err == nil {
		t.Fatal("an unreadable Formula was silently accepted")
	}
}

func TestMatchCodingAllowlistKeepsEarlierPlansVerifyingAtTheEntryBound(t *testing.T) {
	checks := []readiness.Check{shellGofmtCheck("lab")}
	for i := 0; i < 300; i++ {
		checks = append(checks, readiness.Check{Argv: []string{"go", "test", fmt.Sprintf("./pkg%d", i)}, Dir: "."})
	}
	paths := []string{}
	for i := 0; i < 256; i++ {
		paths = append(paths, fmt.Sprintf("lab/f%d.go", i))
	}
	scope := scopeOf(paths...)
	derived, err := DeriveCodingAllowlist(emptyFormula, checks, scope...)
	if err != nil {
		t.Fatalf("formatter overflow must be dropped, not fail derivation: %v", err)
	}
	if n := countPrefix(derived, "Bash(gofmt -w "); n != 0 {
		t.Fatalf("the fixture no longer exceeds the entry bound: %d formatter entries kept", n)
	}
	legacy := preFormatter(t, emptyFormula, checks)
	got, ok, err := MatchCodingAllowlist(emptyFormula, checks, scope, legacy)
	if err != nil || !ok || !reflect.DeepEqual(got, legacy) {
		t.Fatalf("an earlier approved list stopped verifying: %v %v", ok, err)
	}
	if _, ok, _ = MatchCodingAllowlist(emptyFormula, checks, scope, append(append([]string{}, legacy...), "Bash(gofmt -w lab/f1.go)")); ok {
		t.Fatal("a list matching neither derivation was accepted")
	}
}
