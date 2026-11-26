import HeavyLockV2

/// The job a control targets, captured when the user chose it so the
/// confirmation names the job that will receive the request.
public struct JobReference: Equatable, Hashable, Sendable {
    public var id: String
    public var command: String
    public var state: Heavylock_V2_JobState

    public init(id: String, command: String, state: Heavylock_V2_JobState) {
        self.id = id
        self.command = command
        self.state = state
    }

    public init(_ job: Heavylock_V2_Job) {
        self.init(id: job.jobID, command: job.command, state: job.state)
    }
}

/// What the monitor asks before sending a control.
public struct Confirmation: Equatable, Sendable {
    public var title: String
    public var message: String
    public var confirmTitle: String
    public var keepTitle: String
}

/// The monitor's queue controls. History deletion is deliberately absent: it
/// remains a REST/gRPC operation, not a monitor control.
public enum ControlAction: Equatable, Hashable, Sendable {
    case pause
    case resume
    case moveToFront(JobReference)
    case moveToBack(JobReference)
    case forceStart(JobReference)
    case cancel(JobReference)

    /// Recorded by the broker as the cancellation reason.
    public static let cancelReason = "cancelled from tuna monitor"

    public var title: String {
        switch self {
        case .pause: "Pause Queue"
        case .resume: "Resume Queue"
        case .moveToFront: "Move to Front"
        case .moveToBack: "Move to Back"
        case .forceStart: "Force Start…"
        case .cancel: "Cancel…"
        }
    }

    public var systemImageName: String {
        switch self {
        case .pause: "pause.fill"
        case .resume: "play.fill"
        case .moveToFront: "arrow.up.to.line"
        case .moveToBack: "arrow.down.to.line"
        case .forceStart: "bolt.fill"
        case .cancel: "xmark.circle"
        }
    }

    /// The control's name in messages.
    public var name: String {
        switch self {
        case .forceStart: "Force start"
        case .cancel: "Cancel"
        default: self.title
        }
    }

    /// Every cancellation and force-start is confirmed, whatever the job's
    /// state; pause, resume and reorder are sent without confirmation.
    public var confirmation: Confirmation? {
        switch self {
        case .pause, .resume, .moveToFront, .moveToBack:
            return nil
        case .forceStart(let job):
            return Confirmation(
                title: "Force-start this job?",
                message: """
                    \(job.command)

                    It starts now, out of queue order and even while the queue is paused, \
                    bypassing capacity, memory and load checks. Its weight still counts toward \
                    occupied capacity, so the machine may be overloaded.
                    """,
                confirmTitle: "Force Start",
                keepTitle: "Don't Start"
            )
        case .cancel(let job):
            let consequence =
                switch job.state {
                case .queued:
                    "It loses its queue position. If it is admitted before the request arrives, it is stopped instead."
                default:
                    "Its processes are stopped immediately without a graceful shutdown. This cannot be undone."
                }
            return Confirmation(
                title: "Cancel this \(Presentation.stateName(job.state).lowercased()) job?",
                message: "\(job.command)\n\n\(consequence)",
                confirmTitle: "Cancel Job",
                keepTitle: "Keep Job"
            )
        }
    }

    /// Job controls offered for a job in its current state. A move is
    /// omitted when it would leave `queue`, in broker order, unchanged: the
    /// broker only reorders, so moving the head to the front or the tail to
    /// the back has no effect.
    public static func jobActions(for job: Heavylock_V2_Job, in queue: [Heavylock_V2_Job]) -> [ControlAction] {
        let reference = JobReference(job)
        switch job.state {
        case .queued:
            var actions: [ControlAction] = []
            if queue.first?.jobID != job.jobID { actions.append(.moveToFront(reference)) }
            if queue.last?.jobID != job.jobID { actions.append(.moveToBack(reference)) }
            return actions + [.forceStart(reference), .cancel(reference)]
        case .reserved, .running, .finishing:
            return [.cancel(reference)]
        default:
            return []
        }
    }
}
