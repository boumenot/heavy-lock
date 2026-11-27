import AppKit
import Foundation
import HeavyLockV2
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

        #expect(state.apply(jobEvent(seq: 6, job("q1", .running)), at: self.now) == .applied)
        #expect(state.queue.isEmpty)
        #expect(Set(state.active.map(\.jobID)) == ["q1", "r1"])

        #expect(state.apply(jobEvent(seq: 7, job("r1", .finished, forced: true)), at: self.now) == .applied)
        #expect(state.active.map(\.jobID) == ["q1"])
        #expect(state.recent.map(\.jobID) == ["r1"])
        #expect(state.recent.first?.forced == true)
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

    @Test func unavailableBannerLabelsLastKnownStateAsNotLive() {
        var state = MonitorState()
        state.lastUpdate = self.now.addingTimeInterval(-30)
        state.connection = .unavailable("connection refused")
        let banner = Presentation.banner(state, port: 49783, now: self.now)
        #expect(banner.tone == .error)
        #expect(banner.title == "Broker unavailable: connection refused")
        #expect(banner.detail?.contains("it is not live") == true)

        state.connection = .incompatible("the service on this port is not a heavy-lock v2 broker")
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
        var queue = Heavylock_V2_Queue()
        queue.queueState = .idle
        var snapshot = snapshotEvent(seq: 1, status: initial)
        snapshot.snapshot.queue.queueState = .idle
        _ = state.apply(snapshot, at: self.now)
        #expect(state.queueState == .idle)
        #expect(Presentation.statusLine(state)?.contains("queue idle") == true)

        var blocked = queue
        blocked.queueState = .blocked
        blocked.headBlockedReason = .insufficientSlots
        var queueEvent = Heavylock_V2_BrokerEvent()
        queueEvent.eventSeq = 2
        queueEvent.queueChanged = blocked
        _ = state.apply(queueEvent, at: self.now)
        #expect(state.queueState == .blocked)
        #expect(Presentation.statusLine(state)?.contains("queue blocked") == true)

        var paused = status(paused: true)
        paused.queueMode = .paused
        paused.queueState = .paused
        paused.headBlockedReason = .paused
        var statusEvent = Heavylock_V2_BrokerEvent()
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
    func liveSession(_ broker: FakeBroker, snapshot: Heavylock_V2_BrokerEvent) async -> (
        MonitorSession, FakeBroker.Stream.Continuation
    ) {
        let stream = broker.openStream()
        let session = MonitorSession(client: broker, reconnectDelay: .milliseconds(20))
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

    @Test func brokerWithoutExplicitQueueStateIsIncompatible() async {
        var health = FakeBroker.liveHealth()
        health.apiMinor = 0
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
    _ id: String, _ state: Heavylock_V2_JobState, worklog: String? = nil, repository: String? = nil,
    cwd: String? = nil
) -> Heavylock_V2_Job {
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
        state.status = Heavylock_V2_BrokerStatus()
        let cases: [(Heavylock_V2_QueueState, Presentation.QueueLight)] = [
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
