#!/bin/zsh
set -euo pipefail

export PATH="/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin"

CODEX_HOME="${1:?Codex home is required}"
STORE="${2:?CodexFold store is required}"
MOUNT="${3:?mount path is required}"
NATIVE_ROOT="${4:?native root is required}"
BIN="${5:?CodexFold binary is required}"
REOPEN_APP="${6:-1}"
CRITICAL_IDS_FILE="${7:-${CODEXFOLD_CRITICAL_IDS_FILE:-}}"
RUN_ROOT="${STORE}/activation/canonical-$(date '+%Y%m%d-%H%M%S')"

mkdir -p "${RUN_ROOT}"
exec >"${RUN_ROOT}/run.log" 2>&1

activated=0
finish() {
  exit_code=$?
  if (( exit_code != 0 )); then
    if (( activated == 1 )); then
      "${BIN}" fs service stop --apply || true
      if "${BIN}" fs namespace deactivate --apply \
        --codex-home "${CODEX_HOME}" --mount "${MOUNT}" --native-root "${NATIVE_ROOT}"; then
        "${BIN}" fs service start --apply \
          --codex-home "${CODEX_HOME}" --mount "${MOUNT}" || true
      fi
    fi
    date '+%Y-%m-%dT%H:%M:%S%z' >"${RUN_ROOT}/FAILED"
  fi
  if [[ "${REOPEN_APP}" == "1" ]]; then
    open -a /Applications/ChatGPT.app || true
  fi
  exit "${exit_code}"
}
trap finish EXIT

codex_running() {
  pgrep -f '/Applications/ChatGPT.app/Contents/MacOS/ChatGPT($| )' >/dev/null 2>&1 ||
    pgrep -f '/opt/homebrew/(Cellar/codex/[^/]+/bin|bin)/codex($| )' >/dev/null 2>&1
}

app_servers_running() {
  local pid command_line app_home
  while IFS= read -r pid; do
    [[ -z "${pid}" ]] && continue
    command_line="$(ps eww -p "${pid}" -o command= 2>/dev/null || true)"
    [[ -z "${command_line}" ]] && continue
    app_home="$(sed -n 's/.* CODEX_HOME=\([^ ]*\).*/\1/p' <<<"${command_line}")"
    if [[ -z "${app_home}" || "${app_home}" == "${CODEX_HOME}" ]]; then
      return 0
    fi
  done < <(pgrep -f '/Applications/ChatGPT.app/Contents/Resources/codex .*app-server' 2>/dev/null || true)
  return 1
}

wait_for_codex_drain() {
  echo "waiting for Codex Desktop and CLI to exit"
  while codex_running; do
    sleep 2
  done
  for _ in 1 2 3; do
    sleep 1
    if codex_running; then
      while codex_running; do
        sleep 2
      done
    fi
  done
  for _ in {1..30}; do
    if ! app_servers_running; then
      return 0
    fi
    sleep 1
  done
  echo "real-home Codex app servers did not drain"
  return 1
}

service_status="$(${BIN} fs service status --json)"
jq -e '.daemon_running == true and .mount_healthy == true' <<<"${service_status}" >/dev/null
compatibility="$(${BIN} fs compatibility --codex-home "${CODEX_HOME}" --store "${STORE}" --json)"
jq -e '.evaluation.approved == true and .evaluation.quarantine == false' <<<"${compatibility}" >/dev/null

managed_count="$(find "${STORE}/fs/sessions" -type f -name state.json 2>/dev/null | wc -l | tr -d ' ')"
[[ "${managed_count}" == "0" ]]
fold_route_count="$(sqlite3 "${CODEX_HOME}/state_5.sqlite" "select count(*) from threads where rollout_path like '${MOUNT}/%';")"
[[ "${fold_route_count}" == "0" ]]

snapshot_native_tree() {
  local root="$1"
  local output="$2"
  (
    cd "${root}"
    find -H sessions archived_sessions -type f ! -name '._*' \
      -exec stat -f '%N|%z|%m|%i|%p|%u|%g' {} + | LC_ALL=C sort
  ) >"${output}"
}

snapshot_critical() {
  local root="$1"
  local output="$2"
  local id rollout digest
  : >"${output}"
  [[ -z "${CRITICAL_IDS_FILE}" ]] && return 0
  [[ -f "${CRITICAL_IDS_FILE}" ]]
  while IFS= read -r id || [[ -n "${id}" ]]; do
    [[ -z "${id}" || "${id}" == \#* ]] && continue
    grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' <<<"${id}"
    rollout="$(find -H "${root}/sessions" "${root}/archived_sessions" -type f -name "rollout-*-${id}.jsonl" ! -name '._*' -print -quit)"
    [[ -n "${rollout}" ]]
    digest="$(shasum -a 256 "${rollout}" | awk '{print $1}')"
    printf '%s\t%s\n' "${id}" "${digest}" >>"${output}"
  done <"${CRITICAL_IDS_FILE}"
}

while true; do
  wait_for_codex_drain
  snapshot_native_tree "${CODEX_HOME}" "${RUN_ROOT}/tree.before"
  snapshot_critical "${CODEX_HOME}" "${RUN_ROOT}/critical.before"
  if codex_running || app_servers_running; then
    echo "Codex restarted during activation preflight; waiting again"
    continue
  fi
  break
done

"${BIN}" fs namespace activate --apply \
  --codex-home "${CODEX_HOME}" --mount "${MOUNT}" --native-root "${NATIVE_ROOT}" --json \
  >"${RUN_ROOT}/activate.json"
activated=1

[[ "$(readlink "${CODEX_HOME}/sessions")" == "${MOUNT}/sessions" ]]
[[ "$(readlink "${CODEX_HOME}/archived_sessions")" == "${MOUNT}/archived_sessions" ]]
trigger_count="$(sqlite3 "${CODEX_HOME}/state_5.sqlite" "select count(*) from sqlite_master where type='trigger' and name like 'codexfold_normalize_rollout_path_%';")"
[[ "${trigger_count}" == "2" ]]

snapshot_native_tree "${NATIVE_ROOT}" "${RUN_ROOT}/tree.native.after"
snapshot_critical "${NATIVE_ROOT}" "${RUN_ROOT}/critical.after"
diff -u "${RUN_ROOT}/tree.before" "${RUN_ROOT}/tree.native.after"
diff -u "${RUN_ROOT}/critical.before" "${RUN_ROOT}/critical.after"

service_status="$(${BIN} fs service status --json)"
jq -e '.daemon_running == true and .mount_healthy == true' <<<"${service_status}" >/dev/null
namespace_status="$(${BIN} fs namespace status --codex-home "${CODEX_HOME}" --mount "${MOUNT}" --native-root "${NATIVE_ROOT}" --json)"
jq -e '.active == true' <<<"${namespace_status}" >/dev/null

date '+%Y-%m-%dT%H:%M:%S%z' >"${RUN_ROOT}/COMPLETE"
trap - EXIT
if [[ "${REOPEN_APP}" == "1" ]]; then
  open -a /Applications/ChatGPT.app
fi
