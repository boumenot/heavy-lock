# Development lifecycle

This lifecycle covers the Tuna broker and runner (`apps/tuna`), its macOS
monitor (`apps/tuna-monitor-macos`), the Swift contract library
(`packages/tuna-contract-swift`) and the `tool/` orchestrator.

## Ready, implementation and done

1. **Capture the work.** Use a GitHub worklog for substantial features,
   architectural changes or multi-session work. Small fixes may use an
   existing active issue. Record the request, scope, non-goals, observable
   acceptance criteria, affected platforms, risks and dependencies.
   Creating/updating a worklog does not start implementation.
2. **Agree the design.** Read relevant code and technical decisions. Resolve
   material behavior choices; preserve the current broker contract by default.
   Record durable decisions with rationale and limitations, proportionate to
   risk. Document any explicitly authorized contract migration.
3. **Implement bounded changes.** Use the host branch/worktree. Keep
   platform adapters explicit. Tests belong at the layer that owns the
   behavior: policy/parsing unit tests, real process/admission integration
   tests, native monitor model/presentation checks.
4. **Verify and review.** Run applicable gates below. Fix introduced failures,
   not the test expectations. Record each manual check as `passed`, `failed`,
   `not run` or `not applicable`, separately from whether it is a required
   gate. UI changes need a focused macOS manual check when automation cannot
   establish the visible result. Review the diff for process lifetime,
   protocol compatibility, permission boundaries and error reporting.
5. **Integrate.** "Implement #N", "ship #N", "merge #N" or "land #N"
   (a worklog or its PR) authorizes the full `ship-worklog` loop: commit,
   push, PR, gates, fixes, rebase, merge to `master` and close the worklog.
   "Implement #N" continues through PR and merge. Use judgement on
   approvals per that skill's approval criteria; it owns the loop. The
   [shipping criteria](#shipping-criteria) below are this repository's
   additional requirements. Other requests keep their scope. Respect
   explicit local-only requests and host constraints; never switch/update
   the default branch outside the provided workspace. Use required commit
   trailers when applicable.
6. **Close the loop.** Record actual results, commit/PR links, remaining
   risks and follow-ups in the worklog. Do not call local completion
   "merged". Close only when its agreed scope is done. Required unperformed
   checks need explicit acceptance, not an invented pass.

## Shipping criteria

Repository gates and criteria for the `ship-worklog` loop, in addition to
its own safety rules:

- Heavy checks run through the shared broker,
  `/Users/boumenot/.local/bin/tuna --port 49783 -- <command>`; never unlocked.
- `/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c dart tool/dev.dart check <app|all>` passes
  for every affected application, with analyzer and formatter clean (see
  [Gates](#gates)).
- When `apps/tuna`, its Go dependencies or `flake.nix` change:
  `/Users/boumenot/.local/bin/tuna --port 49783 -- nix build .#tuna` and the race tests
  `/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c go -C apps/tuna test -race ./scheduler ./storage -count=1`.
- The worklog records the checks run and not run, with their results.
- The worklog has exactly one `status:*` label at every phase.

## Gates

Run gates inside the Nix dev shell (`nix develop`, or prefix each command
with `nix develop -c`); see [flake.nix](../flake.nix). It provides the
pinned Go, protoc and Dart toolchains. Swift comes from the system Xcode
toolchain on macOS.

```bash
dart pub get
dart analyze
dart format --output=none --set-exit-if-changed tool
gofmt -l apps/tuna
go -C apps/tuna vet ./...
/Users/boumenot/.local/bin/tuna --port 49783 -- dart tool/dev.dart check all
```

`check all` builds and tests every application implemented for the host
(on macOS, `tuna`, the Swift contract library and the monitor; the
monitor's broker integration test runs when
`build/tuna/tuna-test-broker` exists, which `check all` builds first); use `build`,
`test` or `check` with a target name for one application. The `tuna` test
target also runs the bounded descendant-discovery, finish-report and monitor
freshness models and their witnesses; TLC logs go under `build/tuna-model`.
Focused tests run directly, e.g. `cd tool && dart test` for the orchestrator.
The two host-wide operations are
`/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c dart tool/dev.dart check all`
(brokered build and test), then
`nix develop -c dart tool/dev.dart install all` (deploy the unchanged validated
artifacts, without builds/tests). Installation is not a verification gate
and requires separate deployment authorization; use temporary destinations
in tooling tests. It stages the runner at `~/.local/bin/tuna` and the monitor
at `~/Applications/Tuna Monitor.app`, refuses running apps and legacy
symlinks, and never manages launchd, login settings or state. Stop/drain and
restart steps remain explicit; see [installation](../README.md#install-and-supervision).
Record unperformed live-install/restart checks separately from focused
installer tests. An unavailable/incompatible shared broker blocks heavy
validation; bootstrap/cutover exceptions must be explicitly authorized,
never inferred from another worklog.
Regenerate contract bindings with `/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c tool/generate.sh`,
or add `--check` to verify them. The opt-in Swift gRPC interoperability probe
in the [README](../README.md#contract-bindings) is an
integration test and also uses the shared broker.

If the installed broker is unavailable, do not run heavy work unlocked and
do not run another checkout's build; report the blocker (see the
[troubleshooting](../README.md#troubleshooting) notes). Do not mix
incompatible runner and broker versions in actual workloads. Tests may use
temporary brokers, ports and lock directories only inside their isolated
child processes. Full/coverage suites, monitor suites and all compiles run
through the shared broker. Analyzers and formatting run directly. Targeted
checks can precede the full release gates. Monitor UI changes also need a
manual launch check
(`open "build/tuna-monitor-macos/Tuna Monitor.app" --args --port PORT`
against a temporary broker). Adding `--diagnostics-port N` lets
`curl http://127.0.0.1:N/diagnostics` check the monitor's presented
connection, status and Dock state; that is not evidence of rendered
pixels, so it does not replace a required visual check.

Check CLI exit propagation, admission/no-wait, nesting, cancellation and the
shared default port when changing runner behavior. Use temporary brokers
only for isolated tests. Record the platforms actually exercised. No fixed coverage percentage or named-model
review requirements are imported.

## Worklog state

Use exactly one `status:*` label and at most one `size:*`. Labels and
`Phase:` must match reality:

| Phase | Label |
|---|---|
| not started | `status:not-started` |
| intake / implementing / gating | `status:in-progress` |
| blocked | `status:blocked` |
| pr-open / awaiting user check / integrating | `status:pr-open` |
| done | `status:completed` |
| cancelled | `status:cancelled` |

PR phases apply only when a PR was requested. Local-only work can finish at
an authorized, verified commit. Preserve append-only session history.
The [worklogs skill](../.github/skills/worklogs/SKILL.md) defines the body
template; [implementing-worklogs](../.github/skills/implementing-worklogs/SKILL.md)
records the authorized scope and resumes only within it.
