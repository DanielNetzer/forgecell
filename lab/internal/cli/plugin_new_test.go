package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

// pluginUsageBeforeNew is the usage of plugin test, list and doctor, which the
// new and pin commands extend without changing.
const pluginUsageBeforeNew = "usage: forgecell plugin test <manifest> [--json] | forgecell plugin list [--lab DIR] [--lab-name NAME] [--json] | forgecell plugin doctor [NAME] [--lab DIR] [--lab-name NAME] [--json]"

// noAbsolutePath fails when text names the temporary root or holds any
// absolute path.
func noAbsolutePath(t *testing.T, label, text, root string) {
	t.Helper()
	if strings.Contains(text, root) || strings.Contains(text, " /") || strings.HasPrefix(text, "/") || strings.Contains(text, "\n/") {
		t.Fatalf("%s names an absolute path:\n%s", label, text)
	}
}

func TestPluginNewWritesASkeletonAndPrintsRelativeCommands(t *testing.T) {
	root := plugintest.TempDir(t)
	t.Chdir(root)
	code, out, errOut := runCLI("plugin", "new", "--kind", "tracker", "--lang", "go", "demo")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{
		"  go -C ./demo build -o demo .\n",
		"  forgecell plugin pin ./demo --server ./demo/demo\n",
		"  forgecell plugin test ./demo/plugin.json\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	noAbsolutePath(t, "the output", out, root)
	if _, err := os.Lstat(filepath.Join(root, "demo", "plugin.json.tmpl")); err != nil {
		t.Fatal(err)
	}

	// An absolute --dir is still shown relative to the working directory.
	other := filepath.Join(root, "nested")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = runCLI("plugin", "new", "--dir", filepath.Join(other, "fg"), "--kind", "forge", "--lang", "go", "fg")
	if code != 0 || !strings.Contains(out, "go -C ./nested/fg build -o fg .") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	noAbsolutePath(t, "the output", out, root)

	// A directory with characters a shell reads specially is shown as <dir>.
	code, out, errOut = runCLI("plugin", "new", "--dir", "odd dir;x", "--kind", "reviewer", "--lang", "go", "rv")
	if code != 0 || !strings.Contains(out, "go -C <dir> build -o rv .") || strings.Contains(out, "odd dir") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestPluginNewRefusals(t *testing.T) {
	root := plugintest.TempDir(t)
	t.Chdir(root)
	full := filepath.Join(root, "full")
	if err := os.Mkdir(full, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--kind", "wiki", "--lang", "go", "x"}, "kind must be"},
		{[]string{"--kind", "tracker", "--lang", "rust", "x"}, "language must be go"},
		{[]string{"--kind", "tracker", "--lang", "cobol", "x"}, "language must be go"},
		{[]string{"--kind", "tracker", "--lang", "go", "Bad_Name"}, "plugin name must be"},
		{[]string{"--kind", "tracker", "--lang", "go", "--dir", full, "x"}, "not empty"},
	} {
		code, out, errOut := runCLI(append([]string{"plugin", "new"}, c.args...)...)
		if code != 1 || out != "" || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: exit %d, %q%q", c.args, code, out, errOut)
		}
		noAbsolutePath(t, "the error", errOut, root)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatalf("a refusal left %d entries", len(entries))
	}
	for _, args := range [][]string{
		{"plugin", "new"},
		{"plugin", "new", "x"},
		{"plugin", "new", "--kind", "tracker", "x"},
		{"plugin", "new", "--kind", "tracker", "--lang", "go"},
		{"plugin", "new", "--kind", "tracker", "--kind", "forge", "--lang", "go", "x"},
		{"plugin", "new", "--kind", "tracker", "--lang", "go", "x", "y"},
		{"plugin", "new", "--kind", "tracker", "--lang", "go", "--verbose", "x"},
		{"plugin", "new", "--kind", "tracker", "--lang"},
	} {
		if code, _, errOut := runCLI(args...); code != 1 || errOut != "forgecell: "+pluginNewUsage+"\n" {
			t.Errorf("%v: exit %d, %q", args, code, errOut)
		}
	}
}

func TestPluginPinPrintsTheDigestAndFormulaLine(t *testing.T) {
	root := plugintest.TempDir(t)
	t.Chdir(root)
	if code, out, errOut := runCLI("plugin", "new", "--kind", "reviewer", "--lang", "go", "rv"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	// pin only hashes the server, so a stand-in file serves.
	if err := os.WriteFile(filepath.Join(root, "rv", "rv"), []byte("stand-in"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("plugin", "pin", "./rv", "--server", "./rv/rv")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	raw, err := os.ReadFile(filepath.Join(root, "rv", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	entry := "  - {name: rv, manifest: plugins/rv/plugin.json, sha256: \"" + digest + "\"}"
	if !strings.Contains(out, "Manifest SHA-256: "+digest+"\n") ||
		!strings.HasSuffix(out, " under the Formula's plugins:\n"+entry+"\n") {
		t.Fatalf("output:\n%s", out)
	}
	// The printed entry is the Formula's own list form, so it parses as is.
	recipe := "schemaVersion: v0\nkind: formula\nid: sample\nintake:\n  repo: acme/widgets\n" +
		"atoms:\n  - id: intake\n    type: intake\n  - id: work\n    type: harness\nplugins:\n" + entry + "\n"
	f, err := formula.Parse([]byte(recipe))
	if err != nil {
		t.Fatalf("the printed plugins: entry does not parse: %v", err)
	}
	if len(f.Plugins) != 1 || f.Plugins[0] != (formula.PluginRef{Name: "rv", Manifest: "plugins/rv/plugin.json", SHA256: digest}) {
		t.Fatalf("the printed entry decoded as %+v", f.Plugins)
	}
	noAbsolutePath(t, "the output", out, root)
	if !strings.Contains(string(raw), filepath.Join(root, "rv", "rv")) {
		t.Fatal("plugin.json does not record the server's absolute path")
	}

	if err := os.WriteFile(filepath.Join(root, `q"s`), nil, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "rv", "rv"), link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"./rv", "--server", `./q"s`}, `must not contain "`},
		{[]string{"./rv", "--server", link}, "symlink"},
		{[]string{"./rv", "--server", filepath.Join(root, "absent")}, "missing"},
		{[]string{filepath.Join(root, "absent"), "--server", "./rv/rv"}, "missing"},
	} {
		code, out, errOut := runCLI(append([]string{"plugin", "pin"}, c.args...)...)
		if code != 1 || out != "" || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: exit %d, %q%q", c.args, code, out, errOut)
		}
		noAbsolutePath(t, "the error", errOut, root)
	}
	for _, args := range [][]string{
		{"plugin", "pin"},
		{"plugin", "pin", "./rv"},
		{"plugin", "pin", "--server", "./rv/rv"},
		{"plugin", "pin", "./rv", "--server"},
		{"plugin", "pin", "./rv", "./other", "--server", "./rv/rv"},
		{"plugin", "pin", "./rv", "--server", "a", "--server", "b"},
	} {
		if code, _, errOut := runCLI(args...); code != 1 || errOut != "forgecell: "+pluginPinUsage+"\n" {
			t.Errorf("%v: exit %d, %q", args, code, errOut)
		}
	}
}

func TestPluginUsageKeepsTestListAndDoctor(t *testing.T) {
	if pluginUsage != pluginUsageBeforeNew {
		t.Fatalf("the test, list and doctor usage changed:\n%s", pluginUsage)
	}
	if pluginNewUsage != "usage: forgecell plugin new --kind tracker|forge|reviewer --lang go|python|typescript [--dir DIR] <name>" ||
		pluginPinUsage != "usage: forgecell plugin pin <dir> --server <path> [--interpreter <path>]" {
		t.Fatalf("new and pin usage: %q, %q", pluginNewUsage, pluginPinUsage)
	}
	// Each usage error of plugin test, list and doctor prints the base text,
	// byte for byte.
	for _, args := range [][]string{
		{"plugin"}, {"plugin", "frobnicate"}, {"plugin", "test"}, {"plugin", "test", "a", "b"},
		{"plugin", "list", "extra"}, {"plugin", "doctor", "a", "b"},
	} {
		if code, out, errOut := runCLI(args...); code != 1 || out != "" || errOut != "forgecell: "+pluginUsageBeforeNew+"\n" {
			t.Errorf("%v: exit %d, %q%q", args, code, out, errOut)
		}
	}
}
