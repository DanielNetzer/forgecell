#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
checker=$script_dir/check-repo-hygiene.sh
fixtures=$(mktemp -d "${TMPDIR:-/tmp}/repo-hygiene-test.XXXXXX")
trap 'rm -rf -- "$fixtures"' 0
trap 'exit 1' HUP INT TERM

# Keep fixture Git operations independent of the caller's repository/config.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR
GIT_CONFIG_NOSYSTEM=1
GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM GIT_CONFIG_GLOBAL
count=0

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

new_repo() {
    count=$((count + 1))
    repo=$fixtures/fixture-$count
    git init -q -- "$repo"
    cd -- "$repo"
}

track() {
    mkdir -p -- "$(dirname -- "$1")"
    printf 'fixture\n' > "$1"
    git add -f -- "$1"
}

check() {
    expected=$1
    label=$2
    status=0
    sh "$checker" > "$fixtures/output" 2>&1 || status=$?
    if [ "$status" -ne "$expected" ]; then
        cat "$fixtures/output" >&2
        fail "$label: expected exit $expected, got $status"
    fi
    if [ "$expected" -eq 1 ]; then
        grep -F 'Forbidden tracked path' "$fixtures/output" > /dev/null ||
            fail "$label: missing hygiene diagnostic"
        grep -F -- "$3" "$fixtures/output" > /dev/null ||
            fail "$label: missing forbidden path segment"
    fi
    printf 'PASS: %s\n' "$label"
}

new_repo
check 0 'empty index'

new_repo
for path in .github/workflows/example.yml docs/plans/plan.md docs/design/design.md lab/main.go web/index.html assets/logo.svg 'docs/product plans/with spaces.md' superpowers-extra/file docs/.codex-example/file docs/my.agents/file; do
    track "$path"
done
check 0 'normal product paths, spaces, and similar names'

# Retired paths are rooted at the repository, not matched by basename.
new_repo
for path in web/package.json web/package-lock.json lab/go.mod lab/go.sum lab/cmd/forgecell/main.go lab/internal/config/config.go scripts/install-native.sh scripts/build-native-release.sh lab/src-extra/file lab/package.json.extra scripts/install.sh.extra docs/lab/src/file docs/lab/package.json docs/scripts/install.sh; do
    track "$path"
done
check 0 'Go Lab, web manifests, native installers, and similar paths'

for directory in lab/src lab/scripts lab/examples lab/licenses; do
    for suffix in file 'nested directory/file with spaces'; do
        new_repo
        path=$directory/$suffix
        track "$path"
        check 1 "retired directory: $path" "$directory"
        mkdir -p web
        cd web
        check 1 "retired directory from subdirectory: $path" "$directory"
        cd "$repo"
        rm -- "$path"
        check 1 "retired directory absent on disk: $path" "$directory"
    done
done

for path in lab/package.json lab/package-lock.json lab/tsconfig.json scripts/install.sh scripts/install-release.sh; do
    new_repo
    track "$path"
    check 1 "retired exact file: $path" "$path"
    mkdir -p web
    cd web
    check 1 "retired exact file from subdirectory: $path" "$path"
    cd "$repo"
    rm -- "$path"
    check 1 "retired exact file absent on disk: $path" "$path"
done

new_repo
track README.md
printf 'lab/src/\nlab/package.json\n' > .gitignore
git add -- .gitignore
for path in lab/src/local lab/scripts/local lab/examples/local lab/licenses/local lab/package.json lab/package-lock.json lab/tsconfig.json scripts/install.sh scripts/install-release.sh; do
    mkdir -p -- "$(dirname -- "$path")"
    printf 'keep local data\n' > "$path"
done
check 0 'ignored and untracked retired paths'
for path in lab/src/local lab/scripts/local lab/examples/local lab/licenses/local lab/package.json lab/package-lock.json lab/tsconfig.json scripts/install.sh scripts/install-release.sh; do
    [ "$(cat "$path")" = 'keep local data' ] || fail "modified $path"
done

for forbidden in superpowers .claude .codex .cursor .agents; do
    new_repo
    track "$forbidden/config"
    check 1 "root $forbidden directory" "$forbidden"

    new_repo
    track "docs/product plans/$forbidden/config with spaces"
    check 1 "nested $forbidden directory with spaces" "$forbidden"
    mkdir -p lab
    cd lab
    check 1 "whole repository from subdirectory: $forbidden" "$forbidden"

    new_repo
    track "docs/$forbidden"
    check 1 "terminal $forbidden segment" "$forbidden"
done

new_repo
track README.md
printf '.claude/\n.codex/\n.forgecell-local/\n' > .gitignore
git add -- .gitignore
for path in .claude/config .codex/config .cursor/config .agents/config superpowers/config .forgecell-local/lab/data; do
    mkdir -p -- "$(dirname -- "$path")"
    printf 'keep local data\n' > "$path"
done
check 0 'ignored and untracked configuration and local Lab data'
for path in .claude/config .codex/config .cursor/config .agents/config superpowers/config .forgecell-local/lab/data; do
    [ "$(cat "$path")" = 'keep local data' ] || fail "modified $path"
done
track .codex/config
check 1 'tracked configuration is rejected even when ignored' .codex
rm .codex/config
check 1 'tracked paths are checked even when absent on disk' .codex

new_repo
track 'docs/$(touch INJECTED)/`touch INJECTED_BACKTICK`; spaces.txt'
check 0 'shell metacharacters in clean filenames'
track 'docs/$(touch INJECTED)/.agents/`touch INJECTED_BACKTICK`; spaces.txt'
check 1 'shell metacharacters in forbidden filenames' .agents
[ ! -e INJECTED ] && [ ! -e INJECTED_BACKTICK ] || fail 'filename executed as shell code'

new_repo
track 'docs/line
break/.cursor/config'
check 1 'newline in a path' .cursor

mkdir "$fixtures/not-a-repository"
cd "$fixtures/not-a-repository"
status=0
sh "$checker" > "$fixtures/output" 2>&1 || status=$?
[ "$status" -ne 0 ] || fail 'Git failure silently accepted'
grep -F 'Unable to read Git-tracked paths' "$fixtures/output" > /dev/null ||
    fail 'missing Git failure diagnostic'
printf 'PASS: Git failure is reported\n'
printf 'All repository hygiene tests passed.\n'
