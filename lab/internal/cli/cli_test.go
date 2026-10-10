package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/evaluation"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/learning"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCLIHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--version"}} {
		var out, stderr bytes.Buffer
		code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "test-version")
		if code != 0 || out.Len() == 0 {
			t.Fatalf("code %d: %s", code, stderr.String())
		}
	}
}
func TestLedgerInspectionIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ledgers"), 0700)
	raw := []byte(`{"schemaVersion":"v0","kind":"molecule","id":"mol-test","formulaId":"f","status":"waiting","extra":"preserve"}`)
	file := filepath.Join(dir, "ledgers", "mol-test.json")
	os.WriteFile(file, raw, 0600)
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"ledger", "mol-test", "--lab", dir}, strings.NewReader(""), &out, &stderr, "dev")
	if code != 0 || !strings.Contains(out.String(), "preserve") {
		t.Fatalf("%d %s %s", code, out.String(), stderr.String())
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(after, raw) {
		t.Fatal("reading rewrote ledger")
	}
	if code = Run(context.Background(), []string{"ledger", "../escape", "--lab", dir}, strings.NewReader(""), &out, &stderr, "dev"); code == 0 {
		t.Fatal("unsafe id accepted")
	}
}

func TestDoctorPreservesSavedBinding(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"git", "gh", "codex", "claude", "cursor-agent"} {
		body := "#!/bin/sh\nprintf '%s\\n' 'Codex Claude Code Cursor Agent logged in --sandbox --output-last-message --output-schema --ephemeral --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --strict-mcp-config --mcp-config --allowedTools --disallowedTools --mode --trust'\n"
		if name == "claude" {
			body = "#!/bin/sh\ncase \"$1\" in auth) printf '%s\\n' '{\"loggedIn\":true}' ;; *) printf '%s\\n' 'Claude Code --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --strict-mcp-config --mcp-config --allowedTools --disallowedTools' ;; esac\n"
		}
		if e := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); e != nil {
			t.Fatal(e)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CURSOR_AGENT", "")
	t.Setenv("TERM_PROGRAM", "")
	lab := t.TempDir()
	os.MkdirAll(filepath.Join(lab, "formulas"), 0700)
	os.WriteFile(filepath.Join(lab, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"f"}`), 0600)
	recipe := []byte("kind: formula\nid: f\nharness:\n  binding: codex\n  command: [/missing/old-forgecell, __adapter, codex, /missing/saved-codex]\natoms:\n  - type: harness\n")
	file := filepath.Join(lab, "formulas/f.yaml")
	os.WriteFile(file, recipe, 0600)
	for _, extra := range [][]string{nil, {"--harness", "claude-code"}} {
		var out, err bytes.Buffer
		args := append([]string{"doctor", "--lab", lab, "--json"}, extra...)
		code := Run(context.Background(), args, strings.NewReader(""), &out, &err, "dev")
		if code != 2 || !strings.Contains(out.String(), `"savedBinding":{"id":"codex","readiness":"blocked"`) {
			t.Fatalf("%d %s %s", code, out.String(), err.String())
		}
		var report map[string]json.RawMessage
		if e := json.Unmarshal(out.Bytes(), &report); e != nil || len(report["metaBinding"]) == 0 || len(report["metaCandidates"]) == 0 || string(report["schemaVersion"]) == "" {
			t.Fatalf("doctor JSON lost meta reporting or schemaVersion: %s %v", out.String(), e)
		}
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(after, recipe) {
		t.Fatal("doctor changed Formula")
	}
}

const (
	codexTool = `#!/bin/sh
echo "$*" >> "$0.calls"
case "$*" in
"--version") echo "codex-cli 1.0" ;;
"--help"|"exec --help") echo "Codex -c --disable --color --strict-config --skip-git-repo-check --ignore-rules --sandbox --output-last-message --output-schema --ephemeral" ;;
"login status") echo "LOGIN" ;;
"--disable plugins --disable apps mcp list --json") echo "[]" ;;
*) exit 64 ;;
esac
`
	claudeTool = `#!/bin/sh
case "$*" in
"--version") echo "2.1.0 (Claude Code)" ;;
"--help") echo "Claude Code --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --strict-mcp-config --mcp-config --allowedTools --disallowedTools --safe-mode --strict-mcp-config --no-chrome --disable-slash-commands" ;;
"auth status --json") echo '{"loggedIn":LOGGED}' ;;
*) exit 64 ;;
esac
`
	cursorTool = `#!/bin/sh
case "$*" in
"--version") echo "cursor-agent 2026.09.26" ;;
"--help") echo "Cursor Agent --print --output-format HELPFLAGS" ;;
"status --format json") echo '{"isAuthenticated":LOGGED}' ;;
*) exit 64 ;;
esac
`
)

func writeTool(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// doctorFixture isolates the process: an empty working directory, git and gh
// fakes on PATH, no harness environment signals, and a fresh Lab directory.
func doctorFixture(t *testing.T) (bin, lab string) {
	t.Helper()
	t.Chdir(t.TempDir())
	bin = t.TempDir()
	writeTool(t, bin, "git", "#!/bin/sh\nexit 0\n")
	writeTool(t, bin, "gh", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", bin)
	for _, key := range []string{"CLAUDECODE", "CODEX_THREAD_ID", "CURSOR_AGENT", "TERM_PROGRAM", "FORGECELL_LAB"} {
		t.Setenv(key, "")
	}
	return bin, t.TempDir()
}

func saveBinding(t *testing.T, lab, binding string, command []string) {
	t.Helper()
	os.MkdirAll(filepath.Join(lab, "formulas"), 0700)
	os.WriteFile(filepath.Join(lab, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"f"}`), 0600)
	recipe := "kind: formula\nid: f\nharness:\n  binding: " + binding + "\n  command: [" + strings.Join(command, ", ") + "]\natoms:\n  - type: harness\n"
	if err := os.WriteFile(filepath.Join(lab, "formulas", "f.yaml"), []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
}

func runDoctor(t *testing.T, lab string, extra ...string) (int, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	args := append([]string{"doctor", "--lab", lab}, extra...)
	code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "0.3.0")
	if code == 1 {
		t.Fatalf("doctor errored: %s", stderr.String())
	}
	return code, out.String()
}

func decodeDoctor(t *testing.T, out string) map[string]any {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return report
}

func collectKey(value any, key string, found *[]string) {
	switch v := value.(type) {
	case map[string]any:
		for k, child := range v {
			if s, ok := child.(string); ok && k == key {
				*found = append(*found, s)
			}
			collectKey(child, key, found)
		}
	case []any:
		for _, child := range v {
			collectKey(child, key, found)
		}
	}
}

func without(value any, key string) any {
	switch v := value.(type) {
	case map[string]any:
		copied := map[string]any{}
		for k, child := range v {
			if k != key {
				copied[k] = without(child, key)
			}
		}
		return copied
	case []any:
		copied := make([]any, len(v))
		for i, child := range v {
			copied[i] = without(child, key)
		}
		return copied
	}
	return value
}

func candidateByID(t *testing.T, report map[string]any, id string) map[string]any {
	t.Helper()
	for _, c := range report["candidates"].([]any) {
		if c.(map[string]any)["id"] == id {
			return c.(map[string]any)
		}
	}
	t.Fatalf("no candidate %s", id)
	return nil
}

func assertOnlyExistingCommands(t *testing.T, report map[string]any) {
	t.Helper()
	var commands []string
	collectKey(report, "nextCommand", &commands)
	for _, command := range commands {
		for _, forbidden := range []string{"--rebind", "update", "handoff"} {
			if strings.Contains(command, forbidden) {
				t.Fatalf("command uses unreleased %q: %s", forbidden, command)
			}
		}
		if !strings.Contains(command, "forgecell init") && !strings.Contains(command, "forgecell doctor") && command != "codex login" && command != "claude auth login" && command != "agent login" {
			t.Fatalf("unexpected command: %s", command)
		}
	}
}

func TestDoctorJSONIsVersionedAdditiveAndHidesStepsByDefault(t *testing.T) {
	bin, lab := doctorFixture(t)
	writeTool(t, bin, "cursor-agent", strings.NewReplacer("HELPFLAGS", "--mode", "LOGGED", "true").Replace(cursorTool))
	var reports []map[string]any
	for _, extra := range [][]string{nil, {"--json"}} {
		code, out := runDoctor(t, lab, extra...)
		report := decodeDoctor(t, out)
		reports = append(reports, report)
		if code != 2 || report["schemaVersion"] != "v1" || strings.Contains(out, `"steps"`) {
			t.Fatalf("%d %s", code, out)
		}
		for _, key := range []string{"activeFormulaId", "labDir", "legacyLabDir", "savedBinding", "learnBound", "workflow", "gitReady", "githubAuthenticated", "candidates", "selection", "note"} {
			if _, ok := report[key]; !ok {
				t.Fatalf("pre-existing key %s was removed: %s", key, out)
			}
		}
		for _, c := range report["candidates"].([]any) {
			for _, key := range []string{"id", "readiness", "detail", "project", "capabilities", "installed", "authentication"} {
				if _, ok := c.(map[string]any)[key]; !ok {
					t.Fatalf("pre-existing candidate key %s was removed: %v", key, c)
				}
			}
		}
		cursor := candidateByID(t, report, "cursor")
		failures := cursor["failures"].([]any)
		if cursor["readiness"] != "unsupported" || len(failures) != 2 || failures[0].(map[string]any)["step"] != "required-flag:--sandbox" || failures[1].(map[string]any)["step"] != "required-flag:--trust" {
			t.Fatalf("cursor failure is not named precisely: %v", cursor)
		}
		assertOnlyExistingCommands(t, report)
	}
	if !reflect.DeepEqual(reports[0], reports[1]) {
		t.Fatal("--json changes the default output")
	}
	_, verbose := runDoctor(t, lab, "--verbose", "--json")
	detailed := decodeDoctor(t, verbose)
	if !strings.Contains(verbose, `"steps"`) || !reflect.DeepEqual(without(detailed, "steps"), without(reports[0], "steps")) {
		t.Fatalf("--verbose --json must add only steps: %s", verbose)
	}
	steps := candidateByID(t, detailed, "cursor")["steps"].([]any)
	if len(steps) < 5 || steps[0].(map[string]any)["step"] != "version" {
		t.Fatalf("steps: %v", steps)
	}
}

func TestDoctorVerboseHumanViewNamesStepsPathsVersionsAndNextCommand(t *testing.T) {
	bin, lab := doctorFixture(t)
	writeTool(t, bin, "cursor-agent", strings.NewReplacer("HELPFLAGS", "--mode", "LOGGED", "true").Replace(cursorTool))
	provider := writeTool(t, t.TempDir(), "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	launcher := writeTool(t, t.TempDir(), "forgecell", "#!/bin/sh\nexit 0\n")
	saveBinding(t, lab, "codex", []string{launcher, "__adapter", "codex", provider})
	code, out := runDoctor(t, lab, "--verbose")
	if code != 2 || json.Valid([]byte(out)) {
		t.Fatalf("verbose without --json must print a human view: %d %s", code, out)
	}
	for _, want := range []string{
		"Saved binding: codex — blocked", "launcher-different", launcher, provider, "0.3.0", "unknown",
		"Step version: passed", "Step required-flag:--sandbox: failed", "Step required-flag:--trust: failed",
		"Candidate: cursor — unsupported", "required-flag:--sandbox", "Next: ", "forgecell init --lab",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorProbesCodexBoundOutsidePathAtItsBoundPathAndNeverRunsTheForeignLauncher(t *testing.T) {
	for _, onPath := range []bool{false, true} {
		t.Run(fmt.Sprintf("on PATH %t", onPath), func(t *testing.T) {
			bin, lab := doctorFixture(t)
			provider := writeTool(t, filepath.Join(t.TempDir(), "Contents", "Resources"), "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
			if onPath {
				t.Setenv("PATH", bin+string(os.PathListSeparator)+filepath.Dir(provider))
			}
			launcherDir := t.TempDir()
			sentinel := filepath.Join(launcherDir, "executed")
			launcher := writeTool(t, launcherDir, "forgecell", "#!/bin/sh\ntouch '"+sentinel+"'\n")
			os.WriteFile(filepath.Join(launcherDir, "manifest.json"), []byte(`{"version":"0.2.0-preview.1","sha256":"x"}`), 0600)
			saveBinding(t, lab, "codex", []string{launcher, "__adapter", "codex", provider})
			code, out := runDoctor(t, lab, "--verbose", "--json")
			report := decodeDoctor(t, out)
			saved := report["savedBinding"].(map[string]any)
			if code != 2 || saved["executable"] != provider || saved["readiness"] != "blocked" || saved["installed"].(map[string]any)["state"] != "verified" || saved["authentication"].(map[string]any)["state"] != "verified" {
				t.Fatalf("%d %s", code, out)
			}
			diagnostics := saved["binding"].(map[string]any)
			states := diagnostics["states"].([]any)
			if len(states) != 1 || states[0].(map[string]any)["state"] != "launcher-different" || diagnostics["launcher"] != launcher || diagnostics["launcherVersion"] != "0.2.0-preview.1" || diagnostics["runningVersion"] != "0.3.0" || diagnostics["provider"] != provider {
				t.Fatalf("%v", diagnostics)
			}
			if len(saved["steps"].([]any)) == 0 || candidateByID(t, report, "codex")["executable"] != provider {
				t.Fatalf("saved binding evidence must replace the PATH-only candidate: %s", out)
			}
			if _, err := os.Stat(sentinel); err == nil {
				t.Fatal("foreign launcher was executed")
			}
			calls, err := os.ReadFile(provider + ".calls")
			if err != nil || len(calls) == 0 {
				t.Fatalf("provider was not probed at its bound path: %v", err)
			}
			allowed := map[string]bool{"--version": true, "--help": true, "exec --help": true, "login status": true, "--disable plugins --disable apps mcp list --json": true}
			for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if !allowed[call] {
					t.Fatalf("unexpected provider call %q", call)
				}
			}
			want := fmt.Sprintf("forgecell init --lab %q --harness codex", lab)
			if !onPath {
				want = "PATH='" + filepath.Dir(provider) + "':\"$PATH\" " + want
			}
			if saved["nextCommand"] != want || report["nextCommand"] != want {
				t.Fatalf("next command %v / %v, want %s", saved["nextCommand"], report["nextCommand"], want)
			}
			assertOnlyExistingCommands(t, report)
		})
	}
}

func TestDoctorPrintsOneConcreteNextCommandForEveryNonReadyState(t *testing.T) {
	t.Run("provider login", func(t *testing.T) {
		bin, lab := doctorFixture(t)
		writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Not logged in", 1))
		writeTool(t, bin, "claude", strings.Replace(claudeTool, "LOGGED", "false", 1))
		writeTool(t, bin, "cursor-agent", strings.NewReplacer("HELPFLAGS", "--sandbox --mode --trust", "LOGGED", "false").Replace(cursorTool))
		code, out := runDoctor(t, lab)
		report := decodeDoctor(t, out)
		want := map[string]string{"codex": "codex login", "claude-code": "claude auth login", "cursor": "agent login"}
		for id, command := range want {
			if c := candidateByID(t, report, id); c["readiness"] != "login-required" || c["nextCommand"] != command {
				t.Fatalf("%s: %v", id, c)
			}
		}
		if code != 2 || report["nextCommand"] != fmt.Sprintf("forgecell doctor --lab %q", lab) {
			t.Fatalf("%d %v", code, report["nextCommand"])
		}
		assertOnlyExistingCommands(t, report)
	})
	t.Run("not installed", func(t *testing.T) {
		_, lab := doctorFixture(t)
		code, out := runDoctor(t, lab)
		report := decodeDoctor(t, out)
		for _, id := range []string{"codex", "claude-code", "cursor"} {
			if want := fmt.Sprintf("forgecell doctor --lab %q --harness %s", lab, id); candidateByID(t, report, id)["nextCommand"] != want {
				t.Fatalf("%s: %v", id, candidateByID(t, report, id))
			}
		}
		if code != 2 || report["nextCommand"] == nil {
			t.Fatalf("%d %s", code, out)
		}
	})
	t.Run("provider login outranks a different launcher", func(t *testing.T) {
		bin, lab := doctorFixture(t)
		provider := writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Not logged in", 1))
		launcher := writeTool(t, t.TempDir(), "forgecell", "#!/bin/sh\nexit 0\n")
		saveBinding(t, lab, "codex", []string{launcher, "__adapter", "codex", provider})
		_, out := runDoctor(t, lab)
		report := decodeDoctor(t, out)
		if report["nextCommand"] != "codex login" || report["savedBinding"].(map[string]any)["nextCommand"] != "codex login" {
			t.Fatalf("%s", out)
		}
	})
	t.Run("provider missing with a ready provider on PATH", func(t *testing.T) {
		bin, lab := doctorFixture(t)
		writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
		self, _ := os.Executable()
		saveBinding(t, lab, "codex", []string{self, "__adapter", "codex", "/missing/saved-codex"})
		code, out := runDoctor(t, lab)
		report := decodeDoctor(t, out)
		saved := report["savedBinding"].(map[string]any)
		states := saved["binding"].(map[string]any)["states"].([]any)
		want := fmt.Sprintf("forgecell init --lab %q --harness codex", lab)
		if code != 2 || len(states) != 1 || states[0].(map[string]any)["state"] != "provider-missing" || states[0].(map[string]any)["path"] != "/missing/saved-codex" || saved["nextCommand"] != want || saved["executable"] != nil {
			t.Fatalf("%d %s", code, out)
		}
	})
	t.Run("launcher and provider both missing", func(t *testing.T) {
		_, lab := doctorFixture(t)
		saveBinding(t, lab, "codex", []string{"/missing/old-forgecell", "__adapter", "codex", "/missing/saved-codex"})
		code, out := runDoctor(t, lab)
		report := decodeDoctor(t, out)
		states := report["savedBinding"].(map[string]any)["binding"].(map[string]any)["states"].([]any)
		if code != 2 || len(states) != 2 || states[0].(map[string]any)["state"] != "launcher-missing" || states[1].(map[string]any)["state"] != "provider-missing" {
			t.Fatalf("%d %s", code, out)
		}
		if want := fmt.Sprintf("forgecell doctor --lab %q --harness codex", lab); report["nextCommand"] != want {
			t.Fatalf("no ready provider exists, so the re-check is the next command: %v", report["nextCommand"])
		}
	})
	t.Run("adapter mismatch", func(t *testing.T) {
		_, lab := doctorFixture(t)
		self, _ := os.Executable()
		saveBinding(t, lab, "codex", []string{self, "__adapter", "cursor", "/missing/saved-cursor"})
		code, out := runDoctor(t, lab)
		report := decodeDoctor(t, out)
		diagnostics := report["savedBinding"].(map[string]any)["binding"].(map[string]any)
		if code != 2 || diagnostics["formulaBinding"] != "codex" || diagnostics["adapter"] != "cursor" || report["nextCommand"] == nil {
			t.Fatalf("%d %s", code, out)
		}
		assertOnlyExistingCommands(t, report)
	})
}

func TestDoctorReadyRunStillExitsZeroWithoutAnOverallNextCommand(t *testing.T) {
	bin, lab := doctorFixture(t)
	writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
	t.Setenv("CODEX_THREAD_ID", "thread")
	code, out := runDoctor(t, lab)
	report := decodeDoctor(t, out)
	if code != 0 || report["schemaVersion"] != "v1" || report["nextCommand"] != nil || candidateByID(t, report, "codex")["nextCommand"] != nil {
		t.Fatalf("%d %s", code, out)
	}
	if code, _ = runDoctor(t, lab, "--verbose"); code != 0 {
		t.Fatalf("verbose changed the exit code: %d", code)
	}
}

func TestDoctorErrorsKeepExitCodeOneAndHelpListsVerbose(t *testing.T) {
	_, lab := doctorFixture(t)
	os.WriteFile(filepath.Join(lab, "lab.json"), []byte("{"), 0600)
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--lab", lab, "--verbose"}, strings.NewReader(""), &out, &stderr, "dev"); code != 1 {
		t.Fatalf("invalid Lab: %d", code)
	}
	out.Reset()
	if code := Run(context.Background(), []string{"doctor", "--lab", t.TempDir(), "extra"}, strings.NewReader(""), &out, &stderr, "dev"); code != 1 {
		t.Fatalf("unexpected argument: %d", code)
	}
	out.Reset()
	Run(context.Background(), []string{"--help"}, strings.NewReader(""), &out, &stderr, "dev")
	if !strings.Contains(out.String(), "doctor [--lab DIR] [--harness ID] [--json] [--verbose]") {
		t.Fatal("doctor usage omits --verbose")
	}
}

func TestSavedProposalReviewOutsideRepositoryIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	recipe := "kind: formula\nid: sample\nintake: {repo: acme/widgets}\nharness: {binding: codex}\natoms: [{id: scope, type: gate, mode: human}, {id: tests, type: check, command: [go, test, ./...]}]\n"
	p, err := onboarding.SaveProposal(dir, recipe)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "assays", p.ID+".json")
	before, _ := os.ReadFile(file)
	t.Setenv("PATH", t.TempDir())
	if err = os.Mkdir(filepath.Join(dir, ".formula-write.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, jsonMode := range []bool{false, true} {
		var out, stderr bytes.Buffer
		args := []string{"init", "--lab", dir, "--review", p.ID}
		if jsonMode {
			args = append(args, "--json")
		}
		code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev")
		if code != 0 {
			t.Fatalf("%d %s", code, stderr.String())
		}
		if jsonMode {
			var view struct {
				Verified   bool                `json:"verified"`
				Proposal   onboarding.Proposal `json:"proposal"`
				NextAction string              `json:"nextAction"`
			}
			if err = json.Unmarshal(out.Bytes(), &view); err != nil || !view.Verified || view.Proposal.YAML != recipe || !strings.Contains(view.NextAction, "--approve") {
				t.Fatalf("%s %v", out.String(), err)
			}
		} else {
			for _, text := range []string{recipe, p.YAMLHash, "acme/widgets", "codex", "human", "tests", "--approve", "--review", "Historical capability evidence was not recorded", "saved observation; not current verification", "Model access: unknown", "Meta-learning adapter: unknown"} {
				if !strings.Contains(out.String(), text) {
					t.Fatalf("missing %q: %s", text, out.String())
				}
			}
		}
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("review wrote proposal")
	}
	if _, err = os.Stat(filepath.Join(dir, "lab.json")); !os.IsNotExist(err) {
		t.Fatal("review activated")
	}
	p.Status = "dismissed"
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"init", "--lab", dir, "--review", p.ID}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 || strings.Contains(out.String(), "--approve") {
		t.Fatalf("%d %s %s", code, out.String(), stderr.String())
	}
	p.YAML += "# tampered"
	raw, _ = json.Marshal(p)
	os.WriteFile(file, raw, 0600)
	out.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"init", "--lab", dir, "--review", p.ID, "--json"}, strings.NewReader(""), &out, &stderr, "dev"); code == 0 || out.Len() != 0 {
		t.Fatalf("unverified output: %d %s", code, out.String())
	}
}

func TestSavedReviewActivationCrashWindows(t *testing.T) {
	for _, stage := range []string{"intent", "applied", "approved", "complete"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			p, err := onboarding.SaveProposal(dir, "kind: formula\nid: sample\natoms: [{type: harness}]\n")
			if err != nil {
				t.Fatal(err)
			}
			a := formula.Approval{FormulaID: "sample", SHA256: p.YAMLHash, DecisionID: p.ID, SourceKind: "bootstrap-assay", SourceID: p.ID}
			if err = formula.BeginActivation(dir, p.YAML, a, p.ConfigBefore, p.TargetBefore); err != nil {
				t.Fatal(err)
			}
			if stage != "intent" {
				p.Applying = true
				if err = formula.ApplyActivation(dir, a); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "approved" || stage == "complete" {
				p.Status = "approved"
			}
			if stage == "complete" {
				if err = formula.CompleteActivation(dir, a); err != nil {
					t.Fatal(err)
				}
			}
			file := filepath.Join(dir, "assays", p.ID+".json")
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			decision := filepath.Join(dir, "formula-decisions", p.ID+".json")
			before, err := os.ReadFile(decision)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", t.TempDir())
			for _, jsonMode := range []bool{false, true} {
				var out, stderr bytes.Buffer
				args := []string{"init", "--lab", dir, "--review", p.ID}
				if jsonMode {
					args = append(args, "--json")
				}
				if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
					t.Fatalf("%d %s", code, stderr.String())
				}
				next := out.String()
				if jsonMode {
					var view struct {
						NextAction string `json:"nextAction"`
					}
					if err = json.Unmarshal(out.Bytes(), &view); err != nil {
						t.Fatal(err)
					}
					next = view.NextAction
				}
				if stage == "complete" {
					if strings.Contains(next, "--approve") || strings.Contains(next, "--dismiss") {
						t.Fatalf("completed activation requests decision: %s", next)
					}
				} else if !strings.Contains(next, fmt.Sprintf("forgecell init --lab %q --approve %s", dir, p.ID)) || strings.Contains(next, "--dismiss") {
					t.Fatalf("incomplete activation lacks exact resume action: %s", next)
				}
			}
			after, _ := os.ReadFile(decision)
			saved, _ := os.ReadFile(file)
			if !bytes.Equal(before, after) || !bytes.Equal(raw, saved) {
				t.Fatal("review mutated saved activation or proposal")
			}
		})
	}
}

func TestSavedCursorReviewExplainsWorkflowLimitation(t *testing.T) {
	dir := t.TempDir()
	p, err := onboarding.SaveProposal(dir, "kind: formula\nid: sample\nintake: {repo: acme/widgets}\nharness: {binding: cursor, command: [/forgecell, __adapter, cursor, /cursor]}\natoms: [{type: gate}]\n")
	if err != nil {
		t.Fatal(err)
	}
	c := onboarding.Candidate{ID: "cursor", Readiness: "ready", Detail: "Authenticated CLI probe; model access unverified.", Capabilities: harness.AdapterCapabilities("cursor")}
	p.CapabilityEvidence = &onboarding.BindingReport{Candidate: c, Command: []string{"/forgecell", "__adapter", "cursor", "/cursor"}, Workflow: c.WorkflowCapability()}
	t.Setenv("PATH", t.TempDir())
	for _, jsonMode := range []bool{false, true} {
		var out, stderr bytes.Buffer
		if code := renderProposal(&out, &stderr, p, dir, jsonMode); code != 0 {
			t.Fatalf("%d %s", code, stderr.String())
		}
		for _, want := range []string{"action-capable MCP/plugin isolation has not been verified", "saved observation; not current verification", "unknown", "unsupported"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %s: %s", want, out.String())
			}
		}
	}
}

// Candidate documentation evidence; this does not establish independent acceptance.
func TestOnboardingDocumentationConsistency(t *testing.T) {
	readDoc := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(strings.Fields(string(data)), " ")
	}
	guide, atoms := readDoc("agent-onboarding.md"), readDoc("atoms.md")
	require := func(doc, claim string) {
		t.Helper()
		if !strings.Contains(doc, claim) {
			t.Errorf("missing contract claim %q", claim)
		}
	}
	dir := t.TempDir()
	recipe := "kind: formula\nid: documentation-fixture\nintake: {repo: acme/widgets}\nharness: {binding: codex}\natoms: [{type: harness}, {type: check}, {type: gate}]\n"
	p, err := onboarding.SaveProposal(dir, recipe)
	if err != nil {
		t.Fatal(err)
	}
	// No discovery executables are available; review must use saved bytes only.
	t.Setenv("PATH", t.TempDir())
	var out, stderr bytes.Buffer
	args := []string{"init", "--lab", dir, "--review", p.ID, "--json"}
	if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("review exit=%d: %s", code, &stderr)
	}
	var view map[string]json.RawMessage
	if err = json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"proposal", "verified", "repository", "harness", "atoms", "repositoryContext", "reviewCommand", "nextAction", "capabilityEvidence", "capabilityEvidenceSource"} {
		if _, ok := view[key]; !ok {
			t.Errorf("review missing %s", key)
		}
		require(guide, "`"+key+"`")
	}
	var next, source string
	if err = json.Unmarshal(view["nextAction"], &next); err != nil || !strings.Contains(next, "--approve") {
		t.Fatalf("nextAction is not descriptive decision text: %s", view["nextAction"])
	}
	if err = json.Unmarshal(view["capabilityEvidenceSource"], &source); err != nil {
		t.Fatal(err)
	}
	require(guide, source)
	var saved onboarding.Proposal
	if err = json.Unmarshal(view["proposal"], &saved); err != nil || saved.YAML != recipe || saved.YAMLHash != p.YAMLHash {
		t.Fatal("review did not retain exact YAML and hash")
	}
	out.Reset()
	stderr.Reset()
	if code := Run(context.Background(), args[:len(args)-1], strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("terminal review exit=%d: %s", code, &stderr)
	}
	for _, claim := range []string{recipe, p.YAMLHash, source, "Learn command bound: false"} {
		if !strings.Contains(out.String(), claim) {
			t.Errorf("terminal review missing %q", claim)
		}
	}
	// Preparation has its own JSON shape, distinct from saved review and decisions.
	raw, err := json.Marshal(onboarding.Outcome{Status: "pending", Proposal: &p})
	if err != nil {
		t.Fatal(err)
	}
	var preparation map[string]json.RawMessage
	if err = json.Unmarshal(raw, &preparation); err != nil {
		t.Fatal(err)
	}
	for key := range preparation {
		require(guide, "`"+key+"`")
	}
	if _, ok := preparation["nextAction"]; ok {
		t.Fatal("preparation unexpectedly exposes nextAction")
	}
	for _, claim := range []string{"Initial init output has no `nextAction`", "`nextAction` is a descriptive string", "Both terminal and JSON modes are noninteractive", "no learn Atom command", "verified: true", "ticket scope, checks and publication require their separate approvals"} {
		require(guide, claim)
	}
	for _, obsolete := range []string{"**argv array**", "interactive Approve / Edit / Deny flow", "proposed coding and learn bindings"} {
		if strings.Contains(guide, obsolete) {
			t.Errorf("obsolete promise %q", obsolete)
		}
	}
	for _, claim := range []string{"fresh checkout", "temporary HOME", "unrestricted network", "review gate waiting", "publication Atoms remain skipped", "Passing candidate checks does not establish independent acceptance", "exact PR head"} {
		require(atoms, claim)
	}
	if strings.Contains(atoms, "Reads recent GitHub Actions runs as context") || strings.Contains(atoms, "Unbound runs remain record-only") {
		t.Error("obsolete Atom behavior")
	}
}

func TestSuggestionComparisonRenderingAndDecisionIsolation(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "suggestions"), 0700)
	yaml := "kind: formula\nid: sample\nharness: {command: [echo], instructions: Check tests.}\natoms: [{type: gate}]\n"
	candidate := strings.Replace(yaml, "Check tests.", "Check targeted tests.", 1)
	p := learning.Suggestion{ID: "suggestion-cli", FormulaID: "sample", Status: "pending", OriginalYAML: yaml, OriginalHash: evaluation.Hash([]byte(yaml)), ProposedYAML: candidate, ProposedHash: evaluation.Hash([]byte(candidate))}
	raw, _ := json.Marshal(p)
	file := filepath.Join(dir, "suggestions", p.ID+".json")
	os.WriteFile(file, raw, 0600)
	for _, jsonMode := range []bool{false, true} {
		var out, stderr bytes.Buffer
		args := []string{"suggestion", p.ID, "--lab", dir}
		if jsonMode {
			args = append(args, "--json")
		}
		if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
			t.Fatalf("%d %s", code, stderr.String())
		}
		for _, want := range []string{p.OriginalHash, p.ProposedHash, "missing", "instruction-only"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %q: %s", want, out.String())
			}
		}
	}
	for _, extra := range [][]string{{"--link-evaluation", dir, "--approve"}, {"--link-evaluation", dir, "--dismiss"}, {"--baseline-ledger", "one", "--candidate-ledger", "two", "--comparability-basis", "Human basis", "--approve"}, {"--evaluation-parent", dir}, {"--comparability-basis", "unused"}} {
		var out, stderr bytes.Buffer
		args := append([]string{"suggestion", p.ID, "--lab", dir}, extra...)
		if Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev") == 0 {
			t.Fatal("incompatible linkage flags accepted")
		}
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(raw, after) {
		t.Fatal("inspection/invalid options rewrote suggestion")
	}
}

func TestChecksPersistsNonpassingEvidenceWithoutSideEffects(t *testing.T) {
	for _, status := range []string{"passed", "failed", "missing", "pending", "unknown", "stale"} {
		t.Run(status, func(t *testing.T) {
			dir := t.TempDir()
			bin := t.TempDir()
			head := strings.Repeat("a", 40)
			p := delivery.PreviewResult{MoleculeID: "mol-ci", Repo: "owner/repo"}
			raw, _ := json.Marshal(p)
			p.Digest = fmt.Sprintf("%x", sha256.Sum256(raw))
			receipt := delivery.Receipt{State: "published", Preview: p, Commit: head, URL: "https://github.com/owner/repo/pull/9"}
			record := molecule.Record{ID: "mol-ci", Issue: molecule.Issue{Repo: p.Repo}, Verification: &verification.Result{}}
			os.MkdirAll(filepath.Join(dir, "deliveries"), 0700)
			os.MkdirAll(filepath.Join(dir, "ledgers"), 0700)
			os.MkdirAll(filepath.Join(dir, "formulas"), 0700)
			receiptRaw, _ := json.Marshal(receipt)
			ledgerRaw, _ := json.Marshal(record)
			os.WriteFile(filepath.Join(dir, "deliveries/mol-ci.json"), receiptRaw, 0600)
			os.WriteFile(filepath.Join(dir, "ledgers/mol-ci.json"), ledgerRaw, 0600)
			os.WriteFile(filepath.Join(dir, "formulas/default.yaml"), []byte("original formula"), 0600)
			entries := `{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}`
			observed := head
			switch status {
			case "failed":
				entries = strings.Replace(entries, "SUCCESS", "FAILURE", 1)
			case "missing":
				entries = ""
			case "pending":
				entries = strings.Replace(entries, "COMPLETED", "IN_PROGRESS", 1)
			case "stale":
				observed = strings.Repeat("b", 40)
			}
			response := fmt.Sprintf(`{"headRefOid":%q,"statusCheckRollup":[%s]}`, observed, entries)
			if status == "unknown" {
				response = "null"
			}
			script := "#!/bin/sh\nif [ \"$*\" != 'pr view 9 --repo owner/repo --json headRefOid,statusCheckRollup' ]; then exit 99; fi\nprintf '%s' '" + response + "'\n"
			os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0700)
			t.Setenv("PATH", bin)
			var out, stderr bytes.Buffer
			args := []string{"checks", "--lab", dir, "--molecule", "mol-ci", "--repo", p.Repo, "--pr", "9", "--commit", head, "--required", "test"}
			code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "test")
			want := 2
			if status == "passed" {
				want = 0
			}
			if code != want {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, &out, &stderr)
			}
			history, err := delivery.ReadCI(delivery.Options{LabDir: dir, MoleculeID: "mol-ci"})
			if err != nil || len(history) != 1 || history[0].Snapshot.Status != status {
				t.Fatalf("%+v %v", history, err)
			}
			for path, original := range map[string]string{"deliveries/mol-ci.json": string(receiptRaw), "ledgers/mol-ci.json": string(ledgerRaw), "formulas/default.yaml": "original formula"} {
				after, _ := os.ReadFile(filepath.Join(dir, path))
				if string(after) != original {
					t.Fatalf("rewrote %s", path)
				}
			}
			// A mismatched target must fail before collecting or appending another observation.
			args[6] = "other/repo"
			out.Reset()
			stderr.Reset()
			if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "test"); code != 1 {
				t.Fatalf("mismatch accepted: %d", code)
			}
			history, err = delivery.ReadCI(delivery.Options{LabDir: dir, MoleculeID: "mol-ci"})
			if err != nil || len(history) != 1 {
				t.Fatal("mismatch changed history")
			}
			args[6] = p.Repo
			if err := os.Mkdir(filepath.Join(dir, ".delivery-lock"), 0700); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			stderr.Reset()
			if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "test"); code != 1 || out.Len() != 0 {
				t.Fatalf("failed persistence reported saved evidence: code=%d output=%s", code, &out)
			}
			history, err = delivery.ReadCI(delivery.Options{LabDir: dir, MoleculeID: "mol-ci"})
			if err != nil || len(history) != 1 {
				t.Fatal("failed persistence changed history")
			}
		})
	}
}
