// Package scaffold writes a plugin skeleton that passes forgecell plugin test
// before its author changes a line, and pins its manifest to a built server.
// The skeleton's tool table is generated from the contract at write time, so
// it never drifts from the contract.
package scaffold

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	case "go":
	case "python", "typescript":
		return errors.New("that language is not supported yet; this version writes go skeletons only")
	default:
		return errors.New("the language must be go")
	}
	if !extension.ValidName(opts.Name) {
		return errors.New("the plugin name must be lowercase letters, digits and dashes, starting with a letter, at most 32 characters")
	}
	files, err := render(c, opts.Name)
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
