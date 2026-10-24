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
`launcher-incompatible` and `launcher-unknown` (the saved launcher is a different
Forgecell build whose adapter protocol differs or cannot be established; these block
the binding), `launcher-compatible` (a different build with the same adapter protocol;
informational, the binding still runs),
`adapter-mismatch` (the saved adapter differs from the Formula binding) and
`provider-missing` (the bound provider is absent or not an executable file). The
launcher is never executed; its version is read from the adjacent release
`manifest.json` when present, otherwise it is `unknown`. When the adapter matches
and the provider exists, doctor still probes the provider at its own absolute path
with the same fixed read-only calls, so a harness installed outside `PATH` is
checked where it is bound. The binding stays `blocked` until the launcher matches.

Doctor probes the saved binding, the meta binding and each role's provider only when
the active Formula has a current exact-byte approval. For an unapproved Formula it
executes none of them: the saved binding and the meta binding report
`not probed (unapproved)`, no role is probed, doctor is not ready (exit 2) and the
`nextCommand` is `forgecell init --lab DIR` to review and approve the Formula. Every
printed command quotes paths for a POSIX shell, so a Lab path containing `$(...)`,
quotes or spaces pastes as one literal word.

Every result that is not ready carries one `nextCommand`: the provider's login
command (`codex login`, `claude auth login` or `agent login`) when authentication
failed; `forgecell init --lab DIR --harness ID` when a ready provider is available
to rebind (prefixed with the provider's directory on `PATH` when it is bound
outside `PATH`, because init discovers harnesses through `PATH`); otherwise
`forgecell doctor --lab DIR --harness ID` to re-check. A meta role (the learn Atom
or a `harnesses.meta` override) saved on an older Forgecell build, in any
`launcher-*` state including the runnable `launcher-compatible`, instead carries
`forgecell init --lab DIR --rebind CODING_ID`, where `CODING_ID` is the current
coding binding: `--harness` with the meta adapter would rebind the coding harness,
not the learn Atom. Doctor prints it only when `init --rebind` would repoint that
launcher and `CODING_ID` is a harness init discovers ready, and it leaves `ready`,
the exit code and the other JSON fields unchanged.

`init --rebind ID` proposes a reviewed, binding-only change. Besides the coding
binding, it repoints the launcher (`command[0]`) of every native
`[launcher, "__adapter", adapter, provider]` command in the learn Atom and in the
`harnesses.analysis` and `harnesses.meta` overrides that is not on the target
launcher, so a learn Atom stranded on an older build moves with the coding harness.
A launcher is repointed only when it is established as a Forgecell executable
without running it: the running executable, a managed launcher, a launcher beside a
release `manifest.json`, or the launcher the approved default or coding binding
already records. A custom tool that merely has the same argv shape is left
unchanged and named in the proposal note. Adapter, provider, timeout and every
other key are kept, custom commands are never touched, and each rewrite is listed
as `path: old -> new` in the proposal note. The proposal is verified to equal the
approved Formula with only the target binding keys and those recorded launcher
rewrites applied, and `--rebind` refuses with "nothing to rebind" when no key would
change.

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
the default Lab. In the development CLI the default Lab belongs to the repository,
not to the checkout path (see [Labs and repository identity](#labs-and-repository-identity));
`doctor --json` reports its exact path. Existing checkout
`.forgecell` data is never moved or deleted automatically. Pass `--lab .forgecell`
to inspect an older Lab deliberately.

Delivery is a separate review: `deliver` previews the exact file scope without
publishing. A separately approved digest authorizes committing those files,
pushing the isolated branch and opening a **draft PR**. Local checks do not
replace exact-commit GitHub CI.
Merge, deployment and issue closure are not automatic; scope approval does not
authorize them or publication. See [ticket readiness](../docs/ticket-readiness.md)
for delivery, checks, amendments, recovery and execution limits.

## Labs and repository identity

Development build. A fresh clone, a renamed directory or a `git worktree` of the
same repository finds the same default Lab, because the default is keyed by
`OWNER/REPO` from the checkout's `origin` remote (https, `git@` and `ssh://` GitHub
URLs are normalized), not by its path. Resolution reads local git configuration
only: no network, no `gh`, no model.

- **Default directory:** `~/.forgecell/labs/<owner>--<repo>` (lowercase), or
  `FORGECELL_HOME/labs/…` when set. `--lab-name NAME` appends `@<name>`
  (`<owner>--<repo>@<name>`) and is accepted by every command that takes a Lab
  (`init`, `doctor`, `run`, `ledger`, `amend`, `recover`, `issues`, `learn`,
  `suggestion`, `deliver`, `checks`, `loop`). A name is 1–32 letters, digits, `-` or
  `_` and is lowercased; anything else exits 1 before any filesystem access. GitHub
  owner names contain neither `--` nor `@`, so no two repositories (or repository and
  Lab name pairs) share a directory.
- **Legacy directory names:** Labs created as `<owner>-<repo>` or
  `<owner>-<repo>-<name>` keep resolving. When the new-format directory does not
  exist, the old-named Lab is used only if its `lab.json` Formula names the same
  repository (never by its name alone), and stderr suggests renaming it. A new Lab is
  never created beside it. When both exist, the new-format directory wins. If the
  exact old-named directory for this repository exists but cannot be confirmed
  (corrupt or unreadable `lab.json` or Formula, a symbolic link, or no `lab.json` yet
  because onboarding is pending), the CLI exits 1 naming that directory and points to
  `--lab DIR`, a repair or a rename; it never chooses or creates the new-format
  sibling. A legacy-named Lab that belongs to another repository is ignored. When
  several Labs match and one is the exact old name, it is chosen.
- **Reusing an existing Lab:** if exactly one Lab under `FORGECELL_HOME/labs` has an
  active Formula whose `intake.repo` matches the checkout's repository
  (case-insensitive), it is used even when its directory has another name, such as
  an older path-keyed `forgecell-core-<hash>`. The chosen directory is printed on
  stderr; stdout (a `ledger` dump, `doctor --json`) is unchanged.
- **Ambiguity is never guessed:** with more than one match, the command exits 1
  before any side effect, lists every candidate and asks for `--lab DIR`.
  `--lab-name` cannot select the unnamed Lab, so it is not suggested. When the
  repository's own identity directory is among the matches it is chosen, and stderr
  names the other matching Labs.
- **Unreadable Labs:** a Lab with a corrupt `lab.json` or Formula is never matched.
  Stderr names every such Lab, so a new Lab beside one is not created silently. Every
  directory name and error shown this way has terminal escape sequences, control and
  direction-override characters removed.
- **Not a real directory:** if the identity path is a symbolic link (valid or
  dangling) or a file, the CLI refuses and points to `--lab DIR`; it never follows
  the link and creates nothing. If the identity directory belongs to another
  repository, the CLI refuses too and never reuses that Lab.
- **Fallback:** with no `origin`, a non-GitHub remote, an unreadable remote or no git
  executable, the legacy path-keyed default is used and stderr says so;
  `--lab-name` is refused there because there is no identity to name. Repository
  renames and transfers are not tracked.
- **Explicit choices win:** `--lab DIR` (and `FORGECELL_LAB`) skip the remote lookup,
  the Lab scan and the notice. `--lab-name` together with `FORGECELL_LAB`, or with
  `--lab DIR`, is an error naming both rather than a silently ignored name.

## Lab loop

`forgecell loop` starts intake for eligible open Issues of the Formula's repository
and approves a scope gate only where the Formula's policy approves that exact plan.
It never delivers, merges or approves a human gate.

- **Base pinning:** a loop approval is pinned to the plan's frozen base commit, which
  must equal the Molecule workspace's base or the approval is refused and reported.
  The source checkout's `HEAD` may move after intake without stopping the loop; the
  report notes the move. A manual `forgecell run --approve` still checks the current
  `HEAD` and refuses until intake is refreshed.
- **Locks:** the loop holds a per-Molecule advisory lock (`ledgers/<id>.loop-lock`)
  and then the approval lock directory (`ledgers/<id>.lock`). Manual approval,
  `amend` and `recover` take the same advisory lock first, so they fail fast with a
  locked error while a loop works on that Molecule. The loop never removes a lock
  directory.
- **Selection:** `label:` and `milestone:` selections are part of the GitHub query. A
  selection that reaches the 300-Issue cap is an error and starts nothing.
- **Exit codes:** 1 for a loop-level error, 2 when a Molecule this run started or
  approved failed or blocked, and 3 when the final pass left an Issue stalled (an
  approval that never applied, for example because its lock directory is held or left
  behind, or a ledger left running by a crash) or in a state the loop could not
  determine. Otherwise 0: waiting at a human gate, questions and Molecules skipped
  because another loop holds the lock still exit 0. The report ends with the count of
  Issues that made no progress.
- **Interrupted ledgers:** a ledger reported `running` or `allocating` is called
  stalled, not active, only when it has not been written for longer than the
  Formula's analysis timeout and no live process owns its approval lock (and, before
  a scope plan exists, no loop holds the Issue's start lock). The report then points
  to `forgecell ledger <id>`. Anything uncertain keeps the plain active report.
- **Watching:** `forgecell loop --watch [--interval DURATION]` repeats single passes
  (each identical to `--once`, with the same server-side selection) until it receives
  SIGINT or SIGTERM. The interval defaults to 5 minutes and must be at least 1
  minute (`1m`, `90s` and `5m` are accepted; `30s` is not). `--watch` cannot be
  combined with `--once`, and `--interval` needs `--watch`; every invalid
  combination exits 1 before GitHub is called or a Lab is created. Each pass
  re-reads the approved Formula and re-lists open Issues, and prints its own report.
  A stop request ends the watch between passes: it dispatches no further Issue,
  lets Molecules already in flight finish under their usual harness timeouts and
  locks, and starts no new pass. The exit code is the one the final pass would give
  under `--once`. A transient pass error (a failed or hung Issue listing, an
  incomplete ledger inventory) is reported and retried with exponential backoff,
  capped at four intervals; a stop during such a state exits 1. A Formula or
  configuration error (the approved Formula cannot be reloaded, `intake.repo`
  changed, a truncated listing under `label:` or `milestone:`) ends the watch with
  exit 1. The first SIGINT or SIGTERM stops the loop as described; a second one
  terminates the process at once, which is safe because the Molecule and Issue
  locks are advisory flocks that the operating system drops on exit. A stop
  request also cancels a running Issue listing. No pass delivers,
  merges or approves a human gate. Notices about recurrence and pending learning
  are printed after every pass, so they repeat each interval.
- **Learning:** `--learn` is off by default and works with or without `--watch`.
  After each pass (and after that pass's recurrence check) the loop runs the pending
  learn requests for the current approved Formula through the same path as
  `forgecell learn --pending` and prints each result the same way. Without
  `--learn` the loop only reports pending requests. Suggestions stay pending: the
  output offers `forgecell suggestion ID --lab DIR` and the loop never approves or
  dismisses one. A request that fails stays pending, is reported and does not
  change the loop's exit code. To keep a failing request from starting the model
  every pass, the loop skips a request that has failed 3 times (reported as
  `waiting for operator: 3 failed attempts`) or whose last failure is newer than
  three intervals (the default 5 minutes applies without `--watch`). Run
  `forgecell learn --pending` yourself to retry; it is never throttled. A learn run
  cancelled by a stop request is not counted as a failed attempt.
- **Recovery proposals:** when a ledger is reported stalled because it is
  interrupted and no live process owns its approval lock, the loop saves
  `recovery-proposals/<molecule-id>.json` in the Lab (private permissions, written
  atomically, outside `ledgers/`). It holds the Molecule and Issue, the ledger status
  and phase, the latest plan digest, and the exact
  `forgecell recover ID --plan DIGEST --confirm-stopped --lab DIR` command. When the
  latest harness attempt retained a captured tree it also records the ordered
  follow-up `forgecell amend ID --parent DIGEST --plan FILE --lab DIR`, where `FILE`
  is `recovery-proposals/adopt-plans/<molecule-id>.json`: the latest plan with
  `continuation: adopt-failed-tree` and that attempt id. The command is exact and
  runnable once `recover` has succeeded; review the plan file first. A proposal is written only when the ledger holds a plan and its latest
  harness attempt has reconciled stopped-process evidence, which `recover` requires;
  otherwise the report keeps its reason and says why no proposal was made. There is
  one file per Molecule: a later pass over the same state leaves the bytes unchanged
  and reports it as already proposed, and a changed plan digest replaces the file.
  The loop only proposes. It never runs `recover` or `amend`, and you run them only
  after every Lab process has stopped. The report prints each proposal under its
  Issue, and `forgecell doctor` adds a read-only `recoveryProposals` array (absent
  when empty; `schemaVersion` stays `v1`) and a `--verbose` section for proposals
  whose Molecule is still interrupted by the loop's own criteria (the ledger status,
  its age against the analysis timeout, and approval-lock ownership) at the recorded
  plan digest. Doctor writes nothing; each loop pass removes the proposal files that
  no longer apply. Every printed argument is quoted for a POSIX shell.

`forgecell labs [--json]` lists every directory under `FORGECELL_HOME/labs`, legacy
path-keyed Labs included, sorted by name. Each entry shows the repository, active
Formula and binding identifiers, the last Molecule (id, status, start time) and a
count per raw ledger status. The last Molecule is the latest start time among valid
ledgers (ties go to the smaller id) and is shown as unknown when a start time is
unusable or the scan was truncated; records that fail validation are counted as
invalid. A Lab with an unreadable `lab.json` or Formula is listed with an error
instead of stopping the list, symbolic links are listed but not followed, and
ledger reads are bounded. The command is read-only and never prints harness
commands, credentials, ledger bodies or traces. Portable Lab export and import are
tracked separately in #54.

## Harnesses and learning

Native bindings support Codex, Claude Code and Cursor, retaining the harness's
configured model unless the Formula pins one (see below). Choose explicitly with
`init --harness codex`, `claude-code` or `cursor`. Codex coding uses workspace-write and
learning uses read-only mode; Claude Code uses print mode with an exact tool allowlist,
project settings only (`--setting-sources project`), no MCP servers beyond the approved
set (`--strict-mcp-config`) and edits under the checkout's `.claude/` denied; Cursor uses
print mode with its sandbox enabled. These permission modes provide different guarantees;
arbitrary adapters retain their process permissions.

Claude Code coding, formula-improvement (meta) and analysis with extensions attached run
with `--setting-sources project`, so the operator's user settings, plugins and hooks (including
hooks that rewrite tool output) do not change what the harness sees or what the ledger
records. Analysis and review without extensions keep `--safe-mode`. Whether a user-level
`CLAUDE.md`, user agents or user skills still load under `--setting-sources project`, and
whether managed (enterprise) policy settings reach the run, have not been verified.

Project `.claude` files can still load, so they are guarded. The binding fingerprint
covers the project `.claude/settings.json` and `.claude/settings.local.json` committed at
the plan's base commit (never the source checkout's working tree), while edits to
user-level Claude settings no longer change it. A coding run, and a formula-improvement
run started from a directory that holds them, is refused with no harness started when the
base commit carries `.claude/settings*.json`, `.claude/hooks/**` or `.claude/agents/**`:
remove them (for example `git rm -r .claude/settings*.json .claude/hooks .claude/agents`),
commit, and refresh intake. The Formula has no setting that allows project settings. After a
coding run, and before checks, `.claude/**` in the workspace must be byte-identical to the
base commit; creating, editing or deleting anything there, including through repository
code that an allowed Bash entry ran or through a gitignored file, fails the attempt as a
scope violation that names the paths.

Doctor reports the isolation of each adapter as `isolated`, `partial` or `unknown`, each
with its reason. Claude Code is `partial`, because of the unverified items above; Codex is
`partial` (it still reads `CODEX_HOME/config.toml`); Cursor and any custom command are
`unknown`. Codex and Cursor isolation is documented, not changed.

Migration: adapter protocol 3 changes every binding fingerprint, so a plan reviewed
before the upgrade cannot be approved. After upgrading, run `forgecell init --rebind ID`
and refresh intake for each pending ticket; approving such a plan stops with "approved
under adapter protocol N; refresh intake after rebinding" (`2 or earlier` for a ledger
that did not record its protocol), and nothing is started.

Adapter protocol 4, the current protocol, changes the ticket-analysis instruction so the
analysis returns every blocking clarification question at once. Like protocol 3 it
changes every binding fingerprint, so the same rebind and intake refresh apply to a plan
reviewed under protocol 3.

A Formula may pin the provider model with an optional `harness.model` and per-role
`harnesses.<role>.model`. A role override never inherits the default's model, because a
model name only means something to the provider that binding names. A pinned model is
passed as `--model` to the native Claude Code adapter only; Codex and Cursor refuse it,
and a custom command receives it in the request without Forgecell enforcing it. Without
a pin behaviour is unchanged and provenance records `model unpinned`. Each harness
attempt in the Molecule ledger records its `model` and `isolation` mode. Setting or
changing a model is a Formula change, so it needs human approval; no Lab Formula sets
one.

`forgecell init --lab DIR --model ID [--role analysis|coding|meta|review]` proposes that
change as a reviewed Formula proposal, like `--rebind`: it saves a pending proposal, shows
the before/after YAML, and takes effect only after `init --lab DIR --approve PROPOSAL_ID`.
It needs a current exact approval and no pending proposal, and cannot be combined with
`--harness`, `--rebind`, `--meta-harness`, `--meta-command`, the timeout flags, `--review`,
`--approve` or `--dismiss`. The proposal differs from the approved Formula only in `model`
keys; atoms, checks, instructions and timeouts are preserved. Only the `model:` lines of
the approved text are inserted, changed or removed; comments, blank lines, anchors, key
order and quoting everywhere else stay byte for byte. An exact check refuses any other
change, and a binding written in flow style (`harness: {...}`) is refused rather than
rewritten. `ID` is validated with the same rules as `harness.model`.

- Without `--role` it sets `model` on the default `harness` and on every declared
  `harnesses.<role>` override. A role without an override resolves to the default harness
  and inherits its model (analysis and coding); meta without an override runs the learn
  Atom's command, which has no model, and does not inherit it. The proposal summary states
  this for each such role, derived from the resolution code.
- With `--role ROLE` it changes only `harnesses.ROLE`. If that override is not declared the
  proposal is refused, naming the role; run without `--role` or declare the override first.
  Init never creates an override.
- Setting a pin on a binding that is not native Claude Code is refused: the message names
  each Codex, Cursor or custom command binding (Forgecell cannot enforce a pin on a custom
  command), and without `--role` the whole proposal is refused. A Formula that declares no
  `harness` is refused with that reason, not as a custom command.
- `--model ""` removes the pin from every targeted binding that carries one, including
  Codex, Cursor and custom bindings, and lists each one. If none carries a model there is
  nothing to change and no proposal is saved.
- Changing the pin changes the binding fingerprint of the native Claude binding each role
  runs; the summary names only the roles (analysis, coding, meta, review) whose binding's
  model actually changes, and in-flight plans for those roles need a fresh intake.

`doctor` shows each role's pinned model after its readiness text, or `unpinned`; the JSON
role entries carry an additive `model` key that is omitted when unpinned. Doctor makes no
model call.

Bootstrap Formulas set `harness.timeoutMs` (default 900000) and a reviewed ceiling
`harness.maxTimeoutMs` (default 3600000); override them with `init --harness-timeout-ms MS`
and `--max-timeout-ms MS`. Intake may then propose a longer per-Molecule `harnessTimeoutMs`
for larger approved scopes, shown at the scope gate and bounded by the ceiling.

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

## Extensions (development build)

Extensions let a Lab attach a pinned, out-of-process capability provider, such as a
Graphify code index, to the analysis and coding roles without changing Forgecell. They are
**not in the published `0.2.0-preview.1`**, and on this branch only part of the v1 design
exists. The manifest verifier, the Formula grammar, the prepare step and the cache are
built; Codex and Cursor bindings are rejected at proposal. The Claude Code run-time wiring
(and its adapter protocol bump), the scope-gate and ledger records, the `doctor` section and
the `extensionCacheKeep` setting are **planned, not current behaviour**, and until they land
no run attaches an extension. Design:
[extensions design](../docs/design/2026-10-11-extensions-design.md).

Extension output is a hint, never frozen evidence, and never a gate input. An extension
cannot approve, decline, deliver, merge or change a gate, and it does not write to the
coding checkout or the ledger. The "no MCP servers" statement for Claude Code under
[Harnesses and learning](#harnesses-and-learning) is true today for every Formula; once the
wiring lands it will apply to Formulas that attach no extension.

### Declaration

A Formula declares extensions once, then binds them by name to roles:

```yaml
extensions:
  graphify:
    manifest: extensions/graphify.json   # Lab-relative, clean path, no symlinks
    sha256: <64 lowercase hex>           # exact manifest bytes
    required: false                      # default; true blocks a role when unavailable
harness:                                 # the coding role
  binding: claude-code
  command: [...]
  extensions: [graphify]
harnesses:
  analysis:
    binding: claude-code
    command: [...]
    extensions: [graphify]
```

- A name matches `^[a-z][a-z0-9-]{0,31}$`. A role may reference only declared names, and
  only the analysis and coding roles may bind extensions. A `harnesses.coding` override
  replaces the default `harness` list instead of inheriting it.
- The manifest path must stay inside the Lab and not under `workspaces/`, `verification/`
  or `ledgers/`. Parsing is syntactic; it never reads or hashes the manifest.
- Formulas without `extensions` keep their exact bytes and SHA. Adding or changing an
  extension, its digest, `required` or a role's list changes the Formula SHA, so it needs
  human Formula approval. The learn Atom may attach, detach or retune an already declared
  extension; it cannot add a manifest or change a digest.

The manifest is strict JSON of at most 64 KiB (unknown fields, duplicate keys and trailing
data are rejected), `schemaVersion` `v1` and `kind` `mcp-provider`:

- `server`: a pinned `executable` (`path` and `sha256`), an optional pinned `interpreter`
  and `args`.
- `prepare` (optional): the same command shape, plus `timeoutMs` (1 to 1800000) and
  `network` (`none` or `declared`). It builds the provider's cache.
- `tools`: at most 32 exact names matching `^[A-Za-z0-9_-]{1,64}$`, each `read-only` or
  `read-write`.
- `roles`: `analysis`, `coding` or both. Analysis gets only `read-only` tools.
- `dependencies`: pinned files, each with `path`, `sha256`, `role` and `state: "pinned"`.
- `limitations`: free text saying how far to trust the answers.

In `args`, `{{CACHE}}` and `{{SOURCE}}` are the only placeholders, allowed as a whole
argument or as a path prefix such as `{{CACHE}}/graph.json`. The literal path of a pinned
file must not appear in `args`.

[`internal/extension/testdata/graphify.example.json`](internal/extension/testdata/graphify.example.json)
is an AST-only Graphify manifest. Its paths (`/ABSOLUTE/PATH/TO/...`) and digests
(`REPLACE_WITH_SHA256_...`) are placeholders, and its subcommands and flags are
illustrative: confirm them with `graphify --help` before use. Copy it to
`<Lab>/extensions/graphify.json`, replace every placeholder and put that file's SHA-256 in
the Formula. `TestExampleManifestDecodes` decodes it with the placeholders swapped for
temporary pinned files.

### Trust model

- **Pinned files.** The server and prepare executables, their interpreters and every
  dependency are pinned by absolute path and SHA-256. A pinned file is refused when it is a
  symlink, is group- or world-writable, sits in a directory others can write to without the
  sticky bit, or does not match its digest. The manifest bytes themselves must match the
  Formula's `sha256`.
- **Copied before exec.** Prepare copies the verified bytes of the executable and
  interpreter into a private directory and runs the copy, so a swap after verification is
  never executed. The planned wiring does the same for the server. A pinned path in `args`
  is refused so that nothing can point back at the original.
- **Read-only analysis.** Analysis may use only tools the manifest marks `read-only`; coding
  gets every declared tool of a role the manifest lists. A role the manifest does not list
  gets none, and any other role gets none.
- **Declared network.** `prepare.network` is a claim, `none` or `declared`, recorded as one.
  Forgecell does not sandbox it, and approving a prepare step approves running its pinned
  code with your user's permissions.
- **Isolated build.** Prepare reads a snapshot of the plan's base commit (tracked files
  only, no `.git`, read-only) and never the coding workspace. Its environment is a minimal
  `PATH`, a temporary `HOME` and `TMPDIR`, and `LANG=C.UTF-8`; its working directory is
  private and its time and output are bounded.

### Cache and pruning

The cache key is the extension name, the manifest SHA-256 and the base commit. An entry
lives at `<Lab>/extensions/<name>/<manifest-sha>/<base-commit>/` with private permissions.
Prepare builds in a `.partial-<nonce>` directory under a per-key lock. On success a
completion marker records the output tree's SHA-256 and the entry is renamed into place
and made read-only (directories 0500, files 0400). Reuse needs a marker whose digest still
matches the tree, and a failed or timed-out prepare leaves no entry.

Pruning keeps the most recent completed entries of an extension and removes the rest. It
never removes an entry a Molecule still references, an entry whose lock is held, or a
partial build. The planned `extensionCacheKeep` Lab setting (default 5) will choose how
many base commits to keep, and `forgecell doctor` will report cache usage; neither exists
yet.

When an optional (`required: false`) extension cannot be verified or prepared, the planned
behaviour is that it is unavailable for that Molecule and each role runs without it. When
it is `required: true`, the Molecule stops at intake with an actionable message.

### v1 limits

- Claude Code only. Codex and Cursor report `extensions: unsupported`, and a Formula that
  binds an extension to either is rejected at proposal.
- No per-call logging: no MCP request or response is recorded, and the planned ledger entry
  records only identity, cache outcome and tools.
- Analysis with an extension will run without `--safe-mode` and with `--setting-sources
  project`, because Claude Code drops an explicit `--mcp-config` under safe mode. User
  settings, plugins and hooks are excluded. Without built-in tools project settings
  cannot touch the filesystem, but they can steer the analysis; analysis is refused when
  the checkout carries `.claude/settings.json` or `.claude/settings.local.json`.
- Only the `mcp-provider` kind exists. Lab-run evidence, repository-artifact, memory and
  knowledge providers are later kinds.

### Computing digests

Pin a file by its SHA-256 as 64 lowercase hex characters:

```sh
shasum -a 256 /ABSOLUTE/PATH/TO/graphify
shasum -a 256 <Lab>/extensions/graphify.json   # the Formula's `sha256` for the manifest
```

On Linux, `sha256sum FILE` prints the same digest. Compute each digest from the exact file
you pin. Changing a byte of any pinned file, or of the manifest, needs a new digest and,
for the manifest, a Formula change that a human approves.

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

For a quick local loop while you edit, run `go test -short ./...`. It skips the
tests that drive the real Go toolchain (install builds, evaluation replay and
base-flake re-runs) and the ones that wait out real process deadlines over
5 s. CI runs the full suite without `-short`, so run it before you push.

### Gate scenario suite

`TestGateScenarios` in `lab/internal/cli/gate_scenarios_test.go` is one table that
runs every approval and digest gate command against malformed, stale or missing
state and expects it to fail closed with an actionable message and no changes to
the Lab. Every new or changed approval, digest or handoff gate must add rows to
that table and register its command in `gsGates`; the coverage test fails when a
registered gate has no deny row or when the CLI help shows an approval or digest
flag on an unregistered command. Rows must stay offline and deterministic: fake
`git`/`gh`/harness scripts in temporary directories, no network, credentials or
model, and concurrent attempts simulated by pre-creating the lock directory. Run
it with:

```sh
cd lab
go test -run '^TestGateScenarios' ./internal/cli
```

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
