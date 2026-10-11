# Ticket readiness (development build)

This branch adds pre-coding analysis and scope approval to the Go Lab. It is not in
`0.2.0-preview.1`. The public release is unchanged. Installed Codex dogfood verified a clear ticket and blocked an ambiguous one before
coding. Draft publication and its exact-commit CI complete the delivery experiment;
these results do not establish release readiness across all supported harnesses.
See [the research record](research/ticket-readiness-dogfood.md).

The intake Atom assesses the ticket. The scope gate pauses. Both are part of the
same Molecule. The Formula orders intake → scope gate → harness → check → review
gate → ship → document. Existing approved Formulas are never silently rewritten;
they need a reviewed scope gate before new execution. Historical ledgers remain readable.

## Run and review

```sh
forgecell run 123 --target main --json
# Optional human-authored acceptance, setup, artifacts and environment:
forgecell run 123 --target main --verification reviewed-checks.json --json
# Read the returned plan, exact paths, commands, evidence, and unknowns.
forgecell run 123 --approve PLAN_DIGEST --json
forgecell ledger MOLECULE_ID
```

`--verification` accepts a bounded JSON object with `checkInputs` (ID/path/SHA-256/
content), `checks`, `setup`, `artifacts`, and optional `policy`. Its frozen content
is supplied to analysis and included in the pending plan. Loading the file does
not approve its commands. Each independent check must identify its substantive
human-review provenance. New inputs cannot be substituted during `--approve`;
use an amendment and a new digest.

### Corrective analysis retry

The analysis validator is never relaxed. Three rejections of an otherwise complete
response are retryable: the response asserts independent acceptance
(`independent-acceptance`, `independentProvenance` or `reviewedAcceptance`), it
carries more than 16 evidence requests (`at most 16 evidence requests permitted`),
or a check is defined by a file the same plan proposes to edit
(`drop or replace checks defined by in-scope files`).
For any of them, intake makes **one** corrective analysis call. It receives the exact
validator message and the rejected response, under a `correction` key; the rejected
response is also kept on its failed attempt in the ledger. For the evidence-request
cap the correction also carries an `instruction` to return at most 16
`evidenceRequests`, ordered by importance to the ticket scope. The corrective
response must pass the same validation: 16 requests are accepted, 17 are not, and a
response that is still over the cap is rejected, never accepted. A second rejection
blocks intake, and no coding harness runs. The over-cap response never becomes the
plan's analysis, so none of its requests is collected or approved.

A check is defined by an in-scope file when one of its `definitions` evidence IDs
resolves to a scope path, or when an `argv` token, taken as written or relative to
the check's `dir`, is exactly a scope path. The message names each such path and
check ID after the fixed phrase, so the correction can drop or replace those
checks. A conventional test path that `argv` merely names is not rejected, matching
the run-time guard's non-blocking test-source assessment, and a read that no `argv`
token spells out (through a test runner or a shell string) is not detected at
intake. The rule applies to the model's response only, once it has no evidence
requests left; it does not apply to `--verification` checks or Formula baseline
checks. The run-time guard stays as the backstop: approval still refuses a plan
whose frozen definition overlaps its scope (`frozen definition overlaps proposed
scope`).

The three cases share one budget: one retry per intake, whichever pass it occurs in
and whichever rejection uses it. So there are at most three analysis invocations
(one supplemental evidence pass plus one retry). Every invocation is its own attempt
with its own provenance action.

Missing evidence, invalid or unresolved evidence requests (a bad path, a duplicate
or an unknown citation), malformed or oversize output, a failed or timed-out
analysis harness and every other error never retry.

### Per-Molecule coding timeout

A Formula's harness `timeoutMs` stays the default. A Formula may also declare
`maxTimeoutMs` (an integer from 1 to 3600000, not below `timeoutMs`). That is the
ceiling for a reviewed, per-Molecule `harnessTimeoutMs` in the scope plan. Without
`maxTimeoutMs` the ceiling equals `timeoutMs`, so existing Formulas gain no extension.
Adding it changes the Formula's hash, so it needs one Formula approval; after that a
larger scope needs only the scope gate.

```yaml
harness:
  timeoutMs: 900000
  maxTimeoutMs: 1800000
```

Intake proposes the value from scope size alone, never from the model: up to 8 scope
paths keep the Formula timeout; each further path adds an eighth of it, rounded up to
a minute and clamped to the ceiling. The scope gate prints the effective value, the
Formula default and the ceiling. `harnessTimeoutMs` is part of the plan digest, so it
needs exact approval, and a plan without it keeps its earlier digest. A proposal
is clamped to the ceiling, and a value above it is rejected at `amend` and again at
approval, before any coding process starts, with an error naming both numbers. When used, the value is recorded in
the immutable plan, in the ledger notes and in a timeout's failure detail.

When the coding harness times out, the gate message reports the elapsed time, the
configured timeout and its source, the retained tree with its changed-path count and
any violations, and the exact commands to keep the work: `forgecell recover MOLECULE
--plan DIGEST --confirm-stopped --lab LAB`, then export the latest plan with
`forgecell ledger MOLECULE --lab LAB`, set `"continuation":"adopt-failed-tree"` and
`"adoptAttempt"` to the failed attempt, and run `forgecell amend MOLECULE --parent
DIGEST --plan FILE --lab LAB`. Leaving `continuation` out instead authorizes another
coding attempt against the retained tree, with `harnessTimeoutMs` up to the ceiling.
Nothing is retried automatically.

Analysis uses frozen GitHub Issue title/body and committed repository evidence.
Clarifications belong in the Issue body. Forgecell does not post questions or read
comments in this milestone. Named Go/JS/TS/Python/Rust/shell source paths in the
Issue are included within bounded evidence; other implementation behavior stays
unknown. Unsupported or insufficient evidence yields questions, not coding.

Approval is single-use and bound to the Molecule, revision, Issue content, Formula,
base, target, repository, scope, checks, binding and, for Claude Code, the exact
coding allowlist derived from those checks and the Formula's suggested checks.
The allowlist is part of the plan, so a changed allowlist changes the digest; coding
refuses to start if the stored list differs from the one rederived. It never includes
commit, push or publishing commands, and each attempt records it in the ledger. Changing
consequential inputs stops execution. Issue retrieval/update timestamps alone are not semantic changes.
Approval never authorizes a push, merge, deployment, or Issue comment.

A scope plan is not a Formula mutation. It describes this ticket's work. Formula
learning still proposes separately reviewed changes to the reusable process.

## Amend without losing work

Export the latest plan from the JSON ledger and edit its proposed scope, evidence,
checks or assessment. Keep original input identities. The engine supplies the new
revision, parent digest, paused workspace tree and `codingAllowlist`: the allowlist is
rederived from the amended checks and the Formula snapshot, so any submitted value is
replaced and shown at the scope gate for approval.

```sh
forgecell amend MOLECULE_ID --parent OLD_DIGEST --plan revised-plan.json
# Review both immutable plans and the retained workspace diff.
forgecell run 123 --approve NEW_DIGEST
```

The harness can stop with `{"kind":"scope-change","reason":"…","paths":["…"]}`.
It must not write newly requested paths before approval. Existing out-of-scope
writes remain a violation in the ledger. Amending such a run also requires
`--accept-existing-tree EXACT_TREE` after reviewing that existing diff. The new
scope must include all retained changes; drift invalidates approval. Required
verification runs again in a fresh revision-specific checkout.


### Retry checks without coding

For completed coding output, set `"continuation": "checks-only"` in the successor
plan before `amend`. Review its new digest and approve with the normal `run`
command. The terminal explicitly prints that no coding harness will run. This
mode requires unchanged scope and retained source, the same frozen intake,
Formula and binding identities, and no unresolved violation or uncertain coding
attempt. Check/setup changes remain visible in the newly approved plan. Checks
use a fresh checkout; this mode never invokes the coding harness or its preparation
hook. Omitting `continuation` retains the ordinary coding continuation.

Each verification attempt records its plan digest, source tree, start/end and
outcome. Full results and completed command observations are stored in immutable,
hash-linked files under `verification-history/<molecule>/`; the JSON/Markdown
ledger indexes them. The current verification field is a compact projection;
stdout/stderr live in the referenced files. Amendments clear only the current
projection, never prior attempts. Recovery marks a started attempt interrupted
and retains completed observations. A process killed before its observation is
saved remains uncertain; it is never called passed.

Limits: 100 attempts per Molecule, 16 setup/check commands per candidate view
(up to 32 observations with the protected view), 64 MB per evidence file and
5 MB for the main ledger. A limit or persistence failure stops progress instead
of deleting history. Interrupted writes can leave unreferenced evidence files;
inspect these before recovery. Missing or modified referenced evidence blocks
resume, delivery and learning. Keep the history directory with the ledger when
archiving a Lab; local hashes do not authenticate against an owner who can rewrite
all local state.

### Learning eligibility

Learning rejects active execution, pending scope approval and unreconciled
publication. It distinguishes failure evidence, locally verified but undelivered
work, and publication evidence. A receipt with a recorded draft flag supports
`draft-published` at publication time only; older receipts have unknown draft
status. Neither establishes merge, deployment or production success. Historical
finished ledgers without sufficient lifecycle evidence remain explicitly unknown.

The meta harness receives these classifications and complete selected verification
results, including prior failures, verified against their stored hashes. The
combined archived-result input is limited to 4 MB; larger selections fail with an
explanation instead of truncation. Formula suggestions persist the classifications
and still require human review. Learning may reason from failure; it cannot treat
that failure as proof of a successful process.

## Interrupted attempts

Stop the Lab and all its child processes, inspect the workspace, then:

```sh
forgecell recover MOLECULE_ID --plan DIGEST --confirm-stopped
```

Recovery refuses an apparently live recorded owner. It captures retained work and
marks the outcome interrupted. It does not run the harness or checks, erase files,
or infer success. The approval stays consumed. After inspection, submit an amended plan and approve
its new digest to authorize a new attempt against the retained state. Recovery
itself never retries the uncertain attempt. Out-of-scope writes also require
review of their exact existing tree. An ownerless execution lock is refused because
an approval process may still be starting.

## Stalled harness attempts

### Behavior that was already supported

These limits existed before the diagnostics below and are unchanged by them.

- **Bounded run.** Every harness call goes through `process.Run`. It applies
  `Options.Timeout` and uses 10 minutes when that is unset or not positive. It also
  caps captured output at 1,000,000 bytes unless a different positive cap is given.
- **Three distinct stops.** Deadline expiry sets `timedOut`. Cancellation of the
  parent context sets `interrupted`. Reaching the output cap sets `overflow` and
  neither of the other two. On Linux and macOS the managed process group is killed
  and then reconciled, and a process that cannot be shown stopped is never `ok`.
- **Analysis timeout.** Analysis, through the bound harness or an approved analysis
  wrapper, uses the bound binding's `timeoutMs`. `0` means the process default.
- **Coding timeout.** Coding uses the plan's `harnessTimeoutMs` when it has one,
  otherwise the Formula `timeoutMs`. A coding timeout produces the recover and amend
  guidance above and nothing is retried automatically.
- **Retries.** The only automatic retry is the single corrective analysis retry after
  a policy violation, within three analysis invocations. A timeout, interruption,
  stall or any other analysis error never retries. A consumed approval digest never
  starts another coding process.
- **What analysis attempts kept.** Before the diagnostics, an analysis attempt kept
  only a status and an error string, so a timeout and an interruption could be told
  apart only by reading that text.

### Diagnostics

Each run now records what the Lab observed. Nothing here is a progress claim.

- **Activity.** `activity` holds `stdoutBytes`, `stderrBytes` and `lastOutputMs`, the
  offset from process start of the last observed output byte. Bytes dropped past the
  output cap are still counted. `lastOutputMs` is absent when no byte was observed.
  An `activity` object with zero bytes is a recorded silent run. A record with no
  `activity` at all, such as an older ledger, a supplied analysis function or a
  process that never started, is *not recorded*, which is different from silent.
- **Terminal state.** One of `succeeded`, `timeout`, `interrupted`, `output-limit`,
  `unreconciled`, `failed` (a non-zero exit, a start failure or a recorded error) or
  `unknown` (no discriminating evidence). It is derived only from the flags above.
  An unreconciled process outranks the cause that stopped it, because it may still be
  running. A timeout or interruption outranks the output cap, since the cap can also be
  reached while a process is being stopped.
- **Elapsed time.** The existing `elapsedMs`.
- **Reconnect evidence.** Always `unknown`. No provider-neutral reconnect signal
  exists, so none is invented. Binding-supplied reconnect markers are deferred to a
  follow-up.

Analysis attempts persist these as a `diagnostics` object (`elapsedMs`, `activity`,
`terminalState`, `reconnect`) for completed and failed attempts, and the Markdown
ledger shows it in the attempt's JSON block. An attempt that came from a supplied
analysis function, or that stopped before any process result existed, has no
`diagnostics`, which reads as unknown. Coding attempts carry `activity` in
`result` and in the immutable `harness-ATTEMPT-result.json`. The Markdown ledger adds
one `Diagnostics:` line per harness attempt, with the terminal state derived from the
stored flags. Existing lines are unchanged, and `forgecell ledger` prints the stored
JSON as before.

Activity is the number of bytes seen on the child's stdout and stderr pipes. It never
comes from PID liveness, reconciliation results, argv, environment or output content,
so the diagnostics cannot carry a secret. Absence of output is not proof of a stall,
since some providers write their final answer to a file after the run. Presence of
output is not proof of progress.

An original attempt is never merged, rewritten or relabelled. A later successful
availability probe is a separate observation. It does not reclassify the earlier
attempt and it authorizes nothing: continuing still needs `recover`, `amend` and a
new approved digest.

### What an operator can determine now

| Question, without opening the process or provider output | Before | After | Deterministic test |
| --- | --- | --- | --- |
| Was an analysis attempt a timeout or an interruption? | Only by reading the error text | `diagnostics.terminalState` is `timeout` or `interrupted` | `TestAnalysisAttemptsRecordStallInterruptionAndSuccessDiagnostics` |
| Was any output ever observed? | Not recorded for analysis; coding needed the immutable result file | `activity` byte counts, with recorded silence distinct from no record | `TestSilentStallTimesOutWithRecordedSilence`, `TestSilentCodingStallIsDiagnosableAndNeverReplayed`, `TestRecordedSilenceIsDistinguishableFromNoRecord` |
| When was the last output? | Not recorded | `lastOutputMs`, never later than `elapsedMs` | `TestPartialOutputThenStallRecordsLastOutput`, `TestPartialOutputThenCodingTimeoutKeepsOutputOutOfDiagnostics` |
| Timeout, interruption or output cap? | Three flags to compare | One `terminalState` | `TestTerminalStateClassifierUsesOnlyExistingFlags`, `TestOutputLimitCountsDroppedBytesAndIsNotATimeout`, `TestCancellationAfterPartialOutputIsInterruptedNotTimeout`, `TestCancelledCodingAttemptIsInterruptedNotTimeoutAndNotReplayed` |
| Was there reconnect evidence? | No signal | Always `unknown` | `TestTerminalStateClassifierUsesOnlyExistingFlags` |
| Is the harness available after a stall? | A new probe, read by hand | A new probe is a separate observation and leaves the stalled one unchanged | `TestHealthyProbeAfterStalledProbeIsASeparateObservation` |
| Can output leak into the diagnostics? | Not applicable | Only states, byte counts and offsets are stored | `TestDiagnosticsNeverContainOutputText` |

This compares what can be read from the ledger. Cost and reliability gains are not
measured, and no claim is made about either.

## What check results mean

- **Regression:** previously covered behavior. When conventional test files or
  cited check definitions change, retain a protected baseline test view alongside
  the full candidate view. Each observation records its actual tree. This is not
  a universal analysis of arbitrary dynamically loaded test helpers.
- **Candidate:** model-proposed or newly written checks; useful evidence, not an
  independent correctness oracle.
- **Independent acceptance:** separately reviewed frozen input with recorded
  provenance, as in the controlled evaluation path. A model cannot assign itself
  this category. Running a model-selected command separately is not independence.

Every required check must pass on the correct view. Missing commands, failures,
timeouts, output overflow and source/index mutation block delivery. Changed
approved command definitions need a reviewed amendment. Legacy runs without
readiness evidence cannot be newly delivered. If independent behavioral acceptance
is absent, final publication also requires explicit `--accept-unverified` review.
Local checks never stand in for exact-commit GitHub CI.

Checks run in a fresh worktree, with a temporary HOME and an explicit environment
allowlist. Network access is unrestricted; this is **not an OS sandbox**. Commands
can execute untrusted repository code. Credential forwarding must be explicit.
Artifact roots belong to approved setup/check commands and cannot overlap source.
Their v1 default limits are 100,000 entries, 1 GB, depth 32; a plan may lower them.
Inventories are hashed, nested repositories and escaping links are rejected, and
artifacts never enter the delivered tree or seed a fresh verification checkout.
A repository-tree audit cannot observe every transient or host-wide write.

### Expected protected deltas

The protected view restores base-commit test sources, so a ticket whose approved
purpose is to change a pinned contract (for example a `testdata` fixture) would
otherwise always fail there. A reviewed plan may declare `expectedProtectedDeltas`:

```json
"expectedProtectedDeltas": [{
  "checkId": "go-test-lab",
  "package": "github.com/owner/repo/lab/internal/harness",
  "test": "TestAdapterCompatibility/claude-code/molecule",
  "scopePath": "lab/internal/harness/testdata/contracts.json",
  "reason": "The approved change alters the pinned Claude coding argv.",
  "evidence": ["file-5"]
}]
```

- Each entry is an exact Go package import path plus an exact test name
  (subtests as `Test/sub/case`; no wildcards). The check must be a required
  `regression` check whose argv requests `-json` (for example
  `go test -json ./...`). `scopePath` must be an approved scope path that the
  protected view restores (a conventional test path or a regression check
  definition). Duplicates and more than 64 entries are rejected.
- The declaration is part of the plan digest. It is proposed in a reviewed
  amendment (`forgecell amend`) and accepted only by approving the exact digest at the
  scope gate, which lists every delta with its package, test, scope path and
  reason. Intake never writes or approves a delta; when scope touches `testdata` or
  `fixtures` it only records a labelled advisory ("proposed for review; never
  auto-approved"). Plans without the field keep their earlier digests.
- Deltas apply to the protected view only. The candidate view still has to pass
  entirely. In the protected view the check passes if it exits non-zero, its
  complete `go test -json` stream parses, and every failing test is declared.
  Package and test must both match. Failing parents of a declared failing subtest
  are accounted for; a parent that also fails for an unrelated reason cannot be
  told apart from that, so review declared subtests with care.
- Any undeclared failing test fails the check, naming its package and test. So do
  a build or setup failure with no failing test, unparseable output, timeout,
  interruption, output over the 256,000-byte capture limit, or a non-zero exit with
  no failing test. For a large module, narrow the check argv to the affected
  packages so the JSON stream fits the limit.
- A declared test that does not fail is reported as a warning and does not fail
  the check. The final review output lists the accepted deltas and warnings next to
  the required-check result, and the compact ledger keeps them.
- Delivery accepts a protected observation with failures only when the approved
  plan's declared deltas still cover its recorded failing set.
- Only Go tests are supported. Matching is exact equality, never a pattern.

## Provider and policy support

Codex analysis preserves the configured provider/model and disables action tools,
plugins, apps and configured MCP servers. A live ambiguous-input probe passed on
2026-10-03. Project-scoped provider configuration currently stops analysis until
its model selection can be preserved explicitly. Claude Code needs the verified
safe-mode/tool controls; a live Claude analysis probe is still outstanding. Cursor
and custom bindings remain inspectable but stop new readiness runs because their
read-only action isolation has not been established, unless a custom binding
carries an approved analysis wrapper (below). No provider substitution.

### Approved analysis wrapper for a custom binding

A custom coding binding can opt in to ticket analysis by adding a separate
`analysisWrapper` reference (`manifest`: a Lab-relative path, `sha256`: its exact
digest) to the Formula `harness` block. The coding command, its argv and native
adapters are unchanged. A Formula without the reference parses and hashes exactly
as before and gains no analysis capability. A reference on a native `__adapter`
binding, or without a coding command, is refused.

The manifest is a strict, size-bounded JSON file that pins by content digest the
wrapper executable, its argv, configuration files, interpreter, policy files and
reviewed enforcement-critical dependencies. A mutable dependency nobody explained
is declared `unknown` and is never treated as verified. It also declares the
wrapper's own claims about inputs (stdin only), scratch writes, processes, tools,
network and credentials. The enforcement evidence is a pinned file: each entry
names a file `path` and its `sha256` plus origin, limitations and review basis,
and Forgecell recomputes the file's digest at every verification. A digest that
points at no file is refused, and the digest it computed is recorded in the
decision.

The permitted control vocabulary is deliberately small. `inputs` must be exactly
`stdin`, `tools` and `network` must be `none`, and no credential is ever forwarded:
a declared credential name is refused, both because a name such as `PATH`,
`TMPDIR`, `HOME`, `LD_*`, `DYLD_*`, `SSH_*` or an agent socket could replace the
fixed environment and because credentials combined with `network: none` are
contradictory. `scratchWrites` may be `none` or `temporary-directory`, and
`processes` may be `none` or `wrapper-managed`. The last two are explicit
relaxations of strict read-only analysis. They are recorded as such in the
decision (`relaxations`) and mean only that the wrapper claims to confine its
writes to its private temporary directory and to manage any helper processes it
starts. Forgecell removes that directory and reconciles the process group
afterwards. It does not confine the writes or prevent the processes.

These are claims, not proof. A human approves the exact Formula bytes, which pin
the manifest digest; trusting an external review of the enforcement is that
person's explicit decision. Forgecell observes only pin integrity, approval
freshness, the process outcome and whether the output is valid and bounded. It
does not establish external authenticity or substantive containment, and this is
not an OS sandbox.

Before every analysis invocation (initial, supplemental and corrective) the Lab
revalidates the Formula approval and re-hashes the manifest and every pinned
identity. A missing, self-attested, unrecognized-origin, stale, changed, denied or
unknown control or identity is refused before any process starts. Pinned paths
must be absolute and clean, must not be symlinks or resolve through one, must not
be group- or world-writable, and must not live under a directory that others can
write to (a sticky directory such as `/tmp` is accepted). On Windows these checks
cannot be made, so a wrapper is refused there.

The decision, including a refusal, is saved as immutable ledger evidence before
the process starts and is cited from the intake Atom provenance, whose analysis
source records the wrapper identity (`analysis-wrapper:BINDING@sha256:MANIFEST`).
The verified bytes, not the original paths, are what runs. The executable,
configuration, policy and pinned dependencies are streamed from the hash-checked
handle into a private temporary directory (`exec/` at mode 0500, the rest 0400
or 0500), hashed again there, and the copied executable is run with the approved
argv. The wrapper finds the copies under `$FORGECELL_WRAPPER_HOME` (`config/`,
`policy/`, `dependencies/`; files keep their names, so names must be distinct
within a role). A swap of an original after verification therefore cannot change
what runs. The interpreter cannot be copied: it is verified in place under the
same placement rules, and a script's shebang must name it exactly.

The environment is fixed: `PATH=/usr/bin:/bin`, `TMPDIR` and
`FORGECELL_WRAPPER_HOME`, nothing inherited. The working directory is the private
`scratch/`. The wrapper receives the frozen issue and repository evidence, the
human verification inputs and the Formula check Atoms over stdin and returns one
Analysis JSON document, bounded and validated like a native analysis. Wrapper
stderr is never returned or stored; only its byte count is. Timeout,
interruption, overflow, malformed output, a failed process or an unreconciled
descendant stop intake: no coding harness runs, no other provider or binding is
substituted and no plan is created. The human scope gate and exact plan approval
are unchanged, and a wrapper analysis authorizes no publication.

`doctor` reports a custom binding with a reference only as declared and unverified
until intake revalidates it; it never runs the wrapper.

Real intake records available GitHub ruleset and classic branch-protection
observations. Permission errors, missing responses and incomplete bounded results
are unknown, not evidence of no protection. Delivery refreshes these observations.
Advanced initial plans may pin reviewed observations with repeated
`--require-policy github-ruleset-sha256:HASH` or
`--require-policy github-protection-sha256:HASH`. A changed/unreadable required
snapshot blocks approval/publication. Unsupported constraints are rejected.
Observing unchanged rules does not prove compliance with every rule; merges and
deployments remain outside this milestone. API provenance:
[GitHub rules](https://docs.github.com/en/rest/repos/rules) and
[branch protection](https://docs.github.com/en/rest/branches/branch-protection).

## Quick rebind and per-role harness bindings

`forgecell init --lab DIR --rebind HARNESS` proposes a reviewed Formula diff that
changes only the binding block of an exactly approved Formula: `harness.binding`,
`harness.command` and the binding of the harness Atom. Atoms, checks, instructions,
learned guidance, the learn Atom and any per-role bindings are preserved, and the
proposal is refused unless its YAML differs from the approved YAML only at those
keys. It needs a current exact approval, a ready discovered harness with an
executable, no pending proposal, and none of `--harness`, `--meta-harness`,
`--meta-command`, the timeout flags or `--review`/`--approve`/`--dismiss`. The
saved capability evidence follows the new binding; the usual `--review`,
`--approve ID` and `--dismiss ID` flow applies, and dismissing leaves the active
Formula untouched. When `harnesses.coding` overrides the coding binding, the
harness Atom binding is left alone.

A Formula may declare optional per-role bindings next to the default `harness`:

```yaml
harness: {binding: codex, command: [...]}   # default for every role not listed
harnesses:
  analysis: {binding: claude-code, command: [...]}
  coding:   {binding: codex, command: [...]}
  meta:     {binding: claude-code, command: [...]}
```

Each entry has the shape of `harness` (the `command` is required; a `policy` key is
rejected) and may only name `analysis`, `coding` or `meta`. A role that is not
declared uses `harness`. `analysis` runs the intake Atom's ticket analysis,
`coding` the harness Atom, and `meta` the learn Atom, which must exist and must not
contradict the role binding. A declared role whose adapter cannot perform its
operation, for example Cursor for analysis or a custom command for analysis, is
rejected when the proposal is saved or inspected, with the adapter's own reason.
The default `harness` is not judged this way, so existing Formulas are unchanged.
Adding or changing `harnesses` changes the Formula bytes and therefore needs a new
exact approval.

Every harness-bound Atom action records the Formula binding that executed it as
`executedBinding` in the ledger, including each analysis attempt; older ledgers
decode unchanged. `forgecell doctor` adds a `roles` array (role, binding, source
`default`, `override` or `learn-atom`, readiness, capability and next command) and,
for a Formula with `harnesses`, is ready only when every configured role is ready.

## Remaining release evidence

The installed clear/ambiguous GitHub cases and independent acceptance passed.
Draft publication, exact-commit CI and final implementation review are recorded
in the research record. A replacement public preview requires a separate release
decision and provider coverage; this development milestone does not publish it. No automatic
Formula mutation, issue comments, MCP Events, or local UI is included.
