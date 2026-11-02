package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/fixround"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
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
	propose := flags.Bool("propose", false, "Propose the next fix round from the latest complete findings round on the head")
	var includes []string
	flags.Func("include", "With --propose, also propose this below-threshold finding fingerprint (repeatable)", func(value string) error {
		if !findingFingerprint.MatchString(value) {
			return fmt.Errorf("--include takes a finding fingerprint: 64 lower-case hex characters")
		}
		if len(includes) >= maxFixRoundIncludes {
			return fmt.Errorf("--include may be given at most %d times", maxFixRoundIncludes)
		}
		for _, fp := range includes {
			if fp == value {
				return nil
			}
		}
		includes = append(includes, value)
		return nil
	})
	push := flags.Bool("push", false, "Push the verified fix round to the same change request (preview without --approve)")
	approve := flags.String("approve", "", "With --push, push exactly this fix round preview digest")
	confirm := flags.Bool("confirm", false, "With --push, explicitly confirm a push whose acceptance is unverified or whose review is not green")
	title := flags.String("title", "", "With --push, the fix round commit message")
	dismiss := flags.String("dismiss", "", "Dismiss this finding fingerprint with --reason, so it is no longer open or proposed")
	reason := flags.String("reason", "", "With --dismiss, why the finding is dismissed (plain text, at most 500 characters)")
	undismiss := flags.String("undismiss", "", "Remove the dismissal of this finding fingerprint")
	if err := flags.Parse(args[1:]); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("unexpected findings arguments; usage: forgecell %s", findingsUsage))
	}
	if !safeID.MatchString(id) {
		return fail(fmt.Errorf("invalid Molecule id"))
	}
	if (*propose || *push) && (*collect || *jsonMode || (*propose && *push)) {
		return fail(fmt.Errorf("--propose and --push are used alone, without --collect or --json"))
	}
	if len(includes) > 0 && !*propose {
		return fail(fmt.Errorf("--include requires --propose"))
	}
	if (*approve != "" || *confirm || *title != "") && !*push {
		return fail(fmt.Errorf("--approve, --confirm and --title require --push"))
	}
	// A flag given an empty value still counts as given, so it is refused
	// instead of silently showing the findings.
	given := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { given[f.Name] = true })
	dismissing, undismissing := given["dismiss"], given["undismiss"]
	if (dismissing || undismissing) && (dismissing && undismissing || *collect || *jsonMode || *propose || *push) {
		return fail(fmt.Errorf("--dismiss and --undismiss are used alone, without --collect, --json, --propose or --push"))
	}
	if *reason != "" && !dismissing {
		return fail(fmt.Errorf("--reason requires --dismiss"))
	}
	if dismissing && *reason == "" {
		return fail(fmt.Errorf("--dismiss requires --reason"))
	}
	if (dismissing && !findingFingerprint.MatchString(*dismiss)) || (undismissing && !findingFingerprint.MatchString(*undismiss)) {
		return fail(fmt.Errorf("--dismiss and --undismiss take a finding fingerprint: 64 lower-case hex characters"))
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
	if *push {
		return runFindingsPush(ctx, resolved, id, *approve, *title, *confirm, out, stderr)
	}
	if dismissing || undismissing {
		return runFindingsDismiss(resolved, id, *dismiss, *undismiss, *reason, out, stderr)
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
	if *propose {
		return runFindingsPropose(ctx, resolved, *lab, id, loaded.Formula, includes, out, stderr)
	}
	var record delivery.FindingsRecord
	if *collect {
		if err := delivery.RefuseDemoLab(resolved); err != nil {
			return fail(err)
		}
		record, err = collectFindings(ctx, resolved, id, loaded.Formula)
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
func collectFindings(ctx context.Context, lab, id string, f formula.Formula) (delivery.FindingsRecord, error) {
	return fixround.Collect(ctx, lab, id, f, fixround.Options{LookupEnv: pluginLookupEnv})
}

// runFindingsDismiss is findings --dismiss and --undismiss: it records or
// removes one dismissal, which only changes what is shown and proposed.
func runFindingsDismiss(lab, id, dismiss, undismiss, reason string, out, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	if err := delivery.RefuseDemoLab(lab); err != nil {
		return fail(err)
	}
	if undismiss != "" {
		if err := delivery.UndismissFinding(lab, id, undismiss); err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "Finding %s is no longer dismissed.\n", undismiss)
		return 0
	}
	if err := delivery.DismissFinding(lab, id, dismiss, reason, nil); err != nil {
		return fail(err)
	}
	fmt.Fprintf(out, "Finding %s dismissed; it is left out of the open findings and of fix-round proposals until --undismiss.\n", dismiss)
	return 0
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
	if s.Dismissed > 0 {
		fmt.Fprintf(out, "dismissed: %d\n", s.Dismissed)
	}
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
