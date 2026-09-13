#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: prepare-isolated-codex-home.sh SOURCE_CODEX_HOME TARGET_CODEX_HOME [options]

Options:
  --session-count N   Copy the N most recently updated sessions (maximum 10).
  --session ID        Copy this exact session. May be repeated (maximum 10).

Without a session option the command preserves the original behavior and creates
an empty sessions namespace. When sessions are selected, it also creates a
transactionally consistent, pruned state_5.sqlite and session_index.jsonl.
EOF
  exit 2
}

if [[ $# -lt 2 ]]; then
  usage
fi

source_input=$1
target_home=$2
shift 2

session_count=0
session_ids=("")
session_id_count=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --session-count)
      [[ $# -ge 2 ]] || usage
      session_count=$2
      shift 2
      ;;
    --session)
      [[ $# -ge 2 ]] || usage
      session_ids[session_id_count]=$2
      session_id_count=$((session_id_count + 1))
      shift 2
      ;;
    -h|--help)
      usage
      ;;
    *)
      echo "unknown option: $1" >&2
      usage
      ;;
  esac
done

if [[ ! "$session_count" =~ ^[0-9]+$ ]] || (( session_count > 10 )); then
  echo "--session-count must be an integer between 0 and 10" >&2
  exit 2
fi
if (( session_count > 0 && session_id_count > 0 )); then
  echo "--session-count and --session cannot be combined" >&2
  exit 2
fi
if (( session_id_count > 10 )); then
  echo "at most 10 explicit sessions may be selected" >&2
  exit 2
fi
for ((session_index = 0; session_index < session_id_count; session_index++)); do
  session_id=${session_ids[$session_index]}
  if [[ ! "$session_id" =~ ^[0-9A-Za-z_-]+$ ]]; then
    echo "invalid session id: $session_id" >&2
    exit 2
  fi
done

source_home=$(cd "$source_input" && pwd -P)
if [[ -e "$target_home" ]]; then
  echo "target CODEX_HOME already exists: $target_home" >&2
  exit 2
fi
target_parent=$(dirname "$target_home")
mkdir -p "$target_parent"
target_parent=$(cd "$target_parent" && pwd -P)
target_home="$target_parent/$(basename "$target_home")"

if [[ "$source_home" == "$target_home" ]]; then
  echo "source and target CODEX_HOME must differ" >&2
  exit 2
fi
if [[ ! -f "$source_home/config.toml" || ! -f "$source_home/auth.json" ]]; then
  echo "source CODEX_HOME must contain config.toml and auth.json" >&2
  exit 2
fi

selected_limit=$session_count
if (( session_id_count > 0 )); then
  selected_limit=$session_id_count
fi
if (( selected_limit > 0 )); then
  command -v sqlite3 >/dev/null || {
    echo "sqlite3 is required to create a consistent session slice" >&2
    exit 2
  }
  command -v shasum >/dev/null || {
    echo "shasum is required to record copied session identity" >&2
    exit 2
  }
  if [[ -f "$source_home/session_index.jsonl" ]]; then
    command -v jq >/dev/null || {
      echo "jq is required to slice session_index.jsonl" >&2
      exit 2
    }
  fi
  if [[ ! -f "$source_home/state_5.sqlite" ]]; then
    echo "selected sessions require source state_5.sqlite" >&2
    exit 2
  fi
fi

created=false
selection_rows=
candidate_rows=
selected_db_rows=
skipped_ids=
changed_rows=
session_index_copy=
cleanup() {
  status=$?
  [[ -z "$selection_rows" ]] || rm -f "$selection_rows"
  [[ -z "$candidate_rows" ]] || rm -f "$candidate_rows"
  [[ -z "$selected_db_rows" ]] || rm -f "$selected_db_rows"
  [[ -z "$skipped_ids" ]] || rm -f "$skipped_ids"
  [[ -z "$changed_rows" ]] || rm -f "$changed_rows"
  [[ -z "$session_index_copy" ]] || rm -f "$session_index_copy"
  if (( status != 0 )) && [[ "$created" == true ]]; then
    rm -rf "$target_home"
  fi
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$target_home"
created=true
chmod 700 "$target_home"

# Provider/auth inputs are copied byte-for-byte but are never printed. The
# disposable home is private to the current user.
for name in config.toml auth.json; do
  cp -p "$source_home/$name" "$target_home/$name"
  chmod 600 "$target_home/$name"
done
if [[ -f "$source_home/models_cache.json" ]]; then
  cp -p "$source_home/models_cache.json" "$target_home/models_cache.json"
  chmod 600 "$target_home/models_cache.json"
fi

catalog=cockpit-local-access-model-catalog.json
if [[ -f "$source_home/$catalog" ]]; then
  cp -p "$source_home/$catalog" "$target_home/$catalog"
  chmod 600 "$target_home/$catalog"
fi

mkdir -p "$target_home/sessions" "$target_home/archived_sessions"

# APFS clone avoids duplicating immutable assets. The fallback keeps the script
# usable on filesystems that do not support clonefile(2).
for name in plugins skills vendor_sources computer-use; do
  if [[ -e "$source_home/$name" ]]; then
    if ! cp -cR "$source_home/$name" "$target_home/$name" 2>/dev/null; then
      cp -pR "$source_home/$name" "$target_home/$name"
    fi
  fi
done

sql_quote() {
  local value=$1
  value=${value//\'/\'\'}
  printf '%s' "$value"
}

source_file_identity() {
  local path=$1
  [[ -f "$path" && ! -L "$path" ]] || return 1
  stat -f '%d:%i:%z:%m:%c:%p' "$path"
}

source_thread_row() {
  local id=$1
  local quoted_id
  quoted_id=$(sql_quote "$id")
  sqlite3 -batch -noheader -separator $'\t' "$source_home/state_5.sqlite" \
    "SELECT archived, rollout_path FROM threads WHERE id = '$quoted_id' LIMIT 1;"
}

thread_row_identity() {
  local db=$1 id=$2 quoted_id count
  quoted_id=$(sql_quote "$id")
  count=$(sqlite3 -batch -noheader "$db" "SELECT count(*) FROM threads WHERE id = '$quoted_id';") || return 1
  [[ "$count" == 1 ]] || return 1
  sqlite3 -batch -noheader -quote "$db" "SELECT * FROM threads WHERE id = '$quoted_id';" | \
    shasum -a 256 | awk '{print $1}'
}

path_has_control_characters() {
  local value=$1
  [[ "$value" == *$'\t'* || "$value" == *$'\r'* || "$value" == *$'\n'* ]]
}

copy_selected_rollout() {
  local id=$1
  local db_archived=$2
  local db_rollout_path=$3
  local source_path source_parent source_namespace source_namespace_root sessions_root archived_root namespace_name target_path relative_path actual_archived
  local namespace_identity_before namespace_identity_after namespace_root_identity_before namespace_root_identity_after
  local source_identity_before source_identity_after source_sha_before source_sha_after source_bytes
  local target_sha target_bytes current_row expected_row row_identity_before row_identity_after

  [[ "$id" =~ ^[0-9A-Za-z_-]+$ ]] || {
    echo "selected session id is not safe for an exact slice" >&2
    return 1
  }
  path_has_control_characters "$db_rollout_path" && {
    echo "selected rollout path contains control characters: $id" >&2
    return 1
  }

  if [[ -z "$db_rollout_path" || ! -f "$db_rollout_path" || -L "$db_rollout_path" ]]; then
    return 1
  fi
  source_parent=$(cd "$(dirname "$db_rollout_path")" 2>/dev/null && pwd -P) || return 1
  source_path="$source_parent/$(basename "$db_rollout_path")"
  sessions_root=$(cd "$source_home/sessions" 2>/dev/null && pwd -P) || return 1
  archived_root=$(cd "$source_home/archived_sessions" 2>/dev/null && pwd -P) || return 1
  case "$source_path" in
    "$sessions_root/"*)
      actual_archived=0
      namespace_name=sessions
      source_namespace="$source_home/sessions"
      source_namespace_root=$sessions_root
      ;;
    "$archived_root/"*)
      actual_archived=1
      namespace_name=archived_sessions
      source_namespace="$source_home/archived_sessions"
      source_namespace_root=$archived_root
      ;;
    *)
      echo "selected rollout escaped the Codex session namespace: $id" >&2
      return 1
      ;;
  esac
  namespace_identity_before=$(stat -f '%d:%i:%z:%m:%c:%p:%HT:%Y' "$source_namespace" 2>/dev/null) || return 1
  namespace_root_identity_before=$(stat -f '%d:%i:%m:%c:%p:%HT' "$source_namespace_root" 2>/dev/null) || return 1
  relative_path="$namespace_name/${source_path#"$source_namespace_root/"}"
  if [[ "$actual_archived" != "$db_archived" ]]; then
    echo "session archive state disagrees with its rollout location: $id" >&2
    return 1
  fi

  row_identity_before=$(thread_row_identity "$source_home/state_5.sqlite" "$id") || return 1
  source_identity_before=$(source_file_identity "$source_path") || {
    echo "selected rollout is not a regular non-symlink file: $id" >&2
    return 1
  }
  source_bytes=$(stat -f '%z' "$source_path")
  source_sha_before=$(shasum -a 256 "$source_path" | awk '{print $1}')

  target_path="$target_home/$relative_path"
  mkdir -p "$(dirname "$target_path")"
  rm -f "$target_path"
  if ! cp -c -p "$source_path" "$target_path" 2>/dev/null; then
    rm -f "$target_path"
    cp -p "$source_path" "$target_path" || {
      rm -f "$target_path"
      return 1
    }
  fi
  chmod 600 "$target_path"
  source_identity_after=$(source_file_identity "$source_path") || {
    rm -f "$target_path"
    echo "selected rollout changed type during copy: $id" >&2
    return 1
  }
  namespace_identity_after=$(stat -f '%d:%i:%z:%m:%c:%p:%HT:%Y' "$source_namespace" 2>/dev/null || true)
  namespace_root_identity_after=$(stat -f '%d:%i:%m:%c:%p:%HT' "$source_namespace_root" 2>/dev/null || true)
  source_sha_after=$(shasum -a 256 "$source_path" | awk '{print $1}')
  target_bytes=$(stat -f '%z' "$target_path")
  target_sha=$(shasum -a 256 "$target_path" | awk '{print $1}')
  if [[ "$namespace_identity_before" != "$namespace_identity_after" || \
        "$namespace_root_identity_before" != "$namespace_root_identity_after" || \
        "$source_identity_before" != "$source_identity_after" || \
        "$source_sha_before" != "$source_sha_after" || \
        "$target_bytes" != "$source_bytes" || "$target_sha" != "$source_sha_before" ]]; then
    rm -f "$target_path"
    echo "selected rollout changed while it was copied: $id" >&2
    return 1
  fi

  current_row=$(source_thread_row "$id")
  expected_row=$(printf '%s\t%s' "$db_archived" "$db_rollout_path")
  if [[ "$current_row" != "$expected_row" ]]; then
    rm -f "$target_path"
    echo "selected session row changed while its rollout was copied: $id" >&2
    return 1
  fi
  row_identity_after=$(thread_row_identity "$source_home/state_5.sqlite" "$id") || {
    rm -f "$target_path"
    return 1
  }
  if [[ "$row_identity_before" != "$row_identity_after" ]]; then
    rm -f "$target_path"
    echo "selected session row changed while its rollout was copied: $id" >&2
    return 1
  fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$id" "$actual_archived" "$relative_path" "$target_bytes" "$target_sha" "$row_identity_after" >> "$selection_rows"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$id" "$db_archived" "$db_rollout_path" "$relative_path" "$row_identity_after" "$source_identity_after" "$source_bytes" "$source_sha_after" >> "$selected_db_rows"
}

verify_selected_rows_in_db() {
  local db=$1
  local changed=$2
  local id archived rollout_path _relative_path row_identity _source_identity _source_bytes _source_sha quoted_id row expected current_identity
  local valid=true
  [[ -f "$db" ]] || return 1
  while IFS=$'\t' read -r id archived rollout_path _relative_path row_identity _source_identity _source_bytes _source_sha; do
    [[ -n "$id" ]] || continue
    quoted_id=$(sql_quote "$id")
    if ! row=$(sqlite3 -batch -noheader -separator $'\t' "$db" \
      "SELECT archived, rollout_path FROM threads WHERE id = '$quoted_id' LIMIT 1;"); then
      printf '%s\n' "$id" >> "$changed"
      valid=false
      continue
    fi
    expected=$(printf '%s\t%s' "$archived" "$rollout_path")
    if [[ "$row" != "$expected" ]]; then
      printf '%s\n' "$id" >> "$changed"
      valid=false
      continue
    fi
    current_identity=$(thread_row_identity "$db" "$id" 2>/dev/null || true)
    if [[ "$current_identity" != "$row_identity" ]]; then
      printf '%s\n' "$id" >> "$changed"
      valid=false
    fi
  done < "$selected_db_rows"
  [[ "$valid" == true ]]
}

verify_selected_rollout_copies() {
  local id _archived source_path relative_path _row_identity expected_source_identity expected_bytes expected_sha
  local source_identity source_bytes source_sha target_path target_bytes target_sha
  ROLLOUT_CHANGED_ID=''
  while IFS=$'\t' read -r id _archived source_path relative_path _row_identity expected_source_identity expected_bytes expected_sha; do
    [[ -n "$id" ]] || continue
    source_identity=$(source_file_identity "$source_path") || { ROLLOUT_CHANGED_ID=$id; return 1; }
    [[ "$source_identity" == "$expected_source_identity" ]] || { ROLLOUT_CHANGED_ID=$id; return 1; }
    source_bytes=$(stat -f '%z' "$source_path") || { ROLLOUT_CHANGED_ID=$id; return 1; }
    source_sha=$(shasum -a 256 "$source_path" | awk '{print $1}') || { ROLLOUT_CHANGED_ID=$id; return 1; }
    [[ "$source_bytes" == "$expected_bytes" && "$source_sha" == "$expected_sha" ]] || { ROLLOUT_CHANGED_ID=$id; return 1; }
    target_path="$target_home/$relative_path"
    [[ -f "$target_path" && ! -L "$target_path" ]] || { ROLLOUT_CHANGED_ID=$id; return 1; }
    target_bytes=$(stat -f '%z' "$target_path") || { ROLLOUT_CHANGED_ID=$id; return 1; }
    target_sha=$(shasum -a 256 "$target_path" | awk '{print $1}') || { ROLLOUT_CHANGED_ID=$id; return 1; }
    [[ "$target_bytes" == "$expected_bytes" && "$target_sha" == "$expected_sha" ]] || { ROLLOUT_CHANGED_ID=$id; return 1; }
  done < "$selected_db_rows"
}

remember_skipped_rows() {
  local changed=$1
  local id
  while IFS= read -r id; do
    [[ -n "$id" ]] || continue
    if ! grep -Fqx "$id" "$skipped_ids"; then
      printf '%s\n' "$id" >> "$skipped_ids"
    fi
  done < "$changed"
}

session_index_row_count() {
  local path=$1 id=$2
  jq -c --arg id "$id" 'select(.id == $id)' "$path" | wc -l | tr -d ' '
}

if (( selected_limit > 0 )); then
  selection_rows=$(mktemp "${TMPDIR:-/tmp}/codexfold-selected.XXXXXX")
  candidate_rows=$(mktemp "${TMPDIR:-/tmp}/codexfold-candidates.XXXXXX")
  selected_db_rows=$(mktemp "${TMPDIR:-/tmp}/codexfold-selected-db.XXXXXX")
  skipped_ids=$(mktemp "${TMPDIR:-/tmp}/codexfold-skipped.XXXXXX")
  changed_rows=$(mktemp "${TMPDIR:-/tmp}/codexfold-changed.XXXXXX")

  if (( session_id_count > 0 )); then
    for ((session_index = 0; session_index < session_id_count; session_index++)); do
      session_id=${session_ids[$session_index]}
      quoted_id=$(sql_quote "$session_id")
      row=$(sqlite3 -batch -noheader -separator $'\t' "$source_home/state_5.sqlite" \
        "SELECT id, archived, rollout_path FROM threads WHERE id = '$quoted_id' LIMIT 1;")
      if [[ -z "$row" ]]; then
        echo "session is absent from state_5.sqlite: $session_id" >&2
        exit 1
      fi
      printf '%s\n' "$row" >> "$candidate_rows"
    done
  else
    sqlite3 -batch -noheader -separator $'\t' "$source_home/state_5.sqlite" \
      "SELECT id, archived, rollout_path FROM threads ORDER BY updated_at DESC, id DESC;" > "$candidate_rows"
  fi

  target_db="$target_home/state_5.sqlite"
  snapshot_attempts=0
  while true; do
    snapshot_attempts=$((snapshot_attempts + 1))
    (( snapshot_attempts <= 30 )) || {
      echo "could not obtain a stable session/SQLite slice after 30 attempts" >&2
      exit 1
    }
    : > "$selection_rows"
    : > "$selected_db_rows"
    rm -rf "$target_home/sessions" "$target_home/archived_sessions"
    mkdir -p "$target_home/sessions" "$target_home/archived_sessions"
    selected=0
    while IFS=$'\t' read -r session_id archived rollout_path; do
      [[ -n "$session_id" ]] || continue
      if grep -Fqx "$session_id" "$skipped_ids"; then
        continue
      fi
      if [[ -e "$source_home/session_index.jsonl" ]]; then
        index_row_count=$(session_index_row_count "$source_home/session_index.jsonl" "$session_id") || {
          echo "source session_index.jsonl is not valid JSONL" >&2
          exit 1
        }
        if [[ "$index_row_count" != 1 ]]; then
          if (( session_id_count > 0 )); then
            echo "session_index.jsonl does not contain exactly one row for $session_id" >&2
            exit 1
          fi
          printf '%s\n' "$session_id" >> "$skipped_ids"
          continue
        fi
      fi
      if copy_selected_rollout "$session_id" "$archived" "$rollout_path"; then
        selected=$((selected + 1))
        if (( selected == selected_limit )); then
          break
        fi
      elif (( session_id_count > 0 )); then
        echo "selected session could not be copied as one stable version: $session_id" >&2
        exit 1
      else
        printf '%s\n' "$session_id" >> "$skipped_ids"
      fi
    done < "$candidate_rows"
    if (( selected != selected_limit )); then
      echo "requested $selected_limit sessions but only found $selected stable sessions" >&2
      exit 1
    fi

    : > "$changed_rows"
    if ! verify_selected_rows_in_db "$source_home/state_5.sqlite" "$changed_rows"; then
      if (( session_id_count > 0 )); then
        echo "selected session row changed before SQLite snapshot" >&2
        exit 1
      fi
      remember_skipped_rows "$changed_rows"
      continue
    fi
    if ! verify_selected_rollout_copies; then
      if (( session_id_count > 0 )); then
        echo "selected rollout changed before SQLite snapshot: $ROLLOUT_CHANGED_ID" >&2
        exit 1
      fi
      printf '%s\n' "$ROLLOUT_CHANGED_ID" > "$changed_rows"
      remember_skipped_rows "$changed_rows"
      continue
    fi
    session_index_identity=''
    session_index_sha=''
    if [[ -e "$source_home/session_index.jsonl" ]]; then
      [[ -f "$source_home/session_index.jsonl" && ! -L "$source_home/session_index.jsonl" ]] || {
        echo "source session_index.jsonl must be a regular non-symlink file" >&2
        exit 1
      }
      session_index_identity=$(source_file_identity "$source_home/session_index.jsonl")
      session_index_sha=$(shasum -a 256 "$source_home/session_index.jsonl" | awk '{print $1}')
    fi

    rm -f "$target_db"
    quoted_target_db=$(sql_quote "$target_db")
    sqlite3 -batch "$source_home/state_5.sqlite" \
      "PRAGMA busy_timeout=5000; VACUUM INTO '$quoted_target_db';"

    : > "$changed_rows"
    source_rows_stable=true
    snapshot_rows_match=true
    verify_selected_rows_in_db "$source_home/state_5.sqlite" "$changed_rows" || source_rows_stable=false
    verify_selected_rows_in_db "$target_db" "$changed_rows" || snapshot_rows_match=false
    rollouts_stable=true
    verify_selected_rollout_copies || {
      rollouts_stable=false
      [[ -z "$ROLLOUT_CHANGED_ID" ]] || printf '%s\n' "$ROLLOUT_CHANGED_ID" >> "$changed_rows"
    }
    session_index_stable=true
    if [[ -n "$session_index_identity" ]]; then
      current_index_identity=$(source_file_identity "$source_home/session_index.jsonl" 2>/dev/null || true)
      current_index_sha=$(shasum -a 256 "$source_home/session_index.jsonl" 2>/dev/null | awk '{print $1}' || true)
      [[ "$current_index_identity" == "$session_index_identity" && "$current_index_sha" == "$session_index_sha" ]] || session_index_stable=false
    fi
    if [[ "$source_rows_stable" != true || "$snapshot_rows_match" != true || "$rollouts_stable" != true || "$session_index_stable" != true ]]; then
      rm -f "$target_db"
      if (( session_id_count > 0 )); then
        echo "selected session, rollout, or session index changed during SQLite snapshot" >&2
        exit 1
      fi
      if [[ "$session_index_stable" != true ]]; then
        echo "session_index.jsonl changed during SQLite snapshot" >&2
        exit 1
      fi
      remember_skipped_rows "$changed_rows"
      continue
    fi
    break
  done

  prune_sql=$(mktemp "${TMPDIR:-/tmp}/codexfold-prune.XXXXXX")
  {
    printf 'PRAGMA foreign_keys=OFF;\nBEGIN IMMEDIATE;\n'
    printf 'CREATE TEMP TABLE codexfold_selected_threads(id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL, archived INTEGER NOT NULL);\n'
    while IFS=$'\t' read -r session_id archived relative_path _bytes _sha _row_identity; do
      quoted_id=$(sql_quote "$session_id")
      quoted_path=$(sql_quote "$target_home/$relative_path")
      printf "INSERT INTO codexfold_selected_threads VALUES('%s','%s',%s);\n" "$quoted_id" "$quoted_path" "$archived"
    done < "$selection_rows"
    if [[ "$(sqlite3 "$target_db" "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='thread_dynamic_tools';")" == 1 ]]; then
      printf 'DELETE FROM thread_dynamic_tools WHERE thread_id NOT IN (SELECT id FROM codexfold_selected_threads);\n'
    fi
    if [[ "$(sqlite3 "$target_db" "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='thread_spawn_edges';")" == 1 ]]; then
      printf 'DELETE FROM thread_spawn_edges WHERE parent_thread_id NOT IN (SELECT id FROM codexfold_selected_threads) OR child_thread_id NOT IN (SELECT id FROM codexfold_selected_threads);\n'
    fi
    printf 'DELETE FROM threads WHERE id NOT IN (SELECT id FROM codexfold_selected_threads);\n'
    printf 'UPDATE threads SET rollout_path=(SELECT rollout_path FROM codexfold_selected_threads WHERE id=threads.id), archived=(SELECT archived FROM codexfold_selected_threads WHERE id=threads.id);\n'
    if [[ "$(sqlite3 "$target_db" "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='remote_control_enrollments';")" == 1 ]]; then
      printf 'DELETE FROM remote_control_enrollments;\n'
    fi
    printf 'COMMIT;\nVACUUM;\n'
  } > "$prune_sql"
  sqlite3 -batch "$target_db" < "$prune_sql"
  rm -f "$prune_sql"
  if [[ "$(sqlite3 -batch -noheader "$target_db" 'PRAGMA integrity_check;')" != ok ]]; then
    echo "sliced state_5.sqlite failed integrity_check" >&2
    exit 1
  fi
  chmod 600 "$target_db"

  cp "$selection_rows" "$target_home/selected-sessions.tsv"
  chmod 600 "$target_home/selected-sessions.tsv"

  if [[ -n "${session_index_identity:-}" ]]; then
    session_index_copy=$(mktemp "${TMPDIR:-/tmp}/codexfold-session-index.XXXXXX")
    before_index_identity=$(source_file_identity "$source_home/session_index.jsonl")
    before_index_sha=$(shasum -a 256 "$source_home/session_index.jsonl" | awk '{print $1}')
    [[ "$before_index_identity" == "$session_index_identity" && "$before_index_sha" == "$session_index_sha" ]] || {
      echo "session_index.jsonl changed before its stable copy" >&2
      exit 1
    }
    cp -p "$source_home/session_index.jsonl" "$session_index_copy"
    after_index_identity=$(source_file_identity "$source_home/session_index.jsonl")
    after_index_sha=$(shasum -a 256 "$source_home/session_index.jsonl" | awk '{print $1}')
    copied_index_sha=$(shasum -a 256 "$session_index_copy" | awk '{print $1}')
    [[ "$after_index_identity" == "$session_index_identity" && "$after_index_sha" == "$session_index_sha" && "$copied_index_sha" == "$session_index_sha" ]] || {
      echo "session_index.jsonl changed while it was copied" >&2
      exit 1
    }
    : > "$target_home/session_index.jsonl"
    while IFS=$'\t' read -r session_id _archived _relative_path _bytes _sha _row_identity; do
      index_match=$(mktemp "${TMPDIR:-/tmp}/codexfold-session-index-match.XXXXXX")
      jq -c --arg id "$session_id" 'select(.id == $id)' "$session_index_copy" > "$index_match"
      [[ "$(wc -l < "$index_match" | tr -d ' ')" == 1 ]] || {
        rm -f "$index_match"
        echo "session_index.jsonl does not contain exactly one row for $session_id" >&2
        exit 1
      }
      cat "$index_match" >> "$target_home/session_index.jsonl"
      rm -f "$index_match"
    done < "$selection_rows"
    chmod 600 "$target_home/session_index.jsonl"
    rm -f "$session_index_copy"
    session_index_copy=''
  fi
fi

trap - EXIT HUP INT TERM
[[ -z "$selection_rows" ]] || rm -f "$selection_rows"
[[ -z "$candidate_rows" ]] || rm -f "$candidate_rows"
[[ -z "$selected_db_rows" ]] || rm -f "$selected_db_rows"
[[ -z "$skipped_ids" ]] || rm -f "$skipped_ids"
[[ -z "$changed_rows" ]] || rm -f "$changed_rows"
[[ -z "$session_index_copy" ]] || rm -f "$session_index_copy"
printf 'prepared isolated CODEX_HOME: %s (sessions=%d)\n' "$target_home" "$selected_limit"
