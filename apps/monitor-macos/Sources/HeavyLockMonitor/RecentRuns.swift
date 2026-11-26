import Foundation

struct RecentRun {
    let record: StatusRecord
    let admittedAt: Date?
    let observedEnd: Date
    let duration: TimeInterval?

    init(record: StatusRecord, observedEnd: Date) {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        var admission = formatter.date(from: record.stateChanged)
        if admission == nil {
            formatter.formatOptions = [.withInternetDateTime]
            admission = formatter.date(from: record.stateChanged)
        }
        self.record = record
        self.admittedAt = admission
        self.observedEnd = observedEnd
        self.duration = admission.map { max(0, observedEnd.timeIntervalSince($0)) }
    }
}

final class RecentRuns {
    private struct Identity: Hashable {
        let pid: Int32
        let processStart: String

        init(_ record: StatusRecord) {
            pid = record.pid
            processStart = record.processStart
        }
    }

    static let limit = 100
    private var observed: [Identity: StatusRecord] = [:]
    private(set) var entries: [RecentRun] = []

    func observe(_ snapshot: StatusSnapshot, at observedEnd: Date) {
        let endedIdentities = Set(entries.map { Identity($0.record) })
        let liveIdentities = Set(snapshot.holders.map { Identity($0) })
        for holder in snapshot.holders {
            let identity = Identity(holder)
            if !endedIdentities.contains(identity) {
                observed[identity] = holder
            }
        }
        guard !snapshot.hasUnknownLock else { return }

        let ended = observed.filter { !liveIdentities.contains($0.key) }.sorted {
            if $0.key.pid != $1.key.pid { return $0.key.pid < $1.key.pid }
            return $0.key.processStart < $1.key.processStart
        }
        entries.insert(contentsOf: ended.map { RecentRun(record: $0.value, observedEnd: observedEnd) }, at: 0)
        for (identity, _) in ended {
            observed.removeValue(forKey: identity)
        }
        if entries.count > Self.limit {
            entries.removeLast(entries.count - Self.limit)
        }
    }
}
