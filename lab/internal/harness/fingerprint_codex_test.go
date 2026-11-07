package harness

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const codexConfigBase = `model = "gpt-5"
approval_policy = "on-request"
sandbox_mode = "workspace-write"

[sandbox_workspace_write]
network_access = false

[mcp_servers.docs]
command = "docs-mcp"
args = [
  "--port",
  "7000",
]

[plugins.search]
enabled = true

[marketplaces.official]
source = "https://example.invalid/market"
last_updated = "2026-11-01T00:00:00Z"

[desktop]
theme = "dark"

[desktop.window]
width = 1200

[projects."/work/repo"]
trust_level = "trusted"

[profiles.fast]
model = "gpt-5-mini"
`

func codexFingerprintSetup(t *testing.T) (func(string) string, []string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	provider := filepath.Join(t.TempDir(), "codex")
	os.WriteFile(provider, []byte("v1"), 0700)
	build, _ := fakeBuild(t, "1.0.0", AdapterProtocolVersion)
	command := []string{build, "__adapter", "codex", provider}
	return func(config string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := Fingerprint(command)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}, command
}

func TestFingerprintCodexConfigIgnoresCodexBookkeeping(t *testing.T) {
	write, _ := codexFingerprintSetup(t)
	base := write(codexConfigBase)
	for name, config := range map[string]string{
		"marketplace last_updated":         strings.Replace(codexConfigBase, "2026-11-01T00:00:00Z", "2026-11-07T19:00:00Z", 1),
		"marketplace last_updated removed": strings.Replace(codexConfigBase, "last_updated = \"2026-11-01T00:00:00Z\"\n", "", 1),
		"desktop key":                      strings.Replace(codexConfigBase, `theme = "dark"`, `theme = "light"`, 1),
		"desktop subtable":                 strings.Replace(codexConfigBase, "width = 1200", "width = 900\nheight = [\n  1,\n  [2],\n]", 1),
		"project trust":                    strings.Replace(codexConfigBase, `trust_level = "trusted"`, `trust_level = "untrusted"`, 1),
		"project added":                    codexConfigBase + "\n[projects.\"/work/forgecell-trial\"]\ntrust_level = \"trusted\"\n",
		"blank lines and trailing spaces":  strings.ReplaceAll(codexConfigBase, "\n", "  \n\n"),
		"crlf line endings":                strings.ReplaceAll(codexConfigBase, "\n", "\r\n"),
	} {
		if got := write(config); got != base {
			t.Errorf("%s changed the Codex fingerprint", name)
		}
	}
}

func TestFingerprintCodexConfigCountsBehaviouralKeys(t *testing.T) {
	write, _ := codexFingerprintSetup(t)
	base := write(codexConfigBase)
	for name, config := range map[string]string{
		"model":                   strings.Replace(codexConfigBase, `model = "gpt-5"`, `model = "gpt-4"`, 1),
		"approval policy":         strings.Replace(codexConfigBase, `"on-request"`, `"never"`, 1),
		"sandbox mode":            strings.Replace(codexConfigBase, `"workspace-write"`, `"danger-full-access"`, 1),
		"sandbox_workspace_write": strings.Replace(codexConfigBase, "network_access = false", "network_access = true", 1),
		"mcp server entry":        strings.Replace(codexConfigBase, `"7000"`, `"7001"`, 1),
		"mcp server added":        codexConfigBase + "\n[mcp_servers.other]\ncommand = \"other\"\n",
		"plugin enabled":          strings.Replace(codexConfigBase, "enabled = true", "enabled = false", 1),
		"marketplace source":      strings.Replace(codexConfigBase, "example.invalid/market", "example.invalid/other", 1),
		"unknown top-level key":   "service_tier = \"flex\"\n" + codexConfigBase,
		"profile":                 strings.Replace(codexConfigBase, `"gpt-5-mini"`, `"gpt-5-nano"`, 1),
		"table after dropped one": codexConfigBase + "\n[desktop.panel]\nopen = true\n  [model_providers.local]\nbase_url = \"http://localhost\"\n",
		"comment":                 "# pinned\n" + codexConfigBase,
	} {
		if got := write(config); got == base {
			t.Errorf("%s did not change the Codex fingerprint", name)
		}
	}
}

func TestFingerprintCodexConfigFallsBackToRawBytes(t *testing.T) {
	for name, config := range map[string]string{
		"multi-line basic string":   codexConfigBase + "instructions = \"\"\"\nbe careful\n\"\"\"\n",
		"multi-line literal string": codexConfigBase + "instructions = '''\nbe careful\n'''\n",
		"root dotted desktop":       "desktop.theme = \"dark\"\n" + codexConfigBase,
		"root inline projects":      "projects = { \"/work\" = { trust_level = \"trusted\" } }\n" + codexConfigBase,
		"root dotted last_updated":  "marketplaces.official.last_updated = \"x\"\n" + codexConfigBase,
		"array of dropped tables":   codexConfigBase + "\n[[desktop.recent]]\npath = \"/work\"\n",
		"bare projects table":       codexConfigBase + "\n[projects]\n\"/work\" = { trust_level = \"trusted\" }\n",
		"unreadable line":           codexConfigBase + "\nthis is not toml\n",
		"unclosed array":            codexConfigBase + "\nnotify = [\n",
	} {
		if _, ok := normalizeCodexConfig(config); ok {
			t.Errorf("%s was normalized instead of hashed raw", name)
		}
		write, _ := codexFingerprintSetup(t)
		a := write(config)
		if b := write(strings.Replace(config, `theme = "dark"`, `theme = "light"`, 1)); a == b {
			t.Errorf("%s: a change in a would-be-dropped section was invisible under the raw fallback", name)
		}
		if b := write(config + " "); a == b {
			t.Errorf("%s: a byte change was invisible under the raw fallback", name)
		}
	}
}

func TestFingerprintCodexConfigTagsItsForm(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(file, []byte(codexConfigBase), 0600)
	normalized, err := codexConfigDigest(file)
	if err != nil || !strings.HasPrefix(normalized, "normalized:") {
		t.Fatalf("normalized digest = %q, %v", normalized, err)
	}
	os.WriteFile(file, []byte("desktop.x = 1\n"), 0600)
	raw, err := codexConfigDigest(file)
	if err != nil || !strings.HasPrefix(raw, "raw:") {
		t.Fatalf("raw digest = %q, %v", raw, err)
	}
	text, _ := normalizeCodexConfig(codexConfigBase)
	os.WriteFile(file, []byte(text), 0600)
	same, _ := codexConfigDigest(file)
	if same != normalized {
		t.Fatal("an already-normalized file digested differently")
	}
	os.Remove(file)
	absent, err := codexConfigDigest(file)
	if err != nil || !strings.HasPrefix(absent, "absent:") {
		t.Fatalf("absent digest = %q, %v", absent, err)
	}
	if _, err := codexConfigDigest(t.TempDir()); err == nil {
		t.Fatal("a non-regular config.toml was fingerprinted")
	}
}

func TestFingerprintCodexConfigLeavesOtherHarnessesAlone(t *testing.T) {
	_, command := codexFingerprintSetup(t)
	home := os.Getenv("CODEX_HOME")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	script := filepath.Join(t.TempDir(), "custom")
	os.WriteFile(script, []byte("one"), 0700)
	commands := map[string][]string{
		"claude-code": {command[0], "__adapter", "claude-code", command[3]},
		"cursor":      {command[0], "__adapter", "cursor", command[3]},
		"custom":      {script, "--run"},
	}
	before := map[string]string{}
	for name, c := range commands {
		got, err := Fingerprint(c)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = got
	}
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"other\"\n"), 0600)
	for name, c := range commands {
		if got, err := Fingerprint(c); err != nil || got != before[name] {
			t.Errorf("%s fingerprint changed with the Codex config: %v", name, err)
		}
	}
	// The fingerprint input of a binding without a Codex config is the one used
	// before the Codex field existed.
	for name, files := range map[string][]string{"cursor": {command[3]}, "custom": {script}} {
		contract := commands[name]
		if name == "cursor" {
			contract = contract[1:]
		}
		digest, err := fingerprintFiles(files)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(struct {
			Files       string
			Command     []string
			Protocol    string
			Environment map[string]string
		}{digest, contract, AdapterProtocolVersion, map[string]string{}})
		if want := fmt.Sprintf("%x", sha256.Sum256(raw)); before[name] != want {
			t.Errorf("%s fingerprint input changed", name)
		}
	}
}
