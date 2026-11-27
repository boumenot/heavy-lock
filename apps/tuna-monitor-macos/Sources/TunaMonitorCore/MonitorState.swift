import Foundation
import TunaContract

/// The monitor's last known broker state. Everything except `connection`
/// describes past observations; only a `.live` connection means it is
/// currently being kept up to date.
public struct MonitorState: Equatable, Sendable {
    public enum Connection: Equatable, Sendable {
        /// Establishing (or re-establishing) the connection.
        case connecting
        /// Health was checked and the event stream is delivering updates.
        case live
        /// The broker could not be reached or the stream failed.
        case unavailable(String)
        /// Something answered that is not a compatible Tuna broker.
        case incompatible(String)
        /// The broker is running but its durable state failed, so it holds
        /// every admission and cannot stream events until it is restarted.
        case brokerError(String)
    }

    public enum ApplyResult: Equatable, Sendable {
        case applied
        /// Already reflected; ignored.
        case duplicate
        /// One or more events are missing; state must be refreshed from a snapshot.
        case gap
        /// The event is empty or uses a oneof case this monitor does not understand.
        case unrecognized
    }

    public static let recentLimit = 20

    public var connection: Connection = .connecting
    public var health: Tuna_Health?
    public var status: Tuna_BrokerStatus?
    /// Queue order, head first.
    public var queue: [Tuna_Job] = []
    public var headBlockedReason: Tuna_BlockedReason = .unspecified
    /// The broker's operational queue state, from whichever status or queue
    /// event arrived last; both carry the state at their commit.
    public var queueState: Tuna_QueueState = .unspecified
    /// Reserved, running and finishing jobs.
    public var active: [Tuna_Job] = []
    /// Retained finished jobs, newest first.
    public var recent: [Tuna_Job] = []
    /// The last broker event sequence reflected; 0 before the first snapshot.
    public var lastEventSeq: Int64 = 0
    /// When the last stream message (including a heartbeat) arrived.
    public var lastUpdate: Date?
    /// Whether a snapshot has ever been applied.
    public var hasSnapshot = false
    /// Set when the state was rebuilt after missing events.
    public var notice: String?
    /// The most recent failed control, until the next control is sent.
    public var controlError: String?

    public init() {}

    public var isLive: Bool { self.connection == .live }

    /// The queue state to show now: the broker's own while live, `.error`
    /// when it reports failed storage and `.unspecified` (unknown) otherwise.
    public var currentQueueState: Tuna_QueueState {
        switch self.connection {
        case .live: self.queueState
        case .brokerError: .error
        default: .unspecified
        }
    }

    // The broker does not emit `status_changed` for every reservation, start
    // or finish, so counts come from the event-maintained queue and active
    // jobs rather than from the last `BrokerStatus`.
    public var queuedCount: Int { self.queue.count }
    public var runningCount: Int { self.active.filter { $0.state == .running }.count }
    /// Capacity held by reserved, running and finishing jobs.
    public var occupied: UInt32 { self.active.reduce(0) { $0 + $1.weight } }

    /// Applies one event from `WatchEvents`. Sequence checks are repeated here
    /// so a missing event is never silently skipped.
    public mutating func apply(_ event: Tuna_BrokerEvent, at time: Date) -> ApplyResult {
        switch event.event {
        case .snapshot(let snapshot):
            self.status = snapshot.status
            self.queue = snapshot.queue.jobs
            self.headBlockedReason = snapshot.queue.headBlockedReason
            self.queueState = snapshot.queue.queueState
            self.active = Self.sortedActive(snapshot.activeJobs)
            self.lastEventSeq = event.eventSeq
            self.lastUpdate = time
            self.hasSnapshot = true
            return .applied
        case .heartbeat:
            guard self.hasSnapshot, event.eventSeq <= self.lastEventSeq else { return .gap }
            self.lastUpdate = time
            return .applied
        case nil:
            return .unrecognized
        default:
            break
        }
        guard self.hasSnapshot else { return .gap }
        if event.eventSeq <= self.lastEventSeq { return .duplicate }
        guard event.eventSeq == self.lastEventSeq + 1 else { return .gap }
        switch event.event {
        case .jobChanged(let job):
            self.update(job)
        case .queueChanged(let queue):
            self.queue = queue.jobs
            self.headBlockedReason = queue.headBlockedReason
            self.queueState = queue.queueState
            let queued = Set(queue.jobs.map(\.jobID))
            self.active.removeAll { queued.contains($0.jobID) }
        case .statusChanged(let status):
            self.status = status
            self.headBlockedReason = status.headBlockedReason
            self.queueState = status.queueState
        case .jobDeleted(let id):
            self.remove(id)
        default:
            break
        }
        self.lastEventSeq = event.eventSeq
        self.lastUpdate = time
        return .applied
    }

    /// Forgets the event position so the next watch starts from a snapshot.
    /// The displayed data is kept, flagged by `connection`, until it arrives.
    public mutating func requireSnapshot(notice: String) {
        self.lastEventSeq = 0
        self.hasSnapshot = false
        self.notice = notice
    }

    public mutating func setRecent(_ jobs: [Tuna_Job]) {
        self.recent = Array(jobs.filter { $0.state == .finished }.prefix(Self.recentLimit))
    }

    private mutating func update(_ job: Tuna_Job) {
        switch job.state {
        case .queued:
            self.active.removeAll { $0.jobID == job.jobID }
            if let index = self.queue.firstIndex(where: { $0.jobID == job.jobID }) {
                self.queue[index] = job
            } else {
                let position = job.hasQueuePosition ? Int(job.queuePosition) : self.queue.count
                self.queue.insert(job, at: min(position, self.queue.count))
            }
        case .reserved, .running, .finishing:
            self.queue.removeAll { $0.jobID == job.jobID }
            self.active.removeAll { $0.jobID == job.jobID }
            self.active = Self.sortedActive(self.active + [job])
        case .finished:
            self.queue.removeAll { $0.jobID == job.jobID }
            self.active.removeAll { $0.jobID == job.jobID }
            self.recent.removeAll { $0.jobID == job.jobID }
            self.recent.insert(job, at: 0)
            if self.recent.count > Self.recentLimit { self.recent.removeLast(self.recent.count - Self.recentLimit) }
        default:
            break
        }
    }

    private mutating func remove(_ id: String) {
        self.queue.removeAll { $0.jobID == id }
        self.active.removeAll { $0.jobID == id }
        self.recent.removeAll { $0.jobID == id }
    }

    private static func sortedActive(_ jobs: [Tuna_Job]) -> [Tuna_Job] {
        jobs.sorted {
            ($0.hasAdmittedAt ? $0.admittedAt.seconds : Int64.max,
                $0.hasAdmittedAt ? $0.admittedAt.nanos : Int32.max, $0.jobID)
                < ($1.hasAdmittedAt ? $1.admittedAt.seconds : Int64.max,
                    $1.hasAdmittedAt ? $1.admittedAt.nanos : Int32.max, $1.jobID)
        }
    }
}
