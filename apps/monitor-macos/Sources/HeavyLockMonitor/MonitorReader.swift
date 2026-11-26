import Foundation

enum MonitorReader {
    static func gateURL(environment: [String: String], fallbackDirectory: String) -> URL {
        let home = environment["HOME"] ?? fallbackDirectory
        let path = environment["HEAVY_LOCK_PATH"] ?? home + "/.cache/heavy-lock.lock"
        return URL(fileURLWithPath: path).standardizedFileURL
    }

    static func statusDirectory(gate: URL) -> URL {
        let name = gate.lastPathComponent
        let stem = name.hasSuffix(".lock") ? String(name.dropLast(5)) : name
        return gate.deletingLastPathComponent().appendingPathComponent("\(stem).status")
    }

    static func read(gate: URL) -> (snapshot: StatusSnapshot, legacy: Bool) {
        let status = StatusReader.read(directory: statusDirectory(gate: gate))
        let gateState = LockProbe.conflict(path: gate.path, requestWrite: false)
        let name = gate.lastPathComponent
        let stem = name.hasSuffix(".lock") ? String(name.dropLast(5)) : name
        let directory = gate.deletingLastPathComponent()
        var unavailable = status.readError || gateState == .unavailable
        var slotOwners: [Int32?] = []
        do {
            let files = try FileManager.default.contentsOfDirectory(atPath: directory.path)
            for name in files where name.hasPrefix("\(stem).slot-") && name.hasSuffix(".lock") {
                let index = name.dropFirst(stem.count + 6).dropLast(5)
                guard !index.isEmpty, index.allSatisfy(\.isNumber) else { continue }
                switch LockProbe.conflict(path: directory.appendingPathComponent(name).path, requestWrite: true) {
                case .held(let owner): slotOwners.append(owner)
                case .unavailable: unavailable = true
                case .free: break
                }
            }
        } catch {
            let error = error as NSError
            if error.domain != NSCocoaErrorDomain ||
                (error.code != NSFileReadNoSuchFileError && error.code != NSFileNoSuchFileError) {
                unavailable = true
            }
        }
        return (
            StatusSnapshot(records: status.records, identity: processStart, isGone: processGone, slotOwners: slotOwners, readError: unavailable),
            { if case .held = gateState { return true }; return false }()
        )
    }
}
