# Running and Queued: approved reference

The [interactive prototype](prototype.html) and the
[running](running-selection.png) and [queued](queued-selection.png) selection
screenshots preserve the approved compact design. Open the self-contained HTML
locally. Its jobs, service version and controls are illustrative; it does not
connect to a broker.

This is a cross-platform design reference, not authorization to implement
Running or Queued changes in [#77](https://github.com/boumenot/tuna/issues/77).
Implementations should adapt it to native controls and accessibility while
retaining the broker's existing scheduling and control semantics.

- Use compact rows with start or queue-time markers. Selecting a row, slot or
  queue position updates the right-hand summary and inspector; keyboard
  selection must remain available.
- Show held/free capacity, queue order and meaningful warnings. Lower numerical
  priorities run first; queue position is not a prediction of start time.
  Preserve forced, backfill, oversized, disconnected and cancellation details.
- Show elapsed runtime and queue wait as open-ended, relative bars, not
  progress, an ETA or a predicted start. Reserved jobs without a start time
  have no fabricated runtime. Missing measurements are not zero, and sampled
  resource figures can lag.
- Provide state/attention tabs and a collapsible, initially collapsed worklog
  activity matrix. Preserve full details and recorded worklog, session and
  checkout links without guessing an issue repository.
- Preserve existing pause, resume, reorder, force-start and cancellation
  behavior and confirmations. Do not invent admissions or queue states.
  When the broker is unavailable, clearly label last-known data, freeze its
  apparent age and disable controls.

See the separate [Recently finished reference](../recently-finished/README.md)
for its completion-order window, outcome ribbon and finished-job summary.
