#!/bin/bash
# Regenerates the committed Go and Swift bindings for docs/tuna.proto.
#
#   tuna --port 49783 -- nix develop -c tool/generate.sh          # regenerate
#   tuna --port 49783 -- nix develop -c tool/generate.sh --check  # verify only
#
# protoc and the Go plugins come from the Nix dev shell. The Swift plugins are
# built with Xcode's swift from packages/tuna-contract-swift, whose dependencies
# (pinned in flake.nix and checked against Package.resolved) also provide the
# Swift runtime libraries. Building them is
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
swift_package="$root/packages/tuna-contract-swift"

for tool in protoc protoc-gen-go protoc-gen-go-grpc swift; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$tool not found; run inside \`nix develop\` on macOS with Xcode." >&2
    exit 69
  fi
done

# Only the Package.resolved versions, which flake.nix verifies, are used.
swift_build=(swift build --package-path "$swift_package" -c release
  --only-use-versions-from-resolved-file)
for product in protoc-gen-swift protoc-gen-grpc-swift-2; do
  "${swift_build[@]}" --product "$product"
done
plugins="$("${swift_build[@]}" --show-bin-path)"

out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT
mkdir -p "$out/go" "$out/swift"

# The contract's go_package option names apps/tuna/gen/tunapb; outputs are
# written relative to the proto and copied into place below.
protoc -I "$root/docs" \
  --go_out="$out/go" --go_opt=paths=source_relative \
  --go-grpc_out="$out/go" --go-grpc_opt=paths=source_relative \
  --plugin=protoc-gen-swift="$plugins/protoc-gen-swift" \
  --swift_out="$out/swift" --swift_opt=Visibility=Public \
  --plugin=protoc-gen-grpc-swift-2="$plugins/protoc-gen-grpc-swift-2" \
  --grpc-swift-2_out="$out/swift" \
  --grpc-swift-2_opt=Visibility=Public,Client=true,Server=false \
  tuna.proto

targets=(
  "go/tuna.pb.go:apps/tuna/gen/tunapb/tuna.pb.go"
  "go/tuna_grpc.pb.go:apps/tuna/gen/tunapb/tuna_grpc.pb.go"
  "swift/tuna.pb.swift:packages/tuna-contract-swift/Sources/TunaContract/tuna.pb.swift"
  "swift/tuna.grpc.swift:packages/tuna-contract-swift/Sources/TunaContract/tuna.grpc.swift"
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
  echo "Bindings are out of date; run tool/generate.sh." >&2
  exit 1
fi
((check)) && echo "Bindings are up to date."
exit 0
