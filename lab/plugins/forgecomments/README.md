# Review bot comments reviewer plugin

`forgecell-forgecomments` is a first-party reviewer plugin. It is an MCP server on stdio
that serves the `forgecell.reviewer/v1` contract. It turns the review comments that the
engine already read from the forge, such as Cursor BugBot's or GitLab Duo's, into
findings.

It runs offline. It reads no environment variables, makes no network request and needs no
credentials. Everything it reads arrives in the call.

## Build

From the `lab` module directory, build the server into your Lab:

```sh
go build -o <lab>/plugins/forgecomments/forgecell-forgecomments ./plugins/forgecomments
```

The server takes no arguments.

## Write the manifest

Copy `plugin.json.tmpl` to `<lab>/plugins/forgecomments/plugin.json`. Then replace the two
placeholders:

- `{{SERVER_PATH}}` with the absolute path of the built server.
- `{{SERVER_SHA256}}` with its digest, the first field of
  `shasum -a 256 <lab>/plugins/forgecomments/forgecell-forgecomments`.

The path is written into a JSON string, so it must not contain `"` or `\`. Rebuilding the
server changes its digest, so write the manifest again after every build.

The manifest declares no `needs.env`, no `needs.network` and no roles. Both tools are
`read-only`.

## Declare it in the Formula

Pin the manifest by its own digest (`shasum -a 256 <lab>/plugins/forgecomments/plugin.json`)
and list the plugin under `reviewers`:

```yaml
reviewers:
  - plugin: forgecomments
    autoFix: high
    config:
      bots: ["cursor[bot]", gitlab-duo] # optional
      defaultSeverity: medium           # optional
plugins:
  - {name: forgecomments, manifest: plugins/forgecomments/plugin.json, sha256: "<manifest hex>"}
```

The `config` object is reviewed with the Formula. It is a JSON object of at most 4 KiB:

| Key | Required | Meaning |
|---|---|---|
| `bots` | no | A list of up to 16 author names of at most 100 characters each, compared case-insensitively. A comment is a bot comment when the forge marks it as a bot's (`isBot`) or its author is listed. `null` is refused, and so is a name listed twice, in any case. |
| `defaultSeverity` | no | `high`, `medium`, `low` or `info`: the severity of a bot comment with no marker. It is `medium` when absent. |

No other key is accepted. Both tools check the config and fail with reason `failed` and a
message that names the problem, never the value.

## Collect findings

After a change is published, collect the findings for a Molecule:

```sh
forgecell findings <id> --collect
```

`reviewer_describe` lists the `forge-comments` capability, so the engine passes the forge's
review comments in every `reviewer_findings` call.

GitHub Labs can't serve review comments in v1. This plugin works with a plugin forge that
serves `forge_comments`, such as GitLab.

To triage the findings, propose a fix round and push it to the same change request, see
[Outside reviewers and fix rounds](../../../docs/install.md#outside-reviewers-and-fix-rounds).

## Behaviour

- **Describe.** `reviewer_describe` checks the config and answers `displayName`
  `Review bot comments`, the severities `high`, `medium`, `low` and `info`, and the
  capability `forge-comments`.
- **No comments.** A `reviewer_findings` call without `comments` fails with
  `this reviewer needs the forge's review comments`. It never answers an empty
  `complete`. An empty `comments` list is a complete review with no findings.
- **Findings.** Every bot comment becomes one finding, and every other comment is ignored.
  The answer's status is `complete`. Each finding has:
  - `id`: the comment id.
  - `path` and `line`: copied from the comment when it has them.
  - `resolved`: always false, because comments carry no resolution.
  - `title`: the first line with text. Control characters become spaces, format
    characters such as bidi controls are dropped, whitespace collapses, and markdown
    heading and emphasis markers (`#`, `*`, `_`, `>` and backticks) are stripped from
    the start only, so a closing backtick or a trailing underscore stays. It is cut to 300 characters, ending in `…`. A comment with no text has the
    title `Review bot comment`.
  - `body`: the comment body, cut to 65536 characters and ending in `[truncated]` when cut.
- **Severity.** The first explicit marker in the first five lines wins, in any case. The
  markers are:
  - `High Severity`, `Medium Severity` or `Low Severity` at the start of a line, after
    leading markdown markers, or as the whole content of `**…**`.
  - `severity: high`, `medium`, `low` or `info` anywhere in a line.
  - A line that starts with `[high]`, `[medium]`, `[low]` or `[info]`, after leading
    markdown markers, unless the tag starts a markdown link, as in `[High](…)`, a
    reference link, as in `[high][ref]` or `[high][]`, or a link reference definition, as
    in `[high]: http://x`.

  The severity word must end the word: one followed by a Unicode letter, mark or digit, as
  in `severity: highé` or `High Severityé`, is not a marker, and neither is `severity`
  right after a letter. Within a line, the marker that starts first wins, whatever its
  form. A marker doesn't count when the text before it ends in the word `not` or `no`, or
  in a word ending in `n't` or `n’t`, as in `isn't`, with only markdown markers,
  punctuation and spaces between them, as in `not: severity: high` or `not, severity:
  high`.

  Text inside an inline code span, as in `` `High Severity` `` or ``` ``severity: high`` ```,
  never counts. As in CommonMark, a run of backticks opens a span that the next run of
  exactly the same length closes, even on a later line of the same paragraph, and a run
  with no such closer is literal. A backslash does not escape a backtick here. Lines inside
  a fenced code block (```` ``` ```` or `~~~`) are never read. As in CommonMark, only a line
  holding a run of the opening character at least as long as the opener, and nothing
  else, closes the fence, and a fence left open hides the rest of the five lines. Outside
  a list, a line indented by four or more columns is an indented code line and is never
  read; a tab reaches the next multiple of four columns. Such a line never opens or closes
  a fence, so a marker on the line after an indented ```` ``` ```` is still read. A list
  item starts a list, and an indented line in a list is list content and is read. Any
  other line indented by less than four columns ends the list, even a lazy continuation,
  so an indented line after it is code. A line is a blockquote line, and never read, when
  a `>` appears anywhere in its leading markdown markers, list bullets and spaces, as in
  `> x` or `- > x`. A lone CR, U+0085, U+2028 and U+2029 each end a line, as LF does, and
  CRLF is one break. Without a marker, `defaultSeverity` applies.
- **Order and bounds.** Findings are ordered highest severity first, and latest comment
  first within a severity. At most 500 are kept, so the lowest-severity, oldest ones are
  the ones left out. Comments that share an id are kept as separate findings, since the
  contract doesn't require unique ids. When the bodies would take the
  answer over its output bound, they are cut to equal shares, each ending in
  `[truncated]`. If even that does not fit, the call fails rather than dropping findings.
