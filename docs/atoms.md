# Atom types

An Atom is one workflow step. The table distinguishes the current executor from the
longer-term ticket-to-production flow.

| Type | Current behavior | Planned behavior / limitation |
|---|---|---|
| `intake` | Reads a GitHub Issue with `gh`; records provenance | No automatic clarification conversation |
| `harness` | Executes explicit `harness.command` with issue and recipe guidance | Requires approved Formula and exact ticket scope; exit zero is not proof of shipping |
| `check` | Runs approved local verification of the captured source tree in a fresh checkout | Required failure stops the run; exact-head CI is a separate gate |
| `gate` | Waits for exact scope approval before coding and human review after successful verification | Formula approval does not approve ticket execution or publication |
| `ship` | Skipped | Explicit draft-PR delivery is separate; merge and deploy are not implemented |
| `document` | Skipped | Issue write-back is not implemented |
| `learn` | Explicit `forgecell learn` proposes a Formula change for human review | Skipped during a normal run; evaluation of follow-up ledgers is manual |
| `observe` | No day-one binding | Future post-production observation; no Datadog integration |

## Gate modes and policies

A `gate` Atom decides by `mode`. Absent `mode`, `mode: human` and any unrecognised value
mean a human decides; that is the default and nothing changes for an existing Formula.
`mode: policy` plus `policy: <id>` declares a reviewed, versioned policy instead:

| Gate `purpose` | Valid policy | Meaning |
|---|---|---|
| `scope` | `auto-when-ready` | Approve the exact plan digest when every condition below holds |
| `review` | `deliver-when-checks-pass` | Open a PR after all required checks pass; merging is a separate opt-in |

```yaml
- id: scope
  type: gate
  purpose: scope
  mode: policy
  policy: auto-when-ready
```

Formula validation rejects a `policy` on any other Atom type or gate purpose, a pair that
does not match the table, an unknown or non-string id, `mode: policy` without a `policy`,
a `policy` without `mode: policy`, and a `policy` key at the Formula root or inside the
`harness` mapping. Formula, binding, release and deploy changes always remain human.

`auto-when-ready` approves only a first plan at the waiting scope gate, and only when:

- the Formula snapshot hashes to its recorded SHA-256 and was approved for execution;
- the coding checkout is available and no coding attempt has started;
- the intake analysis and the plan have no open questions;
- the scope is not empty and every path is an exact repository path;
- the plan's coding allowlist is non-empty and equals the allowlist derived from the Formula
  snapshot and the plan's checks;
- the plan digest and proposal history are intact and the plan has no continuation.

Any other result is `decline` (a condition fails, so a human decides) or `human` (the gate
declares no evaluable policy, so a human decides). An automatic decision is recorded in the
ledger as `approvedBy: policy:<id>@<formula-sha256>` with the ledger pointers it relied on,
and is rendered as policy-approved, never as a human decision. Policies relax nothing else:
approved checks still run repository code, and passing candidate checks is not independent
acceptance.

This change adds the Formula grammar, the evaluator and the ledger record only. No runner
applies a decision yet, so `forgecell run --approve` remains the only way a scope is
approved, and `deliver-when-checks-pass` is accepted by validation but always evaluates to
a human decision.

The check Atom executes reviewed argv, working directories and timeouts after validating
command definition hashes. It uses a temporary HOME and filtered environment, with
unrestricted network access and no OS filesystem/network sandbox. Source mutations
or writes outside approved artifact allowances fail verification.

Required-check failure blocks the run and leaves subsequent Atoms skipped. Successful
verification leaves the review gate waiting; later publication Atoms remain skipped.
When test paths or cited regression definitions change, regression checks also run
against a protected view that restores those frozen baseline files, so candidate edits
cannot replace the preserved suite. Passing
candidate checks does not establish independent acceptance; that requires separately
supplied checks with independent provenance. See [ticket readiness](ticket-readiness.md).

Named GitHub checks for the exact PR head (`forgecell checks`) and explicitly approved
draft-PR delivery (`forgecell deliver`) are separate gates. Local check success does not
prove CI, merge, deployment or acceptance. These ticket-readiness and delivery commands
are development-build features until a new release is published; see [Install](install.md).

The coding harness implements the ticket. The learn Atom invokes the distinct meta harness,
which proposes improvements to the process. See [Learning](learning.md).
