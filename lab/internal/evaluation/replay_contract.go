package evaluation

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// replay_contract.go builds the interface contract for a replay task whose hidden tests fail
// to compile at the base tree (a `test-compile` initial failure). It parses the compiler
// errors emitted at base, resolves the referenced declarations at the merge commit with
// go/parser + go/ast + go/printer over the merge tree's non-test .go files, and renders a
// signature-only contract grouped by package. See docs/learning.md ("Replay tasks").

const (
	// replayContractMaxLines caps the rendered contract. A PR whose contract exceeds it is
	// skipped with "interface contract too large".
	replayContractMaxLines = 400

	// replayContractHeading is appended, verbatim, ahead of the per-package go blocks.
	replayContractHeading = "## Interface contract (from the merged change)\n\nHidden acceptance tests use these declarations. Implement them with these exact names and signatures; bodies are yours to write."
)

var (
	// compileErrLineRe matches a Go compiler error line "path:line:col: message" (the column
	// is optional) and captures the message.
	compileErrLineRe = regexp.MustCompile(`^(?:.+\.go):\d+:(?:\d+:)?\s*(.*)$`)

	errUndefinedRe = regexp.MustCompile(`^undefined: (\S+)$`)
	// errFieldMethodRe tolerates a pointer prefix and type arguments on the receiver type, e.g.
	// "x.M undefined (type *pkg.T[int] has no field or method M)".
	errFieldMethodRe  = regexp.MustCompile(`undefined \(type (\**[\w./]+)(?:\[[^\]]*\])? has no field or method (\w+)`)
	errUnknownFieldRe = regexp.MustCompile(`^unknown field (\w+) in struct literal of type (\**[\w./]+)`)
	errArgCountRe     = regexp.MustCompile(`^(?:not enough|too many) arguments in call to (.+)$`)
	errCannotUseRe    = regexp.MustCompile(`^cannot use .+ value in argument to (\S+)$`)
	// errCannotUseStructRe handles "cannot use … value in struct literal of type T" -> T.
	errCannotUseStructRe = regexp.MustCompile(`^cannot use .+ value in struct literal of type (\**[\w./]+)`)
	// errCannotUseTypeRe handles "cannot use … as T value in assignment|return statement" -> T.
	errCannotUseTypeRe = regexp.MustCompile(`^cannot use .+ as (\**[\w./]+) value in (?:assignment|return statement)`)

	moduleLineRe = regexp.MustCompile(`(?m)^module\s+(\S+)`)

	// predeclaredIdents are Go's predeclared type, constant and builtin-function names. A compile
	// error that names one of these (e.g. "cannot use x as string value in assignment" -> string)
	// is a type mismatch against an existing predeclared name, not an identifier the PR adds, so it
	// is never part of the interface contract.
	predeclaredIdents = map[string]bool{
		"bool": true, "byte": true, "complex64": true, "complex128": true,
		"error": true, "float32": true, "float64": true,
		"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
		"rune": true, "string": true,
		"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
		"any": true, "comparable": true,
		"nil": true, "true": true, "false": true, "iota": true,
		// builtin functions
		"append": true, "cap": true, "clear": true, "close": true, "complex": true,
		"copy": true, "delete": true, "imag": true, "len": true, "make": true,
		"max": true, "min": true, "new": true, "panic": true, "print": true,
		"println": true, "real": true, "recover": true,
	}
)

// parseCompileErrors scans build output for "path:line:col: message" lines, extracts the
// identifiers the hidden tests need from the recognized message shapes (sorted, deduplicated),
// and returns every unrecognized compile-error line verbatim (sorted, deduplicated).
func parseCompileErrors(output string) (idents, other []string) {
	identSet := map[string]bool{}
	otherSet := map[string]bool{}
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		m := compileErrLineRe.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		msg := strings.TrimSpace(m[1])
		tok, ok, recognized := extractIdent(msg)
		if ok {
			identSet[tok] = true
			continue
		}
		if recognized {
			// A recognized shape that names only a predeclared identifier is dropped entirely: it is
			// neither a contract identifier nor an unrecognized "other" error.
			continue
		}
		otherSet[trimmed] = true
	}
	return sortedKeys(identSet), sortedKeys(otherSet)
}

// extractIdent extracts the wanted contract identifier from one compiler message. ok is true when
// the message is a recognized shape AND names a non-predeclared identifier. recognized is true
// whenever the message matched a shape, even if the token was then dropped as predeclared, so the
// caller can tell a dropped-predeclared line apart from a genuinely unrecognized one. Tokens are
// either a bare name, a package-qualified name (pkg.Name), a type member (T.Name) or a
// package-qualified type member (pkg.T.Name). Type arguments on a call target are stripped
// (New[int] -> New).
func extractIdent(msg string) (tok string, ok, recognized bool) {
	raw, matched := matchCompileIdent(msg)
	if !matched {
		return "", false, false
	}
	// A bare predeclared name (string, int, error, len, …) is never a contract identifier.
	if !strings.Contains(raw, ".") && predeclaredIdents[raw] {
		return "", false, true
	}
	return raw, true, true
}

// matchCompileIdent matches one compiler message against the recognized shapes and returns the
// raw identifier token, before the predeclared-name filter in extractIdent is applied.
func matchCompileIdent(msg string) (string, bool) {
	if m := errUndefinedRe.FindStringSubmatch(msg); m != nil {
		return m[1], true
	}
	if m := errFieldMethodRe.FindStringSubmatch(msg); m != nil {
		return strings.TrimPrefix(m[1], "*") + "." + m[2], true
	}
	if m := errUnknownFieldRe.FindStringSubmatch(msg); m != nil {
		return strings.TrimPrefix(m[2], "*") + "." + m[1], true
	}
	if m := errArgCountRe.FindStringSubmatch(msg); m != nil {
		return stripTypeArgs(strings.TrimSpace(m[1])), true
	}
	if m := errCannotUseRe.FindStringSubmatch(msg); m != nil {
		return stripTypeArgs(m[1]), true
	}
	if m := errCannotUseStructRe.FindStringSubmatch(msg); m != nil {
		return strings.TrimPrefix(m[1], "*"), true
	}
	if m := errCannotUseTypeRe.FindStringSubmatch(msg); m != nil {
		return strings.TrimPrefix(m[1], "*"), true
	}
	return "", false
}

// stripTypeArgs removes an instantiation suffix ("New[int]" -> "New", "pkg.F[T]" -> "pkg.F").
func stripTypeArgs(s string) string {
	if i := strings.IndexByte(s, '['); i >= 0 {
		return s[:i]
	}
	return s
}

// mergeTree abstracts reading the merge-commit tree, so declaration resolution is exercised
// over an in-memory fixture in tests and over `git show`/`git ls-tree` in production.
type mergeTree interface {
	// read returns the repo-relative file's content at the merge commit.
	read(path string) ([]byte, bool)
	// goFiles returns the repo-relative non-test .go files directly in dir (sorted).
	goFiles(dir string) []string
}

// gitMergeTree reads the merge commit of the source repository.
type gitMergeTree struct {
	ctx    context.Context
	source string
	commit string
}

func (g gitMergeTree) read(path string) ([]byte, bool) {
	return gitShowFile(g.ctx, g.source, g.commit, path)
}

func (g gitMergeTree) goFiles(dir string) []string {
	r := gitResult(g.ctx, g.source, "ls-tree", "-r", "--name-only", g.commit, "--", dir)
	if !r.OK {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		p := strings.TrimSpace(line)
		if p == "" || filepath.ToSlash(filepath.Dir(p)) != dir {
			continue
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, replayGoTestFileSuffx) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// renderInterfaceContract resolves tokens at the merge commit and renders one fenced ```go
// block per package (each headed `// package <import path>`), sorted by package path then by
// name. lineCount is the number of content lines (headers plus declarations, fences excluded),
// used for the cap. resolved counts the tokens that resolved to an actual declaration (not
// "declaration not found"); a caller skips a PR whose contract resolves nothing. Tokens that are
// package-qualified by an import outside the lab module (e.g. io.Reader) are dropped entirely.
// When tokens is empty, or all of them are dropped, the contract is absent (markdown is "").
func renderInterfaceContract(tree mergeTree, testFiles, tokens []string) (markdown string, lineCount, resolved int) {
	if len(tokens) == 0 {
		return "", 0, 0
	}
	module := moduleName(tree)
	cache := map[string]*pkgDecls{}
	getPkg := func(dir string) *pkgDecls {
		if p, ok := cache[dir]; ok {
			return p
		}
		p := parsePkg(tree, dir)
		cache[dir] = p
		return p
	}
	nameMap, dotDirs, outNames := testImports(tree, module, testFiles, getPkg)
	testDirs := uniqueDirs(testFiles)

	type entry struct{ name, rendered string }
	grouped := map[string][]entry{}
	for _, tok := range tokens {
		if i := strings.IndexByte(tok, '.'); i >= 0 && outNames[tok[:i]] {
			continue // out-of-module qualified token: never enters the contract or the fallback
		}
		ip, rendered, found := resolveToken(module, nameMap, dotDirs, testDirs, getPkg, tok)
		if found {
			resolved++
		}
		grouped[ip] = append(grouped[ip], entry{name: tok, rendered: rendered})
	}
	if len(grouped) == 0 {
		return "", 0, 0
	}

	paths := make([]string, 0, len(grouped))
	for p := range grouped {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b strings.Builder
	for _, p := range paths {
		entries := grouped[p]
		sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
		var content strings.Builder
		content.WriteString("// package " + p + "\n")
		lineCount++
		for _, e := range entries {
			lines := strings.Split(strings.TrimRight(e.rendered, "\n"), "\n")
			for _, l := range lines {
				content.WriteString(l + "\n")
			}
			lineCount += len(lines)
		}
		fence := backtickFence(content.String())
		b.WriteString(fence + "go\n")
		b.WriteString(content.String())
		b.WriteString(fence + "\n\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n", lineCount, resolved
}

// backtickFence returns a backtick run one longer than the longest run inside s (minimum three),
// so a rendered declaration that itself contains a ``` run cannot break out of the fence.
func backtickFence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// moduleName returns the module path from lab/go.mod at the merge commit, or "" if unreadable.
func moduleName(tree mergeTree) string {
	raw, ok := tree.read(replayModuleSubdir + "/go.mod")
	if !ok {
		return ""
	}
	if m := moduleLineRe.FindSubmatch(raw); m != nil {
		return string(m[1])
	}
	return ""
}

// testImports maps the package qualifiers used by the hidden test files to the repo-relative
// directory they name, limited to imports inside the lab module. Every in-module import maps
// both its source-spelled name (its alias, or its declared package name when unaliased) and the
// imported package's declared package name, because the compiler prints type names with the
// declared package name regardless of the import alias. dotDirs lists the directories dot-imported
// by the tests, searched for bare names. outNames holds the qualifier names of imports OUTSIDE the
// lab module (stdlib or another module), so a token like io.Reader is dropped from the contract and
// never reaches the cross-package method fallback. The first test file (sorted) to define a name wins.
func testImports(tree mergeTree, module string, testFiles []string, getPkg func(string) *pkgDecls) (nameMap map[string]string, dotDirs []string, outNames map[string]bool) {
	nameMap = map[string]string{}
	outNames = map[string]bool{}
	if module == "" {
		return nameMap, nil, outNames
	}
	put := func(name, dir string) {
		if name == "" {
			return
		}
		if _, exists := nameMap[name]; !exists {
			nameMap[name] = dir
		}
	}
	dotSeen := map[string]bool{}
	files := append([]string{}, testFiles...)
	sort.Strings(files)
	for _, tf := range files {
		src, ok := tree.read(tf)
		if !ok {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), tf, src, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			last := path[strings.LastIndex(path, "/")+1:]
			var dir string
			switch {
			case path == module:
				dir = replayModuleSubdir
			case strings.HasPrefix(path, module+"/"):
				dir = replayModuleSubdir + "/" + strings.TrimPrefix(path, module+"/")
			default:
				// Out-of-module import: record every qualifier name the compiler might print — the
				// alias when aliased, and the last path element (the declared name for stdlib and
				// the usual case) — so io.Reader & friends are excluded from the contract.
				if imp.Name != nil && imp.Name.Name != "_" && imp.Name.Name != "." {
					outNames[imp.Name.Name] = true
				}
				outNames[last] = true
				continue
			}
			realName := getPkg(dir).pkgName
			if realName == "" {
				realName = last
			}
			if imp.Name != nil {
				switch imp.Name.Name {
				case "_":
					continue
				case ".":
					if !dotSeen[dir] {
						dotSeen[dir] = true
						dotDirs = append(dotDirs, dir)
					}
				default:
					put(imp.Name.Name, dir)
				}
			}
			// The compiler prints type names with the package's declared name, so always map it.
			put(realName, dir)
		}
	}
	sort.Strings(dotDirs)
	return nameMap, dotDirs, outNames
}

// dirToImportPath maps a repo-relative directory inside the lab module to its import path.
func dirToImportPath(module, dir string) string {
	if dir == replayModuleSubdir {
		return module
	}
	return module + "/" + strings.TrimPrefix(dir, replayModuleSubdir+"/")
}

// importedDirs returns the sorted, unique in-module directories the hidden test files import
// (both package-qualified imports and dot imports), used for the cross-package method fallback.
func importedDirs(nameMap map[string]string, dotDirs []string) []string {
	set := map[string]bool{}
	for _, dir := range nameMap {
		set[dir] = true
	}
	for _, dir := range dotDirs {
		set[dir] = true
	}
	return sortedKeys(set)
}

// uniqueDirs returns the sorted, unique directories of the given files.
func uniqueDirs(files []string) []string {
	set := map[string]bool{}
	for _, f := range files {
		set[filepath.ToSlash(filepath.Dir(f))] = true
	}
	return sortedKeys(set)
}

// pkgDecls holds the top-level declarations of one package directory at the merge commit,
// parsed into a single FileSet so go/printer can render any of them.
type pkgDecls struct {
	fset          *token.FileSet
	pkgName       string
	funcs         map[string]*ast.FuncDecl   // name -> func (no receiver)
	methods       map[string]*ast.FuncDecl   // "T.M" -> method
	methodsByName map[string][]*ast.FuncDecl // "M" -> every method named M
	types         map[string]*ast.TypeSpec   // name -> type spec
	values        map[string]valueRef        // name -> const/var spec
}

// valueRef is one resolved const/var binding. For a const in a group, typ and val carry the
// implicitly repeated type and value of the last explicit spec. tuple marks a spec whose single
// value cannot be split across its names (e.g. `var a, b = f()`), which renders with no value.
type valueRef struct {
	tok   token.Token // token.CONST or token.VAR
	typ   ast.Expr    // effective declared type, or nil
	val   ast.Expr    // effective value for this name, or nil
	names []string    // all names declared by the spec, for tuple rendering
	tuple bool
}

// parsePkg parses every non-test .go file in dir at the merge commit (best-effort) and indexes
// its top-level declarations.
func parsePkg(tree mergeTree, dir string) *pkgDecls {
	pd := &pkgDecls{
		fset:          token.NewFileSet(),
		funcs:         map[string]*ast.FuncDecl{},
		methods:       map[string]*ast.FuncDecl{},
		methodsByName: map[string][]*ast.FuncDecl{},
		types:         map[string]*ast.TypeSpec{},
		values:        map[string]valueRef{},
	}
	for _, path := range tree.goFiles(dir) {
		src, ok := tree.read(path)
		if !ok {
			continue
		}
		// ParseComments so Doc and line comments are attached to the AST; every render path then
		// strips them with stripComments, since go/printer emits node-attached comments when
		// printing a standalone declaration and no comment text may leak into the contract.
		file, err := parser.ParseFile(pd.fset, path, src, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			continue
		}
		if pd.pkgName == "" && file.Name != nil {
			pd.pkgName = file.Name.Name
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					pd.funcs[d.Name.Name] = d
				} else if recv := recvTypeName(d.Recv); recv != "" {
					pd.methods[recv+"."+d.Name.Name] = d
					pd.methodsByName[d.Name.Name] = append(pd.methodsByName[d.Name.Name], d)
				}
			case *ast.GenDecl:
				indexGenDecl(pd, d)
			}
		}
	}
	return pd
}

// indexGenDecl records the type and value specs of one GenDecl, resolving the implicit repeat of
// a const group's type and value expression list.
func indexGenDecl(pd *pkgDecls, d *ast.GenDecl) {
	var lastType ast.Expr
	var lastValues []ast.Expr
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			pd.types[s.Name.Name] = s
		case *ast.ValueSpec:
			typ, values := s.Type, s.Values
			if d.Tok == token.CONST {
				if typ == nil && len(values) == 0 {
					typ, values = lastType, lastValues
				} else {
					lastType, lastValues = s.Type, s.Values
				}
			}
			names := make([]string, len(s.Names))
			for i, n := range s.Names {
				names[i] = n.Name
			}
			tuple := len(values) > 0 && len(values) != len(s.Names)
			for i, n := range s.Names {
				var val ast.Expr
				if !tuple && i < len(values) {
					val = values[i]
				}
				pd.values[n.Name] = valueRef{tok: d.Tok, typ: typ, val: val, names: names, tuple: tuple}
			}
		}
	}
}

// resolveToken resolves one token to its package import path and rendered declaration. A
// package-qualified token resolves through the test imports; otherwise the token is searched in
// the hidden test packages' directories, then the dot-imported directories. found reports whether
// the token resolved to a real declaration; an unresolved token renders as
// "Name // declaration not found" under its best-known package with found false.
func resolveToken(module string, nameMap map[string]string, dotDirs, testDirs []string, getPkg func(string) *pkgDecls, tok string) (importPath, rendered string, found bool) {
	segs := strings.Split(tok, ".")
	if len(segs) >= 2 {
		if dir, ok := nameMap[segs[0]]; ok {
			ip := dirToImportPath(module, dir)
			if r, ok := resolveTarget(getPkg(dir), segs[1:]); ok {
				return ip, r, true
			}
			return ip, tok + " // declaration not found", false
		}
	}
	for _, dir := range testDirs {
		if r, ok := resolveTarget(getPkg(dir), segs); ok {
			return dirToImportPath(module, dir), r, true
		}
	}
	for _, dir := range dotDirs {
		if r, ok := resolveTarget(getPkg(dir), segs); ok {
			return dirToImportPath(module, dir), r, true
		}
	}
	// Cross-package fallback for `x.M` where x is a local variable (not an import alias — those
	// return above — and not a type in the searched packages): search for a method named M in
	// every in-module directory imported by the hidden test files, in sorted order so resolution
	// is deterministic. This catches e.g. a test in lab/app calling a new method defined in lab/calc.
	if len(segs) == 2 {
		for _, dir := range importedDirs(nameMap, dotDirs) {
			if ms := getPkg(dir).methodsByName[segs[1]]; len(ms) > 0 {
				return dirToImportPath(module, dir), renderMethods(getPkg(dir).fset, ms), true
			}
		}
	}
	ip := module
	if len(testDirs) > 0 {
		ip = dirToImportPath(module, testDirs[0])
	}
	return ip, tok + " // declaration not found", false
}

// resolveTarget renders the declaration named by segs within one package, or reports false.
func resolveTarget(pd *pkgDecls, segs []string) (string, bool) {
	switch len(segs) {
	case 1:
		name := segs[0]
		if fd, ok := pd.funcs[name]; ok {
			return renderFuncDecl(pd.fset, fd), true
		}
		if ts, ok := pd.types[name]; ok {
			return renderTypeSpec(pd.fset, ts), true
		}
		if vr, ok := pd.values[name]; ok {
			return renderValue(pd.fset, vr, name), true
		}
	case 2:
		t, member := segs[0], segs[1]
		if fd, ok := pd.methods[t+"."+member]; ok {
			return renderFuncDecl(pd.fset, fd), true
		}
		if ts, ok := pd.types[t]; ok {
			// t names a known type: resolve the member as a field (or interface member), and do
			// not fall through to the method-by-name search — the member genuinely does not exist.
			switch tt := ts.Type.(type) {
			case *ast.StructType:
				if f := findField(tt, member); f != nil {
					return renderField(pd.fset, t, member, f), true
				}
			case *ast.InterfaceType:
				// T.M where T is an interface: render the whole interface spec.
				return renderTypeSpec(pd.fset, ts), true
			}
			return "", false
		}
		// t is a local variable (not a type and not an import): resolve the member across every
		// method of that name in the package.
		if ms := pd.methodsByName[member]; len(ms) > 0 {
			return renderMethods(pd.fset, ms), true
		}
	}
	return "", false
}

// recvTypeName returns the base type name of a method receiver (pointer and type parameters
// stripped), or "".
func recvTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	t := recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch e := t.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexListExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// findField returns the field named name in st, matching both ordinary named fields and embedded
// (promoted) fields whose name is the base name of the embedded type.
func findField(st *ast.StructType, name string) *ast.Field {
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			if embeddedName(f.Type) == name {
				return f
			}
			continue
		}
		for _, n := range f.Names {
			if n.Name == name {
				return f
			}
		}
	}
	return nil
}

// embeddedName returns the field name of an embedded field: the base identifier of its type.
func embeddedName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return embeddedName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr:
		return embeddedName(t.X)
	case *ast.IndexListExpr:
		return embeddedName(t.X)
	}
	return ""
}

// stripComments clears every documentation and line comment anywhere within node. go/printer
// prints the Doc and Comment fields attached to declarations, fields, type specs and value specs,
// so clearing them across the whole subtree — params, results, struct fields, interface methods
// and fields nested inside func-typed fields — guarantees no comment text from the merged source
// leaks into the rendered contract.
func stripComments(node ast.Node) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Field:
			x.Doc, x.Comment = nil, nil
		case *ast.TypeSpec:
			x.Doc, x.Comment = nil, nil
		case *ast.ValueSpec:
			x.Doc, x.Comment = nil, nil
		case *ast.FuncDecl:
			x.Doc = nil
		case *ast.GenDecl:
			x.Doc = nil
		}
		return true
	})
}

// renderFuncDecl renders a func or method signature with its body (and all comments) removed.
func renderFuncDecl(fset *token.FileSet, fd *ast.FuncDecl) string {
	clone := *fd
	clone.Body = nil
	stripComments(&clone)
	return printNode(fset, &clone)
}

// renderMethods renders every method in ms (sorted for determinism), one per line.
func renderMethods(fset *token.FileSet, ms []*ast.FuncDecl) string {
	rendered := make([]string, 0, len(ms))
	for _, m := range ms {
		rendered = append(rendered, renderFuncDecl(fset, m))
	}
	sort.Strings(rendered)
	return strings.Join(rendered, "\n")
}

// renderTypeSpec renders a type spec in full — including any type parameters and preserving an
// alias (`type T = U`) — with its doc and line comment removed.
func renderTypeSpec(fset *token.FileSet, ts *ast.TypeSpec) string {
	clone := *ts
	stripComments(&clone)
	return "type " + printNode(fset, &clone)
}

// renderField renders a single struct field within its enclosing type. An embedded field is
// rendered as just its type.
func renderField(fset *token.FileSet, typeName, fieldName string, f *ast.Field) string {
	stripComments(f.Type)
	var line string
	if len(f.Names) == 0 {
		line = printNode(fset, f.Type)
	} else {
		line = fieldName + " " + printNode(fset, f.Type)
	}
	if f.Tag != nil {
		line += " " + f.Tag.Value
	}
	return "type " + typeName + " struct {\n\t" + line + "\n}"
}

// renderValue renders a const or var spec. A value that contains a function literal anywhere — or
// is not a simple literal/identifier/selector/composite/unary/binary expression — is omitted so
// no function body can leak into the contract; the declared type is kept when present.
func renderValue(fset *token.FileSet, vr valueRef, name string) string {
	kw := "var"
	if vr.tok == token.CONST {
		kw = "const"
	}
	if vr.tuple {
		return kw + " " + strings.Join(vr.names, ", ") + " // value omitted"
	}
	if vr.typ != nil {
		stripComments(vr.typ)
	}
	if vr.val != nil && !printableValue(vr.val) {
		if vr.typ != nil {
			return kw + " " + name + " " + printNode(fset, vr.typ)
		}
		return kw + " " + name + " // value omitted"
	}
	out := kw + " " + name
	if vr.typ != nil {
		out += " " + printNode(fset, vr.typ)
	}
	if vr.val != nil {
		stripComments(vr.val)
		out += " = " + printNode(fset, vr.val)
	}
	return out
}

// printableValue reports whether a const/var value is safe to print verbatim: a basic literal, a
// bare identifier, a selector, or a composite/unary/binary/parenthesized expression that contains
// no function literal. Anything else (notably a call expression) is omitted.
func printableValue(e ast.Expr) bool {
	switch e.(type) {
	case *ast.BasicLit, *ast.Ident, *ast.SelectorExpr:
		return !containsFuncLit(e)
	case *ast.CompositeLit, *ast.UnaryExpr, *ast.BinaryExpr, *ast.ParenExpr:
		return !containsFuncLit(e)
	}
	return false
}

// containsFuncLit reports whether e contains a function literal anywhere in its subtree.
func containsFuncLit(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			found = true
			return false
		}
		return !found
	})
	return found
}

// printNode renders an AST node with gofmt's printer configuration.
func printNode(fset *token.FileSet, node any) string {
	var b bytes.Buffer
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	if err := cfg.Fprint(&b, fset, node); err != nil {
		return ""
	}
	return b.String()
}

// appendInterfaceContract appends the contract heading and the per-package go blocks to the
// issue body, ensuring a blank-line separation.
func appendInterfaceContract(body, blocks string) string {
	var b strings.Builder
	b.WriteString(body)
	if len(body) == 0 || !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("\n")
	b.WriteString(replayContractHeading)
	b.WriteString("\n\n")
	b.WriteString(blocks)
	if !strings.HasSuffix(blocks, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}
