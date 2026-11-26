import Darwin
import Foundation
import HeavyLockV2
import Testing

@testable import TunaMonitorCore

/// Drives the monitor's gRPC client and session against a real broker. Opt
/// in with `TUNA_MONITOR_TEST_TUNA=<path to a built tuna>`; the broker gets a
/// temporary data directory and a free port, so it never touches the user's
/// broker. Cancelling running work is left to the recovery worklog.
@MainActor
@Suite(
    .enabled(
        if: ProcessInfo.processInfo.environment["TUNA_MONITOR_TEST_TUNA"] != nil,
        "set TUNA_MONITOR_TEST_TUNA to a built tuna executable"
    ),
    .serialized
)
struct BrokerIntegrationTests {
    @Test func monitorsAndControlsARealBroker() async throws {
        let tuna = try #require(ProcessInfo.processInfo.environment["TUNA_MONITOR_TEST_TUNA"])
        let dataDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("tuna-monitor-test-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dataDir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dataDir) }
        let port = try freePort()
        let broker = try launch(tuna, ["service", "--port", "\(port)", "--data-dir", dataDir.path])
        var runners: [Process] = []
        defer {
            for process in runners + [broker] where process.isRunning {
                process.terminate()
                process.waitUntilExit()
            }
        }

        let session = MonitorSession(client: try GRPCBrokerClient(port: port), reconnectDelay: .milliseconds(100))
        session.start()
        defer { session.stop() }
        #expect(await waitFor(seconds: 15) { session.state.isLive })
        let capacity = try #require(session.state.status?.capacity)
        #expect(capacity > 0)

        session.request(.pause)
        #expect(await waitFor { session.state.status?.paused == true })

        runners.append(try launch(tuna, ["--port", "\(port)", "--", "/bin/sleep", "30"]))
        #expect(await waitFor { session.state.queue.count == 1 })
        runners.append(try launch(tuna, ["--port", "\(port)", "--weight", "\(capacity)", "--", "/usr/bin/true"]))
        #expect(await waitFor { session.state.queue.count == 2 })
        let queued = try #require(session.state.queue.first { $0.command.contains("sleep") })
        let forced = try #require(session.state.queue.first { $0.command.contains("true") })
        #expect(ControlAction.jobActions(for: forced).count == 4)

        session.request(.moveToBack(JobReference(queued)))
        #expect(await waitFor { session.state.queue.last?.jobID == queued.jobID })

        session.request(.forceStart(JobReference(forced)))
        #expect(session.pendingConfirmation != nil)
        session.confirmPending()
        #expect(await waitFor { session.state.recent.contains { $0.jobID == forced.jobID && $0.forced } })

        session.request(.cancel(JobReference(queued)))
        session.confirmPending()
        #expect(
            await waitFor {
                session.state.recent.contains {
                    $0.jobID == queued.jobID && $0.outcome.kind == .cancelledBeforeStart
                }
            })
        #expect(session.state.queue.isEmpty)

        session.request(.resume)
        #expect(await waitFor { session.state.status?.paused == false })
        #expect(session.state.controlError == nil)

        broker.terminate()
        broker.waitUntilExit()
        #expect(await waitFor(seconds: 30) { !session.state.isLive })
        #expect(Presentation.banner(session.state, port: port, now: Date()).detail?.contains("not live") == true)
    }
}

@MainActor
private func waitFor(seconds: Int = 10, _ condition: () -> Bool) async -> Bool {
    let deadline = ContinuousClock.now + .seconds(seconds)
    while ContinuousClock.now < deadline {
        if condition() { return true }
        try? await Task.sleep(for: .milliseconds(20))
    }
    return condition()
}

private func launch(_ executable: String, _ arguments: [String]) throws -> Process {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: executable)
    process.arguments = arguments
    process.standardOutput = FileHandle.nullDevice
    try process.run()
    return process
}

/// A loopback port that was free a moment ago.
private func freePort() throws -> Int {
    let fd = socket(AF_INET, SOCK_STREAM, 0)
    guard fd >= 0 else { throw POSIXError(.EIO) }
    defer { close(fd) }
    var address = sockaddr_in()
    address.sin_family = sa_family_t(AF_INET)
    address.sin_addr.s_addr = inet_addr("127.0.0.1")
    address.sin_port = 0
    var length = socklen_t(MemoryLayout<sockaddr_in>.size)
    let bound = withUnsafeMutablePointer(to: &address) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            bind(fd, $0, length) == 0 && getsockname(fd, $0, &length) == 0
        }
    }
    guard bound else { throw POSIXError(.EADDRINUSE) }
    return Int(UInt16(bigEndian: address.sin_port))
}
