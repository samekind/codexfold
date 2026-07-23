#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/../.." && pwd)

if rg -n '/Users/jstar|/Volumes/JSData|019[0-9a-f]{5,}|mcxin|jstarctl' "$repo_root/scripts" --glob '!**/test-public-scripts-sanitized.sh'; then
  echo "public scripts contain private paths, session IDs, or control-plane names" >&2
  exit 1
fi

grep -q 'CRITICAL_IDS_FILE' "$repo_root/scripts/activate-canonical-after-codex-exit.sh"

echo "PASS: public scripts are sanitized and critical session checks are parameterized"
