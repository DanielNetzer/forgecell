#!/bin/sh
# Maintainer-only build. Users download the result and never run this script.
set -eu
version=${1:?Usage: build-native-release.sh VERSION OUTPUT_DIRECTORY}
output=${2:?Usage: build-native-release.sh VERSION OUTPUT_DIRECTORY}
printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$' || { echo 'Invalid release version' >&2; exit 1; }
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$output"
output=$(CDPATH= cd -- "$output" && pwd)
[ ! -e "$output/$version" ] || { echo 'Version output already exists; choose a fresh output directory.' >&2; exit 1; }
stage=$(mktemp -d "$output/.release.XXXXXXXX")
trap 'rm -rf "$stage"' EXIT HUP INT TERM
cd "$root/lab"
for platform in darwin linux; do
 for arch in amd64 arm64; do
  name="forgecell-$platform-$arch"
  env CGO_ENABLED=0 GOOS="$platform" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/$name" ./cmd/forgecell
  if command -v shasum >/dev/null 2>&1; then checksum=$(shasum -a 256 "$stage/$name"); else checksum=$(sha256sum "$stage/$name"); fi
  printf '%s\n' "${checksum%% *}" > "$stage/$name.sha256"
 done
done
cp internal/install/notices.txt "$stage/THIRD-PARTY-NOTICES.txt"
cp "$root/LICENSE" "$stage/LICENSE"
sed -e "s/@FORGECELL_VERSION@/$version/g" -e "s/@FORGECELL_RELEASE_TAG@/v$version/g" \
 "$root/scripts/install-native.sh" > "$stage/install.sh"
if grep -q '@FORGECELL_' "$stage/install.sh"; then echo 'Unresolved installer placeholder' >&2; exit 1; fi
mv "$stage" "$output/$version"
cp "$output/$version/install.sh" "$output/install.sh"
printf '%s\n' "$version" > "$output/latest.txt"
echo "Built $output/$version. Installer pinned to GitHub tag v$version; publication requires review."
