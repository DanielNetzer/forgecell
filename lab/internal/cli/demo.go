package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/demo"
)

const (
	demoUsage = "demo [--keep DIR]"
	demoHelp  = "demo is the first command to run after install: it runs one complete Molecule (intake and plan, scope approval, coding, independent checks with the protected regression, review and a local delivery preview) against a built-in sample repository with a deterministic stub harness. It needs only git and go, makes no model calls, never runs gh or touches the network, and publishes nothing; its scope approval is the demo Formula's policy, labelled as such. The Lab is a temporary directory removed afterwards unless --keep DIR (absent, or an empty directory you own) keeps it for forgecell ledger ID --lab DIR."
)

// githubCommands reach GitHub (gh, git fetch or push, or the network) and so are
// refused in a Lab kept by forgecell demo, whose origin is never contacted.
var githubCommands = map[string]bool{"deliver": true, "run": true, "loop": true, "issues": true, "checks": true}

// refuseDemoLab refuses a GitHub command whose Lab is a demo Lab, before any
// process starts. Every --lab value on the command line is checked, not only the
// one the command's flags would keep, so no spelling slips past; without --lab
// the resolved default Lab is checked.
func refuseDemoLab(args []string, defaultLab string) error {
	if !githubCommands[args[0]] {
		return nil
	}
	labs := []string{}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		switch {
		case arg == "--lab" || arg == "-lab":
			if i+1 < len(args) {
				i++
				labs = append(labs, args[i])
			}
		case strings.HasPrefix(arg, "--lab="):
			labs = append(labs, strings.TrimPrefix(arg, "--lab="))
		case strings.HasPrefix(arg, "-lab="):
			labs = append(labs, strings.TrimPrefix(arg, "-lab="))
		}
	}
	if len(labs) == 0 {
		labs = append(labs, defaultLab)
	}
	for _, lab := range labs {
		if err := delivery.RefuseDemoLab(lab); err != nil {
			return err
		}
	}
	return nil
}

// runDemoWith runs the demo; a variable only so tests can observe the parsed options.
var runDemoWith = demo.Run

// runDemo is the demo command. The process-wide cleanup in main removes the
// temporary Lab on SIGINT or SIGTERM as well as on return; the demo removes it
// itself on return.
func runDemo(ctx context.Context, args []string, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell: demo:", err); return 1 }
	flags := flag.NewFlagSet("demo", flag.ContinueOnError)
	flags.SetOutput(stderr)
	keep := flags.String("keep", "", "Keep the demo Lab in DIR: absent, or an empty directory you own")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("demo takes no positional arguments; usage: forgecell %s", demoUsage))
	}
	keepSet := false
	flags.Visit(func(f *flag.Flag) { keepSet = keepSet || f.Name == "keep" })
	if keepSet && *keep == "" {
		return fail(fmt.Errorf("--keep needs a directory"))
	}
	if err := runDemoWith(ctx, demo.Options{Keep: *keep, Out: out}); err != nil {
		return fail(err)
	}
	return 0
}
