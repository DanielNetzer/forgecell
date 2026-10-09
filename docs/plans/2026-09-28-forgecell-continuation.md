# Forgecell continuation implementation plan

Status: historical Node Lab implementation record. The Go CLI and native installer are now the supported paths; local UI implementation remains deferred.

**Goal:** Continue PR #2 with real opt-in BYO execution, auditable ledgers, human-reviewed Formula learning and a minimal local companion.

**Spec:** [Product bible](../product-bible.md) plus the approved Runs & suggestions screen.

**Architecture:** Preserve the terminal runner and existing Formula/ledger files. Add an executable stdin adapter to approved Formulas, a separately configured learn Atom, and a loopback-only viewer using the same files. Keep all external writes (merge/deploy/comment) unimplemented and labelled skipped.

**Tech stack:** TypeScript, Ink and YAML for the historical terminal runner.

## Constraints

No new primitive. No provider-specific model dependency. No auto-invoking detected binaries. Never execute from a draft. No raw shell interpolation of issue text. No implicit Formula writes from model output. No hosted access to local ledgers. No Linear/Datadog implementation claim.

## Review focus

- Invalid or duplicate Atom IDs cannot result in unexpected execution.
- Timeout, nonzero exit and failed intake must leave an honest ledger.
- Suggestions apply only to the exact Formula snapshot they were generated for.
- Browser requests cannot read local data or mutate Formulas cross-origin.
- Historic ledgers without new provenance fields still render.

## Tasks

- [ ] 1. Runner: add validated harness.command argv + timeout, stdin execution adapter, Formula snapshot/hash and source/action records. Tests: unbound preserves record-only; configured command receives JSON and does local work; failed intake/draft never invoke; timeout/failure recorded; scoped GitHub Actions reads.
- [ ] 2. Learning: learn Atom command returns full proposed YAML with rationale; store original/proposed text and source ledger id; approve/dismiss commands; tests for no auto-apply, invalid proposals, no-op, stale Formula, dismissal, and persisted before/after.
- [ ] 3. DEFERRED BY USER — design/UX only, do not implement companion UI. Original planned companion: forgecell ui loopback server, React two-column screen reading actual ledgers and suggestions. Exact snapshot, sources, Atom details, YAML diff and review controls. Empty state and responsive detail view. Tests for origin/host checks and malformed decisions.
- [ ] 4. CLI documentation, real read-only dogfood on issue #5, test/build verification and code review. Update PR #2 without merging.

## Execution record

- Fresh clone of PR #2; merged origin/main to include existing CLI. No local user work affected.
- Baseline: CLI 31 tests + typecheck pass.
- Implementation runs inline. Product bible is the authority; prior mockup sample data is not real ledger evidence.


## Latest scope override — 2026-09-28

User: “Don’t implement the UI yet, lets continue iterating on the design and UX.” Product companion stays as mockups/design only. No UI server, React companion, or live animation implementation without renewed user authorization. CLI/harness/ledger groundwork remains within the active dogfood goal. Do not expand UI implementation during design iteration. The proposed Atom visualization is a presentation of existing workflow steps, not a new primitive.

CLI work precedes any optional local UI.

## Verification and decisions — 2026-09-28

- Explicit BYO execution, source/action provenance and Formula snapshots implemented. Real issue #5 remains record-only; issue #6 generated a Unicode regression test and fix through the configured coding harness in an isolated checkout.
- Learning commands and stale-safe human approval implemented. Actual meta invocation returned unchanged YAML; no proposal was manufactured or applied. A real approved improvement still needs sufficient evidence.
- Final review found one Important interruption gap. Intake/check SIGTERM subprocess tests failed first, then passed after run-wide finalization. Full suite: 49 passed, typecheck/build passed.
- Final minor (deferred): Windows descendant-process cancellation is unverified; current install/dogfood path is macOS/POSIX.
- Ruling: local UI implementation remains deferred per the user's latest scope, despite its presence in the original goal. No companion UI was implemented.
- Ruling: do not manufacture a Formula mutation to satisfy the dogfood demonstration. Cost: the human-approved learning example remains pending until the evidence warrants a change.

## Current outcome — 2026-09-28, after human review

Earlier execution notes above are historical. The user approved the first Formula fallback lesson; the next issue #6 Molecule
used its exact revised snapshot and reported the blocked command separately from 52 passing
fallback tests. The run stopped at the waiting ship gate. No issue comment or merge occurred.

Learning now requires plain-language process change, ledger reasoning, expected impact and
next-run evaluation before the diff. Cosmetic/inert edits do not qualify. README and related
docs distinguish current execution from the shipping ambition, and document the iterative
research loop. Follow-up effectiveness evaluation remains manual. Local UI stays deferred.
