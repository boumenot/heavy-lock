import Foundation
import TunaContract

/// A bounded, completion-ordered window and its unfiltered summary.
public struct FinishedHistory: Sendable {
    public enum Outcome: String, CaseIterable, Hashable, Sendable {
        case success
        case failed
        case cancelled
        case unknown

        public var title: String {
            switch self {
            case .success: "Success"
            case .failed: "Failed"
            case .cancelled: "Cancelled"
            case .unknown: "Unknown"
            }
        }
    }

    public enum Worklog: Hashable, Sendable {
        case issue(repository: String?, number: Int, cwd: String?)
        case recorded(repository: String?, value: String)
        case unattributed

        public var label: String {
            switch self {
            case .issue(let repository, let number, let cwd):
                if let repository { return "\(repository)#\(number)" }
                if let cwd { return "#\(number) · \(Presentation.homeRelative(cwd))" }
                return "#\(number) · repository unspecified"
            case .recorded(let repository, let value):
                return [repository, value].compactMap { $0 }.joined(separator: " · ")
            case .unattributed:
                return "No worklog"
            }
        }
    }

    public struct Entry: Identifiable, Sendable {
        public let job: Tuna_Job
        public let outcome: Outcome
        public let elapsed: TimeInterval?
        public let worklog: Worklog

        public var id: String { self.job.jobID }
    }

    public struct Summary: Sendable {
        public let total: Int
        public let counts: [Outcome: Int]
        public let measured: Int
        public let mean: TimeInterval?
        public let median: TimeInterval?
        public let longest: TimeInterval?
    }

    public struct Group: Identifiable, Sendable {
        public let id: Worklog
        public let total: Int
        public let counts: [Outcome: Int]
        public var label: String { self.id.label }
    }

    public let entries: [Entry]
    public let summary: Summary
    public let groups: [Group]

    /// `jobs` must be newest first by completion time and job ID.
    public init(jobs: [Tuna_Job], limit: Int) {
        let entries = jobs.prefix(max(0, limit)).map {
            Entry(job: $0, outcome: Self.classify($0), elapsed: Self.elapsed($0), worklog: Self.worklog($0))
        }
        self.entries = entries
        let elapsed = entries.compactMap(\.elapsed).sorted()
        let count = elapsed.count
        self.summary = Summary(
            total: entries.count,
            counts: Self.counts(entries),
            measured: count,
            mean: count == 0 ? nil : elapsed.reduce(0, +) / Double(count),
            median: count == 0 ? nil : (elapsed[(count - 1) / 2] + elapsed[count / 2]) / 2,
            longest: elapsed.last
        )
        var keys: [Worklog] = []
        var grouped: [Worklog: [Entry]] = [:]
        for entry in entries {
            if grouped[entry.worklog] == nil { keys.append(entry.worklog) }
            grouped[entry.worklog, default: []].append(entry)
        }
        self.groups = keys.map { key in
            let entries = grouped[key, default: []]
            return Group(id: key, total: entries.count, counts: Self.counts(entries))
        }
    }

    public func visible(outcome: Outcome?, worklog: Worklog?) -> [Entry] {
        self.entries.filter { entry in
            (outcome == nil || entry.outcome == outcome) && (worklog == nil || entry.worklog == worklog)
        }
    }

    public static func classify(_ job: Tuna_Job) -> Outcome {
        guard job.hasOutcome else { return .unknown }
        switch job.outcome.kind {
        case .exited:
            guard job.outcome.hasExitCode else { return .unknown }
            return job.outcome.exitCode == 0 ? .success : .failed
        case .signaled:
            return job.hasCancelRequestedAt ? .cancelled : .failed
        case .cancelledBeforeStart:
            return .cancelled
        case .removedDisconnected, .launchFailed:
            return .failed
        default:
            return .unknown
        }
    }

    public static func formatElapsed(_ seconds: TimeInterval?) -> String {
        guard let seconds else { return "—" }
        if seconds > 0, seconds < 0.1 { return "<0.1s" }
        if seconds < 60, seconds.rounded(.down) != seconds {
            return String(format: "%.1fs", seconds)
        }
        return Presentation.duration(milliseconds: Int64(seconds * 1_000))
    }

    public static func measurement(seconds: Int64, nanos: Int32) -> TimeInterval? {
        guard seconds >= 0, seconds <= 315_576_000_000, nanos >= 0, nanos < 1_000_000_000 else { return nil }
        return Double(seconds) + Double(nanos) / 1_000_000_000
    }

    private static func elapsed(_ job: Tuna_Job) -> TimeInterval? {
        guard job.hasResources, job.resources.hasElapsed else { return nil }
        let duration = job.resources.elapsed
        return Self.measurement(seconds: duration.seconds, nanos: duration.nanos)
    }

    private static func worklog(_ job: Tuna_Job) -> Worklog {
        if let reference = WorklogReference(job) {
            return .issue(
                repository: reference.repository, number: reference.number,
                cwd: reference.repository == nil && job.hasCwd ? job.cwd : nil)
        }
        if job.attribution.hasWorklog, !job.attribution.worklog.isEmpty {
            return .recorded(
                repository: job.attribution.hasRepository ? job.attribution.repository : nil,
                value: job.attribution.worklog)
        }
        return .unattributed
    }

    private static func counts(_ entries: [Entry]) -> [Outcome: Int] {
        var result = Dictionary(uniqueKeysWithValues: Outcome.allCases.map { ($0, 0) })
        for entry in entries { result[entry.outcome, default: 0] += 1 }
        return result
    }
}
