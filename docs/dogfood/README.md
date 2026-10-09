# Forgecell builds Forgecell

Historical evidence from the retired TypeScript Lab. The source paths below identify
the implementation tested at the time; the supported Lab is now the Go CLI.

Historical issue numbers, commit hashes and original URLs refer to private repository
ID1390966758, planned to become `DanielNetzer/forgecell-private-history` (access required).
Retained frozen ledgers, issue snapshots, approvals, patches and JSON preserve original
bytes and embedded URLs as historical identifiers, not current navigation or execution
targets. The current source/root identity is separate; this notice does not record a
completed rename or publication.

On 2026-09-28 (Asia/Jerusalem), the local Lab took GitHub Issue #6, ran an explicitly
bound Codex coding harness in a separate checkout, and recorded its outcome.
The harness added `lab/src/invoke.test.ts` and corrected UTF-8 streaming in
`lab/src/invoke.ts`. This patch was brought back only after independent verification.

- [Issue #5 — private historical evidence; access required](https://github.com/DanielNetzer/forgecell-private-history/issues/5): record-only as requested. No harness invocation.
- [Issue #6 — private historical evidence; access required](https://github.com/DanielNetzer/forgecell-private-history/issues/6): real local code changes from the Molecule.
- Regression independently reproduced against the old code: split UTF-8 became replacement characters on both streams.
- With the generated patch: 47 tests passed, typecheck and CLI build passed.
- The coding harness's sandbox denied the `tsx --test` wrapper's IPC socket; it accurately reported this and ran the same tests via `node --import tsx --test` instead. Outside that sandbox, the regular `npm test` also passed.
- The separate learn Atom read the finished run and returned the original Formula: no evidence-backed improvement was proposed. The Lab rejected a no-op and did not invent or apply a mutation.
- Human ship gate remains waiting. No merge, deployment, issue comment or closure was performed by either harness.

The Markdown ledgers here are redacted copies (local checkout paths replaced).
The complete JSON ledgers, Formula snapshot/hash and run timestamps remain in the
local `.forgecell/ledgers/` directories. These files are evidence of local execution,
not evidence of production shipping. Local UI implementation remains deferred.

A later meta-harness invocation produced the first [evidence-backed Formula suggestion](formula-suggestion.md). The user approved its sandbox test-runner guidance. A subsequent Molecule used the revised Formula and reported the fallback separately, with 52 tests passing inside its sandbox. This demonstrates the approved process was followed, not general effectiveness. See the linked finding for scope and limitations.
