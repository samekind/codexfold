#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PREPARE="$ROOT_DIR/scripts/prepare-isolated-codex-home.sh"
TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT
REAL_CP=$(command -v cp)
REAL_SQLITE=$(command -v sqlite3)

make_copy_mutation_path() {
  local directory=$1
  local watched=$2
  local marker=$3
  mkdir -p "$directory"
  # The generated wrapper must retain its own variables and positional args.
  # shellcheck disable=SC2016
  printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    "real_cp=$(printf '%q' "$REAL_CP")" \
    "watched=$(printf '%q' "$watched")" \
    "marker=$(printf '%q' "$marker")" \
    '"$real_cp" "$@"' \
    'for arg in "$@"; do' \
    '  if [[ "$arg" == "$watched" && ! -e "$marker" ]]; then' \
    '    echo '\''{"session":"changed-during-copy"}'\'' >> "$watched"' \
    '    : > "$marker"' \
    '  fi' \
    'done' \
    > "$directory/cp"
  chmod 700 "$directory/cp"
}

make_vacuum_mutation_path() {
  local directory=$1
  local db=$2
  local id=$3
  local marker=$4
  local update_sql=${5:-"UPDATE threads SET archived=0 WHERE id='$id';"}
  mkdir -p "$directory"
  # The generated wrapper must retain its own variables and positional args.
  # shellcheck disable=SC2016
  printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    "real_sqlite=$(printf '%q' "$REAL_SQLITE")" \
    "db=$(printf '%q' "$db")" \
    "update_sql=$(printf '%q' "$update_sql")" \
    "marker=$(printf '%q' "$marker")" \
    'set +e' \
    '"$real_sqlite" "$@"' \
    'status=$?' \
    'set -e' \
    'if (( status == 0 )) && [[ "$*" == *"VACUUM INTO"* && ! -e "$marker" ]]; then' \
    '  "$real_sqlite" "$db" "$update_sql"' \
    '  : > "$marker"' \
    'fi' \
    'exit "$status"' \
    > "$directory/sqlite3"
  chmod 700 "$directory/sqlite3"
}

make_vacuum_file_mutation_path() {
  local directory=$1 watched=$2 marker=$3 mutation=$4
  mkdir -p "$directory"
  # shellcheck disable=SC2016
  printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    "real_sqlite=$(printf '%q' "$REAL_SQLITE")" \
    "watched=$(printf '%q' "$watched")" \
    "marker=$(printf '%q' "$marker")" \
    "mutation=$(printf '%q' "$mutation")" \
    'set +e' \
    '"$real_sqlite" "$@"' \
    'status=$?' \
    'set -e' \
    'if (( status == 0 )) && [[ "$*" == *"VACUUM INTO"* && ! -e "$marker" ]]; then' \
    '  printf "%s\n" "$mutation" >> "$watched"' \
    '  : > "$marker"' \
    'fi' \
    'exit "$status"' \
    > "$directory/sqlite3"
  chmod 700 "$directory/sqlite3"
}

source_home="$TEST_ROOT/source"
target_home="$TEST_ROOT/target"
mkdir -p "$source_home/plugins/cache/example" "$source_home/skills/example"
printf 'model_provider = "main"\n[model_providers.main]\nname = "third-party"\nbase_url = "https://third-party.example/v1"\n' > "$source_home/config.toml"
printf '{"access_token":"test-token"}\n' > "$source_home/auth.json"
printf '{"client_version":"test","models":[]}\n' > "$source_home/models_cache.json"
printf '{"models":[]}\n' > "$source_home/cockpit-local-access-model-catalog.json"
printf 'plugin-source\n' > "$source_home/plugins/cache/example/state"
printf 'skill-source\n' > "$source_home/skills/example/SKILL.md"
chmod 700 "$source_home"
chmod 600 "$source_home/config.toml" "$source_home/auth.json"

"$PREPARE" "$source_home" "$target_home" >/dev/null

cmp -s "$source_home/config.toml" "$target_home/config.toml"
cmp -s "$source_home/auth.json" "$target_home/auth.json"
cmp -s "$source_home/models_cache.json" "$target_home/models_cache.json"
cmp -s "$source_home/cockpit-local-access-model-catalog.json" "$target_home/cockpit-local-access-model-catalog.json"
[[ "$(stat -f '%Lp' "$target_home/config.toml")" == 600 ]]
[[ "$(stat -f '%Lp' "$target_home/auth.json")" == 600 ]]
[[ "$(stat -f '%Lp' "$target_home/models_cache.json")" == 600 ]]
[[ "$(stat -f '%Lp' "$target_home/cockpit-local-access-model-catalog.json")" == 600 ]]
[[ -d "$target_home/plugins" && ! -L "$target_home/plugins" ]]
printf 'plugin-target\n' > "$target_home/plugins/cache/example/state"
grep -Fqx 'plugin-source' "$source_home/plugins/cache/example/state"

active_old=11111111-1111-7111-8111-111111111111
active_new=22222222-2222-7222-8222-222222222222
archived_new=33333333-3333-7333-8333-333333333333
mkdir -p "$source_home/sessions/2026/07/24" "$source_home/sessions/2026/07/26" "$source_home/archived_sessions"
printf '{"session":"old"}\n' > "$source_home/sessions/2026/07/24/rollout-2026-07-24T01-00-00-$active_old.jsonl"
printf '{"session":"active-new"}\n' > "$source_home/sessions/2026/07/26/rollout-2026-07-26T01-00-00-$active_new.jsonl"
printf '{"session":"archived-new"}\n' > "$source_home/archived_sessions/rollout-2026-07-26T02-00-00-$archived_new.jsonl"
sqlite3 "$source_home/state_5.sqlite" <<SQL
CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL, updated_at INTEGER NOT NULL, archived INTEGER NOT NULL DEFAULT 0);
CREATE TABLE thread_dynamic_tools(thread_id TEXT NOT NULL, position INTEGER NOT NULL, PRIMARY KEY(thread_id, position));
CREATE TABLE thread_spawn_edges(parent_thread_id TEXT NOT NULL, child_thread_id TEXT NOT NULL PRIMARY KEY, status TEXT NOT NULL);
CREATE TABLE remote_control_enrollments(server_id TEXT PRIMARY KEY);
INSERT INTO threads VALUES('$active_old', '$source_home/sessions/2026/07/24/rollout-2026-07-24T01-00-00-$active_old.jsonl', 1, 0);
INSERT INTO threads VALUES('$active_new', '$source_home/sessions/2026/07/26/rollout-2026-07-26T01-00-00-$active_new.jsonl', 3, 0);
INSERT INTO threads VALUES('$archived_new', '$source_home/archived_sessions/rollout-2026-07-26T02-00-00-$archived_new.jsonl', 2, 1);
INSERT INTO thread_dynamic_tools VALUES('$active_old', 0);
INSERT INTO thread_dynamic_tools VALUES('$active_new', 0);
INSERT INTO thread_spawn_edges VALUES('$active_old', '$active_new', 'complete');
INSERT INTO remote_control_enrollments VALUES('must-not-copy');
SQL
printf '{"id":"%s","thread_name":"old","updated_at":"1"}\n' "$active_old" > "$source_home/session_index.jsonl"
printf '{"id":"%s","thread_name":"active-new","updated_at":"3"}\n' "$active_new" >> "$source_home/session_index.jsonl"
printf '{"id":"%s","thread_name":"archived-new","updated_at":"2"}\n' "$archived_new" >> "$source_home/session_index.jsonl"

slice_home="$TEST_ROOT/target slice's"
"$PREPARE" "$source_home" "$slice_home" --session-count 2 >/dev/null

[[ -f "$slice_home/sessions/2026/07/26/rollout-2026-07-26T01-00-00-$active_new.jsonl" ]]
[[ -f "$slice_home/archived_sessions/rollout-2026-07-26T02-00-00-$archived_new.jsonl" ]]
[[ ! -e "$slice_home/sessions/2026/07/24/rollout-2026-07-24T01-00-00-$active_old.jsonl" ]]
[[ "$(sqlite3 "$slice_home/state_5.sqlite" 'SELECT count(*) FROM threads;')" == 2 ]]
[[ "$(sqlite3 "$slice_home/state_5.sqlite" 'SELECT count(*) FROM remote_control_enrollments;')" == 0 ]]
[[ "$(sqlite3 "$slice_home/state_5.sqlite" 'SELECT count(*) FROM thread_dynamic_tools;')" == 1 ]]
[[ "$(sqlite3 "$slice_home/state_5.sqlite" 'SELECT count(*) FROM thread_spawn_edges;')" == 0 ]]
[[ "$(sqlite3 "$slice_home/state_5.sqlite" 'PRAGMA integrity_check;')" == ok ]]
[[ "$(sqlite3 "$slice_home/state_5.sqlite" "SELECT rollout_path FROM threads WHERE id='$active_new';")" == "$slice_home/sessions/2026/07/26/rollout-2026-07-26T01-00-00-$active_new.jsonl" ]]
[[ "$(wc -l < "$slice_home/selected-sessions.tsv" | tr -d ' ')" == 2 ]]
awk -F '\t' 'NF == 6 && $4 ~ /^[0-9]+$/ && $5 ~ /^[0-9a-f]{64}$/ && $6 ~ /^[0-9a-f]{64}$/ { ok++ } END { exit(ok == 2 ? 0 : 1) }' \
  "$slice_home/selected-sessions.tsv"
[[ "$(wc -l < "$slice_home/session_index.jsonl" | tr -d ' ')" == 2 ]]
if grep -Fq "$active_old" "$slice_home/session_index.jsonl"; then
  echo "unselected session remained in session_index.jsonl" >&2
  exit 1
fi

# Default selection skips a recent SQLite thread that has no exact
# session_index.jsonl row and fills the slice from the next consistent thread.
missing_index_id=99999999-9999-7999-8999-999999999999
missing_index_rollout="$source_home/sessions/2026/07/26/rollout-2026-07-26T09-00-00-$missing_index_id.jsonl"
printf '{"session":"missing-index"}\n' > "$missing_index_rollout"
sqlite3 "$source_home/state_5.sqlite" \
  "INSERT INTO threads VALUES('$missing_index_id', '$missing_index_rollout', 10, 0);"
missing_index_home="$TEST_ROOT/missing-index"
"$PREPARE" "$source_home" "$missing_index_home" --session-count 2 >/dev/null
[[ "$(sqlite3 "$missing_index_home/state_5.sqlite" "SELECT count(*) FROM threads WHERE id='$missing_index_id';")" == 0 ]]
[[ "$(sqlite3 "$missing_index_home/state_5.sqlite" 'SELECT count(*) FROM threads;')" == 2 ]]
sqlite3 "$source_home/state_5.sqlite" "DELETE FROM threads WHERE id='$missing_index_id';"
rm "$missing_index_rollout"

explicit_home="$TEST_ROOT/explicit"
"$PREPARE" "$source_home" "$explicit_home" --session "$active_old" >/dev/null
[[ "$(sqlite3 "$explicit_home/state_5.sqlite" 'SELECT count(*) FROM threads;')" == 1 ]]
[[ "$(sqlite3 "$explicit_home/state_5.sqlite" 'SELECT id FROM threads;')" == "$active_old" ]]
[[ -f "$explicit_home/sessions/2026/07/24/rollout-2026-07-24T01-00-00-$active_old.jsonl" ]]

# A default recent slice must skip a rollout that changes during clone/copy and
# continue to the next stable SQLite candidate.
active_new_path="$source_home/sessions/2026/07/26/rollout-2026-07-26T01-00-00-$active_new.jsonl"
active_new_path="$(cd "$(dirname "$active_new_path")" && pwd -P)/$(basename "$active_new_path")"
copy_hook="$TEST_ROOT/copy-hook"
copy_marker="$TEST_ROOT/copy-mutated"
make_copy_mutation_path "$copy_hook" "$active_new_path" "$copy_marker"
copy_race_home="$TEST_ROOT/copy-race"
PATH="$copy_hook:$PATH" "$PREPARE" "$source_home" "$copy_race_home" --session-count 2 >/dev/null 2>"$TEST_ROOT/copy-race.log"
[[ -f "$copy_marker" ]]
[[ "$(sqlite3 "$copy_race_home/state_5.sqlite" "SELECT count(*) FROM threads WHERE id='$active_new';")" == 0 ]]
[[ "$(sqlite3 "$copy_race_home/state_5.sqlite" 'SELECT count(*) FROM threads;')" == 2 ]]
grep -Fq 'changed while it was copied' "$TEST_ROOT/copy-race.log"

# A selected thread row that changes immediately after VACUUM INTO is rejected;
# default selection retries without that ID and fills from the next stable row.
sqlite_hook="$TEST_ROOT/sqlite-hook"
sqlite_marker="$TEST_ROOT/sqlite-mutated"
make_vacuum_mutation_path "$sqlite_hook" "$source_home/state_5.sqlite" "$archived_new" "$sqlite_marker"
sqlite_race_home="$TEST_ROOT/sqlite-race"
PATH="$sqlite_hook:$PATH" "$PREPARE" "$source_home" "$sqlite_race_home" --session-count 2 >/dev/null
[[ -f "$sqlite_marker" ]]
[[ "$(sqlite3 "$sqlite_race_home/state_5.sqlite" "SELECT count(*) FROM threads WHERE id='$archived_new';")" == 0 ]]
[[ "$(sqlite3 "$sqlite_race_home/state_5.sqlite" 'SELECT count(*) FROM threads;')" == 2 ]]

# Exact-ID requests fail closed instead of silently accepting a different file
# version when that source changes during copy.
active_old_path="$source_home/sessions/2026/07/24/rollout-2026-07-24T01-00-00-$active_old.jsonl"
active_old_path="$(cd "$(dirname "$active_old_path")" && pwd -P)/$(basename "$active_old_path")"
explicit_hook="$TEST_ROOT/explicit-copy-hook"
explicit_marker="$TEST_ROOT/explicit-copy-mutated"
make_copy_mutation_path "$explicit_hook" "$active_old_path" "$explicit_marker"
unstable_explicit_home="$TEST_ROOT/unstable-explicit"
if PATH="$explicit_hook:$PATH" "$PREPARE" "$source_home" "$unstable_explicit_home" --session "$active_old" >/dev/null 2>&1; then
  echo "explicit session accepted a source that changed during copy" >&2
  exit 1
fi
[[ -f "$explicit_marker" ]]
[[ ! -e "$unstable_explicit_home" ]]

# The copied rollout must remain the same exact source inode/bytes through the
# complete VACUUM snapshot window, not only through cp(1).
vacuum_rollout_hook="$TEST_ROOT/vacuum-rollout-hook"
vacuum_rollout_marker="$TEST_ROOT/vacuum-rollout-mutated"
make_vacuum_file_mutation_path "$vacuum_rollout_hook" "$active_old_path" "$vacuum_rollout_marker" '{"session":"changed-after-vacuum"}'
vacuum_rollout_home="$TEST_ROOT/vacuum-rollout-race"
if PATH="$vacuum_rollout_hook:$PATH" "$PREPARE" "$source_home" "$vacuum_rollout_home" --session "$active_old" >/dev/null 2>&1; then
  echo "explicit session accepted a rollout that changed across VACUUM" >&2
  exit 1
fi
[[ -f "$vacuum_rollout_marker" ]]
[[ ! -e "$vacuum_rollout_home" ]]

# Full selected SQLite row identity is fenced. A change to updated_at alone is
# enough to reject the slice even when archived and rollout_path are unchanged.
full_row_hook="$TEST_ROOT/full-row-hook"
full_row_marker="$TEST_ROOT/full-row-mutated"
make_vacuum_mutation_path "$full_row_hook" "$source_home/state_5.sqlite" "$active_old" "$full_row_marker" \
  "UPDATE threads SET updated_at=updated_at+1000 WHERE id='$active_old';"
full_row_home="$TEST_ROOT/full-row-race"
if PATH="$full_row_hook:$PATH" "$PREPARE" "$source_home" "$full_row_home" --session "$active_old" >/dev/null 2>&1; then
  echo "explicit session accepted a selected SQLite row that changed outside archived/path" >&2
  exit 1
fi
[[ -f "$full_row_marker" ]]
[[ ! -e "$full_row_home" ]]

# session_index.jsonl is part of the same stable evidence window.
index_hook="$TEST_ROOT/index-hook"
index_marker="$TEST_ROOT/index-mutated"
make_vacuum_file_mutation_path "$index_hook" "$source_home/session_index.jsonl" "$index_marker" \
  "{\"id\":\"$active_old\",\"thread_name\":\"raced\"}"
index_race_home="$TEST_ROOT/index-race"
if PATH="$index_hook:$PATH" "$PREPARE" "$source_home" "$index_race_home" --session "$active_old" >/dev/null 2>&1; then
  echo "explicit session accepted session_index.jsonl that changed across VACUUM" >&2
  exit 1
fi
[[ -f "$index_marker" ]]
[[ ! -e "$index_race_home" ]]

symlink_id=66666666-6666-7666-8666-666666666666
symlink_rollout="$source_home/sessions/2026/07/26/rollout-2026-07-26T05-00-00-$symlink_id.jsonl"
ln -s "$active_old_path" "$symlink_rollout"
sqlite3 "$source_home/state_5.sqlite" \
  "INSERT INTO threads VALUES('$symlink_id', '$symlink_rollout', 4, 0);"
symlink_home="$TEST_ROOT/symlink-explicit"
if "$PREPARE" "$source_home" "$symlink_home" --session "$symlink_id" >/dev/null 2>&1; then
  echo "explicit session accepted a symlink rollout" >&2
  exit 1
fi
[[ ! -e "$symlink_home" ]]

special_id=77777777-7777-7777-8777-777777777777
special_rollout="$source_home/sessions/2026/07/26/rollout-2026-07-26T06-00-00-$special_id.jsonl"
mkfifo "$special_rollout"
sqlite3 "$source_home/state_5.sqlite" \
  "INSERT INTO threads VALUES('$special_id', '$special_rollout', 5, 0);"
special_home="$TEST_ROOT/special-explicit"
if "$PREPARE" "$source_home" "$special_home" --session "$special_id" >/dev/null 2>&1; then
  echo "explicit session accepted a special-file rollout" >&2
  exit 1
fi
[[ ! -e "$special_home" ]]

echo "PASS: isolated CODEX_HOME copies exact sessions and a consistent pruned SQLite slice"
