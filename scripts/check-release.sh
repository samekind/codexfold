#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

version=$(tr -d '[:space:]' < VERSION)
expected_tag="v$version"
requested_tag=${1:-$expected_tag}
marketing_version=${version%%-*}

fail() {
  printf 'release metadata error: %s\n' "$1" >&2
  exit 1
}

[ "$requested_tag" = "$expected_tag" ] || fail "tag $requested_tag does not match VERSION $version"
grep -Fqx 'module github.com/samekind/codexfold' go.mod || fail "canonical Go module is not samekind/codexfold"
grep -Fq "MARKETING_VERSION: \"$marketing_version\"" platform/darwin/fskit/project.yml || fail "FSKit marketing version does not match $marketing_version"
grep -Fq "## [$version]" CHANGELOG.md || fail "CHANGELOG has no $version entry"
[ -f "docs/releases/$expected_tag.md" ] || fail "release notes for $expected_tag are missing"
grep -Fq "github.com/samekind/codexfold/cmd/codexfold@$expected_tag" README.md || fail "README install command is not pinned to $expected_tag"

if git grep -n 'github\.com/jstar0/codexfold' -- ':!CHANGELOG.md'; then
  fail "legacy Go module references remain"
fi

if git ls-files --error-unmatch codexfold >/dev/null 2>&1; then
  fail "a built codexfold binary is tracked"
fi

printf 'release metadata: %s\n' "$expected_tag"
