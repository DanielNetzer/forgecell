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
- the plan digest and proposal history are intact and the plan has no continuation;
- the intake analysis and the plan have no unknowns, the plan has no setup commands, no
  mandatory constraints and no `allowNoChange`, and no expected protected-regression delta;
- the plan has no `artifacts` (artifact write allowances) and no `checkInputs` (human-supplied
  frozen inputs); a human gate examines both, so either one declines;
- no scope path is on the sensitive-path list below, and no scope path is one that a check
  definition cites: a frozen definition file reached through a check's `definitions`, or an
  exact non-test path named in a check's argv. This holds even when the check carries a
  `reviewedAcceptance` repair, because the policy never approves edits to a check definition;
- the plan's `harnessTimeoutMs` is within the coding binding's Formula ceiling;
- the plan's execution policy is exactly the Lab default: `environment` is `["PATH"]`, there
  are no `credentialNames`, `setupNetwork` and `checkNetwork` are `unrestricted` and
  `containment` is `filtered-environment`. These are what a human approves on every default
  plan, so the policy adds no access. Any credential name, any other environment name, and
  any other network or containment value declines. The accepted set is fixed in the
  evaluator rather than read from the Lab defaults, so a later tightening is a reviewed change.

Any other result is `decline` (a condition fails, so a human decides) or `human` (the gate
declares no evaluable policy, so a human decides). An automatic decision is recorded in the
ledger as `approvedBy: policy:<id>@<formula-sha256>` with the ledger pointers it relied on,
and is rendered as policy-approved, never as a human decision. Its evidence holds the values
the evaluator observed (counts, the scope paths, the policy fields, the timeout, a hash of the
derived allowlist, the artifact and check-input counts, the sensitive-path list version and the
number of scope paths examined against it), not fixed wording. Policies relax only the
conditions above: approved checks still run repository code, and passing candidate checks is
not independent acceptance.

The policy does not re-read the live issue itself. A decision is recorded only inside the
approval transaction, which re-reads the live issue, evidence, base, origin, branch and
workspace capture as `run --approve` does, and persists the decision in the same save as the
approved event. A changed issue, base commit, origin, branch or frozen evidence set declines
and records nothing; the ledger stays at the waiting scope gate.

This change adds the Formula grammar, the evaluator, the ledger record and an internal
approval-transaction mode. No runner applies a decision yet, so `forgecell run --approve`
remains the only way a scope is approved, and `deliver-when-checks-pass` is accepted by
validation but always evaluates to a human decision.

### Sensitive-path list (version `v1`)

A scope path on this list declines with `scope-path-sensitive`, because an edit there changes
what later runs execute. A path that a check definition cites declines with
`scope-path-check-definition`. The list is versioned, recorded in each decision's evidence and
pinned by a test, so changing it is a reviewed change to the code, that test and this page.
Matching runs on the already-validated clean path and ignores case:

- directory prefixes at the repository root: `.github/`, `.claude/`, `.forgecell/`,
  `.githooks/`, `.husky/` and `scripts/`. The whole `scripts/` directory is listed because the
  CI workflow and the Formula baseline checks run several scripts from it;
- file names at any depth: `.gitlab-ci.yml`, `go.mod`, `go.sum`, `package.json`,
  `package-lock.json`, `npm-shrinkwrap.json`, `pnpm-lock.yaml`, `bun.lockb` and `Makefile`;
- any file with a `.lock` extension.

Paths under `.git` are already rejected by path validation. A decline only sends the plan to a
human, who can approve it with `run --approve` as before.

### Accepted residual risk: check argv

Approved check argv runs repository code with unrestricted network access, a temporary `HOME`
and a filtered environment, and no OS filesystem or network sandbox. This is an
accepted residual risk of the Lab-default `auto-when-ready` policy. The policy does not
restrict check argv: it adds no access beyond what a human approves on a default plan, where
the same argv, network and containment are approved today. It does not decline checks that are
not a Formula baseline or a derived regression check, because that would decline every
model-proposed candidate check. Passing candidate checks is not independent acceptance. A
stricter rule is a reviewed policy change.

### Extensions are hints, never gate inputs

Output from an extension (an MCP provider attached to the analysis or coding role) is a hint.
It is never frozen evidence and never an input to a gate. An extension cannot approve,
decline, deliver, merge or change a gate, and it does not affect any `auto-when-ready`
condition above or the sensitive-path list: the policy evaluates the plan, the Formula
snapshot and the ledger pointers it records, never what a provider said. See
[Extensions](../lab/README.md#extensions-development-build).

### Downgrade behaviour

`policyDecisions` is an additive ledger field. A Lab binary that predates it still decodes a
ledger that carries it, but it saves the ledger from its own record type and silently drops
the field the next time it writes that ledger. The record that a policy approved a scope is
then lost, although the approved readiness event remains. Do not run an older Lab binary on a
ledger that has a policy decision. The ledger schema version is unchanged, so an older writer
is not refused.

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
