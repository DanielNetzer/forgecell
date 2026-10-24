# Forgecell Lab UI — handoff to Claude

Updated: 24 October 2026

## Purpose and scope

This folder contains an interactive design prototype for the local Forgecell Lab. Use it as the UX reference when integrating with the actual product. It is not a production client or an implementation of Forgecell's execution engine.

The intended experience is a compact tracking and human-approval interface: inspect work, understand evidence, decide what may happen next, and evaluate process improvements. Do not turn it into an IDE chat, team workspace or large navigation-heavy dashboard.

The core product and engine are maintained separately. Before implementing, inspect their current contracts, documentation and tests. This document describes UI requirements; it does not assert that proposed APIs or fields already exist.

## Files and local preview

- `index.html`: page structure, analytics, selected-Molecule region and shared dialog.
- `style.css`: visual system, responsive layouts, motion and accumulated prototype overrides.
- `app.js`: synthetic runs, client-only interactions and simulated approvals/evaluations.
- `assets/`: existing Forgecell Blob sprite, botanical artwork and fonts.
- `DESIGN-PROMPT.md`: initial direction and some iteration notes; the current HTML/CSS/JS and this handoff take precedence when older notes conflict.
- PNG screenshots: review snapshots from successive iterations, not all the latest state.

No build step is required. Serve this directory with a local static server. The current preview uses port 3112. Do not ship this static server as the product backend.

The prototype is vanilla JavaScript. Port its interactions to the product's existing frontend framework; do not introduce a framework merely to reproduce this folder. Consolidate the appended CSS overrides during integration.

## Locked product ontology

- **Lab**: the installed runtime and entire application context.
- **Molecule**: one ticket unit of work plus its ledger.
- **Atom**: one workflow step.
- **Formula**: durable YAML recipe arranging the workflow; learning proposes changes to this process.

The Lab orchestrates the user's local harness and its available capabilities. Do not add provider-specific execution implementations to the UI. GitHub is intake and a destination for evidence/PR links, not the orchestration engine. Preserve local Lab storage and provider-neutral boundaries.

## Visual and interaction direction

Follow the current Forgecell marketing palette: warm cream, forest-green text and actions, teal/mint accents, restrained glass surfaces. Keep the Blob small and purposeful; botanical assets sit behind frames, never over text or controls.

Use dense, legible UI. The Molecule grid is inspired by a contribution graph, but **one cell is one Molecule, not one day**. Do not imply calendar semantics or productivity scoring. Activity animation communicates running status; paused and completed work stay still. Preserve the Motion checkbox, reduced-motion support and hidden-page/offscreen animation suspension.

Maintain visible focus, keyboard operation, semantic labels and internal scrolling on narrow screens. The grid and Atom flow may scroll inside their containers; the page must not overflow horizontally at 400px.

## UX flows

### 1. Default Lab overview

With nothing selected, show the grid and Lab analytics. Display outcome counts, recorded duration coverage, cost availability, harness/model breakdown and a compact decision queue. Separate published, merged and production-verified outcomes. No evidence is not success.

Show the active Formula version for future runs. In production this is a per-Formula/default-binding relationship, not one global recipe for every kind of work. Make that scope explicit.

### 2. Find and select a Molecule

- Filters: All, Running, Needs my decision, Paused and Finished, with live counts.
- Search: issue number, title or Formula name; combine with the active filter.
- Keep the selected Molecule open when filtering/searching hides its cell. Consider a small “selected run is outside this filter” hint.
- Hover or keyboard focus shows title, current Atom and status. Escape dismisses the tooltip.
- Select a cell to open its details below. Select it again, or use Clear selection, to return to analytics.
- Arrow keys navigate cells; Enter/Space selects. The prototype uses seven-row column-major movement.

The prototype's Finished filter means its synthetic `finished` state, not all terminal backend outcomes. Map filter membership explicitly to the real lifecycle; do not silently classify failed work as delivered.

### 3. Inspect a Molecule and Atom

Selected details show intake identity, repository links, saved Formula version, partial duration/cost summaries and a horizontal Atom flow with statuses.

Selecting an Atom opens one panel with:

- Harness/executor and model identity, when recorded.
- Tools accessed or actions taken, separate from sources used for context.
- Inputs, actions, evidence, duration and cost coverage.
- A one-click detailed record/log view.

Distinguish planned capabilities from actual accesses. Derive the Atom sequence from the saved Formula/run, not the hardcoded six steps. Expandable ledger details remain attached to the selected Molecule.

### 4. Human delivery approval

For a pending gate show, compactly:

1. Exact retained change, revision and patch.
2. Checks tied to that revision, with individual evidence links.
3. Risks, uncertainty and missing evidence.
4. The exact operation, target and excluded authority.
5. Review/confirm and Keep paused actions.

The prototype's operation is draft PR publication only. Approval must not imply merge, deployment, Formula mutation or permission expansion.

After a real decision is accepted by the engine, update the cube, ledger, counts and queue from canonical state. Advance to the next pending item; when none remain show the overview. Keep paused removes the item from the immediate review queue while retaining it in the Paused filter; reopening returns it for review. Confirm that these semantics match the engine before implementing them.

Approved-but-unpublished is distinct from published. Never infer publication from a successful approval call.

### 5. Formula suggestion review

Show the process change, preserved gates/bindings, source Molecules and their ledgers, and evaluation status. A suggestion must explain what changes in the software-creation process rather than presenting an ordinary code patch as learning.

“Accept for evaluation” is separate from “Activate.” Acceptance must not mutate the active recipe, run evaluation commands without the required scope, or alter existing Molecules.

### 6. Evaluate a candidate

Compare baseline and candidate using duration, observed cost and declared quality outcomes. Show sample sizes, input comparability, harness/model versions, measurement coverage, failures/retries, uncertainty and trade-offs.

The current comparison UI is a **synthetic example**, not a real evaluation. Real results must come from retained evaluation evidence. If evidence is absent, display Not evaluated and do not invent a winner.

Keep current and Activate candidate are separate decisions. A faster candidate that costs more is a trade-off, not an automatic improvement. Acceptance-test success is one quality metric, not a claim about all software quality.

### 7. Activate and roll back

Activation changes the default for **future Molecules using that Formula**. Existing Molecules retain their original snapshots. Version and evaluation history remain one click away.

Rollback shows the source and destination versions, process changes and retained gates. Require a reason and confirmation. Retain both versions, evaluation evidence, actor, timestamp, reason and exact decision identities. Rollback is a new audited selection of retained bytes, not deletion of history or rewriting old ledgers.

## What is mocked — do not ship as real behavior

- 196 generated sample Molecules and repeated titles; sample numbers are not verified GitHub issue identities.
- Every Formula name/version association, Atom status and approval state.
- The patch (`demo-a72c` and `docs/example.md`), check results and claimed file counts.
- Harness/source/tool metadata; these are illustrative, not observed accesses.
- Atom durations: fixed synthetic values. Most model identities and all ordinary cost observations are missing.
- Before/after example: 10 tasks per version, 4m10s vs 3m40s, $0.18 vs $0.21 and 9/10 pass rates. All invented for design demonstration.
- Formula v3/v4, activation, rollback and their history. There are no executable candidate bytes or verified digests in this prototype.
- Decisions and history exist only in browser memory and reset on reload.
- No API calls, command execution, persistence, authentication, live logs, PR creation, installation change, merge or deployment occurs.
- Repository-level GitHub links are real. Direct issue/PR links are deliberately absent; do not build them from synthetic IDs.

The prototype reuses some illustrative details across unrelated sample runs. Replace them with run-specific records. Do not treat prototype data as fixtures proving backend correctness.

## Connecting to the real Lab

### Inspect existing contracts first

Find the current Lab storage resolver, Formula snapshots and activation records, Molecule/Atom ledger schemas, event/history model, approval gate commands, evaluation records and receipt types. Use their existing identifiers and validation. Do not assume their structure from this JavaScript.

Prefer a narrow local service or the product's existing IPC boundary around engine capabilities. The browser must not read arbitrary filesystem paths, execute shell commands, hold provider credentials or call GitHub/Vercel directly to perform workflow operations.

Suggested responsibilities below are conceptual operations, **not invented endpoint specifications**:

| Read capability | UI use |
| --- | --- |
| Lab identity and Formula bindings | Repository context and future-run default |
| Paginated/queryable Molecule summaries | Grid, filters, search and queue |
| Molecule snapshot plus ledger/events | Details, lifecycle and original Formula |
| Atom observations and bounded log chunks | Inputs/actions/tools/sources/evidence |
| Pending exact gate request | Patch/check/risk review and approval scope |
| Formula proposals and source ledgers | Process improvement review |
| Evaluation comparisons and coverage | Before/after evidence |
| Formula version/activation history | Active version, prior recipes and rollback |
| Authoritative aggregate accounting | Analytics without fabricated totals |

For live updates use the existing event/IPC mechanism, or a bounded polling adapter if no stream exists. Reconcile by stable IDs and monotonic revisions; reconnect must not duplicate operations or lose retained evidence. Keep UI updates separate from authoritative execution state.

Use actual source URLs from validated records for Issue, PR, commit and check links. Open external links in a new tab with safe rel attributes. If a PR has not been created, say so rather than supplying a placeholder URL.

### Approval safety contract

1. Fetch the engine's complete pending request and exact identity: Molecule, gate, plan/revision, retained source/check evidence and Formula bytes/digest as applicable.
2. Present the reviewed scope and effects. A human click authorizes only that operation.
3. Submit the decision through the existing engine gate, including its expected revision/identity. Do not bypass gates because the UI is local.
4. The engine must atomically reject stale, superseded, unauthorized or mismatched requests and preserve the decision audit trail.
5. Disable duplicate submissions while pending. Use the supported idempotency mechanism; after an ambiguous response, read back the existing decision instead of blindly retrying.
6. Show pending feedback immediately, but only mark a decision committed and advance after authoritative acknowledgement.
7. On stale evidence, retain context and require review of the new exact inputs. On failure, keep the item selected with a retryable explanation; do not silently remove it from the queue.
8. Formula activation and rollback require the complete exact executable bytes and applicable human approval. The prototype's process sketch is not a substitute.
9. No approval authorizes new credentials, permissions, deployment or destructive work beyond its explicit scope.

Do not add direct integration logic for provider operations; route them through the Lab's approved harness/capability architecture. Keep credentials and private evidence local under the actual configured Lab location (normally `~/.forgecell/labs/`).

## Accounting rules

- Separate wall-clock Molecule duration, active Atom time and waiting time. Summing parallel Atom durations is not elapsed run time.
- Preserve units, currency, provenance, observation timestamps, coverage and estimated/observed distinction.
- Unknown cost is not zero. “Not applicable” is different from “Not recorded.”
- Do not mix currencies or infer money from tokens without an explicit recorded pricing basis.
- Group by recorded harness/model identities; retain Unknown/Not recorded groups rather than dropping them.
- Define retry/cancellation/failure accounting consistently and avoid counting attempts twice.
- Cost per merged PR requires verified merge attribution and a clear denominator. A draft publication receipt does not qualify.
- Evaluation benefits require comparable baseline/candidate evidence. Report uncertainty and negative outcomes.

The prototype sums completed synthetic Atom durations and labels them partial. Production analytics should be computed from canonical observations, not from UI labels or hardcoded phase tests.

## Implementation limitations to address

- Replace the large `innerHTML` render functions with maintainable components or safe DOM rendering. Never interpolate issue text, logs, YAML, reasons or external URLs into HTML without escaping/validation. Render untrusted content as text.
- Replace frontend-only flags and in-memory histories with authoritative durable engine state; server timestamps and decision identities must govern the audit trail.
- Do not hardcode one registry entry, v3/v4, six Atoms or one candidate. Support actual Formula identities and version relationships.
- The compact contribution-style cells need a production touch-accessibility review. Consider an accessible list alternative or enlarged mobile targets.
- Verify tooltip hover persistence, focus, Escape, dialog focus return, arrow navigation and screen-reader announcements.
- When selecting an item outside the current filter, keep context clear. After queue advancement, do not steal focus unexpectedly or silently lose the selected item.
- Version/evaluation history currently uses a text dialog. A structured compact history is preferable once real records are connected.
- Ensure activation/rollback race handling; another window can change the active default while a dialog is open.
- Synthetic example results must not appear as measured production results. Remove the preview-only paths from the real build or clearly isolate a demo mode.

## Acceptance checks before integration is considered complete

- A real retained Molecule loads with the correct intake, original Formula bytes/version, Atoms and ledger.
- Filters/search preserve selection; zero results are clear; counts reconcile with the same data scope.
- Atom logs and metadata reflect actual observations and redact secrets where required.
- Analytics reconcile with ledger observations, including missing coverage and retries.
- Approval, pause and reopen use real gate semantics; duplicate and stale submissions cannot bypass validation.
- A decision updates UI only after canonical acknowledgement; failures and reconnects retain context.
- A suggestion links to actual source ledgers and a real candidate diff; unevaluated claims remain unevaluated.
- Evaluation displays comparable measurements and trade-offs, including missing cost/quality data.
- Activation changes only the appropriate future-run binding. Existing Molecule snapshots remain byte-identical.
- Rollback requires a reason, binds to retained version bytes and records an append-only decision without erasing history.
- No operation implicitly publishes, merges, deploys or expands permissions.
- Keyboard, reduced motion, narrow mobile screens, focus management, contrast and long content work.

## Recommended delivery sequence

1. Read-only Lab adapter and canonical data mapping.
2. Grid, search/filter, selected Molecule, Atom detail and bounded logs.
3. Trustworthy measurement aggregation with coverage.
4. Existing human gate integration with stale/duplicate/reconnect handling.
5. Suggestion/evaluation review backed by retained records.
6. Exact-version activation and rollback through existing engine gates.

Keep the first connected version small. Where the backend has no capability, show an honest unavailable state and document the gap rather than implementing an alternate engine in the UI.
