package onboarding

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

const readinessAtoms = "atoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness, binding: codex}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"

// metaRoleFormula binds learning only through harnesses.meta: the learn Atom has no command.
func metaRoleFormula(t *testing.T, learn string) formula.Formula {
	t.Helper()
	text := "kind: formula\nid: meta-role\nintake: {repo: acme/widgets}\nharness: {binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 1000}\nharnesses:\n  meta: {binding: codex, command: [/cli, __adapter, codex, /codex]}\n" + readinessAtoms + learn
	f, err := formula.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.ValidateHarnessRoles(); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLearnBoundSeesAMetaRoleOnACommandlessLearnAtom(t *testing.T) {
	f := metaRoleFormula(t, "  - {id: learn, type: learn}\n")
	if !LearnBound(f) {
		t.Fatal("a commandless learn Atom with harnesses.meta is not learn-bound")
	}
	b, source, ok := MetaBinding(f)
	if !ok || source != "override" || b.Binding != "codex" || !reflect.DeepEqual(b.Command, []string{"/cli", "__adapter", "codex", "/codex"}) || b.TimeoutMS != 1000 {
		t.Fatalf("meta binding = %+v source=%q ok=%v", b, source, ok)
	}
	if got := learnCommand(f); !reflect.DeepEqual(got, b.Command) {
		t.Fatalf("learnCommand = %v", got)
	}
}

func TestInspectMetaReportsTheMetaRoleBinding(t *testing.T) {
	f := metaRoleFormula(t, "  - {id: learn, type: learn}\n")
	want := []string{"/cli", "__adapter", "codex", "/codex"}
	for name, report := range map[string]MetaReport{
		"inspect":  InspectMeta(context.Background(), f, t.TempDir(), nil),
		"describe": DescribeFormulaMeta(f),
	} {
		if !reflect.DeepEqual(report.Command, want) || report.Binding != "codex" || report.TimeoutMS != 1000 {
			t.Fatalf("%s: %+v", name, report)
		}
		if report.Capability.State == "unavailable" {
			t.Fatalf("%s: the meta role was reported as unbound: %+v", name, report.Capability)
		}
	}
}

func TestTheLearnAtomTimeoutStillWinsOverTheMetaRole(t *testing.T) {
	f := metaRoleFormula(t, "  - {id: learn, type: learn, timeoutMs: 250}\n")
	if b, _, ok := MetaBinding(f); !ok || b.TimeoutMS != 250 {
		t.Fatalf("learning runs the Atom timeout when it declares one: %+v ok=%v", b, ok)
	}
	if report := DescribeFormulaMeta(f); report.TimeoutMS != 250 {
		t.Fatalf("report timeout = %d", report.TimeoutMS)
	}
}

func TestRoleCapabilitiesAndInspectRolesListTheMetaRole(t *testing.T) {
	f := metaRoleFormula(t, "  - {id: learn, type: learn}\n")
	var meta *RoleCapability
	for _, r := range RoleCapabilities(f) {
		if r.Role == string(formula.MetaHarness) {
			r := r
			meta = &r
		}
	}
	if meta == nil || meta.Binding != "codex" || meta.Source != "override" {
		t.Fatalf("role capabilities = %+v", RoleCapabilities(f))
	}
	found := false
	for _, r := range InspectRoles(context.Background(), f, t.TempDir(), nil) {
		if r.Role == string(formula.MetaHarness) {
			found = r.Binding == "codex" && r.Source == "override"
		}
	}
	if !found {
		t.Fatal("InspectRoles does not list the meta role")
	}
}

func TestAtomCommandOnlyFormulasAreUnchanged(t *testing.T) {
	learn := "  - {id: learn, type: learn, binding: byo, command: [/meta], timeoutMs: 1200}\n"
	text := "kind: formula\nid: atom-only\nintake: {repo: acme/widgets}\nharness: {binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 1000}\n" + readinessAtoms + learn
	f, err := formula.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	b, source, ok := MetaBinding(f)
	if !ok || source != "learn-atom" || b.Binding != "byo" || b.TimeoutMS != 1200 || !reflect.DeepEqual(b.Command, []string{"/meta"}) {
		t.Fatalf("%+v %q %v", b, source, ok)
	}
	if !LearnBound(f) || !reflect.DeepEqual(learnCommand(f), []string{"/meta"}) {
		t.Fatal("the learn Atom command is no longer the meta binding")
	}
	if report := DescribeFormulaMeta(f); report.Binding != "byo" || report.TimeoutMS != 1200 || !reflect.DeepEqual(report.Command, []string{"/meta"}) {
		t.Fatalf("%+v", report)
	}
	commandless, err := formula.Parse([]byte(strings.Replace(text, learn, "  - {id: learn, type: learn, binding: byo}\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if LearnBound(commandless) || learnCommand(commandless) != nil {
		t.Fatal("a commandless learn Atom without harnesses.meta must stay unbound")
	}
	if report := DescribeFormulaMeta(commandless); report.Binding != "byo" || len(report.Command) != 0 || report.Capability.State == "supported" {
		t.Fatalf("%+v", report)
	}
	none, err := formula.Parse([]byte(strings.Replace(text, learn, "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := MetaBinding(none); ok || LearnBound(none) {
		t.Fatal("a Formula without a learn Atom has no meta binding")
	}
}

func TestMetaOptInIsRefusedForAMetaRoleBinding(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	approveEdited(t, dir, func(doc map[string]any) {
		learn := learnAtomOf(t, doc)
		delete(learn, "command")
		delete(learn, "timeoutMs")
		doc["harnesses"] = map[string]any{"meta": map[string]any{"binding": "codex", "command": nativeCommand("/cli", "codex")}}
	})
	_, err := rebind(t, root, runner, dir, "claude-code", func(o *InitOptions) {
		o.Rebind, o.Preferred, o.MetaHarness = "", "claude-code", "codex"
	})
	if err == nil || !strings.Contains(err.Error(), "existing learn binding preserved") {
		t.Fatalf("a meta role binding must count as an existing learn binding: %v", err)
	}
}
