package cli

import (
	"context"
	"flag"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"io"
	"strconv"
)

type issueQueueRow struct {
	Issue    intake.QueueIssue     `json:"issue"`
	Molecule molecule.QueueSummary `json:"molecule"`
}
type issueQueueReport struct {
	ReadOnly bool                    `json:"readOnly"`
	Remote   string                  `json:"remote"`
	Error    string                  `json:"error,omitempty"`
	Page     intake.IssuePage        `json:"pagination"`
	Local    molecule.QueueInventory `json:"local"`
	Issues   []issueQueueRow         `json:"issues"`
	Notice   string                  `json:"notice"`
}

func runIssueQueue(ctx context.Context, args []string, defaultLab string, out, stderr io.Writer) int {
	flags := flag.NewFlagSet("issues", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", "", "Repository OWNER/REPO")
	page := flags.Int("page", 1, "Remote page (positive)")
	size := flags.Int("page-size", 30, "Remote page size (1–100)")
	selection := flags.String("select", "", "Explicit read-only Issue selection")
	lab := flags.String("lab", defaultLab, "Lab directory")
	jsonMode := flags.Bool("json", false, "Structured read-only queue")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("issues accepts flags only"))
	}
	paginationSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "page" || f.Name == "page-size" {
			paginationSet = true
		}
	})
	if *selection != "" && paginationSet {
		return fail(fmt.Errorf("--select cannot be combined with pagination flags"))
	}
	if _, err := intake.BindRepository(intake.IssueRef{Number: 1}, *repo); err != nil {
		return fail(err)
	}
	if *page < 1 || *size < 1 || *size > 100 {
		return fail(fmt.Errorf("page must be positive; page-size must be 1–100"))
	}
	if *selection != "" {
		ref, err := intake.ParseIssueRef(*selection)
		if err != nil {
			return fail(err)
		}
		if _, err = intake.BindRepository(ref, *repo); err != nil {
			return fail(err)
		}
	}
	report := issueQueueReport{ReadOnly: true, Remote: "available", Issues: []issueQueueRow{}, Notice: "Read-only snapshot. Active counts are ledger-reported, not process liveness. Selection grants no scope, execution, publication or merge approval."}
	var err error
	report.Page, err = intake.ReadIssueQueue(ctx, *repo, *page, *size, *selection)
	report.Local = molecule.ReadQueueInventory(*lab)
	code := 0
	if err != nil {
		report.Remote = "unavailable"
		report.Error = err.Error()
		code = 1
	} else {
		for _, issue := range report.Page.Issues {
			report.Issues = append(report.Issues, issueQueueRow{issue, report.Local.Summarize(issue.Repository, issue.Number)})
		}
	}
	if *jsonMode {
		if c := encode(out, stderr, report); c != 0 {
			return c
		}
		return code
	}
	fmt.Fprintln(out, report.Notice)
	if report.Error != "" {
		fmt.Fprintln(out, strconv.QuoteToASCII(report.Error))
	}
	fmt.Fprintf(out, "Remote: %s · page %d · page size %d · underlying items %d · more possible: %t · selected: %t\n", report.Remote, report.Page.Page, report.Page.PageSize, report.Page.RemoteItems, report.Page.MorePossible, report.Page.Selected)
	fmt.Fprintf(out, "Local inventory complete: %t · truncated: %t\n", report.Local.Complete, report.Local.Truncated)
	for _, d := range report.Local.Diagnostics {
		fmt.Fprintf(out, "Warning: %s\n", strconv.QuoteToASCII(d))
	}
	for _, row := range report.Issues {
		s := row.Molecule
		latest := "unknown"
		if s.Latest != nil {
			phase := s.Latest.Phase
			if phase == "" {
				phase = "unknown"
			}
			latest = fmt.Sprintf("%s status=%s phase=%s", s.Latest.ID, strconv.QuoteToASCII(s.Latest.Status), strconv.QuoteToASCII(phase))
		}
		fmt.Fprintf(out, "%s#%d %s %s · state=%s · latest=%s · ledger active=%d · blocked=%d · partial=%t · ordering uncertain=%t\n", row.Issue.Repository, row.Issue.Number, row.Issue.State, strconv.QuoteToASCII(row.Issue.Title), s.State, latest, s.Active, s.Blocked, s.Partial, s.OrderingUncertain)
		if s.OrderingUncertain {
			for _, a := range s.Attempts {
				fmt.Fprintf(out, "Recorded attempt %s · raw status=%s · phase=%s · startedAt=%s\n", a.ID, strconv.QuoteToASCII(a.Status), strconv.QuoteToASCII(a.Phase), strconv.QuoteToASCII(a.StartedAt))
			}
		}
		if s.DuplicateActive {
			fmt.Fprintln(out, "Warning: duplicate ledger-reported active runs; informational only.")
		}
	}
	return code
}
