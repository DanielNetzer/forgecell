# Installing Forgecell from an agent

A person may ask Codex, Grok, or another assistant to install Forgecell. The installing
assistant and the coding harness selected for the Lab need not be the same product.
The same Bootstrap Assay and Formula approval govern terminal and agent onboarding.

## Agent flow

1. Install the compiled CLI using the one-line command in [Install](install.md).
   The core source is [MIT licensed](../LICENSE); release packages do not require source access.
   Keep local Lab ledgers, credentials, environment files and raw harness traces private.
   Ticket-readiness commands described below are development-build features until a new release is published.
2. In the target repository, run `forgecell doctor --json`. Check the exit code and JSON:
   installed CLIs, authentication, capability readiness, selection reasoning and next actions.
   The development CLI also reports `labDir`: the checkout's private Lab under
   `~/.forgecell/labs/`. The currently published preview needs an explicit, consistent
   `--lab` path to store state outside the checkout; see [Install](install.md).
3. Run `forgecell init --json`. This performs observation only and saves a proposal when
   selection is ready, or preserves an already approved Formula.
   Use `--harness codex`, `--harness claude-code`, or `--harness cursor` when the person
   explicitly chooses a harness or the environment is ambiguous.
4. Explain the selected harness, why it was selected, the proposed coding binding,
   and what a future run can do. Fresh init binds coding only: no learn Atom command is
   created by default. Current-source functionality offers explicit opt-in with
   `forgecell init --meta-harness codex --json` (also `claude-code` or `cursor`),
   or `forgecell init --meta-command '["/absolute/meta-command","arg"]' --json`.
   These flags are not included in published v0.2.0-preview.2.
   Native opt-in requires observed installation/authentication readiness and supported
   meta operation; unavailable or unknown support saves no proposal and provides BYO guidance.
   BYO commands are never probed; their capability and containment remain unknown.
   Opt-in proposes one trailing learn Atom with separate argv and a 900000 ms timeout.
   Review command, permission limits, ledger inputs and exact before/after YAML before
   deciding. See [Learning](learning.md).
   Show the saved Formula with `forgecell init --lab <same-lab> --review <proposal-id>`
   (add `--json` for structured review). Ask for a decision if the user
   has not already explicitly authorized this binding. “Install” alone is not permission to
   start changing a ticket's code or silently approve the Formula.
5. After approval, call `forgecell init --approve <proposalId> --json` with the same `--lab`
   directory. This applies the saved YAML, not a regenerated proposal. Use
   `forgecell init --lab <same-lab> --dismiss <proposal-id> --json` for a denial.
   Changed proposals fail integrity checks; stale approval requires a new proposal.
6. Run a user-selected issue after Formula approval. In the development build, `run` first
   may invoke read-only ticket analysis and then waits at the scope gate. Explain exact
   paths, acceptance criteria,
   check commands, environment/network limits and unknowns. Only explicit approval of that
   plan permits `run ISSUE --approve DIGEST`. Do not treat installation or Formula approval
   as approval of model-selected commands, checks or publication.
   See [ticket readiness](ticket-readiness.md).

## Output and saved review contract

Preparation (`init --json`) returns `status`, `repository`, `candidates`, `selection`
and `note`, plus `metaBinding`, with optional `bindingCapabilities`, `proposal` and `beforeYaml`.
Initial init output has no `nextAction`. A proposal contains the exact `yaml`,
`yamlHash`, ID, status and saved provenance; it is not an active Formula.

Saved review (`init --lab <same-lab> --review <proposal-id> --json`) returns
`proposal`, `verified`, `repository`, `harness`, `atoms`, `repositoryContext`,
`reviewCommand`, `nextAction`, `capabilityEvidence`, `capabilityEvidenceSource`,
`metaBinding` and `beforeYaml`. Meta reporting separates adapter support, observed
installation/authentication, unknown model access and unverified containment.
New meta observations and exact original YAML are bound into proposal identities;
altered, removed or downgraded metadata is rejected before approval and recovery.
Historical proposals remain readable without rewriting.
`nextAction` is a descriptive string, not a command/argv object. `reviewCommand`
is also display text, not an argv execution contract. Construct arguments explicitly
from the documented commands and replace placeholders with the person's choices.

Review displays the exact saved Formula YAML and verified hashes without discovery,
activation or writes; it works outside the repository. `verified: true` proves saved
integrity, not current capability readiness or that approval will succeed. Capability
evidence is labeled `saved observation; not current verification`; historical proposals
without observations report unknowns. Use `doctor` for current probes.
Successful decision JSON (`--approve` or `--dismiss`) is the proposal itself, not the
preparation or review wrapper. Inspect its status and any incomplete activation state;
interrupted activation must be recovered before execution, following the saved review's
resume instruction.

Both terminal and JSON modes are noninteractive: terminal output displays the Formula
and manual review/decision commands, with no approval prompts or edit interaction.

| Command | Exit behavior |
|---|---|
| `init` preparation | 0 for `pending` or `unchanged`; 2 for other returned statuses |
| Saved review / successful decision | 0 |
| `doctor` | 0 only when the selected workflow is `conditional`, Git and GitHub authentication succeed, selection is `ready`, and any saved binding is `ready`; otherwise 2 |
| `run` | 0 when waiting; 2 when failed or blocked |
| Errors | 1 |

An init exit 0 does not mean a ticket is runnable. Formula approval activates exact
recipe bytes; ticket scope, checks and publication require their separate approvals.

## Interpret the states

- **Missing:** install the supported executable. A config directory is not an installation.
- **Login required:** use the harness's login flow (`codex login`, `claude auth login`,
  `agent login`). Never ask the person to paste credentials into the conversation.
- **Unsupported / unknown:** capability or authentication checks could not establish readiness.
  Inspect the reported blocker rather than assuming success from a binary's presence.
- **Ambiguous:** one explicit harness choice is needed; non-interactive mode never guesses.
- **Awaiting approval:** proposal saved, nothing activated and no model invoked.
- **Approved / denied:** decision recorded. Approval activates the recipe, not a ticket run.

Adapter support and installed isolation controls are separate capability fields.
Authentication does not prove model access. Cursor preserves its coding binding but
ticket analysis is unsupported because action-capable MCP/plugin isolation is unverified.
A conditional workflow means analysis can be attempted, not end-to-end success. Custom
commands are preserved but are not safely probed by doctor.

Readiness checks do not prove model quota, network availability, or permission to execute
all repository tests. Those are observed during real runs and recorded honestly.

## Selection and preservation

Explicit `--harness` choice comes first. Otherwise preserve the existing approved binding,
then prefer an identified current harness environment, a single project configuration, or
one uniquely ready installation. Multiple candidates are not resolved by catalog order.
Environment markers are hints about context, not a universal operating-system default.

Re-init preserves pending proposals; review or dismiss before preparing another.
Interrupted activation retains its exact saved recovery decision. Stale approvals are
refused. Re-init preserves the full approved recipe. Explicit rebinding changes the coding binding
while retaining learned instructions, custom Atom order and the separately configured learn
binding. It does not silently migrate an existing meta harness.

## Adapter boundaries

Native adapters are compiled into the Go CLI and preserve the harness's configured model.

- Codex: non-interactive `exec`, workspace-write for coding and read-only for learning.
- Claude Code: print mode, `dontAsk`, user-level settings only (`--setting-sources user`), no MCP
  servers (`--strict-mcp-config`), edits under the checkout's `.claude/` denied (Claude Code
  hot-reloads settings, so a run cannot widen its own permissions), and an exact tool allowlist for coding: file/read tools,
  `git status`/`git diff`, plus the approved plan's check commands and the Formula's
  `repositoryContext` suggested checks (see the [lab README](../lab/README.md#harnesses-and-learning)).
  Commit, push and publishing commands are never added. A denied tool call is never
  escalated or retried with broader permissions. During coding it is recorded as evidence
  (`permissionDenials` on the attempt result) and does not discard an otherwise valid
  outcome; ticket analysis and learning still treat any denial as a failure. Learning
  disables built-in tools and project MCP servers for that invocation.
- Cursor: print mode with sandbox enabled, preserving the configured approval mode; ask mode for learning.
  No `--auto-review`, `--force`, `--yolo`, approval bypass or automatic MCP approval is added.

These are provider-specific permission modes, not equivalent security guarantees. Existing
harness settings, trusted repository instructions and integrations still matter. Forgecell
records outcomes; it does not claim a harness exit proves correct code or production shipping.

CLI references checked for this implementation:
[Cursor parameters](https://cursor.com/docs/cli/reference/parameters),
[Cursor authentication](https://cursor.com/docs/cli/reference/authentication),
[Claude programmatic use](https://code.claude.com/docs/en/headless),
and the installed Codex `exec --help` / `login status` commands.

Meta provider controls are requests, not verified containment. Codex meta requests a
read-only sandbox and approval policy never but lacks ticket analysis's additional
tool/MCP/plugin isolation and temporary working directory. Claude meta disables
built-in tools and uses strict empty MCP configuration, but lacks analysis safe-mode
controls. Cursor meta requests sandbox-enabled ask mode; MCP/plugin isolation remains
unverified and ask mode is not verified read-only containment. Meta retains the supplied
working directory. No-edit and supplied-evidence-only prompts are declarations.
Arbitrary BYO commands run with the invoking local process's permissions.

Activation alone invokes no model. A later `learn MOLECULE_ID` selects eligible finished
same-snapshot ledgers and saves only a pending suggestion. Formula mutation requires a
separate human suggestion approval.
