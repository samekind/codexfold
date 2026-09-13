#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)
RUNNER="$ROOT_DIR/scripts/run-isolated-codex-acceptance.sh"
TEST_ROOT=$(mktemp -d)
candidate_pid=''
cleanup() {
  if [[ -n "$candidate_pid" ]] && kill -0 "$candidate_pid" 2>/dev/null; then
    kill "$candidate_pid" 2>/dev/null || true
    wait "$candidate_pid" 2>/dev/null || true
  fi
  rm -rf "$TEST_ROOT"
}
trap cleanup EXIT

# Pure parsers are sourceable without dispatching the runner. An old registered
# module path must not satisfy the exact current-candidate path check.
(
  # shellcheck disable=SC1090,SC1091
  source "$RUNNER"
  candidate_module='/private/tmp/current candidate/CodexFoldVerification.app/Contents/Extensions/CodexFoldFSKitModule.appex'
  old_output=$'+    vip.jstar.codexfold.verification.module(0.3.0)\tOLD\t2026-07-24 18:23:02 +0000\t/Users/test/Applications/CodexFoldVerification.app/Contents/Extensions/CodexFoldFSKitModule.appex'
  if registered_fskit_module_output_contains "$old_output" "$candidate_module"; then
    echo "old registered FSKit module path matched the current candidate" >&2
    exit 1
  fi
  exact_output="$old_output"$'\n'"+    vip.jstar.codexfold.verification.module(0.3.0)"$'\tCURRENT\t2026-07-26 10:00:45 +0000\t'"$candidate_module"
  registered_fskit_module_output_contains "$exact_output" "$candidate_module"
  while IFS= read -r module_pid; do
    [[ "$module_pid" =~ ^[0-9]+$ ]] || {
      echo "FSKit module process discovery returned a non-numeric PID" >&2
      exit 1
    }
  done < <(list_fskit_module_process_ids)

  computer_use_runtime_executable '/Applications/ChatGPT.app/Contents/Resources/cua_node/bin/node'
  computer_use_runtime_executable '/Applications/ChatGPT.app/Contents/Resources/cua_node/bin/node_repl'
  if computer_use_runtime_executable '/Applications/ChatGPT.app/Contents/Resources/codex'; then
    echo "Codex runtime was mistaken for Computer Use infrastructure" >&2
    exit 1
  fi

  module_baseline_fixture="$TEST_ROOT/module-baseline.fixture.tsv"
  module_current_fixture="$TEST_ROOT/module-current.fixture.tsv"
  module_new_fixture="$TEST_ROOT/module-new.fixture.tsv"
  printf '100\t1\tstart-a\t/old/module-a\t%s\n200\t1\tstart-b\t/old/module-b\t%s\n' \
    "$(printf 'a%.0s' {1..64})" "$(printf 'b%.0s' {1..64})" > "$module_baseline_fixture"
  cp -p "$module_baseline_fixture" "$module_current_fixture"
  printf '300\t1\tstart-c\t/new/module\t%s\n' "$(printf 'c%.0s' {1..64})" >> "$module_current_fixture"
  write_new_fskit_module_process_snapshot "$module_baseline_fixture" "$module_current_fixture" "$module_new_fixture"
  [[ "$(wc -l < "$module_new_fixture" | tr -d ' ')" == 1 ]]
  grep -Fq $'300\t1\tstart-c\t/new/module' "$module_new_fixture"
  tail -n 2 "$module_current_fixture" > "$TEST_ROOT/module-current.missing-baseline.tsv"
  if write_new_fskit_module_process_snapshot "$module_baseline_fixture" "$TEST_ROOT/module-current.missing-baseline.tsv" "$TEST_ROOT/module-new.invalid.tsv"; then
    echo "missing pre-existing FSKit module process passed delta validation" >&2
    exit 1
  fi

  snapshot_repo="$TEST_ROOT/source-snapshot-repo"
  mkdir "$snapshot_repo"
  git -C "$snapshot_repo" init -q
  printf '/.tmp/\n' > "$snapshot_repo/.gitignore"
  printf 'tracked-v1\n' > "$snapshot_repo/tracked.txt"
  git -C "$snapshot_repo" add .gitignore tracked.txt
  snapshot_v1=$(source_snapshot_sha "$snapshot_repo")
  printf 'tracked-v2\n' > "$snapshot_repo/tracked.txt"
  snapshot_v2=$(source_snapshot_sha "$snapshot_repo")
  [[ "$snapshot_v1" != "$snapshot_v2" ]]
  printf 'untracked\n' > "$snapshot_repo/untracked.txt"
  snapshot_with_untracked=$(source_snapshot_sha "$snapshot_repo")
  [[ "$snapshot_v2" != "$snapshot_with_untracked" ]]
  mkdir -p "$snapshot_repo/.tmp/nested-repository"
  git -C "$snapshot_repo/.tmp/nested-repository" init -q
  [[ "$(source_snapshot_sha "$snapshot_repo")" == "$snapshot_with_untracked" ]]
  if source_snapshot_sha "$TEST_ROOT" >/dev/null 2>&1; then
    echo "source snapshot accepted a non-repository directory" >&2
    exit 1
  fi

  # A mounted-path hash must fail within its bound when the target never
  # produces EOF; otherwise a broken FSKit endpoint can wedge the verifier.
  hung_fifo="$TEST_ROOT/hung-rollout.fifo"
  mkfifo "$hung_fifo"
  started_at=$(date +%s)
  if bounded_file_sha 1 "$hung_fifo" >/dev/null 2>&1; then
    echo "bounded_file_sha unexpectedly read a non-terminating FIFO" >&2
    exit 1
  fi
  elapsed=$(( $(date +%s) - started_at ))
  (( elapsed <= 3 )) || {
    echo "bounded_file_sha exceeded its timeout: ${elapsed}s" >&2
    exit 1
  }

  api_home="$TEST_ROOT/api-home"
  mkdir -p "$api_home"
  printf '%s\n' \
    'preferred_auth_method = "apikey"' \
    'cli_auth_credentials_store = "file"' \
    '[model_providers.main]' \
    'requires_openai_auth = true' \
    'base_url = "https://provider.invalid/v1"' > "$api_home/config.toml"
  printf '{}\n' > "$api_home/auth.json"
  sanitize_acceptance_credentials "$api_home"
  [[ "$(grep -E '^[[:space:]]*requires_openai_auth[[:space:]]*=' "$api_home/config.toml" | tr -d '[:space:]')" == 'requires_openai_auth=false' ]]
  [[ "$(grep -E '^[[:space:]]*env_key[[:space:]]*=' "$api_home/config.toml" | tr -d '[:space:]')" == 'env_key="OPENAI_API_KEY"' ]]
  [[ "$(grep -E '^[[:space:]]*forced_login_method[[:space:]]*=' "$api_home/config.toml" | tr -d '[:space:]')" == 'forced_login_method="api"' ]]
  if grep -Eq '^[[:space:]]*notify[[:space:]]*=' "$api_home/config.toml"; then
    echo "isolated API-key config retained notify" >&2
    exit 1
  fi
  if grep -Eq '^\[(plugins|mcp_servers)\.' "$api_home/config.toml"; then
    echo "isolated API-key config retained plugin or MCP configuration" >&2
    exit 1
  fi
  if grep -Eq '^\[marketplaces\.' "$api_home/config.toml"; then
    echo "isolated API-key config retained marketplace configuration" >&2
    exit 1
  fi
  if grep -Eq '^[[:space:]]*requires_openai_auth[[:space:]]*=[[:space:]]*true' "$api_home/config.toml"; then
    echo "isolated API-key config still requests OpenAI OAuth" >&2
    exit 1
  fi
  printf '{}\n' > "$api_home/auth.json"
  fake_login="$TEST_ROOT/fake-codex-login"
  # shellcheck disable=SC2016
  printf '#!/usr/bin/env bash\nset -euo pipefail\n[[ "$1" == login && "$2" == --with-api-key ]]\nIFS= read -r _key\nprintf seen > "$HOME/.api-key-seen"\nprintf '\''{"auth_mode":"apikey","OPENAI_API_KEY":"fixture-key"}\n'\'' > "$CODEX_HOME/auth.json"\n' > "$fake_login"
  chmod 700 "$fake_login"
  HOME="$api_home" install_acceptance_api_key "$api_home" fixture-key "$fake_login"
  [[ -f "$api_home/.codexfold-acceptance-api-key" ]]
  [[ -f "$api_home/.api-key-seen" ]]
  [[ "$(jq -c 'keys | sort' "$api_home/auth.json")" == '["OPENAI_API_KEY","auth_mode"]' ]]
  [[ "$(stat -f '%p' "$api_home/auth.json")" == 100600 ]]

  runtime_epoch_advanced 100 'Sun Jul 26 00:00:00 2026' 101 'Sun Jul 26 00:00:01 2026'
  if runtime_epoch_advanced 100 'Sun Jul 26 00:00:00 2026' 100 'Sun Jul 26 00:00:01 2026'; then
    echo "same-PID STOP/CONT transition was accepted as a crash runtime epoch" >&2
    exit 1
  fi

  # Native fault targets are allowed only below the exact resource root
  # parsed from the candidate service definition, while the task observer may
  # legitimately bind to the crash-before identity.
  CANDIDATE_ROOT="$TEST_ROOT/candidate-root"
  mkdir -p "$CANDIDATE_ROOT"
  VALIDATED_CANDIDATE_RESOURCE_ROOT="$TEST_ROOT/candidate-resource"
  mkdir -p "$VALIDATED_CANDIDATE_RESOURCE_ROOT"
  printf 'descriptor\n' > "$VALIDATED_CANDIDATE_RESOURCE_ROOT/descriptor.bin"
  expected_descriptor="$(cd "$VALIDATED_CANDIDATE_RESOURCE_ROOT" && pwd -P)/descriptor.bin"
  [[ "$(canonical_candidate_resource_file "$VALIDATED_CANDIDATE_RESOURCE_ROOT/descriptor.bin")" == "$expected_descriptor" ]]
  printf 'outside\n' > "$TEST_ROOT/outside-descriptor"
  if canonical_candidate_resource_file "$TEST_ROOT/outside-descriptor" >/dev/null 2>&1; then
    echo "outside native descriptor target passed resource-root validation" >&2
    exit 1
  fi
  EVIDENCE_ROOT="$TEST_ROOT/runtime-evidence"
  mkdir -p "$EVIDENCE_ROOT"
  jq -n '{before:{pid:101,processStart:"before",commandSHA256:"before-sha"},after:{pid:202,processStart:"after",commandSHA256:"after-sha"}}' > "$EVIDENCE_ROOT/candidate-backend-crash-respawn.json"
  # These globals are consumed indirectly by the sourced runtime validator.
  # shellcheck disable=SC2034
  BACKEND_CRASH_RESPAWN_EVIDENCE_VALID=true
  # shellcheck disable=SC2034
  VALIDATED_CANDIDATE_PID=202
  # shellcheck disable=SC2034
  VALIDATED_CANDIDATE_PROCESS_START=after
  # shellcheck disable=SC2034
  VALIDATED_CANDIDATE_COMMAND_SHA=after-sha
  candidate_task_runtime_matches 101 before before-sha
  if candidate_task_runtime_matches 303 unrelated unrelated-sha; then
    echo "unrelated task runtime passed crash-before/after binding" >&2
    exit 1
  fi

  backend_pid_fixture="$TEST_ROOT/backend.pid"
  printf '1111\n' > "$backend_pid_fixture"
  write_verified_backend_pid_file "$backend_pid_fixture" 2222
  [[ "$(tr -d '[:space:]' < "$backend_pid_fixture")" == 2222 ]]
  ln -s "$backend_pid_fixture" "$TEST_ROOT/backend.pid.symlink"
  if write_verified_backend_pid_file "$TEST_ROOT/backend.pid.symlink" 3333; then
    echo "backend PID refresh followed a symlink" >&2
    exit 1
  fi

  census="$TEST_ROOT/window-census.valid.jsonl"
  printf '%s\n' \
    '{"occurrenceID":"one","recoveryEpochID":"epoch-one","phase":"pre-threshold","elapsedMilliseconds":9999,"matchingWindowCount":0,"windowID":null}' \
    '{"occurrenceID":"one","recoveryEpochID":"epoch-one","phase":"active","elapsedMilliseconds":10000,"matchingWindowCount":1,"windowID":11}' \
    '{"occurrenceID":"one","recoveryEpochID":"epoch-one","phase":"steady","elapsedMilliseconds":11000,"matchingWindowCount":1,"windowID":11}' \
    '{"occurrenceID":"one","recoveryEpochID":"epoch-one","phase":"steady","elapsedMilliseconds":12000,"matchingWindowCount":1,"windowID":11}' \
    '{"occurrenceID":"one","recoveryEpochID":"epoch-one","phase":"recovered","elapsedMilliseconds":13000,"matchingWindowCount":1,"windowID":11}' \
    '{"occurrenceID":"two","recoveryEpochID":"epoch-two","phase":"pre-threshold","elapsedMilliseconds":9999,"matchingWindowCount":0,"windowID":null}' \
    '{"occurrenceID":"two","recoveryEpochID":"epoch-two","phase":"active","elapsedMilliseconds":10000,"matchingWindowCount":1,"windowID":22}' \
    '{"occurrenceID":"two","recoveryEpochID":"epoch-two","phase":"steady","elapsedMilliseconds":11000,"matchingWindowCount":1,"windowID":22}' \
    > "$census"
  validate_native_window_census "$census" one epoch-one 11 two epoch-two 22
  jq -c 'if .occurrenceID=="one" and .phase=="steady" then .matchingWindowCount=2 else . end' "$census" > "$TEST_ROOT/window-census.duplicate.jsonl"
  if validate_native_window_census "$TEST_ROOT/window-census.duplicate.jsonl" one epoch-one 11 two epoch-two 22; then
    echo "duplicate native windows for one occurrence passed the census oracle" >&2
    exit 1
  fi
  jq -c 'if .occurrenceID=="two" then .recoveryEpochID="epoch-one" else . end' "$census" > "$TEST_ROOT/window-census.same-epoch.jsonl"
  if validate_native_window_census "$TEST_ROOT/window-census.same-epoch.jsonl" one epoch-one 11 two epoch-two 22; then
    echo "second occurrence reused the first recovery epoch" >&2
    exit 1
  fi

  EVIDENCE_ROOT="$TEST_ROOT/export-evidence"
  # These globals are consumed by the sourced export validator.
  # shellcheck disable=SC2034
  RUN_ROOT="$TEST_ROOT/run-root-forbidden"
  # shellcheck disable=SC2034
  CANDIDATE_ROOT="$TEST_ROOT/candidate-root-forbidden"
  # shellcheck disable=SC2034
  SOURCE_REPO_ROOT="$TEST_ROOT/source-root-forbidden"
  # shellcheck disable=SC2034
  CODEX_HOME_ISOLATED="$TEST_ROOT/codex-home-forbidden"
  mkdir "$EVIDENCE_ROOT"
  EVIDENCE_ROOT=$(cd "$EVIDENCE_ROOT" && pwd -P)
  valid_export="$EVIDENCE_ROOT/valid.json"
  printf '%s\n' '{"schema_version":2,"application":"CodexFoldFSKit","redaction":"conservative-structured","occurrence_continuity":"verified_or_publisher","local_authorization":{"administrator_membership":"member","sudo_cache_state":"not_probed","automatic_elevation_allowed":false,"explicit_user_action_required":true},"incident":{"id":"one","reason":"Unavailable","impact":"Writes paused","recommendations":["Wait for recovery"]}}' > "$valid_export"
  validate_redacted_incident_export "$valid_export" "$(file_sha "$valid_export")" "$valid_export" one false
  leaked_export="$EVIDENCE_ROOT/leaked.json"
  printf '%s\n' '{"schema_version":2,"application":"CodexFoldFSKit","redaction":"conservative-structured","occurrence_continuity":"verified_or_publisher","incident":{"id":"one","reason":"Unavailable","impact":"Writes paused","recommendations":["Wait"],"note":"access_token=fixture-secret"}}' > "$leaked_export"
  if validate_redacted_incident_export "$leaked_export" "$(file_sha "$leaked_export")" "$leaked_export" one false; then
    echo "credential-bearing native incident export passed redaction validation" >&2
    exit 1
  fi
)

source_home="$TEST_ROOT/source"
workspace="$TEST_ROOT/workspace"
fake_app="$TEST_ROOT/Fake Codex.app"
run_root="$TEST_ROOT/run's path"
session_id=44444444-4444-7444-8444-444444444444
mkdir -p "$source_home/sessions/2026/07/26" "$source_home/archived_sessions" "$workspace" "$fake_app/Contents/MacOS" "$fake_app/Contents/Resources"
printf 'model_provider = "test"\npreferred_auth_method = "chatgpt"\nexperimental_bearer_token = "fixture-secret"\n' > "$source_home/config.toml"
printf '{"access_token":"secret-that-must-not-appear-in-evidence"}\n' > "$source_home/auth.json"
printf '{"session":"copied without being printed"}\n' > "$source_home/sessions/2026/07/26/rollout-2026-07-26T03-00-00-$session_id.jsonl"
sqlite3 "$source_home/state_5.sqlite" <<SQL
CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL, updated_at INTEGER NOT NULL, archived INTEGER NOT NULL DEFAULT 0);
INSERT INTO threads VALUES('$session_id', '$source_home/sessions/2026/07/26/rollout-2026-07-26T03-00-00-$session_id.jsonl', 1, 0);
SQL
printf '{"id":"%s","thread_name":"test","updated_at":"1"}\n' "$session_id" > "$source_home/session_index.jsonl"
printf '#!/usr/bin/env bash\nexit 0\n' > "$fake_app/Contents/MacOS/fake-codex"
chmod +x "$fake_app/Contents/MacOS/fake-codex"
plutil -create xml1 "$fake_app/Contents/Info.plist"
plutil -insert CFBundleExecutable -string fake-codex "$fake_app/Contents/Info.plist"
plutil -insert CFBundleIdentifier -string com.example.CockpitToolsIsolated "$fake_app/Contents/Info.plist"
plutil -insert CFBundleShortVersionString -string 1.2.3 "$fake_app/Contents/Info.plist"
plutil -insert CFBundleVersion -string 123 "$fake_app/Contents/Info.plist"
printf 'workspace\n' > "$workspace/README"
git -C "$workspace" init -q

"$RUNNER" prepare \
  --source-home "$source_home" \
  --run-root "$run_root" \
  --session-count 1 \
  --app "$fake_app" \
  --protected-app "$fake_app" \
  --cockpit-app "$fake_app" \
  --workspace "$workspace" >/dev/null

run_root=$(cd "$run_root" && pwd -P)

[[ "$(jq -r '.autoSyncThreads' "$run_root/run.json")" == false ]]
[[ "$(jq -r '.candidateAttached' "$run_root/run.json")" == false ]]
[[ "$(jq -r '.candidateBuildSHA' "$run_root/run.json")" == null ]]
[[ "$(jq -r '.managedRouteObserved' "$run_root/run.json")" == false ]]
[[ "$(jq -r '.backendCrashRespawnEvidenceSHA256' "$run_root/run.json")" == null ]]
[[ "$(jq -r '.nativeIncidentEvidenceSHA256' "$run_root/run.json")" == null ]]
[[ "$(jq -r '.nativeIncidentReviewSHA256' "$run_root/run.json")" == null ]]
[[ "$(jq -r '.cockpitBundleIdentifier' "$run_root/run.json")" == com.example.CockpitToolsIsolated ]]
[[ "$(jq -r '.cockpitBundleShortVersion' "$run_root/run.json")" == 1.2.3 ]]
[[ "$(jq -r '.cockpitBundleVersion' "$run_root/run.json")" == 123 ]]
[[ "$(jq -r '.defaultSettings.autoSyncThreads' "$run_root/cockpit-instance.json")" == false ]]
[[ "$(jq -r '.defaultSettings.autoRepairSessionVisibilityOnLaunch' "$run_root/cockpit-instance.json")" == false ]]
[[ -x "$run_root/launch-isolated-codex.sh" ]]
[[ -x "$run_root/register-cockpit-instance.sh" ]]
[[ -x "$run_root/open-isolated-cockpit.sh" ]]
grep -Fq 'cockpit-register' "$run_root/launch-isolated-codex.sh"
grep -Fq 'COCKPIT_TOOLS_TEST_DATA_DIR=' "$run_root/open-isolated-cockpit.sh"
[[ -x "$run_root/observe-real-task.sh" ]]
[[ -x "$run_root/record-candidate-evidence.sh" ]]
[[ -x "$run_root/verify.sh" ]]
[[ -x "$run_root/inject-fault.sh" ]]
[[ -x "$run_root/record-native-incident-evidence.sh" ]]
[[ -x "$run_root/record-native-incident-review.sh" ]]
grep -Fq '/usr/bin/open -n' "$RUNNER"
open_help_probe="open_help=\$(/usr/bin/open -h 2>&1 || true)"
grep -Fq "$open_help_probe" "$RUNNER"
grep -Fq 'unset CODEXFOLD_FSKIT_SCHEME' "$RUNNER"
if grep -Fq "open -h 2>&1 | grep -q -- '--env'" "$RUNNER"; then
  echo 'launch capability detection must not use grep -q under pipefail' >&2
  exit 1
fi
grep -Fq 'CODEX_HOME=' "$run_root/open-isolated-cockpit.sh"
grep -Fq '/usr/bin/env -i' "$run_root/open-isolated-cockpit.sh"
grep -Fq '/usr/bin/env -i' "$run_root/launch-isolated-desktop.sh"
grep -Fq 'ZDOTDIR=' "$run_root/launch-isolated-desktop.sh"
if grep -Fq '/usr/bin/open -n' "$run_root/launch-isolated-desktop.sh"; then
  echo "direct Desktop launcher unexpectedly uses LaunchServices" >&2
  exit 1
fi
[[ "$(jq -r '.schema' "$run_root/run.json")" == codexfold.isolated-acceptance.v3 ]]
[[ "$(jq -r '.cockpitCodexHome' "$run_root/run.json")" == "$(jq -r '.codexHome' "$run_root/run.json")" ]]
[[ "$(jq -r '.acceptanceAuthMode' "$run_root/run.json")" == isolated-file-api-key ]]
[[ "$(plutil -extract Label raw -o - "$ROOT_DIR/platform/darwin/fskit/Host/CodexFoldMenuBar.plist")" == vip.jstar.codexfold.fskitprofileprobe.menu-bar ]]
[[ "$(plutil -extract BundleProgram raw -o - "$ROOT_DIR/platform/darwin/fskit/Host/CodexFoldMenuBar.plist")" == Contents/MacOS/CodexFoldFSKit ]]
[[ "$(plutil -extract Label raw -o - "$ROOT_DIR/platform/darwin/fskit/Host/CodexFoldIncidentMonitor.plist")" == vip.jstar.codexfold.fskitprofileprobe.incident-monitor ]]
[[ -n "$(jq -r '.pathFences.runRoot.identity' "$run_root/run.json")" ]]
module_baseline=$(jq -r '.fskitModuleProcessBaselinePath' "$run_root/run.json")
[[ "$module_baseline" == "$run_root/evidence/fskit-module-processes.before.tsv" ]]
[[ -f "$module_baseline" && ! -L "$module_baseline" ]]
[[ "$(shasum -a 256 "$module_baseline" | awk '{print $1}')" == "$(jq -r '.fskitModuleProcessBaselineSHA256' "$run_root/run.json")" ]]
protected_baseline=$(jq -r '.protectedProcessBaselinePath' "$run_root/run.json")
[[ "$protected_baseline" == "$run_root/evidence/protected-processes.before.tsv" ]]
[[ -f "$protected_baseline" && ! -L "$protected_baseline" ]]
[[ "$(shasum -a 256 "$protected_baseline" | awk '{print $1}')" == "$(jq -r '.protectedProcessBaselineSHA256' "$run_root/run.json")" ]]
[[ "$(jq -r '.sourceProvenance.repoRoot' "$run_root/run.json")" == "$ROOT_DIR" ]]
[[ "$(jq -r '.sourceProvenance.repoRootIdentity' "$run_root/run.json")" =~ ^[0-9]+:[0-9]+$ ]]
[[ "$(jq -r '.sourceProvenance.snapshotSHA256' "$run_root/run.json")" =~ ^[0-9a-f]{64}$ ]]
grep -Fq 'exact source/build manifest' "$run_root/run.json"
grep -Fq 'exact signed and registered Swift FSKit module process' "$run_root/run.json"
[[ "$(jq -r '.ok' "$run_root/evidence/verification.json")" == true ]]
[[ "$(jq -r '.cockpitManaged' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.cockpitControlPlaneIsolated' "$run_root/evidence/verification.json")" == true ]]
[[ "$(jq -r '.cockpitBundleMatches' "$run_root/evidence/verification.json")" == true ]]
[[ "$(jq -r '.autoSyncThreadsDisabled' "$run_root/evidence/verification.json")" == true ]]
[[ "$(jq -r 'has("autoSyncThreads")' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.launcherMode' "$run_root/evidence/verification.json")" == cockpit-compatible-adapter-prepared-only ]]
[[ "$(jq -r '.codexInstanceAcceptanceComplete' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.codexFoldCandidateAcceptanceComplete' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.realAcceptanceComplete' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.backendCrashRespawnEvidenceValid' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.nativeIncidentEvidenceValid' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.nativeIncidentGUIReviewComplete' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.candidateFaultAcceptanceComplete' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.nativeIncidentAcceptanceComplete' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.candidateRuntimeEpoch' "$run_root/evidence/verification.json")" == none ]]
grep -Fq "Cockpit safety setting: \`autoSyncThreads\` persistently disabled=\`true\`" "$run_root/evidence/report.md"
[[ "$(jq -r '.startup_page' "$run_root/cockpit-data/config.json")" == codex-instances ]]
if rg -q 'mark-launched|"\$APP_PATH" --args' "$RUNNER" "$ROOT_DIR/scripts/cockpit-codex-instance-adapter.sh"; then
  echo "prepared-only adapter can fabricate Cockpit launch evidence" >&2
  exit 1
fi
[[ "$(sqlite3 "$run_root/codex-home/state_5.sqlite" 'SELECT count(*) FROM threads;')" == 1 ]]
[[ "$(jq -c 'keys | sort' "$run_root/codex-home/auth.json")" == '["OPENAI_API_KEY","auth_mode"]' ]]
[[ "$(jq -r '.auth_mode' "$run_root/codex-home/auth.json")" == apikey ]]
[[ "$(jq -r '.OPENAI_API_KEY | type' "$run_root/codex-home/auth.json")" == string ]]
[[ "$(jq -r '.OPENAI_API_KEY | length' "$run_root/codex-home/auth.json")" -gt 0 ]]
for isolated_dir in "$run_root/codex-home/.tmp/plugins" "$run_root/codex-home/skills" "$run_root/codex-home/plugins"; do
  [[ -d "$isolated_dir" && ! -L "$isolated_dir" ]]
  [[ "$(stat -f '%p' "$isolated_dir")" == 40700 ]]
done
if rg -q '^\[(plugins|marketplaces|mcp_servers)\.' "$run_root/codex-home/config.toml"; then
  echo "isolated CODEX_HOME retained plugin, marketplace, or MCP configuration" >&2
  exit 1
fi
grep -Fq 'preferred_auth_method = "apikey"' "$run_root/codex-home/config.toml"
grep -Fq 'forced_login_method = "api"' "$run_root/codex-home/config.toml"
grep -Fq 'cli_auth_credentials_store = "file"' "$run_root/codex-home/config.toml"
if grep -Fq 'experimental_bearer_token' "$run_root/codex-home/config.toml"; then
  echo "source provider bearer value leaked into isolated acceptance config" >&2
  exit 1
fi

# Historical v2 runs cannot be upgraded in place because they did not capture
# the loaded Swift module or source/build provenance.
cp -p "$run_root/run.json" "$TEST_ROOT/run.v3.saved.json"
jq '.schema="codexfold.isolated-acceptance.v2"' "$run_root/run.json" > "$TEST_ROOT/run.v2.json"
mv "$TEST_ROOT/run.v2.json" "$run_root/run.json"
if legacy_run_error=$("$RUNNER" verify --run-root "$run_root" 2>&1); then
  echo "legacy v2 run passed the v3 verifier" >&2
  exit 1
fi
grep -Fq 'create a fresh codexfold.isolated-acceptance.v3 run' <<< "$legacy_run_error"
mv "$TEST_ROOT/run.v3.saved.json" "$run_root/run.json"

# The prepared pre-mount process baseline is immutable evidence.
cp -p "$module_baseline" "$TEST_ROOT/module-baseline.saved.tsv"
printf 'tampered\n' >> "$module_baseline"
if "$RUNNER" verify --run-root "$run_root" >/dev/null 2>&1; then
  echo "tampered FSKit module process baseline passed verification" >&2
  exit 1
fi
cp -p "$TEST_ROOT/module-baseline.saved.tsv" "$module_baseline"
mv "$module_baseline" "$TEST_ROOT/module-baseline.missing.tsv"
if "$RUNNER" verify --run-root "$run_root" >/dev/null 2>&1; then
  echo "missing FSKit module process baseline passed verification" >&2
  exit 1
fi
mv "$TEST_ROOT/module-baseline.missing.tsv" "$module_baseline"
cp -p "$protected_baseline" "$TEST_ROOT/protected-baseline.saved.tsv"
printf 'tampered\n' >> "$protected_baseline"
if "$RUNNER" verify --run-root "$run_root" >/dev/null 2>&1; then
  echo "tampered protected Codex baseline passed verification" >&2
  exit 1
fi
cp -p "$TEST_ROOT/protected-baseline.saved.tsv" "$protected_baseline"

# Bundle metadata alone is not build identity. Keeping Info.plist unchanged while
# mutating the executable must invalidate the prepared evidence.
fake_executable="$fake_app/Contents/MacOS/fake-codex"
cp -p "$fake_executable" "$TEST_ROOT/fake-codex.saved"
printf '# mutated bytes\n' >> "$fake_executable"
if "$RUNNER" verify --run-root "$run_root" >/dev/null 2>&1; then
  echo "bundle executable mutation passed with unchanged Info.plist" >&2
  exit 1
fi
cp -p "$TEST_ROOT/fake-codex.saved" "$fake_executable"
"$RUNNER" verify --run-root "$run_root" >/dev/null
if rg -q 'secret-that-must-not-appear-in-evidence|copied without being printed' "$run_root/evidence" "$run_root/run.json" "$run_root/cockpit-instance.json"; then
  echo "credential or session contents leaked into evidence" >&2
  exit 1
fi

cockpit_root=$(jq -r '.cockpitDataRoot' "$run_root/run.json")
cockpit_store=$(jq -r '.cockpitStorePath' "$run_root/run.json")
[[ "$cockpit_root" == "$(jq -r '.codexHome' "$run_root/run.json" | sed 's#/codex-home$#/cockpit-data#')" ]]
printf '%s\n' '{"instances":[{"id":"unrelated","name":"keep","userDataDir":"/tmp/unrelated","workingDir":null,"extraArgs":"","bindAccountId":null,"launchMode":"app","createdAt":1,"lastLaunchedAt":null,"lastPid":null}],"defaultSettings":{"autoSyncThreads":true,"protectConfigOnLaunch":true}}' > "$cockpit_store"

store_before=$(shasum -a 256 "$cockpit_store" | awk '{print $1}')
register_dry=$("$RUNNER" cockpit-register --run-root "$run_root" --dry-run)
grep -Fq 'dry-run:' <<< "$register_dry"
[[ "$(shasum -a 256 "$cockpit_store" | awk '{print $1}')" == "$store_before" ]]

"$RUNNER" cockpit-register --run-root "$run_root" --apply >/dev/null
instance_id=$(jq -r '.runId' "$run_root/run.json")
isolated_home=$(jq -r '.codexHome' "$run_root/run.json")
expected_electron="$cockpit_root/instances/codex-app-data/$(md5 -q -s "$isolated_home")"
[[ "$(jq -r '.electronUserData' "$run_root/run.json")" == "$expected_electron" ]]
[[ "$(jq -r '.defaultSettings.autoSyncThreads' "$cockpit_store")" == false ]]
[[ "$(jq --arg id "$instance_id" '[.instances[] | select(.id == $id)] | length' "$cockpit_store")" == 1 ]]
[[ "$(jq -r --arg id "$instance_id" '.instances[] | select(.id == $id) | .userDataDir' "$cockpit_store")" == "$isolated_home" ]]
[[ "$(jq --arg id unrelated '[.instances[] | select(.id == $id)] | length' "$cockpit_store")" == 1 ]]
[[ -f "$run_root/private/cockpit-store.before.json" ]]
"$RUNNER" verify --run-root "$run_root" >/dev/null
[[ "$(jq -r '.cockpitManaged' "$run_root/evidence/verification.json")" == true ]]

# Persisted safety switches are exact booleans. Missing autoSyncThreads or an
# enabled launch-time repair policy must fail closed.
cp "$cockpit_store" "$TEST_ROOT/cockpit-store.safe.json"
jq 'del(.defaultSettings.autoSyncThreads)' "$cockpit_store" > "$TEST_ROOT/store.tmp"
mv "$TEST_ROOT/store.tmp" "$cockpit_store"
if "$RUNNER" verify --run-root "$run_root" >/dev/null 2>&1; then
  echo "missing persisted autoSyncThreads was accepted" >&2
  exit 1
fi
cp "$TEST_ROOT/cockpit-store.safe.json" "$cockpit_store"
jq '.defaultSettings.autoRepairSessionVisibilityOnLaunch=true' "$cockpit_store" > "$TEST_ROOT/store.tmp"
mv "$TEST_ROOT/store.tmp" "$cockpit_store"
if "$RUNNER" verify --run-root "$run_root" >/dev/null 2>&1; then
  echo "enabled automatic session repair was accepted" >&2
  exit 1
fi
cp "$TEST_ROOT/cockpit-store.safe.json" "$cockpit_store"

# The adapter will not carry stale positive lastPid/lastLaunchedAt into a fresh
# launch observation.
jq --arg id "$instance_id" '(.instances[] | select(.id==$id) | .lastPid)=123 | (.instances[] | select(.id==$id) | .lastLaunchedAt)=456' \
  "$cockpit_store" > "$TEST_ROOT/store.tmp"
mv "$TEST_ROOT/store.tmp" "$cockpit_store"
if "$RUNNER" cockpit-register --run-root "$run_root" --apply >/dev/null 2>&1; then
  echo "adapter reused stale Cockpit launch state" >&2
  exit 1
fi
cp "$TEST_ROOT/cockpit-store.safe.json" "$cockpit_store"
if "$run_root/verify.sh" >/dev/null 2>&1; then
  echo "prepared-only evidence incorrectly passed the real acceptance gate" >&2
  exit 1
fi

launch_output=$("$RUNNER" launch --run-root "$run_root" --dry-run)
grep -Fq 'dry-run:' <<< "$launch_output"
[[ ! -e "$run_root/evidence/launch.applied" ]]

prompt_file="$TEST_ROOT/prompt.txt"
printf 'Inspect the isolated fixture.\n' > "$prompt_file"
task_output=$("$RUNNER" task --run-root "$run_root" --prompt-file "$prompt_file" --dry-run)
grep -Fq 'dry-run:' <<< "$task_output"

# A real isolated task creates another thread. The source slice remains an
# identity fence, while additional paths are accepted only inside this home.
new_session=55555555-5555-7555-8555-555555555555
new_rollout="$isolated_home/sessions/2026/07/26/rollout-2026-07-26T04-00-00-$new_session.jsonl"
printf '{"session":"new isolated work"}\n' > "$new_rollout"
sql_new_rollout=${new_rollout//\'/\'\'}
sqlite3 "$run_root/codex-home/state_5.sqlite" \
  "INSERT INTO threads VALUES('$new_session', '$sql_new_rollout', 2, 0);"
"$RUNNER" verify --run-root "$run_root" >/dev/null
[[ "$(jq -r '.sessionCount' "$run_root/evidence/verification.json")" == 1 ]]
[[ "$(jq -r '.currentThreadCount' "$run_root/evidence/verification.json")" == 2 ]]

descriptor="$run_root/candidate/backend.json"
printf '{"socket":"candidate.sock"}\n' > "$descriptor"
fault_output=$("$RUNNER" fault --run-root "$run_root" --kind descriptor --target "$descriptor" --duration 1 --dry-run)
grep -Fq 'dry-run:' <<< "$fault_output"
grep -Fq 'candidate.sock' "$descriptor"

if "$RUNNER" fault --run-root "$run_root" --kind descriptor --target "$descriptor" --duration 1 --apply >/dev/null 2>&1; then
  echo "candidate fault ran without externally validated candidate attachment evidence" >&2
  exit 1
fi

# A legacy backend STOP/CONT diagnostic row has no crash-completion authority.
printf '%s\tapply\tbackend\t12\tresumed\tbackend.pid\t%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$(printf '0%.0s' {1..64})" > "$run_root/evidence/faults.tsv"
chmod 600 "$run_root/evidence/faults.tsv"
"$RUNNER" verify --run-root "$run_root" >/dev/null
[[ "$(jq -r '.backendCrashRespawnEvidenceValid' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.candidateFaultAcceptanceComplete' "$run_root/evidence/verification.json")" == false ]]
[[ "$(jq -r '.realAcceptanceComplete' "$run_root/evidence/verification.json")" == false ]]

candidate_root=$(jq -r '.candidateRoot' "$run_root/run.json")
candidate_mount="$candidate_root/mount"
mkdir -p "$candidate_mount"
candidate_binary="$candidate_root/codexfold-candidate"
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' > "$candidate_binary"
chmod 700 "$candidate_binary"
candidate_sha=$(shasum -a 256 "$candidate_binary" | awk '{print $1}')
candidate_definition="$candidate_root/service.plist"
printf '<plist version="1.0"></plist>\n' > "$candidate_definition"
managed_path=$(sqlite3 "$run_root/codex-home/state_5.sqlite" "SELECT rollout_path FROM threads WHERE id='$session_id';")
managed_bytes=$(stat -f '%z' "$managed_path")
managed_sha=$(shasum -a 256 "$managed_path" | awk '{print $1}')
legacy_candidate_input="$TEST_ROOT/candidate-input-v2.json"
jq -n \
  --arg schema codexfold.external-candidate-evidence.v2 \
  --arg observedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  --arg codexHome "$isolated_home" \
  --arg candidateRoot "$candidate_root" \
  --arg mountPoint "$candidate_mount" \
  --arg candidateBinaryPath "$candidate_binary" \
  --arg serviceDefinitionPath "$candidate_definition" \
  --arg candidateBuildSHA "$candidate_sha" \
  --arg sessionId "$session_id" \
  --arg rolloutPath "$managed_path" \
  --arg routeSHA "$managed_sha" \
  --argjson daemonPid "$$" \
  --argjson routeBytes "$managed_bytes" \
  '{schema:"codexfold.external-candidate-evidence.v2",observedAt:$observedAt,codexHome:$codexHome,candidateRoot:$candidateRoot,mountPoint:$mountPoint,candidateAttached:true,candidateBinaryPath:$candidateBinaryPath,serviceDefinitionPath:$serviceDefinitionPath,candidateBuildSHA:$candidateBuildSHA,serviceStatus:{daemon_running:true,daemon_pid:$daemonPid,mount_healthy:true,build:{healthy:true,running_build_sha256:$candidateBuildSHA,configured_build_sha256:$candidateBuildSHA,configured_binary_path:$candidateBinaryPath}},managedRouteObserved:true,managedRoutes:[{sessionId:$sessionId,rolloutPath:$rolloutPath,bytes:$routeBytes,sha256:$routeSHA}],faultTargets:{descriptor:$rolloutPath},untrustedSecret:"candidate-secret-must-not-be-retained"}' \
  > "$legacy_candidate_input"

# v2 bound only the Go daemon/build and cannot be reused as v3 Swift-module
# evidence.
if legacy_candidate_error=$("$RUNNER" candidate-evidence --run-root "$run_root" --input "$legacy_candidate_input" --dry-run 2>&1); then
  echo "legacy v2 external candidate evidence passed the v3 verifier" >&2
  exit 1
fi
grep -Fq 'cannot prove the loaded Swift FSKit module' <<< "$legacy_candidate_error"

# A candidate App may not name a module from another build. This must fail at
# the exact nested-module boundary before any mount or live-service check.
candidate_app="$candidate_root/Current Candidate.app"
old_module="$candidate_root/OldBuild/CodexFoldFSKitModule.appex"
mkdir -p "$candidate_app/Contents" "$old_module/Contents"
plutil -create xml1 "$candidate_app/Contents/Info.plist"
plutil -create xml1 "$old_module/Contents/Info.plist"
candidate_pid_file="$candidate_root/backend.pid"
candidate_backend_status="$candidate_root/backend-status.json"
printf '4242\n' > "$candidate_pid_file"
printf '{"schemaVersion":2,"component":"daemon","state":"healthy","pid":4242,"mountPoint":"%s","backendID":"fixture-backend"}\n' "$candidate_mount" > "$candidate_backend_status"
candidate_input="$TEST_ROOT/candidate-input-v3-module-mismatch.json"
zero_sha=$(printf '0%.0s' {1..64})
zero_cdhash=$(printf '0%.0s' {1..40})
jq \
  --arg schema codexfold.external-candidate-evidence.v3 \
  --arg candidateApp "$candidate_app" \
  --arg candidateAppIdentity "$(stat -f '%d:%i' "$candidate_app")" \
  --arg oldModule "$old_module" \
  --arg oldModuleIdentity "$(stat -f '%d:%i' "$old_module")" \
  --arg zeroSHA "$zero_sha" \
  --arg zeroCDHash "$zero_cdhash" \
  --arg baselineSHA "$(jq -r '.fskitModuleProcessBaselineSHA256' "$run_root/run.json")" \
  --arg candidatePIDFile "$candidate_pid_file" \
  --arg candidateBackendStatus "$candidate_backend_status" \
  '.schema=$schema |
   .faultTargets.backendPidFile=$candidatePIDFile |
   .faultTargets.backendStatus=$candidateBackendStatus |
   .candidateApp={path:$candidateApp,directoryIdentity:$candidateAppIdentity,bundleIdentifier:"vip.jstar.codexfold.verification",shortVersion:"0.3.0",bundleVersion:"104",executablePath:($candidateApp+"/Contents/MacOS/CodexFoldVerification"),executableSHA256:$zeroSHA,codeDirectoryHash:$zeroCDHash,teamIdentifier:"ABCDEFGHIJ"} |
   .fskitModule={bundlePath:$oldModule,directoryIdentity:$oldModuleIdentity,bundleIdentifier:"vip.jstar.codexfold.verification.module",shortVersion:"0.3.0",bundleVersion:"104",fsShortName:"codexfoldverification",executablePath:($oldModule+"/Contents/MacOS/CodexFoldFSKitModule"),executableSHA256:$zeroSHA,codeDirectoryHash:$zeroCDHash,teamIdentifier:"ABCDEFGHIJ",process:{pid:4242,ppid:1,processStart:"Sun Jul 26 00:00:00 2026",executablePath:($oldModule+"/Contents/MacOS/CodexFoldFSKitModule"),executableSHA256:$zeroSHA,commandSHA256:$zeroSHA,baselineSHA256:$baselineSHA}}' \
  "$legacy_candidate_input" > "$candidate_input"
if module_mismatch_error=$("$RUNNER" candidate-evidence --run-root "$run_root" --input "$candidate_input" --dry-run 2>&1); then
  echo "module from another App build fabricated candidate evidence" >&2
  exit 1
fi
grep -Fq 'FSKit module bundle is not the exact module nested in candidate App' <<< "$module_mismatch_error"
[[ ! -e "$run_root/evidence/codexfold-candidate-observed.json" ]]

outside="$TEST_ROOT/outside.json"
printf '{}\n' > "$outside"
if "$RUNNER" fault --run-root "$run_root" --kind descriptor --target "$outside" --dry-run >/dev/null 2>&1; then
  echo "outside fault target was accepted" >&2
  exit 1
fi

"$RUNNER" verify --run-root "$run_root" >/dev/null
[[ "$(jq -r '.protectedCodexUnchanged' "$run_root/evidence/verification.json")" == true ]]
[[ "$(jq -r '.sessionSliceValid' "$run_root/evidence/verification.json")" == true ]]
grep -Fq "Pre-existing Codex PID/start-time unchanged: \`true\`" "$run_root/evidence/report.md"

# Every fixed root is fenced by leaf type, real path, and inode. Replacing a
# candidate root with an outside symlink is rejected before any fault action.
mv "$run_root/candidate" "$run_root/candidate.saved"
outside_candidate="$TEST_ROOT/outside-candidate"
mkdir "$outside_candidate"
ln -s "$outside_candidate" "$run_root/candidate"
if "$RUNNER" verify --run-root "$run_root" >/dev/null 2>&1; then
  echo "symlink-swapped candidate root escaped the run fence" >&2
  exit 1
fi
rm "$run_root/candidate"
mv "$run_root/candidate.saved" "$run_root/candidate"

# Cockpit data root replacement must not redirect registration into an outside
# store. Use a separate fresh run so the main fixture remains usable.
escape_run="$TEST_ROOT/escape-run"
"$RUNNER" prepare --source-home "$source_home" --run-root "$escape_run" --session-count 1 \
  --app "$fake_app" --protected-app "$fake_app" --cockpit-app "$fake_app" --workspace "$workspace" >/dev/null
mv "$escape_run/cockpit-data" "$escape_run/cockpit-data.saved"
outside_cockpit="$TEST_ROOT/outside-cockpit"
mkdir "$outside_cockpit"
ln -s "$outside_cockpit" "$escape_run/cockpit-data"
if "$RUNNER" cockpit-register --run-root "$escape_run" --apply >/dev/null 2>&1; then
  echo "symlink-swapped Cockpit root redirected adapter writes" >&2
  exit 1
fi
[[ ! -e "$outside_cockpit/codex_instances.json" ]]

# Durable descriptor recovery removes only the exact placeholder inode created
# by the fault transaction. Candidate-rebuilt content is preserved, while the
# displaced original remains retained for inspection.
fault_recovery_run="$TEST_ROOT/fault-recovery-run"
"$RUNNER" prepare --source-home "$source_home" --run-root "$fault_recovery_run" --session-count 1 \
  --app "$fake_app" --protected-app "$fake_app" --cockpit-app "$fake_app" --workspace "$workspace" >/dev/null
fault_candidate="$fault_recovery_run/candidate"

# Handwritten GUI booleans without the bound census, screenshots, exports, and
# observed artifact can never satisfy native incident acceptance.
review_only="$fault_recovery_run/evidence/native-incident-review.json"
printf '%s\n' '{"schema":"codexfold.native-incident-review.v1","observedEvidenceSHA256":"'"$(printf '0%.0s' {1..64})"'","reviewedAt":"2026-07-27T00:00:00Z","reviewedWindowIDs":[11,22],"reviewedScreenshotSHA256s":["a","b","c"],"reasonVisible":true,"impactVisible":true,"recommendationsVisible":true,"technicalDetailsExpandedAndVisible":true,"recoveryUpdateVisible":true,"newEpochNewWindowVisible":true,"reviewerStatement":"I reviewed these alleged windows."}' > "$review_only"
review_only_sha=$(shasum -a 256 "$review_only" | awk '{print $1}')
jq --arg sha "$review_only_sha" '.nativeIncidentReviewSHA256=$sha' "$fault_recovery_run/run.json" > "$TEST_ROOT/run.tmp"
mv "$TEST_ROOT/run.tmp" "$fault_recovery_run/run.json"
chmod 600 "$fault_recovery_run/run.json"
if "$RUNNER" verify --run-root "$fault_recovery_run" >/dev/null 2>&1; then
  echo "boolean-only native incident review passed verification" >&2
  exit 1
fi
[[ "$(jq -r '.nativeIncidentGUIReviewComplete' "$fault_recovery_run/evidence/verification.json")" == false ]]
[[ "$(jq -r '.realAcceptanceComplete' "$fault_recovery_run/evidence/verification.json")" == false ]]
rm "$review_only"
jq '.nativeIncidentReviewSHA256=null' "$fault_recovery_run/run.json" > "$TEST_ROOT/run.tmp"
mv "$TEST_ROOT/run.tmp" "$fault_recovery_run/run.json"
chmod 600 "$fault_recovery_run/run.json"

fault_evidence="$fault_recovery_run/evidence/codexfold-candidate-observed.json"
printf '{"schema":"synthetic-recovery-anchor"}\n' > "$fault_evidence"
chmod 600 "$fault_evidence"
fault_evidence_sha=$(shasum -a 256 "$fault_evidence" | awk '{print $1}')
jq --arg sha "$fault_evidence_sha" \
  '.candidateAttached=true | .managedRouteObserved=true | .candidateBuildSHA=("0"*64) | .candidateEvidenceSHA256=$sha' \
  "$fault_recovery_run/run.json" > "$TEST_ROOT/run.tmp"
mv "$TEST_ROOT/run.tmp" "$fault_recovery_run/run.json"
chmod 600 "$fault_recovery_run/run.json"
if anchor_overwrite_error=$("$RUNNER" candidate-evidence --run-root "$fault_recovery_run" --input "$fault_evidence" --apply 2>&1); then
  echo "existing candidate anchor was overwritten" >&2
  exit 1
fi
grep -Fq 'candidate anchor evidence already exists and is immutable' <<< "$anchor_overwrite_error"

fault_tx="$fault_candidate/.acceptance-faults/restore-test"
mkdir -p "$fault_tx"
fault_target="$fault_candidate/descriptor.json"
printf 'original-descriptor\n' > "$fault_tx/original"
printf 'acceptance-placeholder\n' > "$fault_target"
placeholder_identity=$(stat -f '%d:%i:%z:%m:%c:%p:%HT' "$fault_target")
jq -n --arg target "$fault_target" --arg evidenceSHA "$fault_evidence_sha" --arg placeholder "$placeholder_identity" \
  '{schema:"codexfold.acceptance-fault.v1",kind:"descriptor",state:"injected",target:$target,candidateEvidenceSHA256:$evidenceSHA,placeholderIdentity:$placeholder}' \
  > "$fault_tx/token.json"
chmod 600 "$fault_tx/token.json"
if "$RUNNER" verify --run-root "$fault_recovery_run" >/dev/null 2>&1; then
  echo "synthetic candidate metadata unexpectedly passed live verification" >&2
  exit 1
fi
grep -Fqx 'original-descriptor' "$fault_target"
[[ "$(jq -r '.state' "$fault_tx/token.json")" == restored ]]

conflict_tx="$fault_candidate/.acceptance-faults/conflict-test"
mkdir -p "$conflict_tx"
printf 'retained-original\n' > "$conflict_tx/original"
printf 'candidate-rebuilt-content\n' > "$fault_target"
jq -n --arg target "$fault_target" --arg evidenceSHA "$fault_evidence_sha" --arg placeholder 'nonmatching-placeholder-identity' \
  '{schema:"codexfold.acceptance-fault.v1",kind:"descriptor",state:"injected",target:$target,candidateEvidenceSHA256:$evidenceSHA,placeholderIdentity:$placeholder}' \
  > "$conflict_tx/token.json"
chmod 600 "$conflict_tx/token.json"
if "$RUNNER" verify --run-root "$fault_recovery_run" >/dev/null 2>&1; then
  echo "synthetic candidate metadata unexpectedly passed after conflict recovery" >&2
  exit 1
fi
grep -Fqx 'candidate-rebuilt-content' "$fault_target"
grep -Fqx 'retained-original' "$conflict_tx/original"
[[ "$(jq -r '.state' "$conflict_tx/token.json")" == candidate-rebuilt-preserved ]]

echo "PASS: isolated acceptance runner prepares, guards, verifies, and records without touching production"
