import Foundation
import HeavyLockV2
import Observation

/// Keeps `state` synchronized with the broker: checks health, follows
/// `WatchEvents`, resumes after disconnects, refreshes from a snapshot after
/// a replay gap and sends controls, confirming those that require it.
@MainActor
@Observable
public final class MonitorSession {
    public private(set) var state = MonitorState()
    /// A control waiting for the user's confirmation; nothing is sent until
    /// `confirmPending()`.
    public private(set) var pendingConfirmation: ControlAction?
    /// Controls sent and not yet answered.
    public private(set) var inFlight: Set<ControlAction> = []

    @ObservationIgnored private let client: any BrokerClient
    @ObservationIgnored private let reconnectDelay: Duration
    @ObservationIgnored private let minimumHeartbeatTimeout: Duration
    @ObservationIgnored private var task: Task<Void, Never>?
    /// The most recently sent control; each control waits for the previous
    /// one so the broker receives them in the order they were requested.
    @ObservationIgnored private var lastControl: Task<Void, Never>?
    @ObservationIgnored private var lastMessage = ContinuousClock.now
    @ObservationIgnored private var heartbeatInterval: Duration = .zero
    @ObservationIgnored private var timedOut: Duration?

    public init(
        client: any BrokerClient,
        reconnectDelay: Duration = .seconds(2),
        minimumHeartbeatTimeout: Duration = .seconds(15)
    ) {
        self.client = client
        self.reconnectDelay = reconnectDelay
        self.minimumHeartbeatTimeout = minimumHeartbeatTimeout
    }

    public func start() {
        guard self.task == nil else { return }
        self.task = Task { [weak self] in await self?.run() }
    }

    public func stop() {
        self.task?.cancel()
        self.task = nil
    }

    // MARK: Controls

    /// Sends a routine control at once; a cancellation or force-start waits
    /// for `confirmPending()`.
    public func request(_ action: ControlAction) {
        guard self.state.isLive else {
            self.state.controlError = "The broker is not live; \(action.name) was not sent."
            return
        }
        if action.confirmation != nil {
            self.pendingConfirmation = action
            return
        }
        self.send(action)
    }

    public func confirmPending() {
        guard let action = self.pendingConfirmation else { return }
        self.pendingConfirmation = nil
        guard self.state.isLive else {
            self.state.controlError = "The broker is no longer live; \(action.name) was not sent."
            return
        }
        self.send(action)
    }

    public func dismissPending() {
        self.pendingConfirmation = nil
    }

    private func send(_ action: ControlAction) {
        self.state.controlError = nil
        self.inFlight.insert(action)
        let previous = self.lastControl
        self.lastControl = Task { [client] in
            await previous?.value
            do {
                try await client.perform(action)
                self.inFlight.remove(action)
            } catch {
                self.inFlight.remove(action)
                let title = action.name
                self.state.controlError = "\(title) failed: \(BrokerFailure.from(error))"
            }
        }
    }

    // MARK: Connection loop

    private enum Restart: Error {
        case gap
        case streamEnded
        case heartbeatTimeout(Duration)
        case invalidHeartbeat
    }

    private func run() async {
        while !Task.isCancelled {
            do {
                let health = try await self.client.health()
                do {
                    // Minor 1 adds the explicit queue mode and state.
                    try HeavyLockV2.checkService(health, minimumMinor: 1)
                } catch {
                    self.state.connection = .incompatible(Self.describe(error))
                    try await Task.sleep(for: self.reconnectDelay)
                    continue
                }
                self.state.health = health
                if health.hasStorageDetail {
                    // Failed storage is sticky until the broker restarts and
                    // it cannot stream events, so poll health instead.
                    self.state.connection = .brokerError(health.storageDetail)
                    try await Task.sleep(for: self.reconnectDelay)
                    continue
                }
                try await self.watch()
                throw Restart.streamEnded
            } catch {
                if Task.isCancelled { return }
                if let restart = error as? Restart {
                    switch restart {
                    case .gap:
                        self.state.requireSnapshot(notice: "Missed broker events; refreshed from a new snapshot.")
                        self.markUnavailable("missed events; refreshing")
                        continue
                    case .streamEnded:
                        self.markUnavailable("the broker closed the event stream")
                    case .heartbeatTimeout(let timeout):
                        self.markUnavailable("no heartbeat from the broker for \(timeout)")
                    case .invalidHeartbeat:
                        self.markUnavailable("the broker sent an invalid heartbeat interval")
                    }
                } else if let failure = error as? BrokerFailure, failure.isEventsExpired {
                    self.state.requireSnapshot(
                        notice: "Event replay expired; refreshed from a new snapshot. Intermediate changes are not shown."
                    )
                    self.markUnavailable("event replay expired; refreshing")
                    continue
                } else {
                    let failure = BrokerFailure.from(error)
                    self.markUnavailable(
                        failure.code == "unavailable" && failure.reason == nil
                            ? "cannot connect; is `tuna service` running?" : failure.description)
                }
            }
            do {
                try await Task.sleep(for: self.reconnectDelay)
            } catch {
                return
            }
        }
    }

    private func markUnavailable(_ message: String) {
        self.state.connection = .unavailable(message)
    }

    private func watch() async throws {
        let stream = self.client.events(after: self.state.lastEventSeq)
        self.lastMessage = .now
        self.timedOut = nil
        let consumer = Task { @MainActor [weak self] in
            for try await event in stream {
                guard let self else { return }
                try await self.receive(event)
            }
        }
        let watchdog = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .milliseconds(100))
                guard let self else { return }
                let timeout = self.heartbeatTimeout
                if ContinuousClock.now - self.lastMessage > timeout {
                    self.timedOut = timeout
                    consumer.cancel()
                    return
                }
            }
        }
        defer {
            consumer.cancel()
            watchdog.cancel()
        }
        do {
            try await withTaskCancellationHandler {
                try await consumer.value
            } onCancel: {
                consumer.cancel()
            }
        } catch {
            if let timeout = self.timedOut { throw Restart.heartbeatTimeout(timeout) }
            throw error
        }
        if let timeout = self.timedOut { throw Restart.heartbeatTimeout(timeout) }
        try Task.checkCancellation()
    }

    private func receive(_ event: Heavylock_V2_BrokerEvent) async throws {
        self.lastMessage = .now
        if case .heartbeat(let heartbeat) = event.event, heartbeat.hasHeartbeatInterval {
            let interval = heartbeat.heartbeatInterval
            guard interval.seconds >= 0, interval.seconds <= 315_576_000_000,
                interval.nanos >= 0, interval.nanos < 1_000_000_000
            else { throw Restart.invalidHeartbeat }
            if interval.seconds > 0 || interval.nanos > 0 {
                self.heartbeatInterval = .seconds(interval.seconds) + .nanoseconds(Int64(interval.nanos))
            }
        }
        let wasSnapshot: Bool
        if case .snapshot = event.event { wasSnapshot = true } else { wasSnapshot = false }
        switch self.state.apply(event, at: Date()) {
        case .gap:
            throw Restart.gap
        case .applied, .duplicate:
            break
        }
        if wasSnapshot {
            await self.refreshRecent()
        }
        if !self.state.isLive {
            self.state.connection = .live
        }
    }

    private func refreshRecent() async {
        if let jobs = try? await self.client.recentFinishedJobs(limit: UInt32(MonitorState.recentLimit)) {
            self.state.setRecent(jobs)
        }
    }

    /// Three missed broker heartbeats, but never less than the minimum.
    private var heartbeatTimeout: Duration {
        max(self.minimumHeartbeatTimeout, self.heartbeatInterval * 3)
    }

    private static func describe(_ error: any Error) -> String {
        switch error as? HeavyLockV2.ServiceCompatibilityError {
        case .identityMismatch: "the service on this port is not a heavy-lock v2 broker"
        case .incompatibleVersion: "the broker's API version is not supported by this monitor"
        case .missingServiceVersion: "the broker did not report its version"
        case .notLive: "the broker reports that it is not live"
        case nil: BrokerFailure.from(error).description
        }
    }
}
