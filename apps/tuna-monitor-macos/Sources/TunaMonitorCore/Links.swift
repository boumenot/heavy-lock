import Foundation
import HeavyLockV2
import Observation

/// A worklog issue named by a job's attribution. The repository is never
/// inferred: it is either explicit (`owner/repo#N` or the job's repository
/// attribution) or left to `gh` in the job's working directory, as in v1.
public struct WorklogReference: Hashable, Sendable {
    public var repository: String?
    public var number: Int

    public init(repository: String?, number: Int) {
        self.repository = repository
        self.number = number
    }

    public init?(_ job: Heavylock_V2_Job) {
        guard job.attribution.hasWorklog else { return nil }
        let value = job.attribution.worklog.trimmingCharacters(in: .whitespaces)
        let explicit = job.attribution.hasRepository ? job.attribution.repository : nil
        if let hash = value.lastIndex(of: "#") {
            let repository = String(value[..<hash])
            guard let number = Int(value[value.index(after: hash)...]), number > 0 else { return nil }
            self.init(repository: repository.isEmpty ? explicit : repository, number: number)
        } else {
            guard let number = Int(value), number > 0 else { return nil }
            self.init(repository: explicit, number: number)
        }
    }

    /// The short label, e.g. `#18`.
    public var label: String { "#\(self.number)" }
}

/// Resolved issue title and URL, as returned by `gh issue view`.
public struct IssueMetadata: Equatable, Sendable, Decodable {
    public var title: String
    public var url: URL?

    public init(title: String, url: URL?) {
        self.title = title
        self.url = url
    }
}

/// The checkout containing a job's working directory.
public struct Checkout: Equatable, Sendable {
    /// The checkout's top-level directory, or nil when it is not a git checkout.
    public var directory: String?
    /// The current branch, `Detached HEAD`, or nil when unknown.
    public var branch: String?

    public init(directory: String?, branch: String?) {
        self.directory = directory
        self.branch = branch
    }

    /// Parses `git rev-parse --show-toplevel --abbrev-ref HEAD`.
    public static func parse(_ output: String?) -> Checkout? {
        let parts = output?.components(separatedBy: "\n") ?? []
        guard parts.count == 2, parts[0].hasPrefix("/"), !parts[1].isEmpty else { return nil }
        return Checkout(directory: parts[0], branch: parts[1] == "HEAD" ? "Detached HEAD" : parts[1])
    }
}

/// Runs a command (`executable`, `arguments`, working directory) and returns
/// its trimmed standard output when it exits 0.
public typealias MetadataRunner = @Sendable (String, [String], String?) async -> String?

/// Looks up worklog issues and checkouts for displayed jobs, once per key, so
/// rows can link to them. Failures are cached as unresolved; no link is guessed.
@MainActor
@Observable
public final class JobMetadata {
    public enum Lookup<Value: Equatable & Sendable>: Equatable, Sendable {
        case loading
        case resolved(Value)
        case unavailable
    }

    private struct IssueKey: Hashable {
        var cwd: String?
        var worklog: WorklogReference
    }

    // Lookups are started while SwiftUI evaluates a body, so the caches are
    // not observed; reads observe `revision`, which changes when one resolves.
    @ObservationIgnored private var issues: [IssueKey: Lookup<IssueMetadata>] = [:]
    @ObservationIgnored private var checkouts: [String: Lookup<Checkout>] = [:]
    private var revision = 0
    @ObservationIgnored private let runner: MetadataRunner

    public init(runner: @escaping MetadataRunner = JobMetadata.run) {
        self.runner = runner
    }

    public func issue(_ worklog: WorklogReference, cwd: String?) -> Lookup<IssueMetadata> {
        _ = self.revision
        // Without an explicit repository, gh resolves the repository from
        // the job's checkout; with neither, there is nothing to look up.
        guard worklog.repository != nil || cwd != nil else { return .unavailable }
        let key = IssueKey(cwd: worklog.repository == nil ? cwd : nil, worklog: worklog)
        if let value = self.issues[key] { return value }
        self.issues[key] = .loading
        var arguments = ["issue", "view", String(worklog.number), "--json", "title,url"]
        if let repository = worklog.repository { arguments += ["--repo", repository] }
        let runner = self.runner
        Task {
            let output = await runner("gh", arguments, key.cwd)
            let decoded = output.flatMap { try? JSONDecoder().decode(IssueMetadata.self, from: Data($0.utf8)) }
            self.issues[key] = decoded.map { .resolved($0) } ?? .unavailable
            self.revision += 1
        }
        return .loading
    }

    public func checkout(_ cwd: String) -> Lookup<Checkout> {
        _ = self.revision
        if let value = self.checkouts[cwd] { return value }
        self.checkouts[cwd] = .loading
        let runner = self.runner
        Task {
            let output = await runner("git", ["-C", cwd, "rev-parse", "--show-toplevel", "--abbrev-ref", "HEAD"], nil)
            self.checkouts[cwd] = Checkout.parse(output).map { .resolved($0) } ?? .unavailable
            self.revision += 1
        }
        return .loading
    }

    /// Runs through `/usr/bin/env` with common tool directories on PATH,
    /// because a Finder-launched app has a minimal environment.
    public nonisolated static func run(_ executable: String, _ arguments: [String], _ cwd: String?) async -> String? {
        await withCheckedContinuation { continuation in
            DispatchQueue.global(qos: .utility).async {
                let process = Process()
                process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
                process.arguments = [executable] + arguments
                if let cwd { process.currentDirectoryURL = URL(fileURLWithPath: cwd, isDirectory: true) }
                var environment = ProcessInfo.processInfo.environment
                if executable == "gh" { environment.removeValue(forKey: "GH_REPO") }
                environment["PATH"] =
                    (environment["PATH"] ?? "") + ":/opt/homebrew/bin:/usr/local/bin:/run/current-system/sw/bin"
                process.environment = environment
                let output = Pipe()
                process.standardOutput = output
                process.standardError = FileHandle.nullDevice
                var value: String?
                if (try? process.run()) != nil {
                    let data = output.fileHandleForReading.readDataToEndOfFile()
                    process.waitUntilExit()
                    let text = String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines)
                    if process.terminationStatus == 0, let text, !text.isEmpty { value = text }
                }
                continuation.resume(returning: value)
            }
        }
    }
}
