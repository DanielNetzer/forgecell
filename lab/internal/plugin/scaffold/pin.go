package scaffold

import (
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
// another user owns or can write without the sticky bit. The manifest must load through plugin.LoadFile before it replaces
// anything. Errors never name a local path.
func Pin(dir, server string) (Pinned, error) {
	if err := checkServerPath(server); err != nil {
		return Pinned{}, err
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

// checkServerPath refuses a server path that cannot be written verbatim into a
// JSON string or a manifest: a quote, a backslash, a control character,
// invalid UTF-8 or a placeholder.
func checkServerPath(path string) error {
	switch {
	case path == "":
		return errors.New("the server path is empty")
	case strings.ContainsAny(path, `"\`):
		return errors.New(`the server path must not contain " or \`)
	case !utf8.ValidString(path) || strings.IndexFunc(path, unicode.IsControl) >= 0:
		return errors.New("the server path must be valid text without control characters")
	case strings.Contains(path, "{{"):
		return errors.New("the server path must not contain {{")
	}
	return nil
}

// hashServer resolves the server to an absolute path with every symlink in its
// directories resolved, refuses a server path that is itself a symlink, and
// hashes the file through a handle proven to be the file that was checked.
func hashServer(server string) (resolved, digest string, err error) {
	abs, err := filepath.Abs(server)
	if err != nil {
		return "", "", errors.New("the server path cannot be resolved")
	}
	info, err := os.Lstat(abs)
	switch {
	case err != nil:
		return "", "", errors.New("the server is missing or unreadable")
	case info.Mode()&fs.ModeSymlink != 0:
		return "", "", errors.New("the server is a symlink; pin the file it points to")
	case !info.Mode().IsRegular():
		return "", "", errors.New("the server must be a regular file")
	case info.Mode().Perm()&0o100 == 0:
		return "", "", errors.New("the server is not executable by its owner")
	}
	if resolved, err = filepath.EvalSymlinks(abs); err != nil || !filepath.IsAbs(resolved) {
		return "", "", errors.New("the server path cannot be resolved")
	}
	if err := checkServerPath(resolved); err != nil {
		return "", "", errors.New("the resolved server path must not contain \", \\, {{ or control characters")
	}
	if err := checkPlacement(resolved); err != nil {
		return "", "", err
	}
	f, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", "", errors.New("the server is missing or unreadable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm() != info.Mode().Perm() {
		return "", "", errors.New("the server changed while it was opened")
	}
	if opened.Size() > pinned.MaxFileBytes {
		return "", "", errors.New("the server exceeds the pinned file bound")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, pinned.MaxFileBytes+1))
	if err != nil {
		return "", "", errors.New("the server is unreadable")
	}
	if n > pinned.MaxFileBytes {
		return "", "", errors.New("the server exceeds the pinned file bound")
	}
	return resolved, hex.EncodeToString(h.Sum(nil)), nil
}

// checkPlacement applies the engine's placement rules (pinned.CheckPlacement)
// to the resolved server, so a server the engine would refuse to start is
// refused at pin time. Its error names paths, so it is mapped to a fixed
// reason: the file's own mode and links are told apart, and anything else is
// an owner or an ancestor directory the rules do not trust.
func checkPlacement(resolved string) error {
	if pinned.CheckPlacement(resolved) == nil {
		return nil
	}
	info, err := os.Lstat(resolved)
	switch {
	case err != nil:
		return errors.New("the server is missing or unreadable")
	case info.Mode().Perm()&0o022 != 0:
		return errors.New("the server is group- or world-writable; make it writable by its owner only")
	}
	if n, ok := pinned.LinkCount(info); ok && n > 1 {
		return errors.New("the server has more than one hard link; copy it to a private location and pin that copy")
	}
	return errors.New("the server or a directory above it is owned by another user, or a directory above it is writable by others without the sticky bit")
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

// replaceManifest writes the manifest to a fresh temporary file in dir, loads
// it through plugin.LoadFile and only then renames it over plugin.json. A
// failure removes the temporary file and leaves plugin.json as it was.
func replaceManifest(root *os.Root, dirAbs string, manifest []byte) (Pinned, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return Pinned{}, errors.New("no random name for the temporary manifest")
	}
	tmp := ".plugin.json." + hex.EncodeToString(suffix[:]) + ".tmp"
	made, err := writeExclusive(root, tmp, manifest)
	done := false
	defer func() {
		if made && !done {
			root.Remove(tmp)
		}
	}()
	if err != nil {
		return Pinned{}, errors.New("writing the manifest failed")
	}
	m, digest, err := plugin.LoadFile(filepath.Join(dirAbs, tmp))
	if err != nil {
		return Pinned{}, errors.New("the pinned manifest does not load: " + string(mcp.ReasonOf(err)))
	}
	if err := root.Rename(tmp, ManifestFile); err != nil {
		return Pinned{}, errors.New("replacing plugin.json failed")
	}
	done = true
	return Pinned{Name: m.Name, ManifestSHA256: digest}, nil
}
