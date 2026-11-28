import AppKit
import TunaContract
import Observation
import SwiftUI
import TunaMonitorCore

@main
struct TunaMonitorApp: App {
    @NSApplicationDelegateAdaptor private var delegate: AppDelegate
    @State private var session: MonitorSession
    @State private var diagnostics: DiagnosticsHost
    private let port: Int

    init() {
        MonitorPreferences.registerDefaults()
        let port = Self.port(arguments: CommandLine.arguments, environment: ProcessInfo.processInfo.environment)
        self.port = port
        let client: any BrokerClient
        do {
            client = try GRPCBrokerClient(port: port)
        } catch {
            client = UnavailableClient(message: String(describing: error))
        }
        let session = MonitorSession(client: client)
        self._session = State(initialValue: session)
        let diagnostics = DiagnosticsHost(
            DiagnosticsConfiguration.parse(
                arguments: CommandLine.arguments, environment: ProcessInfo.processInfo.environment),
            session: session, brokerPort: port)
        self._diagnostics = State(initialValue: diagnostics)
        diagnostics.start()
    }

    var body: some Scene {
        Window("Tuna Monitor", id: "main") {
            MonitorView(session: self.session, port: self.port, diagnostics: self.diagnostics.status)
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

    func health() async throws -> Tuna_Health {
        throw BrokerFailure(code: "error", message: self.message)
    }

    func events(after afterEventSeq: Int64) -> AsyncThrowingStream<Tuna_BrokerEvent, any Error> {
        AsyncThrowingStream { $0.finish(throwing: BrokerFailure(code: "error", message: self.message)) }
    }

    func recentFinishedJobs(limit: UInt32) async throws -> Tuna_ListJobsResponse {
        throw BrokerFailure(code: "error", message: self.message)
    }

    func perform(_ action: ControlAction) async throws {
        throw BrokerFailure(code: "error", message: self.message)
    }
}

/// Runs the opt-in diagnostics endpoint and reports whether it is listening.
@MainActor
@Observable
final class DiagnosticsHost {
    enum Status: Equatable {
        case off
        case starting(UInt16)
        case listening(UInt16)
        case failed(String)
    }

    private(set) var status: Status = .off
    @ObservationIgnored private let configuration: DiagnosticsConfiguration
    @ObservationIgnored private let session: MonitorSession
    @ObservationIgnored private let brokerPort: Int
    @ObservationIgnored private let startedAt = Date()
    @ObservationIgnored private var server: DiagnosticsServer?

    init(_ configuration: DiagnosticsConfiguration, session: MonitorSession, brokerPort: Int) {
        self.configuration = configuration
        self.session = session
        self.brokerPort = brokerPort
    }

    func start() {
        let port: UInt16
        switch self.configuration {
        case .disabled: return
        case .invalid(let message):
            self.report(.failed(message))
            return
        case .enabled(let value): port = value
        }
        do {
            let server = try DiagnosticsServer(port: port) { [weak self] in
                try await MainActor.run {
                    guard let self else { throw CancellationError() }
                    return try self.snapshot(port: port).json()
                }
            }
            self.server = server
            server.start { [weak self] state in
                Task { @MainActor in
                    switch state {
                    case .starting: self?.report(.starting(port))
                    case .listening(let bound): self?.report(.listening(bound))
                    case .failed(let message): self?.report(.failed("port \(port): \(message)"))
                    case .stopped: self?.report(.off)
                    }
                }
            }
        } catch {
            self.report(.failed("port \(port): \(error)"))
        }
    }

    private func report(_ status: Status) {
        self.status = status
        let line: String? =
            switch status {
            case .listening(let port): "Tuna Monitor diagnostics: http://127.0.0.1:\(port)\(DiagnosticsServer.path)"
            case .failed(let message): "Tuna Monitor diagnostics unavailable: \(message)"
            default: nil
            }
        if let line { FileHandle.standardError.write(Data((line + "\n").utf8)) }
    }

    private func snapshot(port: UInt16) -> MonitorDiagnostics {
        let dockView = NSApp.dockTile.contentView as? DockTileView
        let windows = NSApp.windows.filter(\.canBecomeMain).map {
            MonitorDiagnostics.Window(
                title: $0.title, visible: $0.isVisible, unoccluded: $0.occlusionState.contains(.visible),
                miniaturized: $0.isMiniaturized, key: $0.isKeyWindow)
        }
        return MonitorDiagnostics(
            state: self.session.state, brokerPort: self.brokerPort, now: Date(),
            monitor: .init(
                pid: ProcessInfo.processInfo.processIdentifier,
                version: Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String,
                bundlePath: Bundle.main.bundlePath, startedAt: self.startedAt, diagnosticsPort: port),
            dock: .init(installed: dockView != nil, shown: dockView?.tile, badgeLabel: NSApp.dockTile.badgeLabel),
            application: .init(active: NSApp.isActive, hidden: NSApp.isHidden, windows: windows))
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }
}
