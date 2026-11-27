import Foundation
import HeavyLockV2
import Synchronization
import TunaMonitorCore

/// A scripted broker: each `events(after:)` call gets the next stream the
/// test opened with `openStream()`, and every request is recorded.
final class FakeBroker: BrokerClient {
    struct Recorded {
        var watches: [Int64] = []
        var performed: [ControlAction] = []
        var healthChecks = 0
        var recentRequests = 0
    }

    typealias Stream = AsyncThrowingStream<Heavylock_V2_BrokerEvent, any Error>

    private let recorded = Mutex(Recorded())
    private let streams = Mutex<[Stream]>([])
    private let healthResult: Mutex<Result<Heavylock_V2_Health, BrokerFailure>>
    private let recentJobs = Mutex<[Heavylock_V2_Job]>([])
    private let performFailure = Mutex<BrokerFailure?>(nil)

    init(health: Heavylock_V2_Health = FakeBroker.liveHealth()) {
        self.healthResult = Mutex(.success(health))
    }

    static func liveHealth() -> Heavylock_V2_Health {
        var health = Heavylock_V2_Health()
        health.serviceIdentity = HeavyLockV2.errorDomain
        health.serviceVersion = HeavyLockV2.serviceVersion
        health.live = true
        health.ready = true
        return health
    }

    var snapshot: Recorded { self.recorded.withLock { $0 } }

    /// Queues the stream returned by the next watch and its continuation.
    func openStream() -> Stream.Continuation {
        let (stream, continuation) = Stream.makeStream()
        self.streams.withLock { $0.append(stream) }
        return continuation
    }

    func setHealth(_ result: Result<Heavylock_V2_Health, BrokerFailure>) {
        self.healthResult.withLock { $0 = result }
    }

    func setRecent(_ jobs: [Heavylock_V2_Job]) {
        self.recentJobs.withLock { $0 = jobs }
    }

    func failControls(_ failure: BrokerFailure?) {
        self.performFailure.withLock { $0 = failure }
    }

    func health() async throws -> Heavylock_V2_Health {
        self.recorded.withLock { $0.healthChecks += 1 }
        return try self.healthResult.withLock { $0 }.get()
    }

    func events(after afterEventSeq: Int64) -> Stream {
        self.recorded.withLock { $0.watches.append(afterEventSeq) }
        let next = self.streams.withLock { $0.isEmpty ? nil : $0.removeFirst() }
        if let next { return next }
        // No scripted stream: stay open (silent) until cancelled.
        return Stream { _ in }
    }

    func recentFinishedJobs(limit: UInt32) async throws -> [Heavylock_V2_Job] {
        self.recorded.withLock { $0.recentRequests += 1 }
        return self.recentJobs.withLock { $0 }
    }

    func perform(_ action: ControlAction) async throws {
        self.recorded.withLock { $0.performed.append(action) }
        if let failure = self.performFailure.withLock({ $0 }) { throw failure }
    }
}

func job(
    _ id: String, _ state: Heavylock_V2_JobState, weight: UInt32 = 1, forced: Bool = false,
    position: UInt32? = nil, blocked: Heavylock_V2_BlockedReason = .unspecified
) -> Heavylock_V2_Job {
    var job = Heavylock_V2_Job()
    job.jobID = id
    job.command = "make \(id)"
    job.state = state
    job.weight = weight
    job.forced = forced
    job.runnerConnected = true
    job.blockedReason = blocked
    if let position { job.queuePosition = position }
    return job
}

func status(capacity: UInt32 = 2, occupied: UInt32 = 0, paused: Bool = false) -> Heavylock_V2_BrokerStatus {
    var status = Heavylock_V2_BrokerStatus()
    status.capacity = capacity
    status.occupied = occupied
    status.paused = paused
    return status
}

func snapshotEvent(
    seq: Int64, queue: [Heavylock_V2_Job] = [], active: [Heavylock_V2_Job] = [],
    status brokerStatus: Heavylock_V2_BrokerStatus = status()
) -> Heavylock_V2_BrokerEvent {
    var snapshot = Heavylock_V2_BrokerSnapshot()
    snapshot.status = brokerStatus
    snapshot.queue.jobs = queue
    snapshot.activeJobs = active
    var event = Heavylock_V2_BrokerEvent()
    event.eventSeq = seq
    event.snapshot = snapshot
    return event
}

func jobEvent(seq: Int64, _ job: Heavylock_V2_Job) -> Heavylock_V2_BrokerEvent {
    var event = Heavylock_V2_BrokerEvent()
    event.eventSeq = seq
    event.jobChanged = job
    return event
}

func heartbeatEvent(seq: Int64, intervalMs: Int64 = 0) -> Heavylock_V2_BrokerEvent {
    var event = Heavylock_V2_BrokerEvent()
    event.eventSeq = seq
    var heartbeat = Heavylock_V2_Heartbeat()
    if intervalMs > 0 {
        heartbeat.heartbeatInterval.seconds = intervalMs / 1000
        heartbeat.heartbeatInterval.nanos = Int32(intervalMs % 1000) * 1_000_000
    }
    event.heartbeat = heartbeat
    return event
}

/// Polls `condition` on the main actor until it holds or two seconds pass.
@MainActor
func eventually(_ condition: () -> Bool) async -> Bool {
    let deadline = ContinuousClock.now + .seconds(2)
    while ContinuousClock.now < deadline {
        if condition() { return true }
        try? await Task.sleep(for: .milliseconds(5))
    }
    return condition()
}
