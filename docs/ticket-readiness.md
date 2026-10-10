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
revision, parent digest and paused workspace tree. Keep `codingAllowlist` as exported
unless the checks it derives from change; amending checks without updating it makes
coding refuse to start, and omitting it leaves coding with only the base allowlist.

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
read-only action isolation has not been established. No provider substitution.

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

## Remaining release evidence

The installed clear/ambiguous GitHub cases and independent acceptance passed.
Draft publication, exact-commit CI and final implementation review are recorded
in the research record. A replacement public preview requires a separate release
decision and provider coverage; this development milestone does not publish it. No automatic
Formula mutation, issue comments, MCP Events, local UI, or multi-harness routing
is included.
