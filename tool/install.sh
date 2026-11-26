#!/bin/bash
set -euo pipefail

if [[ $# -gt 1 || ( $# -eq 1 && -z "$1" ) ]]; then
  echo "Usage: $0 [destination-directory]" >&2
  exit 64
fi

root="$(cd "$(dirname "$0")/.." && pwd)"
artifact="$root/build/heavy-lock/heavy-lock"
if [[ ! -s "$artifact" || ! -f "$artifact" ]]; then
  echo "Built executable not found: $artifact" >&2
  echo "Run 'dart tool/dev.dart build heavy-lock' first." >&2
  exit 1
fi

destination="${1:-${HOME:?HOME must be set}/.local/bin}"
mkdir -p "$destination"
target="$destination/heavy-lock"
if [[ -d "$target" ]]; then
  echo "Install destination is a directory: $target" >&2
  exit 1
fi

temporary="$(mktemp "$destination/.heavy-lock.XXXXXX")"
trap 'rm -f -- "$temporary"' EXIT
if ! install -m 755 "$artifact" "$temporary"; then
  echo "Installation failed; existing executable was not replaced." >&2
  exit 1
fi
mv -f "$temporary" "$target"
echo "Installed: $target"
