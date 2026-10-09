# CLI MVP delivery implementation plan

Status: historical TypeScript implementation plan. The Node Lab and installers
described below were retired after the Go CLI and native installer replaced them.
Use [Install](../install.md) for current user instructions.

**Goal:** Install without source access, deliver a fresh issue to a reviewable PR, and evaluate one process change with independent evidence.

**Architecture:** Keep the TypeScript/Ink Lab and its four primitives. Produce an installation-ready Node CLI artifact in CI, execute issue work in an isolated checkout, and store exact revision/check provenance in the Molecule ledger. Evaluate immutable baseline/candidate Formula snapshots with fixed task inputs and independent checks; human review controls promotion and shipping.

**Tech Stack:** Node 20+, TypeScript, Ink, Git/GitHub CLI, existing BYO harness adapters.

**Spec:** `docs/design/distribution-routing-evaluation.md`, refined by the user's approval of the installer → PR delivery → evaluation milestone. The user approved public compiled CLI downloads with private source on 2026-09-29.

## Global constraints

- Atom / Molecule / Formula / Lab retain their meanings. No fifth primitive.
- Bootstrap Assay is observe-only. Approved Formula required before harness execution.
- Preserve provider model settings and permission boundaries; no broader-permission retry.
- No automatic merge, deployment, issue closure or Formula mutation.
- No local UI, automatic model routing or large queue.
- Preserve the original checkout and existing Lab data. New work uses isolated checkouts.
- Missing model identity, usage or cost is unknown, never zero.

## Review focus

- Interrupted/corrupt downloads and concurrent installs must preserve the previous working CLI.
- Installed adapter commands must still work after upgrades, with spaces in paths and without source files.
- Dirty repositories, wrong remotes, stale commits and unexpected files must not silently enter a PR.
- Pending/missing/stale CI must never count as passing commit-specific verification.
- Evaluation comparisons must share task/base inputs and never pass from agent self-report alone.

## 1. Installation-ready CLI and installer

Files: `lab/scripts/build-release.mjs`, `lab/src/cli.tsx`, `lab/src/version.ts`, `lab/src/adapters/cli.ts`, `lab/src/harness-selection.ts`, `scripts/install.sh`, release/installer integration tests, release workflow, install docs.

- [ ] Add packaged-runtime regressions: version/help/JSON work outside source tree; bundled adapters launch through the same artifact; paths with spaces work.
- [ ] Bundle CLI/dependencies into a self-contained JavaScript artifact requiring only Node 20+ (not npm or a source build). Embed version metadata and third-party notices. Keep the existing development build.
- [ ] Add an internal adapter entrypoint; generated bindings use the installed CLI launch path, not source/dist layout assumptions.
- [ ] Build a versioned release manifest with SHA-256; installer verifies downloads, serializes activation, installs atomically and keeps the old version for rollback. Never delete per-project Lab files.
- [ ] Test success, corrupt download, failed smoke check, upgrade and existing-data preservation against a disposable HTTP server and isolated installation directories.
- [ ] Publish only the authorized release artifact/installer; verify the actual one-line command without Forgecell repository authentication. No access tokens in public artifacts.

## 2. Isolated Molecule and reviewed PR delivery

Files: new focused workspace/delivery modules and tests in `lab/src/`, `commands/run.ts`, `cli.tsx`, `types.ts`, `ledger.ts`, `molecule.ts`, adapter instructions, docs.

- [ ] Add explicit isolated execution tied to a repository/base commit and unique branch, retaining source Lab records and leaving original work unchanged.
- [ ] Record workspace, base/result revision, Formula hash and independent check commands/results in the ledger.
- [ ] Add an explicit submission action that reviews/stages the authorized changes, commits, pushes and opens a PR only for the selected Molecule. No merge/deploy/close action.
- [ ] Query checks for the exact PR head SHA; pending, missing and stale checks stay distinct from success. Record links and conclusions.
- [ ] Test wrong repo, dirty/unexpected output, failed checks, duplicate submission/recovery and stale head handling.
- [ ] Create one scoped GitHub dogfood issue and run Forgecell end-to-end in isolation. Attach the resulting PR and record actual checks; leave it open for review.

## 3. Reproducible Formula evaluation

Files: focused evaluation module/command/tests, versioned evaluation cases, ledger additions, learning documentation.

- [ ] Define evaluation input with task/base revision, baseline/candidate Formula hashes, fixed independent acceptance checks and named hypothesis.
- [ ] Refuse mismatched tasks/base revisions and unapproved candidate execution. Run variants in clean isolated checkouts; retain every attempt, including failures and blocked work.
- [ ] Produce a readable paired report with independent correctness/regressions, timing, interventions/retries and actual cost/usage where exposed. Report sample size and unknowns.
- [ ] Use an already approved process lesson only if its exact scope matches; otherwise prepare the concrete mutation and obtain the required human approval before candidate execution.
- [ ] Execute the comparison on matched snapshots, including a held-out task if making a generalization claim. Do not claim optimality or significance from a small sample.
- [ ] Human review controls adoption; no automatic learning application. Publish curated findings without private raw transcripts.

## Completion

- [ ] Full tests/typecheck/build and real packaged install pass; code review addressed.
- [ ] Fresh issue → isolated harness work → PR → exact-commit CI ledger verified.
- [ ] Baseline/candidate evaluation has actual independent evidence and honest limitations.
- [ ] Documentation and reviewable PRs updated; new PRs remain unmerged.

## Execution record

- 2026-09-28: Reused the clean managed worktree on `feat/cli-mvp-delivery` from merged main `262ade7`.
- Ruling: retain Node 20+ as an explicit prerequisite for this initial installer, while removing npm/source-build requirements. Runtime provisioning remains a later friction reduction; do not claim zero prerequisites.
- Added a release bundle with embedded version and an internal adapter entrypoint. Packaging smoke test runs version/help and a real fake-harness subprocess outside the source tree.
- Found Ink's optional developer inspector caused an unresolved package in the bundle; release builds omit that inspector explicitly. Yoga's npm artifact lacked its license notice; include the matching upstream-version notice and fail on missing notices.
- Installer integration test verifies install, upgrade, checksum refusal, failed-smoke refusal, rollback, concurrent-install exclusion, shell paths containing spaces/quotes, and preserved Lab data.
- Generated bundles remain local. No public artifact has been published; public versus invitation-only beta access is awaiting the user's answer.
- Remaining in installation stage: end-to-end installed binding/upgrade test, release manifest publishing integration and public/authorized endpoint verification. Delivery began with isolated execution below; PR submission and evaluation remain unimplemented.

- Added `run --isolated [--base <commit>]`: verifies origin against intake, starts from a resolved immutable local commit, preserves dirty original files, and retains the workspace in the ledger. Both Ink and plain output use the same execution path.
- Verified 87 CLI tests, 2 packaged-release/installer tests, typecheck, build. Isolation integration invokes a real test subprocess and checks both checkout contents and ledger provenance. No live model run is claimed by these tests.
- CI now runs the CLI and packaging checks on Linux/macOS with Node 20/22 and retains bundled artifacts privately in Actions. No release is published by this workflow.
- Added the commit-check collector with explicit named checks and exact PR-head validation. Tests distinguish stale, missing, pending, skipped and failed evidence from success. This collector is not yet connected to a delivery command or ledger; that integration remains required.
- Added `deliver` preview/publish with a content-bound approval digest, exact file scope, isolated branch/origin checks, and a persistent commit record before network publication. Repeating after a failed push resumes the recorded commit; repeated completed publication does not create another PR. Network operations are test doubles so far; local Git commit behavior is real in these tests.
- Added `checks` to persist named-check results for the delivered PR's exact commit in JSON and Markdown ledgers. Updated CLI documentation. Verified 91-test full suite, typecheck, build, both packaging tests, then the additional delivery retry regression independently.
- Remaining delivery verification: review concurrency/crash edges and exact remote identity, run the actual fresh issue and PR flow, attach created PRs, and record real CI. No live PR created during this step.
- 2026-09-29: Moved plans from vendor-specific folders to docs/plans at the user's request. Added runtime/repository-context research and a concrete paired-evaluation protocol. Go is recommended; final runtime selection is pending. No migration code or new learning mutation has been applied.
- Revalidated 93 CLI tests, 2 packaging tests and typecheck. The current delivery prototype also rejects divergent Git push destinations. It remains unreviewed against crash recovery and has not delivered a live issue.
