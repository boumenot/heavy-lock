---
name: receiving-code-review
description: Verify review findings against the actual implementation and tests before making scoped corrections.
---

# Receiving code review

Read the complete feedback, reproduce the claimed behavior, and check the
reviewer's assumptions against the current code and supported platforms.
Clarify material ambiguity before changing behavior. Neither agree
automatically nor dismiss a concern because current tests pass.

Fix clear bugs within scope, starting with protocol and process-lifetime
risks. Add focused regressions when behavior changes, rerun affected checks
through the appropriate lock, and explain the correction directly.

If a finding is incorrect, provide concrete code/behavior evidence. If it is
valid but outside scope, record it as a follow-up without silently expanding
the change. Avoid defensive or performative responses.

Feedback is not permission for a new dependency, destructive cleanup,
push/PR/merge, or a second gate. Respect the user's actual authorization.
