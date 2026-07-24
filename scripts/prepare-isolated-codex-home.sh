#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  printf 'usage: %s SOURCE_CODEX_HOME TARGET_CODEX_HOME\n' "$0" >&2
  exit 2
fi

source_home=$(cd "$1" && pwd)
target_home=$2

if [[ "$source_home" == "$target_home" ]]; then
  echo "source and target CODEX_HOME must differ" >&2
  exit 2
fi
if [[ ! -f "$source_home/config.toml" || ! -f "$source_home/auth.json" ]]; then
  echo "source CODEX_HOME must contain config.toml and auth.json" >&2
  exit 2
fi
if [[ -e "$target_home" ]]; then
  echo "target CODEX_HOME already exists: $target_home" >&2
  exit 2
fi

mkdir -p "$target_home"
chmod 700 "$target_home"

# Keep provider/auth state byte-identical; only the home directory around it is isolated.
for name in config.toml auth.json; do
  cp -p "$source_home/$name" "$target_home/$name"
  chmod 600 "$target_home/$name"
done
if [[ -f "$source_home/models_cache.json" ]]; then
  cp -p "$source_home/models_cache.json" "$target_home/models_cache.json"
  chmod 600 "$target_home/models_cache.json"
fi

# Current Codex configurations may reference a local model catalog by a relative
# path. Keep that immutable input inside the isolated home as well.
for catalog in cockpit-local-access-model-catalog.json; do
  if [[ -f "$source_home/$catalog" ]]; then
    cp -p "$source_home/$catalog" "$target_home/$catalog"
    chmod 600 "$target_home/$catalog"
  fi
done

mkdir -p "$target_home/sessions" "$target_home/archived_sessions"

# These assets are immutable inputs for the canary. APFS clone avoids duplicating their
# contents while copy-on-write keeps a canary update from touching the real home.
for name in plugins skills vendor_sources computer-use; do
  if [[ -e "$source_home/$name" ]]; then
    cp -cR "$source_home/$name" "$target_home/$name"
  fi
done

printf 'prepared isolated CODEX_HOME: %s\n' "$target_home"
