#!/bin/bash
set -euo pipefail

if [[ $# -ne 0 ]]; then
  echo "Usage: $0" >&2
  exit 64
fi

root="$(cd "$(dirname "$0")/.." && pwd)"
build_dir="$root/build"
artifact="$build_dir/heavy-lock"
mkdir -p "$build_dir"
if [[ -d "$artifact" ]]; then
  echo "Build destination is a directory: $artifact" >&2
  exit 1
fi

temporary="$(mktemp "$build_dir/.heavy-lock.XXXXXX")"
trap 'rm -f -- "$temporary"' EXIT

cd "$root"
if ! nix develop "$root" -c dart compile exe lib/heavy_lock.dart -o "$temporary"; then
  echo "Build failed; existing executable was not replaced." >&2
  exit 1
fi
chmod 755 "$temporary"
mv -f "$temporary" "$artifact"
echo "Built: $artifact"
