package cli

import (
	"context"
	"flag"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"io"
	"os"
)

// runLoop is a seam so tests can exercise rendering and exit codes without a
// full approved Lab.
var runLoop = molecule.RunLoop

// runLoopCommand validates every argument before it reads GitHub, writes a
// ledger or creates a Molecule. Exit codes mirror run: 1 for a loop-level error,
// 2 when a Molecule this run started or approved ended failed or blocked.
func runLoopCommand(ctx context.Context, args []string, defaultLab string, out, stderr io.Writer) int {
	flags := flag.NewFlagSet("loop", flag.ContinueOnError)
	flags.SetOutput(stderr)
	lab := flags.String("lab", defaultLab, "Lab directory")
	selection := flags.String("select", intake.SelectOldest, "Eligible Issues: oldest, label:NAME or milestone:TITLE")
	concurrent := flags.Int("max-concurrent", 1, "Molecules advanced at once (at least 1)")
	once := flags.Bool("once", false, "Make a single pass and exit")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("loop accepts flags only"))
	}
	selector, err := intake.ParseSelector(*selection)
	if err != nil {
		return fail(err)
	}
	if *concurrent < 1 {
		return fail(fmt.Errorf("--max-concurrent must be at least 1"))
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	report, err := runLoop(ctx, molecule.LoopOptions{Template: molecule.Options{LabDir: *lab, SourceRoot: cwd}, Select: selector, MaxConcurrent: *concurrent, Once: *once})
	renderLoop(out, report, selector, *concurrent)
	if err != nil {
		return fail(err)
	}
	if report.Failed() {
		return 2
	}
	return 0
}

func renderLoop(out io.Writer, report molecule.LoopReport, selector intake.Selector, concurrent int) {
	if report.Repository == "" {
		return
	}
	fmt.Fprintf(out, "Lab loop for %s · select %s · max concurrent %d\n", report.Repository, selector, concurrent)
	for n, pass := range report.Passes {
		fmt.Fprintf(out, "Pass %d: %d eligible Issue(s)\n", n+1, len(pass.Items))
		for _, note := range pass.Notes {
			fmt.Fprintf(out, "Warning: %s\n", note)
		}
		for _, item := range pass.Items {
			id := item.MoleculeID
			if id == "" {
				id = "none"
			}
			actions := ""
			if item.Started {
				actions += " · started intake"
			}
			if item.Approved {
				actions += " · approved by policy"
			}
			fmt.Fprintf(out, "%s#%d · %s · Molecule %s%s\n  %s\n", item.Repository, item.Issue, item.State, id, actions, item.Reason)
		}
	}
}
