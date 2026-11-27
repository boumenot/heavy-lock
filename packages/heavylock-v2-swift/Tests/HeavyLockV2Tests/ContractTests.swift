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

@Test func finishMeasurementsMatchGoEncoding() throws {
    var report = Heavylock_V2_ReportFinishedRequest()
    report.jobID = "job"
    report.grantID = "grant"
    report.process.pid = 42
    report.process.startTime.seconds = 1_764_121_200
    report.process.startTime.nanos = 123_456_789
    report.elapsed = .init()
    report.rootCpuTime.seconds = 1
    report.rootCpuTime.nanos = 234_000_000
    let encoded: Data = try report.serializedBytes()
    #expect(hex(encoded) == "0a036a6f6212056772616e742a0f082a120b08f0b499c90610959aef3a32003a07080110809dca6f")
    let decoded = try Heavylock_V2_ReportFinishedRequest(serializedBytes: encoded)
    #expect(decoded == report)
    #expect(decoded.hasElapsed && decoded.elapsed.seconds == 0 && decoded.elapsed.nanos == 0)
    #expect(decoded.hasRootCpuTime && decoded.rootCpuTime.seconds == 1 && decoded.rootCpuTime.nanos == 234_000_000)
}

@Test func contractNames() {
    #expect(Heavylock_V2_RunnerService.descriptor.fullyQualifiedService == "heavylock.v2.RunnerService")
    #expect(Heavylock_V2_ControlService.descriptor.fullyQualifiedService == "heavylock.v2.ControlService")
    #expect(Heavylock_V2_ErrorReason.jobNotWaiting.rawValue == 5)
    #expect(HeavyLockV2.errorDomain == "heavylock.v2")
}

@Test func serviceIdentityAndVersion() throws {
    var health = Heavylock_V2_Health()
    health.live = true
    health.ready = false
    health.serviceIdentity = "heavylock.v2"
    health.serviceVersion = HeavyLockV2.serviceVersion
    try HeavyLockV2.checkService(health, minimumMinor: 2)

    var wrong = health
    wrong.serviceIdentity = "unrelated"
    #expect(throws: HeavyLockV2.ServiceCompatibilityError.identityMismatch) {
        try HeavyLockV2.checkService(wrong)
    }
    wrong = health
    wrong.serviceVersion = "3.2.0"
    #expect(throws: HeavyLockV2.ServiceCompatibilityError.incompatibleVersion) {
        try HeavyLockV2.checkService(wrong)
    }
    #expect(throws: HeavyLockV2.ServiceCompatibilityError.incompatibleVersion) {
        try HeavyLockV2.checkService(health, minimumMinor: 3)
    }
    wrong = health
    wrong.serviceVersion = ""
    #expect(throws: HeavyLockV2.ServiceCompatibilityError.missingServiceVersion) {
        try HeavyLockV2.checkService(wrong)
    }
    wrong = health
    wrong.live = false
    #expect(throws: HeavyLockV2.ServiceCompatibilityError.notLive) {
        try HeavyLockV2.checkService(wrong)
    }

    for (client, server, minimumMinor, compatible) in [
        ("2.2.0-dev.34", "2.2.0-dev.34", 2, true),
        ("2.2.0-dev.34", "2.2.0-dev.35", 0, false),
        ("2.2.0-dev.34", "2.2.0", 0, false),
        ("2.2.0", "2.2.0-dev.34", 0, false),
        ("2.1.0", "2.3.5", 2, true),
        ("2.1.0", "2.1.9", 2, false),
        ("2.0.0+client", "2.1.0+server", 1, true),
        ("02.0.0", "2.0.0", 0, false),
        ("2.0.0-dev.01", "2.0.0-dev.01", 0, false),
        ("2.0.0", "2.0.0+bad+metadata", 0, false),
    ] as [(String, String, UInt32, Bool)] {
        health.serviceVersion = server
        let result = Result { try HeavyLockV2.checkService(health, clientVersion: client, minimumMinor: minimumMinor) }
        #expect(((try? result.get()) != nil) == compatible)
    }
}
