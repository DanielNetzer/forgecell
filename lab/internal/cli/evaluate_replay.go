package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/DanielNetzer/forgecell/lab/internal/evaluation"
)

// runEvaluateReplay implements `forgecell evaluate replay --repo OWNER/REPO --pr N [--pr M ...]
// --source CHECKOUT --out NEW_DIR`, building frozen evaluation task plans from merged PRs with
// hidden acceptance tests. args[0] is "evaluate" and args[1] is "replay". All GitHub access is
// read-only `gh api`; the builder writes only under --out.
func runEvaluateReplay(ctx context.Context, args []string, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	flags := flag.NewFlagSet("evaluate replay", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", "", "OWNER/REPO to read merged PRs from")
	source := flags.String("source", ".", "Repository checkout containing the PR history")
	output := flags.String("out", "", "New cohort directory to build")
	var prs []int
	flags.Func("pr", "Merged PR number (repeatable)", func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("invalid --pr %q", v)
		}
		prs = append(prs, n)
		return nil
	})
	if err := flags.Parse(args[2:]); err != nil {
		return 1
	}
	if *repo == "" || *output == "" || len(prs) == 0 || flags.NArg() != 0 {
		return fail(fmt.Errorf("evaluate replay requires --repo OWNER/REPO, at least one --pr N and --out DIR"))
	}
	report, err := evaluation.BuildReplay(ctx, evaluation.ReplayOptions{Repo: *repo, Source: *source, Out: *output, PRs: prs})
	if err != nil {
		return fail(err)
	}
	return encode(out, stderr, report)
}
