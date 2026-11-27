-------------------------- MODULE MonitorFreshness ---------------------------
EXTENDS Naturals

CONSTANTS MaxSeq, MaxCrashes, MaxStorageFailures, MaxDisconnects,
          MaxDrops, MaxPrunes, TimeoutTicks, EnableTimeout,
          InitialMode, AllowUnknownEvent, AllowEmptyFirst,
          BugKeepLive, BugStaleMode, BugUnknownEnabled, BugSkipHealth,
          BugAcceptUnknown

Modes == {"enabled", "paused", "unspecified", "unrecognized"}
Up == {"ready", "recovering"}
Active == {"snapshot", "replay", "tail"}
Entry(k, m) == [kind |-> k, mode |-> m]
NoEntry == Entry("none", "none")
Message(k, n, m, source) ==
  [kind |-> k, seq |-> n, mode |-> m, source |-> source]
NoMessage == Message("none", 0, "none", "none")

VARIABLES broker, monitor, stream
vars == <<broker, monitor, stream>>

Init ==
  /\ broker = [
       state |-> "ready", epoch |-> 0, seq |-> 1, floor |-> 0,
       mode |-> InitialMode,
       log |-> [n \in 1..MaxSeq |->
                  IF n = 1 THEN Entry("status", InitialMode) ELSE NoEntry],
       crashes |-> 0, failures |-> 0, prunes |-> 0]
  /\ monitor = [
       connection |-> "connecting", health |-> "none", last |-> 0,
       snapshot |-> FALSE, mode |-> "none", loss |-> "none",
       seen |-> 0, source |-> "none"]
  /\ stream = [
       stage |-> "idle", epoch |-> 0, healthEpoch |-> 0,
       healthy |-> FALSE, cursor |-> 0, age |-> 0,
       pending |-> NoMessage, disconnects |-> 0, drops |-> 0]

TypeOK ==
  /\ InitialMode \in Modes
  /\ broker \in [
       state: Up \cup {"down", "failed"},
       epoch: 0..(MaxCrashes + MaxStorageFailures),
       seq: 1..MaxSeq, floor: 0..MaxSeq, mode: Modes,
       log: [1..MaxSeq -> [kind: {"none", "status", "unknown"},
                            mode: Modes \cup {"none"}]],
       crashes: 0..MaxCrashes, failures: 0..MaxStorageFailures,
       prunes: 0..MaxPrunes]
  /\ monitor \in [
       connection: {"connecting", "live", "unavailable", "brokerError"},
       health: {"none", "ready", "recovering", "storage"},
       last: 0..MaxSeq, snapshot: BOOLEAN, mode: Modes \cup {"none"},
       loss: {"none", "disconnect", "timeout", "gap", "storage"},
       seen: 0..MaxSeq, source: {"none", "snapshot", "replay", "tail", "empty"}]
  /\ stream \in [
       stage: Active \cup {"idle", "health"},
       epoch: 0..(MaxCrashes + MaxStorageFailures),
       healthEpoch: 0..(MaxCrashes + MaxStorageFailures),
       healthy: BOOLEAN, cursor: 0..MaxSeq, age: 0..TimeoutTicks,
       pending: [kind: {"none", "snapshot", "status", "unknown", "heartbeat"},
                 seq: 0..MaxSeq, mode: Modes \cup {"none"},
                 source: {"none", "snapshot", "replay", "tail", "empty"}],
       disconnects: 0..MaxDisconnects, drops: 0..MaxDrops]
  /\ broker.floor <= broker.seq
  /\ monitor.last <= broker.seq

Idle(s) ==
  [s EXCEPT !.stage = "idle", !.healthy = FALSE,
            !.pending = NoMessage, !.age = 0]

Unavailable(m, reason, refresh) ==
  [m EXCEPT
    !.connection = IF BugKeepLive /\ m.connection = "live"
                   THEN "live" ELSE "unavailable",
    !.loss = reason,
    !.last = IF refresh THEN 0 ELSE @,
    !.snapshot = IF refresh THEN FALSE ELSE @]

PublishStatus(m) ==
  /\ broker.state \in Up /\ broker.seq < MaxSeq /\ m \in Modes
  /\ broker' = [broker EXCEPT
       !.seq = @ + 1, !.mode = m,
       !.log[broker.seq + 1] = Entry("status", m)]
  /\ UNCHANGED <<monitor, stream>>

\* A future oneof case decodes as nil in the current Swift bindings.
PublishUnknown(m) ==
  /\ AllowUnknownEvent
  /\ broker.state \in Up /\ broker.seq < MaxSeq /\ m \in Modes
  /\ broker' = [broker EXCEPT
       !.seq = @ + 1, !.mode = m,
       !.log[broker.seq + 1] = Entry("unknown", m)]
  /\ UNCHANGED <<monitor, stream>>

Prune ==
  /\ broker.state \in Up /\ broker.prunes < MaxPrunes
  /\ broker.floor < broker.seq
  /\ broker' = [broker EXCEPT !.floor = broker.seq, !.prunes = @ + 1]
  /\ UNCHANGED <<monitor, stream>>

Crash ==
  /\ broker.state \in Up /\ broker.crashes < MaxCrashes
  /\ broker' = [broker EXCEPT !.state = "down", !.crashes = @ + 1]
  /\ UNCHANGED <<monitor, stream>>

StorageFailure ==
  /\ broker.state \in Up /\ broker.failures < MaxStorageFailures
  /\ broker' = [broker EXCEPT !.state = "failed", !.failures = @ + 1]
  /\ UNCHANGED <<monitor, stream>>

Restart ==
  /\ broker.state \in {"down", "failed"}
  /\ broker' = [broker EXCEPT !.state = "recovering", !.epoch = @ + 1]
  /\ UNCHANGED <<monitor, stream>>

CompleteRecovery ==
  /\ broker.state = "recovering"
  /\ broker' = [broker EXCEPT !.state = "ready"]
  /\ UNCHANGED <<monitor, stream>>

ObserveHealth ==
  /\ stream.stage = "idle"
  /\ CASE broker.state \in Up ->
       /\ monitor' = [monitor EXCEPT !.health = broker.state]
       /\ stream' = [stream EXCEPT !.stage = "health", !.healthy = TRUE,
                                    !.healthEpoch = broker.epoch]
     [] broker.state = "failed" ->
       /\ monitor' = [monitor EXCEPT !.health = "storage",
                      !.connection = "brokerError", !.loss = "storage"]
       /\ UNCHANGED stream
     [] broker.state = "down" ->
       /\ monitor' = Unavailable(monitor, "disconnect", FALSE)
       /\ UNCHANGED stream
  /\ UNCHANGED broker

BeginWatch ==
  /\ stream.stage = "health"
  /\ stream' = [stream EXCEPT
       !.stage = IF monitor.last = 0 THEN "snapshot" ELSE "replay",
       !.epoch = broker.epoch, !.cursor = monitor.last,
       !.pending = NoMessage, !.age = 0]
  /\ UNCHANGED <<broker, monitor>>

BeginBlindReplay ==
  /\ BugSkipHealth /\ stream.stage = "idle"
  /\ monitor.snapshot /\ monitor.last > 0 /\ monitor.last < broker.seq
  /\ stream' = [stream EXCEPT !.stage = "replay", !.healthy = FALSE,
                              !.epoch = broker.epoch, !.cursor = monitor.last,
                              !.pending = NoMessage, !.age = 0]
  /\ UNCHANGED <<broker, monitor>>

CanSend ==
  /\ broker.state \in Up /\ stream.epoch = broker.epoch
  /\ stream.pending = NoMessage

SendSnapshot ==
  /\ stream.stage = "snapshot" /\ CanSend
  /\ stream' = [stream EXCEPT !.stage = "replay", !.cursor = broker.seq,
       !.pending = Message("snapshot", broker.seq, broker.mode, "snapshot")]
  /\ UNCHANGED <<broker, monitor>>

SendEmptyFirst ==
  /\ AllowEmptyFirst /\ stream.stage = "snapshot" /\ CanSend
  /\ stream' = [stream EXCEPT
       !.pending = Message("unknown", stream.cursor, "none", "empty")]
  /\ UNCHANGED <<broker, monitor>>

ReplayExpired ==
  /\ stream.stage = "replay" /\ CanSend
  /\ (stream.cursor < broker.floor \/ stream.cursor > broker.seq)
  /\ monitor' = Unavailable(monitor, "gap", TRUE)
  /\ stream' = Idle(stream)
  /\ UNCHANGED broker

SendReplay ==
  /\ stream.stage = "replay" /\ CanSend
  /\ stream.cursor >= broker.floor /\ stream.cursor < broker.seq
  /\ LET n == stream.cursor + 1
         e == broker.log[n]
     IN stream' = [stream EXCEPT !.cursor = n,
                      !.pending = Message(e.kind, n, e.mode, "replay")]
  /\ UNCHANGED <<broker, monitor>>

FinishReplay ==
  /\ stream.stage = "replay" /\ CanSend
  /\ stream.cursor >= broker.floor /\ stream.cursor = broker.seq
  /\ stream' = [stream EXCEPT !.stage = "tail"]
  /\ UNCHANGED <<broker, monitor>>

SendTail ==
  /\ stream.stage = "tail" /\ CanSend /\ stream.cursor < broker.seq
  /\ LET n == stream.cursor + 1
         e == broker.log[n]
     IN stream' = [stream EXCEPT !.cursor = n,
                      !.pending = Message(e.kind, n, e.mode, "tail")]
  /\ UNCHANGED <<broker, monitor>>

SendHeartbeat ==
  /\ stream.stage = "tail" /\ CanSend
  /\ stream' = [stream EXCEPT
       !.pending = Message("heartbeat", stream.cursor, "none", "tail")]
  /\ UNCHANGED <<broker, monitor>>

Receive ==
  /\ stream.stage \in Active /\ stream.pending # NoMessage
  /\ LET msg == stream.pending
     IN IF msg.kind = "snapshot"
        THEN
          /\ monitor' = [monitor EXCEPT
               !.connection = "live", !.loss = "none", !.snapshot = TRUE,
               !.last = msg.seq, !.mode = msg.mode,
               !.seen = msg.seq, !.source = msg.source]
          /\ stream' = [stream EXCEPT !.pending = NoMessage, !.age = 0]
        ELSE IF msg.kind = "unknown"
        THEN
          IF BugAcceptUnknown
          THEN
            /\ monitor' = [monitor EXCEPT !.connection = "live",
                           !.loss = "none", !.seen = msg.seq, !.source = msg.source]
            /\ stream' = [stream EXCEPT !.pending = NoMessage, !.age = 0]
          ELSE
            /\ monitor' = Unavailable(monitor, "gap", TRUE)
            /\ stream' = Idle(stream)
        ELSE IF monitor.snapshot /\
                ((msg.kind = "heartbeat" /\ msg.seq <= monitor.last) \/
                 (msg.kind = "status" /\ msg.seq <= monitor.last + 1))
        THEN
          /\ monitor' = [monitor EXCEPT
               !.connection = "live", !.loss = "none",
               !.seen = msg.seq, !.source = msg.source,
               !.last = IF msg.kind = "status" /\ msg.seq > monitor.last
                        THEN msg.seq ELSE @,
               !.mode = IF msg.kind = "status" /\ msg.seq > monitor.last
                        THEN msg.mode ELSE @]
          /\ stream' = [stream EXCEPT !.pending = NoMessage, !.age = 0]
        ELSE
          /\ monitor' = Unavailable(monitor, "gap", TRUE)
          /\ stream' = Idle(stream)
  /\ UNCHANGED broker

DropMessage ==
  /\ stream.stage \in Active /\ stream.pending # NoMessage
  /\ stream.drops < MaxDrops
  /\ stream' = [stream EXCEPT !.pending = NoMessage, !.drops = @ + 1]
  /\ UNCHANGED <<broker, monitor>>

Disconnect ==
  /\ stream.stage \in Active /\ stream.disconnects < MaxDisconnects
  /\ monitor' = Unavailable(monitor, "disconnect", FALSE)
  /\ stream' = [Idle(stream) EXCEPT !.disconnects = @ + 1]
  /\ UNCHANGED broker

DetectServerLoss ==
  /\ stream.stage \in Active
  /\ (broker.state \notin Up \/ stream.epoch # broker.epoch)
  /\ monitor' = Unavailable(monitor, "disconnect", FALSE)
  /\ stream' = Idle(stream)
  /\ UNCHANGED broker

Tick ==
  /\ EnableTimeout /\ stream.stage \in Active /\ stream.age < TimeoutTicks
  /\ stream' = [stream EXCEPT !.age = @ + 1]
  /\ UNCHANGED <<broker, monitor>>

Timeout ==
  /\ EnableTimeout /\ stream.stage \in Active /\ stream.age = TimeoutTicks
  /\ monitor' = Unavailable(monitor, "timeout", FALSE)
  /\ stream' = Idle(stream)
  /\ UNCHANGED broker

Next ==
  \/ \E m \in Modes: PublishStatus(m)
  \/ \E m \in Modes: PublishUnknown(m)
  \/ Prune \/ Crash \/ StorageFailure \/ Restart
  \/ CompleteRecovery \/ ObserveHealth \/ BeginWatch \/ BeginBlindReplay
  \/ SendSnapshot \/ SendEmptyFirst \/ ReplayExpired \/ SendReplay
  \/ FinishReplay \/ SendTail \/ SendHeartbeat \/ Receive \/ DropMessage
  \/ Disconnect \/ DetectServerLoss \/ Tick \/ Timeout

Spec == Init /\ [][Next]_vars

FairSpec ==
  /\ Spec
  /\ WF_vars(Restart) /\ WF_vars(CompleteRecovery)
  /\ WF_vars(ObserveHealth) /\ WF_vars(BeginWatch)
  /\ WF_vars(SendSnapshot) /\ WF_vars(ReplayExpired)
  /\ WF_vars(SendReplay) /\ WF_vars(FinishReplay)
  /\ SF_vars(SendTail) /\ WF_vars(SendHeartbeat)
  /\ WF_vars(Receive) /\ WF_vars(DetectServerLoss)

NoDetectedLossLive == monitor.loss = "none" \/ monitor.connection # "live"
LiveHasAttemptHealth ==
  monitor.connection # "live" \/
    (stream.stage \in Active /\ stream.healthy /\
     monitor.health \in {"ready", "recovering"})
LiveHasSnapshot == monitor.connection # "live" \/ monitor.snapshot
LiveReflectsDeliveredSeq ==
  monitor.connection # "live" \/ monitor.seen <= monitor.last
CachedStatusAtSequence ==
  ~monitor.snapshot \/
    (monitor.last > 0 /\ monitor.mode = broker.log[monitor.last].mode)

NormalizeMode(m) ==
  IF m \in {"enabled", "paused"} THEN m
  ELSE IF BugUnknownEnabled THEN "enabled" ELSE "unknown"

PresentedMode ==
  IF monitor.connection = "live" \/ BugStaleMode
  THEN NormalizeMode(monitor.mode) ELSE "unknown"

NonLiveModeUnknown ==
  monitor.connection = "live" \/ PresentedMode = "unknown"
EnabledOnlyWhenKnown ==
  PresentedMode # "enabled" \/
    (monitor.connection = "live" /\ monitor.mode = "enabled")

NeverLive == monitor.connection # "live"
NeverReplayAfterDisconnect ==
  ~(monitor.connection = "live" /\ monitor.source = "replay" /\
    stream.disconnects > 0)
NeverGapRefresh ==
  ~(stream.stage = "snapshot" /\ monitor.loss = "gap" /\
    monitor.mode # "none" /\ stream.disconnects > 0)
NeverStorageObserved == monitor.connection # "brokerError"
NeverRecoveringLive ==
  ~(monitor.connection = "live" /\ monitor.health = "recovering")
NeverTimeout == monitor.loss # "timeout"
LiveHealthSameEpoch ==
  monitor.connection # "live" \/ stream.healthEpoch = stream.epoch
LiveAtBrokerHead ==
  monitor.connection # "live" \/ monitor.last = broker.seq

EventuallyStableLive == <>[](monitor.connection = "live")
EventuallySynchronized ==
  <>[](monitor.connection = "live" /\ monitor.last = broker.seq)
=============================================================================
