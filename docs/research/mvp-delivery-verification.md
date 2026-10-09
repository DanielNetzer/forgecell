# MVP delivery verification — 2026-09-29

This record covers the installer → dogfood PR → paired-evaluation milestone. It does not claim complete migration parity, a stable production release, autonomous production delivery or a proven Formula improvement.

## Public CLI; private source

The user approved public CLI downloads while retaining private source. [Download repository](https://github.com/DanielNetzer/forgecell-releases) is public; the historical source repository ID1390966758 was private under `DanielNetzer/forgecell` (planned historical name: `DanielNetzer/forgecell-private-history`; access required), separate from the current core identity. Published [0.2.0-preview.1](https://github.com/DanielNetzer/forgecell-releases/releases/tag/0.2.0-preview.1) is explicitly a prerelease.

Binaries were built from clean source commit `b0754ed3f5b181b9fd77af40b5ee36e90c645758` using Go 1.27.1 with trimpath and stripped symbols. All six [CI jobs — private historical evidence; access required](https://github.com/DanielNetzer/forgecell-private-history/actions/runs/36514161359) passed, including native installer lifecycle tests on macOS and Linux. Four packages are cross-compiled; execution coverage is CI host architectures and the local macOS arm64 host.

Only four compiled executables, their four checksums, dependency notices and the rendered installer were uploaded. All ten GitHub asset digests matched the reviewed local files before publication. No private source archive, runtime ledger, credentials or evaluation trace was uploaded. An independent focused review found no blocking publication issue.

## Hosted installer verification

The exact documented curl-to-sh command installed `0.2.0-preview.1` anonymously in a fresh temporary home, with a restricted PATH containing download/shell utilities but no Node, Go, Git or gh. Its environment included no GitHub authentication variables. The installed CLI returned the expected version.

- Repeating the public install succeeded and preserved a pre-existing ledger sentinel.
- Anonymous downloads of all four architecture artifacts matched their published SHA-256 files.
- Installing the public preview over a separate local verification release succeeded; rollback restored that prior verified executable without requiring installation environment variables.
- The pre-publication local HTTPS integration also rejected a corrupted upgrade without replacing the current CLI, and verified notices/manifest hashes.
- Read-only live doctor probes verified installed Codex, Claude Code and Cursor capabilities/authentication, plus Git and gh. This does not claim new live coding runs with all providers.

The download endpoint uses version-pinned installers. Checksums share the trusted HTTPS endpoint; no independent signing authority is claimed. The prior verification version used for rollback testing was local only and was not published.

## Dogfood and evaluation

[Issue #9 — private historical evidence; access required](https://github.com/DanielNetzer/forgecell-private-history/issues/9) was run through Forgecell using Codex in an isolated worktree. [PR #11 — private historical evidence; access required](https://github.com/DanielNetzer/forgecell-private-history/pull/11) remains open at `c3395418284d97f241495410ea8c75b7bd2440a4`, with exact-commit checks recorded. [Detailed ledger and delivery evidence](../dogfood/issue-9-ledger.md) retains original Atom outcomes and separate delivery receipts. The Issue remains open. Neither PR #10 nor #11 was merged.

The paired evaluation uses one human-approved process-only Formula change, the same frozen issue/base and separate outputs. Both variants passed fresh independent acceptance, 84 original regression tests, typecheck and build. Both encountered IPC EPERM and recovered with the fallback: no observed correctness/recovery benefit. Timing and provider-reported tokens are recorded, actual cost and hidden retries remain unknown. Frozen approval/task/checker and both patch hashes were revalidated. No new Formula mutation was applied. The original evaluation bundle is preserved in the private historical repository (access required) and omitted from the public snapshot.

## Boundaries retained

One default coding harness, explicitly separate meta binding, human Formula approval, the four locked primitives and no local UI, routing layer or large queue system. Installer distribution does not grant automatic merge/deploy/issue-closure authority. Remaining broader onboarding/context-policy and migration parity work stays on the product roadmap rather than being represented as completed by this milestone.
