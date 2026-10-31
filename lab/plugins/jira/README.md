# Jira tracker plugin

`forgecell-jira` is a first-party tracker plugin. It is an MCP server on stdio that
serves the `forgecell.tracker/v1` contract over the Jira Cloud REST API v3. It lists,
reads and checks the state of Issues in one Jira project. It only reads: it never
comments on, transitions or edits an Issue.

## Build

From the `lab` module directory, build the server into your Lab:

```sh
go build -o <lab>/plugins/jira/forgecell-jira ./plugins/jira
```

The server takes no arguments.

## Write the manifest

Copy `plugin.json.tmpl` to `<lab>/plugins/jira/plugin.json`. Then replace the two
placeholders:

- `{{SERVER_PATH}}` with the absolute path of the built server.
- `{{SERVER_SHA256}}` with its digest, the first field of
  `shasum -a 256 <lab>/plugins/jira/forgecell-jira`.

The path is written into a JSON string, so it must not contain `"` or `\`. Rebuilding
the server changes its digest, so write the manifest again after every build.

`needs.network` lists `atlassian.net`. Add your exact site host, for example
`example.atlassian.net`, to that list, then pin the manifest again.

## Pin it in the Formula

Pin the manifest by its own digest (`shasum -a 256 <lab>/plugins/jira/plugin.json`)
and select the plugin as the intake source:

```yaml
intake:
  source: jira
  config:
    site: example.atlassian.net   # required
    project: ENG                  # required
    jql: component = Backend      # optional
plugins:
  jira: {manifest: plugins/jira/plugin.json, sha256: "<manifest hex>", required: true}
```

The `config` object is reviewed with the Formula:

| Key | Required | Meaning |
|---|---|---|
| `site` | yes | The Jira Cloud host name. It must be lowercase, end in `.atlassian.net`, and have no scheme, port or path. When `JIRA_CA_FILE` is set, any lowercase dotted host name is accepted. |
| `project` | yes | The Jira project key. It must match `^[A-Z][A-Z0-9_]{1,9}$`. |
| `jql` | no | An extra JQL filter of at most 1000 characters on one line. Its quotes must close and its parentheses must balance. It is AND-ed, in parentheses, into every list query. |

No other key is accepted. `tracker_describe` checks the keys that are present but
requires none, and makes no request. `tracker_list`, `tracker_get` and `tracker_states` fail with reason
`failed` when `site` or `project` is missing.

## Configuration

The plugin reads only the names in `needs.env`:

| Name | Required | Meaning |
|---|---|---|
| `JIRA_EMAIL` | with `JIRA_API_TOKEN` | The Atlassian account email the token belongs to. |
| `JIRA_API_TOKEN` | for any project that is not public | An Atlassian API token, created at `id.atlassian.com` under Security, API tokens. A classic token acts with the account's own permissions; a scoped token needs the `read:jira-work` scope. The account needs the Browse projects permission on the project. The email and token are sent only as HTTP Basic auth, and the token is redacted from every diagnostic and error. Set both or neither. |
| `JIRA_CA_FILE` | no | A clean absolute path to a PEM bundle of at most 1 MiB, for a Jira behind a private CA. Every PEM block must be a certificate that parses, and there must be at least one. The file must not be a symlink, must be owned by root or you, and must not be writable by others. Its certificates are added to the system pool, which is kept. TLS verification is never skipped, and plain `http` is never used. |

An invalid configuration does not stop the server. Every tool call fails with reason
`failed` and a message that names the setting, never its value.

## Behaviour

- **Describe.** `tracker_describe` makes no request. Its `displayName` is `Jira`, its
  states are `open` and `closed`, and its selectors are `oldest`, `label:<name>` and
  `filter:<id>`. Its `keyPattern` is `^<PROJECT>-[1-9][0-9]{0,9}$` when the config names a
  project, and `^[A-Z][A-Z0-9_]{1,9}-[1-9][0-9]{0,9}$` otherwise.
- **List.** `tracker_list` searches `project = "<PROJECT>" AND statusCategory != Done`,
  then `AND (<jql>)`, then `AND labels = "<name>"` for `label:<name>` or
  `AND filter = <id>` for `filter:<id>`, `ORDER BY created ASC`. A label name must
  match `^[A-Za-z0-9_.-]{1,64}$` and a filter id must be numeric; anything else is
  refused. The milestone is the first fix version's name. The cursor is Jira's
  `nextPageToken`.
- **Get.** `tracker_get` flattens the ADF description to plain text: paragraphs,
  headings, lists, code blocks, quotes, panels and tables become lines, and text marks
  are dropped. Unknown nodes are skipped. A body past 65,536 characters, 50,000 nodes or
  32 levels of nesting is cut and marked `[truncated]`. The URL is
  `https://<site>/browse/<key>`.
- **States.** `tracker_states` returns only the keys asked for. A key Jira does not
  return is left out.
- **State mapping.** Status category `done` is `closed`. Its `stateReason` is
  `not_planned` when the resolution is Won't Do, Won't Fix, Duplicate, Cannot Reproduce
  or Declined, in any case, and `completed` otherwise. `closedAt` is the resolution
  date. Every other status category is `open`.
- **Keys.** Every key must match the project's key pattern before any request is made.
- **Bounds.** Each request has a 20 s timeout, and each tool's contract `timeoutMs`
  bounds the whole call. Redirects are never followed: a 3xx fails the call. A response
  body over 4 MiB fails the call, and is never cut. One call follows at most five pages.
  A `tracker_list` call cut at that bound returns the next token as `nextCursor`. A
  repeated page token fails the call. A title, label or milestone past its contract
  bound is cut and ends in `…`. Only the first 20 labels are reported.
- **Errors.** 401 is `auth-missing`. 429 is `rate-limited`. A transport error, TLS
  refusal or timeout is `network`. Any other answer, 403 and 404 included, is `failed`.
  The message names the HTTP status, never the body.
