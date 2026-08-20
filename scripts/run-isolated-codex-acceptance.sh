#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
SOURCE_REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd -P)
PREPARE_HOME="$SCRIPT_DIR/prepare-isolated-codex-home.sh"
COCKPIT_ADAPTER="$SCRIPT_DIR/cockpit-codex-instance-adapter.sh"
SCHEMA=codexfold.isolated-acceptance.v3
CANDIDATE_INPUT_SCHEMA=codexfold.external-candidate-evidence.v3
CANDIDATE_EVIDENCE_SCHEMA=codexfold.candidate-evidence.v3
BACKEND_CRASH_EVIDENCE_SCHEMA=codexfold.candidate-backend-crash-respawn.v1
NATIVE_INCIDENT_INPUT_SCHEMA=codexfold.external-native-incident-observation.v1
NATIVE_INCIDENT_EVIDENCE_SCHEMA=codexfold.native-incident-observed.v1
NATIVE_INCIDENT_REVIEW_SCHEMA=codexfold.native-incident-review.v1
FSKIT_APP_BUNDLE_IDENTIFIER=vip.jstar.codexfold.fskitprofileprobe
FSKIT_MODULE_BUNDLE_IDENTIFIER=vip.jstar.codexfold.fskitprofileprobe.module
FSKIT_MODULE_BUNDLE_NAME=CodexFoldFSKitModule.appex
FSKIT_MODULE_PROCESS_NAME=CodexFoldFSKitModule
FSKIT_SHORT_NAME=codexfoldnative

usage() {
  cat >&2 <<'EOF'
usage: run-isolated-codex-acceptance.sh COMMAND [options]

Commands:
  prepare  Create a private CODEX_HOME/session slice and executable harnesses.
  cockpit-register  Persist the isolated instance in Cockpit (dry-run unless --apply).
  launch   Open isolated Cockpit and observe its real Start action (dry-run unless --apply).
  task     Run an isolated CLI diagnostic task (not Desktop real-task evidence).
  observe-task  Wait for a real task performed in the Cockpit-started Desktop.
  candidate-evidence  Validate externally captured real CodexFold candidate evidence.
  incident-evidence  Validate and retain native incident census/screenshots/exports.
  incident-review  Retain the operator's bound review of the native incident GUI.
  verify   Verify isolation, copied data, ancestry, and protected PIDs.
  fault    Inject a bounded candidate-only fault (dry-run unless --apply).
  report   Regenerate the redacted Markdown evidence summary.

Run COMMAND --help for its options. No command stops, restarts, or signals a
pre-existing Codex process. Fault injection accepts only candidate artifacts
under the marked run root.
EOF
  exit 2
}

die() {
  echo "$*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null || die "required command is unavailable: $1"
}

timestamp() {
  date -u '+%Y-%m-%dT%H:%M:%SZ'
}

epoch_millis() {
  printf '%s000\n' "$(date -u '+%s')"
}

file_sha() {
  local path=$1
  [[ -f "$path" && ! -L "$path" ]] || return 1
  shasum -a 256 "$path" | awk '{print $1}'
}

text_sha() {
  shasum -a 256 | awk '{print $1}'
}

source_snapshot_sha() {
  local root=$1 inventory paths_before paths_after relative path kind mode digest link_target
  local repo_top identity_before identity_after snapshot
  [[ -d "$root" && ! -L "$root" ]] || return 1
  root=$(cd "$root" 2>/dev/null && pwd -P) || return 1
  repo_top=$(git -C "$root" rev-parse --show-toplevel 2>/dev/null) || return 1
  repo_top=$(cd "$repo_top" 2>/dev/null && pwd -P) || return 1
  [[ "$repo_top" == "$root" ]] || return 1
  inventory=$(mktemp "${TMPDIR:-/tmp}/codexfold-source-snapshot.XXXXXX") || return 1
  paths_before=$(mktemp "${TMPDIR:-/tmp}/codexfold-source-paths-before.XXXXXX") || { rm -f "$inventory"; return 1; }
  paths_after=$(mktemp "${TMPDIR:-/tmp}/codexfold-source-paths-after.XXXXXX") || {
    rm -f "$inventory" "$paths_before"
    return 1
  }
  git -C "$root" ls-files -z --cached --others --exclude-standard > "$paths_before" || {
    rm -f "$inventory" "$paths_before" "$paths_after"
    return 1
  }
  while IFS= read -r -d '' relative; do
    case "$relative" in
      *$'\t'*|*$'\r'*|*$'\n'*) rm -f "$inventory" "$paths_before" "$paths_after"; return 1 ;;
    esac
    path="$root/$relative"
    identity_before=$(stat -f '%d:%i:%z:%m:%c:%p:%HT' "$path" 2>/dev/null || true)
    if [[ -L "$path" ]]; then
      kind=symlink
      mode=$(stat -f '%p' "$path") || { rm -f "$inventory" "$paths_before" "$paths_after"; return 1; }
      link_target=$(readlink "$path") || { rm -f "$inventory" "$paths_before" "$paths_after"; return 1; }
      digest=$(printf '%s' "$link_target" | text_sha)
    elif [[ -f "$path" ]]; then
      kind='file'
      mode=$(stat -f '%p' "$path") || { rm -f "$inventory" "$paths_before" "$paths_after"; return 1; }
      digest=$(file_sha "$path") || { rm -f "$inventory" "$paths_before" "$paths_after"; return 1; }
    elif [[ ! -e "$path" ]]; then
      kind=missing
      mode=0
      digest=$(printf 'missing' | text_sha)
    else
      rm -f "$inventory" "$paths_before" "$paths_after"
      return 1
    fi
    identity_after=$(stat -f '%d:%i:%z:%m:%c:%p:%HT' "$path" 2>/dev/null || true)
    [[ "$identity_before" == "$identity_after" ]] || {
      rm -f "$inventory" "$paths_before" "$paths_after"
      return 1
    }
    printf '%s\t%s\t%s\t%s\n' "$kind" "$mode" "$digest" "$relative" >> "$inventory"
  done < "$paths_before"
  git -C "$root" ls-files -z --cached --others --exclude-standard > "$paths_after" || {
    rm -f "$inventory" "$paths_before" "$paths_after"
    return 1
  }
  /usr/bin/cmp -s "$paths_before" "$paths_after" || {
    rm -f "$inventory" "$paths_before" "$paths_after"
    return 1
  }
  snapshot=$(LC_ALL=C sort "$inventory" | text_sha) || {
    rm -f "$inventory" "$paths_before" "$paths_after"
    return 1
  }
  rm -f "$inventory" "$paths_before" "$paths_after"
  printf '%s\n' "$snapshot"
}

source_git_head() {
  git -C "$1" rev-parse --verify HEAD 2>/dev/null || true
}

directory_identity() {
  local path=$1
  [[ -d "$path" && ! -L "$path" ]] || return 1
  stat -f '%d:%i' "$path"
}

canonical_existing_directory() {
  local path=$1
  [[ -d "$path" && ! -L "$path" ]] || return 1
  (cd "$path" && pwd -P)
}

require_fenced_directory() {
  local label=$1 path=$2 expected_path=$3 expected_identity=$4
  local canonical identity
  canonical=$(canonical_existing_directory "$path") || die "$label is not a real non-symlink directory: $path"
  [[ "$canonical" == "$expected_path" ]] || die "$label real path changed: $path"
  identity=$(directory_identity "$canonical") || die "$label identity is unavailable"
  [[ "$identity" == "$expected_identity" ]] || die "$label inode changed since prepare"
}

canonical_new_path() {
  local value=$1
  local parent
  mkdir -p "$(dirname "$value")"
  parent=$(cd "$(dirname "$value")" && pwd -P)
  printf '%s/%s\n' "$parent" "$(basename "$value")"
}

load_run() {
  local requested=$1
  local marker schema
  [[ -d "$requested" && ! -L "$requested" ]] || die "run root is not a real non-symlink directory: $requested"
  RUN_ROOT=$(cd "$requested" 2>/dev/null && pwd -P) || die "run root does not exist: $requested"
  marker="$RUN_ROOT/.codexfold-isolated-acceptance"
  [[ -f "$marker" && ! -L "$marker" && -f "$RUN_ROOT/run.json" && ! -L "$RUN_ROOT/run.json" ]] || die "not a marked isolated acceptance root: $RUN_ROOT"
  schema=$(jq -r '.schema // empty' "$RUN_ROOT/run.json")
  if [[ "$schema" != "$SCHEMA" ]]; then
    case "$schema" in
      codexfold.isolated-acceptance.v1|codexfold.isolated-acceptance.v2)
        die "legacy acceptance run $schema cannot prove the loaded Swift FSKit module identity; create a fresh $SCHEMA run"
        ;;
      *) die "unsupported acceptance run schema: $schema" ;;
    esac
  fi
  [[ "$(cat "$marker")" == "$SCHEMA" ]] || die "acceptance marker does not match run metadata"
  [[ "$(jq -r '.runRoot // empty' "$RUN_ROOT/run.json")" == "$RUN_ROOT" ]] || die "run root metadata does not match its real path"
  CODEX_HOME_ISOLATED=$(jq -r '.codexHome' "$RUN_ROOT/run.json")
  ELECTRON_DATA=$(jq -r '.electronUserData' "$RUN_ROOT/run.json")
  CANDIDATE_ROOT=$(jq -r '.candidateRoot' "$RUN_ROOT/run.json")
  EVIDENCE_ROOT=$(jq -r '.evidenceRoot' "$RUN_ROOT/run.json")
  APP_PATH=$(jq -r '.appPath' "$RUN_ROOT/run.json")
  PROTECTED_APP_PATH=$(jq -r '.protectedAppPath // .appPath' "$RUN_ROOT/run.json")
  COCKPIT_APP_PATH=$(jq -r '.cockpitAppPath' "$RUN_ROOT/run.json")
  COCKPIT_BUNDLE_IDENTIFIER=$(jq -r '.cockpitBundleIdentifier // empty' "$RUN_ROOT/run.json")
  COCKPIT_BUNDLE_SHORT_VERSION=$(jq -r '.cockpitBundleShortVersion // empty' "$RUN_ROOT/run.json")
  COCKPIT_BUNDLE_VERSION=$(jq -r '.cockpitBundleVersion // empty' "$RUN_ROOT/run.json")
  COCKPIT_EXECUTABLE_SHA=$(jq -r '.cockpitExecutableSHA256 // empty' "$RUN_ROOT/run.json")
  CODEX_APP_EXECUTABLE_SHA=$(jq -r '.codexAppExecutableSHA256 // empty' "$RUN_ROOT/run.json")
  COCKPIT_DATA_ROOT=$(jq -r '.cockpitDataRoot' "$RUN_ROOT/run.json")
  COCKPIT_STORE=$(jq -r '.cockpitStorePath' "$RUN_ROOT/run.json")
  COCKPIT_INSTANCE_ID=$(jq -r '.runId' "$RUN_ROOT/run.json")
  WORKSPACE=$(jq -r '.workspace' "$RUN_ROOT/run.json")
  RUN_CANDIDATE_ATTACHED=$(jq -r '.candidateAttached // false' "$RUN_ROOT/run.json")
  RUN_CANDIDATE_BUILD_SHA=$(jq -r '.candidateBuildSHA // empty' "$RUN_ROOT/run.json")
  RUN_CANDIDATE_APP_IDENTITY=$(jq -r '.candidateAppIdentitySHA256 // empty' "$RUN_ROOT/run.json")
  RUN_CANDIDATE_MODULE_SHA=$(jq -r '.candidateFSKitModuleSHA256 // empty' "$RUN_ROOT/run.json")
  RUN_CANDIDATE_BUILD_MANIFEST_SHA=$(jq -r '.candidateBuildManifestSHA256 // empty' "$RUN_ROOT/run.json")
  RUN_MANAGED_ROUTE_OBSERVED=$(jq -r '.managedRouteObserved // false' "$RUN_ROOT/run.json")
  RUN_CANDIDATE_EVIDENCE_SHA=$(jq -r '.candidateEvidenceSHA256 // empty' "$RUN_ROOT/run.json")
  RUN_BACKEND_CRASH_EVIDENCE_SHA=$(jq -r '.backendCrashRespawnEvidenceSHA256 // empty' "$RUN_ROOT/run.json")
  RUN_NATIVE_INCIDENT_EVIDENCE_SHA=$(jq -r '.nativeIncidentEvidenceSHA256 // empty' "$RUN_ROOT/run.json")
  RUN_NATIVE_INCIDENT_REVIEW_SHA=$(jq -r '.nativeIncidentReviewSHA256 // empty' "$RUN_ROOT/run.json")
  FSKIT_MODULE_BASELINE=$(jq -r '.fskitModuleProcessBaselinePath // empty' "$RUN_ROOT/run.json")
  FSKIT_MODULE_BASELINE_SHA=$(jq -r '.fskitModuleProcessBaselineSHA256 // empty' "$RUN_ROOT/run.json")
  PROTECTED_PROCESS_BASELINE=$(jq -r '.protectedProcessBaselinePath // empty' "$RUN_ROOT/run.json")
  PROTECTED_PROCESS_BASELINE_SHA=$(jq -r '.protectedProcessBaselineSHA256 // empty' "$RUN_ROOT/run.json")
  SOURCE_PROVENANCE_REPO=$(jq -r '.sourceProvenance.repoRoot // empty' "$RUN_ROOT/run.json")
  SOURCE_PROVENANCE_REPO_IDENTITY=$(jq -r '.sourceProvenance.repoRootIdentity // empty' "$RUN_ROOT/run.json")
  SOURCE_PROVENANCE_HEAD=$(jq -r '.sourceProvenance.gitHead // empty' "$RUN_ROOT/run.json")
  SOURCE_PROVENANCE_SNAPSHOT_SHA=$(jq -r '.sourceProvenance.snapshotSHA256 // empty' "$RUN_ROOT/run.json")
  [[ "$CODEX_HOME_ISOLATED" == "$RUN_ROOT/"* ]] || die "isolated CODEX_HOME escaped run root"
  [[ -n "$ELECTRON_DATA" && "$ELECTRON_DATA" != "$CODEX_HOME_ISOLATED" ]] || die "invalid Cockpit Electron data directory"
  [[ "$CANDIDATE_ROOT" == "$RUN_ROOT/"* ]] || die "candidate root escaped run root"
  [[ "$EVIDENCE_ROOT" == "$RUN_ROOT/"* ]] || die "evidence root escaped run root"
  [[ "$COCKPIT_DATA_ROOT" == "$RUN_ROOT/"* ]] || die "Cockpit control plane escaped run root"
  [[ "$COCKPIT_STORE" == "$COCKPIT_DATA_ROOT/"* ]] || die "Cockpit instance store escaped isolated control plane"
  [[ "$ELECTRON_DATA" == "$COCKPIT_DATA_ROOT/"* ]] || die "Cockpit Electron data escaped isolated control plane"
  [[ "$(jq -r '.autoSyncThreads' "$RUN_ROOT/run.json")" == false ]] || die "autoSyncThreads must be false"
  [[ "$RUN_CANDIDATE_ATTACHED" == true || "$RUN_CANDIDATE_ATTACHED" == false ]] || die "candidateAttached must be a JSON boolean"
  [[ "$RUN_MANAGED_ROUTE_OBSERVED" == true || "$RUN_MANAGED_ROUTE_OBSERVED" == false ]] || die "managedRouteObserved must be a JSON boolean"
  require_fenced_directory "run root" "$RUN_ROOT" "$RUN_ROOT" "$(jq -r '.pathFences.runRoot.identity // empty' "$RUN_ROOT/run.json")"
  require_fenced_directory "isolated CODEX_HOME" "$CODEX_HOME_ISOLATED" "$CODEX_HOME_ISOLATED" "$(jq -r '.pathFences.codexHome.identity // empty' "$RUN_ROOT/run.json")"
  require_fenced_directory "candidate root" "$CANDIDATE_ROOT" "$CANDIDATE_ROOT" "$(jq -r '.pathFences.candidateRoot.identity // empty' "$RUN_ROOT/run.json")"
  require_fenced_directory "evidence root" "$EVIDENCE_ROOT" "$EVIDENCE_ROOT" "$(jq -r '.pathFences.evidenceRoot.identity // empty' "$RUN_ROOT/run.json")"
  require_fenced_directory "Cockpit data root" "$COCKPIT_DATA_ROOT" "$COCKPIT_DATA_ROOT" "$(jq -r '.pathFences.cockpitDataRoot.identity // empty' "$RUN_ROOT/run.json")"
  require_fenced_directory "Cockpit Electron data" "$ELECTRON_DATA" "$ELECTRON_DATA" "$(jq -r '.pathFences.electronData.identity // empty' "$RUN_ROOT/run.json")"
  [[ ! -L "$COCKPIT_STORE" ]] || die "Cockpit instance store may not be a symbolic link"
  [[ "$FSKIT_MODULE_BASELINE" == "$EVIDENCE_ROOT/fskit-module-processes.before.tsv" && -f "$FSKIT_MODULE_BASELINE" && ! -L "$FSKIT_MODULE_BASELINE" ]] || \
    die "FSKit module pre-mount process baseline is missing or escaped the evidence root"
  [[ "$FSKIT_MODULE_BASELINE_SHA" =~ ^[0-9a-f]{64}$ && "$(file_sha "$FSKIT_MODULE_BASELINE")" == "$FSKIT_MODULE_BASELINE_SHA" ]] || \
    die "FSKit module pre-mount process baseline changed since prepare"
  [[ "$PROTECTED_PROCESS_BASELINE" == "$EVIDENCE_ROOT/protected-processes.before.tsv" && -f "$PROTECTED_PROCESS_BASELINE" && ! -L "$PROTECTED_PROCESS_BASELINE" ]] || \
    die "protected Codex process baseline is missing or escaped the evidence root"
  [[ "$PROTECTED_PROCESS_BASELINE_SHA" =~ ^[0-9a-f]{64}$ && "$(file_sha "$PROTECTED_PROCESS_BASELINE")" == "$PROTECTED_PROCESS_BASELINE_SHA" ]] || \
    die "protected Codex process baseline changed since prepare"
  [[ "$SOURCE_PROVENANCE_REPO" == "$SOURCE_REPO_ROOT" && "$SOURCE_PROVENANCE_REPO_IDENTITY" == "$(directory_identity "$SOURCE_REPO_ROOT")" && \
     "$SOURCE_PROVENANCE_SNAPSHOT_SHA" =~ ^[0-9a-f]{64}$ ]] || die "candidate source provenance does not match this repository"
}

cockpit_verify_json() {
  "$COCKPIT_ADAPTER" verify \
    --store "$COCKPIT_STORE" \
    --instance-id "$COCKPIT_INSTANCE_ID" \
    --codex-home "$CODEX_HOME_ISOLATED" \
    --working-dir "$WORKSPACE" \
    --name 'CodexFold isolated acceptance'
}

load_cockpit_verification() {
  local result=$1
  COCKPIT_MANAGED=$(jq -r '.managed' <<< "$result")
  COCKPIT_SYNC_FALSE=$(jq -r '.autoSyncThreadsFalse' <<< "$result")
  COCKPIT_LAST_PID=$(jq -r '.lastPid // empty' <<< "$result")
  COCKPIT_ELECTRON_DATA=$(jq -r '.electronUserData' <<< "$result")
  COCKPIT_LAST_LAUNCHED=$(jq -r '.lastLaunchedAt // empty' <<< "$result")
}

bundle_executable_path() {
  local app=$1
  local executable
  executable=$(plutil -extract CFBundleExecutable raw -o - "$app/Contents/Info.plist" 2>/dev/null || true)
  if [[ -z "$executable" ]]; then
    executable=$(basename "$app" .app)
  fi
  printf '%s/Contents/MacOS/%s\n' "$app" "$executable"
}

bundle_info_value() {
  local app=$1
  local key=$2
  plutil -extract "$key" raw -o - "$app/Contents/Info.plist" 2>/dev/null || true
}

codesign_detail_value() {
  local path=$1 key=$2 output
  output=$(/usr/bin/codesign -d --verbose=4 "$path" 2>&1) || return 1
  printf '%s\n' "$output" | sed -n "s/^${key}=//p" | head -n 1
}

candidate_bundle_executable_path() {
  local bundle=$1 executable path
  executable=$(bundle_info_value "$bundle" CFBundleExecutable)
  [[ -n "$executable" && "$executable" == "$(basename "$executable")" && "$executable" != "." && "$executable" != ".." ]] || return 1
  path="$bundle/Contents/MacOS/$executable"
  canonical_candidate_file "$path"
}

candidate_app_directory() {
  local app
  app=$(canonical_candidate_directory "$1") || return 1
  [[ "$(basename "$app")" == *.app && -f "$app/Contents/Info.plist" && ! -L "$app/Contents/Info.plist" ]] || return 1
  printf '%s\n' "$app"
}

candidate_module_directory() {
  local app=$1 requested=$2 expected module
  expected="$app/Contents/Extensions/$FSKIT_MODULE_BUNDLE_NAME"
  module=$(canonical_candidate_directory "$requested") || return 1
  [[ "$module" == "$expected" && -f "$module/Contents/Info.plist" && ! -L "$module/Contents/Info.plist" ]] || return 1
  printf '%s\n' "$module"
}

candidate_app_identity_sha() {
  printf '%s\n' \
    "$VALIDATED_CANDIDATE_APP" \
    "$VALIDATED_CANDIDATE_APP_DIRECTORY_IDENTITY" \
    "$VALIDATED_CANDIDATE_APP_BUNDLE_IDENTIFIER" \
    "$VALIDATED_CANDIDATE_APP_SHORT_VERSION" \
    "$VALIDATED_CANDIDATE_APP_BUNDLE_VERSION" \
    "$VALIDATED_CANDIDATE_APP_EXECUTABLE" \
    "$VALIDATED_CANDIDATE_APP_EXECUTABLE_SHA" \
    "$VALIDATED_CANDIDATE_APP_CDHASH" \
    "$VALIDATED_CANDIDATE_APP_TEAM" \
    "$VALIDATED_CANDIDATE_MODULE" \
    "$VALIDATED_CANDIDATE_MODULE_DIRECTORY_IDENTITY" \
    "$VALIDATED_CANDIDATE_MODULE_BUNDLE_IDENTIFIER" \
    "$VALIDATED_CANDIDATE_MODULE_SHORT_VERSION" \
    "$VALIDATED_CANDIDATE_MODULE_BUNDLE_VERSION" \
    "$VALIDATED_CANDIDATE_MODULE_EXECUTABLE" \
    "$VALIDATED_CANDIDATE_MODULE_EXECUTABLE_SHA" \
    "$VALIDATED_CANDIDATE_MODULE_CDHASH" \
    "$VALIDATED_CANDIDATE_MODULE_TEAM" \
    "$VALIDATED_CANDIDATE_MODULE_SHORT_NAME" | text_sha
}

registered_fskit_module_output_contains() {
  local output=$1 target=$2
  printf '%s\n' "$output" | awk -F '\t' -v target="$target" '
    {
      candidate=$NF
      sub(/^[[:space:]]+/, "", candidate)
      sub(/[[:space:]]+$/, "", candidate)
      if (candidate == target) found=1
    }
    END { exit(found ? 0 : 1) }
  '
}

candidate_module_registered() {
  local target=$1 output
  output=$(/usr/bin/pluginkit -m -A -D -v -i "$FSKIT_MODULE_BUNDLE_IDENTIFIER" 2>/dev/null) || return 1
  registered_fskit_module_output_contains "$output" "$target"
}

list_fskit_module_process_ids() {
  local uid
  uid=$(id -u) || return 1
  /bin/ps -axo uid=,pid=,comm= | awk -v uid="$uid" -v name="$FSKIT_MODULE_PROCESS_NAME" '
    {
      row=$0
      if ($1 != uid || $2 !~ /^[0-9]+$/) next
      pid=$2
      sub(/^[[:space:]]*[0-9]+[[:space:]]+[0-9]+[[:space:]]+/, "", row)
      sub(/^[[:space:]]+/, "", row)
      sub(/[[:space:]]+$/, "", row)
      count=split(row, components, "/")
      if (components[count] == name) print pid
    }
  '
}

write_fskit_module_process_snapshot() {
  local output=$1 temporary pid ppid start executable command_sha count=0
  temporary=$(mktemp "$(dirname "$output")/.fskit-module-processes.XXXXXX") || return 1
  while IFS= read -r pid; do
    [[ -n "$pid" ]] || continue
    [[ "$pid" =~ ^[0-9]+$ && "$pid" -gt 1 ]] || { rm -f "$temporary"; return 1; }
    count=$((count + 1))
    (( count <= 32 )) || { rm -f "$temporary"; return 1; }
    ppid=$(ps -p "$pid" -o ppid= 2>/dev/null | trim_ps_value)
    start=$(process_start "$pid")
    executable=$(process_executable_path "$pid")
    command_sha=$(process_command_sha "$pid")
    [[ "$ppid" =~ ^[0-9]+$ && -n "$start" && -n "$executable" && "$command_sha" =~ ^[0-9a-f]{64}$ ]] || {
      rm -f "$temporary"
      return 1
    }
    printf '%s\t%s\t%s\t%s\t%s\n' "$pid" "$ppid" "$start" "$executable" "$command_sha" >> "$temporary"
  done < <(list_fskit_module_process_ids)
  chmod 600 "$temporary"
  mv "$temporary" "$output"
}

write_new_fskit_module_process_snapshot() {
  local baseline=$1 current=$2 output=$3 temporary row
  [[ -f "$baseline" && ! -L "$baseline" && -f "$current" && ! -L "$current" ]] || return 1
  temporary=$(mktemp "$(dirname "$output")/.new-fskit-module-processes.XXXXXX") || return 1
  while IFS= read -r row; do
    [[ -n "$row" ]] || continue
    grep -Fqx -- "$row" "$current" || { rm -f "$temporary"; return 1; }
  done < "$baseline"
  while IFS= read -r row; do
    [[ -n "$row" ]] || continue
    if ! grep -Fqx -- "$row" "$baseline"; then
      printf '%s\n' "$row" >> "$temporary"
    fi
  done < "$current"
  chmod 600 "$temporary"
  mv "$temporary" "$output"
}

list_relevant_process_ids() {
  local app=$1
  local desktop codex_resource process_file
  desktop=$(bundle_executable_path "$app")
  codex_resource="$app/Contents/Resources/codex"
  process_file=$(mktemp "${TMPDIR:-/tmp}/codexfold-processes.XXXXXX")
  ps -axww -o pid=,ppid=,command= > "$process_file"
  awk -v desktop="$desktop" -v codex="$codex_resource" '
    index($0, desktop) || (index($0, codex) && index($0, "app-server")) {
      print $1 "\t" $2
    }
  ' "$process_file"
  rm -f "$process_file"
}

trim_ps_value() {
  sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}

process_start() {
	TZ=UTC /bin/ps -p "$1" -o lstart= 2>/dev/null | trim_ps_value
}

process_command() {
  ps -ww -p "$1" -o command= 2>/dev/null || true
}

process_command_with_environment() {
  ps -Eww -p "$1" -o command= 2>/dev/null || true
}

process_command_sha() {
  local command_line
  command_line=$(process_command "$1")
  [[ -n "$command_line" ]] || return 1
  printf '%s' "$command_line" | text_sha
}

process_executable_path() {
  local value parent
  value=$(ps -ww -p "$1" -o comm= 2>/dev/null | trim_ps_value)
  [[ -n "$value" && -e "$value" ]] || return 1
  parent=$(cd "$(dirname "$value")" 2>/dev/null && pwd -P) || return 1
	printf '%s/%s\n' "$parent" "$(basename "$value")"
}

computer_use_runtime_executable() {
	case "$1" in
		*/Contents/Resources/cua_node/bin/node|*/Contents/Resources/cua_node/bin/node_repl) return 0 ;;
		*) return 1 ;;
	esac
}

process_has_computer_use_ancestor() {
	local current=$1 depth=0 executable
	while (( current > 1 && depth < 32 )); do
		current=$(ps -p "$current" -o ppid= 2>/dev/null | trim_ps_value)
		[[ "$current" =~ ^[0-9]+$ ]] || return 1
		executable=$(process_executable_path "$current" 2>/dev/null || true)
		computer_use_runtime_executable "$executable" && return 0
		depth=$((depth + 1))
	done
	return 1
}

process_environment_has_exact() {
  local pid=$1 key=$2 value=$3 command_line
  command_line=$(process_command_with_environment "$pid")
  [[ -n "$command_line" ]] || return 1
  case "$command_line" in
    *" $key=$value "*|*" $key=$value") return 0 ;;
    *) return 1 ;;
  esac
}

process_has_exact_user_data_dir() {
  local pid=$1 value=$2 command_line
  command_line=$(process_command "$pid")
  case "$command_line" in
    *" --user-data-dir=$value "*|*" --user-data-dir=$value"|*" --user-data-dir $value "*|*" --user-data-dir $value") return 0 ;;
    *) return 1 ;;
  esac
}

process_role() {
  local pid=$1
  local command_line executable desktop codex_resource protected_desktop protected_codex_resource
  command_line=$(process_command "$pid")
  executable=$(process_executable_path "$pid" 2>/dev/null || true)
  desktop=$(bundle_executable_path "$APP_PATH")
  codex_resource="$APP_PATH/Contents/Resources/codex"
  protected_desktop=$(bundle_executable_path "$PROTECTED_APP_PATH")
  protected_codex_resource="$PROTECTED_APP_PATH/Contents/Resources/codex"
  if [[ "$executable" == "$desktop" || "$executable" == "$protected_desktop" ]]; then
    printf 'desktop\n'
  elif [[ "$executable" == "$codex_resource" || "$executable" == "$protected_codex_resource" ]] && \
       [[ " $command_line " == *" app-server "* ]]; then
    printf 'app-server\n'
  else
    printf 'unknown\n'
  fi
}

write_baseline_snapshot() {
  local output=$1
  local pid ppid start role executable command_sha executable_sha
  : > "$output"
  while IFS=$'\t' read -r pid ppid; do
    [[ -n "$pid" ]] || continue
    start=$(process_start "$pid")
    [[ -n "$start" ]] || continue
		role=$(process_role "$pid")
		[[ "$role" != unknown ]] || continue
		if [[ "$role" == app-server ]] && process_has_computer_use_ancestor "$pid"; then
			continue
		fi
		executable=$(process_executable_path "$pid") || continue
    executable_sha=$(printf '%s' "$executable" | text_sha)
    command_sha=$(process_command_sha "$pid") || continue
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$pid" "$ppid" "$role" "$start" "$executable_sha" "$command_sha" >> "$output"
  done < <(list_relevant_process_ids "$PROTECTED_APP_PATH")
  chmod 600 "$output"
}

write_current_snapshot() {
  local output=$1
  local pid ppid start role isolated home_bound data_bound executable command_sha executable_sha
  : > "$output"
  while IFS=$'\t' read -r pid ppid; do
    [[ -n "$pid" ]] || continue
    start=$(process_start "$pid")
    [[ -n "$start" ]] || continue
		role=$(process_role "$pid")
		if [[ "$role" == app-server ]] && process_has_computer_use_ancestor "$pid"; then
			continue
		fi
		home_bound=false
    data_bound=false
    if process_environment_has_exact "$pid" CODEX_HOME "$CODEX_HOME_ISOLATED"; then
      home_bound=true
    fi
    if process_has_exact_user_data_dir "$pid" "$ELECTRON_DATA"; then
      data_bound=true
    fi
    isolated=false
    if [[ "$home_bound" == true || "$data_bound" == true ]]; then
      isolated=true
    fi
    executable=$(process_executable_path "$pid" 2>/dev/null || true)
    executable_sha=$(printf '%s' "$executable" | text_sha)
    command_sha=$(process_command_sha "$pid" 2>/dev/null || true)
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$pid" "$ppid" "$role" "$isolated" "$home_bound" "$data_bound" "$start" "$executable_sha" "$command_sha" >> "$output"
  done < <(
    {
      list_relevant_process_ids "$APP_PATH"
      if [[ "$PROTECTED_APP_PATH" != "$APP_PATH" ]]; then
        list_relevant_process_ids "$PROTECTED_APP_PATH"
      fi
    } | awk -F '\t' '!seen[$1]++'
  )
  chmod 600 "$output"
}

baseline_contains() {
  local baseline=$1
  local pid=$2
  local start=$3
  awk -F '\t' -v pid="$pid" -v start="$start" '$1 == pid && $4 == start { found=1 } END { exit(found ? 0 : 1) }' "$baseline"
}

ancestor_reaches() {
  local child=$1
  local expected=$2
  local current depth
  current=$child
  depth=0
  while (( current > 1 && depth < 32 )); do
    if [[ "$current" == "$expected" ]]; then
      return 0
    fi
    current=$(ps -p "$current" -o ppid= 2>/dev/null | trim_ps_value)
    [[ "$current" =~ ^[0-9]+$ ]] || return 1
    depth=$((depth + 1))
  done
  return 1
}

verify_protected_processes() {
  local baseline=$1
  local current=$2
  local pid ppid role start expected_executable_sha expected_command_sha current_start isolated home_bound data_bound current_executable current_executable_sha current_command_sha
  PROTECTED_UNCHANGED=true
  NEW_UNBOUND_PROCESSES=0

  while IFS=$'\t' read -r pid ppid role start expected_executable_sha expected_command_sha; do
    [[ -n "$pid" ]] || continue
    current_start=$(process_start "$pid")
    if [[ "$current_start" != "$start" ]]; then
      PROTECTED_UNCHANGED=false
      continue
    fi
    current_executable=$(process_executable_path "$pid" 2>/dev/null || true)
    current_executable_sha=$(printf '%s' "$current_executable" | text_sha)
    current_command_sha=$(process_command_sha "$pid" 2>/dev/null || true)
    [[ "$current_executable_sha" == "$expected_executable_sha" && "$current_command_sha" == "$expected_command_sha" ]] || PROTECTED_UNCHANGED=false
  done < "$baseline"

  while IFS=$'\t' read -r pid ppid role isolated home_bound data_bound start _executable_sha _command_sha; do
    [[ -n "$pid" ]] || continue
    if [[ "$isolated" == false ]] && ! baseline_contains "$baseline" "$pid" "$start"; then
      NEW_UNBOUND_PROCESSES=$((NEW_UNBOUND_PROCESSES + 1))
      PROTECTED_UNCHANGED=false
    fi
  done < "$current"
}

verify_slice() {
  local selection="$CODEX_HOME_ISOLATED/selected-sessions.tsv"
  local db="$CODEX_HOME_ISOLATED/state_5.sqlite"
  local expected=0 actual=0 id archived relative expected_bytes sha row_identity path actual_sha actual_bytes db_path db_archived row_id row_path
  SLICE_VALID=true
  if [[ -f "$selection" ]]; then
    expected=$(wc -l < "$selection" | tr -d ' ')
  fi
  if (( expected > 0 )); then
    if [[ ! -f "$db" ]]; then
      SLICE_VALID=false
      SLICE_COUNT=$expected
      CURRENT_THREAD_COUNT=0
      return
    fi
    if [[ "$SLICE_VALID" == true ]]; then
      actual=$(sqlite3 "$db" 'SELECT count(*) FROM threads;' 2>/dev/null || printf '%s' -1)
      if [[ ! "$actual" =~ ^[0-9]+$ ]] || (( actual < expected )); then
        SLICE_VALID=false
      fi
      [[ "$(sqlite3 "$db" 'PRAGMA integrity_check;' 2>/dev/null || true)" == ok ]] || SLICE_VALID=false
      if [[ "$(sqlite3 "$db" "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='remote_control_enrollments';")" == 1 ]]; then
        [[ "$(sqlite3 "$db" 'SELECT count(*) FROM remote_control_enrollments;' 2>/dev/null || true)" == 0 ]] || SLICE_VALID=false
      fi
    fi
    while IFS=$'\t' read -r id archived relative expected_bytes sha row_identity; do
      [[ -n "$id" ]] || continue
      [[ "$id" =~ ^[0-9A-Za-z_-]+$ && "$archived" =~ ^[01]$ && "$expected_bytes" =~ ^[0-9]+$ && "$sha" =~ ^[0-9a-f]{64}$ && "$row_identity" =~ ^[0-9a-f]{64}$ ]] || {
        SLICE_VALID=false
        continue
      }
      db_path=$(sqlite3 "$db" "SELECT rollout_path FROM threads WHERE id='${id//\'/\'\'}';" 2>/dev/null || true)
      db_archived=$(sqlite3 "$db" "SELECT archived FROM threads WHERE id='${id//\'/\'\'}';" 2>/dev/null || true)
      path=$db_path
      if [[ ! -f "$path" ]]; then
        SLICE_VALID=false
        continue
      fi
      case "$path" in
        "$CODEX_HOME_ISOLATED/sessions/"*|"$CODEX_HOME_ISOLATED/archived_sessions/"*) ;;
        *) SLICE_VALID=false; continue ;;
      esac
      actual_sha=$(shasum -a 256 "$path" | awk '{print $1}')
      actual_bytes=$(stat -f '%z' "$path")
      [[ "$actual_sha" == "$sha" && "$actual_bytes" == "$expected_bytes" && "$db_archived" == "$archived" ]] || SLICE_VALID=false
      [[ "$path" == "$CODEX_HOME_ISOLATED/$relative" ]] || SLICE_VALID=false
    done < "$selection"

    while IFS=$'\t' read -r row_id row_path; do
      [[ -n "$row_id" ]] || continue
      case "$row_path" in
        "$CODEX_HOME_ISOLATED/sessions/"*|"$CODEX_HOME_ISOLATED/archived_sessions/"*) ;;
        *) SLICE_VALID=false; continue ;;
      esac
      [[ -f "$row_path" ]] || SLICE_VALID=false
    done < <(sqlite3 -batch -noheader -separator $'\t' "$db" 'SELECT id, rollout_path FROM threads;' 2>/dev/null || true)
  fi
  SLICE_COUNT=$expected
  CURRENT_THREAD_COUNT=$actual
}

snapshot_rollout_identities() {
  local output=$1
  local db="$CODEX_HOME_ISOLATED/state_5.sqlite"
  local _attempt rows_before rows_after snapshot_tmp id path sha bytes identity_before identity_after bytes_after valid
  [[ -f "$db" && ! -L "$db" ]] || return 1
  for _attempt in 1 2 3; do
    rows_before=$(mktemp "${TMPDIR:-/tmp}/codexfold-rollout-rows-before.XXXXXX")
    rows_after=$(mktemp "${TMPDIR:-/tmp}/codexfold-rollout-rows-after.XXXXXX")
    snapshot_tmp=$(mktemp "${TMPDIR:-/tmp}/codexfold-rollout-snapshot.XXXXXX")
    valid=true
    if ! sqlite3 -batch -json "$db" 'SELECT id, rollout_path FROM threads ORDER BY id;' > "$rows_before" || \
       [[ "$(sqlite3 -batch -noheader "$db" 'PRAGMA integrity_check;' 2>/dev/null || true)" != ok ]] || \
       ! jq -e 'type == "array" and all(.[]; (.id | type == "string" and test("^[^\\t\\r\\n]+$")) and (.rollout_path | type == "string" and test("^/[^\\t\\r\\n]+$")))' "$rows_before" >/dev/null; then
      valid=false
    fi
    if [[ "$valid" == true ]]; then
      while IFS=$'\t' read -r id path; do
        [[ -n "$id" && -n "$path" ]] || { valid=false; break; }
        case "$path" in
          "$CODEX_HOME_ISOLATED/sessions/"*|"$CODEX_HOME_ISOLATED/archived_sessions/"*) ;;
          *) valid=false; break ;;
        esac
        [[ -f "$path" && ! -L "$path" ]] || { valid=false; break; }
        identity_before=$(stat -f '%d:%i:%z:%m:%c:%p' "$path") || { valid=false; break; }
        bytes=$(stat -f '%z' "$path") || { valid=false; break; }
        sha=$(file_sha "$path") || { valid=false; break; }
        identity_after=$(stat -f '%d:%i:%z:%m:%c:%p' "$path") || { valid=false; break; }
        bytes_after=$(stat -f '%z' "$path") || { valid=false; break; }
        [[ "$identity_before" == "$identity_after" && "$bytes" == "$bytes_after" ]] || { valid=false; break; }
        printf '%s\t%s\t%s\n' "$id" "$bytes" "$sha" >> "$snapshot_tmp"
      done < <(jq -r '.[] | [.id,.rollout_path] | @tsv' "$rows_before")
    fi
    if [[ "$valid" == true ]]; then
      sqlite3 -batch -json "$db" 'SELECT id, rollout_path FROM threads ORDER BY id;' > "$rows_after" || valid=false
      cmp -s "$rows_before" "$rows_after" || valid=false
    fi
    if [[ "$valid" == true ]]; then
      chmod 600 "$snapshot_tmp"
      mv "$snapshot_tmp" "$output"
      rm -f "$rows_before" "$rows_after"
      return 0
    fi
    rm -f "$rows_before" "$rows_after" "$snapshot_tmp"
    sleep 1
  done
  return 1
}

canonical_candidate_file() {
  local requested=$1
  local parent canonical candidate
  [[ -f "$requested" && ! -L "$requested" ]] || return 1
  parent=$(cd "$(dirname "$requested")" 2>/dev/null && pwd -P) || return 1
  canonical="$parent/$(basename "$requested")"
  candidate=$(cd "$CANDIDATE_ROOT" 2>/dev/null && pwd -P) || return 1
  [[ "$canonical" == "$candidate/"* ]] || return 1
  printf '%s\n' "$canonical"
}

canonical_candidate_directory() {
  local requested=$1
  local canonical candidate
  [[ -d "$requested" && ! -L "$requested" ]] || return 1
  canonical=$(cd "$requested" 2>/dev/null && pwd -P) || return 1
  candidate=$(cd "$CANDIDATE_ROOT" 2>/dev/null && pwd -P) || return 1
  [[ "$canonical" == "$candidate/"* ]] || return 1
  printf '%s\n' "$canonical"
}

real_mount_identity() {
  local requested=$1 canonical mount_line stat_identity
  canonical=$(canonical_candidate_directory "$requested") || return 1
  mount_line=$(/sbin/mount 2>/dev/null | awk -v target="$canonical" 'index($0, " on " target " (") { print; exit }')
  [[ -n "$mount_line" ]] || return 1
  stat_identity=$(stat -f '%d:%i:%T' "$canonical") || return 1
  printf '%s\n%s' "$stat_identity" "$mount_line" | text_sha
}

canonical_candidate_status_file() {
  canonical_candidate_file "$1"
}

canonical_candidate_node() {
  local requested=$1 parent canonical candidate
  [[ ! -L "$requested" ]] || return 1
  [[ -e "$requested" || -S "$requested" ]] || return 1
  parent=$(cd "$(dirname "$requested")" 2>/dev/null && pwd -P) || return 1
  canonical="$parent/$(basename "$requested")"
  candidate=$(cd "$CANDIDATE_ROOT" 2>/dev/null && pwd -P) || return 1
  [[ "$canonical" == "$candidate/"* ]] || return 1
  printf '%s\n' "$canonical"
}

canonical_candidate_leaf_path() {
  local requested=$1 parent canonical candidate
  parent=$(cd "$(dirname "$requested")" 2>/dev/null && pwd -P) || return 1
  canonical="$parent/$(basename "$requested")"
  candidate=$(cd "$CANDIDATE_ROOT" 2>/dev/null && pwd -P) || return 1
  [[ "$canonical" == "$candidate/"* ]] || return 1
  printf '%s\n' "$canonical"
}

canonical_regular_file() {
  local requested=$1 parent
  [[ -f "$requested" && ! -L "$requested" ]] || return 1
  parent=$(cd "$(dirname "$requested")" 2>/dev/null && pwd -P) || return 1
  printf '%s/%s\n' "$parent" "$(basename "$requested")"
}

canonical_evidence_file() {
  local requested=$1 canonical evidence
  canonical=$(canonical_regular_file "$requested") || return 1
  evidence=$(cd "$EVIDENCE_ROOT" 2>/dev/null && pwd -P) || return 1
  [[ "$canonical" == "$evidence/"* ]] || return 1
  printf '%s\n' "$canonical"
}

current_source_provenance_matches() {
  local current_head current_snapshot
  [[ "$SOURCE_PROVENANCE_REPO" == "$SOURCE_REPO_ROOT" && \
     "$SOURCE_PROVENANCE_REPO_IDENTITY" == "$(directory_identity "$SOURCE_REPO_ROOT")" ]] || return 1
  current_head=$(source_git_head "$SOURCE_REPO_ROOT")
  [[ "$current_head" == "$SOURCE_PROVENANCE_HEAD" ]] || return 1
  current_snapshot=$(source_snapshot_sha "$SOURCE_REPO_ROOT") || return 1
  [[ "$current_snapshot" == "$SOURCE_PROVENANCE_SNAPSHOT_SHA" ]]
}

snapshot_preserves_protected_baseline() {
  local baseline=$1 snapshot=$2
  awk -F '\t' '
    NR == FNR {
      expected[$1]=$3 FS $4 FS $5 FS $6
      count++
      next
    }
    $4 == "false" {
      observed[$1]=$3 FS $7 FS $8 FS $9
    }
    END {
      if (count == 0) exit 0
      for (pid in expected) {
        if (!(pid in observed) || observed[pid] != expected[pid]) exit 1
      }
    }
  ' "$baseline" "$snapshot"
}

validate_protected_snapshot_file() {
  local requested=$1 expected_sha=$2 snapshot
  snapshot=$(canonical_evidence_file "$requested") || return 1
  [[ "$expected_sha" =~ ^[0-9a-f]{64}$ && "$(file_sha "$snapshot")" == "$expected_sha" ]] || return 1
  snapshot_preserves_protected_baseline "$PROTECTED_PROCESS_BASELINE" "$snapshot" || return 1
  verify_protected_processes "$PROTECTED_PROCESS_BASELINE" "$snapshot"
  [[ "$PROTECTED_UNCHANGED" == true && "$NEW_UNBOUND_PROCESSES" == 0 ]]
}

isolated_fence_from_snapshot() {
  local snapshot=$1 desktop_pid desktop_start app_server_pid app_server_start
  desktop_pid=$(awk -F '\t' '$3=="desktop" && $4=="true" && $6=="true" {print $1; exit}' "$snapshot")
  desktop_start=$(awk -F '\t' '$3=="desktop" && $4=="true" && $6=="true" {print $7; exit}' "$snapshot")
  app_server_pid=$(awk -F '\t' '$3=="app-server" && $4=="true" && $5=="true" {print $1; exit}' "$snapshot")
  app_server_start=$(awk -F '\t' '$3=="app-server" && $4=="true" && $5=="true" {print $7; exit}' "$snapshot")
  [[ "$desktop_pid" =~ ^[0-9]+$ && "$app_server_pid" =~ ^[0-9]+$ && -n "$desktop_start" && -n "$app_server_start" ]] || return 1
  ancestor_reaches "$app_server_pid" "$desktop_pid" || return 1
  printf '%s\t%s\t%s\t%s\n' "$desktop_pid" "$desktop_start" "$app_server_pid" "$app_server_start"
}

snapshot_matches_isolated_fence() {
  local snapshot=$1 desktop_pid=$2 desktop_start=$3 app_server_pid=$4 app_server_start=$5
  awk -F '\t' -v desktopPID="$desktop_pid" -v desktopStart="$desktop_start" \
    -v appServerPID="$app_server_pid" -v appServerStart="$app_server_start" '
      $1 == desktopPID && $3 == "desktop" && $4 == "true" && $6 == "true" && $7 == desktopStart { desktop=1 }
      $1 == appServerPID && $3 == "app-server" && $4 == "true" && $5 == "true" && $7 == appServerStart { appServer=1 }
      END { exit(desktop && appServer ? 0 : 1) }
    ' "$snapshot"
}

runtime_epoch_advanced() {
  local before_pid=$1 before_start=$2 after_pid=$3 after_start=$4
  [[ "$before_pid" =~ ^[0-9]+$ && "$after_pid" =~ ^[0-9]+$ && "$before_pid" != "$after_pid" && \
     -n "$before_start" && -n "$after_start" && "$before_start" != "$after_start" ]]
}

candidate_validation_fail() {
  CANDIDATE_VALIDATION_ERROR=$1
  return 1
}

validate_candidate_app_and_module() {
  local evidence=$1 mode=${2:-external}
  local app module app_executable module_executable
  local app_identifier app_short_version app_bundle_version app_directory_identity app_executable_sha app_cdhash app_team app_signing_identifier
  local module_identifier module_short_version module_bundle_version module_directory_identity module_executable_sha module_cdhash module_team module_signing_identifier module_short_name

  CANDIDATE_VALIDATION_ERROR=''
  jq -e '
    (.candidateApp | type == "object") and
    (.candidateApp.path | type == "string" and startswith("/") and test("^[^\\t\\r\\n]+$")) and
    (.candidateApp.directoryIdentity | type == "string" and test("^[0-9]+:[0-9]+$")) and
    (.candidateApp.bundleIdentifier | type == "string" and length > 0) and
    (.candidateApp.shortVersion | type == "string" and length > 0) and
    (.candidateApp.bundleVersion | type == "string" and length > 0) and
    (.candidateApp.executablePath | type == "string" and startswith("/") and test("^[^\\t\\r\\n]+$")) and
    (.candidateApp.executableSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
    (.candidateApp.codeDirectoryHash | type == "string" and test("^([0-9a-f]{40}|[0-9a-f]{64})$")) and
    (.candidateApp.teamIdentifier | type == "string" and test("^[A-Z0-9]{10}$")) and
    (.fskitModule | type == "object") and
    (.fskitModule.bundlePath | type == "string" and startswith("/") and test("^[^\\t\\r\\n]+$")) and
    (.fskitModule.directoryIdentity | type == "string" and test("^[0-9]+:[0-9]+$")) and
    (.fskitModule.bundleIdentifier | type == "string" and length > 0) and
    (.fskitModule.shortVersion | type == "string" and length > 0) and
    (.fskitModule.bundleVersion | type == "string" and length > 0) and
    (.fskitModule.fsShortName | type == "string" and length > 0) and
    (.fskitModule.executablePath | type == "string" and startswith("/") and test("^[^\\t\\r\\n]+$")) and
    (.fskitModule.executableSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
    (.fskitModule.codeDirectoryHash | type == "string" and test("^([0-9a-f]{40}|[0-9a-f]{64})$")) and
    (.fskitModule.teamIdentifier | type == "string" and test("^[A-Z0-9]{10}$")) and
    (.fskitModule.process | type == "object") and
    (.fskitModule.process.pid | type == "number" and floor == . and . > 1) and
    (.fskitModule.process.ppid | type == "number" and floor == . and . >= 0) and
    (.fskitModule.process.processStart | type == "string" and length > 0) and
    (.fskitModule.process.executablePath | type == "string" and startswith("/") and test("^[^\\t\\r\\n]+$")) and
    (.fskitModule.process.executableSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
    (.fskitModule.process.commandSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
    (.fskitModule.process.baselineSHA256 | type == "string" and test("^[0-9a-f]{64}$"))
  ' "$evidence" >/dev/null || candidate_validation_fail "candidate App/module evidence fields are incomplete or malformed" || return 1

  app=$(candidate_app_directory "$(jq -r '.candidateApp.path' "$evidence")") || \
    candidate_validation_fail "candidate App is not a real app bundle inside candidateRoot" || return 1
  [[ "$app" == "$(jq -r '.candidateApp.path' "$evidence")" ]] || \
    candidate_validation_fail "candidate App path is not canonical" || return 1
  module=$(candidate_module_directory "$app" "$(jq -r '.fskitModule.bundlePath' "$evidence")") || \
    candidate_validation_fail "FSKit module bundle is not the exact module nested in candidate App" || return 1
  [[ "$module" == "$(jq -r '.fskitModule.bundlePath' "$evidence")" ]] || \
    candidate_validation_fail "FSKit module bundle path is not canonical" || return 1
  app_executable=$(candidate_bundle_executable_path "$app") || \
    candidate_validation_fail "candidate App executable is missing, unsafe, or outside candidateRoot" || return 1
  module_executable=$(candidate_bundle_executable_path "$module") || \
    candidate_validation_fail "candidate FSKit module executable is missing, unsafe, or outside candidateRoot" || return 1
  [[ "$app_executable" == "$(jq -r '.candidateApp.executablePath' "$evidence")" ]] || \
    candidate_validation_fail "candidate App executable path does not match its signed bundle" || return 1
  [[ "$module_executable" == "$(jq -r '.fskitModule.executablePath' "$evidence")" ]] || \
    candidate_validation_fail "candidate FSKit module executable path does not match its signed bundle" || return 1

  app_identifier=$(bundle_info_value "$app" CFBundleIdentifier)
  app_short_version=$(bundle_info_value "$app" CFBundleShortVersionString)
  app_bundle_version=$(bundle_info_value "$app" CFBundleVersion)
  module_identifier=$(bundle_info_value "$module" CFBundleIdentifier)
  module_short_version=$(bundle_info_value "$module" CFBundleShortVersionString)
  module_bundle_version=$(bundle_info_value "$module" CFBundleVersion)
  module_short_name=$(plutil -extract EXAppExtensionAttributes.FSShortName raw -o - "$module/Contents/Info.plist" 2>/dev/null || true)
  [[ "$app_identifier" == "$FSKIT_APP_BUNDLE_IDENTIFIER" && "$module_identifier" == "$FSKIT_MODULE_BUNDLE_IDENTIFIER" && "$module_short_name" == "$FSKIT_SHORT_NAME" ]] || \
    candidate_validation_fail "candidate App/module bundle identity or FS short name is not CodexFold" || return 1
  [[ -n "$app_short_version" && -n "$app_bundle_version" && -n "$module_short_version" && -n "$module_bundle_version" ]] || \
    candidate_validation_fail "candidate App/module version metadata is missing" || return 1
  [[ "$app_identifier" == "$(jq -r '.candidateApp.bundleIdentifier' "$evidence")" && \
     "$app_short_version" == "$(jq -r '.candidateApp.shortVersion' "$evidence")" && \
     "$app_bundle_version" == "$(jq -r '.candidateApp.bundleVersion' "$evidence")" && \
     "$module_identifier" == "$(jq -r '.fskitModule.bundleIdentifier' "$evidence")" && \
     "$module_short_version" == "$(jq -r '.fskitModule.shortVersion' "$evidence")" && \
     "$module_bundle_version" == "$(jq -r '.fskitModule.bundleVersion' "$evidence")" && \
     "$module_short_name" == "$(jq -r '.fskitModule.fsShortName' "$evidence")" ]] || \
    candidate_validation_fail "candidate App/module bundle or version metadata changed" || return 1

  /usr/bin/codesign --verify --deep --strict "$app" >/dev/null 2>&1 || \
    candidate_validation_fail "candidate App nested signature verification failed" || return 1
  /usr/bin/codesign --verify --strict "$module" >/dev/null 2>&1 || \
    candidate_validation_fail "candidate FSKit module signature verification failed" || return 1
  app_cdhash=$(codesign_detail_value "$app" CDHash) || candidate_validation_fail "candidate App code-directory hash is unavailable" || return 1
  app_team=$(codesign_detail_value "$app" TeamIdentifier) || candidate_validation_fail "candidate App signing team is unavailable" || return 1
  app_signing_identifier=$(codesign_detail_value "$app" Identifier) || candidate_validation_fail "candidate App signing identifier is unavailable" || return 1
  module_cdhash=$(codesign_detail_value "$module" CDHash) || candidate_validation_fail "candidate FSKit module code-directory hash is unavailable" || return 1
  module_team=$(codesign_detail_value "$module" TeamIdentifier) || candidate_validation_fail "candidate FSKit module signing team is unavailable" || return 1
  module_signing_identifier=$(codesign_detail_value "$module" Identifier) || candidate_validation_fail "candidate FSKit module signing identifier is unavailable" || return 1
  [[ "$app_signing_identifier" == "$app_identifier" && "$module_signing_identifier" == "$module_identifier" && \
     "$app_team" =~ ^[A-Z0-9]{10}$ && "$module_team" == "$app_team" ]] || \
    candidate_validation_fail "candidate App/module signature identity is inconsistent" || return 1

  app_directory_identity=$(directory_identity "$app") || candidate_validation_fail "candidate App directory identity is unavailable" || return 1
  module_directory_identity=$(directory_identity "$module") || candidate_validation_fail "candidate FSKit module directory identity is unavailable" || return 1
  app_executable_sha=$(file_sha "$app_executable") || candidate_validation_fail "candidate App executable hash is unavailable" || return 1
  module_executable_sha=$(file_sha "$module_executable") || candidate_validation_fail "candidate FSKit module executable hash is unavailable" || return 1
  [[ "$app_directory_identity" == "$(jq -r '.candidateApp.directoryIdentity' "$evidence")" && \
     "$app_executable_sha" == "$(jq -r '.candidateApp.executableSHA256' "$evidence")" && \
     "$app_cdhash" == "$(jq -r '.candidateApp.codeDirectoryHash' "$evidence")" && \
     "$app_team" == "$(jq -r '.candidateApp.teamIdentifier' "$evidence")" && \
     "$module_directory_identity" == "$(jq -r '.fskitModule.directoryIdentity' "$evidence")" && \
     "$module_executable_sha" == "$(jq -r '.fskitModule.executableSHA256' "$evidence")" && \
     "$module_cdhash" == "$(jq -r '.fskitModule.codeDirectoryHash' "$evidence")" && \
     "$module_team" == "$(jq -r '.fskitModule.teamIdentifier' "$evidence")" ]] || \
    candidate_validation_fail "candidate App/module path, executable, or signature identity changed" || return 1

  candidate_module_registered "$module" || \
    candidate_validation_fail "the exact candidate FSKit module path is not registered; an old or missing registration is not candidate evidence" || return 1

  VALIDATED_CANDIDATE_APP=$app
  VALIDATED_CANDIDATE_APP_DIRECTORY_IDENTITY=$app_directory_identity
  VALIDATED_CANDIDATE_APP_BUNDLE_IDENTIFIER=$app_identifier
  VALIDATED_CANDIDATE_APP_SHORT_VERSION=$app_short_version
  VALIDATED_CANDIDATE_APP_BUNDLE_VERSION=$app_bundle_version
  VALIDATED_CANDIDATE_APP_EXECUTABLE=$app_executable
  VALIDATED_CANDIDATE_APP_EXECUTABLE_SHA=$app_executable_sha
  VALIDATED_CANDIDATE_APP_CDHASH=$app_cdhash
  VALIDATED_CANDIDATE_APP_TEAM=$app_team
  VALIDATED_CANDIDATE_MODULE=$module
  VALIDATED_CANDIDATE_MODULE_DIRECTORY_IDENTITY=$module_directory_identity
  VALIDATED_CANDIDATE_MODULE_BUNDLE_IDENTIFIER=$module_identifier
  VALIDATED_CANDIDATE_MODULE_SHORT_VERSION=$module_short_version
  VALIDATED_CANDIDATE_MODULE_BUNDLE_VERSION=$module_bundle_version
  VALIDATED_CANDIDATE_MODULE_EXECUTABLE=$module_executable
  VALIDATED_CANDIDATE_MODULE_EXECUTABLE_SHA=$module_executable_sha
  VALIDATED_CANDIDATE_MODULE_CDHASH=$module_cdhash
  VALIDATED_CANDIDATE_MODULE_TEAM=$module_team
  VALIDATED_CANDIDATE_MODULE_SHORT_NAME=$module_short_name
  VALIDATED_CANDIDATE_APP_IDENTITY=$(candidate_app_identity_sha) || \
    candidate_validation_fail "candidate App identity digest could not be computed" || return 1
  if [[ "$mode" == retained ]]; then
    [[ "$(jq -r '.candidateApp.identitySHA256 // empty' "$evidence")" == "$VALIDATED_CANDIDATE_APP_IDENTITY" ]] || \
      candidate_validation_fail "retained candidate App identity digest changed" || return 1
  fi
}

validate_candidate_module_process() {
  local evidence=$1 mode=${2:-external} retained_snapshot=${3:-}
  local snapshot temporary new_snapshot count pid ppid start executable executable_sha command_sha snapshot_sha
  [[ "$(file_sha "$FSKIT_MODULE_BASELINE")" == "$FSKIT_MODULE_BASELINE_SHA" ]] || \
    candidate_validation_fail "FSKit module pre-mount process baseline changed" || return 1
  [[ "$(jq -r '.fskitModule.process.baselineSHA256' "$evidence")" == "$FSKIT_MODULE_BASELINE_SHA" ]] || \
    candidate_validation_fail "candidate module process does not bind the prepared pre-mount baseline" || return 1

  if [[ -n "$retained_snapshot" ]]; then
    snapshot=$retained_snapshot
  else
    temporary=$(mktemp "${TMPDIR:-/tmp}/codexfold-fskit-processes.XXXXXX") || return 1
    snapshot=$temporary
  fi
  if ! write_fskit_module_process_snapshot "$snapshot"; then
    [[ -z "$temporary" ]] || rm -f "$temporary"
    candidate_validation_fail "could not obtain a bounded FSKit module process snapshot" || return 1
  fi
  new_snapshot=$(mktemp "${TMPDIR:-/tmp}/codexfold-new-fskit-processes.XXXXXX") || {
    [[ -z "$temporary" ]] || rm -f "$temporary"
    return 1
  }
  if ! write_new_fskit_module_process_snapshot "$FSKIT_MODULE_BASELINE" "$snapshot" "$new_snapshot"; then
    [[ -z "$temporary" ]] || rm -f "$temporary"
    rm -f "$new_snapshot"
    candidate_validation_fail "a pre-existing CodexFold FSKit module process changed or exited during candidate attachment" || return 1
  fi
  count=$(wc -l < "$new_snapshot" | tr -d ' ')
  if [[ "$count" != 1 ]]; then
    [[ -z "$temporary" ]] || rm -f "$temporary"
    rm -f "$new_snapshot"
    candidate_validation_fail "candidate mount requires exactly one newly observed CodexFold FSKit module process; found $count" || return 1
  fi
  IFS=$'\t' read -r pid ppid start executable command_sha < "$new_snapshot"
  executable_sha=$(file_sha "$executable") || {
    [[ -z "$temporary" ]] || rm -f "$temporary"
    rm -f "$new_snapshot"
    candidate_validation_fail "the live FSKit module executable hash is unavailable" || return 1
  }
  snapshot_sha=$(file_sha "$snapshot")
  if [[ "$pid" != "$(jq -r '.fskitModule.process.pid' "$evidence")" || \
        "$ppid" != "$(jq -r '.fskitModule.process.ppid' "$evidence")" || \
        "$start" != "$(jq -r '.fskitModule.process.processStart' "$evidence")" || \
        "$executable" != "$VALIDATED_CANDIDATE_MODULE_EXECUTABLE" || \
        "$executable" != "$(jq -r '.fskitModule.process.executablePath' "$evidence")" || \
        "$executable_sha" != "$VALIDATED_CANDIDATE_MODULE_EXECUTABLE_SHA" || \
        "$executable_sha" != "$(jq -r '.fskitModule.process.executableSHA256' "$evidence")" || \
        "$command_sha" != "$(jq -r '.fskitModule.process.commandSHA256' "$evidence")" ]]; then
    [[ -z "$temporary" ]] || rm -f "$temporary"
    rm -f "$new_snapshot"
    candidate_validation_fail "the live FSKit module process is not the exact signed candidate module" || return 1
  fi
  if [[ "$mode" == retained && "$(jq -r '.fskitModule.process.snapshotSHA256 // empty' "$evidence")" != "$snapshot_sha" ]]; then
    [[ -z "$temporary" ]] || rm -f "$temporary"
    rm -f "$new_snapshot"
    candidate_validation_fail "retained FSKit module post-mount process snapshot changed" || return 1
  fi
  VALIDATED_CANDIDATE_MODULE_SNAPSHOT_SHA=$snapshot_sha
  rm -f "$new_snapshot"
  [[ -z "$temporary" ]] || rm -f "$temporary"
}

validate_candidate_app_module_process() {
  local evidence=$1 mode=${2:-external} retained_snapshot=${3:-}
  validate_candidate_app_and_module "$evidence" "$mode" || return 1
  validate_candidate_module_process "$evidence" "$mode" "$retained_snapshot"
}

validate_candidate_source_and_build_manifest() {
  local evidence=$1
  local manifest manifest_sha current_snapshot current_head
  jq -e '
    (.sourceProvenance | type == "object") and
    (.sourceProvenance.repoRoot | type == "string" and startswith("/")) and
    (.sourceProvenance.repoRootIdentity | type == "string" and test("^[0-9]+:[0-9]+$")) and
    ((.sourceProvenance.gitHead == null) or (.sourceProvenance.gitHead | type == "string" and test("^[0-9a-f]{40,64}$"))) and
    (.sourceProvenance.snapshotSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
    (.buildManifest | type == "object") and
    (.buildManifest.path | type == "string" and startswith("/") and test("^[^\\t\\r\\n]+$")) and
    (.buildManifest.sha256 | type == "string" and test("^[0-9a-f]{64}$"))
  ' "$evidence" >/dev/null || candidate_validation_fail "candidate source provenance or build-manifest fields are incomplete" || return 1
  [[ "$(jq -r '.sourceProvenance.repoRoot' "$evidence")" == "$SOURCE_PROVENANCE_REPO" && \
     "$(jq -r '.sourceProvenance.repoRootIdentity' "$evidence")" == "$SOURCE_PROVENANCE_REPO_IDENTITY" && \
     "$(jq -r '.sourceProvenance.gitHead // empty' "$evidence")" == "$SOURCE_PROVENANCE_HEAD" && \
     "$(jq -r '.sourceProvenance.snapshotSHA256' "$evidence")" == "$SOURCE_PROVENANCE_SNAPSHOT_SHA" ]] || \
    candidate_validation_fail "candidate source provenance does not match the prepared worktree snapshot" || return 1
  current_head=$(source_git_head "$SOURCE_PROVENANCE_REPO")
  current_snapshot=$(source_snapshot_sha "$SOURCE_PROVENANCE_REPO") || candidate_validation_fail "current source snapshot could not be recomputed" || return 1
  [[ "$current_head" == "$SOURCE_PROVENANCE_HEAD" && "$current_snapshot" == "$SOURCE_PROVENANCE_SNAPSHOT_SHA" ]] || \
    candidate_validation_fail "the worktree changed after prepare; create a fresh run and rebuild the candidate" || return 1

  manifest=$(canonical_candidate_file "$(jq -r '.buildManifest.path' "$evidence")") || \
    candidate_validation_fail "candidate build manifest is not a regular file inside candidateRoot" || return 1
  [[ "$manifest" == "$(jq -r '.buildManifest.path' "$evidence")" && "$(stat -f '%z' "$manifest")" -le 1048576 ]] || \
    candidate_validation_fail "candidate build manifest path or size is invalid" || return 1
  manifest_sha=$(file_sha "$manifest") || candidate_validation_fail "candidate build manifest hash is unavailable" || return 1
  [[ "$manifest_sha" == "$(jq -r '.buildManifest.sha256' "$evidence")" ]] || \
    candidate_validation_fail "candidate build manifest bytes changed" || return 1
  jq -e \
    --arg repo "$SOURCE_PROVENANCE_REPO" \
    --arg repoIdentity "$SOURCE_PROVENANCE_REPO_IDENTITY" \
    --arg head "$SOURCE_PROVENANCE_HEAD" \
    --arg sourceSHA "$SOURCE_PROVENANCE_SNAPSHOT_SHA" \
    --arg binary "$VALIDATED_CANDIDATE_BINARY" \
    --arg binarySHA "$VALIDATED_CANDIDATE_SHA" \
    --arg app "$VALIDATED_CANDIDATE_APP" \
    --arg appIdentity "$VALIDATED_CANDIDATE_APP_IDENTITY" \
    --arg module "$VALIDATED_CANDIDATE_MODULE" \
    --arg moduleExecutable "$VALIDATED_CANDIDATE_MODULE_EXECUTABLE" \
    --arg moduleSHA "$VALIDATED_CANDIDATE_MODULE_EXECUTABLE_SHA" \
    --arg moduleCDHash "$VALIDATED_CANDIDATE_MODULE_CDHASH" '
      (.schema == "codexfold.candidate-build-manifest.v1") and
      (.createdAt | type == "string" and length > 0) and
      (.sourceProvenance.repoRoot == $repo) and
      (.sourceProvenance.repoRootIdentity == $repoIdentity) and
      ((if $head == "" then .sourceProvenance.gitHead == null else .sourceProvenance.gitHead == $head end)) and
      (.sourceProvenance.snapshotSHA256 == $sourceSHA) and
      (.artifacts.candidateBinary.path == $binary) and
      (.artifacts.candidateBinary.sha256 == $binarySHA) and
      (.artifacts.candidateApp.path == $app) and
      (.artifacts.candidateApp.identitySHA256 == $appIdentity) and
      (.artifacts.fskitModule.bundlePath == $module) and
      (.artifacts.fskitModule.executablePath == $moduleExecutable) and
      (.artifacts.fskitModule.executableSHA256 == $moduleSHA) and
      (.artifacts.fskitModule.codeDirectoryHash == $moduleCDHash)
    ' "$manifest" >/dev/null || candidate_validation_fail "candidate build manifest does not bind the prepared source snapshot to the live artifacts" || return 1
  VALIDATED_CANDIDATE_BUILD_MANIFEST_SHA=$manifest_sha
}

candidate_process_identity() {
  local pid=$1 expected_executable=$2 expected_executable_sha=$3 expected_start=$4 expected_command_sha=$5
  local start executable executable_sha command_sha
  start=$(process_start "$pid")
  [[ -n "$start" && "$start" == "$expected_start" ]] || return 1
  executable=$(process_executable_path "$pid" 2>/dev/null || true)
  [[ "$executable" == "$expected_executable" ]] || return 1
  executable_sha=$(file_sha "$executable") || return 1
  [[ "$executable_sha" == "$expected_executable_sha" ]] || return 1
  command_sha=$(process_command_sha "$pid" 2>/dev/null || true)
  [[ -n "$command_sha" && "$command_sha" == "$expected_command_sha" ]] || return 1
}

candidate_status_json() {
  local binary=$1 definition=$2 mount=$3 output
  output=$(CODEX_HOME="$CODEX_HOME_ISOLATED" "$binary" fs service status --json \
    --codex-home "$CODEX_HOME_ISOLATED" --mount "$mount" --definition "$definition") || return 1
  [[ "$(printf '%s' "$output" | wc -c | tr -d ' ')" -le 1048576 ]] || return 1
  jq -e . >/dev/null <<< "$output" || return 1
  printf '%s\n' "$output"
}

stable_json_file() {
  local requested=$1 path identity_before identity_after sha_before sha_after size
  path=$(canonical_regular_file "$requested") || return 1
  size=$(stat -f '%z' "$path") || return 1
  [[ "$size" =~ ^[0-9]+$ && "$size" -le 1048576 ]] || return 1
  identity_before=$(stat -f '%d:%i:%z:%m:%c:%p' "$path") || return 1
  sha_before=$(file_sha "$path") || return 1
  jq -e . "$path" >/dev/null || return 1
  identity_after=$(stat -f '%d:%i:%z:%m:%c:%p' "$path") || return 1
  sha_after=$(file_sha "$path") || return 1
  [[ "$identity_before" == "$identity_after" && "$sha_before" == "$sha_after" ]] || return 1
  printf '%s\n' "$path"
}

copy_stable_artifact() {
  local source=$1 destination=$2 maximum_bytes=$3
  local source_path destination_parent evidence identity_before identity_after sha_before sha_after temporary size
  source_path=$(canonical_regular_file "$source") || return 1
  size=$(stat -f '%z' "$source_path") || return 1
  [[ "$size" =~ ^[0-9]+$ && "$size" -le "$maximum_bytes" ]] || return 1
  destination_parent=$(cd "$(dirname "$destination")" 2>/dev/null && pwd -P) || return 1
  evidence=$(cd "$EVIDENCE_ROOT" 2>/dev/null && pwd -P) || return 1
  [[ "$destination_parent/$(basename "$destination")" == "$evidence/"* && ! -e "$destination" ]] || return 1
  identity_before=$(stat -f '%d:%i:%z:%m:%c:%p' "$source_path") || return 1
  sha_before=$(file_sha "$source_path") || return 1
  temporary=$(mktemp "$destination_parent/.artifact.XXXXXX") || return 1
  cp -p "$source_path" "$temporary" || { rm -f "$temporary"; return 1; }
  chmod 600 "$temporary"
  identity_after=$(stat -f '%d:%i:%z:%m:%c:%p' "$source_path") || { rm -f "$temporary"; return 1; }
  sha_after=$(file_sha "$source_path") || { rm -f "$temporary"; return 1; }
  [[ "$identity_before" == "$identity_after" && "$sha_before" == "$sha_after" && "$(file_sha "$temporary")" == "$sha_before" ]] || {
    rm -f "$temporary"
    return 1
  }
  mv "$temporary" "$destination"
  printf '%s\n' "$sha_before"
}

write_json_evidence_text() {
  local destination=$1 content=$2 temporary
  [[ ! -e "$destination" ]] || return 1
  jq -e . >/dev/null <<< "$content" || return 1
  temporary=$(mktemp "$(dirname "$destination")/.json.XXXXXX") || return 1
  printf '%s\n' "$content" > "$temporary"
  chmod 600 "$temporary"
  mv "$temporary" "$destination"
}

validate_backend_status_file() {
  local requested=$1 expected_pid=$2 expected_mount=$3 expected_backend_id=${4:-}
  local path backend_id
  path=$(stable_json_file "$requested") || return 1
  jq -e \
    --argjson pid "$expected_pid" \
    --arg mount "$expected_mount" '
      (.schemaVersion == 2) and
      (.component == "daemon") and
      (.state == "healthy") and
      (.pid == $pid) and
      (.mountPoint == $mount) and
      (.backendID | type == "string" and length > 0 and test("^[^\\t\\r\\n]+$"))
    ' "$path" >/dev/null || return 1
  backend_id=$(jq -r '.backendID' "$path")
  [[ -z "$expected_backend_id" || "$backend_id" == "$expected_backend_id" ]] || return 1
  VALIDATED_BACKEND_STATUS_PATH=$path
  VALIDATED_BACKEND_ID=$backend_id
}

load_candidate_fault_targets() {
  local evidence=$1 kind path
  VALIDATED_BACKEND_PID_FILE=''
  VALIDATED_BACKEND_STATUS_PATH=''
  VALIDATED_SOCKET_TARGET=''
  VALIDATED_DESCRIPTOR_TARGET=''
  for kind in backendPidFile backendStatus socket descriptor; do
    path=$(jq -r --arg kind "$kind" '.faultTargets[$kind] // empty' "$evidence")
    [[ -n "$path" ]] || continue
    case "$kind" in
      backendPidFile) path=$(canonical_candidate_file "$path") || return 1; VALIDATED_BACKEND_PID_FILE=$path ;;
      backendStatus) path=$(canonical_regular_file "$path") || return 1; VALIDATED_BACKEND_STATUS_PATH=$path ;;
      socket) path=$(canonical_candidate_node "$path") || return 1; [[ -S "$path" ]] || return 1; VALIDATED_SOCKET_TARGET=$path ;;
      descriptor) path=$(canonical_candidate_file "$path") || return 1; VALIDATED_DESCRIPTOR_TARGET=$path ;;
    esac
  done
}

validate_candidate_anchor() {
  local evidence=$1 binary definition mount mount_identity backend_status backend_id
  validate_candidate_app_module_process "$evidence" retained || return 1
  binary=$(jq -r '.candidateBinaryPath' "$evidence")
  binary=$(canonical_candidate_file "$binary") || return 1
  [[ -x "$binary" ]] || return 1
  [[ "$(file_sha "$binary")" == "$(jq -r '.candidateBuildSHA' "$evidence")" ]] || return 1
  VALIDATED_CANDIDATE_BINARY=$binary
  VALIDATED_CANDIDATE_SHA=$(jq -r '.candidateBuildSHA' "$evidence")
  validate_candidate_source_and_build_manifest "$evidence" || return 1
  definition=$(jq -r '.serviceDefinitionPath' "$evidence")
  definition=$(canonical_candidate_status_file "$definition") || return 1
  [[ "$(file_sha "$definition")" == "$(jq -r '.serviceDefinitionSHA256' "$evidence")" ]] || return 1
  mount=$(jq -r '.mountPoint' "$evidence")
  mount=$(canonical_candidate_directory "$mount") || return 1
  mount_identity=$(real_mount_identity "$mount") || return 1
  [[ "$mount_identity" == "$(jq -r '.mountIdentity' "$evidence")" ]] || return 1
  load_candidate_fault_targets "$evidence" || return 1
  [[ -n "$VALIDATED_BACKEND_PID_FILE" && -n "$VALIDATED_BACKEND_STATUS_PATH" ]] || return 1
  backend_status=$(canonical_regular_file "$(jq -r '.backendStatus.path' "$evidence")") || return 1
  [[ "$backend_status" == "$VALIDATED_BACKEND_STATUS_PATH" ]] || return 1
  backend_id=$(jq -r '.backendStatus.backendID' "$evidence")
  [[ -n "$backend_id" && "$backend_id" != null ]] || return 1
  VALIDATED_BACKEND_ID=$backend_id
  VALIDATED_CANDIDATE_MOUNT=$mount
  VALIDATED_CANDIDATE_MOUNT_IDENTITY=$mount_identity
  VALIDATED_CANDIDATE_DEFINITION=$definition
  VALIDATED_CANDIDATE_DEFINITION_SHA=$(jq -r '.serviceDefinitionSHA256' "$evidence")
}

load_expected_candidate_runtime() {
  local evidence=$1 crash="$EVIDENCE_ROOT/candidate-backend-crash-respawn.json"
  EXPECTED_CANDIDATE_PID=$(jq -r '.daemonPid' "$evidence")
  EXPECTED_CANDIDATE_PROCESS_START=$(jq -r '.daemonProcessStart' "$evidence")
  EXPECTED_CANDIDATE_COMMAND_SHA=$(jq -r '.daemonCommandSHA256' "$evidence")
  EXPECTED_CANDIDATE_DAEMON_EXECUTABLE=$(jq -r '.daemonExecutablePath' "$evidence")
  EXPECTED_CANDIDATE_DAEMON_EXECUTABLE_SHA=$(jq -r '.daemonExecutableSHA256' "$evidence")
  CANDIDATE_RUNTIME_EPOCH=initial
  if [[ -e "$crash" ]]; then
    validate_backend_crash_respawn_evidence "$evidence" current || return 1
    EXPECTED_CANDIDATE_PID=$(jq -r '.after.pid' "$crash")
    EXPECTED_CANDIDATE_PROCESS_START=$(jq -r '.after.processStart' "$crash")
    EXPECTED_CANDIDATE_COMMAND_SHA=$(jq -r '.after.commandSHA256' "$crash")
    EXPECTED_CANDIDATE_DAEMON_EXECUTABLE=$(jq -r '.after.executablePath' "$crash")
    EXPECTED_CANDIDATE_DAEMON_EXECUTABLE_SHA=$(jq -r '.after.executableSHA256' "$crash")
    CANDIDATE_RUNTIME_EPOCH=backend-crash-respawn
  fi
}

validate_candidate_runtime() {
  local evidence=$1 status status_pid command_line daemon_executable
  load_expected_candidate_runtime "$evidence" || return 1
  status=$(candidate_status_json "$VALIDATED_CANDIDATE_BINARY" "$VALIDATED_CANDIDATE_DEFINITION" "$VALIDATED_CANDIDATE_MOUNT") || return 1
  jq -e \
    --arg sha "$VALIDATED_CANDIDATE_SHA" \
    --arg binary "$VALIDATED_CANDIDATE_BINARY" '
      (.daemon_running == true) and
      (.daemon_pid | type == "number" and floor == . and . > 1) and
      (.mount_healthy == true) and
      (.build.healthy == true) and
      (.build.running_build_sha256 == $sha) and
      (.build.configured_build_sha256 == $sha) and
      (.build.configured_binary_path == $binary)
    ' <<< "$status" >/dev/null || return 1
  status_pid=$(jq -r '.daemon_pid' <<< "$status")
  [[ "$EXPECTED_CANDIDATE_PID" == "$status_pid" ]] || return 1
	daemon_executable=$EXPECTED_CANDIDATE_DAEMON_EXECUTABLE
	daemon_executable=$(canonical_candidate_file "$daemon_executable") || return 1
	[[ "$daemon_executable" == "$VALIDATED_CANDIDATE_BINARY" ]] || return 1
	candidate_process_identity "$EXPECTED_CANDIDATE_PID" "$daemon_executable" "$EXPECTED_CANDIDATE_DAEMON_EXECUTABLE_SHA" \
		"$EXPECTED_CANDIDATE_PROCESS_START" "$EXPECTED_CANDIDATE_COMMAND_SHA" || return 1
	command_line=$(process_command "$EXPECTED_CANDIDATE_PID")
	[[ "$command_line" == *"$CODEX_HOME_ISOLATED"* && "$command_line" == *"$VALIDATED_CANDIDATE_MOUNT"* ]] || return 1
  [[ "$(tr -d '[:space:]' < "$VALIDATED_BACKEND_PID_FILE")" == "$EXPECTED_CANDIDATE_PID" ]] || return 1
  validate_backend_status_file "$VALIDATED_BACKEND_STATUS_PATH" "$EXPECTED_CANDIDATE_PID" "$VALIDATED_CANDIDATE_MOUNT" "$VALIDATED_BACKEND_ID" || return 1
  VALIDATED_CANDIDATE_PID=$EXPECTED_CANDIDATE_PID
  VALIDATED_CANDIDATE_PROCESS_START=$EXPECTED_CANDIDATE_PROCESS_START
  VALIDATED_CANDIDATE_COMMAND_SHA=$EXPECTED_CANDIDATE_COMMAND_SHA
  VALIDATED_CANDIDATE_DAEMON_EXECUTABLE=$daemon_executable
  VALIDATED_CANDIDATE_DAEMON_EXECUTABLE_SHA=$EXPECTED_CANDIDATE_DAEMON_EXECUTABLE_SHA
}

validate_live_candidate() {
  local evidence=$1
  validate_candidate_anchor "$evidence" || return 1
  validate_candidate_runtime "$evidence"
}

validate_candidate_routes() {
  local evidence=$1
  local db="$CODEX_HOME_ISOLATED/state_5.sqlite"
  local id path bytes sha db_path actual_bytes actual_sha route_parent resolved_path expected_mount_root
  VALIDATED_ROUTE_COUNT=0
  [[ -f "$db" ]] || return 1
  [[ -n "${VALIDATED_CANDIDATE_MOUNT:-}" ]] || return 1
  [[ "$(readlink "$CODEX_HOME_ISOLATED/sessions" 2>/dev/null || true)" == "$VALIDATED_CANDIDATE_MOUNT/sessions" ]] || return 1
  [[ "$(readlink "$CODEX_HOME_ISOLATED/archived_sessions" 2>/dev/null || true)" == "$VALIDATED_CANDIDATE_MOUNT/archived_sessions" ]] || return 1
  jq -e '
    (.managedRoutes | type == "array" and length > 0) and
    ([.managedRoutes[].sessionId] | length == (unique | length)) and
    all(.managedRoutes[];
      (.sessionId | type == "string" and test("^[^\\t\\r\\n]+$")) and
      (.rolloutPath | type == "string" and test("^/[^\\t\\r\\n]+$")) and
      (.bytes | type == "number" and floor == . and . >= 0) and
      (.sha256 | type == "string" and test("^[0-9a-f]{64}$"))
    )
  ' "$evidence" >/dev/null || return 1

  while IFS=$'\t' read -r id path bytes sha; do
    [[ -n "$id" && -n "$path" ]] || return 1
    case "$path" in
      "$CODEX_HOME_ISOLATED/sessions/"*) expected_mount_root="$VALIDATED_CANDIDATE_MOUNT/sessions" ;;
      "$CODEX_HOME_ISOLATED/archived_sessions/"*) expected_mount_root="$VALIDATED_CANDIDATE_MOUNT/archived_sessions" ;;
      *) return 1 ;;
    esac
    [[ -f "$path" && ! -L "$path" ]] || return 1
    route_parent=$(cd "$(dirname "$path")" 2>/dev/null && pwd -P) || return 1
    resolved_path="$route_parent/$(basename "$path")"
    [[ "$resolved_path" == "$expected_mount_root/"* ]] || return 1
    db_path=$(sqlite3 -batch -noheader "$db" "SELECT rollout_path FROM threads WHERE id='${id//\'/\'\'}';" 2>/dev/null) || return 1
    [[ -n "$db_path" && "$db_path" == "$path" ]] || return 1
    actual_bytes=$(stat -f '%z' "$path") || return 1
    [[ "$actual_bytes" == "$bytes" ]] || return 1
    actual_sha=$(shasum -a 256 "$path" | awk '{print $1}') || return 1
    [[ "$actual_sha" == "$sha" ]] || return 1
    VALIDATED_ROUTE_COUNT=$((VALIDATED_ROUTE_COUNT + 1))
  done < <(jq -r '.managedRoutes[] | [.sessionId,.rolloutPath,(.bytes|tostring),.sha256] | @tsv' "$evidence")
  (( VALIDATED_ROUTE_COUNT > 0 ))
}

validate_external_candidate_input() {
  local input=$1 retained_module_snapshot=${2:-}
  local input_binary configured_binary command_line status status_pid daemon_executable daemon_executable_sha
  [[ -f "$input" && ! -L "$input" ]] || return 1
  [[ "$(stat -f '%z' "$input")" -le 1048576 ]] || return 1
  jq -e \
    --arg schema "$CANDIDATE_INPUT_SCHEMA" \
    --arg home "$CODEX_HOME_ISOLATED" \
    --arg candidate "$CANDIDATE_ROOT" '
      (.schema == $schema) and
      (.observedAt | type == "string" and length > 0) and
      (.codexHome == $home) and
      (.candidateRoot == $candidate) and
      (.mountPoint | type == "string" and startswith("/")) and
      (.candidateAttached == true) and
      (.managedRouteObserved == true) and
      (.candidateBinaryPath | type == "string" and startswith("/")) and
      (.serviceDefinitionPath | type == "string" and startswith("/")) and
      (.candidateBuildSHA | type == "string" and test("^[0-9a-f]{64}$")) and
      (.serviceStatus.daemon_running == true) and
      (.serviceStatus.daemon_pid | type == "number" and floor == . and . > 1) and
      (.serviceStatus.mount_healthy == true) and
      (.serviceStatus.build.healthy == true) and
      (.serviceStatus.build.running_build_sha256 == .candidateBuildSHA) and
      (.serviceStatus.build.configured_build_sha256 == .candidateBuildSHA) and
      (.serviceStatus.build.configured_binary_path == .candidateBinaryPath) and
      ((.faultTargets // {}) | type == "object") and
      (.faultTargets.backendPidFile | type == "string" and startswith("/")) and
      (.faultTargets.backendStatus | type == "string" and startswith("/")) and
      all((.faultTargets // {})[]; type == "string" and startswith("/") and test("^[^\\t\\r\\n]+$"))
    ' "$input" >/dev/null || return 1

  validate_candidate_app_module_process "$input" external "$retained_module_snapshot" || return 1

  input_binary=$(jq -r '.candidateBinaryPath' "$input")
  VALIDATED_CANDIDATE_MOUNT=$(canonical_candidate_directory "$(jq -r '.mountPoint' "$input")") || return 1
  [[ "$VALIDATED_CANDIDATE_MOUNT" == "$(jq -r '.mountPoint' "$input")" ]] || return 1
  VALIDATED_CANDIDATE_MOUNT_IDENTITY=$(real_mount_identity "$VALIDATED_CANDIDATE_MOUNT") || return 1
  VALIDATED_CANDIDATE_BINARY=$(canonical_candidate_file "$input_binary") || return 1
  [[ "$VALIDATED_CANDIDATE_BINARY" == "$input_binary" && -x "$VALIDATED_CANDIDATE_BINARY" ]] || return 1
  VALIDATED_CANDIDATE_SHA=$(shasum -a 256 "$VALIDATED_CANDIDATE_BINARY" | awk '{print $1}') || return 1
  [[ "$VALIDATED_CANDIDATE_SHA" == "$(jq -r '.candidateBuildSHA' "$input")" ]] || return 1
  validate_candidate_source_and_build_manifest "$input" || return 1
  configured_binary=$(jq -r '.serviceStatus.build.configured_binary_path' "$input")
  [[ "$configured_binary" == "$VALIDATED_CANDIDATE_BINARY" ]] || return 1
  VALIDATED_CANDIDATE_DEFINITION=$(canonical_candidate_status_file "$(jq -r '.serviceDefinitionPath' "$input")") || return 1
  [[ "$VALIDATED_CANDIDATE_DEFINITION" == "$(jq -r '.serviceDefinitionPath' "$input")" ]] || return 1
  VALIDATED_CANDIDATE_DEFINITION_SHA=$(file_sha "$VALIDATED_CANDIDATE_DEFINITION") || return 1
  status=$(candidate_status_json "$VALIDATED_CANDIDATE_BINARY" "$VALIDATED_CANDIDATE_DEFINITION" "$VALIDATED_CANDIDATE_MOUNT") || return 1
  jq -e --arg sha "$VALIDATED_CANDIDATE_SHA" --arg binary "$VALIDATED_CANDIDATE_BINARY" '
    (.daemon_running == true) and
    (.daemon_pid | type == "number" and floor == . and . > 1) and
    (.mount_healthy == true) and
    (.build.healthy == true) and
    (.build.running_build_sha256 == $sha) and
    (.build.configured_build_sha256 == $sha) and
    (.build.configured_binary_path == $binary)
  ' <<< "$status" >/dev/null || return 1
  status_pid=$(jq -r '.daemon_pid' <<< "$status")
  VALIDATED_CANDIDATE_PID=$(jq -r '.serviceStatus.daemon_pid' "$input")
  [[ "$VALIDATED_CANDIDATE_PID" == "$status_pid" ]] || return 1
	VALIDATED_CANDIDATE_PROCESS_START=$(process_start "$VALIDATED_CANDIDATE_PID")
	[[ -n "$VALIDATED_CANDIDATE_PROCESS_START" ]] || return 1
	command_line=$(process_command "$VALIDATED_CANDIDATE_PID")
	[[ "$command_line" == *"$CODEX_HOME_ISOLATED"* && "$command_line" == *"$VALIDATED_CANDIDATE_MOUNT"* ]] || return 1
	VALIDATED_CANDIDATE_COMMAND_SHA=$(process_command_sha "$VALIDATED_CANDIDATE_PID") || return 1
	daemon_executable=$(process_executable_path "$VALIDATED_CANDIDATE_PID") || return 1
	daemon_executable=$(canonical_candidate_file "$daemon_executable") || return 1
	[[ "$daemon_executable" == "$VALIDATED_CANDIDATE_BINARY" ]] || return 1
  daemon_executable_sha=$(file_sha "$daemon_executable") || return 1
  VALIDATED_CANDIDATE_DAEMON_EXECUTABLE=$daemon_executable
  VALIDATED_CANDIDATE_DAEMON_EXECUTABLE_SHA=$daemon_executable_sha
  load_candidate_fault_targets "$input" || return 1
  [[ -n "$VALIDATED_BACKEND_PID_FILE" && -n "$VALIDATED_BACKEND_STATUS_PATH" ]] || return 1
  [[ "$(tr -d '[:space:]' < "$VALIDATED_BACKEND_PID_FILE")" == "$VALIDATED_CANDIDATE_PID" ]] || return 1
  validate_backend_status_file "$VALIDATED_BACKEND_STATUS_PATH" "$VALIDATED_CANDIDATE_PID" "$VALIDATED_CANDIDATE_MOUNT" || return 1
  [[ -z "$VALIDATED_SOCKET_TARGET" || -S "$VALIDATED_SOCKET_TARGET" ]] || return 1
  [[ -z "$VALIDATED_DESCRIPTOR_TARGET" || -f "$VALIDATED_DESCRIPTOR_TARGET" ]] || return 1
  validate_candidate_routes "$input"
}

candidate_task_route_matches() {
  local before="$EVIDENCE_ROOT/real-task.before.tsv"
  local current="$EVIDENCE_ROOT/real-task.current.tsv"
  local id bytes sha db_path route_parent expected_mount_root
  CANDIDATE_TASK_ROUTE_MATCHES=0
  [[ -f "$before" && -f "$current" ]] || return 0
  while IFS=$'\t' read -r id bytes sha; do
    if awk -F '\t' -v id="$id" -v bytes="$bytes" -v sha="$sha" \
         '$1 == id && $2 == bytes && $3 == sha { found=1 } END { exit(found ? 0 : 1) }' "$before"; then
      continue
    fi
    db_path=$(sqlite3 -batch -noheader "$CODEX_HOME_ISOLATED/state_5.sqlite" "SELECT rollout_path FROM threads WHERE id='${id//\'/\'\'}';" 2>/dev/null || true)
    case "$db_path" in
      "$CODEX_HOME_ISOLATED/sessions/"*) expected_mount_root="$VALIDATED_CANDIDATE_MOUNT/sessions" ;;
      "$CODEX_HOME_ISOLATED/archived_sessions/"*) expected_mount_root="$VALIDATED_CANDIDATE_MOUNT/archived_sessions" ;;
      *) continue ;;
    esac
    [[ -f "$db_path" && ! -L "$db_path" ]] || continue
    route_parent=$(cd "$(dirname "$db_path")" 2>/dev/null && pwd -P) || continue
    [[ "$route_parent/$(basename "$db_path")" == "$expected_mount_root/"* ]] || continue
    [[ "$(stat -f '%z' "$db_path")" == "$bytes" && "$(file_sha "$db_path")" == "$sha" ]] || continue
    CANDIDATE_TASK_ROUTE_MATCHES=$((CANDIDATE_TASK_ROUTE_MATCHES + 1))
  done < "$current"
}

acceptance_binding_common_matches() {
  local artifact=$1 candidate_evidence="$EVIDENCE_ROOT/codexfold-candidate-observed.json"
  [[ -f "$candidate_evidence" && ! -L "$candidate_evidence" ]] || return 1
  [[ "$(file_sha "$candidate_evidence")" == "$RUN_CANDIDATE_EVIDENCE_SHA" ]] || return 1
  current_source_provenance_matches || return 1
  jq -e \
    --arg runID "$COCKPIT_INSTANCE_ID" \
    --arg candidateEvidenceSHA "$RUN_CANDIDATE_EVIDENCE_SHA" \
    --arg buildSHA "$RUN_CANDIDATE_BUILD_SHA" \
    --arg appIdentity "$RUN_CANDIDATE_APP_IDENTITY" \
    --arg moduleSHA "$RUN_CANDIDATE_MODULE_SHA" \
    --arg repoIdentity "$SOURCE_PROVENANCE_REPO_IDENTITY" \
    --arg head "$SOURCE_PROVENANCE_HEAD" \
    --arg sourceSHA "$SOURCE_PROVENANCE_SNAPSHOT_SHA" \
    --arg protectedBaselineSHA "$PROTECTED_PROCESS_BASELINE_SHA" '
      (.binding.runID == $runID) and
      (.binding.candidateEvidenceSHA256 == $candidateEvidenceSHA) and
      (.binding.candidateBuildSHA256 == $buildSHA) and
      (.binding.candidateAppIdentitySHA256 == $appIdentity) and
      (.binding.candidateFSKitModuleSHA256 == $moduleSHA) and
      (.binding.sourceProvenance.repoRootIdentity == $repoIdentity) and
      ((if $head == "" then .binding.sourceProvenance.gitHead == null else .binding.sourceProvenance.gitHead == $head end)) and
      (.binding.sourceProvenance.snapshotSHA256 == $sourceSHA) and
      (.binding.protectedCodexFence.baselineSHA256 == $protectedBaselineSHA) and
      (.binding.protectedCodexFence.unchanged == true) and
      (.binding.protectedCodexFence.newUnboundProcesses == 0)
    ' "$artifact" >/dev/null
}

validate_service_status_evidence_file() {
  local requested=$1 expected_sha=$2 expected_pid=$3 expected_exact=$4 path
  path=$(canonical_evidence_file "$requested") || return 1
  [[ "$path" == "$expected_exact" && "$(file_sha "$path")" == "$expected_sha" ]] || return 1
  jq -e \
    --argjson pid "$expected_pid" \
    --arg build "$VALIDATED_CANDIDATE_SHA" \
    --arg binary "$VALIDATED_CANDIDATE_BINARY" '
      (.daemon_running == true) and
      (.daemon_pid == $pid) and
      (.mount_healthy == true) and
      (.build.healthy == true) and
      (.build.running_build_sha256 == $build) and
      (.build.configured_build_sha256 == $build) and
      (.build.configured_binary_path == $binary)
    ' "$path" >/dev/null
}

validate_backend_status_evidence_file() {
  local requested=$1 expected_sha=$2 expected_pid=$3 expected_exact=$4 path
  path=$(canonical_evidence_file "$requested") || return 1
  [[ "$path" == "$expected_exact" && "$(file_sha "$path")" == "$expected_sha" ]] || return 1
  jq -e \
    --argjson pid "$expected_pid" \
    --arg mount "$VALIDATED_CANDIDATE_MOUNT" \
    --arg backendID "$VALIDATED_BACKEND_ID" '
      (.schemaVersion == 2) and
      (.component == "daemon") and
      (.state == "healthy") and
      (.pid == $pid) and
      (.mountPoint == $mount) and
      (.backendID == $backendID)
    ' "$path" >/dev/null
}

validate_backend_crash_respawn_evidence() {
  local candidate_evidence=$1 mode=${2:-current}
  local crash="$EVIDENCE_ROOT/candidate-backend-crash-respawn.json" crash_sha
  local before_process during_process after_process desktop_pid desktop_start app_server_pid app_server_start
  local before_pid after_pid before_start after_start after_executable after_executable_sha after_command_sha
  BACKEND_CRASH_RESPAWN_EVIDENCE_VALID=false
  [[ -f "$crash" && ! -L "$crash" && "$(stat -f '%z' "$crash")" -le 1048576 ]] || return 1
  crash_sha=$(file_sha "$crash") || return 1
  [[ "$RUN_BACKEND_CRASH_EVIDENCE_SHA" =~ ^[0-9a-f]{64}$ && "$crash_sha" == "$RUN_BACKEND_CRASH_EVIDENCE_SHA" ]] || return 1
  acceptance_binding_common_matches "$crash" || return 1
  jq -e \
    --arg schema "$BACKEND_CRASH_EVIDENCE_SCHEMA" \
    --arg pidFile "$VALIDATED_BACKEND_PID_FILE" \
    --arg initialPID "$(jq -r '.daemonPid' "$candidate_evidence")" \
    --arg initialStart "$(jq -r '.daemonProcessStart' "$candidate_evidence")" \
		--arg executable "$VALIDATED_CANDIDATE_BINARY" \
		--arg executableSHA "$VALIDATED_CANDIDATE_SHA" \
    --arg commandSHA "$(jq -r '.daemonCommandSHA256' "$candidate_evidence")" \
    --arg backendID "$VALIDATED_BACKEND_ID" \
    --arg buildSHA "$VALIDATED_CANDIDATE_SHA" \
    --arg definitionSHA "$VALIDATED_CANDIDATE_DEFINITION_SHA" \
    --arg mountIdentity "$VALIDATED_CANDIDATE_MOUNT_IDENTITY" '
      (.schema == $schema) and
      (.recordedAt | type == "string" and length > 0) and
      (.fault.kind == "backend-crash") and
      (.fault.signal == "SIGKILL") and
      (.fault.targetPIDFile == $pidFile) and
      (.fault.faultID | type == "string" and length > 0) and
      (.before.pid == ($initialPID | tonumber)) and
      (.before.processStart == $initialStart) and
      (.before.executablePath == $executable) and
      (.before.executableSHA256 == $executableSHA) and
      (.before.commandSHA256 == $commandSHA) and
      (.before.backendID == $backendID) and
      (.before.candidateBuildSHA256 == $buildSHA) and
      (.before.serviceDefinitionSHA256 == $definitionSHA) and
      (.before.mountIdentity == $mountIdentity) and
      (.crash.oldProcessIdentityAbsent == true) and
      (.crash.oldIdentityGoneAt | type == "string" and length > 0) and
      (.after.pid | type == "number" and floor == . and . > 1 and . != ($initialPID | tonumber)) and
      (.after.processStart | type == "string" and length > 0 and . != $initialStart) and
      (.after.executablePath == $executable) and
      (.after.executableSHA256 == $executableSHA) and
      (.after.commandSHA256 == $commandSHA) and
      (.after.backendID == $backendID) and
      (.after.candidateBuildSHA256 == $buildSHA) and
      (.after.serviceDefinitionSHA256 == $definitionSHA) and
      (.after.mountIdentity == $mountIdentity) and
      (.after.pidFileValue == .after.pid) and
      (.after.daemonRunning == true) and
      (.after.mountHealthy == true) and
      (.after.buildHealthy == true) and
      (.binding.isolatedCodexFence.ancestryValid == true) and
      (.binding.isolatedCodexFence.unchangedDuringOccurrence == true)
    ' "$crash" >/dev/null || return 1

  before_process="$EVIDENCE_ROOT/backend-crash/processes.before.tsv"
  during_process="$EVIDENCE_ROOT/backend-crash/processes.during.tsv"
  after_process="$EVIDENCE_ROOT/backend-crash/processes.after.tsv"
  [[ "$(jq -r '.binding.protectedCodexFence.beforeSnapshotPath' "$crash")" == "$before_process" && \
     "$(jq -r '.binding.protectedCodexFence.duringSnapshotPath' "$crash")" == "$during_process" && \
     "$(jq -r '.binding.protectedCodexFence.afterSnapshotPath' "$crash")" == "$after_process" ]] || return 1
  validate_protected_snapshot_file "$before_process" "$(jq -r '.binding.protectedCodexFence.beforeSnapshotSHA256' "$crash")" || return 1
  validate_protected_snapshot_file "$during_process" "$(jq -r '.binding.protectedCodexFence.duringSnapshotSHA256' "$crash")" || return 1
  validate_protected_snapshot_file "$after_process" "$(jq -r '.binding.protectedCodexFence.afterSnapshotSHA256' "$crash")" || return 1

  desktop_pid=$(jq -r '.binding.isolatedCodexFence.desktopPID' "$crash")
  desktop_start=$(jq -r '.binding.isolatedCodexFence.desktopProcessStart' "$crash")
  app_server_pid=$(jq -r '.binding.isolatedCodexFence.appServerPID' "$crash")
  app_server_start=$(jq -r '.binding.isolatedCodexFence.appServerProcessStart' "$crash")
  [[ "$desktop_pid" =~ ^[0-9]+$ && "$app_server_pid" =~ ^[0-9]+$ && -n "$desktop_start" && -n "$app_server_start" ]] || return 1
  snapshot_matches_isolated_fence "$before_process" "$desktop_pid" "$desktop_start" "$app_server_pid" "$app_server_start" || return 1
  snapshot_matches_isolated_fence "$during_process" "$desktop_pid" "$desktop_start" "$app_server_pid" "$app_server_start" || return 1
  snapshot_matches_isolated_fence "$after_process" "$desktop_pid" "$desktop_start" "$app_server_pid" "$app_server_start" || return 1
  [[ "$(process_start "$desktop_pid")" == "$desktop_start" && "$(process_start "$app_server_pid")" == "$app_server_start" ]] || return 1
  ancestor_reaches "$app_server_pid" "$desktop_pid" || return 1

  before_pid=$(jq -r '.before.pid' "$crash")
  after_pid=$(jq -r '.after.pid' "$crash")
  before_start=$(jq -r '.before.processStart' "$crash")
  after_start=$(jq -r '.after.processStart' "$crash")
  after_executable=$(jq -r '.after.executablePath' "$crash")
  after_executable_sha=$(jq -r '.after.executableSHA256' "$crash")
  after_command_sha=$(jq -r '.after.commandSHA256' "$crash")
  runtime_epoch_advanced "$before_pid" "$before_start" "$after_pid" "$after_start" || return 1
  validate_service_status_evidence_file "$(jq -r '.before.serviceStatusSnapshotPath' "$crash")" \
    "$(jq -r '.before.serviceStatusSnapshotSHA256' "$crash")" "$before_pid" "$EVIDENCE_ROOT/backend-crash/service-status.before.json" || return 1
  validate_service_status_evidence_file "$(jq -r '.after.serviceStatusSnapshotPath' "$crash")" \
    "$(jq -r '.after.serviceStatusSnapshotSHA256' "$crash")" "$after_pid" "$EVIDENCE_ROOT/backend-crash/service-status.after.json" || return 1
  validate_backend_status_evidence_file "$(jq -r '.before.backendStatusSnapshotPath' "$crash")" \
    "$(jq -r '.before.backendStatusSnapshotSHA256' "$crash")" "$before_pid" "$EVIDENCE_ROOT/backend-crash/backend-status.before.json" || return 1
  validate_backend_status_evidence_file "$(jq -r '.after.backendStatusSnapshotPath' "$crash")" \
    "$(jq -r '.after.backendStatusSnapshotSHA256' "$crash")" "$after_pid" "$EVIDENCE_ROOT/backend-crash/backend-status.after.json" || return 1
  [[ "$(process_start "$before_pid" 2>/dev/null || true)" != "$before_start" ]] || return 1
  if [[ "$mode" == current ]]; then
    candidate_process_identity "$after_pid" "$after_executable" "$after_executable_sha" "$after_start" "$after_command_sha" || return 1
    [[ "$(tr -d '[:space:]' < "$VALIDATED_BACKEND_PID_FILE")" == "$after_pid" ]] || return 1
  fi
  BACKEND_CRASH_RESPAWN_EVIDENCE_VALID=true
}

validate_png_evidence_file() {
  local requested=$1 expected_sha=$2 expected_exact=$3 path size magic
  path=$(canonical_evidence_file "$requested") || return 1
  [[ "$path" == "$expected_exact" && "$expected_sha" =~ ^[0-9a-f]{64}$ && "$(file_sha "$path")" == "$expected_sha" ]] || return 1
  size=$(stat -f '%z' "$path") || return 1
  [[ "$size" -ge 68 && "$size" -le 26214400 ]] || return 1
  magic=$(od -An -tx1 -N8 "$path" | tr -d '[:space:]')
  [[ "$magic" == 89504e470d0a1a0a ]]
}

validate_redacted_incident_export() {
  local requested=$1 expected_sha=$2 expected_exact=$3 occurrence_id=$4 recovered=$5 path
  path=$(canonical_evidence_file "$requested") || return 1
  [[ "$path" == "$expected_exact" && "$expected_sha" =~ ^[0-9a-f]{64}$ && "$(file_sha "$path")" == "$expected_sha" ]] || return 1
  [[ "$(stat -f '%z' "$path")" -le 1048576 ]] || return 1
  jq -e \
    --arg occurrenceID "$occurrence_id" \
    --argjson recovered "$recovered" '
      (.schema_version == 2) and
      (.application == "CodexFoldFSKit") and
      (.redaction == "conservative-structured") and
      (.occurrence_continuity == "verified_or_publisher") and
      (.incident.id == $occurrenceID) and
      (.incident.reason | type == "string" and length > 0) and
      (.incident.impact | type == "string" and length > 0) and
      (.incident.recommendations | type == "array" and length > 0 and all(.[]; type == "string" and length > 0)) and
      (if $recovered then (.incident.recovered_at | type == "string" and length > 0) else (.incident | has("recovered_at") | not) end) and
      ([.. | strings | select(startswith("/") or test("^https?://";"i"))] | length == 0) and
      ([paths(scalars) as $p | select(($p[-1] | tostring | test("^(technical_details|technicalDetails|detail)$";"i")))] | length == 0)
    ' "$path" >/dev/null || return 1
  if LC_ALL=C rg -n -i '(bearer[[:space:]]+[A-Za-z0-9._~+/=-]+|access[_-]?token|api[_-]?key|password|client[_-]?secret|(^|[^[:alnum:]_])authorization["[:space:]]*:)' "$path" >/dev/null; then
    return 1
  fi
  for forbidden in "$RUN_ROOT" "$CANDIDATE_ROOT" "$SOURCE_REPO_ROOT" "$CODEX_HOME_ISOLATED"; do
    [[ -z "$forbidden" ]] || ! LC_ALL=C grep -F "$forbidden" "$path" >/dev/null || return 1
  done
}

validate_incident_presenter() {
  local observed=$1 occurrence_index=$2 presenter_path pid start role executable executable_sha command_sha expected_helper team
  pid=$(jq -r --argjson index "$occurrence_index" '.occurrences[$index].presenter.pid' "$observed")
  start=$(jq -r --argjson index "$occurrence_index" '.occurrences[$index].presenter.processStart' "$observed")
  role=$(jq -r --argjson index "$occurrence_index" '.occurrences[$index].presenter.role' "$observed")
  executable=$(jq -r --argjson index "$occurrence_index" '.occurrences[$index].presenter.executablePath' "$observed")
  executable_sha=$(jq -r --argjson index "$occurrence_index" '.occurrences[$index].presenter.executableSHA256' "$observed")
  command_sha=$(jq -r --argjson index "$occurrence_index" '.occurrences[$index].presenter.commandSHA256' "$observed")
  [[ "$pid" =~ ^[0-9]+$ && -n "$start" && "$executable_sha" =~ ^[0-9a-f]{64}$ && "$command_sha" =~ ^[0-9a-f]{64}$ ]] || return 1
  presenter_path=$(canonical_candidate_file "$executable") || return 1
  expected_helper="$VALIDATED_CANDIDATE_APP/Contents/MacOS/CodexFoldIncidentMonitor"
  case "$role" in
    menu-bar)
      [[ "$presenter_path" == "$VALIDATED_CANDIDATE_APP_EXECUTABLE" ]] || return 1
      ;;
    incident-monitor)
      [[ "$presenter_path" == "$expected_helper" ]] || return 1
      team=$(codesign_detail_value "$presenter_path" TeamIdentifier) || return 1
      [[ "$team" == "$VALIDATED_CANDIDATE_APP_TEAM" ]] || return 1
      ;;
    *) return 1 ;;
  esac
  [[ "$(file_sha "$presenter_path")" == "$executable_sha" ]] || return 1
  candidate_process_identity "$pid" "$presenter_path" "$executable_sha" "$start" "$command_sha"
}

validate_native_window_census() {
  local census=$1 first_id=$2 first_epoch=$3 first_window=$4 second_id=$5 second_epoch=$6 second_window=$7
  jq -s -e \
    --arg firstID "$first_id" --arg firstEpoch "$first_epoch" --argjson firstWindow "$first_window" \
    --arg secondID "$second_id" --arg secondEpoch "$second_epoch" --argjson secondWindow "$second_window" '
      length >= 8 and
      all(.[];
        (.occurrenceID | type == "string" and length > 0) and
        (.recoveryEpochID | type == "string" and length > 0) and
        (.phase == "pre-threshold" or .phase == "active" or .phase == "steady" or .phase == "recovered") and
        (.elapsedMilliseconds | type == "number" and floor == . and . >= 0) and
        (.matchingWindowCount | type == "number" and floor == . and . >= 0) and
        ((.windowID == null) or (.windowID | type == "number" and floor == . and . > 0))
      ) and
      all(.[] | select(.occurrenceID == $firstID); .recoveryEpochID == $firstEpoch and .matchingWindowCount <= 1) and
      all(.[] | select(.occurrenceID == $secondID); .recoveryEpochID == $secondEpoch and .matchingWindowCount <= 1) and
      ([.[] | select(.occurrenceID == $firstID and .elapsedMilliseconds < 10000)] | length > 0 and all(.[]; .matchingWindowCount == 0)) and
      any(.[]; .occurrenceID == $firstID and .phase == "pre-threshold" and .elapsedMilliseconds >= 9000 and .elapsedMilliseconds < 10000 and .matchingWindowCount == 0) and
      any(.[]; .occurrenceID == $firstID and .phase == "active" and .elapsedMilliseconds >= 10000 and .matchingWindowCount == 1 and .windowID == $firstWindow) and
      ([.[] | select(.occurrenceID == $firstID and .phase == "steady")] | length >= 2 and all(.[]; .matchingWindowCount == 1 and .windowID == $firstWindow)) and
      any(.[]; .occurrenceID == $firstID and .phase == "recovered" and .matchingWindowCount == 1 and .windowID == $firstWindow) and
      ([.[] | select(.occurrenceID == $secondID and .elapsedMilliseconds < 10000)] | length > 0 and all(.[]; .matchingWindowCount == 0)) and
      any(.[]; .occurrenceID == $secondID and .phase == "pre-threshold" and .elapsedMilliseconds >= 9000 and .elapsedMilliseconds < 10000 and .matchingWindowCount == 0) and
      any(.[]; .occurrenceID == $secondID and .phase == "active" and .elapsedMilliseconds >= 10000 and .matchingWindowCount == 1 and .windowID == $secondWindow)
    ' "$census" >/dev/null
}

validate_native_incident_evidence() {
  local observed=${1:-$EVIDENCE_ROOT/native-incident-observed.json} mode=${2:-committed}
  local observed_path observed_sha census first_id first_epoch first_window second_id second_epoch second_window
  local desktop_pid desktop_start app_server_pid app_server_start phase field path sha
  NATIVE_INCIDENT_EVIDENCE_VALID=false
  observed_path=$(canonical_evidence_file "$observed") || return 1
  [[ "$(stat -f '%z' "$observed_path")" -le 1048576 ]] || return 1
  observed_sha=$(file_sha "$observed_path") || return 1
  if [[ "$mode" == committed ]]; then
    [[ "$observed_path" == "$EVIDENCE_ROOT/native-incident-observed.json" && "$RUN_NATIVE_INCIDENT_EVIDENCE_SHA" == "$observed_sha" ]] || return 1
  fi
  acceptance_binding_common_matches "$observed_path" || return 1
  [[ "$(jq -r '.binding.backendCrashRespawnEvidenceSHA256' "$observed_path")" == "$RUN_BACKEND_CRASH_EVIDENCE_SHA" && \
     "$BACKEND_CRASH_RESPAWN_EVIDENCE_VALID" == true ]] || return 1
  jq -e \
    --arg schema "$NATIVE_INCIDENT_EVIDENCE_SCHEMA" '
      (.schema == $schema) and
      ((keys | sort) == (["binding","census","occurrences","recordedAt","schema"] | sort)) and
      (.recordedAt | type == "string" and length > 0) and
      (.occurrences | type == "array" and length == 2) and
      all(.occurrences[]; ((keys | sort) == (["active","closedAfterRecoveryAt","incidentSince","occurrenceID","presenter","recovery","recoveryEpochID"] | sort) or (keys | sort) == (["active","incidentSince","occurrenceID","presenter","recoveryEpochID"] | sort))) and
      all(.occurrences[]; (.presenter | keys | sort) == (["commandSHA256","executablePath","executableSHA256","pid","processStart","role"] | sort)) and
      (.occurrences[0].occurrenceID | type == "string" and length > 0) and
      (.occurrences[1].occurrenceID | type == "string" and length > 0) and
      (.occurrences[0].occurrenceID != .occurrences[1].occurrenceID) and
      (.occurrences[0].recoveryEpochID | type == "string" and length > 0) and
      (.occurrences[1].recoveryEpochID | type == "string" and length > 0) and
      (.occurrences[0].recoveryEpochID != .occurrences[1].recoveryEpochID) and
      (.occurrences[0].active.window.windowID | type == "number" and floor == . and . > 0) and
      (.occurrences[1].active.window.windowID | type == "number" and floor == . and . > 0) and
      (.occurrences[0].active.window.windowID != .occurrences[1].active.window.windowID) and
      (.occurrences[0].active.window.reasonVisible == true) and
      (.occurrences[0].active.window.impactVisible == true) and
      (.occurrences[0].active.window.recommendationCount | type == "number" and floor == . and . > 0) and
      (.occurrences[0].active.window.technicalDetailsExpanded == true) and
      (.occurrences[0].active.window.technicalDetailsNonempty == true) and
      (.occurrences[0].recovery.sameOccurrenceID == true) and
      (.occurrences[0].recovery.sameWindowID == true) and
      (.occurrences[0].recovery.recoveredTitleVisible == true) and
      (.occurrences[0].recovery.recoveredBodyVisible == true) and
      (.occurrences[0].closedAfterRecoveryAt | type == "string" and length > 0) and
      (.occurrences[1].incidentSince > .occurrences[0].recovery.observedAt) and
      (.occurrences[1].active.window.reasonVisible == true) and
      (.occurrences[1].active.window.impactVisible == true) and
      (.occurrences[1].active.window.recommendationCount | type == "number" and floor == . and . > 0) and
      (.occurrences[1].active.window.technicalDetailsExpanded == true) and
      (.occurrences[1].active.window.technicalDetailsNonempty == true) and
      all(.occurrences[];
        (.presenter.pid | type == "number" and floor == . and . > 1) and
        (.presenter.processStart | type == "string" and length > 0) and
        (.presenter.role == "menu-bar" or .presenter.role == "incident-monitor") and
        (.presenter.executablePath | type == "string" and startswith("/")) and
        (.presenter.executableSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
        (.presenter.commandSHA256 | type == "string" and test("^[0-9a-f]{64}$"))
      )
    ' "$observed_path" >/dev/null || return 1
  if jq -e '[paths(scalars) as $p | select(($p[-1] | tostring | test("token|secret|password|api.?key";"i")))] | length > 0' "$observed_path" >/dev/null; then
    return 1
  fi
  if LC_ALL=C rg -n -i '(bearer[[:space:]]+[A-Za-z0-9._~+/=-]+|access[_-]?token|api[_-]?key|password|client[_-]?secret|(^|[^[:alnum:]_])authorization["[:space:]]*:)' "$observed_path" >/dev/null; then
    return 1
  fi

  first_id=$(jq -r '.occurrences[0].occurrenceID' "$observed_path")
  first_epoch=$(jq -r '.occurrences[0].recoveryEpochID' "$observed_path")
  first_window=$(jq -r '.occurrences[0].active.window.windowID' "$observed_path")
  second_id=$(jq -r '.occurrences[1].occurrenceID' "$observed_path")
  second_epoch=$(jq -r '.occurrences[1].recoveryEpochID' "$observed_path")
  second_window=$(jq -r '.occurrences[1].active.window.windowID' "$observed_path")
  census="$EVIDENCE_ROOT/native-incident/window-census.jsonl"
  [[ "$(jq -r '.census.path' "$observed_path")" == "$census" && "$(file_sha "$census" 2>/dev/null || true)" == "$(jq -r '.census.sha256' "$observed_path")" ]] || return 1
  canonical_evidence_file "$census" >/dev/null || return 1
  validate_native_window_census "$census" "$first_id" "$first_epoch" "$first_window" "$second_id" "$second_epoch" "$second_window" || return 1

  validate_incident_presenter "$observed_path" 0 || return 1
  validate_incident_presenter "$observed_path" 1 || return 1
  validate_png_evidence_file "$(jq -r '.occurrences[0].active.window.screenshotPath' "$observed_path")" \
    "$(jq -r '.occurrences[0].active.window.screenshotSHA256' "$observed_path")" "$EVIDENCE_ROOT/native-incident/occurrence-1-active.png" || return 1
  validate_png_evidence_file "$(jq -r '.occurrences[0].recovery.screenshotPath' "$observed_path")" \
    "$(jq -r '.occurrences[0].recovery.screenshotSHA256' "$observed_path")" "$EVIDENCE_ROOT/native-incident/occurrence-1-recovered.png" || return 1
  validate_png_evidence_file "$(jq -r '.occurrences[1].active.window.screenshotPath' "$observed_path")" \
    "$(jq -r '.occurrences[1].active.window.screenshotSHA256' "$observed_path")" "$EVIDENCE_ROOT/native-incident/occurrence-2-active.png" || return 1
  validate_redacted_incident_export "$(jq -r '.occurrences[0].active.diagnosticExport.path' "$observed_path")" \
    "$(jq -r '.occurrences[0].active.diagnosticExport.sha256' "$observed_path")" "$EVIDENCE_ROOT/native-incident/occurrence-1-active.json" "$first_id" false || return 1
  validate_redacted_incident_export "$(jq -r '.occurrences[0].recovery.diagnosticExportPath' "$observed_path")" \
    "$(jq -r '.occurrences[0].recovery.diagnosticExportSHA256' "$observed_path")" "$EVIDENCE_ROOT/native-incident/occurrence-1-recovered.json" "$first_id" true || return 1
  validate_redacted_incident_export "$(jq -r '.occurrences[1].active.diagnosticExport.path' "$observed_path")" \
    "$(jq -r '.occurrences[1].active.diagnosticExport.sha256' "$observed_path")" "$EVIDENCE_ROOT/native-incident/occurrence-2-active.json" "$second_id" false || return 1

  desktop_pid=$(jq -r '.binding.isolatedCodexFence.desktopPID' "$observed_path")
  desktop_start=$(jq -r '.binding.isolatedCodexFence.desktopProcessStart' "$observed_path")
  app_server_pid=$(jq -r '.binding.isolatedCodexFence.appServerPID' "$observed_path")
  app_server_start=$(jq -r '.binding.isolatedCodexFence.appServerProcessStart' "$observed_path")
  [[ "$desktop_pid" =~ ^[0-9]+$ && "$app_server_pid" =~ ^[0-9]+$ && \
     "$(process_start "$desktop_pid")" == "$desktop_start" && "$(process_start "$app_server_pid")" == "$app_server_start" ]] || return 1
  ancestor_reaches "$app_server_pid" "$desktop_pid" || return 1
  for phase in before duringFirst afterFirst duringSecond afterSecond; do
    case "$phase" in
      before) field=beforeSnapshot ;;
      duringFirst) field=duringFirstSnapshot ;;
      afterFirst) field=afterFirstSnapshot ;;
      duringSecond) field=duringSecondSnapshot ;;
      afterSecond) field=afterSecondSnapshot ;;
    esac
    path=$(jq -r --arg field "$field" '.binding.protectedCodexFence[$field + "Path"]' "$observed_path")
    sha=$(jq -r --arg field "$field" '.binding.protectedCodexFence[$field + "SHA256"]' "$observed_path")
    [[ "$path" == "$EVIDENCE_ROOT/native-incident/processes.$phase.tsv" ]] || return 1
    validate_protected_snapshot_file "$path" "$sha" || return 1
    snapshot_matches_isolated_fence "$path" "$desktop_pid" "$desktop_start" "$app_server_pid" "$app_server_start" || return 1
  done
  NATIVE_INCIDENT_EVIDENCE_VALID=true
}

validate_native_incident_review_evidence() {
  local review=${1:-$EVIDENCE_ROOT/native-incident-review.json} mode=${2:-committed}
  local observed="$EVIDENCE_ROOT/native-incident-observed.json" review_sha observed_sha review_path
  NATIVE_INCIDENT_GUI_REVIEW_COMPLETE=false
  review_path=$(canonical_evidence_file "$review") || return 1
  [[ -f "$observed" && ! -L "$observed" ]] || return 1
  review_sha=$(file_sha "$review_path") || return 1
  observed_sha=$(file_sha "$observed") || return 1
  [[ "$RUN_NATIVE_INCIDENT_EVIDENCE_SHA" == "$observed_sha" ]] || return 1
  if [[ "$mode" == committed ]]; then
    [[ "$review_path" == "$EVIDENCE_ROOT/native-incident-review.json" && "$RUN_NATIVE_INCIDENT_REVIEW_SHA" == "$review_sha" ]] || return 1
  fi
  jq -e \
    --arg schema "$NATIVE_INCIDENT_REVIEW_SCHEMA" \
    --arg observedSHA "$observed_sha" \
    --argjson firstWindow "$(jq -r '.occurrences[0].active.window.windowID' "$observed")" \
    --argjson secondWindow "$(jq -r '.occurrences[1].active.window.windowID' "$observed")" \
    --arg firstActiveSHA "$(jq -r '.occurrences[0].active.window.screenshotSHA256' "$observed")" \
    --arg firstRecoveredSHA "$(jq -r '.occurrences[0].recovery.screenshotSHA256' "$observed")" \
    --arg secondActiveSHA "$(jq -r '.occurrences[1].active.window.screenshotSHA256' "$observed")" '
      (.schema == $schema) and
      (.observedEvidenceSHA256 == $observedSHA) and
      (.reviewedAt | type == "string" and length > 0) and
      (.reviewedWindowIDs == [$firstWindow,$secondWindow]) and
      (.reviewedScreenshotSHA256s == [$firstActiveSHA,$firstRecoveredSHA,$secondActiveSHA]) and
      (.reasonVisible == true) and
      (.impactVisible == true) and
      (.recommendationsVisible == true) and
      (.technicalDetailsExpandedAndVisible == true) and
      (.recoveryUpdateVisible == true) and
      (.newEpochNewWindowVisible == true) and
      (.reviewerStatement | type == "string" and length >= 20) and
      ((keys | sort) == (["impactVisible","newEpochNewWindowVisible","observedEvidenceSHA256","reasonVisible","recommendationsVisible","recoveryUpdateVisible","reviewedAt","reviewedScreenshotSHA256s","reviewedWindowIDs","reviewerStatement","schema","technicalDetailsExpandedAndVisible"] | sort))
    ' "$review_path" >/dev/null || return 1
  NATIVE_INCIDENT_GUI_REVIEW_COMPLETE=true
}

verify_candidate_evidence() {
  local evidence="$EVIDENCE_ROOT/codexfold-candidate-observed.json"
  local evidence_sha candidate_sha
  CANDIDATE_ATTACHED=false
  CANDIDATE_BUILD_SHA=''
  MANAGED_ROUTE_OBSERVED=false
  CANDIDATE_EVIDENCE_VALID=false
  CANDIDATE_METADATA_CONSISTENT=false
  BACKEND_CRASH_RESPAWN_EVIDENCE_VALID=false
  CANDIDATE_ROUTE_COUNT=0
  CANDIDATE_TASK_ROUTE_MATCHES=0

  if [[ ! -e "$evidence" ]]; then
    if [[ "$RUN_CANDIDATE_ATTACHED" == false && "$RUN_MANAGED_ROUTE_OBSERVED" == false && \
          -z "$RUN_CANDIDATE_BUILD_SHA" && -z "$RUN_CANDIDATE_APP_IDENTITY" && \
          -z "$RUN_CANDIDATE_MODULE_SHA" && -z "$RUN_CANDIDATE_BUILD_MANIFEST_SHA" && \
          -z "$RUN_CANDIDATE_EVIDENCE_SHA" && -z "$RUN_BACKEND_CRASH_EVIDENCE_SHA" && \
          -z "$RUN_NATIVE_INCIDENT_EVIDENCE_SHA" && -z "$RUN_NATIVE_INCIDENT_REVIEW_SHA" ]]; then
      CANDIDATE_METADATA_CONSISTENT=true
    fi
    return 0
  fi

  [[ -f "$evidence" && ! -L "$evidence" ]] || return 0
  [[ -z "$RUN_BACKEND_CRASH_EVIDENCE_SHA" || -f "$EVIDENCE_ROOT/candidate-backend-crash-respawn.json" ]] || return 0
  [[ "$(stat -f '%z' "$evidence")" -le 1048576 ]] || return 0
  jq -e \
    --arg schema "$CANDIDATE_EVIDENCE_SCHEMA" \
    --arg home "$CODEX_HOME_ISOLATED" \
    --arg candidate "$CANDIDATE_ROOT" '
      (.schema == $schema) and
      (.source == "external-real-codexfold-candidate") and
      (.recordedAt | type == "string" and length > 0) and
      (.observedAt | type == "string" and length > 0) and
      (.codexHome == $home) and
      (.candidateRoot == $candidate) and
      (.mountPoint | type == "string" and startswith("/")) and
      (.mountIdentity | type == "string" and test("^[0-9a-f]{64}$")) and
      (.candidateAttached == true) and
      (.managedRouteObserved == true) and
      (.candidateBinaryPath | type == "string" and startswith("/")) and
      (.serviceDefinitionPath | type == "string" and startswith("/")) and
      (.serviceDefinitionSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
      (.backendStatus.path | type == "string" and startswith("/")) and
      (.backendStatus.initialSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
      (.backendStatus.backendID | type == "string" and length > 0) and
      (.candidateBuildSHA | type == "string" and test("^[0-9a-f]{64}$")) and
      (.daemonPid | type == "number" and floor == . and . > 1) and
      (.daemonProcessStart | type == "string" and length > 0) and
      (.daemonCommandSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
      (.daemonExecutablePath | type == "string" and startswith("/")) and
      (.daemonExecutableSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
      (.serviceStatus.daemonRunning == true) and
      (.serviceStatus.mountHealthy == true) and
      (.serviceStatus.buildHealthy == true) and
      (.serviceStatus.runningBuildSHA256 == .candidateBuildSHA) and
      (.serviceStatus.configuredBuildSHA256 == .candidateBuildSHA) and
      (.serviceStatus.configuredBinaryPath == .candidateBinaryPath) and
      (.candidateApp.identitySHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
      (.fskitModule.executableSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
      (.fskitModule.process.snapshotSHA256 | type == "string" and test("^[0-9a-f]{64}$")) and
      ((.faultTargets // {}) | type == "object")
    ' "$evidence" >/dev/null || return 0

  validate_live_candidate "$evidence" || return 0
  candidate_sha=$VALIDATED_CANDIDATE_SHA
  validate_candidate_routes "$evidence" || return 0
  evidence_sha=$(shasum -a 256 "$evidence" | awk '{print $1}') || return 0
  [[ "$RUN_CANDIDATE_ATTACHED" == true && "$RUN_MANAGED_ROUTE_OBSERVED" == true ]] || return 0
  [[ "$RUN_CANDIDATE_BUILD_SHA" == "$candidate_sha" && \
     "$RUN_CANDIDATE_APP_IDENTITY" == "$VALIDATED_CANDIDATE_APP_IDENTITY" && \
     "$RUN_CANDIDATE_MODULE_SHA" == "$VALIDATED_CANDIDATE_MODULE_EXECUTABLE_SHA" && \
     "$RUN_CANDIDATE_BUILD_MANIFEST_SHA" == "$VALIDATED_CANDIDATE_BUILD_MANIFEST_SHA" && \
     "$RUN_CANDIDATE_EVIDENCE_SHA" == "$evidence_sha" ]] || return 0

  candidate_task_route_matches
  CANDIDATE_ATTACHED=true
  CANDIDATE_BUILD_SHA=$candidate_sha
  MANAGED_ROUTE_OBSERVED=true
  CANDIDATE_EVIDENCE_VALID=true
  CANDIDATE_METADATA_CONSISTENT=true
  CANDIDATE_ROUTE_COUNT=$VALIDATED_ROUTE_COUNT
}

write_report() {
  local report="$EVIDENCE_ROOT/report.md"
  local verification="$EVIDENCE_ROOT/verification.json"
  local launch_status=not-launched
  local task_status=not-observed
  local cli_task_status=not-run
  local fault_count=0 applied_fault_count=0
  local protected=not-checked slice=not-checked desktop=not-required app_server=not-required ancestry=not-required sync_disabled=false
  local cockpit_control_plane=false cockpit_managed=false cockpit_pid_match=not-required cockpit_command=false
  local cockpit_bundle_id=not-recorded cockpit_bundle_short=not-recorded cockpit_bundle_build=not-recorded
  local launcher_mode=cockpit-compatible-adapter-prepared-only real_complete=false codex_instance_complete=false candidate_complete=false
  local candidate_attached=false candidate_build=not-observed managed_route=false candidate_evidence=false candidate_routes=0 candidate_task_matches=0
  local crash_valid=false incident_valid=false gui_review=false fault_complete=false incident_complete=false
  if [[ -f "$EVIDENCE_ROOT/task-result.json" ]]; then
    cli_task_status=$(jq -r '.status' "$EVIDENCE_ROOT/task-result.json")
  fi
  if [[ -f "$EVIDENCE_ROOT/faults.tsv" ]]; then
    fault_count=$(wc -l < "$EVIDENCE_ROOT/faults.tsv" | tr -d ' ')
    applied_fault_count=$(awk -F '\t' '$2 == "apply" { count++ } END { print count+0 }' "$EVIDENCE_ROOT/faults.tsv")
  fi
  if [[ -f "$verification" ]]; then
    protected=$(jq -r '.protectedCodexUnchanged' "$verification")
    slice=$(jq -r '.sessionSliceValid' "$verification")
    desktop=$(jq -r '.isolatedDesktopBound' "$verification")
    app_server=$(jq -r '.isolatedAppServerBound' "$verification")
    ancestry=$(jq -r '.isolatedAncestryValid' "$verification")
    sync_disabled=$(jq -r '.autoSyncThreadsDisabled' "$verification")
    cockpit_managed=$(jq -r '.cockpitManaged' "$verification")
    cockpit_control_plane=$(jq -r '.cockpitControlPlaneIsolated' "$verification")
    cockpit_pid_match=$(jq -r '.cockpitLastPidMatches' "$verification")
    cockpit_command=$(jq -r '.cockpitCommandObserved' "$verification")
    [[ "$cockpit_command" != true ]] || launch_status=verified-actual-cockpit-start
    cockpit_bundle_id=$(jq -r '.cockpitBundleIdentifier // "not-recorded"' "$verification")
    cockpit_bundle_short=$(jq -r '.cockpitBundleShortVersion // "not-recorded"' "$verification")
    cockpit_bundle_build=$(jq -r '.cockpitBundleVersion // "not-recorded"' "$verification")
    launcher_mode=$(jq -r '.launcherMode' "$verification")
    codex_instance_complete=$(jq -r '.codexInstanceAcceptanceComplete' "$verification")
    candidate_attached=$(jq -r '.candidateAttached' "$verification")
    candidate_build=$(jq -r '.candidateBuildSHA // "not-observed"' "$verification")
    candidate_evidence=$(jq -r '.candidateEvidenceValid' "$verification")
    managed_route=$(jq -r '.managedRouteObserved' "$verification")
    candidate_routes=$(jq -r '.candidateRouteCount' "$verification")
    candidate_task_matches=$(jq -r '.candidateRealTaskRouteMatches' "$verification")
    crash_valid=$(jq -r '.backendCrashRespawnEvidenceValid' "$verification")
    incident_valid=$(jq -r '.nativeIncidentEvidenceValid' "$verification")
    gui_review=$(jq -r '.nativeIncidentGUIReviewComplete' "$verification")
    fault_complete=$(jq -r '.candidateFaultAcceptanceComplete' "$verification")
    incident_complete=$(jq -r '.nativeIncidentAcceptanceComplete' "$verification")
    [[ "$(jq -r '.realTaskObserved' "$verification")" != true ]] || task_status=verified
    candidate_complete=$(jq -r '.codexFoldCandidateAcceptanceComplete' "$verification")
    real_complete=$(jq -r '.realAcceptanceComplete' "$verification")
  fi
  # ShellCheck's SC2016 warning is intentionally inapplicable: Markdown
  # backticks in these constant format strings must not be shell-expanded.
  # shellcheck disable=SC2016
  {
    printf '# Isolated Codex Acceptance Evidence\n\n'
    printf -- '- Run ID: `%s`\n' "$(jq -r '.runId' "$RUN_ROOT/run.json")"
    printf -- '- Generated: `%s`\n' "$(timestamp)"
    printf -- '- Session slice: `%s` source session(s), current isolated threads=`%s`, valid=`%s`\n' \
      "${SLICE_COUNT:-$(wc -l < "$CODEX_HOME_ISOLATED/selected-sessions.tsv" 2>/dev/null || printf 0)}" \
      "${CURRENT_THREAD_COUNT:-unknown}" "$slice"
    printf -- '- Cockpit safety setting: `autoSyncThreads` persistently disabled=`%s`\n' "$sync_disabled"
    printf -- '- Cockpit control plane: isolated=`%s`, record prepared=`%s`, real Start observed=`%s`, persisted PID match=`%s`\n' \
      "$cockpit_control_plane" "$cockpit_managed" "$cockpit_command" "$cockpit_pid_match"
    printf -- '- Cockpit bundle: identifier=`%s`, short version=`%s`, build=`%s`\n' \
      "$cockpit_bundle_id" "$cockpit_bundle_short" "$cockpit_bundle_build"
    printf -- '- Launcher mode: `%s`; isolated Codex instance acceptance complete=`%s`\n' "$launcher_mode" "$codex_instance_complete"
    printf -- '- Desktop: `%s`, isolated user-data binding=`%s`\n' "$launch_status" "$desktop"
    printf -- '- App server isolated `CODEX_HOME` binding: `%s`\n' "$app_server"
    printf -- '- Desktop/app-server ancestry: `%s`\n' "$ancestry"
    printf -- '- Pre-existing Codex PID/start-time unchanged: `%s`\n' "$protected"
    printf -- '- Real Desktop task mutation: `%s`; optional CLI diagnostic=`%s`\n' "$task_status" "$cli_task_status"
    printf -- '- CodexFold candidate: evidence valid=`%s`, attached=`%s`, build SHA-256=`%s`, managed route observed=`%s`, observed routes=`%s`, real-task route matches=`%s`\n' \
      "$candidate_evidence" "$candidate_attached" "$candidate_build" "$managed_route" "$candidate_routes" "$candidate_task_matches"
    printf -- '- Candidate backend crash/respawn: evidence valid=`%s`, fault acceptance complete=`%s`\n' "$crash_valid" "$fault_complete"
    printf -- '- Native incident window: evidence valid=`%s`, GUI review complete=`%s`, incident acceptance complete=`%s`\n' \
      "$incident_valid" "$gui_review" "$incident_complete"
    printf -- '- CodexFold candidate acceptance complete: `%s`; combined real acceptance complete=`%s`\n' "$candidate_complete" "$real_complete"
    printf -- '- Recorded candidate fault plans/runs: total=`%s`, applied=`%s`\n' "$fault_count" "$applied_fault_count"
    printf '\nNo credential values, process environments, session contents, or raw command lines are included in this report.\n'
  } > "$report"
  chmod 600 "$report"
}

command_prepare() {
  local source_home="$HOME/.codex"
  local run_root=''
  local app_path=/Applications/ChatGPT.app
  local protected_app_path=/Applications/ChatGPT.app
  local workspace=$PWD
  local session_count=3
  local explicit_ids=("")
  local explicit_count=0
  local cockpit_app_path='/Applications/Cockpit Tools.app'
  local created=false

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --source-home) source_home=${2:?}; shift 2 ;;
      --run-root) run_root=${2:?}; shift 2 ;;
      --app) app_path=${2:?}; shift 2 ;;
      --protected-app) protected_app_path=${2:?}; shift 2 ;;
      --workspace) workspace=${2:?}; shift 2 ;;
      --session-count) session_count=${2:?}; shift 2 ;;
      --session)
        explicit_ids[explicit_count]=${2:?}
        explicit_count=$((explicit_count + 1))
        shift 2
        ;;
      --cockpit-app) cockpit_app_path=${2:?}; shift 2 ;;
      -h|--help)
        cat <<'EOF'
prepare options:
  --run-root DIR               Required; must not already exist.
  --source-home DIR            Default: ~/.codex
  --session-count N            Default: 3; maximum: 10.
  --session ID                 Exact session selection; repeatable.
  --app APP                    Default: /Applications/ChatGPT.app
  --protected-app APP          Production bundle whose PID/start-time is fenced.
  --workspace DIR              Workspace opened by the isolated instance.
  --cockpit-app APP             Cockpit Tools bundle used for isolated UI inspection.
EOF
        return 0
        ;;
      *) die "unknown prepare option: $1" ;;
    esac
  done
  [[ -n "$run_root" ]] || die "prepare requires --run-root"
  [[ ! -e "$run_root" ]] || die "run root already exists: $run_root"
  [[ -d "$source_home" ]] || die "source CODEX_HOME does not exist: $source_home"
  [[ -d "$workspace" ]] || die "workspace does not exist: $workspace"
  need_command jq
  need_command git
  need_command sqlite3
  need_command shasum
  need_command plutil
  need_command uuidgen
  [[ -x "$COCKPIT_ADAPTER" ]] || die "Cockpit instance adapter is unavailable"
  if [[ ! "$session_count" =~ ^[0-9]+$ ]] || (( session_count < 1 || session_count > 10 )); then
    die "--session-count must be an integer between 1 and 10"
  fi

  run_root=$(canonical_new_path "$run_root")
  mkdir -p "$run_root"
  chmod 700 "$run_root"
  created=true
  cleanup_prepare() {
    local cleanup_status=$?
    if (( cleanup_status != 0 )) && [[ "$created" == true ]]; then
      rm -rf "$run_root"
    fi
    exit "$cleanup_status"
  }
  trap cleanup_prepare EXIT HUP INT TERM

  RUN_ROOT=$run_root
  CODEX_HOME_ISOLATED="$RUN_ROOT/codex-home"
  CANDIDATE_ROOT="$RUN_ROOT/candidate"
  EVIDENCE_ROOT="$RUN_ROOT/evidence"
  COCKPIT_DATA_ROOT="$RUN_ROOT/cockpit-data"
  COCKPIT_STORE="$COCKPIT_DATA_ROOT/codex_instances.json"
  APP_PATH=$(cd "$(dirname "$app_path")" && pwd -P)/$(basename "$app_path")
  PROTECTED_APP_PATH=$(cd "$(dirname "$protected_app_path")" && pwd -P)/$(basename "$protected_app_path")
  COCKPIT_APP_PATH=$(cd "$(dirname "$cockpit_app_path")" && pwd -P)/$(basename "$cockpit_app_path")
  WORKSPACE=$(cd "$workspace" && pwd -P)
  mkdir -p "$CANDIDATE_ROOT" "$EVIDENCE_ROOT" "$RUN_ROOT/private" "$COCKPIT_DATA_ROOT"
  chmod 700 "$CANDIDATE_ROOT" "$EVIDENCE_ROOT" "$RUN_ROOT/private" "$COCKPIT_DATA_ROOT"
  mkdir "$EVIDENCE_ROOT/native-incident"
  chmod 700 "$EVIDENCE_ROOT/native-incident"

  prepare_args=("$source_home" "$CODEX_HOME_ISOLATED")
  if (( explicit_count > 0 )); then
    for ((i = 0; i < explicit_count; i++)); do
      prepare_args+=(--session "${explicit_ids[$i]}")
    done
  else
    prepare_args+=(--session-count "$session_count")
  fi
  "$PREPARE_HOME" "${prepare_args[@]}"

  ELECTRON_DATA=$("$COCKPIT_ADAPTER" electron-data \
    --store "$COCKPIT_STORE" \
    --codex-home "$CODEX_HOME_ISOLATED")
  [[ "$ELECTRON_DATA" == "$COCKPIT_DATA_ROOT/"* ]] || die "Cockpit Electron data escaped the isolated control plane"
  mkdir -p "$ELECTRON_DATA"
  chmod 700 "$ELECTRON_DATA"

  run_id=$(date -u '+%Y%m%dT%H%M%SZ')-$(uuidgen | tr '[:upper:]' '[:lower:]')
  cockpit_available=false
  cockpit_sync_safe=null
  if [[ -d "$COCKPIT_APP_PATH" ]]; then
    cockpit_available=true
  fi
  cockpit_bundle_identifier=$(bundle_info_value "$COCKPIT_APP_PATH" CFBundleIdentifier)
  cockpit_bundle_short_version=$(bundle_info_value "$COCKPIT_APP_PATH" CFBundleShortVersionString)
  cockpit_bundle_version=$(bundle_info_value "$COCKPIT_APP_PATH" CFBundleVersion)
  cockpit_executable=$(bundle_executable_path "$COCKPIT_APP_PATH")
  app_executable=$(bundle_executable_path "$APP_PATH")
  cockpit_executable=$(bundle_executable_path "$COCKPIT_APP_PATH")
  cockpit_executable_sha=''
  [[ ! -f "$cockpit_executable" ]] || cockpit_executable_sha=$(file_sha "$cockpit_executable")
  app_executable=$(bundle_executable_path "$APP_PATH")
  app_executable_sha=''
  [[ ! -f "$app_executable" ]] || app_executable_sha=$(file_sha "$app_executable")
  run_root_identity=$(directory_identity "$RUN_ROOT")
  codex_home_identity=$(directory_identity "$CODEX_HOME_ISOLATED")
  candidate_root_identity=$(directory_identity "$CANDIDATE_ROOT")
  evidence_root_identity=$(directory_identity "$EVIDENCE_ROOT")
  cockpit_data_identity=$(directory_identity "$COCKPIT_DATA_ROOT")
  electron_data_identity=$(directory_identity "$ELECTRON_DATA")
  fskit_module_baseline="$EVIDENCE_ROOT/fskit-module-processes.before.tsv"
  write_fskit_module_process_snapshot "$fskit_module_baseline" || die "could not capture the bounded pre-mount FSKit module process baseline"
  fskit_module_baseline_sha=$(file_sha "$fskit_module_baseline")
  protected_process_baseline="$EVIDENCE_ROOT/protected-processes.before.tsv"
  write_baseline_snapshot "$protected_process_baseline"
  protected_process_baseline_sha=$(file_sha "$protected_process_baseline")
  source_repo_identity=$(directory_identity "$SOURCE_REPO_ROOT")
  source_head=$(source_git_head "$SOURCE_REPO_ROOT")
  source_snapshot=$(source_snapshot_sha "$SOURCE_REPO_ROOT") || die "could not compute the tracked and untracked source snapshot digest"

  jq -n \
    --arg schema "$SCHEMA" \
    --arg runRoot "$RUN_ROOT" \
    --arg runId "$run_id" \
    --arg createdAt "$(timestamp)" \
    --arg sourceCodexHome "$(cd "$source_home" && pwd -P)" \
    --arg codexHome "$CODEX_HOME_ISOLATED" \
    --arg electronUserData "$ELECTRON_DATA" \
    --arg candidateRoot "$CANDIDATE_ROOT" \
    --arg evidenceRoot "$EVIDENCE_ROOT" \
    --arg appPath "$APP_PATH" \
    --arg protectedAppPath "$PROTECTED_APP_PATH" \
    --arg cockpitAppPath "$COCKPIT_APP_PATH" \
    --arg cockpitBundleIdentifier "$cockpit_bundle_identifier" \
    --arg cockpitBundleShortVersion "$cockpit_bundle_short_version" \
    --arg cockpitBundleVersion "$cockpit_bundle_version" \
    --arg cockpitExecutableSHA "$cockpit_executable_sha" \
    --arg appExecutableSHA "$app_executable_sha" \
    --arg cockpitDataRoot "$COCKPIT_DATA_ROOT" \
    --arg cockpitStorePath "$COCKPIT_STORE" \
    --arg workspace "$WORKSPACE" \
    --argjson cockpitAvailable "$cockpit_available" \
    --argjson cockpitSyncSafe "$cockpit_sync_safe" \
    --arg runRootIdentity "$run_root_identity" \
    --arg codexHomeIdentity "$codex_home_identity" \
    --arg candidateRootIdentity "$candidate_root_identity" \
    --arg evidenceRootIdentity "$evidence_root_identity" \
    --arg cockpitDataIdentity "$cockpit_data_identity" \
    --arg electronDataIdentity "$electron_data_identity" \
    --arg fskitModuleBaseline "$fskit_module_baseline" \
    --arg fskitModuleBaselineSHA "$fskit_module_baseline_sha" \
    --arg protectedProcessBaseline "$protected_process_baseline" \
    --arg protectedProcessBaselineSHA "$protected_process_baseline_sha" \
    --arg sourceRepoRoot "$SOURCE_REPO_ROOT" \
    --arg sourceRepoIdentity "$source_repo_identity" \
    --arg sourceGitHead "$source_head" \
    --arg sourceSnapshotSHA "$source_snapshot" \
    '{schema:$schema,runRoot:$runRoot,runId:$runId,createdAt:$createdAt,sourceCodexHome:$sourceCodexHome,codexHome:$codexHome,electronUserData:$electronUserData,candidateRoot:$candidateRoot,evidenceRoot:$evidenceRoot,appPath:$appPath,protectedAppPath:$protectedAppPath,cockpitAppPath:$cockpitAppPath,cockpitBundleIdentifier:(if $cockpitBundleIdentifier == "" then null else $cockpitBundleIdentifier end),cockpitBundleShortVersion:(if $cockpitBundleShortVersion == "" then null else $cockpitBundleShortVersion end),cockpitBundleVersion:(if $cockpitBundleVersion == "" then null else $cockpitBundleVersion end),cockpitExecutableSHA256:(if $cockpitExecutableSHA == "" then null else $cockpitExecutableSHA end),codexAppExecutableSHA256:(if $appExecutableSHA == "" then null else $appExecutableSHA end),cockpitDataRoot:$cockpitDataRoot,cockpitStorePath:$cockpitStorePath,cockpitControlPlaneEnv:"COCKPIT_TOOLS_TEST_DATA_DIR",cockpitCodexHome:$codexHome,pathFences:{runRoot:{identity:$runRootIdentity},codexHome:{identity:$codexHomeIdentity},candidateRoot:{identity:$candidateRootIdentity},evidenceRoot:{identity:$evidenceRootIdentity},cockpitDataRoot:{identity:$cockpitDataIdentity},electronData:{identity:$electronDataIdentity}},sourceProvenance:{repoRoot:$sourceRepoRoot,repoRootIdentity:$sourceRepoIdentity,gitHead:(if $sourceGitHead == "" then null else $sourceGitHead end),snapshotSHA256:$sourceSnapshotSHA},fskitModuleProcessBaselinePath:$fskitModuleBaseline,fskitModuleProcessBaselineSHA256:$fskitModuleBaselineSHA,protectedProcessBaselinePath:$protectedProcessBaseline,protectedProcessBaselineSHA256:$protectedProcessBaselineSHA,launcherMode:"cockpit-compatible-adapter-prepared-only",cockpitLaunchContract:"real acceptance requires a fresh Cockpit UI codex_start_instance transition; adapter cannot fabricate positive launch state",candidateEvidenceContract:"v3 combined acceptance requires a live real mount, exact source/build manifest, exact Go daemon/build identity, the exact signed and registered Swift FSKit module process, a real task, exact backend crash/respawn, and reviewed native incident windows while that same candidate anchor remains attached",candidateAttached:false,candidateBuildSHA:null,candidateAppIdentitySHA256:null,candidateFSKitModuleSHA256:null,candidateBuildManifestSHA256:null,managedRouteObserved:false,candidateEvidenceSHA256:null,backendCrashRespawnEvidenceSHA256:null,nativeIncidentEvidenceSHA256:null,nativeIncidentReviewSHA256:null,workspace:$workspace,autoSyncThreads:false,cockpitToolsAvailable:$cockpitAvailable,cockpitAutoSyncThreadsBeforeRegistration:$cockpitSyncSafe}' \
    > "$RUN_ROOT/run.json"
  chmod 600 "$RUN_ROOT/run.json"
  printf '%s\n' "$SCHEMA" > "$RUN_ROOT/.codexfold-isolated-acceptance"
  chmod 600 "$RUN_ROOT/.codexfold-isolated-acceptance"

  jq -n \
    --arg id "$run_id" \
    --arg userDataDir "$CODEX_HOME_ISOLATED" \
    --arg workingDir "$WORKSPACE" \
    '{instances:[{id:$id,name:"CodexFold isolated acceptance",userDataDir:$userDataDir,workingDir:$workingDir,extraArgs:"",bindAccountId:null,launchMode:"app",lastLaunchedAt:null,lastPid:null}],defaultSettings:{autoSyncThreads:false,protectConfigOnLaunch:true,autoRepairSessionVisibilityOnLaunch:false}}' \
    > "$RUN_ROOT/cockpit-instance.json"
  chmod 600 "$RUN_ROOT/cockpit-instance.json"

  jq -n --arg codexAppPath "$APP_PATH" '{
    ws_enabled: true,
    startup_page: "codex-instances",
    startup_minimized: false,
    app_auto_launch_enabled: false,
    token_keeper_enabled: false,
    auto_import_from_local_enabled: false,
    antigravity_startup_wakeup_enabled: false,
    codex_startup_wakeup_enabled: false,
    codex_app_path: $codexAppPath,
    codex_specified_app_path: ""
  }' > "$COCKPIT_DATA_ROOT/config.json"
  chmod 600 "$COCKPIT_DATA_ROOT/config.json"

  runner=$SCRIPT_DIR/run-isolated-codex-acceptance.sh
  printf '#!/usr/bin/env bash\nset -euo pipefail\n%q cockpit-register --run-root %q --apply\nexec %q launch --run-root %q --apply "$@"\n' \
    "$runner" "$RUN_ROOT" "$runner" "$RUN_ROOT" > "$RUN_ROOT/launch-isolated-codex.sh"
  printf '#!/usr/bin/env bash\nset -euo pipefail\nexec %q cockpit-register --run-root %q "$@"\n' \
    "$runner" "$RUN_ROOT" > "$RUN_ROOT/register-cockpit-instance.sh"
  printf '#!/usr/bin/env bash\nset -euo pipefail\nexec /usr/bin/open -n --env %q --env %q %q\n' \
    "COCKPIT_TOOLS_TEST_DATA_DIR=$COCKPIT_DATA_ROOT" "CODEX_HOME=$CODEX_HOME_ISOLATED" "$COCKPIT_APP_PATH" > "$RUN_ROOT/open-isolated-cockpit.sh"
  # The generated script must retain its own positional parameters verbatim.
  # shellcheck disable=SC2016
  printf '#!/usr/bin/env bash\nset -euo pipefail\nexec %q observe-task --run-root %q --apply "$@"\n' \
    "$runner" "$RUN_ROOT" > "$RUN_ROOT/observe-real-task.sh"
  printf '#!/usr/bin/env bash\nset -euo pipefail\nexec %q candidate-evidence --run-root %q "$@"\n' \
    "$runner" "$RUN_ROOT" > "$RUN_ROOT/record-candidate-evidence.sh"
  printf '#!/usr/bin/env bash\nset -euo pipefail\nexec %q verify --run-root %q --require-real "$@"\n' \
    "$runner" "$RUN_ROOT" > "$RUN_ROOT/verify.sh"
  printf '#!/usr/bin/env bash\nset -euo pipefail\nexec %q fault --run-root %q "$@"\n' \
    "$runner" "$RUN_ROOT" > "$RUN_ROOT/inject-fault.sh"
  printf '#!/usr/bin/env bash\nset -euo pipefail\nexec %q incident-evidence --run-root %q "$@"\n' \
    "$runner" "$RUN_ROOT" > "$RUN_ROOT/record-native-incident-evidence.sh"
  printf '#!/usr/bin/env bash\nset -euo pipefail\nexec %q incident-review --run-root %q "$@"\n' \
    "$runner" "$RUN_ROOT" > "$RUN_ROOT/record-native-incident-review.sh"
  chmod 700 "$RUN_ROOT/launch-isolated-codex.sh" "$RUN_ROOT/register-cockpit-instance.sh" "$RUN_ROOT/open-isolated-cockpit.sh" "$RUN_ROOT/observe-real-task.sh" "$RUN_ROOT/record-candidate-evidence.sh" "$RUN_ROOT/verify.sh" "$RUN_ROOT/inject-fault.sh" "$RUN_ROOT/record-native-incident-evidence.sh" "$RUN_ROOT/record-native-incident-review.sh"

  load_run "$RUN_ROOT"
  command_verify --run-root "$RUN_ROOT" --prepared-only
  trap - EXIT HUP INT TERM
  echo "prepared isolated acceptance run: $RUN_ROOT"
  echo "launch harness: $RUN_ROOT/launch-isolated-codex.sh"
}

command_cockpit_register() {
  local run_root='' apply=false
  local result
  local adapter_args
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --run-root) run_root=${2:?}; shift 2 ;;
      --apply) apply=true; shift ;;
      --dry-run) apply=false; shift ;;
      -h|--help)
        echo 'cockpit-register options: --run-root DIR [--apply|--dry-run]'
        return 0
        ;;
      *) die "unknown cockpit-register option: $1" ;;
    esac
  done
  [[ -n "$run_root" ]] || die "cockpit-register requires --run-root"
  load_run "$run_root"
  adapter_args=(
    --store "$COCKPIT_STORE"
    --instance-id "$COCKPIT_INSTANCE_ID"
    --codex-home "$CODEX_HOME_ISOLATED"
    --working-dir "$WORKSPACE"
    --name 'CodexFold isolated acceptance'
    --backup "$RUN_ROOT/private/cockpit-store.before.json"
  )
  if [[ "$apply" != true ]]; then
    "$COCKPIT_ADAPTER" register "${adapter_args[@]}" --dry-run
    return 0
  fi
  result=$("$COCKPIT_ADAPTER" register "${adapter_args[@]}" --apply)
  load_cockpit_verification "$result"
  [[ "$COCKPIT_MANAGED" == true && "$COCKPIT_SYNC_FALSE" == true ]] || die "Cockpit registration did not persist the required isolation policy"
  [[ "$COCKPIT_ELECTRON_DATA" == "$ELECTRON_DATA" ]] || die "Cockpit Electron data derivation changed"
  printf '%s\n' "$result" > "$EVIDENCE_ROOT/cockpit-registration.json"
  chmod 600 "$EVIDENCE_ROOT/cockpit-registration.json"
  echo "prepared isolated Cockpit instance record: $COCKPIT_INSTANCE_ID"
}

command_launch() {
  local run_root='' apply=false wait_seconds=600
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --run-root) run_root=${2:?}; shift 2 ;;
      --wait-seconds) wait_seconds=${2:?}; shift 2 ;;
      --apply) apply=true; shift ;;
      --dry-run) apply=false; shift ;;
      -h|--help)
        echo 'launch options: --run-root DIR [--wait-seconds N] [--apply|--dry-run]'
        return 0
        ;;
      *) die "unknown launch option: $1" ;;
    esac
  done
  [[ -n "$run_root" ]] || die "launch requires --run-root"
  if [[ ! "$wait_seconds" =~ ^[0-9]+$ ]] || (( wait_seconds < 10 || wait_seconds > 1800 )); then
    die "--wait-seconds must be 10..1800"
  fi
  load_run "$run_root"
  [[ -d "$APP_PATH" && -f "$APP_PATH/Contents/Info.plist" ]] || die "Desktop app bundle is unavailable: $APP_PATH"
  cockpit_bundle_identifier=$(bundle_info_value "$COCKPIT_APP_PATH" CFBundleIdentifier)
  cockpit_bundle_short_version=$(bundle_info_value "$COCKPIT_APP_PATH" CFBundleShortVersionString)
  cockpit_bundle_version=$(bundle_info_value "$COCKPIT_APP_PATH" CFBundleVersion)
  cockpit_executable=$(bundle_executable_path "$COCKPIT_APP_PATH")
  app_executable=$(bundle_executable_path "$APP_PATH")
  cockpit_result=$(cockpit_verify_json)
  load_cockpit_verification "$cockpit_result"
  [[ "$COCKPIT_ELECTRON_DATA" == "$ELECTRON_DATA" ]] || die "Cockpit Electron data derivation changed"
  if [[ "$apply" != true ]]; then
    echo "dry-run: would open Cockpit with isolated COCKPIT_TOOLS_TEST_DATA_DIR and wait for its Start action"
    echo "dry-run: one click in the isolated Cockpit UI is required; this runner will not start Codex or write lastPid itself"
    return 0
  fi
  [[ "$COCKPIT_MANAGED" == true ]] || die "isolated instance is not persisted in Cockpit; run cockpit-register --apply first"
  [[ "$COCKPIT_SYNC_FALSE" == true ]] || die "Cockpit autoSyncThreads is not persistently disabled"
  [[ -n "$cockpit_bundle_identifier" ]] || die "Cockpit bundle has no CFBundleIdentifier"
  [[ "$cockpit_bundle_identifier" == "$COCKPIT_BUNDLE_IDENTIFIER" && \
     "$cockpit_bundle_short_version" == "$COCKPIT_BUNDLE_SHORT_VERSION" && \
     "$cockpit_bundle_version" == "$COCKPIT_BUNDLE_VERSION" ]] || die "Cockpit bundle identity/version changed since prepare"
  [[ -n "$COCKPIT_EXECUTABLE_SHA" && "$(file_sha "$cockpit_executable")" == "$COCKPIT_EXECUTABLE_SHA" ]] || die "Cockpit executable bytes changed since prepare"
  [[ -n "$CODEX_APP_EXECUTABLE_SHA" && "$(file_sha "$app_executable")" == "$CODEX_APP_EXECUTABLE_SHA" ]] || die "Codex Desktop executable bytes changed since prepare"
  need_command codesign
  codesign --verify --deep --strict "$COCKPIT_APP_PATH" >/dev/null 2>&1 || die "Cockpit bundle signature verification failed"
  codesign --verify --deep --strict "$APP_PATH" >/dev/null 2>&1 || die "Codex Desktop bundle signature verification failed"
  open_help=$(/usr/bin/open -h 2>&1 || true)
  grep -F -- '--env' <<< "$open_help" >/dev/null || die "this macOS open(1) cannot pass a per-launch environment safely"

  current="$EVIDENCE_ROOT/processes.pre-launch.tsv"
  write_current_snapshot "$current"
  verify_protected_processes "$PROTECTED_PROCESS_BASELINE" "$current"
  [[ "$PROTECTED_UNCHANGED" == true ]] || die "protected Codex processes changed since prepare; create a fresh run before launching"

  [[ -d "$COCKPIT_APP_PATH" ]] || die "Cockpit Tools app is unavailable: $COCKPIT_APP_PATH"
  initial_last_pid=$COCKPIT_LAST_PID
  initial_last_launched=$COCKPIT_LAST_LAUNCHED
  [[ -z "$initial_last_pid" && -z "$initial_last_launched" ]] || die "isolated Cockpit record already contains launch state; create a fresh run"
  launch_requested_at=$(epoch_millis)
  server_file="$COCKPIT_DATA_ROOT/server.json"
  [[ ! -e "$server_file" && ! -L "$server_file" ]] || die "isolated Cockpit server evidence already exists; create a fresh run"
  /usr/bin/open -n \
    --env "COCKPIT_TOOLS_TEST_DATA_DIR=$COCKPIT_DATA_ROOT" \
    --env "CODEX_HOME=$CODEX_HOME_ISOLATED" \
    "$COCKPIT_APP_PATH"

  server_deadline=$((SECONDS + 20))
  cockpit_pid=''
  while (( SECONDS < server_deadline )); do
    if [[ -f "$server_file" && ! -L "$server_file" ]]; then
      cockpit_pid=$(jq -r '.pid // empty' "$server_file" 2>/dev/null || true)
      if [[ "$cockpit_pid" =~ ^[0-9]+$ ]] && [[ -n "$(process_start "$cockpit_pid")" ]] && \
         [[ "$(process_executable_path "$cockpit_pid" 2>/dev/null || true)" == "$cockpit_executable" ]] && \
         process_environment_has_exact "$cockpit_pid" COCKPIT_TOOLS_TEST_DATA_DIR "$COCKPIT_DATA_ROOT" && \
         process_environment_has_exact "$cockpit_pid" CODEX_HOME "$CODEX_HOME_ISOLATED"; then
        break
      fi
    fi
    sleep 1
  done
  if [[ ! "$cockpit_pid" =~ ^[0-9]+$ ]]; then
    die "isolated Cockpit did not start; an existing Cockpit singleton may have intercepted the launch. This runner will not stop it."
  fi
  cockpit_started_at=$(jq -r '.started_at // empty' "$server_file")
  cockpit_version=$(jq -r '.version // empty' "$server_file")
  cockpit_process_start=$(process_start "$cockpit_pid")
  cockpit_command_sha=$(process_command_sha "$cockpit_pid")
  echo "Cockpit 已打开到 Codex Instances。请只在 ‘CodexFold isolated acceptance’ 这一行点击一次 Start；其余证据会自动收集。"

  deadline=$((SECONDS + wait_seconds))
  observed_last_launched=''
  observed_store_pid=''
  while (( SECONDS < deadline )); do
    cockpit_result=$(cockpit_verify_json)
    load_cockpit_verification "$cockpit_result"
    write_current_snapshot "$EVIDENCE_ROOT/processes.after-launch.tsv"
    desktop_pid=$(awk -F '\t' '$3=="desktop" && $4=="true" && $6=="true" {print $1; exit}' "$EVIDENCE_ROOT/processes.after-launch.tsv")
    app_server_pid=$(awk -F '\t' '$3=="app-server" && $4=="true" && $5=="true" {print $1; exit}' "$EVIDENCE_ROOT/processes.after-launch.tsv")
    observed_last_launched=$(jq -r '.lastLaunchedAt // empty' <<< "$cockpit_result")
    observed_store_pid=$COCKPIT_LAST_PID
    if [[ "$desktop_pid" =~ ^[0-9]+$ && "$app_server_pid" =~ ^[0-9]+$ ]] && \
       [[ "$observed_store_pid" == "$desktop_pid" ]] && \
       [[ "$observed_last_launched" =~ ^[0-9]+$ ]] && \
       (( observed_last_launched >= launch_requested_at )); then
      break
    fi
    sleep 1
  done
  [[ "$desktop_pid" =~ ^[0-9]+$ && "$observed_store_pid" == "$desktop_pid" ]] || die "Cockpit did not persist a matching isolated Desktop PID before timeout"
  [[ "$app_server_pid" =~ ^[0-9]+$ ]] || die "isolated Cockpit start did not produce a bound app-server before timeout"
  ancestor_reaches "$app_server_pid" "$desktop_pid" || die "isolated app-server ancestry does not lead to Cockpit's Desktop PID"
  desktop_start=$(process_start "$desktop_pid")
  app_server_start=$(process_start "$app_server_pid")
  printf '%s\n' "$(timestamp)" > "$EVIDENCE_ROOT/launch.applied"
  chmod 600 "$EVIDENCE_ROOT/launch.applied"
  jq -n \
    --arg source 'cockpit-codex_start_instance' \
    --arg observedAt "$(timestamp)" \
    --arg instanceId "$COCKPIT_INSTANCE_ID" \
    --argjson cockpitPid "$cockpit_pid" \
    --arg cockpitStartedAt "$cockpit_started_at" \
    --arg cockpitProcessStart "$cockpit_process_start" \
    --arg cockpitCommandSHA256 "$cockpit_command_sha" \
    --arg cockpitServerVersion "$cockpit_version" \
    --arg cockpitBundleIdentifier "$cockpit_bundle_identifier" \
    --arg cockpitBundleShortVersion "$cockpit_bundle_short_version" \
    --arg cockpitBundleVersion "$cockpit_bundle_version" \
    --argjson desktopPid "$desktop_pid" \
    --argjson appServerPid "$app_server_pid" \
    --arg desktopProcessStart "$desktop_start" \
    --arg appServerProcessStart "$app_server_start" \
    --argjson lastLaunchedAt "$observed_last_launched" \
    '{source:$source,observedAt:$observedAt,instanceId:$instanceId,cockpitPid:$cockpitPid,cockpitStartedAt:$cockpitStartedAt,cockpitProcessStart:$cockpitProcessStart,cockpitCommandSHA256:$cockpitCommandSHA256,cockpitServerVersion:$cockpitServerVersion,cockpitBundleIdentifier:$cockpitBundleIdentifier,cockpitBundleShortVersion:(if $cockpitBundleShortVersion == "" then null else $cockpitBundleShortVersion end),cockpitBundleVersion:(if $cockpitBundleVersion == "" then null else $cockpitBundleVersion end),desktopPid:$desktopPid,desktopProcessStart:$desktopProcessStart,appServerPid:$appServerPid,appServerProcessStart:$appServerProcessStart,lastLaunchedAt:$lastLaunchedAt}' \
    > "$EVIDENCE_ROOT/cockpit-command-observed.json"
  chmod 600 "$EVIDENCE_ROOT/cockpit-command-observed.json"
  command_verify --run-root "$RUN_ROOT"
}

command_task() {
  local run_root='' prompt_file='' apply=false sandbox=read-only codex_bin=''
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --run-root) run_root=${2:?}; shift 2 ;;
      --prompt-file) prompt_file=${2:?}; shift 2 ;;
      --sandbox) sandbox=${2:?}; shift 2 ;;
      --codex-bin) codex_bin=${2:?}; shift 2 ;;
      --apply) apply=true; shift ;;
      --dry-run) apply=false; shift ;;
      -h|--help)
        echo 'task options: --run-root DIR --prompt-file FILE [--sandbox read-only|workspace-write] [--codex-bin PATH] [--apply]'
        return 0
        ;;
      *) die "unknown task option: $1" ;;
    esac
  done
  [[ -n "$run_root" && -n "$prompt_file" ]] || die "task requires --run-root and --prompt-file"
  [[ "$sandbox" == read-only || "$sandbox" == workspace-write ]] || die "task sandbox must be read-only or workspace-write"
  [[ -f "$prompt_file" ]] || die "prompt file does not exist: $prompt_file"
  load_run "$run_root"
  if [[ -z "$codex_bin" ]]; then
    codex_bin=$(command -v codex || true)
  fi
  [[ -x "$codex_bin" ]] || die "Codex CLI is unavailable; pass --codex-bin"
  if [[ "$apply" != true ]]; then
    echo "dry-run: would run one Codex task in isolated CODEX_HOME with sandbox=$sandbox"
    return 0
  fi
  [[ -f "$EVIDENCE_ROOT/cockpit-command-observed.json" ]] || die "real task requires an observed actual Cockpit Start first"
  task_mode_lock="$EVIDENCE_ROOT/.task-mode.lock"
  mkdir "$task_mode_lock" 2>/dev/null || die "another CLI task or Desktop task observer is already active"
  printf 'cli-diagnostic\n' > "$task_mode_lock/mode"
  trap 'rm -rf "$task_mode_lock"' EXIT HUP INT TERM

  write_current_snapshot "$EVIDENCE_ROOT/processes.pre-task.tsv"
  verify_protected_processes "$EVIDENCE_ROOT/protected-processes.before.tsv" "$EVIDENCE_ROOT/processes.pre-task.tsv"
  [[ "$PROTECTED_UNCHANGED" == true ]] || die "protected Codex processes changed since prepare; refusing task"

  started=$(timestamp)
  set +e
  CODEX_HOME="$CODEX_HOME_ISOLATED" \
  CODEX_ELECTRON_USER_DATA_PATH="$ELECTRON_DATA" \
  CODEX_AUTO_SYNC_THREADS=false \
    "$codex_bin" exec -C "$WORKSPACE" -s "$sandbox" -a never --color never \
      -o "$RUN_ROOT/private/cli-task-last-message.txt" - < "$prompt_file" \
      > "$RUN_ROOT/private/cli-task-events.log" 2>&1 &
  task_pid=$!
  task_start=$(process_start "$task_pid")
  wait "$task_pid"
  task_exit=$?
  set -e
  chmod 600 "$RUN_ROOT/private/cli-task-events.log" "$RUN_ROOT/private/cli-task-last-message.txt" 2>/dev/null || true
  task_status=passed
  (( task_exit == 0 )) || task_status=failed
  jq -n --arg status "$task_status" --arg startedAt "$started" --arg completedAt "$(timestamp)" \
    --arg pid "$task_pid" --arg processStart "$task_start" --argjson exitCode "$task_exit" \
    '{status:$status,startedAt:$startedAt,completedAt:$completedAt,pid:($pid|tonumber),processStart:$processStart,exitCode:$exitCode}' \
    > "$EVIDENCE_ROOT/task-result.json"
  chmod 600 "$EVIDENCE_ROOT/task-result.json"
  command_verify --run-root "$RUN_ROOT"
  rm -rf "$task_mode_lock"
  trap - EXIT HUP INT TERM
  (( task_exit == 0 )) || return "$task_exit"
}

count_added_or_changed_rollout_identities() {
  local before=$1 current=$2
  awk -F '\t' '
    NR == FNR { before[$1] = $2 FS $3; next }
    !($1 in before) || before[$1] != ($2 FS $3) { count++ }
    END { print count + 0 }
  ' "$before" "$current"
}

command_observe_task() {
  local run_root='' apply=false wait_seconds=1800
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --run-root) run_root=${2:?}; shift 2 ;;
      --wait-seconds) wait_seconds=${2:?}; shift 2 ;;
      --apply) apply=true; shift ;;
      --dry-run) apply=false; shift ;;
      -h|--help)
        echo 'observe-task options: --run-root DIR [--wait-seconds 60..7200] [--apply]'
        return 0
        ;;
      *) die "unknown observe-task option: $1" ;;
    esac
  done
  [[ -n "$run_root" ]] || die "observe-task requires --run-root"
  if [[ ! "$wait_seconds" =~ ^[0-9]+$ ]] || (( wait_seconds < 60 || wait_seconds > 7200 )); then
    die "--wait-seconds must be 60..7200"
  fi
  load_run "$run_root"
  [[ -f "$EVIDENCE_ROOT/cockpit-command-observed.json" ]] || die "real task observation requires an observed actual Cockpit Start"
  if [[ "$apply" != true ]]; then
    echo "dry-run: would wait for rollout/SQLite changes produced by a real task in the Cockpit-started Desktop"
    return 0
  fi

  [[ ! -e "$EVIDENCE_ROOT/real-task-observed.json" && ! -e "$EVIDENCE_ROOT/real-task.before.tsv" && ! -e "$EVIDENCE_ROOT/real-task.current.tsv" ]] || \
    die "real task evidence already exists; create a fresh run"
  task_mode_lock="$EVIDENCE_ROOT/.task-mode.lock"
  mkdir "$task_mode_lock" 2>/dev/null || die "a CLI diagnostic or another Desktop observer is already active"
  printf 'desktop-observer\n' > "$task_mode_lock/mode"
  trap 'rm -rf "$task_mode_lock"' EXIT HUP INT TERM

  command_verify --run-root "$RUN_ROOT"
  [[ "$(jq -r '.cockpitCommandObserved' "$EVIDENCE_ROOT/verification.json")" == true ]] || die "actual Cockpit Start is not currently valid"
  verify_candidate_evidence
  [[ "$CANDIDATE_EVIDENCE_VALID" == true && "$CANDIDATE_ATTACHED" == true && "$MANAGED_ROUTE_OBSERVED" == true ]] || \
    die "real Desktop task observation requires a live validated CodexFold candidate before the baseline"
  baseline_candidate_evidence_sha=$(file_sha "$EVIDENCE_ROOT/codexfold-candidate-observed.json")
  baseline_candidate_pid=$VALIDATED_CANDIDATE_PID
  baseline_candidate_start=$VALIDATED_CANDIDATE_PROCESS_START
  baseline_candidate_binary=$VALIDATED_CANDIDATE_BINARY
  baseline_candidate_build=$VALIDATED_CANDIDATE_SHA
  baseline_candidate_mount=$VALIDATED_CANDIDATE_MOUNT
  baseline_candidate_mount_identity=$VALIDATED_CANDIDATE_MOUNT_IDENTITY
  baseline_candidate_command_sha=$VALIDATED_CANDIDATE_COMMAND_SHA
  observer_processes="$EVIDENCE_ROOT/processes.observer-start.tsv"
  write_current_snapshot "$observer_processes"
  observer_desktop_pid=$(awk -F '\t' '$3=="desktop" && $4=="true" && $6=="true" {print $1; exit}' "$observer_processes")
  observer_app_server_pid=$(awk -F '\t' '$3=="app-server" && $4=="true" && $5=="true" {print $1; exit}' "$observer_processes")
  [[ "$observer_desktop_pid" =~ ^[0-9]+$ && "$observer_app_server_pid" =~ ^[0-9]+$ ]] || die "isolated Desktop/app-server are not both live before observation"
  ancestor_reaches "$observer_app_server_pid" "$observer_desktop_pid" || die "isolated app-server ancestry is invalid before observation"
  observer_desktop_start=$(process_start "$observer_desktop_pid")
  observer_app_server_start=$(process_start "$observer_app_server_pid")

  before="$EVIDENCE_ROOT/real-task.before.tsv"
  current="$EVIDENCE_ROOT/real-task.current.tsv"
  snapshot_rollout_identities "$before" || die "could not obtain a complete stable rollout/SQLite baseline"
  before_snapshot_sha=$(file_sha "$before")
  before_count=$(wc -l < "$before" | tr -d ' ')
  started_at=$(timestamp)
  deadline=$((SECONDS + wait_seconds))
  changed=false
  while (( SECONDS < deadline )); do
    write_current_snapshot "$EVIDENCE_ROOT/processes.real-task.tsv"
    verify_protected_processes "$EVIDENCE_ROOT/protected-processes.before.tsv" "$EVIDENCE_ROOT/processes.real-task.tsv"
    [[ "$PROTECTED_UNCHANGED" == true ]] || die "production Codex changed while waiting for the real task"
    current_desktop_pid=$(awk -F '\t' '$3=="desktop" && $4=="true" && $6=="true" {print $1; exit}' "$EVIDENCE_ROOT/processes.real-task.tsv")
    current_app_server_pid=$(awk -F '\t' '$3=="app-server" && $4=="true" && $5=="true" {print $1; exit}' "$EVIDENCE_ROOT/processes.real-task.tsv")
    [[ "$current_desktop_pid" == "$observer_desktop_pid" && "$current_app_server_pid" == "$observer_app_server_pid" ]] || die "isolated Desktop/app-server identity changed during task observation"
    [[ "$(process_start "$current_desktop_pid")" == "$observer_desktop_start" && "$(process_start "$current_app_server_pid")" == "$observer_app_server_start" ]] || die "isolated Desktop/app-server restarted during task observation"
    ancestor_reaches "$current_app_server_pid" "$current_desktop_pid" || die "isolated app-server ancestry changed during task observation"
    verify_candidate_evidence
    [[ "$CANDIDATE_EVIDENCE_VALID" == true ]] || die "candidate became unverifiable during task observation"
    [[ "$(file_sha "$EVIDENCE_ROOT/codexfold-candidate-observed.json")" == "$baseline_candidate_evidence_sha" && \
       "$VALIDATED_CANDIDATE_PID" == "$baseline_candidate_pid" && \
       "$VALIDATED_CANDIDATE_PROCESS_START" == "$baseline_candidate_start" && \
       "$VALIDATED_CANDIDATE_BINARY" == "$baseline_candidate_binary" && \
       "$VALIDATED_CANDIDATE_SHA" == "$baseline_candidate_build" && \
       "$VALIDATED_CANDIDATE_MOUNT" == "$baseline_candidate_mount" && \
       "$VALIDATED_CANDIDATE_MOUNT_IDENTITY" == "$baseline_candidate_mount_identity" && \
       "$VALIDATED_CANDIDATE_COMMAND_SHA" == "$baseline_candidate_command_sha" ]] || die "candidate identity changed during task observation"
    snapshot_rollout_identities "$current" || die "could not obtain a complete stable rollout/SQLite snapshot"
    changed_count=$(count_added_or_changed_rollout_identities "$before" "$current")
    if (( changed_count > 0 )); then
      changed=true
      break
    fi
    sleep 2
  done
  [[ "$changed" == true ]] || die "no real Desktop task mutation was observed before timeout"
  after_count=$(wc -l < "$current" | tr -d ' ')
  current_snapshot_sha=$(file_sha "$current")
  jq -n \
    --arg source actual-cockpit-ui \
    --arg startedAt "$started_at" \
    --arg observedAt "$(timestamp)" \
    --argjson beforeThreads "$before_count" \
    --argjson afterThreads "$after_count" \
    --argjson changedIdentityRows "$changed_count" \
    --arg candidateEvidenceSHA256 "$baseline_candidate_evidence_sha" \
    --argjson candidateDaemonPid "$baseline_candidate_pid" \
    --arg candidateDaemonStart "$baseline_candidate_start" \
    --arg candidateBinaryPath "$baseline_candidate_binary" \
    --arg candidateBuildSHA "$baseline_candidate_build" \
    --arg candidateMountPoint "$baseline_candidate_mount" \
    --arg candidateMountIdentity "$baseline_candidate_mount_identity" \
    --arg candidateCommandSHA256 "$baseline_candidate_command_sha" \
    --argjson desktopPid "$observer_desktop_pid" \
    --arg desktopProcessStart "$observer_desktop_start" \
    --argjson appServerPid "$observer_app_server_pid" \
    --arg appServerProcessStart "$observer_app_server_start" \
    --arg currentSnapshotSHA256 "$current_snapshot_sha" \
    --arg beforeSnapshotSHA256 "$before_snapshot_sha" \
    '{source:$source,startedAt:$startedAt,observedAt:$observedAt,beforeThreads:$beforeThreads,afterThreads:$afterThreads,changedIdentityRows:$changedIdentityRows,candidateEvidenceSHA256:$candidateEvidenceSHA256,candidateDaemonPid:$candidateDaemonPid,candidateDaemonStart:$candidateDaemonStart,candidateBinaryPath:$candidateBinaryPath,candidateBuildSHA:$candidateBuildSHA,candidateMountPoint:$candidateMountPoint,candidateMountIdentity:$candidateMountIdentity,candidateCommandSHA256:$candidateCommandSHA256,desktopPid:$desktopPid,desktopProcessStart:$desktopProcessStart,appServerPid:$appServerPid,appServerProcessStart:$appServerProcessStart,beforeSnapshotSHA256:$beforeSnapshotSHA256,currentSnapshotSHA256:$currentSnapshotSHA256}' \
    > "$EVIDENCE_ROOT/real-task-observed.json"
  chmod 600 "$EVIDENCE_ROOT/real-task-observed.json"
  command_verify --run-root "$RUN_ROOT" --require-real
  rm -rf "$task_mode_lock"
  trap - EXIT HUP INT TERM
}

command_candidate_evidence() {
  local run_root='' input='' apply=false
  local evidence=''
  local evidence_tmp run_tmp evidence_sha input_copy input_identity_before input_identity_after input_sha_before input_sha_after input_schema
  local module_snapshot_tmp='' module_snapshot=''
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --run-root) run_root=${2:?}; shift 2 ;;
      --input) input=${2:?}; shift 2 ;;
      --apply) apply=true; shift ;;
      --dry-run) apply=false; shift ;;
      -h|--help)
        cat <<EOF
candidate-evidence options: --run-root DIR --input FILE [--apply|--dry-run]

FILE must use schema $CANDIDATE_INPUT_SCHEMA and come from an externally
attached real CodexFold candidate. The runner verifies the prepared source
snapshot and build manifest, the signed candidate App and its exact nested
Swift FSKit module, the module's exact registered path and live process identity,
the live Go daemon/build identity, the exact candidate-local mount linked from
both isolated session namespaces, and at least one current isolated SQLite
rollout route with exact bytes/SHA-256. Unknown input fields are never copied
into retained evidence.
EOF
        return 0
        ;;
      *) die "unknown candidate-evidence option: $1" ;;
    esac
  done
  [[ -n "$run_root" && -n "$input" ]] || die "candidate-evidence requires --run-root and --input"
  load_run "$run_root"
  evidence="$EVIDENCE_ROOT/codexfold-candidate-observed.json"
  module_snapshot="$EVIDENCE_ROOT/fskit-module-processes.after.tsv"
  [[ ! -e "$evidence" && ! -e "$module_snapshot" && -z "$RUN_CANDIDATE_EVIDENCE_SHA" && \
     -z "$RUN_BACKEND_CRASH_EVIDENCE_SHA" && -z "$RUN_NATIVE_INCIDENT_EVIDENCE_SHA" && -z "$RUN_NATIVE_INCIDENT_REVIEW_SHA" ]] || \
    die "candidate anchor evidence already exists and is immutable; create a fresh v3 run for another candidate"
  need_command codesign
  need_command jq
  need_command pluginkit
  need_command plutil
  need_command sqlite3
  need_command shasum
  [[ -f "$input" && ! -L "$input" ]] || die "candidate evidence input must be a regular non-symlink file"
  input_identity_before=$(stat -f '%d:%i:%z:%m:%c:%p' "$input")
  input_sha_before=$(file_sha "$input")
  input_copy=$(mktemp "$RUN_ROOT/private/.candidate-input.XXXXXX")
  cp -p "$input" "$input_copy"
  chmod 600 "$input_copy"
  input_identity_after=$(stat -f '%d:%i:%z:%m:%c:%p' "$input")
  input_sha_after=$(file_sha "$input")
  [[ "$input_identity_before" == "$input_identity_after" && "$input_sha_before" == "$input_sha_after" && \
     "$(file_sha "$input_copy")" == "$input_sha_before" ]] || {
    rm -f "$input_copy"
    die "candidate evidence input changed while it was copied"
  }
  trap 'rm -f "$input_copy" "${module_snapshot_tmp:-}"' EXIT HUP INT TERM

  input_schema=$(jq -r '.schema // empty' "$input_copy" 2>/dev/null || true)
  if [[ "$input_schema" != "$CANDIDATE_INPUT_SCHEMA" ]]; then
    case "$input_schema" in
      codexfold.external-candidate-evidence.v1|codexfold.external-candidate-evidence.v2)
        die "legacy candidate evidence $input_schema cannot prove the loaded Swift FSKit module; recapture $CANDIDATE_INPUT_SCHEMA evidence"
        ;;
      *) die "unsupported external candidate evidence schema: $input_schema" ;;
    esac
  fi
  if ! validate_external_candidate_input "$input_copy"; then
    die "${CANDIDATE_VALIDATION_ERROR:-external candidate evidence did not prove the live candidate build and managed route for this run}"
  fi

  if [[ "$apply" != true ]]; then
    echo "dry-run: validated external candidate build SHA-256 and $VALIDATED_ROUTE_COUNT managed route(s); no evidence was retained"
    rm -f "$input_copy"
    trap - EXIT HUP INT TERM
    return 0
  fi

  write_current_snapshot "$EVIDENCE_ROOT/processes.pre-candidate-evidence.tsv"
  verify_protected_processes "$EVIDENCE_ROOT/protected-processes.before.tsv" "$EVIDENCE_ROOT/processes.pre-candidate-evidence.tsv"
  [[ "$PROTECTED_UNCHANGED" == true ]] || die "protected Codex processes changed since prepare; refusing candidate evidence"
  module_snapshot_tmp=$(mktemp "$EVIDENCE_ROOT/.fskit-module-processes.after.XXXXXX")
  if ! validate_external_candidate_input "$input_copy" "$module_snapshot_tmp"; then
    die "${CANDIDATE_VALIDATION_ERROR:-candidate identity changed before evidence commit}"
  fi

  evidence_tmp=$(mktemp "$EVIDENCE_ROOT/.candidate-evidence.XXXXXX")
  jq \
    --arg schema "$CANDIDATE_EVIDENCE_SCHEMA" \
    --arg source external-real-codexfold-candidate \
    --arg recordedAt "$(timestamp)" \
    --arg daemonProcessStart "$VALIDATED_CANDIDATE_PROCESS_START" \
    --arg daemonCommandSHA256 "$VALIDATED_CANDIDATE_COMMAND_SHA" \
    --arg daemonExecutablePath "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE" \
    --arg daemonExecutableSHA256 "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE_SHA" \
    --arg serviceDefinitionSHA256 "$VALIDATED_CANDIDATE_DEFINITION_SHA" \
    --arg mountIdentity "$VALIDATED_CANDIDATE_MOUNT_IDENTITY" \
    --arg backendStatusPath "$VALIDATED_BACKEND_STATUS_PATH" \
    --arg backendStatusSHA256 "$(file_sha "$VALIDATED_BACKEND_STATUS_PATH")" \
    --arg backendID "$VALIDATED_BACKEND_ID" \
    --arg candidateAppIdentity "$VALIDATED_CANDIDATE_APP_IDENTITY" \
    --arg moduleSnapshotSHA "$VALIDATED_CANDIDATE_MODULE_SNAPSHOT_SHA" \
    --arg moduleRegisteredPathSHA "$(printf '%s' "$VALIDATED_CANDIDATE_MODULE" | text_sha)" '
      {
        schema:$schema,
        source:$source,
        recordedAt:$recordedAt,
        observedAt:.observedAt,
        codexHome:.codexHome,
        candidateRoot:.candidateRoot,
        mountPoint:.mountPoint,
        mountIdentity:$mountIdentity,
        candidateAttached:true,
        candidateBinaryPath:.candidateBinaryPath,
        candidateBuildSHA:.candidateBuildSHA,
        sourceProvenance:{
          repoRoot:.sourceProvenance.repoRoot,
          repoRootIdentity:.sourceProvenance.repoRootIdentity,
          gitHead:.sourceProvenance.gitHead,
          snapshotSHA256:.sourceProvenance.snapshotSHA256
        },
        buildManifest:{path:.buildManifest.path,sha256:.buildManifest.sha256},
        candidateApp:{
          path:.candidateApp.path,
          identitySHA256:$candidateAppIdentity,
          directoryIdentity:.candidateApp.directoryIdentity,
          bundleIdentifier:.candidateApp.bundleIdentifier,
          shortVersion:.candidateApp.shortVersion,
          bundleVersion:.candidateApp.bundleVersion,
          executablePath:.candidateApp.executablePath,
          executableSHA256:.candidateApp.executableSHA256,
          codeDirectoryHash:.candidateApp.codeDirectoryHash,
          teamIdentifier:.candidateApp.teamIdentifier
        },
        fskitModule:{
          bundlePath:.fskitModule.bundlePath,
          directoryIdentity:.fskitModule.directoryIdentity,
          bundleIdentifier:.fskitModule.bundleIdentifier,
          shortVersion:.fskitModule.shortVersion,
          bundleVersion:.fskitModule.bundleVersion,
          fsShortName:.fskitModule.fsShortName,
          executablePath:.fskitModule.executablePath,
          executableSHA256:.fskitModule.executableSHA256,
          codeDirectoryHash:.fskitModule.codeDirectoryHash,
          teamIdentifier:.fskitModule.teamIdentifier,
          registration:{exactPathMatched:true,pathSHA256:$moduleRegisteredPathSHA},
          process:{
            pid:.fskitModule.process.pid,
            ppid:.fskitModule.process.ppid,
            processStart:.fskitModule.process.processStart,
            executablePath:.fskitModule.process.executablePath,
            executableSHA256:.fskitModule.process.executableSHA256,
            commandSHA256:.fskitModule.process.commandSHA256,
            baselineSHA256:.fskitModule.process.baselineSHA256,
            snapshotSHA256:$moduleSnapshotSHA
          }
        },
        serviceDefinitionPath:.serviceDefinitionPath,
        serviceDefinitionSHA256:$serviceDefinitionSHA256,
        backendStatus:{path:$backendStatusPath,initialSHA256:$backendStatusSHA256,backendID:$backendID},
        daemonPid:.serviceStatus.daemon_pid,
        daemonProcessStart:$daemonProcessStart,
        daemonCommandSHA256:$daemonCommandSHA256,
        daemonExecutablePath:$daemonExecutablePath,
        daemonExecutableSHA256:$daemonExecutableSHA256,
        serviceStatus:{
          daemonRunning:.serviceStatus.daemon_running,
          mountHealthy:.serviceStatus.mount_healthy,
          buildHealthy:.serviceStatus.build.healthy,
          runningBuildSHA256:.serviceStatus.build.running_build_sha256,
          configuredBuildSHA256:.serviceStatus.build.configured_build_sha256,
          configuredBinaryPath:.serviceStatus.build.configured_binary_path
        },
        managedRouteObserved:true,
        managedRoutes:[.managedRoutes[] | {sessionId,rolloutPath,bytes,sha256}],
        faultTargets:(.faultTargets // {})
      }
    ' "$input_copy" > "$evidence_tmp"
  chmod 600 "$evidence_tmp"
  mv "$evidence_tmp" "$evidence"
  mv "$module_snapshot_tmp" "$module_snapshot"
  module_snapshot_tmp=''
  evidence_sha=$(shasum -a 256 "$evidence" | awk '{print $1}')

  run_tmp=$(mktemp "$RUN_ROOT/.run.XXXXXX")
  jq \
    --arg buildSHA "$VALIDATED_CANDIDATE_SHA" \
    --arg appIdentity "$VALIDATED_CANDIDATE_APP_IDENTITY" \
    --arg moduleSHA "$VALIDATED_CANDIDATE_MODULE_EXECUTABLE_SHA" \
    --arg buildManifestSHA "$VALIDATED_CANDIDATE_BUILD_MANIFEST_SHA" \
    --arg evidenceSHA "$evidence_sha" '
      .candidateAttached = true |
      .candidateBuildSHA = $buildSHA |
      .candidateAppIdentitySHA256 = $appIdentity |
      .candidateFSKitModuleSHA256 = $moduleSHA |
      .candidateBuildManifestSHA256 = $buildManifestSHA |
      .managedRouteObserved = true |
      .candidateEvidenceSHA256 = $evidenceSHA
    ' "$RUN_ROOT/run.json" > "$run_tmp"
  chmod 600 "$run_tmp"
  mv "$run_tmp" "$RUN_ROOT/run.json"
  command_verify --run-root "$RUN_ROOT"
  rm -f "$input_copy"
  trap - EXIT HUP INT TERM
}

command_incident_evidence() {
  local run_root='' input='' apply=false input_path input_identity_before input_identity_after input_sha_before input_sha_after
  local observed observed_tmp observed_sha run_tmp
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --run-root) run_root=${2:?}; shift 2 ;;
      --input) input=${2:?}; shift 2 ;;
      --apply) apply=true; shift ;;
      --dry-run) apply=false; shift ;;
      -h|--help)
        echo 'incident-evidence options: --run-root DIR --input EVIDENCE_ROOT/native-incident/observation-input.json [--apply|--dry-run]'
        return 0
        ;;
      *) die "unknown incident-evidence option: $1" ;;
    esac
  done
  [[ -n "$run_root" && -n "$input" ]] || die "incident-evidence requires --run-root and --input"
  load_run "$run_root"
  observed="$EVIDENCE_ROOT/native-incident-observed.json"
  [[ ! -e "$observed" && -z "$RUN_NATIVE_INCIDENT_EVIDENCE_SHA" ]] || die "native incident evidence already exists and is immutable"
  input_path=$(canonical_evidence_file "$input") || die "native incident observation input must be a regular file inside evidenceRoot"
  [[ "$input_path" == "$EVIDENCE_ROOT/native-incident/observation-input.json" ]] || die "native incident observation input path is not the fixed staging path"
  [[ "$(jq -r '.schema // empty' "$input_path")" == "$NATIVE_INCIDENT_INPUT_SCHEMA" ]] || die "unsupported native incident observation schema"
  verify_candidate_evidence
  [[ "$CANDIDATE_EVIDENCE_VALID" == true && "$BACKEND_CRASH_RESPAWN_EVIDENCE_VALID" == true ]] || \
    die "native incident evidence requires a live candidate and valid backend crash/respawn evidence"
  input_identity_before=$(stat -f '%d:%i:%z:%m:%c:%p' "$input_path")
  input_sha_before=$(file_sha "$input_path")
  observed_tmp=$(mktemp "$EVIDENCE_ROOT/.native-incident.XXXXXX")
  jq --arg schema "$NATIVE_INCIDENT_EVIDENCE_SCHEMA" --arg recordedAt "$(timestamp)" \
    '.schema=$schema | .recordedAt=$recordedAt' "$input_path" > "$observed_tmp"
  chmod 400 "$observed_tmp"
  input_identity_after=$(stat -f '%d:%i:%z:%m:%c:%p' "$input_path")
  input_sha_after=$(file_sha "$input_path")
  [[ "$input_identity_before" == "$input_identity_after" && "$input_sha_before" == "$input_sha_after" ]] || {
    rm -f "$observed_tmp"
    die "native incident observation input changed while it was retained"
  }
  validate_native_incident_evidence "$observed_tmp" precommit || {
    rm -f "$observed_tmp"
    die "native incident census, screenshots, exports, process fences, or occurrence binding are invalid"
  }
  if [[ "$apply" != true ]]; then
    rm -f "$observed_tmp"
    echo "dry-run: validated native incident evidence; nothing was retained"
    return 0
  fi
  mv "$observed_tmp" "$observed"
  observed_sha=$(file_sha "$observed")
  run_tmp=$(mktemp "$RUN_ROOT/.run.XXXXXX")
  jq --arg sha "$observed_sha" '.nativeIncidentEvidenceSHA256=$sha' "$RUN_ROOT/run.json" > "$run_tmp"
  chmod 600 "$run_tmp"
  mv "$run_tmp" "$RUN_ROOT/run.json"
  load_run "$RUN_ROOT"
  verify_candidate_evidence
  validate_native_incident_evidence "$observed" committed || die "committed native incident evidence did not revalidate"
  command_verify --run-root "$RUN_ROOT"
}

command_incident_review() {
  local run_root='' input='' apply=false input_path input_identity_before input_identity_after input_sha_before input_sha_after
  local review review_tmp review_sha run_tmp
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --run-root) run_root=${2:?}; shift 2 ;;
      --input) input=${2:?}; shift 2 ;;
      --apply) apply=true; shift ;;
      --dry-run) apply=false; shift ;;
      -h|--help)
        echo 'incident-review options: --run-root DIR --input EVIDENCE_ROOT/native-incident/review-input.json [--apply|--dry-run]'
        return 0
        ;;
      *) die "unknown incident-review option: $1" ;;
    esac
  done
  [[ -n "$run_root" && -n "$input" ]] || die "incident-review requires --run-root and --input"
  load_run "$run_root"
  review="$EVIDENCE_ROOT/native-incident-review.json"
  [[ ! -e "$review" && -z "$RUN_NATIVE_INCIDENT_REVIEW_SHA" ]] || die "native incident GUI review already exists and is immutable"
  input_path=$(canonical_evidence_file "$input") || die "native incident review input must be a regular file inside evidenceRoot"
  [[ "$input_path" == "$EVIDENCE_ROOT/native-incident/review-input.json" ]] || die "native incident review input path is not the fixed staging path"
  [[ "$(jq -r '.schema // empty' "$input_path")" == "$NATIVE_INCIDENT_REVIEW_SCHEMA" ]] || die "unsupported native incident review schema"
  verify_candidate_evidence
  [[ "$CANDIDATE_EVIDENCE_VALID" == true && "$BACKEND_CRASH_RESPAWN_EVIDENCE_VALID" == true ]] || die "incident review requires the live crash-advanced candidate runtime"
  validate_native_incident_evidence "$EVIDENCE_ROOT/native-incident-observed.json" committed || die "incident review requires valid retained native incident evidence"
  input_identity_before=$(stat -f '%d:%i:%z:%m:%c:%p' "$input_path")
  input_sha_before=$(file_sha "$input_path")
  review_tmp=$(mktemp "$EVIDENCE_ROOT/.native-review.XXXXXX")
  cp -p "$input_path" "$review_tmp"
  chmod 400 "$review_tmp"
  input_identity_after=$(stat -f '%d:%i:%z:%m:%c:%p' "$input_path")
  input_sha_after=$(file_sha "$input_path")
  [[ "$input_identity_before" == "$input_identity_after" && "$input_sha_before" == "$input_sha_after" ]] || {
    rm -f "$review_tmp"
    die "native incident review input changed while it was retained"
  }
  validate_native_incident_review_evidence "$review_tmp" precommit || {
    rm -f "$review_tmp"
    die "native incident GUI review does not bind the exact windows and screenshots"
  }
  if [[ "$apply" != true ]]; then
    rm -f "$review_tmp"
    echo "dry-run: validated native incident GUI review; nothing was retained"
    return 0
  fi
  mv "$review_tmp" "$review"
  review_sha=$(file_sha "$review")
  run_tmp=$(mktemp "$RUN_ROOT/.run.XXXXXX")
  jq --arg sha "$review_sha" '.nativeIncidentReviewSHA256=$sha' "$RUN_ROOT/run.json" > "$run_tmp"
  chmod 600 "$run_tmp"
  mv "$run_tmp" "$RUN_ROOT/run.json"
  load_run "$RUN_ROOT"
  validate_native_incident_review_evidence "$review" committed || die "committed native incident review did not revalidate"
  command_verify --run-root "$RUN_ROOT"
}

command_verify() {
  local run_root='' prepared_only=false require_real=false
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --run-root) run_root=${2:?}; shift 2 ;;
      --prepared-only) prepared_only=true; shift ;;
      --require-real) require_real=true; shift ;;
      -h|--help)
        echo 'verify options: --run-root DIR [--prepared-only|--require-real]'
        return 0
        ;;
      *) die "unknown verify option: $1" ;;
    esac
  done
  [[ -n "$run_root" ]] || die "verify requires --run-root"
  load_run "$run_root"
  need_command jq
  need_command sqlite3
  need_command shasum
  recover_pending_fault_transactions || die "a previous candidate fault transaction could not be recovered without overwriting candidate data"

  current="$EVIDENCE_ROOT/processes.current.tsv"
  write_current_snapshot "$current"
  verify_protected_processes "$EVIDENCE_ROOT/protected-processes.before.tsv" "$current"
  verify_slice
  verify_candidate_evidence
  candidate_runtime_epoch=${CANDIDATE_RUNTIME_EPOCH:-none}
  cockpit_result=$(cockpit_verify_json)
  load_cockpit_verification "$cockpit_result"

  sync_false=false
  cockpit_control_plane_isolated=false
  if jq -e '.autoSyncThreads | type == "boolean" and . == false' "$RUN_ROOT/run.json" >/dev/null && \
     jq -e '.defaultSettings | (.autoSyncThreads == false and .autoRepairSessionVisibilityOnLaunch == false and .protectConfigOnLaunch == true)' "$RUN_ROOT/cockpit-instance.json" >/dev/null; then
    sync_false=true
  fi
  if [[ -e "$COCKPIT_STORE" && "$COCKPIT_MANAGED" != true ]]; then
    sync_false=false
  elif [[ "$COCKPIT_MANAGED" == true && "$COCKPIT_SYNC_FALSE" != true ]]; then
    sync_false=false
  fi
  [[ "$COCKPIT_ELECTRON_DATA" == "$ELECTRON_DATA" ]] || sync_false=false
  if [[ "$COCKPIT_STORE" == "$COCKPIT_DATA_ROOT/"* ]] && \
     [[ "$ELECTRON_DATA" == "$COCKPIT_DATA_ROOT/"* ]] && \
     [[ "$(jq -r '.cockpitControlPlaneEnv' "$RUN_ROOT/run.json")" == COCKPIT_TOOLS_TEST_DATA_DIR ]] && \
     [[ "$(jq -r '.cockpitCodexHome' "$RUN_ROOT/run.json")" == "$CODEX_HOME_ISOLATED" ]]; then
    cockpit_control_plane_isolated=true
  fi

  desktop_bound=false
  app_server_bound=false
  ancestry_valid=false
  desktop_pid=$(awk -F '\t' '$3=="desktop" && $4=="true" && $6=="true" {print $1; exit}' "$current")
  app_server_pid=$(awk -F '\t' '$3=="app-server" && $4=="true" && $5=="true" {print $1; exit}' "$current")
  [[ -z "$desktop_pid" ]] || desktop_bound=true
  [[ -z "$app_server_pid" ]] || app_server_bound=true
  if [[ -n "$desktop_pid" && -n "$app_server_pid" ]] && ancestor_reaches "$app_server_pid" "$desktop_pid"; then
    ancestry_valid=true
  fi
  cockpit_last_pid_matches=false
  if [[ -n "$desktop_pid" && "$COCKPIT_MANAGED" == true && "$COCKPIT_LAST_PID" == "$desktop_pid" ]]; then
    cockpit_last_pid_matches=true
  fi
  cockpit_bundle_identifier=$(bundle_info_value "$COCKPIT_APP_PATH" CFBundleIdentifier)
  cockpit_bundle_short_version=$(bundle_info_value "$COCKPIT_APP_PATH" CFBundleShortVersionString)
  cockpit_bundle_version=$(bundle_info_value "$COCKPIT_APP_PATH" CFBundleVersion)
  cockpit_bundle_matches=false
  bundle_signatures_valid=true
  if [[ -f "$EVIDENCE_ROOT/launch.applied" ]]; then
    codesign --verify --deep --strict "$COCKPIT_APP_PATH" >/dev/null 2>&1 || bundle_signatures_valid=false
    codesign --verify --deep --strict "$APP_PATH" >/dev/null 2>&1 || bundle_signatures_valid=false
  fi
  cockpit_executable=$(bundle_executable_path "$COCKPIT_APP_PATH")
  app_executable=$(bundle_executable_path "$APP_PATH")
  if [[ "$cockpit_bundle_identifier" == "$COCKPIT_BUNDLE_IDENTIFIER" && \
        "$cockpit_bundle_short_version" == "$COCKPIT_BUNDLE_SHORT_VERSION" && \
        "$cockpit_bundle_version" == "$COCKPIT_BUNDLE_VERSION" && \
        -n "$COCKPIT_EXECUTABLE_SHA" && "$(file_sha "$cockpit_executable" 2>/dev/null || true)" == "$COCKPIT_EXECUTABLE_SHA" && \
        -n "$CODEX_APP_EXECUTABLE_SHA" && "$(file_sha "$app_executable" 2>/dev/null || true)" == "$CODEX_APP_EXECUTABLE_SHA" && \
        "$bundle_signatures_valid" == true ]]; then
    cockpit_bundle_matches=true
  fi
  cockpit_command_observed=false
  cockpit_evidence_pid=''
  if [[ -f "$EVIDENCE_ROOT/cockpit-command-observed.json" ]] && \
     [[ "$(jq -r '.source // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" == cockpit-codex_start_instance ]] && \
     [[ "$(jq -r '.instanceId // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" == "$COCKPIT_INSTANCE_ID" ]] && \
     [[ -n "$COCKPIT_BUNDLE_IDENTIFIER" ]] && \
     [[ "$(jq -r '.cockpitBundleIdentifier // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" == "$COCKPIT_BUNDLE_IDENTIFIER" ]] && \
     [[ "$(jq -r '.cockpitBundleShortVersion // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" == "$COCKPIT_BUNDLE_SHORT_VERSION" ]] && \
     [[ "$(jq -r '.cockpitBundleVersion // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" == "$COCKPIT_BUNDLE_VERSION" ]] && \
     [[ "$(jq -r '.desktopPid // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" == "$COCKPIT_LAST_PID" ]] && \
     [[ "$(jq -r '.lastLaunchedAt // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" == "$COCKPIT_LAST_LAUNCHED" ]] && \
     [[ "$(jq -r '.desktopProcessStart // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" == "$(process_start "$desktop_pid")" ]] && \
     [[ "$(jq -r '.appServerPid // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" == "$app_server_pid" ]] && \
     [[ "$(jq -r '.appServerProcessStart // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" == "$(process_start "$app_server_pid")" ]]; then
    cockpit_evidence_pid=$(jq -r '.cockpitPid // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")
    if [[ "$cockpit_evidence_pid" =~ ^[0-9]+$ ]] && \
       [[ "$(process_start "$cockpit_evidence_pid")" == "$(jq -r '.cockpitProcessStart // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" ]] && \
       [[ "$(process_command_sha "$cockpit_evidence_pid" 2>/dev/null || true)" == "$(jq -r '.cockpitCommandSHA256 // empty' "$EVIDENCE_ROOT/cockpit-command-observed.json")" ]] && \
       [[ "$(process_executable_path "$cockpit_evidence_pid" 2>/dev/null || true)" == "$cockpit_executable" ]] && \
       process_environment_has_exact "$cockpit_evidence_pid" COCKPIT_TOOLS_TEST_DATA_DIR "$COCKPIT_DATA_ROOT" && \
       process_environment_has_exact "$cockpit_evidence_pid" CODEX_HOME "$CODEX_HOME_ISOLATED"; then
      cockpit_command_observed=true
    fi
  fi
  real_task_runtime_valid=false
  if [[ -f "$EVIDENCE_ROOT/real-task-observed.json" && -f "$EVIDENCE_ROOT/codexfold-candidate-observed.json" ]]; then
    task_pid=$(jq -r '.candidateDaemonPid // empty' "$EVIDENCE_ROOT/real-task-observed.json")
    task_start=$(jq -r '.candidateDaemonStart // empty' "$EVIDENCE_ROOT/real-task-observed.json")
    task_command_sha=$(jq -r '.candidateCommandSHA256 // empty' "$EVIDENCE_ROOT/real-task-observed.json")
    if [[ "$task_pid" == "$(jq -r '.daemonPid' "$EVIDENCE_ROOT/codexfold-candidate-observed.json")" && \
          "$task_start" == "$(jq -r '.daemonProcessStart' "$EVIDENCE_ROOT/codexfold-candidate-observed.json")" && \
          "$task_command_sha" == "$(jq -r '.daemonCommandSHA256' "$EVIDENCE_ROOT/codexfold-candidate-observed.json")" ]]; then
      real_task_runtime_valid=true
    elif [[ "$BACKEND_CRASH_RESPAWN_EVIDENCE_VALID" == true && \
            "$task_pid" == "$(jq -r '.after.pid' "$EVIDENCE_ROOT/candidate-backend-crash-respawn.json")" && \
            "$task_start" == "$(jq -r '.after.processStart' "$EVIDENCE_ROOT/candidate-backend-crash-respawn.json")" && \
            "$task_command_sha" == "$(jq -r '.after.commandSHA256' "$EVIDENCE_ROOT/candidate-backend-crash-respawn.json")" ]]; then
      real_task_runtime_valid=true
    fi
  fi
  real_task_observed=false
  if [[ -f "$EVIDENCE_ROOT/real-task-observed.json" ]] && \
     [[ ! -L "$EVIDENCE_ROOT/real-task-observed.json" ]] && \
     [[ "$(jq -r '.source // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == actual-cockpit-ui ]] && \
     [[ "$(jq -r '.changedIdentityRows // 0' "$EVIDENCE_ROOT/real-task-observed.json")" -gt 0 ]] && \
     [[ "$CANDIDATE_EVIDENCE_VALID" == true ]] && \
     [[ "$(jq -r '.candidateEvidenceSHA256 // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == "$RUN_CANDIDATE_EVIDENCE_SHA" ]] && \
     [[ "$real_task_runtime_valid" == true ]] && \
     [[ "$(jq -r '.candidateBinaryPath // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == "$VALIDATED_CANDIDATE_BINARY" ]] && \
     [[ "$(jq -r '.candidateBuildSHA // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == "$VALIDATED_CANDIDATE_SHA" ]] && \
     [[ "$(jq -r '.candidateMountPoint // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == "$VALIDATED_CANDIDATE_MOUNT" ]] && \
     [[ "$(jq -r '.candidateMountIdentity // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == "$VALIDATED_CANDIDATE_MOUNT_IDENTITY" ]] && \
     [[ "$(jq -r '.desktopPid // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == "$desktop_pid" ]] && \
     [[ "$(jq -r '.desktopProcessStart // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == "$(process_start "$desktop_pid")" ]] && \
     [[ "$(jq -r '.appServerPid // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == "$app_server_pid" ]] && \
     [[ "$(jq -r '.appServerProcessStart // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == "$(process_start "$app_server_pid")" ]] && \
     [[ -f "$EVIDENCE_ROOT/real-task.before.tsv" && ! -L "$EVIDENCE_ROOT/real-task.before.tsv" ]] && \
     [[ -f "$EVIDENCE_ROOT/real-task.current.tsv" && ! -L "$EVIDENCE_ROOT/real-task.current.tsv" ]] && \
     [[ "$(jq -r '.beforeSnapshotSHA256 // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == "$(file_sha "$EVIDENCE_ROOT/real-task.before.tsv" 2>/dev/null || true)" ]] && \
     [[ "$(jq -r '.currentSnapshotSHA256 // empty' "$EVIDENCE_ROOT/real-task-observed.json")" == "$(file_sha "$EVIDENCE_ROOT/real-task.current.tsv" 2>/dev/null || true)" ]] && \
     [[ "$(jq -r '.changedIdentityRows // 0' "$EVIDENCE_ROOT/real-task-observed.json")" == "$(count_added_or_changed_rollout_identities "$EVIDENCE_ROOT/real-task.before.tsv" "$EVIDENCE_ROOT/real-task.current.tsv")" ]] && \
     [[ "$CANDIDATE_TASK_ROUTE_MATCHES" -gt 0 ]]; then
    real_task_observed=true
  fi

  launch_required=false
  if [[ -f "$EVIDENCE_ROOT/launch.applied" && "$prepared_only" != true ]]; then
    launch_required=true
  fi
  codex_instance_acceptance_complete=false
  if [[ "$cockpit_command_observed" == true && "$real_task_observed" == true && "$cockpit_last_pid_matches" == true && \
        "$desktop_bound" == true && "$app_server_bound" == true && "$ancestry_valid" == true && \
        "$PROTECTED_UNCHANGED" == true && "$SLICE_VALID" == true && "$sync_false" == true ]]; then
    codex_instance_acceptance_complete=true
  fi
  native_incident_evidence_valid=false
  native_incident_gui_review_complete=false
  completion_evidence_metadata_consistent=true
  if [[ -f "$EVIDENCE_ROOT/native-incident-observed.json" || -n "$RUN_NATIVE_INCIDENT_EVIDENCE_SHA" ]]; then
    if [[ "$CANDIDATE_EVIDENCE_VALID" == true && "$BACKEND_CRASH_RESPAWN_EVIDENCE_VALID" == true ]] && \
       validate_native_incident_evidence "$EVIDENCE_ROOT/native-incident-observed.json" committed; then
      native_incident_evidence_valid=$NATIVE_INCIDENT_EVIDENCE_VALID
    else
      completion_evidence_metadata_consistent=false
    fi
  fi
  if [[ -f "$EVIDENCE_ROOT/native-incident-review.json" || -n "$RUN_NATIVE_INCIDENT_REVIEW_SHA" ]]; then
    if [[ "$native_incident_evidence_valid" == true ]] && validate_native_incident_review_evidence "$EVIDENCE_ROOT/native-incident-review.json" committed; then
      native_incident_gui_review_complete=$NATIVE_INCIDENT_GUI_REVIEW_COMPLETE
    else
      completion_evidence_metadata_consistent=false
    fi
  fi
  if [[ -f "$EVIDENCE_ROOT/candidate-backend-crash-respawn.json" || -n "$RUN_BACKEND_CRASH_EVIDENCE_SHA" ]]; then
    [[ "$BACKEND_CRASH_RESPAWN_EVIDENCE_VALID" == true ]] || completion_evidence_metadata_consistent=false
  fi
  candidate_fault_acceptance_complete=false
  if [[ "$BACKEND_CRASH_RESPAWN_EVIDENCE_VALID" == true && "$PROTECTED_UNCHANGED" == true && \
        "$NEW_UNBOUND_PROCESSES" == 0 && "$desktop_bound" == true && "$app_server_bound" == true && \
        "$ancestry_valid" == true ]] && current_source_provenance_matches; then
    candidate_fault_acceptance_complete=true
  fi
  native_incident_acceptance_complete=false
  if [[ "$native_incident_evidence_valid" == true && "$native_incident_gui_review_complete" == true ]]; then
    native_incident_acceptance_complete=true
  fi
  codexfold_candidate_acceptance_complete=false
  if [[ "$codex_instance_acceptance_complete" == true && "$CANDIDATE_EVIDENCE_VALID" == true && \
        "$CANDIDATE_ATTACHED" == true && "$MANAGED_ROUTE_OBSERVED" == true && \
        "$CANDIDATE_TASK_ROUTE_MATCHES" -gt 0 && "$candidate_fault_acceptance_complete" == true && \
        "$native_incident_acceptance_complete" == true ]]; then
    codexfold_candidate_acceptance_complete=true
  fi
  real_acceptance_complete=false
  if [[ "$codex_instance_acceptance_complete" == true && "$codexfold_candidate_acceptance_complete" == true ]]; then
    real_acceptance_complete=true
  fi
  verification_ok=true
  [[ "$PROTECTED_UNCHANGED" == true ]] || verification_ok=false
  [[ "$NEW_UNBOUND_PROCESSES" == 0 ]] || verification_ok=false
  [[ "$SLICE_VALID" == true ]] || verification_ok=false
  [[ "$sync_false" == true ]] || verification_ok=false
  [[ "$cockpit_control_plane_isolated" == true ]] || verification_ok=false
  [[ "$cockpit_bundle_matches" == true ]] || verification_ok=false
  [[ "$CANDIDATE_METADATA_CONSISTENT" == true ]] || verification_ok=false
  [[ "$completion_evidence_metadata_consistent" == true ]] || verification_ok=false
  if [[ -f "$EVIDENCE_ROOT/cockpit-registration.json" ]]; then
    [[ "$COCKPIT_MANAGED" == true ]] || verification_ok=false
  fi
  if [[ "$launch_required" == true ]]; then
    [[ "$desktop_bound" == true && "$app_server_bound" == true && "$ancestry_valid" == true ]] || verification_ok=false
    [[ "$COCKPIT_MANAGED" == true && "$cockpit_last_pid_matches" == true && "$cockpit_command_observed" == true ]] || verification_ok=false
  fi
  if [[ "$require_real" == true && "$real_acceptance_complete" != true ]]; then
    verification_ok=false
  fi

  jq -n \
    --arg checkedAt "$(timestamp)" \
    --argjson ok "$verification_ok" \
    --argjson protected "$PROTECTED_UNCHANGED" \
    --argjson newUnbound "$NEW_UNBOUND_PROCESSES" \
    --argjson slice "$SLICE_VALID" \
    --argjson sliceCount "$SLICE_COUNT" \
    --argjson currentThreadCount "$CURRENT_THREAD_COUNT" \
    --argjson sync "$sync_false" \
    --argjson cockpitManaged "$COCKPIT_MANAGED" \
    --argjson cockpitControlPlaneIsolated "$cockpit_control_plane_isolated" \
    --argjson cockpitLastPidMatches "$cockpit_last_pid_matches" \
    --argjson cockpitCommandObserved "$cockpit_command_observed" \
    --arg cockpitBundleIdentifier "$COCKPIT_BUNDLE_IDENTIFIER" \
    --arg cockpitBundleShortVersion "$COCKPIT_BUNDLE_SHORT_VERSION" \
    --arg cockpitBundleVersion "$COCKPIT_BUNDLE_VERSION" \
    --argjson cockpitBundleMatches "$cockpit_bundle_matches" \
    --argjson realTaskObserved "$real_task_observed" \
    --argjson candidateAttached "$CANDIDATE_ATTACHED" \
    --arg candidateBuildSHA "$CANDIDATE_BUILD_SHA" \
    --arg candidateRuntimeEpoch "$candidate_runtime_epoch" \
    --argjson managedRouteObserved "$MANAGED_ROUTE_OBSERVED" \
    --argjson candidateEvidenceValid "$CANDIDATE_EVIDENCE_VALID" \
    --argjson candidateMetadataConsistent "$CANDIDATE_METADATA_CONSISTENT" \
    --argjson candidateRouteCount "$CANDIDATE_ROUTE_COUNT" \
    --argjson candidateRealTaskRouteMatches "$CANDIDATE_TASK_ROUTE_MATCHES" \
    --argjson backendCrashRespawnEvidenceValid "$BACKEND_CRASH_RESPAWN_EVIDENCE_VALID" \
    --argjson nativeIncidentEvidenceValid "$native_incident_evidence_valid" \
    --argjson nativeIncidentGUIReviewComplete "$native_incident_gui_review_complete" \
    --argjson candidateFaultAcceptanceComplete "$candidate_fault_acceptance_complete" \
    --argjson nativeIncidentAcceptanceComplete "$native_incident_acceptance_complete" \
    --argjson desktop "$desktop_bound" \
    --argjson appServer "$app_server_bound" \
    --argjson ancestry "$ancestry_valid" \
    --argjson launchRequired "$launch_required" \
    --argjson requireReal "$require_real" \
    --argjson codexInstanceComplete "$codex_instance_acceptance_complete" \
    --argjson codexFoldCandidateComplete "$codexfold_candidate_acceptance_complete" \
    --argjson realComplete "$real_acceptance_complete" \
    --arg launcherMode "$(if [[ "$cockpit_command_observed" == true ]]; then printf actual-cockpit-ui; else printf cockpit-compatible-adapter-prepared-only; fi)" \
    '{checkedAt:$checkedAt,ok:$ok,launcherMode:$launcherMode,codexInstanceAcceptanceComplete:$codexInstanceComplete,codexFoldCandidateAcceptanceComplete:$codexFoldCandidateComplete,realAcceptanceComplete:$realComplete,requireReal:$requireReal,protectedCodexUnchanged:$protected,newUnboundCodexProcesses:$newUnbound,sessionSliceValid:$slice,sessionCount:$sliceCount,currentThreadCount:$currentThreadCount,autoSyncThreadsDisabled:$sync,cockpitControlPlaneIsolated:$cockpitControlPlaneIsolated,cockpitManaged:$cockpitManaged,cockpitLastPidMatches:$cockpitLastPidMatches,cockpitCommandObserved:$cockpitCommandObserved,cockpitBundleIdentifier:(if $cockpitBundleIdentifier == "" then null else $cockpitBundleIdentifier end),cockpitBundleShortVersion:(if $cockpitBundleShortVersion == "" then null else $cockpitBundleShortVersion end),cockpitBundleVersion:(if $cockpitBundleVersion == "" then null else $cockpitBundleVersion end),cockpitBundleMatches:$cockpitBundleMatches,realTaskObserved:$realTaskObserved,candidateAttached:$candidateAttached,candidateBuildSHA:(if $candidateBuildSHA == "" then null else $candidateBuildSHA end),candidateRuntimeEpoch:$candidateRuntimeEpoch,managedRouteObserved:$managedRouteObserved,candidateEvidenceValid:$candidateEvidenceValid,candidateMetadataConsistent:$candidateMetadataConsistent,candidateRouteCount:$candidateRouteCount,candidateRealTaskRouteMatches:$candidateRealTaskRouteMatches,backendCrashRespawnEvidenceValid:$backendCrashRespawnEvidenceValid,nativeIncidentEvidenceValid:$nativeIncidentEvidenceValid,nativeIncidentGUIReviewComplete:$nativeIncidentGUIReviewComplete,candidateFaultAcceptanceComplete:$candidateFaultAcceptanceComplete,nativeIncidentAcceptanceComplete:$nativeIncidentAcceptanceComplete,isolatedDesktopBound:$desktop,isolatedAppServerBound:$appServer,isolatedAncestryValid:$ancestry,launchRequired:$launchRequired}' \
    > "$EVIDENCE_ROOT/verification.json"
  chmod 600 "$EVIDENCE_ROOT/verification.json"
  write_report

  if [[ "$verification_ok" == true ]]; then
    echo "PASS: isolated acceptance evidence verified: $EVIDENCE_ROOT/report.md"
  else
    echo "FAIL: isolated acceptance verification failed: $EVIDENCE_ROOT/report.md" >&2
    return 1
  fi
}

fault_node_identity() {
  local path=$1
  [[ ! -L "$path" ]] || return 1
  [[ -e "$path" || -S "$path" ]] || return 1
  stat -f '%d:%i:%z:%m:%c:%p:%HT' "$path"
}

fault_token_update() {
  local token=$1 state=$2 result=${3:-}
  local temp
  [[ -f "$token" && ! -L "$token" ]] || return 1
  [[ -f "$EVIDENCE_ROOT/codexfold-candidate-observed.json" && ! -L "$EVIDENCE_ROOT/codexfold-candidate-observed.json" ]] || return 1
  [[ "$(file_sha "$EVIDENCE_ROOT/codexfold-candidate-observed.json")" == "$(jq -r '.candidateEvidenceSHA256' "$token")" && \
     "$(jq -r '.candidateEvidenceSHA256' "$token")" == "$RUN_CANDIDATE_EVIDENCE_SHA" ]] || return 1
  temp=$(mktemp "$(dirname "$token")/.token.XXXXXX")
  jq --arg state "$state" --arg updatedAt "$(timestamp)" --arg result "$result" \
    '.state=$state | .updatedAt=$updatedAt | .result=(if $result == "" then null else $result end)' \
    "$token" > "$temp"
  chmod 600 "$temp"
  mv "$temp" "$token"
  sync
}

new_fault_transaction() {
  local kind=$1 target=$2 duration=$3 root tx
  root="$CANDIDATE_ROOT/.acceptance-faults"
  if [[ -e "$root" ]]; then
    [[ -d "$root" && ! -L "$root" ]] || die "candidate fault transaction root is not a real directory"
  else
    mkdir "$root"
    chmod 700 "$root"
  fi
  tx="$root/$(uuidgen | tr '[:upper:]' '[:lower:]')"
  mkdir "$tx"
  chmod 700 "$tx"
  jq -n \
    --arg schema codexfold.acceptance-fault.v1 \
    --arg createdAt "$(timestamp)" \
    --arg kind "$kind" \
    --arg target "$target" \
    --argjson duration "$duration" \
    --arg candidateEvidenceSHA256 "$RUN_CANDIDATE_EVIDENCE_SHA" \
    --argjson daemonPid "$VALIDATED_CANDIDATE_PID" \
    --arg daemonProcessStart "$VALIDATED_CANDIDATE_PROCESS_START" \
    --arg daemonExecutablePath "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE" \
    --arg daemonExecutableSHA256 "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE_SHA" \
    --arg daemonCommandSHA256 "$VALIDATED_CANDIDATE_COMMAND_SHA" \
    '{schema:$schema,createdAt:$createdAt,updatedAt:$createdAt,state:"prepared",kind:$kind,target:$target,duration:$duration,candidateEvidenceSHA256:$candidateEvidenceSHA256,daemonPid:$daemonPid,daemonProcessStart:$daemonProcessStart,daemonExecutablePath:$daemonExecutablePath,daemonExecutableSHA256:$daemonExecutableSHA256,daemonCommandSHA256:$daemonCommandSHA256,result:null}' \
    > "$tx/token.json"
  chmod 600 "$tx/token.json"
  sync
  printf '%s\n' "$tx"
}

recover_file_fault_token() {
  local token=$1 tx target kind state original placeholder_identity current_identity
  [[ -f "$token" && ! -L "$token" ]] || return 1
  [[ -f "$EVIDENCE_ROOT/codexfold-candidate-observed.json" && ! -L "$EVIDENCE_ROOT/codexfold-candidate-observed.json" ]] || return 1
  [[ "$(file_sha "$EVIDENCE_ROOT/codexfold-candidate-observed.json")" == "$(jq -r '.candidateEvidenceSHA256' "$token")" && \
     "$(jq -r '.candidateEvidenceSHA256' "$token")" == "$RUN_CANDIDATE_EVIDENCE_SHA" ]] || return 1
  tx=$(cd "$(dirname "$token")" && pwd -P) || return 1
  [[ "$tx" == "$CANDIDATE_ROOT/.acceptance-faults/"* ]] || return 1
  kind=$(jq -r '.kind' "$token")
  state=$(jq -r '.state' "$token")
  target=$(jq -r '.target' "$token")
  target=$(canonical_candidate_leaf_path "$target") || return 1
  original="$tx/original"
  [[ "$kind" == socket || "$kind" == descriptor ]] || return 0
  case "$state" in
    restored|candidate-rebuilt-preserved|aborted) return 0 ;;
    prepared|displacing)
      if [[ ! -e "$original" && ! -S "$original" ]]; then
        fault_token_update "$token" aborted no-displacement
        return 0
      fi
      ;;
  esac
  [[ -e "$original" || -S "$original" ]] || return 1
  if [[ -e "$target" || -S "$target" || -L "$target" ]]; then
    [[ ! -L "$target" ]] || {
      fault_token_update "$token" candidate-rebuilt-preserved symlink-conflict
      return 0
    }
    placeholder_identity=$(jq -r '.placeholderIdentity // empty' "$token")
    current_identity=$(fault_node_identity "$target" 2>/dev/null || true)
    if [[ -n "$placeholder_identity" && "$current_identity" == "$placeholder_identity" ]]; then
      rm -f "$target"
    else
      fault_token_update "$token" candidate-rebuilt-preserved concurrent-rebuild
      return 0
    fi
  fi
  mv "$original" "$target"
  fault_token_update "$token" restored original-restored
}

recover_backend_fault_token() {
  local token=$1 state pid start executable executable_sha command_sha
  [[ -f "$token" && ! -L "$token" ]] || return 1
  [[ -f "$EVIDENCE_ROOT/codexfold-candidate-observed.json" && ! -L "$EVIDENCE_ROOT/codexfold-candidate-observed.json" ]] || return 1
  [[ "$(file_sha "$EVIDENCE_ROOT/codexfold-candidate-observed.json")" == "$(jq -r '.candidateEvidenceSHA256' "$token")" && \
     "$(jq -r '.candidateEvidenceSHA256' "$token")" == "$RUN_CANDIDATE_EVIDENCE_SHA" ]] || return 1
  state=$(jq -r '.state' "$token")
  [[ "$(jq -r '.kind' "$token")" == backend ]] || return 0
  case "$state" in resumed|aborted) return 0 ;; prepared) fault_token_update "$token" aborted not-stopped; return 0 ;; esac
  [[ "$state" == stopping || "$state" == stopped ]] || return 1
  pid=$(jq -r '.daemonPid' "$token")
  start=$(jq -r '.daemonProcessStart' "$token")
  executable=$(jq -r '.daemonExecutablePath' "$token")
  executable_sha=$(jq -r '.daemonExecutableSHA256' "$token")
  command_sha=$(jq -r '.daemonCommandSHA256' "$token")
  candidate_process_identity "$pid" "$executable" "$executable_sha" "$start" "$command_sha" || return 1
  kill -CONT "$pid"
  candidate_process_identity "$pid" "$executable" "$executable_sha" "$start" "$command_sha" || return 1
  fault_token_update "$token" resumed daemon-resumed
}

recover_pending_fault_transactions() {
  local root token
  root="$CANDIDATE_ROOT/.acceptance-faults"
  [[ ! -e "$root" ]] && return 0
  [[ -d "$root" && ! -L "$root" ]] || return 1
  while IFS= read -r token; do
    [[ -n "$token" ]] || continue
    case "$(jq -r '.kind // empty' "$token" 2>/dev/null || true)" in
      backend) recover_backend_fault_token "$token" || return 1 ;;
      socket|descriptor) recover_file_fault_token "$token" || return 1 ;;
      *) return 1 ;;
    esac
  done < <(find "$root" -mindepth 2 -maxdepth 2 -name token.json -type f -print | sort)
}

safe_candidate_target() {
  local requested=$1
  local parent canonical candidate
  [[ ! -L "$requested" ]] || die "fault target may not be a symbolic link"
  [[ -e "$requested" || -S "$requested" ]] || die "fault target does not exist: $requested"
  parent=$(cd "$(dirname "$requested")" && pwd -P)
  canonical="$parent/$(basename "$requested")"
  candidate=$(cd "$CANDIDATE_ROOT" && pwd -P)
  [[ "$canonical" == "$candidate/"* ]] || die "fault target is outside the isolated candidate root"
  printf '%s\n' "$canonical"
}

record_fault() {
  local mode=$1 kind=$2 target=$3 duration=$4 result=$5
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$(timestamp)" "$mode" "$kind" "$duration" "$result" "$(basename "$target")" "${CANDIDATE_BUILD_SHA:-}" \
    >> "$EVIDENCE_ROOT/faults.tsv"
  chmod 600 "$EVIDENCE_ROOT/faults.tsv"
}

command_backend_crash() {
  local target=$1 crash="$EVIDENCE_ROOT/candidate-backend-crash-respawn.json" crash_dir="$EVIDENCE_ROOT/backend-crash"
  local before_process="$EVIDENCE_ROOT/backend-crash/processes.before.tsv"
  local during_process="$EVIDENCE_ROOT/backend-crash/processes.during.tsv"
  local after_process="$EVIDENCE_ROOT/backend-crash/processes.after.tsv"
  local service_before_path="$EVIDENCE_ROOT/backend-crash/service-status.before.json"
  local service_after_path="$EVIDENCE_ROOT/backend-crash/service-status.after.json"
  local backend_before_path="$EVIDENCE_ROOT/backend-crash/backend-status.before.json"
  local backend_after_path="$EVIDENCE_ROOT/backend-crash/backend-status.after.json"
  local old_pid old_start old_executable old_executable_sha old_command_sha old_backend_id
  local new_pid='' new_start='' new_executable='' new_executable_sha='' new_command_sha='' new_command_line=''
  local service_before service_after backend_before_sha backend_after_sha old_gone_at deadline fault_id requested_at
  local desktop_pid desktop_start app_server_pid app_server_start fence
  local crash_tmp crash_sha run_tmp source_head_json

  [[ ! -e "$crash" && ! -e "$crash_dir" && -z "$RUN_BACKEND_CRASH_EVIDENCE_SHA" ]] || \
    die "backend crash evidence already exists; it is immutable and cannot be replaced"
  [[ "$target" == "$VALIDATED_BACKEND_PID_FILE" ]] || die "backend-crash target is not the exact retained candidate PID file"
  old_pid=$(tr -d '[:space:]' < "$target")
  [[ "$old_pid" == "$VALIDATED_CANDIDATE_PID" ]] || die "backend-crash PID file does not name the validated candidate runtime"
  candidate_process_identity "$old_pid" "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE" "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE_SHA" \
    "$VALIDATED_CANDIDATE_PROCESS_START" "$VALIDATED_CANDIDATE_COMMAND_SHA" || die "candidate daemon identity changed before backend crash"
  current_source_provenance_matches || die "source provenance changed before backend crash"

  mkdir "$crash_dir"
  chmod 700 "$crash_dir"
  write_current_snapshot "$before_process"
  validate_protected_snapshot_file "$before_process" "$(file_sha "$before_process")" || die "protected Codex fence changed before backend crash"
  fence=$(isolated_fence_from_snapshot "$before_process") || die "isolated Desktop/app-server fence is unavailable before backend crash"
  IFS=$'\t' read -r desktop_pid desktop_start app_server_pid app_server_start <<< "$fence"

  service_before=$(candidate_status_json "$VALIDATED_CANDIDATE_BINARY" "$VALIDATED_CANDIDATE_DEFINITION" "$VALIDATED_CANDIDATE_MOUNT") || \
    die "candidate service status is unavailable before backend crash"
  write_json_evidence_text "$service_before_path" "$service_before" || die "could not retain pre-crash service status"
  validate_backend_status_file "$VALIDATED_BACKEND_STATUS_PATH" "$old_pid" "$VALIDATED_CANDIDATE_MOUNT" "$VALIDATED_BACKEND_ID" || \
    die "candidate backend status is not healthy before crash"
  backend_before_sha=$(copy_stable_artifact "$VALIDATED_BACKEND_STATUS_PATH" "$backend_before_path" 1048576) || \
    die "could not retain pre-crash backend status"

  old_start=$VALIDATED_CANDIDATE_PROCESS_START
  old_executable=$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE
  old_executable_sha=$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE_SHA
  old_command_sha=$VALIDATED_CANDIDATE_COMMAND_SHA
  old_backend_id=$VALIDATED_BACKEND_ID
  fault_id=$(uuidgen | tr '[:upper:]' '[:lower:]')
  requested_at=$(timestamp)
  [[ "$(tr -d '[:space:]' < "$target")" == "$old_pid" ]] || die "backend PID file changed immediately before SIGKILL"
  candidate_process_identity "$old_pid" "$old_executable" "$old_executable_sha" "$old_start" "$old_command_sha" || \
    die "candidate daemon identity changed immediately before SIGKILL"
  /bin/kill -KILL "$old_pid"

  deadline=$((SECONDS + 15))
  while (( SECONDS < deadline )); do
    if [[ "$(process_start "$old_pid" 2>/dev/null || true)" != "$old_start" ]]; then
      break
    fi
    sleep 1
  done
  [[ "$(process_start "$old_pid" 2>/dev/null || true)" != "$old_start" ]] || \
    die "the exact pre-crash candidate process identity did not disappear"
  old_gone_at=$(timestamp)
  write_current_snapshot "$during_process"
  validate_protected_snapshot_file "$during_process" "$(file_sha "$during_process")" || die "protected Codex fence changed during backend crash"
  snapshot_matches_isolated_fence "$during_process" "$desktop_pid" "$desktop_start" "$app_server_pid" "$app_server_start" || \
    die "isolated Desktop/app-server fence changed during backend crash"
  ancestor_reaches "$app_server_pid" "$desktop_pid" || die "isolated app-server ancestry changed during backend crash"

  deadline=$((SECONDS + 45))
  while (( SECONDS < deadline )); do
    new_pid=$(tr -d '[:space:]' < "$target" 2>/dev/null || true)
    if [[ "$new_pid" =~ ^[0-9]+$ && "$new_pid" -gt 1 && "$new_pid" != "$old_pid" ]]; then
      new_start=$(process_start "$new_pid" 2>/dev/null || true)
      new_executable=$(process_executable_path "$new_pid" 2>/dev/null || true)
      new_executable_sha=$(file_sha "$new_executable" 2>/dev/null || true)
      new_command_sha=$(process_command_sha "$new_pid" 2>/dev/null || true)
      if [[ -n "$new_start" && "$new_start" != "$old_start" && "$new_executable" == "$old_executable" && \
            "$new_executable_sha" == "$old_executable_sha" && "$new_command_sha" == "$old_command_sha" ]]; then
        service_after=$(candidate_status_json "$VALIDATED_CANDIDATE_BINARY" "$VALIDATED_CANDIDATE_DEFINITION" "$VALIDATED_CANDIDATE_MOUNT" 2>/dev/null || true)
        if jq -e --argjson pid "$new_pid" --arg build "$VALIDATED_CANDIDATE_SHA" --arg binary "$VALIDATED_CANDIDATE_BINARY" \
          '(.daemon_running == true) and (.daemon_pid == $pid) and (.mount_healthy == true) and (.build.healthy == true) and (.build.running_build_sha256 == $build) and (.build.configured_build_sha256 == $build) and (.build.configured_binary_path == $binary)' \
          >/dev/null <<< "$service_after" && \
          validate_backend_status_file "$VALIDATED_BACKEND_STATUS_PATH" "$new_pid" "$VALIDATED_CANDIDATE_MOUNT" "$old_backend_id"; then
          break
        fi
      fi
    fi
    new_pid=''
    sleep 1
  done
  [[ "$new_pid" =~ ^[0-9]+$ && "$new_pid" != "$old_pid" ]] || die "candidate backend did not respawn with a new healthy PID"
	new_command_line=$(process_command "$new_pid")
	[[ "$new_command_line" == *"$CODEX_HOME_ISOLATED"* && "$new_command_line" == *"$VALIDATED_CANDIDATE_MOUNT"* ]] || \
		die "respawned candidate command no longer binds the same candidate inputs"
  [[ "$(real_mount_identity "$VALIDATED_CANDIDATE_MOUNT")" == "$VALIDATED_CANDIDATE_MOUNT_IDENTITY" && \
     "$(file_sha "$VALIDATED_CANDIDATE_DEFINITION")" == "$VALIDATED_CANDIDATE_DEFINITION_SHA" ]] || \
    die "candidate definition or mount identity changed across backend crash"
  current_source_provenance_matches || die "source provenance changed during backend crash recovery"

  write_json_evidence_text "$service_after_path" "$service_after" || die "could not retain post-crash service status"
  backend_after_sha=$(copy_stable_artifact "$VALIDATED_BACKEND_STATUS_PATH" "$backend_after_path" 1048576) || \
    die "could not retain post-crash backend status"
  write_current_snapshot "$after_process"
  validate_protected_snapshot_file "$after_process" "$(file_sha "$after_process")" || die "protected Codex fence changed after backend respawn"
  snapshot_matches_isolated_fence "$after_process" "$desktop_pid" "$desktop_start" "$app_server_pid" "$app_server_start" || \
    die "isolated Desktop/app-server fence changed after backend respawn"
  ancestor_reaches "$app_server_pid" "$desktop_pid" || die "isolated app-server ancestry changed after backend respawn"

  source_head_json=null
  [[ -z "$SOURCE_PROVENANCE_HEAD" ]] || source_head_json="\"$SOURCE_PROVENANCE_HEAD\""
  crash_tmp=$(mktemp "$EVIDENCE_ROOT/.backend-crash.XXXXXX")
  jq -n \
    --arg schema "$BACKEND_CRASH_EVIDENCE_SCHEMA" \
    --arg recordedAt "$(timestamp)" \
    --arg runID "$COCKPIT_INSTANCE_ID" \
    --arg candidateEvidenceSHA "$RUN_CANDIDATE_EVIDENCE_SHA" \
    --arg buildSHA "$VALIDATED_CANDIDATE_SHA" \
    --arg appIdentity "$VALIDATED_CANDIDATE_APP_IDENTITY" \
    --arg moduleSHA "$VALIDATED_CANDIDATE_MODULE_EXECUTABLE_SHA" \
    --arg repoIdentity "$SOURCE_PROVENANCE_REPO_IDENTITY" \
    --argjson gitHead "$source_head_json" \
    --arg sourceSHA "$SOURCE_PROVENANCE_SNAPSHOT_SHA" \
    --arg protectedBaselineSHA "$PROTECTED_PROCESS_BASELINE_SHA" \
    --arg beforeProcess "$before_process" --arg beforeProcessSHA "$(file_sha "$before_process")" \
    --arg duringProcess "$during_process" --arg duringProcessSHA "$(file_sha "$during_process")" \
    --arg afterProcess "$after_process" --arg afterProcessSHA "$(file_sha "$after_process")" \
    --argjson desktopPID "$desktop_pid" --arg desktopStart "$desktop_start" \
    --argjson appServerPID "$app_server_pid" --arg appServerStart "$app_server_start" \
    --arg faultID "$fault_id" --arg pidFile "$target" --arg requestedAt "$requested_at" \
    --argjson oldPID "$old_pid" --arg oldStart "$old_start" --arg executable "$old_executable" \
    --arg executableSHA "$old_executable_sha" --arg commandSHA "$old_command_sha" --arg backendID "$old_backend_id" \
    --arg definitionSHA "$VALIDATED_CANDIDATE_DEFINITION_SHA" --arg mountIdentity "$VALIDATED_CANDIDATE_MOUNT_IDENTITY" \
    --arg serviceBefore "$service_before_path" --arg serviceBeforeSHA "$(file_sha "$service_before_path")" \
    --arg backendBefore "$backend_before_path" --arg backendBeforeSHA "$backend_before_sha" \
    --arg oldGoneAt "$old_gone_at" --argjson newPID "$new_pid" --arg newStart "$new_start" \
    --arg serviceAfter "$service_after_path" --arg serviceAfterSHA "$(file_sha "$service_after_path")" \
    --arg backendAfter "$backend_after_path" --arg backendAfterSHA "$backend_after_sha" '
      {
        schema:$schema,recordedAt:$recordedAt,
        binding:{runID:$runID,candidateEvidenceSHA256:$candidateEvidenceSHA,candidateBuildSHA256:$buildSHA,candidateAppIdentitySHA256:$appIdentity,candidateFSKitModuleSHA256:$moduleSHA,sourceProvenance:{repoRootIdentity:$repoIdentity,gitHead:$gitHead,snapshotSHA256:$sourceSHA},protectedCodexFence:{baselineSHA256:$protectedBaselineSHA,beforeSnapshotPath:$beforeProcess,beforeSnapshotSHA256:$beforeProcessSHA,duringSnapshotPath:$duringProcess,duringSnapshotSHA256:$duringProcessSHA,afterSnapshotPath:$afterProcess,afterSnapshotSHA256:$afterProcessSHA,unchanged:true,newUnboundProcesses:0},isolatedCodexFence:{desktopPID:$desktopPID,desktopProcessStart:$desktopStart,appServerPID:$appServerPID,appServerProcessStart:$appServerStart,ancestryValid:true,unchangedDuringOccurrence:true}},
        fault:{faultID:$faultID,kind:"backend-crash",signal:"SIGKILL",targetPIDFile:$pidFile,requestedAt:$requestedAt},
        before:{observedAt:$requestedAt,pid:$oldPID,processStart:$oldStart,executablePath:$executable,executableSHA256:$executableSHA,commandSHA256:$commandSHA,backendID:$backendID,candidateBuildSHA256:$buildSHA,serviceDefinitionSHA256:$definitionSHA,mountIdentity:$mountIdentity,serviceStatusSnapshotPath:$serviceBefore,serviceStatusSnapshotSHA256:$serviceBeforeSHA,backendStatusSnapshotPath:$backendBefore,backendStatusSnapshotSHA256:$backendBeforeSHA},
        crash:{oldIdentityGoneAt:$oldGoneAt,oldProcessIdentityAbsent:true},
        after:{healthyAt:$recordedAt,pid:$newPID,processStart:$newStart,executablePath:$executable,executableSHA256:$executableSHA,commandSHA256:$commandSHA,backendID:$backendID,candidateBuildSHA256:$buildSHA,serviceDefinitionSHA256:$definitionSHA,mountIdentity:$mountIdentity,serviceStatusSnapshotPath:$serviceAfter,serviceStatusSnapshotSHA256:$serviceAfterSHA,backendStatusSnapshotPath:$backendAfter,backendStatusSnapshotSHA256:$backendAfterSHA,pidFileValue:$newPID,daemonRunning:true,mountHealthy:true,buildHealthy:true}
      }
    ' > "$crash_tmp"
  chmod 400 "$crash_tmp"
  mv "$crash_tmp" "$crash"
  crash_sha=$(file_sha "$crash")
  run_tmp=$(mktemp "$RUN_ROOT/.run.XXXXXX")
  jq --arg sha "$crash_sha" '.backendCrashRespawnEvidenceSHA256=$sha' "$RUN_ROOT/run.json" > "$run_tmp"
  chmod 600 "$run_tmp"
  mv "$run_tmp" "$RUN_ROOT/run.json"
  load_run "$RUN_ROOT"
  validate_candidate_anchor "$EVIDENCE_ROOT/codexfold-candidate-observed.json" || die "candidate anchor changed while committing backend crash evidence"
  validate_backend_crash_respawn_evidence "$EVIDENCE_ROOT/codexfold-candidate-observed.json" current || die "committed backend crash evidence did not revalidate"
  record_fault apply backend-crash "$target" 0 "respawned:$old_pid->$new_pid:$crash_sha"
  command_verify --run-root "$RUN_ROOT"
}

command_fault() {
  local run_root='' kind='' target='' duration=12 apply=false
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --run-root) run_root=${2:?}; shift 2 ;;
      --kind) kind=${2:?}; shift 2 ;;
      --target) target=${2:?}; shift 2 ;;
      --duration) duration=${2:?}; shift 2 ;;
      --apply) apply=true; shift ;;
      --dry-run) apply=false; shift ;;
      -h|--help)
        echo 'fault options: --run-root DIR --kind backend|backend-crash|socket|descriptor --target PATH --duration 1..60 [--apply]'
        echo 'backend/backend-crash target is the exact retained candidate PID file; socket/descriptor targets must also be retained candidate targets.'
        return 0
        ;;
      *) die "unknown fault option: $1" ;;
    esac
  done
  [[ -n "$run_root" && -n "$kind" && -n "$target" ]] || die "fault requires --run-root, --kind, and --target"
  [[ "$kind" == backend || "$kind" == backend-crash || "$kind" == socket || "$kind" == descriptor ]] || die "unsupported fault kind: $kind"
  if [[ ! "$duration" =~ ^[0-9]+$ ]] || (( duration < 1 || duration > 60 )); then
    die "--duration must be 1..60 seconds"
  fi
  load_run "$run_root"
  recover_pending_fault_transactions || die "could not safely recover a previous candidate fault transaction"
  target=$(safe_candidate_target "$target")

  if [[ "$kind" == socket ]]; then
    [[ -S "$target" ]] || die "socket fault target is not a Unix socket"
  else
    [[ -f "$target" ]] || die "$kind fault target must be a regular file"
  fi

  if [[ "$kind" == backend || "$kind" == backend-crash ]]; then
    pid=$(tr -d '[:space:]' < "$target")
    [[ "$pid" =~ ^[0-9]+$ ]] || die "backend PID file is invalid"
  fi

  if [[ "$apply" != true ]]; then
    echo "dry-run: would inject $kind fault for ${duration}s into isolated candidate target $(basename "$target")"
    return 0
  fi

  write_current_snapshot "$EVIDENCE_ROOT/processes.pre-fault.tsv"
  verify_protected_processes "$EVIDENCE_ROOT/protected-processes.before.tsv" "$EVIDENCE_ROOT/processes.pre-fault.tsv"
  [[ "$PROTECTED_UNCHANGED" == true ]] || die "protected Codex processes changed since prepare; refusing candidate fault"
  verify_candidate_evidence
  [[ "$CANDIDATE_EVIDENCE_VALID" == true && "$CANDIDATE_ATTACHED" == true && "$MANAGED_ROUTE_OBSERVED" == true ]] || \
    die "applied faults require validated external evidence that the real CodexFold candidate is attached to a managed isolated route"
  case "$kind" in
    backend|backend-crash)
      [[ -n "$VALIDATED_BACKEND_PID_FILE" && "$target" == "$VALIDATED_BACKEND_PID_FILE" ]] || die "backend fault target is not the exact PID file retained in candidate evidence"
      [[ "$pid" == "$VALIDATED_CANDIDATE_PID" ]] || die "backend PID file does not name the validated candidate daemon"
      candidate_process_identity "$pid" "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE" "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE_SHA" "$VALIDATED_CANDIDATE_PROCESS_START" "$VALIDATED_CANDIDATE_COMMAND_SHA" || die "candidate daemon identity changed before fault"
      ;;
    socket)
      [[ -n "$VALIDATED_SOCKET_TARGET" && "$target" == "$VALIDATED_SOCKET_TARGET" ]] || die "socket fault target is not retained in candidate evidence"
      ;;
    descriptor)
      [[ -n "$VALIDATED_DESCRIPTOR_TARGET" && "$target" == "$VALIDATED_DESCRIPTOR_TARGET" ]] || die "descriptor fault target is not retained in candidate evidence"
      ;;
  esac

  if [[ "$kind" == backend-crash ]]; then
    command_backend_crash "$target"
    return
  fi

  tx=$(new_fault_transaction "$kind" "$target" "$duration")
  token="$tx/token.json"

  if [[ "$kind" == backend ]]; then
    fault_token_update "$token" stopping watchdog-armed
    (
      sleep $((duration + 5))
      "$SCRIPT_DIR/run-isolated-codex-acceptance.sh" fault-recover-backend --run-root "$RUN_ROOT" --token "$token"
    ) >/dev/null 2>&1 &
    candidate_process_identity "$pid" "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE" "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE_SHA" "$VALIDATED_CANDIDATE_PROCESS_START" "$VALIDATED_CANDIDATE_COMMAND_SHA" || die "candidate daemon identity changed immediately before SIGSTOP"
    kill -STOP "$pid"
    candidate_process_identity "$pid" "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE" "$VALIDATED_CANDIDATE_DAEMON_EXECUTABLE_SHA" "$VALIDATED_CANDIDATE_PROCESS_START" "$VALIDATED_CANDIDATE_COMMAND_SHA" || die "stopped PID no longer has the validated candidate identity"
    fault_token_update "$token" stopped daemon-stopped
    sleep "$duration"
    recover_backend_fault_token "$token" || die "candidate daemon could not be resumed with the same process identity"
  else
    original_identity=$(fault_node_identity "$target") || die "fault target identity is unavailable"
    token_tmp=$(mktemp "$tx/.token.XXXXXX")
    jq --arg originalIdentity "$original_identity" '.originalIdentity=$originalIdentity | .state="displacing" | .updatedAt=(now|todateiso8601)' "$token" > "$token_tmp"
    chmod 600 "$token_tmp"
    mv "$token_tmp" "$token"
    mv "$target" "$tx/original"
    sync
    staged_identity=$(fault_node_identity "$tx/original") || die "staged fault target identity is unavailable"
    if [[ "$staged_identity" != "$original_identity" ]]; then
      fault_token_update "$token" candidate-rebuilt-preserved displacement-race
      die "fault target changed during durable displacement; retained both states without overwriting candidate data"
    fi
    fault_token_update "$token" displaced original-staged
    if [[ "$kind" == descriptor ]]; then
      placeholder="$tx/placeholder"
      printf '{"schema":"codexfold.acceptance.invalid-descriptor"}\n' > "$placeholder"
      chmod 600 "$placeholder"
      placeholder_identity=$(fault_node_identity "$placeholder") || die "descriptor placeholder identity is unavailable"
      mv -n "$placeholder" "$target"
      [[ ! -e "$placeholder" && "$(fault_node_identity "$target" 2>/dev/null || true)" == "$placeholder_identity" ]] || {
        fault_token_update "$token" candidate-rebuilt-preserved concurrent-rebuild-before-injection
        die "candidate rebuilt the descriptor before injection; preserved its content and retained the original"
      }
      token_tmp=$(mktemp "$tx/.token.XXXXXX")
      jq --arg placeholderIdentity "$placeholder_identity" '.placeholderIdentity=$placeholderIdentity | .state="injected" | .updatedAt=(now|todateiso8601)' "$token" > "$token_tmp"
      chmod 600 "$token_tmp"
      mv "$token_tmp" "$token"
    fi
    sleep "$duration"
    recover_file_fault_token "$token" || die "candidate file fault could not be recovered safely"
  fi
  recovery_deadline=$((SECONDS + 15))
  recovered=false
  while (( SECONDS < recovery_deadline )); do
    verify_candidate_evidence
    if [[ "$CANDIDATE_EVIDENCE_VALID" == true ]]; then
      recovered=true
      break
    fi
    sleep 1
  done
  [[ "$recovered" == true ]] || die "candidate did not return to the same verified healthy identity after fault recovery"
  fault_result=$(jq -r '.state' "$token")
  token_sha=$(file_sha "$token")
  record_fault apply "$kind" "$target" "$duration" "$fault_result:$token_sha"
  command_verify --run-root "$RUN_ROOT"
}

command_fault_recover_backend() {
  local run_root='' token=''
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --run-root) run_root=${2:?}; shift 2 ;;
      --token) token=${2:?}; shift 2 ;;
      *) die "unknown fault-recover-backend option: $1" ;;
    esac
  done
  [[ -n "$run_root" && -n "$token" ]] || die "fault-recover-backend requires --run-root and --token"
  load_run "$run_root"
  token_parent=$(cd "$(dirname "$token")" 2>/dev/null && pwd -P) || die "fault recovery token parent is unavailable"
  token="$token_parent/$(basename "$token")"
  [[ "$token" == "$CANDIDATE_ROOT/.acceptance-faults/"*/token.json && -f "$token" && ! -L "$token" ]] || die "fault recovery token escaped the candidate root"
  recover_backend_fault_token "$token" || die "backend recovery identity check failed"
}

command_report() {
  local run_root=''
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --run-root) run_root=${2:?}; shift 2 ;;
      -h|--help) echo 'report options: --run-root DIR'; return 0 ;;
      *) die "unknown report option: $1" ;;
    esac
  done
  [[ -n "$run_root" ]] || die "report requires --run-root"
  load_run "$run_root"
  SLICE_COUNT=$(wc -l < "$CODEX_HOME_ISOLATED/selected-sessions.tsv" 2>/dev/null | tr -d ' ' || printf 0)
  CURRENT_THREAD_COUNT=$(sqlite3 "$CODEX_HOME_ISOLATED/state_5.sqlite" 'SELECT count(*) FROM threads;' 2>/dev/null || printf unknown)
  write_report
  echo "$EVIDENCE_ROOT/report.md"
}

main() {
  local command_name
  [[ $# -ge 1 ]] || usage
  command_name=$1
  shift
  case "$command_name" in
    prepare) command_prepare "$@" ;;
    cockpit-register) command_cockpit_register "$@" ;;
    launch) command_launch "$@" ;;
    task) command_task "$@" ;;
    observe-task) command_observe_task "$@" ;;
    candidate-evidence) command_candidate_evidence "$@" ;;
    incident-evidence) command_incident_evidence "$@" ;;
    incident-review) command_incident_review "$@" ;;
    verify) command_verify "$@" ;;
    fault) command_fault "$@" ;;
    fault-recover-backend) command_fault_recover_backend "$@" ;;
    report) command_report "$@" ;;
    -h|--help|help) usage ;;
    *) die "unknown command: $command_name" ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
