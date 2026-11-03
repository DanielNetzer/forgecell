// Package scaffold writes a plugin skeleton that passes forgecell plugin test
// before its author changes a line, and pins its manifest to a built server.
// The skeleton's tool table is generated from the contract at write time, so
// it never drifts from the contract.
package scaffold

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/DanielNetzer/forgecell/lab/internal/extension"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// Options name the skeleton: its kind (tracker, forge or reviewer), its
// language and its plugin name.
type Options struct {
	Kind, Lang, Name string
}

// File names of a skeleton.
const (
	TemplateFile = "plugin.json.tmpl"
	ManifestFile = "plugin.json"
)

const (
	fileMode = 0o600
	dirMode  = 0o700
)

//go:embed templates
var templates embed.FS

// Write creates a skeleton in dir. dir must not exist yet (its parent must), or
// be an empty directory the user owns; it is never a symlink. Files are created
// exclusively, never overwritten, and on any failure only the files this call
// created are removed. Errors never name a local path.
func Write(dir string, opts Options) error {
	c, ok := contract.Lookup(opts.Kind)
	if !ok {
		return errors.New("the kind must be tracker, forge or reviewer")
	}
	switch opts.Lang {
	case "go", "python", "typescript":
	default:
		return errors.New("the language must be go, python or typescript")
	}
	if !extension.ValidName(opts.Name) {
		return errors.New("the plugin name must be lowercase letters, digits and dashes, starting with a letter, at most 32 characters")
	}
	var files []skeletonFile
	var err error
	if opts.Lang == "go" {
		files, err = render(c, opts.Name)
	} else {
		files, err = renderInterpreted(c, opts.Name, opts.Lang)
	}
	if err != nil {
		return err
	}
	if dir == "" {
		return errors.New("the directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return errors.New("the directory cannot be resolved")
	}
	return writeFiles(abs, files)
}

// skeletonFile is one file of a skeleton, in write order.
type skeletonFile struct {
	name string
	data []byte
}

// render builds every file in memory, so nothing is written until all of them
// are ready.
func render(c *contract.Contract, name string) ([]skeletonFile, error) {
	tools, err := toolsJSON(c)
	if err != nil {
		return nil, err
	}
	fixture, err := fixtureJSON(c, name)
	if err != nil {
		return nil, err
	}
	manifest, err := manifestTemplate(c, name)
	if err != nil {
		return nil, err
	}
	quote := func(s string) string { return strconv.Quote(s) }
	mainGo, err := fill("templates/go/main.go.tmpl", strings.NewReplacer(
		"{{SERVER_NAME}}", quote(name),
		"{{CONTRACT_ID}}", quote(c.ID),
		"{{PROTOCOL_VERSION}}", quote(mcp.ProtocolVersion),
	))
	if err != nil {
		return nil, err
	}
	goMod, err := fill("templates/go/go.mod.tmpl", strings.NewReplacer("{{NAME}}", name))
	if err != nil {
		return nil, err
	}
	readme, err := fill("templates/go/README.md.tmpl", strings.NewReplacer(
		"{{NAME}}", name,
		"{{KIND}}", c.Kind(),
		"{{CONTRACT}}", c.ID,
		"{{ENABLE}}", enableSnippet(c.Kind(), name),
	))
	if err != nil {
		return nil, err
	}
	return []skeletonFile{
		{"go.mod", goMod},
		{"main.go", mainGo},
		{"tools.json", tools},
		{"fixture.json", fixture},
		{TemplateFile, manifest},
		{"README.md", readme},
	}, nil
}

// renderInterpreted builds a python or typescript skeleton. The engine runs a
// private copy of the pinned script alone, so the tool table and the describe
// fixture are embedded into the script as literals made from the same
// tools.json and fixture.json bytes the skeleton holds for its author to read.
func renderInterpreted(c *contract.Contract, name, lang string) ([]skeletonFile, error) {
	tools, err := toolsJSON(c)
	if err != nil {
		return nil, err
	}
	fixture, err := fixtureJSON(c, name)
	if err != nil {
		return nil, err
	}
	manifest, err := manifestTemplate(c, name)
	if err != nil {
		return nil, err
	}
	if manifest, err = withInterpreter(manifest); err != nil {
		return nil, err
	}
	str, key, value := pyString, pyString, pyValue
	script, dir := "server.py", "templates/python/"
	if lang == "typescript" {
		str, key, value = jsString, jsString, jsValue
		script, dir = "server.ts", "templates/typescript/"
	}
	toolsLit, fixturesLit, err := literals(tools, fixture, key, value)
	if err != nil {
		return nil, err
	}
	server, err := fill(dir+script+".tmpl", strings.NewReplacer(
		"{{SERVER_NAME}}", str(name),
		"{{CONTRACT_ID}}", str(c.ID),
		"{{PROTOCOL_VERSION}}", str(mcp.ProtocolVersion),
		"{{TOOLS}}", toolsLit,
		"{{FIXTURES}}", fixturesLit,
	))
	if err != nil {
		return nil, err
	}
	readme, err := fill(dir+"README.md.tmpl", strings.NewReplacer(
		"{{NAME}}", name,
		"{{KIND}}", c.Kind(),
		"{{CONTRACT}}", c.ID,
		"{{ENABLE}}", enableSnippet(c.Kind(), name),
	))
	if err != nil {
		return nil, err
	}
	files := []skeletonFile{{script, server}}
	if lang == "typescript" {
		build, err := fill(dir+"build.mjs.tmpl", strings.NewReplacer())
		if err != nil {
			return nil, err
		}
		files = append(files, skeletonFile{"build.mjs", build})
	}
	return append(files,
		skeletonFile{"tools.json", tools},
		skeletonFile{"fixture.json", fixture},
		skeletonFile{TemplateFile, manifest},
		skeletonFile{"README.md", readme},
	), nil
}

// withInterpreter adds the server.interpreter line, with the two interpreter
// placeholders, right after the server.executable line of a rendered manifest
// template, so templates/plugin.json.tmpl itself stays as the Go skeleton uses
// it.
func withInterpreter(manifest []byte) ([]byte, error) {
	executable := `    "executable": {"path": "` + serverPath + `", "sha256": "` + serverSHA256 + `"},` + "\n"
	interpreter := `    "interpreter": {"path": "` + interpreterPath + `", "sha256": "` + interpreterSHA256 + `"},` + "\n"
	s := string(manifest)
	if strings.Count(s, executable) != 1 || strings.Contains(s, interpreterPath) || strings.Contains(s, interpreterSHA256) {
		return nil, errors.New("the manifest template has no single server executable line")
	}
	return []byte(strings.Replace(s, executable, executable+interpreter, 1)), nil
}

// literals renders tools.json's tool list and fixture.json's object as source
// literals, one tool or fixture per line, with the fixtures in sorted order.
func literals(tools, fixture []byte, key func(string) string, value func(json.RawMessage) (string, error)) (string, string, error) {
	var table struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(tools, &table); err != nil {
		return "", "", errors.New("the tool table cannot be embedded")
	}
	lines := make([]string, 0, len(table.Tools))
	for _, raw := range table.Tools {
		v, err := value(raw)
		if err != nil {
			return "", "", err
		}
		lines = append(lines, "  "+v+",")
	}
	toolsLit := "[\n" + strings.Join(lines, "\n") + "\n]"
	var fixtures map[string]json.RawMessage
	if err := json.Unmarshal(fixture, &fixtures); err != nil {
		return "", "", errors.New("the fixture cannot be embedded")
	}
	names := make([]string, 0, len(fixtures))
	for name := range fixtures {
		names = append(names, name)
	}
	sort.Strings(names)
	lines = lines[:0]
	for _, name := range names {
		v, err := value(fixtures[name])
		if err != nil {
			return "", "", err
		}
		lines = append(lines, "  "+key(name)+": "+v+",")
	}
	return toolsLit, "{\n" + strings.Join(lines, "\n") + "\n}", nil
}

// jsValue renders a JSON value as a JavaScript literal: compact JSON, which is
// valid JavaScript.
func jsValue(raw json.RawMessage) (string, error) {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return "", errors.New("a value cannot be embedded")
	}
	return b.String(), nil
}

func jsString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// pyValue renders a JSON value as a Python literal: objects as dicts with
// sorted keys, arrays as lists, numbers as written, and true, false and null
// as True, False and None.
func pyValue(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", errors.New("a value cannot be embedded")
	}
	var b strings.Builder
	if err := pyLiteral(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func pyLiteral(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("None")
	case bool:
		if x {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case json.Number:
		b.WriteString(x.String())
	case string:
		b.WriteString(pyString(x))
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := pyLiteral(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyString(k))
			b.WriteString(": ")
			if err := pyLiteral(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return errors.New("a value cannot be embedded")
	}
	return nil
}

// pyString renders s as a double-quoted, ASCII-only Python string literal:
// printable ASCII as itself, and every other character as a \u or \U escape.
func pyString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// fill renders an embedded template and refuses one with a placeholder left.
func fill(path string, r *strings.Replacer) ([]byte, error) {
	raw, err := templates.ReadFile(path)
	if err != nil {
		return nil, errors.New("an embedded template is missing")
	}
	out := r.Replace(string(raw))
	if strings.Contains(out, "{{") {
		return nil, errors.New("an embedded template has a placeholder left over")
	}
	return []byte(out), nil
}

// manifestTemplate renders plugin.json.tmpl: every contract tool with its
// access mapped as plugin.AccessFor maps it, and the server path and digest
// left as the two placeholders pin replaces.
func manifestTemplate(c *contract.Contract, name string) ([]byte, error) {
	raw, err := templates.ReadFile("templates/plugin.json.tmpl")
	if err != nil {
		return nil, errors.New("an embedded template is missing")
	}
	lines := make([]string, 0, len(c.Tools))
	for _, t := range c.Tools {
		entry, err := json.Marshal(map[string]string{"name": t.Name, "access": plugin.AccessFor(t.Access)})
		if err != nil {
			return nil, err
		}
		lines = append(lines, "    "+string(entry))
	}
	values := map[string]string{"{{KIND}}": c.Kind(), "{{NAME}}": name, "{{CONTRACT}}": c.ID}
	pairs := []string{"{{TOOLS}}", strings.Join(lines, ",\n")}
	for placeholder, value := range values {
		quoted, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, placeholder, string(quoted))
	}
	out := strings.NewReplacer(pairs...).Replace(string(raw))
	if leftover(out) {
		return nil, errors.New("the manifest template has a placeholder left over")
	}
	return []byte(out), nil
}

// leftover reports whether a manifest template holds any placeholder other
// than the two pin replaces.
func leftover(s string) bool {
	s = strings.ReplaceAll(s, serverPath, "")
	s = strings.ReplaceAll(s, serverSHA256, "")
	return strings.Contains(s, "{{")
}

// enableSnippet is the Formula block that selects the plugin for its kind.
func enableSnippet(kind, name string) string {
	switch kind {
	case "tracker":
		return "intake:\n  source: " + name
	case "forge":
		return "forge:\n  plugin: " + name
	}
	return "reviewers:\n  - plugin: " + name
}

// writeFiles creates dir (or adopts it when it is an empty directory the user
// owns) and writes every file inside it through an os.Root, so no write can
// leave dir or follow a symlink.
func writeFiles(abs string, files []skeletonFile) (err error) {
	parent, err := os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		return errors.New("the directory's parent is missing or unreadable")
	}
	defer parent.Close()
	base := filepath.Base(abs)
	created := false
	var info fs.FileInfo
	var written []string
	var root *os.Root
	// The cleanup is registered before the directory can be created, so every
	// refusal after that removes it again. It removes only what this call
	// created, newest first, and the directory only while it is still the one
	// this call made (rmdir never removes a directory that holds an entry).
	defer func() {
		if err == nil {
			return
		}
		if root != nil {
			for i := len(written) - 1; i >= 0; i-- {
				root.Remove(written[i])
			}
			root.Close()
			root = nil
		}
		if created {
			now, statErr := parent.Lstat(base)
			if statErr == nil && now.IsDir() && (info == nil || os.SameFile(now, info)) {
				parent.Remove(base)
			}
		}
	}()
	info, err = parent.Lstat(base)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err = parent.Mkdir(base, dirMode); err != nil {
			return errors.New("the directory cannot be created")
		}
		created = true
		if info, err = parent.Lstat(base); err != nil {
			info = nil
			return errors.New("the directory changed while it was created")
		}
	case err != nil:
		return errors.New("the directory is unreadable")
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return errors.New("the directory is a symlink")
	}
	if !info.IsDir() || !ownedByMe(info) {
		return errors.New("the directory must be a directory you own")
	}
	if root, err = parent.OpenRoot(base); err != nil {
		return errors.New("the directory cannot be opened")
	}
	if err = checkEmpty(root, info); err != nil {
		return err
	}
	for _, f := range files {
		made, writeErr := writeExclusive(root, f.name, f.data)
		if made {
			written = append(written, f.name)
		}
		if writeErr != nil {
			return errors.New("writing the skeleton failed")
		}
	}
	return root.Close()
}

// checkEmpty proves the opened root is the directory that was checked and
// that it holds no entry.
func checkEmpty(root *os.Root, want fs.FileInfo) error {
	got, err := root.Stat(".")
	if err != nil || !os.SameFile(want, got) {
		return errors.New("the directory changed while it was opened")
	}
	d, err := root.Open(".")
	if err != nil {
		return errors.New("the directory is unreadable")
	}
	defer d.Close()
	entries, err := d.ReadDir(1)
	if len(entries) > 0 {
		return errors.New("the directory is not empty")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return errors.New("the directory is unreadable")
	}
	return nil
}

// writeExclusive creates name with O_EXCL and O_NOFOLLOW, so an existing file
// or symlink is never written through. made reports whether this call created
// the file, so a failure removes it and never a file it did not create.
func writeExclusive(root *os.Root, name string, data []byte) (made bool, err error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, fileMode)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return true, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return true, err
	}
	return true, f.Close()
}

func ownedByMe(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
