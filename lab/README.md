# Lab

Forgecell's terminal runtime is a Go CLI. **Compose once. Improve forever.**

The **Lab** runs **Molecules**: one ticket unit of work and its ledger. Each Molecule
is **Atoms** (workflow steps) arranged by a **Formula** (a durable YAML recipe).
Your coding harness implements the ticket; the Lab records the evidence and human decisions.

## Install and start

Use [Install](../docs/install.md) for the existing versioned public CLI installer
(`0.2.0-preview.1`), prerequisites, PATH, upgrades and rollback. Installing the
compiled CLI requires neither Node nor Go. Running tickets requires Git,
authenticated GitHub CLI (`gh`) and an authenticated supported harness.

Inside the repository you want to work on, choose a distinct local Lab name and use
it for every command with the published preview:

```sh
lab_dir="$HOME/.forgecell/labs/YOUR_REPOSITORY"
forgecell doctor --lab "$lab_dir"
forgecell init --lab "$lab_dir"
# Inspect the saved Formula YAML and binding, then approve its actual proposal ID:
forgecell init --lab "$lab_dir" --approve PROPOSAL_ID
forgecell run ISSUE_NUMBER --lab "$lab_dir"
```

Doctor performs read-only capability and authentication checks; it does not prove
model access or repository policy readiness. Init's Bootstrap Assay proposes a
Formula without running a model and preserves existing approved Formulas.
For structured onboarding and harness selection, see [agent onboarding](../docs/agent-onboarding.md).

### Reading doctor output

`doctor` prints JSON by default, as before, with a top-level `schemaVersion` of
`"v1"`. Within `v1`, changes only add fields. `doctor --verbose` prints a human
view with every probe step, the bound paths and versions, and the next command.
`doctor --verbose --json` prints the same JSON plus a `steps` list on each probed
candidate and on `savedBinding`. Exit codes are unchanged: 0 only when ready, 2
when not ready, 1 on error.

Each harness probe names the step that failed, with bounded evidence (the fixed
argument list, an error class such as `timeout` or `exit-code-2`, the expected
marker or flag, or the authentication state). Provider output is never echoed.
The steps are `version`, `help` (Codex also checks `exec --help`), `identity`
(version and help ran, but the product was not recognized),
`required-flag:<flag>` (one entry for every missing flag), `auth` and `isolation`.
A failed `isolation` step blocks the ticket workflow without changing the
harness's `readiness`.

A saved binding is reported by state, each with its paths: `launcher-missing`,
`launcher-different` (the saved launcher is not the running executable),
`adapter-mismatch` (the saved adapter differs from the Formula binding) and
`provider-missing` (the bound provider is absent or not an executable file). The
launcher is never executed; its version is read from the adjacent release
`manifest.json` when present, otherwise it is `unknown`. When the adapter matches
and the provider exists, doctor still probes the provider at its own absolute path
with the same fixed read-only calls, so a harness installed outside `PATH` is
checked where it is bound. The binding stays `blocked` until the launcher matches.

Every result that is not ready carries one `nextCommand`: the provider's login
command (`codex login`, `claude auth login` or `agent login`) when authentication
failed; `forgecell init --lab DIR --harness ID` when a ready provider is available
to rebind (prefixed with the provider's directory on `PATH` when it is bound
outside `PATH`, because init discovers harnesses through `PATH`); otherwise
`forgecell doctor --lab DIR --harness ID` to re-check.

## Review a ticket (development build)

Ticket readiness on this branch is **not in the published `0.2.0-preview.1`**.
Formula approval and ticket scope approval are separate decisions. Existing
Formulas need an explicitly reviewed scope gate before new readiness runs.

```sh
forgecell run ISSUE_NUMBER --target TARGET_BRANCH
# Inspect exact paths, acceptance criteria, check commands, limits and unknowns.
# Only then use the CLI-generated approval command with PLAN_DIGEST.
forgecell ledger MOLECULE_ID
```

Choose `TARGET_BRANCH` as the intended PR base in your repository. Follow the
generated approval command to retain the reviewed base commit, target, Formula
and Lab directory; approval must use the same inputs as analysis.

The first run analyzes the issue and waits before coding. Approval binds the exact
plan; execution uses an isolated checkout, and required checks run in a fresh
checkout before final review. The ledger preserves the Molecule, exact Formula
snapshot and check provenance. Use the same `--lab DIR` throughout when overriding
the default Lab. In the development CLI, each checkout has a private Lab under
`~/.forgecell/labs/`; `doctor --json` reports its exact path. Existing checkout
`.forgecell` data is never moved or deleted automatically. Pass `--lab .forgecell`
to inspect an older Lab deliberately.

Delivery is a separate review: `deliver` previews the exact file scope without
publishing. A separately approved digest authorizes committing those files,
pushing the isolated branch and opening a **draft PR**. Local checks do not
replace exact-commit GitHub CI.
Merge, deployment and issue closure are not automatic; scope approval does not
authorize them or publication. See [ticket readiness](../docs/ticket-readiness.md)
for delivery, checks, amendments, recovery and execution limits.

## Harnesses and learning

Native bindings support Codex, Claude Code and Cursor, retaining the harness's
configured model. Choose explicitly with `init --harness codex`, `claude-code` or
`cursor`. Codex coding uses workspace-write and learning uses read-only mode;
Claude Code uses print mode with an exact tool allowlist, user-level settings only, no MCP
servers and edits under the checkout's `.claude/` denied (personal user hooks still apply;
see #36); Cursor uses print mode with
its sandbox enabled. These permission modes provide different guarantees;
arbitrary adapters retain their process permissions.

The Claude Code coding allowlist is derived from reviewed evidence, never a constant.
It holds `Read`, `Glob`, `Grep`, `Edit`, `Write` and the read-only `Bash(git status)`
and `Bash(git diff)`; the argv of each approved plan check; and each
`repositoryContext.components[].suggestedChecks` entry in the approved Formula
snapshot. Entries render as `Bash(<argv>)` with no wildcard added. A component outside
the checkout root gets a directory flag (`go -C lab test ./...`, `npm --prefix web run
test`) so the command runs from the root. A `package.json` component also keeps the
earlier Node preset (`npm test`, `npm run test *`, `npm run typecheck`, `npm run build`,
`node --test *`). An argv is omitted, not escaped, if it contains shell syntax or
whitespace inside an argument, names a shell, `git` beyond status and diff, `gh`,
`curl`, a publish, push, deploy or login word, or a directory with no known flag.
Commit, push and publication are never added. The omission rules are a best-effort lint,
not a sandbox: every allowed check runs repository code, including tests the harness has
just written, so approving a check approves arbitrary execution with your user's
permissions. Review derived entries at the scope gate accordingly. The sorted, de-duplicated list is stored
in the plan, so it joins the approval digest and a change needs re-approval. Coding
refuses to start if the stored list differs from one rederived from the Formula and plan
checks. Amendments always rederive the list from their checks, so a payload can neither
add nor drop entries. A plan with no stored list was approved by an earlier Lab and keeps
the allowance it was approved under: the previous fixed Node entries plus the read-only
base. Amending such a plan rederives its list from the Formula, which can drop that Node
allowance when the Formula has no `package.json` component; the scope gate prints the exact
list either way. Each harness attempt records the list the provider received
in the ledger as `codingAllowlist`; a historical attempt without the field is
unrecorded, not empty.

Binding availability does not imply ticket-analysis support. Codex analysis has
been verified; Claude Code's live analysis probe remains outstanding. Cursor and
custom bindings stop new readiness runs because read-only action isolation has
not been established. See [adapter boundaries](../docs/agent-onboarding.md#adapter-boundaries)
and [provider support](../docs/ticket-readiness.md#provider-and-policy-support).

The coding harness changes ticket code. A separately bound meta harness uses
finished Molecule evidence to propose a Formula change for future work:
`learn MOLECULE_ID` → inspect `suggestion SUGGESTION_ID` → human approval or dismissal.
Go onboarding does not silently configure a learn binding. Approval changes the
recipe; it does not prove improvement. Evaluation remains manual. See
[Learning](../docs/learning.md).

## Contribute

From a source checkout with the Go toolchain required by `go.mod`:

```sh
cd lab
go test -p 1 ./...
go vet ./...
go build ./cmd/forgecell
./forgecell --help
```

The build writes `lab/forgecell`; `--help` lists the development CLI commands.
For the wider project, see the [repository README](../README.md).

### Exact current Formula approval

Only the currently active, explicitly approved YAML bytes can execute. `lab.json`
stores `currentApproval` with the Formula ID, exact SHA-256, decision ID and
proposal/suggestion identity. Its matching record in `formula-decisions/` must
contain the same exact intent and a completed activation decision. A comment-only
edit changes the digest. `run --formula ID` can select the current approved ID;
it cannot activate injected YAML, historical IDs or restored historical bytes.

Existing Labs without this record fail closed. Run `forgecell init --lab DIR` to
save the current YAML unchanged for explicit review, then use the displayed
`forgecell init --lab DIR --approve PROPOSAL_ID` command. Historical approvals
are evidence for inspection and never silently migrate a Lab. Missing or malformed
YAML must be restored or corrected before it can be reviewed. `doctor` still
inspects saved bindings without granting execution approval.

Bootstrap and learning approvals share `.formula-write.lock`. Activation persists
an exact intent before replacing YAML and configuration; execution remains blocked
until the decision completes. After an interruption, confirm the writer has
stopped and inspect the lock and saved proposal/suggestion before removing a stale
lock, then repeat the same approve command. Conflicting or newer state refuses
replay. An applying approval cannot be dismissed. Historical files and ledgers
remain readable; approval grants neither ticket scope nor delivery authority.

Evaluation needs a **new** exact execution decision. Run
`forgecell evaluate --inputs PLAN --source REPO --out NEW_DIRECTORY` to see the
frozen plan, both exact recipes, hashes, destinations and approval command. Before
approval, no output directory, setup, check or model execution is created. Review
those inputs, then repeat with the displayed `--approve DIGEST`. Changing plan
bytes, either variant, source or destination invalidates the digest. The new
activation provenance is retained in the report and each isolated Lab. A saved
historical learning decision cannot authorize this execution. `--recheck` grades
retained output without approving or rerunning a model.

Direct `__adapter` CLI invocation is unsupported. Approved `run` and `learn`
paths retain the native in-process binding.
