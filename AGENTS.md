# Contributor and agent guidance

This is a standalone Dart CLI plus an optional macOS Swift monitor.
Follow [docs/development-lifecycle.md](docs/development-lifecycle.md).

- Work only in the host-provided checkout/branch; preserve unrelated changes.
- Shipped apps live in `apps/`: the runner in `apps/heavy-lock` (code in
  `lib/heavy_lock.dart`, status publication in `lib/src/heavy_lock_status.dart`,
  entrypoints in `bin/`, integration tests in `test/`) and the macOS monitor
  in `apps/monitor-macos/`. The v2 Go prototype `tuna` is in `apps/tuna`,
  its v2 macOS monitor in `apps/tuna-monitor-macos/` and its generated
  Swift contract library in `packages/heavylock-v2-swift`;
  regenerate bindings with `tool/generate_v2.sh`, never by hand.
  `tool/dev.dart` (library and tests in `tool/`)
  builds, tests and installs them; the root `pubspec.yaml` is a pub workspace.
  Windows/Linux monitors are future targets: do not create empty projects.
- Develop inside the Nix dev shell: `nix develop` (or `nix develop -c
  <command>`). It pins Dart via `flake.lock` and sets
  `HEAVY_LOCK_MONITOR_DART`; macOS Swift comes from Xcode. Keep `flake.nix`
  and docs in sync when the SDK constraint changes. Swift package
  dependencies of `packages/heavylock-v2-swift` and `apps/tuna-monitor-macos`
  are pinned as `flake.nix`
  inputs; change them there, in `Package.swift` and in `Package.resolved`
  together, or flake evaluation fails.
- Keep the runner SDK-only. No Flutter/FVM or runtime package dependencies.
- Preserve the documented gate, `HEAVY_LOCK_*` variables, nesting marker,
  version-1 status payload and OS-lock lifetime. Coordinate any future
  protocol changes explicitly; mixed versions can create separate gates or
  block nested runs.
- Prefix Dart instance members with `this.` as in the extracted runner.
  Reuse existing admission, process-tree and status mechanisms.
- Full suites and compiles use `heavy-lock -- <command>`. If not installed,
  bootstrap with `dart apps/heavy-lock/lib/heavy_lock.dart -- <command>`
  from this checkout.
  Complete migration of all invokers first; do not mix old and current
  runners in actual workloads, including nested runs. Never run a different
  checkout's code or change machine settings to bypass contention. Analyzers,
  formatters and focused tests run directly.
- Run `dart analyze`, formatter check, focused tests and locked
  `dart tool/dev.dart check <app|all>` (build and tests; `check all` covers
  the monitor on macOS). Keep `build` free of tests and `test` free of builds.
- Tests may use temporary gates/settings only inside their isolated child
  processes; production heavy commands keep the shared defaults.
- Do not remove shared lock files, kill runners with SIGKILL, suppress failed
  tests, infer issue repositories, or claim stale status is live.
- Keep docs and consumer skills consistent with behavior. Do not change
  global personal skills as part of repository work.
- Commit only when requested. Do not push, open a PR, merge or modify another
  branch unless the user explicitly requests that action. Worklog creation
  and implementation are separate permissions. Exception: "implement/ship/
  merge/land #N" authorizes the full `ship-worklog` loop, including PR and
  merge, under the
  [lifecycle](docs/development-lifecycle.md) step 5 and its
  [shipping criteria](docs/development-lifecycle.md#shipping-criteria).
