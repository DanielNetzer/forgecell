# Issue 9: first live Go Molecule

This is curated evidence from a local Lab ledger, not a replacement for that ledger or proof that the migration is complete.

- Intake: [GitHub issue #9](https://github.com/DanielNetzer/forgecell/issues/9), extract and test the repository hygiene guard.
- Base: `93d7f8c13be64f2ba2971f35b7fa2c1b401b0c92`.
- Molecule: `mol-9-2a6a94d7ab38d4d783b270c8`.
- Coding harness: Codex through the compiled Go adapter; existing model configuration preserved.
- Harness elapsed time: 125,809 ms. Actual cost was not captured and is unknown.
- Outcome: waiting at the human gate. No commit, push, PR, merge, deployment or issue comment was performed by the harness.

## Work and independent verification

The issue restricted edits to `scripts/check-repo-hygiene.sh`, `scripts/check-repo-hygiene.test.sh` and `.github/workflows/lab.yml`. The retained isolated checkout contains changes only in those three paths. Its HEAD remained the original base.

After the harness returned, an independent invocation ran shell syntax checks, the repository hygiene checker, its regression suite and `git diff --check`. All passed; the regression suite reported 29 passing checks. Cases include tracked forbidden path segments, ordinary product directories, ignored/untracked local configuration, whitespace, newlines and literal shell metacharacters. The workflow change only replaces its inline guard with the extracted script.

These are local checks on an uncommitted working tree. They are not remote CI evidence or review approval. The check Atom remains honestly recorded rather than retroactively marked as executed. Ship and document remain skipped. The later delivery receipt and exact-commit CI evidence are recorded below.

## Findings

The Go path can discover a real repository, save and approve an onboarding Formula in a disposable Lab, preserve it on repeated init, fetch a GitHub issue, invoke the configured coding harness in isolation and retain its Molecule ledger. It does not yet establish installer parity, production delivery, successful learning or measured improvement.

The task is narrow enough to verify independently and produced an inspectable artifact. No Formula improvement follows automatically from one successful run. Ticket ambiguity, scope enforcement and repository policy verification need further implementation and evaluation.

## Delivery preview

The Go CLI generated a preview for the exact three-file scope without changing the real Git index. The prospective PR is stacked onto `feat/cli-mvp-delivery`, since the task was run against the unmerged Go implementation.

- Reviewed tree: `72f2542b4359727fa5b80617537fef0c9bb53a6a`.
- Preview digest: `f813a96abb9714c07a373822202bba18fbc2b0b6fa18a9b7bd2cede79a1690fb`.
- Diff size: 5,769 bytes.

This digest identifies content for review, not an approval to merge. Publication and exact-commit CI are recorded below.


## Publication and CI evidence

The Go CLI published draft [PR #11](https://github.com/DanielNetzer/forgecell/pull/11), stacked on migration [PR #10](https://github.com/DanielNetzer/forgecell/pull/10). Delivery commit: `c3395418284d97f241495410ea8c75b7bd2440a4`. No merge or issue closure occurred.

At 2026-09-29T01:31:40Z, the Go `checks` command verified that PR #11 still pointed to that exact SHA and recorded success for the two Go jobs (Ubuntu/macOS) and four TypeScript jobs (Ubuntu/macOS, Node 20/22). Vercel checks also succeeded. [GitHub Actions run](https://github.com/DanielNetzer/forgecell/actions/runs/36507752004).

These checks validate that commit, not later migration changes. The original execution ledger retains the original Atom outcomes; delivery has a separate durable receipt. Passing checks do not approve merging, prove a Formula improvement or complete the installer/evaluation portions of the MVP.
