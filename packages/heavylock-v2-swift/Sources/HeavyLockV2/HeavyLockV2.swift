import Foundation

/// Constants shared by every heavy-lock v2 client. The protobuf messages and
/// gRPC clients in this module are generated from docs/v2/heavylock.proto.
public enum HeavyLockV2 {
    /// The `google.rpc.ErrorInfo` domain attached to every non-OK status.
    public static let errorDomain = "heavylock.v2"

    public enum ServiceCompatibilityError: Error, Equatable {
        case identityMismatch
        case incompatibleVersion
        case missingServiceVersion
        case notLive
    }

    public static func checkService(
        _ health: Heavylock_V2_Health,
        minimumMinor: UInt32 = 0
    ) throws {
        guard health.serviceIdentity == "heavylock.v2" else {
            throw ServiceCompatibilityError.identityMismatch
        }
        guard health.apiMajor == 2, health.apiMinor >= minimumMinor else {
            throw ServiceCompatibilityError.incompatibleVersion
        }
        guard !health.serviceVersion.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw ServiceCompatibilityError.missingServiceVersion
        }
        guard health.live else {
            throw ServiceCompatibilityError.notLive
        }
    }
}
