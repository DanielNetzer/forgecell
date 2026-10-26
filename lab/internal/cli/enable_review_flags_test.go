package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
)

func TestInitEnableReviewFlagConflictsAreRefused(t *testing.T) {
	approval := "assay-" + strings.Repeat("a", 64)
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"harness":          {[]string{"--harness", "codex"}, "--enable-review and --harness cannot be combined"},
		"rebind":           {[]string{"--rebind", "codex"}, "--enable-review and --rebind cannot be combined"},
		"meta harness":     {[]string{"--meta-harness", "codex"}, "--enable-review cannot be combined with --meta-harness or --meta-command"},
		"meta command":     {[]string{"--meta-command", `["/bin/meta"]`}, "--enable-review cannot be combined with --meta-harness or --meta-command"},
		"model":            {[]string{"--model", "claude-opus-4-8"}, "--enable-review cannot be combined with --model or --role"},
		"role":             {[]string{"--role", "coding"}, "--enable-review cannot be combined with --model or --role"},
		"harness timeout":  {[]string{"--harness-timeout-ms", "1000"}, "--enable-review cannot be combined with the timeout flags"},
		"max timeout":      {[]string{"--max-timeout-ms", "1000"}, "--enable-review cannot be combined with the timeout flags"},
		"refresh baseline": {[]string{"--refresh-baseline"}, "--enable-review and --refresh-baseline cannot be combined"},
		"approve":          {[]string{"--approve", approval}, "--enable-review proposes a change; review, approve or dismiss the saved proposal in a separate command"},
		"dismiss":          {[]string{"--dismiss", approval}, "--enable-review proposes a change"},
		"review":           {[]string{"--review", approval}, "--enable-review proposes a change"},
	} {
		t.Run(name, func(t *testing.T) {
			gsEnv(t, "")
			lab := t.TempDir()
			args := append([]string{"init", "--lab", lab, "--enable-review"}, c.args...)
			code, out, errOut := gsRun(args)
			if code != 1 || out != "" || !strings.Contains(errOut, c.want) {
				t.Fatalf("exit %d stdout %q stderr %q, want %q", code, out, errOut, c.want)
			}
			if entries, _ := os.ReadDir(lab); len(entries) != 0 {
				t.Fatalf("a refused --enable-review wrote %v", entries)
			}
		})
	}
}

func TestInitReviewShowsTheResolvedReviewer(t *testing.T) {
	gsEnv(t, "")
	lab := t.TempDir()
	reviewed := "kind: formula\nid: roles\nintake: {repo: acme/widgets}\nharness: {binding: codex, command: [/cli, __adapter, codex, /codex]}\natoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness, binding: codex}\n  - {id: check, type: check}\n  - {id: review, type: review}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n  - {id: learn, type: learn, binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 900000}\n"
	p, err := onboarding.SaveProposal(lab, reviewed)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := gsRun([]string{"init", "--lab", lab, "--review", p.ID})
	if code != 0 || !strings.Contains(out, "Reviewer: codex (from learn-atom)\n") || !strings.Contains(out, "WARNING: "+onboarding.SameReviewerWarning+"\n") {
		t.Fatalf("exit %d stdout %s stderr %s", code, out, errOut)
	}
	code, out, errOut = gsRun([]string{"init", "--lab", lab, "--review", p.ID, "--json"})
	if code != 0 || !strings.Contains(out, `"reviewer"`) || !strings.Contains(out, `"sameCommandAsCoding"`) {
		t.Fatalf("exit %d stdout %s stderr %s", code, out, errOut)
	}
}

func TestInitEnableReviewHelpLine(t *testing.T) {
	code, out, errOut := gsRun([]string{"--help"})
	if code != 0 || !strings.Contains(out, "\ninit --enable-review [--lab DIR] [--json]\n") {
		t.Fatalf("help exit %d lacks the --enable-review usage line: %s %s", code, out, errOut)
	}
}
