#!/bin/bash
# Compatibility wrapper: builds the runner with the pinned Nix toolchain.
# Prefer `dart tool/dev.dart build heavy-lock` inside `nix develop`.
set -euo pipefail

if [[ $# -ne 0 ]]; then
  echo "Usage: $0" >&2
  exit 64
fi

root="$(cd "$(dirname "$0")/.." && pwd)"
exec nix develop "$root" -c dart "$root/tool/dev.dart" build heavy-lock
