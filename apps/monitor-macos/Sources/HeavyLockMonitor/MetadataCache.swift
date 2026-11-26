import Foundation

func parseCheckout(_ value: String?) -> (branch: String, directory: URL?) {
    let parts = value?.components(separatedBy: "\n") ?? []
    if parts.count == 2, !parts[0].isEmpty, !parts[1].isEmpty, parts[0].hasPrefix("/") {
        return (parts[1] == "HEAD" ? "Detached HEAD" : parts[1],
                URL(fileURLWithPath: parts[0], isDirectory: true))
    }
    return ("Branch unavailable", nil)
}

struct IssueMetadata: Decodable {
    let title: String
    let url: URL?
}

typealias MetadataRunner = (String, [String], String?, @escaping (String?) -> Void) -> Void

final class MetadataCache {
    private struct IssueKey: Hashable {
        let cwd: String
        let number: Int
    }
    private var issues: [IssueKey: IssueMetadata] = [:]
    private var checkouts: [String: (branch: String, directory: URL?)] = [:]
    private let changed: () -> Void
    private let runner: MetadataRunner

    init(changed: @escaping () -> Void, runner: @escaping MetadataRunner = MetadataCache.run) {
        self.changed = changed
        self.runner = runner
    }

    func issue(_ record: StatusRecord) -> IssueMetadata {
        guard let number = record.worklog, record.attribution == "declared" else {
            return IssueMetadata(title: "Unattributed", url: nil)
        }
        let key = IssueKey(cwd: record.cwd, number: number)
        if let value = issues[key] { return value }
        let loading = IssueMetadata(title: "Loading title…", url: nil)
        issues[key] = loading
        runner("gh", ["issue", "view", String(number), "--json", "title,url"], record.cwd) {
            [weak self] value in
            self?.issues[key] = value.flatMap {
                try? JSONDecoder().decode(IssueMetadata.self, from: Data($0.utf8))
            } ?? IssueMetadata(title: "Title unavailable", url: nil)
            self?.changed()
        }
        return loading
    }

    func checkout(_ cwd: String) -> (branch: String, directory: URL?) {
        if let value = checkouts[cwd] { return value }
        checkouts[cwd] = ("Loading branch…", nil)
        runner("git", ["-C", cwd, "rev-parse", "--show-toplevel", "--abbrev-ref", "HEAD"], nil) { [weak self] value in
            self?.checkouts[cwd] = parseCheckout(value)
            self?.changed()
        }
        return ("Loading branch…", nil)
    }

    private static func run(_ executable: String, _ arguments: [String], _ cwd: String?,
                            _ completion: @escaping (String?) -> Void) {
        DispatchQueue.global(qos: .utility).async {
            let process = Process()
            process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
            process.arguments = [executable] + arguments
            if let cwd { process.currentDirectoryURL = URL(fileURLWithPath: cwd, isDirectory: true) }
            var environment = ProcessInfo.processInfo.environment
            if executable == "gh" { environment.removeValue(forKey: "GH_REPO") }
            environment["PATH"] = (environment["PATH"] ?? "") +
                ":/opt/homebrew/bin:/usr/local/bin:/run/current-system/sw/bin"
            process.environment = environment
            let output = Pipe()
            process.standardOutput = output
            process.standardError = FileHandle.nullDevice
            var value: String?
            do {
                try process.run()
                let data = output.fileHandleForReading.readDataToEndOfFile()
                process.waitUntilExit()
                let text = String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines)
                if process.terminationStatus == 0, let text, !text.isEmpty { value = text }
            } catch {
                value = nil
            }
            let result = value
            DispatchQueue.main.async { completion(result) }
        }
    }
}
