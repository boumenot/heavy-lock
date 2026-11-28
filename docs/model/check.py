#!/usr/bin/env python3
"""Run bounded TLC cases; fail unless their documented outcomes occur."""

import argparse
import math
from pathlib import Path
import re
import subprocess
import tempfile


CASES = {
    "Safety": None,
    "Scheduling": None,
    "StorageSchedulingSafety": None,
    "Recovery": None,
    "Stress": None,
    "ClaimSafety": None,
    "TimeoutSafety": None,
    "RecoveryClaimSafety": None,
    "RecoveryReportSafety": None,
    "StorageRecoverySafety": None,
    "CompletionSafety": None,
    "KnownChildRace": None,
    "RecoverySchedulingSafety": None,
    "RecoveryAdmissionSafety": None,
    "BackfillScenarioSafety": None,
    "OversizedScenarioSafety": None,
    "NoWaitBackfillSafety": None,
    "NoWaitOrderSafety": None,
    "NoWaitHeadSafety": None,
    "NoWaitDisconnectedSafety": None,
    "NoWaitStrictSafety": None,
    "NoWaitPausedSafety": None,
    "NoWaitRegistrationSafety": None,
    "NoWaitLaunchSafety": None,
    "RunnerDeathCleanup": None,
    "CrashBeforeLaunch": "NeverCrashBeforeLaunchUnknown",
    "PreventedClaim": "NeverReleasePreventedClaim",
    "SealNoSpawn": "NeverReleaseSealedNoSpawn",
    "FinishFirst": "NeverFinishBeforeStart",
    "RecoveryReports": "NeverRecoveryReport",
    "RecoverySpawn": "NeverSpawnWhileRecovering",
    "StorageFailureSpawn": "NeverSpawnWhileStorageFailed",
    "ForcePaused": "NeverForcedWhilePaused",
    "GraceSafety": None,
    "GraceReset": "NeverGraceReset",
    "VolatileFence": "NoDuplicateLaunch",
    "UnfencedAbsence": "NoLiveUnreserved",
    "IncompleteTracking": "NoLiveUnreserved",
    "ForcedOverload": "NeverForcedOverload",
    "RecoveryBlocked": "RecoveryAcceptsPendingIdentity",
    "HeadBlocked": "NeverHeadBlocked",
    "Backfill": "NeverBackfill",
    "ReadyWithUncertain": "NeverReadyWithUnreconciled",
    "RecoveryAdmission": "NeverAdmissionBesideUncertain",
    "BackfillDisconnected": "NeverBackfillPastDisconnected",
    "OversizedSkipped": "NeverAdmitPastOversized",
    "NoWaitBackfill": "NeverNoWaitBackfill",
    "NoWaitOrder": "NeverNoWaitCandidateBusy",
    "NoWaitLaunch": "NeverNoWaitLaunched",
    "DescendantScanQuick": None,
    "DescendantScanSafety": None,
    "DescendantScanNoBarrier": "NoCapturedLiveUnreserved",
    "DescendantScanUnstable": "NoUnstableDescendants",
    "DescendantScanStable": "NeverStableCaptured",
    "DescendantScanRestart": "NeverRestartWithKnown",
    "DescendantScanFailure": "NeverScanFailed",
    "RunnerFinishSafety": None,
    "RunnerFinishBlocking": "temporal",
    "RunnerFinishDropped": "NoDroppedEarlyFinal",
    "RunnerFinishEarlyAck": "NeverEarlyAck",
    "RunnerFinishEarlyFinal": "NeverEarlyFinal",
    "RunnerExitSafety": None,
    "RunnerExitFailureSafety": None,
    "RunnerExitOldRule": "NoSpuriousTakeover",
    "RunnerExitNoBarrier": "NoCapturedLiveUnreserved",
    "RunnerExitCleanRelease": "NeverCleanExitRelease",
    "RunnerExitDeathTakeover": "NeverDeathTakeover",
    "MonitorDelivery": None,
    "MonitorLoss": None,
    "MonitorRestart": None,
    "MonitorStorage": None,
    "MonitorGap": None,
    "MonitorFair": None,
    "MonitorUnknownSafe": None,
    "MonitorEmptySafe": None,
    "MonitorStaleLive": "NoDetectedLossLive",
    "MonitorStaleMode": "NonLiveModeUnknown",
    "MonitorUnknownMode": "EnabledOnlyWhenKnown",
    "MonitorBlindReplay": "LiveHasAttemptHealth",
    "MonitorUnknownEvent": "LiveReflectsDeliveredSeq",
    "MonitorEmptyFirst": "LiveHasSnapshot",
    "MonitorReplayReached": "NeverReplayAfterDisconnect",
    "MonitorGapReached": "NeverGapRefresh",
    "MonitorStorageReached": "NeverStorageObserved",
    "MonitorRecoveryReached": "NeverRecoveringLive",
    "MonitorEpochWitness": "LiveHealthSameEpoch",
    "MonitorLagWitness": "LiveAtBrokerHead",
    "QueuePriorityQuick": None,
    "QueuePrioritySafety": None,
    "QueuePriorityStrictSafety": None,
    "QueuePrioritySeqOnly": "OrderedAdmission",
    "QueuePriorityNoClamp": "MoveExact",
    "QueuePriorityNoWaitAppend": "NoWaitExact",
    "QueuePriorityNoWaitFront": "OrderedAdmission",
    "QueuePriorityOvertake": "NeverPriorityOvertake",
    "QueuePriorityHeld": "NeverLowerWaitsBehindHeld",
    "QueuePriorityClamp": "NeverClamp",
    "QueuePriorityRestart": "NeverOvertakeAfterRestart",
    "SSEReplaySafety": None,
    "SSEReplayFair": None,
    "SSEReplayOldEpoch": "EpochFenced",
    "SSEReplayReached": "NeverResume",
    "FinishedHistorySafe": None,
    "FinishedHistoryNoFence": "CorrectProjection",
    "FinishedHistoryAhead": "NeverAhead",
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("cases", nargs="+", choices=CASES)
    parser.add_argument("--logs", type=Path, required=True)
    parser.add_argument("--timeout", type=float, help="maximum seconds per TLC case")
    args = parser.parse_args()
    if args.timeout is not None and (not math.isfinite(args.timeout) or args.timeout <= 0):
        parser.error("--timeout must be finite and positive")
    args.logs.mkdir(parents=True, exist_ok=True)

    for case in args.cases:
        log = args.logs.resolve() / f"{case}.log"
        with tempfile.TemporaryDirectory(prefix=f"tuna-{case}-") as tmp:
            with log.open("w") as output:
                command = [
                    "tlc", "-workers", "1", "-fpmem", "0.05",
                    "-noGenerateSpecTE", "-metadir", tmp,
                ]
                if case.startswith(("Monitor", "RunnerExit", "QueuePriority", "SSEReplay", "FinishedHistory")):
                    command.extend(["-coverage", "1"])
                command.extend([
                    "-config", f"{case}.cfg",
                    (
                        "DescendantScan.tla" if case.startswith("DescendantScan")
                        else "RunnerFinish.tla" if case.startswith("RunnerFinish")
                        else "RunnerExit.tla" if case.startswith("RunnerExit")
                        else "MonitorFreshness.tla" if case.startswith("Monitor")
                        else "QueuePriority.tla" if case.startswith("QueuePriority")
                        else "SSEReplay.tla" if case.startswith("SSEReplay")
                        else "FinishedHistory.tla" if case.startswith("FinishedHistory")
                        else "BrokerRunner.tla"
                    ),
                ])
                try:
                    result = subprocess.run(
                        command,
                        cwd=Path(__file__).resolve().parent,
                        stdout=output,
                        stderr=subprocess.STDOUT,
                        check=False,
                        timeout=args.timeout,
                    )
                except subprocess.TimeoutExpired:
                    raise SystemExit(f"{case}: exceeded {args.timeout}s; log: {log}")
        text = log.read_text()
        expected = CASES[case]
        violation = re.search(r"Error: Invariant (\w+) is violated\.", text)
        if expected is None:
            ok = result.returncode == 0 and (
                "Model checking completed. No error has been found." in text
            )
        elif expected == "temporal":
            ok = result.returncode == 13 and (
                "Error: Temporal properties were violated." in text
            )
        else:
            ok = (
                result.returncode == 12
                and violation is not None
                and violation.group(1) == expected
            )
        print(f"{case}: exit {result.returncode}; log: {log}", flush=True)
        for line in text.splitlines():
            if (
                "states generated," in line
                or "state graph search" in line
                or line.startswith("Finished in")
                or line.startswith("Error:")
            ):
                print(line, flush=True)
        if not ok:
            print(text, flush=True)
            raise SystemExit(f"{case}: unexpected TLC outcome")


if __name__ == "__main__":
    main()
