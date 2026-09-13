#!/usr/bin/env bash
set -euo pipefail

# Build the one and only disposable native-FSKit verification identity.  The
# source project remains untouched: XcodeGen runs against a private copy, and
# the finished signed bundle is atomically published at the fixed path below.

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd -P)
SOURCE_FSKIT="$REPO_ROOT/platform/darwin/fskit"
VERIFICATION_ROOT="$REPO_ROOT/.tmp/codexfold-verification"
VERIFICATION_APP="$HOME/Applications/CodexFoldVerification.app"
VERIFICATION_MANIFEST="$VERIFICATION_ROOT/manifest.json"

APP_BUNDLE_ID='vip.jstar.codexfold.fskitacceptance108'
MODULE_BUNDLE_ID='vip.jstar.codexfold.fskitacceptance108.module'
INCIDENT_BUNDLE_ID='vip.jstar.codexfold.verification.incident-monitor'
HOST_TEST_BUNDLE_ID='vip.jstar.codexfold.verification.hosttests'
FSKIT_SHORT_NAME='codexfoldverification'
APP_DISPLAY_NAME='CodexFold Verification'
MODULE_DISPLAY_NAME='CodexFold Verification Module'
APP_PRODUCT_NAME='CodexFoldVerification'
TEAM_ID='Y987FUR837'
SIGNING_IDENTITY="${CODEXFOLD_SIGNING_IDENTITY:-Apple Development: Jingyi YU (ASR9D8X5Q4)}"
PLIST_BUDDY=/usr/libexec/PlistBuddy

usage() {
  cat <<'EOF'
usage: build-codexfold-verification-app.sh

Build and register the fixed disposable native-FSKit verification App.

The output is always:
  ~/Applications/CodexFoldVerification.app

The script never changes the production App, production helper, production
CODEX_HOME, production mount, or production LaunchAgents. It registers only
the fixed verification path with LaunchServices; the FSKit security switch is
left for the operator to approve in System Settings.
EOF
}

die() {
  echo "build-codexfold-verification-app: $*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command is unavailable: $1"
}

file_sha() {
  shasum -a 256 "$1" | awk '{print $1}'
}

bundle_value() {
  local bundle=$1 key=$2
  plutil -extract "$key" raw -o - "$bundle/Contents/Info.plist"
}

codesign_value() {
  local path=$1 key=$2
  codesign -d --verbose=4 "$path" 2>&1 | sed -n "s/^${key}=//p" | head -n 1
}

provisioning_profile() {
  local identifier=$1 profile
  for profile in "$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles/"*.provisionprofile; do
    [[ -f "$profile" ]] || continue
    if security cms -D -i "$profile" 2>/dev/null \
      | plutil -extract Entitlements xml1 -o - - \
      | plutil -convert json -o - - \
      | jq -e --arg id "${TEAM_ID}.${identifier}" '."com.apple.application-identifier" == $id' >/dev/null; then
      printf '%s\n' "$profile"
      return 0
    fi
  done
  return 1
}

canonical_dir() {
  [[ -d "$1" && ! -L "$1" ]] || return 1
  (cd "$1" && pwd -P)
}

lsregister_path() {
  printf '%s\n' /System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister
}

ensure_no_running_fixed_module() {
  local module_exec="$VERIFICATION_APP/Contents/Extensions/CodexFoldFSKitModule.appex/Contents/MacOS/CodexFoldFSKitModule"
  [[ -x "$module_exec" ]] || return 0
  if pgrep -f "$module_exec" >/dev/null 2>&1; then
    die "the fixed verification module is running; finish/clean the isolated run before rebuilding"
  fi
}

ensure_single_registration() {
  local module_id=$1 output_module=$2 rows path
  rows=$(pluginkit -m -A -D -v -i "$module_id" 2>/dev/null || true)
  while IFS= read -r row; do
    [[ -n "$row" ]] || continue
    row=$(printf '%s' "$row" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
    [[ "$row" == "(no matches)" ]] && continue
    [[ "$row" == *$'\t'* ]] || continue
    path=${row##*$'\t'}
    path=$(printf '%s' "$path" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
    [[ -n "$path" ]] || continue
    [[ "$path" == "$output_module" ]] || die "another registration already uses $module_id: $path; remove that exact disposable registration first"
  done <<< "$rows"
}

patch_copy() {
  local copy_root=$1
  # These replacements are deliberately limited to the copied source. They
  # give macOS a distinct identity from the installed production module and
  # prevent Login Items & Extensions from merging the two switches.
  perl -pi -e "s/vip\\.jstar\\.codexfold\\.fskitprofileprobe/${APP_BUNDLE_ID}/g; s/codexfoldnative/${FSKIT_SHORT_NAME}/g; s/CodexFold Native FSKit Module/${MODULE_DISPLAY_NAME}/g; s/CodexFold FSKit/${APP_DISPLAY_NAME}/g" \
    "$copy_root/project.yml" \
    "$copy_root/Host/Info.plist" \
    "$copy_root/Extension/Info.plist" \
    "$copy_root/Host/CodexFoldMenuBar.plist" \
    "$copy_root/Host/CodexFoldIncidentMonitor.plist" \
    "$copy_root/Host/IncidentPresentation.swift" \
    "$copy_root/Extension/ProfileModule.swift"
  # The broad replacement above maps every target to the App id. Restore the
  # target-specific identifiers and make the built product name deterministic.
  perl -pi -e "s/PRODUCT_BUNDLE_IDENTIFIER: ${APP_BUNDLE_ID}\.incident-monitor/PRODUCT_BUNDLE_IDENTIFIER: ${INCIDENT_BUNDLE_ID}/; s/PRODUCT_BUNDLE_IDENTIFIER: ${APP_BUNDLE_ID}\.hosttests/PRODUCT_BUNDLE_IDENTIFIER: ${HOST_TEST_BUNDLE_ID}/; s/PRODUCT_BUNDLE_IDENTIFIER: ${APP_BUNDLE_ID}\.module/PRODUCT_BUNDLE_IDENTIFIER: ${MODULE_BUNDLE_ID}/" "$copy_root/project.yml"
  local project_with_product="${copy_root}/.project-with-product.$$"
  awk -v app="$APP_BUNDLE_ID" -v product="$APP_PRODUCT_NAME" '
    !inserted && $0 == "        PRODUCT_BUNDLE_IDENTIFIER: " app {
      print
      print "        PRODUCT_NAME: " product
      inserted=1
      next
    }
    { print }
    END { if (!inserted) exit 1 }
  ' "$copy_root/project.yml" > "$project_with_product" || {
    rm -f "$project_with_product"
    return 1
  }
  mv "$project_with_product" "$copy_root/project.yml"
  # The copied Swift log subsystem is not an identity gate, but keeping it
  # distinct makes diagnostics unambiguous when production is running.
  perl -pi -e "s/${APP_BUNDLE_ID}\\.module/${MODULE_BUNDLE_ID}/g" "$copy_root/Extension/ProfileModule.swift"
}

main() {
  [[ $# -eq 0 || ( $# -eq 1 && ( "$1" == "-h" || "$1" == "--help" ) ) ]] || die "no options are accepted; the verification path and identity are fixed"
  [[ $# -eq 0 ]] || { usage; return 0; }

  need_command ditto
  need_command xcodegen
  need_command xcodebuild
  need_command codesign
  need_command plutil
  need_command shasum
  need_command pgrep
  need_command jq
  need_command security
  [[ -d "$SOURCE_FSKIT" && -f "$SOURCE_FSKIT/project.yml" ]] || die "FSKit source project is unavailable"
  [[ "$(uname -s)" == Darwin ]] || die "the verification App requires macOS"
  [[ -x "$PLIST_BUDDY" ]] || die "PlistBuddy is unavailable"
  security find-identity -v -p codesigning | grep -Fq "\"$SIGNING_IDENTITY\"" || \
    die "the signing identity is unavailable: $SIGNING_IDENTITY"
  local host_profile module_profile
  host_profile=$(provisioning_profile "$APP_BUNDLE_ID") || die "no local provisioning profile for $APP_BUNDLE_ID"
  module_profile=$(provisioning_profile "$MODULE_BUNDLE_ID") || die "no local provisioning profile for $MODULE_BUNDLE_ID"

  mkdir -p "$REPO_ROOT/.tmp"
  local existing_root existing_module
  existing_root=$(canonical_dir "$VERIFICATION_ROOT" 2>/dev/null || true)
  if [[ -n "$existing_root" ]]; then
    existing_module="$VERIFICATION_APP/Contents/Extensions/CodexFoldFSKitModule.appex"
    ensure_no_running_fixed_module
    ensure_single_registration "$MODULE_BUNDLE_ID" "$existing_module"
  fi

  local build_root source_copy derived_data staged_app staged_root
  build_root=$(mktemp -d "$REPO_ROOT/.tmp/.codexfold-verification-build.XXXXXX")
  source_copy="$build_root/fskit"
  derived_data="$build_root/DerivedData"
  staged_root="$build_root/published"
  staged_app="$staged_root/CodexFoldVerification.app"
  cleanup_build() {
    local cleanup_status=$?
    [[ -z "${build_root:-}" ]] || rm -rf "$build_root"
    exit "$cleanup_status"
  }
  trap cleanup_build EXIT HUP INT TERM

  ditto "$SOURCE_FSKIT" "$source_copy"
  patch_copy "$source_copy"
  xcodegen --spec "$source_copy/project.yml" --project "$source_copy" --no-env --quiet
  xcodebuild \
    -project "$source_copy/CodexFoldFSKit.xcodeproj" \
    -scheme CodexFoldFSKit \
    -configuration Release \
    -destination 'platform=macOS' \
    -derivedDataPath "$derived_data" \
    CODE_SIGNING_ALLOWED=NO \
    CODE_SIGNING_REQUIRED=NO \
    CODE_SIGN_INJECT_BASE_ENTITLEMENTS=NO \
    clean build

  local built_app="$derived_data/Build/Products/Release/${APP_PRODUCT_NAME}.app"
  [[ -d "$built_app" && ! -L "$built_app" ]] || die "Xcode did not produce $built_app"
  # Xcode's RegisterWithLaunchServices build phase may register this private
  # output before it is signed and published.  Remove that exact disposable
  # path before checking the fixed identity, otherwise a stale temporary
  # registration can make the one-registration invariant fail.
  local lsreg
  lsreg=$(lsregister_path)
  "$lsreg" -u "$built_app" >/dev/null 2>&1 || true
  [[ "$(bundle_value "$built_app" CFBundleIdentifier)" == "$APP_BUNDLE_ID" ]] || die "verification App bundle identifier is wrong"
  [[ "$(bundle_value "$built_app" CFBundleDisplayName)" == "$APP_DISPLAY_NAME" ]] || die "verification App display name is wrong"
  local module="$built_app/Contents/Extensions/CodexFoldFSKitModule.appex"
  [[ -d "$module" && ! -L "$module" ]] || die "built FSKit module is missing"
  [[ "$(bundle_value "$module" CFBundleIdentifier)" == "$MODULE_BUNDLE_ID" ]] || die "verification module bundle identifier is wrong"
  [[ "$(bundle_value "$module" CFBundleDisplayName)" == "$MODULE_DISPLAY_NAME" ]] || die "verification module display name is wrong"
  [[ "$(bundle_value "$module" EXAppExtensionAttributes.FSShortName)" == "$FSKIT_SHORT_NAME" ]] || die "verification FSShortName is wrong"
  local module_exec="$module/Contents/MacOS/CodexFoldFSKitModule"
  local app_exec="$built_app/Contents/MacOS/${APP_PRODUCT_NAME}"
  [[ -x "$module_exec" && -x "$app_exec" ]] || die "built verification executables are missing"
  local app_entitlements="$build_root/app.entitlements"
  local module_entitlements="$build_root/module.entitlements"
  cp "$source_copy/CodexFoldFSKit.entitlements" "$app_entitlements"
  cp "$source_copy/Extension/CodexFoldFSKitModule.entitlements" "$module_entitlements"
  "$PLIST_BUDDY" -c "Add :com.apple.application-identifier string ${TEAM_ID}.${APP_BUNDLE_ID}" "$app_entitlements"
  "$PLIST_BUDDY" -c "Add :com.apple.developer.team-identifier string ${TEAM_ID}" "$app_entitlements"
  "$PLIST_BUDDY" -c "Add :com.apple.application-identifier string ${TEAM_ID}.${MODULE_BUNDLE_ID}" "$module_entitlements"
  "$PLIST_BUDDY" -c "Add :com.apple.developer.team-identifier string ${TEAM_ID}" "$module_entitlements"
  cp "$host_profile" "$built_app/Contents/embedded.provisionprofile"
  cp "$module_profile" "$module/Contents/embedded.provisionprofile"
  codesign --force --timestamp=none --sign "$SIGNING_IDENTITY" --entitlements "$module_entitlements" "$module"
  codesign --force --timestamp=none --sign "$SIGNING_IDENTITY" --entitlements "$source_copy/CodexFoldFSKit.entitlements" "$built_app/Contents/MacOS/CodexFoldIncidentMonitor"
  codesign --force --timestamp=none --sign "$SIGNING_IDENTITY" --entitlements "$app_entitlements" "$built_app"
  codesign --verify --deep --strict "$built_app" || die "verification App signature is invalid"
  codesign --verify --strict "$module" || die "verification module signature is invalid"
  [[ "$(codesign_value "$built_app" Identifier)" == "$APP_BUNDLE_ID" ]] || die "verification App signing identifier is wrong"
  [[ "$(codesign_value "$module" Identifier)" == "$MODULE_BUNDLE_ID" ]] || die "verification module signing identifier is wrong"

  mkdir -p "$staged_root"
  ditto "$built_app" "$staged_app"
  mkdir -p "$VERIFICATION_ROOT" "$(dirname "$VERIFICATION_APP")"
  ensure_single_registration "$MODULE_BUNDLE_ID" "$VERIFICATION_APP/Contents/Extensions/CodexFoldFSKitModule.appex"
  if [[ -d "$VERIFICATION_APP" ]]; then
    "$lsreg" -u "$VERIFICATION_APP" >/dev/null 2>&1 || true
  fi
  rm -rf "$VERIFICATION_APP"
  mv "$staged_app" "$VERIFICATION_APP"
  # Recurse so LaunchServices records the embedded FSKit appex as well as the
  # host App.  The extension record is what System Settings and pluginkit use
  # for the user approval step.
  "$lsreg" -f -R -trusted "$VERIFICATION_APP" >/dev/null 2>&1 || die "could not register the fixed verification App path"
  pluginkit -a "$VERIFICATION_APP/Contents/Extensions/CodexFoldFSKitModule.appex" || die "could not register the verification module"
  pluginkit -m -A -D -v -i "$MODULE_BUNDLE_ID" | \
    awk -F '\t' -v expected="$VERIFICATION_APP/Contents/Extensions/CodexFoldFSKitModule.appex" \
      '$NF == expected { found=1 } END { exit(found ? 0 : 1) }' || die "verification module registration is absent"
  ensure_single_registration "$MODULE_BUNDLE_ID" "$VERIFICATION_APP/Contents/Extensions/CodexFoldFSKitModule.appex"

  local app_sha module_sha app_cdhash module_cdhash team build short_version
  app_sha=$(file_sha "$VERIFICATION_APP/Contents/MacOS/${APP_PRODUCT_NAME}")
  module_sha=$(file_sha "$VERIFICATION_APP/Contents/Extensions/CodexFoldFSKitModule.appex/Contents/MacOS/CodexFoldFSKitModule")
  app_cdhash=$(codesign_value "$VERIFICATION_APP" CDHash)
  module_cdhash=$(codesign_value "$VERIFICATION_APP/Contents/Extensions/CodexFoldFSKitModule.appex" CDHash)
  team=$(codesign_value "$VERIFICATION_APP" TeamIdentifier)
  build=$(bundle_value "$VERIFICATION_APP" CFBundleVersion)
  short_version=$(bundle_value "$VERIFICATION_APP" CFBundleShortVersionString)
  local manifest_tmp="$VERIFICATION_ROOT/.manifest.tmp.$$"
  jq -n \
    --arg schema 'codexfold.verification-app.v1' \
    --arg generated_at "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    --arg app "$VERIFICATION_APP" --arg app_id "$APP_BUNDLE_ID" --arg app_name "$APP_DISPLAY_NAME" \
    --arg module "$VERIFICATION_APP/Contents/Extensions/CodexFoldFSKitModule.appex" --arg module_id "$MODULE_BUNDLE_ID" --arg module_name "$MODULE_DISPLAY_NAME" \
    --arg fs_short_name "$FSKIT_SHORT_NAME" --arg app_sha "$app_sha" --arg module_sha "$module_sha" \
    --arg app_cdhash "$app_cdhash" --arg module_cdhash "$module_cdhash" --arg team "$team" \
    --arg build "$build" --arg short_version "$short_version" \
    '{schema:$schema,generated_at:$generated_at,app:{path:$app,bundle_id:$app_id,display_name:$app_name,short_version:$short_version,bundle_version:$build,executable_sha256:$app_sha,code_directory_hash:$app_cdhash,team_identifier:$team},module:{path:$module,bundle_id:$module_id,display_name:$module_name,fs_short_name:$fs_short_name,executable_sha256:$module_sha,code_directory_hash:$module_cdhash,team_identifier:$team},registration:{single_path:true,launch_services_path:$app,system_settings_approval_required:true,sip_disable_required:false,sudo_required:false}}' \
    > "$manifest_tmp"
  chmod 600 "$manifest_tmp"
  mv "$manifest_tmp" "$VERIFICATION_MANIFEST"
  trap - EXIT HUP INT TERM
  rm -rf "$build_root"
  printf '%s\n' "$VERIFICATION_APP"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
