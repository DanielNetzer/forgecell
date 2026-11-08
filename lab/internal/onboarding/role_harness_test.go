package onboarding

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"go.yaml.in/yaml/v4"
)

func roleHarness(root string, runner Runner, dir, id, role string, mutate ...func(*InitOptions)) (Outcome, error) {
	o := InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Preferred: id, Role: role, RoleHarness: true, Runner: runner}
	for _, m := range mutate {
		m(&o)
	}
	return Prepare(context.Background(), o)
}

func roleOf(t *testing.T, doc map[string]any, role string) map[string]any {
	t.Helper()
	roles, _ := doc["harnesses"].(map[string]any)
	m, _ := roles[role].(map[string]any)
	return m
}

func TestHarnessRoleDeclaresAnalysisOverride(t *testing.T) {
	root, runner, dir, boot := approvedLab(t)
	active, err := formula.Load(dir, "")
	if err != nil || len(active.Formula.Harnesses) != 0 {
		t.Fatalf("fixture already declares a role: %v %v", err, active.Formula.Harnesses)
	}
	next, err := roleHarness(root, runner, dir, "claude-code", "analysis")
	if err != nil || next.Status != "pending" || next.Proposal == nil {
		t.Fatalf("%+v %v", next, err)
	}
	command := []string{"/cli", "__adapter", "claude-code", "/claude-code"}
	got := roleOf(t, decodeYAML(t, next.Proposal.YAML), "analysis")
	if want := map[string]any{"binding": "claude-code", "command": anyArgv(command)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("harnesses.analysis = %v", got)
	}
	if err = verifyRoleHarnessExact(active.Formula.YAML, next.Proposal.YAML, formula.AnalysisHarness, "claude-code", command); err != nil {
		t.Fatal(err)
	}
	if want := `harnesses.analysis: (none) -> {"binding":"claude-code","command":["/cli","__adapter","claude-code","/claude-code"]}`; !strings.Contains(next.Note, want) {
		t.Fatalf("the note omits %q:\n%s", want, next.Note)
	}
	if next.BeforeYAML != active.Formula.YAML {
		t.Fatal("the outcome does not show the approved YAML")
	}
	saved, err := InspectProposal(dir, next.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.EvidenceHash != boot.Proposal.EvidenceHash || saved.EvidencePath != boot.Proposal.EvidencePath {
		t.Fatal("detached repository evidence changed")
	}
	if now, _ := formula.Load(dir, ""); now.Formula.SHA256 != active.Formula.SHA256 {
		t.Fatal("proposing activated the change")
	}
	if _, err = Review(dir, next.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	now, err := formula.Load(dir, "")
	if err != nil || !now.Approved {
		t.Fatalf("approval failed: %v", err)
	}
	if b, declared := now.Formula.RoleBinding(formula.AnalysisHarness); !declared || b.Binding != "claude-code" || !reflect.DeepEqual(b.Command, command) {
		t.Fatalf("approved analysis binding = %+v %v", b, declared)
	}
	if now.Formula.Harness.Binding != "codex" || !reflect.DeepEqual(now.Formula.Harness.Command, active.Formula.Harness.Command) {
		t.Fatalf("the default harness changed: %+v", now.Formula.Harness)
	}
}

func TestHarnessRoleRebindsDeclaredOverride(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	approved := approveEdited(t, dir, func(doc map[string]any) {
		doc["harnesses"] = map[string]any{"analysis": map[string]any{"binding": "claude-code", "command": nativeCommand("/old-cli", "claude-code"), "timeoutMs": 1234}}
	})
	next, err := roleHarness(root, runner, dir, "codex", "analysis")
	if err != nil || next.Proposal == nil {
		t.Fatalf("%+v %v", next, err)
	}
	command := []string{"/cli", "__adapter", "codex", "/codex"}
	got := roleOf(t, decodeYAML(t, next.Proposal.YAML), "analysis")
	if want := map[string]any{"binding": "codex", "command": anyArgv(command), "timeoutMs": 1234}; !reflect.DeepEqual(got, want) {
		t.Fatalf("harnesses.analysis = %v", got)
	}
	if err = verifyRoleHarnessExact(approved.Formula.YAML, next.Proposal.YAML, formula.AnalysisHarness, "codex", command); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`harnesses.analysis.binding: "claude-code" -> "codex"`, `harnesses.analysis.command: ["/old-cli","__adapter","claude-code","/claude-code"] -> ["/cli","__adapter","codex","/codex"]`} {
		if !strings.Contains(next.Note, want) {
			t.Fatalf("the note omits %q:\n%s", want, next.Note)
		}
	}
	if strings.Contains(next.Note, "timeoutMs") {
		t.Fatalf("an unchanged key is listed: %s", next.Note)
	}
}

func TestHarnessRoleRefusesCursorForAnalysis(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	before := assayCount(dir)
	reason := harness.AdapterCapabilities("cursor").Analysis.Reason
	if reason == "" || harness.AdapterCapabilities("cursor").Analysis.State != "unsupported" {
		t.Fatalf("fixture expects Cursor to refuse analysis: %+v", harness.AdapterCapabilities("cursor").Analysis)
	}
	_, err := roleHarness(root, runner, dir, "cursor", "analysis")
	if err == nil || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "harnesses.analysis") {
		t.Fatalf("Cursor was not refused with its own reason: %v", err)
	}
	if assayCount(dir) != before {
		t.Fatal("a refused proposal was saved")
	}
}

func TestHarnessRoleRefusesUnreadyOrUnknownHarness(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	for _, c := range []struct {
		name, id, want string
		mutate         func(*InitOptions)
	}{
		{"not ready", "claude-code", "cannot bind harnesses.analysis to claude-code", func(o *InitOptions) {
			o.Candidates = []Candidate{{ID: "claude-code", Executable: "/claude-code", Readiness: "blocked", Detail: "not logged in"}}
		}},
		{"no executable", "claude-code", "cannot bind harnesses.analysis to claude-code", func(o *InitOptions) {
			o.Candidates = []Candidate{{ID: "claude-code", Readiness: "ready"}}
		}},
		{"no launcher", "claude-code", "launcher is unknown", func(o *InitOptions) { o.Launcher = "" }},
		{"relative launcher", "claude-code", "must be an absolute path", func(o *InitOptions) { o.Launcher = "forgecell" }},
		{"unknown", "nonesuch", "no such harness was discovered", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := assayCount(dir)
			var mutate []func(*InitOptions)
			if c.mutate != nil {
				mutate = append(mutate, c.mutate)
			}
			_, err := roleHarness(root, runner, dir, c.id, "analysis", mutate...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
			if assayCount(dir) != before {
				t.Fatal("a refused proposal was saved")
			}
		})
	}
}

func TestHarnessRoleNothingToChange(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	before := assayCount(dir)
	if _, err := roleHarness(root, runner, dir, "codex", "coding"); err == nil || !strings.Contains(err.Error(), "nothing to change") || !strings.Contains(err.Error(), "default binding") {
		t.Fatalf("an unchanged coding role was proposed: %v", err)
	}
	if _, err := roleHarness(root, runner, dir, "codex", "analysis"); err == nil || !strings.Contains(err.Error(), "nothing to change") {
		t.Fatalf("an unchanged analysis role was proposed: %v", err)
	}
	if _, err := roleHarness(root, runner, dir, "codex", "meta"); err == nil || !strings.Contains(err.Error(), "nothing to change") || !strings.Contains(err.Error(), "learn Atom") {
		t.Fatalf("an unchanged meta role was proposed: %v", err)
	}
	approveEdited(t, dir, func(doc map[string]any) {
		doc["harnesses"] = map[string]any{"analysis": map[string]any{"binding": "claude-code", "command": nativeCommand("/cli", "claude-code")}}
	})
	before = assayCount(dir)
	if _, err := roleHarness(root, runner, dir, "claude-code", "analysis"); err == nil || !strings.Contains(err.Error(), "nothing to change") || !strings.Contains(err.Error(), "its own override") {
		t.Fatalf("an unchanged declared role was proposed: %v", err)
	}
	if assayCount(dir) != before {
		t.Fatal("a refused proposal was saved")
	}
}

func TestHarnessRoleRefusesAnInvalidResult(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	before := assayCount(dir)
	// The harness Atom is bound to codex, so a claude-code coding override conflicts.
	if _, err := roleHarness(root, runner, dir, "claude-code", "coding"); err == nil || !strings.Contains(err.Error(), "would not be a valid Formula") || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("a conflicting coding override was proposed: %v", err)
	}
	// The learn Atom carries the codex command, so another meta command conflicts.
	if _, err := roleHarness(root, runner, dir, "claude-code", "meta"); err == nil || !strings.Contains(err.Error(), "would not be a valid Formula") {
		t.Fatalf("a conflicting meta override was proposed: %v", err)
	}
	if assayCount(dir) != before {
		t.Fatal("a refused proposal was saved")
	}
}

func TestHarnessRoleVerifyExact(t *testing.T) {
	before := "schemaVersion: v0\nkind: formula\nid: sample\nharness:\n  binding: codex\n  command: [/cli, __adapter, codex, /codex]\nharnesses:\n  meta: {binding: codex, command: [/cli, __adapter, codex, /codex]}\natoms:\n  - {id: intake, type: intake}\n"
	command := []string{"/cli", "__adapter", "claude-code", "/claude-code"}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(before), &doc); err != nil {
		t.Fatal(err)
	}
	changes, err := roleHarnessRecipe(doc, formula.AnalysisHarness, "claude-code", command)
	if err != nil || len(changes) != 1 {
		t.Fatalf("%v %v", changes, err)
	}
	encode := func(doc map[string]any) string {
		b, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	exact := encode(doc)
	if err = verifyRoleHarnessExact(before, exact, formula.AnalysisHarness, "claude-code", command); err != nil {
		t.Fatalf("the exact proposal was refused: %v", err)
	}
	for name, tamper := range map[string]func(doc map[string]any){
		"other top-level key": func(doc map[string]any) { doc["id"] = "other" },
		"default harness":     func(doc map[string]any) { doc["harness"].(map[string]any)["binding"] = "claude-code" },
		"other role":          func(doc map[string]any) { roleOf(t, doc, "meta")["binding"] = "claude-code" },
		"extra role key":      func(doc map[string]any) { roleOf(t, doc, "analysis")["timeoutMs"] = 1 },
		"wrong command": func(doc map[string]any) {
			roleOf(t, doc, "analysis")["command"] = nativeCommand("/elsewhere", "claude-code")
		},
		"role missing": func(doc map[string]any) { delete(doc["harnesses"].(map[string]any), "analysis") },
	} {
		t.Run(name, func(t *testing.T) {
			var proposed map[string]any
			if err := yaml.Unmarshal([]byte(exact), &proposed); err != nil {
				t.Fatal(err)
			}
			tamper(proposed)
			if err := verifyRoleHarnessExact(before, encode(proposed), formula.AnalysisHarness, "claude-code", command); err == nil || !strings.Contains(err.Error(), "no proposal saved") {
				t.Fatalf("a tampered proposal was accepted: %v", err)
			}
		})
	}
}

func TestHarnessRoleOptionConflicts(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	for name, c := range map[string]struct {
		mutate func(*InitOptions)
		want   string
	}{
		"review role":  {func(o *InitOptions) { o.Role = "review" }, "use analysis, coding or meta"},
		"unknown role": {func(o *InitOptions) { o.Role = "ship" }, "use analysis, coding or meta"},
		"no harness":   {func(o *InitOptions) { o.Preferred = "" }, "--role needs --harness"},
		"rebind":       {func(o *InitOptions) { o.Rebind = "codex" }, "changes only harnesses.ROLE"},
		"model":        {func(o *InitOptions) { o.Model, o.ModelSet = "m", true }, "changes only harnesses.ROLE"},
		"meta harness": {func(o *InitOptions) { o.MetaHarness = "codex" }, "changes only harnesses.ROLE"},
		"meta command": {func(o *InitOptions) { o.MetaCommand = []string{"/bin/x"} }, "changes only harnesses.ROLE"},
		"timeout":      {func(o *InitOptions) { o.HarnessTimeoutMS = 1000 }, "changes only harnesses.ROLE"},
	} {
		t.Run(name, func(t *testing.T) {
			before := assayCount(dir)
			if _, err := roleHarness(root, runner, dir, "claude-code", "analysis", c.mutate); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
			if assayCount(dir) != before {
				t.Fatal("a refused proposal was saved")
			}
		})
	}
	t.Run("role without harness or model", func(t *testing.T) {
		_, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Runner: runner, Role: "analysis", Preferred: "claude-code"})
		if err == nil || !strings.Contains(err.Error(), "--role needs --model") || !strings.Contains(err.Error(), "--harness") {
			t.Fatal(err)
		}
	})
}

func TestHarnessRoleRefusedWithoutApprovalOrWhilePending(t *testing.T) {
	root, runner := repoFixture(t)
	empty := t.TempDir()
	if _, err := roleHarness(root, runner, empty, "claude-code", "analysis"); err == nil || !strings.Contains(err.Error(), "needs an approved Formula") {
		t.Fatalf("no Formula: %v", err)
	}
	if assayCount(empty) != 0 {
		t.Fatal("a refused proposal was saved")
	}
	root, runner, dir, _ := approvedLab(t)
	first, err := roleHarness(root, runner, dir, "claude-code", "analysis")
	if err != nil || first.Proposal == nil {
		t.Fatalf("%+v %v", first, err)
	}
	before := assayCount(dir)
	if _, err = roleHarness(root, runner, dir, "claude-code", "analysis"); err == nil || !strings.Contains(err.Error(), "already pending") || !strings.Contains(err.Error(), first.Proposal.ID) {
		t.Fatalf("a second proposal was not refused: %v", err)
	}
	if assayCount(dir) != before {
		t.Fatal("a refused proposal was saved")
	}
}

func TestHarnessRoleAnalysisFallbackNamesTheCommand(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	before := assayCount(dir)
	_, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Preferred: "cursor", Runner: runner})
	if err == nil {
		t.Fatal("cursor became the analysis binding by fallback")
	}
	for _, want := range []string{harness.AdapterCapabilities("cursor").Analysis.Reason, "harnesses.analysis", "forgecell init --lab DIR --harness claude-code --role analysis"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal omits %q: %v", want, err)
		}
	}
	if assayCount(dir) != before {
		t.Fatal("a refused proposal was saved")
	}
}
