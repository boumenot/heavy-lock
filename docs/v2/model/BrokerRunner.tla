---------------------------- MODULE BrokerRunner ----------------------------
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANTS Jobs, Weight, Capacity, MaxBrokerCrashes, MaxRunnerCrashes,
          MaxDisconnects, GraceLimit, DurableFence, SafeAbsence,
          CompleteTracking, ForceWhilePaused, RecoveryRPCs

ASSUME /\ Jobs # {}
       /\ Weight \in [Jobs -> (Nat \ {0})]
       /\ Capacity \in (Nat \ {0})

VARIABLES db, queue, runner, roots, children, tracked, launches, grants,
          reports, broker, paused, reconciled, absent, probes, brokerCrashes,
          runnerCrashes, disconnects, grace, normalOK, recoveryOK

vars == <<db, queue, runner, roots, children, tracked, launches, grants,
          reports, broker, paused, reconciled, absent, probes, brokerCrashes,
          runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

Phases == {"absent", "queued", "reserved", "running", "finishing", "finished"}
Fences == {"fresh", "requested", "intent", "sealed"}
LifecycleReports == {"started", "finished", "aborted", "seal", "seal_no_spawn"}
ReportKinds == LifecycleReports \cup {"claim_request", "claim_reply"}
Outcomes == {"pending", "unknown", "reported", "aborted", "cancelled", "removed"}
FenceProofs == {"none", "seal", "runner_dead", "claim_prevented"}
Active == {j \in Jobs : db[j].held}
UnitWeights == [j \in Jobs |-> 1]
MixedWeights == [j \in Jobs |-> IF j = CHOOSE k \in Jobs : TRUE THEN 2 ELSE 1]
RECURSIVE Slots(_)
Slots(S) == IF S = {} THEN 0
            ELSE LET j == CHOOSE k \in S : TRUE
                 IN Weight[j] + Slots(S \ {j})
Occupied == Slots(Active)
Remove(q, j) == SelectSeq(q, LAMBDA k : k # j)
Callable == broker = "ready" \/ (RecoveryRPCs /\ broker = "recovering")
Live(j) == j \in roots \/ j \in children
HasReport(j, kind) ==
  \E m \in reports : m.job = j /\ m.kind = kind
                      /\ (~DurableFence \/ db[j].assigned = m.incarnation)
CanSpawn(j) == runner[j].up /\ runner[j].grant
              /\ runner[j].ticket
              /\ ~runner[j].cancel /\ launches[j].count < 2

Init ==
  /\ db = [j \in Jobs |-> [phase |-> "absent", held |-> FALSE,
                           issued |-> FALSE, forced |-> FALSE, forcedWhilePaused |-> FALSE,
                           cancel |-> FALSE, started |-> FALSE,
                           assigned |-> 0, claim |-> 0, closed |-> FALSE,
                           proof |-> "none", claimReady |-> TRUE,
                           noLaunch |-> FALSE, knownChild |-> FALSE,
                           rootId |-> 0, exit |-> 0, fromFinish |-> FALSE,
                           reportedDuringRecovery |-> FALSE, graceReset |-> FALSE,
                           outcome |-> "pending"]]
  /\ queue = <<>>
  /\ runner = [j \in Jobs |-> [up |-> TRUE, connected |-> TRUE,
                               grant |-> FALSE, cancel |-> FALSE,
                               fence |-> "fresh", ticket |-> FALSE,
                               identity |-> FALSE, incarnation |-> 1]]
  /\ roots = {}
  /\ children = {}
  /\ tracked = {}
  /\ launches = [j \in Jobs |-> [count |-> 0, valid |-> TRUE,
                                  childEver |-> FALSE, duringRecovery |-> FALSE,
                                  duringStorageFailure |-> FALSE]]
  /\ grants = {}
  /\ reports = {}
  /\ broker = "ready"
  /\ paused = FALSE
  /\ reconciled = {}
  /\ absent = {}
  /\ probes = {}
  /\ brokerCrashes = 0
  /\ runnerCrashes = 0
  /\ disconnects = 0
  /\ grace = [j \in Jobs |-> 0]
  /\ normalOK = TRUE
  /\ recoveryOK = TRUE

\* A transaction is atomic; crashes occur before or after it, not inside it.
\* A job denotes one logical submission, idempotency key and unique grant.
Submit(j) ==
  /\ broker = "ready" /\ runner[j].up /\ runner[j].connected
  /\ db[j].phase \in {"absent", "queued"}
  /\ IF db[j].phase = "absent"
        THEN /\ db' = [db EXCEPT ![j].phase = "queued"]
             /\ queue' = Append(queue, j)
        ELSE UNCHANGED <<db, queue>>
  /\ UNCHANGED <<runner, roots, children, tracked, launches, grants, reports,
                 broker, paused, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

Reserve(j, forced, resourcePass) ==
  /\ broker = "ready" /\ db[j].phase = "queued"
  /\ runner[j].up /\ runner[j].connected
  /\ IF forced THEN (~paused \/ ForceWhilePaused)
               ELSE /\ ~paused /\ Head(queue) = j
                    /\ Occupied + Weight[j] <= Capacity
                    /\ (Occupied = 0 \/ resourcePass)
  /\ db' = [db EXCEPT ![j].phase = "reserved", ![j].held = TRUE,
                     ![j].issued = TRUE, ![j].forced = forced,
                     ![j].forcedWhilePaused = forced /\ paused,
                     ![j].assigned = runner[j].incarnation]
  /\ queue' = Remove(queue, j)
  /\ normalOK' = normalOK /\
       (forced \/ (~paused /\ Head(queue) = j
                   /\ Occupied + Weight[j] <= Capacity
                   /\ (Occupied = 0 \/ resourcePass)))
  /\ recoveryOK' = recoveryOK /\ broker = "ready"
  /\ UNCHANGED <<runner, roots, children, tracked, launches, grants, reports,
                 broker, paused, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace>>

Move(j, front) ==
  /\ broker = "ready" /\ db[j].phase = "queued"
  /\ queue' = IF front THEN <<j>> \o Remove(queue, j)
                       ELSE Append(Remove(queue, j), j)
  /\ UNCHANGED <<db, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

Pause ==
  /\ broker = "ready"
  /\ paused' = ~paused
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

\* Grant snapshots may be lost, duplicated or delivered after cancellation.
SendGrant(j) ==
  /\ Callable /\ db[j].held
  /\ grants' = grants \cup {[job |-> j, cancel |-> db[j].cancel]}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

DeliverGrant(m) ==
  /\ m \in grants /\ runner[m.job].up
  /\ runner' = [runner EXCEPT ![m.job].grant = TRUE,
                             ![m.job].cancel = @ \/ m.cancel]
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

DropGrant(m) ==
  /\ m \in grants /\ grants' = grants \ {m}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

\* The durable broker transaction and delivery of its reply are separate from
\* the OS spawn. Requests and replies can be dropped, duplicated or delayed.
RequestClaim(j) ==
  /\ runner[j].up /\ runner[j].grant /\ ~runner[j].cancel
  /\ runner[j].fence \in {"fresh", "requested"}
  /\ runner' = [runner EXCEPT ![j].fence = "requested"]
  /\ reports' = reports \cup {[job |-> j, kind |-> "claim_request",
                               incarnation |-> runner[j].incarnation,
                               root |-> 0, exit |-> 0]}
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

CommitClaim(m) ==
  /\ m \in reports /\ m.kind = "claim_request" /\ broker = "ready"
  /\ db[m.job].phase = "reserved" /\ db[m.job].held
  /\ ~db[m.job].closed /\ ~db[m.job].cancel
  /\ db[m.job].outcome # "aborted"
  /\ (~DurableFence \/ (db[m.job].assigned = m.incarnation
                         /\ db[m.job].claim \in {0, m.incarnation}))
  /\ db' = IF DurableFence
              THEN [db EXCEPT ![m.job].claim = m.incarnation,
                              ![m.job].claimReady = @ /\ broker = "ready"]
              ELSE db
  /\ reports' = reports \cup {[job |-> m.job, kind |-> "claim_reply",
                               incarnation |-> m.incarnation,
                               root |-> 0, exit |-> 0]}
  /\ UNCHANGED <<queue, runner, roots, children, tracked, launches, grants,
                 broker, paused, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

DeliverClaim(m) ==
  /\ m \in reports /\ m.kind = "claim_reply"
  /\ runner[m.job].up /\ runner[m.job].incarnation = m.incarnation
  /\ runner[m.job].fence = "requested"
  /\ runner' = [runner EXCEPT ![m.job].fence = "intent",
                             ![m.job].ticket = TRUE]
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

Spawn(j) ==
  /\ CanSpawn(j)
  /\ runner' = [runner EXCEPT ![j].ticket = FALSE, ![j].identity = TRUE]
  /\ roots' = roots \cup {j}
  /\ launches' = [launches EXCEPT ![j].count = @ + 1,
                      ![j].valid = @ /\ db[j].held /\ ~db[j].closed
                                      /\ db[j].claim = runner[j].incarnation
                                      /\ db[j].assigned = runner[j].incarnation,
                      ![j].duringRecovery = @ \/ broker = "recovering",
                      ![j].duringStorageFailure = @ \/
                          broker \in {"storage_failed", "down_storage_failed"}]
  /\ UNCHANGED <<db, queue, children, tracked, grants, reports, broker,
                 paused, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

Fork(j) ==
  /\ j \in roots /\ ~launches[j].childEver
  /\ children' = children \cup {j}
  /\ launches' = [launches EXCEPT ![j].childEver = TRUE]
  /\ UNCHANGED <<db, queue, runner, roots, tracked, grants, reports,
                 broker, paused, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

TrackChild(j) ==
  /\ Callable /\ j \in children /\ db[j].held /\ ~db[j].knownChild
  /\ db' = [db EXCEPT ![j].knownChild = TRUE]
  /\ absent' = absent \ {j}
  /\ UNCHANGED <<queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

ExitRoot(j) ==
  /\ j \in roots /\ roots' = roots \ {j}
  /\ UNCHANGED <<db, queue, runner, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

ExitChild(j) ==
  /\ j \in children /\ children' = children \ {j}
  /\ UNCHANGED <<db, queue, runner, roots, tracked, launches, grants, reports,
                 broker, paused, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

SendReport(j, kind) ==
  /\ runner[j].up
  /\ CASE kind = "started" -> launches[j].count > 0
                                /\ runner[j].identity
       [] kind = "finished" -> runner[j].identity /\ j \notin roots
       [] kind = "aborted" -> runner[j].fence = "sealed" /\ ~runner[j].identity
       [] kind = "seal" -> runner[j].fence = "sealed"
       [] kind = "seal_no_spawn" -> runner[j].fence = "sealed" /\ ~runner[j].identity
  /\ reports' = reports \cup {[job |-> j, kind |-> kind,
                               incarnation |-> runner[j].incarnation,
                               root |-> IF kind \in {"started", "finished"} THEN 1 ELSE 0,
                               exit |-> IF kind = "finished" THEN 1 ELSE 0]}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

\* Conflicting duplicates are generated after a first accepted identity or
\* outcome; the receiver must not replace the persisted values.
SendConflict(j, kind) ==
  /\ runner[j].up /\ runner[j].identity /\ db[j].started
  /\ kind \in {"started", "finished"}
  /\ reports' = reports \cup {[job |-> j, kind |-> kind,
                               incarnation |-> runner[j].incarnation,
                               root |-> 2,
                               exit |-> IF kind = "finished" THEN 2 ELSE 0]}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

SendOutcomeConflict(j) ==
  /\ runner[j].up /\ runner[j].identity /\ db[j].outcome = "reported"
  /\ reports' = reports \cup {[job |-> j, kind |-> "finished",
                               incarnation |-> runner[j].incarnation,
                               root |-> 1, exit |-> 2]}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

ReceiveStarted(m) ==
  /\ Callable /\ m \in reports /\ m.kind = "started"
  /\ db[m.job].phase = "reserved"
  /\ (~DurableFence \/ (db[m.job].assigned = m.incarnation
                         /\ db[m.job].claim = m.incarnation))
  /\ m.root # 0 /\ db[m.job].rootId \in {0, m.root}
  /\ db' = [db EXCEPT ![m.job].phase = "running", ![m.job].started = TRUE,
                     ![m.job].rootId = m.root,
                     ![m.job].reportedDuringRecovery = @ \/ broker = "recovering"]
  /\ tracked' = tracked \cup {m.job}
  /\ absent' = absent \ {m.job}
  /\ UNCHANGED <<queue, runner, roots, children, launches, grants, reports,
                 broker, paused, reconciled, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

ReceiveFinished(m) ==
  /\ Callable /\ m \in reports /\ m.kind = "finished"
  /\ db[m.job].phase \in {"reserved", "running"}
  /\ (~DurableFence \/ (db[m.job].assigned = m.incarnation
                         /\ db[m.job].claim = m.incarnation))
  /\ m.root # 0 /\ m.exit # 0 /\ db[m.job].rootId \in {0, m.root}
  /\ db' = [db EXCEPT ![m.job].phase = "finishing", ![m.job].started = TRUE,
                     ![m.job].rootId = m.root, ![m.job].exit = m.exit,
                     ![m.job].fromFinish = @ \/ db[m.job].phase = "reserved",
                     ![m.job].reportedDuringRecovery = @ \/ broker = "recovering",
                     ![m.job].outcome = "reported"]
  /\ tracked' = tracked \cup {m.job}
  /\ absent' = absent \ {m.job}
  /\ UNCHANGED <<queue, runner, roots, children, launches, grants,
                 reports, broker, paused, reconciled, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

ReceiveAborted(j) ==
  /\ Callable /\ HasReport(j, "aborted")
  /\ db[j].phase = "reserved"
  /\ db' = [db EXCEPT ![j].outcome = "aborted"]
  /\ UNCHANGED <<queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

DropReport(m) ==
  /\ m \in reports /\ reports' = reports \ {m}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches, grants,
                 broker, paused, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

Cancel(j) ==
  /\ broker = "ready" /\ db[j].phase \notin {"absent", "finished"}
  /\ db' = [db EXCEPT ![j].cancel = TRUE,
             ![j].phase = IF @ = "queued" THEN "finished" ELSE @,
             ![j].outcome = IF db[j].phase = "queued" THEN "cancelled" ELSE @]
  /\ queue' = Remove(queue, j)
  /\ UNCHANGED <<runner, roots, children, tracked, launches, grants, reports,
                 broker, paused, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

\* Timeout triggers investigation, never release. The original runner
\* serializes sealing with spawn; the separate report may be lost or delayed.
ProbeTimeout(j) ==
  /\ broker = "ready" /\ db[j].phase = "reserved"
  /\ probes' = probes \cup {j}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

SealLaunch(j) ==
  /\ runner[j].up /\ runner[j].grant
  /\ (~DurableFence \/ runner[j].incarnation = db[j].assigned)
  /\ runner[j].fence \in {"fresh", "requested", "intent"}
  /\ runner' = [runner EXCEPT ![j].fence = "sealed", ![j].ticket = FALSE]
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

ReceiveSeal(j) ==
  /\ Callable /\ db[j].held
  /\ (HasReport(j, "seal") \/ HasReport(j, "seal_no_spawn"))
  /\ (~db[j].closed \/ (~db[j].noLaunch /\ HasReport(j, "seal_no_spawn")))
  /\ db' = [db EXCEPT ![j].closed = TRUE,
                     ![j].proof = IF @ = "none" THEN "seal" ELSE @,
                     ![j].noLaunch = @ \/ HasReport(j, "seal_no_spawn")]
  /\ UNCHANGED <<queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

\* Closing an unclaimed grant and observing its unclaimed state is one
\* broker transaction, serialized with CommitClaim.
PreventClaim(j) ==
  /\ broker \in {"ready", "recovering"} /\ db[j].held
  /\ db[j].claim = 0 /\ ~db[j].noLaunch
  /\ db' = [db EXCEPT ![j].closed = TRUE,
                     ![j].proof = IF @ = "none" THEN "claim_prevented" ELSE @,
                     ![j].noLaunch = TRUE]
  /\ UNCHANGED <<queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

\* This abstracts a successful OS verification, not merely a lost stream.
VerifyRunnerDead(j) ==
  /\ broker \in {"ready", "recovering"} /\ db[j].held /\ ~db[j].closed
  /\ (~runner[j].up \/ runner[j].incarnation # db[j].assigned)
  /\ db' = [db EXCEPT ![j].closed = TRUE, ![j].proof = "runner_dead"]
  /\ UNCHANGED <<queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

MarkUnknown(j) ==
  /\ broker \in {"ready", "recovering"} /\ db[j].held
  /\ db[j].phase \in {"reserved", "running"} /\ db[j].outcome = "pending"
  /\ (broker = "recovering" \/ ~runner[j].up
      \/ ~runner[j].connected \/ j \in probes)
  /\ db' = [db EXCEPT ![j].outcome = "unknown"]
  /\ UNCHANGED <<queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

\* A successful scan needs the recorded root identity and all known children
\* gone. The CompleteTracking variant additionally excludes untracked children.
ObserveAbsent(j) ==
  /\ broker \in {"ready", "recovering"} /\ db[j].held
  /\ IF SafeAbsence
        THEN /\ db[j].closed
             /\ (db[j].noLaunch
                 \/ (j \in tracked /\ j \notin roots
                     /\ ~(db[j].knownChild /\ j \in children)))
        ELSE j \notin roots
  /\ CompleteTracking => j \notin children
  /\ absent' = absent \cup {j}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, reports, broker, paused, reconciled, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

Release(j) ==
  /\ broker \in {"ready", "recovering"} /\ db[j].held /\ j \in absent
  /\ db' = [db EXCEPT ![j].phase = "finished", ![j].held = FALSE,
             ![j].outcome = IF @ = "pending" THEN "unknown" ELSE @]
  /\ UNCHANGED <<queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

BrokerCrash ==
  /\ broker \notin {"down", "down_storage_failed"}
  /\ brokerCrashes < MaxBrokerCrashes
  /\ broker' = IF broker = "storage_failed" THEN "down_storage_failed" ELSE "down"
  /\ brokerCrashes' = brokerCrashes + 1
  /\ runner' = [j \in Jobs |-> [runner[j] EXCEPT !.connected = FALSE]]
  /\ reconciled' = {} /\ absent' = {}
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, paused, probes, runnerCrashes, disconnects,
                 grace, normalOK, recoveryOK>>

BrokerRestart ==
  /\ broker \in {"down", "down_storage_failed"}
  /\ broker' = IF broker = "down_storage_failed" THEN "storage_failed"
               ELSE "recovering"
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, reports, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

StorageFailure ==
  /\ broker \in {"ready", "recovering"}
  /\ broker' = "storage_failed" /\ reconciled' = {} /\ absent' = {}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, reports, paused, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

StorageRestored ==
  /\ broker = "storage_failed" /\ broker' = "recovering"
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, reports, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

Reconcile(j) ==
  /\ broker = "recovering"
  /\ ~db[j].held \/ (j \in tracked /\ Live(j))
  /\ reconciled' = reconciled \cup {j}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, reports, broker, paused, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

RecoveryDone ==
  /\ broker = "recovering"
  /\ Active \subseteq reconciled
  /\ broker' = "ready"
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, reports, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

RunnerCrash(j) ==
  /\ runner[j].up /\ runnerCrashes < MaxRunnerCrashes
  /\ runner' = [runner EXCEPT ![j].up = FALSE, ![j].connected = FALSE,
             ![j].grant = FALSE, ![j].cancel = FALSE,
             ![j].ticket = FALSE, ![j].identity = FALSE,
             ![j].fence = "fresh"]
  /\ runnerCrashes' = runnerCrashes + 1
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, disconnects, grace, normalOK, recoveryOK>>

RunnerRestart(j) ==
  /\ ~runner[j].up
  /\ runner' = [runner EXCEPT ![j].up = TRUE,
                              ![j].incarnation = @ + 1]
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

Disconnect(j) ==
  /\ runner[j].connected /\ disconnects < MaxDisconnects
  /\ runner' = [runner EXCEPT ![j].connected = FALSE]
  /\ disconnects' = disconnects + 1
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, grace, normalOK, recoveryOK>>

Reconnect(j) ==
  /\ Callable /\ runner[j].up /\ ~runner[j].connected
  /\ db[j].phase # "finished"
  /\ runner' = [runner EXCEPT ![j].connected = TRUE]
  /\ grace' = IF db[j].phase = "queued"
                 THEN [grace EXCEPT ![j] = 0]
                 ELSE grace
  /\ db' = [db EXCEPT ![j].graceReset =
                @ \/ (db[j].phase = "queued" /\ grace[j] > 0)]
  /\ UNCHANGED <<queue, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects,
                 normalOK, recoveryOK>>

TickGrace(j) ==
  /\ broker = "ready" /\ db[j].phase = "queued"
  /\ ~runner[j].connected /\ grace[j] < GraceLimit
  /\ grace' = [grace EXCEPT ![j] = @ + 1]
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, reports, broker, paused, reconciled, absent,
                 probes, brokerCrashes, runnerCrashes, disconnects,
                 normalOK, recoveryOK>>

ExpireGrace(j) ==
  /\ broker = "ready" /\ db[j].phase = "queued"
  /\ ~runner[j].connected /\ grace[j] = GraceLimit
  /\ db' = [db EXCEPT ![j].phase = "finished", ![j].outcome = "removed"]
  /\ queue' = Remove(queue, j)
  /\ UNCHANGED <<runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

Next ==
  \/ Pause \/ BrokerCrash \/ BrokerRestart \/ StorageFailure \/ StorageRestored
  \/ RecoveryDone
  \/ \E m \in grants : DeliverGrant(m) \/ DropGrant(m)
  \/ \E m \in reports : DropReport(m) \/ CommitClaim(m) \/ DeliverClaim(m)
                            \/ ReceiveStarted(m) \/ ReceiveFinished(m)
  \/ \E j \in Jobs :
       \/ Submit(j)
       \/ \E forced, resourcePass \in BOOLEAN : Reserve(j, forced, resourcePass)
       \/ Move(j, TRUE) \/ Move(j, FALSE) \/ SendGrant(j)
       \/ RequestClaim(j) \/ Spawn(j) \/ Fork(j) \/ TrackChild(j)
       \/ ExitRoot(j) \/ ExitChild(j)
       \/ \E kind \in LifecycleReports : SendReport(j, kind)
       \/ \E kind \in {"started", "finished"} : SendConflict(j, kind)
       \/ SendOutcomeConflict(j) \/ ReceiveAborted(j) \/ ReceiveSeal(j)
       \/ Cancel(j) \/ ProbeTimeout(j) \/ SealLaunch(j)
       \/ PreventClaim(j) \/ VerifyRunnerDead(j) \/ MarkUnknown(j)
       \/ ObserveAbsent(j) \/ Release(j) \/ Reconcile(j)
       \/ RunnerCrash(j) \/ RunnerRestart(j) \/ Disconnect(j) \/ Reconnect(j)
       \/ TickGrace(j) \/ ExpireGrace(j)

Spec == Init /\ [][Next]_vars
\* Focused profiles omit queue reordering, pause/force and child creation;
\* their bounds and omissions are documented, not a proof of the full model.
ClaimNext ==
  \/ BrokerCrash \/ BrokerRestart \/ RecoveryDone
  \/ \E m \in grants : DeliverGrant(m) \/ DropGrant(m)
  \/ \E m \in reports : DropReport(m) \/ CommitClaim(m) \/ DeliverClaim(m)
  \/ \E j \in Jobs :
       \/ Submit(j) \/ Reserve(j, FALSE, TRUE) \/ SendGrant(j)
       \/ RequestClaim(j) \/ Spawn(j) \/ Cancel(j) \/ SealLaunch(j)
       \/ SendReport(j, "seal") \/ SendReport(j, "seal_no_spawn")
       \/ ReceiveSeal(j) \/ PreventClaim(j) \/ VerifyRunnerDead(j)
       \/ MarkUnknown(j) \/ ObserveAbsent(j) \/ Release(j)
       \/ RunnerCrash(j) \/ RunnerRestart(j) \/ Reconnect(j)
ClaimSpec == Init /\ [][ClaimNext]_vars
TimeoutNext == ClaimNext \/ \E j \in Jobs : ProbeTimeout(j)
TimeoutSpec == Init /\ [][TimeoutNext]_vars
StorageNext == ClaimNext \/ StorageFailure \/ StorageRestored
StorageSpec == Init /\ [][StorageNext]_vars
FocusedNext ==
  \/ ClaimNext
  \/ \E m \in reports : ReceiveStarted(m) \/ ReceiveFinished(m)
  \/ \E j \in Jobs :
       \/ ExitRoot(j) \/ Fork(j) \/ TrackChild(j) \/ ExitChild(j)
       \/ SendReport(j, "started") \/ SendReport(j, "finished")
       \/ SendReport(j, "aborted") \/ ReceiveAborted(j)
       \/ \E kind \in {"started", "finished"} : SendConflict(j, kind)
       \/ SendOutcomeConflict(j) \/ Reconcile(j)
FocusedSpec == Init /\ [][FocusedNext]_vars
FinishNext ==
  \/ \E m \in grants : DeliverGrant(m) \/ DropGrant(m)
  \/ \E m \in reports : DropReport(m) \/ CommitClaim(m) \/ DeliverClaim(m)
                            \/ ReceiveStarted(m) \/ ReceiveFinished(m)
  \/ \E j \in Jobs :
       \/ Submit(j) \/ Reserve(j, FALSE, TRUE) \/ SendGrant(j)
       \/ RequestClaim(j) \/ Spawn(j) \/ ExitRoot(j)
       \/ SendReport(j, "started") \/ SendReport(j, "finished")
       \/ \E kind \in {"started", "finished"} : SendConflict(j, kind)
       \/ SendOutcomeConflict(j)
FinishSpec == Init /\ [][FinishNext]_vars
RecoveryReportNext ==
  \/ BrokerCrash \/ BrokerRestart \/ RecoveryDone
  \/ \E m \in grants : DeliverGrant(m) \/ DropGrant(m)
  \/ \E m \in reports : DropReport(m) \/ CommitClaim(m) \/ DeliverClaim(m)
                            \/ ReceiveStarted(m) \/ ReceiveFinished(m)
  \/ \E j \in Jobs :
       \/ Submit(j) \/ Reserve(j, FALSE, TRUE) \/ SendGrant(j)
       \/ RequestClaim(j) \/ Spawn(j) \/ ExitRoot(j) \/ Cancel(j)
       \/ \E kind \in LifecycleReports : SendReport(j, kind)
       \/ \E kind \in {"started", "finished"} : SendConflict(j, kind)
       \/ SendOutcomeConflict(j) \/ ReceiveAborted(j) \/ ReceiveSeal(j)
       \/ SealLaunch(j) \/ PreventClaim(j) \/ MarkUnknown(j)
       \/ ObserveAbsent(j) \/ Release(j) \/ Reconcile(j) \/ Reconnect(j)
RecoveryReportSpec == Init /\ [][RecoveryReportNext]_vars
ObservationNext ==
  \/ \E m \in grants : DeliverGrant(m)
  \/ \E m \in reports : CommitClaim(m) \/ DeliverClaim(m) \/ ReceiveStarted(m)
  \/ \E j \in Jobs :
       \/ Submit(j) \/ Reserve(j, FALSE, TRUE) \/ SendGrant(j)
       \/ RequestClaim(j) \/ Spawn(j) \/ Fork(j) \/ TrackChild(j) \/ ExitRoot(j)
       \/ SendReport(j, "started") \/ SealLaunch(j) \/ SendReport(j, "seal")
       \/ ReceiveSeal(j) \/ ObserveAbsent(j) \/ Release(j)
ObservationSpec == Init /\ [][ObservationNext]_vars
GraceNext ==
  \/ BrokerCrash \/ BrokerRestart \/ RecoveryDone
  \/ \E j \in Jobs : Submit(j) \/ Disconnect(j) \/ Reconnect(j)
                      \/ TickGrace(j) \/ ExpireGrace(j)
GraceSpec == Init /\ [][GraceNext]_vars
SchedulingNext ==
  \/ Pause
  \/ \E j \in Jobs :
       \/ Submit(j) \/ Move(j, TRUE) \/ Move(j, FALSE) \/ Cancel(j)
       \/ \E forced, resourcePass \in BOOLEAN : Reserve(j, forced, resourcePass)
SchedulingSpec == Init /\ [][SchedulingNext]_vars
StorageSchedulingNext == SchedulingNext \/ StorageFailure \/ StorageRestored
                         \/ RecoveryDone
StorageSchedulingSpec == Init /\ [][StorageSchedulingNext]_vars

TypeOK ==
  /\ db \in [Jobs -> [phase : Phases, held : BOOLEAN, issued : BOOLEAN,
                       forced : BOOLEAN, forcedWhilePaused : BOOLEAN,
                       cancel : BOOLEAN, started : BOOLEAN,
                       assigned : 0..(MaxRunnerCrashes + 1),
                       claim : 0..(MaxRunnerCrashes + 1), closed : BOOLEAN,
                       proof : FenceProofs, claimReady : BOOLEAN,
                       noLaunch : BOOLEAN, knownChild : BOOLEAN,
                       rootId : 0..2, exit : 0..2, fromFinish : BOOLEAN,
                       reportedDuringRecovery : BOOLEAN, graceReset : BOOLEAN,
                       outcome : Outcomes]]
  /\ runner \in [Jobs -> [up : BOOLEAN, connected : BOOLEAN, grant : BOOLEAN,
                           cancel : BOOLEAN, fence : Fences, ticket : BOOLEAN,
                           identity : BOOLEAN,
                           incarnation : 1..(MaxRunnerCrashes + 1)]]
  /\ queue \in Seq(Jobs) /\ Len(queue) <= Cardinality(Jobs)
  /\ roots \subseteq Jobs /\ children \subseteq Jobs /\ tracked \subseteq Jobs
  /\ launches \in [Jobs -> [count : 0..2, valid : BOOLEAN, childEver : BOOLEAN,
                             duringRecovery : BOOLEAN,
                             duringStorageFailure : BOOLEAN]]
  /\ grants \subseteq [job : Jobs, cancel : BOOLEAN]
  /\ reports \subseteq [job : Jobs, kind : ReportKinds,
                          incarnation : 1..(MaxRunnerCrashes + 1),
                          root : 0..2, exit : 0..2]
  /\ broker \in {"ready", "down", "recovering", "storage_failed",
                  "down_storage_failed"} /\ paused \in BOOLEAN
  /\ reconciled \subseteq Jobs /\ absent \subseteq Jobs /\ probes \subseteq Jobs
  /\ brokerCrashes \in 0..MaxBrokerCrashes
  /\ runnerCrashes \in 0..MaxRunnerCrashes
  /\ disconnects \in 0..MaxDisconnects /\ grace \in [Jobs -> 0..GraceLimit]
  /\ normalOK \in BOOLEAN /\ recoveryOK \in BOOLEAN

QueueConsistent ==
  /\ {queue[i] : i \in 1..Len(queue)} = {j \in Jobs : db[j].phase = "queued"}
  /\ Len(queue) = Cardinality({queue[i] : i \in 1..Len(queue)})
ReservationsConsistent ==
  \A j \in Jobs : db[j].held = (db[j].phase \in {"reserved", "running", "finishing"})
DurableBeforeDelivery ==
  \A m \in grants : db[m.job].issued
NoDuplicateLaunch == \A j \in Jobs : launches[j].count <= 1
ClaimBeforeSpawn == \A j \in Jobs : launches[j].valid
ClaimBoundToRunner ==
  \A j \in Jobs : db[j].claim = 0 \/ db[j].claim = db[j].assigned
NoClaimWhenNotReady == \A j \in Jobs : db[j].claimReady
NeverSpawnWhileRecovering == \A j \in Jobs : ~launches[j].duringRecovery
NeverSpawnWhileStorageFailed ==
  \A j \in Jobs : ~launches[j].duringStorageFailure
NoLiveUnreserved == \A j \in Jobs : Live(j) => db[j].held
NoLiveRootUnreserved == \A j \in Jobs : j \in roots => db[j].held
NoKnownChildUnreserved ==
  \A j \in Jobs : (db[j].knownChild /\ j \in children) => db[j].held
NoPrematureRelease ==
  \A j \in Jobs : (db[j].issued /\ ~db[j].held) =>
                   (db[j].closed
                    /\ (db[j].noLaunch \/ (j \in tracked /\ j \notin roots
                         /\ ~(db[j].knownChild /\ j \in children)))
                    /\ ~runner[j].ticket)
NoFutureProof == \A j \in Jobs : db[j].closed = (db[j].proof # "none")
NoFalseNoLaunch == \A j \in Jobs : db[j].noLaunch => launches[j].count = 0
NoConflictingReportsAccepted ==
  \A j \in Jobs : db[j].rootId \in {0, 1} /\ db[j].exit \in {0, 1}
UnknownOutcomeHeld ==
  \A j \in Jobs : (db[j].outcome = "unknown" /\ db[j].phase # "finished") =>
                   db[j].held
NormalCapacity == Slots({j \in Active : ~db[j].forced}) <= Capacity
NormalFIFO == normalOK
NoReservationWhenNotReady == recoveryOK
StartedBeforeFinishing ==
  \A j \in Jobs : db[j].phase = "finishing" => db[j].started
NeverForcedOverload == Occupied <= Capacity
NeverForcedWhilePaused == \A j \in Jobs : ~db[j].forcedWhilePaused
NeverFinishBeforeStart == \A j \in Jobs : ~db[j].fromFinish
NeverRecoveryReport == \A j \in Jobs : ~db[j].reportedDuringRecovery
NeverReleasePreventedClaim ==
  \A j \in Jobs : ~(db[j].phase = "finished" /\ db[j].noLaunch
                    /\ db[j].proof = "claim_prevented")
NeverReleaseSealedNoSpawn ==
  \A j \in Jobs : ~(db[j].phase = "finished" /\ db[j].proof = "seal"
                    /\ db[j].noLaunch /\ db[j].claim # 0)
GraceResetWhenConnected ==
  \A j \in Jobs : (db[j].phase = "queued" /\ runner[j].connected) => grace[j] = 0
ExpiredJobNotRevived ==
  \A j \in Jobs : db[j].outcome = "removed" =>
    (db[j].phase = "finished" /\ ~runner[j].connected /\ grace[j] = GraceLimit)
NeverGraceReset == \A j \in Jobs : ~db[j].graceReset
NeverHeadBlocked ==
  ~(Len(queue) > 1
    /\ Occupied + Weight[Head(queue)] > Capacity
    /\ \E i \in 2..Len(queue) : Occupied + Weight[queue[i]] <= Capacity)
RecoveryAcceptsPendingIdentity ==
  \A j \in Jobs :
    (broker = "recovering" /\ db[j].phase = "reserved"
     /\ Live(j) /\ j \notin tracked
     /\ HasReport(j, "started")) => RecoveryRPCs
NeverCrashBeforeLaunchUnknown ==
  \A j \in Jobs :
    ~(db[j].held /\ db[j].claim # 0 /\ ~runner[j].up
      /\ launches[j].count = 0 /\ db[j].outcome = "unknown")

=============================================================================
