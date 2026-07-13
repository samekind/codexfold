#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/../.." && pwd)
script="$repo_root/scripts/activate-canonical-after-codex-exit.sh"

grep -Fq 'find -H sessions archived_sessions' "$script"
grep -Fq 'find -H "${CODEX_HOME}/sessions" "${CODEX_HOME}/archived_sessions"' "$script"
grep -Fq 'app_servers_running()' "$script"
grep -Fq 'real-home Codex app servers did not drain' "$script"

stop_line=$(grep -n 'fs service stop --apply' "$script" | head -n 1 | cut -d: -f1)
deactivate_line=$(grep -n 'fs namespace deactivate --apply' "$script" | head -n 1 | cut -d: -f1)
start_line=$(grep -n 'fs service start --apply' "$script" | head -n 1 | cut -d: -f1)
[[ -n "$stop_line" && -n "$deactivate_line" && -n "$start_line" ]]
(( stop_line < deactivate_line && deactivate_line < start_line ))

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/native/sessions/2026/07/13" "$root/native/archived_sessions"
touch "$root/native/sessions/2026/07/13/rollout.jsonl"
ln -s "$root/native/sessions" "$root/sessions"
ln -s "$root/native/archived_sessions" "$root/archived_sessions"

count=$(cd "$root" && find -H sessions archived_sessions -type f | wc -l | tr -d ' ')
[[ "$count" == "1" ]]

echo "PASS: activation snapshots traverse canonical session symlinks"
