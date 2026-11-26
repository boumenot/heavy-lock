/// Constants shared by every heavy-lock v2 client. The protobuf messages and
/// gRPC clients in this module are generated from docs/v2/heavylock.proto.
public enum HeavyLockV2 {
    /// The `google.rpc.ErrorInfo` domain attached to every non-OK status.
    public static let errorDomain = "heavylock.v2"
}
