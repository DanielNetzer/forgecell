# Readiness hardening — 2026-10-03

The user approved retaining complete verification history, a reviewed checks-only
continuation, and explicit learning eligibility. This extends the existing
Molecule lifecycle; it introduces no primitive, provider routing or UI.

## Implemented behavior

- Complete verification results and completed command observations live in
  append-only, hash-linked local evidence files. The main ledger contains attempt
  identity, timing, status and references plus a compact current projection.
  Amendments preserve earlier attempts. Recovery marks interrupted verification
  and keeps already-persisted command evidence. Corruption blocks continuation.
- `continuation: checks-only` is part of the amended plan digest. It retains scope
  and source and revalidates Issue, Formula, base and binding before fresh checks.
  Uncertain coding cannot enter it. The coding harness and preparation hook are
  not called. Changed check commands still need exact successor-plan approval.
- Learning rejects pending execution and unreconciled publication. Failed,
  verified-undelivered and publication evidence are explicitly labelled. Draft
  status requires recorded publication provenance; legacy status stays unknown.
  Complete, verified archived results are included within a 4 MB selection budget.
  No classification establishes merge, deployment or production health.

## Verification

A regression test first reproduced coding on a check-only retry. It now asserts
one coding invocation, two verification attempts, preserved prior output and final
review eligibility. Tests cover scope/source/Formula drift, uncertain coding,
corrupted evidence, large captured output outside the ledger, interruption
recovery and bounded learning inputs. Learning tests cover pending states,
failures, historical unknowns, local verification, publication uncertainty,
draft provenance and mismatched receipts.

Independent review caught output duplication and overclaimed draft provenance;
both were corrected. A separate fault-injection test reproduced continuing into
the protected view after evidence persistence failed. That path now stops without
starting further verification commands. Full Go suite and vet passed again after that final fault-path fix.
The built-CLI smoke test also passed against the final candidate.

Built-CLI smoke test: [script](../dogfood/readiness-hardening-smoke.py). It supplies
local GitHub/provider fixtures and exercises real CLI subprocesses, worktrees,
approval digests, the failed check, amendment and successful retry. Observed:
one coding invocation, two verification attempts, old result bytes unchanged,
final `review-waiting`. This tests lifecycle wiring, not live model reasoning or
GitHub access; the earlier #13/#14 research retains that evidence.

## Limits

See [current CLI contract](../ticket-readiness.md) for storage and input bounds.
History remains trusted local data, not protection against someone rewriting the
entire Lab. A killed command can have an unknown outcome; only persisted completed
observations are available. Filesystem errors stop progress and may leave orphan
append-only evidence for manual inspection. No automatic deletion, retry, merge,
release, deployment, or Formula mutation is included.
