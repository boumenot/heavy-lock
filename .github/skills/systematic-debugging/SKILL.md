---
name: systematic-debugging
description: Diagnose CLI, process lifetime, admission or monitor failures from reproducible evidence before editing.
---

# Systematic debugging

1. **Investigate:** read the exact error and exit code, reproduce safely,
   inspect recent changes and trace the data/process boundary where the
   failure occurs. Confirm SDK, command arguments, cwd, gate, environment,
   process identity and permissions. Never log secrets.
2. **Compare:** find a working example in the existing tests or helpers;
   explain the meaningful difference instead of guessing.
3. **Hypothesize:** state one cause and test it with the smallest controlled
   change. Use temporary gates for isolated reproduction, not a different
   production gate or deletion of shared lock files.
4. **Fix and verify:** address the cause, add a focused regression and rerun
   affected checks. Full suites/compiles use the shared heavy lock.

Waiting is not automatically failure. Status records are best-effort and can
be stale; inspect ownership/liveness separately. POSIX snapshots race with
forks and process exit; reused PIDs need process-start identity. Linux flock
is not Dart record-lock interop.

If evidence is insufficient, gather more. After repeated failed hypotheses,
reassess the model rather than piling on workarounds. Report an actionable
blocker if the safe reproduction or required platform is unavailable.
