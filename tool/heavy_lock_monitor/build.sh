#!/bin/bash
set -euo pipefail

root="$(cd "$(dirname "$0")" && pwd)"
action="${1:-build}"
if [[ $# -gt 1 || ( "$action" != build && "$action" != install && "$action" != launch ) ]]; then
  echo "Usage: $0 [build|install|launch]" >&2
  exit 64
fi

app="$root/.build/Heavy Lock Status.app"
swift build --package-path "$root" -c release
binary="$(swift build --package-path "$root" -c release --show-bin-path)/HeavyLockMonitor"
mkdir -p "$app/Contents/MacOS"
cp "$binary" "$app/Contents/MacOS/HeavyLockMonitor"
cp "$root/Info.plist" "$app/Contents/Info.plist"
codesign --force --sign - "$app"

if [[ "$action" == install ]]; then
  destination="$HOME/Applications/Heavy Lock Status.app"
  if [[ -e "$destination" ]]; then
    echo "Refusing to overwrite existing app: $destination" >&2
    exit 1
  fi
  mkdir -p "$HOME/Applications"
  cp -R "$app" "$destination"
  echo "Installed: $destination"
elif [[ "$action" == launch ]]; then
  open "$app"
else
  echo "Built: $app"
fi
