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

The analysis validator is never relaxed. If a response is rejected only because it
asserts independent acceptance (`independent-acceptance`, `independentProvenance` or
`reviewedAcceptance`), intake makes **one** corrective analysis call. It receives the
exact validator message and the rejected response, under a `correction` key; the
rejected response is also kept on its failed attempt in the ledger. The corrective
response must pass the same validation. A second violation blocks intake, and no
coding harness runs. The budget is one retry per intake, whichever pass it occurs
in, so there are at most three analysis invocations (one supplemental evidence pass
plus one retry). Every invocation is its own attempt with its own provenance action.

Missing evidence, invalid or unresolved evidence requests, malformed or oversize
output, a failed or timed-out analysis harness and every other error never retry.

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
