#!/bin/sh
# Native CLI bootstrap template; the builder pins the published version and tag.
set -eu
version=${FORGECELL_VERSION:-@FORGECELL_VERSION@}
tag="@FORGECELL_RELEASE_TAG@"
if [ -n "${FORGECELL_RELEASE_BASE:-}" ]; then
 base=$FORGECELL_RELEASE_BASE
 layout=generic
else
 base=https://github.com/DanielNetzer/forgecell/releases/download
 layout=github
fi
case "$base" in https://*) ;; *) echo 'Forgecell: set FORGECELL_RELEASE_BASE to the HTTPS release endpoint.' >&2; exit 1;; esac
case "$base" in *'?'*|*'#'*|*'@'*|*' '*|*'
'*) echo 'Forgecell: invalid release endpoint.' >&2; exit 1;; esac
for tool in curl uname mktemp chmod; do
 command -v "$tool" >/dev/null 2>&1 || { echo "Forgecell: $tool is required to download the CLI." >&2; exit 1; }
done
if command -v shasum >/dev/null 2>&1; then checksum() { shasum -a 256 "$1"; }
elif command -v sha256sum >/dev/null 2>&1; then checksum() { sha256sum "$1"; }
else echo 'Forgecell: shasum or sha256sum is required.' >&2; exit 1
fi
case "$(uname -s)" in Darwin) platform=darwin;; Linux) platform=linux;; *) echo 'Forgecell: supported operating systems are macOS and Linux.' >&2; exit 1;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64;; x86_64|amd64) arch=amd64;; *) echo 'Forgecell: supported architectures are arm64 and amd64.' >&2; exit 1;; esac
stage=$(mktemp -d "${TMPDIR:-/tmp}/forgecell-download.XXXXXXXX")
trap 'rm -rf "$stage"' EXIT HUP INT TERM
fetch() { curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 120 --max-filesize "$3" "$1" -o "$2"; }
base=${base%/}
if [ -z "$version" ]; then fetch "$base/latest.txt" "$stage/latest" 200; version=$(cat "$stage/latest"); fi
case "$version" in ''|*[!0-9A-Za-z.-]*|.*|-*) echo 'Forgecell: invalid release version.' >&2; exit 1;; esac
if [ "$layout" = github ]; then
 [ "$version" = "@FORGECELL_VERSION@" ] || { echo 'Forgecell: use the versioned installer for the requested GitHub release.' >&2; exit 1; }
 asset_base="$base/$tag"
else
 asset_base="$base/$version"
fi
artifact="forgecell-$platform-$arch"
fetch "$asset_base/$artifact.sha256" "$stage/checksum" 200
expected=$(cat "$stage/checksum")
case "$expected" in *[!a-f0-9]*|'') echo 'Forgecell: invalid checksum.' >&2; exit 1;; esac
[ "${#expected}" -eq 64 ] || { echo 'Forgecell: invalid checksum length.' >&2; exit 1; }
fetch "$asset_base/$artifact" "$stage/forgecell" 100000000
actual=$(checksum "$stage/forgecell"); actual=${actual%% *}
[ "$actual" = "$expected" ] || { echo 'Forgecell: checksum mismatch; current CLI unchanged.' >&2; exit 1; }
chmod 700 "$stage/forgecell"
[ "$("$stage/forgecell" --version)" = "$version" ] || { echo 'Forgecell: release version mismatch.' >&2; exit 1; }
"$stage/forgecell" __install
