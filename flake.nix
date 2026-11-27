{
  description = "Tuna development shell";

  # nixpkgs-unstable provides Dart 3.13+ required by pubspec.yaml; stable
  # channels may lag behind.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  # Swift package dependencies of packages/tuna-contract-swift and
  # apps/tuna-monitor-macos, pinned here by release tag; flake.lock records
  # each tag's commit. SwiftPM still fetches them, but evaluating this flake
  # fails unless each package's Package.resolved (and the exact requirements
  # in its Package.swift) match these pins, and dev.dart and
  # tool/generate.sh only build from Package.resolved. To update one,
  # change its tag here and in both Package.swift files, run `nix flake lock`
  # and `swift package resolve` in both packages, then commit all the files.
  inputs.grpc-swift-2 = {
    url = "github:grpc/grpc-swift-2/2.4.4";
    flake = false;
  };
  inputs.grpc-swift-protobuf = {
    url = "github:grpc/grpc-swift-protobuf/2.5.0";
    flake = false;
  };
  inputs.grpc-swift-nio-transport = {
    url = "github:grpc/grpc-swift-nio-transport/2.13.3";
    flake = false;
  };
  inputs.swift-protobuf = {
    url = "github:apple/swift-protobuf/1.41.0";
    flake = false;
  };
  # Transitive SwiftPM dependencies are pinned so every resolved package is
  # also defined by the flake.
  inputs.swift-algorithms = {
    url = "github:apple/swift-algorithms/1.2.1";
    flake = false;
  };
  inputs.swift-asn1 = {
    url = "github:apple/swift-asn1/1.7.5";
    flake = false;
  };
  inputs.swift-async-algorithms = {
    url = "github:apple/swift-async-algorithms/1.1.7";
    flake = false;
  };
  inputs.swift-atomics = {
    url = "github:apple/swift-atomics/1.3.1";
    flake = false;
  };
  inputs.swift-certificates = {
    url = "github:apple/swift-certificates/1.21.2";
    flake = false;
  };
  inputs.swift-collections = {
    url = "github:apple/swift-collections/1.7.6";
    flake = false;
  };
  inputs.swift-crypto = {
    url = "github:apple/swift-crypto/5.2.0";
    flake = false;
  };
  inputs.swift-http-structured-headers = {
    url = "github:apple/swift-http-structured-headers/1.7.0";
    flake = false;
  };
  inputs.swift-http-types = {
    url = "github:apple/swift-http-types/1.8.0";
    flake = false;
  };
  inputs.swift-log = {
    url = "github:apple/swift-log/1.17.2";
    flake = false;
  };
  inputs.swift-nio = {
    url = "github:apple/swift-nio/2.105.2";
    flake = false;
  };
  inputs.swift-nio-extras = {
    url = "github:apple/swift-nio-extras/1.35.3";
    flake = false;
  };
  inputs.swift-nio-http2 = {
    url = "github:apple/swift-nio-http2/1.46.1";
    flake = false;
  };
  inputs.swift-nio-ssl = {
    url = "github:apple/swift-nio-ssl/2.37.7";
    flake = false;
  };
  inputs.swift-nio-transport-services = {
    url = "github:apple/swift-nio-transport-services/1.28.0";
    flake = false;
  };
  inputs.swift-numerics = {
    url = "github:apple/swift-numerics/1.1.1";
    flake = false;
  };
  inputs.swift-service-lifecycle = {
    url = "github:swift-server/swift-service-lifecycle/2.13.0";
    flake = false;
  };
  inputs.swift-system = {
    url = "github:apple/swift-system/1.9.0";
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

      # Swift pins whose tag and commit come from the inputs above. Every
      # package below resolves exactly these pins; its `direct` dependencies
      # must also appear in its Package.swift with `exact:`.
      grpcDirect = [
        "grpc-swift-2"
        "grpc-swift-protobuf"
        "grpc-swift-nio-transport"
      ];
      swiftPackages = {
        "packages/tuna-contract-swift" = {
          src = ./packages/tuna-contract-swift;
          direct = grpcDirect ++ [ "swift-protobuf" ];
        };
        "apps/tuna-monitor-macos" = {
          src = ./apps/tuna-monitor-macos;
          direct = grpcDirect;
        };
      };
      swiftDependencies = {
        grpc-swift-2.owner = "grpc";
        grpc-swift-protobuf.owner = "grpc";
        grpc-swift-nio-transport.owner = "grpc";
        swift-protobuf.owner = "apple";
        swift-service-lifecycle.owner = "swift-server";
      }
      //
        lib.genAttrs
          [
            "swift-algorithms"
            "swift-asn1"
            "swift-async-algorithms"
            "swift-atomics"
            "swift-certificates"
            "swift-collections"
            "swift-crypto"
            "swift-http-structured-headers"
            "swift-http-types"
            "swift-log"
            "swift-nio"
            "swift-nio-extras"
            "swift-nio-http2"
            "swift-nio-ssl"
            "swift-nio-transport-services"
            "swift-numerics"
            "swift-system"
          ]
          (_: {
            owner = "apple";
          });
      lock = lib.importJSON ./flake.lock;
      swiftPins = lib.mapAttrs (
        name: dependency:
        let
          node = lock.nodes.${lock.nodes.root.inputs.${name}};
        in
        {
          location = "https://github.com/${dependency.owner}/${name}.git";
          version = node.original.ref;
          revision = node.locked.rev;
        }
      ) swiftDependencies;
      swiftPackageErrors =
        path: package:
        let
          resolvedPins = lib.listToAttrs (
            map (pin: lib.nameValuePair pin.identity pin)
              (lib.importJSON (package.src + "/Package.resolved")).pins
          );
          manifest = builtins.readFile (package.src + "/Package.swift");
        in
        map (error: "${path}: ${error}") (
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
                lib.elem name package.direct && !lib.hasInfix requirement manifest
              ) "Package.swift lacks ${requirement}"
            ) swiftPins
          )
        );
      swiftPinErrors = lib.concatLists (lib.mapAttrsToList swiftPackageErrors swiftPackages);
      checkSwiftPins =
        value:
        if swiftPinErrors == [ ] then
          value
        else
          throw ''
            Swift packages do not match the Swift pins in flake.nix:
              ${lib.concatStringsSep "\n  " swiftPinErrors}'';
      # The runtime SQLite driver is defined here; Go's manifests carry the
      # same pin for ordinary Go tooling and buildGoModule vendoring.
      sqliteVersion = "v1.63.0";
      checkGoPins =
        value:
        if lib.hasInfix "modernc.org/sqlite ${sqliteVersion}" (builtins.readFile ./apps/tuna/go.mod) then
          value
        else
          throw "apps/tuna/go.mod must match modernc.org/sqlite ${sqliteVersion} in flake.nix";
    in
    {
      devShells = forAllSystems (pkgs: {
        # mkShellNoCC avoids injecting a Nix C toolchain/SDK, so the macOS
        # monitor keeps using the system Xcode `swift`.
        default = checkGoPins (
          checkSwiftPins (
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
            }
          )
        );
      });

      # Builds tuna from the Go manifests; vendorHash pins the
      # module dependencies used by apps/tuna. Update it when the vendored
      # package set changes, even if go.sum does not.
      packages = forAllSystems (pkgs: {
        tuna = checkGoPins (
          pkgs.buildGoModule {
            pname = "tuna";
            version = "0.0.0";
            src = ./apps/tuna;
            vendorHash = "sha256-pqR/LhyPj9V2QKtFOTC+p4TQsjaKcU5Ls1NZj4v7mjg=";
            env.CGO_ENABLED = 0;
            # Tests run through `dart tool/dev.dart test tuna`.
            doCheck = false;
          }
        );
      });

      # `nix flake check` also reports Swift pin drift.
      checks = forAllSystems (
        pkgs:
        checkGoPins (checkSwiftPins {
          swift-pins = pkgs.runCommand "swift-pins" { } "touch $out";
        })
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
