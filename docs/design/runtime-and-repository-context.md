# Runtime and repository context

Date: 2026-09-29
Status: historical Go migration decision. The Go CLI and native installer are now the only supported Lab implementation; desktop remains deferred. Statements below about preserving the TypeScript CLI describe migration gates at the time, not current operation.

## Recommendation

Use Go for the installed Lab runtime. Python is useful for optional research notebooks and evaluation analysis, but should not become a second required runtime. Preserve the TypeScript CLI and its tests as the compatibility reference until a Go implementation passes the same behavioral contracts.

This recommendation prioritizes one-line installation, explicit process supervision, typed state transitions and a small operational footprint. It is not a measured claim that Go will make model inference, tests or CI faster. Those external operations can dominate elapsed time; collect baseline measurements before claiming a performance win.

| Criterion | Go | Python | Existing TypeScript |
| --- | --- | --- | --- |
| Local CLI distribution | Build executable per supported OS/architecture; installer selects it | uv can manage Python and isolated tools; runtime still exists | Current bundle needs Node 20+ |
| Harness integration | Subprocess/JSON adapters; native SDK not necessary | Good Python SDK access where useful | Existing tested adapters and Ink UI |
| Reliable orchestration | Standard context/process primitives; explicit persistence still required | Feasible with async subprocesses and explicit state | Feasible with current subprocess implementation |
| Migration cost | Rewrite runtime and terminal presentation; preserve protocols/data | Rewrite runtime without removing interpreter packaging | Lowest short-term change cost |
| Best fit here | Recommended core runtime | Optional analysis tooling | Retain CLI temporarily as reference |

Go does not automatically provide durable workflows, idempotency, safe parallelism or deterministic model output. Those are application contracts in every language. Avoid adding an orchestration framework or database solely because a language changed. Initially preserve versioned YAML Formulas and JSON ledgers; add a journal with durable operation intent/result records and exclusive writers. Any later persistence-engine change needs crash/recovery tests and a compatibility migration.

## Language-neutral integration

- Codex: existing CLI JSON adapter first; evaluate App Server's versioned JSON-RPC interface when approvals/session events require it. Detect actual installed capabilities, not presumed latest documentation behavior.
- Claude Code: headless JSON/stream JSON adapter. Python/TypeScript SDKs are optional adapter choices, not the product's foundation.
- Cursor: headless structured output adapter; preserve user permission mode. Never retry with broader permissions.
- Git/GitHub: explicit argv and typed API responses, exact repository and commit identities, no shell interpolation of ticket text.
- Unsupported models, versions, usage or cost remain unknown. Authentication discovery never spends tokens or changes settings.

## Onboarding establishes context; intake specializes it

No fifth primitive is introduced. Repository evidence belongs to the Lab and approved Formula. Each Molecule freezes the relevant evidence and issue snapshot in its ledger. The following describes records, not new product entities.

Onboarding reads Git remotes and root, workspaces/package manifests, build/test scripts, CI workflows, CODEOWNERS and readable GitHub rules. It proposes a human-reviewable Formula containing:

1. Repository identity, local root, component paths and command working directories.
2. Approved default harness, permissions, timeout and resource budgets.
3. Test/build commands, required check identities and owners/review policy.
4. Branch/base selection and PR publication rules; merge/deploy remain explicit gates.
5. Available deployment/health evidence, environment identity and rollback procedure. If absent, say unavailable; no invented monitoring or Datadog integration.
6. Evidence source, revision, retrieval time and unresolved questions for every consequential inference.

An issue trigger then binds to an exact issue body/version, repository and base commit. Intake derives acceptance criteria, relevant components, likely dependency paths, allowed write scope, independent acceptance checks, risk areas and questions. Model-proposed scope must cite files/symbols or recorded repo evidence; the deterministic validator enforces the approved bounds. Missing scope, conflicting requirements or absent acceptance evidence stop coding with a blocked Molecule and concrete questions.

Examples: which service owns an endpoint; whether API compatibility may change; whether a migration may modify existing data; what behavior establishes success. Draft questions locally and record them. Posting back to the issue creator requires a configured, approved document/write-back action and permission; post once using a durable idempotency key, retain the comment URL, and resume only against the answered issue snapshot. Do not silently post during onboarding.

## Execution contract

- Freeze Formula hash, repository/base, issue snapshot, harness/version, resolved model when exposed, policy revision and independent check definitions.
- Use explicit states and legal transitions; unknown evidence cannot become success.
- Each external action has a persisted intent, scoped identity and outcome. On restart reconcile remote state before retrying a push, PR or comment. Timeouts mean uncertain outcome until reconciled, not permission to duplicate writes.
- Scope enforcement must include filesystem/process permissions where the harness supports them. A prompt and post-run diff check alone cannot prevent an out-of-scope write; describe the actual enforcement level honestly.
- Assess blast radius through dependency evidence, interface changes, schemas, configuration, permissions and deployment targets. Record unresolved dependencies; do not present a model's risk score as a proof.
- Revalidate changed scope before publish. Tests/CI attach to an exact revision. Review approval becomes stale if the reviewed revision changes.
- The MVP ends at a reviewable PR with exact checks. Production merge/deploy/health/rollback can be discovered and described now, but cannot execute without later explicit authorization.

## Parallelism

A Formula may eventually express dependencies between Atoms and bounded resource/write scopes. Begin sequentially. Read-only analysis or independent tests may run concurrently only with declared dependencies and resource limits. Concurrent code changes need isolated workspaces and disjoint ownership; shared schemas, lockfiles, interfaces or uncertain dependencies force serialization. Integrate deterministically and rerun checks on the combined commit. Keep one default harness for this MVP; planning parallel work does not introduce automatic routing or a multi-agent mesh.

## Learning and evaluation

Record observations, hypotheses, intervention, comparison and limitations. Learning proposes a process-changing Formula diff with an expected effect and independent acceptance plan; it cannot mutate the codebase under the name of learning. Human approval binds to the exact candidate hash.

Compare baseline/candidate on identical issue and base snapshots, with fixed external acceptance checks. Keep failed/blocked attempts; report correctness and regressions before speed, interventions, retries and real cost when available. Repetition and held-out tasks are needed before generalization. Separate Lab startup/scheduler overhead from model/test/network time. No language selection proves improvement.

## Migration gates

1. Freeze and export fixtures for Formula/ledger parsing, trust decisions, adapter messages, intake failures, Unicode streaming, cancellation, permission denial and delivery recovery.
2. Implement Go doctor/init and read-only ledger inspection while preserving existing files. Do not overwrite user Formulas or credentials.
3. Port isolated execution and adapters. Verify Codex, Claude and Cursor against installed capabilities with real opt-in runs.
4. Port delivery, check provenance and evaluation; run identical fixtures and crash tests. Resolve crash gaps in the current prototype rather than treating it as flawless.
5. Build versioned artifacts in CI, verify checksums/signatures and upgrade/rollback preservation, and test installation without Node/Python or private repo access. Users still need their selected harness and Git/GitHub tools.
6. Only switch the default installer after parity and live dogfood evidence. Remove obsolete Node release paths after the replacement works. The user approved public compiled CLI downloads with private source on 2026-09-29.

## Repository hygiene

Keep product design in docs/design, implementation plans in docs/plans, research findings in docs/research. No committed agent-tool workspaces or vendor-specific planning directories. Harness detection can read user configuration without copying it into the repository. The ignored .forgecell directory is the product's own runtime data, not agent-tool scaffolding. Do not erase user credentials or local ledgers as cleanup.

## Sources inspected

- Go process supervision and argv behavior: https://pkg.go.dev/os/exec
- Go target environments: https://go.dev/doc/install/source#environment
- uv Python provisioning: https://docs.astral.sh/uv/guides/install-python/
- uv isolated tool installation: https://docs.astral.sh/uv/guides/tools/
- Codex App Server protocol: https://learn.chatgpt.com/docs/app-server
- Claude Code structured execution: https://code.claude.com/docs/en/headless
- Claude Agent SDK overview: https://code.claude.com/docs/en/agent-sdk/overview
- Cursor headless execution: https://cursor.com/docs/cli/headless

These sources establish available integration mechanisms, not measured Forgecell performance or parity. No comparative runtime benchmark has been executed yet.
