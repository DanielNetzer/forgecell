package evaluation

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// memTree is an in-memory merge tree for resolution tests.
type memTree map[string]string

func (m memTree) read(path string) ([]byte, bool) {
	s, ok := m[path]
	return []byte(s), ok
}

func (m memTree) goFiles(dir string) []string {
	var out []string
	for p := range m {
		if filepath.ToSlash(filepath.Dir(p)) != dir {
			continue
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func TestParseCompileErrors(t *testing.T) {
	output := strings.Join([]string{
		"./x.go:1:2: undefined: Foo",
		"./x.go:1:2: undefined: pkg.Bar",
		"./x.go:3:4: c.Speed undefined (type Car has no field or method Speed)",
		"./x.go:5:6: not enough arguments in call to Drive",
		"./x.go:7:8: too many arguments in call to pkg.Drive",
		"./x.go:9:10: cannot use x (variable of type int) as string value in argument to Greet",
		"./x.go:11:12: unknown field Color in struct literal of type Car",
		"./x.go:13:14: syntax error: unexpected newline",
	}, "\n")

	idents, other := parseCompileErrors(output)
	wantIdents := []string{"Car.Color", "Car.Speed", "Drive", "Foo", "Greet", "pkg.Bar", "pkg.Drive"}
	if strings.Join(idents, ",") != strings.Join(wantIdents, ",") {
		t.Fatalf("idents = %v, want %v", idents, wantIdents)
	}
	wantOther := []string{"./x.go:13:14: syntax error: unexpected newline"}
	if strings.Join(other, ",") != strings.Join(wantOther, ",") {
		t.Fatalf("other = %v, want %v", other, wantOther)
	}
}

func TestParseCompileErrorsDedup(t *testing.T) {
	// Identical ident references collapse to one; identical verbatim "other" lines collapse to
	// one while distinct lines are all kept.
	output := "a.go:1:1: undefined: Foo\nb.go:2:2: undefined: Foo\nc.go:3: some other error\nc.go:3: some other error\n"
	idents, other := parseCompileErrors(output)
	if strings.Join(idents, ",") != "Foo" {
		t.Fatalf("idents = %v", idents)
	}
	if len(other) != 1 || other[0] != "c.go:3: some other error" {
		t.Fatalf("other = %v", other)
	}
}

// TestParseCompileErrorsDropsPredeclared asserts a compile error that names only a predeclared
// identifier (a type, constant or builtin) yields no contract identifier: "cannot use x as string
// value in assignment" is a mismatch against an existing name, not an identifier the PR adds.
func TestParseCompileErrorsDropsPredeclared(t *testing.T) {
	output := strings.Join([]string{
		"./x.go:1:2: cannot use x (variable of type int) as string value in assignment",
		"./x.go:3:4: cannot use y (variable of type T) as error value in return statement",
		"./x.go:5:6: undefined: Celsius",
	}, "\n")
	idents, other := parseCompileErrors(output)
	// string and error are predeclared and dropped; only the genuinely new Celsius remains, and the
	// predeclared lines are not reclassified as "other" either.
	if strings.Join(idents, ",") != "Celsius" {
		t.Fatalf("idents = %v, want [Celsius]", idents)
	}
	if len(other) != 0 {
		t.Fatalf("other = %v, want none", other)
	}
}

// TestRenderInterfaceContractDropsOutOfModuleQualifier asserts a token qualified by an import
// outside the lab module (io.Reader) is dropped from the contract and never resolved through the
// cross-package method fallback, even when some in-module package happens to define a method of
// that name (here lab/other.Reader).
func TestRenderInterfaceContractDropsOutOfModuleQualifier(t *testing.T) {
	tree := memTree{
		"lab/go.mod":          "module m\n\ngo 1.21\n",
		"lab/other/other.go":  "package other\n\ntype T struct{}\n\nfunc (t *T) Reader() int { return 0 }\n",
		"lab/app/app_test.go": "package app\n\nimport (\n\t\"io\"\n\n\t\"m/other\"\n)\n\nvar _ io.Reader\nvar _ = other.T{}\n",
	}
	got, _, resolved := renderInterfaceContract(tree, []string{"lab/app/app_test.go"}, []string{"io.Reader"})
	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0 (io.Reader must not resolve)", resolved)
	}
	if got != "" {
		t.Fatalf("io.Reader should be dropped, got contract:\n%s", got)
	}
	if strings.Contains(got, "Reader()") {
		t.Fatalf("io.Reader wrongly resolved via the cross-package method fallback:\n%s", got)
	}
}

// TestRenderInterfaceContractAllUnresolved asserts resolved is zero when no extracted token maps to
// a declaration at the merge tree (so the caller can skip with "no interface identifiers recognized").
func TestRenderInterfaceContractAllUnresolved(t *testing.T) {
	tree := memTree{
		"lab/go.mod":      "module m\n\ngo 1.21\n",
		"lab/p/p.go":      "package p\n\nfunc Known() int { return 0 }\n",
		"lab/p/p_test.go": "package p\n",
	}
	got, _, resolved := renderInterfaceContract(tree, []string{"lab/p/p_test.go"}, []string{"Unknown"})
	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0", resolved)
	}
	if !strings.Contains(got, "Unknown // declaration not found") {
		t.Fatalf("unresolved token should render as declaration not found:\n%s", got)
	}
}

// TestRenderInterfaceContract resolves a func, a pointer-receiver method, a struct, a struct
// field, an interface, a const, a cross-package pkg.Name and one unresolved name over an
// in-memory merge tree, and asserts the exact rendered contract with no function bodies.
func TestRenderInterfaceContract(t *testing.T) {
	tree := memTree{
		"lab/go.mod": "module example.com/mod\n\ngo 1.21\n",
		"lab/calc/calc.go": "package calc\n\n" +
			"type Config struct {\n\tName  string\n\tValue int\n}\n\n" +
			"type Shape interface {\n\tArea() float64\n}\n\n" +
			"const MaxRetries = 3\n\n" +
			"func Value2() int { return 2 }\n\n" +
			"type Calc struct{}\n\n" +
			"func (c *Calc) Add(n int) int { return n }\n",
		"lab/other/other.go": "package other\n\nfunc Helper() string { return \"\" }\n",
	}
	testFiles := []string{"lab/calc/calc_test.go"}
	tree["lab/calc/calc_test.go"] = "package calc\n\nimport (\n\t\"testing\"\n\n\t\"example.com/mod/other\"\n)\n\nfunc TestX(t *testing.T) { _ = other.Helper }\n"

	tokens := []string{"Value2", "Calc.Add", "Config", "Config.Name", "Shape", "MaxRetries", "other.Helper", "Missing"}
	got, lineCount, _ := renderInterfaceContract(tree, testFiles, tokens)

	want := "```go\n" +
		"// package example.com/mod/calc\n" +
		"func (c *Calc) Add(n int) int\n" +
		"type Config struct {\n\tName  string\n\tValue int\n}\n" +
		"type Config struct {\n\tName string\n}\n" +
		"const MaxRetries = 3\n" +
		"Missing // declaration not found\n" +
		"type Shape interface {\n\tArea() float64\n}\n" +
		"func Value2() int\n" +
		"```\n\n" +
		"```go\n" +
		"// package example.com/mod/other\n" +
		"func Helper() string\n" +
		"```\n"
	if got != want {
		t.Fatalf("contract mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if lineCount != 17 {
		t.Fatalf("lineCount = %d, want 17", lineCount)
	}
	if strings.Contains(got, "return") || strings.Contains(got, "{ ") {
		t.Fatalf("contract leaked a function body:\n%s", got)
	}
}

func TestRenderInterfaceContractEmpty(t *testing.T) {
	tree := memTree{"lab/go.mod": "module example.com/mod\n\ngo 1.21\n"}
	got, lineCount, _ := renderInterfaceContract(tree, []string{"lab/calc/calc_test.go"}, nil)
	if got != "" || lineCount != 0 {
		t.Fatalf("empty tokens should yield no contract, got %q (%d lines)", got, lineCount)
	}
}

// TestParseCompileErrorsGenericAndPointerShapes covers type-argument stripping on call targets,
// a pointer- and package-qualified field/method message, and deterministic sorting of the
// unrecognized "other" lines.
func TestParseCompileErrorsGenericAndPointerShapes(t *testing.T) {
	output := strings.Join([]string{
		"./x.go:1:2: not enough arguments in call to New[int]",
		"./x.go:3:4: too many arguments in call to pkg.Make[string]",
		"./x.go:5:6: m.Speed undefined (type *calc.Car has no field or method Speed)",
		"./x.go:9:10: zzz unrecognized error",
		"./x.go:7:8: aaa unrecognized error",
	}, "\n")
	idents, other := parseCompileErrors(output)
	wantIdents := []string{"New", "calc.Car.Speed", "pkg.Make"}
	if strings.Join(idents, ",") != strings.Join(wantIdents, ",") {
		t.Fatalf("idents = %v, want %v", idents, wantIdents)
	}
	// Even though line :9: appears before line :7: in the input, "other" comes back sorted.
	wantOther := []string{"./x.go:7:8: aaa unrecognized error", "./x.go:9:10: zzz unrecognized error"}
	if strings.Join(other, "|") != strings.Join(wantOther, "|") {
		t.Fatalf("other = %v, want sorted %v", other, wantOther)
	}
}

// TestRenderValueOmitsFuncLiterals asserts a const/var value that contains a function literal
// anywhere (or is otherwise not a plain literal/identifier/selector/composite) is never printed:
// the type is kept when present, otherwise the value is omitted, so no body can leak.
func TestRenderValueOmitsFuncLiterals(t *testing.T) {
	tree := memTree{
		"lab/go.mod": "module m\n\ngo 1.21\n",
		"lab/p/p.go": "package p\n\n" +
			"type T struct{ Run func() }\n\n" +
			"var H = http.HandlerFunc(func() {})\n" +
			"var R = map[string]func(){\"a\": func() {}}\n" +
			"var P = &T{Run: func() {}}\n" +
			"var Q http.Handler = http.HandlerFunc(func() {})\n",
		"lab/p/p_test.go": "package p\n",
	}
	got, _, _ := renderInterfaceContract(tree, []string{"lab/p/p_test.go"}, []string{"H", "P", "Q", "R"})
	for _, want := range []string{"var H // value omitted", "var R // value omitted", "var P // value omitted", "var Q http.Handler"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "{") {
		t.Fatalf("a function-literal body leaked into the contract:\n%s", got)
	}
}

// TestRenderGenericDecls asserts type parameters survive on both a generic type and a generic
// func, and the func body is still dropped.
func TestRenderGenericDecls(t *testing.T) {
	tree := memTree{
		"lab/go.mod": "module m\n\ngo 1.21\n",
		"lab/g/g.go": "package g\n\n" +
			"type Stack[T any] struct {\n\titems []T\n}\n\n" +
			"func Map[T any, U any](in []T, f func(T) U) []U { return nil }\n",
		"lab/g/g_test.go": "package g\n",
	}
	got, _, _ := renderInterfaceContract(tree, []string{"lab/g/g_test.go"}, []string{"Map", "Stack"})
	for _, want := range []string{"type Stack[T any] struct {", "func Map[T any, U any]", "[]U"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "return nil") {
		t.Fatalf("leaked a function body:\n%s", got)
	}
}

// TestResolvePackageQualifiers covers an aliased import, an import whose declared package name
// differs from the last path element, a dot import resolved for a bare name, and a type-member
// token spelled with the compiler-printed package name while the test file aliased that import.
func TestResolvePackageQualifiers(t *testing.T) {
	tree := memTree{
		"lab/go.mod":           "module m\n\ngo 1.21\n",
		"lab/calc/calc.go":     "package calc\n\ntype Car struct {\n\tDoors int\n}\n\nfunc (c *Car) Speed() int { return 0 }\n",
		"lab/widget/widget.go": "package gadget\n\ntype Widget struct {\n\tN int\n}\n",
		"lab/dots/dots.go":     "package dots\n\nfunc Dotted() string { return \"\" }\n",
		"lab/app/app_test.go":  "package app\n\nimport (\n\tc \"m/calc\"\n\t\"m/widget\"\n\t. \"m/dots\"\n)\n\nvar _ = c.Car{}\nvar _ = gadget.Widget{}\nvar _ = Dotted\n",
	}
	testFiles := []string{"lab/app/app_test.go"}
	tokens := []string{"c.Car", "gadget.Widget", "Dotted", "calc.Car.Speed"}
	got, _, _ := renderInterfaceContract(tree, testFiles, tokens)
	for _, want := range []string{
		"// package m/calc",
		"type Car struct {",
		"func (c *Car) Speed() int",
		"// package m/widget",
		"type Widget struct {",
		"// package m/dots",
		"func Dotted() string",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "declaration not found") {
		t.Fatalf("a qualifier failed to resolve:\n%s", got)
	}
}

// TestResolveMethodByNameAndGeneric resolves a `v.M` token whose receiver is a local variable
// (not a type or import) by searching methods of that name, and a generic call target whose
// instantiation suffix was stripped.
func TestResolveMethodByNameAndGeneric(t *testing.T) {
	tree := memTree{
		"lab/go.mod":      "module m\n\ngo 1.21\n",
		"lab/p/p.go":      "package p\n\ntype Calc struct{}\n\nfunc (x *Calc) Add(n int) int { return n }\n\nfunc New[T any](v T) *Calc { return nil }\n",
		"lab/p/p_test.go": "package p\n",
	}
	got, _, _ := renderInterfaceContract(tree, []string{"lab/p/p_test.go"}, []string{"c.Add", "New"})
	for _, want := range []string{"func (x *Calc) Add(n int) int", "func New[T any](v T) *Calc"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "return") {
		t.Fatalf("leaked a function body:\n%s", got)
	}
}

// TestResolveInterfaceAndEmbedded resolves T.M when T is an interface (printing the whole
// interface) and a promoted (embedded) field by the embedded type's base name.
func TestResolveInterfaceAndEmbedded(t *testing.T) {
	tree := memTree{
		"lab/go.mod":      "module m\n\ngo 1.21\n",
		"lab/p/p.go":      "package p\n\ntype Reader interface {\n\tRead() int\n}\n\ntype Base struct{ ID int }\n\ntype Wrapper struct {\n\t*Base\n\tName string\n}\n",
		"lab/p/p_test.go": "package p\n",
	}
	got, _, _ := renderInterfaceContract(tree, []string{"lab/p/p_test.go"}, []string{"Reader.Read", "Wrapper.Base"})
	for _, want := range []string{"type Reader interface {", "Read() int", "type Wrapper struct {", "*Base"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// The promoted field renders as the embedded type only, with no synthetic field name.
	if strings.Contains(got, "Base *Base") {
		t.Fatalf("embedded field rendered with a name:\n%s", got)
	}
}

// TestRenderConstsAndVars covers an implicitly repeated const in an iota group and a multi-name
// var bound to a single tuple value.
func TestRenderConstsAndVars(t *testing.T) {
	tree := memTree{
		"lab/go.mod":      "module m\n\ngo 1.21\n",
		"lab/p/p.go":      "package p\n\ntype Color int\n\nconst (\n\tA Color = iota\n\tB\n\tC\n)\n\nfunc f() (int, int) { return 0, 0 }\n\nvar a, b = f()\n",
		"lab/p/p_test.go": "package p\n",
	}
	got, _, _ := renderInterfaceContract(tree, []string{"lab/p/p_test.go"}, []string{"B", "a"})
	for _, want := range []string{"const B Color = iota", "var a, b // value omitted"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

// TestParseCompileErrorsCannotUseShapes covers the additional "cannot use …" shapes: an
// assignment and a return statement extract the target type, a struct-literal form extracts the
// struct type, the argument form still extracts the callee, and a form whose target is a
// non-identifier type (a slice) stays in the unrecognized "other" list.
func TestParseCompileErrorsCannotUseShapes(t *testing.T) {
	output := strings.Join([]string{
		"./x.go:1:2: cannot use y (variable of type int) as Celsius value in assignment",
		"./x.go:3:4: cannot use z (variable of type int) as pkg.Temp value in return statement",
		"./x.go:5:6: cannot use \"a\" (untyped string constant) as int value in struct literal of type Config",
		"./x.go:7:8: cannot use w (variable of type *T) as io.Reader value in argument to Consume",
		"./x.go:9:10: cannot use q (variable of type int) as []byte value in assignment",
	}, "\n")
	idents, other := parseCompileErrors(output)
	wantIdents := []string{"Celsius", "Config", "Consume", "pkg.Temp"}
	if strings.Join(idents, ",") != strings.Join(wantIdents, ",") {
		t.Fatalf("idents = %v, want %v", idents, wantIdents)
	}
	wantOther := []string{"./x.go:9:10: cannot use q (variable of type int) as []byte value in assignment"}
	if strings.Join(other, "|") != strings.Join(wantOther, "|") {
		t.Fatalf("other = %v, want %v", other, wantOther)
	}
}

// TestResolveCrossPackageMethodFallback resolves a `x.M` token whose receiver is a local variable
// in the test package by searching the methods of every in-module package the hidden test imports,
// not just the test's own directory: the method lives in a different package (lab/calc) than the
// test (lab/app).
func TestResolveCrossPackageMethodFallback(t *testing.T) {
	tree := memTree{
		"lab/go.mod":          "module m\n\ngo 1.21\n",
		"lab/calc/calc.go":    "package calc\n\ntype Car struct{}\n\nfunc New() *Car { return nil }\n\nfunc (c *Car) Add(a, b int) int { return a + b }\n",
		"lab/app/app_test.go": "package app\n\nimport (\n\t\"testing\"\n\n\t\"m/calc\"\n)\n\nfunc TestApp(t *testing.T) {\n\tc := calc.New()\n\t_ = c.Add(1, 2)\n}\n",
	}
	got, _, _ := renderInterfaceContract(tree, []string{"lab/app/app_test.go"}, []string{"c.Add"})
	if !strings.Contains(got, "// package m/calc") || !strings.Contains(got, "func (c *Car) Add(a, b int) int") {
		t.Fatalf("cross-package method not resolved:\n%s", got)
	}
	if strings.Contains(got, "declaration not found") {
		t.Fatalf("token should resolve:\n%s", got)
	}
	if strings.Contains(got, "return") {
		t.Fatalf("leaked a function body:\n%s", got)
	}
}

// TestRenderStripsComments asserts that no comment text from the merged source — a struct field's
// doc comment, a trailing field comment, or an interface method's doc comment — leaks into the
// rendered contract. The only "//" the contract may contain is the synthetic package header.
func TestRenderStripsComments(t *testing.T) {
	tree := memTree{
		"lab/go.mod": "module m\n\ngo 1.21\n",
		"lab/p/p.go": "package p\n\n" +
			"// Config configures things.\n" +
			"type Config struct {\n" +
			"\t// Name is the doc comment.\n" +
			"\tName string\n" +
			"\tValue int // trailing comment\n" +
			"}\n\n" +
			"// Runner runs.\n" +
			"type Runner interface {\n" +
			"\t// Run does the thing.\n" +
			"\t// impl: return bar(x)+1\n" +
			"\tRun(x int) int\n" +
			"}\n",
		"lab/p/p_test.go": "package p\n",
	}
	got, _, _ := renderInterfaceContract(tree, []string{"lab/p/p_test.go"}, []string{"Config", "Runner"})
	for _, bad := range []string{"configures things", "doc comment", "trailing comment", "Runner runs", "Run does", "impl:"} {
		if strings.Contains(got, bad) {
			t.Fatalf("comment %q leaked into the contract:\n%s", bad, got)
		}
	}
	// Only the synthetic "// package" header may remain; no source "//" comment survives.
	for _, line := range strings.Split(got, "\n") {
		if i := strings.Index(line, "//"); i >= 0 && !strings.HasPrefix(strings.TrimSpace(line), "// package ") {
			t.Fatalf("unexpected comment line %q in:\n%s", line, got)
		}
	}
	for _, want := range []string{"type Config struct {", "Name", "Value int", "type Runner interface {", "Run(x int) int"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

// TestRenderInterfaceContractFencesRawBackticks asserts a block whose rendered text contains a
// run of backticks is fenced with a longer run so it cannot break out of the code fence.
func TestRenderInterfaceContractFencesRawBackticks(t *testing.T) {
	tree := memTree{
		"lab/go.mod":      "module m\n\ngo 1.21\n",
		"lab/p/p.go":      "package p\n\nconst Doc = \"```\"\n",
		"lab/p/p_test.go": "package p\n",
	}
	got, _, _ := renderInterfaceContract(tree, []string{"lab/p/p_test.go"}, []string{"Doc"})
	fence := strings.Repeat("`", 4)
	if !strings.HasPrefix(got, fence+"go\n") {
		t.Fatalf("block not fenced with four backticks:\n%s", got)
	}
	if !strings.Contains(got, "const Doc = \"```\"") {
		t.Fatalf("missing const declaration:\n%s", got)
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), fence) {
		t.Fatalf("closing fence not four backticks:\n%s", got)
	}
}
