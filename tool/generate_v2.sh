#!/bin/bash
# Regenerates the committed Go and Swift bindings for docs/v2/heavylock.proto.
#
#   heavy-lock -- nix develop -c tool/generate_v2.sh          # regenerate
#   heavy-lock -- nix develop -c tool/generate_v2.sh --check  # verify only
#
# protoc and the Go plugins come from the Nix dev shell. The Swift plugins are
# built with Xcode's swift from packages/heavylock-v2-swift, whose pinned
# dependencies also provide the Swift runtime libraries. Building them is
# heavy, so run this script through the shared heavy-work gate.
set -euo pipefail

usage() {
  echo "Usage: $0 [--check]" >&2
  exit 64
}

check=0
case $# in
  0) ;;
  1) [[ $1 == --check ]] || usage; check=1 ;;
  *) usage ;;
esac

root="$(cd "$(dirname "$0")/.." && pwd)"
swift_package="$root/packages/heavylock-v2-swift"
go_import="github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"

for tool in protoc protoc-gen-go protoc-gen-go-grpc swift; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$tool not found; run inside \`nix develop\` on macOS with Xcode." >&2
    exit 69
  fi
done

for product in protoc-gen-swift protoc-gen-grpc-swift-2; do
  swift build --package-path "$swift_package" -c release --product "$product"
done
plugins="$(swift build --package-path "$swift_package" -c release --show-bin-path)"

out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT
mkdir -p "$out/go" "$out/swift"

# The M mapping places the Go package under apps/tuna without changing the
# canonical contract's go_package option.
protoc -I "$root/docs/v2" \
  --go_out="$out/go" --go_opt=paths=source_relative \
  --go_opt="Mheavylock.proto=$go_import;heavylockv2" \
  --go-grpc_out="$out/go" --go-grpc_opt=paths=source_relative \
  --go-grpc_opt="Mheavylock.proto=$go_import;heavylockv2" \
  --plugin=protoc-gen-swift="$plugins/protoc-gen-swift" \
  --swift_out="$out/swift" --swift_opt=Visibility=Public \
  --plugin=protoc-gen-grpc-swift-2="$plugins/protoc-gen-grpc-swift-2" \
  --grpc-swift-2_out="$out/swift" \
  --grpc-swift-2_opt=Visibility=Public,Client=true,Server=false \
  heavylock.proto

targets=(
  "go/heavylock.pb.go:apps/tuna/gen/heavylock/v2/heavylock.pb.go"
  "go/heavylock_grpc.pb.go:apps/tuna/gen/heavylock/v2/heavylock_grpc.pb.go"
  "swift/heavylock.pb.swift:packages/heavylock-v2-swift/Sources/HeavyLockV2/heavylock.pb.swift"
  "swift/heavylock.grpc.swift:packages/heavylock-v2-swift/Sources/HeavyLockV2/heavylock.grpc.swift"
)

stale=0
for entry in "${targets[@]}"; do
  generated="$out/${entry%%:*}"
  target="$root/${entry#*:}"
  if [[ ! -f $generated ]]; then
    echo "protoc did not produce ${entry%%:*}" >&2
    exit 1
  fi
  if ((check)); then
    if ! cmp -s "$generated" "$target"; then
      echo "Stale: ${entry#*:}" >&2
      stale=1
    fi
  else
    mkdir -p "$(dirname "$target")"
    cp "$generated" "$target"
    echo "Generated: ${entry#*:}"
  fi
done

if ((stale)); then
  echo "Bindings are out of date; run tool/generate_v2.sh." >&2
  exit 1
fi
((check)) && echo "Bindings are up to date."
exit 0
