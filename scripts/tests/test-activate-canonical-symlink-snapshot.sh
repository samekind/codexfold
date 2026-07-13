#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/../.." && pwd)
script="$repo_root/scripts/activate-canonical-after-codex-exit.sh"

grep -Fq 'find -H sessions archived_sessions' "$script"
grep -Fq 'find -H "${CODEX_HOME}/sessions" "${CODEX_HOME}/archived_sessions"' "$script"

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/native/sessions/2026/07/13" "$root/native/archived_sessions"
touch "$root/native/sessions/2026/07/13/rollout.jsonl"
ln -s "$root/native/sessions" "$root/sessions"
ln -s "$root/native/archived_sessions" "$root/archived_sessions"

count=$(cd "$root" && find -H sessions archived_sessions -type f | wc -l | tr -d ' ')
[[ "$count" == "1" ]]

echo "PASS: activation snapshots traverse canonical session symlinks"
