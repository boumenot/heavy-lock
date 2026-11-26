# Development lifecycle

This lifecycle covers the Dart CLI (`apps/heavy-lock`), the standalone
Swift monitor (`apps/monitor-macos`), the v2 Go prototype (`apps/tuna`),
the Swift v2 contract library and the `tool/` orchestrator.

## Ready, implementation and done

1. **Capture the work.** Use a GitHub worklog for substantial features,
   architectural changes or multi-session work. Small fixes may use an
   existing active issue. Record the request, scope, non-goals, observable
   acceptance criteria, affected platforms, risks and dependencies.
   Creating/updating a worklog does not start implementation.
2. **Agree the design.** Read relevant code and technical decisions. Resolve
   material behavior choices; preserve the current lock protocol by default.
   Record durable decisions with rationale and limitations, proportionate to
   risk. Document any explicitly authorized protocol migration.
3. **Implement bounded changes.** Use the host branch/worktree. Keep the
   runner SDK-only and platform adapters explicit. Tests belong at the layer
   that owns the behavior: policy/parsing unit tests, real process/admission
   integration tests, native monitor model/presentation checks.
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

- Heavy checks run through the shared gate,
  `/Users/boumenot/.local/bin/heavy-lock -- <command>`; never unlocked.
- `heavy-lock -- nix develop -c dart tool/dev.dart check <app|all>` passes
  for every affected application, with analyzer and formatter clean (see
  [Gates](#gates)).
- When `apps/tuna`, its Go dependencies or `flake.nix` change:
  `heavy-lock -- nix build .#tuna` and the race tests
  `heavy-lock -- nix develop -c go -C apps/tuna test -race ./scheduler ./storage -count=1`.
- The worklog records the checks run and not run, with their results.
- The worklog has exactly one `status:*` label at every phase.

## Gates

Run gates inside the Nix dev shell (`nix develop`, or prefix each command
with `nix develop -c`); see [flake.nix](../flake.nix). It provides the
pinned Dart toolchain and sets `HEAVY_LOCK_MONITOR_DART`. Swift comes from
the system Xcode toolchain on macOS.

```bash
dart pub get
dart analyze
dart format --output=none --set-exit-if-changed apps/heavy-lock tool
heavy-lock -- dart tool/dev.dart check all
build/heavy-lock/heavy-lock --version
build/heavy-lock/heavy-lock -- sh -c 'exit 0'
build/heavy-lock/heavy-lock-status --json
```

`check all` builds and tests every application implemented for the host
(on macOS, the runner, monitor, `tuna` and Swift v2 library); use `build`,
`test` or `check` with a target name for one application. Focused tests run
directly, e.g. `cd tool && dart test` for the orchestrator. Regenerate v2
bindings with `heavy-lock -- nix develop -c tool/generate_v2.sh`, or add
`--check` to verify them. The opt-in Swift gRPC interoperability probe in
the [README](../README.md#v2-prototype-and-contract-bindings) is an
integration test and also uses the shared lock.

If the installed runner is unavailable, replace `heavy-lock` with
`dart apps/heavy-lock/lib/heavy_lock.dart` after all invokers on the machine
have migrated; see the [migration warning](../README.md#migration). Do not
use source in another checkout or mix runner versions in actual workloads.
Until migration, wrap validation in the host's existing runner; new runners
may use temporary gates only in isolated tests. Full/coverage suites,
monitor suites and all compiles run through the machine-wide lock.
Analyzers and formatting run directly. Targeted checks can precede the full
release gates. Monitor UI changes also need a manual launch check
(`open "build/monitor-macos/Heavy Lock Status.app"`).

Check CLI exit propagation, admission/no-wait, nesting, cancellation and the
same default gate when changing runner behavior. Use temporary gates only
for isolated tests. Native integration must use the extracted runner.
Record the platforms actually exercised; macOS validation is not evidence
of Linux/Windows execution. No fixed coverage percentage or named-model
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
