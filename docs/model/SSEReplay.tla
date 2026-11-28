---------------------------- MODULE SSEReplay ----------------------------
EXTENDS Naturals

CONSTANTS MaxSeq, MaxEpoch, BugIgnoreEpoch, EnableFaults, StartSynced
VARIABLES epoch, head, floor, up, phase, cursor, cursorEpoch, clientSeq,
          clientEpoch, synced, pending, overflow
vars == <<epoch, head, floor, up, phase, cursor, cursorEpoch, clientSeq,
          clientEpoch, synced, pending, overflow>>

Init ==
  /\ epoch = 0 /\ head = 1 /\ floor = 0 /\ up = TRUE
  /\ phase = "disconnected" /\ cursor = 0 /\ cursorEpoch = 0
  /\ clientSeq = IF StartSynced THEN 1 ELSE 0
  /\ clientEpoch = 0 /\ synced = StartSynced
  /\ pending = 0 /\ overflow = FALSE

TypeOK ==
  /\ epoch \in 0..MaxEpoch /\ head \in 1..MaxSeq /\ floor \in 0..MaxSeq
  /\ floor <= head /\ up \in BOOLEAN
  /\ phase \in {"disconnected", "snapshot", "replay", "resuming", "tail", "failed"}
  /\ cursor \in 0..MaxSeq /\ clientSeq \in 0..MaxSeq
  /\ cursorEpoch \in 0..MaxEpoch /\ clientEpoch \in 0..MaxEpoch
  /\ synced \in BOOLEAN /\ pending \in 0..MaxSeq /\ overflow \in BOOLEAN

StartSnapshot ==
  /\ up /\ phase \in {"disconnected", "failed"}
  /\ phase' = "snapshot" /\ pending' = 0 /\ overflow' = FALSE
  /\ UNCHANGED <<epoch, head, floor, up, cursor, cursorEpoch, clientSeq,
                  clientEpoch, synced>>

StartResume ==
  /\ up /\ phase = "disconnected" /\ clientSeq > 0
  /\ (clientEpoch = epoch \/ BugIgnoreEpoch)
  /\ clientSeq >= floor /\ clientSeq <= head
  /\ phase' = "resuming" /\ cursor' = clientSeq /\ cursorEpoch' = epoch
  /\ pending' = 0 /\ overflow' = FALSE
  /\ UNCHANGED <<epoch, head, floor, up, clientSeq, clientEpoch, synced>>

SendSnapshot ==
  /\ up /\ phase = "snapshot" /\ pending = 0
  /\ cursor' = head /\ cursorEpoch' = epoch /\ clientSeq' = head
  /\ clientEpoch' = epoch /\ synced' = TRUE /\ phase' = "replay"
  /\ UNCHANGED <<epoch, head, floor, up, pending, overflow>>

SendNext ==
  /\ up /\ phase \in {"replay", "resuming", "tail"} /\ pending = 0 /\ ~overflow
  /\ cursorEpoch = epoch /\ cursor < head /\ cursor >= floor
  /\ cursor' = cursor + 1 /\ pending' = cursor + 1
  /\ UNCHANGED <<epoch, head, floor, up, phase, cursorEpoch, clientSeq,
                  clientEpoch, synced, overflow>>

Deliver ==
  /\ pending > 0 /\ up /\ cursorEpoch = epoch
  /\ clientSeq' = pending /\ clientEpoch' = epoch
  /\ pending' = 0
  /\ UNCHANGED <<epoch, head, floor, up, phase, cursor, cursorEpoch, synced, overflow>>

FinishReplay ==
  /\ up /\ phase \in {"replay", "resuming"} /\ pending = 0 /\ cursor = head
  /\ cursorEpoch = epoch /\ cursor >= floor /\ ~overflow
  /\ phase' = "tail"
  /\ UNCHANGED <<epoch, head, floor, up, cursor, cursorEpoch, clientSeq,
                  clientEpoch, synced, pending, overflow>>

Heartbeat ==
  /\ up /\ phase = "tail" /\ pending = 0 /\ ~overflow
  /\ UNCHANGED vars

Publish ==
  /\ up /\ head < MaxSeq
  /\ head' = head + 1
  /\ UNCHANGED <<epoch, floor, up, phase, cursor, cursorEpoch, clientSeq,
                  clientEpoch, synced, pending, overflow>>

Prune ==
  /\ EnableFaults /\ up /\ floor < head
  /\ floor' = head
  /\ UNCHANGED <<epoch, head, up, phase, cursor, cursorEpoch, clientSeq,
                  clientEpoch, synced, pending, overflow>>

Fail ==
  /\ phase \in {"snapshot", "replay", "resuming", "tail"}
  /\ (~up \/ cursorEpoch # epoch \/ overflow \/
      (phase \in {"replay", "resuming"} /\ cursor < floor))
  /\ phase' = "failed" /\ pending' = 0
  /\ UNCHANGED <<epoch, head, floor, up, cursor, cursorEpoch, clientSeq,
                  clientEpoch, synced, overflow>>

Overflow ==
  /\ EnableFaults /\ phase \in {"snapshot", "replay", "resuming", "tail"} /\ ~overflow
  /\ overflow' = TRUE
  /\ UNCHANGED <<epoch, head, floor, up, phase, cursor, cursorEpoch, clientSeq,
                  clientEpoch, synced, pending>>

Disconnect ==
  /\ EnableFaults /\ phase \in {"snapshot", "replay", "resuming", "tail", "failed"}
  /\ phase' = "disconnected" /\ pending' = 0
  /\ UNCHANGED <<epoch, head, floor, up, cursor, cursorEpoch, clientSeq,
                  clientEpoch, synced, overflow>>

Crash ==
  /\ EnableFaults /\ up /\ epoch < MaxEpoch
  /\ up' = FALSE
  /\ UNCHANGED <<epoch, head, floor, phase, cursor, cursorEpoch, clientSeq,
                  clientEpoch, synced, pending, overflow>>

Restart ==
  /\ ~up /\ epoch < MaxEpoch
  /\ up' = TRUE /\ epoch' = epoch + 1 /\ head' = 1 /\ floor' = 0
  /\ synced' = FALSE
  /\ UNCHANGED <<phase, cursor, cursorEpoch, clientSeq, clientEpoch,
                  pending, overflow>>

Next ==
  StartSnapshot \/ StartResume \/ SendSnapshot \/ SendNext \/ Deliver \/
  FinishReplay \/ Heartbeat \/ Publish \/ Prune \/ Fail \/ Overflow \/ Disconnect \/
  Crash \/ Restart
Spec == Init /\ [][Next]_vars
FairSpec ==
  /\ Spec
  /\ WF_vars(StartSnapshot) /\ WF_vars(StartResume) /\ WF_vars(SendSnapshot)
  /\ WF_vars(SendNext) /\ WF_vars(Deliver) /\ WF_vars(FinishReplay)
  /\ WF_vars(Fail) /\ WF_vars(Restart)

EpochFenced == (phase = "tail" /\ up /\ cursorEpoch = epoch) =>
                 (synced /\ clientEpoch = epoch)
CursorDelivered == (phase = "tail" /\ pending = 0) => clientSeq = cursor
NeverResume == phase # "resuming"
EventuallySynchronized == <>[] (phase = "tail" /\ clientSeq = head /\ pending = 0)
=============================================================================
