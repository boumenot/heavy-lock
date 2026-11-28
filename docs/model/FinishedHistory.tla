-------------------------- MODULE FinishedHistory ---------------------------
EXTENDS Naturals, FiniteSets, Sequences

CONSTANTS Limit, MaxSeq, MaxFailures, MaxResnapshots, BugIgnoreFence
Jobs == {"a", "b", "c"}
Rank == [a |-> 3, b |-> 2, c |-> 1]
Top(s) == {j \in s : Cardinality({k \in s : Rank[k] > Rank[j]}) < Limit}

VARIABLES brokerJobs, everFinished, seq, log, worlds,
          cursor, history, historySeq, needRefresh,
          inFlight, reply, replySeq, failures, resnapshots
vars == <<brokerJobs, everFinished, seq, log, worlds,
          cursor, history, historySeq, needRefresh,
          inFlight, reply, replySeq, failures, resnapshots>>

Init ==
  /\ brokerJobs = {}
  /\ everFinished = {}
  /\ seq = 0
  /\ log = <<>>
  /\ worlds = [n \in 0..MaxSeq |-> {}]
  /\ cursor = 0
  /\ history = {}
  /\ historySeq = 0
  /\ needRefresh = TRUE
  /\ inFlight = FALSE
  /\ reply = {}
  /\ replySeq = 0
  /\ failures = 0
  /\ resnapshots = 0

TypeOK ==
  /\ brokerJobs \subseteq Jobs
  /\ everFinished \subseteq Jobs
  /\ seq \in 0..MaxSeq
  /\ log \in Seq([kind: {"finished", "deleted"}, id: Jobs])
  /\ Len(log) = seq
  /\ worlds \in [0..MaxSeq -> SUBSET Jobs]
  /\ cursor \in 0..seq
  /\ history \subseteq Jobs
  /\ historySeq \in 0..seq
  /\ needRefresh \in BOOLEAN
  /\ inFlight \in BOOLEAN
  /\ reply \subseteq Jobs
  /\ replySeq \in 0..seq
  /\ failures \in 0..MaxFailures
  /\ resnapshots \in 0..MaxResnapshots

Finish(j) ==
  /\ j \in Jobs \ everFinished /\ seq < MaxSeq
  /\ brokerJobs' = brokerJobs \cup {j}
  /\ everFinished' = everFinished \cup {j}
  /\ log' = Append(log, [kind |-> "finished", id |-> j])
  /\ worlds' = [worlds EXCEPT ![seq + 1] = brokerJobs \cup {j}]
  /\ seq' = seq + 1
  /\ UNCHANGED <<cursor, history, historySeq, needRefresh,
                 inFlight, reply, replySeq, failures, resnapshots>>

Delete(j) ==
  /\ j \in brokerJobs /\ seq < MaxSeq
  /\ brokerJobs' = brokerJobs \ {j}
  /\ log' = Append(log, [kind |-> "deleted", id |-> j])
  /\ worlds' = [worlds EXCEPT ![seq + 1] = brokerJobs \ {j}]
  /\ seq' = seq + 1
  /\ UNCHANGED <<everFinished, cursor, history, historySeq, needRefresh,
                 inFlight, reply, replySeq, failures, resnapshots>>

\* SQLite reads the selected jobs and event sequence in one transaction.
StartRead ==
  /\ needRefresh /\ ~inFlight
  /\ inFlight' = TRUE
  /\ reply' = Top(brokerJobs)
  /\ replySeq' = seq
  /\ UNCHANGED <<brokerJobs, everFinished, seq, log, worlds, cursor,
                 history, historySeq, needRefresh, failures, resnapshots>>

\* The RPC response may be delayed while more broker changes commit.
FinishRead ==
  /\ inFlight
  /\ history' = reply
  /\ historySeq' = replySeq
  /\ needRefresh' = FALSE
  /\ inFlight' = FALSE
  /\ UNCHANGED <<brokerJobs, everFinished, seq, log, worlds, cursor,
                 reply, replySeq, failures, resnapshots>>

FailRead ==
  /\ inFlight /\ failures < MaxFailures
  /\ inFlight' = FALSE
  /\ failures' = failures + 1
  /\ UNCHANGED <<brokerJobs, everFinished, seq, log, worlds, cursor,
                 history, historySeq, needRefresh, reply, replySeq, resnapshots>>

\* Stream delivery is serial with the history request. A deletion of a
\* displayed job invalidates the bounded projection until it is refilled.
Deliver ==
  /\ ~inFlight /\ cursor < seq
  /\ LET event == log[cursor + 1]
         covered == needRefresh \/ (~BugIgnoreFence /\ cursor + 1 <= historySeq)
     IN
       /\ history' =
            IF covered THEN history
            ELSE IF event.kind = "finished" THEN Top(history \cup {event.id})
            ELSE history \ {event.id}
       /\ needRefresh' =
            IF ~covered /\ event.kind = "deleted" /\ event.id \in history
            THEN TRUE ELSE needRefresh
  /\ cursor' = cursor + 1
  /\ UNCHANGED <<brokerJobs, everFinished, seq, log, worlds, historySeq,
                 inFlight, reply, replySeq, failures, resnapshots>>

\* A gap/replay expiry replaces the stream position with a fresh snapshot.
Resnapshot ==
  /\ ~inFlight /\ resnapshots < MaxResnapshots
  /\ cursor' = seq
  /\ needRefresh' = TRUE
  /\ resnapshots' = resnapshots + 1
  /\ UNCHANGED <<brokerJobs, everFinished, seq, log, worlds, history,
                 historySeq, inFlight, reply, replySeq, failures>>

Idle == UNCHANGED vars
Next ==
  \/ \E j \in Jobs : Finish(j)
  \/ \E j \in Jobs : Delete(j)
  \/ StartRead \/ FinishRead \/ FailRead \/ Deliver \/ Resnapshot \/ Idle

Spec == Init /\ [][Next]_vars
FairSpec == Spec /\ WF_vars(StartRead) /\ WF_vars(FinishRead) /\ WF_vars(Deliver)

BoundedHistory == Cardinality(history) <= Limit
CorrectProjection ==
  needRefresh \/ inFlight
    \/ history = Top(worlds[IF historySeq > cursor THEN historySeq ELSE cursor])
Visible == ~needRefresh /\ ~inFlight /\ cursor >= historySeq
VisibleMatchesSequence == ~Visible \/ history = Top(worlds[cursor])
EventuallySynchronized == <>[](Visible /\ cursor = seq)
NeverAhead == ~(~needRefresh /\ historySeq > cursor)
=============================================================================
