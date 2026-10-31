# GitLab forge plugin

`forgecell-gitlab` is a first-party forge plugin. It is an MCP server on stdio that
serves the `forgecell.forge/v1` contract over the GitLab REST API v4. It opens draft
merge requests and reads projects, protection, merge requests, pipelines and notes.
It never pushes: `forge_publish_draft` returns the head commit GitLab reports, and the
engine checks it against the approved commit.

## Build

From the `lab` module directory, build the server into your Lab:

```sh
go build -o <lab>/plugins/gitlab/forgecell-gitlab ./plugins/gitlab
```

The server takes no arguments.

## Write the manifest

Copy `plugin.json.tmpl` to `<lab>/plugins/gitlab/plugin.json`. Then replace the two
placeholders:

- `{{SERVER_PATH}}` with the absolute path of the built server.
- `{{SERVER_SHA256}}` with its digest, the first field of
  `shasum -a 256 <lab>/plugins/gitlab/forgecell-gitlab`.

The path is written into a JSON string, so it must not contain `"` or `\`. Rebuilding
the server changes its digest, so write the manifest again after every build.

## Pin it in the Formula

Pin the manifest by its own digest (`shasum -a 256 <lab>/plugins/gitlab/plugin.json`)
and select the plugin as the forge:

```yaml
forge:
  plugin: gitlab
  project: welltech-com/backend   # a GitLab namespace/project path or a numeric id
plugins:
  gitlab: {manifest: plugins/gitlab/plugin.json, sha256: "<manifest hex>", required: true}
```

## Configuration

The plugin reads only the names in `needs.env`:

| Name | Required | Meaning |
|---|---|---|
| `GITLAB_TOKEN` | yes, for private projects and for publishing | A personal, group or project access token with the `api` scope. It is sent only as the `PRIVATE-TOKEN` header, and it is redacted from every diagnostic and error. |
| `GITLAB_URL` | no | The GitLab origin. The default is `https://gitlab.com`. It must be `https` with a lowercase host, an optional port, and no credentials, path (other than `/`), query or fragment. Plain `http` is refused, loopback included. |
| `GITLAB_CA_FILE` | no | A clean absolute path to a PEM bundle of at most 1 MiB, for a self-hosted GitLab with a private CA. Every PEM block must be a certificate that parses, and there must be at least one. The file must not be a symlink, must be owned by root or you, and must not be writable by others. Its certificates are added to the system pool, which is kept. TLS verification is never skipped. |

When `GITLAB_URL` names a host other than `gitlab.com`, add that host to
`needs.network` in your manifest, then pin the manifest again.

An invalid configuration does not stop the server. Every tool call fails with reason
`failed` and a message that names the setting, never its value.

## Behaviour

- **Bounds.** Each request has a 20 s timeout, and each tool's contract `timeoutMs`
  bounds the whole call. Redirects are never followed: a 3xx fails the call. A response
  body over 4 MiB fails the call, and is never cut. Listings follow `X-Next-Page` for at
  most five pages of 100, except notes, which are read in pages of 20 for at most 25
  pages (500 notes). Past that bound, `forge_checks` reports `complete: false`, and
  every other listing fails.
- **Errors.** 401, or 403 with no token, is `auth-missing`. 429 is `rate-limited`. A
  transport error, TLS refusal or timeout is `network`. Any other answer, 404 and 403
  with a token included, is `failed`. The message names the HTTP status, never the body.
- **Refs.** A change ref is `!<iid>` and must match `^![1-9][0-9]{0,9}$`. Any other ref is
  refused before a request is made.
- **Drafts.** The title is trimmed, then gets a `Draft: ` prefix unless it already starts
  with `Draft:`, `[Draft]` or `(Draft)`, in any case. A blank title, or a final title over
  GitLab's 255-character limit, is refused, and never cut. Every body line whose first
  non-whitespace character is `/` gets a backslash before the slash, so the body runs no
  GitLab quick action such as `/merge` or `/ready`.
- **Lost answers.** When the create request may have reached GitLab but its answer is
  missing, malformed, oversized, timed out or fails validation, the plugin reads the
  open merge requests on the head branch once. If exactly one, from the project itself,
  is a draft at the approved commit into the base branch, it is returned. Otherwise the
  original failure is returned. A create GitLab refuses with an HTTP status is never
  looked up.
- **Policy.** `protected` reads the exact branch name, so a wildcard protection rule reads
  as unprotected. `requiredChecks` is `["pipeline"]` when the project allows a merge only
  after a successful pipeline. `requiredReviews` is the project's `approvals_before_merge`,
  or 0.
- **Checks.** The newest pipeline of the merge request for the commit gives one check per
  job, plus an aggregate check named `pipeline`. A job that is itself named `pipeline` is
  reported as `pipeline (job)`. Only `success`, `failed`, `canceled` and `skipped` are
  completed. `manual` and every other status stay `queued` or `in_progress`, so they
  never count as green. A failed job with `allow_failure` set is `neutral`. A job name over
  200 characters is cut and ends in `…`.
- **Merged-results pipelines.** Only a pipeline whose `sha` is the commit is matched. A
  merged-results pipeline runs on the merge ref, so its sha differs, and its checks
  report no checks and `complete: false`.
- **Comments.** System notes are skipped, and `since` keeps notes created at or after it.
  A body over 65,536 characters is cut and marked `[truncated]`. An author over 100
  characters is cut and ends in `…`. `isBot` is true when GitLab marks the author a bot,
  or the username is a project or group token bot (`project_<n>_bot`, `group_<n>_bot`,
  optionally with a hex suffix), `ghost`, or ends in `-bot` or `_bot`.
- **Comment output bound.** Up to 500 notes are read, but the whole `forge_comments`
  result must still fit the contract's 2 MiB output bound. A larger result is never
  sent: the call fails with reason `failed`.
- **Forks.** `forge_find_change` ignores merge requests from forks that share the head
  branch name.
