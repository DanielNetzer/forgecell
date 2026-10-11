# Forgecell extensions: pinned capability providers (v1: harness MCP providers)

Status: proposed design, for operator review
Date: 2026-10-11
First extension: a Graphify-style repository index, used by the analysis and coding roles

## 1. Goal and decisions

Forgecell should let capabilities such as memory, knowledge, code indexing and extra
harness tools attach to a Lab **without forking the core**. The core keeps owning
Formulas, gates, evidence, ledgers and learning. Extensions supply context.

Decisions already made:

| Question | Decision |
|---|---|
| Who writes extensions | Anyone. Extensions are **out-of-process executables** in any language, not compiled into Forgecell. |
| v1 scope | **Harness-provided only.** A Formula allowlists MCP providers for harness roles. Lab-run evidence providers, repository-artifact providers, memory and knowledge are later kinds. |
| Roles | **Coding and analysis.** Analysis may use only tools the manifest marks `read-only`. |
| Identity | **Pinned provider manifest**: the server command, args, exact tool allowlist and executable digest, verified before every use. |
| Index build | A pinned **prepare** step runs at the Molecule's base commit, writing into a **Lab-owned cache outside the checkout**. |

### Success criteria

With Graphify attached, compared with the recent v3-Lab baseline:

- fewer analysis evidence requests per intake;
- fewer evidence-cap rejections;
- fewer `scope-change` outcomes caused by a file missing from scope;
- equal or lower coding elapsed time and timeout rate.

Validators, gates, isolation and approval semantics are all unchanged.

### Non-goals (v1)

- Extensions never approve, decline, deliver, merge or change gates.
- Extensions never write to the coding checkout or the ledger.
- MCP answers are **hints**, never frozen evidence.
- Codex and Cursor extension support.
- Per-tool-call logging.
- Lab-run evidence, repository-artifact, memory and knowledge kinds.

## 2. Model

An **extension** is a pinned manifest of a declared `kind`. A Formula declares
extensions once and **binds** them to roles. One shared verifier handles pinning,
verification, materialisation and recording for every kind. It is generalised from the
analysis-wrapper verifier (`harness/analysis_wrapper.go`, #2), so new kinds reuse it
instead of inventing their own trust story.

```
Formula ──declares──> extensions{name → manifest@sha256}
   │                          │
   └─roles bind names──> harness / harnesses.{analysis,coding}.extensions
                              │
Lab ── verify manifest ── prepare (base snapshot → cache) ── wire into harness argv
                              │
Ledger ← extension record (identity, cache outcome, tools)   (no MCP content)
```

## 3. Declaration

### 3.1 Formula

```yaml
extensions:
  graphify:
    manifest: extensions/graphify.json   # Lab-relative, clean path, no symlinks
    sha256: <64 lowercase hex>           # exact manifest bytes
    required: false                      # default; true blocks a role when unavailable
harness:                                 # default binding = coding role
  binding: claude-code
  command: [...]
  extensions: [graphify]
harnesses:
  analysis:
    binding: claude-code
    command: [...]
    extensions: [graphify]
```

Parse rules:

- An extension name must match `^[a-z][a-z0-9-]{0,31}$`, and names are unique.
- `manifest` uses the same path rules as `analysisWrapper` (#2).
- A role may reference only declared names. A reference to an unknown name is a parse
  error.
- `extensions` is `omitempty` on bindings, so the bytes of existing Formulas (and their
  SHAs) are unchanged when no extension is used.
- Adding or changing an extension, its digest, `required`, or a role's list changes the
  Formula SHA. That always means human Formula approval, and it invalidates plans
  approved under the previous Formula, as today.
- The `learn` Atom may propose diffs that attach, detach or retune an *already declared*
  extension. It may not add a manifest or change a digest. A proposal that does is
  rejected.

### 3.2 Manifest (`kind: mcp-provider`, `schemaVersion: v1`)

```json
{
  "schemaVersion": "v1",
  "kind": "mcp-provider",
  "name": "graphify",
  "server":  { "executable": {"path": "/abs/path/graphify", "sha256": "…"},
               "interpreter": null,
               "args": ["mcp", "--graph", "{{CACHE}}/graph.json"] },
  "prepare": { "executable": {"path": "/abs/path/graphify", "sha256": "…"},
               "args": ["build", "{{SOURCE}}", "--out", "{{CACHE}}", "--no-semantic"],
               "timeoutMs": 600000,
               "network": "none" },
  "tools": [ {"name": "query_graph",  "access": "read-only"},
             {"name": "find_callers", "access": "read-only"} ],
  "roles": ["analysis", "coding"],
  "dependencies": [ {"path": "/abs/…", "sha256": "…", "state": "pinned"} ],
  "limitations": "AST-only graph; no LLM semantic pass; answers are not authoritative."
}
```

Verification reuses #2's rules and adds a few of its own:

- **Files.** Executable, interpreter and dependencies are pinned files. They are refused
  if they are symlinks, group- or world-writable, in a writable non-sticky parent, or
  owned by someone other than root or the current uid. Their digests are checked at
  verify time, and verified bytes are copied into a private 0700 directory (executables
  0500) before they run (#2's TOCTOU fix). The interpreter is re-hashed immediately
  before exec (#78).
- **Placeholders.** The only allowed placeholders are `{{CACHE}}` and `{{SOURCE}}`,
  substituted as whole arguments or as a path prefix. Any other `{{…}}` is a
  verification error. The literal original paths of pinned files must not appear in
  `args` (#78).
- **Tools.** Every tool is listed by exact name (`^[A-Za-z0-9_-]{1,64}$`), with `access`
  set to either `read-only` or `read-write`. Analysis gets only `read-only` tools. At most
  32 tools.
- **Roles.** A role may bind an extension only if the manifest lists that role.
- **Network.** `prepare.network` is a **declared claim** (`none | declared`), recorded as
  such. Forgecell does not sandbox it, and the scope gate says so.
- **Unknown fields.** The manifest is size-bounded (64 KiB) and strictly decoded:
  unknown fields, duplicate keys and trailing data are all rejected.

## 4. Prepare and cache lifecycle

**Trigger.** Prepare runs before the first role of a Molecule that uses the extension,
normally at intake for analysis. Coding reuses the result.

**Input.** A read-only snapshot of the plan's base commit, made with
`git archive <base> | tar -x` into a temp directory (`{{SOURCE}}`). It contains tracked
files only, has no `.git`, and is made read-only after extraction. Git runs with
`-c core.fsmonitor=false -c core.hooksPath=/dev/null`, as in #55. The coding workspace is
never passed to prepare.

**Cache.**

- The key is `(extension name, manifest sha256, base commit)`. The directory is
  `<Lab>/extensions/<name>/<manifest-sha>/<base-commit>/`, permissions 0700.
- Prepare writes into `.partial-<nonce>/` under a per-key lock (`mkdir` lock with an
  owner PID, the same convention as the Molecule lock). On success, a completion marker
  records the output tree's SHA-256 digest and the cache is atomically renamed into
  place.
- Once complete, the entry is made read-only (directories 0500, files 0400) so the
  server can read it but not alter it.
- Reuse requires a complete marker whose recorded digest matches the tree.

**Process rules:**

- the pinned, copied executable;
- an explicit minimal environment containing only `PATH` (fixed and minimal), `HOME`
  (temp), `TMPDIR` (private) and `LANG=C.UTF-8`, plus any manifest-declared variables
  that pass the #2 deny list;
- cwd is a private temp directory;
- the manifest's timeout (at most 30 minutes) and bounded output;
- process-group kill and reconciliation, including the Linux reaping grace from #68.

**Failure.**

- When `required: false` (the default), the extension becomes **unavailable** for the
  Molecule. Each role runs without it, and the ledger and gate show the reason.
- When `required: true`, the Molecule stops at intake with an actionable message.
- A failed prepare never leaves a partial cache entry.

**Housekeeping.** `forgecell doctor` reports cache usage. A Lab setting
`extensionCacheKeep` (default 5) keeps the most recent base commits per extension. An
entry referenced by a waiting or running Molecule is never pruned.

## 5. Harness wiring

### 5.1 Claude Code (the only adapter in v1)

**Coding.** Today the coding run uses
`--strict-mcp-config --mcp-config '{"mcpServers":{}}'`. With extensions, the Lab
generates `mcpServers` from the verified manifests:

```json
{"mcpServers": {"graphify": {"command": "<private copy>", "args": ["mcp", "--graph", "<cache>/graph.json"], "env": {}}}}
```

- `--strict-mcp-config` stays.
- Each allowlisted tool is appended to `--allowedTools` as `mcp__<name>__<tool>`.
- Unlisted tools are denied by `--permission-mode dontAsk`.
- The derived Bash allowlist (#35), `--setting-sources user` and the
  `Edit(/.claude/**)` deny (#60) are unchanged.

**Analysis.** Today analysis uses
`--tools "" --safe-mode --strict-mcp-config --mcp-config '{"mcpServers":{}}' --no-chrome --disable-slash-commands`.
With extensions, the generated config contains **only `read-only` tools**, and
`--allowedTools` lists exactly those.

**Open item, to be verified against the live CLI before implementation (first plan
step).** `--safe-mode` documents disabling MCP servers, so it may ignore an explicit
`--mcp-config`.

- If it does, the analysis invocation *with extensions* replaces `--safe-mode` with
  `--setting-sources ""` (or the narrowest equivalent the probe confirms) and keeps
  `--tools ""`, strict MCP, `--no-chrome` and `--disable-slash-commands`.
- Analysis *without* extensions keeps its current flags byte-for-byte.
- The probe result is recorded in the plan PR.

### 5.2 Codex and Cursor

Both report `extensions: unsupported` in `AdapterCapabilities` and doctor. Binding an
extension to a role on those adapters is rejected at proposal time, with the adapter
reason (#38's capability validation), so it never fails at run time. Support can be
added later via Codex `-c mcp_servers.<name>.*` overrides and Cursor's MCP configuration.

### 5.3 Protocol pin

This changes the provider invocation contract.

- `protocol_contract_test.go` gains golden cases with one extension for coding and
  analysis. The pinned digest changes, and `AdapterProtocolVersion` becomes `"2"` (#76).
- Existing bindings classify as `launcher-incompatible` until they are rebound. On v3,
  whose launcher carries a manifest, that is one reviewed `init --rebind` (#83).
- The bump lands in the same PR as the wiring.

## 6. Ledger, gates, doctor, learning

### 6.1 Ledger

Each Molecule gains `extensions: [{name, kind, manifestSha256, roles, tools,
cache: {key, outcome: built|reused|unavailable, treeSha256, prepare: {code, elapsedMs,
timedOut}, reason}}]`.

- The field is `omitempty`, so historical ledgers verify unchanged (the projection rules
  of #60 and #74 apply).
- No MCP request or response content is recorded in v1.

### 6.2 Scope gate

The gate prints, per role: each extension, its manifest digest, its tools with their
access, the cache outcome and the declared network claim. A required extension that is
unavailable blocks with a fix command.

### 6.3 Gate policies (#66)

Gate policies are unchanged, so an extension can never contribute to a policy decision.
The policy's recorded evidence lists the active extensions, for auditability.

### 6.4 Doctor

Doctor adds an `extensions` section:

- manifest parse and verify, and executable digest match;
- whether each role binding is allowed for its adapter;
- cache entries and size;
- one concrete fix command per problem.

Doctor never runs prepare or the server.

### 6.5 Learning

`learn` sees extension outcomes in the ledgers and may propose attaching, detaching or
retuning a declared extension (§3.1), with the usual evidence and evaluation plan.

## 7. Error handling (summary)

| Condition | Result |
|---|---|
| Manifest digest mismatch or verification failure | Treated like a prepare failure: unavailable, or blocking if `required` |
| Executable swapped after verification | Caught by the copy-then-hash step: unavailable or blocking, never executed |
| Prepare timeout, non-zero exit or overflow | Unavailable or blocking. A partial cache entry is removed; the process group is reconciled |
| Concurrent prepare for the same key | Second waits on the lock or reuses the result. A stale lock is reported, never stolen |
| Server fails during a role | The harness reports a tool error. The run continues under the harness's normal rules, and the ledger records that the extension was available |
| Extension bound on an unsupported adapter | Rejected at Formula proposal |

## 8. Testing

Every test uses fake executables. No real Graphify, MCP server or model calls.

- **Manifest:**
  - each verification rule, including symlinked or writable executables, wrong owner,
    digest mismatch, unknown placeholders, original path in args, unlisted or overlong
    tool names, a role not listed and an unknown kind;
  - strict decode of unknown fields, duplicates and oversize manifests.
- **Formula:**
  - parse and validation of `extensions` and role bindings;
  - SHA unchanged when no extensions are present;
  - `learn` cannot add manifests or change digests.
- **Prepare and cache:**
  - snapshot is read-only and contains no `.git`;
  - atomic build and reuse;
  - concurrent same-key builds;
  - a timeout leaves no partial entry;
  - degrade versus `required` behaviour;
  - pruning never removes a referenced entry.
- **Wiring:**
  - golden argv for coding and analysis, with and without extensions;
  - analysis receives only `read-only` tools;
  - the `mcp__` allowlist entries are exact;
  - the protocol-2 pin;
  - Codex and Cursor are rejected.
- **End to end with fake `claude` and a fake prepare script:**
  - intake and coding run with an extension;
  - the capture shows no checkout writes;
  - the ledger record is correct;
  - historical ledgers still verify.

## 9. Rollout

1. Live probe of the Claude analysis flags (§5.1), with the result recorded.
2. Implement the mechanism as Forgecell Molecules. Likely slices:
   1. manifest kind, verifier generalisation and Formula grammar;
   2. prepare and cache;
   3. Claude wiring, protocol 2, gate, ledger and doctor.
3. Ship an example `docs/examples/extensions/graphify.json` (AST-only, `--no-semantic`).
4. Dogfood in the v3 Lab:
   1. rebind to protocol 2;
   2. attach Graphify to analysis and coding (operator Formula approval);
   3. compare the next about 5 Molecules with the baseline metrics in §1.

## 10. Future kinds (same envelope and verifier)

- `evidence-provider`: the Lab runs it and freezes the output digest into plan evidence.
  Intake can then *cite* it, not just receive hints.
- `repository-artifact`: a committed index such as `graph.json`, with a freshness check
  against the base commit.
- `memory`: post-Molecule store plus pre-intake recall, in an extension-owned directory
  outside the checkout.
- `knowledge`: an OKF bundle, for example product knowledge distilled from reviewed
  findings, used by intake as untrusted context.
- Codex and Cursor `mcp-provider` support.
