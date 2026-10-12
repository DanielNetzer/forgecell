package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hardeningManifest(version, protocol string) string {
	return fmt.Sprintf(`{"kind":%q,"version":%q,"sha256":"x","protocolVersion":%q}`, ReleaseKind, version, protocol)
}

func writeManifest(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadManifestDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(real, []byte(hardeningManifest("1.0.0", AdapterProtocolVersion)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if m := ReadManifest(dir); m.Found || m.Version != "unknown" || m.Protocol != "unknown" {
		t.Fatalf("a symlinked manifest was followed: %+v", m)
	}
	if m := ReadManifest(filepath.Dir(real)); m.Found {
		t.Fatalf("a manifest.json-less directory read as a release: %+v", m)
	}
}

func TestReadManifestDecodesStrictly(t *testing.T) {
	valid := hardeningManifest("1.2.3", AdapterProtocolVersion)
	// onDisk is the manifest every release installed before the kind marker carries.
	onDisk := fmt.Sprintf(`{"version":"1.2.3","sha256":"x","protocolVersion":%q}`, AdapterProtocolVersion)
	for name, tc := range map[string]struct {
		content  string
		found    bool
		protocol string
	}{
		"installer manifest":             {valid, true, AdapterProtocolVersion},
		"manifest installed before kind": {onDisk, true, AdapterProtocolVersion},
		"kind absent":                    {fmt.Sprintf(`{"version":"1.2.3","protocolVersion":%q}`, AdapterProtocolVersion), true, AdapterProtocolVersion},
		"protocolVersion absent":         {`{"version":"1.2.3","sha256":"x"}`, true, "unknown"},
		"kind without protocolVersion":   {fmt.Sprintf(`{"kind":%q,"version":"1.2.3","sha256":"x"}`, ReleaseKind), true, "unknown"},
		"trailing whitespace":            {valid + "\n  \n", true, AdapterProtocolVersion},
		"prerelease version":             {strings.Replace(valid, "1.2.3", "1.2.3-rc.1", 1), true, AdapterProtocolVersion},
		"unknown field":                  {strings.TrimSuffix(valid, "}") + `,"extra":true}`, false, ""},
		"unknown nested field":           {strings.TrimSuffix(valid, "}") + `,"tool":{"name":"x"}}`, false, ""},
		"trailing object":                {valid + `{}`, false, ""},
		"trailing garbage":               {valid + ` x`, false, ""},
		"two manifests":                  {valid + valid, false, ""},
		"foreign kind":                   {fmt.Sprintf(`{"kind":"other-tool","version":"1.2.3","protocolVersion":%q}`, AdapterProtocolVersion), false, ""},
		"empty kind":                     {fmt.Sprintf(`{"kind":"","version":"1.2.3","protocolVersion":%q}`, AdapterProtocolVersion), true, AdapterProtocolVersion},
		"invalid protocolVersion":        {fmt.Sprintf(`{"kind":%q,"version":"1.2.3","protocolVersion":"1; rm"}`, ReleaseKind), false, ""},
		"invalid protocolVersion alone":  {`{"version":"1.2.3","protocolVersion":"1; rm"}`, false, ""},
		"non-semver version":             {fmt.Sprintf(`{"kind":%q,"version":"latest","protocolVersion":%q}`, ReleaseKind, AdapterProtocolVersion), false, ""},
		"missing version":                {fmt.Sprintf(`{"kind":%q,"protocolVersion":%q}`, ReleaseKind, AdapterProtocolVersion), false, ""},
		"null":                           {`null`, false, ""},
		"array":                          {`[]`, false, ""},
		"wrong field type":               {fmt.Sprintf(`{"kind":%q,"version":1,"protocolVersion":%q}`, ReleaseKind, AdapterProtocolVersion), false, ""},
		"empty file":                     {``, false, ""},
		"truncated":                      {valid[:len(valid)-1], false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeManifest(t, dir, tc.content)
			m := ReadManifest(dir)
			if m.Found != tc.found {
				t.Fatalf("Found = %t, want %t: %+v", m.Found, tc.found, m)
			}
			if tc.found {
				sum := sha256.Sum256([]byte(tc.content))
				if m.Version == "unknown" || m.Protocol != tc.protocol || m.Digest != hex.EncodeToString(sum[:]) {
					t.Fatalf("%+v", m)
				}
			} else if m.Version != "unknown" || m.Protocol != "unknown" || m.Digest != "" {
				t.Fatalf("a rejected manifest leaked fields: %+v", m)
			}
		})
	}
}

func TestStrayManifestIsNotForgecell(t *testing.T) {
	for name, content := range map[string]string{
		"another tool's fields":     `{"name":"other","version":"1.2.3","license":"MIT"}`,
		"another tool's kind":       `{"kind":"npm-package","version":"1.2.3"}`,
		"foreign kind with fields":  fmt.Sprintf(`{"kind":"other-tool","version":"1.2.3","sha256":"x","protocolVersion":%q}`, AdapterProtocolVersion),
		"forgecell fields plus any": fmt.Sprintf(`{"version":"1.2.3","sha256":"x","protocolVersion":%q,"scripts":{}}`, AdapterProtocolVersion),
	} {
		t.Run(name, func(t *testing.T) {
			launcher, sentinel := fakeBuild(t, "1.2.3", AdapterProtocolVersion)
			writeManifest(t, filepath.Dir(launcher), content)
			m := ReadManifest(filepath.Dir(launcher))
			if m.Found || m.Version != "unknown" || m.Protocol != "unknown" {
				t.Fatalf("a stray manifest.json counted as Forgecell: %+v", m)
			}
			id := ResolveLauncher(launcher)
			if id.Manifest.Found || id.Class != LauncherUnknown || id.Runnable() {
				t.Fatalf("a launcher beside a stray manifest was established: %+v", id)
			}
			if _, err := os.Stat(sentinel); err == nil {
				t.Fatal("the launcher was executed")
			}
		})
	}
	launcher, _ := fakeBuild(t, "1.2.3", AdapterProtocolVersion)
	if id := ResolveLauncher(launcher); !id.Manifest.Found || id.Class != LauncherCompatible {
		t.Fatalf("a launcher beside the manifest the installer wrote must stay established: %+v", id)
	}
	legacy, _ := fakeBuild(t, "1.2.3", AdapterProtocolVersion)
	writeManifest(t, filepath.Dir(legacy), fmt.Sprintf(`{"version":"1.2.3","sha256":"x","protocolVersion":%q}`, AdapterProtocolVersion))
	if id := ResolveLauncher(legacy); !id.Manifest.Found || id.Class != LauncherCompatible {
		t.Fatalf("a launcher beside a release installed before the kind marker must stay established: %+v", id)
	}
}

func TestRelativeLauncherClassifiesUnknown(t *testing.T) {
	launcher, sentinel := fakeBuild(t, "1.0.0", AdapterProtocolVersion)
	dir := filepath.Dir(launcher)
	if id := ResolveLauncher(launcher); id.Class != LauncherCompatible {
		t.Fatalf("the absolute launcher must stay compatible: %+v", id)
	}
	t.Setenv("PATH", dir)
	t.Chdir(dir)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "forgecell"), []byte("#!/bin/sh\ntouch '"+sentinel+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, filepath.Join(dir, "sub"), hardeningManifest("1.0.0", AdapterProtocolVersion))
	for _, saved := range []string{"forgecell", "./forgecell", "sub/forgecell", "../" + filepath.Base(dir) + "/forgecell", ""} {
		id := ResolveLauncher(saved)
		if id.Class != LauncherUnknown || id.Runnable() || id.Path != "" || id.Executable != "" || id.Manifest.Found || id.Managed {
			t.Fatalf("%q resolved: %+v", saved, id)
		}
		message := id.Describe("claude-code")
		for _, want := range []string{"absolute path", "not resolved, read or executed", "forgecell init --lab DIR --harness claude-code"} {
			if !strings.Contains(message, want) {
				t.Fatalf("%q: message omits %q: %s", saved, want, message)
			}
		}
		provider := fake(t, completedProvider)
		providerDir := t.TempDir()
		r := InvokeBinding(context.Background(), []string{saved, "__adapter", "claude-code", provider}, providerDir, 30*time.Second, map[string]any{"kind": "molecule"})
		if r.OK || r.Code != -1 || !strings.Contains(r.Error, "absolute path") {
			t.Fatalf("%q: %+v", saved, r)
		}
		if _, err := os.Stat(filepath.Join(providerDir, "started")); err == nil {
			t.Fatalf("%q: the provider started", saved)
		}
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("a relative launcher was executed")
	}
}

func TestRelativeProviderIsRefusedBeforeExecution(t *testing.T) {
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "started")
	script := "#!/bin/sh\ntouch '" + marker + "'\nprintf '{}'\n"
	for _, name := range []string{"claude", filepath.Join("sub", "claude")} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Chdir(dir)
	for _, provider := range []string{"claude", "./claude", "sub/claude", ""} {
		command := []string{running, "__adapter", "claude-code", provider}
		for name, run := range map[string]func() (int, string, bool){
			"InvokeBinding": func() (int, string, bool) {
				r := InvokeBinding(context.Background(), command, t.TempDir(), 30*time.Second, map[string]any{"kind": "molecule"})
				return r.Code, r.Error, r.OK
			},
			"InvokeReviewBinding": func() (int, string, bool) {
				r := InvokeReviewBinding(context.Background(), command, t.TempDir(), 30*time.Second, map[string]any{"kind": ReviewKind}, nil)
				return r.Code, r.Error, r.OK
			},
		} {
			code, message, ok := run()
			if ok || code != -1 || !strings.Contains(message, "absolute path") || !strings.Contains(message, "claude-code") {
				t.Fatalf("%s with provider %q: code=%d ok=%t %q", name, provider, code, ok, message)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("%s with provider %q started a process", name, provider)
			}
		}
	}
}

func TestFingerprintRejectsRelativeNativeProvider(t *testing.T) {
	provider := fake(t, "exit 0\n")
	launcher, _ := fakeBuild(t, "1.0.0", AdapterProtocolVersion)
	if _, err := Fingerprint([]string{launcher, "__adapter", "codex", provider}); err != nil {
		t.Fatalf("an absolute provider must stay fingerprintable: %v", err)
	}
	t.Setenv("PATH", filepath.Dir(provider))
	t.Chdir(filepath.Dir(provider))
	for _, relative := range []string{filepath.Base(provider), "./" + filepath.Base(provider), ""} {
		if _, err := Fingerprint([]string{launcher, "__adapter", "codex", relative}); err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Fatalf("provider %q: %v", relative, err)
		}
	}
	custom := fake(t, "exit 0\n")
	t.Setenv("PATH", filepath.Dir(custom))
	if _, err := Fingerprint([]string{filepath.Base(custom), "--flag"}); err != nil {
		t.Fatalf("custom commands keep PATH resolution: %v", err)
	}
}

func TestCompatibleLauncherIsNeverExecutedOnLearnAndAnalysisPaths(t *testing.T) {
	for _, kind := range []string{"formula-improvement", "ticket-analysis", ReviewKind} {
		invokers := map[string]func(command []string, dir string) bool{
			"InvokeBinding": func(command []string, dir string) bool {
				return InvokeBinding(context.Background(), command, dir, 30*time.Second, map[string]any{"kind": kind}).OK
			},
		}
		if kind == ReviewKind {
			invokers["InvokeReviewBinding"] = func(command []string, dir string) bool {
				return InvokeReviewBinding(context.Background(), command, dir, 30*time.Second, map[string]any{"kind": kind}, nil).OK
			}
		}
		for name, invoke := range invokers {
			t.Run(kind+"/"+name, func(t *testing.T) {
				marker := filepath.Join(t.TempDir(), "provider-ran")
				provider := fake(t, "touch '"+marker+"'\n"+`if [ "$1" = "--help" ]; then printf '%s' '--safe-mode --tools --strict-mcp-config --json-schema --no-chrome --disable-slash-commands'; exit 0; fi
cat > /dev/null
printf '{}'
`)
				launcher, sentinel := fakeBuild(t, "1.0.0", AdapterProtocolVersion)
				if id := ResolveLauncher(launcher); id.Class != LauncherCompatible || !id.Runnable() {
					t.Fatalf("the fixture launcher is not compatible: %+v", id)
				}
				invoke([]string{launcher, "__adapter", "claude-code", provider}, t.TempDir())
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("the running adapter never served the %s request: %v", kind, err)
				}
				if _, err := os.Stat(sentinel); err == nil {
					t.Fatalf("the saved compatible launcher was executed for %s", kind)
				}
			})
		}
	}
}

func TestRecordProvenanceHashesRunningInode(t *testing.T) {
	sum := func(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
	inode := filepath.Join(t.TempDir(), "inode")
	content := []byte("the inode behind /proc/self/exe")
	if err := os.WriteFile(inode, content, 0600); err != nil {
		t.Fatal(err)
	}
	previous := selfExecutablePath
	t.Cleanup(func() { selfExecutablePath = previous })

	selfExecutablePath = inode
	if p := RecordProvenance([]string{"custom"}); p.ExecutableSHA256 != sum(content) {
		t.Fatalf("hashed %s, want the running inode %s", p.ExecutableSHA256, sum(content))
	}

	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(running)
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(t.TempDir(), "absent"), "directory": t.TempDir()} {
		selfExecutablePath = path
		if p := RecordProvenance([]string{"custom"}); p.ExecutableSHA256 != sum(raw) {
			t.Fatalf("%s: hashed %s, want the os.Executable path %s", name, p.ExecutableSHA256, sum(raw))
		}
	}
}

func TestAttemptResolvesItsLauncherExactlyOnce(t *testing.T) {
	provider := fake(t, completedProvider)
	launcher, sentinel := fakeBuild(t, "1.0.0", AdapterProtocolVersion)
	command := []string{launcher, "__adapter", "claude-code", provider}
	calls := 0
	previous := launcherResolver
	launcherResolver = func(saved string) LauncherIdentity { calls++; return ResolveLauncher(saved) }
	t.Cleanup(func() { launcherResolver = previous })

	id := ResolveAttempt(command)
	if id == nil || id.Class != LauncherCompatible || calls != 1 {
		t.Fatalf("the attempt did not resolve its launcher once: %+v calls=%d", id, calls)
	}
	p := RecordProvenanceWith(command, id)
	if err := os.RemoveAll(filepath.Dir(launcher)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r := InvokeBindingWith(context.Background(), id, command, dir, 30*time.Second, map[string]any{"kind": "molecule"})
	if calls != 1 {
		t.Fatalf("the launcher was resolved %d times in one attempt", calls)
	}
	if p.LauncherClass != id.Class || !r.OK {
		t.Fatalf("the attempt recorded class %q but was gated on %q: %+v", p.LauncherClass, id.Class, r)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("the launcher was executed")
	}

	if got := ResolveAttempt([]string{"custom-tool", "--flag"}); got != nil || calls != 1 {
		t.Fatalf("a custom command has no launcher to resolve: %+v calls=%d", got, calls)
	}
	InvokeBinding(context.Background(), command, t.TempDir(), 30*time.Second, map[string]any{"kind": "molecule"})
	if calls != 2 {
		t.Fatalf("an unresolved invocation resolves exactly once itself, got %d", calls-1)
	}
}

func TestExecutableMatchesManifestOnlyWithItsOwnDigest(t *testing.T) {
	build := func(t *testing.T, manifest func(sha string) string) LauncherIdentity {
		t.Helper()
		dir := t.TempDir()
		launcher := filepath.Join(dir, "forgecell")
		content := []byte("#!/bin/sh\nexit 0\n")
		if err := os.WriteFile(launcher, content, 0700); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		writeManifest(t, dir, manifest(hex.EncodeToString(sum[:])))
		return ResolveLauncher(launcher)
	}
	versioned := func(extra string) func(string) string {
		return func(sha string) string {
			return fmt.Sprintf(`{"version":"1.2.3",%s"protocolVersion":%q}`, extra, AdapterProtocolVersion)
		}
	}
	for name, tc := range map[string]struct {
		manifest func(sha string) string
		match    bool
	}{
		"real shaped, matching digest": {func(sha string) string { return hardeningManifestWithSHA("1.2.3", sha) }, true},
		"legacy shaped, matching":      {func(sha string) string { return fmt.Sprintf(`{"version":"1.2.3","sha256":%q}`, sha) }, true},
		"bare version":                 {func(string) string { return `{"version":"1.2.3"}` }, false},
		"digest of another file":       {func(string) string { return hardeningManifestWithSHA("1.2.3", strings.Repeat("a", 64)) }, false},
		"placeholder digest":           {versioned(`"sha256":"x",`), false},
		"no digest":                    {versioned(""), false},
	} {
		t.Run(name, func(t *testing.T) {
			id := build(t, tc.manifest)
			if !id.Manifest.Found {
				t.Fatalf("the fixture manifest must read as a release: %+v", id.Manifest)
			}
			if got := id.ExecutableMatchesManifest(); got != tc.match {
				t.Fatalf("ExecutableMatchesManifest = %t, want %t: %+v", got, tc.match, id)
			}
		})
	}
	if (LauncherIdentity{Manifest: Manifest{Found: true, SHA256: "unknown"}}).ExecutableMatchesManifest() {
		t.Fatal("an identity with no executable matched")
	}
	if id := ResolveLauncher(filepath.Join(t.TempDir(), "missing")); id.ExecutableMatchesManifest() {
		t.Fatal("a missing launcher matched")
	}
}

func hardeningManifestWithSHA(version, sha string) string {
	return fmt.Sprintf(`{"kind":%q,"version":%q,"sha256":%q,"protocolVersion":%q}`, ReleaseKind, version, sha, AdapterProtocolVersion)
}

func TestProvenanceReusesAttemptIdentity(t *testing.T) {
	provider := fake(t, completedProvider)
	launcher, sentinel := fakeBuild(t, "1.0.0", AdapterProtocolVersion)
	command := []string{launcher, "__adapter", "claude-code", provider}
	id := ResolveLauncher(launcher)
	if id.Class != LauncherCompatible {
		t.Fatalf("%+v", id)
	}
	if err := os.RemoveAll(filepath.Dir(launcher)); err != nil {
		t.Fatal(err)
	}

	if p := RecordProvenanceWith(command, &id); p.LauncherClass != LauncherCompatible || p.SavedLauncher != launcher {
		t.Fatalf("the recorded class must be the attempt's resolved class: %+v", p)
	}
	if p := RecordProvenance(command); p.LauncherClass != LauncherMissing {
		t.Fatalf("without an identity the launcher is resolved afresh: %+v", p)
	}
	other := filepath.Join(t.TempDir(), "forgecell")
	if p := RecordProvenanceWith([]string{other, "__adapter", "claude-code", provider}, &id); p.LauncherClass != LauncherMissing {
		t.Fatalf("an identity of another launcher was reused: %+v", p)
	}
	if p := RecordProvenanceWith([]string{"custom"}, &id); p.LauncherClass != "custom" || p.SavedLauncher != "" {
		t.Fatalf("%+v", p)
	}

	dir := t.TempDir()
	r := InvokeBindingWith(context.Background(), &id, command, dir, 30*time.Second, map[string]any{"kind": "molecule"})
	if !r.OK {
		t.Fatalf("the attempt's identity must gate the run, not a second resolution: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "started")); err != nil {
		t.Fatal("the provider did not run")
	}
	if r = InvokeBinding(context.Background(), command, t.TempDir(), 30*time.Second, map[string]any{"kind": "molecule"}); r.OK || !strings.Contains(r.Error, "was not found") {
		t.Fatalf("without an identity the removed launcher blocks: %+v", r)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("the launcher was executed")
	}
}
