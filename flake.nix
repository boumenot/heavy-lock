{
  description = "heavy-lock development shell";

  # nixpkgs-unstable provides Dart 3.13+ required by pubspec.yaml; stable
  # channels may lag behind.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  # Swift package dependencies of packages/heavylock-v2-swift, pinned here by
  # release tag; flake.lock records each tag's commit. SwiftPM still fetches
  # them, but evaluating this flake fails unless Package.resolved (and the
  # exact requirements in Package.swift) match these pins, and dev.dart and
  # tool/generate_v2.sh only build from Package.resolved. To update one,
  # change its tag here and in Package.swift, run `nix flake lock` and
  # `swift package resolve`, then commit all three files.
  inputs.grpc-swift-2 = {
    url = "github:grpc/grpc-swift-2/2.4.4";
    flake = false;
  };
  inputs.grpc-swift-protobuf = {
    url = "github:grpc/grpc-swift-protobuf/2.5.0";
    flake = false;
  };
  inputs.swift-protobuf = {
    url = "github:apple/swift-protobuf/1.41.0";
    flake = false;
  };
  # Transitive dependency of grpc-swift-2; pinned so every resolved package
  # is defined by the flake.
  inputs.swift-collections = {
    url = "github:apple/swift-collections/1.7.6";
    flake = false;
  };

  outputs =
    { self, nixpkgs, ... }:
    let
      lib = nixpkgs.lib;
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # Swift pins whose tag and commit come from the inputs above. `direct`
      # dependencies must also appear in Package.swift with `exact:`.
      swiftPackage = ./packages/heavylock-v2-swift;
      swiftDependencies = {
        grpc-swift-2 = {
          owner = "grpc";
          direct = true;
        };
        grpc-swift-protobuf = {
          owner = "grpc";
          direct = true;
        };
        swift-protobuf = {
          owner = "apple";
          direct = true;
        };
        swift-collections = {
          owner = "apple";
          direct = false;
        };
      };
      lock = lib.importJSON ./flake.lock;
      swiftPins = lib.mapAttrs (
        name: dependency:
        let
          node = lock.nodes.${lock.nodes.root.inputs.${name}};
        in
        {
          inherit (dependency) direct;
          location = "https://github.com/${dependency.owner}/${name}.git";
          version = node.original.ref;
          revision = node.locked.rev;
        }
      ) swiftDependencies;
      resolvedPins = lib.listToAttrs (
        map (pin: lib.nameValuePair pin.identity pin)
          (lib.importJSON (swiftPackage + "/Package.resolved")).pins
      );
      manifest = builtins.readFile (swiftPackage + "/Package.swift");
      swiftPinErrors =
        lib.optional (lib.attrNames resolvedPins != lib.attrNames swiftPins)
          "Package.resolved pins [${toString (lib.attrNames resolvedPins)}], flake pins [${toString (lib.attrNames swiftPins)}]"
        ++ lib.concatLists (
          lib.mapAttrsToList (
            name: pin:
            let
              resolved = resolvedPins.${name} or null;
              requirement = ''.package(url: "${pin.location}", exact: "${pin.version}")'';
            in
            lib.optional (
              resolved != null
              && (
                resolved.location != pin.location
                || resolved.state.version or null != pin.version
                || resolved.state.revision != pin.revision
              )
            ) "Package.resolved ${name} differs from flake pin ${pin.version} (${pin.revision})"
            ++ lib.optional (
              pin.direct && !lib.hasInfix requirement manifest
            ) "Package.swift lacks ${requirement}"
          ) swiftPins
        );
      checkSwiftPins =
        value:
        if swiftPinErrors == [ ] then
          value
        else
          throw ''
            packages/heavylock-v2-swift does not match the Swift pins in flake.nix:
              ${lib.concatStringsSep "\n  " swiftPinErrors}'';
    in
    {
      devShells = forAllSystems (pkgs: {
        # mkShellNoCC avoids injecting a Nix C toolchain/SDK, so the macOS
        # monitor keeps using the system Xcode `swift`.
        default = checkSwiftPins (
          pkgs.mkShellNoCC {
            packages = [
              pkgs.dart
              pkgs.go
              pkgs.protobuf
              pkgs.protoc-gen-go
              pkgs.protoc-gen-go-grpc
              pkgs.python3
              pkgs.git
              pkgs.gh
              pkgs.jq
              pkgs.tlaplus18
            ];
            shellHook = ''
              export HEAVY_LOCK_MONITOR_DART="$(command -v dart)"
            '';
          }
        );
      });

      # Builds the v2 prototype from the Go manifests; vendorHash pins the
      # module dependencies recorded in apps/tuna/go.sum. Update it whenever
      # go.sum changes.
      packages = forAllSystems (pkgs: {
        tuna = pkgs.buildGoModule {
          pname = "tuna";
          version = "0.0.0";
          src = ./apps/tuna;
          vendorHash = "sha256-VMKF82vS2W0aUR1Up3+3Su9IvkuUHO3Guj7jNamTbjA=";
          env.CGO_ENABLED = 0;
          # Tests run through `dart tool/dev.dart test tuna`.
          doCheck = false;
        };
      });

      # `nix flake check` also reports Swift pin drift.
      checks = forAllSystems (
        pkgs: checkSwiftPins { swift-pins = pkgs.runCommand "swift-pins" { } "touch $out"; }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
