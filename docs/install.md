# Install Forgecell CLI

Public CLI `0.2.0-preview.2` prerelease; core source licensed under the [MIT License](../LICENSE). No repository access, Node or Go is needed to install.

```sh
sh -c 'set -eu; script=$(mktemp "${TMPDIR:-/tmp}/forgecell-bootstrap.XXXXXXXX") || { echo "Forgecell: cannot create bootstrap temporary file." >&2; exit 1; }; trap '"'"'rm -f "$script"'"'"' 0; trap '"'"'exit 1'"'"' HUP INT TERM; curl -fsSL --proto "=https" --proto-redir "=https" --connect-timeout 15 --max-time 30 https://github.com/DanielNetzer/forgecell/releases/download/v0.2.0-preview.2/install.sh -o "$script" || { echo "Forgecell: bootstrap download failed." >&2; exit 1; }; [ -s "$script" ] || { echo "Forgecell: bootstrap download was empty." >&2; exit 1; }; sh "$script"'
```

The command downloads the bootstrap over HTTPS before executing it. A failed download
(including HTTP errors or the 30-second timeout) or an empty response prints a failure
and returns nonzero. Its temporary file is removed on exit.

Packages: macOS and Linux, arm64 and amd64. The bootstrap needs curl, standard shell utilities, and shasum or sha256sum. It verifies the executable checksum and version before activation. Checksums are delivered alongside binaries over GitHub HTTPS, not by an independent signing authority.

## First run

Ensure `~/.local/bin` is on PATH. The installer does not edit shell profiles. Inside the
repository you want Forgecell to work on, you may choose an explicit local Lab name
and use the same path for every command:

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

The `0.2.0-preview.2` prerelease uses a distinct directory under `~/.forgecell/labs/`
for each checkout by default; `forgecell doctor --json` reports the exact location.
Existing preview.1 checkout Labs in the ignored `.forgecell` directory are left
intact. Use the same `--lab .forgecell` path for every command to continue using one;
`init` will not silently replace its approved Formula with a new home Lab. Labs and
saved bindings are not automatically migrated or rebound. With preview.1, an explicit
path such as the example above keeps records outside the checkout.

## Versions, upgrade and rollback

The command above pins `0.2.0-preview.2`. Repeating it safely reinstalls that version. For another GitHub release, use its matching versioned installer; `FORGECELL_VERSION` overrides require the generic `FORGECELL_RELEASE_BASE` layout. Review release notes before upgrading.

`forgecell rollback` restores the previous verified installation when one exists. Installation preserves Formulas and ledgers, stores versioned executables under `~/.forgecell/releases`, and refuses conflicting unmanaged launchers.

`FORGECELL_HOME` and `FORGECELL_BIN` customize paths. Set these variables before the opening `sh -c` so they reach the downloaded installer, not only curl.

## Contributors

Core source may be used and distributed under MIT; retain its copyright and permission notice.
Keep local Lab records, credentials, environment files and raw harness traces private.
Native maintainers use [the distribution design](design/native-cli-distribution.md). Source access is not required for users of the public CLI packages.

## Published public-core release and legacy provenance

The published [v0.2.0-preview.2 prerelease](https://github.com/DanielNetzer/forgecell/releases/tag/v0.2.0-preview.2)
was built from public-core source `801a9846fc9066dde985bb678348cac2bc89a249`.
The pinned installer uses `DanielNetzer/forgecell` and the `v0.2.0-preview.2` tag.
Installation requires no GitHub token, Git, gh, Go or Node.

The legacy `DanielNetzer/forgecell-releases` preview `0.2.0-preview.1` used an
unprefixed version tag. Its original assets, checksums and provenance remain unchanged.
Checksums from the same GitHub HTTPS endpoint verify integrity, not independently
signed authenticity. Never relabel an old executable as a new release.

Future releases require local verification, independent exact-source review,
exact-head Linux/macOS CI, draft review and approved publication, followed by anonymous
install/upgrade/rollback validation before updating the identical README and install
commands. This documentation patch also requires independent exact-source review and
exact-head Linux/macOS CI before merge. Operator release evidence is separate from
source tests; publication does not authorize backlog merges.
