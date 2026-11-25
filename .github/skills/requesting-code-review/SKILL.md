---
name: requesting-code-review
description: Request a bounded review with scope, evidence and protocol/lifetime risks, without unrequested model or multi-agent gates.
---

# Requesting code review

Follow host delegation rules;
small changes can be reviewed directly and do not require a subagent.

Provide the request, intended behavior, base/head or diff, relevant tests and
their actual results, and known limitations. Ask for high-confidence bugs and
missing behavior, not unrelated redesigns.

For heavy-lock, prioritize same-gate compatibility, OS-lock lifetime,
admission/sharing, nested runs, signal cleanup, process identity, exit codes,
status error visibility and cross-repository attribution. Review docs and
skills against the commands they instruct consumers to run.

Handle clear in-scope findings, record deferred/out-of-scope ideas, rerun
affected checks after fixes, and resolve genuine behavioral disagreements
with evidence. A review does not authorize commits, pushes, PRs or merges.
No fixed review model or number of reviews is required.
