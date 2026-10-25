package cli

import (
	"os"
	"strings"
	"testing"
)

func TestInitRefreshBaselineFlagConflictsAreRefused(t *testing.T) {
	approval := "assay-" + strings.Repeat("a", 64)
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"harness":           {[]string{"--harness", "codex"}, "--refresh-baseline and --harness cannot be combined; use --refresh-baseline for a baseline-only change"},
		"rebind":            {[]string{"--rebind", "codex"}, "--refresh-baseline and --rebind cannot be combined"},
		"meta harness":      {[]string{"--meta-harness", "codex"}, "--refresh-baseline cannot be combined with --meta-harness or --meta-command"},
		"meta command":      {[]string{"--meta-command", `["/bin/meta"]`}, "--refresh-baseline cannot be combined with --meta-harness or --meta-command"},
		"model":             {[]string{"--model", "claude-opus-4-8"}, "--refresh-baseline cannot be combined with --model or --role"},
		"role":              {[]string{"--role", "coding"}, "--refresh-baseline cannot be combined with --model or --role"},
		"harness timeout":   {[]string{"--harness-timeout-ms", "1000"}, "--refresh-baseline cannot be combined with the timeout flags"},
		"max timeout":       {[]string{"--max-timeout-ms", "1000"}, "--refresh-baseline cannot be combined with the timeout flags"},
		"approve":           {[]string{"--approve", approval}, "--refresh-baseline proposes a change; review, approve or dismiss the saved proposal in a separate command"},
		"dismiss":           {[]string{"--dismiss", approval}, "--refresh-baseline proposes a change"},
		"review":            {[]string{"--review", approval}, "--refresh-baseline proposes a change"},
		"harness and model": {[]string{"--model", "claude-opus-4-8", "--harness", "codex"}, "--refresh-baseline and --harness cannot be combined"},
	} {
		t.Run(name, func(t *testing.T) {
			gsEnv(t, "")
			lab := t.TempDir()
			args := append([]string{"init", "--lab", lab, "--refresh-baseline"}, c.args...)
			code, out, errOut := gsRun(args)
			if code != 1 || out != "" || !strings.Contains(errOut, c.want) {
				t.Fatalf("exit %d stdout %q stderr %q, want %q", code, out, errOut, c.want)
			}
			if entries, _ := os.ReadDir(lab); len(entries) != 0 {
				t.Fatalf("a refused refresh wrote %v", entries)
			}
		})
	}
}

func TestInitRefreshBaselineHelpLine(t *testing.T) {
	code, out, errOut := gsRun([]string{"--help"})
	if code != 0 || !strings.Contains(out, "\ninit --refresh-baseline [--lab DIR] [--json]\n") {
		t.Fatalf("help exit %d lacks the --refresh-baseline usage line: %s %s", code, out, errOut)
	}
}
