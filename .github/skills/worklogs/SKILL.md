---
name: worklogs
description: Use when asked to open, create, update, continue, audit or close a GitHub-issue worklog, record progress or decisions in one, summarize a session into one, or review the worklog queue; not for implementing a worklog to merge (see implementing-worklogs).
---

# Worklogs

Worklogs are persistent engineering lab notebooks for work in flight. They preserve context across sessions, record decisions and surprises, and make active work searchable through GitHub Issues.

| Artifact | Scope | Purpose | Lifetime |
|---|---|---|---|
| Agent plan | Current session | Decide how to execute now | Ephemeral |
| Worklog | Task or feature | Track context, progress, blockers, outcomes | Until work is done |
| Design doc | Project or system | Explain durable what/why decisions | Long-lived |

Use GitHub Issues in the current repository as the backend, through `gh`. `docs/development-lifecycle.md` is the canonical contract; this skill only keeps the record.

**Creating or updating a worklog never starts implementation, and never writes an authorization record.** Implementation starts only when the user explicitly asks to implement the worklog. "Continue #N" resumes implementation only when the worklog already carries a valid authorization record (defined in `implementing-worklogs`); otherwise it is record-keeping.

Use the current session model; no fixed review or writing model is required.

## Core principles

1. Keep worklogs lean. Most worklogs should be 50-100 lines.
2. Capture context before it disappears: what, why, what changed, blockers, links, next step.
3. Append progress rather than rewriting history. Read the body before editing and preserve prior notes.
4. Check for duplicate active worklogs before creating a new one.
5. Ask before creating labels or making destructive changes. Closing follows *Close a worklog*.
6. Prefer concrete verification steps over vague "works correctly" statements.
7. Keep `Phase:` and labels true. A worklog whose labels disagree with reality is worse than none.

## Labels

Use exactly one `status:*` label and at most one `size:*` label per worklog, open or closed. If a label below does not exist (`gh label list`), ask before creating it.

| Label | Meaning | Color |
|---|---|---|
| `worklog` | Identifies worklog issues | `5319e7` |
| `status:not-started` | Captured, not started | `cfd3d7` |
| `status:in-progress` | Actively being worked | `fbca04` |
| `status:blocked` | Waiting on external input | `d73a4a` |
| `status:pr-open` | PR open; in review or awaiting the user check | `0e8a16` |
| `status:completed` | Closed as done | `0e8a16` |
| `status:cancelled` | Closed without completing | `6a737d` |
| `size:XS` … `size:XL` | ≤30 min / hours / ~1 day / multi-day / split it | `ededed` |

```bash
gh label create status:pr-open --description "Worklog has an open pull request" --color 0e8a16
```

Change status by removing the old label in the same command:

```bash
gh issue edit <n> --remove-label status:in-progress --add-label status:pr-open
```

## Title

`Worklog: <short task title>`

## Issue body template

Omit sections that add nothing for a very small task, but keep `Context`, the status block, `What to Build`, `Session Log` and `Links`.

```markdown
## Context

<Why this work exists: the user's request (quote it), problem, links, constraints.>

Phase: not started
User check: <required — manual/device/UI items | not required — reason | undecided>

## What to Build

- [ ] <Deliverable>

## Non-goals

- <What this will not do.>

## Dependencies

- <Related issue, PR, decision, or "None known".>

## Closing the Loop

- [ ] <Test, analyzer, build, coverage or end-to-end check that proves it is done.>

### Manual checks

| Check | Platform | Result | Disposition |
|---|---|---|---|
| <behavior> | <macOS / Linux / Windows> | not run | <required for this scoped change / non-gating for <scope> / not applicable> |

## Session Log

### <YYYY-MM-DD HH:mm UTC>

- Started from: <prompt, issue, PR>
- Progress: <what happened>
- Decisions: <choices and why>
- Next: <single next action>

## Surprises

- <Gotchas and reusable lessons.>

## Links

- Branch: <name or "None yet">
- PR: <link and state: open / merged <commit> / none>
- Related issues: <links or "None">
```

`Phase:` values: `not started`, `intake`, `implementing`, `gating`, `blocked`, `pr-open`, `awaiting user check`, `integrating`, `done`, `cancelled`. Results and dispositions follow lifecycle step 4.

## Editing issue bodies safely

Write the body to a UTF-8 file and pass it with `--body-file`; never pipe it through a shell variable. Before pushing, check that the file still contains every prior Session Log entry. When in doubt, add a comment (`gh issue comment <n> --body-file <file>`) instead: comments never round-trip prior content. On Windows PowerShell 5.1, set `[Console]::OutputEncoding` and `$OutputEncoding` to UTF-8 first, or non-ASCII text (em dashes, quotes) is corrupted permanently.

## Workflow: create a worklog

1. Understand the task well enough to write useful context. Ask one focused question if it is ambiguous.
2. Search for duplicates: `gh issue list --state open --label worklog --search "<keywords>" --limit 20`. If one is likely, ask whether to update it instead.
3. Estimate size (`XS`–`XL`). Recommend splitting `XL`.
4. Fill the template. Decide `User check:` when it is already clear (a manual/device check automation cannot cover, or any user-visible UI change, makes it `required`); otherwise write `undecided`.
5. Create it with `status:not-started`, unless the user said work has started:

   ```bash
   gh issue create --title "Worklog: <title>" --body-file <file> --label worklog --label status:not-started --label size:<SIZE> --assignee @me
   ```

6. Report the issue number and the next step. Stop. Do not implement.

## Workflow: update a worklog

1. Identify it (number, or `gh issue list --state open --label worklog --limit 20`).
2. Read it: `gh issue view <n> --json title,body,labels,url`.
3. Append a dated Session Log entry: progress, files/PRs/commands worth remembering, decisions and why, blockers, next step. Update `Phase:`, checkboxes, Manual checks and Links in place.
4. Push with `gh issue edit <n> --body-file <file>`, and correct the status label if it changed.

## Workflow: continue a worklog

1. Read the full body and labels.
2. If it has an authorization record (an uncommented `Implementation authorized: <date> — "<words>"` line with a real date and quote; see `implementing-worklogs`) and its authorized scope is unfinished, use `implementing-worklogs` and resume from `Phase:`. That record does not grant push/PR/merge permission unless the user's words explicitly request those actions.
3. Otherwise summarize the goal, completed items, blockers and next step, and keep the record current. Do not commit, push, open PRs or merge unless the user asks.

## Workflow: review the queue

```bash
gh issue list --state open --label worklog --json number,title,labels,updatedAt,url --limit 50
```

Recommend next work: skip `blocked` unless asked; `pr-open` usually needs follow-up first; small items fill gaps; check Dependencies; recently updated in-progress work has the freshest context.

## Workflow: audit the queue (read-only)

Reports label/state drift. It never edits issues; fixing what it finds is a separate step the user approves.

```bash
repo=$(gh repo view --json nameWithOwner -q .nameWithOwner)
cutoff=$(date -u -v-14d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '14 days ago' +%Y-%m-%dT%H:%M:%SZ)
data=$(mktemp)
gh issue list -R "$repo" --label worklog --state all --limit 500 --json number,state,labels,updatedAt > "$data"
jq -r --arg cutoff "$cutoff" '.[]
  | ([.labels[].name | select(startswith("status:"))]) as $s
  | if .state == "CLOSED" and ($s != ["status:completed"] and $s != ["status:cancelled"])
      then "#\(.number) closed with status labels \($s)"
    elif .state == "OPEN" and ($s | length) != 1
      then "#\(.number) open with status labels \($s)"
    elif .state == "OPEN" and ($s[0] == "status:completed" or $s[0] == "status:cancelled")
      then "#\(.number) open but labeled \($s[0])"
    elif .state == "OPEN" and $s[0] == "status:in-progress" and .updatedAt < $cutoff
      then "#\(.number) in-progress, no update since \(.updatedAt[0:10])"
    else empty end' "$data"
for n in $(jq -r '.[] | select(.state == "OPEN") | .number' "$data"); do
  prs=$(gh api "repos/$repo/issues/$n/timeline?per_page=100" --paginate \
    -q '.[] | select(.event == "cross-referenced") | .source.issue
        | select(.pull_request) | "\(.number):\(.state):\(.pull_request.merged_at // "")"')
  merged=$(echo "$prs" | awk -F: '$3 != "" {printf "%s ", $1}')
  [ -n "$merged" ] && echo "#$n open; merged PR(s) reference it: $merged"
  if jq -e --argjson n "$n" '.[] | select(.number == $n) | .labels[].name | select(. == "status:pr-open")' "$data" >/dev/null \
     && ! echo "$prs" | grep -q ':open:'; then echo "#$n status:pr-open, but no open PR"; fi
done
rm -f "$data"
```

A merged PR that references an open worklog may be partial work; read the worklog before proposing to close it. Present the findings as a list with a proposed fix for each, and ask before changing anything.

## Workflow: close a worklog

1. Read the body. Confirm the `Closing the Loop` checks and Manual checks are complete, or record why any were skipped. Unperformed required checks need the user's explicit acceptance of the remaining risk; never invent a result.
2. Append a final Session Log entry: outcome, verification, PR and merge commit, follow-ups, lessons moved to skills or instructions. Set `Phase: done`.
3. Set exactly one terminal status: `status:completed` or `status:cancelled`.
4. Closing: close only when explicitly authorized and the agreed scope is complete, whether local-only or merged. Otherwise ask first.

   ```bash
   gh issue close <n> --comment "<short completion summary>"
   ```

5. If work remains, keep it open with the correct status label and record what remains.

## Workflow: convert a session into a worklog

Extract only meaningful work: decisions, files changed, commands, blockers, PRs and next steps; no routine narration. Create or update an open worklog if the work is active; create a completed one only if the user wants a record.

## Smell test

Trim before saving if it is longer than the implementation is likely to be, has placeholder sections, has invariants for low-risk UI/doc work, or reads like process theater instead of useful memory.
