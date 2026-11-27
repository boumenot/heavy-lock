import Foundation
import HeavyLockV2

/// Whether the opt-in, read-only diagnostics endpoint should listen:
/// `--diagnostics-port N`, then `TUNA_MONITOR_DIAGNOSTICS_PORT`, otherwise off.
/// A value that is not a port is reported rather than ignored.
public enum DiagnosticsConfiguration: Equatable, Sendable {
    case disabled
    case enabled(port: UInt16)
    case invalid(String)

    public static let argument = "--diagnostics-port"
    public static let environmentVariable = "TUNA_MONITOR_DIAGNOSTICS_PORT"

    public static func parse(arguments: [String], environment: [String: String]) -> Self {
        if let index = arguments.firstIndex(of: Self.argument) {
            guard index + 1 < arguments.count else { return .invalid("\(Self.argument) needs a port") }
            return Self.port(arguments[index + 1], source: Self.argument)
        }
        if let text = environment[Self.environmentVariable] {
            return Self.port(text, source: Self.environmentVariable)
        }
        return .disabled
    }

    private static func port(_ text: String, source: String) -> Self {
        guard let port = Int(text), (1...65535).contains(port) else {
            return .invalid("\(source) must be a port from 1 to 65535, not \"\(text)\"")
        }
        return .enabled(port: UInt16(port))
    }
}

/// The JSON served at `/diagnostics`: what this monitor instance presents,
/// derived from the same `MonitorState` and `Presentation` functions as the
/// window and Dock tile, plus the Dock tile and window state read back from
/// AppKit. It is not a capture of displayed pixels, carries no job
/// commands, arguments, working directories or history, and offers no
/// controls.
public struct MonitorDiagnostics: Encodable, Equatable, Sendable {
    public static let schemaVersion = 1
    public static let note =
        "Presentation state derived from the monitor's model and read back from AppKit; not a capture of displayed pixels."

    public struct Monitor: Encodable, Equatable, Sendable {
        public var pid: Int32
        public var version: String?
        public var bundlePath: String
        public var startedAt: Date
        public var diagnosticsPort: UInt16

        public init(pid: Int32, version: String?, bundlePath: String, startedAt: Date, diagnosticsPort: UInt16) {
            self.pid = pid
            self.version = version
            self.bundlePath = bundlePath
            self.startedAt = startedAt
            self.diagnosticsPort = diagnosticsPort
        }
    }

    public struct Broker: Encodable, Equatable, Sendable {
        /// The broker this monitor is configured to watch.
        public var address: String
        /// From the last compatible `Health`, if any.
        public var serviceIdentity: String?
        public var serviceVersion: String?
        public var apiVersion: String?
        public var startedAt: Date?
    }

    public struct Connection: Encodable, Equatable, Sendable {
        /// `connecting`, `live`, `unavailable`, `incompatible` or `brokerError`.
        public var state: String
        public var message: String?
        public var live: Bool
        public var hasSnapshot: Bool
        public var lastEventSeq: Int64
        public var lastUpdate: Date?
    }

    /// The window header's text, as `MonitorView` renders it.
    public struct Header: Encodable, Equatable, Sendable {
        public var title: String
        public var detail: String?
        /// `normal`, `warning` or `error`.
        public var tone: String
        public var statusLine: String?
        public var notice: String?
        public var controlError: String?
    }

    public struct Queue: Encodable, Equatable, Sendable {
        /// `enabled` or `paused` from the last status; nil before any.
        public var mode: String?
        /// The state the status line and stop light show.
        public var state: String
        public var running: Int
        public var queued: Int
        public var recentlyFinished: Int
    }

    public struct DockTile: Encodable, Equatable, Sendable {
        /// `idle`, `running` or `notLive` (the ring).
        public var style: String
        public var label: String
        public var otherRunning: Int
        public var badge: String?
        /// `go` (green), `stalled` (amber), `stopped` (red) or `unknown` (dark).
        public var light: String

        public init(_ tile: Presentation.DockTile) {
            self.style =
                switch tile.style {
                case .idle: "idle"
                case .running: "running"
                case .notLive: "notLive"
                }
            self.label = tile.label
            self.otherRunning = tile.otherRunning
            self.badge = tile.badge
            self.light =
                switch tile.light {
                case .go: "go"
                case .stalled: "stalled"
                case .stopped: "stopped"
                case .unknown: "unknown"
                }
        }
    }

    /// The Dock tile as AppKit holds it, read back when the snapshot is taken.
    public struct DockReport: Encodable, Equatable, Sendable {
        /// Whether the monitor's tile view is the Dock tile's content view.
        public var installed: Bool
        /// The tile the installed view will draw; nil if not installed.
        public var shown: DockTile?
        /// `NSDockTile.badgeLabel`.
        public var badgeLabel: String?

        public init(installed: Bool, shown: Presentation.DockTile?, badgeLabel: String?) {
            self.installed = installed
            self.shown = shown.map(DockTile.init)
            self.badgeLabel = badgeLabel
        }
    }

    public struct Dock: Encodable, Equatable, Sendable {
        /// The tile `Presentation.dockTile` gives for the current state.
        public var expected: DockTile
        public var installed: Bool
        public var shown: DockTile?
        public var badgeLabel: String?
        /// Whether AppKit holds the expected tile and badge. False briefly
        /// while an update is pending, or if the Dock is not being updated.
        public var matchesExpected: Bool
    }

    public struct Window: Encodable, Equatable, Sendable {
        public var title: String
        public var visible: Bool
        /// `NSWindow.occlusionState` contains `.visible`.
        public var unoccluded: Bool
        public var miniaturized: Bool
        public var key: Bool

        public init(title: String, visible: Bool, unoccluded: Bool, miniaturized: Bool, key: Bool) {
            self.title = title
            self.visible = visible
            self.unoccluded = unoccluded
            self.miniaturized = miniaturized
            self.key = key
        }
    }

    public struct Application: Encodable, Equatable, Sendable {
        public var active: Bool
        public var hidden: Bool
        public var windows: [Window]

        public init(active: Bool, hidden: Bool, windows: [Window]) {
            self.active = active
            self.hidden = hidden
            self.windows = windows
        }
    }

    public var schemaVersion: Int
    public var note: String
    public var generatedAt: Date
    public var monitor: Monitor
    public var broker: Broker
    public var connection: Connection
    public var header: Header
    public var queue: Queue
    public var dock: Dock
    public var application: Application

    public init(
        state: MonitorState, brokerPort: Int, now: Date, monitor: Monitor, dock: DockReport,
        application: Application
    ) {
        self.schemaVersion = Self.schemaVersion
        self.note = Self.note
        self.generatedAt = now
        self.monitor = monitor
        let health = state.health
        self.broker = Broker(
            address: "127.0.0.1:\(brokerPort)",
            serviceIdentity: health?.serviceIdentity,
            serviceVersion: health?.serviceVersion,
            apiVersion: health.map { "\($0.apiMajor).\($0.apiMinor)" },
            startedAt: health.flatMap {
                $0.startedAtUnixNs > 0 ? Date(timeIntervalSince1970: Double($0.startedAtUnixNs) / 1e9) : nil
            })
        let (name, message): (String, String?) =
            switch state.connection {
            case .connecting: ("connecting", nil)
            case .live: ("live", nil)
            case .unavailable(let text): ("unavailable", text)
            case .incompatible(let text): ("incompatible", text)
            case .brokerError(let text): ("brokerError", text)
            }
        self.connection = Connection(
            state: name, message: message, live: state.isLive, hasSnapshot: state.hasSnapshot,
            lastEventSeq: state.lastEventSeq, lastUpdate: state.lastUpdate)
        let banner = Presentation.banner(state, port: brokerPort, now: now)
        let tone =
            switch banner.tone {
            case .normal: "normal"
            case .warning: "warning"
            case .error: "error"
            }
        self.header = Header(
            title: banner.title, detail: banner.detail, tone: tone,
            statusLine: Presentation.statusLine(state), notice: state.notice, controlError: state.controlError)
        self.queue = Queue(
            mode: state.status.map { $0.queueMode == .paused ? "paused" : "enabled" },
            state: Presentation.queueStateName(state.currentQueueState).lowercased(),
            running: state.runningCount, queued: state.queuedCount, recentlyFinished: state.recent.count)
        let expected = DockTile(Presentation.dockTile(state))
        self.dock = Dock(
            expected: expected, installed: dock.installed, shown: dock.shown, badgeLabel: dock.badgeLabel,
            matchesExpected: dock.installed && dock.shown == expected && dock.badgeLabel == expected.badge)
        self.application = application
    }

    public func json() throws -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        encoder.dateEncodingStrategy = .iso8601
        return try encoder.encode(self)
    }
}
