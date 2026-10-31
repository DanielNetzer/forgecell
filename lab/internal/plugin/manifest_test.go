package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/extension"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

// dummyServer writes a small executable under the test's temporary directory and
// pins it; decoding never runs it.
func dummyServer(t *testing.T) pinned.File {
	t.Helper()
	path := filepath.Join(plugintest.TempDir(t), "server")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("#!/bin/sh\n"))
	return pinned.File{Path: path, SHA256: hex.EncodeToString(sum[:])}
}

func edit(t *testing.T, raw []byte, change func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	change(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func needs(m map[string]any) map[string]any { return m["needs"].(map[string]any) }

func TestDecodeAcceptsEveryPluginKind(t *testing.T) {
	server := dummyServer(t)
	for _, kind := range []string{"tracker", "forge", "reviewer"} {
		m, err := Decode(plugintest.Manifest(t, server, kind, "good", "API_TOKEN"))
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if m.Kind != kind || m.Contract != "forgecell."+kind+"/v1" || m.Needs.MinPollSeconds != 60 || m.Needs.Env[0] != "API_TOKEN" {
			t.Fatalf("%s decoded as %+v", kind, m)
		}
	}
	extra := edit(t, plugintest.Manifest(t, server, "forge", "good"), func(m map[string]any) {
		m["tools"] = append(m["tools"].([]any), map[string]any{"name": "search_code", "access": "read-only"})
		m["roles"] = []any{"analysis"}
	})
	if _, err := Decode(extra); err != nil {
		t.Fatalf("an extra harness tool and a role were refused: %v", err)
	}
}

func TestDecodeRefusesBadManifests(t *testing.T) {
	server := dummyServer(t)
	base := plugintest.Manifest(t, server, "forge", "good")
	cases := map[string]func(map[string]any){
		"mcp-provider":      func(m map[string]any) { m["kind"] = "mcp-provider" },
		"unknown kind":      func(m map[string]any) { m["kind"] = "memory" },
		"wrong contract":    func(m map[string]any) { m["contract"] = "forgecell.tracker/v1" },
		"contract v2":       func(m map[string]any) { m["contract"] = "forgecell.forge/v2" },
		"bad name":          func(m map[string]any) { m["name"] = "GitLab" },
		"missing tool":      func(m map[string]any) { m["tools"] = m["tools"].([]any)[1:] },
		"write as read":     func(m map[string]any) { setAccess(m, "forge_publish_draft", "read-only") },
		"read as write":     func(m map[string]any) { setAccess(m, "forge_describe", "read-write") },
		"poll 59":           func(m map[string]any) { needs(m)["minPollSeconds"] = 59 },
		"poll 3601":         func(m map[string]any) { needs(m)["minPollSeconds"] = 3601 },
		"poll fraction":     func(m map[string]any) { needs(m)["minPollSeconds"] = 60.5 },
		"no needs":          func(m map[string]any) { delete(m, "needs") },
		"env PATH":          func(m map[string]any) { needs(m)["env"] = []any{"PATH"} },
		"env HOME":          func(m map[string]any) { needs(m)["env"] = []any{"HOME"} },
		"env loader":        func(m map[string]any) { needs(m)["env"] = []any{"LD_PRELOAD"} },
		"env twice":         func(m map[string]any) { needs(m)["env"] = []any{"A_TOKEN", "A_TOKEN"} },
		"env invalid":       func(m map[string]any) { needs(m)["env"] = []any{"A-TOKEN"} },
		"bad host":          func(m map[string]any) { needs(m)["network"] = []any{"GitLab.com"} },
		"host twice":        func(m map[string]any) { needs(m)["network"] = []any{"gitlab.com", "gitlab.com"} },
		"needs unknown key": func(m map[string]any) { needs(m)["shell"] = true },
		"unknown key":       func(m map[string]any) { m["autoApprove"] = true },
		"bad role":          func(m map[string]any) { m["roles"] = []any{"meta"} },
		"placeholder arg":   func(m map[string]any) { m["server"].(map[string]any)["args"] = []any{"{{CACHE}}"} },
		"relative server": func(m map[string]any) {
			m["server"].(map[string]any)["executable"] = map[string]any{"path": "bin/x", "sha256": strings.Repeat("a", 64)}
		},
		"unpinned dep": func(m map[string]any) {
			m["dependencies"] = []any{map[string]any{"path": "/opt/x", "sha256": "abc", "role": "lib", "state": "pinned"}}
		},
		"bad schema": func(m map[string]any) { m["schemaVersion"] = "v2" },
	}
	for name, change := range cases {
		if _, err := Decode(edit(t, base, change)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := extension.Decode(base); err == nil {
		t.Fatal("extension.Decode accepted a plugin manifest")
	}
	variant := strings.Replace(string(base), `"kind"`, `"Kind"`, 1)
	duplicate := strings.Replace(string(base), `"kind": "forge",`, `"kind": "forge", "kind": "tracker",`, 1)
	for name, raw := range map[string]string{
		"case variant": variant,
		"duplicate":    duplicate,
		"oversized":    string(base[:len(base)-1]) + `,"limitations":"` + strings.Repeat("a", extension.MaxManifestBytes) + `"}`,
		"trailing":     string(base) + "{}",
	} {
		if raw == string(base) {
			t.Fatalf("%s: the edit did not apply", name)
		}
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func setAccess(m map[string]any, tool, access string) {
	for _, entry := range m["tools"].([]any) {
		if e := entry.(map[string]any); e["name"] == tool {
			e["access"] = access
		}
	}
}

func TestLoadFileDigestsTheBytes(t *testing.T) {
	server := dummyServer(t)
	raw := plugintest.Manifest(t, server, "tracker", "good")
	path := plugintest.WriteManifest(t, raw)
	m, digest, err := LoadFile(path)
	sum := sha256.Sum256(raw)
	if err != nil || digest != hex.EncodeToString(sum[:]) || m.Name != "fake-tracker" {
		t.Fatalf("LoadFile = %+v, %s, %v", m, digest, err)
	}
	link := filepath.Join(filepath.Dir(path), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadFile(link); mcp.ReasonOf(err) != mcp.NotInstalled {
		t.Fatalf("a symlinked manifest: %v", err)
	}
	if _, _, err := LoadFile("plugin.json"); err == nil {
		t.Fatal("a relative manifest path was accepted")
	}
	if _, _, err := LoadFile(filepath.Join(filepath.Dir(path), "missing.json")); mcp.ReasonOf(err) != mcp.NotInstalled {
		t.Fatalf("a missing manifest: %v", err)
	}
	bad := plugintest.WriteManifest(t, edit(t, raw, func(m map[string]any) { m["kind"] = "forge" }))
	if _, _, err := LoadFile(bad); mcp.ReasonOf(err) != mcp.SchemaInvalid {
		t.Fatalf("an invalid manifest: %v", err)
	}
}

func TestLoadVerifiesTheManifestAndItsPins(t *testing.T) {
	server := dummyServer(t)
	lab := plugintest.TempDir(t)
	raw := plugintest.Manifest(t, server, "reviewer", "good")
	if err := os.MkdirAll(filepath.Join(lab, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "plugins", "bot.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	m, ids, err := Load(lab, "plugins/bot.json", digest)
	if err != nil || len(ids) != 1 || ids[0].SHA256 != server.SHA256 || m.Kind != "reviewer" {
		t.Fatalf("Load = %+v, %+v, %v", m, ids, err)
	}
	if _, _, err := Load(lab, "plugins/bot.json", strings.Repeat("0", 64)); mcp.ReasonOf(err) != mcp.ManifestChanged {
		t.Fatalf("a changed manifest: %v", err)
	}
	for _, rel := range []string{"../bot.json", "/plugins/bot.json", "plugins//bot.json", "extensions/x/" + strings.Repeat("a", 64) + "/m.json"} {
		if _, _, err := Load(lab, rel, digest); err == nil {
			t.Errorf("Load accepted %q", rel)
		}
	}
	if err := os.Symlink(filepath.Join(lab, "plugins"), filepath.Join(lab, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(lab, "linked/bot.json", digest); err == nil {
		t.Fatal("Load passed through a symlink")
	}
	if err := os.WriteFile(server.Path, []byte("changed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(lab, "plugins/bot.json", digest); mcp.ReasonOf(err) != mcp.ManifestChanged {
		t.Fatalf("a changed server: %v", err)
	}
	if err := os.Remove(server.Path); err != nil {
		t.Fatal(err)
	}
	_, _, err = Load(lab, "plugins/bot.json", digest)
	if mcp.ReasonOf(err) != mcp.NotInstalled || strings.Contains(err.Error(), lab) {
		t.Fatalf("a missing server: %v", err)
	}
}
