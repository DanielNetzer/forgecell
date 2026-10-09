#!/bin/sh
set -eu

# Let Git match complete path segments in the index. Do not parse filenames
# as shell words or walk the working tree (which may contain local user data).
# Top-level pathspecs also check the entire repository from a subdirectory.
set --
for segment in superpowers .claude .codex .cursor .agents; do
    set -- "$@" ":(top,glob)**/$segment" ":(top,glob)**/$segment/**"
done
for directory in lab/src lab/scripts lab/examples lab/licenses; do
    set -- "$@" ":(top,glob)$directory/**"
done
for path in lab/package.json lab/package-lock.json lab/tsconfig.json scripts/install.sh scripts/install-release.sh; do
    set -- "$@" ":(top,literal)$path"
done

if ! forbidden_paths=$(git ls-files --cached --full-name -- "$@"); then
    printf 'Unable to read Git-tracked paths for repository hygiene check.\n' >&2
    exit 2
fi

if [ -n "$forbidden_paths" ]; then
    printf 'Forbidden tracked path segment (superpowers, .claude, .codex, .cursor, or .agents), or retired Node Lab path:\n%s\n' "$forbidden_paths" >&2
    printf 'Move product documentation into docs/design, docs/plans or docs/research.\n' >&2
    exit 1
fi
