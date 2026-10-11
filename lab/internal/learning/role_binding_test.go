package learning

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

func withRoles(roles, learn string) string {
	return strings.Replace(recipe, "atoms:\n", roles+"atoms:\n", 1) + learn
}

func parseRoles(t *testing.T, yaml string) formula.Formula {
	t.Helper()
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// createCapturing runs create against an approved Formula and records the learn
// invocation without executing anything.
func createCapturing(t *testing.T, yaml string) (argv []string, timeout time.Duration, invoked bool, err error) {
	t.Helper()
	d, _ := fixture(t)
	approveLearning(t, d, yaml, "generation")
	r := molecule.Record{ID: "mol-1", SchemaVersion: "v0", Kind: "molecule", Mode: "ticket", FinishedAt: stamp(), FormulaApproved: true, FormulaID: "default", FormulaSnapshot: molecule.Snapshot{YAML: yaml, SHA256: hash(yaml)}}
	if err = os.MkdirAll(filepath.Join(d, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	if err = os.WriteFile(filepath.Join(d, "ledgers/mol-1.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = create(context.Background(), Options{LabDir: d, MoleculeIDs: []string{"mol-1"}}, func(_ context.Context, args []string, _ string, limit time.Duration, _ map[string]any) process.Result {
		argv, timeout, invoked = args, limit, true
		return process.Result{OK: false, Error: "stopped by the test"}
	})
	return
}

func TestLearnCommandFollowsTheDeclaredMetaRole(t *testing.T) {
	const metaRole = "harnesses:\n  meta: {binding: meta-role, command: [meta-role-cmd], timeoutMs: 4000}\n"
	const bare = "  - type: learn\n"
	for _, c := range []struct {
		name          string
		yaml          string
		wantArgv      []string
		wantTimeoutMS int
		wantError     string
	}{
		{"a commandless learn Atom runs the role command and timeout", withRoles(metaRole, bare), []string{"meta-role-cmd"}, 4000, ""},
		{"the Atom timeout wins over the role timeout", withRoles(metaRole, bare+"    timeoutMs: 2000\n"), []string{"meta-role-cmd"}, 2000, ""},
		{"a matching Atom command runs once", withRoles(metaRole, bare+"    command: [meta-role-cmd]\n    timeoutMs: 3000\n"), []string{"meta-role-cmd"}, 3000, ""},
		{"a conflicting Atom command refuses before invoking", withRoles(metaRole, bare+"    command: [other-learner]\n"), nil, 0, "conflicts with the command of learn Atom"},
		{"without the role the Atom command is used unchanged", withRoles("", bare+"    command: [atom-learner]\n    timeoutMs: 1500\n"), []string{"atom-learner"}, 1500, ""},
		{"without the role a commandless Atom is refused", withRoles("", bare), nil, 0, "explicit learn Atom command"},
		{"the coding role is never used for learning", withRoles("harnesses:\n  coding: {binding: coder, command: [coder-cmd]}\n", bare), nil, 0, "explicit learn Atom command"},
	} {
		t.Run(c.name, func(t *testing.T) {
			argv, timeout, invoked, err := createCapturing(t, c.yaml)
			if c.wantError != "" {
				if invoked || err == nil || !strings.Contains(err.Error(), c.wantError) {
					t.Fatalf("invoked=%t error=%v, want a refusal containing %q", invoked, err, c.wantError)
				}
				return
			}
			if want := time.Duration(c.wantTimeoutMS) * time.Millisecond; !invoked || !reflect.DeepEqual(argv, c.wantArgv) || timeout != want {
				t.Fatalf("invoked=%t argv=%v timeout=%v (error %v), want %v %v", invoked, argv, timeout, err, c.wantArgv, want)
			}
		})
	}
}

func TestBehaviorFollowsTheCodingRoleBinding(t *testing.T) {
	coding := func(command, extra string) string {
		return "harnesses:\n  coding: {binding: coder, command: [" + command + "]" + extra + "}\n"
	}
	same := func(name string, a, b string, want bool) {
		t.Helper()
		got := reflect.DeepEqual(behavior(parseRoles(t, a)), behavior(parseRoles(t, b)))
		if got != want {
			t.Fatalf("%s: behaviours equal = %t, want %t", name, got, want)
		}
	}
	base := withRoles(coding("coder-a", ", instructions: role guidance, timeoutMs: 500"), "")
	same("a changed coding role command is workflow behaviour", base, withRoles(coding("coder-b", ", instructions: role guidance, timeoutMs: 500"), ""), false)
	same("changed coding role instructions are workflow behaviour", base, withRoles(coding("coder-a", ", instructions: other guidance, timeoutMs: 500"), ""), false)
	same("changed coding role timeout is workflow behaviour", base, withRoles(coding("coder-a", ", instructions: role guidance, timeoutMs: 600"), ""), false)
	same("a change only to the overridden default harness is not behaviour", base, strings.Replace(base, "command: [echo]", "command: [echo, other]", 1), true)
	same("an omitted coding instruction follows the approved default", withRoles(coding("coder-a", ""), ""), strings.Replace(withRoles(coding("coder-a", ""), ""), "Check tests.", "Check more tests.", 1), false)
	same("an explicit coding instruction ignores the default", base, strings.Replace(base, "Check tests.", "Check more tests.", 1), true)
	same("a Formula without roles compares by its default harness", recipe, strings.Replace(recipe, "command: [echo]\n", "command: [echo, other]\n", 1), false)
	same("a Formula without roles ignores cosmetic edits", recipe, recipe+"# pretty\n", true)
}
