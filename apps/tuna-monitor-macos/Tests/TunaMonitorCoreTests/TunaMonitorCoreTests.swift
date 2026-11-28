import AppKit
import Foundation
import TunaContract
import Synchronization
import Testing

@testable import TunaMonitorCore

@Suite struct MonitorStateTests {
    let now = Date(timeIntervalSince1970: 1_000)

    @Test func snapshotReplacesStateAndContiguousEventsApply() {
        var state = MonitorState()
        let applied = state.apply(
            snapshotEvent(seq: 5, queue: [job("q1", .queued)], active: [job("r1", .running)]), at: self.now)
        #expect(applied == .applied)
        #expect(state.lastEventSeq == 5)
        #expect(state.queue.map(\.jobID) == ["q1"])
        #expect(state.active.map(\.jobID) == ["r1"])
        state.setRecent([], through: 5)

        #expect(state.apply(jobEvent(seq: 6, job("q1", .running)), at: self.now) == .applied)
        #expect(state.queue.isEmpty)
        #expect(Set(state.active.map(\.jobID)) == ["q1", "r1"])

        #expect(state.apply(jobEvent(seq: 7, job("r1", .finished, forced: true)), at: self.now) == .applied)
        #expect(state.active.map(\.jobID) == ["q1"])
        #expect(state.recent.map(\.jobID) == ["r1"])
        #expect(state.recent.first?.forced == true)
    }

    @Test func queueEventPreservesBrokerPriorityOrder() {
        var state = MonitorState()
        var older = job("older", .queued, position: 0)
        older.priority = 1337
        _ = state.apply(snapshotEvent(seq: 1, queue: [older]), at: self.now)
        var urgent = job("urgent", .queued, position: 0)
        urgent.priority = -5
        older.queuePosition = 1
        var queue = Tuna_Queue()
        queue.jobs = [urgent, older]
        var event = Tuna_BrokerEvent()
        event.eventSeq = 2
        event.queueChanged = queue
        #expect(state.apply(event, at: self.now) == .applied)
        #expect(state.queue.map(\.jobID) == ["urgent", "older"])
        #expect(state.queue.map(\.priority) == [-5, 1337])
    }

    @Test func duplicatesAreIgnoredAndGapsDetected() {
        var state = MonitorState()
        _ = state.apply(snapshotEvent(seq: 5), at: self.now)
        #expect(state.apply(jobEvent(seq: 5, job("x", .queued)), at: self.now) == .duplicate)
        #expect(state.queue.isEmpty)
        #expect(state.apply(jobEvent(seq: 7, job("x", .queued)), at: self.now) == .gap)
        #expect(state.queue.isEmpty)
        #expect(state.lastEventSeq == 5)
        #expect(state.apply(heartbeatEvent(seq: 6), at: self.now) == .gap)
        #expect(state.apply(heartbeatEvent(seq: 5), at: self.now) == .applied)
    }

    @Test func eventsBeforeSnapshotAreAGap() {
        var state = MonitorState()
        #expect(state.apply(jobEvent(seq: 1, job("x", .queued)), at: self.now) == .gap)
        #expect(state.apply(heartbeatEvent(seq: 0), at: self.now) == .gap)
    }

    @Test func requireSnapshotKeepsDataButRestartsFromZero() {
        var state = MonitorState()
        _ = state.apply(snapshotEvent(seq: 9, queue: [job("q1", .queued)]), at: self.now)
        state.requireSnapshot(notice: "refreshed")
        #expect(state.lastEventSeq == 0)
        #expect(state.queue.map(\.jobID) == ["q1"])
        #expect(state.apply(jobEvent(seq: 10, job("q2", .queued)), at: self.now) == .gap)
    }

    @Test func activeJobsSortByTimestampIncludingNanoseconds() {
        var first = job("a", .running)
        first.admittedAt.seconds = 100
        first.admittedAt.nanos = 2
        var second = job("b", .running)
        second.admittedAt.seconds = 100
        second.admittedAt.nanos = 1
        var state = MonitorState()
        _ = state.apply(snapshotEvent(seq: 1, active: [first, job("missing", .running), second]), at: self.now)
        #expect(state.active.map(\.jobID) == ["b", "a", "missing"])
    }

    @Test func finishedUpdatesStayInCompletionOrderRatherThanEventOrder() {
        var state = MonitorState()
        _ = state.apply(snapshotEvent(seq: 1), at: self.now)
        state.setRecent([], through: 1)
        var newest = job("a", .finished)
        newest.finishedAt.seconds = 300
        var older = job("b", .finished)
        older.finishedAt.seconds = 100
        var tied = job("c", .finished)
        tied.finishedAt.seconds = 300

        _ = state.apply(jobEvent(seq: 2, newest), at: self.now)
        _ = state.apply(jobEvent(seq: 3, older), at: self.now)
        _ = state.apply(jobEvent(seq: 4, tied), at: self.now)
        #expect(state.recent.map(\.jobID) == ["c", "a", "b"])
    }

    @Test func historySnapshotDoesNotResurrectAnAlreadyDeletedJob() {
        var state = MonitorState()
        _ = state.apply(snapshotEvent(seq: 1), at: self.now)
        state.connection = .live
        // The list query saw both the finish and deletion while the stream
        // still has those earlier events buffered.
        state.setRecent([], through: 3)
        #expect(!state.isRecentCurrent)
        var finished = job("deleted", .finished)
        finished.finishedAt.seconds = 10
        _ = state.apply(jobEvent(seq: 2, finished), at: self.now)
        #expect(state.recent.isEmpty)
        var deletion = Tuna_BrokerEvent()
        deletion.eventSeq = 3
        deletion.jobDeleted = finished.jobID
        _ = state.apply(deletion, at: self.now)
        #expect(state.recent.isEmpty)
        #expect(state.isRecentCurrent)
    }
}

@Suite struct FinishedHistoryTests {
    @Test func summaryUsesOnlyTheSelectedWindowAndMeasuredWallTime() {
        var success = job("success", .finished)
        success.outcome.kind = .exited
        success.outcome.exitCode = 0
        success.resources.elapsed.seconds = 0
        var failure = job("failure", .finished)
        failure.outcome.kind = .exited
        failure.outcome.exitCode = 2
        failure.resources.elapsed.seconds = 30
        failure.resources.elapsed.nanos = 500_000_000
        var cancelled = job("cancelled", .finished)
        cancelled.outcome.kind = .signaled
        cancelled.cancelRequestedAt.seconds = 123
        cancelled.resources.elapsed.seconds = 60
        var signal = job("signal", .finished)
        signal.outcome.kind = .signaled
        let unknown = job("unknown", .finished)
        var outsideWindow = job("outside", .finished)
        outsideWindow.outcome.kind = .launchFailed

        let history = FinishedHistory(
            jobs: [success, failure, cancelled, signal, unknown, outsideWindow], limit: 5)
        #expect(history.entries.map(\.job.jobID) == ["success", "failure", "cancelled", "signal", "unknown"])
        #expect(history.summary.total == 5)
        #expect(history.summary.counts[.success] == 1)
        #expect(history.summary.counts[.failed] == 2)
        #expect(history.summary.counts[.cancelled] == 1)
        #expect(history.summary.counts[.unknown] == 1)
        #expect(history.summary.measured == 3)
        #expect(abs((history.summary.mean ?? 0) - 30.166_666_666_7) < 0.000_001)
        #expect(history.summary.median == 30.5)
        #expect(history.summary.longest == 60)
        #expect(history.visible(outcome: .failed, worklog: nil).map(\.job.jobID) == ["failure", "signal"])
        #expect(history.summary.total == 5)
    }

    @Test func classifiesIncompleteAndCancellationEvidenceWithoutGuessing() {
        var missingExit = job("missing-exit", .finished)
        missingExit.outcome.kind = .exited
        var launch = job("launch", .finished)
        launch.outcome.kind = .launchFailed
        var disconnected = job("disconnected", .finished)
        disconnected.outcome.kind = .removedDisconnected
        var beforeStart = job("before-start", .finished)
        beforeStart.outcome.kind = .cancelledBeforeStart
        var badMeasurement = job("bad-measurement", .finished)
        badMeasurement.resources.elapsed.seconds = -1

        #expect(FinishedHistory.classify(missingExit) == .unknown)
        #expect(FinishedHistory.classify(launch) == .failed)
        #expect(FinishedHistory.classify(disconnected) == .failed)
        #expect(FinishedHistory.classify(beforeStart) == .cancelled)
        let history = FinishedHistory(jobs: [badMeasurement], limit: 10)
        #expect(history.summary.measured == 0)
        #expect(history.summary.mean == nil)
        #expect(history.summary.median == nil)
        #expect(history.summary.longest == nil)
    }

    @Test func worklogGroupsKeepRepositoriesAndUnattributedJobsDistinct() {
        var first = job("first", .finished)
        first.attribution.repository = "one/repo"
        first.attribution.worklog = "#7"
        first.outcome.kind = .exited
        first.outcome.exitCode = 0
        var second = job("second", .finished)
        second.attribution.repository = "other/repo"
        second.attribution.worklog = "#7"
        second.outcome.kind = .launchFailed
        var noRepo = job("no-repo", .finished)
        noRepo.attribution.worklog = "#7"
        noRepo.cwd = "/tmp/first-checkout"
        var otherCheckout = noRepo
        otherCheckout.jobID = "other-checkout"
        otherCheckout.cwd = "/tmp/other-checkout"
        let none = job("none", .finished)
        let history = FinishedHistory(jobs: [first, second, noRepo, otherCheckout, none], limit: 10)

        #expect(history.groups.count == 5)
        #expect(history.groups.map(\.total) == [1, 1, 1, 1, 1])
        let firstWorklog = history.entries[0].worklog
        #expect(history.visible(outcome: nil, worklog: firstWorklog).map(\.job.jobID) == ["first"])
        #expect(history.visible(outcome: .failed, worklog: firstWorklog).isEmpty)
        #expect(history.summary.counts[.failed] == 1)
        #expect(history.groups.last?.label == "No worklog")
    }
}

@Suite struct PresentationTests {
    let now = Date(timeIntervalSince1970: 1_000)

    @Test func monitorRegistersOneSecondTooltipDelayWithoutOverridingUserPreference() {
        let suiteName = "TunaMonitorTests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suiteName)!
        defer { defaults.removePersistentDomain(forName: suiteName) }

        MonitorPreferences.registerDefaults(in: defaults)
        #expect(defaults.integer(forKey: "NSInitialToolTipDelay") == 1_000)

        defaults.set(500, forKey: "NSInitialToolTipDelay")
        MonitorPreferences.registerDefaults(in: defaults)
        #expect(defaults.integer(forKey: "NSInitialToolTipDelay") == 500)
    }

    @Test func forcedJobsAreMarkedInRowsIncludingHistory() {
        #expect(Presentation.row(job("a", .running, forced: true), capacity: 2, now: self.now).forced)
        #expect(Presentation.row(job("b", .finished, forced: true), capacity: 2, now: self.now).forced)
        #expect(!Presentation.row(job("c", .running), capacity: 2, now: self.now).forced)
    }

    @Test func queuedRowShowsAdmissionPriority() {
        var queued = job("q", .queued)
        queued.priority = -12
        queued.queuePosition = 1
        let row = Presentation.row(queued, capacity: 2, now: self.now)
        #expect(row.summary.contains("Queued #2"))
        #expect(row.summary.contains("priority -12"))
    }

    @Test func oversizedQueuedJobHasPersistentWarning() {
        let expected =
            "Weight 5 exceeds capacity 4: normal admission will not start this job. Force-start or cancel it."
        let flagged = job("big", .queued, weight: 5, blocked: .weightExceedsCapacity)
        #expect(Presentation.row(flagged, capacity: 4, now: self.now).warnings.contains(expected))
        // Derived from weight and capacity even without a per-job reason.
        let unflagged = job("big", .queued, weight: 5)
        #expect(Presentation.row(unflagged, capacity: 4, now: self.now).warnings == [expected])
        #expect(Presentation.row(job("ok", .queued, weight: 4), capacity: 4, now: self.now).warnings.isEmpty)
    }

    @Test func runnerConnectivityIsWarnedOnlyWhileQueued() {
        // The broker projects runner_connected only for queued jobs.
        var queued = job("q", .queued)
        queued.runnerConnected = false
        #expect(Presentation.row(queued, capacity: 2, now: self.now).warnings == ["Runner disconnected"])
        for state in [Tuna_JobState.reserved, .running, .finishing, .finished] {
            var active = job("a", state)
            active.runnerConnected = false
            #expect(Presentation.row(active, capacity: 2, now: self.now).warnings.isEmpty)
        }
    }

    @Test func brokerTerminationIsActiveOnlyUntilFinished() {
        var running = job("r", .running)
        running.brokerTermination.reason = .runnerDead
        #expect(
            Presentation.row(running, capacity: 2, now: self.now).warnings
                == ["Broker is terminating the job's processes: runner died"])
        var finished = job("f", .finished)
        finished.brokerTermination.reason = .cancelAckDeadline
        finished.outcome.kind = .unknown
        let row = Presentation.row(finished, capacity: 2, now: self.now)
        #expect(row.warnings == ["Outcome unknown"])
        #expect(row.details.contains("Broker took over termination: cancellation not acknowledged in time"))
    }

    @Test func overCapacityIsExplained() {
        var state = MonitorState()
        state.status = status(capacity: 2, occupied: 0)
        state.active = [job("a", .running, weight: 2, forced: true), job("b", .running, weight: 1)]
        #expect(Presentation.statusLine(state)?.hasPrefix("Capacity 3/2 · over capacity (force-started work)") == true)
    }

    @Test func countsFollowJobEventsWithoutStatusChanged() {
        var state = MonitorState()
        _ = state.apply(
            snapshotEvent(seq: 1, queue: [job("q", .queued, weight: 2)], status: status(capacity: 2, occupied: 0)),
            at: self.now)
        #expect(Presentation.statusLine(state)?.hasPrefix("Capacity 0/2 · 0 running · 1 queued") == true)
        _ = state.apply(jobEvent(seq: 2, job("q", .running, weight: 2)), at: self.now)
        #expect(Presentation.statusLine(state)?.hasPrefix("Capacity 2/2 · 1 running · 0 queued") == true)
    }

    @Test func displaysPresentLoadsAndLocalJobTime() {
        var state = MonitorState()
        var sample = status()
        sample.loadAverage1M = 0
        sample.loadAverage15M = 2.5
        state.status = sample
        let line = Presentation.statusLine(state) ?? ""
        #expect(line.contains("load 1m 0.00 / 15m 2.50"))
        #expect(!line.contains(" / 5m "))

        var running = job("r", .running)
        running.startedAt.seconds = 900
        let row = Presentation.row(running, capacity: 2, now: self.now)
        let local = DateFormatter.localizedString(
            from: Date(timeIntervalSince1970: 900), dateStyle: .short, timeStyle: .medium)
        #expect(row.summary.contains("1m 40s"))
        #expect(row.details.contains("Started \(local)"))

        var finished = job("f", .finished)
        finished.resources.elapsed.seconds = 90
        #expect(Presentation.row(finished, capacity: 2, now: self.now).details.contains("Ran 1m 30s"))
    }

    @Test func unavailableBannerLabelsLastKnownStateAsNotLive() {
        var state = MonitorState()
        state.lastUpdate = self.now.addingTimeInterval(-30)
        state.connection = .unavailable("connection refused")
        let banner = Presentation.banner(state, port: 49783, now: self.now)
        #expect(banner.tone == .error)
        #expect(banner.title == "Broker unavailable: connection refused")
        #expect(banner.detail?.contains("it is not live") == true)

        state.connection = .incompatible("the service on this port is not a Tuna broker")
        #expect(Presentation.banner(state, port: 1234, now: self.now).title == "Incompatible service on 127.0.0.1:1234")

        state.connection = .brokerError("disk I/O error")
        let error = Presentation.banner(state, port: 1234, now: self.now)
        #expect(error.tone == .error)
        #expect(error.detail?.hasPrefix("disk I/O error") == true)
    }

    @Test func queueStateFollowsLatestStatusOrQueueEvent() {
        var state = MonitorState()
        state.connection = .live
        var initial = status()
        initial.queueState = .idle
        var queue = Tuna_Queue()
        queue.queueState = .idle
        var snapshot = snapshotEvent(seq: 1, status: initial)
        snapshot.snapshot.queue.queueState = .idle
        _ = state.apply(snapshot, at: self.now)
        #expect(state.queueState == .idle)
        #expect(Presentation.statusLine(state)?.contains("queue idle") == true)

        var blocked = queue
        blocked.queueState = .blocked
        blocked.headBlockedReason = .insufficientSlots
        var queueEvent = Tuna_BrokerEvent()
        queueEvent.eventSeq = 2
        queueEvent.queueChanged = blocked
        _ = state.apply(queueEvent, at: self.now)
        #expect(state.queueState == .blocked)
        #expect(Presentation.statusLine(state)?.contains("queue blocked") == true)

        var paused = status(paused: true)
        paused.queueMode = .paused
        paused.queueState = .paused
        paused.headBlockedReason = .paused
        var statusEvent = Tuna_BrokerEvent()
        statusEvent.eventSeq = 3
        statusEvent.statusChanged = paused
        _ = state.apply(statusEvent, at: self.now)
        #expect(state.queueState == .paused)
        #expect(state.headBlockedReason == .paused)
    }

    @Test func controlsConfirmOnlyCancelAndForceStart() {
        let queued = JobReference(job("q", .queued))
        let running = JobReference(job("r", .running))
        #expect(ControlAction.pause.confirmation == nil)
        #expect(ControlAction.resume.confirmation == nil)
        #expect(ControlAction.moveToFront(queued).confirmation == nil)
        #expect(ControlAction.moveToBack(queued).confirmation == nil)
        #expect(ControlAction.forceStart(queued).confirmation != nil)
        #expect(ControlAction.cancel(queued).confirmation?.message.contains("queue position") == true)
        #expect(ControlAction.cancel(running).confirmation?.message.contains("cannot be undone") == true)
        #expect(ControlAction.jobActions(for: job("f", .finished), in: []).isEmpty)
        #expect(ControlAction.jobActions(for: job("r", .running), in: []) == [.cancel(running)])
    }

    @Test func movesThatWouldNotReorderAreOmitted() {
        let queue = [job("a", .queued), job("b", .queued), job("c", .queued)]
        let (a, b, c) = (JobReference(queue[0]), JobReference(queue[1]), JobReference(queue[2]))
        #expect(ControlAction.jobActions(for: queue[0], in: queue) == [.moveToBack(a), .forceStart(a), .cancel(a)])
        #expect(
            ControlAction.jobActions(for: queue[1], in: queue)
                == [.moveToFront(b), .moveToBack(b), .forceStart(b), .cancel(b)])
        #expect(ControlAction.jobActions(for: queue[2], in: queue) == [.moveToFront(c), .forceStart(c), .cancel(c)])
        #expect(ControlAction.jobActions(for: queue[0], in: [queue[0]]) == [.forceStart(a), .cancel(a)])
    }

    @Test @MainActor func availableJobActionIconsAreDistinctAndResolvable() {
        let queue = [job("a", .queued), job("b", .queued), job("c", .queued)]
        let actions = ControlAction.jobActions(for: queue[1], in: queue)
        let symbols = actions.map(\.systemImageName)
        #expect(Set(symbols).count == actions.count)
        for action in actions {
            #expect(NSImage(systemSymbolName: action.systemImageName, accessibilityDescription: action.title) != nil)
        }
    }
}

@MainActor
@Suite struct MonitorSessionTests {
    func liveSession(_ broker: FakeBroker, snapshot: Tuna_BrokerEvent) async -> (
        MonitorSession, FakeBroker.Stream.Continuation
    ) {
        let stream = broker.openStream()
        let session = MonitorSession(client: broker, reconnectDelay: .milliseconds(20))
        broker.setRecentSequence(snapshot.eventSeq)
        session.start()
        stream.yield(snapshot)
        _ = await eventually { session.state.isLive }
        return (session, stream)
    }

    @Test func becomesLiveFromSnapshotAndLoadsRecentHistory() async {
        let broker = FakeBroker()
        broker.setRecent([job("old", .finished, forced: true)])
        let (session, _) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 3, queue: [job("q", .queued)]))
        defer { session.stop() }
        #expect(session.state.isLive)
        #expect(await eventually { session.state.recent.map(\.jobID) == ["old"] })
        #expect(broker.snapshot.watches == [0])
        #expect(broker.snapshot.healthChecks == 1)
    }

    @Test func deletingAVisibleFinishedJobRefillsTheWindow() async {
        let broker = FakeBroker()
        var first = job("first", .finished)
        first.finishedAt.seconds = 30
        var second = job("second", .finished)
        second.finishedAt.seconds = 20
        var replacement = job("replacement", .finished)
        replacement.finishedAt.seconds = 10
        broker.setRecent([first, second])
        let (session, stream) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 1))
        defer { session.stop() }
        #expect(session.state.isRecentCurrent)

        broker.setRecent([second, replacement], through: 2)
        var deletion = Tuna_BrokerEvent()
        deletion.eventSeq = 2
        deletion.jobDeleted = first.jobID
        stream.yield(deletion)
        #expect(await eventually {
            session.state.recent.map(\.jobID) == ["second", "replacement"]
                && session.state.isRecentCurrent
        })
        #expect(broker.snapshot.recentRequests == 2)
    }

    @Test func failedHistoryQueryIsVisibleAndRetriedOnHeartbeat() async {
        let broker = FakeBroker()
        broker.failRecent(BrokerFailure(code: "unavailable", message: "history request failed"))
        let (session, stream) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 1))
        defer { session.stop() }
        #expect(session.state.isLive)
        #expect(session.state.recentLoad == .unavailable("history request failed"))
        #expect(!session.state.isRecentCurrent)

        broker.failRecent(nil)
        broker.setRecent([job("recovered", .finished)], through: 1)
        stream.yield(heartbeatEvent(seq: 1))
        #expect(await eventually { session.state.isRecentCurrent })
        #expect(session.state.recent.map(\.jobID) == ["recovered"])
        #expect(broker.snapshot.recentRequests == 2)
    }

    @Test func staleHistoryResponseIsRejectedAndRetried() async {
        let broker = FakeBroker()
        broker.setRecent([job("stale", .finished)], through: 4)
        let stream = broker.openStream()
        let session = MonitorSession(client: broker, reconnectDelay: .milliseconds(20))
        defer { session.stop() }
        session.start()
        stream.yield(snapshotEvent(seq: 5))
        #expect(await eventually { session.state.isLive })
        #expect(session.state.recentLoad == .unavailable("history is older than the event stream"))
        #expect(session.state.recent.isEmpty)
        #expect(!session.state.isRecentCurrent)

        broker.setRecent([job("current", .finished)], through: 5)
        stream.yield(heartbeatEvent(seq: 5))
        #expect(await eventually { session.state.isRecentCurrent })
        #expect(session.state.recent.map(\.jobID) == ["current"])
    }

    @Test func historyAheadOfStreamIsNotCurrentUntilBufferedEventsCatchUp() async {
        let broker = FakeBroker()
        broker.setRecent([], through: 3)
        let stream = broker.openStream()
        let session = MonitorSession(client: broker, reconnectDelay: .milliseconds(20))
        defer { session.stop() }
        session.start()
        stream.yield(snapshotEvent(seq: 1))
        #expect(await eventually { session.state.isLive })
        #expect(!session.state.isRecentCurrent)
        let finished = job("deleted", .finished)
        stream.yield(jobEvent(seq: 2, finished))
        #expect(await eventually { session.state.lastEventSeq == 2 })
        #expect(session.state.recent.isEmpty)
        #expect(!session.state.isRecentCurrent)
        var deletion = Tuna_BrokerEvent()
        deletion.eventSeq = 3
        deletion.jobDeleted = finished.jobID
        stream.yield(deletion)
        #expect(await eventually { session.state.isRecentCurrent })
        #expect(session.state.recent.isEmpty)
    }

    @Test func gapRewatchesFromSnapshotWithNotice() async {
        let broker = FakeBroker()
        let (session, first) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 3))
        defer { session.stop() }
        let second = broker.openStream()
        first.yield(jobEvent(seq: 5, job("x", .queued)))
        #expect(await eventually { broker.snapshot.watches == [0, 0] })
        #expect(session.state.notice?.contains("Missed broker events") == true)
        #expect(!session.state.isLive)
        second.yield(snapshotEvent(seq: 9, queue: [job("x", .queued)]))
        #expect(await eventually { session.state.isLive && session.state.lastEventSeq == 9 })
        #expect(broker.snapshot.healthChecks == 2)
    }

    @Test func expiredReplayRefreshesFromSnapshot() async {
        let broker = FakeBroker()
        let (session, first) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 3))
        defer { session.stop() }
        _ = broker.openStream()
        first.finish(throwing: BrokerFailure(code: "outOfRange", reason: "EVENTS_EXPIRED", message: "expired"))
        #expect(await eventually { broker.snapshot.watches == [0, 0] })
        #expect(session.state.notice?.contains("Event replay expired") == true)
    }

    @Test func disconnectResumesAfterLastSequenceKeepingStaleData() async {
        let broker = FakeBroker()
        let (session, first) = await self.liveSession(
            broker, snapshot: snapshotEvent(seq: 3, queue: [job("q", .queued)]))
        defer { session.stop() }
        broker.setHealth(.failure(BrokerFailure(code: "unavailable", message: "connection refused")))
        first.finish(throwing: BrokerFailure(code: "unavailable", message: "connection refused"))
        #expect(await eventually { session.state.connection == .unavailable("cannot connect; is `tuna service` running?") })
        #expect(session.state.queue.map(\.jobID) == ["q"])

        let resumed = broker.openStream()
        broker.setHealth(.success(FakeBroker.liveHealth()))
        #expect(await eventually { broker.snapshot.watches == [0, 3] })
        resumed.yield(heartbeatEvent(seq: 3))
        #expect(await eventually { session.state.isLive })
    }

    @Test func unknownReplayedEventRequiresFreshSnapshot() async throws {
        let broker = FakeBroker()
        var known = status()
        known.queueMode = .enabled
        let (session, first) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 3, status: known))
        defer { session.stop() }
        #expect(session.state.isLive)

        let resumed = broker.openStream()
        first.finish(throwing: BrokerFailure(code: "unavailable", message: "disconnected"))
        #expect(await eventually { !session.state.isLive })
        #expect(await eventually { broker.snapshot.watches == [0, 3] })
        #expect(broker.snapshot.healthChecks >= 2)

        // Field 20 represents an event oneof case unknown to these bindings.
        let future = try Tuna_BrokerEvent(serializedBytes: Data([0x08, 0x04, 0xa2, 0x01, 0x00]))
        #expect(future.event == nil)
        let refreshed = broker.openStream()
        resumed.yield(future)
        #expect(await eventually { broker.snapshot.watches == [0, 3, 0] })
        #expect(!session.state.isLive)
        #expect(session.state.lastEventSeq == 0)
        #expect(!session.state.hasSnapshot)
        #expect(session.state.status?.queueMode == .enabled)
        #expect(session.state.notice?.contains("unrecognized") == true)
        let banner = Presentation.banner(session.state, port: 1, now: Date())
        #expect(banner.tone == .error)
        #expect(banner.detail?.contains("it is not live") == true)
        session.request(.pause)
        #expect(broker.snapshot.performed.isEmpty)
        #expect(session.state.controlError != nil)
        var paused = status()
        paused.queueMode = .paused
        refreshed.yield(snapshotEvent(seq: 4, status: paused))
        #expect(await eventually { session.state.isLive && session.state.status?.queueMode == .paused })
    }

    @Test func unknownLiveEventImmediatelyInvalidatesCachedState() async throws {
        let broker = FakeBroker()
        let (session, stream) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 3))
        defer { session.stop() }
        let refreshed = broker.openStream()
        let future = try Tuna_BrokerEvent(serializedBytes: Data([0x08, 0x04, 0xa2, 0x01, 0x00]))
        stream.yield(future)
        #expect(await eventually { broker.snapshot.watches == [0, 0] })
        #expect(!session.state.isLive)
        #expect(!session.state.hasSnapshot)
        #expect(session.state.lastEventSeq == 0)
        refreshed.yield(snapshotEvent(seq: 4))
        #expect(await eventually { session.state.isLive && session.state.lastEventSeq == 4 })
    }

    @Test func emptyFirstEventRequiresFreshSnapshot() async {
        let broker = FakeBroker()
        let stream = broker.openStream()
        let refreshed = broker.openStream()
        let session = MonitorSession(client: broker, reconnectDelay: .milliseconds(20))
        session.start()
        defer { session.stop() }

        stream.yield(Tuna_BrokerEvent())
        #expect(await eventually { broker.snapshot.watches == [0, 0] })
        #expect(!session.state.isLive)
        #expect(!session.state.hasSnapshot)
        #expect(session.state.lastEventSeq == 0)
        #expect(session.state.notice?.contains("unrecognized") == true)
        refreshed.yield(snapshotEvent(seq: 1))
        #expect(await eventually { session.state.isLive && session.state.hasSnapshot })
    }

    @Test func unrecognizedEventRetriesWithReconnectDelay() async {
        let broker = FakeBroker()
        let stream = broker.openStream()
        _ = broker.openStream()
        let session = MonitorSession(client: broker, reconnectDelay: .milliseconds(500))
        session.start()
        defer { session.stop() }

        stream.yield(Tuna_BrokerEvent())
        #expect(await eventually { session.state.connection == .unavailable("unrecognized broker event; refreshing") })
        let observedAt = ContinuousClock.now
        #expect(broker.snapshot.watches == [0])
        #expect(await eventually { broker.snapshot.watches == [0, 0] })
        #expect(ContinuousClock.now - observedAt >= .milliseconds(350))
        #expect(!session.state.isLive)
    }

    @Test func brokerWithDifferentMajorIsIncompatible() async {
        var health = FakeBroker.liveHealth()
        health.serviceVersion = "2.0.0"
        let broker = FakeBroker(health: health)
        let session = MonitorSession(client: broker, reconnectDelay: .milliseconds(20))
        session.start()
        defer { session.stop() }
        #expect(await eventually {
            if case .incompatible = session.state.connection { return true }
            return false
        })
        #expect(broker.snapshot.watches.isEmpty)
    }

    @Test func invalidHeartbeatMarksConnectionUnavailable() async {
        let broker = FakeBroker()
        let (session, stream) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 1))
        defer { session.stop() }
        var event = heartbeatEvent(seq: 1, intervalMs: 1_000)
        event.heartbeat.heartbeatInterval.nanos = 1_000_000_000
        stream.yield(event)
        #expect(await eventually {
            if case .unavailable(let detail) = session.state.connection {
                return detail.contains("invalid heartbeat interval")
            }
            return false
        })
    }

    @Test func failedBrokerStorageIsAnErrorAndNotWatched() async {
        var health = FakeBroker.liveHealth()
        health.ready = false
        health.storageDetail = "disk I/O error"
        let broker = FakeBroker(health: health)
        let session = MonitorSession(client: broker, reconnectDelay: .milliseconds(20))
        session.start()
        defer { session.stop() }
        #expect(await eventually { session.state.connection == .brokerError("disk I/O error") })
        #expect(session.state.currentQueueState == .error)
        #expect(Presentation.dockTile(session.state).light == .stalled)
        #expect(broker.snapshot.watches.isEmpty)
        broker.setHealth(.success(FakeBroker.liveHealth()))
        #expect(await eventually { broker.snapshot.watches == [0] })
    }

    @Test func incompatibleServiceIsReportedAndNotWatched() async {
        var health = FakeBroker.liveHealth()
        health.serviceIdentity = "something.else"
        let broker = FakeBroker(health: health)
        let session = MonitorSession(client: broker, reconnectDelay: .milliseconds(20))
        session.start()
        defer { session.stop() }
        #expect(await eventually {
            if case .incompatible = session.state.connection { return true }
            return false
        })
        #expect(broker.snapshot.watches.isEmpty)
    }

    @Test func missingHeartbeatsMarkTheBrokerUnavailable() async {
        let broker = FakeBroker()
        let stream = broker.openStream()
        let session = MonitorSession(
            client: broker, reconnectDelay: .seconds(60), minimumHeartbeatTimeout: .milliseconds(200))
        session.start()
        defer { session.stop() }
        stream.yield(snapshotEvent(seq: 1))
        #expect(await eventually { session.state.isLive })
        #expect(await eventually {
            if case .unavailable(let message) = session.state.connection { return message.contains("no heartbeat") }
            return false
        })
    }

    @Test func routineControlsAreSentImmediately() async {
        let broker = FakeBroker()
        let (session, _) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 1))
        defer { session.stop() }
        let queued = JobReference(job("q", .queued))
        session.request(.pause)
        session.request(.moveToFront(queued))
        #expect(session.pendingConfirmation == nil)
        #expect(await eventually { broker.snapshot.performed == [.pause, .moveToFront(queued)] })
    }

    @Test func cancelAndForceStartWaitForConfirmation() async {
        let broker = FakeBroker()
        let (session, _) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 1))
        defer { session.stop() }
        let queued = JobReference(job("q", .queued))
        let running = JobReference(job("r", .running))

        session.request(.forceStart(queued))
        #expect(session.pendingConfirmation == .forceStart(queued))
        session.dismissPending()
        #expect(session.pendingConfirmation == nil)

        session.request(.cancel(running))
        #expect(session.pendingConfirmation == .cancel(running))
        try? await Task.sleep(for: .milliseconds(50))
        #expect(broker.snapshot.performed.isEmpty)

        session.confirmPending()
        #expect(await eventually { broker.snapshot.performed == [.cancel(running)] })
    }

    @Test func controlsAreRefusedWhileNotLive() async {
        let broker = FakeBroker()
        broker.setHealth(.failure(BrokerFailure(code: "internal", message: "down")))
        let session = MonitorSession(client: broker, reconnectDelay: .seconds(60))
        session.start()
        defer { session.stop() }
        #expect(await eventually { session.state.connection == .unavailable("down") })
        session.request(.pause)
        session.request(.cancel(JobReference(job("q", .queued))))
        #expect(session.pendingConfirmation == nil)
        #expect(session.state.controlError?.contains("not live") == true)
        try? await Task.sleep(for: .milliseconds(50))
        #expect(broker.snapshot.performed.isEmpty)
    }

    @Test func confirmationIsNotSentIfTheBrokerWentAway() async {
        let broker = FakeBroker()
        let (session, stream) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 1))
        defer { session.stop() }
        let queued = JobReference(job("q", .queued))
        session.request(.cancel(queued))
        broker.setHealth(.failure(BrokerFailure(code: "unavailable", message: "down")))
        stream.finish(throwing: BrokerFailure(code: "unavailable", message: "down"))
        #expect(await eventually { !session.state.isLive })
        session.confirmPending()
        try? await Task.sleep(for: .milliseconds(50))
        #expect(broker.snapshot.performed.isEmpty)
        #expect(session.state.controlError?.contains("no longer live") == true)
    }

    @Test func controlFailuresAreShown() async {
        let broker = FakeBroker()
        broker.failControls(BrokerFailure(code: "failedPrecondition", reason: "JOB_NOT_QUEUED", message: "not queued"))
        let (session, _) = await self.liveSession(broker, snapshot: snapshotEvent(seq: 1))
        defer { session.stop() }
        session.request(.moveToBack(JobReference(job("q", .queued))))
        #expect(await eventually { session.state.controlError == "Move to Back failed: JOB_NOT_QUEUED: not queued" })
        #expect(session.inFlight.isEmpty)
    }
}

private func attributed(
    _ id: String, _ state: Tuna_JobState, worklog: String? = nil, repository: String? = nil,
    cwd: String? = nil
) -> Tuna_Job {
    var value = job(id, state)
    if let worklog { value.attribution.worklog = worklog }
    if let repository { value.attribution.repository = repository }
    if let cwd { value.cwd = cwd }
    return value
}

@Suite struct DockAndLinkTests {
    let now = Date(timeIntervalSince1970: 1_000)

    @Test func dockTileShowsLongestRunningWorklogOthersAndQueuedBadge() {
        var state = MonitorState()
        state.connection = .live
        #expect(Presentation.dockTile(state) == .init(style: .idle, label: "—", otherRunning: 0, badge: nil, light: .unknown))

        state.active = [
            attributed("a", .running, worklog: "18"), attributed("b", .running), attributed("c", .reserved),
        ]
        state.queue = [job("q1", .queued), job("q2", .queued)]
        #expect(Presentation.dockTile(state) == .init(style: .running, label: "#18", otherRunning: 1, badge: "2", light: .unknown))

        state.active = [attributed("b", .running), attributed("a", .running, worklog: "owner/repo#7")]
        #expect(Presentation.dockTile(state).label == "#?")
        state.active.reverse()
        #expect(Presentation.dockTile(state).label == "#7")

        state.connection = .unavailable("refused")
        #expect(
            Presentation.dockTile(state)
                == .init(style: .notLive, label: "#7", otherRunning: 1, badge: nil, light: .unknown))
        state.active = []
        #expect(Presentation.dockTile(state).label == "!")
    }

    @Test func stopLightFollowsExplicitQueueState() {
        var state = MonitorState()
        state.connection = .live
        state.status = Tuna_BrokerStatus()
        let cases: [(Tuna_QueueState, Presentation.QueueLight)] = [
            (.idle, .go), (.running, .go), (.blocked, .stalled), (.paused, .stopped),
            (.recovering, .stalled), (.error, .stalled), (.unspecified, .unknown),
        ]
        for (queueState, light) in cases {
            state.queueState = queueState
            #expect(Presentation.dockTile(state).light == light)
        }
        // A blocked head alone is diagnostic; the explicit state decides.
        state.queueState = .running
        state.headBlockedReason = .insufficientSlots
        #expect(Presentation.dockTile(state).light == .go)

        state.queueState = .paused
        state.connection = .unavailable("refused")
        #expect(Presentation.dockTile(state).light == .unknown)
        state.connection = .brokerError("disk I/O error")
        #expect(state.currentQueueState == .error)
        #expect(Presentation.dockTile(state).light == .stalled)
        #expect(Presentation.dockTile(state).style == .notLive)
    }

    @Test func pathsUnderHomeUseTilde() {
        #expect(Presentation.homeRelative("/Users/me/src/x", home: "/Users/me") == "~/src/x")
        #expect(Presentation.homeRelative("/Users/me", home: "/Users/me/") == "~")
        #expect(Presentation.homeRelative("/Users/meow/x", home: "/Users/me") == "/Users/meow/x")
        let row = Presentation.row(
            attributed("a", .running, worklog: "18", cwd: "/Users/me/w"), capacity: 2, now: self.now, home: "/Users/me")
        #expect(row.cwd == "/Users/me/w")
        #expect(row.cwdDisplay == "~/w")
        #expect(row.worklog == WorklogReference(repository: nil, number: 18))
        #expect(!row.details.contains { $0.contains("/Users/me") || $0.contains("worklog") })
    }

    @Test func worklogReferencesAreExplicitOnly() {
        #expect(WorklogReference(attributed("a", .running, worklog: "18"))?.repository == nil)
        #expect(
            WorklogReference(attributed("a", .running, worklog: "o/r#3"))
                == WorklogReference(repository: "o/r", number: 3))
        #expect(
            WorklogReference(attributed("a", .running, worklog: "5", repository: "o/r"))
                == WorklogReference(repository: "o/r", number: 5))
        #expect(WorklogReference(attributed("a", .running, worklog: "release")) == nil)
        #expect(WorklogReference(attributed("a", .running)) == nil)
        let row = Presentation.row(attributed("a", .running, worklog: "release"), capacity: 2, now: self.now)
        #expect(row.worklog == nil)
        #expect(row.details == ["worklog release"])
    }

    @Test func recordedAttributionQualifiesWorklogsAndLinksSessions() {
        var value = attributed("a", .running, worklog: "boumenot/tuna#33", repository: "boumenot/tuna")
        value.attribution.branch = "main"
        value.attribution.commitSha = String(repeating: "a", count: 40)
        value.attribution.agentSession = "22caf59c-ccac-4db8-96ac-9723739b61fb"
        value.attribution.extra = [
            AttributionKey.repositoryHost: "github.com",
            AttributionKey.worklogURL: "https://github.com/boumenot/tuna/issues/33",
            AttributionKey.aiAgent: "github_copilot_app_agent",
        ]
        value.client.name = "tuna"
        value.client.version = "2.0.0"
        value.client.os = .macos
        value.client.osVersion = "26.1"
        value.client.architecture = .arm64
        let row = Presentation.row(value, capacity: 2, now: self.now)
        let url = URL(string: "https://github.com/boumenot/tuna/issues/33")
        #expect(row.worklog == WorklogReference(repository: "github.com/boumenot/tuna", number: 33, url: url))
        #expect(row.worklog?.qualifiedLabel == "boumenot/tuna#33")
        #expect(row.worklog?.label == "#33")
        #expect(row.details.contains("boumenot/tuna · main · aaaaaaa"))
        #expect(row.details.contains("macOS 26.1 · arm64 · tuna 2.0.0"))
        #expect(
            row.session
                == AgentSession(
                    id: "22caf59c-ccac-4db8-96ac-9723739b61fb",
                    url: URL(string: "ghapp://sessions/22caf59c-ccac-4db8-96ac-9723739b61fb")))

        // A legacy bare number takes the job's repository and host.
        var legacy = attributed("b", .running, worklog: "16", repository: "o/r")
        legacy.attribution.extra = [AttributionKey.repositoryHost: "ghe.example"]
        #expect(
            WorklogReference(legacy)
                == WorklogReference(repository: "ghe.example/o/r", number: 16, url: URL(string: "https://ghe.example/o/r/issues/16")))
        // Unsafe recorded URLs and sessions from other agents are not linked.
        var other = attributed("c", .running, worklog: "o/r#2")
        other.attribution.extra = [AttributionKey.worklogURL: "javascript:alert(1)", AttributionKey.aiAgent: "other"]
        other.attribution.agentSession = "abc"
        #expect(WorklogReference(other) == WorklogReference(repository: "o/r", number: 2))
        #expect(AgentSession(other) == AgentSession(id: "abc", url: nil))
        #expect(Presentation.clientText(attributed("d", .running)) == nil)
    }

    @Test func checkoutOutputIsParsed() {
        #expect(Checkout.parse("/w\nmain") == Checkout(directory: "/w", branch: "main"))
        #expect(Checkout.parse("/w\nHEAD")?.branch == "Detached HEAD")
        #expect(Checkout.parse(nil) == nil)
        #expect(Checkout.parse("fatal") == nil)
    }

    @MainActor
    @Test func metadataUsesGhInTheJobDirectoryOrExplicitRepositoryAndCaches() async {
        let calls = Mutex<[[String]]>([])
        let metadata = JobMetadata { executable, arguments, cwd in
            calls.withLock { $0.append([executable, cwd ?? "-"] + arguments) }
            switch executable {
            case "gh" where arguments.contains("18"):
                return #"{"title":"Monitor","url":"https://github.com/o/r/issues/18"}"#
            case "git": return "/w\nmain"
            default: return nil
            }
        }
        let local = WorklogReference(repository: nil, number: 18)
        #expect(metadata.issue(local, cwd: "/w/sub") == .loading)
        #expect(metadata.issue(local, cwd: nil) == .unavailable)
        _ = metadata.issue(WorklogReference(repository: "o/r", number: 9), cwd: "/w")
        _ = metadata.checkout("/w/sub")
        #expect(await eventually { calls.withLock { $0.count } == 3 && metadata.checkout("/w/sub") != .loading })
        #expect(await eventually { metadata.issue(local, cwd: "/w/sub") != .loading })

        #expect(
            metadata.issue(local, cwd: "/w/sub")
                == .resolved(IssueMetadata(title: "Monitor", url: URL(string: "https://github.com/o/r/issues/18"))))
        #expect(metadata.issue(WorklogReference(repository: "o/r", number: 9), cwd: "/w") == .unavailable)
        #expect(metadata.checkout("/w/sub") == .resolved(Checkout(directory: "/w", branch: "main")))
        let recorded = calls.withLock { $0 }
        #expect(recorded.count == 3)
        #expect(recorded.contains(["gh", "/w/sub", "issue", "view", "18", "--json", "title,url"]))
        #expect(recorded.contains(["gh", "-", "issue", "view", "9", "--json", "title,url", "--repo", "o/r"]))
        #expect(recorded.contains(["git", "-", "-C", "/w/sub", "rev-parse", "--show-toplevel", "--abbrev-ref", "HEAD"]))
    }
}
