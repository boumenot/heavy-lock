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
Fences == {"fresh", "intent", "sealed"}
ReportKinds == {"started", "finished", "aborted"}
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
CanSpawn(j) == runner[j].up /\ runner[j].grant
              /\ runner[j].ticket
              /\ ~runner[j].cancel /\ launches[j] < 2

Init ==
  /\ db = [j \in Jobs |-> [phase |-> "absent", held |-> FALSE,
                           issued |-> FALSE, forced |-> FALSE,
                           cancel |-> FALSE, started |-> FALSE]]
  /\ queue = <<>>
  /\ runner = [j \in Jobs |-> [up |-> TRUE, connected |-> TRUE,
                               grant |-> FALSE, cancel |-> FALSE,
                               fence |-> "fresh", ticket |-> FALSE,
                               identity |-> FALSE]]
  /\ roots = {}
  /\ children = {}
  /\ tracked = {}
  /\ launches = [j \in Jobs |-> 0]
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
                    /\ Occupied = 0 \/ resourcePass
  /\ db' = [db EXCEPT ![j].phase = "reserved", ![j].held = TRUE,
                     ![j].issued = TRUE, ![j].forced = forced]
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

\* Intent and OS spawn deliberately are different actions. Persisting intent
\* can prevent replay, but cannot establish whether the OS actually spawned.
BeginLaunch(j) ==
  /\ runner[j].up /\ runner[j].grant /\ ~runner[j].cancel
  /\ runner[j].fence = "fresh"
  /\ runner' = [runner EXCEPT ![j].fence = "intent", ![j].ticket = TRUE]
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

Spawn(j) ==
  /\ CanSpawn(j)
  /\ runner' = [runner EXCEPT ![j].ticket = FALSE, ![j].identity = TRUE]
  /\ roots' = roots \cup {j}
  /\ launches' = [launches EXCEPT ![j] = @ + 1]
  /\ UNCHANGED <<db, queue, children, tracked, grants, reports, broker,
                 paused, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

Fork(j) ==
  /\ j \in roots /\ j \notin children
  /\ children' = children \cup {j}
  /\ UNCHANGED <<db, queue, runner, roots, tracked, launches, grants, reports,
                 broker, paused, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

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
  /\ CASE kind = "started" -> launches[j] > 0
                                /\ runner[j].identity
       [] kind = "finished" -> runner[j].identity /\ j \notin roots
       [] kind = "aborted" -> runner[j].fence = "sealed"
  /\ reports' = reports \cup {[job |-> j, kind |-> kind]}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

ReceiveStarted(j) ==
  /\ Callable /\ [job |-> j, kind |-> "started"] \in reports
  /\ db[j].phase = "reserved"
  /\ db' = [db EXCEPT ![j].phase = "running", ![j].started = TRUE]
  /\ tracked' = tracked \cup {j}
  /\ absent' = absent \ {j}
  /\ UNCHANGED <<queue, runner, roots, children, launches, grants, reports,
                 broker, paused, reconciled, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

ReceiveFinished(j) ==
  /\ Callable /\ [job |-> j, kind |-> "finished"] \in reports
  /\ db[j].phase = "running"
  /\ db' = [db EXCEPT ![j].phase = "finishing"]
  /\ UNCHANGED <<queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

ReceiveAborted(j) ==
  /\ Callable /\ [job |-> j, kind |-> "aborted"] \in reports
  /\ db[j].phase = "reserved"
  /\ db' = [db EXCEPT ![j].phase = "finished", ![j].held = FALSE]
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
             ![j].phase = IF @ = "queued" THEN "finished" ELSE @]
  /\ queue' = Remove(queue, j)
  /\ UNCHANGED <<runner, roots, children, tracked, launches, grants, reports,
                 broker, paused, reconciled, absent, probes, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

\* Timeout triggers investigation, never release. Sealing is an explicit
\* runner acknowledgement that this grant can no longer cause a launch.
ProbeTimeout(j) ==
  /\ broker = "ready" /\ db[j].phase = "reserved"
  /\ probes' = probes \cup {j}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, brokerCrashes,
                 runnerCrashes, disconnects, grace, normalOK, recoveryOK>>

SealLaunch(j) ==
  /\ runner[j].up /\ runner[j].grant
  /\ runner[j].fence \in {"fresh", "intent"}
  /\ runner[j].fence = "fresh" \/ runner[j].ticket
  /\ runner[j].cancel \/ j \in probes
  /\ runner' = [runner EXCEPT ![j].fence = "sealed", ![j].ticket = FALSE]
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

ObserveAbsent(j) ==
  /\ broker \in {"ready", "recovering"} /\ db[j].held
  /\ IF SafeAbsence
        THEN /\ runner[j].fence = "sealed"
                \/ (j \in tracked /\ ~runner[j].ticket)
             /\ j \notin roots
        ELSE j \notin roots
  /\ CompleteTracking => j \notin children
  /\ absent' = absent \cup {j}
  /\ UNCHANGED <<db, queue, runner, roots, children, tracked, launches,
                 grants, reports, broker, paused, reconciled, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

Release(j) ==
  /\ broker \in {"ready", "recovering"} /\ db[j].held /\ j \in absent
  /\ db' = [db EXCEPT ![j].phase = "finished", ![j].held = FALSE]
  /\ UNCHANGED <<queue, runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

BrokerCrash ==
  /\ broker # "down" /\ brokerCrashes < MaxBrokerCrashes
  /\ broker' = "down" /\ brokerCrashes' = brokerCrashes + 1
  /\ runner' = [j \in Jobs |-> [runner[j] EXCEPT !.connected = FALSE]]
  /\ reconciled' = {} /\ absent' = {}
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, paused, probes, runnerCrashes, disconnects,
                 grace, normalOK, recoveryOK>>

BrokerRestart ==
  /\ broker = "down" /\ broker' = "recovering"
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
             ![j].fence = IF DurableFence THEN @ ELSE "fresh"]
  /\ runnerCrashes' = runnerCrashes + 1
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, disconnects, grace, normalOK, recoveryOK>>

RunnerRestart(j) ==
  /\ ~runner[j].up
  /\ runner' = [runner EXCEPT ![j].up = TRUE]
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
  /\ broker # "down" /\ runner[j].up /\ ~runner[j].connected
  /\ runner' = [runner EXCEPT ![j].connected = TRUE]
  /\ UNCHANGED <<db, queue, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
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
  /\ db' = [db EXCEPT ![j].phase = "finished"]
  /\ queue' = Remove(queue, j)
  /\ UNCHANGED <<runner, roots, children, tracked, launches, grants,
                 reports, broker, paused, reconciled, absent, probes,
                 brokerCrashes, runnerCrashes, disconnects, grace,
                 normalOK, recoveryOK>>

Next ==
  \/ Pause \/ BrokerCrash \/ BrokerRestart \/ RecoveryDone
  \/ \E m \in grants : DeliverGrant(m) \/ DropGrant(m)
  \/ \E m \in reports : DropReport(m)
  \/ \E j \in Jobs :
       \/ Submit(j)
       \/ \E forced, resourcePass \in BOOLEAN : Reserve(j, forced, resourcePass)
       \/ Move(j, TRUE) \/ Move(j, FALSE) \/ SendGrant(j)
       \/ BeginLaunch(j) \/ Spawn(j) \/ Fork(j) \/ ExitRoot(j) \/ ExitChild(j)
       \/ \E kind \in ReportKinds : SendReport(j, kind)
       \/ ReceiveStarted(j) \/ ReceiveFinished(j) \/ ReceiveAborted(j)
       \/ Cancel(j) \/ ProbeTimeout(j) \/ SealLaunch(j)
       \/ ObserveAbsent(j) \/ Release(j) \/ Reconcile(j)
       \/ RunnerCrash(j) \/ RunnerRestart(j) \/ Disconnect(j) \/ Reconnect(j)
       \/ TickGrace(j) \/ ExpireGrace(j)

Spec == Init /\ [][Next]_vars
SchedulingNext ==
  \/ Pause
  \/ \E j \in Jobs :
       \/ Submit(j) \/ Move(j, TRUE) \/ Move(j, FALSE) \/ Cancel(j)
       \/ \E forced, resourcePass \in BOOLEAN : Reserve(j, forced, resourcePass)
SchedulingSpec == Init /\ [][SchedulingNext]_vars

TypeOK ==
  /\ db \in [Jobs -> [phase : Phases, held : BOOLEAN, issued : BOOLEAN,
                       forced : BOOLEAN, cancel : BOOLEAN, started : BOOLEAN]]
  /\ runner \in [Jobs -> [up : BOOLEAN, connected : BOOLEAN, grant : BOOLEAN,
                           cancel : BOOLEAN, fence : Fences, ticket : BOOLEAN,
                           identity : BOOLEAN]]
  /\ queue \in Seq(Jobs) /\ Len(queue) <= Cardinality(Jobs)
  /\ roots \subseteq Jobs /\ children \subseteq Jobs /\ tracked \subseteq Jobs
  /\ launches \in [Jobs -> 0..2]
  /\ grants \subseteq [job : Jobs, cancel : BOOLEAN]
  /\ reports \subseteq [job : Jobs, kind : ReportKinds]
  /\ broker \in {"ready", "down", "recovering"} /\ paused \in BOOLEAN
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
NoDuplicateLaunch == \A j \in Jobs : launches[j] <= 1
NoLiveUnreserved == \A j \in Jobs : Live(j) => db[j].held
NoPrematureRelease ==
  \A j \in Jobs : (db[j].issued /\ ~db[j].held) =>
                   (~Live(j) /\ runner[j].fence # "fresh" /\ ~runner[j].ticket)
NormalCapacity == Slots({j \in Active : ~db[j].forced}) <= Capacity
NormalFIFO == normalOK
NoAdmissionDuringRecovery == recoveryOK
StartedBeforeFinishing ==
  \A j \in Jobs : db[j].phase = "finishing" => db[j].started
NeverForcedOverload == Occupied <= Capacity
NeverHeadBlocked ==
  ~(Len(queue) > 1
    /\ Occupied + Weight[Head(queue)] > Capacity
    /\ \E i \in 2..Len(queue) : Occupied + Weight[queue[i]] <= Capacity)
RecoveryAcceptsPendingIdentity ==
  \A j \in Jobs :
    (broker = "recovering" /\ db[j].phase = "reserved"
     /\ Live(j) /\ j \notin tracked
     /\ [job |-> j, kind |-> "started"] \in reports) => RecoveryRPCs

=============================================================================
