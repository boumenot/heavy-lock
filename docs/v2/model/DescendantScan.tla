---------------------------- MODULE DescendantScan ----------------------------
EXTENDS Naturals

CONSTANTS MaxCrashes, MaxScanFailures, MaxStorageFailures,
          AllowReleaseDuringScan, AllowUnstableTracking

VARIABLES held, rootAlive, childAlive, rootVerified, childVerified,
          known, stableKnown, scanState, scanReady, broker,
          crashes, scanFailures, storageFailures, crashedKnown

vars == <<held, rootAlive, childAlive, rootVerified, childVerified,
          known, stableKnown, scanState, scanReady, broker,
          crashes, scanFailures, storageFailures, crashedKnown>>

Init ==
  /\ held = TRUE
  /\ rootAlive = TRUE /\ childAlive = TRUE
  /\ rootVerified = FALSE /\ childVerified = FALSE
  /\ known = FALSE /\ stableKnown = FALSE
  /\ scanState = "idle" /\ scanReady = FALSE
  /\ broker = "up"
  /\ crashes = 0 /\ scanFailures = 0 /\ storageFailures = 0
  /\ crashedKnown = FALSE

StartScan ==
  /\ broker = "up" /\ held /\ scanState = "idle"
  /\ scanState' = "open"
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, childVerified,
                 known, stableKnown, scanReady, broker, crashes,
                 scanFailures, storageFailures, crashedKnown>>

\* A successful snapshot may omit an otherwise live child. Only a stable
\* matching root or previously known child can anchor positive discovery.
CaptureStable ==
  /\ broker = "up" /\ scanState = "open"
  /\ childAlive /\ (rootAlive \/ known)
  /\ scanState' = "observed"
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, childVerified,
                 known, stableKnown, scanReady, broker, crashes,
                 scanFailures, storageFailures, crashedKnown>>

CaptureUnstable ==
  /\ broker = "up" /\ scanState = "open" /\ childAlive
  /\ scanState' = "unstable"
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, childVerified,
                 known, stableKnown, scanReady, broker, crashes,
                 scanFailures, storageFailures, crashedKnown>>

CaptureOmission ==
  /\ broker = "up" /\ scanState = "open"
  /\ scanState' = "omitted"
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, childVerified,
                 known, stableKnown, scanReady, broker, crashes,
                 scanFailures, storageFailures, crashedKnown>>

SnapshotStep == CaptureStable \/ CaptureUnstable \/ CaptureOmission

\* The scan barrier remains held until every positive identity is durable.
FinishScan ==
  /\ broker = "up" /\ scanState \in {"observed", "omitted"}
  /\ known' = (known \/ scanState = "observed")
  /\ stableKnown' = (stableKnown \/ scanState = "observed")
  /\ scanReady' = TRUE /\ scanState' = "idle"
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, childVerified,
                 broker, crashes, scanFailures, storageFailures, crashedKnown>>

RejectUnstable ==
  /\ broker = "up" /\ scanState = "unstable"
  /\ known' = (known \/ AllowUnstableTracking)
  /\ scanReady' = FALSE /\ scanState' = "idle"
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, childVerified,
                 stableKnown, broker, crashes, scanFailures, storageFailures,
                 crashedKnown>>

FailScan ==
  /\ broker = "up" /\ scanState = "open"
  /\ scanFailures < MaxScanFailures
  /\ scanFailures' = scanFailures + 1
  /\ scanReady' = FALSE /\ scanState' = "idle"
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, childVerified,
                 known, stableKnown, broker, crashes, storageFailures,
                 crashedKnown>>

ExitRoot ==
  /\ rootAlive /\ rootAlive' = FALSE
  /\ UNCHANGED <<held, childAlive, rootVerified, childVerified, known,
                 stableKnown, scanState, scanReady, broker, crashes,
                 scanFailures, storageFailures, crashedKnown>>

ExitChild ==
  /\ childAlive /\ childAlive' = FALSE
  /\ UNCHANGED <<held, rootAlive, rootVerified, childVerified, known,
                 stableKnown, scanState, scanReady, broker, crashes,
                 scanFailures, storageFailures, crashedKnown>>

VerifyRoot ==
  /\ broker = "up" /\ ~rootAlive /\ ~rootVerified
  /\ rootVerified' = TRUE
  /\ UNCHANGED <<held, rootAlive, childAlive, childVerified, known,
                 stableKnown, scanState, scanReady, broker, crashes,
                 scanFailures, storageFailures, crashedKnown>>

VerifyChild ==
  /\ broker = "up" /\ known /\ ~childAlive /\ ~childVerified
  /\ childVerified' = TRUE
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, known,
                 stableKnown, scanState, scanReady, broker, crashes,
                 scanFailures, storageFailures, crashedKnown>>

\* The pre-existing launch fence and outcome checks are assumed satisfied.
Release ==
  /\ broker = "up" /\ held /\ scanReady /\ rootVerified
  /\ (~known \/ childVerified)
  /\ (scanState = "idle" \/ AllowReleaseDuringScan)
  /\ held' = FALSE
  /\ UNCHANGED <<rootAlive, childAlive, rootVerified, childVerified, known,
                 stableKnown, scanState, scanReady, broker, crashes,
                 scanFailures, storageFailures, crashedKnown>>

Crash ==
  /\ broker # "down" /\ held /\ crashes < MaxCrashes
  /\ broker' = "down" /\ crashes' = crashes + 1
  /\ crashedKnown' = (crashedKnown \/ known)
  /\ scanState' = "idle" /\ scanReady' = FALSE
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, childVerified,
                 known, stableKnown, scanFailures, storageFailures>>

Restart ==
  /\ broker = "down" /\ broker' = "up"
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, childVerified,
                 known, stableKnown, scanState, scanReady, crashes,
                 scanFailures, storageFailures, crashedKnown>>

StorageFailure ==
  /\ broker = "up" /\ held /\ storageFailures < MaxStorageFailures
  /\ broker' = "storage_failed"
  /\ storageFailures' = storageFailures + 1
  /\ scanReady' = FALSE
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, childVerified,
                 known, stableKnown, scanState, crashes, scanFailures,
                 crashedKnown>>

\* Storage failure is sticky: reopening is a fresh broker run, losing
\* uncommitted observations but preserving committed identities.
ReopenStorage ==
  /\ broker = "storage_failed" /\ broker' = "up"
  /\ scanState' = "idle" /\ scanReady' = FALSE
  /\ UNCHANGED <<held, rootAlive, childAlive, rootVerified, childVerified,
                 known, stableKnown, crashes, scanFailures, storageFailures,
                 crashedKnown>>

Next ==
  \/ StartScan \/ SnapshotStep \/ FinishScan \/ RejectUnstable \/ FailScan
  \/ ExitRoot \/ ExitChild \/ VerifyRoot \/ VerifyChild \/ Release
  \/ Crash \/ Restart \/ StorageFailure \/ ReopenStorage

Spec == Init /\ [][Next]_vars

FairSpec ==
  /\ Spec
  /\ WF_vars(Restart) /\ WF_vars(ReopenStorage)
  /\ WF_vars(StartScan) /\ WF_vars(SnapshotStep)
  /\ WF_vars(FinishScan) /\ WF_vars(RejectUnstable)
  /\ WF_vars(VerifyRoot) /\ WF_vars(VerifyChild)
  /\ SF_vars(Release)

TypeOK ==
  /\ held \in BOOLEAN /\ rootAlive \in BOOLEAN /\ childAlive \in BOOLEAN
  /\ rootVerified \in BOOLEAN /\ childVerified \in BOOLEAN
  /\ known \in BOOLEAN /\ stableKnown \in BOOLEAN
  /\ scanState \in {"idle", "open", "observed", "unstable", "omitted"}
  /\ scanReady \in BOOLEAN /\ broker \in {"up", "down", "storage_failed"}
  /\ crashes \in 0..MaxCrashes /\ scanFailures \in 0..MaxScanFailures
  /\ storageFailures \in 0..MaxStorageFailures /\ crashedKnown \in BOOLEAN

NoKnownLiveUnreserved == known /\ childAlive => held
NoCapturedLiveUnreserved == scanState = "observed" /\ childAlive => held
NoReleaseWithoutSuccessfulScan == ~held => scanReady
NoUnstableDescendants == known => stableKnown
CrashPreservesKnown == crashedKnown => known
ExitEvidenceSound == (rootVerified => ~rootAlive) /\ (childVerified => ~childAlive)

EventuallyRelease ==
  []((held /\ ~rootAlive /\ ~childAlive) => <>~held)

\* These diagnostics are deliberately false when the respective state is
\* reachable; the check wrapper expects a counterexample for each.
NeverStableCaptured == ~(scanState = "observed")
NeverRestartWithKnown == ~(crashedKnown /\ broker = "up")
NeverScanFailed == ~(scanFailures > 0)
=============================================================================
