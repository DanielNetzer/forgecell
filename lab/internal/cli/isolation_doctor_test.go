package cli

import (
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
)

func isolationList(t *testing.T, report map[string]any, key string) []map[string]any {
	t.Helper()
	isolation, ok := report["isolation"].(map[string]any)
	if !ok {
		t.Fatalf("doctor JSON has no isolation object: %v", report)
	}
	list, ok := isolation[key].([]any)
	if !ok {
		t.Fatalf("isolation has no %s array: %v", key, isolation)
	}
	entries := []map[string]any{}
	for _, v := range list {
		entries = append(entries, v.(map[string]any))
	}
	return entries
}

func TestDoctorReportsAdapterIsolation(t *testing.T) {
	_, lab := doctorFixture(t)
	codex := argv(t, "/missing/forgecell", "__adapter", "codex", "/missing/codex")
	claude := argv(t, "/missing/forgecell", "__adapter", "claude-code", "/missing/claude")
	harnesses := "harnesses:\n  coding: {binding: claude-code, command: " + claude + ", model: model-fixture}\n  meta: {binding: custom, command: [/bin/custom]}\n"
	saveRoleFormula(t, lab, codex, harnesses)

	_, out := runDoctor(t, lab)
	report := decodeDoctor(t, out)
	adapters := map[string]map[string]any{}
	for _, entry := range isolationList(t, report, "adapters") {
		adapters[entry["adapter"].(string)] = entry
	}
	want := map[string]string{"claude-code": harness.IsolationPartial, "codex": harness.IsolationPartial, "cursor": harness.IsolationUnknown}
	if len(adapters) != len(want) {
		t.Fatalf("adapters: %v", adapters)
	}
	for id, state := range want {
		entry := adapters[id]
		if entry["state"] != state || strings.TrimSpace(entry["reason"].(string)) == "" {
			t.Fatalf("%s: %v", id, entry)
		}
	}
	for _, mention := range []string{"--setting-sources project", "managed (enterprise) policy settings", "user-level CLAUDE.md"} {
		if !strings.Contains(adapters["claude-code"]["reason"].(string), mention) {
			t.Fatalf("the claude-code reason omits %q: %v", mention, adapters["claude-code"])
		}
	}

	roles := map[string]map[string]any{}
	for _, entry := range isolationList(t, report, "roles") {
		roles[entry["role"].(string)] = entry
	}
	for role, expected := range map[string][3]string{
		"analysis": {"codex", harness.IsolationPartial, harness.ModelUnpinned},
		"coding":   {"claude-code", harness.IsolationPartial, "model-fixture"},
		"meta":     {"custom", harness.IsolationUnknown, harness.ModelUnpinned},
	} {
		entry := roles[role]
		if entry["binding"] != expected[0] || entry["state"] != expected[1] || entry["model"] != expected[2] || strings.TrimSpace(entry["reason"].(string)) == "" {
			t.Fatalf("%s role: %v", role, entry)
		}
	}
	if _, reported := roles["review"]; reported {
		t.Fatalf("a Formula without a review role reported one: %v", roles)
	}
	if !strings.Contains(roles["meta"]["reason"].(string), "custom command") {
		t.Fatalf("a custom command is not explained: %v", roles["meta"])
	}

	_, human := runDoctor(t, lab, "--verbose")
	for _, text := range []string{"Isolation from personal", "claude-code: partial — ", "codex: partial — ", "cursor: unknown — ", "coding role (claude-code binding): partial; model-fixture", "meta role (custom binding): unknown; model unpinned"} {
		if !strings.Contains(human, text) {
			t.Fatalf("the human view omits %q:\n%s", text, human)
		}
	}

	_, empty := runDoctor(t, t.TempDir())
	none := decodeDoctor(t, empty)
	if len(isolationList(t, none, "adapters")) != 3 || len(isolationList(t, none, "roles")) != 0 {
		t.Fatalf("without an active Formula doctor still reports adapter isolation and no roles: %v", none["isolation"])
	}
}

func TestExtensionAnalysisDescriptionNoLongerNamesUserSettings(t *testing.T) {
	e := newDoctorEnv(t)
	role := onboarding.RoleReadiness{Role: "analysis", Capability: harness.Capability{State: "conditional", Reason: "usual"}}
	describeAnalysisWithExtensions(&role, e.formula(t, e.digest))
	reason := role.Capability.Reason
	if !strings.Contains(reason, "--setting-sources project") || !strings.Contains(reason, "without safe mode") {
		t.Fatalf("%q", reason)
	}
	for _, stale := range []string{"--setting-sources user", "User-level", "user-level"} {
		if strings.Contains(reason, stale) {
			t.Fatalf("the description still says %q: %s", stale, reason)
		}
	}
}
