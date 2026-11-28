import Foundation
import TunaContract
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

    typealias Stream = AsyncThrowingStream<Tuna_BrokerEvent, any Error>

    private let recorded = Mutex(Recorded())
    private let streams = Mutex<[Stream]>([])
    private let healthResult: Mutex<Result<Tuna_Health, BrokerFailure>>
    private let recentJobs = Mutex(Tuna_ListJobsResponse())
    private let recentFailure = Mutex<BrokerFailure?>(nil)
    private let performFailure = Mutex<BrokerFailure?>(nil)

    init(health: Tuna_Health = FakeBroker.liveHealth()) {
        self.healthResult = Mutex(.success(health))
    }

    static func liveHealth() -> Tuna_Health {
        var health = Tuna_Health()
        health.serviceIdentity = TunaContract.errorDomain
        health.serviceVersion = TunaContract.serviceVersion
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

    func setHealth(_ result: Result<Tuna_Health, BrokerFailure>) {
        self.healthResult.withLock { $0 = result }
    }

    func setRecent(_ jobs: [Tuna_Job], through eventSeq: Int64 = 0) {
        self.recentJobs.withLock {
            $0.jobs = jobs
            $0.eventSeq = eventSeq
        }
    }

    func setRecentSequence(_ eventSeq: Int64) {
        self.recentJobs.withLock { $0.eventSeq = eventSeq }
    }

    func failRecent(_ failure: BrokerFailure?) {
        self.recentFailure.withLock { $0 = failure }
    }

    func failControls(_ failure: BrokerFailure?) {
        self.performFailure.withLock { $0 = failure }
    }

    func health() async throws -> Tuna_Health {
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

    func recentFinishedJobs(limit: UInt32) async throws -> Tuna_ListJobsResponse {
        self.recorded.withLock { $0.recentRequests += 1 }
        if let failure = self.recentFailure.withLock({ $0 }) { throw failure }
        return self.recentJobs.withLock { $0 }
    }

    func perform(_ action: ControlAction) async throws {
        self.recorded.withLock { $0.performed.append(action) }
        if let failure = self.performFailure.withLock({ $0 }) { throw failure }
    }
}

func job(
    _ id: String, _ state: Tuna_JobState, weight: UInt32 = 1, forced: Bool = false,
    position: UInt32? = nil, blocked: Tuna_BlockedReason = .unspecified
) -> Tuna_Job {
    var job = Tuna_Job()
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

func status(capacity: UInt32 = 2, occupied: UInt32 = 0, paused: Bool = false) -> Tuna_BrokerStatus {
    var status = Tuna_BrokerStatus()
    status.capacity = capacity
    status.occupied = occupied
    status.paused = paused
    return status
}

func snapshotEvent(
    seq: Int64, queue: [Tuna_Job] = [], active: [Tuna_Job] = [],
    status brokerStatus: Tuna_BrokerStatus = status()
) -> Tuna_BrokerEvent {
    var snapshot = Tuna_BrokerSnapshot()
    snapshot.status = brokerStatus
    snapshot.queue.jobs = queue
    snapshot.activeJobs = active
    var event = Tuna_BrokerEvent()
    event.eventSeq = seq
    event.snapshot = snapshot
    return event
}

func jobEvent(seq: Int64, _ job: Tuna_Job) -> Tuna_BrokerEvent {
    var event = Tuna_BrokerEvent()
    event.eventSeq = seq
    event.jobChanged = job
    return event
}

func heartbeatEvent(seq: Int64, intervalMs: Int64 = 0) -> Tuna_BrokerEvent {
    var event = Tuna_BrokerEvent()
    event.eventSeq = seq
    var heartbeat = Tuna_Heartbeat()
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
