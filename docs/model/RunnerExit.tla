----------------------------- MODULE RunnerExit -----------------------------
\* One claimed, sealed job whose root identity is recorded. It models the race
\* between the runner's finish report, the runner's own exit, descendant
\* discovery passes and the broker's reconciliation tick that decides runner
\* death takeover. Process probes are accurate; a probe error is abstracted
\* away (the code treats it as still needing cleanup).
EXTENDS Naturals

CONSTANTS
  OldTakeoverRule,   \* TRUE: record takeover whenever release is not yet possible
  NoBarrier,         \* TRUE: release without waiting for a committed discovery pass
  DiscoveryCanFail   \* TRUE: a discovery pass can fail and leave the barrier closed

VARIABLES runner, reported, unknown, root, child, tracked, rootVerified,
          childVerified, disc, captured, takeover, spurious, released, tick

vars == <<runner, reported, unknown, root, child, tracked, rootVerified,
          childVerified, disc, captured, takeover, spurious, released, tick>>

TypeOK ==
  /\ runner \in {"up", "exited"}
  /\ reported \in BOOLEAN /\ unknown \in BOOLEAN
  /\ root \in {"alive", "dead"}
  /\ child \in {"none", "alive", "dead"}
  /\ tracked \in BOOLEAN /\ rootVerified \in BOOLEAN /\ childVerified \in BOOLEAN
  /\ disc \in {"none", "missed", "inflight", "ready", "failed"}
  /\ captured \in BOOLEAN
  /\ takeover \in BOOLEAN /\ spurious \in BOOLEAN /\ released \in BOOLEAN
  /\ tick \in {"discover", "reconcile"}

\* ReportStarted has committed; no discovery pass has yet covered the root.
Init ==
  /\ runner = "up" /\ reported = FALSE /\ unknown = FALSE
  /\ root = "alive" /\ child = "none" /\ tracked = FALSE
  /\ rootVerified = FALSE /\ childVerified = FALSE
  /\ disc = "none" /\ captured = FALSE
  /\ takeover = FALSE /\ spurious = FALSE /\ released = FALSE
  /\ tick = "discover"

\* Probes inside one transaction: a verified exit is OS evidence (PID and start).
RootVerified1 == rootVerified \/ root = "dead"
ChildVerified1 == childVerified \/ (tracked /\ child = "dead")

\* Scheduler.reconcile: barrier, then tracked exits, then a matching outcome.
BarrierOK == NoBarrier \/ disc = "ready"
LiveTracked == root = "alive" \/ (tracked /\ child = "alive")

-----------------------------------------------------------------------------
\* Process environment.
SpawnChild ==
  /\ root = "alive" /\ child = "none"
  /\ child' = "alive"
  /\ UNCHANGED <<runner, reported, unknown, root, tracked, rootVerified,
                 childVerified, disc, captured, takeover, spurious, released, tick>>

RootExit ==
  /\ root = "alive" /\ root' = "dead"
  /\ UNCHANGED <<runner, reported, unknown, child, tracked, rootVerified,
                 childVerified, disc, captured, takeover, spurious, released, tick>>

ChildExit ==
  /\ child = "alive" /\ child' = "dead"
  /\ UNCHANGED <<runner, reported, unknown, root, tracked, rootVerified,
                 childVerified, disc, captured, takeover, spurious, released, tick>>

\* The runner exits normally after its finish report returns, or crashes at
\* any point. Neither is release evidence.
RunnerExit ==
  /\ runner = "up" /\ runner' = "exited"
  /\ UNCHANGED <<reported, unknown, root, child, tracked, rootVerified,
                 childVerified, disc, captured, takeover, spurious, released, tick>>

-----------------------------------------------------------------------------
\* runnerOp(ReportFinished): commit the outcome and, in the same
\* transaction, any release it makes provable.
ReportFinish ==
  /\ runner = "up" /\ root = "dead" /\ ~reported
  /\ reported' = TRUE
  /\ LET ok == BarrierOK /\ RootVerified1 /\ (tracked => ChildVerified1) IN
       /\ rootVerified' = IF BarrierOK THEN RootVerified1 ELSE rootVerified
       /\ childVerified' = IF BarrierOK THEN ChildVerified1 ELSE childVerified
       /\ released' = ok
  /\ UNCHANGED <<runner, unknown, root, child, tracked, disc, captured,
                 takeover, spurious, tick>>

\* Scheduler.Discover: the snapshot is taken outside the scheduler mutex and
\* sees a child reachable from a live recorded ancestor.
DiscoverStart ==
  /\ ~released /\ tick = "discover" /\ disc # "inflight"
  /\ disc' = "inflight"
  /\ captured' = (child = "alive" /\ (root = "alive" \/ tracked))
  /\ UNCHANGED <<runner, reported, unknown, root, child, tracked, rootVerified,
                 childVerified, takeover, spurious, released, tick>>

DiscoverCommit ==
  /\ disc = "inflight" /\ ~released
  /\ disc' = "ready" /\ tick' = "reconcile"
  /\ tracked' = (tracked \/ captured)
  /\ childVerified' = IF captured THEN FALSE ELSE childVerified
  /\ captured' = FALSE
  /\ UNCHANGED <<runner, reported, unknown, root, child, rootVerified,
                 takeover, spurious, released>>

\* A pass whose target list was read before ReportStarted committed does not
\* cover this root, so it leaves no discovery state for the job.
DiscoverMissed ==
  /\ ~released /\ tick = "discover" /\ disc = "none"
  /\ disc' = "missed" /\ tick' = "reconcile"
  /\ UNCHANGED <<runner, reported, unknown, root, child, tracked, rootVerified,
                 childVerified, captured, takeover, spurious, released>>

DiscoverFail ==
  /\ DiscoveryCanFail /\ disc = "inflight"
  /\ disc' = "failed" /\ captured' = FALSE /\ tick' = "reconcile"
  /\ UNCHANGED <<runner, reported, unknown, root, child, tracked, rootVerified,
                 childVerified, takeover, spurious, released>>

\* service.maintain runs Discover and then Reconcile in sequence each tick;
\* runner reports interleave anywhere. Scheduler.Reconcile for this job is one
\* transaction: verify runner death
\* (outcome unknown unless a report committed), try release and, if still
\* held, decide whether runner death needs a takeover.
Reconcile ==
  /\ ~released /\ tick = "reconcile" /\ tick' = "discover"
  /\ LET dead == runner = "exited"
         unk == unknown \/ (dead /\ ~reported)
         ok == /\ BarrierOK
                /\ RootVerified1 /\ (tracked => ChildVerified1)
                /\ (reported \/ unk)
         needs == IF OldTakeoverRule THEN TRUE ELSE LiveTracked
         probed == BarrierOK \/ (~OldTakeoverRule /\ dead /\ ~ok /\ ~takeover)
         record == dead /\ ~ok /\ ~takeover /\ needs
     IN /\ unknown' = unk
        /\ released' = ok
        \* reconcile probes past the barrier; the cleanup check probes too.
        /\ rootVerified' = IF probed THEN RootVerified1 ELSE rootVerified
        /\ childVerified' = IF probed THEN ChildVerified1 ELSE childVerified
        /\ takeover' = (takeover \/ record)
        /\ spurious' = (spurious \/ (record /\ ~LiveTracked))
  /\ UNCHANGED <<runner, reported, root, child, tracked, disc, captured>>

\* Scheduler.Terminate: fingerprint-checked kills of tracked, unverified
\* processes after a recorded takeover; never release evidence.
Terminate ==
  /\ takeover /\ ~released /\ LiveTracked
  /\ root' = "dead"
  /\ child' = IF tracked /\ child = "alive" THEN "dead" ELSE child
  /\ UNCHANGED <<runner, reported, unknown, tracked, rootVerified,
                 childVerified, disc, captured, takeover, spurious, released, tick>>

Next ==
  \/ SpawnChild \/ RootExit \/ ChildExit \/ RunnerExit \/ ReportFinish
  \/ DiscoverStart \/ DiscoverCommit \/ DiscoverMissed \/ DiscoverFail
  \/ Reconcile \/ Terminate

Spec == Init /\ [][Next]_vars

FairSpec == Spec /\ WF_vars(ReportFinish) /\ WF_vars(RunnerExit)
            /\ WF_vars(DiscoverStart) /\ WF_vars(DiscoverCommit)
            /\ WF_vars(Reconcile) /\ WF_vars(Terminate)

-----------------------------------------------------------------------------
\* Safety.
NoSpuriousTakeover == ~spurious
TakeoverNeedsDeadRunner == takeover => runner = "exited"
NoTrackedLiveUnreserved == released => root = "dead" /\ (tracked => child = "dead")
NoCapturedLiveUnreserved == released => ~(captured /\ child = "alive")
ReleaseNeedsOutcome == released => reported \/ unknown

\* Liveness under fairness.
CleanupEventually ==
  (runner = "exited" /\ tracked /\ child = "alive") ~> (takeover \/ child = "dead")
ReleaseEventually == (runner = "exited") ~> released

\* Reachability witnesses: each must be violated.
NeverCleanExitRelease == ~(released /\ reported /\ runner = "exited" /\ ~takeover)
NeverDeathTakeover == ~(takeover /\ tracked)
=============================================================================
