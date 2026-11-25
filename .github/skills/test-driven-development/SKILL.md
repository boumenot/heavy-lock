---
name: test-driven-development
description: Add focused failing regressions before changing CLI, admission, cancellation or status behavior.
---

# Test-driven development

1. **Red:** write a focused test for one observable behavior. Run it and
   confirm it fails because the behavior is missing/broken, not a typo or
   missing SDK. Prefer existing test files.
2. **Green:** make the smallest reliable change that fixes that failure
   while preserving adjacent behavior and the shared-lock protocol.
3. **Refactor:** simplify only when useful, keeping the regression green.
   Run affected tests; use full lifecycle gates before completion.

```bash
dart test test/heavy_lock_test.dart --name 'specific behavior'
```

Focused tests run directly. Full suites and native builds go through
`heavy-lock` (or the local bootstrap runner).

Use real child processes and isolated temporary gates for admission,
nesting, exit-code and cancellation behavior. Use existing fake process
tables for deterministically exercising snapshot races. Order asynchronous
events with explicit handshakes/polling deadlines, not arbitrary long sleeps.
Clean up your test's processes and temporary paths even on failure.

Do not test constants or language behavior, mock away the lock being tested,
weaken assertions to accept a broken outcome, or change production lock
settings to speed up tests. Mechanical extraction can retain existing
regressions; new behavior requires focused additional evidence.
