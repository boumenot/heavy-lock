# SSE replay model

`SSEReplay.tla` models the `/events` adapter's incarnation-fenced cursor and
the underlying `WatchEvents` snapshot, replay, tail, overflow and disconnect
transitions. One client and one broker are bounded to three event sequences
and one restart. It starts at sequence one; the zero-sequence snapshot
sentinel is covered by transport tests instead. A restart can replace the
database and reuse a sequence.
Each reconnect either starts with a snapshot or resumes only if the
incarnation matches and its cursor is inside the retained window. An
overflow, replay gap, or broker loss ends the stream. The model tracks
delivery separately from replay send, so reconnect uses the last delivered
ID rather than an in-flight one.

`StartSnapshot`/`SendSnapshot` abstract the subscribed atomic snapshot in
`broker.Control.WatchEvents`; snapshot send and delivery are a single step.
`StartResume` maps to the incarnation and cursor checks in
`transport.serveEvents` and `storage.Reader.Events`. `SendNext`, `Deliver`
and `FinishReplay` abstract replay, flush, and live transition;
`Overflow`/`Fail` correspond to the bounded `broker.Hub` and the terminal
`stream_error`. `Disconnect`, `Crash`, and `Restart` include lost in-flight
frames, a lost error frame and replacement storage. `Prune` represents
retention or deletion. A heartbeat is a stuttering transition; HTTP buffers,
real timing, JSON, random collisions, and the client's
processing/acknowledgment distinction are not modeled. Clients are responsible
for storing IDs only after processing. The model does not assert immediate
failure detection or liveness under permanent faults.

`TypeOK`, `EpochFenced` and `CursorDelivered` check types, require a snapshot
in the current incarnation before a caught-up stream, and prevent an
undelivered frame from advancing the client's cursor. `SSEReplayFair`
starts from a synchronized but disconnected client and checks eventual
synchronization with the final head under weak fairness of snapshot, replay,
delivery and failure handling. It assumes a stable broker and network,
no further disconnects, overflows or pruning, and a finite number of changes.
In the 2026-11-28 bounded macOS run, `SSEReplaySafety` explored 6,696
generated and 1,757 distinct states; `SSEReplayFair` explored 56 generated
and 33 distinct states. With coverage enabled, all safety actions were
exercised. The deliberate `BugIgnoreEpoch` mutation violated
`EpochFenced` after reconnecting to a replacement database with an old ID;
`SSEReplayReached` reached a real resume. These are bounded design checks,
not proofs of the Go implementation.

The mutation trace snapshots sequence 1, disconnects, crashes, and starts a
replacement database also at sequence 1. Without the incarnation check,
the old ID is accepted and replay finishes with no snapshot of the new
database. The curl integration regression checks the real adapter rejects
an ID from a different listener and accepts a fresh snapshot request.

Run the cases with:

```bash
nix develop -c env JAVA_TOOL_OPTIONS=-Xmx256m python3 docs/model/check.py \
  SSEReplaySafety SSEReplayFair SSEReplayOldEpoch SSEReplayReached --timeout 30 --logs build/tuna-model
```

They also run with `dart tool/dev.dart test tuna` through the shared broker.
