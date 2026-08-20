#!/usr/bin/env bash
set -euo pipefail

# Controlled adapter for Cockpit Tools' persisted Codex instance source of
# truth. It mirrors the stable InstanceStore fields used by
# codex_create_instance(existingdir) and update_default_settings without reading
# account/token stores. It deliberately cannot write launch PID/time; only the
# real Cockpit codex_start_instance command may supply that acceptance evidence.

usage() {
  cat >&2 <<'EOF'
usage: cockpit-codex-instance-adapter.sh COMMAND [options]

Commands:
  electron-data  Print Cockpit's deterministic Electron data directory.
  register       Persist/update one managed instance (dry-run unless --apply).
  verify         Emit a redacted JSON verification result.

Common options:
  --store FILE          Cockpit codex_instances.json source of truth.
  --instance-id ID      Managed instance ID.
  --codex-home DIR      Existing isolated CODEX_HOME.
  --working-dir DIR     Existing task workspace.
  --name NAME           Managed instance display name.

Mutation options:
  --backup FILE         Private byte-for-byte pre-mutation backup.
  --apply               Execute the mutation; otherwise print a dry-run.
EOF
  exit 2
}

die() {
  echo "$*" >&2
  exit 1
}

timestamp() {
  date -u '+%Y-%m-%dT%H:%M:%SZ'
}

epoch_millis() {
  printf '%s000\n' "$(date -u '+%s')"
}

file_sha() {
  local path=$1
  if [[ ! -e "$path" ]]; then
    printf 'missing\n'
    return
  fi
  [[ -f "$path" && ! -L "$path" ]] || die "Cockpit instance store is not a regular non-symlink file: $path"
  shasum -a 256 "$path" | awk '{print $1}'
}

directory_identity() {
  local path=$1
  [[ -d "$path" && ! -L "$path" ]] || die "directory is not a real non-symlink directory: $path"
  stat -f '%d:%i' "$path"
}

canonical_existing_dir() {
  local path=$1
  [[ -d "$path" && ! -L "$path" ]] || die "directory is not a real non-symlink directory: $path"
  (cd "$path" && pwd -P)
}

validate_store() {
  local store=$1
  [[ ! -L "$store" ]] || die "Cockpit instance store may not be a symbolic link: $store"
  [[ ! -e "$store" || -f "$store" ]] || die "Cockpit instance store is not a regular file: $store"
  if [[ -f "$store" ]]; then
    jq -e '
      type == "object" and
      ((.instances // []) | type == "array") and
      ((.defaultSettings // {}) | type == "object")
    ' "$store" >/dev/null || die "Cockpit instance store is invalid JSON or has an unsupported shape"
  fi
}

with_store_lock() {
  local store=$1
  local callback=$2
  local lock_dir="${store}.codexfold-acceptance.lock"
  if ! mkdir "$lock_dir" 2>/dev/null; then
    die "Cockpit instance store is already being changed by another acceptance adapter"
  fi
  lock_held=true
  trap 'if [[ "$lock_held" == true ]]; then rmdir "$lock_dir" 2>/dev/null || true; fi' EXIT HUP INT TERM
  "$callback"
  rmdir "$lock_dir"
  lock_held=false
  trap - EXIT HUP INT TERM
}

write_store_atomically() {
  local store=$1
  local prepared=$2
  local expected_sha=$3
  local current_sha temp parent_identity
  parent_identity=$(directory_identity "$(dirname "$store")")
  current_sha=$(file_sha "$store")
  [[ "$current_sha" == "$expected_sha" ]] || die "Cockpit instance store changed during mutation; refusing to overwrite it"
  temp=$(mktemp "$(dirname "$store")/.codex_instances.XXXXXX")
  cp "$prepared" "$temp"
  chmod 600 "$temp"
  [[ "$(directory_identity "$(dirname "$store")")" == "$parent_identity" ]] || die "Cockpit store directory changed during mutation"
  [[ ! -L "$store" ]] || die "Cockpit store became a symbolic link during mutation"
  [[ "$(file_sha "$store")" == "$expected_sha" ]] || die "Cockpit instance store changed before commit"
  mv "$temp" "$store"
}

compute_electron_data() {
  local store=$1
  local codex_home=$2
  local data_root digest
  data_root=$(cd "$(dirname "$store")" && pwd -P)
  if command -v md5 >/dev/null; then
    digest=$(md5 -q -s "$codex_home")
  elif command -v md5sum >/dev/null; then
    digest=$(printf '%s' "$codex_home" | md5sum | awk '{print $1}')
  else
    die "md5 or md5sum is required to derive Cockpit's Electron data directory"
  fi
  printf '%s/instances/codex-app-data/%s\n' "$data_root" "$digest"
}

command_name=${1:-}
[[ -n "$command_name" ]] || usage
shift

store=''
instance_id=''
codex_home=''
working_dir=''
instance_name='CodexFold isolated acceptance'
backup=''
apply=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --store) store=${2:?}; shift 2 ;;
    --instance-id) instance_id=${2:?}; shift 2 ;;
    --codex-home) codex_home=${2:?}; shift 2 ;;
    --working-dir) working_dir=${2:?}; shift 2 ;;
    --name) instance_name=${2:?}; shift 2 ;;
    --backup) backup=${2:?}; shift 2 ;;
    --apply) apply=true; shift ;;
    --dry-run) apply=false; shift ;;
    -h|--help) usage ;;
    *) die "unknown adapter option: $1" ;;
  esac
done

[[ -n "$store" && -n "$codex_home" ]] || die "--store and --codex-home are required"
store_parent=$(dirname "$store")
[[ -d "$store_parent" && ! -L "$store_parent" ]] || die "Cockpit data directory is not a real non-symlink directory: $store_parent"
store_parent=$(cd "$store_parent" && pwd -P)
store="$store_parent/$(basename "$store")"
[[ ! -L "$store" ]] || die "Cockpit instance store may not be a symbolic link"
codex_home=$(canonical_existing_dir "$codex_home")

if [[ "$command_name" == electron-data ]]; then
  compute_electron_data "$store" "$codex_home"
  exit 0
fi

[[ -n "$instance_id" && "$instance_id" =~ ^[0-9A-Za-z._-]+$ ]] || die "a safe --instance-id is required"
[[ -n "$working_dir" ]] || die "--working-dir is required"
working_dir=$(canonical_existing_dir "$working_dir")
[[ -n "${instance_name//[[:space:]]/}" ]] || die "instance name may not be empty"
if [[ -n "$backup" ]]; then
  backup_parent=$(dirname "$backup")
  [[ -d "$backup_parent" && ! -L "$backup_parent" ]] || die "backup directory is not a real non-symlink directory"
  backup_parent=$(cd "$backup_parent" && pwd -P)
  backup="$backup_parent/$(basename "$backup")"
  [[ ! -L "$backup" ]] || die "backup path may not be a symbolic link"
fi
validate_store "$store"

verify_json() {
  local registered=false auto_sync=false auto_repair=false protect_config=false home_match=false working_match=false launch_mode_match=false bind_null=false
  local count=0 home_conflicts=0 last_pid=null last_launched=null sha
  if [[ -f "$store" ]]; then
    count=$(jq --arg id "$instance_id" '[.instances[]? | select(.id == $id)] | length' "$store")
    home_conflicts=$(jq --arg id "$instance_id" --arg home "$codex_home" \
      '[.instances[]? | select(.id != $id and .userDataDir == $home)] | length' "$store")
    if [[ "$count" == 1 ]]; then
      registered=true
      [[ "$(jq -r --arg id "$instance_id" '.instances[] | select(.id == $id) | .userDataDir' "$store")" == "$codex_home" ]] && home_match=true
      [[ "$(jq -r --arg id "$instance_id" '.instances[] | select(.id == $id) | .workingDir' "$store")" == "$working_dir" ]] && working_match=true
      [[ "$(jq -r --arg id "$instance_id" '.instances[] | select(.id == $id) | .launchMode' "$store")" == app ]] && launch_mode_match=true
      [[ "$(jq -r --arg id "$instance_id" '.instances[] | select(.id == $id) | .bindAccountId' "$store")" == null ]] && bind_null=true
      last_pid=$(jq --arg id "$instance_id" '.instances[] | select(.id == $id) | .lastPid // null' "$store")
      last_launched=$(jq --arg id "$instance_id" '.instances[] | select(.id == $id) | .lastLaunchedAt // null' "$store")
    fi
    jq -e '.defaultSettings | (.autoSyncThreads | type == "boolean" and . == false)' "$store" >/dev/null 2>&1 && auto_sync=true
    jq -e '.defaultSettings | (.autoRepairSessionVisibilityOnLaunch | type == "boolean" and . == false)' "$store" >/dev/null 2>&1 && auto_repair=true
    jq -e '.defaultSettings | (.protectConfigOnLaunch | type == "boolean" and . == true)' "$store" >/dev/null 2>&1 && protect_config=true
  fi
  sha=$(file_sha "$store")
  jq -n \
    --arg checkedAt "$(timestamp)" \
    --arg instanceId "$instance_id" \
    --arg storeSha256 "$sha" \
    --arg electronUserData "$(compute_electron_data "$store" "$codex_home")" \
    --argjson registered "$registered" \
    --argjson instanceCount "$count" \
    --argjson conflictingHomes "$home_conflicts" \
    --argjson autoSyncFalse "$auto_sync" \
    --argjson autoRepairFalse "$auto_repair" \
    --argjson protectConfigTrue "$protect_config" \
    --argjson homeMatch "$home_match" \
    --argjson workingMatch "$working_match" \
    --argjson launchModeMatch "$launch_mode_match" \
    --argjson bindAccountNull "$bind_null" \
    --argjson lastPid "$last_pid" \
    --argjson lastLaunchedAt "$last_launched" \
    '{checkedAt:$checkedAt,instanceId:$instanceId,registered:$registered,instanceCount:$instanceCount,conflictingHomes:$conflictingHomes,autoSyncThreadsFalse:$autoSyncFalse,autoRepairSessionVisibilityFalse:$autoRepairFalse,protectConfigOnLaunchTrue:$protectConfigTrue,codexHomeMatches:$homeMatch,workingDirMatches:$workingMatch,launchModeApp:$launchModeMatch,bindAccountNull:$bindAccountNull,lastPid:$lastPid,lastLaunchedAt:$lastLaunchedAt,electronUserData:$electronUserData,storeSha256:$storeSha256,managed:($registered and $instanceCount == 1 and $conflictingHomes == 0 and $autoSyncFalse and $autoRepairFalse and $protectConfigTrue and $homeMatch and $workingMatch and $launchModeMatch and $bindAccountNull)}'
}

case "$command_name" in
  verify)
    verify_json
    ;;
  register)
    if [[ "$apply" != true ]]; then
      echo "dry-run: would prepare Cockpit instance record $instance_id and set autoSyncThreads=false"
      exit 0
    fi
    mutate_register() {
      local before_sha prepared existing_count conflicting_count created_at existing_last_pid existing_last_launched
      before_sha=$(file_sha "$store")
      prepared=$(mktemp "${TMPDIR:-/tmp}/codexfold-cockpit-register.XXXXXX")
      if [[ -f "$store" ]]; then
        existing_count=$(jq --arg id "$instance_id" '[.instances[]? | select(.id == $id)] | length' "$store")
        conflicting_count=$(jq --arg id "$instance_id" --arg home "$codex_home" \
          '[.instances[]? | select(.id != $id and .userDataDir == $home)] | length' "$store")
        (( existing_count <= 1 )) || die "Cockpit store contains duplicate acceptance instance IDs"
        (( conflicting_count == 0 )) || die "another Cockpit instance already owns this isolated CODEX_HOME"
        if (( existing_count == 1 )); then
          existing_last_pid=$(jq --arg id "$instance_id" '.instances[] | select(.id == $id) | .lastPid // null' "$store")
          existing_last_launched=$(jq --arg id "$instance_id" '.instances[] | select(.id == $id) | .lastLaunchedAt // null' "$store")
          [[ "$existing_last_pid" == null && "$existing_last_launched" == null ]] || \
            die "acceptance instance already contains launch state; create a fresh run instead of reusing it"
        fi
        if [[ -n "$backup" && ! -e "$backup" ]]; then
          cp "$store" "$backup"
          chmod 600 "$backup"
        fi
        created_at=$(epoch_millis)
        jq \
          --arg id "$instance_id" \
          --arg name "$instance_name" \
          --arg home "$codex_home" \
          --arg working "$working_dir" \
          --argjson createdAt "$created_at" '
            .instances = (.instances // []) |
            .defaultSettings = ((.defaultSettings // {}) + {
              autoSyncThreads: false,
              protectConfigOnLaunch: true,
              autoRepairSessionVisibilityOnLaunch: false
            }) |
            if ([.instances[] | select(.id == $id)] | length) == 1 then
              .instances |= map(
                if .id == $id then
                  . + {
                    name: $name,
                    userDataDir: $home,
                    workingDir: $working,
                    extraArgs: "",
                    bindAccountId: null,
                    launchMode: "app",
                    createdAt: (.createdAt // $createdAt)
                  }
                else . end
              )
            else
              .instances += [{
                id: $id,
                name: $name,
                userDataDir: $home,
                workingDir: $working,
                extraArgs: "",
                bindAccountId: null,
                launchMode: "app",
                createdAt: $createdAt,
                lastLaunchedAt: null,
                lastPid: null
              }]
            end
          ' "$store" > "$prepared"
      else
        created_at=$(epoch_millis)
        jq -n \
          --arg id "$instance_id" \
          --arg name "$instance_name" \
          --arg home "$codex_home" \
          --arg working "$working_dir" \
          --argjson createdAt "$created_at" '
            {
              instances: [{
                id: $id,
                name: $name,
                userDataDir: $home,
                workingDir: $working,
                extraArgs: "",
                bindAccountId: null,
                launchMode: "app",
                createdAt: $createdAt,
                lastLaunchedAt: null,
                lastPid: null
              }],
              defaultSettings: {
                autoSyncThreads: false,
                protectConfigOnLaunch: true,
                autoRepairSessionVisibilityOnLaunch: false
              }
            }
          ' > "$prepared"
      fi
      write_store_atomically "$store" "$prepared" "$before_sha"
      rm -f "$prepared"
    }
    with_store_lock "$store" mutate_register
    result=$(verify_json)
    [[ "$(jq -r '.managed' <<< "$result")" == true ]] || die "Cockpit managed instance verification failed after registration"
    printf '%s\n' "$result"
    ;;
  *)
    die "unknown adapter command: $command_name"
    ;;
esac
