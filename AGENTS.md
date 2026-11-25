# Contributor and agent guidance

This is a standalone Dart CLI plus an optional macOS Swift monitor.
Follow [docs/development-lifecycle.md](docs/development-lifecycle.md).

- Work only in the host-provided checkout/branch; preserve unrelated changes.
- Runner code is in `lib/heavy_lock.dart`, status publication in
  `lib/src/heavy_lock_status.dart`, entrypoints in `bin/`, and integration
  tests in `test/`. The monitor is `tool/heavy_lock_monitor/`.
- Develop inside the Nix dev shell: `nix develop` (or `nix develop -c
  <command>`). It pins Dart via `flake.lock` and sets
  `HEAVY_LOCK_MONITOR_DART`; macOS Swift comes from Xcode. Keep `flake.nix`
  and docs in sync when the SDK constraint changes.
- Keep the runner SDK-only. No Flutter/FVM or runtime package dependencies.
- Preserve the documented gate, `HEAVY_LOCK_*` variables, nesting marker,
  version-1 status payload and OS-lock lifetime. Coordinate any future
  protocol changes explicitly; mixed versions can create separate gates or
  block nested runs.
- Prefix Dart instance members with `this.` as in the extracted runner.
  Reuse existing admission, process-tree and status mechanisms.
- Full suites and compiles use `heavy-lock -- <command>`. If not installed,
  bootstrap with `dart lib/heavy_lock.dart -- <command>` from this checkout.
  Complete migration of all invokers first; do not mix old and current
  runners in actual workloads, including nested runs. Never run a different
  checkout's code or change machine settings to bypass contention. Analyzers,
  formatters and focused tests run directly.
- Run `dart analyze`, formatter check, applicable `dart test` cases and
  locked CLI compilation. For monitor changes, also run locked `swift test`
  and `bash tool/heavy_lock_monitor/build.sh build` (outside the Nix shell,
  set `HEAVY_LOCK_MONITOR_DART` to the absolute Dart executable).
- Tests may use temporary gates/settings only inside their isolated child
  processes; production heavy commands keep the shared defaults.
- Do not remove shared lock files, kill runners with SIGKILL, suppress failed
  tests, infer issue repositories, or claim stale status is live.
- Keep docs and consumer skills consistent with behavior. Do not change
  global personal skills as part of repository work.
- Commit only when requested. Do not push, open a PR, merge or modify another
  branch unless the user explicitly requests that action. Worklog creation
  and implementation are separate permissions.
