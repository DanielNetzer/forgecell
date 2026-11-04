# Write a Forgecell plugin

A plugin connects Forgecell to a tracker, a forge or an outside reviewer. It is an MCP
server on stdio that serves one of three contracts. You pin it by digest, and a Formula
enables it. Forgecell starts it, calls its tools and stops it. A plugin never drives
Forgecell.

Plugins ship in the v0.4.0 release.

| Kind | Contract | What it does |
|---|---|---|
| `tracker` | `forgecell.tracker/v1` | Lists Issues and reads one Issue and its state. |
| `forge` | `forgecell.forge/v1` | Reads change requests, checks and review comments, and opens a draft change request. |
| `reviewer` | `forgecell.reviewer/v1` | Returns findings for a change request at one commit. |

The first-party [review bot comments plugin](forgecomments/README.md) is a complete
reviewer plugin.

## How Forgecell runs a plugin

- It runs a private copy of the pinned server, with the pinned interpreter for a Python or
  TypeScript server. Nothing next to the server is there at run time, so the server must
  not read files beside it.
- The environment holds exactly `PATH`, `HOME`, `LANG` and the `needs.env` names that are
  set. `HOME` is a private run directory.
- It speaks newline-delimited JSON-RPC 2.0: `initialize`, `notifications/initialized`,
  `tools/list` and `tools/call`. The `initialize` result must use protocol version
  `2025-06-18` and declare the contract in `capabilities.experimental.forgecell.contract`.
- Every tool answer must carry a `structuredContent` object that matches the contract's
  output schema exactly. Unknown fields, missing required fields and values out of bounds
  are refused.
- Every message, every answer and every call is bounded. An answer over the tool's
  `maxOutputBytes`, or later than its `timeoutMs`, fails the call.
- Write only protocol messages to stdout.

## Contracts

The contract files are the source of truth:
[`tracker.v1.json`](../internal/plugin/contract/tracker.v1.json),
[`forge.v1.json`](../internal/plugin/contract/forge.v1.json) and
[`reviewer.v1.json`](../internal/plugin/contract/reviewer.v1.json). The tables below are a
summary. Where they differ, the file wins.

Every tool in a contract is required. Access `read` maps to `read-only` in the manifest
and `write` to `read-write`. A plugin's `tools/list` must list every contract tool with an
input schema of type object that declares each required property with the contract's JSON
type, and requires nothing the contract does not require.

Every `config` input is an object of at most 64 properties. A tracker gets the Formula's
`intake.config`, and a reviewer its `reviewers[].config`.

### Tracker: `forgecell.tracker/v1`

| Tool | Access | Required inputs | Main output | Max output | Timeout |
|---|---|---|---|---|---|
| `tracker_describe` | read | `config` | `displayName` (1-100), `keyPattern` (1-200, a regular expression for keys), `states` (at most 32), `selectors` (at most 64, each 1-200), optional `capabilities` (at most 32) | 64 KiB | 15 s |
| `tracker_list` | read | `config`, `selector` (1-200), `limit` (integer 1-100); optional `cursor` (1-1024) | `items` (at most 100), each `key` (1-100), `title` (at most 300), `labels` (at most 20), `updatedAt` (RFC 3339), `state` (`open` or `closed`), optional `milestone`; optional `nextCursor`, `total` | 2 MiB | 30 s |
| `tracker_get` | read | `config`, `key` (1-100) | `key`, `title`, `body` (at most 65536), `state` (`open` or `closed`), `url` (http or https), `updatedAt`, `labels`; optional `stateReason`, `milestone` | 128 KiB | 30 s |
| `tracker_states` | read | `config`, `keys` (at most 50, each 1-100) | `states`: an object of at most 50 keys, each `{state}` (`open` or `closed`), optional `stateReason`, `closedAt`, `reopenedAt` | 256 KiB | 30 s |

`keyPattern` is a Go RE2 regular expression, so it has no lookaround and no
backreferences. Forgecell matches it against the whole key: the engine anchors it with
`^` and `$` itself, so `DEMO-[0-9]+` and `^DEMO-[0-9]+$` match the same keys, and a
pattern that only matches part of a key never admits it.

Forgecell passes `tracker_list` only a selector the description admits: a selector it
lists exactly, or a listed prefix that ends in `:` followed by a non-empty value. For
example, a tracker that lists `oldest` and `label:` gets `oldest` or `label:bug`, never a
bare `label:`. `plugin test --live` lists the first described selector exactly as written,
so make that one a complete selector.

### Forge: `forgecell.forge/v1`

`project` is always 1-200 characters, and a commit is 40 or 64 lowercase hex digits.

| Tool | Access | Required inputs | Main output | Max output | Timeout |
|---|---|---|---|---|---|
| `forge_describe` | read | `project` | `identity` (1-300), `host` (a lowercase host, optional port), `displayName` (1-100), `changeNoun` (1-40), `defaultBranch` (1-255), `ciKind` (`github-actions`, `gitlab-ci` or `none`), `webURL`; optional `capabilities` | 64 KiB | 15 s |
| `forge_policy` | read | `project`, `branch` (1-255) | `requiredChecks` (at most 64), `requiredReviews` (0-100), `protected` (boolean) | 64 KiB | 15 s |
| `forge_find_change` | read | `project`, `headBranch` (1-255) | `changes` (at most 20), each `ref` (1-200), `url`, `state` (`open`, `closed` or `merged`), `draft`, `headCommit` | 64 KiB | 15 s |
| `forge_publish_draft` | write | `project`, `headBranch`, `base`, `title` (1-300), `body` (at most 65536), `commit` | `ref`, `url`, `draft` (always `true`), `headCommit` | 64 KiB | 60 s |
| `forge_change` | read | `project`, `ref` (1-200) | `state`, `draft`, `headCommit`, `url`; optional `mergedAt`, `mergeCommit` | 64 KiB | 15 s |
| `forge_checks` | read | `project`, `ref`, `commit` | `checks` (at most 200), each `name`, `status` (`queued`, `in_progress` or `completed`), `conclusion`, optional `url`; `complete` (boolean) | 512 KiB | 30 s |
| `forge_comments` | read | `project`, `ref`; optional `since` (RFC 3339) | `comments` (at most 500), each `id` (1-100), `author`, `isBot`, `body` (at most 65536), `createdAt`, optional `path`, `line` | 2 MiB | 30 s |

### Reviewer: `forgecell.reviewer/v1`

| Tool | Access | Required inputs | Main output | Max output | Timeout |
|---|---|---|---|---|---|
| `reviewer_describe` | read | `config` | `displayName` (1-100), `severities` (at most 4 of `high`, `medium`, `low`, `info`), optional `capabilities` | 64 KiB | 15 s |
| `reviewer_findings` | read | `forge` (an object of at most 16 properties), `ref` (1-200), `commit`; optional `config`, `comments` (at most 500), `checks` (at most 200) | `status` (`pending` or `complete`), `findings` (at most 500), each `id` (1-100), `severity`, `title` (1-300), `body` (at most 65536), `resolved`, optional `path`, `line` | 2 MiB | 60 s |

A reviewer asks for forge data through the capabilities its `reviewer_describe` lists:

| Capability | What the reviewer gets |
|---|---|
| `forge-comments` | The forge's review comments, as `comments`, in every `reviewer_findings` call. |
| `forge-checks` | The forge's checks for the commit, as `checks`, in every `reviewer_findings` call. |

A reviewer that lists neither gets only `forge`, `ref`, `commit` and its `config`.

## Errors

A tool that can't answer returns a result with `isError: true` and a `structuredContent`
of `{"reason": "...", "message": "..."}`. Forgecell shows only the reason and the tool
name, never your message, so keep the message short and free of secrets. Use one of these
reasons:

| Reason | Use it when |
|---|---|
| `auth-missing` | A credential the plugin needs is unset, expired or refused (for example HTTP 401 or 403). |
| `rate-limited` | The service asks you to slow down (for example HTTP 429). `plugin test --live` stops there and skips the remaining checks, with no retry. |
| `network` | The service can't be reached: DNS, connection, TLS or a timeout on the plugin's side. |
| `failed` | Anything else, including arguments your handler refuses and a key that doesn't exist. |

Any other reason, or none, is read as `failed`. Answer a malformed request (bad JSON, an
unknown method or an unknown tool) with a JSON-RPC error instead. Forgecell names its own
failures with further reasons, such as `timeout`, `output-too-large`, `schema-invalid`,
`contract-mismatch`, `not-installed` and `manifest-changed`. A plugin doesn't send those.

In a Go skeleton, a handler names its reason with `fail(reason, message)` and the
constants `authMissing`, `rateLimited`, `network` and `failed`. `fail` turns any other
reason into `failed`, and an error that is not from `fail` is answered with reason
`failed` and a fixed message. The skeleton also enforces each tool's contract
`maxOutputBytes` and `timeoutMs` itself, from the `limits` table `plugin new` copies from
the contract: a larger result is never sent, and a call past its timeout is answered then.
Both fail with reason `failed`. Until a timed-out handler returns, every later call fails.

## The manifest

`plugin.json` is strict JSON: no unknown or repeated keys. `plugin new` writes a template,
`plugin.json.tmpl`, and `plugin pin` fills it in.

| Field | Meaning |
|---|---|
| `schemaVersion` | Always `v1`. |
| `kind` | `tracker`, `forge` or `reviewer`. |
| `name` | Lowercase letters, digits and dashes, starting with a letter, at most 32 characters. Not `github` or `github-issues`, which a Formula reserves. |
| `contract` | The kind's contract: `forgecell.tracker/v1`, `forgecell.forge/v1` or `forgecell.reviewer/v1`. |
| `server.executable` | `{path, sha256}`: the absolute path of the server and its SHA-256. |
| `server.interpreter` | For a Python or TypeScript server only, `{path, sha256}` of the interpreter that runs it. |
| `server.args` | The server's arguments. No `{{` placeholders. |
| `tools` | At most 32 `{name, access}` entries. Every contract tool, with `access` `read-only` for a read tool and `read-write` for a write tool. |
| `roles` | Empty, or `analysis` and `coding`, each at most once. |
| `needs.env` | At most 16 environment variable names to pass through when set. `PATH`, `HOME` and `LANG` are set by Forgecell and can't be listed, nor can any `LD_*` or `DYLD_*` name. |
| `needs.network` | At most 16 lowercase hostnames the plugin talks to. Forgecell doesn't enforce this yet; reviewers check it. |
| `needs.minPollSeconds` | 60 to 3600: the shortest interval, in seconds, at which the engine may poll this plugin. |
| `dependencies` | Other files the server needs, each `{path, sha256, role, state}` with state `pinned`. No two share a file name. |
| `limitations` | Plain text of at most 2000 characters: what the plugin doesn't do. |

### Pinning the interpreter

A Python or TypeScript plugin pins its interpreter as well as its script, so an
interpreter upgrade changes the digest and Forgecell refuses to start the old pin. Pass
the interpreter's absolute path with every symlink resolved:

```sh
python3 -c 'import os, sys; print(os.path.realpath(sys.executable))'
node -p 'require("fs").realpathSync(process.execPath)'
```

The skeletons need Python 3.9 or later, or Node 22.18 or later. The server and the
interpreter must be owned by you or root, not group- or world-writable, with one hard link
(a root-owned system interpreter may have more), and in no directory that another user
owns or can write without the sticky bit. `plugin pin` refuses any other.

On macOS, Homebrew's prefix (`/opt/homebrew`, or `/usr/local` on Intel) is group-writable
by `admin`, so `plugin pin` refuses a Homebrew `python3` or `node`. What works instead: the
system `/usr/bin/python3`, which resolves into the Command Line Tools; an interpreter under
`$HOME` from pyenv, nvm or fnm; or `chmod g-w` on the Homebrew directories above the
interpreter, accepting that this can affect other admin users of that Homebrew.

## Walkthrough

1. **Make a skeleton.**

   ```sh
   forgecell plugin new --kind tracker --lang python --dir ./demo-tracker demo-tracker
   ```

   `--kind` is `tracker`, `forge` or `reviewer`, and `--lang` is `go`, `python` or
   `typescript`. Without `--dir`, the skeleton goes into `./<name>`. `plugin new` writes
   the manifest template `plugin.json.tmpl`, not `plugin.json`, so there is nothing to test
   yet. Once you build and pin it (steps 3 and 4), the unedited skeleton passes
   `plugin test`: its describe tool answers a fixed example and every other tool fails
   with `not implemented yet`.

2. **Edit it.** Replace the tools one at a time. Keep the advertised input schemas as the
   contract has them, check every argument yourself, and keep every answer within its
   bounds.

3. **Build it or pick the interpreter.** For Go,
   `GOWORK=off GOFLAGS= go -C ./demo-tracker build -o demo-tracker .`. `GOWORK=off` builds
   the skeleton's module on its own even when a `go.work` file in a directory above would
   join it to other modules, and the empty `GOFLAGS` keeps flags from your environment,
   such as `-mod=vendor`, from changing what is built. For TypeScript,
   `node ./demo-tracker/build.mjs` builds `server.js`. For Python, there is nothing to
   build: find the interpreter's path as shown above.

4. **Pin it.**

   ```sh
   forgecell plugin pin ./demo-tracker --server ./demo-tracker/server.py --interpreter <python3>
   ```

   For Go, give `--server` the built binary and leave out `--interpreter`. `plugin pin`
   writes `plugin.json` from `plugin.json.tmpl`, and prints the manifest's own SHA-256 and
   the Formula's `plugins:` entry (step 7). Pin again after every change to the server or
   the interpreter.

5. **Test it offline.**

   ```sh
   forgecell plugin test ./demo-tracker/plugin.json [--json]
   ```

   Contract mode starts the plugin, checks `initialize` and `tools/list`, calls the
   describe tool twice and wants the same answer, sends malformed arguments to every read
   tool and wants an error each time, and checks the output and time bounds. It never calls
   a write tool. It exits 0 when every check passes and 2 when one fails.

6. **Test it live.**

   ```sh
   forgecell plugin test ./demo-tracker/plugin.json --live --target DEMO --config ./config.json
   ```

   Live mode runs contract mode, then calls the read tools against a real target. `--config`
   names a JSON object file of at most 64 KiB and 64 properties, passed to the plugin as
   `config`. The demo tracker needs no settings, so its `config.json` holds just `{}`.
   A tracker run lists the first described selector, then gets and reads the
   states of up to 5 keys. A forge run takes `--change REF --commit SHA` to read one change
   request, and only with `--allow-write --head BRANCH --base BRANCH --commit SHA` opens one
   draft change request. A reviewer run needs `--change` and `--commit`.

7. **Add it to a Formula.** Put the directory in your Lab as `plugins/<name>`, pin it there,
   and list it under `plugins:` by name, manifest path and manifest digest. Then name it as
   the tracker (`intake.source`), the forge (`forge.plugin`) or a reviewer
   (`reviewers[].plugin`):

   ```yaml
   intake:
     source: demo-tracker   # keep the intake block's other keys
     config: {}             # optional: the tracker's config object
   plugins:
     - {name: demo-tracker, manifest: plugins/demo-tracker/plugin.json, sha256: "<manifest hex>"}
   ```

   A forge is `forge: {plugin: <name>, project: <project>}`, and a reviewer is
   `reviewers: [{plugin: <name>, autoFix: high}]` with an optional `config`. `plugins:`
   takes exactly `name`, `manifest` and `sha256`, and at most 16 entries; there is no
   `required` key. `plugin pin` prints this entry with the manifest digest filled in.
   Approve the changed Formula as any other.

8. **Check it in the Lab.**

   ```sh
   forgecell plugin doctor [NAME] [--lab DIR] [--lab-name NAME] [--json]
   ```

   `plugin doctor` starts each plugin the approved Formula declares, or only `NAME`, runs
   its describe tool and names any `needs.env` entry that is unset. `forgecell plugin list`
   takes the same Lab flags, loads every pinned manifest against its digest and starts
   nothing.

## A complete example tracker

This tracker serves `forgecell.tracker/v1` from three Issues in memory. It uses only the
Python standard library. Save it as `server.py` in a skeleton made with
`forgecell plugin new --kind tracker --lang python demo-tracker`, then pin and test it as
above. It passes contract mode and live mode with `--target DEMO`. The guide's own test
runs both on this exact block.

<!-- forgecell:example-tracker -->
```python
import json
import sys

ISSUES = {key: {"key": key, "title": title, "state": state, "updatedAt": "2026-11-01T09:00:00Z", "labels": []}
          for key, title, state in [("DEMO-1", "Add a login page", "open"),
                                    ("DEMO-2", "Fix the footer links", "closed"), ("DEMO-3", "Write the release notes", "open")]}
OBJ, STR = {"type": "object"}, {"type": "string"}
TOOLS = {"tracker_describe": {"config": OBJ}, "tracker_get": {"config": OBJ, "key": STR},  # every property is required
         "tracker_list": {"config": OBJ, "selector": STR, "limit": {"type": "integer"}},
         "tracker_states": {"config": OBJ, "keys": {"type": "array", "items": STR}}}
TYPES = {"object": dict, "string": str, "integer": int, "array": list}
MAX_LINE = 1 << 20  # bounds one request, in bytes

def run(name, a):  # answers one tool call, or raises ValueError with a fixed message
    if not isinstance(a, dict) or set(a) != set(TOOLS[name]) or any(
            isinstance(a[k], bool) or not isinstance(a[k], TYPES[p["type"]]) for k, p in TOOLS[name].items()):
        raise ValueError("the arguments do not match the input schema")
    if name == "tracker_describe":
        return {"displayName": "Demo tracker", "keyPattern": "^DEMO-[0-9]+$", "states": ["open", "closed"], "selectors": ["all"]}
    if name == "tracker_list":
        if a["selector"] != "all" or not 1 <= a["limit"] <= 100:
            raise ValueError("unknown selector, or limit out of bounds")
        return {"items": list(ISSUES.values())[:a["limit"]], "total": len(ISSUES)}
    if name == "tracker_get":
        if a["key"] not in ISSUES:
            raise ValueError("no such Issue")
        return dict(ISSUES[a["key"]], body="A demo Issue.", url="https://tracker.example/" + a["key"])
    if len(a["keys"]) > 50 or not all(isinstance(k, str) for k in a["keys"]):
        raise ValueError("keys must be at most 50 strings")
    return {"states": {k: {"state": ISSUES[k]["state"]} for k in a["keys"] if k in ISSUES}}

def reply(msg):  # the result of one request; LookupError for an unknown method or tool
    method, params = msg.get("method"), msg.get("params", {})
    if method == "initialize":
        return {"protocolVersion": "2025-06-18", "serverInfo": {"name": "demo-tracker", "version": "1.0.0"},
                "capabilities": {"tools": {}, "experimental": {"forgecell": {"contract": "forgecell.tracker/v1"}}}}
    if method == "tools/list":
        return {"tools": [{"name": n, "inputSchema": {"type": "object", "properties": p, "required": list(p),
                                                      "additionalProperties": False}} for n, p in TOOLS.items()]}
    if method != "tools/call" or params.get("name") not in TOOLS:
        raise LookupError("unknown method or tool")
    try:
        out = run(params["name"], params.get("arguments"))
    except ValueError as e:
        return {"isError": True, "content": [{"type": "text", "text": str(e)}], "structuredContent": {"reason": "failed", "message": str(e)}}
    return {"content": [{"type": "text", "text": json.dumps(out)}], "structuredContent": out}

while line := sys.stdin.buffer.readline(MAX_LINE):
    if len(line) == MAX_LINE and not line.endswith(b"\n"):
        sys.exit("demo-tracker: a request is over the size bound")
    msg = None
    try:
        msg = json.loads(line)
        if "id" not in msg:
            continue  # a notification gets no reply
        out = {"jsonrpc": "2.0", "id": msg["id"], "result": reply(msg)}
    except Exception as e:  # bad JSON, an unknown method or tool, or a malformed request
        code = -32700 if msg is None else -32601 if isinstance(e, LookupError) else -32600
        out = {"jsonrpc": "2.0", "id": msg.get("id") if isinstance(msg, dict) else None, "error": {"code": code, "message": "invalid request"}}
    print(json.dumps(out), flush=True)
```

## Review checklist

Code review of a plugin checks each of these:

- **Bounds.** Every input the handler uses is checked against the contract's bounds before
  use, and every answer stays within its output schema and `maxOutputBytes`. Paging,
  retries and total work are bounded. Hitting a bound fails or truncates visibly; it never
  silently drops data that the contract says is there.
- **Untrusted input.** Arguments, config and everything the service returns can be
  malformed, huge or hostile. Validate them, never pass them to a shell, and never let
  them build a path or a URL outside the service.
- **No secrets in output or logs.** No credential, token, environment value or absolute
  local path in a tool answer, an error message, stderr or a log.
- **Fixed error reasons.** Every failure returns `auth-missing`, `rate-limited`, `network`
  or `failed`, as above.
- **Deterministic describe.** The `*_describe` tool returns the same answer for the same
  config every time, with no clock, counter or random value in it.
- **Write tools only behind `read-write`.** Only a contract write tool changes anything,
  and the manifest lists it as `read-write`. Every read tool is `read-only` and changes
  nothing.
- **No network outside `needs.network`.** The plugin talks only to the hosts it lists, and
  an offline plugin lists none.
