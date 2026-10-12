package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const reviewHigh = `{"findings":[{"severity":"high","file":"lab/internal/x.go","line":12,"claim":"label check only","scenario":"a symlink passes the check","status":"confirmed","fix":"resolve then verify"}],"verifiedSound":["env filter"],"reviewedFiles":["lab/internal/x.go"]}`
const reviewClean = `{"findings":[],"verifiedSound":[],"reviewedFiles":[]}`

func hasPair(args []string, first, second string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == first && args[i+1] == second {
			return true
		}
	}
	return false
}

func TestReviewAtomDecodeStrict(t *testing.T) {
	out, err := DecodeReview([]byte(reviewHigh))
	if err != nil || len(out.Findings) != 1 || out.HighFindings() != 1 || out.Findings[0].Line != 12 || len(out.VerifiedSound) != 1 || len(out.ReviewedFiles) != 1 || out.ReviewedFiles[0] != "lab/internal/x.go" {
		t.Fatalf("%+v %v", out, err)
	}
	if out, err = DecodeReview([]byte(reviewClean)); err != nil || len(out.Findings) != 0 || out.HighFindings() != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err = DecodeReview([]byte(`{"findings":[{"severity":"low","file":"a.go","line":0,"claim":"c","scenario":"s","status":"plausible","fix":""}],"verifiedSound":[],"reviewedFiles":["a.go"]}`)); err != nil {
		t.Fatalf("line 0 and an empty fix are valid: %v", err)
	}
}

func TestReviewAtomDecodeFailsClosed(t *testing.T) {
	finding := func(key, value string) string {
		base := map[string]string{"severity": `"high"`, "file": `"a.go"`, "line": `3`, "claim": `"c"`, "scenario": `"s"`, "status": `"confirmed"`, "fix": `"f"`}
		var parts []string
		for _, k := range []string{"severity", "file", "line", "claim", "scenario", "status", "fix"} {
			v := base[k]
			if k == key {
				v = value
			}
			if v == "-" {
				continue
			}
			parts = append(parts, `"`+k+`":`+v)
		}
		return `{"findings":[{` + strings.Join(parts, ",") + `}],"verifiedSound":[],"reviewedFiles":[]}`
	}
	many := strings.Repeat(`{"severity":"low","file":"a","line":1,"claim":"c","scenario":"s","status":"plausible","fix":""},`, MaxReviewFindings+1)
	nulEscape := `"a` + "\\" + `u0000b"`
	cases := map[string]string{
		"empty":                "",
		"whitespace":           "  \n",
		"not json":             "I found no problems.",
		"array":                `[]`,
		"null":                 `null`,
		"trailing data":        reviewClean + ` {}`,
		"two documents":        reviewClean + reviewClean,
		"missing verified":     `{"findings":[],"reviewedFiles":[]}`,
		"missing reviewed":     `{"findings":[],"verifiedSound":[]}`,
		"missing findings":     `{"verifiedSound":[],"reviewedFiles":[]}`,
		"null findings":        `{"findings":null,"verifiedSound":[],"reviewedFiles":[]}`,
		"null verified":        `{"findings":[],"verifiedSound":null,"reviewedFiles":[]}`,
		"null reviewed":        `{"findings":[],"verifiedSound":[],"reviewedFiles":null}`,
		"unknown top field":    `{"findings":[],"verifiedSound":[],"reviewedFiles":[],"approved":true}`,
		"case variant top":     `{"Findings":[],"verifiedSound":[],"reviewedFiles":[]}`,
		"duplicate top key":    `{"findings":[],"findings":[],"verifiedSound":[],"reviewedFiles":[]}`,
		"duplicate reviewed":   `{"findings":[],"verifiedSound":[],"reviewedFiles":[],"reviewedFiles":["a.go"]}`,
		"duplicate nested key": `{"findings":[{"severity":"low","severity":"high","file":"a","line":1,"claim":"c","scenario":"s","status":"confirmed","fix":""}],"verifiedSound":[],"reviewedFiles":[]}`,
		"unknown finding key":  `{"findings":[{"severity":"low","file":"a","line":1,"claim":"c","scenario":"s","status":"confirmed","fix":"","extra":1}],"verifiedSound":[],"reviewedFiles":[]}`,
		"case variant nested":  `{"findings":[{"Severity":"low","severity":"low","file":"a","line":1,"claim":"c","scenario":"s","status":"confirmed","fix":""}],"verifiedSound":[],"reviewedFiles":[]}`,
		"missing fix":          finding("fix", "-"),
		"missing line":         finding("line", "-"),
		"null line":            finding("line", "null"),
		"null severity":        finding("severity", "null"),
		"bad severity":         finding("severity", `"critical"`),
		"case severity":        finding("severity", `"High"`),
		"bad status":           finding("status", `"maybe"`),
		"fractional line":      finding("line", "1.5"),
		"negative line":        finding("line", "-2"),
		"huge line":            finding("line", "99999999999"),
		"string line":          finding("line", `"3"`),
		"empty claim":          finding("claim", `"  "`),
		"empty scenario":       finding("scenario", `""`),
		"empty file":           finding("file", `""`),
		"oversize claim":       finding("claim", `"`+strings.Repeat("x", MaxReviewTextRunes+1)+`"`),
		"oversize file":        finding("file", `"`+strings.Repeat("x", MaxReviewFileBytes+1)+`"`),
		"nul in claim":         finding("claim", nulEscape),
		"too many findings":    `{"findings":[` + strings.TrimSuffix(many, ",") + `],"verifiedSound":[],"reviewedFiles":[]}`,
		"too many sound":       `{"findings":[],"verifiedSound":[` + strings.TrimSuffix(strings.Repeat(`"x",`, MaxReviewVerified+1), ",") + `],"reviewedFiles":[]}`,
		"too many reviewed":    `{"findings":[],"verifiedSound":[],"reviewedFiles":[` + strings.TrimSuffix(strings.Repeat(`"a.go",`, MaxReviewedFiles+1), ",") + `]}`,
		"empty reviewed entry": `{"findings":[],"verifiedSound":[],"reviewedFiles":[""]}`,
		"non string reviewed":  `{"findings":[],"verifiedSound":[],"reviewedFiles":[1]}`,
		"oversize reviewed":    `{"findings":[],"verifiedSound":[],"reviewedFiles":["` + strings.Repeat("x", MaxReviewFileBytes+1) + `"]}`,
		"empty sound entry":    `{"findings":[],"verifiedSound":[""],"reviewedFiles":[]}`,
		"non string sound":     `{"findings":[],"verifiedSound":[1],"reviewedFiles":[]}`,
		"oversize output":      `{"findings":[],"verifiedSound":["` + strings.Repeat("x", MaxReviewOutputBytes) + `"],"reviewedFiles":[]}`,
		"invalid utf8":         "{\"findings\":[],\"verifiedSound\":[\"\xff\"],\"reviewedFiles\":[]}",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if out, err := DecodeReview([]byte(raw)); err == nil {
				t.Fatalf("accepted malformed review output as %+v", out)
			}
		})
	}
}

func TestReviewAtomDisplayTextIsBoundedAndEscaped(t *testing.T) {
	hostile := "ok \x1b[31mred\x1b[0m \x1b]0;title\x07 tail\x00\r\n\tline‮evil x2J " + strings.Repeat("y", 400)
	got := ReviewDisplayText(hostile, 100)
	if strings.ContainsFunc(got, func(r rune) bool { return r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x202e || r == 0x2028 }) || strings.Contains(got, "[31m") || strings.Contains(got, "title") {
		t.Fatalf("control or escape content survived: %q", got)
	}
	if n := len([]rune(got)); n > 100 || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "ok red tail line evil x 2J") {
		t.Fatalf("not bounded or mangled: %d %q", n, got)
	}
	if ReviewDisplayText("a\xffb", 10) != "ab" {
		t.Fatal("invalid UTF-8 survived")
	}
	clean := SanitizeReview(ReviewOutcome{Findings: []ReviewFinding{{Severity: "medium", File: "a\x1b[2Jb.go", Line: 1, Claim: "c\nd", Scenario: "s\x00", Status: "plausible", Fix: strings.Repeat("z", 5000)}}, VerifiedSound: []string{"ok\x1b[0m\n"}})
	f := clean.Findings[0]
	if f.File != "ab.go" || f.Claim != "c d" || f.Scenario != "s" || len([]rune(f.Fix)) > MaxReviewTextRunes || clean.VerifiedSound[0] != "ok" {
		t.Fatalf("%+v", clean)
	}
}

func TestReviewAtomFollowUpsDraftMediumAndLowOnly(t *testing.T) {
	drafts := ReviewFollowUps([]ReviewFinding{
		{Severity: "high", File: "a.go", Line: 1, Claim: "blocker", Scenario: "s", Status: "confirmed", Fix: "f"},
		{Severity: "medium", File: "b.go", Line: 7, Claim: "medium claim", Scenario: "m scenario", Status: "plausible", Fix: "fix it"},
		{Severity: "low", File: "c.go", Line: 0, Claim: "low claim", Scenario: "l scenario", Status: "confirmed", Fix: ""},
	})
	if len(drafts) != 2 || !strings.HasPrefix(drafts[0].Title, "[review medium] medium claim") || !strings.Contains(drafts[0].Body, "b.go:7") || !strings.Contains(drafts[0].Body, "Suggested fix: fix it") {
		t.Fatalf("%+v", drafts)
	}
	if strings.Contains(drafts[1].Body, "c.go:0") || strings.Contains(drafts[1].Body, "Suggested fix") || !strings.Contains(drafts[1].Body, "must confirm it before it is filed") {
		t.Fatalf("%+v", drafts[1])
	}
}

func TestReviewAtomReadOnlyFlagsPerAdapter(t *testing.T) {
	request := map[string]any{"kind": ReviewKind, "issue": map[string]any{"title": "x"}}
	codex, err := BuildInvocation("codex", request, "/out/result.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"--sandbox", "read-only"}, {"-c", `approval_policy="never"`}, {"-c", `web_search="disabled"`}, {"-c", `notify=[]`}, {"--output-schema", "/out/result.txt.schema.json"}} {
		if !hasPair(codex.Args, pair[0], pair[1]) {
			t.Fatalf("codex review lacks %v: %v", pair, codex.Args)
		}
	}
	for _, flag := range []string{"--ephemeral", "--strict-config", "--ignore-rules", "--skip-git-repo-check"} {
		if !slices.Contains(codex.Args, flag) {
			t.Fatalf("codex review lacks %s: %v", flag, codex.Args)
		}
	}
	for _, feature := range []string{"shell_tool", "unified_exec", "apps", "plugins", "hooks", "multi_agent", "browser_use", "computer_use", "image_generation", "view_image", "code_mode_host", "skill_mcp_dependency_install", "shell_snapshot"} {
		if !hasPair(codex.Args, "--disable", feature) {
			t.Fatalf("codex review does not disable %s: %v", feature, codex.Args)
		}
	}
	if slices.Contains(codex.Args, "workspace-write") || slices.Contains(codex.Args, "danger-full-access") {
		t.Fatalf("codex review is not read-only: %v", codex.Args)
	}
	claude, err := BuildInvocation("claude-code", request, "/out/result.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"--tools", ""}, {"--mcp-config", `{"mcpServers":{}}`}, {"--permission-mode", "dontAsk"}, {"--json-schema", ReviewSchema()}} {
		if !hasPair(claude.Args, pair[0], pair[1]) {
			t.Fatalf("claude review lacks %v: %v", pair, claude.Args)
		}
	}
	for _, flag := range []string{"--strict-mcp-config", "--safe-mode", "--no-chrome", "--disable-slash-commands", "--no-session-persistence"} {
		if !slices.Contains(claude.Args, flag) {
			t.Fatalf("claude review lacks %s: %v", flag, claude.Args)
		}
	}
	for _, flag := range []string{"--allowedTools", "--setting-sources", "--disallowedTools"} {
		if slices.Contains(claude.Args, flag) {
			t.Fatalf("claude review grants %s: %v", flag, claude.Args)
		}
	}
	for _, call := range []Invocation{codex, claude} {
		if !strings.HasPrefix(call.Input, reviewInstruction) || strings.Contains(call.Input, "You are the coding harness") {
			t.Fatalf("review used another instruction: %.80s", call.Input)
		}
	}
	if _, err = BuildInvocation("cursor", request, "/out/result.txt"); err == nil || !strings.Contains(err.Error(), "Cursor review is unavailable") {
		t.Fatalf("cursor review must be unsupported: %v", err)
	}
	if c := ReviewCapability("cursor"); c.State != "unsupported" {
		t.Fatalf("%+v", c)
	}
	for _, id := range []string{"codex", "claude-code"} {
		if c := ReviewCapability(id); c.State != "supported" {
			t.Fatalf("%s: %+v", id, c)
		}
	}
	if c := ReviewCapability("custom"); c.State != "unknown" {
		t.Fatalf("%+v", c)
	}
}

func TestReviewAtomRejectsExtensions(t *testing.T) {
	servers := []MCPServer{{Name: "graphify", Command: "/priv/graphify", Args: []string{"mcp"}, Tools: []string{"query_graph"}}}
	for _, id := range []string{"codex", "claude-code", "cursor"} {
		for _, attached := range []any{servers, []MCPServer{}, []any{}} {
			if _, err := BuildInvocation(id, map[string]any{"kind": ReviewKind, "mcpServers": attached}, "/out"); err == nil {
				t.Fatalf("%s review accepted an mcpServers request %v", id, attached)
			}
		}
	}
}

func TestReviewAtomSchemaIsStrict(t *testing.T) {
	schema := ReviewSchema()
	for _, want := range []string{`"additionalProperties":false`, `"enum":["high","medium","low"]`, `"enum":["confirmed","plausible"]`, `"required":["findings","verifiedSound","reviewedFiles"]`, `"required":["severity","file","line","claim","scenario","status","fix"]`} {
		if !strings.Contains(schema, want) {
			t.Fatalf("schema lacks %s: %s", want, schema)
		}
	}
}

func TestReviewAtomCustomBindingIsRefusedWithoutExecution(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "called")
	command := []string{"sh", "-c", "touch " + marker}
	request := map[string]any{"kind": ReviewKind}
	if r := InvokeBinding(context.Background(), command, dir, time.Second, request); r.OK || !strings.Contains(r.Error, "no verified read-only review protocol") {
		t.Fatalf("InvokeBinding: %+v", r)
	}
	if r := InvokeReviewBinding(context.Background(), command, dir, time.Second, request, nil); r.OK || !strings.Contains(r.Error, "no verified read-only review protocol") {
		t.Fatalf("InvokeReviewBinding: %+v", r)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a custom command was invoked as review")
	}
	if r := InvokeReviewBinding(context.Background(), []string{"x", "__adapter", "claude-code", "claude"}, dir, time.Second, map[string]any{"kind": "molecule"}, nil); r.OK || !strings.Contains(r.Error, "only accepts review") {
		t.Fatalf("%+v", r)
	}
}

func TestReviewAtomEnvironmentExcludesGitHubAndCITokens(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin:/bin", "HOME=/home/x", "ANTHROPIC_API_KEY=sk-ant", "CLAUDE_CODE_OAUTH_TOKEN=oauth", "OPENAI_API_KEY=sk-open", "CODEX_HOME=/c",
		"GH_TOKEN=gh", "GITHUB_TOKEN=ghs", "GITHUB_ENTERPRISE_TOKEN=e", "GH_ENTERPRISE_TOKEN=e", "GITLAB_TOKEN=gl", "CI_JOB_TOKEN=ci", "CI=true", "ACTIONS_RUNTIME_TOKEN=a",
		"RUNNER_TOKEN=r", "NPM_TOKEN=n", "NODE_AUTH_TOKEN=n", "AWS_ACCESS_KEY_ID=a", "AWS_SECRET_ACCESS_KEY=s", "AWS_SESSION_TOKEN=t", "AWS_PROFILE=p",
		"PLAN_SECRET=plan", "SSH_AUTH_SOCK=/agent", "CODEX_GITHUB_TOKEN=x", "malformed", "=novalue", "EMPTY=",
	}
	got := ReviewEnvironment(environ, []string{"PLAN_SECRET", "OPENAI_API_KEY"})
	want := []string{"PATH=/usr/bin:/bin", "HOME=/home/x", "ANTHROPIC_API_KEY=sk-ant", "CLAUDE_CODE_OAUTH_TOKEN=oauth", "CODEX_HOME=/c", "AWS_PROFILE=p"}
	if !slices.Equal(got, want) {
		t.Fatalf("review environment = %v, want %v", got, want)
	}
	if empty := ReviewEnvironment(nil, nil); empty == nil || len(empty) != 0 {
		t.Fatalf("an empty review environment must be non-nil so the parent is not inherited: %#v", empty)
	}
}

// reviewProvider is a fake claude executable. It answers the capability probe,
// records its environment, argv and stdin, and prints one structured result.
func reviewProvider(t *testing.T, result string) (executable, envFile, argvFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	envFile, argvFile, stdinFile = filepath.Join(dir, "env.txt"), filepath.Join(dir, "argv.txt"), filepath.Join(dir, "stdin.txt")
	body := `if [ "$1" = "--help" ]; then printf '%s' '--safe-mode --tools --strict-mcp-config --json-schema --no-chrome --disable-slash-commands'; exit 0; fi
env > '` + envFile + `'
printf '%s\n' "$@" > '` + argvFile + `'
cat > '` + stdinFile + `'
printf '%s' '` + result + `'
`
	return fake(t, body), envFile, argvFile, stdinFile
}

func TestReviewAtomProviderNeverReceivesCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "gh-secret-token")
	t.Setenv("GITHUB_TOKEN", "github-secret-token")
	t.Setenv("CI_JOB_TOKEN", "ci-secret-token")
	t.Setenv("NPM_TOKEN", "npm-secret-token")
	t.Setenv("PLAN_CREDENTIAL", "plan-secret-value")
	t.Setenv("ANTHROPIC_API_KEY", "provider-auth-kept")
	executable, envFile, _, stdinFile := reviewProvider(t, `{"structured_output":`+reviewHigh+`}`)
	request := map[string]any{"kind": ReviewKind, "issue": map[string]any{"title": "x"}}
	for name, run := range map[string]func() Execution{
		"nil environment": func() Execution {
			return Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: t.TempDir(), Request: request, Timeout: 30 * time.Second, WithheldEnv: []string{"PLAN_CREDENTIAL"}})
		},
		"full environment supplied": func() Execution {
			return Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: t.TempDir(), Request: request, Env: os.Environ(), Timeout: 30 * time.Second, WithheldEnv: []string{"PLAN_CREDENTIAL"}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := run()
			if !r.Process.OK || strings.TrimSpace(r.Text) == "" {
				t.Fatalf("%+v", r)
			}
			raw, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatal(err)
			}
			seen := string(raw)
			for _, leaked := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CI_JOB_TOKEN", "NPM_TOKEN", "PLAN_CREDENTIAL", "gh-secret-token", "github-secret-token", "ci-secret-token", "npm-secret-token", "plan-secret-value"} {
				if strings.Contains(seen, leaked) {
					t.Fatalf("review provider received %s:\n%s", leaked, seen)
				}
			}
			if !strings.Contains(seen, "ANTHROPIC_API_KEY=provider-auth-kept") || !strings.Contains(seen, "PATH=") {
				t.Fatalf("provider authentication or PATH was dropped:\n%s", seen)
			}
		})
	}
	stdin, err := os.ReadFile(stdinFile)
	if err != nil || !strings.HasPrefix(string(stdin), reviewInstruction) {
		t.Fatalf("review prompt not delivered on stdin: %v", err)
	}
}

func TestReviewAtomInvokeBindingFiltersEnvironmentAndReturnsFindings(t *testing.T) {
	t.Setenv("GH_TOKEN", "gh-secret-token")
	t.Setenv("GITHUB_TOKEN", "github-secret-token")
	executable, envFile, argvFile, _ := reviewProvider(t, `{"structured_output":`+reviewHigh+`}`)
	launcher, _ := fakeBuild(t, "1.0.0", AdapterProtocolVersion)
	request := map[string]any{"kind": ReviewKind}
	for name, run := range map[string]func() (bool, string){
		"InvokeBinding": func() (bool, string) {
			p := InvokeBinding(context.Background(), []string{launcher, "__adapter", "claude-code", executable}, t.TempDir(), 30*time.Second, request)
			return p.OK, p.Stdout
		},
		"InvokeReviewBinding": func() (bool, string) {
			p := InvokeReviewBinding(context.Background(), []string{launcher, "__adapter", "claude-code", executable}, t.TempDir(), 30*time.Second, request, nil)
			return p.OK, p.Stdout
		},
	} {
		t.Run(name, func(t *testing.T) {
			ok, out := run()
			if !ok {
				t.Fatal("review failed")
			}
			if review, err := DecodeReview([]byte(out)); err != nil || review.HighFindings() != 1 {
				t.Fatalf("%q %v", out, err)
			}
			env, _ := os.ReadFile(envFile)
			if strings.Contains(string(env), "GH_TOKEN") || strings.Contains(string(env), "GITHUB_TOKEN") {
				t.Fatalf("tokens reached the review provider:\n%s", env)
			}
			argv, _ := os.ReadFile(argvFile)
			for _, want := range []string{"--tools", "--safe-mode", "--strict-mcp-config", "dontAsk"} {
				if !strings.Contains(string(argv), want) {
					t.Fatalf("argv lacks %s:\n%s", want, argv)
				}
			}
		})
	}
}

func TestReviewAtomExecuteMalformedOutputFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"prose":             `{"result":"No issues found."}`,
		"wrong enum":        `{"structured_output":{"findings":[{"severity":"critical","file":"a","line":1,"claim":"c","scenario":"s","status":"confirmed","fix":""}],"verifiedSound":[],"reviewedFiles":[]}}`,
		"missing field":     `{"structured_output":{"findings":[]}}`,
		"missing reviewed":  `{"structured_output":{"findings":[],"verifiedSound":[]}}`,
		"unknown field":     `{"structured_output":{"findings":[],"verifiedSound":[],"reviewedFiles":[],"approved":true}}`,
		"empty result":      `{"result":""}`,
		"provider error":    `{"is_error":true,"result":"x"}`,
		"permission denial": `{"structured_output":{"findings":[],"verifiedSound":[],"reviewedFiles":[]},"permission_denials":[{"tool_name":"Bash","tool_input":{"command":"ls"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			executable, _, _, _ := reviewProvider(t, body)
			r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": ReviewKind}, Timeout: 30 * time.Second})
			if r.Process.OK || r.Process.Error == "" {
				t.Fatalf("malformed review output was accepted: %+v", r)
			}
		})
	}
	executable := fake(t, "exit 7\n")
	if r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": ReviewKind}, Timeout: 30 * time.Second}); r.Process.OK {
		t.Fatalf("a failed provider produced a review: %+v", r)
	}
	executable, _, _, _ = reviewProvider(t, `{"structured_output":{"findings":[],"verifiedSound":[],"reviewedFiles":[]}}`)
	if r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": ReviewKind}, Timeout: 30 * time.Second}); !r.Process.OK {
		t.Fatalf("a clean review must pass: %+v", r)
	}
}

func TestReviewAtomProviderFailsClosedWithoutVerifiedControls(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	executable := fake(t, `if [ "$1" = "--help" ]; then printf '%s' '--tools'; exit 0; fi
touch '`+started+`'
`)
	r := Execute(context.Background(), ExecuteOptions{ID: "claude-code", Executable: executable, Dir: t.TempDir(), Request: map[string]any{"kind": ReviewKind}, Timeout: 30 * time.Second})
	if r.Process.OK || !strings.Contains(r.Process.Error, "lacks required analysis control") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(started); !os.IsNotExist(err) {
		t.Fatal("review provider started without verified isolation controls")
	}
}

func TestReviewAtomRedactSecrets(t *testing.T) {
	got := RedactSecrets("api_key=abcdef123456 and ghp_" + strings.Repeat("a", 30))
	if strings.Contains(got, "abcdef123456") || strings.Contains(got, "ghp_aaaa") {
		t.Fatalf("%q", got)
	}
}

func TestReviewAtomRedactSecretsCounted(t *testing.T) {
	clean, n := RedactSecretsCounted("nothing secret here")
	if n != 0 || clean != "nothing secret here" {
		t.Fatalf("%q %d", clean, n)
	}
	got, n := RedactSecretsCounted("api_key=abcdef123456 and ghp_" + strings.Repeat("a", 30))
	if n < 2 || strings.Contains(got, "abcdef123456") || strings.Contains(got, "ghp_aaaa") {
		t.Fatalf("%q %d", got, n)
	}
	if _, n = RedactSecretsCounted("already [REDACTED] text"); n != 0 {
		t.Fatalf("an existing marker was counted as a new redaction: %d", n)
	}
}

func TestReviewAtomMissingReviewedFiles(t *testing.T) {
	if got := MissingReviewedFiles([]string{"a.go", "b.go", "c.go"}, []string{"c.go", "a.go", "extra.go"}); len(got) != 1 || got[0] != "b.go" {
		t.Fatalf("%v", got)
	}
	if got := MissingReviewedFiles(nil, nil); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := MissingReviewedFiles([]string{"a.go"}, []string{}); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}
