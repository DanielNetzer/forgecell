package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
)

// The manifest.json that sits beside a fixture executable: "marked" is what the
// installer writes today, "legacy" what releases installed before the kind marker
// carry, "bare" only a version, "mismatch" a manifest recording another file's
// digest, and "stray" belongs to some other tool.
const (
	markedManifest   = "marked"
	legacyManifest   = "legacy"
	bareManifest     = "bare"
	mismatchManifest = "mismatch"
	strayManifest    = "stray"
)

func releaseBuild(t *testing.T, style string) string {
	t.Helper()
	dir := t.TempDir()
	content := []byte("#!/bin/sh\nexit 1\n")
	h := sha256.Sum256(content)
	sha := hex.EncodeToString(h[:])
	protocol := `"protocolVersion":"` + harness.AdapterProtocolVersion + `"`
	var manifest string
	switch style {
	case markedManifest:
		manifest = `{"kind":"` + harness.ReleaseKind + `","version":"0.2.0","sha256":"` + sha + `",` + protocol + `}`
	case legacyManifest:
		manifest = `{"version":"0.2.0","sha256":"` + sha + `",` + protocol + `}`
	case bareManifest:
		manifest = `{"version":"1.2.3"}`
	case mismatchManifest:
		manifest = `{"version":"0.2.0","sha256":"` + strings.Repeat("a", 64) + `",` + protocol + `}`
	default:
		manifest = `{"name":"other-tool","version":"0.2.0","license":"MIT"}`
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "forgecell")
	if err := os.WriteFile(path, content, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func argvJSON(t *testing.T, argv ...string) string {
	t.Helper()
	b, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// conflictedMetaFormula declares harnesses.meta and gives the learn Atom another command.
func conflictedMetaFormula(t *testing.T) formula.Formula {
	t.Helper()
	f := metaRoleFormula(t, "  - {id: learn, type: learn}\n")
	for i := range f.Atoms {
		if f.Atoms[i].Type == "learn" {
			f.Atoms[i].Command = []string{"/cli", "__adapter", "codex", "/other-codex"}
		}
	}
	return f
}

func TestMetaBindingReportsConflictBetweenRoleAndLearnAtom(t *testing.T) {
	f := conflictedMetaFormula(t)
	if id, conflict := MetaConflict(f); !conflict || id != "learn" {
		t.Fatalf("MetaConflict = %q %t", id, conflict)
	}
	if LearnBound(f) {
		t.Fatal("a conflicting meta role and learn Atom are learn-bound; learning refuses to run")
	}
	if b, source, bound := MetaBinding(f); bound || source != "" || len(b.Command) != 0 {
		t.Fatalf("the conflict was reported as a healthy binding: %+v %q", b, source)
	}
	report := DescribeFormulaMeta(f)
	if report.Capability.State != "blocked" || !strings.Contains(report.Capability.Reason, "harnesses.meta") || !strings.Contains(report.Capability.Reason, "learn Atom learn") {
		t.Fatalf("the conflict is not reported: %+v", report.Capability)
	}
	if inspected := InspectMeta(context.Background(), f, t.TempDir(), nil); inspected.Capability.State != "blocked" {
		t.Fatalf("InspectMeta: %+v", inspected.Capability)
	}

	equal := metaRoleFormula(t, "  - {id: learn, type: learn}\n")
	role, _ := equal.RoleBinding(formula.MetaHarness)
	for i := range equal.Atoms {
		if equal.Atoms[i].Type == "learn" {
			equal.Atoms[i].Command = role.Command
		}
	}
	if _, conflict := MetaConflict(equal); conflict || !LearnBound(equal) {
		t.Fatal("equal commands are not a conflict")
	}
	if _, source, bound := MetaBinding(equal); !bound || source != "override" {
		t.Fatalf("source = %q bound = %t", source, bound)
	}
	commandless := metaRoleFormula(t, "  - {id: learn, type: learn}\n")
	if _, conflict := MetaConflict(commandless); conflict || !LearnBound(commandless) {
		t.Fatal("a commandless learn Atom is not a conflict")
	}
	if report := DescribeFormulaMeta(commandless); report.Capability.State == "blocked" {
		t.Fatalf("a healthy meta role was reported as conflicted: %+v", report.Capability)
	}
}

func TestInspectRolesMarksConflictedMetaRoleNotReady(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := fixtureProvider(t)
	command := argvJSON(t, self, "__adapter", "codex", provider)
	build := func(learn string) formula.Formula {
		text := "kind: formula\nid: meta-conflict\nintake: {repo: acme/widgets}\nharness: {binding: codex, command: " + command + ", timeoutMs: 1000}\nharnesses:\n  meta: {binding: codex, command: " + command + "}\n" + readinessAtoms + learn
		f, err := formula.Parse([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	metaRole := func(f formula.Formula) RoleReadiness {
		for _, r := range InspectRoles(context.Background(), f, t.TempDir(), healthyRunner(nil)) {
			if r.Role == string(formula.MetaHarness) {
				return r
			}
		}
		t.Fatal("no meta role")
		return RoleReadiness{}
	}
	healthy := metaRole(build("  - {id: learn, type: learn}\n"))
	if !healthy.Ready || healthy.Source != "override" {
		t.Fatalf("the control meta role must be ready: %+v", healthy)
	}

	conflicted := metaRole(build("  - {id: learn, type: learn, binding: codex, command: " + argvJSON(t, self, "__adapter", "codex", "/elsewhere/codex") + "}\n"))
	if conflicted.Ready || conflicted.Readiness != "blocked" || conflicted.Source != "override" || conflicted.Capability.State != "blocked" {
		t.Fatalf("a conflicting meta role was reported healthy: %+v", conflicted)
	}
	for _, text := range []string{conflicted.Detail, conflicted.Capability.Reason} {
		if !strings.Contains(text, "harnesses.meta") || !strings.Contains(text, "learn Atom learn") {
			t.Fatalf("the conflict does not name the role and the Atom: %q", text)
		}
	}
}

// activateHistorical approves a Formula without the role validation proposals apply,
// as a Formula saved by an earlier release could be.
func activateHistorical(t *testing.T, dir, text string) {
	t.Helper()
	f, err := formula.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	a := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: "historical", SourceKind: "historical", SourceID: "historical"}
	if err = formula.BeginActivation(dir, text, a, "absent", "absent"); err != nil {
		t.Fatal(err)
	}
	if err = formula.ApplyActivation(dir, a); err != nil {
		t.Fatal(err)
	}
	if err = formula.CompleteActivation(dir, a); err != nil {
		t.Fatal(err)
	}
}

func metaRoleLab(t *testing.T, roleCommand string) (root string, runner Runner, dir string) {
	t.Helper()
	root, runner = repoFixture(t)
	dir = t.TempDir()
	text := "kind: formula\nid: meta-role\nintake: {repo: acme/widgets}\nharness: {binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 1000}\nharnesses:\n  meta: {binding: codex, command: " + roleCommand + "}\n" + readinessAtoms
	activateHistorical(t, dir, text)
	return root, runner, dir
}

func TestMetaOptInRefusesCommandThatConflictsWithDeclaredRole(t *testing.T) {
	for name, o := range map[string]InitOptions{
		"meta harness with another launcher": {MetaHarness: "codex", Launcher: "/new-cli"},
		"meta command":                       {MetaCommand: []string{"/other-meta", "arg"}, Launcher: "/cli"},
	} {
		t.Run(name, func(t *testing.T) {
			root, runner, dir := metaRoleLab(t, argvJSON(t, "/cli", "__adapter", "codex", "/codex"))
			o.Cwd, o.LabDir, o.Candidates, o.Runner = root, dir, rebindCandidates(), runner
			out, err := Prepare(context.Background(), o)
			if err == nil || !strings.Contains(err.Error(), "harnesses.meta") {
				t.Fatalf("a learn Atom that conflicts with the declared role was proposed: %+v %v", out.Proposal, err)
			}
			if out.Proposal != nil || assayCount(dir) != 0 {
				t.Fatalf("a refused opt-in saved a proposal: %d", assayCount(dir))
			}
		})
	}
}

func TestMetaOptInReusesDeclaredRoleCommand(t *testing.T) {
	root, runner, dir := metaRoleLab(t, argvJSON(t, "/cli", "__adapter", "codex", "/codex"))
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Runner: runner, MetaHarness: "codex"})
	if err != nil || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	parsed, err := formula.Parse([]byte(out.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	role, _ := parsed.RoleBinding(formula.MetaHarness)
	var learn formula.Atom
	for _, a := range parsed.Atoms {
		if a.Type == "learn" {
			learn = a
		}
	}
	if !reflect.DeepEqual(learn.Command, role.Command) || learn.Binding != role.Binding {
		t.Fatalf("the learn Atom does not reuse the role: %+v vs %+v", learn, role)
	}
	if id, conflict := MetaConflict(parsed); conflict {
		t.Fatalf("the proposal conflicts with the role through learn Atom %s", id)
	}
	if err = parsed.ValidateHarnessRoles(); err != nil || !LearnBound(parsed) {
		t.Fatalf("learning would not run: %v", err)
	}
}

func TestRebindDoesNotRepointLauncherBesideStrayManifest(t *testing.T) {
	marked, legacy := releaseBuild(t, markedManifest), releaseBuild(t, legacyManifest)
	f, err := formula.Parse([]byte(roleFormula("")))
	if err != nil {
		t.Fatal(err)
	}
	if !RebindRepoints(f, marked) || !RebindRepoints(f, legacy) {
		t.Fatal("a launcher beside a Forgecell manifest that records its digest, with or without the kind marker, must stay established")
	}

	for _, style := range []string{strayManifest, bareManifest, mismatchManifest} {
		t.Run(style, func(t *testing.T) {
			launcher := releaseBuild(t, style)
			if RebindRepoints(f, launcher) {
				t.Fatalf("a launcher beside a %s manifest was established as Forgecell", style)
			}
			root, runner, dir, _ := approvedLab(t)
			approveEdited(t, dir, func(doc map[string]any) {
				learnAtomOf(t, doc)["command"] = nativeCommand(launcher, "codex")
			})
			next, err := rebind(t, root, runner, dir, "claude-code", rebindToNewLauncher)
			if err != nil || next.Proposal == nil {
				t.Fatalf("%+v %v", next, err)
			}
			if got := learnAtomOf(t, decodeYAML(t, next.Proposal.YAML))["command"]; !reflect.DeepEqual(got, nativeCommand(launcher, "codex")) {
				t.Fatalf("the launcher beside a %s manifest was repointed: %v", style, got)
			}
			if want := "Left unchanged because the launcher is not established as a Forgecell executable"; !strings.Contains(next.Note, want) || !strings.Contains(next.Note, "atoms[learn].command[0]: "+quoted(t, launcher)) {
				t.Fatalf("the note must list the command under left-unchanged: %s", next.Note)
			}
		})
	}
}

func TestRebindRepointsLauncherBesideForgecellManifest(t *testing.T) {
	for _, style := range []string{markedManifest, legacyManifest} {
		t.Run(style, func(t *testing.T) {
			build := releaseBuild(t, style)
			root, runner, dir, _ := approvedLab(t)
			approveEdited(t, dir, func(doc map[string]any) {
				learnAtomOf(t, doc)["command"] = nativeCommand(build, "codex")
			})
			next, err := rebind(t, root, runner, dir, "claude-code", rebindToNewLauncher)
			if err != nil || next.Proposal == nil {
				t.Fatalf("%+v %v", next, err)
			}
			if got := learnAtomOf(t, decodeYAML(t, next.Proposal.YAML))["command"]; !reflect.DeepEqual(got, nativeCommand("/new-cli", "codex")) {
				t.Fatalf("the launcher beside a %s manifest was not repointed: %v", style, got)
			}
			if strings.Contains(next.Note, "Left unchanged") {
				t.Fatalf("an established launcher was left unchanged: %s", next.Note)
			}
		})
	}
}

func TestPrepareRejectsNonAbsoluteNativeLauncherOrProvider(t *testing.T) {
	for name, c := range map[string]struct{ launcher, executable string }{
		"bare launcher":     {"forgecell", "/codex"},
		"relative launcher": {"./bin/forgecell", "/codex"},
		"bare provider":     {"/cli", "codex"},
		"relative provider": {"/cli", "./node_modules/.bin/codex"},
	} {
		t.Run(name, func(t *testing.T) {
			root, runner := repoFixture(t)
			dir := t.TempDir()
			candidates := []Candidate{{ID: "codex", Executable: c.executable, Readiness: "ready", Capabilities: harness.AdapterCapabilities("codex")}}
			out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: c.launcher, Candidates: candidates, Runner: runner})
			if err == nil || !strings.Contains(err.Error(), "absolute path") || out.Proposal != nil || assayCount(dir) != 0 {
				t.Fatalf("a native command was composed from a non-absolute path: %+v %v", out.Proposal, err)
			}
		})
	}

	root, runner, dir, _ := approvedLab(t)
	before := assayCount(dir)
	for _, launcher := range []string{"new-cli", "./new-cli"} {
		if _, err := rebind(t, root, runner, dir, "claude-code", func(o *InitOptions) { o.Launcher = launcher }); err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Fatalf("rebind to launcher %q: %v", launcher, err)
		}
	}
	if _, err := rebind(t, root, runner, dir, "claude-code", func(o *InitOptions) {
		o.Candidates = []Candidate{{ID: "claude-code", Executable: "claude", Readiness: "ready", Capabilities: harness.AdapterCapabilities("claude-code")}}
	}); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("rebind to a relative provider: %v", err)
	}
	if assayCount(dir) != before {
		t.Fatal("a refused rebind saved a proposal")
	}
}

func TestProbeBindingNeverResolvesRelativeProvider(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := fixtureProvider(t)
	dir := filepath.Dir(provider)
	t.Setenv("PATH", dir)
	t.Chdir(dir)
	for _, relative := range []string{filepath.Base(provider), "./" + filepath.Base(provider)} {
		var calls [][]string
		c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: []string{self, "__adapter", "codex", relative}}, t.TempDir(), recordingRunner(&calls))
		if len(calls) != 0 || c.Readiness != "blocked" || c.Executable != "" || c.Binding.ResolvedProvider != "" || c.Installed.State != "blocked" {
			t.Fatalf("%q: the relative provider was resolved or run: %+v calls=%v", relative, c, calls)
		}
		if names := stateNames(t, c); !reflect.DeepEqual(names, []string{"provider-not-absolute"}) || c.Binding.States[0].Path != relative || !strings.Contains(c.Detail, "absolute path") {
			t.Fatalf("%q: %v %+v", relative, names, c.Binding.States)
		}
	}

	build := releaseBuild(t, markedManifest)
	t.Setenv("PATH", filepath.Dir(build))
	var calls [][]string
	c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: []string{"forgecell", "__adapter", "codex", provider}}, t.TempDir(), recordingRunner(&calls))
	if names := stateNames(t, c); !reflect.DeepEqual(names, []string{"launcher-unknown"}) || c.Binding.States[0].Path != "forgecell" || c.Binding.LauncherVersion != "unknown" || c.Binding.ResolvedLauncher != "" {
		t.Fatalf("a bare launcher resolved through PATH: %v %+v", names, c.Binding)
	}
	if !strings.Contains(c.Binding.States[0].Detail, "absolute path") {
		t.Fatalf("%s", c.Binding.States[0].Detail)
	}
}

func TestValidateRoleBindingsRejectsRelativeNativeCommand(t *testing.T) {
	for name, c := range map[string]struct {
		text    string
		wantErr string
	}{
		"absolute control":           {roleFormula(""), ""},
		"custom command not judged":  {roleFormula("harnesses:\n  coding: {binding: codex, command: [custom-tool, arg]}\n"), ""},
		"default relative launcher":  {strings.Replace(roleFormula(""), "[/cli, __adapter, codex, /codex]", "[cli, __adapter, codex, /codex]", 1), "harness.command[0]: native launcher"},
		"default relative provider":  {strings.Replace(roleFormula(""), "[/cli, __adapter, codex, /codex]", "[/cli, __adapter, codex, codex]", 1), "harness.command[3]: native provider"},
		"analysis relative launcher": {roleFormula("harnesses:\n  analysis: {binding: claude-code, command: [./cli, __adapter, claude-code, /claude]}\n"), "harnesses.analysis.command[0]: native launcher"},
		"coding relative provider":   {roleFormula("harnesses:\n  coding: {binding: codex, command: [/cli, __adapter, codex, bin/codex]}\n"), "harnesses.coding.command[3]: native provider"},
		"learn Atom relative":        {roleFormula("") + "  - {id: learn, type: learn, binding: codex, command: [cli, __adapter, codex, /codex]}\n", "atoms[learn].command[0]: native launcher"},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := formula.Parse([]byte(c.text))
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateRoleBindings(f)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), "absolute path")):
				t.Fatalf("want %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestValidateRoleBindingsNamesTheAbsoluteValueToUse(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "forgecell-on-path")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for name, c := range map[string]struct {
		text string
		want []string
	}{
		"resolvable launcher": {
			roleFormula("harnesses:\n  analysis: {binding: claude-code, command: [forgecell-on-path, __adapter, claude-code, /claude]}\n"),
			[]string{"harnesses.analysis.command[0]", `"forgecell-on-path"`, `set it to "` + tool + `"`},
		},
		"resolvable provider": {
			roleFormula("") + "  - {id: learn, type: learn, binding: codex, command: [/cli, __adapter, codex, forgecell-on-path]}\n",
			[]string{"atoms[learn].command[3]", `"forgecell-on-path"`, `set it to "` + tool + `"`},
		},
		"unresolvable launcher": {
			roleFormula("harnesses:\n  coding: {binding: codex, command: [no-such-forgecell, __adapter, codex, /codex]}\n"),
			[]string{"harnesses.coding.command[0]", `"no-such-forgecell"`, "does not resolve to an executable here"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := formula.Parse([]byte(c.text))
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateRoleBindings(f)
			if err == nil {
				t.Fatal("a relative native command was accepted")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the error omits %q: %v", want, err)
				}
			}
		})
	}
}

func TestPrepareNamesTheKeyOfARelativeNativeCommand(t *testing.T) {
	root, runner := repoFixture(t)
	candidates := []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready", Capabilities: harness.AdapterCapabilities("codex")}}
	_, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: t.TempDir(), Launcher: "no-such-forgecell", Candidates: candidates, Runner: runner})
	if err == nil || !strings.Contains(err.Error(), "harness.command[0]") || !strings.Contains(err.Error(), `"no-such-forgecell"`) {
		t.Fatalf("the refusal does not name the Formula key: %v", err)
	}
}
