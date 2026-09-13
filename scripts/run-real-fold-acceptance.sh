#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd -P)
PREPARE_HOME="$SCRIPT_DIR/prepare-isolated-codex-home.sh"
SCHEMA=codexfold.real-fold-acceptance.v1
FSKIT_MODULE_BUNDLE_NAME=CodexFoldFSKitModule.appex
FSKIT_MODULE_PROCESS_NAME=CodexFoldFSKitModule
VERIFICATION_APP_BUNDLE_ID=vip.jstar.codexfold.fskitacceptance108
VERIFICATION_MODULE_BUNDLE_ID=vip.jstar.codexfold.fskitacceptance108.module
VERIFICATION_APP_DISPLAY_NAME='CodexFold Verification'
VERIFICATION_MODULE_DISPLAY_NAME='CodexFold Verification Module'
VERIFICATION_FSKIT_TYPE=codexfoldverification
VERIFICATION_APP_PATH=''

usage() {
  cat <<'EOF'
usage: run-real-fold-acceptance.sh [options]

Build the current worktree and prove automatic fold -> pack -> migrate against
a byte-exact copy of one real archived Codex session. The unmodified Codex CLI
then unarchives, resumes, and appends to that managed session.

Options:
  --source-home DIR   Read-only source CODEX_HOME (default: ~/.codex).
  --run-root DIR      New disposable run root (default: .tmp/real-fold-<time>).
  --session ID        Exact archived source session; otherwise select one safely.
  --codex-cli PATH    Unmodified Codex CLI (default: ChatGPT.app resource).
  --candidate-bin PATH
                      Execute a byte-exact copy of this prebuilt helper instead
                      of compiling another helper inside the acceptance run.
  --candidate-app PATH
                      The fixed registered Verification App produced by
                      build-codexfold-verification-app.sh. Required for
                      native-fskit; no other App identity is accepted.
  --fskit-type NAME   FSKit scheme from the candidate module Info.plist. When
                      --candidate-app is supplied it must match exactly.
  --model MODEL       Model for the real resume (default: gpt-5.6-terra).
  --frontend NAME     fuse or native-fskit (default: fuse).
  --timeout SECONDS   Maximum real Codex resume time (default: 240).
  --performance       Run real native and managed read/write benchmarks before resume.
  --skip-resume       Do not send a model request; report CLI resume as NOT RUN.
  --desktop-app PATH  Open the independently identified Desktop on the reclaimed
                      session; wait up to 20 minutes for evidence/desktop.done.
  -h, --help          Show this help.

The command never writes to the source CODEX_HOME and never signals production
Codex or CodexFold. Runtime data and copied credentials are removed on exit;
redacted evidence remains under RUN_ROOT/evidence.
EOF
}

die() {
  echo "$*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command is unavailable: $1"
}

timestamp() {
  date -u '+%Y-%m-%dT%H:%M:%SZ'
}

file_sha() {
  shasum -a 256 "$1" | awk '{print $1}'
}

bundle_info_value() {
  local bundle=$1 key=$2
  plutil -extract "$key" raw -o - "$bundle/Contents/Info.plist" 2>/dev/null || true
}

codesign_detail_value() {
  local path=$1 key=$2
  codesign -d --verbose=4 "$path" 2>&1 | sed -n "s/^${key}=//p" | head -n 1
}

candidate_app_path() {
  local requested=$1 canonical module
  canonical=$(canonical_existing_dir "$requested") || return 1
  [[ "$(basename "$canonical")" == *.app ]] || return 1
  module="$canonical/Contents/Extensions/$FSKIT_MODULE_BUNDLE_NAME"
  [[ -d "$module" && ! -L "$module" && -f "$module/Contents/Info.plist" && ! -L "$module/Contents/Info.plist" ]] || return 1
  printf '%s\n' "$canonical"
}

validate_candidate_native_app() {
  local requested=$1 app module app_id module_id module_exec module_exec_path app_exec fs_type registered
  app=$(candidate_app_path "$requested") || return 1
  VERIFICATION_APP_PATH=$(canonical_existing_dir "$HOME/Applications/CodexFoldVerification.app" 2>/dev/null || true)
  [[ -n "$VERIFICATION_APP_PATH" && "$app" == "$VERIFICATION_APP_PATH" ]] || return 1
  module="$app/Contents/Extensions/$FSKIT_MODULE_BUNDLE_NAME"
  app_id=$(bundle_info_value "$app" CFBundleIdentifier)
  module_id=$(bundle_info_value "$module" CFBundleIdentifier)
  module_exec=$(bundle_info_value "$module" CFBundleExecutable)
  fs_type=$(plutil -extract EXAppExtensionAttributes.FSShortName raw -o - "$module/Contents/Info.plist" 2>/dev/null || true)
  [[ -n "$app_id" && -n "$module_id" && -n "$module_exec" && -n "$fs_type" ]] || return 1
  [[ "$app_id" == "$VERIFICATION_APP_BUNDLE_ID" && "$module_id" == "$VERIFICATION_MODULE_BUNDLE_ID" ]] || return 1
  [[ "$(bundle_info_value "$app" CFBundleDisplayName)" == "$VERIFICATION_APP_DISPLAY_NAME" ]] || return 1
  [[ "$(bundle_info_value "$module" CFBundleDisplayName)" == "$VERIFICATION_MODULE_DISPLAY_NAME" ]] || return 1
  [[ "$fs_type" == "$VERIFICATION_FSKIT_TYPE" ]] || return 1
  [[ "$module_exec" == "$(basename "$module_exec")" && "$fs_type" =~ ^[a-z][a-z0-9]{0,31}$ ]] || return 1
  module_exec_path="$module/Contents/MacOS/$module_exec"
  [[ -f "$module_exec_path" && ! -L "$module_exec_path" && -x "$module_exec_path" ]] || return 1
  codesign --verify --deep --strict "$app" >/dev/null 2>&1 || return 1
  registered=$(pluginkit -m -A -D -v -i "$module_id" 2>/dev/null || true)
  printf '%s\n' "$registered" | awk -F '\t' -v target="$module" '{ path=$NF; sub(/^[[:space:]]+/, "", path); sub(/[[:space:]]+$/, "", path); if (path == target) found=1 } END { exit(found ? 0 : 1) }' || return 1
  CANDIDATE_APP="$app"
  CANDIDATE_MODULE="$module"
  CANDIDATE_APP_BUNDLE_ID="$app_id"
  CANDIDATE_MODULE_BUNDLE_ID="$module_id"
  CANDIDATE_MODULE_EXECUTABLE="$module_exec_path"
  CANDIDATE_MODULE_SHA=$(file_sha "$module_exec_path")
  CANDIDATE_FSKIT_TYPE="$fs_type"
  app_exec=$(bundle_info_value "$app" CFBundleExecutable)
  CANDIDATE_APP_SHA=$(file_sha "$app/Contents/MacOS/$app_exec")
}

module_process_pids_for_path() {
  local executable=$1
  ps -axo pid=,comm= | awk -v expected="$executable" '$2 == expected { print $1 }'
}

write_module_process_snapshot_for_path() {
  local executable=$1 output=$2 temporary pid ppid start command_sha
  temporary=$(mktemp "$(dirname "$output")/.module-processes.XXXXXX") || return 1
  while IFS= read -r pid; do
    [[ "$pid" =~ ^[0-9]+$ ]] || { rm -f "$temporary"; return 1; }
    ppid=$(ps -p "$pid" -o ppid= 2>/dev/null | awk '{$1=$1;print}')
    start=$(ps -p "$pid" -o lstart= 2>/dev/null | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
    command_sha=$(ps -ww -p "$pid" -o command= 2>/dev/null | shasum -a 256 | awk '{print $1}')
    [[ "$ppid" =~ ^[0-9]+$ && -n "$start" && "$command_sha" =~ ^[0-9a-f]{64}$ ]] || { rm -f "$temporary"; return 1; }
    printf '%s\t%s\t%s\t%s\t%s\n' "$pid" "$ppid" "$start" "$executable" "$command_sha" >> "$temporary"
  done < <(module_process_pids_for_path "$executable")
  chmod 600 "$temporary"
  mv "$temporary" "$output"
}

wait_for_candidate_module_process() {
  local executable=$1 output=$2 deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    write_module_process_snapshot_for_path "$executable" "$output" || return 1
    [[ -s "$output" ]] && return 0
    sleep 0.25
  done
  return 1
}

production_module_unchanged() {
  local baseline=$1 row pid ppid start executable command_sha current_ppid current_start current_executable current_sha
  [[ -f "$baseline" && ! -L "$baseline" ]] || return 1
  while IFS=$'\t' read -r pid ppid start executable command_sha; do
    [[ -n "$pid" ]] || continue
    current_ppid=$(ps -p "$pid" -o ppid= 2>/dev/null | awk '{$1=$1;print}')
    current_start=$(ps -p "$pid" -o lstart= 2>/dev/null | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
    current_executable=$(ps -p "$pid" -o comm= 2>/dev/null | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
    current_sha=$(ps -ww -p "$pid" -o command= 2>/dev/null | shasum -a 256 | awk '{print $1}')
    [[ "$current_ppid" == "$ppid" && "$current_start" == "$start" && "$current_executable" == "$executable" && "$current_sha" == "$command_sha" ]] || return 1
  done < "$baseline"
}

bounded_exec() {
  local seconds=$1
  shift
  /usr/bin/perl -e 'alarm shift; exec @ARGV' "$seconds" "$@"
}

canonical_existing_dir() {
  [[ -d "$1" && ! -L "$1" ]] || return 1
  (cd "$1" && pwd -P)
}

canonical_existing_file() {
  local requested=$1 parent base
  [[ -f "$requested" && ! -L "$requested" ]] || return 1
  parent=$(cd "$(dirname "$requested")" 2>/dev/null && pwd -P) || return 1
  base=$(basename "$requested")
  [[ -f "$parent/$base" && ! -L "$parent/$base" ]] || return 1
  printf '%s/%s\n' "$parent" "$base"
}

stage_prebuilt_candidate() {
  local requested=$1 target=$2 source source_sha staged_sha
  source=$(canonical_existing_file "$requested") || return 1
  [[ -x "$source" ]] || return 1
  mkdir -p "$(dirname "$target")" || return 1
  cp -p "$source" "$target" || return 1
  [[ -f "$target" && ! -L "$target" && -x "$target" ]] || return 1
  source_sha=$(file_sha "$source") || return 1
  staged_sha=$(file_sha "$target") || return 1
  [[ "$staged_sha" == "$source_sha" ]] || return 1
  printf '%s\n' "$staged_sha"
}

canonical_new_path() {
  local requested=$1 parent base
  parent=$(dirname "$requested")
  base=$(basename "$requested")
  mkdir -p "$parent"
  parent=$(canonical_existing_dir "$parent") || return 1
  printf '%s/%s\n' "$parent" "$base"
}

path_is_within() {
  local path=$1 root=$2
  [[ "$path" == "$root" || "$path" == "$root/"* ]]
}

require_isolated_layout() {
  local run_root=$1 source_home=$2
  [[ -n "$run_root" && -n "$source_home" ]] || return 1
  [[ ! -e "$run_root" ]] || return 1
  path_is_within "$run_root" "$source_home" && return 1
  path_is_within "$source_home" "$run_root" && return 1
  return 0
}

source_file_identity() {
  local path=$1
  [[ -f "$path" && ! -L "$path" ]] || return 1
  stat -f '%d:%i:%z:%m:%c:%p:%HT' "$path"
}

source_session_row() {
  local source_home=$1 requested_id=$2 query row id archived path
  if [[ -n "$requested_id" ]]; then
    [[ "$requested_id" =~ ^[0-9A-Za-z_-]+$ ]] || return 1
    query="SELECT id,archived,rollout_path FROM threads WHERE id='$requested_id' AND archived=1 AND rollout_path IS NOT NULL LIMIT 1;"
    row=$(sqlite3 -batch -noheader -separator $'\t' "$source_home/state_5.sqlite" "$query") || return 1
    [[ -n "$row" ]] || return 1
    printf '%s\n' "$row"
    return 0
  fi

  sqlite3 -batch -noheader -separator $'\t' "$source_home/state_5.sqlite" \
    'SELECT id,archived,rollout_path FROM threads WHERE archived=1 AND rollout_path IS NOT NULL ORDER BY updated_at DESC LIMIT 100;' |
  while IFS=$'\t' read -r id archived path; do
    [[ "$id" =~ ^[0-9A-Za-z_-]+$ && "$archived" == 1 ]] || continue
    [[ -f "$path" && ! -L "$path" ]] || continue
    # Exercise meaningful reconstruction instead of accepting a tiny fixture-like rollout.
    [[ "$(stat -f '%z' "$path" 2>/dev/null || printf 0)" -ge 65536 ]] || continue
    printf '%s\t%s\t%s\n' "$id" "$archived" "$path"
    break
  done
}

write_policy() {
  local path=$1 enabled=$2 temporary
  mkdir -p "$(dirname "$path")"
  temporary=$(mktemp "$(dirname "$path")/.policy.XXXXXX")
  jq -n \
    --argjson enabled "$enabled" \
    '{version:1,enabled:$enabled,interval:"30s",stable_for:"1s",archived_only:true,batch_size:1}' \
    > "$temporary"
  chmod 600 "$temporary"
  mv "$temporary" "$path"
}

normalize_isolated_api_key_config() {
  local config=$1 selected_model=${2:-gpt-5.6-terra} temporary
  [[ -f "$config" && ! -L "$config" ]] || return 1
  temporary=$(mktemp "$(dirname "$config")/.config.api-key.XXXXXX") || return 1
  awk -v selected_model="$selected_model" '
    function flush_main() {
      if (!in_main) return
      if (!seen_auth) print "requires_openai_auth = true"
    }
    BEGIN {
      in_main=0; seen_main=0; seen_env=0; seen_auth=0
      print "forced_login_method = \"api\""
      print "cli_auth_credentials_store = \"file\""
    }
    /^[[:space:]]*(forced_login_method|cli_auth_credentials_store)[[:space:]]*=/ { next }
    /^[[:space:]]*model[[:space:]]*=/ { print "model = \"" selected_model "\""; next }
    /^[[:space:]]*chronicle[[:space:]]*=/ { print "chronicle = false"; next }
    /^\[model_providers\.main\][[:space:]]*$/ {
      flush_main()
      in_main=1
      seen_main=1
      seen_env=0
      seen_auth=0
      print
      next
    }
    /^\[/ {
      flush_main()
      in_main=0
      seen_env=0
      seen_auth=0
      print
      next
    }
    /^[[:space:]]*(experimental_bearer_token|api_key|access_token|refresh_token|client_secret)[[:space:]]*=/ { next }
    in_main && /^[[:space:]]*env_key[[:space:]]*=/ {
      seen_env=1
      next
    }
    in_main && /^[[:space:]]*requires_openai_auth[[:space:]]*=/ {
      print "requires_openai_auth = true"
      seen_auth=1
      next
    }
    { print }
    END {
      flush_main()
      if (!seen_main) {
        print ""
        print "[model_providers.main]"
        print "requires_openai_auth = true"
      }
    }
  ' "$config" > "$temporary" || {
    rm -f "$temporary"
    return 1
  }
  chmod 600 "$temporary"
  mv "$temporary" "$config"
}

source_api_key_for_isolated_run() {
  local source_home=$1 config=$1/auth.json provider_config=$1/config.toml api_key
  api_key=$(jq -r '.OPENAI_API_KEY // empty' "$config" 2>/dev/null || true)
  if [[ -n "$api_key" && "$api_key" != "null" ]]; then
    printf 'auth.json\t%s\n' "$api_key"
    return 0
  fi
  api_key=$(awk '
    BEGIN { in_main=0 }
    /^\[model_providers\.main\][[:space:]]*$/ { in_main=1; next }
    /^\[/ { in_main=0 }
    in_main && /^[[:space:]]*experimental_bearer_token[[:space:]]*=/ {
      line=$0
      sub(/^[^=]*=[[:space:]]*/, "", line)
      gsub(/^"|"$/, "", line)
      print line
      exit
    }
  ' "$provider_config")
  [[ -n "$api_key" ]] || return 1
  printf 'model_providers.main.experimental_bearer_token\t%s\n' "$api_key"
}

write_isolated_api_key_auth() {
  local auth_path=$1 api_key=$2 temporary
  [[ -n "$auth_path" && -n "$api_key" ]] || return 1
  temporary=$(mktemp "$(dirname "$auth_path")/.auth.api-key.XXXXXX") || return 1
  jq -n --arg key "$api_key" '{auth_mode:"apikey",OPENAI_API_KEY:$key}' > "$temporary" || {
    rm -f "$temporary"
    return 1
  }
  chmod 600 "$temporary"
  mv "$temporary" "$auth_path"
}

mount_present() {
  mount | awk -v target="$1" '$2 == "on" && $3 == target { found=1 } END { exit found ? 0 : 1 }'
}

wait_for_mount() {
  local target=$1 backend_pid=$2 supervisor_pid=${3:-} deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    kill -0 "$backend_pid" 2>/dev/null || return 1
    if [[ -n "$supervisor_pid" ]]; then
      kill -0 "$supervisor_pid" 2>/dev/null || return 1
    fi
    if mount_present "$target" && bounded_exec 3 stat "$target" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.25
  done
  return 1
}

wait_for_status() {
  local status_path=$1 filter=$2 timeout=$3
  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    if [[ -f "$status_path" ]] && jq -e "$filter" "$status_path" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

append_progress() {
  local status_path=$1 output=$2 compact previous_file previous current
  PROGRESS_SNAPSHOT=''
  [[ -f "$status_path" ]] || return 0
  current=$(jq -c '[.enabled,.phase,.cycle_done,.cycle_total,.managed_count,.waiting_count,.waiting_known,.next_check_at,.updated_at]' "$status_path" 2>/dev/null) || return 0
  PROGRESS_SNAPSHOT=$current
  previous_file="$output.last"
  previous=$(cat "$previous_file" 2>/dev/null || true)
  [[ "$current" == "$previous" ]] && return 0
  compact=$(timestamp)
  printf '%s\t%s\n' "$compact" "$current" >> "$output"
  printf '%s' "$current" > "$previous_file"
}

wait_for_real_fold() {
  local status_path=$1 output=$2 timeout=$3
  local deadline=$((SECONDS + timeout))
  local saw_waiting=false saw_checking=false saw_folding=false saw_packing=false saw_migrating=false
  local enabled phase completed total managed waiting known rest
  while (( SECONDS < deadline )); do
    append_progress "$status_path" "$output"
    if [[ -n "$PROGRESS_SNAPSHOT" ]]; then
      IFS=$'\t' read -r enabled phase completed total managed waiting known rest <<< "$(jq -r '@tsv' <<< "$PROGRESS_SNAPSHOT")"
      [[ "$known" == true && "$waiting" -ge 1 ]] && saw_waiting=true
      case "$phase" in
        checking) saw_checking=true ;;
        folding) saw_folding=true ;;
        packing) saw_packing=true ;;
        migrating) saw_migrating=true ;;
      esac
      if [[ "$enabled" == true && "$phase" == idle && "$managed" == 1 && "$total" == 6 && "$completed" == 6 ]]; then
        # Fast pack/migrate phases can finish between samples. Record exactly
        # what was observed; the checks below verify their persisted outputs.
        jq -n --argjson checking "$saw_checking" --argjson folding "$saw_folding" \
          --argjson packing "$saw_packing" --argjson migrating "$saw_migrating" \
          '{checking:$checking,folding:$folding,packing:$packing,migrating:$migrating}' > "$output.phases.json"
        [[ "$saw_waiting" == true && "$saw_checking" == true && "$saw_folding" == true ]] || return 1
        return 0
      fi
    fi
    sleep 0.1
  done
  return 1
}

critical_inventory() {
  local store=$1 temporary
  temporary=$(mktemp "${TMPDIR:-/tmp}/codexfold-real-fold-inventory.XXXXXX")
  for subtree in manifests objects packs fs; do
    [[ -d "$store/$subtree" ]] || continue
    find "$store/$subtree" -type f ! -name writer.lease -print
  done | LC_ALL=C sort | while IFS= read -r path; do
    printf '%s\t%s\n' "$(file_sha "$path")" "${path#"$store/"}"
  done > "$temporary"
  file_sha "$temporary"
  rm -f "$temporary"
}

wait_for_idempotent_cycle() {
  local status_path=$1 output=$2 previous_next_check=$3 timeout=$4 next_check
  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    append_progress "$status_path" "$output"
    if [[ -f "$status_path" ]]; then
      next_check=$(jq -r '.next_check_at // empty' "$status_path" 2>/dev/null)
      if [[ -n "$next_check" && "$next_check" != "$previous_next_check" ]] &&
        jq -e '.enabled == true and .phase == "idle" and .managed_count == 1 and .cycle_total == 0 and .cycle_done == 0' "$status_path" >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep 0.2
  done
  return 1
}

prefix_sha() {
  local path=$1 bytes=$2
  /usr/bin/perl -e '
    my $remaining = shift;
    while ($remaining > 0) {
      my $want = $remaining > 1048576 ? 1048576 : $remaining;
      my $read = read(STDIN, my $buffer, $want);
      die "short input" unless defined($read) && $read > 0;
      print $buffer;
      $remaining -= $read;
    }
  ' "$bytes" < "$path" | shasum -a 256 | awk '{print $1}'
}

process_belongs_to_run() {
  local pid=$1 run_root=$2 command
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  command=$(ps -p "$pid" -o command= 2>/dev/null || true)
  [[ -n "$command" && "$command" == *"$run_root"* ]]
}

stop_owned_process() {
  local pid=$1
  [[ -n "$pid" ]] || return 0
  process_belongs_to_run "$pid" "$RUN_ROOT" || return 0
  kill -TERM "$pid" >/dev/null 2>&1 || true
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    kill -0 "$pid" >/dev/null 2>&1 || break
    sleep 0.25
  done
  if kill -0 "$pid" >/dev/null 2>&1 && process_belongs_to_run "$pid" "$RUN_ROOT"; then
    kill -KILL "$pid" >/dev/null 2>&1 || true
  fi
  wait "$pid" >/dev/null 2>&1 || true
}

cleanup_runtime() {
  local cleanup_bin=${CANDIDATE_BIN:-} cleanup_home=${CODEX_HOME_ISOLATED:-}
  local cleanup_store=${STORE_ROOT:-} cleanup_mount=${MOUNT_ROOT:-} cleanup_native=${NATIVE_ROOT:-}
  local cleanup_session state_file serve_pid=${SERVE_PID:-} supervisor_pid=${SUPERVISOR_PID:-}
  local cleanup_candidate_app=${CANDIDATE_APP:-}
  set +e
  stop_owned_process "${DESKTOP_PID:-}"
  if [[ -n "${POLICY_PATH:-}" && -d "$(dirname "$POLICY_PATH")" ]]; then
    write_policy "$POLICY_PATH" false >/dev/null 2>&1
  fi
  if [[ -x "$cleanup_bin" ]] && mount_present "$cleanup_mount"; then
    # Desktop acceptance may have folded children as well as the source.
    for state_file in "$cleanup_store"/fs/sessions/*/state.json; do
      [[ -f "$state_file" && ! -L "$state_file" ]] || continue
      cleanup_session=$(jq -r '.session_id // empty' "$state_file")
      [[ "$cleanup_session" =~ ^[A-Za-z0-9_-]+$ && "$state_file" == "$cleanup_store/fs/sessions/$cleanup_session/state.json" ]] || continue
      bounded_exec 30 "$cleanup_bin" fs rollback "$cleanup_session" \
        --codex-home "$cleanup_home" --store "$cleanup_store" --mount "$cleanup_mount" \
        --native-root "$cleanup_native" --canonical-namespace --apply >/dev/null 2>&1
    done
  fi
  stop_owned_process "$supervisor_pid"
  if mount_present "$cleanup_mount"; then
    /sbin/umount "$cleanup_mount" >/dev/null 2>&1
  fi
  stop_owned_process "$serve_pid"
  if [[ -x "$cleanup_bin" && -L "$cleanup_home/sessions" && -L "$cleanup_home/archived_sessions" ]] && ! mount_present "$cleanup_mount"; then
    bounded_exec 30 "$cleanup_bin" fs namespace deactivate \
      --codex-home "$cleanup_home" --store "$cleanup_store" --mount "$cleanup_mount" \
      --native-root "$cleanup_native" --apply >/dev/null 2>&1
  fi
  if [[ -n "${RUNTIME_ROOT:-}" && -d "$RUNTIME_ROOT" && -d "${EVIDENCE_ROOT:-}" ]]; then
    cp "$RUNTIME_ROOT/serve.log" "$EVIDENCE_ROOT/serve.log" 2>/dev/null || true
    cp "$RUNTIME_ROOT/supervisor.log" "$EVIDENCE_ROOT/supervisor.log" 2>/dev/null || true
  fi
  if mount_present "$cleanup_mount"; then
    /sbin/umount "$cleanup_mount" >/dev/null 2>&1
  fi
  # Only remove a disposable candidate registration created under this
  # repository's .tmp tree. Never unregister the installed production App.
  if [[ -n "$cleanup_candidate_app" && "$cleanup_candidate_app" == "$REPO_ROOT/.tmp/"* && -d "$cleanup_candidate_app" ]]; then
    /System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister \
      -u "$cleanup_candidate_app" >/dev/null 2>&1 || true
  fi
  if [[ "$cleanup_mount" == /private/tmp/codexfold-real-fold-mount-* ]] && ! mount_present "$cleanup_mount" &&
    [[ -f "$cleanup_mount/.acceptance-marker" ]] &&
    [[ "$(cat "$cleanup_mount/.acceptance-marker" 2>/dev/null)" == "$SCHEMA" ]]; then
    find "$cleanup_mount" -depth -delete >/dev/null 2>&1
  fi
  if [[ -n "${FSKIT_RESOURCE:-}" && -n "${FSKIT_ACCEPTANCE_PARENT:-}" ]] &&
    path_is_within "$FSKIT_RESOURCE" "$FSKIT_ACCEPTANCE_PARENT" &&
    [[ "$(basename "$FSKIT_RESOURCE")" == rf-* && -f "$FSKIT_RESOURCE/.acceptance-marker" ]] &&
    [[ "$(cat "$FSKIT_RESOURCE/.acceptance-marker" 2>/dev/null)" == "$SCHEMA" ]]; then
    find "$FSKIT_RESOURCE" -depth -delete >/dev/null 2>&1
  fi
  SERVE_PID=''
  SUPERVISOR_PID=''
  set -e
}

write_failure_evidence() {
  local exit_code=$1
  [[ -n "${EVIDENCE_ROOT:-}" && -d "$EVIDENCE_ROOT" ]] || return 0
  jq -n --arg schema "$SCHEMA" --arg result failure --arg step "${CURRENT_STEP:-unknown}" \
    --arg recorded_at "$(timestamp)" --argjson exit_code "$exit_code" \
    '{schema:$schema,result:$result,failed_step:$step,exit_code:$exit_code,recorded_at:$recorded_at}' \
    > "$EVIDENCE_ROOT/failure.json"
}

on_exit() {
  local exit_code=$?
  trap - EXIT HUP INT TERM
  if (( exit_code != 0 )); then
    write_failure_evidence "$exit_code"
  fi
  cleanup_runtime
  if [[ "${RUNTIME_ROOT:-}" == /private/tmp/codexfold-acceptance-runtime.* && -f "$RUNTIME_ROOT/.acceptance-marker" ]] &&
    [[ "$(cat "$RUNTIME_ROOT/.acceptance-marker")" == "$SCHEMA" ]]; then
    rm -rf "$RUNTIME_ROOT"
  fi
  if [[ -n "${WORK_ROOT:-}" ]] && path_is_within "$WORK_ROOT" "$RUN_ROOT"; then
    rm -rf "$WORK_ROOT"
  fi
  exit "$exit_code"
}

main() {
  local source_input="$HOME/.codex" run_input='' requested_session=''
  local codex_cli='/Applications/ChatGPT.app/Contents/Resources/codex'
  local candidate_input='' candidate_app_input='' fskit_type_input=''
  local candidate_artifact_mode=built-current-worktree build_tags_json='["fuse"]'
  local model='gpt-5.6-terra' frontend=fuse codex_timeout=240
  local performance=false
  local skip_resume=false
  local desktop_app='' desktop_executable='' desktop_deadline
  local source_row source_archived source_rollout source_identity_before source_sha_before source_bytes
  local source_identity_after source_sha_after candidate_sha status_before_next inventory_before inventory_after
  local visible_path full_bytes delta_path delta_bytes full_sha manifest_sha generation
  local physical_before physical_after retirement_proof
  local object_count loose_count pack_generation base_prefix_sha last_message marker disabled_inventory_before disabled_inventory_after
  local isolated_api_key api_key_source api_key_record isolated_base_url socket_suffix production_module_path
  local production_module_baseline candidate_module_process
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --source-home) source_input=${2:?}; shift 2 ;;
      --run-root) run_input=${2:?}; shift 2 ;;
      --session) requested_session=${2:?}; shift 2 ;;
      --codex-cli) codex_cli=${2:?}; shift 2 ;;
      --candidate-bin) candidate_input=${2:?}; shift 2 ;;
      --desktop-app) desktop_app=${2:?}; shift 2 ;;
      --skip-resume) skip_resume=true; shift ;;
      --candidate-app) candidate_app_input=${2:?}; shift 2 ;;
      --fskit-type) fskit_type_input=${2:?}; shift 2 ;;
      --model) model=${2:?}; shift 2 ;;
      --frontend) frontend=${2:?}; shift 2 ;;
      --timeout) codex_timeout=${2:?}; shift 2 ;;
      --performance) performance=true; shift ;;
      -h|--help) usage; return 0 ;;
      *) die "unknown option: $1" ;;
    esac
  done

  [[ "$frontend" == fuse || "$frontend" == native-fskit ]] || die "--frontend must be fuse or native-fskit"
  [[ "$performance" == false || "$frontend" == native-fskit ]] || die "--performance requires native-fskit"
  if [[ "$frontend" == native-fskit && -z "$candidate_app_input" ]]; then
    die "native-fskit real-fold requires --candidate-app so the loaded Swift module can be proven"
  fi
  [[ "$codex_timeout" =~ ^[0-9]+$ && "$codex_timeout" -ge 30 && "$codex_timeout" -le 900 ]] || die "--timeout must be 30..900 seconds"
  [[ -x "$codex_cli" ]] || die "Codex CLI is not executable: $codex_cli"
  [[ -x "$PREPARE_HOME" ]] || die "isolated CODEX_HOME preparer is unavailable"
  need_command go
  need_command git
  need_command jq
  need_command sqlite3
  need_command shasum
  need_command stat
  need_command mount
  need_command ps

  SOURCE_HOME=$(canonical_existing_dir "$source_input") || die "source CODEX_HOME must be a regular directory"
  [[ -f "$SOURCE_HOME/state_5.sqlite" && ! -L "$SOURCE_HOME/state_5.sqlite" ]] || die "source state_5.sqlite is unavailable"
  [[ -f "$SOURCE_HOME/config.toml" && ! -L "$SOURCE_HOME/config.toml" ]] || die "source config.toml is unavailable"
  [[ -f "$SOURCE_HOME/auth.json" && ! -L "$SOURCE_HOME/auth.json" ]] || die "source auth.json is unavailable"

  if [[ -z "$run_input" ]]; then
    run_input="$REPO_ROOT/.tmp/rf-$(date -u '+%H%M%S')-$$"
  fi
  RUN_ROOT=$(canonical_new_path "$run_input") || die "could not resolve run root"
  require_isolated_layout "$RUN_ROOT" "$SOURCE_HOME" || die "run root must be new and outside the source CODEX_HOME"
  if [[ -n "$desktop_app" ]]; then
    local desktop_socket="$RUN_ROOT/work/codex-home/ipc/ipc.sock"
    [[ ${#desktop_socket} -lt 104 ]] || die "Desktop IPC socket path exceeds macOS limit; choose a shorter --run-root"
  fi
  mkdir -p "$RUN_ROOT/evidence" "$RUN_ROOT/work"
  chmod 700 "$RUN_ROOT" "$RUN_ROOT/evidence" "$RUN_ROOT/work"
  printf '%s\n' "$SCHEMA" > "$RUN_ROOT/.codexfold-real-fold-acceptance"
  chmod 600 "$RUN_ROOT/.codexfold-real-fold-acceptance"
  EVIDENCE_ROOT="$RUN_ROOT/evidence"
  WORK_ROOT="$RUN_ROOT/work"
  CODEX_HOME_ISOLATED="$WORK_ROOT/codex-home"
  STORE_ROOT="$CODEX_HOME_ISOLATED/fold-store"
  MOUNT_ROOT="$WORK_ROOT/mount"
  NATIVE_ROOT="$WORK_ROOT/native"
  # Keep executable mappings, cwd and control logs off an ejectable repository
  # volume. The dataset remains in the selected run root on purpose.
  RUNTIME_ROOT=$(mktemp -d /private/tmp/codexfold-acceptance-runtime.XXXXXX)
  printf '%s\n' "$SCHEMA" > "$RUNTIME_ROOT/.acceptance-marker"
  CANDIDATE_BIN="$RUNTIME_ROOT/codexfold"
  POLICY_PATH="$STORE_ROOT/enrollment/policy.json"
  STATUS_PATH="$STORE_ROOT/enrollment/status.json"
  FSKIT_SOCKET=''
  FSKIT_RESOURCE=''
  FSKIT_ACCEPTANCE_PARENT=''
  CANDIDATE_APP=''
  CANDIDATE_MODULE=''
  CANDIDATE_MODULE_EXECUTABLE=''
  CANDIDATE_MODULE_SHA=''
  CANDIDATE_FSKIT_TYPE=''
  CANDIDATE_APP_BUNDLE_ID=''
  CANDIDATE_MODULE_BUNDLE_ID=''
  SERVE_PID=''
  SUPERVISOR_PID=''
  CURRENT_STEP=select-source-session
  trap on_exit EXIT HUP INT TERM

  source_row=$(source_session_row "$SOURCE_HOME" "$requested_session") || die "could not select a stable archived source session"
  IFS=$'\t' read -r SESSION_ID source_archived source_rollout <<EOF
$source_row
EOF
  [[ "$source_archived" == 1 && -f "$source_rollout" && ! -L "$source_rollout" ]] || die "selected source rollout is not a regular archived session"
  source_identity_before=$(source_file_identity "$source_rollout") || die "could not capture source rollout identity"
  source_sha_before=$(file_sha "$source_rollout")
  source_bytes=$(stat -f '%z' "$source_rollout")
  jq -n --arg schema "$SCHEMA" --arg session_id "$SESSION_ID" --argjson archived true \
    --argjson bytes "$source_bytes" --arg sha256 "$source_sha_before" --arg identity "$source_identity_before" \
    '{schema:$schema,session_id:$session_id,archived:$archived,bytes:$bytes,sha256:$sha256,source_identity:$identity}' \
    > "$EVIDENCE_ROOT/source.json"

  CURRENT_STEP=prepare-isolated-home
  "$PREPARE_HOME" "$SOURCE_HOME" "$CODEX_HOME_ISOLATED" --session "$SESSION_ID" \
    > "$EVIDENCE_ROOT/prepare.log" 2>&1
  api_key_record=$(source_api_key_for_isolated_run "$SOURCE_HOME") || die "production CODEX_HOME has no usable third-party API key"
  api_key_source=${api_key_record%%$'\t'*}
  isolated_api_key=${api_key_record#*$'\t'}
  write_isolated_api_key_auth "$CODEX_HOME_ISOLATED/auth.json" "$isolated_api_key" || \
    die "could not bind the isolated CODEX_HOME to the production API-key route"
  jq -e '.auth_mode == "apikey" and (.OPENAI_API_KEY | type == "string" and length > 0)' \
    "$CODEX_HOME_ISOLATED/auth.json" >/dev/null || die "isolated home is not bound to the production API-key route"
  normalize_isolated_api_key_config "$CODEX_HOME_ISOLATED/config.toml" "$model" || die "could not normalize the isolated API-key provider"
  grep -Eq '^[[:space:]]*forced_login_method[[:space:]]*=[[:space:]]*"api"[[:space:]]*$' \
    "$CODEX_HOME_ISOLATED/config.toml" || die "isolated login is not restricted to API keys"
  grep -Eq '^[[:space:]]*cli_auth_credentials_store[[:space:]]*=[[:space:]]*"file"[[:space:]]*$' \
    "$CODEX_HOME_ISOLATED/config.toml" || die "isolated credentials are not file-backed"
  grep -Eq '^[[:space:]]*base_url[[:space:]]*=' "$CODEX_HOME_ISOLATED/config.toml" || die "isolated provider has no third-party API endpoint"
  [[ "$(awk -F '\t' 'NR==1 {print $1}' "$CODEX_HOME_ISOLATED/selected-sessions.tsv")" == "$SESSION_ID" ]] || die "isolated session selection changed"
  isolated_base_url=$(awk '
    BEGIN { in_main=0 }
    /^\[model_providers\.main\][[:space:]]*$/ { in_main=1; next }
    /^\[/ { in_main=0 }
    in_main && /^[[:space:]]*base_url[[:space:]]*=/ {
      line=$0
      sub(/^[^=]*=[[:space:]]*/, "", line)
      gsub(/^"|"$/, "", line)
      print line
      exit
    }
  ' "$CODEX_HOME_ISOLATED/config.toml")
  jq -n --arg schema "$SCHEMA" --arg source "$api_key_source" --arg base_url "$isolated_base_url" \
    '{schema:$schema,auth_mode:"isolated-file-api-key",oauth_used:false,key_source:$source,base_url:$base_url}' \
    > "$EVIDENCE_ROOT/auth-route.json"

  CURRENT_STEP=build-current-worktree
  mkdir -p "$(dirname "$CANDIDATE_BIN")"
  if [[ -n "$candidate_input" ]]; then
    candidate_artifact_mode=prebuilt-byte-exact-copy
    build_tags_json=null
    candidate_sha=$(stage_prebuilt_candidate "$candidate_input" "$CANDIDATE_BIN") || \
      die "could not stage the exact prebuilt candidate helper"
    printf 'staged prebuilt candidate with SHA-256 %s\n' "$candidate_sha" > "$EVIDENCE_ROOT/build.log"
  elif [[ "$frontend" == fuse ]]; then
    (cd "$REPO_ROOT" && go build -trimpath -tags fuse -o "$CANDIDATE_BIN" ./cmd/codexfold) \
      > "$EVIDENCE_ROOT/build.log" 2>&1
  else
    build_tags_json='[]'
    (cd "$REPO_ROOT" && go build -trimpath -o "$CANDIDATE_BIN" ./cmd/codexfold) \
      > "$EVIDENCE_ROOT/build.log" 2>&1
  fi
  candidate_sha=$(file_sha "$CANDIDATE_BIN")
  jq -n --arg schema "$SCHEMA" --arg sha256 "$candidate_sha" \
    --arg git_head "$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || true)" \
    --arg frontend "$frontend" --arg model "$model" --arg artifact_mode "$candidate_artifact_mode" \
    --argjson build_tags "$build_tags_json" \
    '{schema:$schema,candidate_sha256:$sha256,git_head:$git_head,frontend:$frontend,model:$model,artifact_mode:$artifact_mode,build_tags:$build_tags,auth_mode:"isolated-file-api-key",oauth_used:false}' \
    > "$EVIDENCE_ROOT/candidate.json"

  CURRENT_STEP='start-isolated-filesystem'
  production_module_path="$HOME/Applications/CodexFoldFSKit.app/Contents/Extensions/$FSKIT_MODULE_BUNDLE_NAME/Contents/MacOS/$FSKIT_MODULE_PROCESS_NAME"
  production_module_baseline="$EVIDENCE_ROOT/production-module-before.tsv"
  write_module_process_snapshot_for_path "$production_module_path" "$production_module_baseline" || die "could not capture production FSKit module baseline"
  if [[ "$frontend" == native-fskit ]]; then
    validate_candidate_native_app "$candidate_app_input" || die "candidate App is not a registered signed FSKit module"
    if [[ -n "$fskit_type_input" && "$fskit_type_input" != "$CANDIDATE_FSKIT_TYPE" ]]; then
      die "--fskit-type does not match candidate module FSShortName ($CANDIDATE_FSKIT_TYPE)"
    fi
    fskit_type_input="$CANDIDATE_FSKIT_TYPE"
    jq -n --arg app "$CANDIDATE_APP" --arg module "$CANDIDATE_MODULE" \
      --arg app_id "$CANDIDATE_APP_BUNDLE_ID" --arg module_id "$CANDIDATE_MODULE_BUNDLE_ID" \
      --arg fs_type "$CANDIDATE_FSKIT_TYPE" --arg module_exec "$CANDIDATE_MODULE_EXECUTABLE" \
      --arg module_sha "$CANDIDATE_MODULE_SHA" --arg app_sha "$CANDIDATE_APP_SHA" \
      --arg production_path "$production_module_path" \
      --arg production_snapshot "$production_module_baseline" \
      '{schema:"codexfold.real-fold-native-binding.v1",candidate_app:$app,candidate_module:$module,candidate_app_bundle_id:$app_id,candidate_module_bundle_id:$module_id,fskit_type:$fs_type,candidate_app_executable_sha256:$app_sha,candidate_module_executable:$module_exec,candidate_module_sha256:$module_sha,production_module_executable:$production_path,production_module_snapshot:$production_snapshot}' \
      > "$EVIDENCE_ROOT/native-binding.json"
  fi
  mkdir -p "$STORE_ROOT" "$NATIVE_ROOT"
  if [[ "$frontend" == native-fskit ]]; then
    socket_suffix=$(printf '%s' "$RUN_ROOT" | cksum | awk '{print $1}')
    MOUNT_ROOT="/private/tmp/codexfold-real-fold-mount-$socket_suffix"
    [[ ! -e "$MOUNT_ROOT" ]] || die "isolated native FSKit mount root already exists"
    mkdir -p "$MOUNT_ROOT"
    chmod 700 "$MOUNT_ROOT"
    printf '%s\n' "$SCHEMA" > "$MOUNT_ROOT/.acceptance-marker"
    chmod 600 "$MOUNT_ROOT/.acceptance-marker"
    FSKIT_ACCEPTANCE_PARENT=$(canonical_existing_dir "$HOME/Library/Group Containers/group.vip.jstar.codexfold") || \
      die "CodexFold App Group container is unavailable for isolated native FSKit acceptance"
    FSKIT_RESOURCE="$FSKIT_ACCEPTANCE_PARENT/rf-$socket_suffix"
    FSKIT_SOCKET="$FSKIT_RESOURCE/backend.sock"
    [[ ${#FSKIT_SOCKET} -lt 104 ]] || die "isolated native FSKit socket path exceeds the macOS limit"
    [[ ! -e "$FSKIT_RESOURCE" ]] || die "isolated native FSKit resource already exists"
    mkdir -p "$FSKIT_RESOURCE"
    chmod 700 "$FSKIT_RESOURCE"
    printf '%s\n' "$SCHEMA" > "$FSKIT_RESOURCE/.acceptance-marker"
    chmod 600 "$FSKIT_RESOURCE/.acceptance-marker"
  else
    mkdir -p "$MOUNT_ROOT"
  fi
  write_policy "$POLICY_PATH" false
  serve_args=(fs serve --codex-home "$CODEX_HOME_ISOLATED" --store "$STORE_ROOT" --mount "$MOUNT_ROOT" --native-root "$NATIVE_ROOT" --canonical-namespace --frontend "$frontend" --enrollment-interval 30s --enrollment-stable-for 1s --enrollment-batch-size 1 --enrollment-canary --apply)
  if [[ "$frontend" == fuse ]]; then
    serve_args+=(--operation-trace "$EVIDENCE_ROOT/operations.tsv")
  else
    serve_args+=(--fskit-resource "$FSKIT_RESOURCE" --fskit-socket "$FSKIT_SOCKET")
  fi
  (cd "$RUNTIME_ROOT" && exec "$CANDIDATE_BIN" "${serve_args[@]}") > "$RUNTIME_ROOT/serve.log" 2>&1 &
  SERVE_PID=$!
  if [[ "$frontend" == native-fskit ]]; then
    (cd "$RUNTIME_ROOT" && exec "$CANDIDATE_BIN" fs supervise --resource "$FSKIT_RESOURCE" --mount "$MOUNT_ROOT" \
      --fskit-type "$fskit_type_input" \
      --interval 250ms --probe-timeout 2s --recovery-timeout 15s --apply) \
      > "$RUNTIME_ROOT/supervisor.log" 2>&1 &
    SUPERVISOR_PID=$!
  fi
  wait_for_mount "$MOUNT_ROOT" "$SERVE_PID" "$SUPERVISOR_PID" || die "isolated $frontend mount did not become healthy"
  if [[ "$frontend" == native-fskit ]]; then
    candidate_module_process="$EVIDENCE_ROOT/candidate-module-process.tsv"
    wait_for_candidate_module_process "$CANDIDATE_MODULE_EXECUTABLE" "$candidate_module_process" || \
      die "native mount did not start the exact candidate FSKit module process"
    production_module_unchanged "$production_module_baseline" || die "production FSKit module process changed during isolated mount"
    printf '%s\n' "$CANDIDATE_MODULE_EXECUTABLE" > "$EVIDENCE_ROOT/live-module-path.txt"
    printf '%s\n' "$CANDIDATE_MODULE_SHA" > "$EVIDENCE_ROOT/live-module-sha256.txt"
  fi
  wait_for_status "$STATUS_PATH" '.enabled == false and .phase == "disabled"' 15 || die "automatic folding did not start disabled"

  CURRENT_STEP=activate-isolated-namespace
  "$CANDIDATE_BIN" fs namespace activate --codex-home "$CODEX_HOME_ISOLATED" \
    --mount "$MOUNT_ROOT" --native-root "$NATIVE_ROOT" --apply --json \
    > "$EVIDENCE_ROOT/namespace-activate.json"
  "$CANDIDATE_BIN" fs namespace status --codex-home "$CODEX_HOME_ISOLATED" \
    --mount "$MOUNT_ROOT" --native-root "$NATIVE_ROOT" --json \
    > "$EVIDENCE_ROOT/namespace-status.json"
  jq -e --arg home "$CODEX_HOME_ISOLATED" --arg mount "$MOUNT_ROOT" --arg native_root "$NATIVE_ROOT" \
    '.active == true and .home == $home and .mount == $mount and .native_root == $native_root' \
    "$EVIDENCE_ROOT/namespace-status.json" >/dev/null || die "isolated canonical namespace is not active"
  [[ -L "$CODEX_HOME_ISOLATED/sessions" && "$(readlink "$CODEX_HOME_ISOLATED/sessions")" == "$MOUNT_ROOT/sessions" ]] || \
    die "isolated sessions namespace does not target the candidate mount"
  [[ -L "$CODEX_HOME_ISOLATED/archived_sessions" && "$(readlink "$CODEX_HOME_ISOLATED/archived_sessions")" == "$MOUNT_ROOT/archived_sessions" ]] || \
    die "isolated archived namespace does not target the candidate mount"

  CURRENT_STEP=automatic-real-fold
  if [[ "$frontend" == native-fskit ]]; then
    CURRENT_STEP=native-file-replacement
    CODEXFOLD_NATIVE_FSKIT_MOUNT="$MOUNT_ROOT" \
      CODEXFOLD_NATIVE_FSKIT_NATIVE_ROOT="$NATIVE_ROOT" \
      CODEXFOLD_NATIVE_FSKIT_RESOURCE="$FSKIT_RESOURCE" \
      go test ./internal/mountfs -run '^TestNativeFSKitMountedSamePathReplacementWatch$' -count=1 -v -timeout 30s \
      > "$EVIDENCE_ROOT/native-file-replacement.log" 2>&1 || die "native file replacement tracking failed"
  fi

  if [[ "$performance" == true ]]; then
    # The source may itself route through production FSKit. Use a disposable
    # ordinary file as the native baseline, outside the measured store roots.
    cp "$source_rollout" "$WORK_ROOT/performance-native-reference.jsonl"
  fi
  physical_before=$(du -sk "$STORE_ROOT" "$NATIVE_ROOT" | awk '{s+=$1} END {printf "%.0f",s*1024}')
  : > "$EVIDENCE_ROOT/enrollment-progress.tsv"
  write_policy "$POLICY_PATH" true
  wait_for_real_fold "$STATUS_PATH" "$EVIDENCE_ROOT/enrollment-progress.tsv" 110 || \
    die "automatic folding did not prove waiting -> fold -> pack -> migrate"
  rm -f "$EVIDENCE_ROOT/enrollment-progress.tsv.last"

  CURRENT_STEP=verify-folded-state
  "$CANDIDATE_BIN" doctor --codex-home "$CODEX_HOME_ISOLATED" --store "$STORE_ROOT" --json \
    > "$EVIDENCE_ROOT/fold-doctor.json"
  "$CANDIDATE_BIN" pack doctor --codex-home "$CODEX_HOME_ISOLATED" --store "$STORE_ROOT" --json \
    > "$EVIDENCE_ROOT/pack-doctor.json"
  jq -e '.issue_count == 0 and .manifest_count == 1 and .verified_manifest_count == 1' \
    "$EVIDENCE_ROOT/fold-doctor.json" >/dev/null || die "fold doctor rejected the automatic fold"
  jq -e '.issue_count == 0 and .manifest_count == 1 and .verified_manifest_count == 1 and .object_count > 0' \
    "$EVIDENCE_ROOT/pack-doctor.json" >/dev/null || die "pack doctor rejected the automatic pack"
  STATE_PATH="$STORE_ROOT/fs/sessions/$SESSION_ID/state.json"
  [[ -f "$STATE_PATH" && ! -L "$STATE_PATH" ]] || die "automatic migration did not publish managed state"
  base_prefix_sha=$(jq -r '.base_sha256' "$STATE_PATH")
  [[ "$(jq -r '.base_bytes' "$STATE_PATH")" == "$source_bytes" && "$base_prefix_sha" == "$source_sha_before" ]] || die "managed base differs from the source rollout"
  retirement_proof="$STORE_ROOT/fs/sessions/$SESSION_ID/native-retirement.json"
  [[ "$(jq -r '.native_snapshot.path // empty' "$STATE_PATH")" == '' ]] || die "automatic folding retained the native snapshot"
  [[ ! -e "$STORE_ROOT/fs/snapshots/$SESSION_ID/native.jsonl" ]] || die "retired native snapshot still occupies space"
  cp "$retirement_proof" "$EVIDENCE_ROOT/native-retirement.json"
  manifest_sha=$(jq -r '.manifest_sha256' "$STATE_PATH")
  generation=$(jq -r '.generation' "$STATE_PATH")
  object_count=$(jq -r '.object_count' "$EVIDENCE_ROOT/pack-doctor.json")
  pack_generation=$(jq -r '.generation' "$EVIDENCE_ROOT/pack-doctor.json")
  loose_count=$(find "$STORE_ROOT/objects" -type f -name '*.zst' | wc -l | tr -d ' ')
  [[ "$loose_count" == 0 ]] || die "automatic folding retained duplicate loose objects"
  physical_after=$(du -sk "$STORE_ROOT" "$NATIVE_ROOT" | awk '{s+=$1} END {printf "%.0f",s*1024}')
  jq -n --argjson before "$physical_before" --argjson after "$physical_after" \
    '{before_allocated_bytes:$before,after_allocated_bytes:$after,reclaimed_bytes:($before-$after)}' > "$EVIDENCE_ROOT/physical-space.json"
  [[ "$physical_after" -lt "$physical_before" ]] || die "automatic folding did not reduce allocated disk space"

  CURRENT_STEP=idempotent-automatic-cycle
  inventory_before=$(critical_inventory "$STORE_ROOT")
  status_before_next=$(jq -r '.next_check_at // empty' "$STATUS_PATH")
  : > "$EVIDENCE_ROOT/idempotent-cycle.tsv"
  wait_for_idempotent_cycle "$STATUS_PATH" "$EVIDENCE_ROOT/idempotent-cycle.tsv" "$status_before_next" 50 || \
    die "automatic folding did not complete an already-managed no-op cycle"
  rm -f "$EVIDENCE_ROOT/idempotent-cycle.tsv.last"
  inventory_after=$(critical_inventory "$STORE_ROOT")
  [[ "$inventory_after" == "$inventory_before" ]] || die "already-managed automatic cycle changed fold data"

  CURRENT_STEP=hot-disable
  write_policy "$POLICY_PATH" false
  wait_for_status "$STATUS_PATH" '.enabled == false and .phase == "disabled" and .managed_count == 1' 10 || \
    die "automatic folding did not hot-disable"

  if [[ "$performance" == true ]]; then
    CURRENT_STEP=native-performance
    visible_path=$(sqlite3 -batch -noheader "$CODEX_HOME_ISOLATED/state_5.sqlite" "SELECT rollout_path FROM threads WHERE id='$SESSION_ID';")
    ps -p "$SERVE_PID" -o pid=,time=,pcpu=,rss= > "$EVIDENCE_ROOT/performance-process.before.txt"
    CODEXFOLD_NATIVE_FSKIT_MOUNT="$MOUNT_ROOT" \
      CODEXFOLD_NATIVE_FSKIT_NATIVE_ROOT="$NATIVE_ROOT" \
      CODEXFOLD_NATIVE_FSKIT_RESOURCE="$FSKIT_RESOURCE" \
      CODEXFOLD_NATIVE_FSKIT_VIRTUAL_FILE="$visible_path" \
      CODEXFOLD_NATIVE_FSKIT_VIRTUAL_REFERENCE_FILE="$WORK_ROOT/performance-native-reference.jsonl" \
      go test ./internal/mountfs -run '^TestNativeFSKitMounted(Performance|ManagedPerformance)$' -count=1 -v -timeout 3m \
      > "$EVIDENCE_ROOT/native-performance.log" 2>&1 || die "native filesystem performance failed; see native-performance.log"
    ps -p "$SERVE_PID" -o pid=,time=,pcpu=,rss= > "$EVIDENCE_ROOT/performance-process.after.txt"
  fi

  CURRENT_STEP=real-codex-resume
  CODEX_HOME="$CODEX_HOME_ISOLATED" bounded_exec 30 "$codex_cli" unarchive "$SESSION_ID" \
    > "$EVIDENCE_ROOT/codex-unarchive.log" 2>&1
  visible_path=$(sqlite3 -batch -noheader "$CODEX_HOME_ISOLATED/state_5.sqlite" \
    "SELECT rollout_path FROM threads WHERE id='$SESSION_ID' AND archived=0;")
  [[ -n "$visible_path" && -f "$visible_path" ]] || die "Codex unarchive did not expose the managed session"
  [[ "$(stat -f '%z' "$visible_path")" == "$source_bytes" ]] || die "managed view changed before Codex resume"
  [[ "$(file_sha "$visible_path")" == "$source_sha_before" ]] || die "managed view SHA changed before Codex resume"
  marker=CODEXFOLD_REAL_FOLD_CURRENT_WORKTREE_OK
  isolated_api_key=$(jq -r '.OPENAI_API_KEY' "$CODEX_HOME_ISOLATED/auth.json")
  [[ -n "$isolated_api_key" && "$isolated_api_key" != null ]] || die "isolated API key disappeared before Codex resume"
  if [[ "$skip_resume" == false ]]; then
  CODEX_HOME="$CODEX_HOME_ISOLATED" OPENAI_API_KEY="$isolated_api_key" CODEX_API_KEY="$isolated_api_key" \
    bounded_exec "$codex_timeout" "$codex_cli" exec \
    --sandbox read-only --skip-git-repo-check --json \
    --output-last-message "$EVIDENCE_ROOT/codex-last-message.txt" resume --model "$model" \
    "$SESSION_ID" "这是 CodexFold 当前工作树的隔离真实续写验收。请不要调用任何工具，不要修改任何文件，只回复：$marker" \
    > "$EVIDENCE_ROOT/codex-resume.jsonl" 2> "$EVIDENCE_ROOT/codex-resume.stderr"
  isolated_api_key=''
  last_message=$(tr -d '\r\n' < "$EVIDENCE_ROOT/codex-last-message.txt")
  [[ "$last_message" == "$marker" ]] || die "real Codex resume did not return the acceptance marker"
  fi
  isolated_api_key=''

  CURRENT_STEP=verify-real-codex-append
  visible_path=$(sqlite3 -batch -noheader "$CODEX_HOME_ISOLATED/state_5.sqlite" \
    "SELECT rollout_path FROM threads WHERE id='$SESSION_ID' AND archived=0;")
  full_bytes=$(stat -f '%z' "$visible_path")
  if [[ "$skip_resume" == false ]]; then
    [[ "$full_bytes" -gt "$source_bytes" ]] || die "real Codex resume did not append to the managed session"
  fi
  [[ "$(prefix_sha "$visible_path" "$source_bytes")" == "$source_sha_before" ]] || die "real Codex append changed the folded base prefix"
  jq -c . "$visible_path" >/dev/null || die "full managed rollout is not valid JSONL"
  delta_path=$(jq -r '.delta_path' "$STATE_PATH")
  [[ -f "$delta_path" && ! -L "$delta_path" ]] || die "managed append delta is unavailable"
  delta_bytes=$(stat -f '%z' "$delta_path")
  if [[ "$skip_resume" == false ]]; then
    [[ "$delta_bytes" -gt 0 ]] || die "real Codex append did not reach the managed delta"
  fi
  jq -c . "$delta_path" >/dev/null || die "managed append delta is not valid JSONL"
  jq -e '(.cow_path == null) and (.writable_backing == null)' "$STATE_PATH" >/dev/null || \
    die "real append unexpectedly created writable backing"
  full_sha=$(file_sha "$visible_path")
  "$CANDIDATE_BIN" doctor --codex-home "$CODEX_HOME_ISOLATED" --store "$STORE_ROOT" --json \
    > "$EVIDENCE_ROOT/fold-doctor-after-resume.json"
  "$CANDIDATE_BIN" pack doctor --codex-home "$CODEX_HOME_ISOLATED" --store "$STORE_ROOT" --json \
    > "$EVIDENCE_ROOT/pack-doctor-after-resume.json"
  jq -e '.issue_count == 0' "$EVIDENCE_ROOT/fold-doctor-after-resume.json" >/dev/null || die "fold doctor failed after real Codex append"
  jq -e '.issue_count == 0' "$EVIDENCE_ROOT/pack-doctor-after-resume.json" >/dev/null || die "pack doctor failed after real Codex append"

  CURRENT_STEP=verify-disabled-cycle
  disabled_inventory_before=$(critical_inventory "$STORE_ROOT")
  : > "$EVIDENCE_ROOT/disabled-cycle.tsv"
  disabled_wait_iteration=0
  while (( disabled_wait_iteration < 165 )); do
    append_progress "$STATUS_PATH" "$EVIDENCE_ROOT/disabled-cycle.tsv"
    sleep 0.2
    disabled_wait_iteration=$((disabled_wait_iteration + 1))
  done
  rm -f "$EVIDENCE_ROOT/disabled-cycle.tsv.last"
  jq -e '.enabled == false and .phase == "disabled" and .managed_count == 1' "$STATUS_PATH" >/dev/null || \
    die "automatic folding resumed after hot-disable"
  disabled_inventory_after=$(critical_inventory "$STORE_ROOT")
  [[ "$disabled_inventory_after" == "$disabled_inventory_before" ]] || die "disabled automatic folding changed managed data"

  CURRENT_STEP=verify-production-source-unchanged
  if [[ -n "$desktop_app" ]]; then
    CURRENT_STEP=desktop-interactive-acceptance
    desktop_app=$(cd "$desktop_app" && pwd -P)
    [[ "$(bundle_info_value "$desktop_app" CFBundleIdentifier)" == com.codexfold.acceptance.desktop ]] || die "Desktop must use the isolated acceptance identity"
    desktop_executable="$desktop_app/Contents/MacOS/$(bundle_info_value "$desktop_app" CFBundleExecutable)"
    [[ -x "$desktop_executable" ]] || die "isolated Desktop executable is missing"
    codesign --verify --deep --strict "$desktop_app" || die "isolated Desktop signature is invalid"
    isolated_api_key=$(jq -r '.OPENAI_API_KEY' "$CODEX_HOME_ISOLATED/auth.json")
    # Carry only the user's already-completed migration announcement, not
    # production thread lists, workspaces, cloud credentials or browser data.
    if [[ -f "$SOURCE_HOME/.codex-global-state.json" ]]; then
      jq '{"electron-persisted-atom-state": {"chatgpt-migration-announcement-completed-v1": (."electron-persisted-atom-state"."chatgpt-migration-announcement-completed-v1" // false)}}' \
        "$SOURCE_HOME/.codex-global-state.json" > "$CODEX_HOME_ISOLATED/.codex-global-state.json"
    fi
    CODEX_HOME="$CODEX_HOME_ISOLATED" CODEX_ELECTRON_USER_DATA_PATH="$WORK_ROOT/desktop-data" OPENAI_API_KEY="$isolated_api_key" CODEX_API_KEY="$isolated_api_key" \
      "$desktop_executable" --user-data-dir="$WORK_ROOT/desktop-data" > "$EVIDENCE_ROOT/desktop.log" 2>&1 &
    DESKTOP_PID=$!
    isolated_api_key=''
    jq -n --argjson pid "$DESKTOP_PID" --arg app "$desktop_app" --arg home "$CODEX_HOME_ISOLATED" \
      --arg data "$WORK_ROOT/desktop-data" --arg session "$SESSION_ID" --arg mount "$MOUNT_ROOT" \
      '{pid:$pid,app:$app,codex_home:$home,user_data:$data,session_id:$session,mount:$mount}' > "$EVIDENCE_ROOT/desktop-ready.json"
    desktop_deadline=$((SECONDS + 1200))
    while [[ ! -f "$EVIDENCE_ROOT/desktop.done" ]] && (( SECONDS < desktop_deadline )); do
      kill -0 "$DESKTOP_PID" 2>/dev/null || die "isolated Desktop exited before acceptance completed"
      sleep 1
    done
    [[ -f "$EVIDENCE_ROOT/desktop.done" ]] || die "isolated Desktop acceptance timed out"
    stop_owned_process "$DESKTOP_PID"
    DESKTOP_PID=''
  fi
  CURRENT_STEP=verify-production-source-unchanged
  source_identity_after=$(source_file_identity "$source_rollout") || die "source rollout identity disappeared"
  source_sha_after=$(file_sha "$source_rollout")
  [[ "$source_identity_after" == "$source_identity_before" && "$source_sha_after" == "$source_sha_before" ]] || \
    die "production source rollout changed during isolated acceptance"

  CURRENT_STEP=cleanup-isolated-runtime
  cleanup_runtime
  mount_present "$MOUNT_ROOT" && die "isolated mount remained after cleanup"
  if pgrep -f "$CANDIDATE_BIN" >/dev/null 2>&1 || pgrep -f "$MOUNT_ROOT" >/dev/null 2>&1; then
    die "isolated acceptance process remained after cleanup"
  fi

  CURRENT_STEP=write-summary
  jq -n \
    --arg schema "$SCHEMA" --arg result pass --arg completed_at "$(timestamp)" \
    --arg session_id "$SESSION_ID" --arg frontend "$frontend" --arg model "$model" \
    --arg candidate_artifact_mode "$candidate_artifact_mode" \
    --arg candidate_sha256 "$candidate_sha" --arg source_sha256 "$source_sha_before" \
    --argjson source_bytes "$source_bytes" --arg manifest_sha256 "$manifest_sha" \
    --argjson generation "$generation" --arg pack_generation "$pack_generation" \
    --argjson object_count "$object_count" --argjson loose_object_count "$loose_count" \
    --argjson full_bytes "$full_bytes" --arg full_sha256 "$full_sha" --argjson delta_bytes "$delta_bytes" \
    --arg idempotent_inventory_sha256 "$inventory_after" --arg disabled_inventory_sha256 "$disabled_inventory_after" \
    --slurpfile physical_space "$EVIDENCE_ROOT/physical-space.json" \
    '{schema:$schema,result:$result,completed_at:$completed_at,session:{id:$session_id,source_bytes:$source_bytes,source_sha256:$source_sha256,full_bytes:$full_bytes,full_sha256:$full_sha256,delta_bytes:$delta_bytes,base_prefix_unchanged:true,jsonl_valid:true},candidate:{sha256:$candidate_sha256,frontend:$frontend,model:$model,artifact_mode:$candidate_artifact_mode},automatic_enrollment:{hot_enable:true,waiting_observed:true,fold_pack_migrate_reclaim_progress:"0/6..6/6",managed_count:1,idempotent_cycle:true,idempotent_inventory_sha256:$idempotent_inventory_sha256,hot_disable:true,disabled_inventory_sha256:$disabled_inventory_sha256},fold:{manifest_sha256:$manifest_sha256,generation:$generation,pack_generation:$pack_generation,object_count:$object_count,loose_object_count:$loose_object_count,fold_doctor_issues:0,pack_doctor_issues:0,native_snapshot_retained:false,loose_objects_retained:false,physical_space:$physical_space[0]},codex:{unmodified_cli:true,reply_marker:"CODEXFOLD_REAL_FOLD_CURRENT_WORKTREE_OK",append_to_delta:true,writable_backing_created:false},isolation:{production_source_unchanged:true,runtime_cleaned:true,credentials_removed_with_work_root:true}}' \
    > "$EVIDENCE_ROOT/summary.json"
  if [[ "$skip_resume" == true ]]; then
    jq '.codex = {status:"NOT RUN",reason:"--skip-resume; no model request was sent"}' \
      "$EVIDENCE_ROOT/summary.json" > "$EVIDENCE_ROOT/.summary.tmp"
    mv "$EVIDENCE_ROOT/.summary.tmp" "$EVIDENCE_ROOT/summary.json"
  fi
  {
    printf '# Real Fold Acceptance\n\n'
    # shellcheck disable=SC2016
    printf -- '- Result: `PASS`\n'
    # shellcheck disable=SC2016
    printf -- '- Frontend: `%s`\n' "$frontend"
    # shellcheck disable=SC2016
    printf -- '- Session: `%s` (%s -> %s bytes; base SHA unchanged)\n' "$SESSION_ID" "$source_bytes" "$full_bytes"
    # shellcheck disable=SC2016
    printf -- '- Candidate SHA-256: `%s`\n' "$candidate_sha"
    # shellcheck disable=SC2016
    printf -- '- Automatic path: disabled -> waiting -> fold/pack/migrate/reclaim `0/6..6/6` -> idempotent -> disabled\n'
    # shellcheck disable=SC2016
    if [[ "$skip_resume" == false ]]; then
      printf -- '- Codex: unmodified CLI, model `%s`, reply marker matched, delta=%s bytes\n' "$model" "$delta_bytes"
    else
      printf -- '- Codex CLI resume: NOT RUN (--skip-resume); no model request sent\n'
    fi
    printf -- '- Integrity: fold doctor 0 issues; pack doctor 0 issues; full and delta JSONL valid\n'
    printf -- '- Allocated space: %s -> %s bytes; native snapshot and duplicate loose objects retired before resume\n' "$physical_before" "$physical_after"
    printf -- '- Isolation: production source identity/SHA unchanged; runtime and copied credentials removed\n'
  } > "$EVIDENCE_ROOT/summary.md"

  rm -rf "$WORK_ROOT"
  rm -rf "$RUNTIME_ROOT"
  trap - EXIT HUP INT TERM
  echo "$EVIDENCE_ROOT/summary.json"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
