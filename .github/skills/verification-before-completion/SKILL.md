---
name: verification-before-completion
description: Verify fresh evidence before claiming a fix, passing gates, a successful build or completed work.
---

# Verification before completion

1. Identify what proves the claim: reproduce the original behavior, run a
   focused regression, inspect the required output, or build the executable.
2. Run the complete applicable command and read its output and exit code.
   Use the shared heavy lock for full suites and compiles.
3. Confirm the result actually covers the requirement. Analyzer success is
   not a build, a status record is not live lock ownership, and macOS
   execution is not Linux/Windows validation.
4. Fix failures, rerun affected checks, and inspect the diff for unrelated
   changes and missing docs.
5. Report what was verified and what remains uncertain. Never replace an
   unrun or failed required check with a success claim.

Use `docs/development-lifecycle.md` for exact CLI and monitor gates.
New regression tests should fail without the fix for the intended reason.
Do not trust a delegated success report without its evidence, silently
weaken assertions, or infer completion from an idle agent.
