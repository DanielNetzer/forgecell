package scaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
)

// pinFixture writes a skeleton and a stand-in server file. Pin only hashes the
// server, so it needs no build.
func pinFixture(t *testing.T) (base, dir, server string) {
	t.Helper()
	base = tempDir(t)
	dir = filepath.Join(base, "skel")
	if err := Write(dir, Options{Kind: "forge", Lang: "go", Name: "pin-me"}); err != nil {
		t.Fatal(err)
	}
	server = filepath.Join(base, "server")
	if err := os.WriteFile(server, []byte("#!/bin/false\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return base, dir, server
}

func sha(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestPinWritesALoadableManifest(t *testing.T) {
	base, dir, server := pinFixture(t)
	// The server is named through a symlinked directory; the manifest records
	// the resolved path.
	if err := os.Symlink(base, filepath.Join(base, "via")); err != nil {
		t.Fatal(err)
	}
	p, err := Pin(dir, filepath.Join(base, "via", "server"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ManifestFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != fileMode {
		t.Fatalf("plugin.json mode %v, %v", info.Mode(), err)
	}
	raw, _ := os.ReadFile(path)
	m, digest, err := plugin.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	serverBytes, _ := os.ReadFile(server)
	if p.Name != "pin-me" || p.ManifestSHA256 != digest || digest != sha(raw) ||
		m.Server.Executable.Path != server || m.Server.Executable.SHA256 != sha(serverBytes) {
		t.Fatalf("pinned %+v, manifest server %+v", p, m.Server.Executable)
	}
	if strings.Contains(string(raw), "{{") {
		t.Fatal("a placeholder is left in plugin.json")
	}

	// Pinning again after a rebuild replaces plugin.json and leaves no
	// temporary file.
	if err := os.WriteFile(server, []byte("#!/bin/true\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	again, err := Pin(dir, server)
	if err != nil || again.ManifestSHA256 == p.ManifestSHA256 {
		t.Fatalf("re-pin %+v, %v", again, err)
	}
	want := []string{".", "README.md", "fixture.json", "go.mod", "main.go", ManifestFile, TemplateFile, "tools.json"}
	if got := tree(t, dir); !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
}

func TestPinRefusals(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, base, dir, server string) (string, string)
		want  string
	}{
		{"placeholder left over", func(t *testing.T, base, dir, server string) (string, string) {
			editTemplate(t, dir, func(s string) string { return strings.Replace(s, `"roles": []`, `"roles": ["{{ROLE}}"]`, 1) })
			return dir, server
		}, "placeholder left over"},
		{"placeholder missing", func(t *testing.T, base, dir, server string) (string, string) {
			editTemplate(t, dir, func(s string) string { return strings.Replace(s, serverSHA256, strings.Repeat("b", 64), 1) })
			return dir, server
		}, "exactly once"},
		{"invalid manifest", func(t *testing.T, base, dir, server string) (string, string) {
			editTemplate(t, dir, func(s string) string { return strings.Replace(s, `"minPollSeconds": 60`, `"minPollSeconds": 1`, 1) })
			return dir, server
		}, "not a valid plugin manifest"},
		{"quote in server path", func(t *testing.T, base, dir, server string) (string, string) {
			return dir, writeServer(t, base, `ser"ver`)
		}, `must not contain "`},
		{"backslash in server path", func(t *testing.T, base, dir, server string) (string, string) {
			return dir, writeServer(t, base, `ser\ver`)
		}, `must not contain "`},
		{"control character in server path", func(t *testing.T, base, dir, server string) (string, string) {
			return dir, writeServer(t, base, "ser\nver")
		}, "control characters"},
		{"symlinked server", func(t *testing.T, base, dir, server string) (string, string) {
			link := filepath.Join(base, "server-link")
			if err := os.Symlink(server, link); err != nil {
				t.Fatal(err)
			}
			return dir, link
		}, "symlink"},
		{"directory server", func(t *testing.T, base, dir, server string) (string, string) {
			sub := filepath.Join(base, "sub")
			mkdir(t, sub)
			return dir, sub
		}, "regular file"},
		{"missing server", func(t *testing.T, base, dir, server string) (string, string) {
			return dir, filepath.Join(base, "absent")
		}, "missing"},
		{"symlinked dir", func(t *testing.T, base, dir, server string) (string, string) {
			link := filepath.Join(base, "skel-link")
			if err := os.Symlink(dir, link); err != nil {
				t.Fatal(err)
			}
			return link, server
		}, "symlink"},
		{"missing template", func(t *testing.T, base, dir, server string) (string, string) {
			empty := filepath.Join(base, "empty")
			mkdir(t, empty)
			return empty, server
		}, "no plugin.json.tmpl"},
		{"symlinked template", func(t *testing.T, base, dir, server string) (string, string) {
			other := filepath.Join(base, "other")
			mkdir(t, other)
			if err := os.Symlink(filepath.Join(dir, TemplateFile), filepath.Join(other, TemplateFile)); err != nil {
				t.Fatal(err)
			}
			return other, server
		}, "no plugin.json.tmpl"},
		{"group-writable server", func(t *testing.T, base, dir, server string) (string, string) {
			chmod(t, server, 0o720)
			return dir, server
		}, "group- or world-writable"},
		{"world-writable server", func(t *testing.T, base, dir, server string) (string, string) {
			chmod(t, server, 0o702)
			return dir, server
		}, "group- or world-writable"},
		{"no owner exec bit", func(t *testing.T, base, dir, server string) (string, string) {
			chmod(t, server, 0o600)
			return dir, server
		}, "not executable"},
		{"hard-linked server", func(t *testing.T, base, dir, server string) (string, string) {
			if err := os.Link(server, filepath.Join(base, "server-alias")); err != nil {
				t.Fatal(err)
			}
			return dir, server
		}, "more than one hard link"},
		{"writable non-sticky ancestor", func(t *testing.T, base, dir, server string) (string, string) {
			open := filepath.Join(base, "open")
			mkdir(t, open)
			chmod(t, open, 0o777)
			return dir, writeServer(t, open, "server")
		}, "writable by others without the sticky bit"},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, dir, server := pinFixture(t)
			target, serverArg := c.setup(t, base, dir, server)
			before := tree(t, base)
			_, err := Pin(target, serverArg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), base) {
				t.Fatalf("the error names a local path: %v", err)
			}
			if after := tree(t, base); !slices.Equal(before, after) {
				t.Fatalf("the refusal changed the files: %v, was %v", after, before)
			}
		})
	}
}

func TestPinRefusalKeepsTheEarlierManifest(t *testing.T) {
	_, dir, server := pinFixture(t)
	if _, err := Pin(dir, server); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ManifestFile)
	before, _ := os.ReadFile(path)
	editTemplate(t, dir, func(s string) string { return s + "{{LEFT}}" })
	if _, err := Pin(dir, server); err == nil {
		t.Fatal("a template with a placeholder left over was pinned")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refused pin changed plugin.json")
	}
}

// TestPinAcceptsARootOwnedServer pins a system binary that root owns in a
// root-owned directory, which the engine's placement rules trust.
func TestPinAcceptsARootOwnedServer(t *testing.T) {
	server := ""
	for _, candidate := range []string{"/bin/sh", "/usr/bin/true", "/bin/ls", "/usr/bin/env"} {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		info, err := os.Lstat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 || os.Geteuid() == 0 {
			continue
		}
		if pinned.CheckPlacement(resolved) == nil {
			server = resolved
			break
		}
	}
	if server == "" {
		t.Skip("no root-owned system binary passes the placement rules here")
	}
	_, dir, _ := pinFixture(t)
	if _, err := Pin(dir, server); err != nil {
		t.Fatalf("a root-owned server was refused: %v", err)
	}
	m, _, err := plugin.LoadFile(filepath.Join(dir, ManifestFile))
	if err != nil || m.Server.Executable.Path != server {
		t.Fatalf("manifest server %+v, %v", m, err)
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func editTemplate(t *testing.T, dir string, edit func(string) string) {
	t.Helper()
	path := filepath.Join(dir, TemplateFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(edit(string(raw))), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeServer(t *testing.T, base, name string) string {
	t.Helper()
	path := filepath.Join(base, name)
	if err := os.WriteFile(path, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
