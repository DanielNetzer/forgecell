# Smart onboarding verification — 2026-09-28

This records controlled live verification for all three harnesses, with the limits of each run.

| Harness | Installed CLI | Authentication / capability probe | Packaged onboarding | Real Molecule |
|---|---|---|---|---|
| Codex | 0.158.0-alpha.2.1 | Passed | Saved proposal → exact approval | `mol-6-20260928T080243Z-ef2e3ac5`, waiting at human gate |
| Claude Code | 2.1.202 | Passed after user login | Saved proposal → exact approval | `mol-6-20260928T080244Z-f5f4aa88`, waiting at human gate |
| Cursor | 2026.09.26-dd393fe | Passed after user login | Saved proposal → exact approval; re-init preserved YAML | `mol-6-20260928T094315Z-edb9074c`, waiting at human gate |

The CLI was built, packed with `npm pack`, and installed outside its source checkout.
Generated bindings point to the installed `dist/adapters/cli.js`, not a repository example.
Codex and Claude each used a separate checkout, authenticated GitHub intake for issue #6,
and the exact approved Formula. Both found the Unicode fix already merged, verified the
regression and left clean working trees. These are controlled verification runs against a
known issue; they do not claim discovery or delivery of a new bug fix.

Codex reported 57 passing tests via direct Node execution after the regular test runner
was blocked by sandbox IPC EPERM. Claude reported 57 tests passing through `npm test`.
Both reported passing typecheck/build and demonstrated the original decoder failing the
regression before restoring the current implementation. Recent GitHub Actions reads remain
context only. Gate stayed waiting; ship/document were skipped.

Automated tests cover context selection, ambiguity, explicit choice, preservation of existing
bindings and learned recipes, CLI identity/capabilities, sanitized auth results, adapter
request/result normalization, exact-proposal review, modified/stale refusal and the shared
Formula-write lock. Terminal Approve and Deny were exercised in disposable Lab directories;
Deny left no active Formula. In a separate empty Git repository, the installed package's
full-YAML toggle and Edit → Approve flow were exercised; the editor-added YAML comment
survived approval and re-init byte-for-byte. With current-harness environment markers
removed, two ready CLIs produced an explicit ambiguity (JSON exit 2), while an explicit
Cursor choice reported blocked (exit 2), without activating a Formula.

The refreshed installed package was also run with `init --json` from
`/path/to/forgecell`: it selected Codex from current environment evidence,
saved an awaiting-approval proposal, and left active configuration/Formula files unchanged.
No ticket execution was started there.

Both live learning adapters returned pending process improvements from their test ledgers:
Codex suggested clearer reporting of blocked checks and fallback results; Claude suggested
checking whether acceptance criteria are already satisfied before implementing. Both supplied
reasoning, explicitly unverified impact and a next-run evaluation plan. Neither suggestion
was applied. This verifies the proposal contract, not that either change improves outcomes.

Final local verification: 84 tests passed, TypeScript typecheck and build passed, and
`git diff --check` passed. A separate read-only code review found no remaining blocking
findings after fixes to binding preservation, exact proposal approval and review copy.

Full ledgers and raw provider output remain local under the disposable harness-checks
checkouts. No raw account/auth output or credentials are committed.

## Cursor live finding and correction

The first authenticated run, `mol-6-20260928T093922Z-0b0bdc57`, reported retrying a
sandbox-blocked test outside the sandbox. The adapter had explicitly enabled `--auto-review`,
overriding the user's configured Allowlist mode. That run is not counted as a clean permission
verification. Its learning suggestion also proposed that retry; it was dismissed in the
isolated test Lab without changing the Formula.

A failing regression test was added before removing the override. Cursor now retains its
configured approval mode, with sandbox mode explicitly enabled; neither coding nor learning
instructions permit broader-permission retries. Existing user configuration was not edited.
See [Cursor configuration](https://cursor.com/docs/cli/reference/configuration) and
[run modes](https://cursor.com/docs/agent/security/run-modes) for the distinction between
sandbox execution and Auto-review's classifier.

The corrected installed-package run `mol-6-20260928T094315Z-edb9074c` reported the regular
`npm test` blocked by sandbox IPC EPERM, explicitly did not retry with broader permissions,
and passed 57 tests using direct Node execution. It reported passing typecheck/build;
the checkout was independently confirmed clean. Gate stayed waiting and ship/document
stayed skipped. This verifies the tested CLI/configuration, not a universal guarantee about
all user-defined allowlists, integrations or future CLI versions.


The corrected Cursor learning invocation returned pending suggestion
`suggestion-291a1509-9a6c-4e3b-83bc-7d7396e0e211`: use the direct Node test runner for the
specific IPC failure, record both outcomes, and never retry with broader permissions.
It included evidence, expected impact and a next-run evaluation plan. It remains unapplied;
this is evidence of the learning contract, not proof that the proposed process improves runs.
