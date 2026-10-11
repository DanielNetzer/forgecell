// Package extension owns the mcp-provider/v1 manifest: a content-addressed
// description of an out-of-process MCP server that a role may be given.
package extension

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
)

const (
	// MaxManifestBytes bounds a manifest, both when decoded and when loaded.
	MaxManifestBytes = 64 << 10

	maxTools         = 32
	maxPrepareTimeMS = 1_800_000

	schemaVersion = "v1"
	kind          = "mcp-provider"

	roleAnalysis = "analysis"
	roleCoding   = "coding"

	accessReadOnly  = "read-only"
	accessReadWrite = "read-write"

	placeholderCache  = "{{CACHE}}"
	placeholderSource = "{{SOURCE}}"
)

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	toolPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	hex64       = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Tool is one MCP tool the provider exposes. Access is "read-only" or "read-write".
type Tool struct {
	Name   string `json:"name"`
	Access string `json:"access"`
}

// Command is a pinned executable, an optional pinned interpreter and its argv.
type Command struct {
	Executable  pinned.File  `json:"executable"`
	Interpreter *pinned.File `json:"interpreter,omitempty"`
	Args        []string     `json:"args"`
}

// Prepare builds the provider's cache before a role runs. Network is a declared
// claim, "none" or "declared"; Forgecell does not enforce it.
type Prepare struct {
	Command
	TimeoutMS int    `json:"timeoutMs"`
	Network   string `json:"network"`
}

// Dependency is a pinned file the provider needs. State must be "pinned".
type Dependency struct {
	pinned.File
	Role  string `json:"role"`
	State string `json:"state"`
}

// Manifest is the mcp-provider/v1 contract. Roles apply to the whole extension.
type Manifest struct {
	SchemaVersion string       `json:"schemaVersion"`
	Kind          string       `json:"kind"`
	Name          string       `json:"name"`
	Server        Command      `json:"server"`
	Prepare       *Prepare     `json:"prepare,omitempty"`
	Tools         []Tool       `json:"tools"`
	Roles         []string     `json:"roles"`
	Dependencies  []Dependency `json:"dependencies"`
	Limitations   string       `json:"limitations"`
}

// Decode is strict: bounded size, one JSON value, no duplicate keys, no unknown
// fields, and every validation rule of mcp-provider/v1.
func Decode(raw []byte) (Manifest, error) {
	if len(raw) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("extension manifest exceeds %d bytes", MaxManifestBytes)
	}
	if err := pinned.RejectDuplicateKeys(raw); err != nil {
		var dup *pinned.DuplicateKeyError
		if errors.As(err, &dup) {
			return Manifest{}, fmt.Errorf("extension manifest has a duplicate key %q", dup.Key)
		}
		return Manifest{}, fmt.Errorf("invalid extension manifest: %w", err)
	}
	if err := requireExactKeys(raw); err != nil {
		return Manifest{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("invalid extension manifest: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Manifest{}, errors.New("extension manifest must be one JSON object with no trailing data")
	}
	if err := m.validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// requireExactKeys refuses any object key, at any depth, that is not spelled
// exactly like a manifest field. encoding/json matches field names without regard
// to case, so a manifest could otherwise carry {"Kind":"memory","kind":"mcp-provider"}
// or a Kelvin-sign "Kind" and be read differently by another parser.
func requireExactKeys(raw []byte) error {
	var doc any
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&doc); err != nil {
		return fmt.Errorf("invalid extension manifest: %w", err)
	}
	return checkKeys(doc, reflect.TypeOf(Manifest{}))
}

func checkKeys(value any, t reflect.Type) error {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if err := checkKeys(item, t); err != nil {
				return err
			}
		}
	case map[string]any:
		if t.Kind() != reflect.Struct {
			return nil
		}
		fields := jsonFields(t)
		for _, key := range slices.Sorted(maps.Keys(v)) {
			field, exact := fields[key]
			if !exact {
				for name := range fields {
					if strings.EqualFold(name, key) {
						return fmt.Errorf("extension manifest key %q must be spelled exactly %q", key, name)
					}
				}
				return fmt.Errorf("extension manifest has an unknown key %q", key)
			}
			if err := checkKeys(v[key], field); err != nil {
				return err
			}
		}
	}
	return nil
}

// jsonFields maps each JSON key of a struct, including the keys of embedded
// structs, to its Go type.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				maps.Copy(fields, jsonFields(ft))
				continue
			}
		}
		if f.IsExported() {
			if name == "" {
				name = f.Name
			}
			fields[name] = f.Type
		}
	}
	return fields
}

func validPath(p string) bool {
	return p != "" && len(p) <= 4096 && filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsRune(p, 0) && !strings.Contains(p, "{{")
}

func validPin(label string, f pinned.File) error {
	if !validPath(f.Path) || !hex64.MatchString(f.SHA256) {
		return fmt.Errorf("%s needs a clean absolute path without placeholders and a lowercase SHA-256", label)
	}
	return nil
}

func (c Command) validate(label string) error {
	if err := validPin(label+" executable", c.Executable); err != nil {
		return err
	}
	if c.Interpreter != nil {
		if err := validPin(label+" interpreter", *c.Interpreter); err != nil {
			return err
		}
	}
	return nil
}

type labeledFile struct {
	role string
	file pinned.File
}

// pins lists every file the manifest pins, in verification order.
func (m Manifest) pins() []labeledFile {
	files := []labeledFile{{"server", m.Server.Executable}}
	if m.Server.Interpreter != nil {
		files = append(files, labeledFile{"server interpreter", *m.Server.Interpreter})
	}
	if m.Prepare != nil {
		files = append(files, labeledFile{"prepare", m.Prepare.Executable})
		if m.Prepare.Interpreter != nil {
			files = append(files, labeledFile{"prepare interpreter", *m.Prepare.Interpreter})
		}
	}
	for _, d := range m.Dependencies {
		files = append(files, labeledFile{"dependency", d.File})
	}
	return files
}

func (m Manifest) validate() error {
	if m.SchemaVersion != schemaVersion || m.Kind != kind {
		return fmt.Errorf("extension manifest must be schemaVersion %s, kind %s", schemaVersion, kind)
	}
	if !namePattern.MatchString(m.Name) {
		return fmt.Errorf("extension name %q must match %s", m.Name, namePattern)
	}
	if err := m.Server.validate("server"); err != nil {
		return err
	}
	if m.Prepare != nil {
		if err := m.Prepare.Command.validate("prepare"); err != nil {
			return err
		}
		if m.Prepare.TimeoutMS < 1 || m.Prepare.TimeoutMS > maxPrepareTimeMS {
			return fmt.Errorf("prepare timeoutMs must be between 1 and %d", maxPrepareTimeMS)
		}
		if m.Prepare.Network != "none" && m.Prepare.Network != "declared" {
			return fmt.Errorf("prepare network %q must be none or declared", m.Prepare.Network)
		}
	}
	if len(m.Tools) > maxTools {
		return fmt.Errorf("extension declares more than %d tools", maxTools)
	}
	tools := map[string]bool{}
	for _, t := range m.Tools {
		if !toolPattern.MatchString(t.Name) {
			return fmt.Errorf("tool name %q must match %s", t.Name, toolPattern)
		}
		if tools[t.Name] {
			return fmt.Errorf("tool %q is declared twice", t.Name)
		}
		tools[t.Name] = true
		if t.Access != accessReadOnly && t.Access != accessReadWrite {
			return fmt.Errorf("tool %q access %q must be %s or %s", t.Name, t.Access, accessReadOnly, accessReadWrite)
		}
	}
	if len(m.Roles) == 0 {
		return errors.New("extension must list at least one role")
	}
	roles := map[string]bool{}
	for _, r := range m.Roles {
		if r != roleAnalysis && r != roleCoding {
			return fmt.Errorf("role %q must be %s or %s", r, roleAnalysis, roleCoding)
		}
		if roles[r] {
			return fmt.Errorf("role %q is listed twice", r)
		}
		roles[r] = true
	}
	for _, d := range m.Dependencies {
		if err := validPin("dependency", d.File); err != nil {
			return err
		}
		if d.State != "pinned" {
			return fmt.Errorf("dependency %s must have state pinned, not %q", d.Path, d.State)
		}
	}
	return m.validateArgs()
}

func (m Manifest) validateArgs() error {
	argv := [][]string{m.Server.Args}
	if m.Prepare != nil {
		argv = append(argv, m.Prepare.Args)
	}
	pins := m.pins()
	for _, args := range argv {
		for _, arg := range args {
			if strings.ContainsRune(arg, 0) {
				return errors.New("an argument contains a NUL byte")
			}
			if err := checkPlaceholders(arg); err != nil {
				return err
			}
			for _, p := range pins {
				if strings.Contains(arg, p.file.Path) {
					return fmt.Errorf("argument %q contains the pinned path %s", arg, p.file.Path)
				}
			}
		}
	}
	return nil
}

// checkPlaceholders allows {{CACHE}} and {{SOURCE}} only as a whole argument or
// as a path prefix such as {{CACHE}}/graph.json.
func checkPlaceholders(arg string) error {
	rest := arg
	for _, p := range []string{placeholderCache, placeholderSource} {
		if rest == p {
			return nil
		}
		if after, ok := strings.CutPrefix(rest, p+"/"); ok {
			rest = after
			break
		}
	}
	if strings.Contains(rest, "{{") {
		return fmt.Errorf("argument %q uses a placeholder other than %s or %s as a whole argument or path prefix", arg, placeholderCache, placeholderSource)
	}
	return nil
}

// Verify pins every file the manifest names as it is now: the server and prepare
// executables, their interpreters and every dependency. It returns the digests it
// computed, or an error that names the offending path.
func (m Manifest) Verify() ([]pinned.Identity, error) {
	pins := m.pins()
	identities := make([]pinned.Identity, 0, len(pins))
	for _, p := range pins {
		id, finding := pinned.Verify(p.role, p.file, "unavailable")
		if finding != "" {
			return nil, fmt.Errorf("extension %q: %s", m.Name, finding)
		}
		identities = append(identities, id)
	}
	return identities, nil
}

// Load reads the Lab-relative manifest, requires its bytes to match the pinned
// digest, decodes it and verifies every pinned file it names.
func Load(labDir, rel, sha256Hex string) (Manifest, []pinned.Identity, error) {
	if !filepath.IsAbs(labDir) || filepath.Clean(labDir) != labDir {
		return Manifest{}, nil, fmt.Errorf("the Lab directory %q must be a clean absolute path", labDir)
	}
	local := filepath.FromSlash(rel)
	if rel == "" || !filepath.IsLocal(local) || filepath.Clean(local) != local {
		return Manifest{}, nil, fmt.Errorf("extension manifest path %q must be a clean relative path inside the Lab directory", rel)
	}
	file, err := pinned.Open(filepath.Join(labDir, local))
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("extension manifest %s: %w", rel, err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxManifestBytes+1))
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("extension manifest %s is unreadable: %w", rel, err)
	}
	if len(raw) > MaxManifestBytes {
		return Manifest{}, nil, fmt.Errorf("extension manifest %s exceeds %d bytes", rel, MaxManifestBytes)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != sha256Hex {
		return Manifest{}, nil, fmt.Errorf("extension manifest %s changed: SHA-256 %s differs from the pinned %s", rel, got, sha256Hex)
	}
	m, err := Decode(raw)
	if err != nil {
		return Manifest{}, nil, err
	}
	identities, err := m.Verify()
	if err != nil {
		return Manifest{}, nil, err
	}
	return m, identities, nil
}

// ToolsFor returns the tools a role may use, in declared order. A role the
// manifest does not list gets none. Analysis gets only read-only tools, and
// meta or any unknown role gets none.
func (m Manifest) ToolsFor(role string) []Tool {
	tools := []Tool{}
	if role != roleAnalysis && role != roleCoding {
		return tools
	}
	if !slices.Contains(m.Roles, role) {
		return tools
	}
	for _, t := range m.Tools {
		if role == roleAnalysis && t.Access != accessReadOnly {
			continue
		}
		tools = append(tools, t)
	}
	return tools
}
