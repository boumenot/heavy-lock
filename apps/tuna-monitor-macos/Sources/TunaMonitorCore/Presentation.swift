import Foundation
import HeavyLockV2

/// Text the monitor shows, kept separate from SwiftUI so it can be tested.
public enum Presentation {
    public enum Tone: Equatable, Sendable {
        case normal
        case warning
        case error
    }

    public struct Banner: Equatable, Sendable {
        public var title: String
        public var detail: String?
        public var tone: Tone
    }

    public struct JobRow: Equatable, Sendable {
        public var id: String
        public var command: String
        /// State, weight and other facts, e.g. `Queued #1 · weight 2`.
        public var summary: String
        /// Shown when the job was force-started, including in history.
        public var forced: Bool
        public var details: [String]
        public var warnings: [String]
    }

    // MARK: Service

    public static func banner(_ state: MonitorState, port: Int, now: Date) -> Banner {
        switch state.connection {
        case .connecting:
            return Banner(title: "Connecting to the tuna broker on 127.0.0.1:\(port)…", detail: nil, tone: .normal)
        case .incompatible(let message):
            return Banner(title: "Incompatible service on 127.0.0.1:\(port)", detail: message, tone: .error)
        case .unavailable(let message):
            let shown =
                state.lastUpdate.map {
                    "Showing the last known state from \(Self.age(of: $0, now: now)) ago; it is not live."
                } ?? "No broker state has been received."
            return Banner(title: "Broker unavailable: \(message)", detail: shown, tone: .error)
        case .live:
            var lines: [String] = []
            if let health = state.health {
                if !health.ready {
                    lines.append("Not ready: " + (health.hasRecoveryDetail ? health.recoveryDetail : "recovering"))
                }
                if health.hasStorageDetail { lines.append("Storage: \(health.storageDetail)") }
            }
            if let status = state.status {
                if status.storageUnavailable { lines.append("Durable state is unavailable; admissions are held.") }
                if status.recovering { lines.append("Recovering; admissions are held.") }
                if status.unreconciledReservations > 0 {
                    lines.append("\(status.unreconciledReservations) reservation(s) not yet reconciled since restart.")
                }
            }
            let version = state.health.map { " · broker \($0.serviceVersion)" } ?? ""
            return Banner(
                title: "Live\(version)",
                detail: lines.isEmpty ? nil : lines.joined(separator: "\n"),
                tone: lines.isEmpty ? .normal : .warning
            )
        }
    }

    /// Queue and capacity summary, or nil before any status is known.
    public static func statusLine(_ state: MonitorState) -> String? {
        guard let status = state.status else { return nil }
        let occupied = state.occupied
        var parts = ["Capacity \(occupied)/\(status.capacity)"]
        if occupied > status.capacity { parts.append("over capacity (force-started work)") }
        parts.append("\(state.runningCount) running")
        parts.append("\(state.queuedCount) queued")
        if status.paused { parts.append("queue paused") }
        if status.hasFreeMemoryBytes {
            parts.append("\(ByteCountFormatter.string(fromByteCount: status.freeMemoryBytes, countStyle: .memory)) free")
        }
        if status.hasLoadAverage1M { parts.append(String(format: "load %.2f", status.loadAverage1M)) }
        var line = parts.joined(separator: " · ")
        if let head = Self.blockedText(state.headBlockedReason, job: nil, capacity: status.capacity) {
            line += "\nQueue head: \(head)"
        }
        return line
    }

    // MARK: Jobs

    public static func row(_ job: Heavylock_V2_Job, capacity: UInt32?, now: Date) -> JobRow {
        var summary = [Self.stateName(job.state)]
        if job.state == .queued, job.hasQueuePosition { summary[0] += " #\(job.queuePosition + 1)" }
        summary.append("weight \(job.weight)")
        if job.attempt > 1 { summary.append("attempt \(job.attempt)") }
        if let since = Self.since(job), job.state != .finished {
            summary.append(Self.age(of: since, now: now))
        }
        if job.state == .finished, job.hasOutcome { summary.append(Self.outcomeText(job.outcome)) }

        var details: [String] = []
        let attribution = [
            job.attribution.hasRepository ? job.attribution.repository : nil,
            job.attribution.hasBranch ? job.attribution.branch : nil,
            job.attribution.hasWorklog ? "worklog \(job.attribution.worklog)" : nil,
        ].compactMap { $0 }
        if !attribution.isEmpty { details.append(attribution.joined(separator: " · ")) }
        if job.hasCwd { details.append(job.cwd) }
        if job.state == .finished, job.hasResources, job.resources.hasElapsedMs {
            details.append("Ran \(Self.duration(milliseconds: job.resources.elapsedMs))")
        }
        if job.admittedByBackfill { details.append("Admitted by backfill") }

        var warnings: [String] = []
        if job.state == .queued {
            let oversized = capacity.map { job.weight > $0 } ?? false
            let reason = oversized ? .weightExceedsCapacity : job.blockedReason
            if let blocked = Self.blockedText(reason, job: job, capacity: capacity) { warnings.append(blocked) }
        }
        if job.state != .finished, !job.runnerConnected {
            warnings.append("Runner disconnected" + (job.hasGraceRemainingMs ? "; \(Self.duration(milliseconds: job.graceRemainingMs)) grace left" : ""))
        }
        if job.hasCancelRequestedAtUnixNs, job.state != .finished {
            var text = "Cancellation requested"
            if job.hasCancelAcknowledgedAtUnixNs { text += " and acknowledged" }
            warnings.append(text + "; not yet complete")
        }
        if job.hasCancelFailureDetail { warnings.append("Cancellation failed: \(job.cancelFailureDetail)") }
        if job.hasBrokerTermination { warnings.append("Broker is terminating the job's processes") }
        if job.hasOutcome, job.outcome.kind == .unknown { warnings.append("Outcome unknown; capacity may still be held") }

        return JobRow(
            id: job.jobID, command: job.command, summary: summary.joined(separator: " · "),
            forced: job.forced, details: details, warnings: warnings
        )
    }

    public static func stateName(_ state: Heavylock_V2_JobState) -> String {
        switch state {
        case .queued: "Queued"
        case .reserved: "Reserved"
        case .running: "Running"
        case .finishing: "Finishing"
        case .finished: "Finished"
        default: "Unknown"
        }
    }

    public static func outcomeText(_ outcome: Heavylock_V2_Outcome) -> String {
        switch outcome.kind {
        case .exited: outcome.hasExitCode ? "exit \(outcome.exitCode)" : "exited"
        case .signaled: outcome.hasSignalName ? "signal \(outcome.signalName)" : "signaled"
        case .cancelledBeforeStart: "cancelled before start"
        case .removedDisconnected: "removed: runner disconnected"
        case .launchFailed: "launch failed"
        default: "outcome unknown"
        }
    }

    /// Why a queued job or the queue head is not being admitted.
    public static func blockedText(
        _ reason: Heavylock_V2_BlockedReason, job: Heavylock_V2_Job?, capacity: UInt32?
    ) -> String? {
        switch reason {
        case .paused: return "Paused"
        case .insufficientSlots: return "Waiting for capacity"
        case .resourceCheck: return "Waiting for memory/load checks"
        case .recovering: return "Broker recovering"
        case .runnerDisconnected: return "Runner disconnected"
        case .storageUnavailable: return "Durable state unavailable"
        case .weightExceedsCapacity:
            let weight = job.map { "Weight \($0.weight)" } ?? "Weight"
            let limit = capacity.map { " exceeds capacity \($0)" } ?? " exceeds capacity"
            return "\(weight)\(limit): normal admission will not start this job. Force-start or cancel it."
        default: return nil
        }
    }

    // MARK: Formatting

    public static func age(of date: Date, now: Date) -> String {
        Self.duration(milliseconds: Int64(max(0, now.timeIntervalSince(date)) * 1000))
    }

    public static func duration(milliseconds: Int64) -> String {
        let seconds = max(0, milliseconds) / 1000
        if seconds < 60 { return "\(seconds)s" }
        if seconds < 3600 { return "\(seconds / 60)m \(seconds % 60)s" }
        return "\(seconds / 3600)h \(seconds % 3600 / 60)m"
    }

    private static func since(_ job: Heavylock_V2_Job) -> Date? {
        let nanoseconds: Int64
        switch job.state {
        case .queued: nanoseconds = job.queuedAtUnixNs
        case .running where job.hasStartedAtUnixNs: nanoseconds = job.startedAtUnixNs
        default: nanoseconds = job.hasAdmittedAtUnixNs ? job.admittedAtUnixNs : job.queuedAtUnixNs
        }
        guard nanoseconds > 0 else { return nil }
        return Date(timeIntervalSince1970: Double(nanoseconds) / 1_000_000_000)
    }
}
