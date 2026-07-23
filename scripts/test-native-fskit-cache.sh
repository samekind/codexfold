#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

xcrun swiftc -O \
  -o "$out/codexfold-read-cache-tests" \
  "$root/platform/darwin/fskit/Extension/Wire.swift" \
  "$root/platform/darwin/fskit/Extension/ReadCache.swift" \
  "$root/platform/darwin/fskit/Tests/ReadCacheTests.swift"

"$out/codexfold-read-cache-tests"
