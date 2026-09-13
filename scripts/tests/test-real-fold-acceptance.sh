#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)
RUNNER="$ROOT_DIR/scripts/run-real-fold-acceptance.sh"
ISOLATED_RUNNER="$ROOT_DIR/scripts/run-isolated-codex-acceptance.sh"
TMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/codexfold-real-fold-test.XXXXXX")
trap 'rm -rf "$TMP_ROOT"' EXIT HUP INT TERM

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[[ -x "$RUNNER" ]] || fail "real-fold runner is not executable"
bash -n "$RUNNER"
shellcheck -x "$RUNNER"

help=$($RUNNER --help)
[[ "$help" == *'fold -> pack -> migrate'* ]] || fail "help omits the real automatic transaction"
[[ "$help" == *'never writes to the source CODEX_HOME'* ]] || fail "help omits the production fence"
[[ "$help" == *'--candidate-bin PATH'* ]] || fail "help omits exact prebuilt candidate binding"
[[ "$help" == *'fixed registered Verification App'* ]] || fail "help omits the fixed Verification App identity"
isolated_help=$($ISOLATED_RUNNER real-fold --help)
[[ "$isolated_help" == *'gpt-5.6-terra'* ]] || fail "isolated acceptance does not expose real-fold"

# shellcheck disable=SC1090
source "$RUNNER"

prebuilt="$TMP_ROOT/prebuilt-candidate"
staged="$TMP_ROOT/staged/codexfold"
printf '#!/usr/bin/env bash\nexit 0\n' > "$prebuilt"
chmod 755 "$prebuilt"
prebuilt_sha=$(file_sha "$prebuilt")
[[ "$(stage_prebuilt_candidate "$prebuilt" "$staged")" == "$prebuilt_sha" ]] || fail "prebuilt candidate was not staged byte-exactly"
[[ "$(file_sha "$staged")" == "$prebuilt_sha" ]] || fail "staged candidate bytes changed"
ln -s "$prebuilt" "$TMP_ROOT/prebuilt-symlink"
if stage_prebuilt_candidate "$TMP_ROOT/prebuilt-symlink" "$TMP_ROOT/rejected/codexfold" >/dev/null 2>&1; then
  fail "symlinked prebuilt candidate was accepted"
fi

source_home="$TMP_ROOT/source"
external_run="$TMP_ROOT/run"
mkdir -p "$source_home"
require_isolated_layout "$external_run" "$source_home" || fail "external run root was rejected"
if require_isolated_layout "$source_home/run" "$source_home"; then
  fail "run root inside source CODEX_HOME was accepted"
fi
if require_isolated_layout "$TMP_ROOT" "$source_home"; then
  fail "run root containing source CODEX_HOME was accepted"
fi

policy="$TMP_ROOT/policy/enrollment/policy.json"
write_policy "$policy" false
jq -e '.version == 1 and .enabled == false and .interval == "30s" and .stable_for == "1s" and .archived_only == true and .batch_size == 1' \
  "$policy" >/dev/null || fail "disabled policy contract is wrong"
write_policy "$policy" true
jq -e '.enabled == true' "$policy" >/dev/null || fail "policy hot-enable contract is wrong"

config="$TMP_ROOT/config.toml"
cat > "$config" <<'EOF'
[model_providers.main]
base_url = "https://example.invalid/v1"
experimental_bearer_token = "must-be-removed"
env_key = "OPENAI_API_KEY"
requires_openai_auth = true
EOF
normalize_isolated_api_key_config "$config"
! grep -Eq '^env_key[[:space:]]*=' "$config" || fail "environment dependency was retained"
grep -Fq 'requires_openai_auth = true' "$config" || fail "file API key auth was not enabled"
grep -Fq 'forced_login_method = "api"' "$config" || fail "API-only login was not enforced"
grep -Fq 'cli_auth_credentials_store = "file"' "$config" || fail "file credential store was not enforced"
normalize_isolated_api_key_config "$config"
[[ $(grep -c '^forced_login_method =' "$config") == 1 ]] || fail "normalization duplicated auth settings"
if grep -Fq 'must-be-removed' "$config"; then
  fail "copied inline bearer token was retained"
fi

rollout="$source_home/archived.jsonl"
dd if=/dev/zero of="$rollout" bs=65536 count=1 >/dev/null 2>&1
sqlite3 "$source_home/state_5.sqlite" <<SQL
CREATE TABLE threads(id TEXT PRIMARY KEY, archived INTEGER, rollout_path TEXT, updated_at INTEGER);
INSERT INTO threads VALUES('real-session',1,'$rollout',2);
INSERT INTO threads VALUES('active-session',0,'$rollout',3);
SQL
row=$(source_session_row "$source_home" '')
[[ "$(printf '%s' "$row" | awk -F '\t' '{print $1}')" == real-session ]] || fail "automatic selection did not choose the archived real session"
if source_session_row "$source_home" active-session >/dev/null 2>&1; then
  fail "explicit active session was accepted"
fi

run_marker="$TMP_ROOT/marked-run"
mkdir -p "$run_marker"
/bin/sh -c 'sleep 30' "$run_marker" &
owned_pid=$!
process_belongs_to_run "$owned_pid" "$run_marker" || fail "marked disposable process was not recognized"
if process_belongs_to_run "$$" "$run_marker"; then
  fail "unrelated process was treated as disposable"
fi
kill "$owned_pid"
wait "$owned_pid" 2>/dev/null || true

grep -Fq 'production source rollout changed during isolated acceptance' "$RUNNER" || fail "source identity/SHA fence is missing"
grep -Fq 'automatic folding retained duplicate loose objects' "$RUNNER" || fail "duplicate retirement assertion is missing"
grep -Fq 'automatic folding did not reduce allocated disk space' "$RUNNER" || fail "physical disk savings assertion is missing"
grep -Fq 'runtime and copied credentials removed' "$RUNNER" || fail "credential cleanup evidence is missing"
grep -Fq 'VERIFICATION_APP_BUNDLE_ID=vip.jstar.codexfold.fskitacceptance108' "$RUNNER" || fail "native runner does not pin the Verification App bundle id"
grep -Fq 'VERIFICATION_MODULE_BUNDLE_ID=vip.jstar.codexfold.fskitacceptance108.module' "$RUNNER" || fail "native runner does not pin the Verification module bundle id"
grep -Fq 'VERIFICATION_FSKIT_TYPE=codexfoldverification' "$RUNNER" || fail "native runner does not pin the Verification FSShortName"
[[ -x "$ROOT_DIR/scripts/build-codexfold-verification-app.sh" ]] || fail "fixed Verification App builder is missing"
bash -n "$ROOT_DIR/scripts/build-codexfold-verification-app.sh"
shellcheck -x "$ROOT_DIR/scripts/build-codexfold-verification-app.sh"
# shellcheck disable=SC2016
grep -Fq 'OPENAI_API_KEY="$isolated_api_key"' "$RUNNER" || fail "isolated API key is not process-scoped for real Codex"
native_socket_assignment="FSKIT_SOCKET=\"\$FSKIT_RESOURCE/backend.sock\""
grep -Fq "$native_socket_assignment" "$RUNNER" || fail "native FSKit socket is not inside its short isolated resource"
grep -Fq 'fs supervise --resource' "$RUNNER" || fail "native FSKit real-fold acceptance does not start its isolated mount supervisor"

echo "PASS: real-fold acceptance has production fences and a public current-worktree entrypoint"
