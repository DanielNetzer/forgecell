package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/DanielNetzer/forgecell/lab/internal/evaluation"
)

// runEvaluateCohort implements `forgecell evaluate --cohort FILE --source REPO --out DIR
// [--approve DIGEST]`, the cohort comparison of two Formula variants over a task cohort
// with repetitions. The existing single-task `--inputs` mode is unchanged; this file
// carries the whole cohort surface. args[0] is the "evaluate" verb.
func runEvaluateCohort(ctx context.Context, args []string, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	flags := flag.NewFlagSet("evaluate --cohort", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cohort := flags.String("cohort", "", "Reviewed cohort file")
	approve := flags.String("approve", "", "Fresh exact cohort activation digest")
	output := flags.String("out", "", "Evidence directory; created fresh, or resumed when its activation digest matches")
	source := flags.String("source", ".", "Repository checkout")
	recheck := flags.Bool("recheck", false, "Re-grade recorded attempts without rerunning a model")
	if err := flags.Parse(args[1:]); err != nil {
		return 1
	}
	if *cohort == "" || *output == "" || flags.NArg() != 0 {
		return fail(fmt.Errorf("evaluate --cohort requires --cohort FILE and --out DIR"))
	}
	if *recheck {
		if *approve != "" {
			return fail(fmt.Errorf("recheck cannot approve new model execution"))
		}
		// Cohort recheck is deferred in this slice (issue #150 decision 9). The retained
		// per-attempt evidence is untouched and can be regraded by a follow-up.
		return fail(fmt.Errorf("cohort --recheck is deferred; recorded attempts are retained under %s/attempts and are not regraded here", *output))
	}
	report, err := evaluation.RunCohort(ctx, evaluation.CohortOptions{Cohort: *cohort, SourceRoot: *source, Output: *output, Approve: *approve})
	if err != nil {
		return fail(err)
	}
	// Individual attempt failures are expected comparison data, not a command error; the
	// command succeeds whenever the cohort ran to completion or stopped on its stop rule.
	return encode(out, stderr, report)
}
