package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/learning"
)

// Seams so tests drive the CLI without a provider or a real Lab history.
var (
	runPendingLearn    = learning.RunPending
	evaluateRecurrence = learning.EvaluateRecurrence
	listPendingLearn   = learning.Pending
)

// runLearnPending runs the pending learn requests through the normal learn path.
// It is the only command that starts a model for recurrence learning.
func runLearnPending(ctx context.Context, lab string, jsonMode bool, out, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "forgecell:", err)
		return 1
	}
	dir, err := filepath.Abs(lab)
	if err != nil {
		fmt.Fprintln(stderr, "forgecell:", err)
		return 1
	}
	results, err := runPendingLearn(ctx, learning.PendingOptions{LabDir: dir, Dir: cwd})
	if err != nil {
		fmt.Fprintln(stderr, "forgecell:", err)
		return 1
	}
	failed := 0
	for _, r := range results {
		if r.Err != "" {
			failed++
		}
	}
	if jsonMode {
		if code := encode(out, stderr, map[string]any{"results": results}); code != 0 {
			return code
		}
	} else if len(results) == 0 {
		fmt.Fprintln(out, "No pending learn requests.")
	}
	if !jsonMode {
		for _, r := range results {
			label := fmt.Sprintf("Learn request %s (%s, Molecules %s)", r.RequestID, r.Class, strings.Join(r.MoleculeIDs, ", "))
			if r.Err != "" {
				fmt.Fprintf(out, "%s failed: %s\nThe request stays pending; run forgecell learn --pending --lab %s to retry.\n", label, flatten(r.Err), formula.ShellWord(dir))
				continue
			}
			fmt.Fprintf(out, "%s saved suggestion %s (pending your decision).\nforgecell suggestion %s --lab %s\n", label, r.SuggestionID, r.SuggestionID, formula.ShellWord(dir))
		}
	}
	if _, listErr := listPendingLearn(dir); listErr != nil {
		fmt.Fprintf(stderr, "forgecell: pending learn requests unavailable: %s\n", flatten(listErr.Error()))
	}
	if failed > 0 {
		fmt.Fprintf(stderr, "forgecell: %d learn request(s) did not produce a suggestion\n", failed)
		return 1
	}
	return 0
}

// learnNoticeLines renders what a person still has to act on. Class names and
// Molecule ids are validated by the learning package before they get here.
func learnNoticeLines(lab string, items []learning.PendingItem) []string {
	var lines []string
	for _, item := range items {
		ids := strings.Join(item.MoleculeIDs, ", ")
		switch item.Kind {
		case "suggestion":
			lines = append(lines, fmt.Sprintf("Pending Formula suggestion %s from recurring %s (Molecules %s): forgecell suggestion %s --lab %s", item.SuggestionID, item.Class, ids, item.SuggestionID, formula.ShellWord(lab)))
		default:
			line := fmt.Sprintf("Pending learn request %s: %s recurred in Molecules %s. Run: forgecell learn --pending --lab %s", item.RequestID, item.Class, ids, formula.ShellWord(lab))
			if item.LastFailure != "" {
				line += fmt.Sprintf("\n  The last learn run failed (attempt %d): %s", item.Attempts, flatten(item.LastFailure))
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// renderLearnNotices evaluates the recurrence rules once the Molecule work of a
// command is done, then prints the recommendation and the pending learning work.
// It never changes an exit code and prints nothing when there is nothing to say.
// A failed evaluation becomes one diagnostic line; it never queues anything.
func renderLearnNotices(out, stderr io.Writer, lab string) {
	dir, err := filepath.Abs(lab)
	if err != nil {
		return
	}
	var lines []string
	recurrence, err := evaluateRecurrence(dir)
	if err != nil && !errors.Is(err, learning.ErrNoActiveFormula) {
		fmt.Fprintf(stderr, "forgecell: learning recurrence check skipped: %s\n", flatten(err.Error()))
	}
	if len(recurrence.Skipped) > 0 {
		fmt.Fprintf(stderr, "forgecell: learning recurrence check skipped unreadable items: %s\n", flatten(strings.Join(recurrence.Skipped, "; ")))
	}
	for _, line := range recurrence.Recommendations {
		lines = append(lines, learning.DisplayText(line, 1000))
	}
	items, err := listPendingLearn(dir)
	if err != nil {
		fmt.Fprintf(stderr, "forgecell: pending learn requests unavailable: %s\n", flatten(err.Error()))
	}
	lines = append(lines, learnNoticeLines(dir, items)...)
	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
}

// flatten makes untrusted text one display-safe line of at most 300 runes.
func flatten(s string) string { return learning.DisplayText(s, 300) }
