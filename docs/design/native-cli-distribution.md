# Native CLI distribution

Status: core source is [MIT licensed](../../LICENSE); the published `0.2.0-preview.2` prerelease is distributed from the public `DanielNetzer/forgecell` repository.

Users install the CLI with one shell command. Compiled-package users do not need to clone the repository, build source, or install Go/Node. Maintainers produce architecture-specific executables; that packaging detail does not turn Forgecell into a desktop application.

## Download contract

The generic HTTPS package layout supports the following files:

- `install.sh`: rendered copy of `scripts/install-native.sh`, pinned to the built version and `v<version>` tag; also retained in the version bundle.
- `latest.txt`: exact version string.
- `<version>/forgecell-<darwin|linux>-<amd64|arm64>`: executable.
- Each executable's `.sha256` file: exactly one hexadecimal SHA-256 digest.
- `<version>/LICENSE`: the core MIT copyright and permission notice.
- `<version>/THIRD-PARTY-NOTICES.txt`: Go runtime and YAML dependency licenses, also embedded in each executable.

The bootstrap accepts only HTTPS and HTTPS redirects, bounds downloads and verifies the executable checksum and version before executing installation. Checksums protect download integrity; they are delivered through the same trusted HTTPS endpoint, not a separate signing authority. The generated installer defaults to its exact built version. `FORGECELL_RELEASE_BASE` selects the generic `<base>/<version>/<artifact>` layout and permits `FORGECELL_VERSION` overrides. Default GitHub installs require the matching versioned installer.

## Installation and rollback

The native installer stages a version, smoke-checks `--version`, writes installed metadata/notices and switches the `current` symlink. A stable launcher sets `FORGECELL_LAUNCHER` to the native executable through `current`, preserving Formula bindings across upgrades. `previous` supports `forgecell rollback`, which verifies the retained artifact checksum and smoke-checks it before activation.

Defaults: `~/.forgecell/releases/<version>`, `~/.forgecell/bin/forgecell`, and `~/.local/bin/forgecell`. `FORGECELL_HOME` and `FORGECELL_BIN` override installation paths. The `0.2.0-preview.2` prerelease stores each checkout's Formulas and ledgers in a distinct directory under `~/.forgecell/labs/`; `forgecell doctor --json` reports the exact location. Legacy `0.2.0-preview.1` defaults to a project `.forgecell` unless `--lab` is supplied. Continue using an existing Lab with the same explicit `--lab` path on every command. Installation leaves either location untouched and does not automatically migrate Labs or rebind saved Formulas. The installer refuses unrelated commands, unmanaged launchers, conflicting version contents and concurrent installation. It does not delete retained releases or change shell profiles.

The TypeScript/source installer has been retired. The native installer still refuses to
replace unmanaged launchers. Existing prototype installations need a separately
reviewed transition; the native release does not claim automatic migration from them.

Runtime prerequisites: Git, GitHub CLI authentication and the selected coding harness. Installation can finish before those exist; `forgecell doctor` reports readiness. No model is invoked during installation.

`forgecell doctor --lab <directory>` also checks the active Formula’s saved coding binding. Native bindings must resolve to the running Forgecell executable, and the saved provider path must pass capability/authentication probes. A different ready provider on PATH or an explicit `--harness` does not hide a broken saved binding. Missing or stale bindings require a reviewed `init --harness` proposal. Custom commands are preserved, never executed by doctor, and reported as unknown rather than ready.

## Maintainer verification

`sh scripts/build-native-release.sh <version> <output>` builds four native packages. CI tests the actual host architecture on macOS and Linux over a local HTTPS endpoint with certificate verification enabled. Its installer PATH deliberately excludes Node, Go, Git and gh. It verifies fresh install, corrupted upgrade refusal, real upgrade, repeat install, rollback, checksums/notices and existing Lab-data preservation. Other architecture packages are cross-compiled; host tests do not imply every architecture has been executed.

Tagged release automation prepares a draft release after verification. Publishing the user-facing endpoint remains a distinct distribution step. No source-repository access should be required by that endpoint.

## Public GitHub release mapping

The published [v0.2.0-preview.2 prerelease](https://github.com/DanielNetzer/forgecell/releases/tag/v0.2.0-preview.2)
was freshly built from public-core source `801a9846fc9066dde985bb678348cac2bc89a249`.
Release base is `https://github.com/DanielNetzer/forgecell/releases/download`, with
`v<version>` tags. The rendered installer pins its exact version and tag, without
requesting `latest.txt` or GitHub's stable-only latest-release redirect.

Legacy `0.2.0-preview.1` came from the separate public
`DanielNetzer/forgecell-releases` download repository, using the exact version tag
without a `v` prefix. Its installer, assets, checksums and source provenance remain
unchanged. That repository contains distribution documentation; its GitHub-generated
archive is not Forgecell source.

The bundle includes four freshly built executables, their checksum files, MIT LICENSE, third-party notices and the rendered bootstrap. MIT permits core source distribution with the copyright and permission notice retained; source publication and repository visibility remain separate reviewed decisions. Keep local Lab files, ledgers, credentials, environment files and raw harness/evaluation traces private.

Source-tag automation still prepares only a draft. Public promotion is a separate reviewed maintainer action. Verify published asset hashes and anonymous installation before announcing each version. Retain previous version assets for upgrades and rollback.

## Public core mapping and publication gates

New builds default to
`https://github.com/DanielNetzer/forgecell/releases/download/v<version>/<artifact>`.
The builder renders both the exact version and `v<version>` tag without placeholders;
GitHub does not use the generic override's unprefixed version directory. The local
HTTPS fixture routes the generated GitHub origin to localhost while preserving and
asserting its exact request paths. It executes generated installers without version
or base overrides, then checks generic overrides separately. Packaging checks cover
all four artifacts and license/notices. Lifecycle checks include missing/invalid
checksums, corruption, partial downloads, checksum-valid version mismatch, upgrade,
repeat install, rollback, temporary-file cleanup and preserved Lab data.

The preview.2 release has completed source review, release Actions verification,
approved publication and anonymous install/upgrade/rollback validation. Operator
release evidence is separate from local source tests. Future releases retain local
verification, independent exact-source review, exact-head Linux/macOS CI, draft review,
approved publication and anonymous lifecycle validation before documentation switches.
This documentation patch requires independent exact-source review and exact-head
Linux/macOS CI before merge. No tag push authorizes publication or backlog merges.

Prefer a new release freshly built from the final reviewed public source commit.
Never copy an old binary under a new version or imply different source provenance.
If artifacts are relocated, retain their exact original version and checksums.
