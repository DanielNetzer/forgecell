package cli

import (
	"context"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/install"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installRelease(t *testing.T, home, bin, version string) {
	t.Helper()
	source := writeTool(t, t.TempDir(), "forgecell", "#!/bin/sh\nprintf '%s\\n' '"+version+"'\n")
	if _, err := install.Install(context.Background(), install.Options{Home: home, Bin: bin, Source: source, Version: version}); err != nil {
		t.Fatal(err)
	}
}

func savedBindingOf(t *testing.T, lab string) map[string]any {
	t.Helper()
	_, out := runDoctor(t, lab, "--json")
	report := decodeDoctor(t, out)
	saved, ok := report["savedBinding"].(map[string]any)
	if !ok {
		t.Fatalf("no saved binding: %s", out)
	}
	return saved
}

func bindingStates(saved map[string]any) []map[string]any {
	var states []map[string]any
	for _, s := range saved["binding"].(map[string]any)["states"].([]any) {
		states = append(states, s.(map[string]any))
	}
	return states
}

func TestDoctorTreatsADifferentBuildWithTheSameProtocolAsCompatible(t *testing.T) {
	_, lab := doctorFixture(t)
	provider := writeTool(t, t.TempDir(), "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	launcherDir := t.TempDir()
	sentinel := filepath.Join(launcherDir, "executed")
	launcher := writeTool(t, launcherDir, "forgecell", "#!/bin/sh\ntouch '"+sentinel+"'\n")
	os.WriteFile(filepath.Join(launcherDir, "manifest.json"), []byte(`{"version":"0.2.0","sha256":"x","protocolVersion":"`+harness.AdapterProtocolVersion+`"}`), 0600)
	saveBinding(t, lab, "codex", []string{launcher, "__adapter", "codex", provider})
	saved := savedBindingOf(t, lab)
	states := bindingStates(saved)
	if saved["readiness"] != "ready" || len(states) != 1 || states[0]["state"] != "launcher-compatible" || states[0]["path"] != launcher {
		t.Fatalf("a compatible different build must not block: %v", saved)
	}
	diagnostics := saved["binding"].(map[string]any)
	if diagnostics["launcherVersion"] != "0.2.0" || diagnostics["launcherProtocol"] != harness.AdapterProtocolVersion || diagnostics["runningVersion"] != "0.3.0" || diagnostics["runningProtocol"] != harness.AdapterProtocolVersion {
		t.Fatalf("%v", diagnostics)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("saved launcher was executed")
	}
}

func TestDoctorBlocksAnIncompatibleProtocolAndOffersTheReviewedRebind(t *testing.T) {
	_, lab := doctorFixture(t)
	provider := writeTool(t, t.TempDir(), "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	t.Setenv("PATH", filepath.Dir(provider))
	launcherDir := t.TempDir()
	launcher := writeTool(t, launcherDir, "forgecell", "#!/bin/sh\nexit 0\n")
	os.WriteFile(filepath.Join(launcherDir, "manifest.json"), []byte(`{"version":"0.2.0","sha256":"x","protocolVersion":"7"}`), 0600)
	saveBinding(t, lab, "codex", []string{launcher, "__adapter", "codex", provider})
	saved := savedBindingOf(t, lab)
	states := bindingStates(saved)
	if saved["readiness"] != "blocked" || len(states) != 1 || states[0]["state"] != "launcher-incompatible" {
		t.Fatalf("%v", saved)
	}
	for _, want := range []string{launcher, "version 0.2.0", "adapter protocol 7", "adapter protocol " + harness.AdapterProtocolVersion, "version 0.3.0"} {
		if !strings.Contains(states[0]["detail"].(string), want) {
			t.Fatalf("detail omits %q: %v", want, states[0]["detail"])
		}
	}
	want := fmt.Sprintf("forgecell init --lab %q --harness codex", lab)
	if saved["nextCommand"] != want {
		t.Fatalf("next command %v, want the reviewed rebind %s", saved["nextCommand"], want)
	}
	_, human := runDoctor(t, lab, "--verbose")
	for _, want := range []string{"launcher-incompatible", "Launcher (saved): " + launcher, "adapter protocol 7", "adapter protocol " + harness.AdapterProtocolVersion} {
		if !strings.Contains(human, want) {
			t.Fatalf("human view omits %q:\n%s", want, human)
		}
	}
}

func TestDoctorReportsAMissingLauncherSeparately(t *testing.T) {
	_, lab := doctorFixture(t)
	provider := writeTool(t, t.TempDir(), "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	t.Setenv("PATH", filepath.Dir(provider))
	launcher := filepath.Join(t.TempDir(), "gone", "forgecell")
	saveBinding(t, lab, "codex", []string{launcher, "__adapter", "codex", provider})
	states := bindingStates(savedBindingOf(t, lab))
	if len(states) != 1 || states[0]["state"] != "launcher-missing" || !strings.Contains(states[0]["detail"].(string), launcher) || strings.Contains(states[0]["detail"].(string), "different") {
		t.Fatalf("%v", states)
	}
}

func TestNewBindingsUseTheManagedLauncherAndSurviveAnUpgrade(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	installRelease(t, home, bin, "0.2.0")
	t.Setenv("FORGECELL_BIN", bin)
	t.Setenv("FORGECELL_LAUNCHER", filepath.Join(home, "current", "forgecell"))
	managed := initLauncher()
	if managed != filepath.Join(bin, "forgecell") {
		t.Fatalf("new bindings must name the managed launcher, got %q", managed)
	}
	_, lab := doctorFixture(t)
	provider := writeTool(t, t.TempDir(), "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	t.Setenv("PATH", filepath.Dir(provider))
	saveBinding(t, lab, "codex", []string{managed, "__adapter", "codex", provider})
	for _, version := range []string{"0.2.0", "0.2.1"} {
		if version != "0.2.0" {
			installRelease(t, home, bin, version)
		}
		saved := savedBindingOf(t, lab)
		states := bindingStates(saved)
		diagnostics := saved["binding"].(map[string]any)
		if saved["readiness"] != "ready" || len(states) != 1 || states[0]["state"] != "launcher-compatible" || diagnostics["launcherVersion"] != version || diagnostics["launcherProtocol"] != harness.AdapterProtocolVersion {
			t.Fatalf("binding blocked after installing %s: %v", version, saved)
		}
	}
}

func TestInitLauncherFallsBackWhenNoManagedLauncherIsNamed(t *testing.T) {
	t.Setenv("FORGECELL_BIN", "")
	t.Setenv("FORGECELL_LAUNCHER", "/installed/current/forgecell")
	if got := initLauncher(); got != "/installed/current/forgecell" {
		t.Fatalf("%q", got)
	}
	notManaged := t.TempDir()
	writeTool(t, notManaged, "forgecell", "#!/bin/sh\nexit 0\n")
	t.Setenv("FORGECELL_BIN", notManaged)
	if got := initLauncher(); got != "/installed/current/forgecell" {
		t.Fatalf("an arbitrary FORGECELL_BIN command was bound: %q", got)
	}
	t.Setenv("FORGECELL_LAUNCHER", "")
	if got := initLauncher(); got != "" {
		t.Fatalf("%q", got)
	}
}
