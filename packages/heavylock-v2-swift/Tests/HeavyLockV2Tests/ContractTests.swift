import Foundation
import HeavyLockV2
import Testing

// The Go bindings test in apps/tuna/gen/heavylock/v2 asserts the same bytes.
private let submitJobHex =
    "0a2431623461323765632d376537352d346333612d396436322d33323236326538613836343812096461727420746573"
    + "741a042f72657020022a0d0a0474756e611205302e302e3132150a0c626f756d656e6f742f7265702a056275696c643a"
    + "260a2430663162633533632d396135612d343165652d613232622d643462363139303734666233"

private func sampleSubmitJob() -> Heavylock_V2_SubmitJobRequest {
    var request = Heavylock_V2_SubmitJobRequest()
    request.idempotencyKey = "1b4a27ec-7e75-4c3a-9d62-32262e8a8648"
    request.command = "dart test"
    request.cwd = "/rep"
    request.weight = 2
    request.client.name = "tuna"
    request.client.version = "0.0.1"
    request.attribution.repository = "boumenot/rep"
    request.attribution.purpose = "build"
    request.runner.incarnationID = "0f1bc53c-9a5a-41ee-a22b-d4b619074fb3"
    return request
}

private func hex(_ data: Data) -> String {
    data.map { String(format: "%02x", $0) }.joined()
}

@Test func submitJobMatchesGoEncoding() throws {
    let encoded: Data = try sampleSubmitJob().serializedBytes()
    #expect(hex(encoded) == submitJobHex)
    let decoded = try Heavylock_V2_SubmitJobRequest(serializedBytes: encoded)
    #expect(decoded == sampleSubmitJob())
    #expect(!decoded.hasRetryOfJobID)
}

@Test func contractNames() {
    #expect(Heavylock_V2_RunnerService.descriptor.fullyQualifiedService == "heavylock.v2.RunnerService")
    #expect(Heavylock_V2_ControlService.descriptor.fullyQualifiedService == "heavylock.v2.ControlService")
    #expect(Heavylock_V2_ErrorReason.jobNotWaiting.rawValue == 5)
    #expect(HeavyLockV2.errorDomain == "heavylock.v2")
}
