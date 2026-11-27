---------------------------- MODULE RunnerFinish ----------------------------
EXTENDS Naturals

CONSTANTS FinishBlocksLoop, DropEarlyFinal

VARIABLES finishPending, finishStored, replyHasCancel, cancelRequested,
          cancelHandled, ackPending, acknowledged, ackedEarly,
          finalPublished, finalHasCancel, finalDelivered, finalBuffered,
          sawEarlyFinal, runnerDone, retried

vars == <<finishPending, finishStored, replyHasCancel, cancelRequested,
          cancelHandled, ackPending, acknowledged, ackedEarly,
          finalPublished, finalHasCancel, finalDelivered, finalBuffered,
          sawEarlyFinal, runnerDone, retried>>

\* An eligible command has exited; its finish RPC is in flight.
Init ==
  /\ finishPending = TRUE /\ finishStored = FALSE /\ replyHasCancel = FALSE
  /\ cancelRequested = FALSE /\ cancelHandled = FALSE /\ ackPending = FALSE
  /\ acknowledged = FALSE /\ ackedEarly = FALSE /\ finalPublished = FALSE
  /\ finalHasCancel = FALSE /\ finalDelivered = FALSE /\ finalBuffered = FALSE
  /\ sawEarlyFinal = FALSE /\ runnerDone = FALSE /\ retried = FALSE

StoreFinish ==
  /\ ~finishStored
  /\ finishStored' = TRUE /\ replyHasCancel' = cancelRequested
  /\ UNCHANGED <<finishPending, cancelRequested, cancelHandled, ackPending,
                 acknowledged, ackedEarly, finalPublished, finalHasCancel,
                 finalDelivered, finalBuffered, sawEarlyFinal, runnerDone, retried>>

RequestCancel ==
  /\ ~cancelRequested /\ ~finalPublished
  /\ cancelRequested' = TRUE
  /\ UNCHANGED <<finishPending, finishStored, replyHasCancel, cancelHandled,
                 ackPending, acknowledged, ackedEarly, finalPublished,
                 finalHasCancel, finalDelivered, finalBuffered, sawEarlyFinal,
                 runnerDone, retried>>

\* The event loop handles cancellation independently of a pending finish RPC.
HandleCancel ==
  /\ cancelRequested /\ ~cancelHandled /\ ~runnerDone
  /\ (~finishPending \/ ~FinishBlocksLoop)
  /\ cancelHandled' = TRUE /\ ackPending' = TRUE
  /\ UNCHANGED <<finishPending, finishStored, replyHasCancel, cancelRequested,
                 acknowledged, ackedEarly, finalPublished, finalHasCancel,
                 finalDelivered, finalBuffered, sawEarlyFinal, runnerDone, retried>>

StoreAck ==
  /\ ackPending
  /\ ackPending' = FALSE /\ acknowledged' = TRUE
  /\ ackedEarly' = (ackedEarly \/ finishPending)
  /\ UNCHANGED <<finishPending, finishStored, replyHasCancel, cancelRequested,
                 cancelHandled, finalPublished, finalHasCancel, finalDelivered,
                 finalBuffered, sawEarlyFinal, runnerDone, retried>>

\* Release and its finished event may precede the finish RPC response.
PublishFinal ==
  /\ finishStored /\ ~finalPublished
  /\ finalPublished' = TRUE /\ finalHasCancel' = cancelRequested
  /\ UNCHANGED <<finishPending, finishStored, replyHasCancel, cancelRequested,
                 cancelHandled, ackPending, acknowledged, ackedEarly,
                 finalDelivered, finalBuffered, sawEarlyFinal, runnerDone, retried>>

HandleFinal ==
  /\ finalPublished /\ ~finalDelivered /\ ~runnerDone
  /\ (~finishPending \/ ~FinishBlocksLoop)
  /\ finalDelivered' = TRUE
  /\ finalBuffered' = (finishPending /\ ~DropEarlyFinal)
  /\ sawEarlyFinal' = (sawEarlyFinal \/ finishPending)
  /\ UNCHANGED <<finishPending, finishStored, replyHasCancel, cancelRequested,
                 cancelHandled, ackPending, acknowledged, ackedEarly,
                 finalPublished, finalHasCancel, runnerDone, retried>>

\* A finish reply may itself reveal an earlier cancellation.
HandleFinishReply ==
  /\ finishStored /\ finishPending /\ ~runnerDone
  /\ finishPending' = FALSE
  /\ IF replyHasCancel /\ ~cancelHandled
        THEN /\ cancelHandled' = TRUE /\ ackPending' = TRUE
        ELSE UNCHANGED <<cancelHandled, ackPending>>
  /\ UNCHANGED <<finishStored, replyHasCancel, cancelRequested, acknowledged,
                 ackedEarly, finalPublished, finalHasCancel, finalDelivered,
                 finalBuffered, sawEarlyFinal, runnerDone, retried>>

\* A final event enables a retry only if neither cancellation path forbids it.
\* Otherwise a timeout or non-retryable completion can return the native code.
Complete ==
  /\ ~finishPending /\ ~runnerDone /\ ~ackPending
  /\ runnerDone' = TRUE
  /\ retried' = (finalDelivered /\ (~sawEarlyFinal \/ finalBuffered) /\
                  ~cancelHandled /\ ~finalHasCancel)
  /\ UNCHANGED <<finishPending, finishStored, replyHasCancel, cancelRequested,
                 cancelHandled, ackPending, acknowledged, ackedEarly,
                 finalPublished, finalHasCancel, finalDelivered, finalBuffered,
                 sawEarlyFinal>>

Next ==
  StoreFinish \/ RequestCancel \/ HandleCancel \/ StoreAck \/ PublishFinal \/
  HandleFinal \/ HandleFinishReply \/ Complete

Spec == Init /\ [][Next]_vars
FairSpec == Spec /\ WF_vars(HandleCancel) /\ WF_vars(StoreAck)

TypeOK ==
  /\ finishPending \in BOOLEAN /\ finishStored \in BOOLEAN
  /\ replyHasCancel \in BOOLEAN /\ cancelRequested \in BOOLEAN
  /\ cancelHandled \in BOOLEAN /\ ackPending \in BOOLEAN
  /\ acknowledged \in BOOLEAN /\ ackedEarly \in BOOLEAN
  /\ finalPublished \in BOOLEAN /\ finalHasCancel \in BOOLEAN
  /\ finalDelivered \in BOOLEAN /\ finalBuffered \in BOOLEAN
  /\ sawEarlyFinal \in BOOLEAN /\ runnerDone \in BOOLEAN /\ retried \in BOOLEAN

AckSound == acknowledged => cancelRequested /\ cancelHandled
NoReturnBeforeReport == runnerDone => ~finishPending
NoRetryAfterCancel == retried => ~cancelRequested
RetryNeedsFinished == retried => finalDelivered /\ ~finishPending
NoDroppedEarlyFinal == sawEarlyFinal => finalBuffered

\* A withheld finish reply must not indefinitely prevent acknowledgment.
AckWhileFinishBlocked ==
  []((cancelRequested /\ finishPending /\ ~runnerDone) =>
     <>(acknowledged \/ ~finishPending \/ runnerDone))

NeverEarlyAck == ~ackedEarly
NeverEarlyFinal == ~sawEarlyFinal
=============================================================================
