#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PREPARE="$ROOT_DIR/scripts/prepare-isolated-codex-home.sh"
TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT

source_home="$TEST_ROOT/source"
target_home="$TEST_ROOT/target"
mkdir -p "$source_home/plugins/cache/example" "$source_home/skills/example"
printf 'model_provider = "main"\n[model_providers.main]\nname = "third-party"\nbase_url = "https://third-party.example/v1"\n' > "$source_home/config.toml"
printf '{"access_token":"test-token"}\n' > "$source_home/auth.json"
printf '{"client_version":"test","models":[]}\n' > "$source_home/models_cache.json"
printf 'plugin-source\n' > "$source_home/plugins/cache/example/state"
printf 'skill-source\n' > "$source_home/skills/example/SKILL.md"
chmod 700 "$source_home"
chmod 600 "$source_home/config.toml" "$source_home/auth.json"

"$PREPARE" "$source_home" "$target_home" >/dev/null

cmp -s "$source_home/config.toml" "$target_home/config.toml"
cmp -s "$source_home/auth.json" "$target_home/auth.json"
cmp -s "$source_home/models_cache.json" "$target_home/models_cache.json"
[[ "$(stat -f '%Lp' "$target_home/config.toml")" == 600 ]]
[[ "$(stat -f '%Lp' "$target_home/auth.json")" == 600 ]]
[[ "$(stat -f '%Lp' "$target_home/models_cache.json")" == 600 ]]
[[ -d "$target_home/plugins" && ! -L "$target_home/plugins" ]]
printf 'plugin-target\n' > "$target_home/plugins/cache/example/state"
grep -Fqx 'plugin-source' "$source_home/plugins/cache/example/state"

echo "PASS: isolated CODEX_HOME preserves current provider/auth and clones mutable canary assets"
