# Recently finished: approved reference

The [interactive prototype](prototype.html) and [initial screenshot](approved.png)
record the design approved for [#77](https://github.com/boumenot/tuna/issues/77).
Download the HTML and open it locally; it is self-contained and uses
illustrative, not live, jobs. The screenshot shows its default collapsed
worklog matrix. The prototype's 10/25/50 choices and initial 25-job window
were examples; the macOS implementation defaults to 20 to preserve its
previous convention and supports 10, 20, 25 and 50.

The reference is platform-neutral: other monitors should preserve the
behavior and information hierarchy while using their own native controls,
typography, accessibility and window conventions. It does not redesign the
Running or Queued sections.

- **Window and order:** summarize the latest X retained *completed* jobs,
  newest by `(finished_at, job_id)`, not submission time. Use the broker's
  completion-order query and reconcile its event-sequence fence with the
  live stream; a stale, incomplete or unavailable history must not be
  presented as current.
- **Summary and ribbon:** show the outcome sequence and counts for the full
  selected window, including failures, cancellations and unknowns. Show
  mean, median, longest and measured/total coverage for root-command
  wall-clock elapsed time. Missing measurements are not zero and are not
  included in the statistics; elapsed is not CPU time.
- **Compact list:** use a finish-time line, one dense row per job, and an
  elapsed ruler scaled against the longest measured job in the full window.
  The reference uses approximately 32-pixel rows in a 1000-by-650 section;
  adapt to the platform's readable native sizing rather than copying pixels.
  Truncate long commands in the list while retaining the complete value in
  details.
- **Selection and details:** clicking a row selects it and updates the panel
  on the **right**; no per-row disclosure arrow is needed. The ribbon may also
  select a job. Keep keyboard and accessibility support, recorded outcome,
  warnings, attribution, worklog, session and checkout links, and available
  measurements in the selected-job panel. Never infer an issue repository.
- **Filters:** outcome tabs filter the list, while the ribbon, summary and
  worklog matrix continue to describe the full selected window. The matrix
  groups outcomes by worklog, can filter the list and is collapsed by
  default. Keep unknown and unattributed cases visible.
- **Outcome evidence:** exit zero is success; nonzero exit, launch failure
  and disconnected removal are failures. Cancellation before start and a
  signaled job with a recorded cancellation request appear under cancellation;
  the request is evidence of a request, not proof it caused the signal.
  Missing or unrecognized outcomes remain unknown. Preserve the underlying
  outcome and any cancellation details in the inspector.

The broker stores `finished_at` as the terminal-transition wall-clock time;
clocks can move backwards. The completion-time order is deterministic for
the recorded timestamps, not a claim about a monotonic physical clock.
Pagination is keyset-based but is not a cross-page snapshot.

The separately approved [Running and Queued reference](../running-queued/README.md)
extends the same visual language to active jobs; it is not part of #77's
production implementation scope.
