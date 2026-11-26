{
  description = "heavy-lock development shell";

  # nixpkgs-unstable provides Dart 3.13+ required by pubspec.yaml; stable
  # channels may lag behind.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (pkgs: {
        # mkShellNoCC avoids injecting a Nix C toolchain/SDK, so the macOS
        # monitor keeps using the system Xcode `swift`.
        default = pkgs.mkShellNoCC {
          packages = [
            pkgs.dart
            pkgs.python3
            pkgs.git
            pkgs.gh
            pkgs.jq
            pkgs.tlaplus18
          ];
          shellHook = ''
            export HEAVY_LOCK_MONITOR_DART="$(command -v dart)"
          '';
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
