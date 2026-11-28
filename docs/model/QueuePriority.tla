---------------------------- MODULE QueuePriority ----------------------------
\* Queue-admission priority (#52). Waiting jobs are ordered by (priority,
\* queue_seq): a lower number goes first and equal priorities stay FIFO.
\* Priority never preempts a reserved or running job. Each action is one
\* serialized storage transaction of the real scheduler: submit, the no-wait
\* decision, one normal admission (scheduler.Decide), set-priority, reorder
\* (storage MoveBefore with its priority clamp and renumbering), cancel,
\* finish, runner disconnect/reconnect and broker crash/restart. The launch
\* pipeline, sharing checks, forced starts and oversized jobs are covered by
\* BrokerRunner.tla and abstracted away here. `prev` and `last` are history
\* variables so transition properties can be checked as invariants.
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS
  Jobs, Prios, Weight, Capacity, BackfillLimit, MaxCrashes,
  Variant   \* "correct" or a deliberate mutation checked by a negative case

ASSUME /\ Prios \subseteq Nat /\ Prios # {}
       /\ Capacity \in Nat \ {0} /\ BackfillLimit \in Nat /\ MaxCrashes \in Nat
       /\ \A j \in Jobs : Weight[j] \in 1..Capacity
       /\ Variant \in {"correct", "seq_only", "no_clamp", "nowait_append", "nowait_front"}

VARIABLES phase, prio, seq, nextSeq, bypass, connected, broker, crashes,
          prev, last

vars == <<phase, prio, seq, nextSeq, bypass, connected, broker, crashes,
          prev, last>>

None == "none"
Phases == {"absent", "queued", "held", "done", "busy"}
Acts == {"init", "submit", "nowait", "admit", "priority", "move", "cancel",
         "finish", "disconnect", "reconnect", "crash", "restart"}
MaxSeq == Cardinality(Jobs) + 1
MixedWeights == [j \in Jobs |-> IF j = CHOOSE k \in Jobs : TRUE THEN 2 ELSE 1]
Default == CHOOSE p \in Prios : \A q \in Prios : q <= p

Queued == {j \in Jobs : phase[j] = "queued"}
Held == {j \in Jobs : phase[j] = "held"}
RECURSIVE Slots(_)
Slots(S) == IF S = {} THEN 0
            ELSE LET j == CHOOSE k \in S : TRUE IN Weight[j] + Slots(S \ {j})
Free == Capacity - Slots(Held)
Min(S) == CHOOSE i \in S : \A k \in S : i <= k

\* The specified effective order, used by every property.
Prec(pk, sk, pj, sj) == pk < pj \/ (pk = pj /\ sk < sj)
SPrec(k, j) == Prec(prio[k], seq[k], prio[j], seq[j])
\* The order the modeled implementation uses (seq_only ignores priority).
IPrec(k, j) == IF Variant = "seq_only" THEN seq[k] < seq[j] ELSE SPrec(k, j)
Order == LET n == Cardinality(Queued)
             Rank(j) == Cardinality({k \in Queued : IPrec(k, j)}) + 1
         IN [i \in 1..n |-> CHOOSE j \in Queued : Rank(j) = i]
Insert(s, i, j) == SubSeq(s, 1, i) \o <<j>> \o SubSeq(s, i + 1, Len(s))
IndexOf(s, j) == CHOOSE i \in 1..Len(s) : s[i] = j

NoDecision == [admit |-> None, bf |-> None]
\* scheduler.Decide without oversized jobs or sharing checks: a disconnected
\* head blocks; a capacity-blocked head below its bypass limit lets the
\* earliest connected fitting follower in.
Decide(o, conn) ==
  IF Len(o) = 0 THEN NoDecision
  ELSE LET h == o[1] IN
    IF ~conn[h] THEN NoDecision
    ELSE IF Weight[h] <= Free THEN [admit |-> h, bf |-> None]
    ELSE IF bypass[h] >= BackfillLimit THEN NoDecision
    ELSE LET el == {i \in 2..Len(o) : conn[o[i]] /\ Weight[o[i]] <= Free} IN
         IF el = {} THEN NoDecision ELSE [admit |-> o[Min(el)], bf |-> h]
Bypassed(b) == IF b = None THEN bypass ELSE [bypass EXCEPT ![b] = @ + 1]

Snap == [phase |-> phase, prio |-> prio, seq |-> seq, bypass |-> bypass,
         connected |-> connected, broker |-> broker]
Step(act, j, t) == /\ prev' = Snap
                   /\ last' = [act |-> act, job |-> j, target |-> t]

TypeOK ==
  /\ phase \in [Jobs -> Phases] /\ prio \in [Jobs -> Prios]
  /\ seq \in [Jobs -> 0..MaxSeq] /\ nextSeq \in 1..MaxSeq
  /\ bypass \in [Jobs -> 0..BackfillLimit] /\ connected \in [Jobs -> BOOLEAN]
  /\ broker \in {"ready", "down"} /\ crashes \in 0..MaxCrashes
  /\ last.act \in Acts

Init ==
  /\ phase = [j \in Jobs |-> "absent"] /\ prio = [j \in Jobs |-> Default]
  /\ seq = [j \in Jobs |-> 0] /\ nextSeq = 1
  /\ bypass = [j \in Jobs |-> 0] /\ connected = [j \in Jobs |-> FALSE]
  /\ broker = "ready" /\ crashes = 0
  /\ prev = Snap /\ last = [act |-> "init", job |-> None, target |-> None]

-----------------------------------------------------------------------------
\* storage Submit (WAIT): append at the back of its priority level.
Submit(j, p) ==
  /\ broker = "ready" /\ phase[j] = "absent"
  /\ phase' = [phase EXCEPT ![j] = "queued"]
  /\ prio' = [prio EXCEPT ![j] = p]
  /\ seq' = [seq EXCEPT ![j] = nextSeq] /\ nextSeq' = nextSeq + 1
  /\ connected' = [connected EXCEPT ![j] = TRUE]
  /\ UNCHANGED <<bypass, broker, crashes>>
  /\ Step("submit", j, None)

\* TrySubmitAndWatch: one atomic decision with the connected candidate
\* inserted at the back of its priority level; a refusal is a durable busy
\* result with no queue entry.
NoWait(j, p) ==
  /\ broker = "ready" /\ phase[j] = "absent"
  /\ LET pos == CASE Variant = "nowait_append" -> Len(Order)
                  [] Variant = "nowait_front" -> Cardinality({k \in Queued : prio[k] < p})
                  [] OTHER -> Cardinality({k \in Queued : prio[k] <= p})
         d == Decide(Insert(Order, pos, j), [connected EXCEPT ![j] = TRUE])
     IN /\ phase' = [phase EXCEPT ![j] = IF d.admit = j THEN "held" ELSE "busy"]
        /\ bypass' = IF d.admit = j THEN Bypassed(d.bf) ELSE bypass
  /\ prio' = [prio EXCEPT ![j] = p]
  /\ UNCHANGED <<seq, nextSeq, connected, broker, crashes>>
  /\ Step("nowait", j, None)

\* One admitLocked transaction.
Admit ==
  /\ broker = "ready"
  /\ LET d == Decide(Order, connected) IN
     /\ d.admit # None
     /\ phase' = [phase EXCEPT ![d.admit] = "held"]
     /\ bypass' = Bypassed(d.bf)
     /\ Step("admit", d.admit, None)
  /\ UNCHANGED <<prio, seq, nextSeq, connected, broker, crashes>>

\* SetJobPriority: waiting jobs only; queue_seq and bypass count are kept.
SetPriority(j, p) ==
  /\ broker = "ready" /\ phase[j] = "queued" /\ p # prio[j]
  /\ prio' = [prio EXCEPT ![j] = p]
  /\ UNCHANGED <<phase, seq, nextSeq, bypass, connected, broker, crashes>>
  /\ Step("priority", j, None)

\* storage MoveBefore (t = "back" for the back). The job lands exactly where
\* requested; its priority is clamped to its new neighbours' and the queue is
\* renumbered in the new effective order. Front and after-X reorders are
\* MoveBefore calls with the corresponding target.
Move(j, t) ==
  /\ broker = "ready" /\ phase[j] = "queued" /\ t # j
  /\ t = "back" \/ t \in Queued
  /\ LET others == SelectSeq(Order, LAMBDA k : k # j)
         to == IF t = "back" THEN Len(others) ELSE IndexOf(others, t) - 1
         new == Insert(others, to, j)
         up == IF to >= 1 /\ prio[j] < prio[others[to]] THEN prio[others[to]] ELSE prio[j]
         p == IF to < Len(others) /\ up > prio[others[to + 1]] THEN prio[others[to + 1]] ELSE up
     IN /\ prio' = [prio EXCEPT ![j] = IF Variant = "no_clamp" THEN @ ELSE p]
        /\ seq' = [k \in Jobs |-> IF k \in Queued THEN IndexOf(new, k) ELSE seq[k]]
        /\ nextSeq' = Len(new) + 1
  /\ UNCHANGED <<phase, bypass, connected, broker, crashes>>
  /\ Step("move", j, t)

Cancel(j) ==
  /\ broker = "ready" /\ phase[j] = "queued"
  /\ phase' = [phase EXCEPT ![j] = "done"]
  /\ UNCHANGED <<prio, seq, nextSeq, bypass, connected, broker, crashes>>
  /\ Step("cancel", j, None)

\* Release after the job's processes have exited; the only way out of held.
Finish(j) ==
  /\ broker = "ready" /\ phase[j] = "held"
  /\ phase' = [phase EXCEPT ![j] = "done"]
  /\ UNCHANGED <<prio, seq, nextSeq, bypass, connected, broker, crashes>>
  /\ Step("finish", j, None)

Disconnect(j) ==
  /\ phase[j] = "queued" /\ connected[j]
  /\ connected' = [connected EXCEPT ![j] = FALSE]
  /\ UNCHANGED <<phase, prio, seq, nextSeq, bypass, broker, crashes>>
  /\ Step("disconnect", j, None)

Reconnect(j) ==
  /\ broker = "ready" /\ phase[j] = "queued" /\ ~connected[j]
  /\ connected' = [connected EXCEPT ![j] = TRUE]
  /\ UNCHANGED <<phase, prio, seq, nextSeq, bypass, broker, crashes>>
  /\ Step("reconnect", j, None)

\* Committed rows (state, priority, queue_seq, bypass) survive; runner
\* registrations are volatile.
Crash ==
  /\ broker = "ready" /\ crashes < MaxCrashes
  /\ broker' = "down" /\ crashes' = crashes + 1
  /\ connected' = [j \in Jobs |-> FALSE]
  /\ UNCHANGED <<phase, prio, seq, nextSeq, bypass>>
  /\ Step("crash", None, None)

Restart ==
  /\ broker = "down" /\ broker' = "ready"
  /\ UNCHANGED <<phase, prio, seq, nextSeq, bypass, connected, crashes>>
  /\ Step("restart", None, None)

Next ==
  \/ \E j \in Jobs, p \in Prios : Submit(j, p) \/ NoWait(j, p) \/ SetPriority(j, p)
  \/ Admit
  \/ \E j \in Jobs, t \in Jobs \cup {"back"} : Move(j, t)
  \/ \E j \in Jobs : Cancel(j) \/ Finish(j) \/ Disconnect(j) \/ Reconnect(j)
  \/ Crash \/ Restart

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
\* Properties over the step from `prev` to the current state.
PQueued == {j \in Jobs : prev.phase[j] = "queued"}
PHeld == {j \in Jobs : prev.phase[j] = "held"}
PFree == Capacity - Slots(PHeld)
PPrec(k, j) == Prec(prev.prio[k], prev.seq[k], prev.prio[j], prev.seq[j])
PHead == IF PQueued = {} THEN None
         ELSE CHOOSE h \in PQueued : \A k \in PQueued \ {h} : PPrec(h, k)
Admitted == {j \in Jobs : prev.phase[j] # "held" /\ phase[j] = "held"}
\* k was ahead of j in the specified order; a no-wait candidate sits at the
\* back of its priority level.
Ahead(k, j) == IF prev.phase[j] = "queued" THEN PPrec(k, j)
               ELSE prev.prio[k] <= prio[j]
\* Why a job ahead may be passed: it is a connected capacity-blocked head
\* under its bypass limit, or a follower that is disconnected or does not fit.
Passable(k) ==
  IF k = PHead
  THEN prev.connected[k] /\ Weight[k] > PFree /\ prev.bypass[k] < BackfillLimit
  ELSE ~(prev.connected[k] /\ Weight[k] <= PFree)
Admissible(j) == /\ prev.broker = "ready" /\ Weight[j] <= PFree
                 /\ \A k \in PQueued \ {j} : Ahead(k, j) => Passable(k)

CapacitySafe == Slots(Held) <= Capacity
BypassBound == \A j \in Jobs : bypass[j] <= BackfillLimit
SeqUnique == \A j, k \in Queued : j # k => seq[j] # seq[k]

\* Priority never preempts: a held job leaves only through its own release.
NoPreemption ==
  \A j \in PHeld : phase[j] = "held" \/ (last.act = "finish" /\ last.job = j)

\* Every admission takes the first admissible job in (priority, seq) order,
\* passing only jobs the backfill rules allow, and charges a bypass to the
\* head it passed; bypass counts change only that way.
OrderedAdmission ==
  /\ \A j \in Admitted :
       /\ Admissible(j)
       /\ \A k \in Jobs : bypass[k] # prev.bypass[k] =>
            k = PHead /\ k # j /\ Ahead(k, j) /\ bypass[k] = prev.bypass[k] + 1
  /\ Admitted = {} => bypass = prev.bypass

\* A no-wait candidate is admitted exactly when it would be the next normal
\* admission at the back of its priority level.
NoWaitExact ==
  last.act = "nowait" => (phase[last.job] = "held" <=> Admissible(last.job))

\* A submission joins the back of its priority level.
SubmitPlacement ==
  last.act = "submit" =>
    \A k \in PQueued : SPrec(k, last.job) <=> prio[k] <= prio[last.job]

\* No step changes the relative order of two waiting jobs other than the one
\* it targets; with SubmitPlacement this gives FIFO within a priority level
\* for jobs that are never moved or reprioritized.
StableOthers ==
  \A k, m \in PQueued \cap Queued :
    k # last.job /\ m # last.job => (PPrec(k, m) <=> SPrec(k, m))

\* A reorder places the job exactly as requested and changes its priority
\* only when the requested position requires it, to a new neighbour's value.
MoveExact ==
  last.act = "move" =>
    LET j == last.job
        t == last.target
        Others == Queued \ {j}
        Placed == IF t = "back" THEN \A k \in Others : SPrec(k, j)
                  ELSE SPrec(j, t) /\ ~\E k \in Others : SPrec(j, k) /\ SPrec(k, t)
        Fits(k) == IF SPrec(k, j) THEN prio[k] <= prev.prio[j] ELSE prev.prio[j] <= prio[k]
        Kept == prio[j] = prev.prio[j]
    IN /\ Placed
       /\ (\A k \in Others : Fits(k)) => Kept
       /\ ~Kept => \E k \in Others : prio[k] = prio[j]

\* Recovery rebuilds admission order only from committed rows.
RestartPreserves ==
  last.act \in {"crash", "restart"} =>
    /\ phase = prev.phase /\ prio = prev.prio /\ seq = prev.seq
    /\ bypass = prev.bypass

-----------------------------------------------------------------------------
\* Reachability witnesses: each is expected to be violated.

\* A lower number is admitted ahead of an earlier-queued eligible job.
NeverPriorityOvertake ==
  ~\E j \in Admitted, k \in PQueued :
     /\ k # j /\ prev.prio[k] > prio[j]
     /\ prev.phase[j] = "absent" \/ prev.seq[k] < prev.seq[j]
     /\ prev.connected[k] /\ Weight[k] <= PFree

\* A lower-number job waits while a higher-number job keeps running.
NeverLowerWaitsBehindHeld ==
  ~\E j \in Held, k \in Queued : prio[k] < prio[j] /\ connected[k]

\* A reorder clamps the moved job's priority.
NeverClamp == ~(last.act = "move" /\ prio[last.job] # prev.prio[last.job])

\* After a restart, priority order still decides admission.
NeverOvertakeAfterRestart == crashes = 0 \/ NeverPriorityOvertake
=============================================================================
