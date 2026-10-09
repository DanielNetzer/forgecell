# Proposed installed-candidate dogfood

Posted with user approval on 2026-10-03: [clear issue #13 — private historical evidence; access required](https://github.com/DanielNetzer/forgecell-private-history/issues/13) and [ambiguous issue #14 — private historical evidence; access required](https://github.com/DanielNetzer/forgecell-private-history/issues/14). These are inputs for the pending live milestone, not claims of completed product behavior.

## Clear issue

**Title:** Dogfood: bound read-only ledger inspection

The Go CLI's `ledger` command currently reads the ledger with unbounded
`os.ReadFile` and follows a symlink. Change only the inspection path so a local
ledger is inspected as a bounded regular file, without changing its contents.

Relevant committed source: `lab/internal/cli/cli.go`,
`lab/internal/cli/cli_test.go`, `lab/internal/ledger/ledger.go`.

Acceptance:

- A valid regular v0 Molecule ledger at or below 5,000,000 bytes prints successfully
  and preserves unknown extension fields. Reading never rewrites it.
- A ledger larger than 5,000,000 bytes fails nonzero without printing its content.
- A symlink or directory used as the ledger file fails nonzero without printing
  the target's content.
- Keep the current schema/identity validation and ordinary missing-file errors.
- Include meaningful automated coverage; run Go tests and vet from `lab/`.

Proposed exact edit scope: `lab/internal/cli/cli.go`,
`lab/internal/cli/ledger.go`, `lab/internal/cli/ledger_test.go`.
If another path is necessary, request an amendment before editing it.

No UI, install/release changes, model routing, comments, merge or deployment.
The Lab may later publish a draft PR only after separate review of the exact diff.
The independently supplied acceptance script must fail on the baseline and pass
on the candidate. Candidate-authored tests do not replace that check.

## Ambiguous issue

**Title:** Dogfood: make ledger inspection nicer

Ledger inspection should feel nicer. Improve it.

This deliberately omits the requested observable behavior and acceptance criteria.
Expected Lab outcome: specific questions and blocked intake, zero coding requests,
no source edits, PR or Issue comment. Do not infer that this means the clear issue.

## Formula proposal

The isolated local candidate's Bootstrap Assay proposes Codex using the user's
existing configuration, with intake → scope gate → harness → check → review gate
→ ship → document, plus the separately configured learn Atom. Approving the Formula
only activates that recipe in the temporary dogfood Lab. Each ticket still requires
its own exact scope approval, and publication requires another exact diff approval.
The user's existing Lab and public CLI preview remain untouched.

The user approved activating this isolated Formula on 2026-10-03. Ticket scope and publication approvals remain separate.
