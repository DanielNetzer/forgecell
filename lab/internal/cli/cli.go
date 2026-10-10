// Package cli is the terminal entry point; execution and persistence live in the core.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/evaluation"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/install"
	"github.com/DanielNetzer/forgecell/lab/internal/learning"
	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"go.yaml.in/yaml/v4"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, version string) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(out, "Forgecell Lab — Go migration preview\n\ninit [--lab DIR] [--harness ID] [--meta-harness ID | --meta-command JSON_ARGV] [--json] [--review ID | --approve ID | --dismiss ID]\ndoctor [--lab DIR] [--harness ID] [--json]\nrun <issue> [--lab DIR] [--formula ID] [--base COMMIT] [--target BRANCH] [--approve DIGEST] [--json]\namend <molecule> --parent DIGEST --plan FILE [--accept-existing-tree TREE] [--lab DIR]\nrecover <molecule> --plan DIGEST --confirm-stopped [--lab DIR]\nledger <molecule> [--lab DIR]\nissues --repo OWNER/REPO [--page N --page-size N | --select ISSUE_REF] [--lab DIR] [--json]\nlearn <molecule...> [--lab DIR] [--json]\nsuggestion <id> [--lab DIR] [--approve | --dismiss | --link-evaluation DIR | --baseline-ledger ID --candidate-ledger ID --comparability-basis TEXT] [--evaluation-parent DIR] [--json]\nrollback\nevaluate --inputs PLAN --out NEW_DIRECTORY [--source REPO] [--approve DIGEST]\ndeliver <molecule> --lab DIR --base BRANCH [--approve DIGEST --title TITLE] -- FILE...\nchecks --lab DIR --molecule ID --repo OWNER/REPO --pr NUMBER --commit SHA --required NAME [--required NAME]\n--version\n\nRuns analyze a ticket before coding and wait for exact scope approval. Required checks run independently before final review.\nTicket readiness is a development preview; existing Formulas require an explicitly reviewed scope gate.")
		return 0
	}
	if args[0] == "--version" || args[0] == "-v" {
		fmt.Fprintln(out, version)
		return 0
	}
	defaultLab, err := defaultLabDir()
	if err != nil {
		return fail(err)
	}

	if args[0] == "issues" {
		return runIssueQueue(ctx, args[1:], defaultLab, out, stderr)
	}

	if args[0] == "learn" || args[0] == "suggestion" {
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		flags.SetOutput(stderr)
		lab := flags.String("lab", defaultLab, "Lab directory")
		jsonMode := flags.Bool("json", false, "Structured review output")
		approve := flags.Bool("approve", false, "Apply this exact saved Formula suggestion")
		dismiss := flags.Bool("dismiss", false, "Dismiss this suggestion")
		linkEvaluation := flags.String("link-evaluation", "", "Link local evaluation evidence without activating a Formula")
		baselineLedger := flags.String("baseline-ledger", "", "Manual comparison baseline Molecule")
		candidateLedger := flags.String("candidate-ledger", "", "Manual comparison candidate Molecule")
		basis := flags.String("comparability-basis", "", "Human assertion of manual comparability and uncertainty")
		var parents []string
		flags.Func("evaluation-parent", "Recheck parent directory, nearest first; repeat for a bounded chain", func(dir string) error { parents = append(parents, dir); return nil })
		ids := []string{}
		i := 1
		for i < len(args) && !strings.HasPrefix(args[i], "-") {
			ids = append(ids, args[i])
			i++
		}
		if err := flags.Parse(args[i:]); err != nil {
			return 1
		}
		if flags.NArg() != 0 || len(ids) == 0 || (*approve && *dismiss) {
			return fail(fmt.Errorf("provide Molecule ids before flags, or one suggestion id and one decision"))
		}
		linking := *linkEvaluation != "" || *baselineLedger != "" || *candidateLedger != "" || *basis != "" || len(parents) != 0
		if linking && (args[0] != "suggestion" || *approve || *dismiss || (*linkEvaluation == "" && len(parents) != 0)) {
			return fail(fmt.Errorf("evidence linkage requires suggestion and cannot be combined with an approval or dismissal; parents require an evaluation"))
		}
		var p learning.Suggestion
		var err error
		if args[0] == "learn" {
			if *approve || *dismiss {
				return fail(fmt.Errorf("learn only proposes; inspect the saved suggestion before approval"))
			}
			cwd, e := os.Getwd()
			if e != nil {
				return fail(e)
			}
			p, err = learning.Create(ctx, learning.Options{LabDir: *lab, Dir: cwd, MoleculeIDs: ids})
		} else {
			if len(ids) != 1 {
				return fail(fmt.Errorf("suggestion requires one id"))
			}
			if linking {
				p, err = learning.LinkComparison(*lab, ids[0], learning.LinkOptions{Evaluation: *linkEvaluation, Parents: parents, Baseline: *baselineLedger, Candidate: *candidateLedger, Basis: *basis})
			} else if *approve || *dismiss {
				decision := "dismiss"
				if *approve {
					decision = "approve"
				}
				p, err = learning.Review(*lab, ids[0], decision)
			} else {
				p, err = learning.Read(*lab, ids[0])
			}
		}
		if err != nil {
			return fail(err)
		}
		if *jsonMode {
			return encode(out, stderr, p)
		}
		summary := p.Summary
		if summary == "" {
			summary = "Legacy suggestion: inspect reasoning and exact YAML before deciding."
		}
		fmt.Fprintf(out, "Formula suggestion %s · %s\nFormula: %s · Meta harness\n\nProcess change: %s\nEvidence: %s\nReasoning: %s\nExpected impact (not verified): %s\nEvaluate on a comparable run: %s\n\n%s\n", p.ID, p.Status, p.FormulaID, summary, strings.Join(p.MoleculeIDs, ", "), p.Rationale, p.ExpectedImpact, p.Evaluation, p.Diff)

		fmt.Fprintf(out, "Baseline Formula SHA-256: %s\nCandidate Formula SHA-256: %s\nComparison: %s\nIntervention: %s\n", p.OriginalHash, p.ProposedHash, p.Comparison.Status, p.Comparison.Support)
		if p.Comparison.Reason != "" {
			fmt.Fprintf(out, "Comparison limitation: %s\n", p.Comparison.Reason)
		}
		for _, m := range p.MotivatingEvidence {
			fmt.Fprintf(out, "Motivating Molecule %s · ledger SHA-256 %s\n", m.MoleculeID, m.SHA256)
		}
		for _, outcome := range p.EvidenceOutcomes {
			fmt.Fprintf(out, "Motivating outcome %s: %s (independent acceptance: %t)\n", outcome.MoleculeID, outcome.Outcome, outcome.IndependentAcceptance)
		}
		for _, link := range p.Comparison.Links {
			fmt.Fprintf(out, "Evidence link %s · %s · %s\n%s\n", link.ID, link.Kind, link.Status, link.Reason)
			if link.Basis != "" {
				fmt.Fprintf(out, "Human comparability basis: %s\nDifferences: %s\n", link.Basis, strings.Join(link.Differences, "; "))
			}
			if link.Evaluation != nil {
				details, _ := json.MarshalIndent(struct {
					Plan    *evaluation.Plan    `json:"frozenPlan"`
					Reports []evaluation.Report `json:"reports"`
				}{link.Evaluation.Plan, link.Evaluation.Reports}, "", "  ")
				fmt.Fprintf(out, "Frozen inputs and recorded observations:\n%s\n", details)
				for _, limitation := range link.Evaluation.Limitations {
					fmt.Fprintln(out, limitation)
				}
			}
			for i, r := range link.Ledgers {
				details, _ := json.MarshalIndent(r, "", "  ")
				fmt.Fprintf(out, "Manual %s ledger (exact task/check and verification history):\n%s\n", []string{"baseline", "candidate"}[i], details)
			}
			for _, outcome := range link.Outcomes {
				fmt.Fprintf(out, "Manual outcome %s: %s (independent acceptance: %t)\n", outcome.MoleculeID, outcome.Outcome, outcome.IndependentAcceptance)
			}
		}
		for _, limitation := range p.Comparison.Limitations {
			fmt.Fprintln(out, limitation)
		}

		if p.ProposedReady {
			fmt.Fprintln(out, "Proposed Formula readiness: ready")
		} else {
			fmt.Fprintf(out, "Proposed Formula readiness: invalid — %s\n", p.ReadinessError)
		}
		if p.Status == "pending" {
			if p.ProposedReady {
				fmt.Fprintf(out, "Approval changes the future recipe; it does not prove improvement. Inspect commands and permissions.\nforgecell suggestion %s --lab %q --approve\n", p.ID, *lab)
			}
			fmt.Fprintf(out, "forgecell suggestion %s --lab %q --dismiss\n", p.ID, *lab)
		}
		return 0
	}

	if args[0] == "evaluate" {
		flags := flag.NewFlagSet("evaluate", flag.ContinueOnError)
		flags.SetOutput(stderr)
		inputs := flags.String("inputs", "", "Frozen evaluation plan")
		approve := flags.String("approve", "", "Fresh exact evaluation activation digest")
		recheck := flags.String("recheck", "", "Existing completed evidence directory; no model rerun")
		var originalChecks []string
		flags.Func("original-check-file", "Keep this check file from the original base during recheck", func(file string) error { originalChecks = append(originalChecks, file); return nil })
		output := flags.String("out", "", "New evidence directory")
		source := flags.String("source", ".", "Repository checkout")
		if err := flags.Parse(args[1:]); err != nil {
			return 1
		}
		if (*inputs == "") == (*recheck == "") || *output == "" || flags.NArg() != 0 {
			return fail(fmt.Errorf("evaluate requires exactly one of --inputs or --recheck, plus --out; previous evidence cannot be overwritten"))
		}
		var result evaluation.Report
		var err error
		if *recheck != "" {
			if *approve != "" {
				return fail(fmt.Errorf("recheck cannot approve new model execution"))
			}
			result, err = evaluation.Recheck(ctx, *source, *recheck, *output, originalChecks)
		} else {
			if len(originalChecks) > 0 {
				return fail(fmt.Errorf("original-check-file is for recheck; use originalCheckFiles in a new plan"))
			}
			result, err = evaluation.Run(ctx, evaluation.RunOptions{Inputs: *inputs, Output: *output, SourceRoot: *source, Approve: *approve})
		}
		if err != nil {
			return fail(err)
		}
		if code := encode(out, stderr, result); code != 0 {
			return code
		}
		for _, attempt := range result.Attempts {
			if !attempt.Correct {
				return 2
			}
		}
		return 0
	}

	if args[0] == "__install" || args[0] == "rollback" {
		if len(args) != 1 {
			return fail(fmt.Errorf("installer options use FORGECELL_HOME and FORGECELL_BIN environment variables"))
		}
		source, err := os.Executable()
		if err != nil {
			return fail(err)
		}
		options := install.Options{Home: os.Getenv("FORGECELL_HOME"), Bin: os.Getenv("FORGECELL_BIN"), Source: source, Version: version}
		var result install.Result
		if args[0] == "rollback" {
			result, err = install.Rollback(ctx, options)
		} else {
			result, err = install.Install(ctx, options)
		}
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "Forgecell %s installed. Lab data preserved.\nCommand: %s\nNext: forgecell doctor → forgecell init → approve Formula → forgecell run <issue>\n", result.Version, result.Command)
		fmt.Fprintf(out, "Ensure this directory is on PATH: %s\nGit, authenticated GitHub CLI (gh), and a configured coding harness are required to run tickets.\n", filepath.Dir(result.Command))
		return 0
	}

	if args[0] == "deliver" {
		if len(args) < 2 {
			return fail(fmt.Errorf("deliver requires a Molecule id"))
		}
		flags := flag.NewFlagSet("deliver", flag.ContinueOnError)
		flags.SetOutput(stderr)
		lab := flags.String("lab", defaultLab, "Lab directory")
		base := flags.String("base", "", "Explicit PR base branch")
		approve := flags.String("approve", "", "Publish exactly reviewed preview digest as a draft PR")
		title := flags.String("title", "", "Delivery commit and PR title")
		acceptUnverified := flags.Bool("accept-unverified", false, "Explicitly acknowledge human acceptance review where independent evidence is missing")
		if err := flags.Parse(args[2:]); err != nil {
			return 1
		}
		options := delivery.Options{LabDir: *lab, MoleculeID: args[1], BaseBranch: *base, Files: flags.Args()}
		if *approve != "" {
			result, err := delivery.Publish(ctx, delivery.PublishOptions{Options: options, Approve: *approve, Title: *title, AcceptUnverified: *acceptUnverified})
			if err != nil {
				return fail(err)
			}
			return encode(out, stderr, result)
		}
		if *title != "" {
			return fail(fmt.Errorf("title requires an approval digest"))
		}
		result, err := delivery.Preview(ctx, options)
		if err != nil {
			return fail(err)
		}
		return encode(out, stderr, result)
	}

	if args[0] == "checks" {
		flags := flag.NewFlagSet("checks", flag.ContinueOnError)
		flags.SetOutput(stderr)
		lab := flags.String("lab", defaultLab, "Lab directory")
		mol := flags.String("molecule", "", "Molecule delivery identity")
		repo := flags.String("repo", "", "GitHub repository")
		pr := flags.Int("pr", 0, "Pull request number")
		sha := flags.String("commit", "", "Exact expected commit")
		var required []string
		flags.Func("required", "Exact required check name; repeat for multiple checks", func(name string) error { required = append(required, name); return nil })
		if err := flags.Parse(args[1:]); err != nil {
			return 1
		}
		if flags.NArg() != 0 {
			return fail(fmt.Errorf("unexpected checks arguments"))
		}
		link := delivery.Options{LabDir: *lab, MoleculeID: *mol}
		observation, err := delivery.CollectCI(ctx, link, checks.Options{Repo: *repo, PR: *pr, Commit: *sha, Required: required})
		if err != nil {
			return fail(err)
		}
		if code := encode(out, stderr, observation); code != 0 {
			return code
		}
		if observation.Snapshot.Status != "passed" {
			return 2
		}
		return 0
	}

	if args[0] == "init" {
		flags := flag.NewFlagSet("init", flag.ContinueOnError)
		flags.SetOutput(stderr)
		lab := flags.String("lab", defaultLab, "Lab directory")
		preferred := flags.String("harness", "", "Explicit default harness")
		metaHarness := flags.String("meta-harness", "", "Explicit separate native meta binding")
		metaArgv := flags.String("meta-command", "", "BYO meta argv as JSON; containment unknown")
		review := flags.String("review", "", "Inspect exact saved proposal without activation")
		approve := flags.String("approve", "", "Approve exact saved proposal")
		dismiss := flags.String("dismiss", "", "Dismiss exact saved proposal")
		jsonMode := flags.Bool("json", false, "Structured output, never prompts")
		if err := flags.Parse(args[1:]); err != nil {
			return 1
		}
		if flags.NArg() != 0 {
			return fail(fmt.Errorf("unexpected init arguments"))
		}
		var metaCommand []string
		metaRequested := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "meta-harness" || f.Name == "meta-command" {
				metaRequested = true
			}
		})
		if metaRequested && (*review != "" || *approve != "" || *dismiss != "") {
			return fail(fmt.Errorf("meta opt-in cannot accompany saved review or decisions"))
		}
		if *metaHarness != "" && *metaArgv != "" {
			return fail(fmt.Errorf("choose meta-harness or meta-command"))
		}
		if metaRequested && *metaHarness == "" && *metaArgv == "" {
			return fail(fmt.Errorf("meta opt-in requires harness ID or non-empty JSON argv"))
		}
		if *metaArgv != "" {
			if err := json.Unmarshal([]byte(*metaArgv), &metaCommand); err != nil || len(metaCommand) == 0 {
				return fail(fmt.Errorf("meta-command must be a non-empty JSON argv array"))
			}
			for _, arg := range metaCommand {
				if strings.TrimSpace(arg) == "" || strings.ContainsRune(arg, 0) {
					return fail(fmt.Errorf("meta-command arguments must be non-empty strings"))
				}
			}
		}
		if *approve != "" && *dismiss != "" {
			return fail(fmt.Errorf("choose approve or dismiss"))
		}
		if *review != "" {
			if *approve != "" || *dismiss != "" || *preferred != "" {
				return fail(fmt.Errorf("review cannot also change the harness or decision"))
			}
			dir, err := filepath.Abs(*lab)
			if err != nil {
				return fail(err)
			}
			p, err := onboarding.InspectProposal(dir, *review)
			if err != nil {
				return fail(err)
			}
			return renderProposal(out, stderr, p, dir, *jsonMode)
		}
		if os.Getenv("FORGECELL_LAB") == "" {
			explicitLab := false
			flags.Visit(func(f *flag.Flag) {
				if f.Name == "lab" {
					explicitLab = true
				}
			})
			if !explicitLab {
				if legacy := existingCheckoutLab(); legacy != "" {
					return fail(fmt.Errorf("existing checkout Lab at %s; use --lab %q to continue it, or move its contents to the home Lab before retrying", legacy, legacy))
				}
			}
		}
		dir, err := filepath.Abs(*lab)
		if err != nil {
			return fail(err)
		}
		if *approve != "" || *dismiss != "" {
			if *preferred != "" {
				return fail(fmt.Errorf("review cannot also change the harness"))
			}
			id, decision := *approve, "approve"
			if *dismiss != "" {
				id, decision = *dismiss, "dismiss"
			}
			p, err := onboarding.Review(dir, id, decision)
			if err != nil {
				return fail(err)
			}
			if *jsonMode {
				return encode(out, stderr, p)
			}
			fmt.Fprintf(out, "Formula proposal %s: %s\n", p.ID, p.Status)
			return 0
		}
		cwd, err := os.Getwd()
		if err != nil {
			return fail(err)
		}
		launcher := os.Getenv("FORGECELL_LAUNCHER")
		if launcher == "" {
			launcher, err = os.Executable()
			if err != nil {
				return fail(err)
			}
		}
		env := map[string]string{}
		for _, key := range []string{"CODEX_THREAD_ID", "CLAUDECODE", "CURSOR_AGENT", "TERM_PROGRAM"} {
			env[key] = os.Getenv(key)
		}
		result, err := onboarding.Prepare(ctx, onboarding.InitOptions{Cwd: cwd, LabDir: dir, Launcher: launcher, Preferred: *preferred, Env: env, MetaHarness: *metaHarness, MetaCommand: metaCommand})
		if err != nil {
			return fail(err)
		}
		if *jsonMode {
			if code := encode(out, stderr, result); code != 0 {
				return code
			}
		} else {
			fmt.Fprintf(out, "Bootstrap Assay: %s\nRepository: %s\n%s\n%s\n", result.Status, result.Repository.Repo, result.Selection.Reason, result.Note)
			source := "current observation; containment unverified"
			if result.Proposal != nil {
				source = "saved observation; not current verification"
			}
			if result.Binding != nil {
				renderCapabilities(out, result.Binding.Candidate, result.Binding.LearnBound, source)
			}
			if result.Meta != nil {
				renderMeta(out, *result.Meta, source)
			}
			if result.Proposal != nil {
				p, err := onboarding.InspectProposal(dir, result.Proposal.ID)
				if err != nil {
					return fail(err)
				}
				return renderProposal(out, stderr, p, dir, false)
			}
		}
		if result.Status != "pending" && result.Status != "unchanged" {
			return 2
		}
		return 0
	}
	if args[0] == "doctor" {
		flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
		flags.SetOutput(stderr)
		lab := flags.String("lab", defaultLab, "Lab directory")
		preferred := flags.String("harness", "", "Preferred harness")
		flags.Bool("json", true, "Structured output")
		if err := flags.Parse(args[1:]); err != nil {
			return 1
		}
		if flags.NArg() != 0 {
			return fail(fmt.Errorf("unexpected doctor arguments"))
		}
		cwd, err := os.Getwd()
		if err != nil {
			return fail(err)
		}
		active, err := formula.Inspect(*lab, "")
		if err != nil {
			return fail(err)
		}
		candidates := onboarding.Discover(ctx, cwd, nil)
		existing := ""
		var saved *onboarding.Candidate
		if active.Formula.ID != "" {
			existing = active.Formula.Harness.Binding
			checked := onboarding.ProbeBinding(ctx, active.Formula.Harness, cwd, nil)
			saved = &checked
			replaced := false
			for i := range candidates {
				if candidates[i].ID == existing {
					candidates[i] = checked
					replaced = true
				}
			}
			if !replaced {
				candidates = append(candidates, checked)
			}
		}
		env := map[string]string{}
		for _, key := range []string{"CODEX_THREAD_ID", "CLAUDECODE", "CURSOR_AGENT", "TERM_PROGRAM"} {
			env[key] = os.Getenv(key)
		}
		selection := onboarding.Select(candidates, *preferred, existing, env)
		git := process.Run(ctx, process.Options{Argv: []string{"git", "--version"}, Dir: cwd, Stdin: []byte{}, Timeout: 5 * time.Second})
		gh := process.Run(ctx, process.Options{Argv: []string{"gh", "auth", "status"}, Dir: cwd, Stdin: []byte{}, Timeout: 5 * time.Second})
		meta := onboarding.InspectMeta(ctx, active.Formula, cwd, nil)
		report := map[string]any{"metaBinding": meta, "metaCandidates": candidates, "activeFormulaId": active.Formula.ID, "labDir": *lab, "legacyLabDir": existingCheckoutLab(), "savedBinding": saved, "learnBound": onboarding.LearnBound(active.Formula), "workflow": selectedWorkflow(candidates, selection.ID), "gitReady": git.OK, "githubAuthenticated": gh.OK, "candidates": candidates, "selection": selection, "note": "Read-only checks; no model invoked. Repository policy and model access are not verified by authentication."}
		if err = json.NewEncoder(out).Encode(report); err != nil {
			return fail(err)
		}
		workflow := selectedWorkflow(candidates, selection.ID)
		if workflow.State != "conditional" || !git.OK || !gh.OK || selection.Status != "ready" || (saved != nil && saved.Readiness != "ready") {
			return 2
		}
		return 0
	}
	if args[0] == "__adapter" {
		return fail(fmt.Errorf("direct __adapter invocation is unsupported; approve the current Formula with init and use run or learn"))
	}
	if args[0] == "amend" {
		if len(args) < 2 {
			return fail(fmt.Errorf("amend requires a Molecule id"))
		}
		flags := flag.NewFlagSet("amend", flag.ContinueOnError)
		flags.SetOutput(stderr)
		lab := flags.String("lab", defaultLab, "Lab directory")
		parent := flags.String("parent", "", "Previous plan digest")
		file := flags.String("plan", "", "Full successor readiness plan JSON")
		accepted := flags.String("accept-existing-tree", "", "Exact existing violating tree separately reviewed")
		if err := flags.Parse(args[2:]); err != nil {
			return 1
		}
		if flags.NArg() != 0 {
			return fail(fmt.Errorf("unexpected arguments"))
		}
		f, err := os.Open(*file)
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, 1000001))
		if err != nil {
			return fail(err)
		}
		if len(raw) > 1000000 {
			return fail(fmt.Errorf("plan exceeds 1 MB"))
		}
		var plan readiness.Plan
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&plan); err != nil {
			return fail(err)
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return fail(fmt.Errorf("expected one JSON plan"))
		}
		r, err := molecule.Amend(ctx, *lab, args[1], *parent, plan, *accepted)
		if err != nil {
			return fail(err)
		}
		presentRun(out, r)
		return 0
	}
	if args[0] == "recover" {
		if len(args) < 2 {
			return fail(fmt.Errorf("recover requires a Molecule id"))
		}
		flags := flag.NewFlagSet("recover", flag.ContinueOnError)
		flags.SetOutput(stderr)
		lab := flags.String("lab", defaultLab, "Lab directory")
		plan := flags.String("plan", "", "Exact interrupted plan digest")
		stopped := flags.Bool("confirm-stopped", false, "Confirm all Lab and child processes have stopped")
		if err := flags.Parse(args[2:]); err != nil {
			return 1
		}
		if flags.NArg() != 0 {
			return fail(fmt.Errorf("unexpected arguments"))
		}
		r, err := molecule.Recover(ctx, *lab, args[1], *plan, *stopped)
		if err != nil {
			return fail(err)
		}
		return encode(out, stderr, r)
	}
	if args[0] != "run" && args[0] != "ledger" {
		return fail(fmt.Errorf("unknown command %q; use --help", args[0]))
	}
	if len(args) < 2 {
		return fail(fmt.Errorf("%s requires an identifier", args[0]))
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	lab := flags.String("lab", defaultLab, "Lab directory")
	formula := flags.String("formula", "", "Formula id")
	base := flags.String("base", "", "Local base commit")
	isolated := flags.Bool("isolated", true, "Run in isolation (required)")
	approve := flags.String("approve", "", "Approve this exact pending readiness plan")
	target := flags.String("target", "", "Intended PR target branch, bound into approval")
	verificationFile := flags.String("verification", "", "Human-supplied frozen verification inputs JSON")
	var requirements []string
	flags.Func("require-policy", "Pin reviewed github-ruleset-sha256:HASH or github-protection-sha256:HASH", func(value string) error { requirements = append(requirements, value); return nil })
	jsonMode := flags.Bool("json", false, "Structured Molecule and readiness output")
	if err := flags.Parse(args[2:]); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("unexpected positional arguments"))
	}
	if args[0] == "ledger" {
		if !safeID.MatchString(args[1]) {
			return fail(fmt.Errorf("invalid Molecule id"))
		}
		file := filepath.Join(*lab, "ledgers", args[1]+".json")
		raw, err := readLedgerForInspection(file)
		if err != nil {
			return fail(err)
		}
		record, err := ledger.Decode(raw)
		if err != nil {
			return fail(err)
		}
		if record.ID != args[1] {
			return fail(fmt.Errorf("ledger id mismatch"))
		}
		fmt.Fprintln(out, string(raw))
		return 0
	}
	if !*isolated {
		return fail(fmt.Errorf("Go execution requires an isolated checkout"))
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	var supplied readiness.VerificationInputs
	if *verificationFile != "" {
		if *approve != "" {
			return fail(fmt.Errorf("verification inputs cannot change while approving; amend the plan instead"))
		}
		f, e := os.Open(*verificationFile)
		if e != nil {
			return fail(e)
		}
		defer f.Close()
		raw, e := io.ReadAll(io.LimitReader(f, 1000001))
		if e != nil {
			return fail(e)
		}
		supplied, e = readiness.DecodeVerificationInputs(raw)
		if e != nil {
			return fail(e)
		}
	}
	r, err := molecule.Run(ctx, molecule.Options{LabDir: *lab, SourceRoot: cwd, Issue: args[1], FormulaID: *formula, Base: *base, Approve: *approve, TargetBranch: *target, MandatoryConstraints: requirements, CheckInputs: supplied.CheckInputs, Checks: supplied.Checks, Setup: supplied.Setup, Artifacts: supplied.Artifacts, Policy: supplied.Policy})
	if err != nil {
		return fail(err)
	}
	if *jsonMode {
		if err = json.NewEncoder(out).Encode(r); err != nil {
			return fail(err)
		}
	} else {
		presentRun(out, r)
	}
	if r.Status == "failed" || r.Status == "blocked" {
		return 2
	}
	return 0
}

func encode(out, stderr io.Writer, value any) int {
	if err := json.NewEncoder(out).Encode(value); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func presentRun(out io.Writer, r molecule.Record) {
	fmt.Fprintf(out, "Molecule %s · %s\n%s#%d — %s\n", r.ID, r.Status, r.Issue.Repo, r.Issue.Number, r.Issue.Title)
	if r.Analysis != nil {
		fmt.Fprintln(out, r.Analysis.Summary)
		for _, q := range r.Analysis.Questions {
			fmt.Fprintf(out, "Question: %s\nWhy: %s\n", q.Question, q.Reason)
		}
	}
	if r.Readiness != nil && len(r.Readiness.Plans) > 0 {
		proposal := r.Readiness.Plans[len(r.Readiness.Plans)-1]
		p := proposal.Plan
		fmt.Fprintf(out, "\nFormula: %s · base %s · target %s\n", r.FormulaID, p.Inputs.BaseCommit, p.Inputs.TargetBranch)
		if p.Continuation == "checks-only" {
			fmt.Fprintln(out, "Continuation: checks only — retained source, no coding harness invocation.")
		}
		fmt.Fprintln(out, "Scope:")
		for _, s := range p.Analysis.Scope {
			fmt.Fprintf(out, "  %s — %s\n", s.Path, s.Reason)
		}
		fmt.Fprintln(out, "Acceptance:")
		for _, c := range p.Analysis.Acceptance {
			fmt.Fprintf(out, "  %s\n", c.Description)
		}
		fmt.Fprintln(out, "Checks (commands require review):")
		for _, c := range p.Analysis.Checks {
			args, _ := json.Marshal(c.Argv)
			fmt.Fprintf(out, "  %s · %s · cwd %s · timeout %dms · argv %s\n", c.ID, c.Category, c.Dir, c.TimeoutMS, args)
		}
		if p.Continuation == "" {
			if len(p.CodingAllowlist) == 0 {
				fmt.Fprintln(out, "Claude Code coding allowlist: none stored (plan predates derivation); the earlier fixed Node allowance applies.")
			} else {
				fmt.Fprintln(out, "Claude Code coding allowlist (approved with this plan; each Bash entry runs repository code):")
				for _, entry := range p.CodingAllowlist {
					fmt.Fprintf(out, "  %s\n", entry)
				}
			}
		}
		for _, i := range p.Analysis.Impacts {
			fmt.Fprintf(out, "Impact assessment: %s\n", i.Description)
		}
		for _, u := range p.Analysis.Unknowns {
			fmt.Fprintf(out, "Unknown: %s\n", u)
		}
		fmt.Fprintln(out, "Check execution: temporary HOME and reviewed environment; filesystem/network access is not OS-sandboxed. Network is unrestricted.")
		if r.Readiness.Phase == "scope-waiting" {
			fmt.Fprintf(out, "\nReview the full ledger, then approve exactly this plan:\nforgecell run %d --lab %q --formula %q --base %s --target %q --approve %s\n", r.Issue.Number, r.LabDir, r.FormulaID, p.Inputs.BaseCommit, p.Inputs.TargetBranch, proposal.Digest)
		}
	}
	for _, a := range r.Atoms {
		if a.Status == "blocked" || a.Status == "failed" {
			fmt.Fprintf(out, "%s: %s\n", a.ID, a.Detail)
		}
	}
	if r.Verification != nil {
		fmt.Fprintf(out, "Required checks passed: %t · independent acceptance verified: %t\n", r.Verification.RequiredChecksPassed, r.Verification.IndependentAcceptanceVerified)
	}
	fmt.Fprintf(out, "Ledger: %s\n", filepath.Join(r.LabDir, "ledgers", r.ID+".md"))
}

func renderProposal(out, stderr io.Writer, p onboarding.Proposal, dir string, jsonMode bool) int {
	f, err := formula.Parse([]byte(p.YAML))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	next := "No further decision; historical proposal remains inspectable."
	if p.Status == "pending" {
		next = fmt.Sprintf("forgecell init --lab %q --approve %s (or --dismiss %s); stale state is checked at approval", dir, p.ID, p.ID)
	}
	if p.Status == "approved" {
		next = "Approved; no new approval required. Runs retain their human gates."
	}
	incomplete, err := onboarding.ProposalActivationIncomplete(dir, p)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if incomplete {
		next = fmt.Sprintf("Resume exact approval: forgecell init --lab %q --approve %s", dir, p.ID)
	}
	var process map[string]any
	if err = yaml.Unmarshal([]byte(p.YAML), &process); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	command := fmt.Sprintf("forgecell init --lab %q --review %s", dir, p.ID)
	if jsonMode {
		return encode(out, stderr, map[string]any{"proposal": p, "verified": true, "repository": f.Intake.Repo, "harness": f.Harness.Binding, "atoms": process["atoms"], "repositoryContext": process["repositoryContext"], "reviewCommand": command, "nextAction": next, "metaBinding": proposalMeta(p), "beforeYaml": proposalMeta(p).BeforeYAML, "capabilityEvidence": proposalCapabilities(p), "capabilityEvidenceSource": "saved observation; not current verification"})
	}
	fmt.Fprintf(out, "Formula proposal %s: %s\nRepository: %s\nCoding harness: %s\nVerified SHA-256: %s\nReview: %s\nEvidence: %s · %s\nOrdered Atoms (gates and check identities):\n", p.ID, p.Status, f.Intake.Repo, f.Harness.Binding, p.YAMLHash, command, p.EvidencePath, p.EvidenceHash)
	report := proposalCapabilities(p)
	renderCapabilities(out, report.Candidate, report.LearnBound, "saved observation; not current verification")
	meta := proposalMeta(p)
	renderMeta(out, meta, "saved observation; not current verification")
	if meta.BeforeYAML != "" {
		fmt.Fprintf(out, "Exact YAML before proposed change:\n%s\n", meta.BeforeYAML)
	}
	atoms, _ := yaml.Marshal(process["atoms"])
	fmt.Fprintln(out, string(atoms))
	if c, ok := process["repositoryContext"].(map[string]any); ok {
		checks, _ := yaml.Marshal(c["components"])
		fmt.Fprintf(out, "Component checks (proposed, not executed):\n%s\n", checks)
	}
	fmt.Fprintf(out, "Next action: %s\nExact Formula YAML:\n%s", next, p.YAML)
	return 0
}

func proposalCapabilities(p onboarding.Proposal) onboarding.BindingReport {
	if p.CapabilityEvidence != nil {
		return *p.CapabilityEvidence
	}
	f, _ := formula.Parse([]byte(p.YAML))
	c := onboarding.Candidate{ID: f.Harness.Binding, Readiness: "unknown", Detail: "Historical capability evidence was not recorded."}
	c.Capabilities = harness.AdapterCapabilities("")
	c = c.CompleteUnknowns()
	return onboarding.BindingReport{Command: f.Harness.Command, Candidate: c, Workflow: c.WorkflowCapability(), LearnBound: onboarding.LearnBound(f)}
}
func selectedWorkflow(candidates []onboarding.Candidate, id string) harness.Capability {
	for _, c := range candidates {
		if c.ID == id {
			return c.WorkflowCapability()
		}
	}
	return harness.Capability{State: "unknown", Reason: "No binding selected."}
}
func renderCapabilities(out io.Writer, c onboarding.Candidate, learn bool, source string) {
	fmt.Fprintf(out, "Capabilities (%s):\nCLI probe: %s — %s\n", source, c.Readiness, c.Detail)
	for _, item := range []struct {
		name  string
		value harness.Capability
	}{{"Installed CLI", c.Installed}, {"Authentication", c.Authentication}, {"Coding adapter", c.Capabilities.Coding}, {"Read-only ticket analysis adapter", c.Capabilities.Analysis}, {"Installed analysis controls", c.Capabilities.AnalysisControls}, {"Meta-learning adapter", c.Capabilities.Meta}, {"Model access", c.Capabilities.ModelAccess}, {"GitHub Issue workflow", c.WorkflowCapability()}} {
		if item.value.State == "" {
			item.value = harness.Capability{State: "unknown", Reason: "Not recorded."}
		}
		fmt.Fprintf(out, "%s: %s — %s\n", item.name, item.value.State, item.value.Reason)
	}
	fmt.Fprintf(out, "Learn command bound: %t\n", learn)
}

func proposalMeta(p onboarding.Proposal) onboarding.MetaReport {
	if p.CapabilityEvidence != nil && p.CapabilityEvidence.Meta != nil {
		return *p.CapabilityEvidence.Meta
	}
	f, _ := formula.Parse([]byte(p.YAML))
	for _, a := range f.Atoms {
		if a.Type == "learn" {
			return onboarding.DescribeMeta(a.Command, a.Binding, a.TimeoutMS, onboarding.Candidate{})
		}
	}
	return onboarding.DescribeMeta(nil, "", 0, onboarding.Candidate{})
}
func renderMeta(out io.Writer, r onboarding.MetaReport, source string) {
	argv, _ := json.Marshal(r.Command)
	fmt.Fprintf(out, "Meta harness (%s): %s — %s\nCommand argv: %s\nTimeout: %d ms\nInstalled CLI: %s — %s\nAuthentication: %s — %s\nModel access: %s — %s\nMeta containment: %s — %s\nPermission limits: %s\nEvidence inputs: %s\nLearning consent: %s\n", source, r.Capability.State, r.Capability.Reason, argv, r.TimeoutMS, r.Candidate.Installed.State, r.Candidate.Installed.Reason, r.Candidate.Authentication.State, r.Candidate.Authentication.Reason, r.Candidate.Capabilities.ModelAccess.State, r.Candidate.Capabilities.ModelAccess.Reason, r.Containment.State, r.Containment.Reason, r.Restrictions, r.EvidenceInputs, r.NextAction)
}
