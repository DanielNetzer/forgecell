package formula

import (
	"strings"
	"testing"
)

const (
	oldLauncher = "/home/op/.forgecell/releases/0.9.1/forgecell"
	newLauncher = "/home/op/.forgecell/releases/0.9.2-dev.ab12/forgecell"
)

const launcherRecipe = `schemaVersion: v0
kind: formula
id: sample
harness:
  binding: claude-code
  command: [/home/op/.forgecell/releases/0.9.1/forgecell, __adapter, claude-code, /bin/claude]
  timeoutMs: 1000
  instructions: keep /home/op/.forgecell/releases/0.9.1/forgecell untouched here
harnesses:
  analysis: {binding: codex, command: ["/home/op/.forgecell/releases/0.9.1/forgecell", "__adapter", "codex", "/bin/codex"]}
atoms:
  - id: intake
    type: intake
  - id: work
    type: harness
  - id: learn
    type: learn
    command: ['/home/op/.forgecell/releases/0.9.1/forgecell', __adapter, codex, /bin/codex]
`

// rebind replaces the launcher on the given 1-based lines only.
func rebind(t *testing.T, src string, lines ...int) string {
	t.Helper()
	parts := strings.Split(src, "\n")
	for _, n := range lines {
		if !strings.Contains(parts[n-1], oldLauncher) {
			t.Fatalf("line %d holds no launcher: %q", n, parts[n-1])
		}
		parts[n-1] = strings.Replace(parts[n-1], oldLauncher, newLauncher, 1)
	}
	return strings.Join(parts, "\n")
}

func TestLauncherOnlyChangeAccepts(t *testing.T) {
	if _, err := Parse([]byte(launcherRecipe)); err != nil {
		t.Fatal(err)
	}
	for name, next := range map[string]string{
		"coding only":        rebind(t, launcherRecipe, 6),
		"every native role":  rebind(t, launcherRecipe, 6, 10, 18),
		"analysis and learn": rebind(t, launcherRecipe, 10, 18),
	} {
		if err := LauncherOnlyChange([]byte(launcherRecipe), []byte(next)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLauncherOnlyChangeRefuses(t *testing.T) {
	rebound := rebind(t, launcherRecipe, 6)
	cases := map[string]string{
		"identical":          launcherRecipe,
		"instructions path":  rebind(t, launcherRecipe, 8),
		"provider path":      strings.Replace(launcherRecipe, "claude-code, /bin/claude]", "claude-code, /opt/claude]", 1),
		"adapter name":       strings.Replace(launcherRecipe, "__adapter, claude-code,", "__adapter, codex,", 1),
		"timeout":            strings.Replace(rebound, "timeoutMs: 1000", "timeoutMs: 2000", 1),
		"trailing byte":      rebound + "\n",
		"comment":            rebound + "# note\n",
		"whitespace":         strings.Replace(rebound, "binding: claude-code\n", "binding:  claude-code\n", 1),
		"Formula id":         strings.Replace(rebound, "id: sample", "id: other", 1),
		"not a release":      strings.Replace(launcherRecipe, oldLauncher+", __adapter, claude", "/usr/local/bin/forgecell, __adapter, claude", 1),
		"relative release":   strings.Replace(launcherRecipe, oldLauncher+", __adapter, claude", "releases/0.9.2/forgecell, __adapter, claude", 1),
		"traversal":          strings.Replace(launcherRecipe, oldLauncher+", __adapter, claude", "/home/op/.forgecell/releases/../releases/0.9.2/forgecell, __adapter, claude", 1),
		"dot segment":        strings.Replace(launcherRecipe, oldLauncher+", __adapter, claude", "/home/op/./releases/0.9.2/forgecell, __adapter, claude", 1),
		"malformed version":  strings.Replace(launcherRecipe, oldLauncher+", __adapter, claude", "/home/op/.forgecell/releases/latest/forgecell, __adapter, claude", 1),
		"other executable":   strings.Replace(launcherRecipe, oldLauncher+", __adapter, claude", "/home/op/.forgecell/releases/0.9.2/forgecellx, __adapter, claude", 1),
		"invalid next":       strings.Replace(rebound, "kind: formula", "kind: molecule", 1),
		"oversized":          rebound + "# " + strings.Repeat("x", 1_000_000) + "\n",
		"quoting style":      strings.Replace(launcherRecipe, "[/home/op/.forgecell/releases/0.9.1/forgecell,", "[\""+newLauncher+"\",", 1),
		"moved to next line": strings.Replace(launcherRecipe, "  command: [/home/op/.forgecell/releases/0.9.1/forgecell, __adapter, claude-code, /bin/claude]\n  timeoutMs: 1000\n", "  timeoutMs: 1000\n  command: ["+newLauncher+", __adapter, claude-code, /bin/claude]\n", 1),
	}
	for name, next := range cases {
		if err := LauncherOnlyChange([]byte(launcherRecipe), []byte(next)); err == nil {
			t.Errorf("%s: accepted a change that is not launcher-only", name)
		}
	}
	if err := LauncherOnlyChange([]byte("kind: formula\n"), []byte(rebound)); err == nil {
		t.Error("accepted an invalid previous Formula")
	}
}

func TestLauncherOnlyChangeRefusesAnchors(t *testing.T) {
	anchored := strings.Replace(launcherRecipe, "  command: [/home/op/.forgecell/releases/0.9.1/forgecell, __adapter, claude-code, /bin/claude]", "  command: &cmd [/home/op/.forgecell/releases/0.9.1/forgecell, __adapter, claude-code, /bin/claude]", 1)
	anchored = strings.Replace(anchored, "  analysis: {binding: codex, command: [\"/home/op/.forgecell/releases/0.9.1/forgecell\", \"__adapter\", \"codex\", \"/bin/codex\"]}", "  analysis: {binding: codex, command: *cmd}", 1)
	if _, err := Parse([]byte(anchored)); err != nil {
		t.Fatal(err)
	}
	if err := LauncherOnlyChange([]byte(anchored), []byte(rebind(t, anchored, 6))); err == nil {
		t.Error("accepted a launcher change through an anchor")
	}
}

func TestLauncherOnlyChangeReleasePaths(t *testing.T) {
	for p, want := range map[string]bool{
		oldLauncher:                 true,
		newLauncher:                 true,
		"/releases/1.0.0/forgecell": true,
		"/opt/forgecell/releases/1.0.0-rc.1/forgecell":                true,
		"releases/1.0.0/forgecell":                                    false,
		"/opt/releases/1.0/forgecell":                                 false,
		"/opt/releases/1.0.0/forgecell/":                              false,
		"/opt//releases/1.0.0/forgecell":                              false,
		"/opt/../releases/1.0.0/forgecell":                            false,
		"/opt/releases/1.0.0/bin/forgecell":                           false,
		"/opt/\"x/releases/1.0.0/forgecell":                           false,
		"/opt/x\n/releases/1.0.0/forgecell":                           false,
		"/" + strings.Repeat("a", 4096) + "/releases/1.0.0/forgecell": false,
	} {
		if got := IsReleaseLauncher(p); got != want {
			t.Errorf("IsReleaseLauncher(%q) = %v, want %v", p, got, want)
		}
	}
}
