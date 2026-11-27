import Foundation
import HeavyLockV2
import Synchronization
import Testing

@testable import TunaMonitorCore

@Suite struct DiagnosticsTests {
    let now = Date(timeIntervalSince1970: 1_000)

    @Test func endpointIsOffUnlessRequestedAndInvalidPortsAreReported() {
        #expect(DiagnosticsConfiguration.parse(arguments: ["TunaMonitor"], environment: [:]) == .disabled)
        #expect(
            DiagnosticsConfiguration.parse(arguments: ["x", "--diagnostics-port", "50100"], environment: [:])
                == .enabled(port: 50100))
        #expect(
            DiagnosticsConfiguration.parse(arguments: [], environment: ["TUNA_MONITOR_DIAGNOSTICS_PORT": "50101"])
                == .enabled(port: 50101))
        #expect(
            DiagnosticsConfiguration.parse(
                arguments: ["--diagnostics-port", "50100"], environment: ["TUNA_MONITOR_DIAGNOSTICS_PORT": "1"])
                == .enabled(port: 50100))
        guard case .invalid = DiagnosticsConfiguration.parse(arguments: ["--diagnostics-port", "0"], environment: [:])
        else { Issue.record("port 0 accepted"); return }
        guard case .invalid = DiagnosticsConfiguration.parse(arguments: ["--diagnostics-port"], environment: [:])
        else { Issue.record("missing port accepted"); return }
    }

    @Test func snapshotReportsWhatTheWindowAndDockPresentWithoutJobDetails() throws {
        var state = MonitorState()
        var health = FakeBroker.liveHealth()
        health.startedAt = .init(date: Date(timeIntervalSince1970: 2_000))
        state.health = health
        var paused = status(capacity: 2)
        paused.queueMode = .paused
        paused.queueState = .paused
        var running = job("r1", .running)
        running.cwd = "/Users/someone/secret-checkout"
        running.attribution.worklog = "#32"
        var snapshot = snapshotEvent(seq: 4, queue: [job("q1", .queued)], active: [running], status: paused)
        snapshot.snapshot.queue.queueState = .paused
        _ = state.apply(snapshot, at: self.now)
        state.connection = .live
        let expected = Presentation.dockTile(state)
        #expect(expected.light == .stopped)

        let diagnostics = MonitorDiagnostics(
            state: state, brokerPort: 49917, now: self.now,
            monitor: .init(pid: 42, version: "0.1", bundlePath: "/b", startedAt: self.now, diagnosticsPort: 50100),
            dock: .init(installed: true, shown: expected, badgeLabel: expected.badge),
            application: .init(active: false, hidden: false, windows: []))
        #expect(diagnostics.broker.address == "127.0.0.1:49917")
        #expect(diagnostics.broker.serviceVersion == FakeBroker.liveHealth().serviceVersion)
        #expect(diagnostics.broker.startedAt == Date(timeIntervalSince1970: 2_000))
        #expect(diagnostics.connection.state == "live")
        #expect(diagnostics.header.title == Presentation.banner(state, port: 49917, now: self.now).title)
        #expect(diagnostics.header.statusLine == Presentation.statusLine(state))
        #expect(diagnostics.queue.mode == "paused")
        #expect(diagnostics.queue.state == "paused")
        #expect(diagnostics.dock.expected.light == "stopped")
        #expect(diagnostics.dock.expected.badge == "1")
        #expect(diagnostics.dock.matchesExpected)

        let json = String(decoding: try diagnostics.json(), as: UTF8.self)
        #expect(json.contains("\"light\" : \"stopped\""))
        #expect(!json.contains("make r1"))
        #expect(!json.contains("secret-checkout"))
        #expect(!json.contains("\"r1\""))
    }

    @Test func liveQueueModeDistinguishesKnownAndUnknownValues() throws {
        var state = MonitorState()
        state.connection = .live
        #expect(try self.queueModes(state).mode == "unknown")

        var brokerStatus = status()
        let cases: [(Heavylock_V2_QueueMode, String)] = [
            (.enabled, "enabled"), (.paused, "paused"), (.unspecified, "unknown"), (.UNRECOGNIZED(123), "unknown"),
        ]
        for (mode, expected) in cases {
            brokerStatus.queueMode = mode
            state.status = brokerStatus
            let queue = try self.queueModes(state)
            #expect(queue.mode == expected)
            #expect(queue.lastKnownMode == nil)
        }
    }

    @Test func nonLiveQueueModeSeparatesCurrentFromLastKnown() throws {
        var state = MonitorState()
        var brokerStatus = status()
        let connections: [MonitorState.Connection] = [.unavailable("disconnected"), .brokerError("storage failed")]
        for connection in connections {
            state.connection = connection
            state.status = nil
            let noStatus = try self.queueModes(state)
            #expect(noStatus.mode == "unknown")
            #expect(noStatus.lastKnownMode == nil)

            let cases: [(Heavylock_V2_QueueMode, String)] = [
                (.enabled, "enabled"), (.paused, "paused"), (.unspecified, "unknown"),
                (.UNRECOGNIZED(123), "unknown"),
            ]
            for (mode, expected) in cases {
                brokerStatus.queueMode = mode
                state.status = brokerStatus
                let queue = try self.queueModes(state)
                #expect(queue.mode == "unknown")
                #expect(queue.lastKnownMode == expected)
            }
        }
    }

    private struct QueueModes: Decodable {
        struct Queue: Decodable {
            var mode: String?
            var lastKnownMode: String?
        }

        var queue: Queue
    }

    private func queueModes(_ state: MonitorState) throws -> QueueModes.Queue {
        let diagnostics = MonitorDiagnostics(
            state: state, brokerPort: 1, now: self.now,
            monitor: .init(pid: 1, version: nil, bundlePath: "/b", startedAt: self.now, diagnosticsPort: 2),
            dock: .init(installed: false, shown: nil, badgeLabel: nil),
            application: .init(active: false, hidden: false, windows: []))
        return try JSONDecoder().decode(QueueModes.self, from: diagnostics.json()).queue
    }

    @Test func dockMismatchIsReportedWhenAppKitShowsAnOlderTile() {
        var state = MonitorState()
        state.connection = .unavailable("connection refused")
        let stale = Presentation.DockTile(style: .idle, label: "—", otherRunning: 0, badge: nil, light: .go)
        let diagnostics = MonitorDiagnostics(
            state: state, brokerPort: 1, now: self.now,
            monitor: .init(pid: 1, version: nil, bundlePath: "/b", startedAt: self.now, diagnosticsPort: 2),
            dock: .init(installed: true, shown: stale, badgeLabel: nil),
            application: .init(active: false, hidden: false, windows: []))
        #expect(diagnostics.dock.expected.style == "notLive")
        #expect(diagnostics.dock.expected.light == "unknown")
        #expect(!diagnostics.dock.matchesExpected)
        let notInstalled = MonitorDiagnostics(
            state: state, brokerPort: 1, now: self.now,
            monitor: .init(pid: 1, version: nil, bundlePath: "/b", startedAt: self.now, diagnosticsPort: 2),
            dock: .init(installed: false, shown: nil, badgeLabel: nil),
            application: .init(active: false, hidden: false, windows: []))
        #expect(!notInstalled.dock.matchesExpected)
    }

    @Test func requestsAreRefusedUnlessReadOnlyLoopbackDiagnostics() throws {
        func request(_ text: String) throws -> Request { try #require(Request(Data(text.utf8))) }
        #expect(Request(Data("GET /diagnostics HTTP/1.1\r\nHost: 127.0.0.1:9".utf8)) == nil)
        #expect(Response.check(try request("GET /diagnostics HTTP/1.1\r\nHost: 127.0.0.1:9\r\n\r\n"), port: 9) == nil)
        #expect(Response.check(try request("GET /diagnostics?x HTTP/1.1\r\nHost: localhost:9\r\n\r\n"), port: 9) == nil)
        #expect(Response.check(try request("GET /diagnostics HTTP/1.1\r\nHost: evil.example:9\r\n\r\n"), port: 9)?.status == 421)
        #expect(Response.check(try request("GET /diagnostics HTTP/1.1\r\n\r\n"), port: 9)?.status == 421)
        #expect(Response.check(try request("POST /diagnostics HTTP/1.1\r\nHost: 127.0.0.1:9\r\n\r\n"), port: 9)?.status == 405)
        #expect(Response.check(try request("GET / HTTP/1.1\r\nHost: 127.0.0.1:9\r\n\r\n"), port: 9)?.status == 404)
        #expect(Response.check(try request("garbage\r\nHost: 127.0.0.1:9\r\n\r\n"), port: 9)?.status == 400)
    }

    @Test func serverAnswersOverLoopbackAndReportsAPortConflict() async throws {
        let first = try DiagnosticsServer(port: 0) { Data("{\"ok\":true}".utf8) }
        let port = try await Self.start(first)
        defer { first.stop() }
        #expect(port > 0)

        let url = try #require(URL(string: "http://127.0.0.1:\(port)/diagnostics"))
        let (body, response) = try await URLSession.shared.data(from: url)
        let http = try #require(response as? HTTPURLResponse)
        #expect(http.statusCode == 200)
        #expect(http.value(forHTTPHeaderField: "Content-Type") == "application/json")
        #expect(String(decoding: body, as: UTF8.self) == "{\"ok\":true}")

        var post = URLRequest(url: url)
        post.httpMethod = "POST"
        let (_, posted) = try await URLSession.shared.data(for: post)
        #expect((posted as? HTTPURLResponse)?.statusCode == 405)

        let second = try DiagnosticsServer(port: port) { Data() }
        defer { second.stop() }
        do {
            _ = try await Self.start(second)
            Issue.record("a second server bound the same port")
        } catch let error as StartFailure {
            #expect(error.message.contains("in use"))
        }
    }

    struct StartFailure: Error { var message: String }

    /// Starts `server` and returns its port, or throws why it failed.
    private static func start(_ server: DiagnosticsServer) async throws -> UInt16 {
        let resumed = Mutex(false)
        return try await withCheckedThrowingContinuation { continuation in
            server.start { state in
                let result: Result<UInt16, StartFailure>? =
                    switch state {
                    case .listening(let port): .success(port)
                    case .failed(let message): .failure(StartFailure(message: message))
                    default: nil
                    }
                guard let result, resumed.withLock({ done in defer { done = true }; return !done }) else { return }
                continuation.resume(with: result)
            }
        }
    }
}
