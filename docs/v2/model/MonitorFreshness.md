# Broker-to-monitor freshness model

`MonitorFreshness.tla` is a finite model of one v2 broker and one monitor,
not a proof of either implementation. It separates the broker's actual state
from the monitor's last observations: a failure, commit or restart can occur
before a delayed message or a disconnect reaches the monitor. "Live" therefore
does not mean that the monitor instantaneously knows the broker's latest
state or that a remote failure has not just occurred.

## Mapping and assumptions

| Model | Implementation |
|---|---|
| `broker.state`, `epoch`, `mode`, `seq`, `floor`, `log` | [Health and WatchEvents](../../../apps/tuna/broker/control.go), [durable events and retention](../../../apps/tuna/storage/events.go), and [Start/MarkReady](../../../apps/tuna/scheduler/scheduler.go). The epoch represents a process incarnation; sequence, retained data and mode survive a restart. |
| `PublishStatus`, `Prune`, `Crash`, `StorageFailure`, `Restart`, `CompleteRecovery` | Committed status changes, retention, stopped/failed storage, supervised restart and recovery. Storage failure is sticky until restart; a running but recovering broker can answer Health and stream. |
| `ObserveHealth`, `BeginWatch`, `SendSnapshot`, `SendReplay`, `FinishReplay`, `SendTail`, `SendHeartbeat` | [GetHealth/WatchEvents](../../../apps/tuna/broker/control.go), [atomic snapshot](../../../apps/tuna/scheduler/project.go), [subscription](../../../apps/tuna/broker/hub.go), and [MonitorSession.run/watch](../../../apps/tuna-monitor-macos/Sources/TunaMonitorCore/MonitorSession.swift). A successful compatible Health is required for every normal watch attempt, even a replay. |
| `Receive`, `ReplayExpired`, `Disconnect`, `DetectServerLoss`, `Tick`, `Timeout` | [MonitorSession.receive/run/watch](../../../apps/tuna-monitor-macos/Sources/TunaMonitorCore/MonitorSession.swift) and [MonitorState.apply/requireSnapshot](../../../apps/tuna-monitor-macos/Sources/TunaMonitorCore/MonitorState.swift). A gap or unrecognized event resets the cursor and snapshot flag, retains cached data, marks it unavailable, and retries with a snapshot. An unrecognized event waits the normal reconnect delay; that delay is abstracted away. |
| `monitor.mode`, `PresentedMode` | Last status's queue mode and the *current* diagnostics mode in [Diagnostics.swift](../../../apps/tuna-monitor-macos/Sources/TunaMonitorCore/Diagnostics.swift). Non-live means current mode unknown, even when a last-known value is retained. The separate queue **state** is error on `brokerError`, not unknown. |

`monitor.seen`, `monitor.loss`, `stream.healthEpoch` and the fault counters
are model bookkeeping, not wire fields. `seen` records the last delivered
sequence even when Swift does not apply it. `loss` means the monitor has
actually handled a failure, timeout or gap; it is not set at the instant
the broker fails. A new accepted message clears it as in the current session.
The model does not treat replay by itself as health: the positive profiles
require Health on the current watch attempt, followed by a snapshot or
sequence-checked message. Health with `ready = false` is compatible and may
lead to Live while recovery is displayed.

The modeled event log starts at sequence 1, with at most one additional
committed event in the checked profiles. Status changes can carry enabled,
paused, unspecified or unrecognized modes; the latter two overapproximate
what a compatible future server could send. A single in-flight message
models delay and preserves send order. `DropMessage` also allows a silent
loss, a conservative fault beyond normal ordered, reliable gRPC delivery;
the monitor must detect a subsequent sequence gap or timeout. Pruning moves
the replay floor to the current head; the log is retained internally to
abstract an already-subscribed live feed. A reconnect below the floor
requires a snapshot. Ordinary message duplication and arbitrary reordering
are not modeled; Swift's defensive duplicate handling has a focused test.

`Tick` is a bounded abstraction of elapsed time, not seconds. A timeout is
possible only after the configured abstract ticks, and every received
message resets the clock. The real watchdog uses the greater of 15 seconds
and three advertised heartbeat intervals by default. No hard detection
latency follows from this model. The real server can have already-buffered
messages when storage fails, and the model allows one such message to be
delivered before failure is detected.

`CompleteRecovery` abstracts the readiness transition; it does not force the
two separate status and queue events that `MarkReady` commits. Similarly,
`PublishStatus` abstracts a mode change as one event, rather than the
status/queue pair committed by `SetQueueMode`. Same-mode status events stand
in for changes that do not alter the modeled mode. Queue contents, controls,
job scheduling, transport internals, incompatible services, snapshot history
loading, real clock scheduling and rendering are out of scope. In particular,
the model treats snapshot application and the subsequent Live transition as
one step; Swift awaits a recent-history request between them. The modeled
incarnation is not a protocol field or a checked Swift fence.

## Properties and bounds

The positive configurations check:

- `TypeOK`: finite, well-typed broker, monitor and stream records.
- `NoDetectedLossLive`: a handled disconnect, timeout, gap or storage
  failure is not presented Live until a later accepted stream message.
- `LiveHasAttemptHealth`, `LiveHasSnapshot` and
  `LiveReflectsDeliveredSeq`: Live has a successful compatible Health on the
  watch attempt, a snapshot, and has reflected the last delivered sequence.
- `CachedStatusAtSequence`: retained mode corresponds to the modeled status
  at the last applied sequence, rather than necessarily the broker's head.
- `NonLiveModeUnknown` and `EnabledOnlyWhenKnown`: stale, unspecified and
  unrecognized modes are not exposed as a current enabled queue.

`MonitorFair` additionally checks `EventuallyStableLive` and
`EventuallySynchronized`: after finitely many mode changes, crashes, storage
failures, disconnects, losses and prunes, the monitor eventually stays Live
and reaches the final broker sequence. This requires weak fairness for
restart, recovery, health, watch, send, receive and failure detection, plus
strong fairness for tail delivery when heartbeats compete for the buffer.
Its timeouts are disabled, representing a communication period that
eventually delivers before the watchdog fires. It does **not** establish
recovery during a permanent partition, repeated timeouts, an infinite stream
of changes or a permanently failed broker.

The checked TLC 2.18 runs used one worker, a 256 MiB JVM limit, and at most
45 seconds per configuration on macOS 26.7.3/arm64 on 2026-11-27 UTC.
`MaxSeq = 2` means one initial and at most one later event. Each listed fault
bound is one; omitted fault kinds have bound zero.

| Configuration | Faults / scenario | Generated / distinct states | Result |
|---|---|---:|---|
| `MonitorDelivery` | disconnect, timeout, normal and resumed delivery | 2,865 / 1,154 | safety pass |
| `MonitorLoss` | one lost message and timeout | 2,897 / 1,260 | safety pass |
| `MonitorRestart` | one crash and restart/recovery | 13,694 / 4,968 | safety pass |
| `MonitorStorage` | one storage failure and restart/recovery | 15,721 / 5,793 | safety pass |
| `MonitorGap` | one disconnect and prune | 9,667 / 3,652 | safety pass |
| `MonitorUnknownSafe` | unknown event, disconnect and timeout | 5,729 / 2,354 | safety pass |
| `MonitorEmptySafe` | empty first event and timeout | 99 / 52 | safety pass |
| `MonitorFair` | one each of crash, storage failure, disconnect, loss and prune; no timeouts | 594,083 / 207,550 | safety and fair liveness pass |

Coverage from these complete searches exercised snapshot, replay, tail,
heartbeat, receive, timeout, loss, pruning, expiry, failure, crash, restart
and recovery actions across the profiles. Actions disabled by a profile's
zero bound are intentionally absent in that profile. Unknown events and
empty first envelopes have separate positive and mutation profiles. Deadlock
detection is enabled: the positive profiles completed
without deadlocks, while witness searches stop at their expected violations
and do not establish deadlock absence.

## Witnesses and implementation findings

The wrapper expects the named invariant violation for each witness. Exit 12
is evidence of the intended counterexample, not a passing invariant.

| Configuration | Expected violation and purpose |
|---|---|
| `MonitorStaleLive` | `NoDetectedLossLive`: mutation keeps Live after detected disconnect. |
| `MonitorStaleMode` | `NonLiveModeUnknown`: mutation exposes a retained enabled mode after disconnect. |
| `MonitorUnknownMode` | `EnabledOnlyWhenKnown`: mutation maps an unrecognized mode to enabled. |
| `MonitorBlindReplay` | `LiveHasAttemptHealth`: mutation resumes from replay without a new Health observation. |
| `MonitorReplayReached`, `MonitorGapReached` | Deliberately false reachability claims produce a resumed replay and a fresh-snapshot request after a disconnect and replay expiry. |
| `MonitorStorageReached`, `MonitorRecoveryReached` | Deliberately false reachability claims produce observed storage failure and Live during recovery. |
| `MonitorLagWitness` | `LiveAtBrokerHead` is not invariant: a commit can occur while a snapshot is in flight. |
| `MonitorEpochWitness` | `LiveHealthSameEpoch` is not invariant: a restart can occur between Health and the watch. |
| `MonitorUnknownEvent`, `MonitorEmptyFirst` | `LiveReflectsDeliveredSeq` and `LiveHasSnapshot`: the `BugAcceptUnknown` mutation restores the original behavior for unrecognized and empty events. |

For `MonitorUnknownEvent`, a snapshot is applied, the broker emits a
sequenced event with an unknown oneof case, and the monitor receives it.
Swift decodes that case as `event == nil`. Previously, `MonitorState.apply`
returned `.applied` without advancing the cursor, and `MonitorSession.receive`
could mark the session Live. The next heartbeat at the higher sequence detected
a gap and requested a snapshot, but until then the cached mode could be
presented as current. An empty first envelope similarly produced Live without
a snapshot. The current broker emits neither kind:
the first case represents a possible future event case, the second a
malformed stream. Compatibility permits newer stable minor versions, but
pre-release versions must match exactly. The monitor now rejects either
envelope, immediately marks cached data unavailable, forgets the cursor and
snapshot flag, then retries after the reconnect delay. Live resumes only after
a fresh snapshot. Repeated unrecognized envelopes can prevent recovery; a
snapshot can only refresh fields understood by the current monitor. Focused
session tests cover the empty-first, resumed-replay and already-live paths,
including recovery and stale presentation.

Existing [diagnostics tests](../../../apps/tuna-monitor-macos/Tests/TunaMonitorCoreTests/DiagnosticsTests.swift)
also exercise unspecified and unrecognized live modes and distinguish
non-live current mode from the retained last-known mode. The
[session tests](../../../apps/tuna-monitor-macos/Tests/TunaMonitorCoreTests/TunaMonitorCoreTests.swift)
cover replay expiry, heartbeat timeout, recovery and retained stale data.

The epoch witness is a time-of-check/time-of-use limitation, not evidence
that a particular restart has occurred: Health can come from the old broker
and the new broker can serve the snapshot. The session does not correlate
`Health.startedAt` with an event-stream incarnation. The real snapshot also
contains recovery status, which the banner can warn about; this model does
not assert that the rendered banner falsely says ready. Likewise, the lag
witness is expected for asynchronous streams, not proof of a defect.

## Reproduction

The repository's Nix shell provides TLC. Run the full bounded batch through
the shared gate; `--timeout` fails explicitly if any case exceeds its
individual budget and leaves its log for diagnosis:

```bash
/Users/boumenot/.local/bin/heavy-lock -- nix develop -c \
  env JAVA_TOOL_OPTIONS=-Xmx256m python3 docs/v2/model/check.py \
  MonitorDelivery MonitorLoss MonitorRestart MonitorStorage MonitorGap \
  MonitorUnknownSafe MonitorEmptySafe \
  MonitorStaleLive MonitorStaleMode MonitorUnknownMode MonitorBlindReplay \
  MonitorUnknownEvent MonitorEmptyFirst MonitorReplayReached MonitorGapReached \
  MonitorStorageReached MonitorRecoveryReached MonitorEpochWitness \
  MonitorLagWitness MonitorFair --timeout 45 --logs build/tuna-model
```

`dart tool/dev.dart test tuna` runs the fast positive configurations and
key negative/reachability witnesses with the same per-case time bound.
The fuller fair and additional reachability witnesses remain available in
the command above. The scripted Swift regressions can be run with:

```bash
/Users/boumenot/.local/bin/heavy-lock -- nix develop -c swift test \
  --only-use-versions-from-resolved-file --package-path apps/tuna-monitor-macos \
  --filter MonitorSessionTests
```

On the same host and revision, the locked
`dart tool/dev.dart check tuna-monitor-macos` passed 49 Swift tests, including
integration with an isolated test broker built first with
`dart tool/dev.dart check tuna`. The unknown/empty-event cases use a
scripted broker; the real-broker test does not inject those envelopes. Locked
`dart tool/dev.dart check tuna` passed the Go suite and automated model cases;
the focused orchestrator tests, Dart analyzer and formatter check also passed.
The user passed the macOS visual review of unavailable, stale and recovered
states using an isolated simulated broker.

These are bounded model and implementation checks, not a claim that arbitrary
event histories, real timing, process supervision or the Swift code have
been formally verified. TLC fingerprint collisions and the model-to-code gap
remain; future changes to the protocol or monitor should update this model
and its mapping.
