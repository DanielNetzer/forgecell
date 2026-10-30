# Forgecell plugins: engine-called MCP integrations for trackers, forges and reviewers

Status: proposed design, for operator review
Date: 2026-10-30
Replaces the plan in #259 (GitLab + Jira). Absorbs #260 (outside code reviewers). Extends
`docs/design/2026-10-11-extensions-design.md`, whose §10 named this direction.

## 1. Goal and decisions

Forgecell talks to the outside world in three places that today are hard-wired to GitHub:

- **Where work comes from:** the tracker.
- **Where changes go:** the forge.
- **Who else reviews them:** outside review agents.

The operator's team uses **GitLab** for code and **Jira** for intake. Other teams will want
Linear, Bitbucket, Gitea, Azure DevOps or their own review bot. The goal is **one small,
intuitive plugin interface** that lets anyone add the next integration without forking
Forgecell, while keeping Forgecell's trust model intact.

The design rests on one distinction. **MCP inside the harness gives the model tools; a
Forgecell plugin gives the engine an integration.** Intake, delivery, CI evidence and
review findings are engine steps:

- they happen at defined points;
- they use frozen inputs;
- writes happen only behind a human gate;
- they work when no harness session runs;
- they behave the same for Claude Code, Codex and Cursor;
- they are recorded in the ledger.

So the plugin protocol is MCP, the format contributors already know, but **the engine is
the only caller**.

Decisions already made (operator, 2026-10-29/30):

| Question | Decision |
|---|---|
| Protocol | A plugin **is an MCP server** (JSON-RPC 2.0, stdio transport) that exposes a fixed tool set for its kind. Forgecell gains a minimal MCP *client*. |
| Who calls write tools | **Only the engine**, at a named step after the human gate that authorizes it. Write tools are never exposed to a harness. |
| Model context | A plugin's **read-only** tools may also be attached to a harness role as an ordinary extension (existing mechanism) for context, for example a linked Jira ticket. This is optional and separate. |
| Kinds (v1) | `tracker`, `forge`, `reviewer`. `memory` comes later (§11). |
| GitHub | Becomes the **first implementation** behind the same Go interfaces, with no behaviour change. It stays built-in Go code, not a subprocess. |
| First plugins | GitLab `forge` (gitlab.com first), Jira `tracker`, then the `reviewer` kind (#260). |
| Governance | A plugin is enabled by a **reviewed Formula change** that pins its manifest digest, like extensions and harness bindings. |
| Credentials | Plugins read their own credentials from the environment variables they declare, or from their own CLI login. **Forgecell never stores, logs or forwards tokens.** |
| Contribution | `forgecell plugin new` scaffolds a plugin; `forgecell plugin test` runs a conformance kit before anyone reviews it. |

### Success criteria

- A GitLab project with Jira intake runs the whole loop from `forgecell ui`:
  1. Set up your Lab;
  2. Issues from a Jira JQL or project;
  3. plan and approval;
  4. coding and checks with the baseline from `.gitlab-ci.yml`;
  5. a draft MR;
  6. pipeline evidence.
- Moving GitHub behind the interfaces changes no observable behaviour. The existing
  characterization tests pass unchanged.
- A new tracker plugin (for example Linear) can be written outside this repository in any
  language. It passes `forgecell plugin test` and is enabled with one reviewed Formula
  change, with no Forgecell release needed.

### Non-goals (v1)

- No marketplace, registry or auto-install. Plugins are local executables that you install
  yourself, pinned by digest.
- No remote (HTTP/SSE) MCP transport and no OAuth flows inside Forgecell. Remote servers such
  as the hosted Atlassian MCP are a future transport (§11).
- Plugins never approve, merge, deploy, close issues or change gates.
- No model-initiated writes through a plugin, ever.
- Codex and Cursor still lack harness-side extension wiring (unchanged). Plugins don't
  depend on it, because the engine calls them.

### Anti-goals: these would make it a failure even if it works

- A plugin interface so general that writing a plugin needs Forgecell internals knowledge.
- Any path where untrusted text (an issue body, an MR comment, a review finding) can trigger a
  write, an approval or a retry.
- GitHub users seeing any change in behaviour or wording.

## 2. Model

```
              ┌──────────── engine (Go) ────────────┐
 Formula ───▶ │ intake ── tracker ─┐                │
 (pins)       │ delivery ─ forge ──┼── Go interface │
              │ CI evidence forge ─┤   per kind     │
              │ findings ─ reviewer┘                │
              └────────────┬────────────────────────┘
                           │  built-in (github)  OR  mcp adapter
                           ▼                             ▼
                    gh / git (today)          plugin = MCP server (stdio)
                                              any language, pinned manifest
```

- **Engine side.** Every integration point calls a **Go interface per kind**
  (`tracker.Tracker`, `forge.Forge`, `reviewer.Reviewer`; §4). Implementations are either
  **built-in** (GitHub, wrapping today's `gh`/`git` code) or **`mcpadapter`**, one generic
  adapter that maps interface methods to `tools/call` on a plugin process.
- **Plugin side.** A plugin is a normal MCP server that exposes the tool names and JSON
  schemas of its kind's **contract** (§4). It knows nothing about Formulas, ledgers or gates.
- **Configuration.** The Formula names which implementation serves each kind (§3). The
  conformance kit (§8) runs the same suite against both built-in and plugin implementations.

## 3. Declaration

### 3.1 Formula

Two Formula-level selectors name the implementations:
- `intake.source` already exists (`formula/formula.go:110-113`, default `github-issues`).
- A new `forge` block replaces the unread `ship.via` atom key (`onboarding/init.go:673`).

```yaml
intake:
  source: jira               # github-issues (built-in) | <plugin name>
  repo: welltech-com/backend # unchanged meaning: the forge project this Lab ships to
  config:                    # opaque to the engine, passed to the tracker plugin, bounded
    jql: 'project = ENG AND labels = forgecell AND statusCategory != Done'
forge:
  plugin: gitlab             # github (built-in, default) | <plugin name>
  project: welltech-com/backend   # defaults to intake.repo
reviewers:                   # optional; the reviewer kind
  - name: bugbot
    autoFix: high            # #260 policy; never approves
plugins:                     # pinned like extensions
  jira:   {manifest: plugins/jira/plugin.json,   sha256: "<hex>", required: true}
  gitlab: {manifest: plugins/gitlab/plugin.json, sha256: "<hex>", required: true}
  bugbot: {manifest: plugins/bugbot/plugin.json, sha256: "<hex>", required: false}
```

- **Defaults.** Missing keys keep today's behaviour exactly: `github-issues`, `github`, no
  reviewers.
- **Legacy atom keys.** The intake atom's `source` and the ship atom's `via` (written by
  `init` today) are accepted only if they are absent or equal the Formula-level value.
  Anything else is a validation error with a plain reason. *(My call: one source of truth,
  and old Formulas stay valid.)*
- **Plugin settings.** `intake.config` and each reviewer's settings are JSON objects of at
  most 4 KiB. They're passed to the plugin's `*_describe` call, and the plugin validates
  them.
- **Review.** Changing any of these keys is a Formula change, reviewed and approved like a
  rebind. The Formula view (#255) shows it as a readable diff.

### 3.2 Plugin manifest

*My call:* a plugin manifest is **the extension manifest** (`extension/manifest.go`: strict
JSON, 64 KiB, exact keys, `pinned.File` server with its own sha256). `kind` gains three
values and the manifest gets two new keys:

```json
{
  "schemaVersion": "v1",
  "kind": "forge",
  "name": "gitlab",
  "contract": "forgecell.forge/v1",
  "server": {"path": "bin/forgecell-gitlab", "sha256": "<hex>", "args": []},
  "tools": [
    {"name": "forge_describe", "access": "read-only"},
    {"name": "forge_publish_draft", "access": "read-write"}
  ],
  "roles": [],
  "needs": {"env": ["GITLAB_TOKEN"], "network": ["gitlab.com"], "minPollSeconds": 60},
  "dependencies": [],
  "limitations": "gitlab.com only in v1"
}
```

- **What is pinned.** The server **executable itself** is pinned by digest, as for
  extensions, not only the manifest. An interpreted plugin pins its entry script.
  The interpreter (for example `python3` from `PATH`) is listed under `dependencies`, which
  are shown at review but not pinned. That's the same trust level extensions have today.
- **One manifest serves both paths.** For engine calls, the contract tools must all be
  listed. For an optional harness attachment, `roles` names the harness roles, and only
  `read-only` tools are attached (the existing extension path). `read-write` tools can never
  be attached to a harness.
- **Scrubbed environment.** The plugin process gets `PATH`, `HOME` (a temporary per-call
  dir), `LANG`, plus exactly the `needs.env` names. Values are never logged or stored.
- **Polling.** `needs.minPollSeconds` (at least 60, at most 3600) is the slowest cadence the
  sync engine may poll this plugin at. After a `rate limited` reason the engine backs off
  exponentially, up to 15 minutes. *(My call: per-plugin, because Jira and GitLab limits
  differ.)*

### 3.3 Lab, project and Issue identity

*My calls, made so that slice 1 stays a pure refactor and no stored data is migrated:*

- **Project.** The engine passes `{project}` from `forge.project` (default `intake.repo`) to
  every forge call, and `{config}` from `intake.config` to every tracker call. Plugins never
  guess the project from the git remote.
- **Lab key.** GitHub Labs keep today's key (`OWNER/REPO` from `origin`,
  `cli/lab_identity.go:30`). A plugin forge's Lab is keyed by `forge_describe.identity`
  (`host/path`, for example `gitlab.com/welltech-com/backend`). The two never collide,
  because GitHub keys have no host part.
- **Issue keys.**
  - `molecule.Issue` gains `Key string` beside the existing `Number int64`.
  - The GitHub built-in keeps writing `Number`, and `Key` is derived as `#N` for display
    only, so existing ledgers, Molecule ids (`mol-%d-…`, `readiness_run.go:439`), the queue
    and the UI request bounds are untouched.
  - A plugin tracker writes `Key`, plus `Number` 0. Its Molecule id uses a sanitized key
    slug: `mol-eng-1234-<nonce>`, lower-case, `[a-z0-9-]`, at most 40 characters.
  - Readers accept both forms. The about 34 `.Number` call sites move behind a small
    `IssueRef` helper in slice 1, with no behaviour change for numbers.

## 4. Kind contracts (v1)

**Contract ownership (my call):** contracts live in this repository and are owned by its
maintainers. `v1` freezes when slice 2 merges. After that, only optional additions are
allowed (new optional input fields, new optional output fields, new optional tools). Any
breaking change is `v2`, and the engine supports v1 and v2 side by side for at least one
minor release.

Each contract is a set of MCP tools with fixed names and **strict JSON input and output
schemas**. The schemas ship in the repository at `lab/internal/plugin/contract/*.json`.

- **Versioning.** The contract has a version (`forgecell.<kind>/v1`). A plugin declares
  extra optional capabilities in `*_describe`. The engine never requires a tool the contract
  marks optional.
- **Access level.** Every tool is marked `read` or `write`. Only `write` tools change remote
  state, and the engine calls them only at the step named below.
- **Outputs.** Strings are bounded and sanitized by the engine after the plugin returns
  (§6). Plugins don't need to be careful for Forgecell to stay safe.

### 4.1 `tracker`: where work comes from (replaces `intake/*`, `forgesync` issue reads)

| Tool | Access | Input → Output | Engine caller |
|---|---|---|---|
| `tracker_describe` | read | `{config}` → `{displayName, keyPattern, states[], selectors[]}` | doctor, Set up your Lab, Formula view |
| `tracker_list` | read | `{selector, cursor?, limit≤100}` → `{items[{key, title, labels[], milestone?, updatedAt, state}], nextCursor?, total?}` | Issues view, loop, queue |
| `tracker_get` | read | `{key}` → `{key, title, body, state, stateReason?, url, updatedAt, labels[], milestone?}` | intake (frozen as evidence) |
| `tracker_states` | read | `{keys[≤50]}` → `{states{key: {state, stateReason?, closedAt?, reopenedAt?}}}` | sync engine (#242 rows) |

- **Keys.** Keys are opaque strings that match `keyPattern`, for example `ENG-1234` for Jira
  or `#123` for GitHub. Molecule ids and UI copy use the key verbatim.
- **Selectors.** Selectors are plugin-defined names the loop can use, for example
  `jql:<saved filter>`, `label:x` or `sprint:current`. `oldest`, `label:` and `milestone:`
  remain the GitHub built-in's.
- **Writes.** The tracker has **no write tools in v1**. Closing or transitioning tickets stays
  a human action. A `tracker_comment` write tool for posting the Molecule outcome is a v2
  candidate, behind the document Atom's gate.

### 4.2 `forge`: where changes go (replaces `delivery/*`, `checks`, `readiness/policy.go`)

| Tool | Access | Input → Output | Engine caller |
|---|---|---|---|
| `forge_describe` | read | `{project}` → `{identity, host, displayName, changeNoun, defaultBranch, ciKind, webURL}` | identity, onboarding, UI wording |
| `forge_policy` | read | `{branch}` → `{requiredChecks[], requiredReviews, protected}` | readiness (`ReadRemotePolicy`) |
| `forge_find_change` | read | `{headBranch}` → `{changes[{ref, url, state, draft, headCommit}]}` | idempotent publish |
| `forge_publish_draft` | **write** | `{headBranch, base, title, body, commit}` → `{ref, url, draft:true, headCommit}` | `deliver` **after** the publish approval only |
| `forge_change` | read | `{ref}` → `{state: open\|closed\|merged, draft, headCommit, mergedAt?, mergeCommit?, url}` | lifecycle, sync (#242) |
| `forge_checks` | read | `{ref, commit}` → `{checks[{name, status, conclusion, url?}], complete}` | `forgecell checks` (CI evidence) |
| `forge_comments` | read | `{ref, since?}` → `{comments[{id, author, isBot, body, path?, line?, createdAt}]}` | reviewer kind, display |

- **Every forge call carries `{project}`** from §3.3, even though the tables list only
  the tool-specific fields.
- **Branch push stays in the engine.** The engine runs `git push`, as today, using the
  repository's own remote and the operator's git credentials. The plugin only creates and
  reads the change request. This keeps the code transfer in one audited place.
- **CI baseline derivation stays engine-side** as local file parsing, like
  `onboarding/baseline.go`, selected by `ciKind`: `github-actions` (today) or `gitlab-ci`
  (new parser for `.gitlab-ci.yml` jobs and their `script:` lines). It keeps the same
  closed-set, no-injection rules and the 16-check cap. A plugin never supplies commands that
  Forgecell would execute.

### 4.3 `reviewer`: outside review agents (#260)

| Tool | Access | Input → Output | Engine caller |
|---|---|---|---|
| `reviewer_describe` | read | `{config}` → `{displayName, severities[]}` | doctor, Formula view |
| `reviewer_findings` | read | `{forge, ref, commit}` → `{status: pending\|complete, findings[{id, severity: high\|medium\|low\|info, path?, line?, title, body, resolved}]}` | after publish, polled read-only |

- **Inputs.** Most review bots post to the forge, so a reviewer plugin usually reads them
  through its own API, or the engine hands it `forge_comments` and `forge_checks` results.
  Either is allowed. The engine passes the forge results in the request, and the plugin
  doesn't need forge credentials.
- **Findings feed back into the loop** as decided for #260, with this gate model (operator,
  2026-10-30):
  1. Findings are **untrusted evidence**, recorded against the exact pushed commit.
  2. Findings at or above the Formula's `autoFix` severity become a **proposed plan
     revision**: acceptance criteria, plus referenced files as proposed scope.
  3. **You approve that revision at the scope gate.** This is the only human gate in a fix
     round.
  4. Coding, the checks and Forgecell's own review run. If all are green, the engine
     **pushes the new commit to the same branch automatically**, so the existing draft
     change request updates. No second delivery approval is needed. The push is bound to the
     approved revision's digest and the checked commit, and re-reading `forge_change` must
     show the new `headCommit`.
  5. There are at most 3 rounds per change request, and the same finding repeating after a
     fix stops and asks.

  A finding never approves, a fix round never merges, and the change request stays a draft.
  Findings below `autoFix` are shown in the Inbox and the Molecule view, to pick from or
  dismiss with a reason.
- **No forge write tool is needed for fix rounds.** Updating the change request is a
  `git push` to its existing branch, done by the engine. `forge_publish_draft` is used only
  for the first publication.

## 5. Engine wiring

- **Packages:**
  - `lab/internal/plugin`: manifest load and verify (reusing `extension.Load`), a scrubbed
    process launch (reusing `process.Run`), and a **minimal MCP client** (`initialize`,
    `tools/list`, `tools/call`). It is stdio-only, has request and response size caps, and
    applies per-call timeouts from the contract.
  - `lab/internal/plugin/contract`: the JSON schemas and a strict decoder for each tool.
  - `lab/internal/tracker`, `lab/internal/forge`, `lab/internal/reviewer`: the Go interfaces
    plus the GitHub built-ins. The built-ins move today's code: `intake/open_issues.go`,
    `molecule/run.go:461 readIssue`, `delivery/publish.go`, `checks/checks.go`,
    `readiness/policy.go`, and the `forgesync` GraphQL reads.
- **Process lifetime.** One plugin process per engine operation (an intake, a publish, a
  sync pass), not a daemon. It starts, runs `initialize`, checks that `tools/list` covers the
  required tools, makes the call or calls, then shuts down. This is simple, crash-safe and
  needs no shared state. The sync engine may reuse one process across a 60-second pass.
- **Startup check.** On `initialize`, the engine requires the plugin to report the contract
  version from its manifest. A mismatch fails with a plain reason.
- **The harness never sees write tools.** If a plugin's read tools are also attached to a
  harness role for context, they go through the existing extension path
  (`molecule/extensions.go`). `access: read-only` is enforced and write tools are filtered
  out by name.

## 6. Trust, bounds and failure

- **Every plugin output is untrusted.**
  - It is decoded strictly against the contract schema: unknown keys and duplicate keys are
    refused, and the encoding must be valid UTF-8.
  - It is size-capped per tool, for example 64 KiB for `tracker_get` and 4 MB for list
    pages.
  - Text is sanitized with the existing helpers: `forgesync.text`, `ReviewDisplayText`, and
    `redactSecrets` on anything that is logged.
- **Writes are gate-bound.** `forge_publish_draft` runs only inside `deliver --approve DIGEST`.
  The arguments are derived from the approved preview: branch, base, the exact commit, and
  the title you typed. They never come from plugin or model output. After the call, the
  engine reads the change back with `forge_change` and checks `headCommit` against the
  approved commit before it records the receipt. This is today's reconciliation, generalized.
- **Failures map to fixed reasons:**
  - not installed;
  - manifest changed;
  - contract mismatch;
  - auth missing;
  - rate limited;
  - timeout;
  - output too large;
  - schema invalid;
  - network.

  Raw plugin stderr is kept in the ledger, bounded and redacted, but never served by the UI.
  The UI shows the reason and the plugin name.
- **Ledger.** Each call records the plugin name, manifest sha256, tool, a digest of the
  arguments, a digest of the result, duration and the outcome reason. For `tracker_get`, the
  frozen issue snapshot is recorded as evidence, as today.
- **Doctor and the Formula view** show each plugin's manifest, kind, contract, declared env
  and network, and a live `*_describe` health check.

## 7. User experience

- **Set up your Lab (#263)** gains two choices:
  - **Where do Issues come from?** GitHub Issues, or any installed tracker plugin.
  - **Where do changes go?** Detected from `origin`: GitHub built-in, or any installed forge
    plugin.

  Each choice shows the plugin's `needs` ("set `GITLAB_TOKEN`"), and a **Check Again** button
  calls `*_describe`.
- **Issues view.** Shows tracker keys and states verbatim, with links from the plugin's
  `url`. Selectors come from `tracker_describe`.
- **Molecule view.** Uses `forge_describe.changeNoun` ("pull request" or "merge request")
  and `displayName`: for example "Draft merge request" on GitLab. Wording comes from the
  forge's description, never hardcoded per plugin in the UI.
- **CLI.** Three commands:
  - `forgecell plugin list`;
  - `forgecell plugin doctor [name]`;
  - `forgecell plugin test <manifest>` (§8).

## 8. Contributing a plugin

- **`forgecell plugin new --kind tracker|forge|reviewer --lang go|python|typescript <name>`**
  writes a runnable skeleton:
  - an MCP server with every contract tool stubbed;
  - the manifest;
  - a README;
  - a fixture file.
- **`forgecell plugin test <manifest>`** is the conformance kit, the same suite the built-ins
  pass. It runs in two modes:
  1. **Contract mode (offline).** `tools/list` covers the contract, the schemas match, and
     error cases return MCP errors, not crashes. It also checks bounds (oversized output is
     refused), timeouts, and determinism of `*_describe`.
  2. **Live mode (opt-in, read-only by default).** It runs against a target the contributor
     names, for example a scratch GitLab project, and checks
     list → get → states consistency, pagination and rate-limit behaviour. Write tests
     (`forge_publish_draft`) run only with `--allow-write --target <scratch project>`, and
     only create a draft.
- **Docs.** `docs/plugins.md` covers the contract tables, a 60-line Python example tracker,
  and the review checklist used in code review.

## 9. Testing (Forgecell side)

- **No behaviour change.** Characterization tests for every GitHub path are written in slice
  0, before slice 1, and must pass unchanged after GitHub moves behind the interfaces.
- **MCP client.** Tested against an in-repo fake MCP server (Go, `t.TempDir`). It covers
  framing, size caps, timeouts, a malformed JSON-RPC, a crashing server and a slow server,
  with no real network.
- **Contract decoder.** Golden fixtures per tool, including hostile ones: huge strings, bidi
  characters, duplicate keys, wrong types.
- **Gate binding.** Tests show that `forge_publish_draft` arguments come only from the
  approved preview, and that a `headCommit` mismatch after publish refuses the receipt.
- **GitLab and Jira plugins.** Tested with their own fake HTTP servers. Live checks run
  manually on the operator's scratch GitLab project, through the outside-repo trial pattern
  used for v0.3.0.

## 10. Rollout: slices, each through Forgecell

0. **Characterization tests** *(my call: a separate slice)*. Pin today's observable GitHub
   behaviour across `intake`, `delivery`, `checks`, `readiness/policy.go`, `forgesync` and
   the Issue-number paths: argv sent to `gh`/`git`, receipts, ledger fields and UI JSON. Each
   test goes through the runner seams, with no network. Slice 1 must pass these unchanged.
1. **Interfaces + GitHub behind them.** `tracker`, `forge` and `reviewer` Go interfaces, the
   GitHub built-ins wrapping today's code, and the `IssueRef` helper (§3.3). No behaviour
   change, proven by slice 0.
2. **Plugin runtime.**
   - Manifest kinds, plus Formula `plugins:`, `intake.source`, `forge:` and `reviewers:`.
   - The MCP client and `mcpadapter`.
   - The contract schemas.
   - `plugin list|doctor`, and the conformance kit's contract mode.
3. **GitLab forge plugin**, plus the engine's `gitlab-ci` baseline parser and forge-identity
   Lab keys. Includes a live trial on a scratch gitlab.com project.
4. **Jira tracker plugin** with a GitLab or GitHub forge, plus Set up your Lab choices and
   Issues view wording.
5. **Reviewer kind**: findings → reviewed plan revision → same change request (#260), with a
   first reviewer plugin, BugBot or the GitLab Duo comments.
6. **Contributor kit**: `plugin new`, the live mode of `plugin test`, and `docs/plugins.md`
   with the Python example.

Each slice follows `lab/CLAUDE.md`:
- base tests keep compiling;
- old defaults stay unless configured;
- modules stay at or under 600 lines.

Slices 3 and 4 can run in parallel once 2 merges.

## 11. Open architecture roadmap

The plugin kinds in this design open Forgecell **at its integration edges**. The core
stays closed **by design**, and no plugin may weaken it:
- human gates;
- digest binding;
- the ledger;
- the closed-set rule for commands Forgecell executes;
- "never merges or deploys".

The extension points below are committed so that "open" is a stated direction with
contracts, not an accident. Each one gets its own design before it is built.

| Order | Extension point | What it opens | Power it never gets |
|---|---|---|---|
| v1 (this doc) | `tracker`, `forge`, `reviewer` plugins; MCP extensions for model context | New issue sources, forges, review agents and model tools, in any language, with no release | No write without a gate; no merge or deploy |
| v1.x | **`observer` kind: typed lifecycle events** (Molecule created, gate waiting, checks completed, change published, finding raised, Molecule finished) delivered to observer plugins | Chat notifications (Slack, Teams), dashboards, metrics, audit export | Receive-only: an observer can't gate, write, retry or change state |
| v1.x | **Harness adapters as plugins**: the versioned adapter protocol (today protocol 6, built-in for claude-code, codex and cursor) served by an external process | New coding agents (Aider, Gemini CLI, OpenHands…) plug in like trackers | Outcomes stay strictly schema-validated and untrusted; no access to gates, ledger writes or credentials beyond the harness's own |
| later | **Evidence Atoms** (#5): a custom Formula step implemented by a plugin, for example a security scan, licence check or preview URL | Team-specific steps inside the loop | Evidence only: it can fail a step but never approve one |
| later | **`ci` kind**: a plugin proposes check commands from other CI systems (Jenkins, CircleCI, Buildkite) | Required-check baselines beyond GitHub Actions and GitLab CI | Proposed commands still pass the engine's closed-set validation and your Formula review |
| later | **A versioned read API** for ledgers, receipts and Formulas | Stable integration for external tools, instead of reading internal files | Read-only |
| later | **OS-level plugin sandboxing**, which enforces declared network and filesystem access | Safer third-party plugins | — |
| later | **`memory` kind** (below) | Cross-harness knowledge | Proposals approved by you; never instructions from untrusted text |

## 12. Future (not v1)

- **`memory` kind.** A Lab-owned, reviewed store of repo facts and operator decisions, with
  provenance. It's rendered into every role's prompt for every harness, and exported to
  `CLAUDE.md`, `AGENTS.md` and `.cursor/rules`. It shares the governance of learn
  suggestions.
- **Remote MCP transport** (streamable HTTP) with a plugin-owned OAuth flow, so hosted servers
  such as Atlassian's or GitLab's can be used directly, through a mapping manifest.
- **Tracker writes** (`tracker_comment`, `tracker_transition`) behind the document Atom's gate.
- **Self-managed GitLab, Bitbucket, Gitea, Linear, Azure DevOps:** plugins, not core work.

## 13. Left to the builder

- Exact Go package boundaries and names, as long as §5's responsibilities hold.
- Per-tool size caps and timeouts, within the bounds stated.
- How the GitHub built-in shares code with `forgesync` (merge them or keep them separate).
- The scaffolding templates' exact contents, and which three languages ship first, if Go,
  Python and TypeScript turn out impractical.
