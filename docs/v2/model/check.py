#!/usr/bin/env python3
"""Run bounded TLC cases; fail unless their documented outcomes occur."""

import argparse
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
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("cases", nargs="+", choices=CASES)
    parser.add_argument("--logs", type=Path, required=True)
    args = parser.parse_args()
    args.logs.mkdir(parents=True, exist_ok=True)

    for case in args.cases:
        log = args.logs.resolve() / f"{case}.log"
        with tempfile.TemporaryDirectory(prefix=f"heavy-lock-{case}-") as tmp:
            with log.open("w") as output:
                result = subprocess.run(
                    [
                        "tlc", "-workers", "1", "-fpmem", "0.05",
                        "-noGenerateSpecTE", "-metadir", tmp,
                        "-config", f"{case}.cfg", "BrokerRunner.tla",
                    ],
                    cwd=Path(__file__).resolve().parent,
                    stdout=output,
                    stderr=subprocess.STDOUT,
                    check=False,
                )
        text = log.read_text()
        expected = CASES[case]
        violation = re.search(r"Error: Invariant (\w+) is violated\.", text)
        if expected is None:
            ok = result.returncode == 0 and (
                "Model checking completed. No error has been found." in text
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
