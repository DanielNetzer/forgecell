# Learning in the Lab

Forgecell improves the process that creates software. The coding harness changes the
codebase for a ticket; the meta harness uses Molecule ledgers to propose a better Formula
for future tickets. These are different jobs, with the same four primitives.

## An iterative research loop

1. **Observe.** Run a Molecule and preserve its issue, exact Formula snapshot, Atom outcomes,
   commands and limitations in the ledger. An error or blocked run can be useful evidence.
2. **Reason.** Identify a recurring workflow problem or a narrowly supported lesson. Separate
   observations from the harness's interpretation. A successful exit is not proof of quality.
3. **Propose.** Explain one small process change, why the ledger supports trying it, the
   expected benefit, and what would count against it. Attach the complete Formula diff.
4. **Review.** A human approves or dismisses the proposal. Approval permits a recipe change;
   it does not validate the hypothesis. Keep the decision and both recipe versions.
5. **Evaluate.** Run another suitable ticket using the approved recipe. Compare the intended
   behavior and actual outcome, including regressions, skipped checks and environmental differences.
6. **Iterate.** Retain a useful lesson, revise an uncertain one, or propose reversing a harmful
   change. Each further Formula mutation needs a human decision. Insufficient evidence is a
   valid finding; the Lab should not manufacture a change to appear productive.

“Better” depends on the ticket: correctness, useful validation, time, cost, or fewer failed
attempts can conflict. State the desired outcome and tradeoffs. Do not claim an optimum or
causality from a single run. Comparing the same ticket twice may confirm that instructions
were followed without showing that they generalize.

## What a person reviews

Every new proposal records:

- **Process change:** what the next Molecule will do differently, in plain language.
- **Evidence and reasoning:** ledger IDs, observed behavior, uncertainty and scope.
- **Expected impact:** a hypothesis, explicitly unverified.
- **Evaluation:** what to inspect on the next comparable run and signs of regression.
- **Exact change:** original and proposed YAML, hashes, and diff.
- **Human decision:** pending, approved or dismissed, with a review timestamp when decided.

The CLI displays the explanation before the YAML. Earlier suggestions remain readable;
missing evaluation fields are shown as missing, not retroactively invented.

A useful example: “When sandbox IPC restrictions block the usual test runner, try the
verified direct Node invocation and report both results separately.” This changes validation
behavior. “Fix Unicode decoding in invoke.ts” belongs to the ticket, not to a Formula lesson.
Renaming the Formula or adding a comment is not a process improvement.

## Current implementation and limits

`forgecell learn <molecule-id...>` reads finished ticket ledgers from the same approved
Formula snapshot and invokes an explicitly bound learn Atom. The meta harness returns
`summary`, `rationale`, `expectedImpact`, `evaluation`, and complete `yaml` as JSON.
Forgecell saves a pending proposal. `forgecell suggestion <id> --approve` applies it only
if the current Formula still matches its original snapshot; `--dismiss` preserves the recipe.

The runtime rejects proposals that only change metadata or unused fields. This is a
structural check, not proof that prose describes a useful process improvement. A person
must still reject ticket-specific code instructions, unsupported claims and unsafe changes.
The example adapter instructs the meta harness to preserve command bindings and human gates;
this is a prompt instruction, not an enforced restriction on the proposed diff. arbitrary BYO adapters
remain responsible for their own sandbox and tool permissions.

Next-run evaluation is currently a **manual review of ledgers**. Forgecell does not yet
calculate comparative results, automatically reverse a mutation, or post findings to GitHub.

## System of record

The development CLI keeps each checkout's Lab under `~/.forgecell/labs/`. Its local
files are the execution record: approved Formulas, Molecule ledgers, and suggestions
with human decisions. `doctor --json` reports the exact Lab directory. Keep it backed up
deliberately; outputs may contain repository information. A ticket records the requested
work. Curated Markdown findings in `docs/dogfood/` document Forgecell's own research
without publishing raw private output.
GitHub issue summaries can be added deliberately; the Lab currently posts no comments.

Start with the [first approved lesson and follow-up evidence](dogfood/formula-suggestion.md).

## Go CLI review flow

The development Go CLI supports the same saved suggestion format as the TypeScript reference:

```sh
forgecell learn mol-123
forgecell suggestion suggestion-ID
forgecell suggestion suggestion-ID --approve
# Or dismiss without changing the recipe:
forgecell suggestion suggestion-ID --dismiss
```

Add `--json` for an agent-readable result. `learn` never accepts an approval flag:
it saves a pending proposal with the process change, ledger evidence, reasoning,
expected impact and a comparable-run evaluation plan. Approval is a separate human
decision and does not establish that the change improved anything.

The Go onboarding proposal does not silently configure a meta harness. An explicit
`learn` Atom command is required. Current-source functionality offers proposal-only
opt-in; these flags are not included in published v0.2.0-preview.2:

```sh
forgecell init --meta-harness codex --json
# Or a BYO argv array; capability and containment remain unknown:
forgecell init --meta-command '["/absolute/meta-command","arg"]' --json
forgecell init --lab LAB_DIR --review PROPOSAL_ID
# After human review of exact YAML and binding limits:
forgecell init --lab LAB_DIR --approve PROPOSAL_ID
# Separate invocation; activation does not run learning:
forgecell learn MOLECULE_ID --lab LAB_DIR
```

Native choices are `codex`, `claude-code` and `cursor`. Native opt-in requires observed
installation/authentication readiness and supported meta capability. Unsupported or
unknown support saves no proposal and supplies BYO configuration guidance. BYO commands
are never probed. Default init binds coding only. Opt-in proposes exactly one trailing
learn Atom with a separate command and bounded 900000 ms timeout. Saved review displays
argv, permission limits, evidence inputs and exact before/after YAML.
Re-init preserves pending proposals, interrupted approval recovery, approved coding and
learning bindings, learned instructions and custom Atom order. Opt-in does not migrate
an existing learn binding; coding rebinding preserves it.

Eligible inputs are finished ticket Molecule ledgers from the same intact, currently
approved Formula snapshot, including Atom outcomes, verification evidence and limitations.
Pending attempts, mixed snapshots, damaged evidence and uncertain publication are rejected
before invoking the meta command. Learning saves only a pending suggestion; Formula
mutation needs a separate human approval of that suggestion.

Native adapters request provider controls: Codex read-only sandbox with approval policy
never, Claude built-in tools disabled and strict empty MCP configuration, Cursor sandbox
enabled with ask mode. Installed enforcement remains unverified. Codex meta lacks ticket
analysis's additional tool/MCP/plugin isolation; Claude lacks analysis safe-mode controls;
Cursor MCP/plugin isolation is unverified, so ask mode is not verified read-only containment.
Meta retains the supplied working directory rather than analysis's temporary directory.
No-edit, supplied-evidence-only and binding/gate preservation prompts are declarations,
not enforced diff limits. Arbitrary custom commands retain the permissions of the local
process that invokes them. A prompt alone is not a sandbox.

Review recomputes the diff from the exact hashed YAML, rather than trusting stored
diff text. A shared Formula-write lock prevents competing approvals. Approval refuses
stale recipe bytes and persists an intent before replacement so an interrupted
approval can finish its record. Dismissal does not replace the Formula. Learning
rejects mixed snapshots, damaged evidence, and cosmetic changes to unused fields.

### Recurring failures queue a learn request

Nothing used to start learning, so the same failure could repeat until a person noticed.
After `run` and `loop`, Forgecell evaluates cheap, deterministic rules over the Lab's
recent finished ledgers. A rule fires when the newest finished Molecules of the current
approved Formula snapshot exhibit the same failure class the threshold number of times
in a row. Each class is a pure function of the finished ledger; no failure-class field
is stored:

- `process-timed-out`: the Molecule's final coding harness attempt hit its timeout and
  verification did not pass afterwards. A timeout that a later attempt or a passing
  verification superseded does not count, and neither does a timeout that only an
  analysis or review attempt mentions. Ledgers from before attempt results were
  recorded are simply not counted.
- `analysis-policy-violation`: intake stopped because the unchanged policy validator
  rejected the analysis, including the per-class corrective retry stops.
- `protected-regression-failed`: verification ran the protected view and its required
  checks did not pass.

Intake and recovery also guard against two wasted runs; neither is a failure class
above. An analysis whose candidate plan fails `Plan.Validate` (for example a check with
category `vet`) is rejected inside the analysis loop with the policy-violation class
`plan-validation` and gets one corrective retry within the same three-retry budget; a
second invalid plan stops intake. Supplied check inputs, checks, setup and the Formula
baseline are validated before any analysis, so their errors stop intake without a
retry. An amended coding attempt that completes with the tree and artifacts of the
parent revision's paused state stops at the review gate with outcome reason
`noChangeAfterAmendment`: verification is not run, delivery and learning refuse it, and
the next step is a further amendment with new work or a checks-only amendment that
verifies the retained tree.

The streak is the newest run of finished Molecules of the current Formula snapshot,
ordered by finish time and then Molecule id, among the 128 most recently modified
ledgers. Every Molecule in it exhibits the class and is one that `learn` itself would
accept. What each kind of Molecule does to a streak:

- Counts: a finished Molecule that exhibits the class. The newest `threshold` of them
  become the request's evidence.
- Ends it: a finished Molecule of the same snapshot that does not exhibit the class
  (a success, a different class, a plain failure), or that `learn` would refuse (not an
  approved ticket Molecule, a damaged Formula snapshot, evidence it cannot classify).
- Invisible, neither counts nor ends it: unfinished Molecules and Molecules waiting at a
  gate, Molecules of another Formula or snapshot, non-ticket Molecules, and ledgers that
  cannot be read. An unreadable ledger is skipped and reported in one diagnostic line
  instead of stopping the evaluation, so a streak can span a Molecule the Lab could not
  read.

The threshold is an optional Formula setting and defaults to 3:

```yaml
learning:
  recurrenceThreshold: 3   # integer from 1 to 50
```

Formulas without it keep their exact bytes and SHA-256. The threshold in force is the one
in the current approved Formula.

When a rule fires and a learn Atom is bound, Forgecell records one durable, deduplicated
**pending learn request** under `learn-requests/` in the Lab, holding the class, the
triggering Molecule ids and the Formula SHA-256. When no learn Atom is bound, it only
prints one line that names the class and the Molecules and recommends
`forgecell init --meta-harness`. **No model is called automatically.** Evaluation, `run`,
`loop` and `doctor` never start the learn command.

`doctor` (also in `--json` as `learnRequests`), the end of `run` and the `loop` report show
pending requests and the suggestions saved from them, so they are not missed. Exit codes are
unchanged.

```sh
forgecell learn --pending
```

runs each pending request through the normal `learn` path over exactly that request's
Molecules. A success saves a pending suggestion and records its id on the request; a failed
or timed-out run is reported with a bounded, redacted message, the request stays pending and
the exit status is nonzero. `learn --pending` cannot be combined with Molecule ids and never
approves or dismisses anything. Approval, stale-Formula protection and every other
`learn` guard are unchanged. Running it automatically from the loop is left to a later slice.

While the Formula SHA-256 is unchanged, a class never fires again while a request or a
suggestion from it is pending. After a suggestion is dismissed, or a learn run fails and
saves nothing, the class fires again only after the threshold number of **new** occurrences
since that event; a failed request is superseded by the fresh one, and the old request is
retired first so a crash never leaves two pending requests for one class. A Formula change
resets the counters and retires requests tied to the old Formula.

`learn --pending` writes a durable intent (with a random nonce) on the request before it
starts the learn command. If the process dies before the outcome is recorded, the next
`learn --pending` finds the intent, adopts a suggestion that run already saved for exactly
the same Molecules and Formula (no second model call), and only calls the learn command
again when there is none.

A request, suggestion or ledger that cannot be read is skipped and reported once; it never
stops the other failure classes from being evaluated. A class whose own suggestion cannot
be read does not fire until that is fixed. `learn-requests/` stays bounded: the oldest
terminal requests (stale, superseded, or replaced by a newer one) beyond 20 and run locks
of finished requests are removed. Failure text from a learn run and any other untrusted
text is shown on one line with terminal escapes, control, bidirectional and other format
characters removed, and is cut by characters, not bytes.

### Exact activation and suggestion freshness

Learning requires the current Lab's completed exact-byte Formula approval before
the meta harness runs. Each suggestion retains that originating approval identity
and both exact YAML hashes. Reactivating identical bytes under a different human
decision invalidates earlier suggestions; editing YAML or changing approval during
generation prevents a suggestion from being saved.

Approval persists its intent, updates the exact recipe and current Lab approval,
and completes the saved decision durably under the shared Formula write lock.
Interrupted approvals block execution and can be resumed with the same reviewed
suggestion after inspecting stopped writers and conflicting state. Dismissal
cannot undo an applying approval. A newer activation cannot be overwritten by
retrying an older decision.

Historical suggestions remain inspection evidence. Comparing their variants with
`evaluate` requires a fresh `--approve DIGEST` covering the frozen plan, both exact
recipes, source and output destination. The preview performs no setup, checks,
model calls or output-directory writes. The report retains the new decision's
provenance. Rechecking retained output does not approve new model execution.
Formula approval remains separate from ticket scope and publication review.

### Cohort evaluation

The single-task `evaluate` above compares one baseline and one candidate recipe on one
frozen task. `evaluate --cohort` extends the same discipline to a reviewed **cohort**:
two Formula variants (for example identical apart from `model`) run over N frozen task
plans with K repetitions each.

```sh
forgecell evaluate --cohort COHORT.json --source REPO --out DIR            # prints the digest
forgecell evaluate --cohort COHORT.json --source REPO --out DIR --approve DIGEST
forgecell evaluate --cohort COHORT.json --source REPO --out DIR --approve DIGEST --keep-workspaces
```

The cohort file is strict JSON, at most 64 KiB, with unknown fields rejected:
`schemaVersion: 1`; a `hypothesis` (≤ 2000 characters); a `primaryOutcome` of
`hiddenAcceptancePassRate`, `requiredChecksPassRate` or `costPerAcceptedOutcome`;
exactly two `variants` `[{name, formula, formulaSha256}]` whose `name` matches
`^[a-z][a-z0-9-]{0,31}$`, whose `formula` is a cohort-relative Formula file, and whose
pinned bytes differ (a cohort that pins the same Formula for both variants is refused);
1–50 `tasks` `[{name, plan, planSha256}]`, each `plan` an existing evaluation `Plan` file;
`repetitions` 1–10; and an optional `stop` `{maxAttempts?, maxCostUSD?}`. Every
referenced file is pinned by SHA-256. On both fresh and resumed runs every input copied
under `<out>/inputs/` is re-hashed against its pinned hash immediately before each
attempt and the attempt executes from those verified bytes; a mismatch refuses the run
and writes `status: aborted` rather than running a mutated copy.

One activation digest covers the cohort bytes, both Formula files and every plan file,
plus source and destination, reusing the existing exact-approval semantics. Nothing is
created or executed before approval; without `--approve` the command prints the digest
and the exact approve command.

For each task, for each repetition, variant A then variant B run interleaved (A first),
sequentially, each in its own isolated per-attempt Lab under
`<out>/attempts/<task>/<variant>/rep-<n>/`. A failed or timed-out attempt is recorded,
never silently retried, and the run continues. An attempt cancelled by SIGINT or SIGTERM
is discarded rather than saved, so resume re-runs that coordinate instead of skipping it,
and the run is recorded as `status: aborted`. The stop rule is checked after each
attempt; `maxCostUSD` counts only reported cost (list-price estimates included and
labelled), never triggers when no cost is reported, and its stop reason names the cost
basis. A stop that leaves the two variants with unequal attempt counts records an
imbalance note, because rates over different n are not directly comparable.

`--parallel N` (1–4, default 1) runs up to N attempts at a time; the default runs them one
at a time exactly as above. Coordinates still start in the same order, so A and B of a
task and repetition tend to run together under the same machine load, and recorded
coordinates are still skipped on resume. The report keeps attempts in coordinate order,
so `report.json` and `report.md` do not depend on completion order. `--parallel` is not
part of the activation digest, so changing it never invalidates an approval or a resume.
When a stop rule fires, no new attempt starts and the in-flight ones finish and are
recorded; `maxAttempts` counts recorded and in-flight attempts before every coordinate,
so it is never exceeded, even on a resume with gaps. An abort error, a report-save error or
SIGINT/SIGTERM cancels the in-flight attempts, which are discarded unsaved (also when the
cancellation lands during the diff capture) so resume re-runs them. Each `attempt.json`
run with N above 1 records `parallel: N`, and whenever any recorded attempt ran that way,
including after a resume, the report notes that elapsed and harness durations include
contention from concurrent attempts.

By default each attempt's Lab, workspace and verification trees are deleted once its
record and verification observations are saved, to bound disk growth. Before deletion the
run writes `<attempt>/changes.diff` (the unified diff from the base commit to the captured
tree, capped at 5 MiB with truncation marked, its sha256 recorded in the attempt record)
and `<attempt>/changes.json` (the changed paths). The diff is captured with a fixed,
configuration-independent git invocation so it depends only on the pinned bytes; a capture
that fails is recorded as the attempt's `changesError` and never aborts the run. Every
worktree an attempt allocates is registered only in the attempt's own throwaway source (see
Replay tasks below; `--source` is only read and never gains a worktree), so deletion simply
removes the attempt's Lab, verification and source trees together; a cleanup failure
is recorded as the attempt's `cleanupError`. Both error kinds are summarized in the report
rather than swallowed. Report and any later recheck read the retained files, never the
deleted tree. `--keep-workspaces` retains the trees.

The side-by-side report is written to `<out>/report.json` and `<out>/report.md`. For
each variant it reports attempts and completions; hidden-acceptance pass count and rate;
the required-checks pass rate; scope violations, scope-change and amendment counts, and
timeouts; median and p90 of elapsed and harness time; reported cost total and per
accepted outcome with its basis and `N of M` coverage; effective models; and review-Atom
findings by severity when present. The required-checks pass is tri-state: it is reported
only when the plan actually ran a non-acceptance check, and is `null` ("n/a") when the
plan has none. `RequiredChecksPass` and its rate are nullable in `report.json`; aggregates
and per-task cells exclude n/a attempts and state their own measurable n. When the
primary outcome is `requiredChecksPassRate` and every attempt is n/a, the report states
the primary outcome is not measurable. Cost per accepted outcome divides the reported-cost
total only by the accepted attempts among the cost-reporting ones, and always prints its
`N of M` coverage. A per-task × variant table lists the outcomes. **Counts and rates only,
with no significance claims**, and every rate states its n (tasks and repetitions).

An attempt whose harness was served a model other than its pin (`usage.modelMismatch`)
is recorded with `invalid: "model-mismatch"` and is never correct. Invalid attempts stay in
the variant's attempt count, reported cost and cost coverage, because the money was spent,
but are excluded from completions, passes, failures, timeouts, scope and amendment counts,
required checks, every median and p90 and the cost per accepted outcome; each variant
reports `invalid` and `invalidReasons`, and the report adds one note naming how many were
excluded. When a variant's first attempt is invalid this way the run stops (`variant
<name> was not served its pinned model (<pinned> → <served>)`), because the comparison
cannot be made; a mismatch on a later attempt is recorded and the run continues. Each
`attempt.json` records `costUsd` from the reported usage (`null` when nothing reported a
cost), and, before the Lab is deleted, `failedCheckOutput`: for every failed Molecule
setup command (`phase: setup`) and check (`phase: check`), in the candidate view and its
protected run, the ID, exit code and the last 8 KiB of stdout and stderr, copied from the
verification history (already scrubbed of environment secret values) and passed through
the pattern-based credential redaction. The tails stay out of `report.json`, which a
resume must read. `report.md` has an `## Attempts` section: one line per task, variant and
repetition giving the outcome (`accepted`, `failed`, `incomplete` or `invalid:<reason>`),
the served models, the cost and the elapsed time, followed by the last 20 stdout lines of
each failed command (a leading `…` marks a first line cut from a longer stream).

Hidden acceptance runs first. The evaluation lists the plan's `acceptance` check before
every other plan check in the readiness analysis (setup still runs before all checks, and
Formula baseline checks follow), so Molecule verification, which is unchanged — still
fail-fast and still all-required — always observes the hidden test before a regression
check can stop it, for example on a timing-sensitive test in the replayed historical
code. Each attempt records `hiddenAcceptance`: `true` or `false` from the evaluation's
independent verification, the same run that sets `correct` and runs every check (so
`correct` always implies `hiddenAcceptance: true`), or from the Molecule's candidate
verification when independent verification never ran; `null` when acceptance never ran
or did not complete (coding incomplete, setup failed, verification errored first, or the
check timed out, was interrupted or hit the output cap), and for an invalid attempt.
`hiddenAcceptancePass` counts attempts with `hiddenAcceptance: true`, independently of
every other check, and `hiddenAcceptanceN` counts the attempts where it ran; `report.md`
shows `hidden-acceptance pass X of N-ran` beside `correct`, which still requires
acceptance and every required check to pass. A resumed record written before the field
existed lacks the key; it is excluded from the hidden-acceptance rate and the report
notes it. When acceptance passed but a regression check failed, the attempt records
`regressionFailedTests`, the sorted, unique Go test names the failed checks' full
streams report (read before tailing: `go test -json` `fail` events with a test when the
stream carries them, otherwise `--- FAIL:` lines), and the `## Attempts` section lists
them, so flakes in historical code are visible rather than read as the model's failure.
When a failed check's stored stream was itself cut, `regressionFailedTestsTruncated` is
set and the list is marked `(output truncated)`.

Regression failures that are **flaky at base** are classified, so a test that fails in the
untouched historical code does not decide the comparison. The failed-test extraction also
records each test's package import path (the `Package` of a `go test -json` event, or the
`FAIL\t<pkg>` result line that follows a plain stream's `--- FAIL:` lines), and notes an
**unattributed** failure: a package failure no `--- FAIL` in that package explains, or any
`panic:` line, Go runtime abort (`fatal error:`, such as a deadlock) or `signal: ` line (a
test binary killed by a signal), `test timed out`, `[build failed]` or `[setup failed]` (in
plain output or an event's `Output`), even beside a named failed test in the same package,
and a plain `exit status N` line with no `--- FAIL` before it in its package. After independent
verification, and before cleanup reclaims the per-attempt source, an attempt whose hidden
acceptance passed re-runs each failed regression test in a fresh worktree of that source at
the base commit (base only; no candidate change applied). The plan's `setup` commands run
there first, as in verification (each from its directory with its own timeout), under the
replay's scrubbed environment; a setup failure leaves every test unclassified. Each test then
runs **exactly 3 times** (no early stop): `go test -count=1 -run '^<name>$' <pkg>` (with
`-json` to read the result; each subtest level is anchored separately) from the check's
directory, under the same scrubbed environment, with a 5-minute timeout each. A run fails
only on the test's own `fail` event or a timeout after it started and before it passed; a
non-zero exit after its `pass` event, a missing test or package, a build failure or an
interruption is undecided. A test with at least one passing **and** one failing run is
`flakyAtBase`; one that failed all 3 is `brokenAtBase` (it may be the very bug the PR
fixes, so it excuses nothing); one that passed all 3 is neither; anything else is
`unclassified`. At most 10 tests are re-run per attempt, in package and test order; the
rest, and a test whose package is unknown, are `unclassified`. The attempt records
`regressionFailures` (`package`, `test`, `flakyAtBase`, `brokenAtBase`, `baseFails`,
`basePasses`, `unclassified`), keeping `regressionFailedTests`, and
`correctExcludingBaseFlakes`: `correct`, or, judged **only from the evaluation's independent
verification** (which runs every plan check without stopping at a failure), never from the
Molecule's fail-fast verification: that verification ran with every plan check observed and
complete, acceptance passed, every check that is not a `go test` check (go vet, gofmt and any
other) passed, and every failed `go test` check failed only on named tests, none
unattributed, each tagged `flakyAtBase`. `correctExcludingBaseFlakes` is always computed
from that independent verification. It runs whenever coding completed with a captured tree,
even when the Molecule's own fail-fast verification failed or errored on a check: the
attempt's `verification` keeps the refusal (so `correct` and the required checks keep their
strict, Molecule-gated meaning), and the full independent run is recorded as
`evaluationVerification`, which only `hiddenAcceptance`, the regression failure lists and
`correctExcludingBaseFlakes` read. It does not run when coding failed, was blocked, timed out
or asked for a scope change, or the attempt is invalid. When the independent verification
did not run or a check is unobserved, the flag is false: an unobserved check never counts as
passing. The Molecule's own failure evidence must agree as well: every failed check in
`failedCheckOutput` (candidate and protected views) must be a `go test` check that failed only
on named tests tagged `flakyAtBase` (not `brokenAtBase` or unclassified), with no
unattributed failure and a complete failure list, so a test the candidate broke
deterministically in the Molecule's run keeps the flag false even when the independent run
did not fail it; any other Molecule failure (a setup command, acceptance, go vet, gofmt or
another check) does too. Each variant reports a `correctExcludingBaseFlakes` count (records that predate the
field count only when correct), and the `## Attempts` section marks each test
`[flaky at base (k/3 failed)]` or `[broken at base]` and lists the unclassified ones. The
re-runs add up to 10 × 3 × 5 minutes per attempt, the base worktree is removed even after a
cancellation, and a cancellation during the re-runs still discards the attempt.

The protected regression of an evaluation attempt exempts the task's own test files
(the plan's `candidateTestFiles`, which replay sets to the replayed PR's changed test files
and which hidden acceptance grades with their real versions): the evaluation sets them,
sorted, as the readiness plan's `protectedExemptPaths`, so
the protected view keeps the candidate's copies instead of restoring the base versions, and
a correct solution to a PR that changed an existing test is not failed by that old test.
Independent verification likewise applies the candidate's versions of those files, so its
regression checks see the same test sources as the protected run: the PR's own test files
are graded by hidden acceptance and run in their candidate versions everywhere else.
Every other existing test, and every other original check file, is still restored to base;
`originalCheckFiles` alone only means "keep the base version", so a hand-authored plan
without `candidateTestFiles` exempts nothing and files a recheck adds always keep base.
A plan with exemptions is never approved automatically (`plan-has-protected-exemptions`),
an amendment cannot add or change them, and normal ticket runs never set them.

An existing `<out>` whose recorded activation digest matches resumes and skips attempts
already recorded (a directory with a complete attempt record counts as done); a
mismatched digest is refused so a different cohort can never overwrite retained
evidence. Cohort `--recheck` — re-grading retained outputs without rerunning a model — is
**deferred** in this slice; the retained per-attempt evidence under `<out>/attempts` (the
record, verification observations and the captured `changes.diff`/`changes.json`) is
untouched and can be regraded by a follow-up, which reads the captured diff rather than
the deleted workspace tree. The single-task `--inputs`/`--recheck` mode is unchanged.

### Replay tasks

A cohort is only as useful as its tasks. `evaluate replay` builds frozen evaluation task
plans from our **own merged PRs**, so a model comparison runs against real history with
real, hidden acceptance tests:

```sh
forgecell evaluate replay --repo OWNER/REPO --pr 120 --pr 124 --source CHECKOUT --out NEW_DIR
```

For each PR it reads, read-only via `gh api` (the same pattern as
`internal/intake/open_issues.go`), the PR's `merge_commit_sha`, `base.sha`/`base.ref`,
`body`, changed files (`/pulls/N/files`, one page of at most 100) and the linked issue —
the first `Closes #N`/`Fixes #N` in the PR body, else the first `#N` in the title. The
issue body is read from `/issues/N` as the current body (`taskSource: current-issue-body`;
no body history in v1) and is treated as **untrusted text**.

`baseCommit` is computed from the merge commit's parents with local `git` in `--source`: a
single-parent (squash/rebase) merge uses `merge^1`; a two-parent merge uses the
merge-base of its two parents. The rule applied is recorded in `approval.md`.

To keep hidden acceptance material out of anything a reviewer might hand to a harness, each
eligible PR writes to **two** sibling directories. The human-facing `<out>/tasks/pr-<N>/`
holds only:

- `task.md`, the issue title and body as untrusted text for human review.
- `approval.md`, a human-readable summary pinning every hidden artifact by SHA-256 and its
  `hidden/pr-<N>/…` path.

The machine-consumed inputs, which embed or are the hidden tests, live under
`<out>/hidden/pr-<N>/` and are never shown to the harness during a run:

- `plan.json`, an evaluation `Plan` with `allowedFiles` the PR's changed files (plus a
  renamed file's previous path), `originalCheckFiles` the changed `_test.go` files,
  `requireInitialFailure: true`, and `checks` of `go test ./...`, `go vet ./...` and gofmt
  from `lab` plus an `acceptance` check.
- `issue.json`, the linked issue as the plan's task (`Plan.Task`, consumed by the `--cohort`
  runner).
- `acceptance.sh` (`Plan.Acceptance`), a self-contained script that copies the workspace to
  a throwaway tree, writes the changed test files (captured at the merge commit) into it and
  runs, in their packages, **only the tests they define**, so grading never rewrites tracked
  content and an unrelated (for example timing-sensitive) test in the same package can never
  decide acceptance. The command is `go test -buildvcs=false -run '^(<names>)$' <pkgs>`,
  where the names are the top-level `Test`/`Fuzz`/`Example` functions defined in the hidden
  test files, sorted and each quoted with `regexp.QuoteMeta`. If that pattern would exceed
  4,000 bytes the PR is skipped with `hidden test name pattern too long`.
- `files/<repo-relative>`, the raw hidden test files for review.

> **Assumption (recorded):** the frozen `Plan` schema and the #150 cohort runner require
> `Plan.Task`/`Plan.Acceptance` to be *clean* plan-relative paths (no `..`) read from the
> plan's own directory. Keeping hidden content out of `tasks/` while staying directly
> consumable by the cohort runner therefore means `plan.json`, `issue.json` and
> `acceptance.sh` are co-located under `hidden/pr-<N>/` (the cohort stub's task `plan` path
> points there), rather than placing `plan.json` in `tasks/` with a `..`-relative reference.

Replay executes repository test code at build time. It does so only under a scrubbed
environment allowlist (`PATH`, a private `HOME`, `TMPDIR`, `GOCACHE`, `GOMODCACHE`,
`GOFLAGS=-mod=readonly`, `GOTOOLCHAIN=local`, `GOPROXY=off`, `LANG`) — never the inherited
environment — and in throwaway worktrees created with `core.hooksPath=/dev/null`. In a
worktree at `baseCommit` the hidden tests are applied (renamed-away old paths removed) and
run with `go test -json` and the same `-run` filter as `acceptance.sh`, so the initial
failure is reproduced by the task's own tests and the merge-pass validation below is not
failed by an unrelated test; the PR counts as reproducing the initial failure when at least one
hidden test reports a `fail` action (a **test-failure**) **or** when the failure is a
**test-compile**: the test package does not compile at base because the hidden tests call
identifiers the PR adds. To distinguish a test-compile from a real build error, when the base
run reports a build failure the builder runs `go build -buildvcs=false <pkgs>` in the same
worktree (before it is removed) under the same scrubbed env — `go build` ignores `_test.go`,
so a success, or a package with `no non-test Go files` at base (one the PR creates), means the
non-test code builds and only the tests fail to compile. Any other non-test build failure
remains the `build failed at base: …` skip. A missing toolchain or an incomplete run is a
distinct recorded skip, never a reproduced failure. For validity, the hidden tests are also
run in a worktree at the merge commit and **must pass** there.

The non-test build is run once per hidden-test package: the failure is only a test-compile when
every package either builds or reports `no non-test Go files`; any other per-package failure is a
real build error. A base build failure counts as test-compile only when the collected compiler
output actually contains compile-error lines from which at least one identifier is extracted — a
`TestMain`/`init` panic or `os.Exit` before any test runs stays a `build failed at base` skip.

A test-compile task gains an **interface contract**. The builder collects every compile error
(re-running `go test -json -buildvcs=false -gcflags=-e <pkgs>` — `-e` on the named packages only,
so the compiler does not stop at ten), parses `path:line:col: message` lines, and extracts the
identifiers the hidden tests need (`undefined: Name`, `undefined: pkg.Name`, `x.Name undefined
(type T …)` → `T.Name`, too-many/not-enough-arguments and cannot-use → the call target with any
instantiation suffix stripped, and `unknown field Name … type T` → `T.Name`). If that collection
run does not complete (timeout, interruption or output overflow) the PR is skipped with `setup
error: compile-error collection did not complete`. It then resolves those declarations at the
merge commit with `go/parser`+`go/ast`+`go/printer` over the merge tree's **non-test** `.go`
files — funcs and methods with the body dropped, the full type spec (including type parameters)
for a struct or interface, the field line for `T.Name` (promoted embedded fields included), every
method of that name for a `v.M` call on a local variable, and const/var specs (the value dropped
whenever it contains a function literal or is not a plain literal/identifier/selector/composite
expression). Package qualifiers resolve through the test files' imports by the imported package's
declared name (and dot imports), so a compiler-printed `pkg.T` resolves even when the test aliased
that import. Predeclared names (`string`, `int`, `error`, the builtins, …) and identifiers
qualified by an import **outside** the lab module (e.g. `io.Reader`) are excluded — they name
existing declarations, not identifiers the PR adds — so they never enter the contract nor the
cross-package method fallback. Unresolved names render as `Name // declaration not found`. If,
after rendering, no extracted identifier resolves to a declaration, the PR is skipped with `build
failed at base: no interface identifiers recognized: <build detail>`. The result, sorted by
package path then name and capped at 400 lines (a larger contract skips the PR with `interface
contract too large`), is appended to both `issue.json` (`Body`) and `task.md` under the heading
`## Interface contract (from the merged change)`, one fenced go block per package (each fenced
with a backtick run longer than any inside the block). `taskSource` then becomes
`current-issue-body+interface-contract`. `approval.md` records the initial-failure kind, the
extracted identifiers and any unrecognized "Other compile errors at base" (the latter never
leak into the task).

A PR is skipped, with the reason recorded in `<out>/skipped.json`, when it has no `_test.go`
changes, no linked issue, more than 25 changed files, more than 2,000 changed lines, only
non-Go changes, hidden tests outside the `lab` module (v1 assumption), a symlinked test
file, an initial failure that does not reproduce, a non-test build error at base, an interface
contract larger than 400 lines, hidden tests that do not pass at the merge commit, or any
per-PR infrastructure failure (`gh`/`git` error, unsafe path, linked reference that is a pull
request). Per-PR failures never abort the run;
`skipped.json` and `cohort.stub.json` are always written.

To stop the comparison harness from reading the answer out of git history, the builder also
writes a **non-bare, history-free** checkout at `<out>/source` containing only the tasks'
base commits (each fetched shallow, with `--update-shallow`, under `refs/replay/pr-<N>`), so
no merge commit, PR branch or later history is present. It has a detached `HEAD` at the first
built task's base commit and a clean work tree, and `origin` set to
`https://github.com/<repo>.git` with `remote.origin.fetch` set to
`+refs/forgecell-none/*:refs/forgecell-none/*`, a refspec that matches nothing on GitHub (an
unset refspec would still fetch the remote `HEAD`). That refspec only prevents an accidental
bare `git fetch origin`; network isolation of the coding harness is the real control. Every
fetch uses `--no-write-fetch-head`, so no `FETCH_HEAD` records the full-history checkout's
path. A **non-bare** checkout is required because the molecule
runs `git rev-parse --show-toplevel` and `git remote get-url origin` on `--source`, which a
bare repository fails; `evaluate --cohort` refuses a bare `--source` before any attempt with
`--source must be a non-bare git checkout (replay writes <out>/source)`. Cohorts built by
replay **must** use `<out>/source` as `--source`. `<out>/cohort.stub.json` lists every built
task pinned by SHA-256 and records `source` as that checkout; before `forgecell evaluate
--cohort`, add exactly two Formula variants, remove the documentary `source` field (the
strict cohort decoder rejects unknown fields), rename it to `cohort.json` and pass `--source
<out>/source`. When **no** task is built (or the checkout cannot be written, in which case the
partial checkout is removed), `<out>/source` is not created and the stub carries no `source`
hint and an empty `tasks` list.

A multi-task `<out>/source` holds every task's base commit, and a later PR's base contains an
earlier PR's solution. `evaluate --cohort` therefore never runs an attempt from `--source`
itself, which it only reads: for **every** attempt it creates a fresh throwaway source under
that attempt's directory holding **only** the plan's base commit (shallow, no replay refs,
detached `HEAD`, `origin` copied from `--source`, the same no-op fetch refspec, no
`FETCH_HEAD`), runs the molecule, diff capture and verification from it, and removes it after
the attempt. With `--keep-workspaces` the attempt's trees and its source are kept, but they
live in that attempt's own source, never shared, so a later attempt cannot see an earlier
attempt's solution.

Because the task models the issue as it was when work began, `issue.json` freezes
`State: "OPEN"` even when the source issue is now closed — intake's `State == "OPEN"` gate
would otherwise reject every replayed (closed) issue. The live issue state is recorded in
`approval.md` only. The frozen plan stores no manifest evidence: at run time `evaluate`'s
`configureReadiness` cites, for each scope path that does not exist at base, the evidence ID
of the nearest-ancestor manifest (`go.mod`, `package.json` or `pyproject.toml`) in the
repository snapshot `CollectForTicket` collects from the source at the base commit, so
readiness accepts an added file (with no ancestor manifest the attempt fails readiness as
before). The builder writes nothing outside
`<out>`, and its tests use recorded `gh` fixtures with a tiny fixture repo — no network.
