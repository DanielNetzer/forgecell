package harness

import (
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"go.yaml.in/yaml/v4"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestAdapterCompatibility(t *testing.T) {
	data, err := os.ReadFile("testdata/contracts.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID, Kind, Input string
		Args            []string
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.ID+"/"+c.Kind, func(t *testing.T) {
			got, err := BuildInvocation(c.ID, map[string]any{"kind": c.Kind}, "/fixture/result")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Args, c.Args) || got.Input != c.Input {
				t.Fatalf("adapter contract changed: %#v", got)
			}
		})
	}
}
func TestResultsRejectErrorsAndPermissionDenials(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `not json`, `{"result":""}`, `{"is_error":true,"result":"failed"}`, `{"result":"done","permission_denials":[{"tool":"Bash"}]}`} {
		if _, err := ReadResult("claude-code", raw, false); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	result, err := ReadResult("cursor", `{"result":"done"}`, false)
	if err != nil || result != "done" {
		t.Fatalf("%s %v", result, err)
	}
	if _, err := BuildInvocation("other", map[string]any{"kind": "molecule"}, "output"); err == nil {
		t.Fatal("unknown adapter accepted")
	}
	if _, err := BuildInvocation("codex", map[string]any{"kind": "other"}, "output"); err == nil {
		t.Fatal("unknown request accepted")
	}
}

func TestAnalysisEvidenceRequestContract(t *testing.T) {
	invocation, err := BuildInvocation("codex", map[string]any{"kind": "ticket-analysis"}, "output")
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"evidenceRequests", "one read-only supplemental pass", "Tree entries alone", "never grants scope approval", "Do not execute commands"} {
		if !strings.Contains(invocation.Input, phrase) {
			t.Fatalf("missing contract %s", phrase)
		}
	}
	var schema map[string]any
	if err = json.Unmarshal([]byte(readiness.AnalysisSchema()), &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	if props["evidenceRequests"] == nil {
		t.Fatal("request schema absent")
	}
}

func TestCapabilitiesMatchInvocationSupport(t *testing.T) {
	for _, id := range []string{"codex", "claude-code", "cursor"} {
		c := AdapterCapabilities(id)
		for _, op := range []struct {
			kind       string
			capability Capability
		}{{"molecule", c.Coding}, {"ticket-analysis", c.Analysis}, {"formula-improvement", c.Meta}} {
			_, err := BuildInvocation(id, map[string]any{"kind": op.kind}, "/fixture/output")
			if (err == nil) != (op.capability.State == "supported") {
				t.Fatalf("%s %s: %+v %v", id, op.kind, op.capability, err)
			}
			if err != nil && op.capability.Reason != err.Error() {
				t.Fatal("refusal reason differs")
			}
		}
		if c.ModelAccess.State != "unknown" || c.AnalysisControls.State != "unknown" {
			t.Fatal(c)
		}
	}
	if AdapterCapabilities("custom").Coding.State != "unknown" {
		t.Fatal("invented custom support")
	}
}

type testComponent struct {
	path, manifest string
	checks         [][]string
}

func contextFormula(t *testing.T, components ...testComponent) string {
	t.Helper()
	items := []map[string]any{}
	for _, c := range components {
		items = append(items, map[string]any{"path": c.path, "manifest": c.manifest, "suggestedChecks": c.checks})
	}
	raw, err := yaml.Marshal(map[string]any{"kind": "formula", "id": "sample", "repositoryContext": map[string]any{"components": items}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func joinEntries(parts ...[]string) []string {
	out := []string{}
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

var (
	wantBase   = []string{"Read", "Glob", "Grep", "Edit", "Write", "Bash(git status)", "Bash(git diff)"}
	wantPreset = []string{"Bash(npm test)", "Bash(npm run test *)", "Bash(npm run typecheck)", "Bash(npm run build)", "Bash(node --test *)"}
	goChecks   = [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}}
)

type allowlistCase struct {
	name       string
	components []testComponent
	checks     []readiness.Check
	want       []string
}

func allowlistCases() []allowlistCase {
	return []allowlistCase{
		{"go-root", []testComponent{{".", "go.mod", goChecks}}, nil, joinEntries(wantBase, []string{"Bash(go test ./...)", "Bash(go vet ./...)"})},
		{"go-nested", []testComponent{{"lab", "lab/go.mod", goChecks}}, nil, joinEntries(wantBase, []string{"Bash(go -C lab test ./...)", "Bash(go -C lab vet ./...)"})},
		{"node-npm", []testComponent{{".", "package.json", [][]string{{"npm", "run", "test"}, {"npm", "run", "typecheck"}, {"npm", "run", "build"}}}}, nil, joinEntries(wantBase, wantPreset, []string{"Bash(npm run test)"})},
		{"node-pnpm", []testComponent{{".", "package.json", [][]string{{"pnpm", "run", "test"}}}}, nil, joinEntries(wantBase, wantPreset, []string{"Bash(pnpm run test)"})},
		{"node-yarn-nested", []testComponent{{"web", "web/package.json", [][]string{{"yarn", "run", "test"}}}}, nil, joinEntries(wantBase, wantPreset, []string{"Bash(yarn --cwd web run test)"})},
		{"node-npm-nested", []testComponent{{"web", "web/package.json", [][]string{{"npm", "run", "test"}, {"npm", "run", "build"}}}}, nil, joinEntries(wantBase, wantPreset, []string{"Bash(npm --prefix web run build)", "Bash(npm --prefix web run test)"})},
		{"mixed", []testComponent{{"web", "web/package.json", [][]string{{"pnpm", "run", "test"}}}, {"lab", "lab/go.mod", goChecks}}, nil, joinEntries(wantBase, wantPreset, []string{"Bash(go -C lab test ./...)", "Bash(go -C lab vet ./...)", "Bash(pnpm --dir web run test)"})},
		{"plan-adds-check", []testComponent{{".", "go.mod", goChecks}}, []readiness.Check{{Argv: []string{"go", "test", "./internal/harness/"}, Dir: "."}}, joinEntries(wantBase, []string{"Bash(go test ./...)", "Bash(go test ./internal/harness/)", "Bash(go vet ./...)"})},
		{"plan-check-in-component-dir", []testComponent{{"lab", "lab/go.mod", goChecks}}, []readiness.Check{{Argv: []string{"go", "test", "./...", "-run", "Allowlist", "-count=1"}, Dir: "lab"}}, joinEntries(wantBase, []string{"Bash(go -C lab test ./... -run Allowlist -count=1)", "Bash(go -C lab test ./...)", "Bash(go -C lab vet ./...)"})},
		{"no-components", nil, []readiness.Check{{Argv: []string{"sh", "-c", `test -z "$(gofmt -l internal cmd)"`}, Dir: "lab"}}, wantBase},
		{"python-has-no-suggested-checks", []testComponent{{".", "pyproject.toml", [][]string{}}}, nil, wantBase},
	}
}

func TestAllowlistDerivation(t *testing.T) {
	for _, c := range allowlistCases() {
		t.Run(c.name, func(t *testing.T) {
			got, err := DeriveCodingAllowlist(contextFormula(t, c.components...), c.checks)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("allowlist\n got %q\nwant %q", got, c.want)
			}
		})
	}
	if got, err := DeriveCodingAllowlist("kind: formula\nid: sample\n", nil); err != nil || !reflect.DeepEqual(got, wantBase) {
		t.Fatalf("Formula without repositoryContext: %q %v", got, err)
	}
	if _, err := DeriveCodingAllowlist("repositoryContext: {components: nope}", nil); err == nil {
		t.Fatal("unreadable repositoryContext silently ignored")
	}
}

func TestAllowlistOmitsUnsafeAndPublishingCommands(t *testing.T) {
	unsafe := [][]string{
		{},
		{"sh", "-c", "go test ./..."},
		{"bash", "-c", "go test ./..."},
		{"/bin/sh", "-c", "true"},
		{"env", "go", "test", "./..."},
		{"xargs", "go"},
		{"sudo", "go", "test"},
		{"git", "commit", "-m", "x"},
		{"git", "push"},
		{"git", "push", "origin", "HEAD"},
		{"git", "status"},
		{"git", "diff", "--output=out"},
		{"gh", "pr", "create"},
		{"gh", "issue", "comment", "1"},
		{"npm", "publish"},
		{"pnpm", "publish"},
		{"yarn", "publish"},
		{"npm", "run", "release"},
		{"pnpm", "run", "publish:all"},
		{"npm", "login"},
		{"curl", "https://example.test"},
		{"wget", "https://example.test"},
		{"ssh", "host"},
		{"go", "test", "./...", "-run", "A*"},
		{"go", "test", "a,b"},
		{"go", "test", "a b"},
		{"go", "test", "$(id)"},
		{"go", "test", "`id`"},
		{"go", "test", "a;b"},
		{"go", "test", "a|b"},
		{"go", "test", "a&b"},
		{"go", "test", ">out"},
		{"go", "test", "<in"},
		{"go", "test", "a\nb"},
		{"go", "test", `"quoted"`},
		{"go", "test", "'quoted'"},
		{"go", "test", "(x)"},
		{"go", "test", "{input:acceptance}"},
		{"go", "test", ""},
		{"go", "test", "../outside"},
		{"go", "test", "/abs/path"},
		{"go test ./..."},
	}
	for _, argv := range unsafe {
		formula := contextFormula(t, testComponent{".", "go.mod", [][]string{argv}})
		got, err := DeriveCodingAllowlist(formula, []readiness.Check{{Argv: argv, Dir: "."}})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, wantBase) {
			t.Fatalf("%q reached the allowlist: %q", argv, got)
		}
	}
	for _, dir := range []string{"../escape", "/abs", "a b", "lab/../x", "lab*", "lab,x", "-flag"} {
		got, err := DeriveCodingAllowlist("kind: formula\nid: sample\n", []readiness.Check{{Argv: []string{"go", "test", "./..."}, Dir: dir}})
		if err != nil || !reflect.DeepEqual(got, wantBase) {
			t.Fatalf("directory %q reached the allowlist: %q %v", dir, got, err)
		}
	}
	got, err := DeriveCodingAllowlist("kind: formula\nid: sample\n", []readiness.Check{{Argv: []string{"make", "test"}, Dir: "."}, {Argv: []string{"make", "test"}, Dir: "lab"}})
	if err != nil || !reflect.DeepEqual(got, joinEntries(wantBase, []string{"Bash(make test)"})) {
		t.Fatalf("a command with no known directory flag must only render at the root: %q %v", got, err)
	}
}

func TestAllowlistNeverGrantsBareBashOrWildcards(t *testing.T) {
	preset := map[string]bool{}
	for _, entry := range wantPreset {
		preset[entry] = true
	}
	for _, c := range allowlistCases() {
		got, err := DeriveCodingAllowlist(contextFormula(t, c.components...), c.checks)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range got {
			if entry == "Bash" || entry == "Bash(*)" || strings.Contains(entry, ",") || strings.Contains(entry, "\n") {
				t.Fatalf("%s: unsafe entry %q", c.name, entry)
			}
			if strings.Contains(entry, "*") && !preset[entry] {
				t.Fatalf("%s: wildcard %q is not the retained Node preset", c.name, entry)
			}
			if strings.HasPrefix(entry, "Bash(git") && entry != "Bash(git status)" && entry != "Bash(git diff)" {
				t.Fatalf("%s: git entry %q", c.name, entry)
			}
		}
	}
}

func TestAllowlistIsDeterministicDeduplicatedAndIdempotent(t *testing.T) {
	components := []testComponent{{"lab", "lab/go.mod", goChecks}, {"web", "web/package.json", [][]string{{"npm", "run", "test"}, {"npm", "run", "test"}}}}
	checks := []readiness.Check{{Argv: []string{"go", "test", "./..."}, Dir: "lab"}, {Argv: []string{"go", "test", "./..."}, Dir: "lab"}, {Argv: []string{"go", "vet", "./..."}, Dir: "lab"}}
	first, err := DeriveCodingAllowlist(contextFormula(t, components...), checks)
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := DeriveCodingAllowlist(contextFormula(t, components[1], components[0]), []readiness.Check{checks[2], checks[1], checks[0]})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, reversed) {
		t.Fatalf("input order changed the allowlist:\n%q\n%q", first, reversed)
	}
	seen := map[string]bool{}
	for _, entry := range first {
		if seen[entry] {
			t.Fatalf("duplicate %q", entry)
		}
		seen[entry] = true
	}
	again, err := EffectiveCodingAllowlist(first)
	if err != nil || !reflect.DeepEqual(again, first) {
		t.Fatalf("canonicalization is not idempotent: %q %v", again, err)
	}
	shuffled := []string{first[len(first)-1], first[0]}
	shuffled = append(shuffled, first[1:len(first)-1]...)
	if again, err = EffectiveCodingAllowlist(shuffled); err != nil || !reflect.DeepEqual(again, first) {
		t.Fatalf("canonical order depends on input order: %q %v", again, err)
	}
	if got, err := EffectiveCodingAllowlist(nil); err != nil || !reflect.DeepEqual(got, wantBase) {
		t.Fatalf("absent allowlist must be base only: %q %v", got, err)
	}
}

func allowedTools(t *testing.T, args []string) string {
	t.Helper()
	for i, arg := range args {
		if arg == "--allowedTools" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("no --allowedTools in %q", args)
	return ""
}

func TestAllowlistBuildInvocation(t *testing.T) {
	legacy := "Bash(npm test),Bash(npm run test *),Bash(npm run typecheck),Bash(npm run build),Bash(node --test *)"
	base, err := BuildInvocation("claude-code", map[string]any{"kind": "molecule"}, "/fixture/result")
	if err != nil {
		t.Fatal(err)
	}
	if got := allowedTools(t, base.Args); got != strings.Join(wantBase, ",") || strings.Contains(got, legacy) {
		t.Fatalf("a request without an allowlist must not grant the Node constant: %q", got)
	}
	for _, c := range allowlistCases() {
		list, err := DeriveCodingAllowlist(contextFormula(t, c.components...), c.checks)
		if err != nil {
			t.Fatal(err)
		}
		generic := []any{}
		for _, entry := range list {
			generic = append(generic, entry)
		}
		for name, value := range map[string]any{"strings": list, "json-decoded": generic} {
			call, err := BuildInvocation("claude-code", map[string]any{"kind": "molecule", "codingAllowlist": value}, "/fixture/result")
			if err != nil {
				t.Fatalf("%s/%s: %v", c.name, name, err)
			}
			if got := allowedTools(t, call.Args); got != strings.Join(c.want, ",") {
				t.Fatalf("%s/%s: provider received %q", c.name, name, got)
			}
			if !strings.Contains(strings.Join(call.Args, " "), "--permission-mode dontAsk") {
				t.Fatal("dontAsk permission mode lost")
			}
		}
	}
	for _, entries := range [][]string{{"Bash"}, {"Bash(*)"}, {"Bash()"}, {"WebFetch"}, {"Read(*)"}, {"Bash(git push)"}, {"Bash(git commit -m x)"}, {"Bash(sh -c x)"}, {"Bash(gh pr create)"}, {"Bash(npm publish)"}, {"Bash(go test ./... *)"}, {"Bash(go  test)"}, {"Bash(go -C ../x test)"}, {""}} {
		if _, err := EffectiveCodingAllowlist(entries); err == nil {
			t.Fatalf("accepted %q", entries)
		}
		if _, err := BuildInvocation("claude-code", map[string]any{"kind": "molecule", "codingAllowlist": entries}, "/fixture/result"); err == nil {
			t.Fatalf("invocation accepted %q", entries)
		}
	}
	for _, value := range []any{"Read", 7, []any{"Read", 7}, map[string]any{}} {
		if _, err := BuildInvocation("claude-code", map[string]any{"kind": "molecule", "codingAllowlist": value}, "/fixture/result"); err == nil {
			t.Fatalf("invocation accepted %#v", value)
		}
	}
	if AdapterCapabilities("claude-code").Coding.State != "supported" {
		t.Fatal("capability probing must still report coding support from kind alone")
	}
}

func TestAllowlistLeavesOtherInvocationsUnchanged(t *testing.T) {
	supplied := []string{"Read", "Bash(go test ./...)"}
	for _, tc := range []struct{ id, kind string }{{"codex", "molecule"}, {"cursor", "molecule"}, {"codex", "ticket-analysis"}, {"claude-code", "ticket-analysis"}, {"codex", "formula-improvement"}, {"claude-code", "formula-improvement"}, {"cursor", "formula-improvement"}} {
		without, err := BuildInvocation(tc.id, map[string]any{"kind": tc.kind}, "/fixture/result")
		if err != nil {
			t.Fatal(err)
		}
		with, err := BuildInvocation(tc.id, map[string]any{"kind": tc.kind, "codingAllowlist": supplied}, "/fixture/result")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(with.Args, without.Args) {
			t.Fatalf("%s %s argv changed:\n%q\n%q", tc.id, tc.kind, with.Args, without.Args)
		}
	}
}
