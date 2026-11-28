import Foundation

/// Constants shared by every Tuna client. The protobuf messages and
/// gRPC clients in this module are generated from docs/tuna.proto.
public enum TunaContract {
    /// The `google.rpc.ErrorInfo` domain attached to every non-OK status.
    public static let errorDomain = "tuna"
    public static let serviceVersion = "3.0.0-dev.3"

    public enum ServiceCompatibilityError: Error, Equatable {
        case identityMismatch
        case incompatibleVersion
        case missingServiceVersion
        case notLive
    }

    public static func checkService(
        _ health: Tuna_Health,
        clientVersion: String = TunaContract.serviceVersion,
        minimumMinor: UInt32 = 0
    ) throws {
        guard health.serviceIdentity == Self.errorDomain else {
            throw ServiceCompatibilityError.identityMismatch
        }
        guard !health.serviceVersion.isEmpty else {
            throw ServiceCompatibilityError.missingServiceVersion
        }
        guard let client = Self.version(clientVersion), let server = Self.version(health.serviceVersion),
            client.major == server.major, server.minor >= UInt64(minimumMinor),
            !(client.prerelease || server.prerelease) || clientVersion == health.serviceVersion
        else {
            throw ServiceCompatibilityError.incompatibleVersion
        }
        guard health.live else {
            throw ServiceCompatibilityError.notLive
        }
    }

    private struct Version {
        let major: UInt64
        let minor: UInt64
        let prerelease: Bool
    }

    private static func version(_ text: String) -> Version? {
        let build = text.split(separator: "+", maxSplits: 1, omittingEmptySubsequences: false)
        if build.count == 2, !Self.identifiers(build[1], rejectLeadingZeros: false) { return nil }
        let prerelease = build[0].split(separator: "-", maxSplits: 1, omittingEmptySubsequences: false)
        if prerelease.count == 2, !Self.identifiers(prerelease[1], rejectLeadingZeros: true) { return nil }
        let core = prerelease[0].split(separator: ".", omittingEmptySubsequences: false)
        guard core.count == 3, core.allSatisfy({ Self.number($0) }),
            let major = UInt64(core[0]), let minor = UInt64(core[1])
        else { return nil }
        return Version(major: major, minor: minor, prerelease: prerelease.count == 2)
    }

    private static func number(_ text: Substring) -> Bool {
        !text.isEmpty && (text.count == 1 || text.first != "0")
            && text.utf8.allSatisfy { (48...57).contains($0) }
    }

    private static func identifiers(_ text: Substring, rejectLeadingZeros: Bool) -> Bool {
        text.split(separator: ".", omittingEmptySubsequences: false).allSatisfy { identifier in
            let bytes = identifier.utf8
            return !identifier.isEmpty
                && bytes.allSatisfy { (48...57).contains($0) || (65...90).contains($0) || (97...122).contains($0) || $0 == 45 }
                && !(rejectLeadingZeros && bytes.allSatisfy { (48...57).contains($0) }
                    && identifier.count > 1 && identifier.first == "0")
        }
    }
}
