# Atom types

An Atom is one workflow step. The table distinguishes the current executor from the
longer-term ticket-to-production flow.

| Type | Current behavior | Planned behavior / limitation |
|---|---|---|
| `intake` | Reads a GitHub Issue with `gh`; records provenance | No automatic clarification conversation |
| `harness` | Executes explicit `harness.command` with issue and recipe guidance | Requires approved Formula and exact ticket scope; exit zero is not proof of shipping |
| `check` | Runs approved local verification of the captured source tree in a fresh checkout | Required failure stops the run; exact-head CI is a separate gate |
| `review` | Read-only adversarial review of the captured change; findings are ledger evidence | Optional; one Atom after `check` and before the review `gate`; see [Review Atom](#review-atom-adversarial-review) |
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

### Ledger inventory scaling limits

The loop's ledger inventory, clarification history and `forgecell labs` read only each
ledger's header (identity, status, mode, start and finish times, Lab, Issue, workspace and
readiness phase) with one shared streaming reader, so memory stays bounded however large
the ledgers grow. The limits are 4096 directory entries, 4096 ledger JSON files, 5 MB per
ledger and 1 GB read per scan; a Molecule's `.md` report, `.lock` approval lock and the
loop's `.loop-lock` files do not count as entries, and 65536 raw directory entries of any
kind are a further safety bound. There is no other cap on the total bytes of `ledgers/`. A
ledger saved by this version writes its header fields first, ahead of the attempt arrays,
so decoding stops after a few KB; the rest is then only checked to be valid JSON, so a
truncated or corrupt ledger is still refused. The JSON field names are unchanged, so older
binaries still decode it. A ledger saved by an earlier version keeps its order until its
next save and is streamed to its header with constant memory, reading at most 5 MB. Within
one process, such as a loop pass and its re-read under an Issue lock, a ledger whose
device, inode, modification time and size are unchanged is not read again. A ledger that
is oversized, unreadable, unsafe or malformed, or a scan that reaches a limit, makes the
inventory incomplete with the file or limit named, and the loop then starts or advances
nothing.

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

### Check selection: affected Go packages

A Formula `requiredChecks` entry, or a reviewed evaluation plan command, can declare
`select: affected-packages` on a direct `go test` check whose last argument is `./...`
(argv[0] `go`, argv[1] `test`, no other `./...` and no `-args`). Its other arguments
must be flags that leave the package graph unchanged (`-run`, `-skip`, `-count`, `-v`,
`-json`, `-short`, `-race`, `-timeout`, `-failfast`, `-p`, `-cover`, `-covermode`,
`-coverprofile`, `-bench`, `-benchtime`, `-benchmem`); `-tags`, `-mod`, `-modfile`,
`-overlay`, `-C` or any other flag refuses the selection, because `go list` would not see
the packages it changes. The analysis cannot declare it. At approval, a plan may carry
`select` only on a Formula baseline check with the Formula's exact value or on a command
the frozen evaluation plan declares with the same name, dir, argv and select, and its
`fullSuiteTriggers` must equal what the Formula resolves to; an amendment or supplied
verification inputs cannot add a selection or drop or replace the triggers. Verification
then replaces `./...` with the module packages the captured change affects:

- The changed packages are the directories of the changed `.go` files (base commit to
  captured tree).
- The affected set is the changed packages plus every module package whose own, test or
  external-test dependencies include one (`go list -test -json ./...` in the
  verification checkout, with the check's own environment).

The check runs the full `./...` suite exactly as before when any of these holds:

- a changed file lies outside the check's module, or is `go.mod`, `go.sum`, a `go.work`
  file, under `testdata/`, any non-Go file (docs, scripts, `.github/**`, data), or a Go
  file in a directory Go ignores;
- a changed file is under a `cmd/` directory, or matches a Formula `fullSuiteTriggers`
  path or glob (`*`, `?` and `[...]` within a segment, `**` across segments). An absent
  key means `lab/cmd/**`, for packages whose tests build or exec the CLI binary;
- the change adds or removes a package, or a changed file is embedded by a package;
- `go list` fails, reports a package error, times out (5 minutes), exceeds 32 MiB of
  output or 2,000 module packages, or returns packages of another module;
- the affected set is empty while Go files changed, or nothing changed;
- the selected import paths would exceed 128 KiB of argv.

The protected regression view runs the same selection, computed once from the candidate
view's captured change, so the two views stay comparable. If the candidate view never
computed one, the protected view runs the full suite. Each such check's record carries
the selection, and the ledger shows one line per view: `selection: affected-packages
(N of M packages)` or `selection: full suite (reason: …)`. gofmt, vet and the hygiene
scripts keep their scope. Replay-generated evaluation plans select on their `go-test`
check; hidden acceptance keeps its own `-run` filter. GitHub CI still runs the full suite
before merge, so a narrowed Lab run never replaces that gate.

### Amending a plan from flags

`forgecell amend MOL` without `--plan` builds the next revision from the latest plan
(`--from-latest`, the default), so no plan JSON is hand-edited:

```
forgecell amend MOL [--add-scope PATH --reason TEXT]... [--add-acceptance TEXT]...
  [--note TEXT] [--continuation checks-only] [--accept-existing-tree TREE] [--run]
```

- The parent is the latest plan's digest, read under the Molecule lock (the loop's
  flock, then the `ledgers/MOL.lock` approval directory). A second amend that runs at
  the same time fails with a "locked" message; one that runs after the first builds on
  the first's revision. The history never forks, and a stale `--parent` cannot occur.
  `--parent` is accepted only with `--plan`.
- `--add-scope PATH --reason TEXT` (repeatable; each path needs the `--reason` that
  follows it) adds an exact scope path. Forgecell picks its evidence: an existing path
  cites its own frozen evidence when there is one. Otherwise it cites the nearest frozen
  `go.mod`, `package.json` or `pyproject.toml` whose directory is an ancestor of the
  path, with `go.mod` preferred in the same directory. This is the rule approval
  applies to a new path. A new path that no manifest covers is refused. A path a check
  definition cites, or that a check's argv names as an exact non-test path, is refused
  with the intake message `drop or replace checks defined by in-scope files: …`.
- `--add-acceptance TEXT` (repeatable) adds an acceptance item with evidence `[issue]`.
  The coding harness receives it in `newAcceptance`.
- `--note TEXT` sets the revision's single operator status note (`operatorNote`, at
  most 4,000 bytes, no control characters). It is bound by the digest and shown in the
  plan summary. The coding harness receives it inside `approvedPlan` as context, never
  as an acceptance item. A note belongs to the revision that sets it: a later `--note`
  replaces it, and an amend without `--note` clears it.
- `--continuation checks-only` re-verifies the retained tree without coding. The usual
  checks-only conditions apply: unchanged scope and captured, completed coding output.
  Without it the new revision codes again, whatever the parent's continuation was.
- Each amend adds at most 64 scope paths and 64 acceptance items, and each reason or
  item is at most 4,000 bytes. The plan's own bounds still apply, and a refusal writes
  nothing.
- `--run` approves and runs the new plan right after the amend, with its digest and
  recorded base commit, and prints the same summary as `forgecell run`.

Each amend prints the exact `--approve` digest and a one-line diff summary such as
`+2 scope, +1 acceptance, note set`. `--plan FILE --parent DIGEST` still records a
hand-written successor exactly as before, and the plan-building flags cannot be
combined with it.

## Review Atom (adversarial review)

A `review` Atom runs a separately bound review harness once per coding attempt, after
`check` and before the human review `gate`. It exists because the Lab's own checks passed
for changes that an outside adversarial review found defective.

- **Read-only, no tools.** The adapter invocation is the ticket-analysis isolation: Codex
  runs `--sandbox read-only` with approval `never`, `--ephemeral`, `--strict-config`,
  `--ignore-rules`, web search disabled and each tool, app, plugin, hook and agent feature
  disabled; Claude Code runs with `--tools ""`, `--strict-mcp-config` and an empty MCP
  configuration, `dontAsk`, `--safe-mode`, `--no-chrome` and slash commands disabled, and
  never an allowlist. Cursor is unsupported and a custom command is refused without being
  executed. A request carrying extensions is rejected. The provider's controls are probed
  before it starts, as for ticket analysis. At Formula proposal only an adapter whose review
  support is known to be `supported` is accepted; an unsupported or unknown adapter id is
  refused.
- **No credentials.** The review process runs in an explicit environment built from an
  allow-list: system basics and the provider's own authentication and configuration
  (`ANTHROPIC_*`, `CLAUDE_*`, `CODEX_*`, `OPENAI_*`, proxy and CA settings, `AWS_PROFILE`
  style routing). Plan `credentialNames`, `GH_TOKEN`, `GITHUB_TOKEN`, other GitHub, GitLab
  and CI variables, static cloud keys and agent sockets never pass, even when set in the
  Lab's environment. Ticket analysis does not yet filter its environment the same way.
- **Binding.** `harnesses.review` if declared, else the meta binding as `MetaBinding`
  resolves it (`harnesses.meta` with a learn Atom, else the learn Atom's command), else the
  Formula proposal fails with an instruction to configure one. The default coding binding is
  never used, because the coding harness must not review its own change. At run time the
  same resolution fails closed with no process started. If the review command equals the
  coding command, that is recorded as `sameCommandAsCoding` and shown at the gate; it is
  not refused. A `review` Atom cannot declare its own `command`.
- **Input.** The issue, the approved plan without credential names, the diff from the base
  commit to the captured tree, check results with bounded output tails, and a bounded
  ledger view. Every string passes the secret redaction rules, and the number of values
  replaced is counted, sent to the reviewer as `redactions` and recorded on the attempt; the
  gate and the Markdown ledger show a notice when it is not zero. The diff is capped at
  400,000 bytes and is never truncated: a larger diff means the review is not run and the
  attempt fails closed. The diff is not stored in the ledger. The request carries no text the
  coding harness wrote about its own change: its outcome reason, the coding Atom's detail and
  any note that repeats it are left out, so the reviewer judges the diff, not the author's
  account of it.
- **Output.** Strict JSON `{findings:[{severity, file, line, claim, scenario, status, fix}],
  verifiedSound:[], reviewedFiles:[]}` with `high|medium|low` and `confirmed|plausible`. The
  decoder rejects malformed JSON, duplicate or case-variant keys, unknown or missing fields,
  nulls, wrong enumerations and oversize values. `reviewedFiles` must list every changed
  path, spelled as supplied; a pass that omits one, or a change with more paths than a review
  can list (1,000), is a failed review and blocks delivery even when it reports no findings.
  Malformed output, an empty result, a provider failure or a timeout is a failed review. It is
  never read as "no findings".
- **Effect.** One or more `high` findings block delivery. Medium and low findings draft
  follow-up issue text and never file it. A review with no high finding is evidence, not an
  approval: the human review gate, every gate policy and merge stay as they were.
- **Ledger and gate.** `reviewAttempts` records the binding, its source, the reviewed tree,
  the counts, the redaction count, the changed and reviewed file counts, the sanitized
  findings, `verifiedSound`, the drafts and secret-free process diagnostics. Text is stripped
  of control characters and terminal escape sequences, bounded per field, and the gate shows
  at most 10 findings and 5 drafts, escaped again. The gate lists one line for every earlier
  review pass before the latest, with its verdict and high-finding count, so a blocked pass is
  never hidden behind a later clean one. The Markdown ledger renders every review pass, and
  its verification summary says when the review blocked delivery instead of reporting only
  that the required checks passed.

`reviewAttempts` is an additive ledger field. As with `policyDecisions`, an older Lab binary
decodes it but drops it on the next save, so do not run an older binary on a ledger that holds
a review.

**Position.** `Formula.ExecutionRoles` accepts one optional `review` Atom, with no purpose,
immediately after `check` and before the review `gate`, and still accepts a trailing `learn`
Atom. The review runs once the required checks have passed, before readiness records the
verification. A blocking or failed review records the verification as not passed, marks the
`review` Atom failed, skips the later Atoms and stops the Molecule as blocked, so delivery is
not offered. A Formula without a `review` Atom resolves and runs exactly as before. A checks-only
or failed-tree continuation is its own attempt, so it gets its own single pass.

Deferred, each a separate reviewed change: the Formula review-policy grammar (`block` and
`issue`), returning a blocked Molecule to coding with findings as acceptance criteria, the gate
policy readiness condition "no high findings", learn integration, and rebind of the review role.
