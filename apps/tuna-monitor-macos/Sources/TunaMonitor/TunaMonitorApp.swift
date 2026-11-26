import AppKit
import HeavyLockV2
import SwiftUI
import TunaMonitorCore

@main
struct TunaMonitorApp: App {
    @NSApplicationDelegateAdaptor private var delegate: AppDelegate
    @State private var session: MonitorSession
    private let port: Int

    init() {
        let port = Self.port(arguments: CommandLine.arguments, environment: ProcessInfo.processInfo.environment)
        self.port = port
        let client: any BrokerClient
        do {
            client = try GRPCBrokerClient(port: port)
        } catch {
            client = UnavailableClient(message: String(describing: error))
        }
        self._session = State(initialValue: MonitorSession(client: client))
    }

    var body: some Scene {
        Window("Tuna Monitor", id: "main") {
            MonitorView(session: self.session, port: self.port)
                .frame(minWidth: 560, minHeight: 420)
                .task { self.session.start() }
        }
    }

    /// `--port N` (for example via `open … --args --port N`), then
    /// `TUNA_MONITOR_PORT`, then the broker's default port.
    static func port(arguments: [String], environment: [String: String]) -> Int {
        if let index = arguments.firstIndex(of: "--port"), index + 1 < arguments.count,
            let port = Int(arguments[index + 1]), (1...65535).contains(port)
        {
            return port
        }
        if let text = environment["TUNA_MONITOR_PORT"], let port = Int(text), (1...65535).contains(port) {
            return port
        }
        return GRPCBrokerClient.defaultPort
    }
}

/// Reports a client that could not be created instead of crashing.
private struct UnavailableClient: BrokerClient {
    var message: String

    func health() async throws -> Heavylock_V2_Health {
        throw BrokerFailure(code: "error", message: self.message)
    }

    func events(after afterEventSeq: Int64) -> AsyncThrowingStream<Heavylock_V2_BrokerEvent, any Error> {
        AsyncThrowingStream { $0.finish(throwing: BrokerFailure(code: "error", message: self.message)) }
    }

    func recentFinishedJobs(limit: UInt32) async throws -> [Heavylock_V2_Job] { [] }

    func perform(_ action: ControlAction) async throws {
        throw BrokerFailure(code: "error", message: self.message)
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }
}
