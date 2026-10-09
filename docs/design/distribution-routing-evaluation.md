# Distribution, harness routing and evaluation

Supersession — 2026-10-09: the remaining core source is [MIT licensed](../../LICENSE).
Earlier private-source distribution decisions below record history; repository visibility
and historical exposure remain separate decisions. This document does not authorize publication.

Status: historical proposal, not current installation or architecture guidance. The Node Lab and its installers have been retired. See [native distribution](native-cli-distribution.md) for the current installer and [Runtime and repository context](runtime-and-repository-context.md) for the Go decision. Other proposals remain subject to their stated scope.
Reviewed against merged main `ecdbdf8` on 2026-09-28.

## Intent

Let an individual install a local Lab without repository access, run tickets using their
available harnesses/models, and learn which process changes improve outcomes. Keep the
four primitives: Lab runs Molecules; a Formula arranges Atoms. An evaluation is evidence
about those executions, not a fifth primitive. No local UI or team platform is proposed.

## Repository findings

These findings describe the retired Node Lab at the review date.

- `lab/`: TypeScript/Ink runtime, commands, adapters, discovery, execution, learning and tests.
- `scripts/install.sh`: builds from source, requires Node/npm and access to a private checkout.
- `.github/workflows/release.yml`: publishes `dist` plus package manifests to the private
  repository's GitHub Releases. This is a Node package archive, not a standalone executable.
- `docs/`: product ontology, install guidance, learning contract and curated dogfood evidence.
- `.forgecell/` is ignored per-repository runtime state. The main checkout also has untracked
  `work/` containing test fixtures; preserve it, keep it out of source/release artifacts.

Keep the existing `lab/`, `scripts/`, `docs/` boundaries. Do not introduce a large
workspace/package split just to rename directories. As these capabilities are implemented,
extract internal `execution/`, `onboarding/`, `learning/` and `evaluation/` modules inside
`lab/src/`; leave CLI commands and Ink as thin callers. Add versioned evaluation cases outside
production source. Move code when it gains a real responsibility, not in a sweeping rename.

## One default, optional Atom-specific execution

At the review date, every `harness` Atom executes `Formula.harness.command`; an Atom's binding does not
select a different coding adapter. The learn Atom already has its own command. One coding
binding simplified onboarding and made permission behavior testable, but is an implementation
limit, not a product principle.

Proposed resolution: an explicit Atom execution choice overrides a Formula default. The Lab
inventories installed/authenticated capabilities; the Formula owns the approved workflow and
its choices. Record the resolved harness/model per Atom in the Molecule ledger. Preserve old
Formulas by treating their existing binding as the default.

Most Atoms need no LLM: issue intake, test commands, human gates and issue write-back should
remain deterministic where possible. For reasoning Atoms, selecting a harness and selecting a
model are different decisions. Only expose a model choice if that adapter verifies support;
otherwise retain the harness default and record model identity as unknown when unavailable.
Never rewrite global provider settings or silently fall back to broader permissions. Retain
separate coding and meta-harness invocations even if they happen to use the same provider.

A routing change is a Formula change: explain its scope, expected benefit, cost/permission
implications and evidence, then require human approval. Onboarding can recommend a ready
default immediately. It cannot infer the best model from authentication or a config directory.
Optional calibration must be disclosed, budgeted and explicitly initiated; detection itself
remains observe-only.

## Private-beta one-line CLI installation

User clarification: the deliverable is one terminal installation command for the CLI, not a
desktop application, downloadable app or platform-binary selection screen. Standalone
executable compilation is not a requirement for this iteration.

Target experience (placeholder URL, not a live endpoint):

```sh
curl -fsSL <official-install-url> | sh
forgecell doctor
forgecell init
```

Keep the source repository private. Private CI builds and tests versioned CLI release
artifacts; a separate download endpoint serves an installer, release manifest, integrity
metadata and only those artifacts. Users do not clone Forgecell or need access to its GitHub
repository. For an invitation-only beta, use a scoped or expiring artifact-download link;
never distribute a GitHub repository token. Whether artifact downloads are publicly available
or invite-gated remains a product decision.

Prefer the existing Node runtime for the first release. Package built JavaScript and runtime
dependencies in CI, so the installer does not compile TypeScript or run npm install in a
source checkout. Reuse a compatible Node when present; the runtime strategy for machines
without Node must be made explicit before release (provision a verified private runtime, or
state Node as a prerequisite). A bundled runtime can remain an internal installation detail.
No separate OS choice should be needed: the installer detects support and gives an actionable
message on unsupported hosts. Test the actual installation on every supported host before
claiming compatibility.

Blockers at the review date:

- `scripts/install.sh` fetches/builds private source; replace that path for end users with
  a versioned artifact manifest/download and integrity verification.
- The release workflow produces dist plus manifests, leaving production dependencies to
  the user; publish an installation-ready runtime layout instead.
- Generated adapter commands embed absolute installation paths. Keep a stable launch path
  across upgrades or resolve adapter locations at execution, so Formulas survive updates.
- Install into a user-owned versioned directory, expose `forgecell` on PATH, and switch versions
  atomically only after a smoke check. Retain rollback and keep `.forgecell` data separate.
- Verify terminal and JSON onboarding, all adapters, interruption, upgrades and removal from
  an unrelated repository with no source checkout and no GitHub access to Forgecell itself.

Git, GitHub authentication and the user's harness/model access remain separate dependencies;
`doctor` should identify them clearly. The CLI must not embed credentials. Distributing built
JavaScript is compatible with a private repository but does not make the shipped implementation
secret. Packaging into a standalone executable can be evaluated later if runtime installation
friction justifies it; it is not a desktop-app direction.

## Evaluation before optimization

Our repeated issue #6 runs establish adapter behavior against an already-resolved issue.
They are not evidence that one harness, model or Formula produces better software.
At the review date, ledgers preserved Formula hashes and Atom timings/outcomes, but lacked a complete,
normalized evaluation record. Recent GitHub Actions history is not evidence for the run's
exact commit. Capture independent, commit-bound checks before ranking alternatives.

Record for each evaluated Molecule:

- Task snapshot, repository/base commit, resulting commit/diff, Formula hash and changed field.
- Harness executable/version, requested and resolved model when exposed, relevant permission
  mode, check environment and task category. Unavailable identity/usage stays unknown.
- Independent acceptance and regression results tied to the result; agent self-report is
  supporting evidence, never the sole correctness grade.
- Retries, active execution time versus queue/human wait time, actual usage/cost when available,
  human corrections and later rework/reverts. Missing cost is not zero.
- Policy failures: permission escalation, unauthorized writes or shipping, and hidden/skipped
  checks. A faster run that violates these constraints is not an improvement.

Use a scorecard rather than one opaque score: correctness/regressions first; then human
intervention, reliability, cost and time. Define denominators and distinguish blocked,
failed, skipped and unknown results rather than dropping inconvenient runs.

## Test a Formula mutation as a hypothesis

1. State one process hypothesis and the expected observable change before running it.
2. Freeze baseline and candidate Formulas; initially change one dimension (instructions,
   harness or model), so routing and workflow effects are not confounded.
3. Run both against the same task/base snapshots in separate clean checkouts. Randomize order,
   repeat to expose model variability, and keep permissions/check environments comparable.
4. Evaluate with independent tests and a documented human rubric where tests are insufficient.
   Use withheld tasks for confirmation; avoid teaching recipes to the evaluation examples.
5. Report paired outcomes, sample size, variability, cost coverage and uncertainty. Keep
   inconclusive as a valid result; no arbitrary claim of statistical significance from a tiny set.
6. Review the evidence before promoting the candidate. Human approval remains required.
   On real tickets, monitor regressions and propose rollback if needed; no silent self-mutation.

Example hypothesis: for sandbox IPC failure in the standard test runner, a known compatible
runner reduces blocked validation while preserving coverage and permissions. Compare both
commands' coverage/outcomes and time; simply observing a successful exit is insufficient.

Only after comparable evidence exists should onboarding recommend different harness/model
choices for task categories or specific Atoms. Recommendations should explain evidence and
uncertainty and respect user budget/privacy constraints. Logs remain local by default; any
shared benchmark or telemetry needs explicit opt-in and redaction.

## Suggested order

1. Prove and release the one-line private-beta CLI installer without source-repository access.
2. Add commit-bound checks and normalized evaluation evidence.
3. Implement paired Formula evaluation and human-reviewed promotion/rollback proposals.
4. Add Atom-specific routing using that evidence; retain the simple default path.

Open decisions at the review date: public artifact downloads versus an invitation gate, how the installer handles
a missing Node runtime, and the initial quality/cost/time tradeoff for evaluations. No routing schema
or new CLI commands are approved by this document.
