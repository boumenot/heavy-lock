---
name: test-driven-development
description: Add focused failing regressions before changing CLI, admission, cancellation or status behavior.
---

# Test-driven development

1. **Red:** write a focused test for one observable behavior. Run it and
   confirm it fails because the behavior is missing/broken, not a typo or
   missing SDK. Prefer existing test files.
2. **Green:** make the smallest reliable change that fixes that failure
   while preserving adjacent behavior and the broker contract.
3. **Refactor:** simplify only when useful, keeping the regression green.
   Run affected tests; use full lifecycle gates before completion.

```bash
nix develop -c go -C apps/tuna test ./scheduler -run '^TestSpecificBehavior$' -count=1
```

Focused tests run directly. Full suites and native builds go through the
shared broker (`/Users/boumenot/.local/bin/tuna --port 49783 -- ...`).

Use real child processes and isolated temporary brokers for admission,
nesting, exit-code and cancellation behavior. Use existing fake process
tables for deterministically exercising snapshot races. Order asynchronous
events with explicit handshakes/polling deadlines, not arbitrary long sleeps.
Clean up your test's processes and temporary paths even on failure.

Do not test constants or language behavior, mock away the admission being
tested, weaken assertions to accept a broken outcome, or change production
broker settings to speed up tests. Mechanical extraction can retain existing
regressions; new behavior requires focused additional evidence.
