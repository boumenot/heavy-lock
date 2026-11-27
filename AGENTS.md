# Contributor and agent guidance

This repository is Tuna: a machine-wide heavy-work broker and runner (Go)
plus a macOS monitor (Swift).
Follow [docs/development-lifecycle.md](docs/development-lifecycle.md).

- Work only in the host-provided checkout/branch; preserve unrelated changes.
- The broker and runner `tuna` live in `apps/tuna`, its macOS monitor in
  `apps/tuna-monitor-macos/` and the generated Swift contract library in
  `packages/tuna-contract-swift`; the contract is `docs/tuna.proto`.
  Regenerate bindings with `tool/generate.sh`, never by hand.
  `tool/dev.dart` (library and tests in `tool/`) builds and tests them; the
  root `pubspec.yaml` is a pub workspace. Do not create empty projects for
  future platforms.
- Develop inside the Nix dev shell: `nix develop` (or `nix develop -c
  <command>`). It pins Go, protoc and Dart via `flake.lock`; macOS Swift
  comes from Xcode. Keep `flake.nix` and docs in sync when an SDK constraint
  changes. Swift package dependencies of `packages/tuna-contract-swift` and
  `apps/tuna-monitor-macos` are pinned as `flake.nix` inputs; change them
  there, in `Package.swift` and in `Package.resolved` together, or flake
  evaluation fails. Recompute `vendorHash` when the vendored Go package set
  changes.
- Preserve the documented contract (`tuna` identity, service version
  compatibility, REST paths, `TUNA_*` variables, `TUNA_JOB_ID` nesting,
  default port 49783 and the singleton lock). Coordinate any contract change
  explicitly; incompatible runners, brokers and monitors refuse each other.
- Prefix Dart instance members with `this.`. Reuse existing admission,
  process-tree and status mechanisms.
- Full suites and compiles use
  `/Users/boumenot/.local/bin/tuna --port 49783 -- <command>`. If the broker
  is unavailable, report the blocker; never run heavy work unlocked, run a
  different checkout's code or change machine settings to bypass contention.
  Analyzers, formatters and focused tests run directly.
- Run `dart analyze`, formatter checks, `go vet`, focused tests and brokered
  `dart tool/dev.dart check <app|all>` (build and tests; `check all` covers
  the monitor on macOS). Keep `build` free of tests and `test` free of builds.
- Tests may use temporary brokers, ports and lock directories only inside
  their isolated child processes; production heavy commands keep the shared
  defaults.
- Do not remove shared lock files or broker state, kill runners with
  SIGKILL, suppress failed tests, infer issue repositories, or claim stale
  status is live.
- Keep docs and consumer skills consistent with behavior. Do not change
  global personal skills as part of repository work.
- Commit only when requested. Do not push, open a PR, merge or modify another
  branch unless the user explicitly requests that action. Worklog creation
  and implementation are separate permissions. Exception: "implement/ship/
  merge/land #N" authorizes the full `ship-worklog` loop, including PR and
  merge, under the
  [lifecycle](docs/development-lifecycle.md) step 5 and its
  [shipping criteria](docs/development-lifecycle.md#shipping-criteria).
