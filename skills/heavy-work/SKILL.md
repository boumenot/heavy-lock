---
name: heavy-work
description: Run builds, full or coverage test suites, integrations, benchmarks and emulators through the installed machine-wide heavy-lock command.
---

# Machine-wide heavy work

Development sessions in every repository share one machine-wide lock.
Use the installed `heavy-lock` command on each development machine.
Do not assume a specific repository, SDK path, user name or operating system.

## Heavy versus light

Wrap app/APK/release/Docker builds, full/coverage suites, integration tests,
benchmarks, emulators/simulators for their whole lifetime, and work expected
to load the machine for more than a minute.

Run analyzers, formatters, package installs, single test files and small
focused checks directly. Ordinary servers/watchers need no lock unless they
perform heavy work.

## Run

Verify `heavy-lock` is on PATH in the current agent environment. If missing,
report the installation/PATH problem; do not run heavy work unlocked or
fall back to a hard-coded checkout.

```bash
heavy-lock -- npm run build
heavy-lock -- dotnet test
heavy-lock --weight light -- pytest tests/unit
heavy-lock --worklog 42 -- bash -c 'make build && make integration-test'
```

Everything after `--` is the command. The runner inherits terminal I/O and
returns the command's exit code. Heavy weight (2 units) is default; light
(1) is for medium jobs that may share capacity. Positive numeric weights
are accepted, capped at machine capacity.

Wait until admitted, reading the printed reason. Do not kill or bypass the
runner because it waits. `--no-wait` returns 75 instead of waiting; that is
not permission to retry without the lock.

## Shared protocol and safety

- Keep the defaults: `~/.cache/heavy-lock.lock` on macOS/Linux and
  `%LOCALAPPDATA%\heavy-lock\heavy-lock.lock` on Windows.
- Before adopting this version, finish old-runner jobs and update every
  runner, monitor, skill, script and automation on the machine. Older and
  current runners use different gates and nesting markers; do not mix them,
  including nested runs. See the heavy-lock README for migration instructions.
- Do not change `HEAVY_LOCK_PATH`, `HEAVY_LOCK_CAPACITY`, resource,
  settling or priority settings ad hoc. Machine configuration is a
  deliberate user decision, identical across sessions.
- Nested commands inherit `HEAVY_LOCK_HELD` and run directly on the
  same gate. Never forge the marker or use it to launch detached heavy work.
- Never delete shared gate/slot/admission files or the shared cache.
- Stop your own run with Ctrl+C or `kill <runner-pid>`, not SIGKILL. The
  runner forwards termination and holds slots through process-tree cleanup.
- `--worklog` is an optional positive issue number in the current repo.
  Do not guess it from branch names; omit when no worklog is known.
- `heavy-lock-status` reports status records, not proof of lock ownership.
  Records can be stale; use the native macOS monitor for validated state.
- Do not put secrets in command arguments: status records include them.
- Respect repository instructions when they impose stricter requirements,
  but do not use a second gate or run heavy work unlocked.
