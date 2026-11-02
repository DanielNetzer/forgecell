package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// findingsUsage is the findings command's usage line.
const findingsUsage = "findings <molecule> [--lab DIR] [--collect] [--json]"

// runFindings shows a delivery's recorded reviewer findings, after collecting
// one round read-only with --collect. Findings are untrusted evidence: every
// line of their text is sanitized, and nothing here approves, pushes or
// changes the receipt.
func runFindings(ctx context.Context, args []string, defaultLab string, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fail(fmt.Errorf("findings requires a Molecule id; usage: forgecell %s", findingsUsage))
	}
	id := args[0]
	flags := flag.NewFlagSet("findings", flag.ContinueOnError)
	flags.SetOutput(stderr)
	lab := flags.String("lab", defaultLab, "Lab directory")
	collect := flags.Bool("collect", false, "Collect one findings round from the Formula's reviewers first")
	jsonMode := flags.Bool("json", false, "Print the findings record as JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("unexpected findings arguments; usage: forgecell %s", findingsUsage))
	}
	if !safeID.MatchString(id) {
		return fail(fmt.Errorf("invalid Molecule id"))
	}
	abs, err := filepath.Abs(*lab)
	if err != nil {
		return fail(fmt.Errorf("the Lab directory cannot be resolved"))
	}
	// The adapter needs the Lab's real path.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fail(fmt.Errorf("the Lab directory cannot be resolved"))
	}
	loaded, err := formula.Load(resolved, "")
	if err != nil || !loaded.Approved {
		return fail(fmt.Errorf("the Lab has no approved active Formula, so its reviewers are unknown"))
	}
	refs := loaded.Formula.Reviewers()
	if len(refs) == 0 {
		fmt.Fprintln(out, delivery.ErrNoReviewers.Error())
		return 0
	}
	var record delivery.FindingsRecord
	if *collect {
		if err := delivery.RefuseDemoLab(resolved); err != nil {
			return fail(err)
		}
		record, err = collectFindings(ctx, resolved, id, loaded.Formula, refs)
	} else {
		record, err = delivery.ReadFindings(resolved, id)
	}
	if err != nil {
		return fail(err)
	}
	if *jsonMode {
		return encode(out, stderr, record)
	}
	autoFix := map[string]string{}
	for i, r := range refs {
		autoFix[delivery.ReviewerKey(i, r.Plugin)] = r.AutoFix
	}
	presentFindings(out, id, record, delivery.Summarize(record, autoFix))
	return 0
}

// collectFindings starts the Formula's reviewers, and its forge plugin when it
// declares one, with one call log, and collects a round.
func collectFindings(ctx context.Context, lab, id string, f formula.Formula, refs []formula.ReviewerRef) (delivery.FindingsRecord, error) {
	calls := &delivery.PluginCallLog{}
	opts := mcpadapter.Options{LookupEnv: pluginLookupEnv, Observer: calls.Observe}
	list, err := mcpadapter.ReviewersFor(lab, f, opts)
	if err != nil {
		return delivery.FindingsRecord{}, err
	}
	if len(list) != len(refs) {
		return delivery.FindingsRecord{}, fmt.Errorf("the Formula's reviewers could not be started")
	}
	reviewers := make([]delivery.FindingsReviewer, len(list))
	for i, r := range list {
		reviewers[i] = delivery.FindingsReviewer{Plugin: refs[i].Plugin, Reviewer: r}
	}
	o := delivery.FindingsOptions{LabDir: lab, MoleculeID: id, Repo: f.Intake.Repo, Reviewers: reviewers, Calls: calls}
	if c, ok, err := mcpadapter.ForgeFor(lab, f, opts); err != nil {
		return delivery.FindingsRecord{}, err
	} else if ok {
		p, project, declared := f.ForgePlugin()
		if !declared {
			return delivery.FindingsRecord{}, fmt.Errorf("the Formula's forge plugin is not declared")
		}
		o.Changes, o.ForgePlugin, o.ForgeProject = c, p.Name, project
	}
	return delivery.Collect(ctx, o)
}

// presentFindings prints one line per open finding of the latest complete
// round (severity, path:line, title, reviewer), then the counts.
func presentFindings(out io.Writer, id string, record delivery.FindingsRecord, s delivery.FindingsSummary) {
	if len(record.Rounds) == 0 {
		fmt.Fprintf(out, "No findings collected for %s yet; run forgecell findings %s --collect.\n", id, id)
		return
	}
	if s.Round < 0 {
		fmt.Fprintln(out, "No complete findings round: every recorded round lost findings or input to a bound, so none is summarized.")
	} else {
		fmt.Fprintf(out, "Findings on commit %s (round %d of %d; untrusted evidence, never an approval):\n", safetext.Text(s.Commit, 64), s.Round+1, len(record.Rounds))
		for _, g := range s.Open {
			for _, f := range g.Findings {
				fmt.Fprintln(out, findingLine(f))
			}
		}
		for _, entry := range record.Rounds[s.Round].Reviewers {
			if entry.Error != "" {
				fmt.Fprintf(out, "reviewer %s: %s\n", flatten(entry.Reviewer), flatten(entry.Error))
			}
		}
	}
	counts := map[string]int{}
	for _, g := range s.Open {
		counts[g.Severity] = len(g.Findings)
	}
	fmt.Fprintf(out, "Open: %d (high %d, medium %d, low %d, info %d); resolved: %d", s.OpenCount(), counts["high"], counts["medium"], counts["low"], counts["info"], len(s.Resolved))
	if len(s.Pending) > 0 {
		fmt.Fprintf(out, "; pending: %s", strings.Join(flattenAll(s.Pending), ", "))
	}
	if len(s.Failed) > 0 {
		fmt.Fprintf(out, "; failed: %s", strings.Join(flattenAll(s.Failed), ", "))
	}
	fmt.Fprintln(out)
	if s.NewerIncomplete {
		fmt.Fprintln(out, "A newer round is incomplete (a bound cut its findings or input), so it is not summarized; see --json.")
	}
}

// findingLine is one open finding: severity, location, title and reviewer,
// with the autoFix mark when it is at or above the reviewer's threshold.
func findingLine(f delivery.SummaryFinding) string {
	location := "-"
	if f.Path != "" {
		location = safetext.Text(f.Path, 300)
		if f.Line > 0 {
			location += fmt.Sprintf(":%d", f.Line)
		}
	}
	title := flatten(f.Title)
	if title == "" {
		title = "(untitled)"
	}
	line := fmt.Sprintf("%-6s %s  %s  (%s)", f.Severity, location, title, flatten(f.Plugin))
	if f.AutoFix {
		line += " [autoFix]"
	}
	return line
}

func flattenAll(items []string) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = flatten(item)
	}
	return out
}
