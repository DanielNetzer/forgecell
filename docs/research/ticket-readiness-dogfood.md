# Ticket readiness: installed-candidate dogfood

Status: controlled dogfood milestone verified on 2026-10-03. This is a research record, not a release-success claim.

## Candidate and controls

- Implementation PR: [PR #15 — private historical evidence; access required](https://github.com/DanielNetzer/forgecell-private-history/pull/15) (draft).
- Candidate source: `b2fe256f7088e2abeb03df0e1f30dcdd61f14e33`.
- Installed binary SHA-256: `c58e424bf8594349a2d3c4564d5630ccfd16f8933a0e1dd975d4490ccb43a997`.
- Isolated Lab: `/tmp/forgecell-readiness-installed/lab`; public preview unchanged.
- The user approved the isolated Codex Formula, creation of issues #13/#14, and exact clear-ticket plan `2cbd488eae08eb91a7693195660fee76a474b976a7461a829bf077322eca113a`.
- Publication, merge, deployment and Issue comments are outside that scope approval.

## Findings so far

The first clear intake stopped because its required independent acceptance script
was absent from supplied evidence. It did not code. This exposed a normal-CLI gap:
`--verification` now accepts bounded frozen human inputs and proposed checks, setup,
artifact limits and environment policy. They enter the approval digest; loading
them never executes them. Independent review found no approval bypass in that
follow-up. Model output remains distinguishable from human-supplied checks.

The independent black-box script passes normal and exactly-5,000,000-byte input
assertions on the baseline, then fails at symlink rejection. Assertions after that
failure were not reached. The script was rerun against the exact b2fe256 candidate
base; that result supersedes the earlier 638d349 observation. The correction used
a successor plan revision in the same Molecule and retained the first proposal.

Ambiguous issue #14 returned concrete questions about command, desired observable
behavior, compatibility and source context. Its first workspace remained clean
including ignored/untracked files, and the harness Atom was skipped. The repeat on the final candidate also blocked with four specific questions;
Molecule `mol-14-45c2b30e81f49f9fbc19972d` retained base b2fe256, all later Atoms
were skipped, and the workspace was clean including ignored/untracked files.

## Measurements

Elapsed intake includes Issue/evidence collection and provider analysis, not human
waiting. These measurements are individual observations, not comparative proof.

| Attempt | Intake elapsed | Outcome |
|---|---:|---|
| Initial clear intake, missing acceptance evidence | 66.90 s | Blocked before coding |
| Clear intake with supplied verification | 34.71 s | Waiting for scope approval |
| Initial ambiguous intake | 26.09 s | Questions, no coding |
| Final-candidate ambiguous intake | 28.62 s | Questions; all later Atoms skipped, clean workspace |

Provider cost/token usage and server-resolved model identity are not measured.
The native binding preserves provider configuration; a local fingerprint detects
executable/config changes, but does not establish server-side default-model identity.

## Verification and remaining work

`go test ./...`, `go vet ./...` and diff whitespace checks passed. Exact-commit
GitHub Actions for b2fe256 passed Go core/installer checks on macOS and Linux plus
all four Node reference test/build jobs. Existing Vercel automation also built a
preview; no production deployment or release command was issued. No external
GitHub code review was present when checked; internal independent review was used.

The clear coding outcome, candidate/protected checks, independent acceptance and
final patch review passed. The draft was published after the user authorized it
conditional on the overall review; exact dogfood-commit CI is tracked below.

## First approved clear attempt

The coding harness changed only the three approved CLI files. Its own tests were
blocked by its sandbox's Go-cache access; those denials were reported, with no
permission escalation. The independently approved Lab setup used a fresh checkout
and its separately declared unrestricted network policy. It built successfully,
and the targeted candidate boundary test passed.

Both candidate and protected full-suite views failed existing fake-provider tests
(`TestProviderExecution`, also `TestCodexOutputFile` in the candidate) at their
one-second timeouts during concurrent package execution. The engine kept delivery
blocked; independent acceptance and later required steps did not run. An independent
read-only patch review found no correctness/security blocker in the three-file fix.

A diagnostic `go test -p 1 ./...` passed. Proposed revision 3 therefore limits package
parallelism, keeps all tests and permissions, retains the complete failed verification
JSON as frozen evidence, and adds a required exact-tree check against
`7685dbdd0b8a774aae668b8b926520f28dcc2d36`. The user approved exact process-only plan
`5437dffeaa3c6c95cce70a017e8af313f58727ff46747af278df2d37ac8098a6`.
The installed CLI resumed that revision; no fallback was silently applied.

This revealed a concrete efficiency gap: the current generic continuation re-enters
the harness even for a verification-only change. The proposal explicitly forbids new
edits and checks the retained tree, but does not remove that extra invocation. A
verification-only retry should be designed next; no efficiency improvement is claimed.

## Approved revision 3 outcome

All required candidate checks passed on unchanged tree
`7685dbdd0b8a774aae668b8b926520f28dcc2d36`: retained-tree comparison,
candidate boundary tests, serial full Go suite, vet, and frozen independent
acceptance. The protected regression view also passed its full suite and vet.
The candidate records `independentAcceptanceVerified: true`; the protected view
runs regression only and does not claim independent acceptance of its own.

The extra harness invocation took 84.07 s. Candidate full-suite execution took
39.05 s; protected full-suite execution took 38.88 s. The harness reported its
own tree-diff check failed because new files were untracked in its workspace;
the Lab's authoritative check in the materialized verification checkout passed.
The captured source tree exactly matches the previously reviewed patch, with no
scope violations. These observations remain separate in the ledger.

The Molecule now waits at the final delivery gate. Exact draft publication preview
`110f1d714506f7517983c188c5b0be1594af10bba037b6e4f8085ef5548f331d`
contains only the three approved CLI files and targets `feat/ticket-readiness`.
The user subsequently authorized draft publication conditional on framework review.
Independent architecture review found no blocker for this bounded draft milestone.
The installed CLI published [draft PR #16 — private historical evidence; access required](https://github.com/DanielNetzer/forgecell-private-history/pull/16)
at commit `bd6a42284cb0c5633cc8450c8a9f0c8644690834`. The installed CLI confirmed all six named CI jobs passed on that exact commit:
Go core/installer on macOS and Ubuntu, plus Node reference tests/typecheck/build
on both OSes with Node 20 and 22. No external GitHub review was present; the
independent internal review is not represented as an external approval.

## Framework assessment and next experiments

The direction remains model-assisted interpretation inside deterministic runtime
checks: the model proposes a plan, a human approves its exact scope and commands,
the runtime records the attempt and verifies its captured output, and a separate
human decision authorizes draft publication. Ticket analysis is an intake Atom in
the same Molecule. A ticket amendment is not a reusable Formula mutation.

The clear and ambiguous cases support this narrow workflow, not a general claim
that arbitrary tickets ship correctly. Readiness hashes bind reviewed evidence;
they are not authentication against someone who can rewrite all local state.

Follow-up findings at the original dogfood milestone (the history, retry and
learning items are now being hardened; see [current behavior](../ticket-readiness.md)):

- Resume verification without invoking the coding harness when source and bindings
  are unchanged. Retain each verification attempt's complete observations without
  manually importing the previous result as a frozen check input.
- Correct per-attempt presentation: summary Atom timestamps can span earlier
  attempts, while event history and individual process durations are authoritative.
- Clarify learning admission: a completed coding/check attempt waiting for final
  delivery review currently has `FinishedAt` and can be read by `learn`. Its ledger
  remains waiting, not shipped. Pending initial scope approval is excluded. Define
  which outcomes support which process claims before using these as success labels.
- Verify Claude analysis live and establish safe Cursor analysis support before
  claiming onboarding/readiness parity.
- Reuse existing review capabilities through the check Atom. If BazAI exposes a
  GitHub check/review or suitable API, a reviewed Formula can specify its evidence
  and owner. The ledger should bind producer, repository, exact commit, outcome
  and provenance. Missing or stale evidence should wait or request a decision,
  not silently invoke a second reviewer. This is a proposed integration contract,
  not an implemented BazAI adapter or a new primitive.

Evaluate process changes against successful acceptance, escaped regressions,
human interventions, repeat invocations and elapsed execution time on comparable
inputs. Keep failed attempts and state unknown cost/model measurements explicitly;
a single successful retry does not prove a Formula is better.

Compact machine-readable observations: [verification summary](ticket-readiness-evidence.json).
The full ledgers and delivery receipt remain in the isolated Lab specified above.
