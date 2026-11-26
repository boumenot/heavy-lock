import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2TransportServices
import GRPCProtobuf
import HeavyLockV2

/// The broker operations the monitor uses. The gRPC implementation talks to
/// a `tuna service` broker; tests substitute a scripted client.
public protocol BrokerClient: Sendable {
    func health() async throws -> Heavylock_V2_Health
    /// Broker events after `afterEventSeq`; 0 starts with a fresh snapshot.
    func events(after afterEventSeq: Int64) -> AsyncThrowingStream<Heavylock_V2_BrokerEvent, any Error>
    /// The newest retained finished jobs, newest first.
    func recentFinishedJobs(limit: UInt32) async throws -> [Heavylock_V2_Job]
    func perform(_ action: ControlAction) async throws
}

/// A broker or transport failure reduced to what the monitor presents.
public struct BrokerFailure: Error, Equatable, Sendable, CustomStringConvertible {
    /// The gRPC status code name, e.g. `unavailable`.
    public var code: String
    /// The `heavylock.v2` `ErrorInfo` reason, e.g. `EVENTS_EXPIRED`, when present.
    public var reason: String?
    public var message: String

    public init(code: String, reason: String? = nil, message: String) {
        self.code = code
        self.reason = reason
        self.message = message
    }

    public var isEventsExpired: Bool { self.reason == "EVENTS_EXPIRED" }

    public var description: String {
        let detail = self.message.isEmpty ? self.code : self.message
        if let reason = self.reason { return "\(reason): \(detail)" }
        return detail
    }

    public static func from(_ error: any Error) -> BrokerFailure {
        if let failure = error as? BrokerFailure { return failure }
        if let rpc = error as? RPCError {
            var reason: String?
            if let status = try? rpc.unpackGoogleRPCStatus() {
                for detail in status.details {
                    if let info = detail.errorInfo, info.domain == HeavyLockV2.errorDomain {
                        reason = info.reason
                        break
                    }
                }
            }
            return BrokerFailure(code: String(describing: rpc.code), reason: reason, message: rpc.message)
        }
        if error is CancellationError {
            return BrokerFailure(code: "cancelled", message: "request cancelled")
        }
        return BrokerFailure(code: "error", message: String(describing: error))
    }
}

/// Plaintext gRPC to the broker on the IPv4 loopback address. The broker API
/// is unauthenticated; any local process can reach the same port.
public final class GRPCBrokerClient: BrokerClient {
    public static let defaultPort = 49783

    private let grpc: GRPCClient<HTTP2ClientTransport.TransportServices>
    private let control: Heavylock_V2_ControlService.Client<HTTP2ClientTransport.TransportServices>
    private let connections: Task<Void, Never>
    private let unaryOptions: CallOptions

    public init(port: Int = GRPCBrokerClient.defaultPort, timeout: Duration = .seconds(5)) throws {
        let transport = try HTTP2ClientTransport.TransportServices(
            target: .ipv4(address: "127.0.0.1", port: port),
            transportSecurity: .plaintext
        )
        let grpc = GRPCClient(transport: transport)
        self.grpc = grpc
        self.control = Heavylock_V2_ControlService.Client(wrapping: grpc)
        self.connections = Task { try? await grpc.runConnections() }
        var options = CallOptions.defaults
        options.timeout = timeout
        self.unaryOptions = options
    }

    deinit {
        self.grpc.beginGracefulShutdown()
    }

    public func health() async throws -> Heavylock_V2_Health {
        try await self.call { try await self.control.getHealth(Heavylock_V2_GetHealthRequest(), options: self.unaryOptions) }
    }

    public func recentFinishedJobs(limit: UInt32) async throws -> [Heavylock_V2_Job] {
        var request = Heavylock_V2_ListJobsRequest()
        request.states = [.finished]
        request.pageSize = limit
        let listRequest = request
        return try await self.call { try await self.control.listJobs(listRequest, options: self.unaryOptions).jobs }
    }

    public func perform(_ action: ControlAction) async throws {
        try await self.call {
            switch action {
            case .pause:
                _ = try await self.control.pauseQueue(Heavylock_V2_PauseQueueRequest(), options: self.unaryOptions)
            case .resume:
                _ = try await self.control.resumeQueue(Heavylock_V2_ResumeQueueRequest(), options: self.unaryOptions)
            case .moveToFront(let job):
                var request = Heavylock_V2_MoveJobRequest()
                request.jobID = job.id
                request.toFront = true
                _ = try await self.control.moveJob(request, options: self.unaryOptions)
            case .moveToBack(let job):
                var request = Heavylock_V2_MoveJobRequest()
                request.jobID = job.id
                request.toBack = true
                _ = try await self.control.moveJob(request, options: self.unaryOptions)
            case .forceStart(let job):
                var request = Heavylock_V2_ForceStartJobRequest()
                request.jobID = job.id
                _ = try await self.control.forceStartJob(request, options: self.unaryOptions)
            case .cancel(let job):
                var request = Heavylock_V2_CancelJobRequest()
                request.jobID = job.id
                request.reason = ControlAction.cancelReason
                _ = try await self.control.cancelJob(request, options: self.unaryOptions)
            }
        }
    }

    public func events(after afterEventSeq: Int64) -> AsyncThrowingStream<Heavylock_V2_BrokerEvent, any Error> {
        let control = self.control
        return AsyncThrowingStream { continuation in
            let task = Task {
                var request = Heavylock_V2_WatchEventsRequest()
                request.afterEventSeq = afterEventSeq
                do {
                    try await control.watchEvents(request) { response in
                        for try await event in response.messages {
                            continuation.yield(event)
                        }
                    }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: BrokerFailure.from(error))
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    private func call<T: Sendable>(_ body: @Sendable () async throws -> T) async throws -> T {
        do {
            return try await body()
        } catch {
            throw BrokerFailure.from(error)
        }
    }
}
