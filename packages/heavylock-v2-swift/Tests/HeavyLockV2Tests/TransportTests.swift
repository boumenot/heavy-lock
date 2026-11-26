import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2TransportServices
import GRPCProtobuf
import HeavyLockV2
import Testing

private enum ProbeError: Error {
    case invalidPort
    case missingEvent
}

@Test(.enabled(
    if: ProcessInfo.processInfo.environment["TUNA_TEST_SWIFT_PORT"] != nil,
    "Run through the shared-port Go integration probe"
))
func swiftGRPCSharedPort() async throws {
    guard let portText = ProcessInfo.processInfo.environment["TUNA_TEST_SWIFT_PORT"],
        let port = Int(portText), (1...65535).contains(port)
    else {
        throw ProbeError.invalidPort
    }

    try await withGRPCClient(
        transport: .http2NIOTS(
            target: .ipv4(host: "127.0.0.1", port: port),
            transportSecurity: .plaintext
        )
    ) { client in
        let control = Heavylock_V2_ControlService.Client(wrapping: client)
        let health = try await control.getHealth(Heavylock_V2_GetHealthRequest())
        try HeavyLockV2.checkService(health)
        #expect(!health.ready)

        let paused = try await control.pauseQueue(Heavylock_V2_PauseQueueRequest())
        #expect(paused.paused)
        let url = try #require(URL(string: "http://127.0.0.1:\(port)/v1/status"))
        let (body, response) = try await URLSession.shared.data(from: url)
        #expect((response as? HTTPURLResponse)?.statusCode == 200)
        let json = try #require(JSONSerialization.jsonObject(with: body) as? [String: Any])
        #expect(json["paused"] as? Bool == true)

        let sequence = try await control.watchEvents(Heavylock_V2_WatchEventsRequest()) { response in
            for try await event in response.messages {
                return event.eventSeq
            }
            throw ProbeError.missingEvent
        }
        #expect(sequence == 42)

        var cancel = Heavylock_V2_CancelJobRequest()
        cancel.jobID = "job-1"
        do {
            _ = try await control.cancelJob(cancel)
            Issue.record("Expected a structured cancellation error")
        } catch let error as RPCError {
            #expect(error.code == .failedPrecondition)
            let details = try error.unpackGoogleRPCStatus()
            #expect(details?.details.first?.errorInfo?.domain == "heavylock.v2")
            #expect(details?.details.first?.errorInfo?.reason == "JOB_NOT_WAITING")
        }
    }
}
