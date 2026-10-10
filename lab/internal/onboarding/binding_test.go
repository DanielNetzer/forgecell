package onboarding

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixtureProvider is an executable file; probes use a fake Runner so it never runs.
func fixtureProvider(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// foreignLauncher records any execution in a sentinel file, which must stay absent.
func foreignLauncher(t *testing.T) (launcher, sentinel string) {
	t.Helper()
	dir := t.TempDir()
	sentinel = filepath.Join(dir, "executed")
	launcher = filepath.Join(dir, "forgecell")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\ntouch '"+sentinel+"'\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return launcher, sentinel
}

func recordingRunner(calls *[][]string) Runner {
	inner := healthyRunner(nil)
	return func(ctx context.Context, o process.Options) process.Result {
		*calls = append(*calls, append([]string(nil), o.Argv...))
		return inner(ctx, o)
	}
}

func stateNames(t *testing.T, c Candidate) []string {
	t.Helper()
	if c.Binding == nil {
		t.Fatalf("no binding diagnostics: %+v", c)
	}
	var names []string
	for _, s := range c.Binding.States {
		names = append(names, s.State)
	}
	return names
}

func TestBoundReadiness(t *testing.T) {
	self, _ := os.Executable()
	for _, tc := range []struct {
		name    string
		command func(provider string) []string
		status  string
		called  bool
	}{
		{"saved missing launcher", func(p string) []string { return []string{"/missing/forgecell", "__adapter", "codex", p} }, "blocked", true},
		{"custom never executed", func(string) []string { return []string{"arbitrary-script", "--do-work"} }, "unknown", false},
		{"provider mismatch", func(p string) []string { return []string{self, "__adapter", "cursor", p} }, "blocked", false},
		{"saved provider checked", func(p string) []string { return []string{self, "__adapter", "codex", p} }, "ready", true},
		{"unbound", func(string) []string { return nil }, "blocked", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := fixtureProvider(t)
			var calls [][]string
			c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: tc.command(provider)}, t.TempDir(), recordingRunner(&calls))
			if c.Readiness != tc.status || (len(calls) > 0) != tc.called {
				t.Fatalf("%+v calls=%v", c, calls)
			}
			for _, argv := range calls {
				if argv[0] != provider {
					t.Fatalf("checked %s instead of the saved provider %s", argv[0], provider)
				}
			}
		})
	}
}

func TestLauncherMissingAndProviderMissingAreReportedSeparatelyWithPaths(t *testing.T) {
	var calls [][]string
	command := []string{"/missing/old-forgecell", "__adapter", "codex", "/missing/saved-codex"}
	c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: command}, t.TempDir(), recordingRunner(&calls))
	if c.Readiness != "blocked" || c.Executable != "" || len(calls) != 0 || !reflect.DeepEqual(stateNames(t, c), []string{"launcher-missing", "provider-missing"}) {
		t.Fatalf("%+v calls=%v", c, calls)
	}
	if c.Binding.States[0].Path != "/missing/old-forgecell" || c.Binding.States[1].Path != "/missing/saved-codex" {
		t.Fatalf("%+v", c.Binding.States)
	}
	for _, path := range []string{"/missing/old-forgecell", "/missing/saved-codex"} {
		if !strings.Contains(c.Detail, path) {
			t.Fatalf("detail omits %s: %q", path, c.Detail)
		}
	}
	if strings.Contains(c.Detail, "missing or belongs to a different") {
		t.Fatalf("blended message remains: %q", c.Detail)
	}
}

func TestProviderMissingWhenBoundPathIsNotAnExecutableFile(t *testing.T) {
	self, _ := os.Executable()
	dir := t.TempDir()
	plain := filepath.Join(dir, "codex-plain")
	os.WriteFile(plain, []byte("not executable"), 0600)
	for name, provider := range map[string]string{"absent": filepath.Join(dir, "absent"), "directory": dir, "not executable": plain} {
		t.Run(name, func(t *testing.T) {
			var calls [][]string
			c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: []string{self, "__adapter", "codex", provider}}, t.TempDir(), recordingRunner(&calls))
			if c.Readiness != "blocked" || len(calls) != 0 || !reflect.DeepEqual(stateNames(t, c), []string{"provider-missing"}) || c.Binding.States[0].Path != provider || c.Executable != "" {
				t.Fatalf("%+v calls=%v", c, calls)
			}
		})
	}
}

func TestForeignLauncherIsReportedWithManifestVersionAndNeverExecuted(t *testing.T) {
	self, _ := os.Executable()
	provider := fixtureProvider(t)
	launcher, sentinel := foreignLauncher(t)
	os.WriteFile(filepath.Join(filepath.Dir(launcher), "manifest.json"), []byte(`{"version":"0.2.0-preview.1","sha256":"abc"}`), 0600)
	var calls [][]string
	c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: []string{launcher, "__adapter", "codex", provider}}, t.TempDir(), recordingRunner(&calls))
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("foreign launcher was executed")
	}
	d := c.Binding
	if !reflect.DeepEqual(stateNames(t, c), []string{"launcher-different"}) || d.Launcher != launcher || d.RunningLauncher != self || d.LauncherVersion != "0.2.0-preview.1" || d.LauncherVersionSource != "manifest.json" || d.Provider != provider || d.ResolvedProvider != provider {
		t.Fatalf("%+v", d)
	}
	if c.Binding.States[0].Path != launcher || !strings.Contains(c.Detail, launcher) || !strings.Contains(c.Detail, self) {
		t.Fatalf("%+v detail=%q", c.Binding.States, c.Detail)
	}
	if c.Readiness != "blocked" || c.Executable != provider || c.Installed.State != "verified" || c.Authentication.State != "verified" || c.Capabilities.AnalysisControls.State != "supported" || len(c.Steps) == 0 {
		t.Fatalf("provider evidence must come from the bound provider probe while readiness stays blocked: %+v", c)
	}
	if c.WorkflowCapability().State != "blocked" {
		t.Fatalf("a foreign launcher must keep the workflow blocked: %+v", c.WorkflowCapability())
	}
	allowed := map[string]bool{"--version": true, "--help": true, "exec --help": true, "login status": true, "--disable plugins --disable apps mcp list --json": true}
	if len(calls) == 0 {
		t.Fatal("provider was not probed at its bound path")
	}
	for _, argv := range calls {
		if argv[0] != provider || argv[0] == launcher || !allowed[strings.Join(argv[1:], " ")] {
			t.Fatalf("unexpected provider call %v", argv)
		}
	}
}

func TestMissingLauncherVersionFromAdjacentManifest(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":"1.2.3"}`), 0600)
	launcher := filepath.Join(dir, "forgecell")
	c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: []string{launcher, "__adapter", "codex", fixtureProvider(t)}}, t.TempDir(), healthyRunner(nil))
	if !reflect.DeepEqual(stateNames(t, c), []string{"launcher-missing"}) || c.Binding.LauncherVersion != "1.2.3" || c.Binding.LauncherVersionSource != "manifest.json" {
		t.Fatalf("%+v", c.Binding)
	}
}

func TestLauncherVersionIsUnknownUnlessManifestIsValid(t *testing.T) {
	for name, write := range map[string]func(dir string){
		"absent":    func(string) {},
		"malformed": func(dir string) { os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{`), 0600) },
		"not a version": func(dir string) {
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":"latest; rm"}`), 0600)
		},
		"missing field": func(dir string) { os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"sha256":"x"}`), 0600) },
		"oversized": func(dir string) {
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":"1.2.3"`+strings.Repeat(" ", 8192)+`}`), 0600)
		},
		"directory": func(dir string) { os.Mkdir(filepath.Join(dir, "manifest.json"), 0700) },
		"symlink": func(dir string) {
			target := filepath.Join(t.TempDir(), "real.json")
			os.WriteFile(target, []byte(`{"version":"1.2.3"}`), 0600)
			os.Symlink(target, filepath.Join(dir, "manifest.json"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			launcher, _ := foreignLauncher(t)
			write(filepath.Dir(launcher))
			c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: []string{launcher, "__adapter", "codex", fixtureProvider(t)}}, t.TempDir(), healthyRunner(nil))
			if c.Binding.LauncherVersion != "unknown" || c.Binding.LauncherVersionSource != "unavailable" {
				t.Fatalf("%+v", c.Binding)
			}
		})
	}
}

func TestAdapterMismatchReportsIdentitiesAndPathsWithoutProbingProvider(t *testing.T) {
	self, _ := os.Executable()
	provider := fixtureProvider(t)
	var calls [][]string
	c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: []string{self, "__adapter", "cursor", provider}}, t.TempDir(), recordingRunner(&calls))
	d := c.Binding
	if c.Readiness != "blocked" || len(calls) != 0 || !reflect.DeepEqual(stateNames(t, c), []string{"adapter-mismatch"}) || d.FormulaBinding != "codex" || d.Adapter != "cursor" || d.Launcher != self || d.Provider != provider || c.Executable != "" {
		t.Fatalf("%+v calls=%v", c, calls)
	}
	for _, want := range []string{"codex", "cursor", self, provider} {
		if !strings.Contains(c.Detail, want) {
			t.Fatalf("detail omits %s: %q", want, c.Detail)
		}
	}
}

func TestProviderBoundOutsidePathIsProbedAtItsBoundPath(t *testing.T) {
	self, _ := os.Executable()
	provider := filepath.Join(t.TempDir(), "Contents", "Resources", "codex")
	os.MkdirAll(filepath.Dir(provider), 0700)
	os.WriteFile(provider, []byte("#!/bin/sh\nexit 99\n"), 0700)
	t.Setenv("PATH", t.TempDir())
	var calls [][]string
	c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: []string{self, "__adapter", "codex", provider}}, t.TempDir(), recordingRunner(&calls))
	if c.Readiness != "ready" || c.Executable != provider || len(c.Binding.States) != 0 || len(calls) == 0 {
		t.Fatalf("%+v calls=%v", c, calls)
	}
	for _, argv := range calls {
		if argv[0] != provider {
			t.Fatalf("probed %s instead of the bound provider", argv[0])
		}
		for _, arg := range argv[1:] {
			if arg == "--print" || arg == "-p" {
				t.Fatalf("model invocation: %v", argv)
			}
		}
	}
}

func TestRunningVersionLabelsTheLauncherVersionSource(t *testing.T) {
	self, _ := os.Executable()
	c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: []string{self, "__adapter", "codex", fixtureProvider(t)}}, t.TempDir(), healthyRunner(nil))
	c.Binding.WithRunningVersion("0.3.0")
	if c.Binding.RunningVersion != "0.3.0" || c.Binding.LauncherVersion != "0.3.0" || c.Binding.LauncherVersionSource != "running executable" {
		t.Fatalf("%+v", c.Binding)
	}
	launcher, _ := foreignLauncher(t)
	c = ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: []string{launcher, "__adapter", "codex", fixtureProvider(t)}}, t.TempDir(), healthyRunner(nil))
	c.Binding.WithRunningVersion("0.3.0")
	if c.Binding.RunningVersion != "0.3.0" || c.Binding.LauncherVersion != "unknown" {
		t.Fatalf("a different launcher must not inherit the running version: %+v", c.Binding)
	}
}

func TestCustomCapabilitiesPreserveCommand(t *testing.T) {
	c := ProbeBinding(context.Background(), formula.Binding{Binding: "custom", Command: []string{"untrusted-command"}}, t.TempDir(), func(context.Context, process.Options) process.Result {
		t.Fatal("custom command executed")
		return process.Result{}
	})
	if c.Capabilities.Coding.State != "unknown" || c.Capabilities.Meta.State != "unknown" || c.Capabilities.Analysis.State != "unsupported" || c.WorkflowCapability().State != "unsupported" || c.Binding != nil {
		t.Fatalf("%+v", c)
	}
}
