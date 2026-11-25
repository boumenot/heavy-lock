import Darwin
import Foundation

struct StatusRecord: Decodable {
    enum State: String, Decodable {
        case waiting, running
    }

    let version: Int
    let worklog: Int?
    let attribution: String
    let command: [String]
    let cwd: String
    let weight: Int
    let capacity: Int
    let state: State
    let reason: String?
    let pid: Int32
    let processStart: String
    let created: String
    let stateChanged: String
    let heartbeat: String

    var displayWorklog: String {
        guard attribution == "declared", let worklog, worklog > 0 else { return "#?" }
        return "#\(worklog)"
    }
}

func processStart(_ pid: Int32) -> String? {
    guard pid > 0 else { return nil }
    var info = proc_bsdinfo()
    let size = Int32(MemoryLayout<proc_bsdinfo>.size)
    guard proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, size) == size,
          info.pbi_status != 5 else { return nil }
    return "\(info.pbi_start_tvsec):\(info.pbi_start_tvusec)"
}

func processGone(_ pid: Int32) -> Bool {
    guard pid > 0 else { return false }
    return kill(pid, 0) == -1 && errno == ESRCH
}

struct StatusSnapshot {
    let holders: [StatusRecord]
    let waiters: [StatusRecord]
    let staleCount: Int
    let unavailableCount: Int
    let hasUnknownLock: Bool
    var hasUnknownStatus: Bool { hasUnknownLock || staleCount > 0 }
    var extraHolderCount: Int { max(holders.count - 1, 0) }

    init(
        records: [StatusRecord],
        identity: (Int32) -> String?,
        isGone: (Int32) -> Bool = { _ in false },
        slotOwners: [Int32?]? = nil,
        readError: Bool = false
    ) {
        var holders: [StatusRecord] = []
        var waiters: [StatusRecord] = []
        var stale = 0
        var unavailable = 0
        for record in records {
            let validAttribution = (record.attribution == "unknown" && record.worklog == nil) ||
                (record.attribution == "declared" && (record.worklog ?? 0) > 0)
            guard record.version == 1, record.pid > 0, record.weight > 0,
                  record.capacity >= record.weight, !record.command.isEmpty, validAttribution,
                  !record.processStart.isEmpty else {
                unavailable += 1
                continue
            }
            guard let actualStart = identity(record.pid) else {
                if isGone(record.pid) { stale += 1 } else { unavailable += 1 }
                continue
            }
            guard actualStart == record.processStart else {
                stale += 1
                continue
            }

            switch record.state {
            case .running: holders.append(record)
            case .waiting: waiters.append(record)
            }
        }

        var inconsistent = false
        if let slotOwners {
            let livePids = Set((holders + waiters).map(\.pid))
            inconsistent = slotOwners.contains { owner in
                guard let owner else { return true }
                return !livePids.contains(owner)
            }
            holders = holders.filter { holder in
                let ownedSlots = slotOwners.filter { $0 == holder.pid }.count
                if ownedSlots < holder.weight {
                    inconsistent = true
                    return false
                }
                return true
            }
        }
        self.holders = holders
        self.waiters = waiters
        self.staleCount = stale
        self.unavailableCount = unavailable
        self.hasUnknownLock = readError || unavailable > 0 || inconsistent
    }
}
