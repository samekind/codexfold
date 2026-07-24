#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/../.." && pwd)
script="$repo_root/scripts/activate-canonical-after-codex-exit.sh"

grep -Fq 'snapshot_native_tree()' "$script"
grep -Fq "-exec stat -f '%N|%z|%m|%i|%p|%u|%g'" "$script"
! grep -Fq 'snapshot_visible_tree()' "$script"
grep -Fq 'find -H "${root}/sessions" "${root}/archived_sessions"' "$script"
grep -Fq 'shasum -a 256 "${rollout}"' "$script"
[[ "$(grep -Fc 'shasum -a 256' "$script")" == "1" ]]
grep -Fq 'snapshot_critical "${NATIVE_ROOT}" "${RUN_ROOT}/critical.after"' "$script"
grep -Fq 'app_servers_running()' "$script"
grep -Fq 'real-home Codex app servers did not drain' "$script"
grep -Fq 'Codex restarted during activation preflight; waiting again' "$script"
! grep -Fq 'fs compatibility' "$script"

stop_line=$(grep -n 'fs service stop --apply' "$script" | head -n 1 | cut -d: -f1)
deactivate_line=$(grep -n 'fs namespace deactivate --apply' "$script" | head -n 1 | cut -d: -f1)
start_line=$(grep -n 'fs service start --apply' "$script" | head -n 1 | cut -d: -f1)
[[ -n "$stop_line" && -n "$deactivate_line" && -n "$start_line" ]]
(( stop_line < deactivate_line && deactivate_line < start_line ))

root=$(mktemp -d)
runtime_root=""
trap 'rm -rf "$root"; [[ -z "$runtime_root" ]] || rm -rf "$runtime_root"' EXIT
mkdir -p "$root/native/sessions/2026/07/13" "$root/native/archived_sessions"
touch "$root/native/sessions/2026/07/13/rollout.jsonl"
ln -s "$root/native/sessions" "$root/sessions"
ln -s "$root/native/archived_sessions" "$root/archived_sessions"

count=$(cd "$root" && find -H sessions archived_sessions -type f | wc -l | tr -d ' ')
[[ "$count" == "1" ]]

echo "PASS: activation snapshots traverse canonical session symlinks"

runtime_root=$(mktemp -d)
mkdir -p "$runtime_root/home/sessions" "$runtime_root/home/archived_sessions" "$runtime_root/store/fs/sessions" "$runtime_root/mount" "$runtime_root/native" "$runtime_root/bin"
sqlite3 "$runtime_root/home/state_5.sqlite" 'create table threads (rollout_path text);'

cat >"$runtime_root/bin/pgrep" <<'EOF'
#!/bin/sh
if [ -e "$CODEXFOLD_FAKE_REOPEN_MARKER" ]; then
  rm -f "$CODEXFOLD_FAKE_REOPEN_MARKER"
  exit 0
fi
exit 1
EOF
cat >"$runtime_root/bin/sleep" <<'EOF'
#!/bin/sh
exit 0
EOF
cat >"$runtime_root/bin/find" <<'EOF'
#!/bin/sh
case "$*" in
  '-H sessions archived_sessions'*)
    if [ ! -e "$CODEXFOLD_FAKE_FIND_TRIGGERED" ]; then
      : >"$CODEXFOLD_FAKE_FIND_TRIGGERED"
      : >"$CODEXFOLD_FAKE_REOPEN_MARKER"
    fi
    ;;
esac
exec /usr/bin/find "$@"
EOF
cat >"$runtime_root/bin/shasum" <<'EOF'
#!/bin/sh
echo "full-tree SHA must not run during activation" >&2
exit 99
EOF
cat >"$runtime_root/bin/codexfold" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$CODEXFOLD_FAKE_LOG"
case "$*" in
  'fs service status --json')
    printf '%s\n' '{"daemon_running":true,"mount_healthy":true}'
    ;;
  'fs namespace activate'*)
    printf '%s\n' '{"active":true}'
    ;;
esac
EOF
chmod +x "$runtime_root/bin/pgrep" "$runtime_root/bin/sleep" "$runtime_root/bin/find" "$runtime_root/bin/shasum" "$runtime_root/bin/codexfold"

runtime_script="$runtime_root/activate.zsh"
sed "s|^export PATH=.*|export PATH=\"$runtime_root/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin\"|" "$script" >"$runtime_script"
runtime_log="$runtime_root/commands.log"
if CODEXFOLD_FAKE_LOG="$runtime_log" \
  CODEXFOLD_FAKE_REOPEN_MARKER="$runtime_root/reopened" \
  CODEXFOLD_FAKE_FIND_TRIGGERED="$runtime_root/find-triggered" \
  /bin/zsh "$runtime_script" \
  "$runtime_root/home" "$runtime_root/store" "$runtime_root/mount" "$runtime_root/native" \
  "$runtime_root/bin/codexfold" 0; then
  echo "activation unexpectedly succeeded" >&2
  exit 1
fi

grep -Fqx 'fs service stop --apply' "$runtime_log"
grep -Fq 'fs namespace deactivate --apply' "$runtime_log"
grep -Fq 'fs service start --apply' "$runtime_log"
[[ "$(grep -Fc 'fs namespace activate' "$runtime_log")" == "1" ]]
run_log=$(find "$runtime_root/store/activation" -type f -name run.log -print -quit)
if ! grep -Fq 'Codex restarted during activation preflight; waiting again' "$run_log"; then
  sed -n '1,120p' "$run_log" >&2
  exit 1
fi
failed_marker=$(find "$runtime_root/store/activation" -type f -name FAILED -print -quit)
[[ -n "$failed_marker" ]]

echo "PASS: activation failure trap restores the namespace under zsh"
