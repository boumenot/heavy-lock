# Development lifecycle

This lifecycle covers the Dart CLI and standalone Swift monitor.

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
5. **Integrate with permission.** Commit, push, PR and merge are distinct
   actions requiring the user's request. Implementing a worklog does not
   silently authorize a push or merge. Respect explicit local-only requests
   and host constraints; never switch/update the default branch outside
   the provided workspace. Use required commit trailers when applicable.
6. **Close the loop.** Record actual results, commit/PR links, remaining
   risks and follow-ups in the worklog. Do not call local completion
   "merged". Close only when its agreed scope is done and closing is
   authorized. Required unperformed checks need explicit acceptance, not an
   invented pass.

## Gates

Run gates inside the Nix dev shell (`nix develop`, or prefix each command
with `nix develop -c`); see [flake.nix](../flake.nix). It provides the
pinned Dart toolchain and sets `HEAVY_LOCK_MONITOR_DART`. Swift comes from
the system Xcode toolchain on macOS.

```bash
dart pub get
dart analyze
dart format --output=none --set-exit-if-changed bin lib test
heavy-lock -- dart test
heavy-lock -- bash -c \
  'mkdir -p build &&
   dart compile exe bin/heavy_lock.dart -o build/heavy-lock &&
   dart compile exe bin/heavy_lock_status.dart -o build/heavy-lock-status'
build/heavy-lock --help
build/heavy-lock -- sh -c 'exit 0'
build/heavy-lock-status --json
```

If the installed runner is unavailable, replace `heavy-lock` with
`dart lib/heavy_lock.dart` after all invokers on the machine have migrated;
see the [migration warning](../README.md#migration). Do not use source in
another checkout or mix runner versions in actual workloads. Until migration,
wrap validation in the host's existing runner; new runners may use temporary
gates only in isolated tests. Full/coverage suites, monitor suites and all
compiles run through the machine-wide lock. Focused tests, analyzers and
formatting run directly. Targeted checks can precede the full release gates.

For native monitor changes, on macOS:

```bash
heavy-lock -- bash -c \
  'swift test --package-path tool/heavy_lock_monitor &&
   bash tool/heavy_lock_monitor/build.sh build'
```

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
