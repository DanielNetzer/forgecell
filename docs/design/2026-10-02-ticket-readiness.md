# Ticket readiness before coding

Supersession — 2026-10-09: the remaining core source is [MIT licensed](../../LICENSE).
The website and its artwork have been separated into a private repository and removed
from the core tree. Earlier private-source decisions below record history;
repository visibility and historical exposure remain separate decisions.

Status: revised on 2026-10-03 after independent GPT-6 Sol devil’s-advocate review and user-requested revision. Approved design; implementation and dogfood in progress. See [implementation status](../ticket-readiness.md). MCP Events and notification integrations are deferred.

## Outcome and scope

An individual engineer points their installed Lab at a GitHub Issue. Before coding, Forgecell shows where work belongs, what success means, which evidence and checks will assess it, likely impact and unresolved questions. A clear ticket yields a pending human-review plan. An unclear ticket yields specific questions and no coding invocation. Approval is bound to the exact inputs and process; it is not permission to merge or deploy.

Keep Atom (step), Molecule (ticket plus ledger), Formula (durable recipe), Lab (runtime). The intake Atom records ticket analysis; a separate gate Atom instance records scope approval. Both belong to the same ticket Molecule, not a separate analysis Molecule or a fifth primitive. Keep one default BYO coding harness, current provider/model settings and a separately bound meta harness for Formula learning. No local GUI, queue, routing, automatic issue comments, MCP Events or notification transport.

The Formula orders `intake → scope gate → harness → check → review gate → ship → document`. Scope gate and review gate are distinct instances of the existing gate Atom type, with stable IDs and explicit purposes. Existing Formulas require an explicitly reviewed change to introduce the pre-coding gate; the engine must not silently patch their YAML. Reaching the scope gate pauses before coding; reaching the review gate pauses before delivery. Gate position and purpose must be validated, not inferred from display names.

## Existing implementation and gaps

At base 6274e59211f4c66f33f4b4403fcfc465b0a5ca7b:

- onboarding/repository.go captures committed component manifests, hashes, suggested commands, workflow names and policy filenames, but does not specialize evidence to a ticket.
- molecule/run.go fetches the Issue in an isolated checkout, then immediately invokes the coding harness. Its check Atom does not execute independent checks.
- delivery validates exact files/tree/base/destination at publication time. That scope is not bound to a pre-coding decision.
- Provider adapters distinguish coding from Formula-learning requests. Ticket analysis needs an explicit read-only protocol; it must never fall through to coding mode.

## Approach choice

Use model-assisted interpretation with deterministic validation and human approval. A template-only parser would be predictable but would reject normal natural-language Issues or accept filled-in yet contradictory fields. Letting the model approve itself would hide uncertainty and defeat the review boundary. The chosen approach lets the existing provider propose and explain; the runtime validates identities, paths, evidence and transitions; the human approves scope and commands.

## User flow

1. `forgecell init` discovers committed repository evidence and proposes a Formula as today. It additionally records evidence for package relationships, check commands and review requirements, including unknowns. Detection does not call a model.
2. `forgecell run 123` creates a Molecule and performs intake. It snapshots the Issue and repository/base, gathers bounded context, and asks the selected provider for read-only analysis. No coding request is sent.
3. If evidence is insufficient or conflicting, print precise questions with reasons and record a blocked intake. The remaining Atoms are skipped. Do not post to GitHub. After the Issue is clarified, a new run records new input identities.
4. Otherwise show the proposed file scope, acceptance criteria, checks with working directories/timeouts, likely impact with citations, unknowns, and approval digest. The intake Atom completes its assessment and the scope gate waits for approval. The gate, not intake, owns the approval decision and waiting time.
5. `forgecell run 123 --approve DIGEST` locates that exact pending Molecule in the selected Lab, refetches Issue inputs and validates repository/base/Formula/evidence and workspace identity. It resumes only after persisting the approval of that plan revision. No silent regeneration or broadening is allowed. A rejected plan may be revised within the same Molecule through the explicit amendment flow below.
6. The coding harness receives the approved plan and exact restrictions. The check Atom runs approved commands separately from the coding process, retains their evidence categories and exact outcomes, and audits scope. Separate execution alone does not establish an independent acceptance oracle. The existing final human gate still applies.
7. `deliver` retains its existing exact preview digest and explicit file list. It also requires the admitted scope and passing required checks for the exact current content. Missing independent acceptance evidence must be visible in the final review and explicitly acknowledged; generic test success must not be labelled proof that the ticket is solved. Approval before coding never authorizes publication by itself.

Terminal output leads with issue, readiness and next action. JSON includes the same facts for agent callers. Empty/missing data is not a successful assessment. Old ledgers remain readable; new execution must not offer a legacy readiness bypass.

## Scope amendments within the same Molecule

Implementation can reveal a necessary shared type, generated client, migration or lockfile outside the predicted scope. Do not discard the Molecule or paid work solely because the first scope was incomplete.

- The harness may return a structured scope-change request. Stop coding, capture its current output tree and retain the workspace. Show the requested paths, reason, changed checks and additional impact.
- Create an immutable successor plan revision and reopen the scope gate. Bind the new digest to the previous revision, the paused workspace tree and all original input identities. Display a focused before/after diff as well as the full proposed scope. No continuation until explicit approval.
- Before continuation, revalidate the exact captured workspace, issue/base/Formula and approved revision. A mismatch invalidates approval. Resume from retained work, never silently restart or broaden scope. Record a new harness attempt under the same Molecule and preserve prior outcomes.
- If an out-of-scope write already occurred, record a scope violation rather than treating a later approval as retroactive permission. Preserve the diff, block delivery, and require explicit review of those exact existing changes in addition to the new scope. The violation remains in the ledger; approval cannot erase it. Rerun all affected required checks and invalidate prior verification.
- Denying an amendment leaves the run stopped and work intact. A person may explicitly abandon it or restore a reviewed state; Forgecell does not delete unexpected changes automatically. An amended plan changes this ticket’s scope, not the durable Formula. Changes to issue semantics, base or Formula still require refreshed intake and approval rather than reuse of an old digest.

## Evidence and analysis contract

Repository context is tied to a resolved commit and GitHub repository identity. Read committed regular files with bounded count/size, never arbitrary host paths or secrets. Include manifest paths/hashes, package roots, declared local dependencies where parseable, scripts and their provenance, CI workflow content/hashes, CODEOWNERS and CONTRIBUTING evidence. Do not execute discovered scripts during discovery. Unsupported dependency/workflow formats and unreadable branch rules remain explicit unknowns.

Fetch readable GitHub review/branch rules against the chosen target branch when available. Record retrieval time, source and digest; permissions failures become unknowns, never claims of no protection. Local policy files are evidence of expectations, not proof of enforced GitHub rules. Required remote check names remain distinct from local commands. Remote-policy availability is advisory during intake: unavailable rules do not by themselves block local coding. Any known policy the user chooses as a mandatory coding constraint becomes explicit in the approved plan. At delivery, refresh remote-policy evidence; if a configured publication requirement cannot be verified, stop publication and explain the missing evidence. Unknown remote protection is never reported as compliance.

Ticket snapshot includes repository, issue number/URL, state, title, body and update revision/time. Comments are not implicitly trusted or consumed in this iteration; the UI explains that clarifications belong in the Issue body. Closed Issues and repository mismatches stop intake.

Analysis returns structured acceptance criteria, proposed exact file paths (new files permitted with evidence for their parent component), check argv/working directory/timeout, relevant components, cited facts, inferred impacts, unknowns and questions. Evidence references must resolve to the frozen inputs. Model assertions are always labelled assessments. The provider must return strict structured output; invalid, truncated or unsupported output blocks intake.

The read-only analysis request uses the same selected provider/model settings, but disables write/action capabilities. It is not the meta harness and cannot propose or apply Formula mutations. Native adapters must explicitly support this request kind. Custom BYO bindings may declare an explicit analysis subcommand/protocol on the same harness binding, including structured output and tested permission controls. A declaration is not proof of isolation: validate supported controls before invocation. Unsupported custom bindings remain preserved and inspectable, but new readiness runs stop with a capability explanation and setup guidance. Never invoke an arbitrary coding command as analysis, substitute another provider, or claim generic BYO compatibility until this path is tested. This does not introduce a second coding harness or reuse the Formula-learning meta harness.

A model can flag semantic ambiguity; deterministic checks require concrete nonempty acceptance criteria, nonempty bounded write scope, at least one relevant executable check, valid provenance, and no unresolved blocking behavioral question. Check relevance is a reviewed assessment, not something JSON shape alone can prove. This cannot prove all natural-language ambiguity has been detected. Human review is part of the contract, not a model confidence threshold.

## Approval identity and persistence

Canonical approval payload includes schema version, Molecule ID, plan revision/parent digest, paused workspace tree for amendments, repository/target branch/base commit, exact Issue snapshot hash, exact Formula YAML hash, repository evidence digest, policy observations used in the decision, provider binding, accepted criteria, ordered checks and their evidence categories, setup/output allowances, scope, impacts, questions and unknowns. Hash all consequential fields; display the digest and exact values being approved.

Store the immutable proposal plus subsequent decisions in the Molecule ledger. Keep terminal state separate from run phase so a waiting scope gate does not falsely claim execution finished. Record repeated scope-gate decisions and harness attempts as distinct append-only observations rather than overwriting a single Atom status/time interval. Persist approval intent/outcome before coding and hold a per-Molecule exclusive lock. A second invocation cannot start duplicate coding. After interruption, reconcile recorded state; never automatically repeat a coding invocation whose outcome is unknown.

Before resuming, validate current issue and Formula, repository origins/base selection, the pending workspace HEAD and expected tree (clean on initial approval; captured on amendment), consequential approved constraints and approval digest. A changed identity, scope, command or approved workspace produces a stale decision and no coding. Retrieval timestamps, unavailable advisory remote policy and unrelated GitHub metadata must not invalidate otherwise unchanged input content. Preserve those changes as observations; recheck publication requirements at delivery. Never rewrite an old proposal to match new evidence.

Local ledger files are trusted local state, not a cryptographic authorization service. Hashes detect changes against approved snapshots but do not prevent a user with full filesystem control from forging every record.

## Scope and checks

Approve exact normalized repository-relative paths, not shell globs or directory-wide wildcards. Reject absolute paths, traversal, Git metadata, symlink traversal, nested repositories/submodules and duplicate/ambiguous paths. Renames touch both old and new paths. Audit tracked changes and non-allowanced untracked/ignored writes. Handle ordinary dependency/build outputs through explicit, bounded artifact roots tied to approved setup/check commands, such as a component’s `node_modules` or `dist`. These are declared audit exceptions, not source-write permission: reject root-wide, traversal, symlink or tracked-source-overlapping allowances, never stage their contents for delivery, and do not reuse them for independent verification. New source files still require exact-path approval even if ignored by Git. Record allowances and their limits in the approval payload. Do not promise observation of transient writes or host-wide filesystem activity from a Git/worktree audit.

Use the provider sandbox where supported and do not broaden permissions on failure. A provider's workspace-write sandbox is not generally a per-file allowlist. The guaranteed repository-tree boundary is: observed changes outside approved source paths and bounded artifact allowances are retained as evidence and block verification/delivery. Do not claim that all such writes are prevented. No automatic deletion or reset of unexpected changes.

Commands are approved argv arrays, working directories and bounded timeouts, never ticket text interpolated into shell. Discovery/model-proposed commands are untrusted until reviewed. Checks run in a separate verification worktree reconstructed from the captured scoped output tree, with explicit dependency setup and immutable check definitions. Retain baseline check sources where needed so a harness cannot weaken acceptance by editing the test. Check commands execute code and require the same permission/resource controls as other local execution.

Checks record exact input tree, commands, exit codes, duration and captured output. Failure, timeout, cancellation, skipped/unavailable commands and source mutation during checks cannot become pass. Delivery compares the current complete output tree with verified evidence, enforces all changed paths within approved scope, and refuses stale checks. Hooks that change the reviewed tree remain rejected by existing delivery logic. Remote CI is still collected against the exact published commit; local passes are not remote CI success.

## Evidence categories and process containment

Keep three distinct check categories in the plan, ledger and final review:

1. Existing regression checks: preserve evidence about previously covered behavior.
2. Independent acceptance: human-supplied or separately reviewed checks/observations tied to the ticket’s requested behavior, with provenance and frozen definitions. Model-proposed criteria become independently reviewed only after substantive human review; merely running the model’s selected command does not create independent evidence.
3. Candidate tests: tests added or changed during coding, useful supporting evidence but not independent proof.

Report which categories exist and their individual results. `go test ./...` may pass before the requested feature exists. Where feasible, run a focused acceptance check against the original base and record the expected failure before coding; otherwise explain the limitation. If no independent acceptance evidence exists, label behavioral acceptance unverified and require explicit human review at delivery. Failed required checks always block delivery; missing evidence cannot be silently downgraded to an optional check. Avoid an aggregate “ticket correct” flag derived solely from command exit codes.

Separate analysis containment from check execution:

- Analysis receives bounded collected evidence in a temporary input directory, no coding workspace writes, and provider controls disabling action-capable tools/MCP. Preserve the selected model/authentication through documented provider mechanisms. If authenticated configuration cannot be preserved while action permissions are constrained, surface the limitation rather than copying credentials into evidence or widening permissions.
- Setup/check processes use a separate verification checkout, a temporary HOME, a reviewed environment allowlist and bounded execution. Never inherit the whole host environment by default. Approve any required credential forwarding explicitly, and keep secrets out of ledgers. A changed package script or build config is executable behavior, even if argv stayed `npm test`. Bind approval to the relevant script/config definitions; before executing checks, compare the captured candidate definitions and require an amendment for changes to the approved command behavior. Source code under test remains executable untrusted content; this check is not a general containment guarantee.
- Record network policy separately for dependency setup and checks. Use enforceable controls when available; otherwise disclose that network/host access remains possible. A temporary HOME or filtered environment is hardening, not an OS sandbox. Do not claim credential isolation or prevention of external actions based on a later tree audit.

For this milestone, implement provenance, accurate categories, approved commands and clean verification workspaces. Avoid a universal multi-language acceptance framework or mandatory duplicated full suites. Run a protected regression view when candidate edits would change the protected checks, plus the full candidate view; reuse one execution only when its content and check definitions satisfy both views. Record the actual tree per observation. The clear dogfood case must include independent acceptance; its success cannot generalize to tickets without it.

## Compatibility and module boundaries

Keep persisted v0 ledgers readable with optional versioned readiness/verification extensions. New code must require readiness for new runs and fail closed for legacy delivery lacking this evidence; preserve read-only inspection and historical receipts. Document the transition rather than silently fabricating approval.

A shared intake/readiness package owns input snapshots, canonical validation and decisions without importing molecule. Onboarding shares repository evidence collection. Molecule owns lifecycle/ledger integration. Harness owns explicit read-only analysis adapters. Verification owns fresh-checkout execution and captured trees, reusing proven evaluation machinery where it does not depend on paired-trial orchestration. Delivery consumes ledger evidence and validates it again. CLI only parses and presents.

Existing paired-evaluation tests must use an explicit controlled approval fixture rather than a hidden execution bypass. Learning continues to read finished Molecules; pending intake or a waiting scope gate is not finished evidence. Blocked ledgers may explain why work stopped but must not claim successful coding.

## Verification and acceptance

Automated coverage must include clear and ambiguous tickets; missing evidence; malformed provider JSON; exact provider permission arguments; changed Issue/Formula/base/mandatory constraints; unavailable advisory policy without a false coding veto; wrong repo or closed Issue; concurrent approval; interrupted approval/coding; unusual filenames and symlinks; rename/out-of-scope and ignored writes; failing/missing/timed-out checks; check dependency contamination; weakened tests; changes after verification; scope amendment approval/denial and workspace drift; retained violations; build-output allowances; custom analysis capability failures; environment/credential forwarding and declared containment limits; and delivery attempts without readiness. Assert zero coding calls for every unapproved, unclear or stale case. Avoid testing only CLI strings.

Dogfood through a locally installed candidate build on two explicitly scoped GitHub Issues:

- Clear case: analysis → human-reviewed approval → isolated coding → independent checks → final human gate → separately approved draft PR → exact-commit CI. Retain scope, ledger, verification and delivery evidence. Exercise an amendment without losing the Molecule history, and demonstrate that failed required checks or unapproved scope changes block delivery.
- Ambiguous case: missing behavioral decision produces specific useful questions and blocked intake; zero coding invocation, no PR/comment/source changes. Retain exact input and ledger.

Use the same installed executable for both. Do not publish a replacement public preview until this flow passes review and tests. Do not merge either implementation/dogfood PR, post clarification messages, deploy ticket changes or mutate a Formula without the corresponding explicit approval. Keep source private and public packages compiled-only.

Completion means the new flow and both live cases are verified, not merely that a plan was saved or fake-provider tests passed. Report unknown provider cost/model metadata honestly. Measure analysis duration, provider invocations, approval interactions, setup/check duration and available usage for both dogfood cases. Include the overhead for a small ticket; do not claim simpler onboarding or greater efficiency without comparing observed friction.

## Compatibility audit: implementation requirements

Inspected `harness/protocol.go` and `evaluation/verify.go` on the current branch before implementation:

- `BuildInvocation` currently accepts only `molecule` and `formula-improvement`; add a distinct ticket-analysis contract and strict result schema. Unknown kinds must continue to fail closed. Do not relabel analysis as Formula learning or use coding prompts for analysis.
- Read-only filesystem access is not proof that remote tools cannot take actions. Analysis must disable action-capable tools/MCP integrations through verified provider controls, or refuse analysis with an explicit capability limitation. Pass bounded collected context as data. Test each provider's actual invocation contract and failure paths rather than claiming that a read-only prompt alone enforces this.
- Existing verification preserves `OriginalCheckFiles` by omitting their candidate edits. That protects baseline regression checks, but it does not execute newly added tests in those same files. The ticket flow must distinguish protected baseline/independent acceptance from full candidate tests, using separate executions when their input trees differ. Every required group must pass; new tests do not substitute for frozen acceptance evidence. Record the exact reconstructed tree used for each group alongside the candidate output tree, since they can differ intentionally.
- Existing verification types import Molecule records. Extract shared tree reconstruction/command observation below both evaluation and molecule before reusing them, to avoid an import cycle. Keep trial orchestration and human readiness decisions separate.
- These are requirements discovered from code inspection, not completed provider-capability verification or implementation claims.

## Review disposition and deferred work

Accepted from the independent review: same-Molecule scope amendments; a distinct pre-coding gate; honest acceptance/regression/candidate evidence; separate containment contracts; advisory policy availability at intake; bounded build-output exceptions; explicit custom-binding analysis capability; and measured friction.

Deferred: MCP Events, remote notification/subscription plumbing, automatic clarification comments, a mandatory branch-policy service, a universal acceptance-test framework, automatic routing and a new local UI. The CLI and ledger remain sufficient to inspect and act on pending decisions. This revision changes the design only and does not imply user approval of any future run, Formula mutation or implementation plan.
