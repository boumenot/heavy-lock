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
        /// The attributed worklog, shown as a link once resolved.
        public var worklog: WorklogReference?
        /// The job's working directory, and the same path abbreviated with `~`.
        public var cwd: String?
        public var cwdDisplay: String?
        /// The submitting agent session, linked when the Copilot app reported it.
        public var session: AgentSession?
        public var details: [String]
        public var warnings: [String]
    }

    public enum TileStyle: Equatable, Sendable {
        case idle
        case running
        case notLive
    }

    /// The Dock tile's stop light, from the broker's explicit queue state.
    public enum QueueLight: Equatable, Sendable {
        /// Idle or running: nothing queued is waiting that could not start.
        case go
        /// Blocked (queued work cannot be admitted now), recovering or in
        /// error.
        case stalled
        /// The queue is paused.
        case stopped
        /// The broker is not live, so its queue state is unknown.
        case unknown
    }

    /// What the Dock tile shows: the worklog of the longest-running job, how
    /// many other jobs run beside it, the queue's stop light and the queued
    /// count as a badge.
    public struct DockTile: Equatable, Sendable {
        public var style: TileStyle
        public var label: String
        public var otherRunning: Int
        public var badge: String?
        public var light: QueueLight

        public init(style: TileStyle, label: String, otherRunning: Int, badge: String?, light: QueueLight) {
            self.style = style
            self.label = label
            self.otherRunning = otherRunning
            self.badge = badge
            self.light = light
        }
    }

    // MARK: Service

    public static func banner(_ state: MonitorState, port: Int, now: Date) -> Banner {
        switch state.connection {
        case .connecting:
            return Banner(title: "Connecting to the tuna broker on 127.0.0.1:\(port)…", detail: nil, tone: .normal)
        case .incompatible(let message):
            return Banner(title: "Incompatible service on 127.0.0.1:\(port)", detail: message, tone: .error)
        case .brokerError(let message):
            return Banner(
                title: "Broker error: durable state failed",
                detail: "\(message)\nAdmissions are held until the broker is restarted. The state shown is not live.",
                tone: .error)
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
        parts.append("queue \(Self.queueStateName(state.currentQueueState).lowercased())")
        if status.hasFreeMemoryBytes {
            parts.append("\(ByteCountFormatter.string(fromByteCount: status.freeMemoryBytes, countStyle: .memory)) free")
        }
        let loads = [
            status.hasLoadAverage1M ? String(format: "1m %.2f", status.loadAverage1M) : nil,
            status.hasLoadAverage5M ? String(format: "5m %.2f", status.loadAverage5M) : nil,
            status.hasLoadAverage15M ? String(format: "15m %.2f", status.loadAverage15M) : nil,
        ].compactMap { $0 }
        if !loads.isEmpty { parts.append("load " + loads.joined(separator: " / ")) }
        var line = parts.joined(separator: " · ")
        if let head = Self.blockedText(state.headBlockedReason, job: nil, capacity: status.capacity) {
            line += "\nQueue head: \(head)"
        }
        return line
    }

    // MARK: Jobs

    public static func dockTile(_ state: MonitorState) -> DockTile {
        let running = state.active.filter { $0.state == .running }
        let label = running.first.map { WorklogReference($0)?.label ?? "#?" }
        let badge = state.isLive && !state.queue.isEmpty ? String(state.queue.count) : nil
        let light = Self.queueLight(state)
        guard state.isLive else {
            return DockTile(
                style: .notLive, label: label ?? "!", otherRunning: max(0, running.count - 1), badge: nil,
                light: light)
        }
        guard let label else { return DockTile(style: .idle, label: "—", otherRunning: 0, badge: badge, light: light) }
        return DockTile(style: .running, label: label, otherRunning: running.count - 1, badge: badge, light: light)
    }

    static func queueLight(_ state: MonitorState) -> QueueLight {
        switch state.currentQueueState {
        case .idle, .running: .go
        case .blocked, .recovering, .error: .stalled
        case .paused: .stopped
        default: .unknown
        }
    }

    public static func queueStateName(_ state: Heavylock_V2_QueueState) -> String {
        switch state {
        case .idle: "Idle"
        case .running: "Running"
        case .blocked: "Blocked"
        case .paused: "Paused"
        case .recovering: "Recovering"
        case .error: "Error"
        default: "Unknown"
        }
    }

    /// `path` with the home directory replaced by `~`.
    public static func homeRelative(_ path: String, home: String = NSHomeDirectory()) -> String {
        let home = home.hasSuffix("/") ? String(home.dropLast()) : home
        guard !home.isEmpty else { return path }
        if path == home { return "~" }
        if path.hasPrefix(home + "/") { return "~" + path.dropFirst(home.count) }
        return path
    }

    public static func row(
        _ job: Heavylock_V2_Job, capacity: UInt32?, now: Date, home: String = NSHomeDirectory()
    ) -> JobRow {
        var summary = [Self.stateName(job.state)]
        if job.state == .queued, job.hasQueuePosition { summary[0] += " #\(job.queuePosition + 1)" }
        summary.append("weight \(job.weight)")
        if job.attempt > 1 { summary.append("attempt \(job.attempt)") }
        if let since = Self.since(job), job.state != .finished {
            summary.append(Self.age(of: since, now: now))
        }
        if job.state == .finished, job.hasOutcome { summary.append(Self.outcomeText(job.outcome)) }

        let worklog = WorklogReference(job)
        var details: [String] = []
        let attribution = [
            job.attribution.hasRepository ? job.attribution.repository : nil,
            job.attribution.hasBranch ? job.attribution.branch : nil,
            job.attribution.hasCommitSha ? String(job.attribution.commitSha.prefix(7)) : nil,
            // A worklog that is not an issue reference cannot be linked.
            job.attribution.hasWorklog && worklog == nil ? "worklog \(job.attribution.worklog)" : nil,
        ].compactMap { $0 }
        if !attribution.isEmpty { details.append(attribution.joined(separator: " · ")) }
        if let client = Self.clientText(job) { details.append(client) }
        if let (label, date) = Self.eventTime(job) {
            details.append("\(label) \(DateFormatter.localizedString(from: date, dateStyle: .short, timeStyle: .medium))")
        }
        if job.state == .finished, job.hasResources, job.resources.hasElapsed {
            details.append("Ran \(Self.duration(seconds: job.resources.elapsed.seconds))")
        }
        if job.admittedByBackfill { details.append("Admitted by backfill") }

        var warnings: [String] = []
        if job.state == .queued {
            let oversized = capacity.map { job.weight > $0 } ?? false
            let reason = oversized ? .weightExceedsCapacity : job.blockedReason
            if let blocked = Self.blockedText(reason, job: job, capacity: capacity) { warnings.append(blocked) }
        }
        if job.state != .finished, !job.runnerConnected {
            warnings.append(
                "Runner disconnected"
                    + (job.hasGraceRemaining ? "; \(Self.duration(seconds: job.graceRemaining.seconds)) grace left" : ""))
        }
        if job.hasCancelRequestedAt, job.state != .finished {
            var text = "Cancellation requested"
            if job.hasCancelAcknowledgedAt { text += " and acknowledged" }
            warnings.append(text + "; not yet complete")
        }
        if job.hasCancelFailureDetail { warnings.append("Cancellation failed: \(job.cancelFailureDetail)") }
        if job.hasBrokerTermination { warnings.append("Broker is terminating the job's processes") }
        if job.hasOutcome, job.outcome.kind == .unknown { warnings.append("Outcome unknown; capacity may still be held") }

        return JobRow(
            id: job.jobID, command: job.command, summary: summary.joined(separator: " · "),
            forced: job.forced, worklog: worklog, cwd: job.hasCwd ? job.cwd : nil,
            cwdDisplay: job.hasCwd ? Self.homeRelative(job.cwd, home: home) : nil,
            session: AgentSession(job), details: details, warnings: warnings
        )
    }

    /// The submitting client, e.g. `macOS 26.1 · arm64 · tuna 2.0.0`, or nil
    /// when nothing about it was recorded.
    public static func clientText(_ job: Heavylock_V2_Job) -> String? {
        guard job.hasClient else { return nil }
        let client = job.client
        let os: String? =
            switch client.os {
            case .macos: "macOS"
            case .linux: "Linux"
            case .windows: "Windows"
            case .other: "Other OS"
            default: nil
            }
        let architecture: String? =
            switch client.architecture {
            case .arm64: "arm64"
            case .amd64: "amd64"
            case .other: "other architecture"
            default: nil
            }
        let parts = [
            [os, client.hasOsVersion ? client.osVersion : nil].compactMap { $0 }.joined(separator: " "),
            architecture ?? "",
            [client.hasName ? client.name : nil, client.hasVersion ? client.version : nil].compactMap { $0 }.joined(separator: " "),
        ].filter { !$0.isEmpty }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
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
        Self.duration(seconds: max(0, milliseconds) / 1000)
    }

    private static func duration(seconds: Int64) -> String {
        let seconds = max(0, seconds)
        if seconds < 60 { return "\(seconds)s" }
        if seconds < 3600 { return "\(seconds / 60)m \(seconds % 60)s" }
        return "\(seconds / 3600)h \(seconds % 3600 / 60)m"
    }

    private static func since(_ job: Heavylock_V2_Job) -> Date? {
        Self.eventTime(job)?.1
    }

    private static func eventTime(_ job: Heavylock_V2_Job) -> (String, Date)? {
        switch job.state {
        case .queued where job.hasQueuedAt: return ("Queued", job.queuedAt.date)
        case .running where job.hasStartedAt: return ("Started", job.startedAt.date)
        case .finished where job.hasFinishedAt: return ("Finished", job.finishedAt.date)
        default:
            if job.hasAdmittedAt { return ("Admitted", job.admittedAt.date) }
            if job.hasQueuedAt { return ("Queued", job.queuedAt.date) }
            return nil
        }
    }
}
