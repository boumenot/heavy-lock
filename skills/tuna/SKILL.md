---
name: tuna
description: Run heavy builds, full or coverage test suites, integration tests, benchmarks, and emulators through the shared tuna broker so parallel sessions do not overload this Mac.
---

# Heavy work on this Mac

Development sessions in this account share one Tuna broker. Run heavy work
through its installed runner so concurrent jobs are admitted together.

**Cutover freeze:** Until the coordinated cutover is cleared and this
repository's heavy-work instructions and invokers use this broker or are
safely disabled, do not run real heavy workloads under any gate. Report the
migration blocker. Installing this skill alone does not clear the freeze.

## What counts as heavy

Run through the broker:

- App, APK, release, or Docker image builds
- Full or coverage test suites, integration tests, and benchmarks
- Emulators and simulators for their whole lifetime
- Anything else expected to load the machine for more than a minute

Run linters, formatters, type checks, analyzers, package installs, single test
files, small focused tests, and quick scripts directly. Ordinary servers and
watchers do not need the gate unless they perform heavy work.

## How to run

Use the installed runner by absolute path and specify the shared broker port:

```bash
[ -x /Users/boumenot/.local/bin/tuna ] || {
  echo "tuna runner missing: /Users/boumenot/.local/bin/tuna" >&2
  exit 1
}

/Users/boumenot/.local/bin/tuna --port 49783 -- npm run build
/Users/boumenot/.local/bin/tuna --port 49783 -- dotnet test
/Users/boumenot/.local/bin/tuna --port 49783 --weight light -- pytest tests/unit
```

Run from the project directory. Put the command and its arguments after `--`.
The runner does not invoke a shell; use `bash -c '...'` for pipelines or
multiple commands. It returns the command's exit status.

Heavy weight is the default (2 units); `--weight light` uses 1 unit for medium
jobs that may share capacity. A positive integer weight is also accepted, but
the broker rejects weights above its capacity. The runner waits for admission;
do not bypass it because it is waiting. `--no-wait` returns 75 if the broker
does not admit the job immediately.

The broker must already be running and ready. To check it:

```bash
/Users/boumenot/.local/bin/tuna status --port 49783
```

If the runner is missing or the broker is unavailable, incompatible, or
recovering, report the problem rather than running heavy work unlocked. The
runner reports an unavailable broker with exit code 69; a command may itself
return 69 or 75, so inspect the diagnostics.

## Safety

- Never use another gate or a repository-local runner unless it delegates to
  this installed tuna runner and shared broker. Different gates do not exclude
  one another. Report conflicting repository instructions instead of
  bypassing the shared broker.
- Do not change broker port, capacity, or scheduling settings ad hoc, launch a
  second broker, or delete broker state or lock files.
- The runner sets `TUNA_JOB_ID` in the admitted command. Nested tuna calls
  with that marker run directly under the existing admission. Never forge the
  marker or use it for unrelated or detached work.
- Use `--worklog N` only when the worklog number is known; do not infer it from
  a branch name.
- Do not put secrets in command arguments: the broker records job arguments
  and its local control API is unauthenticated.
- Repository instructions may add stricter requirements but must not introduce
  a second gate or run heavy work unlocked.
