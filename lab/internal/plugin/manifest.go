// Package plugin owns the plugin manifest (the extension manifest with the kinds
// tracker, forge and reviewer) and the scrubbed, digest-verified launch of a
// plugin's MCP server.
package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/extension"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// Bounds and fixed values of a plugin manifest.
const (
	maxTools          = 32
	maxEnv            = 16
	maxNetwork        = 16
	minPollSeconds    = 60
	maxPollSeconds    = 3600
	maxLimitations    = 2000
	schemaVersion     = "v1"
	accessReadOnly    = "read-only"
	accessReadWrite   = "read-write"
	extensionCacheDir = "extensions"
)

var (
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	hostName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	hex64    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// reservedEnv are names the launch sets itself or that would change how the
// plugin's executable or interpreter is loaded.
var reservedEnv = []string{"PATH", "HOME", "LANG"}

// Needs is what a plugin declares it needs from its environment. Env names are
// passed through when set; Network is a reviewed claim Forgecell does not yet
// enforce; MinPollSeconds is the slowest cadence the sync engine may poll at.
type Needs struct {
	Env            []string `json:"env"`
	Network        []string `json:"network"`
	MinPollSeconds int      `json:"minPollSeconds"`
}

// Manifest is a plugin manifest: the extension manifest shape (server,
// tools, roles, dependencies, limitations) with a plugin kind, the contract it
// serves and its needs.
type Manifest struct {
	SchemaVersion string                 `json:"schemaVersion"`
	Kind          string                 `json:"kind"`
	Name          string                 `json:"name"`
	Contract      string                 `json:"contract"`
	Server        extension.Command      `json:"server"`
	Tools         []extension.Tool       `json:"tools"`
	Roles         []string               `json:"roles"`
	Needs         Needs                  `json:"needs"`
	Dependencies  []extension.Dependency `json:"dependencies"`
	Limitations   string                 `json:"limitations"`
}

// Decode is strict like extension.Decode: bounded size, one JSON value, no
// duplicate or case-variant keys, no unknown fields, and every rule of a plugin
// manifest. kind mcp-provider is refused here; extension.Decode owns it.
func Decode(raw []byte) (Manifest, error) {
	if len(raw) > extension.MaxManifestBytes {
		return Manifest{}, fmt.Errorf("plugin manifest exceeds %d bytes", extension.MaxManifestBytes)
	}
	if err := pinned.RejectDuplicateKeys(raw); err != nil {
		var dup *pinned.DuplicateKeyError
		if errors.As(err, &dup) {
			return Manifest{}, fmt.Errorf("plugin manifest has a duplicate key %q", dup.Key)
		}
		return Manifest{}, fmt.Errorf("invalid plugin manifest: %w", err)
	}
	if err := extension.RequireExactKeys(raw, reflect.TypeOf(Manifest{})); err != nil {
		return Manifest{}, fmt.Errorf("invalid plugin manifest: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("invalid plugin manifest: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Manifest{}, errors.New("plugin manifest must be one JSON object with no trailing data")
	}
	if err := m.validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) validate() error {
	c, ok := contract.Lookup(m.Kind)
	if m.SchemaVersion != schemaVersion || !ok {
		return fmt.Errorf("plugin manifest must be schemaVersion %s with kind tracker, forge or reviewer", schemaVersion)
	}
	if m.Contract != c.ID {
		return fmt.Errorf("a %s plugin must declare contract %s", m.Kind, c.ID)
	}
	if !extension.ValidName(m.Name) {
		return fmt.Errorf("plugin name %q must be lowercase letters, digits and dashes, starting with a letter, at most 32 characters", m.Name)
	}
	if err := m.Server.Validate("server"); err != nil {
		return err
	}
	if err := m.validateTools(c); err != nil {
		return err
	}
	roles := map[string]bool{}
	for _, r := range m.Roles {
		if r != "analysis" && r != "coding" {
			return fmt.Errorf("role %q must be analysis or coding", r)
		}
		if roles[r] {
			return fmt.Errorf("role %q is listed twice", r)
		}
		roles[r] = true
	}
	if err := m.Needs.validate(); err != nil {
		return err
	}
	names := map[string]string{}
	for _, d := range m.Dependencies {
		if err := extension.ValidatePin("dependency", d.File); err != nil {
			return err
		}
		if d.State != "pinned" {
			return fmt.Errorf("dependency %s must have state pinned, not %q", filepath.Base(d.Path), d.State)
		}
		base := filepath.Base(d.Path)
		if _, repeated := names[base]; repeated {
			return fmt.Errorf("two dependencies share the file name %q", base)
		}
		names[base] = d.Path
	}
	if len([]rune(m.Limitations)) > maxLimitations {
		return fmt.Errorf("limitations exceed %d characters", maxLimitations)
	}
	if err := extension.ValidateArgs(m.Server.Args, m.pinnedFiles()); err != nil {
		return err
	}
	for _, arg := range m.Server.Args {
		if strings.Contains(arg, "{{") {
			return fmt.Errorf("argument %q uses a placeholder; a plugin has no prepared cache or source to substitute", arg)
		}
	}
	return nil
}

// validateTools requires every required contract tool, each with the access its
// contract level maps to: read is read-only and write is read-write. Other
// tools may be listed for harness attachment.
func (m Manifest) validateTools(c *contract.Contract) error {
	if len(m.Tools) > maxTools {
		return fmt.Errorf("plugin declares more than %d tools", maxTools)
	}
	listed := map[string]string{}
	for _, t := range m.Tools {
		if !extension.ValidToolName(t.Name) {
			return fmt.Errorf("tool name %q is invalid", t.Name)
		}
		if _, twice := listed[t.Name]; twice {
			return fmt.Errorf("tool %q is declared twice", t.Name)
		}
		if t.Access != accessReadOnly && t.Access != accessReadWrite {
			return fmt.Errorf("tool %q access %q must be %s or %s", t.Name, t.Access, accessReadOnly, accessReadWrite)
		}
		listed[t.Name] = t.Access
	}
	for _, t := range c.Tools {
		access, ok := listed[t.Name]
		if !ok {
			if t.Required {
				return fmt.Errorf("plugin does not list the contract tool %s", t.Name)
			}
			continue
		}
		if want := AccessFor(t.Access); access != want {
			return fmt.Errorf("contract tool %s must have access %s", t.Name, want)
		}
	}
	return nil
}

// AccessFor maps a contract access level to the manifest access value.
func AccessFor(a contract.Access) string {
	if a == contract.Write {
		return accessReadWrite
	}
	return accessReadOnly
}

func (n Needs) validate() error {
	if len(n.Env) > maxEnv || len(n.Network) > maxNetwork {
		return fmt.Errorf("needs lists more than %d env names or %d network hosts", maxEnv, maxNetwork)
	}
	seen := map[string]bool{}
	for _, name := range n.Env {
		if !envName.MatchString(name) {
			return fmt.Errorf("needs.env name %q is not a valid environment variable name", name)
		}
		for _, reserved := range reservedEnv {
			if name == reserved {
				return fmt.Errorf("needs.env must not name %s; Forgecell sets it", name)
			}
		}
		if strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
			return fmt.Errorf("needs.env must not name the loader variable %s", name)
		}
		if seen[name] {
			return fmt.Errorf("needs.env lists %s twice", name)
		}
		seen[name] = true
	}
	hosts := map[string]bool{}
	for _, host := range n.Network {
		if len(host) > 253 || !hostName.MatchString(host) {
			return fmt.Errorf("needs.network host %q must be a lowercase hostname", host)
		}
		if hosts[host] {
			return fmt.Errorf("needs.network lists %s twice", host)
		}
		hosts[host] = true
	}
	if n.MinPollSeconds < minPollSeconds || n.MinPollSeconds > maxPollSeconds {
		return fmt.Errorf("needs.minPollSeconds must be between %d and %d", minPollSeconds, maxPollSeconds)
	}
	return nil
}

// labeledPin is a pinned file with the role it plays, for verification.
type labeledPin struct {
	role string
	file pinned.File
}

func (m Manifest) pins() []labeledPin {
	pins := []labeledPin{{"server", m.Server.Executable}}
	if m.Server.Interpreter != nil {
		pins = append(pins, labeledPin{"server interpreter", *m.Server.Interpreter})
	}
	for _, d := range m.Dependencies {
		pins = append(pins, labeledPin{"dependency", d.File})
	}
	return pins
}

func (m Manifest) pinnedFiles() []pinned.File {
	var files []pinned.File
	for _, p := range m.pins() {
		files = append(files, p.file)
	}
	return files
}

// Verify hashes every pinned file as it is now. A changed digest is
// ManifestChanged and a missing or unsafely placed file is NotInstalled. The
// error never names a local path.
func (m Manifest) Verify() ([]pinned.Identity, error) {
	identities := make([]pinned.Identity, 0, len(m.pins()))
	for _, p := range m.pins() {
		id, finding := pinned.Verify(p.role, p.file, "unavailable")
		if strings.HasPrefix(finding, "changed:") {
			return nil, mcp.Fail(mcp.ManifestChanged, "", p.role+" changed since its digest was pinned")
		}
		if finding != "" {
			return nil, mcp.Fail(mcp.NotInstalled, "", p.role+" is missing, unreadable or unsafely placed")
		}
		identities = append(identities, id)
	}
	return identities, nil
}

// Load reads a Lab-relative manifest whose bytes must match the pinned digest,
// decodes it and verifies every pinned file, with extension.Load's path rules:
// a clean relative path inside the Lab, never through a symlink and never
// inside the extension cache.
func Load(labDir, rel, sha256Hex string) (Manifest, []pinned.Identity, error) {
	if !filepath.IsAbs(labDir) || filepath.Clean(labDir) != labDir {
		return Manifest{}, nil, errors.New("the Lab directory must be a clean absolute path")
	}
	local := filepath.FromSlash(rel)
	if !pinned.CleanRelative(rel) || !filepath.IsLocal(local) {
		return Manifest{}, nil, fmt.Errorf("plugin manifest path %q must be a clean relative path inside the Lab directory", rel)
	}
	if parts := strings.Split(rel, "/"); len(parts) >= 3 && strings.EqualFold(parts[0], extensionCacheDir) && hex64.MatchString(strings.ToLower(parts[2])) {
		return Manifest{}, nil, fmt.Errorf("plugin manifest path %q is inside the extension cache, which only Forgecell writes", rel)
	}
	lab, err := filepath.EvalSymlinks(labDir)
	if err != nil || !filepath.IsAbs(lab) {
		return Manifest{}, nil, mcp.Fail(mcp.NotInstalled, "", "the Lab directory cannot be resolved")
	}
	at := lab
	for _, part := range strings.Split(local, string(filepath.Separator)) {
		at = filepath.Join(at, part)
		info, statErr := os.Lstat(at)
		if statErr != nil {
			break
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return Manifest{}, nil, fmt.Errorf("plugin manifest %s passes through a symlink", rel)
		}
	}
	m, digest, err := read(filepath.Join(lab, local))
	if err != nil {
		return Manifest{}, nil, err
	}
	if digest != sha256Hex {
		return Manifest{}, nil, mcp.Fail(mcp.ManifestChanged, "", "the manifest bytes differ from the pinned digest")
	}
	identities, err := m.Verify()
	if err != nil {
		return Manifest{}, nil, err
	}
	return m, identities, nil
}

// LoadFile reads and decodes a manifest at an absolute path, as plugin test
// does before anyone has pinned it, and returns the SHA-256 of its bytes. The
// path itself must not be a symlink.
func LoadFile(path string) (Manifest, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Manifest{}, "", errors.New("the manifest path must be a clean absolute path")
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return Manifest{}, "", mcp.Fail(mcp.NotInstalled, "", "the manifest path is a symlink")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Manifest{}, "", mcp.Fail(mcp.NotInstalled, "", "the manifest is missing or unreadable")
	}
	return read(resolved)
}

// read opens a placement-checked manifest, bounds it and decodes it. A decode
// refusal is SchemaInvalid, with its reason kept as diagnostics.
func read(path string) (Manifest, string, error) {
	file, err := pinned.Open(path)
	if err != nil {
		return Manifest{}, "", mcp.Fail(mcp.NotInstalled, "", "the manifest is missing, unreadable or unsafely placed")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, extension.MaxManifestBytes+1))
	if err != nil {
		return Manifest{}, "", mcp.Fail(mcp.NotInstalled, "", "the manifest is unreadable")
	}
	sum := sha256.Sum256(raw)
	m, err := Decode(raw)
	if err != nil {
		return Manifest{}, "", mcp.Fail(mcp.SchemaInvalid, "", err.Error())
	}
	return m, hex.EncodeToString(sum[:]), nil
}
