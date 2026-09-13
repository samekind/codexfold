#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/../.." && pwd)

if rg -n '/Users/jstar|/Volumes/JSData|019[0-9a-f]{5,}|mcxin|jstarctl' "$repo_root/scripts" --glob '!**/test-public-scripts-sanitized.sh'; then
  echo "public scripts contain private paths, session IDs, or control-plane names" >&2
  exit 1
fi

# Docs name real session IDs on purpose — they are the evidence a reader needs to
# follow an incident. A maintainer's home directory is not evidence of anything,
# and a published path that only exists on one machine is worse than useless.
if rg -n '/Users/[a-z]|/Volumes/JSData' "$repo_root/docs"; then
  echo "docs contain a private home or volume path; write ~/ or a repo-relative path" >&2
  exit 1
fi

grep -q 'CRITICAL_IDS_FILE' "$repo_root/scripts/activate-canonical-after-codex-exit.sh"

echo "PASS: public scripts are sanitized and critical session checks are parameterized"
