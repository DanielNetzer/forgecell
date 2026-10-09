# Install Forgecell CLI

Public CLI preview; core source licensed under the [MIT License](../LICENSE). No repository access, Node or Go is needed to install.

```sh
sh -c 'set -eu; script=$(mktemp "${TMPDIR:-/tmp}/forgecell-bootstrap.XXXXXXXX") || { echo "Forgecell: cannot create bootstrap temporary file." >&2; exit 1; }; trap '"'"'rm -f "$script"'"'"' 0; trap '"'"'exit 1'"'"' HUP INT TERM; curl -fsSL --proto "=https" --proto-redir "=https" --connect-timeout 15 --max-time 30 https://github.com/DanielNetzer/forgecell-releases/releases/download/0.2.0-preview.1/install.sh -o "$script" || { echo "Forgecell: bootstrap download failed." >&2; exit 1; }; [ -s "$script" ] || { echo "Forgecell: bootstrap download was empty." >&2; exit 1; }; sh "$script"'
```

The command downloads the bootstrap over HTTPS before executing it. A failed download
(including HTTP errors or the 30-second timeout) or an empty response prints a failure
and returns nonzero. Its temporary file is removed on exit.

Packages: macOS and Linux, arm64 and amd64. The bootstrap needs curl, standard shell utilities, and shasum or sha256sum. It verifies the executable checksum and version before activation. Checksums are delivered alongside binaries over GitHub HTTPS, not by an independent signing authority.

## First run

Ensure `~/.local/bin` is on PATH. The installer does not edit shell profiles. Inside the
repository you want Forgecell to work on, choose a distinct local Lab name for this
repository and use it for every command with the published preview:

```sh
lab_dir="$HOME/.forgecell/labs/YOUR_REPOSITORY"
forgecell doctor --lab "$lab_dir"
forgecell init --lab "$lab_dir"
# Read the saved Formula, then use the actual proposal ID:
forgecell init --lab "$lab_dir" --approve PROPOSAL_ID
forgecell run ISSUE_NUMBER --lab "$lab_dir"
```

Running tickets requires Git, authenticated GitHub CLI (`gh auth login`), and an authenticated supported harness (Codex, Claude Code or Cursor). Doctor performs read-only capability/authentication checks. It does not prove model access or repository policy readiness. `doctor --lab DIR` checks that Lab's saved native binding; stale bindings require a reviewed rebind.

Init chooses a default harness deterministically and preserves existing approved Formulas. Approve the saved YAML before running. A Molecule uses an isolated checkout and stops at human review. This preview does not automatically merge, deploy or close issues. Learning uses a separately bound meta harness and requires explicit approval of Formula changes.

The published `0.2.0-preview.1` still defaults Lab records to the checkout's ignored
`.forgecell` directory. To keep this version's assay, Formula and ledger outside the
checkout, pass the **same** `--lab` path to every Lab command as shown above.
The development CLI uses a distinct directory under `~/.forgecell/labs/` for each
checkout by default; `doctor --json` reports the exact location. Existing checkout
Labs are left intact. Use `--lab .forgecell` to read one; `init` will not silently
replace its approved Formula with a new home Lab.

## Versions, upgrade and rollback

The command above pins `0.2.0-preview.1`. Repeating it safely reinstalls that version. For a future published version, use its versioned installer, or set `FORGECELL_VERSION` on the shell executing the installer. Review release notes before upgrading.

`forgecell rollback` restores the previous verified installation when one exists. Installation preserves Formulas and ledgers, stores versioned executables under `~/.forgecell/releases`, and refuses conflicting unmanaged launchers.

`FORGECELL_HOME` and `FORGECELL_BIN` customize paths. Set these variables before the opening `sh -c` so they reach the downloaded installer, not only curl.

## Contributors

Core source may be used and distributed under MIT; retain its copyright and permission notice.
Keep local Lab records, credentials, environment files and raw harness traces private.
Native maintainers use [the distribution design](design/native-cli-distribution.md). Source access is not required for users of the public CLI packages.

## Core release preparation and migration

The install command remains pinned to the legacy `forgecell-releases` release
`0.2.0-preview.1`. Its historical endpoint uses the unprefixed version tag;
existing assets and checksums remain unchanged. A new core release is being
prepared at `DanielNetzer/forgecell` with a version-pinned installer and `v` tag.
This preparation does not establish public availability.

Publication requires local verification, independent review, import of the reviewed
clean core snapshot, public-visibility approval, exact-head Actions passes, draft
review, approved publication, then anonymous install/upgrade/rollback validation.
Only after those gates should README and install docs switch together to one
identical pinned command. Build a fresh release from the final reviewed public
source commit; never relabel an old executable. Any relocated artifact must retain
its original version, checksums and provenance. Installation requires no GitHub
token, Go or Node; ticket execution requires Git, authenticated gh and a configured,
authenticated coding harness. Backlog publication remains paused.
