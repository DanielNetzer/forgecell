package scaffold

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/extension"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// The two placeholders of a skeleton's manifest template.
const (
	serverPath   = "{{SERVER_PATH}}"
	serverSHA256 = "{{SERVER_SHA256}}"
)

// The two further placeholders of a python or typescript skeleton's manifest
// template, for the interpreter that runs the script.
const (
	interpreterPath   = "{{INTERPRETER_PATH}}"
	interpreterSHA256 = "{{INTERPRETER_SHA256}}"
)

// Pinned is what pin wrote: the plugin's name and the SHA-256 of plugin.json.
type Pinned struct {
	Name           string
	ManifestSHA256 string
}

// Pin reads dir/plugin.json.tmpl, replaces its placeholders with the absolute,
// symlink-resolved path of server and that file's SHA-256, and replaces
// dir/plugin.json by an atomic rename inside dir (mode 0600). The server must
// be a regular file with its owner exec bit, given by a path that is not itself
// a symlink, and must pass the engine's placement rules: owned by the user or
// root, not group- or world-writable, one hard link, and no ancestor that
// another user owns or can write without the sticky bit. The manifest is read
// back through the directory handle, hashed and decoded before it replaces
// anything, and the digest returned is of those bytes. Errors never name a
// local path.
func Pin(dir, server string) (Pinned, error) {
	if err := checkServerPath(server); err != nil {
		return Pinned{}, err
	}
	if pinsInterpreter(dir) {
		return Pinned{}, errInterpreterNeeded
	}
	resolved, digest, err := hashServer(server)
	if err != nil {
		return Pinned{}, err
	}
	dirAbs, root, err := openOwnedDir(dir)
	if err != nil {
		return Pinned{}, err
	}
	defer root.Close()
	tmpl, err := readTemplate(root)
	if err != nil {
		return Pinned{}, err
	}
	if hasInterpreter(tmpl) {
		return Pinned{}, errInterpreterNeeded
	}
	if strings.Count(tmpl, serverPath) != 1 || strings.Count(tmpl, serverSHA256) != 1 {
		return Pinned{}, errors.New("the manifest template must hold each of {{SERVER_PATH}} and {{SERVER_SHA256}} exactly once")
	}
	if leftover(tmpl) {
		return Pinned{}, errors.New("the manifest template has a placeholder left over")
	}
	manifest := strings.NewReplacer(serverPath, resolved, serverSHA256, digest).Replace(tmpl)
	if _, err := plugin.Decode([]byte(manifest)); err != nil {
		return Pinned{}, errors.New("the pinned manifest is not a valid plugin manifest")
	}
	return replaceManifest(root, dirAbs, []byte(manifest))
}

// PinInterpreted is Pin for a python or typescript skeleton, whose server is a
// script that the pinned interpreter runs. It fills the template's two server
// and two interpreter placeholders with each file's absolute, symlink-resolved
// path and SHA-256. The script gets every check Pin gives the server except the
// owner exec bit, which a script run through an interpreter does not need. The
// interpreter gets all of them, the exec bit included; like the server it may
// be owned by root. A template without the interpreter placeholders is refused.
// The template is read and checked before either file is hashed. Errors never
// name a local path.
func PinInterpreted(dir, server, interpreter string) (Pinned, error) {
	if err := checkPathText("server", server); err != nil {
		return Pinned{}, err
	}
	if err := checkPathText("interpreter", interpreter); err != nil {
		return Pinned{}, err
	}
	dirAbs, root, err := openOwnedDir(dir)
	if err != nil {
		return Pinned{}, err
	}
	defer root.Close()
	tmpl, err := readTemplate(root)
	if err != nil {
		return Pinned{}, err
	}
	if !hasInterpreter(tmpl) {
		return Pinned{}, errors.New("the manifest template pins no interpreter; pin it without --interpreter")
	}
	for _, p := range []string{serverPath, serverSHA256, interpreterPath, interpreterSHA256} {
		if strings.Count(tmpl, p) != 1 {
			return Pinned{}, errors.New("the manifest template must hold each of {{SERVER_PATH}}, {{SERVER_SHA256}}, {{INTERPRETER_PATH}} and {{INTERPRETER_SHA256}} exactly once")
		}
	}
	if leftover(strings.NewReplacer(interpreterPath, "", interpreterSHA256, "").Replace(tmpl)) {
		return Pinned{}, errors.New("the manifest template has a placeholder left over")
	}
	resolved, digest, err := hashPinned("server", server, false)
	if err != nil {
		return Pinned{}, err
	}
	interpResolved, interpDigest, err := hashPinned("interpreter", interpreter, true)
	if err != nil {
		return Pinned{}, err
	}
	manifest := strings.NewReplacer(serverPath, resolved, serverSHA256, digest,
		interpreterPath, interpResolved, interpreterSHA256, interpDigest).Replace(tmpl)
	m, err := plugin.Decode([]byte(manifest))
	if err != nil {
		return Pinned{}, errors.New("the pinned manifest is not a valid plugin manifest")
	}
	if m.Server.Interpreter == nil || m.Server.Interpreter.Path != interpResolved || m.Server.Executable.Path != resolved {
		return Pinned{}, errors.New("the manifest template must pin the interpreter as server.interpreter")
	}
	return replaceManifest(root, dirAbs, []byte(manifest))
}

// errInterpreterNeeded refuses Pin for a template that pins an interpreter.
var errInterpreterNeeded = errors.New("the manifest template pins an interpreter; give its path with --interpreter")

func hasInterpreter(tmpl string) bool {
	return strings.Contains(tmpl, interpreterPath) || strings.Contains(tmpl, interpreterSHA256)
}

// pinsInterpreter reports whether dir's manifest template, read before the
// server is hashed, pins an interpreter, so a script without an exec bit is
// told to use --interpreter. Any failure to read it reports false, which
// leaves Pin's own checks, in their order, to refuse it.
func pinsInterpreter(dir string) bool {
	_, root, err := openOwnedDir(dir)
	if err != nil {
		return false
	}
	defer root.Close()
	tmpl, err := readTemplate(root)
	return err == nil && hasInterpreter(tmpl)
}

// checkServerPath refuses a server path that cannot be written verbatim into a
// JSON string or a manifest: a quote, a backslash, a control character,
// invalid UTF-8 or a placeholder.
func checkServerPath(path string) error {
	return checkPathText("server", path)
}

// checkPathText is checkServerPath for the file what names (server or
// interpreter).
func checkPathText(what, path string) error {
	switch {
	case path == "":
		return errors.New("the " + what + " path is empty")
	case strings.ContainsAny(path, `"\`):
		return errors.New(`the ` + what + ` path must not contain " or \`)
	case !utf8.ValidString(path) || strings.IndexFunc(path, unicode.IsControl) >= 0:
		return errors.New("the " + what + " path must be valid text without control characters")
	case strings.Contains(path, "{{"):
		return errors.New("the " + what + " path must not contain {{")
	}
	return nil
}

// hashServer resolves the server to an absolute path with every symlink in its
// directories resolved, refuses a server path that is itself a symlink, and
// hashes the file through a handle proven to be the file that was checked.
func hashServer(server string) (resolved, digest string, err error) {
	return hashPinned("server", server, true)
}

// hashPinned is hashServer for the file what names (server or interpreter);
// needExec requires the owner exec bit.
func hashPinned(what, path string, needExec bool) (resolved, digest string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", errors.New("the " + what + " path cannot be resolved")
	}
	info, err := os.Lstat(abs)
	switch {
	case err != nil:
		return "", "", errors.New("the " + what + " is missing or unreadable")
	case info.Mode()&fs.ModeSymlink != 0:
		return "", "", errors.New("the " + what + " is a symlink; pin the file it points to")
	case !info.Mode().IsRegular():
		return "", "", errors.New("the " + what + " must be a regular file")
	case needExec && info.Mode().Perm()&0o100 == 0:
		return "", "", errors.New("the " + what + " is not executable by its owner")
	}
	if resolved, err = filepath.EvalSymlinks(abs); err != nil || !filepath.IsAbs(resolved) {
		return "", "", errors.New("the " + what + " path cannot be resolved")
	}
	if err := checkPathText(what, resolved); err != nil {
		return "", "", errors.New("the resolved " + what + " path must not contain \", \\, {{ or control characters")
	}
	if err := checkPlacementOf(what, resolved); err != nil {
		return "", "", err
	}
	f, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", "", errors.New("the " + what + " is missing or unreadable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm() != info.Mode().Perm() {
		return "", "", errors.New("the " + what + " changed while it was opened")
	}
	if opened.Size() > pinned.MaxFileBytes {
		return "", "", errors.New("the " + what + " exceeds the pinned file bound")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, pinned.MaxFileBytes+1))
	if err != nil {
		return "", "", errors.New("the " + what + " is unreadable")
	}
	if n > pinned.MaxFileBytes {
		return "", "", errors.New("the " + what + " exceeds the pinned file bound")
	}
	return resolved, hex.EncodeToString(h.Sum(nil)), nil
}

// checkPlacement applies the engine's placement rules (pinned.CheckPlacement)
// to the resolved server, so a server the engine would refuse to start is
// refused at pin time. Its error names paths, so it is mapped to a fixed
// reason: the file's own mode and links are told apart, and anything else is
// an owner or an ancestor directory the rules do not trust.
func checkPlacement(resolved string) error {
	return checkPlacementOf("server", resolved)
}

// interpreterPlacementHint follows the interpreter's owner-or-ancestor refusal.
// Package-manager prefixes such as Homebrew's are group-writable, so it names
// what passes instead. It is fixed text and names no local path.
const interpreterPlacementHint = "; package-manager prefixes such as Homebrew's are group-writable, so use the system interpreter, an interpreter installed under your home directory (pyenv, nvm or fnm), or remove group write from the directories above the interpreter"

// checkPlacementOf is checkPlacement for the file what names (server or
// interpreter). The interpreter launch.go runs is checked by the same
// pinned.CheckPlacement, which trusts files and directories root owns. Only the
// interpreter's owner-or-ancestor refusal adds interpreterPlacementHint.
func checkPlacementOf(what, resolved string) error {
	if pinned.CheckPlacement(resolved) == nil {
		return nil
	}
	info, err := os.Lstat(resolved)
	switch {
	case err != nil:
		return errors.New("the " + what + " is missing or unreadable")
	case info.Mode().Perm()&0o022 != 0:
		return errors.New("the " + what + " is group- or world-writable; make it writable by its owner only")
	}
	if n, ok := pinned.LinkCount(info); ok && n > 1 {
		return errors.New("the " + what + " has more than one hard link; copy it to a private location and pin that copy")
	}
	reason := "the " + what + " or a directory above it is owned by another user, or a directory above it is writable by others without the sticky bit"
	if what == "interpreter" {
		reason += interpreterPlacementHint
	}
	return errors.New(reason)
}

// openOwnedDir opens dir as an os.Root after proving it is a directory the
// user owns and not a symlink, and returns its symlink-resolved absolute path.
func openOwnedDir(dir string) (string, *os.Root, error) {
	if dir == "" {
		return "", nil, errors.New("the directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, errors.New("the directory cannot be resolved")
	}
	info, err := os.Lstat(abs)
	switch {
	case err != nil:
		return "", nil, errors.New("the directory is missing or unreadable")
	case info.Mode()&fs.ModeSymlink != 0:
		return "", nil, errors.New("the directory is a symlink")
	case !info.IsDir() || !ownedByMe(info):
		return "", nil, errors.New("the directory must be a directory you own")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, errors.New("the directory cannot be resolved")
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return "", nil, errors.New("the directory cannot be opened")
	}
	if got, err := root.Stat("."); err != nil || !os.SameFile(info, got) {
		root.Close()
		return "", nil, errors.New("the directory changed while it was opened")
	}
	return resolved, root, nil
}

// readTemplate reads plugin.json.tmpl, a regular file that is not a symlink,
// within the manifest bound.
func readTemplate(root *os.Root) (string, error) {
	info, err := root.Lstat(TemplateFile)
	if err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("the directory has no plugin.json.tmpl regular file")
	}
	f, err := root.OpenFile(TemplateFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("plugin.json.tmpl is unreadable")
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil || !os.SameFile(info, opened) {
		return "", errors.New("plugin.json.tmpl changed while it was opened")
	}
	raw, err := io.ReadAll(io.LimitReader(f, extension.MaxManifestBytes+1))
	if err != nil {
		return "", errors.New("plugin.json.tmpl is unreadable")
	}
	if len(raw) > extension.MaxManifestBytes {
		return "", errors.New("plugin.json.tmpl exceeds the manifest bound")
	}
	return string(raw), nil
}

// afterTempWrite runs between writing the temporary manifest and reading it
// back; a seam for tests that swap the file there. nil in normal use.
var afterTempWrite func(root *os.Root, tmp string)

// replaceManifest writes the manifest to a fresh temporary file in dir, reads
// it back through the root handle, hashes and decodes those bytes, and only
// then renames it over plugin.json. The file read back must be the one this
// call wrote, hold exactly the manifest and pass the engine's placement rules,
// so the digest returned describes the file renamed. A failure removes the
// temporary file and leaves plugin.json as it was.
func replaceManifest(root *os.Root, dirAbs string, manifest []byte) (Pinned, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return Pinned{}, errors.New("no random name for the temporary manifest")
	}
	tmp := ".plugin.json." + hex.EncodeToString(suffix[:]) + ".tmp"
	made, written, err := writeExclusiveStat(root, tmp, manifest)
	done := false
	defer func() {
		if made && !done {
			root.Remove(tmp)
		}
	}()
	if err != nil {
		return Pinned{}, errors.New("writing the manifest failed")
	}
	if afterTempWrite != nil {
		afterTempWrite(root, tmp)
	}
	f, err := openWritten(root, tmp, written)
	if err != nil {
		return Pinned{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, extension.MaxManifestBytes+1))
	switch {
	case err != nil:
		return Pinned{}, errors.New("the pinned manifest does not load: " + string(mcp.NotInstalled))
	case len(raw) > extension.MaxManifestBytes:
		return Pinned{}, errors.New("the pinned manifest exceeds the manifest bound")
	case !bytes.Equal(raw, manifest):
		return Pinned{}, errors.New("the temporary manifest changed before it was read back")
	}
	if pinned.CheckPlacement(filepath.Join(dirAbs, tmp)) != nil {
		return Pinned{}, errors.New("the pinned manifest does not load: " + string(mcp.NotInstalled))
	}
	sum := sha256.Sum256(raw)
	m, err := plugin.Decode(raw)
	if err != nil {
		return Pinned{}, errors.New("the pinned manifest does not load: " + string(mcp.SchemaInvalid))
	}
	if err := root.Rename(tmp, ManifestFile); err != nil {
		return Pinned{}, errors.New("replacing plugin.json failed")
	}
	done = true
	if now, err := root.Lstat(ManifestFile); err != nil || !os.SameFile(now, written) {
		return Pinned{}, errors.New("plugin.json changed while it was replaced")
	}
	return Pinned{Name: m.Name, ManifestSHA256: hex.EncodeToString(sum[:])}, nil
}

// openWritten opens the temporary manifest through the root handle without
// following a symlink and proves it is still the file written: the same file,
// a regular file of one hard link that only its owner, the user, can write.
func openWritten(root *os.Root, tmp string, written fs.FileInfo) (*os.File, error) {
	changed := errors.New("the temporary manifest changed before it was read back")
	info, err := root.Lstat(tmp)
	if err != nil || info.Mode()&fs.ModeSymlink != 0 || !os.SameFile(info, written) {
		return nil, changed
	}
	f, err := root.OpenFile(tmp, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, changed
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(opened, written) || !opened.Mode().IsRegular() || !ownedByMe(opened) {
		f.Close()
		return nil, changed
	}
	if opened.Mode().Perm()&0o022 != 0 {
		f.Close()
		return nil, errors.New("the temporary manifest is group- or world-writable")
	}
	if n, ok := pinned.LinkCount(opened); ok && n > 1 {
		f.Close()
		return nil, errors.New("the temporary manifest has more than one hard link")
	}
	if opened.Size() > extension.MaxManifestBytes {
		f.Close()
		return nil, errors.New("the pinned manifest exceeds the manifest bound")
	}
	return f, nil
}
