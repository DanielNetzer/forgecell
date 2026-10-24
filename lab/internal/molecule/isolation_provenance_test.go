package molecule

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// dotClaudeFixture is the native Claude fixture with files committed to the source
// repository, and with the fake provider running extra shell after it edits code.txt.
// The provider is final before intake, because its bytes are part of the fingerprint.
func dotClaudeFixture(t *testing.T, committed map[string]string, providerShell string) (Options, string) {
	t.Helper()
	o, argv := claudeAllowlistFixture(t)
	provider := filepath.Join(filepath.Dir(o.SourceRoot), "claude")
	raw, err := os.ReadFile(provider)
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(string(raw), "printf changed > code.txt\n", "printf changed > code.txt\n"+providerShell+"\n", 1)
	if script == string(raw) && providerShell != "" {
		t.Fatal("fixture provider was not rewritten")
	}
	if err = writeStub(provider, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	commitToSource(t, o.SourceRoot, committed)
	return o, argv
}

func commitToSource(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if len(files) == 0 {
		return testGit(t, repo, "rev-parse", "HEAD")
	}
	// -f: a global gitignore commonly lists .claude/settings.local.json.
	testGit(t, repo, "add", "-f", "-A")
	testGit(t, repo, "commit", "-qm", "claude project files")
	return testGit(t, repo, "rev-parse", "HEAD")
}

func TestClaudeCodingIsRefusedWhenTheBaseCarriesProjectClaudeConfiguration(t *testing.T) {
	t.Parallel()
	for _, name := range []string{".claude/settings.json", ".claude/settings.local.json", ".claude/hooks/pre.sh", ".claude/agents/reviewer.md"} {
		t.Run(name, func(t *testing.T) {
			o, argv := dotClaudeFixture(t, map[string]string{name: "{}"}, "")
			r, err := Run(context.Background(), o)
			if err != nil || r.Readiness == nil || len(r.Readiness.Plans) != 1 {
				t.Fatalf("intake did not produce a plan: %+v %v", r, err)
			}
			o.Approve = r.Readiness.Plans[0].Digest
			_, err = Run(context.Background(), o)
			if err == nil {
				t.Fatal("a coding run started with project Claude configuration in the base")
			}
			for _, want := range []string{"coding refused", name, "--setting-sources project", "no coding harness was started", "git rm", "refresh intake"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal omits %q: %v", want, err)
				}
			}
			if _, statErr := os.Stat(argv); !os.IsNotExist(statErr) {
				t.Fatal("the provider started despite the refusal")
			}
			saved, err := findPending(o.LabDir, o.Approve)
			if err != nil || len(saved.HarnessAttempts) != 0 || saved.Readiness.Phase != "scope-waiting" {
				t.Fatalf("the refusal consumed the approval: %+v %v", saved.Readiness, err)
			}
		})
	}

	t.Run("other .claude content is not refused", func(t *testing.T) {
		o, argv := dotClaudeFixture(t, map[string]string{".claude/commands/build.md": "build", ".claude/skills/a/SKILL.md": "skill"}, "")
		r, err := runApprovedFixture(t, o)
		if err != nil || r.Status != "waiting" || len(r.HarnessAttempts) != 1 {
			t.Fatalf("%+v %v", r, err)
		}
		if _, err = os.Stat(argv); err != nil {
			t.Fatal("the provider did not run")
		}
	})

	t.Run("a custom command is not asked about Claude settings", func(t *testing.T) {
		o, _ := fixture(t)
		commitToSource(t, o.SourceRoot, map[string]string{".claude/settings.json": "{}"})
		r, err := runApprovedFixture(t, o)
		if err != nil || r.Status != "waiting" {
			t.Fatalf("%+v %v", r, err)
		}
	})
}

func TestClaudeCodingThatChangesDotClaudeFailsAsAScopeViolation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		committed map[string]string
		shell     string
		paths     []string
	}{
		{"a created settings file", nil, "mkdir -p .claude && printf '{}' > .claude/settings.json", []string{".claude/settings.json"}},
		{"a gitignored settings file", map[string]string{".gitignore": ".claude/settings.local.json\n"}, "mkdir -p .claude && printf '{}' > .claude/settings.local.json", []string{".claude/settings.local.json"}},
		{"a new hook", nil, "mkdir -p .claude/hooks && printf x > .claude/hooks/pre.sh", []string{".claude/hooks/pre.sh"}},
		{"an edited file", map[string]string{".claude/commands/build.md": "build"}, "printf edited > .claude/commands/build.md", []string{".claude/commands/build.md"}},
		{"a deleted file", map[string]string{".claude/commands/build.md": "build"}, "rm .claude/commands/build.md", []string{".claude/commands/build.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, argv := dotClaudeFixture(t, tc.committed, tc.shell)
			r, err := runApprovedFixture(t, o)
			if err != nil {
				t.Fatal(err)
			}
			if _, statErr := os.Stat(argv); statErr != nil {
				t.Fatal("the provider did not run")
			}
			if r.Status != "blocked" || r.Readiness == nil || r.Readiness.Phase != "scope-violation" {
				t.Fatalf("a run that changed .claude was not stopped as a scope violation: status %q %+v", r.Status, r.Readiness)
			}
			if r.Verification != nil || len(r.VerificationAttempts) != 0 {
				t.Fatalf("checks ran on a tree that changed .claude: %+v", r.Verification)
			}
			if len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].Capture == nil {
				t.Fatalf("%+v", r.HarnessAttempts)
			}
			for _, path := range tc.paths {
				if !slices.Contains(r.HarnessAttempts[0].Capture.Violations, path) {
					t.Fatalf("the capture does not name %s: %v", path, r.HarnessAttempts[0].Capture.Violations)
				}
				notes := strings.Join(r.Notes, "\n")
				if !strings.Contains(notes, "changed Claude project configuration under .claude ("+path) && !strings.Contains(notes, ", "+path) {
					t.Fatalf("the ledger notes do not say the .claude comparison found %s: %v", path, r.Notes)
				}
			}
		})
	}

	t.Run("a custom command is not held to the native .claude comparison", func(t *testing.T) {
		o, _ := fixture(t)
		script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
		raw, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.Replace(string(raw), "printf changed > code.txt\n", "printf changed > code.txt\nmkdir -p .claude && printf '{}' > .claude/settings.json\n", 1)
		if err = writeStub(script, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		r, err := runApprovedFixture(t, o)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(r.Notes, "\n"), "changed Claude project configuration") {
			t.Fatalf("the native comparison ran for a custom command: %v", r.Notes)
		}
	})
}

func TestProjectSettingsAreFingerprintedFromTheBaseCommit(t *testing.T) {
	t.Parallel()
	o, argv := dotClaudeFixture(t, nil, "")
	first := commitToSource(t, o.SourceRoot, nil)
	second := commitToSource(t, o.SourceRoot, map[string]string{".claude/settings.json": `{"permissions":{"allow":["Bash(ls)"]}}`})
	// The source checkout also carries an uncommitted edit, as an operator's does.
	if err := os.WriteFile(filepath.Join(o.SourceRoot, ".claude", "settings.json"), []byte(`{"hooks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o.Base = first

	r, err := Run(context.Background(), o)
	if err != nil || r.Readiness == nil || r.Workspace.BaseCommit != first {
		t.Fatalf("%+v %v", r, err)
	}
	roles, err := resolveExecutionRoles(r)
	if err != nil {
		t.Fatal(err)
	}
	binding := roles.bindings[formula.Coding]
	atFirst, err := bindingHash(binding, o.SourceRoot, first)
	if err != nil {
		t.Fatal(err)
	}
	atSecond, err := bindingHash(binding, o.SourceRoot, second)
	if err != nil {
		t.Fatal(err)
	}
	if atFirst == atSecond {
		t.Fatal("the settings committed at a base commit do not reach the binding hash")
	}
	if got := r.Readiness.Plans[0].Plan.Inputs.BindingSHA256; got != atFirst {
		t.Fatalf("intake hashed something other than the base commit's settings: %s, want %s", got, atFirst)
	}

	// The base commit has no project settings, so coding starts even though HEAD and
	// the working tree of the source checkout hold some.
	o.Approve = r.Readiness.Plans[0].Digest
	done, err := Run(context.Background(), o)
	if err != nil || done.Status != "waiting" || len(done.HarnessAttempts) != 1 {
		t.Fatalf("%+v %v", done, err)
	}
	if _, err = os.Stat(argv); err != nil {
		t.Fatal("the provider did not run")
	}
}

func TestStaleBindingNamesTheAdapterProtocolAndRefreshes(t *testing.T) {
	t.Parallel()
	o, argv := dotClaudeFixture(t, nil, "")
	r, err := Run(context.Background(), o)
	if err != nil || r.Readiness == nil {
		t.Fatalf("%+v %v", r, err)
	}
	if r.AdapterProtocol != harness.AdapterProtocolVersion {
		t.Fatalf("intake recorded adapter protocol %q", r.AdapterProtocol)
	}
	if raw, err := json.Marshal(r); err != nil || !strings.Contains(string(raw), `"adapterProtocol":"`+harness.AdapterProtocolVersion+`"`) {
		t.Fatalf("the ledger omits the adapter protocol: %v", err)
	}
	if raw, err := json.Marshal(Record{}); err != nil || strings.Contains(string(raw), "adapterProtocol") {
		t.Fatalf("a record without the field encodes it: %s %v", raw, err)
	}

	stale := func(recorded string) string {
		t.Helper()
		pending := r
		pending.AdapterProtocol = recorded
		return rewriteStoredPlan(t, pending, func(p *readiness.Plan) { p.Inputs.BindingSHA256 = strings.Repeat("a", 64) })
	}
	for _, tc := range []struct {
		name, recorded string
		want, unwanted string
	}{
		{"an earlier protocol", "2", "approved under adapter protocol 2; refresh intake after rebinding", ""},
		{"a ledger from before the protocol was recorded", "", "approved under adapter protocol 2 or earlier; refresh intake after rebinding", ""},
		{"the running protocol", harness.AdapterProtocolVersion, "readiness inputs changed; refresh intake before approval", "adapter protocol"},
	} {
		o.Approve = stale(tc.recorded)
		_, err := Run(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "readiness inputs changed") || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.unwanted != "" && strings.Contains(err.Error(), tc.unwanted) {
			t.Fatalf("%s: a binding change that is not about the protocol blamed it: %v", tc.name, err)
		}
		if _, statErr := os.Stat(argv); !os.IsNotExist(statErr) {
			t.Fatalf("%s: the provider started under a stale binding", tc.name)
		}
	}
}

func TestRunLedgerRecordsModelAndIsolationOfTheCodingAttempt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, pinned, model string }{
		{"unpinned", "", harness.ModelUnpinned},
		{"pinned", "model-fixture", "model-fixture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, argv := claudeAllowlistFixture(t)
			if tc.pinned != "" {
				file := filepath.Join(o.LabDir, "formulas", "sample.yaml")
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				recipe := strings.Replace(string(raw), "binding: claude-code\n", "binding: claude-code\n  model: "+tc.pinned+"\n", 1)
				if recipe == string(raw) {
					t.Fatal("fixture Formula was not rewritten")
				}
				if err = os.WriteFile(file, []byte(recipe), 0o600); err != nil {
					t.Fatal(err)
				}
				approveFixture(t, o.LabDir, recipe, "model-pin")
			}
			r, err := runApprovedFixture(t, o)
			if err != nil || len(r.HarnessAttempts) != 1 {
				t.Fatalf("%v %+v", err, r.HarnessAttempts)
			}
			raw, err := os.ReadFile(argv)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			if lineAfter(lines, "--setting-sources") != "project" || slices.Contains(lines, "user") {
				t.Fatalf("the provider did not run with project settings only: %q", lines)
			}
			if tc.pinned == "" && slices.Contains(lines, "--model") {
				t.Fatalf("an unpinned run passed a model: %q", lines)
			}
			modelFlags := 0
			for _, line := range lines {
				if line == "--model" {
					modelFlags++
				}
			}
			if tc.pinned != "" && (lineAfter(lines, "--model") != tc.pinned || modelFlags != 1) {
				t.Fatalf("the provider did not receive exactly one pinned model: %q", lines)
			}
			want := "claude-code:setting-sources=project"
			if a := r.HarnessAttempts[0]; a.Model != tc.model || a.Isolation != want {
				t.Fatalf("attempt records model %q isolation %q", a.Model, a.Isolation)
			}
			data, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved Record
			if err = json.Unmarshal(data, &saved); err != nil || len(saved.HarnessAttempts) != 1 {
				t.Fatalf("%v %+v", err, saved.HarnessAttempts)
			}
			if a := saved.HarnessAttempts[0]; a.Model != tc.model || a.Isolation != want {
				t.Fatalf("persisted ledger records model %q isolation %q", a.Model, a.Isolation)
			}
		})
	}
}

func TestHarnessAttemptRecordsModelAndIsolation(t *testing.T) {
	t.Parallel()
	claude := []string{"/forgecell", "__adapter", "claude-code", "/claude"}
	for _, tc := range []struct {
		name      string
		command   []string
		request   map[string]any
		model     string
		isolation string
	}{
		{"pinned coding", claude, harness.WithModel(map[string]any{"kind": "molecule"}, "model-fixture"), "model-fixture", "claude-code:setting-sources=project"},
		{"unpinned coding", claude, map[string]any{"kind": "molecule"}, "model unpinned", "claude-code:setting-sources=project"},
		{"unpinned analysis", claude, map[string]any{"kind": "ticket-analysis"}, "model unpinned", "claude-code:safe-mode"},
		{"codex coding", []string{"/forgecell", "__adapter", "codex", "/codex"}, map[string]any{"kind": "molecule"}, "model unpinned", "codex:user-config"},
		{"custom command", []string{"/custom"}, map[string]any{"kind": "molecule"}, "model unpinned", "custom:unknown"},
	} {
		var attempt HarnessAttempt
		attempt.RecordInvocation(tc.command, tc.request)
		if attempt.Model != tc.model || attempt.Isolation != tc.isolation {
			t.Fatalf("%s: model %q isolation %q", tc.name, attempt.Model, attempt.Isolation)
		}
	}
	if harness.ModelUnpinned != "model unpinned" {
		t.Fatalf("the unpinned provenance text changed: %q", harness.ModelUnpinned)
	}

	// An invocation that cannot be built records nothing instead of guessing.
	var refused HarnessAttempt
	refused.RecordInvocation([]string{"/forgecell", "__adapter", "codex", "/codex"}, harness.WithModel(map[string]any{"kind": "molecule"}, "model-fixture"))
	if refused.Model != "" || refused.Isolation != "" {
		t.Fatalf("%+v", refused)
	}

	recorded := HarnessAttempt{ID: "a", PlanDigest: "p"}
	recorded.RecordInvocation(claude, harness.WithModel(map[string]any{"kind": "molecule"}, "model-fixture"))
	raw, err := json.Marshal(Record{HarnessAttempts: []HarnessAttempt{recorded}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"model":"model-fixture"`) || !strings.Contains(string(raw), `"isolation":"claude-code:setting-sources=project"`) {
		t.Fatalf("the ledger record omits the model or isolation mode: %s", raw)
	}
	var decoded Record
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.HarnessAttempts) != 1 || !reflect.DeepEqual(decoded.HarnessAttempts[0], recorded) {
		t.Fatalf("the new fields do not survive a ledger round trip: %v %+v", err, decoded.HarnessAttempts)
	}

	// A ledger written before these fields decodes and re-encodes without them.
	historical, err := json.Marshal(HarnessAttempt{ID: "old", PlanDigest: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(historical), `"model"`) || strings.Contains(string(historical), `"isolation"`) {
		t.Fatalf("an attempt without the fields encodes them: %s", historical)
	}
	var old HarnessAttempt
	if err := json.Unmarshal(historical, &old); err != nil || old.Model != "" || old.Isolation != "" {
		t.Fatalf("%v %+v", err, old)
	}
	again, err := json.Marshal(old)
	if err != nil || string(again) != string(historical) {
		t.Fatalf("a historical attempt does not re-encode unchanged: %v\n%s\n%s", err, historical, again)
	}
}
