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
