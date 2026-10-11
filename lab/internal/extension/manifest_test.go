package extension

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
)

func unixOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("permission and symlink checks are unsupported on Windows")
	}
}

func sum(raw []byte) string {
	s := sha256.Sum256(raw)
	return hex.EncodeToString(s[:])
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixture writes fake executables, interpreters and a dependency under a
// symlink-resolved temporary root, each in its own directory so that a change to
// one directory affects only that file. Nothing is installed or executed.
type fixture struct {
	root  string
	lab   string
	files map[string]string
}

const (
	fileServer        = "server"
	fileServerInterp  = "server-interp"
	filePrepare       = "prepare"
	filePrepareInterp = "prepare-interp"
	fileDependency    = "dependency"
)

var fixtureFiles = []string{fileServer, fileServerInterp, filePrepare, filePrepareInterp, fileDependency}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	unixOnly(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fx := &fixture{root: root, lab: filepath.Join(root, "lab"), files: map[string]string{}}
	if err := os.MkdirAll(fx.lab, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range fixtureFiles {
		mode := os.FileMode(0o700)
		if name == fileDependency {
			mode = 0o600
		}
		fx.files[name] = writeFile(t, filepath.Join(root, name, "file"), "content of "+name, mode)
	}
	return fx
}

func (fx *fixture) pin(t *testing.T, name string) pinned.File {
	t.Helper()
	raw, err := os.ReadFile(fx.files[name])
	if err != nil {
		t.Fatal(err)
	}
	return pinned.File{Path: fx.files[name], SHA256: sum(raw)}
}

func (fx *fixture) manifest(t *testing.T) Manifest {
	t.Helper()
	serverInterp := fx.pin(t, fileServerInterp)
	prepareInterp := fx.pin(t, filePrepareInterp)
	m := Manifest{SchemaVersion: "v1", Kind: "mcp-provider", Name: "graphify"}
	m.Server = Command{Executable: fx.pin(t, fileServer), Interpreter: &serverInterp, Args: []string{"mcp", "--graph", "{{CACHE}}/graph.json"}}
	prepare := Command{Executable: fx.pin(t, filePrepare), Interpreter: &prepareInterp, Args: []string{"build", "{{SOURCE}}", "--out", "{{CACHE}}"}}
	m.Prepare = &Prepare{Command: prepare, TimeoutMS: 600000, Network: "none"}
	m.Tools = []Tool{{Name: "query_graph", Access: "read-only"}, {Name: "annotate", Access: "read-write"}}
	m.Roles = []string{"analysis", "coding"}
	m.Dependencies = []Dependency{{File: fx.pin(t, fileDependency), Role: "library", State: "pinned"}}
	m.Limitations = "AST-only graph; answers are not authoritative."
	return m
}

func marshal(t *testing.T, m Manifest) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mutateJSON(t *testing.T, raw []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func section(m map[string]any, key string) map[string]any {
	return m[key].(map[string]any)
}

func setArgs(key string, args ...string) func(map[string]any) {
	return func(m map[string]any) {
		list := make([]any, len(args))
		for i, a := range args {
			list[i] = a
		}
		section(m, key)["args"] = list
	}
}

func tools(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"name": fmt.Sprintf("tool_%d", i), "access": "read-only"}
	}
	return out
}

func (fx *fixture) writeManifest(t *testing.T, raw []byte) (string, string) {
	t.Helper()
	writeFile(t, filepath.Join(fx.lab, "extensions", "graphify.json"), string(raw), 0o600)
	return "extensions/graphify.json", sum(raw)
}

func TestDecodeAcceptsValidManifest(t *testing.T) {
	fx := newFixture(t)
	want := fx.manifest(t)
	got, err := Decode(marshal(t, want))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the manifest:\n got %+v\nwant %+v", got, want)
	}
	raw := mutateJSON(t, marshal(t, want), func(m map[string]any) {
		delete(m, "prepare")
		section(m, "server")["interpreter"] = nil
		m["tools"] = tools(32)
	})
	got, err = Decode(raw)
	if err != nil {
		t.Fatalf("a manifest with no prepare, no interpreter and 32 tools is valid: %v", err)
	}
	if got.Prepare != nil || got.Server.Interpreter != nil || len(got.Tools) != 32 {
		t.Fatalf("unexpected decode: %+v", got)
	}
}

func TestDecodeAcceptsPlaceholdersAndOtherAbsolutePaths(t *testing.T) {
	fx := newFixture(t)
	good := marshal(t, fx.manifest(t))
	for _, arg := range []string{"{{CACHE}}", "{{SOURCE}}", "{{CACHE}}/graph.json", "{{SOURCE}}/src/main.go", "/var/lib/other/tool", "--flag", "plain {text}"} {
		raw := mutateJSON(t, good, func(m map[string]any) {
			setArgs("server", arg)(m)
			setArgs("prepare", arg, "{{CACHE}}")(m)
		})
		if _, err := Decode(raw); err != nil {
			t.Fatalf("%q: %v", arg, err)
		}
	}
	raw := mutateJSON(t, good, func(m map[string]any) { m["name"] = strings.Repeat("a", 32) })
	if _, err := Decode(raw); err != nil {
		t.Fatalf("a 32-character name is valid: %v", err)
	}
}

func TestDecodeRejectsInvalidManifests(t *testing.T) {
	fx := newFixture(t)
	good := marshal(t, fx.manifest(t))
	execPath := fx.files[fileServer]
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unknown schema version", func(m map[string]any) { m["schemaVersion"] = "v2" }},
		{"empty schema version", func(m map[string]any) { m["schemaVersion"] = "" }},
		{"unknown kind", func(m map[string]any) { m["kind"] = "memory" }},
		{"unknown field", func(m map[string]any) { m["extra"] = true }},
		{"unknown nested field", func(m map[string]any) { section(m, "server")["extra"] = true }},
		{"uppercase name", func(m map[string]any) { m["name"] = "Graphify" }},
		{"digit-leading name", func(m map[string]any) { m["name"] = "1graph" }},
		{"empty name", func(m map[string]any) { m["name"] = "" }},
		{"underscore name", func(m map[string]any) { m["name"] = "a_b" }},
		{"long name", func(m map[string]any) { m["name"] = strings.Repeat("a", 33) }},
		{"bad tool name", func(m map[string]any) { m["tools"] = []any{map[string]any{"name": "a b", "access": "read-only"}} }},
		{"empty tool name", func(m map[string]any) { m["tools"] = []any{map[string]any{"name": "", "access": "read-only"}} }},
		{"long tool name", func(m map[string]any) {
			m["tools"] = []any{map[string]any{"name": strings.Repeat("a", 65), "access": "read-only"}}
		}},
		{"bad access", func(m map[string]any) { m["tools"] = []any{map[string]any{"name": "q", "access": "admin"}} }},
		{"missing access", func(m map[string]any) { m["tools"] = []any{map[string]any{"name": "q"}} }},
		{"duplicate tool", func(m map[string]any) {
			m["tools"] = []any{map[string]any{"name": "q", "access": "read-only"}, map[string]any{"name": "q", "access": "read-write"}}
		}},
		{"too many tools", func(m map[string]any) { m["tools"] = tools(33) }},
		{"unknown role", func(m map[string]any) { m["roles"] = []any{"deploy"} }},
		{"meta role", func(m map[string]any) { m["roles"] = []any{"analysis", "meta"} }},
		{"no roles", func(m map[string]any) { m["roles"] = []any{} }},
		{"missing roles", func(m map[string]any) { delete(m, "roles") }},
		{"duplicate role", func(m map[string]any) { m["roles"] = []any{"coding", "coding"} }},
		{"prepare timeout too long", func(m map[string]any) { section(m, "prepare")["timeoutMs"] = 1800001 }},
		{"prepare timeout zero", func(m map[string]any) { section(m, "prepare")["timeoutMs"] = 0 }},
		{"prepare timeout negative", func(m map[string]any) { section(m, "prepare")["timeoutMs"] = -1 }},
		{"prepare network", func(m map[string]any) { section(m, "prepare")["network"] = "internet" }},
		{"prepare network missing", func(m map[string]any) { delete(section(m, "prepare"), "network") }},
		{"unpinned dependency", func(m map[string]any) {
			m["dependencies"] = []any{map[string]any{"path": "/x", "role": "lib", "state": "unknown"}}
		}},
		{"dependency without state", func(m map[string]any) {
			m["dependencies"] = []any{map[string]any{"path": execPath, "sha256": strings.Repeat("a", 64), "role": "lib"}}
		}},
		{"dependency without digest", func(m map[string]any) {
			m["dependencies"] = []any{map[string]any{"path": "/x", "role": "lib", "state": "pinned"}}
		}},
		{"relative executable", func(m map[string]any) { section(section(m, "server"), "executable")["path"] = "bin/server" }},
		{"unclean executable", func(m map[string]any) { section(section(m, "server"), "executable")["path"] = "/a/../b" }},
		{"uppercase digest", func(m map[string]any) {
			section(section(m, "server"), "executable")["sha256"] = strings.Repeat("A", 64)
		}},
		{"short digest", func(m map[string]any) { section(section(m, "prepare"), "interpreter")["sha256"] = "abc" }},
		{"missing executable", func(m map[string]any) { delete(section(m, "server"), "executable") }},
		{"placeholder in a path", func(m map[string]any) { section(section(m, "server"), "executable")["path"] = "/{{CACHE}}/bin" }},
		{"unknown placeholder", func(m map[string]any) { setArgs("server", "{{HOME}}")(m) }},
		{"unknown placeholder in prepare", func(m map[string]any) { setArgs("prepare", "build", "{{HOME}}")(m) }},
		{"mid-argument placeholder", func(m map[string]any) { setArgs("server", "--out={{CACHE}}")(m) }},
		{"trailing placeholder text", func(m map[string]any) { setArgs("server", "{{CACHE}}x")(m) }},
		{"two placeholders", func(m map[string]any) { setArgs("server", "{{CACHE}}/{{SOURCE}}")(m) }},
		{"spaced placeholder", func(m map[string]any) { setArgs("server", "{{ CACHE }}")(m) }},
		{"unterminated placeholder", func(m map[string]any) { setArgs("server", "{{CACHE")(m) }},
		{"placeholder after prefix", func(m map[string]any) { setArgs("prepare", "{{SOURCE}}/{{HOME}}")(m) }},
		{"NUL in argument", func(m map[string]any) { setArgs("server", "a\x00b")(m) }},
		{"pinned path in args", func(m map[string]any) { setArgs("server", execPath)(m) }},
		{"pinned path inside an argument", func(m map[string]any) { setArgs("server", "--config="+execPath+".cfg")(m) }},
		{"pinned path in prepare args", func(m map[string]any) { setArgs("prepare", "build", execPath)(m) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Decode(mutateJSON(t, good, c.mutate)); err == nil {
				t.Fatalf("accepted %s", c.name)
			}
		})
	}
}

func TestDecodeRequiresExactKeySpelling(t *testing.T) {
	fx := newFixture(t)
	good := marshal(t, fx.manifest(t))
	rename := func(parent func(map[string]any) map[string]any, from, to string) func(map[string]any) {
		return func(m map[string]any) {
			p := parent(m)
			p[to] = p[from]
			delete(p, from)
		}
	}
	top := func(m map[string]any) map[string]any { return m }
	server := func(m map[string]any) map[string]any { return section(m, "server") }
	prepare := func(m map[string]any) map[string]any { return section(m, "prepare") }
	// The first letter of kelvin is U+212A KELVIN SIGN, which Unicode case folding equates with "k".
	kelvin := "Kind"
	cases := []struct {
		name   string
		key    string
		mutate func(map[string]any)
	}{
		{"capitalised kind alone", "Kind", rename(top, "kind", "Kind")},
		{"kind and Kind", "Kind", func(m map[string]any) { m["Kind"] = "memory" }},
		{"uppercase server", "SERVER", rename(top, "server", "SERVER")},
		{"Server beside server", "Server", func(m map[string]any) { m["Server"] = section(m, "server") }},
		{"nested Executable", "Executable", rename(server, "executable", "Executable")},
		{"nested prepare timeoutMS", "timeoutMS", rename(prepare, "timeoutMs", "timeoutMS")},
		{"nested prepare Network", "Network", rename(prepare, "network", "Network")},
		{"embedded file Sha256", "Sha256", func(m map[string]any) {
			e := section(section(m, "server"), "executable")
			e["Sha256"] = e["sha256"]
			delete(e, "sha256")
		}},
		{"tool Access", "Access", func(m map[string]any) {
			tool := m["tools"].([]any)[0].(map[string]any)
			tool["Access"] = tool["access"]
			delete(tool, "access")
		}},
		{"dependency State", "State", func(m map[string]any) {
			dep := m["dependencies"].([]any)[0].(map[string]any)
			dep["State"] = dep["state"]
			delete(dep, "state")
		}},
		{"kelvin sign kind", kelvin, rename(top, "kind", kelvin)},
		{"kelvin sign beside kind", kelvin, func(m map[string]any) { m[kelvin] = "memory" }},
		{"two case variants", "KIND", func(m map[string]any) {
			delete(m, "kind")
			m["Kind"], m["KIND"] = "mcp-provider", "mcp-provider"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode(mutateJSON(t, good, c.mutate))
			if err == nil {
				t.Fatalf("accepted %s", c.name)
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Fatalf("the error must name the key %q: %v", c.key, err)
			}
		})
	}
}

func TestDecodeRejectsEveryPinnedPathInArgs(t *testing.T) {
	fx := newFixture(t)
	good := marshal(t, fx.manifest(t))
	for _, name := range fixtureFiles {
		for _, key := range []string{"server", "prepare"} {
			t.Run(name+" in "+key, func(t *testing.T) {
				raw := mutateJSON(t, good, setArgs(key, "--input", fx.files[name]))
				if _, err := Decode(raw); err == nil {
					t.Fatalf("accepted the pinned path %s in %s args", fx.files[name], key)
				}
			})
		}
	}
}

func TestDecodeRejectsMalformedInput(t *testing.T) {
	fx := newFixture(t)
	good := marshal(t, fx.manifest(t))
	withSuffix := func(suffix string) []byte { return append(append([]byte{}, good...), suffix...) }
	cases := []struct {
		name string
		raw  []byte
	}{
		{"trailing object", withSuffix(`{}`)},
		{"trailing garbage", withSuffix(` x`)},
		{"trailing value", withSuffix(` 1`)},
		{"empty", nil},
		{"not an object", []byte(`[]`)},
		{"null", []byte(`null`)},
		{"truncated", good[:len(good)/2]},
		{"top-level dup key", []byte(`{"name":"a","name":"b"}`)},
		{"nested dup key", []byte(strings.Replace(string(good), `"server":{`, `"server":{"args":[],`, 1))},
		{"oversize", bytes.Repeat([]byte(" "), 65<<10)},
	}
	for _, c := range cases {
		if _, err := Decode(c.raw); err == nil {
			t.Fatalf("accepted %s", c.name)
		}
	}
}

func TestDecodeSizeLimitIsExactly64KiB(t *testing.T) {
	fx := newFixture(t)
	good := marshal(t, fx.manifest(t))
	exact := append(append([]byte{}, good...), bytes.Repeat([]byte(" "), MaxManifestBytes-len(good))...)
	if len(exact) != 64<<10 {
		t.Fatalf("test bug: %d bytes", len(exact))
	}
	if _, err := Decode(exact); err != nil {
		t.Fatalf("exactly 64 KiB must be accepted: %v", err)
	}
	if _, err := Decode(append(exact, ' ')); err == nil {
		t.Fatal("64 KiB plus one byte must be rejected")
	}
}

func TestVerifyReturnsComputedIdentities(t *testing.T) {
	fx := newFixture(t)
	m := fx.manifest(t)
	ids, err := m.Verify()
	if err != nil {
		t.Fatal(err)
	}
	roles := []string{"server", "server interpreter", "prepare", "prepare interpreter", "dependency"}
	var got []pinned.Identity
	for i, name := range fixtureFiles {
		got = append(got, pinned.Identity{Role: roles[i], Path: fx.files[name], SHA256: sum([]byte("content of " + name))})
	}
	if !reflect.DeepEqual(ids, got) {
		t.Fatalf("identities\n got %+v\nwant %+v", ids, got)
	}
}

func TestVerifyOnlyChecksDeclaredFiles(t *testing.T) {
	fx := newFixture(t)
	m := fx.manifest(t)
	m.Prepare, m.Server.Interpreter = nil, nil
	ids, err := m.Verify()
	if err != nil || len(ids) != 2 || ids[0].Role != "server" || ids[1].Role != "dependency" {
		t.Fatalf("identities %+v, err %v", ids, err)
	}
}

func TestVerifyRefusesUnsafeExecutables(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{"symlink", func(t *testing.T, path string) {
			real := path + ".real"
			if err := os.Rename(path, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"world-writable file", func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o777); err != nil {
				t.Fatal(err)
			}
		}},
		{"group-writable file", func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o770); err != nil {
				t.Fatal(err)
			}
		}},
		{"writable parent directory", func(t *testing.T, path string) {
			if err := os.Chmod(filepath.Dir(path), 0o777); err != nil {
				t.Fatal(err)
			}
		}},
		{"digest mismatch", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"removed", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, name := range fixtureFiles {
		for _, mu := range mutations {
			t.Run(name+" "+mu.name, func(t *testing.T) {
				fx := newFixture(t)
				m := fx.manifest(t)
				mu.mutate(t, fx.files[name])
				ids, err := m.Verify()
				if err == nil || !strings.Contains(err.Error(), fx.files[name]) {
					t.Fatalf("expected an error naming %s, got %v", fx.files[name], err)
				}
				if ids != nil {
					t.Fatalf("no identities may be returned on failure: %+v", ids)
				}
			})
		}
	}
}

func TestToolsForAnalysisAreReadOnly(t *testing.T) {
	m := Manifest{Tools: []Tool{{Name: "q", Access: "read-only"}, {Name: "w", Access: "read-write"}, {Name: "r", Access: "read-only"}}, Roles: []string{"analysis", "coding"}}
	if got := m.ToolsFor("analysis"); !reflect.DeepEqual(got, []Tool{{Name: "q", Access: "read-only"}, {Name: "r", Access: "read-only"}}) {
		t.Fatalf("analysis must get only read-only tools: %v", got)
	}
	if got := m.ToolsFor("coding"); !reflect.DeepEqual(got, m.Tools) {
		t.Fatalf("coding gets every tool in declared order: %v", got)
	}
}

func TestToolsForMetaAndUnknownRolesGetNothing(t *testing.T) {
	m := Manifest{Tools: []Tool{{Name: "q", Access: "read-only"}}, Roles: []string{"analysis", "coding"}}
	for _, role := range []string{"meta", "deploy", "", "Analysis", "coding "} {
		if got := m.ToolsFor(role); got == nil || len(got) != 0 {
			t.Fatalf("role %q must get an empty list: %#v", role, got)
		}
	}
}

func TestToolsForRoleMustBeListed(t *testing.T) {
	tool := []Tool{{Name: "q", Access: "read-only"}}
	if got := (Manifest{Tools: tool, Roles: []string{"coding"}}).ToolsFor("analysis"); len(got) != 0 {
		t.Fatalf("analysis is not listed: %v", got)
	}
	if got := (Manifest{Tools: tool, Roles: []string{"analysis"}}).ToolsFor("coding"); len(got) != 0 {
		t.Fatalf("coding is not listed: %v", got)
	}
	if got := (Manifest{Tools: tool}).ToolsFor("coding"); len(got) != 0 {
		t.Fatalf("no roles listed: %v", got)
	}
	if got := (Manifest{Roles: []string{"analysis"}}).ToolsFor("analysis"); len(got) != 0 {
		t.Fatalf("no tools declared: %v", got)
	}
}

func TestLoadReturnsVerifiedManifest(t *testing.T) {
	fx := newFixture(t)
	want := fx.manifest(t)
	rel, digest := fx.writeManifest(t, marshal(t, want))
	m, ids, err := Load(fx.lab, rel, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, want) || len(ids) != len(fixtureFiles) {
		t.Fatalf("manifest %+v, identities %+v", m, ids)
	}
}

func TestLoadAcceptsExactlyMaxManifestBytes(t *testing.T) {
	fx := newFixture(t)
	raw := marshal(t, fx.manifest(t))
	raw = append(raw, bytes.Repeat([]byte(" "), MaxManifestBytes-len(raw))...)
	rel, digest := fx.writeManifest(t, raw)
	if _, _, err := Load(fx.lab, rel, digest); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRequiresAbsoluteCleanLabDir(t *testing.T) {
	fx := newFixture(t)
	rel, digest := fx.writeManifest(t, marshal(t, fx.manifest(t)))
	t.Chdir(fx.root)
	for _, bad := range []string{
		"",
		"lab",
		"./lab",
		"../" + filepath.Base(fx.root) + "/lab",
		fx.lab + "/",
		fx.lab + "/.",
		fx.root + "/x/../lab",
		fx.root + "//lab",
	} {
		_, _, err := Load(bad, rel, digest)
		if err == nil || !strings.Contains(err.Error(), "clean absolute path") {
			t.Fatalf("Lab directory %q: expected a rejection, got %v", bad, err)
		}
	}
	if _, _, err := Load(fx.lab, rel, digest); err != nil {
		t.Fatalf("the clean absolute Lab directory must load: %v", err)
	}
}

func TestLoadRejectsPathsOutsideTheLab(t *testing.T) {
	fx := newFixture(t)
	raw := marshal(t, fx.manifest(t))
	rel, digest := fx.writeManifest(t, raw)
	outside := writeFile(t, filepath.Join(fx.root, "outside.json"), string(raw), 0o600)
	for _, bad := range []string{
		"",
		"../outside.json",
		"extensions/../../outside.json",
		outside,
		"extensions/../" + rel,
		"./" + rel,
		"extensions//graphify.json",
		rel + "/",
		"..",
	} {
		_, _, err := Load(fx.lab, bad, digest)
		if err == nil || !strings.Contains(err.Error(), "inside the Lab") {
			t.Fatalf("%q: expected a path rejection, got %v", bad, err)
		}
	}
}

func TestLoadRequiresMatchingDigest(t *testing.T) {
	fx := newFixture(t)
	rel, digest := fx.writeManifest(t, marshal(t, fx.manifest(t)))
	for _, bad := range []string{strings.Repeat("0", 64), strings.ToUpper(digest), "", digest[:63]} {
		if _, _, err := Load(fx.lab, rel, bad); err == nil || !strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("digest %q: expected a mismatch, got %v", bad, err)
		}
	}
}

func TestLoadRejectsOversizeManifest(t *testing.T) {
	fx := newFixture(t)
	raw := marshal(t, fx.manifest(t))
	raw = append(raw, bytes.Repeat([]byte(" "), MaxManifestBytes+1-len(raw))...)
	rel, digest := fx.writeManifest(t, raw)
	if _, _, err := Load(fx.lab, rel, digest); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected the size cap to refuse the manifest, got %v", err)
	}
}

func TestLoadRefusesUnsafeOrMissingManifestFile(t *testing.T) {
	fx := newFixture(t)
	raw := marshal(t, fx.manifest(t))
	rel, digest := fx.writeManifest(t, raw)
	path := filepath.Join(fx.lab, filepath.FromSlash(rel))
	if _, _, err := Load(fx.lab, "extensions/missing.json", digest); err == nil {
		t.Fatal("a missing manifest must be refused")
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(fx.lab, rel, digest); err == nil || !strings.Contains(err.Error(), "group- or world-writable") {
		t.Fatalf("a world-writable manifest must be refused: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(fx.lab, "extensions", "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(fx.lab, "extensions/link.json", digest); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked manifest must be refused: %v", err)
	}
}

func TestLoadRejectsInvalidManifest(t *testing.T) {
	fx := newFixture(t)
	raw := mutateJSON(t, marshal(t, fx.manifest(t)), func(m map[string]any) { m["kind"] = "memory" })
	rel, digest := fx.writeManifest(t, raw)
	if _, _, err := Load(fx.lab, rel, digest); err == nil || !strings.Contains(err.Error(), "mcp-provider") {
		t.Fatalf("expected a decode error, got %v", err)
	}
}

func TestLoadRefusesWhenPinnedFilesChanged(t *testing.T) {
	fx := newFixture(t)
	rel, digest := fx.writeManifest(t, marshal(t, fx.manifest(t)))
	if err := os.WriteFile(fx.files[fileDependency], []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, ids, err := Load(fx.lab, rel, digest)
	if err == nil || !strings.Contains(err.Error(), fx.files[fileDependency]) {
		t.Fatalf("expected an error naming the changed dependency, got %v", err)
	}
	if ids != nil || !reflect.DeepEqual(m, Manifest{}) {
		t.Fatalf("a failed load must return nothing: %+v %+v", m, ids)
	}
}
