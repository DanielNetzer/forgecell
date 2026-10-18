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
	"github.com/DanielNetzer/forgecell/lab/internal/extension"
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
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, version string) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	harness.SetRunningVersion(version)
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(out, "Forgecell Lab — Go migration preview\n\ninit [--lab DIR] [--harness ID | --rebind ID] [--meta-harness ID | --meta-command JSON_ARGV] [--harness-timeout-ms MS] [--max-timeout-ms MS] [--json] [--review ID | --approve ID | --dismiss ID]\ninit --model ID [--role analysis|coding|meta|review] [--lab DIR] [--json]\ndoctor [--lab DIR] [--harness ID] [--json] [--verbose]\nrun <issue> [--lab DIR] [--formula ID] [--base COMMIT] [--target BRANCH] [--approve DIGEST] [--json]\namend <molecule> --parent DIGEST --plan FILE [--accept-existing-tree TREE] [--lab DIR]\nrecover <molecule> --plan DIGEST --confirm-stopped [--lab DIR]\nloop [--lab DIR] [--select oldest|label:L|milestone:M] [--max-concurrent N] [--once]\nloop [--lab DIR] --watch [--interval DURATION] [--learn]\nledger <molecule> [--lab DIR]\nissues --repo OWNER/REPO [--page N --page-size N | --select ISSUE_REF] [--lab DIR] [--json]\nlearn <molecule...> [--lab DIR] [--json]\nlearn --pending [--lab DIR] [--json]\nsuggestion <id> [--lab DIR] [--approve | --dismiss | --link-evaluation DIR | --baseline-ledger ID --candidate-ledger ID --comparability-basis TEXT] [--evaluation-parent DIR] [--json]\nrollback\ndev install [--from CHECKOUT] [--activate] [--allow-dirty]\nevaluate --inputs PLAN --out NEW_DIRECTORY [--source REPO] [--approve DIGEST]\nevaluate --cohort FILE --out DIRECTORY [--source REPO] [--approve DIGEST] [--recheck]\nevaluate replay --repo OWNER/REPO --pr N [--pr M ...] --source CHECKOUT --out NEW_DIRECTORY\ndeliver <molecule> --lab DIR --base BRANCH [--approve DIGEST --title TITLE] -- FILE...\nchecks --lab DIR --molecule ID --repo OWNER/REPO --pr NUMBER --commit SHA --required NAME [--required NAME]\nlabs [--json]\n--version\n--protocol\n\nThe default Lab is keyed by the checkout's GitHub origin (OWNER/REPO), not its path; --lab DIR always wins and --lab-name NAME selects a named Lab for the repository on any command that takes a Lab. labs lists every Lab, read-only.\n\ndev install is developer-only: it builds the checkout's lab/cmd/forgecell with the local Go toolchain and then runs the result, so the checkout is trusted code. Without --activate the build is only staged under releases/ and current is unchanged. A tree with uncommitted, untracked or ignored Go source changes is refused unless --allow-dirty is given. --protocol prints the adapter protocol version.\n\nRuns analyze a ticket before coding and wait for exact scope approval. Required checks run independently before final review.\n\nloop starts intake for eligible open Issues of the Formula's repository, oldest first, and approves a scope gate only where the Formula declares a policy that approves that exact plan. It stops each Molecule at any human gate, open question or failure and reports why; it never delivers, merges or approves a human gate. --once makes a single pass. --watch repeats single passes every --interval (default 5m, at least 1m) until SIGINT or SIGTERM, which stop it between passes after in-flight Molecules finish; it cannot be combined with --once. --learn, which is off by default, runs the pending learn requests after each pass like learn --pending; suggestions stay pending for your decision. For an interrupted Molecule whose owner is gone, the loop saves a recovery proposal (the exact forgecell recover command) under the Lab's recovery-proposals directory and shows it in its report and in doctor; it never runs recover or amend.\n\nTicket readiness is a development preview; existing Formulas require an explicitly reviewed scope gate.")
		return 0
	}
	if args[0] == "--version" || args[0] == "-v" {
		fmt.Fprintln(out, version)
		return 0
	}
	if args[0] == "--protocol" {
		fmt.Fprintln(out, harness.AdapterProtocolVersion)
		return 0
	}
	if args[0] == "labs" {
		return runLabs(args[1:], out, stderr)
	}
	if args[0] == "dev" {
		return runDev(ctx, args[1:], version, out, stderr)
	}
	defaultLab := ""
	if labCommands[args[0]] || args[0] == "loop" {
		rest, name, explicitLab, err := labArgs(args)
		if err != nil {
			return fail(err)
		}
		args = rest
		if !explicitLab {
			resolved, err := resolveDefaultLab(ctx, name)
			if err != nil {
				return fail(err)
			}
			if resolved.Notice != "" {
				fmt.Fprintln(stderr, "forgecell:", resolved.Notice)
			}
			defaultLab = resolved.Dir
		}
	}

	if args[0] == "issues" {
		return runIssueQueue(ctx, args[1:], defaultLab, out, stderr)
	}

	if args[0] == "loop" {
		return runLoopCommand(ctx, args[1:], defaultLab, out, stderr)
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
		pending := flags.Bool("pending", false, "Run the pending learn requests queued by recurring failures")
		ids := []string{}
		i := 1
		for i < len(args) && !strings.HasPrefix(args[i], "-") {
			ids = append(ids, args[i])
			i++
		}
		if err := flags.Parse(args[i:]); err != nil {
			return 1
		}
		if *pending {
			if args[0] != "learn" || len(ids) != 0 || flags.NArg() != 0 || *approve || *dismiss || *linkEvaluation != "" || *baselineLedger != "" || *candidateLedger != "" || *basis != "" || len(parents) != 0 {
				return fail(fmt.Errorf("learn --pending runs the queued learn requests and cannot be combined with Molecule ids or other decisions"))
			}
			return runLearnPending(ctx, *lab, *jsonMode, out, stderr)
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
				fmt.Fprintf(out, "Approval changes the future recipe; it does not prove improvement. Inspect commands and permissions.\nforgecell suggestion %s --lab %s --approve\n", p.ID, shellWord(*lab))
			}
			fmt.Fprintf(out, "forgecell suggestion %s --lab %s --dismiss\n", p.ID, shellWord(*lab))
		}
		return 0
	}

	if args[0] == "evaluate" {
		if len(args) > 1 && args[1] == "replay" {
			return runEvaluateReplay(ctx, args, out, stderr)
		}
		for _, a := range args[1:] {
			if a == "--cohort" || a == "-cohort" || strings.HasPrefix(a, "--cohort=") || strings.HasPrefix(a, "-cohort=") {
				return runEvaluateCohort(ctx, args, out, stderr)
			}
		}
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
		rebind := flags.String("rebind", "", "Propose a binding-only rebind of the approved default harness for review")
		metaHarness := flags.String("meta-harness", "", "Explicit separate native meta binding")
		metaArgv := flags.String("meta-command", "", "BYO meta argv as JSON; containment unknown")
		timeoutMS := flags.Int("harness-timeout-ms", 0, "Reviewed coding attempt budget in ms (default 900000)")
		maxTimeoutMS := flags.Int("max-timeout-ms", 0, "Ceiling for reviewed per-Molecule timeouts in ms (default 3600000)")
		model := flags.String("model", "", "Propose a model pin on the approved Formula's native Claude bindings; an empty value removes the pin")
		role := flags.String("role", "", "With --model, limit the pin to one declared harnesses.ROLE override (analysis, coding, meta or review)")
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
		metaRequested, rebindRequested, modelRequested := false, false, false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "meta-harness" || f.Name == "meta-command" {
				metaRequested = true
			}
			rebindRequested = rebindRequested || f.Name == "rebind"
			modelRequested = modelRequested || f.Name == "model"
		})
		if modelRequested || *role != "" {
			switch {
			case !modelRequested:
				return fail(fmt.Errorf("--role needs --model"))
			case *preferred != "" || rebindRequested || metaRequested || *timeoutMS != 0 || *maxTimeoutMS != 0:
				return fail(fmt.Errorf("--model changes only the model pin; do not combine it with --harness, --rebind, --meta-harness, --meta-command or the timeout flags"))
			case *review != "" || *approve != "" || *dismiss != "":
				return fail(fmt.Errorf("--model proposes a change; review, approve or dismiss the saved proposal in a separate command"))
			}
		}
		if rebindRequested {
			switch {
			case *rebind == "":
				return fail(fmt.Errorf("--rebind needs a harness ID, for example --rebind codex"))
			case *preferred != "":
				return fail(fmt.Errorf("--rebind and --harness cannot be combined; use --rebind for a binding-only change"))
			case metaRequested:
				return fail(fmt.Errorf("--rebind cannot be combined with --meta-harness or --meta-command"))
			case *timeoutMS != 0 || *maxTimeoutMS != 0:
				return fail(fmt.Errorf("--rebind cannot be combined with the timeout flags; it changes only the binding"))
			case *review != "" || *approve != "" || *dismiss != "":
				return fail(fmt.Errorf("--rebind proposes a change; review, approve or dismiss the saved proposal in a separate command"))
			}
		}
		if metaRequested && (*review != "" || *approve != "" || *dismiss != "") {
			return fail(fmt.Errorf("meta opt-in cannot accompany saved review or decisions"))
		}
		if (*timeoutMS != 0 || *maxTimeoutMS != 0) && (*review != "" || *approve != "" || *dismiss != "") {
			return fail(fmt.Errorf("timeout budget flags cannot accompany saved review or decisions"))
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
					return fail(fmt.Errorf("existing checkout Lab at %s; use --lab %s to continue it, or move its contents to the home Lab before retrying", legacy, shellWord(legacy)))
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
		launcher := initLauncher()
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
		result, err := onboarding.Prepare(ctx, onboarding.InitOptions{Cwd: cwd, LabDir: dir, Launcher: launcher, Preferred: *preferred, Rebind: *rebind, Env: env, MetaHarness: *metaHarness, MetaCommand: metaCommand, HarnessTimeoutMS: *timeoutMS, MaxTimeoutMS: *maxTimeoutMS, Model: *model, ModelSet: modelRequested, Role: *role})
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
		jsonFlag := flags.Bool("json", true, "Structured output (the default)")
		verbose := flags.Bool("verbose", false, "Human-readable probe steps; with --json adds steps to the structured output")
		if err := flags.Parse(args[1:]); err != nil {
			return 1
		}
		if flags.NArg() != 0 {
			return fail(fmt.Errorf("unexpected doctor arguments"))
		}
		explicitJSON := false
		flags.Visit(func(f *flag.Flag) { explicitJSON = explicitJSON || f.Name == "json" })
		structured := !*verbose || explicitJSON && *jsonFlag
		cwd, err := os.Getwd()
		if err != nil {
			return fail(err)
		}
		active, err := formula.Inspect(*lab, "")
		if err != nil {
			return fail(describeFormulaLoadFailure(*lab, err))
		}
		candidates := onboarding.Discover(ctx, cwd, doctorRun)
		pathReady := map[string]bool{}
		for _, c := range candidates {
			pathReady[c.ID] = c.Readiness == "ready"
		}
		existing := ""
		var saved *onboarding.Candidate
		// A Formula without a current exact-byte approval names a provider nobody
		// has reviewed, so doctor reads it but never executes it.
		unapproved := false
		// Probes run what the approved bytes declare, not the file read at inspection.
		probed := active.Formula
		if active.Formula.ID != "" {
			loaded, loadErr := formula.Load(*lab, active.Formula.ID)
			unapproved = loadErr != nil || !loaded.Approved
			if !unapproved {
				probed = loaded.Formula
			}
		}
		if active.Formula.ID != "" {
			existing = active.Formula.Harness.Binding
			var checked onboarding.Candidate
			if unapproved {
				checked = onboarding.Candidate{ID: existing, Readiness: "blocked", Detail: notProbedUnapproved}
				checked = checked.CompleteUnknowns()
			} else {
				checked = onboarding.ProbeBinding(ctx, probed.Harness, cwd, doctorRun)
			}
			if checked.Binding != nil {
				checked.Binding.WithRunningVersion(version)
			}
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
		git := doctorRun(ctx, process.Options{Argv: []string{"git", "--version"}, Dir: cwd, Stdin: []byte{}, Timeout: 5 * time.Second})
		gh := doctorRun(ctx, process.Options{Argv: []string{"gh", "auth", "status"}, Dir: cwd, Stdin: []byte{}, Timeout: 5 * time.Second})
		var meta onboarding.MetaReport
		if unapproved {
			meta = onboarding.DescribeFormulaMeta(active.Formula)
			if meta.Binding != "" {
				meta.NextAction = notProbedUnapproved
			}
		} else {
			meta = onboarding.InspectMeta(ctx, probed, cwd, doctorRun)
		}
		workflow := selectedWorkflow(candidates, selection.ID)
		ready := !unapproved && workflow.State == "conditional" && git.OK && gh.OK && selection.Status == "ready" && (saved == nil || saved.Readiness == "ready")
		labDir := *lab
		if abs, e := filepath.Abs(labDir); e == nil {
			labDir = abs
		}
		commands := doctorCommands{lab: labDir, pathReady: pathReady}
		roles := []onboarding.RoleReadiness{}
		if active.Formula.ID != "" && !unapproved {
			roles = onboarding.InspectRoles(ctx, probed, cwd, doctorRun)
		}
		firstRoleCommand := ""
		rolesReady := true
		extensions := inspectExtensions(labDir, active.Formula)
		extensionLocks := inspectExtensionLocks(labDir)
		isolation := inspectIsolation(active.Formula)
		for i := range roles {
			describeAnalysisWithExtensions(&roles[i], active.Formula)
			if !roles[i].Ready {
				rolesReady = false
				roles[i].NextCommand = commands.candidate(roles[i].Candidate)
			}
			if roles[i].Role == string(formula.MetaHarness) {
				coding, _ := active.Formula.RoleBinding(formula.CodingHarness)
				if fix := commands.staleMeta(active.Formula, roles[i].Candidate, coding.Binding); fix != "" {
					roles[i].NextCommand = fix
				}
			}
			if !roles[i].Ready && firstRoleCommand == "" {
				firstRoleCommand = roles[i].NextCommand
			}
		}
		if len(active.Formula.Harnesses) > 0 {
			ready = !unapproved && git.OK && gh.OK && rolesReady
		}
		withSteps := *verbose && structured
		views := make([]doctorCandidate, len(candidates))
		for i, c := range candidates {
			views[i] = commands.view(c, withSteps)
		}
		var savedView *doctorCandidate
		if saved != nil {
			v := commands.view(*saved, withSteps)
			savedView = &v
		}
		next := ""
		if !ready {
			next = commands.overall(savedView, views, selection)
			if len(active.Formula.Harnesses) > 0 && firstRoleCommand != "" {
				next = firstRoleCommand
			}
			if unapproved {
				next = "forgecell init --lab " + shellWord(labDir)
			}
		}
		note := "Read-only checks; no model invoked. Repository policy and model access are not verified by authentication."
		learnItems, learnErr := listPendingLearn(labDir)
		proposals, proposalErr := molecule.ReadRecoveryProposals(labDir, molecule.AnalysisTimeout(active.Formula))
		if structured {
			report := map[string]any{"schemaVersion": doctorSchemaVersion, "roles": roles, "metaBinding": meta, "metaCandidates": views, "activeFormulaId": active.Formula.ID, "labDir": *lab, "legacyLabDir": existingCheckoutLab(), "savedBinding": savedView, "learnBound": onboarding.LearnBound(active.Formula), "workflow": workflow, "gitReady": git.OK, "githubAuthenticated": gh.OK, "candidates": views, "selection": selection, "note": note}
			if next != "" {
				report["nextCommand"] = next
			}
			if len(extensions) > 0 {
				report["extensions"] = extensions
			}
			if len(extensionLocks) > 0 {
				report["extensionLocks"] = extensionLocks
			}
			report["isolation"] = isolation
			if len(learnItems) > 0 {
				report["learnRequests"] = learnItems
			}
			if learnErr != nil {
				report["learnRequestsError"] = flatten(learnErr.Error())
			}
			if len(proposals) > 0 {
				report["recoveryProposals"] = proposals
			}
			if proposalErr != nil {
				report["recoveryProposalsError"] = flatten(proposalErr.Error())
			}
			if err = json.NewEncoder(out).Encode(report); err != nil {
				return fail(err)
			}
		} else {
			renderDoctor(out, doctorView{Meta: &meta, Lab: *lab, FormulaID: active.Formula.ID, Git: git.OK, GitHub: gh.OK, Selection: selection, Workflow: workflow, Saved: savedView, Candidates: views, Roles: roles, Extensions: extensions, ExtensionLocks: extensionLocks, Isolation: isolation, Next: next, Note: note, LearnPending: learnNoticeLines(labDir, learnItems), LearnError: learnErr, RecoveryProposals: proposals, RecoveryError: proposalErr})
		}
		if !ready {
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
		renderLearnNotices(stderr, stderr, *lab)
	} else {
		presentRun(out, r)
		renderLearnNotices(out, stderr, *lab)
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

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// initLauncher names the launcher a new binding should save. The managed launcher
// (FORGECELL_BIN/forgecell) stays valid across upgrades because it resolves to the
// active release at run time; otherwise the installer's current-release path is used.
func initLauncher() string {
	if managed, ok := harness.ManagedLauncher(os.Getenv("FORGECELL_BIN")); ok {
		return managed
	}
	return os.Getenv("FORGECELL_LAUNCHER")
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
		if len(p.ExpectedProtectedDeltas) > 0 {
			fmt.Fprintln(out, "EXPECTED PROTECTED-TEST FAILURES — approving this exact digest accepts these failures in the protected regression run:")
			for _, d := range p.ExpectedProtectedDeltas {
				fmt.Fprintf(out, "  %s %s · check %s · justified by scope path %s — %s\n", d.Package, d.Test, d.CheckID, d.ScopePath, d.Reason)
			}
			fmt.Fprintln(out, "  Any other failing test still fails the check, including the parent of a declared subtest, which must be declared explicitly. A declared test that does not fail is only a warning; amend the plan to remove it.")
		} else if advisory := p.ProtectedDeltaAdvisory(protectedFailures(r)...); advisory != "" {
			fmt.Fprintln(out, advisory)
		}
		if p.Continuation == "" {
			fmt.Fprintln(out, codingTimeoutLine(r.FormulaSnapshot.YAML, p.HarnessTimeoutMS))
			if len(p.CodingAllowlist) == 0 {
				fmt.Fprintln(out, "Claude Code coding allowlist: none stored (plan predates derivation); the earlier fixed Node allowance applies.")
			} else {
				fmt.Fprintln(out, "Claude Code coding allowlist (approved with this plan; each Bash entry runs repository code):")
				for _, entry := range p.CodingAllowlist {
					fmt.Fprintf(out, "  %s\n", entry)
				}
			}
		}
		presentExtensions(out, r)
		for _, i := range p.Analysis.Impacts {
			fmt.Fprintf(out, "Impact assessment: %s\n", i.Description)
		}
		for _, u := range p.Analysis.Unknowns {
			fmt.Fprintf(out, "Unknown: %s\n", u)
		}
		fmt.Fprintln(out, "Check execution: temporary HOME and reviewed environment; filesystem/network access is not OS-sandboxed. Network is unrestricted.")
		if r.Readiness.Phase == "scope-waiting" {
			fmt.Fprintf(out, "\nReview the full ledger, then approve exactly this plan:\nforgecell run %d --lab %s --formula %s --base %s --target %s --approve %s\n", r.Issue.Number, shellWord(r.LabDir), shellWord(r.FormulaID), shellWord(p.Inputs.BaseCommit), shellWord(p.Inputs.TargetBranch), proposal.Digest)
		}
	}
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		presentExtensions(out, r)
	}
	for _, a := range r.Atoms {
		if a.Status == "blocked" || a.Status == "failed" {
			fmt.Fprintf(out, "%s: %s\n", a.ID, a.Detail)
		}
	}
	if r.Verification != nil {
		fmt.Fprintf(out, "Required checks passed: %t · independent acceptance verified: %t\n", r.Verification.RequiredChecksPassed, r.Verification.IndependentAcceptanceVerified)
		if len(r.Verification.AcceptedDeltas) > 0 {
			fmt.Fprintln(out, "ACCEPTED PROTECTED DELTAS — these tests failed in the protected regression run as declared and approved:")
			for _, d := range r.Verification.AcceptedDeltas {
				fmt.Fprintf(out, "  %s %s · check %s · justified by scope path %s — %s\n", d.Package, d.Test, d.CheckID, d.ScopePath, d.Reason)
			}
		}
		for _, w := range r.Verification.Warnings {
			fmt.Fprintf(out, "Warning: %s\n", w)
		}
	}
	presentReview(out, r)
	if lines := molecule.UsageSummaryLines(r); len(lines) > 0 {
		fmt.Fprintln(out, "Usage and cost (evidence, never an approval):")
		for _, line := range lines {
			fmt.Fprintln(out, line)
		}
	}
	fmt.Fprintf(out, "Ledger: %s\n", filepath.Join(r.LabDir, "ledgers", r.ID+".md"))
}

const (
	maxPresentedFindings = 10
	maxPresentedDrafts   = 5
	presentedFieldRunes  = 300
)

// presentReview shows the latest adversarial review pass at the gate, preceded by
// one line for each earlier pass so a blocked pass is not hidden behind a later
// clean one. Findings come from model output, so each field is cut and stripped of
// control characters and terminal escape sequences again here, whatever the ledger
// holds. A review is evidence for the reviewer; it never approves delivery.
func presentReview(out io.Writer, r molecule.Record) {
	if len(r.ReviewAttempts) == 0 {
		return
	}
	text := func(s string) string { return harness.ReviewDisplayText(s, presentedFieldRunes) }
	last := len(r.ReviewAttempts) - 1
	a := r.ReviewAttempts[last]
	for i, e := range r.ReviewAttempts[:last] {
		tree := "a different tree"
		if e.Tree == a.Tree {
			tree = "the same retained tree"
		}
		fmt.Fprintf(out, "Earlier review pass %d of %d: %s · %d high · %d medium · %d low · on %s\n", i+1, len(r.ReviewAttempts), text(e.Status), e.High, e.Medium, e.Low, tree)
	}
	fmt.Fprintf(out, "Adversarial review: %s · %d high · %d medium · %d low · evidence only, never an approval\n", text(a.Status), a.High, a.Medium, a.Low)
	if a.Binding != "" {
		fmt.Fprintf(out, "  Reviewer binding: %s (%s)\n", text(a.Binding), text(a.BindingSource))
	}
	if a.SameCommandAsCoding {
		fmt.Fprintln(out, "  NOTICE: the review binding runs the same command as the coding binding, so the coding harness reviewed its own change.")
	}
	if a.Redactions > 0 {
		fmt.Fprintf(out, "  NOTICE: %d secret-like value(s) were redacted from what the reviewer saw, so its view was incomplete.\n", a.Redactions)
	}
	if a.ChangedFiles > 0 {
		fmt.Fprintf(out, "  Coverage: the reviewer listed %d file(s) as reviewed for %d changed path(s)\n", a.ReviewedFiles, a.ChangedFiles)
	}
	if a.Reason != "" {
		fmt.Fprintf(out, "  Reason: %s\n", text(a.Reason))
	}
	for i, f := range a.Findings {
		if i == maxPresentedFindings {
			fmt.Fprintf(out, "  … %d more finding(s) are in the ledger\n", len(a.Findings)-i)
			break
		}
		fmt.Fprintf(out, "  [%s · %s] %s:%d — %s\n      Scenario: %s\n      Fix: %s\n", text(f.Severity), text(f.Status), text(f.File), f.Line, text(f.Claim), text(f.Scenario), text(f.Fix))
	}
	for i, d := range a.FollowUps {
		if i == maxPresentedDrafts {
			fmt.Fprintf(out, "  … %d more draft follow-up(s) are in the ledger\n", len(a.FollowUps)-i)
			break
		}
		fmt.Fprintf(out, "  Draft follow-up (not filed): %s\n", text(d.Title))
	}
}

// protectedFailures lists the tests that failed in the protected regression run
// recorded for the Molecule, so the delta advisory can name them.
func protectedFailures(r molecule.Record) []readiness.FailingTest {
	if r.Verification == nil || r.Verification.Protected == nil {
		return nil
	}
	var failing []readiness.FailingTest
	for _, o := range r.Verification.Protected.Checks {
		if o.Result.OK {
			continue
		}
		for _, id := range o.FailedTests {
			failing = append(failing, readiness.FailingTest{Package: harness.RedactSecrets(id.Package), Test: harness.RedactSecrets(id.Test)})
		}
	}
	return failing
}

// codingTimeoutLine states the timeout the approved plan will use, so a reviewed
// per-Molecule value is visible at the gate next to the Formula default and maximum.
func codingTimeoutLine(snapshot string, planMS int) string {
	f, err := formula.Parse([]byte(snapshot))
	if err != nil {
		if planMS > 0 {
			return fmt.Sprintf("Coding harness timeout: %dms (reviewed per-Molecule harnessTimeoutMs)", planMS)
		}
		return "Coding harness timeout: Formula timeoutMs unset (process default applies)"
	}
	b, _ := f.RoleBinding(formula.CodingHarness)
	switch {
	case planMS > 0:
		return fmt.Sprintf("Coding harness timeout: %dms (reviewed per-Molecule harnessTimeoutMs; Formula timeoutMs %d, maximum %d)", planMS, b.TimeoutMS, b.TimeoutCeilingMS())
	case b.TimeoutMS == 0 && b.EffectiveTimeoutMS() > 0:
		return fmt.Sprintf("Coding harness timeout: %dms (Formula timeoutMs unset; clamped to maxTimeoutMs)", b.EffectiveTimeoutMS())
	case b.TimeoutMS == 0:
		return "Coding harness timeout: Formula timeoutMs unset (process default applies)"
	case b.TimeoutCeilingMS() > b.TimeoutMS:
		return fmt.Sprintf("Coding harness timeout: %dms (Formula timeoutMs; maximum %d, no per-Molecule extension proposed)", b.TimeoutMS, b.TimeoutCeilingMS())
	}
	return fmt.Sprintf("Coding harness timeout: %dms (Formula timeoutMs; the Formula declares no maximum, so no extension is possible)", b.TimeoutMS)
}

// presentExtensions shows what each pinned extension grants and how its cache
// came to be. The disclosure appears only when a server was passed to analysis.
func presentExtensions(out io.Writer, r molecule.Record) {
	if len(r.Extensions) == 0 {
		return
	}
	fmt.Fprintln(out, "Extensions (pinned MCP servers; the declared network claim is not enforced by Forgecell):")
	for _, e := range r.Extensions {
		for _, line := range e.Lines() {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	if len(r.AnalysisExtensionsAttached()) > 0 {
		fmt.Fprintln(out, molecule.AnalysisWithoutSafeModeDisclosure)
	}
}

type doctorExtensionRole struct {
	Role    string             `json:"role"`
	Binding string             `json:"binding"`
	Adapter harness.Capability `json:"adapter"`
}

type doctorExtension struct {
	Name       string                `json:"name"`
	Manifest   string                `json:"manifest"`
	SHA256     string                `json:"sha256"`
	Required   bool                  `json:"required,omitempty"`
	Status     string                `json:"status"`
	Detail     string                `json:"detail,omitempty"`
	FixCommand string                `json:"fixCommand,omitempty"`
	Roles      []doctorExtensionRole `json:"roles"`
}

// inspectExtensions loads and verifies each declared extension and reports the
// adapter support of every binding that attaches it. It reads manifests and
// pinned files only; it never runs prepare, a server or a model.
func inspectExtensions(labDir string, f formula.Formula) []doctorExtension {
	names := make([]string, 0, len(f.Extensions))
	for name := range f.Extensions {
		names = append(names, name)
	}
	sort.Strings(names)
	report := []doctorExtension{}
	for _, name := range names {
		ref := f.Extensions[name]
		e := doctorExtension{Name: name, Manifest: ref.Manifest, SHA256: ref.SHA256, Required: ref.Required, Status: "verified", Roles: []doctorExtensionRole{}}
		path := filepath.Join(labDir, filepath.FromSlash(ref.Manifest))
		m, _, err := extension.Load(labDir, ref.Manifest, ref.SHA256)
		switch {
		case err != nil:
			e.Status, e.Detail = "problem", err.Error()
			if strings.HasPrefix(err.Error(), "extension manifest "+ref.Manifest+" changed") {
				e.FixCommand = "shasum -a 256 " + shellWord(path)
				e.Detail += "; review the manifest, then update extensions." + name + ".sha256 through a reviewed Formula change"
			} else {
				e.FixCommand = "grep -n '\"path\"' " + shellWord(path)
				e.Detail += "; repair or re-pin the file the message names through a reviewed manifest and Formula change"
			}
		case m.Name != name:
			e.Status, e.Detail = "problem", fmt.Sprintf("the manifest names the extension %q but the Formula declares %q", m.Name, name)
			e.FixCommand = "grep -n '\"name\"' " + shellWord(path)
		}
		for _, role := range []formula.HarnessRole{formula.AnalysisHarness, formula.CodingHarness} {
			if !slices.Contains(f.RoleExtensions(role), name) {
				continue
			}
			b, _ := f.RoleBinding(role)
			e.Roles = append(e.Roles, doctorExtensionRole{Role: string(role), Binding: b.Binding, Adapter: harness.BindingExtensionCapability(b.Command)})
		}
		report = append(report, e)
	}
	return report
}

// doctorExtensionLock is an extension cache lock that nothing should wait on.
type doctorExtensionLock struct {
	Path       string `json:"path"`
	Reason     string `json:"reason"`
	FixCommand string `json:"fixCommand"`
}

// inspectExtensionLocks lists the stale extension cache locks under the Lab. It
// only reads: it never removes a lock and never runs prepare. The fix is a command
// for the operator to run once nothing is building.
func inspectExtensionLocks(labDir string) []doctorExtensionLock {
	stale := extension.StaleLocks(labDir)
	report := make([]doctorExtensionLock, 0, len(stale))
	for _, s := range stale {
		report = append(report, doctorExtensionLock{Path: s.Path, Reason: s.Reason, FixCommand: "rm -r -- " + shellWord(s.Path)})
	}
	return report
}

// describeFormulaLoadFailure says which Formula failed to load and why, and what to
// do about it, instead of the bare validation error.
func describeFormulaLoadFailure(labDir string, err error) error {
	if strings.HasPrefix(err.Error(), "invalid Lab configuration") {
		return err
	}
	return fmt.Errorf("the active Formula in %s cannot be loaded: %w; fix or replace that Formula (a Formula that already used a top-level `extensions` key for something else must rename it, because `extensions` is now a reserved mapping of name to declaration), then run doctor again", labDir, err)
}

// describeAnalysisWithExtensions replaces the analysis role description when
// extensions are attached: that analysis runs without safe mode, so the usual
// controls description would overstate it.
func describeAnalysisWithExtensions(role *onboarding.RoleReadiness, f formula.Formula) {
	if role.Role != string(formula.AnalysisHarness) || role.Capability.State != "conditional" || len(f.RoleExtensions(formula.AnalysisHarness)) == 0 {
		return
	}
	b, _ := f.RoleBinding(formula.AnalysisHarness)
	if b.AnalysisWrapper != nil || harness.BindingExtensionCapability(b.Command).State != "supported" {
		return
	}
	role.Capability.Reason = "Ticket analysis can be attempted, but with extensions attached it runs without safe mode (--setting-sources project instead of --safe-mode): user settings, plugins and hooks are excluded, built-in tools stay disabled and only the attached servers' read-only tools are allowed. Model access and the end-to-end workflow remain unverified."
}

type doctorAdapterIsolation struct {
	Adapter string `json:"adapter"`
	harness.Isolation
}

type doctorRoleIsolation struct {
	Role    string `json:"role"`
	Binding string `json:"binding"`
	Model   string `json:"model"`
	harness.Isolation
}

// doctorIsolation is the static isolation of every native adapter and of each
// role binding in the active Formula. It reads only the Formula and executes
// nothing.
type doctorIsolation struct {
	Adapters []doctorAdapterIsolation `json:"adapters"`
	Roles    []doctorRoleIsolation    `json:"roles"`
}

func inspectIsolation(f formula.Formula) doctorIsolation {
	report := doctorIsolation{Adapters: []doctorAdapterIsolation{}, Roles: []doctorRoleIsolation{}}
	for _, id := range []string{"claude-code", "codex", "cursor"} {
		report.Adapters = append(report.Adapters, doctorAdapterIsolation{Adapter: id, Isolation: harness.AdapterIsolation(id)})
	}
	if f.ID == "" {
		return report
	}
	for _, role := range append(slices.Clone(formula.HarnessRoles), formula.ReviewHarness) {
		b, declared := f.RoleBinding(role)
		if role == formula.ReviewHarness && !declared {
			continue
		}
		model := b.Model
		if model == "" {
			model = harness.ModelUnpinned
		}
		report.Roles = append(report.Roles, doctorRoleIsolation{Role: string(role), Binding: b.Binding, Model: model, Isolation: harness.BindingIsolation(b.Command)})
	}
	return report
}

func renderProposal(out, stderr io.Writer, p onboarding.Proposal, dir string, jsonMode bool) int {
	f, err := formula.Parse([]byte(p.YAML))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	next := "No further decision; historical proposal remains inspectable."
	if p.Status == "pending" {
		next = fmt.Sprintf("forgecell init --lab %s --approve %s (or --dismiss %s); stale state is checked at approval", shellWord(dir), p.ID, p.ID)
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
		next = fmt.Sprintf("Resume exact approval: forgecell init --lab %s --approve %s", shellWord(dir), p.ID)
	}
	var process map[string]any
	if err = yaml.Unmarshal([]byte(p.YAML), &process); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	command := fmt.Sprintf("forgecell init --lab %s --review %s", shellWord(dir), p.ID)
	if jsonMode {
		return encode(out, stderr, map[string]any{"proposal": p, "verified": true, "repository": f.Intake.Repo, "harness": f.Harness.Binding, "harnesses": f.Harnesses, "atoms": process["atoms"], "repositoryContext": process["repositoryContext"], "reviewCommand": command, "nextAction": next, "metaBinding": proposalMeta(p), "beforeYaml": proposalBeforeYAML(p), "capabilityEvidence": proposalCapabilities(p), "capabilityEvidenceSource": "saved observation; not current verification"})
	}
	fmt.Fprintf(out, "Formula proposal %s: %s\nRepository: %s\nCoding harness: %s\nVerified SHA-256: %s\nReview: %s\nEvidence: %s · %s\nOrdered Atoms (gates and check identities):\n", p.ID, p.Status, f.Intake.Repo, f.Harness.Binding, p.YAMLHash, command, p.EvidencePath, p.EvidenceHash)
	for _, role := range formula.HarnessRoles {
		if b, ok := f.Harnesses[string(role)]; ok {
			fmt.Fprintf(out, "Role binding %s: %s (overrides the default harness %s)\n", role, b.Binding, f.Harness.Binding)
		}
	}
	report := proposalCapabilities(p)
	renderCapabilities(out, report.Candidate, report.LearnBound, "saved observation; not current verification")
	renderRoleCapabilities(out, report.Roles, "saved observation; not current verification")
	meta := proposalMeta(p)
	renderMeta(out, meta, "saved observation; not current verification")
	if before := proposalBeforeYAML(p); before != "" {
		fmt.Fprintf(out, "Exact YAML before proposed change:\n%s\n", before)
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

const doctorSchemaVersion = "v1"

const notProbedUnapproved = "not probed (unapproved)"

// doctorRun executes every doctor probe: harness discovery, the saved, meta and
// role bindings, git and gh. Tests replace it so probe results do not depend on
// host load.
var doctorRun onboarding.Runner = process.Run

// doctorCandidate adds guidance to a probed candidate. Steps appear only when the
// caller asks for them with --verbose --json.
type doctorCandidate struct {
	onboarding.Candidate
	NextCommand string            `json:"nextCommand,omitempty"`
	StepDetail  []onboarding.Step `json:"steps,omitempty"`
}

type doctorView struct {
	Meta                       *onboarding.MetaReport
	Lab, FormulaID, Next, Note string
	Git, GitHub                bool
	Selection                  onboarding.Selection
	Workflow                   harness.Capability
	Saved                      *doctorCandidate
	Candidates                 []doctorCandidate
	Roles                      []onboarding.RoleReadiness
	Extensions                 []doctorExtension
	ExtensionLocks             []doctorExtensionLock
	Isolation                  doctorIsolation
	LearnPending               []string
	LearnError                 error
	RecoveryProposals          []molecule.RecoveryProposal
	RecoveryError              error
}

// doctorCommands chooses the one next command for a non-ready state, using only
// commands that exist today. pathReady records which harnesses init would find
// ready through PATH, because init discovery does not read saved bindings.
type doctorCommands struct {
	lab       string
	pathReady map[string]bool
}

func (x doctorCommands) view(c onboarding.Candidate, steps bool) doctorCandidate {
	v := doctorCandidate{Candidate: c, NextCommand: x.candidate(c)}
	if steps {
		v.StepDetail = c.Steps
	}
	return v
}

func (x doctorCommands) recheck(id string) string {
	command := "forgecell doctor --lab " + shellWord(x.lab)
	if id != "" {
		command += " --harness " + harnessArg(id)
	}
	return command
}

// staleMeta returns the reviewed proposal that repoints a meta binding (the learn
// Atom or a meta override) saved on an older Forgecell build, or "" when none
// applies. It keeps the coding binding, which init --harness with the meta adapter
// would change, and is offered only when the same launcher is one init --rebind
// repoints and the coding binding is a harness init discovers ready, so the
// command yields a proposal instead of a refusal.
func (x doctorCommands) staleMeta(f formula.Formula, c onboarding.Candidate, codingID string) string {
	if c.Binding == nil || !x.pathReady[codingID] || !onboarding.RebindRepoints(f, c.Binding.Launcher) {
		return ""
	}
	for _, s := range c.Binding.States {
		if strings.HasPrefix(s.State, "launcher-") {
			return fmt.Sprintf("forgecell init --lab %s --rebind %s", shellWord(x.lab), harnessArg(codingID))
		}
	}
	return ""
}

func harnessArg(id string) string {
	if safeID.MatchString(id) {
		return id
	}
	return shellWord(id)
}

func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func providerOnPath(provider string) bool {
	found, err := exec.LookPath(filepath.Base(provider))
	if err != nil {
		return false
	}
	a, ae := os.Stat(found)
	b, be := os.Stat(provider)
	return ae == nil && be == nil && os.SameFile(a, b)
}

func loginCommand(id string) string {
	return map[string]string{"codex": "codex login", "claude-code": "claude auth login", "cursor": "agent login"}[id]
}

func authFailed(c onboarding.Candidate) bool {
	for _, s := range c.Failures {
		if s.Step == "auth" {
			return true
		}
	}
	return false
}

// candidate returns the single next command for a candidate that is not ready.
// A failing provider outranks binding states: init proposes a rebind only for a
// ready provider.
func (x doctorCommands) candidate(c onboarding.Candidate) string {
	if c.Readiness == "ready" {
		return ""
	}
	if c.Executable != "" {
		if c.Installed.State == "verified" && c.Authentication.State == "verified" {
			init := fmt.Sprintf("forgecell init --lab %s --harness %s", shellWord(x.lab), harnessArg(c.ID))
			if !providerOnPath(c.Executable) {
				init = "PATH=" + shellWord(filepath.Dir(c.Executable)) + `:"$PATH" ` + init
			}
			return init
		}
		if login := loginCommand(c.ID); login != "" && c.Installed.State == "verified" && authFailed(c) {
			return login
		}
		return x.recheck(c.ID)
	}
	if c.Readiness == "blocked" && x.pathReady[c.ID] {
		return fmt.Sprintf("forgecell init --lab %s --harness %s", shellWord(x.lab), harnessArg(c.ID))
	}
	return x.recheck(c.ID)
}

// overall picks the command for the first thing blocking the doctor result.
func (x doctorCommands) overall(saved *doctorCandidate, views []doctorCandidate, selection onboarding.Selection) string {
	if saved != nil && saved.Readiness != "ready" {
		return saved.NextCommand
	}
	id := selection.ID
	if selection.Status == "blocked" {
		for _, v := range views {
			if v.ID == id && v.Readiness != "ready" {
				return v.NextCommand
			}
		}
	}
	if id == "" {
		for _, v := range views {
			if v.Readiness == "ready" {
				id = v.ID
				break
			}
		}
	}
	return x.recheck(id)
}

func renderDoctor(out io.Writer, v doctorView) {
	fmt.Fprintf(out, "Forgecell doctor (schemaVersion %s)\nLab: %s\nActive Formula: %s\nGit ready: %t · GitHub CLI authenticated: %t\nSelection: %s %s — %s\nGitHub Issue workflow: %s — %s\n", doctorSchemaVersion, v.Lab, orNone(v.FormulaID), v.Git, v.GitHub, v.Selection.Status, v.Selection.ID, v.Selection.Reason, v.Workflow.State, v.Workflow.Reason)
	if v.Saved != nil {
		renderDoctorCandidate(out, "Saved binding", *v.Saved)
	}
	for _, c := range v.Candidates {
		if v.Saved == nil || c.ID != v.Saved.ID {
			renderDoctorCandidate(out, "Candidate", c)
		}
	}
	if len(v.Roles) > 0 {
		fmt.Fprintln(out, "\nRole readiness:")
		for _, r := range v.Roles {
			state := "ready"
			if !r.Ready {
				state = "not ready"
			}
			model := r.Model
			if model == "" {
				model = "unpinned"
			}
			fmt.Fprintf(out, "  %s: %s (%s binding) — %s; %s: %s (model: %s)\n", r.Role, r.Binding, r.Source, state, r.Capability.State, r.Capability.Reason, model)
			if r.NextCommand != "" {
				fmt.Fprintf(out, "    Next: %s\n", r.NextCommand)
			}
		}
	}
	if len(v.Extensions) > 0 {
		fmt.Fprintln(out, "\nExtensions (manifest and pinned files are verified; prepare and the server are never run by doctor):")
		for _, e := range v.Extensions {
			required := ""
			if e.Required {
				required = ", required"
			}
			fmt.Fprintf(out, "  %s (%s%s): %s\n", e.Name, e.Manifest, required, e.Status)
			if e.Detail != "" {
				fmt.Fprintf(out, "    %s\n", e.Detail)
			}
			if e.FixCommand != "" {
				fmt.Fprintf(out, "    Fix: %s\n", e.FixCommand)
			}
			for _, r := range e.Roles {
				fmt.Fprintf(out, "    %s role (%s binding): adapter support %s — %s\n", r.Role, r.Binding, r.Adapter.State, r.Adapter.Reason)
			}
		}
	}
	if len(v.ExtensionLocks) > 0 {
		fmt.Fprintln(out, "\nStale extension cache locks (doctor never removes a lock; run the fix once nothing is building):")
		for _, l := range v.ExtensionLocks {
			fmt.Fprintf(out, "  %s\n    %s\n    Fix: %s\n", l.Path, l.Reason, l.FixCommand)
		}
	}
	if len(v.Isolation.Adapters) > 0 {
		fmt.Fprintln(out, "\nIsolation from personal Claude/Codex/Cursor customizations (static; nothing is probed):")
		for _, a := range v.Isolation.Adapters {
			fmt.Fprintf(out, "  %s: %s — %s\n", a.Adapter, a.State, a.Reason)
		}
		for _, r := range v.Isolation.Roles {
			fmt.Fprintf(out, "  %s role (%s binding): %s; %s\n", r.Role, r.Binding, r.State, r.Model)
		}
	}
	if v.Meta != nil && v.Meta.Binding != "" {
		fmt.Fprintf(out, "\nMeta harness: %s — %s: %s\n", v.Meta.Binding, v.Meta.Capability.State, v.Meta.Capability.Reason)
		if v.Meta.NextAction != "" {
			fmt.Fprintf(out, "  Next: %s\n", v.Meta.NextAction)
		}
	}
	if len(v.LearnPending) > 0 || v.LearnError != nil {
		fmt.Fprintln(out, "\nPending learning (suggestions are saved only; approval stays yours):")
		for _, line := range v.LearnPending {
			fmt.Fprintf(out, "  %s\n", line)
		}
		if v.LearnError != nil {
			fmt.Fprintf(out, "  Pending learn requests unavailable: %s\n", flatten(v.LearnError.Error()))
		}
	}
	if len(v.RecoveryProposals) > 0 || v.RecoveryError != nil {
		fmt.Fprintln(out, "\nRecovery proposals (saved by the loop; it never runs them, and you run them only after every Lab process has stopped):")
		for _, p := range v.RecoveryProposals {
			fmt.Fprintf(out, "  %s#%d · Molecule %s · %s %s\n    %s\n", flatten(p.Repository), p.Issue, p.MoleculeID, flatten(p.Status), flatten(p.Phase), p.Command)
			for _, step := range p.Followup {
				fmt.Fprintf(out, "    then: %s\n", step.Command)
			}
		}
		if v.RecoveryError != nil {
			fmt.Fprintf(out, "  Recovery proposals unavailable: %s\n", flatten(v.RecoveryError.Error()))
		}
	}
	if v.Next != "" {
		fmt.Fprintf(out, "\nNext: %s\n", v.Next)
	}
	fmt.Fprintf(out, "\n%s\n", v.Note)
}

func renderDoctorCandidate(out io.Writer, title string, c doctorCandidate) {
	fmt.Fprintf(out, "\n%s: %s — %s\n", title, c.ID, c.Readiness)
	// Binding states are listed below; an unprobed provider leaves nothing else to say.
	if c.Binding == nil || len(c.Binding.States) == 0 || c.Executable != "" {
		fmt.Fprintf(out, "  %s\n", c.Detail)
	}
	if b := c.Binding; b != nil {
		fmt.Fprintf(out, "  Formula binding: %s · adapter: %s\n  Launcher (saved): %s — version %s (%s) · adapter protocol %s\n", b.FormulaBinding, b.Adapter, b.Launcher, b.LauncherVersion, b.LauncherVersionSource, orUnknown(b.LauncherProtocol))
		if b.ResolvedLauncher != "" {
			fmt.Fprintf(out, "  Launcher (resolved at run time): %s\n", b.ResolvedLauncher)
		}
		if b.RunningLauncher != "" {
			fmt.Fprintf(out, "  Launcher (running): %s — version %s · adapter protocol %s\n", b.RunningLauncher, b.RunningVersion, orUnknown(b.RunningProtocol))
		}
		provider := b.Provider
		if b.ResolvedProvider != "" && b.ResolvedProvider != b.Provider {
			provider += " (resolved " + b.ResolvedProvider + ")"
		}
		fmt.Fprintf(out, "  Provider (bound): %s\n", provider)
		for _, s := range b.States {
			fmt.Fprintf(out, "  State %s: %s\n", s.State, s.Detail)
		}
	}
	for _, s := range c.Steps {
		var evidence []string
		for _, e := range []struct{ name, value string }{{"result", s.Result}, {"expected", s.Expected}, {"state", s.State}, {"argv", strings.Join(s.Argv, " ")}} {
			if e.value != "" {
				evidence = append(evidence, e.name+" "+e.value)
			}
		}
		line := fmt.Sprintf("  Step %s: %s — %s", s.Step, s.Status, s.Reason)
		if len(evidence) > 0 {
			line += " (" + strings.Join(evidence, "; ") + ")"
		}
		fmt.Fprintln(out, line)
	}
	if c.NextCommand != "" {
		fmt.Fprintf(out, "  Next: %s\n", c.NextCommand)
	}
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

func renderRoleCapabilities(out io.Writer, roles []onboarding.RoleCapability, source string) {
	for _, r := range roles {
		fmt.Fprintf(out, "Role capability %s (%s): %s adapter support %s — %s [%s]\n", r.Role, r.Source, r.Binding, r.Capability.State, r.Capability.Reason, source)
	}
}

// proposalBeforeYAML is the approved Formula a saved proposal changes, from the
// meta evidence or, for a model pin, the binding evidence; "" when none is saved.
func proposalBeforeYAML(p onboarding.Proposal) string {
	if before := proposalMeta(p).BeforeYAML; before != "" {
		return before
	}
	if p.CapabilityEvidence != nil {
		return p.CapabilityEvidence.BeforeYAML
	}
	return ""
}

func proposalMeta(p onboarding.Proposal) onboarding.MetaReport {
	if p.CapabilityEvidence != nil && p.CapabilityEvidence.Meta != nil {
		return *p.CapabilityEvidence.Meta
	}
	f, _ := formula.Parse([]byte(p.YAML))
	return onboarding.DescribeFormulaMeta(f)
}
func renderMeta(out io.Writer, r onboarding.MetaReport, source string) {
	argv, _ := json.Marshal(r.Command)
	fmt.Fprintf(out, "Meta harness (%s): %s — %s\nCommand argv: %s\nTimeout: %d ms\nInstalled CLI: %s — %s\nAuthentication: %s — %s\nModel access: %s — %s\nMeta containment: %s — %s\nPermission limits: %s\nEvidence inputs: %s\nLearning consent: %s\n", source, r.Capability.State, r.Capability.Reason, argv, r.TimeoutMS, r.Candidate.Installed.State, r.Candidate.Installed.Reason, r.Candidate.Authentication.State, r.Candidate.Authentication.Reason, r.Candidate.Capabilities.ModelAccess.State, r.Candidate.Capabilities.ModelAccess.Reason, r.Containment.State, r.Containment.Reason, r.Restrictions, r.EvidenceInputs, r.NextAction)
}
