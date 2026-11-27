import Foundation
import Network

/// A minimal read-only HTTP/1.1 server bound to 127.0.0.1 that answers
/// `GET /diagnostics` with JSON from `snapshot` and refuses everything else.
/// It never listens on other interfaces, rejects requests whose `Host` is
/// not loopback (DNS rebinding) and closes each connection after one reply.
public final class DiagnosticsServer: @unchecked Sendable {
    public enum State: Equatable, Sendable {
        case starting
        case listening(port: UInt16)
        case failed(String)
        case stopped
    }

    public static let path = "/diagnostics"
    static let maximumRequestBytes = 8192
    static let requestTimeout: DispatchTimeInterval = .seconds(5)

    private let listener: NWListener
    private let queue = DispatchQueue(label: "com.boumenot.tuna-monitor.diagnostics")
    private let snapshot: @Sendable () async throws -> Data

    /// `port` 0 picks a free port; the monitor itself always uses an
    /// explicit port.
    public init(port: UInt16, snapshot: @escaping @Sendable () async throws -> Data) throws {
        let parameters = NWParameters.tcp
        parameters.allowLocalEndpointReuse = false
        parameters.requiredLocalEndpoint = .hostPort(host: .ipv4(.loopback), port: NWEndpoint.Port(rawValue: port)!)
        self.listener = try NWListener(using: parameters)
        self.snapshot = snapshot
    }

    /// Starts listening; `onState` reports the bound port or why listening failed.
    public func start(onState: @escaping @Sendable (State) -> Void) {
        onState(.starting)
        self.listener.stateUpdateHandler = { [listener] state in
            switch state {
            case .ready: onState(.listening(port: listener.port?.rawValue ?? 0))
            case .failed(let error):
                onState(.failed(Self.describe(error)))
                listener.cancel()
            case .waiting(let error):
                // Waiting means the address could not be bound now; report it
                // instead of silently retrying.
                onState(.failed(Self.describe(error)))
                listener.cancel()
            case .cancelled: break
            default: break
            }
        }
        self.listener.newConnectionHandler = { [weak self] connection in self?.accept(connection) }
        self.listener.start(queue: self.queue)
    }

    public func stop() {
        self.listener.cancel()
    }

    private static func describe(_ error: NWError) -> String {
        if case .posix(.EADDRINUSE) = error { return "127.0.0.1 port is already in use" }
        return String(describing: error)
    }

    // MARK: Connections

    private func accept(_ connection: NWConnection) {
        connection.start(queue: self.queue)
        self.queue.asyncAfter(deadline: .now() + Self.requestTimeout) { [weak connection] in
            connection?.cancel()
        }
        self.receive(connection, buffer: Data())
    }

    private func receive(_ connection: NWConnection, buffer: Data) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: Self.maximumRequestBytes) {
            [weak self] data, _, complete, error in
            guard let self else { return }
            var buffer = buffer
            if let data { buffer.append(data) }
            if let request = Request(buffer) {
                self.respond(to: request, on: connection)
            } else if buffer.count > Self.maximumRequestBytes {
                self.send(Response(status: 431, text: "Request header too large\n"), on: connection)
            } else if complete || error != nil {
                connection.cancel()
            } else {
                self.receive(connection, buffer: buffer)
            }
        }
    }

    private func respond(to request: Request, on connection: NWConnection) {
        let port = self.listener.port?.rawValue ?? 0
        if let response = Response.check(request, port: port) {
            self.send(response, on: connection)
            return
        }
        let snapshot = self.snapshot
        Task {
            let response: Response
            do {
                response = Response(status: 200, contentType: "application/json", body: try await snapshot())
            } catch {
                response = Response(status: 500, text: "Snapshot failed: \(error)\n")
            }
            self.queue.async { self.send(response, on: connection) }
        }
    }

    private func send(_ response: Response, on connection: NWConnection) {
        connection.send(content: response.encoded(), completion: .contentProcessed { _ in connection.cancel() })
    }
}

/// The request line and headers of one HTTP request; nil until complete.
struct Request: Equatable {
    var method: String
    var target: String
    var headers: [String: String]

    init?(_ data: Data) {
        guard let end = data.range(of: Data("\r\n\r\n".utf8)),
            let head = String(data: data[..<end.lowerBound], encoding: .utf8)
        else { return nil }
        let lines = head.components(separatedBy: "\r\n")
        let parts = lines[0].split(separator: " ", omittingEmptySubsequences: false)
        self.method = parts.count == 3 ? String(parts[0]) : ""
        self.target = parts.count == 3 ? String(parts[1]) : ""
        var headers: [String: String] = [:]
        for line in lines.dropFirst() {
            guard let colon = line.firstIndex(of: ":") else { continue }
            headers[line[..<colon].lowercased()] = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
        }
        self.headers = headers
    }

    var path: String { String(self.target.prefix { $0 != "?" }) }
}

struct Response: Equatable {
    var status: Int
    var contentType: String
    var body: Data
    var extraHeaders: [(String, String)] = []

    init(status: Int, contentType: String, body: Data, extraHeaders: [(String, String)] = []) {
        self.status = status
        self.contentType = contentType
        self.body = body
        self.extraHeaders = extraHeaders
    }

    init(status: Int, text: String, extraHeaders: [(String, String)] = []) {
        self.init(
            status: status, contentType: "text/plain; charset=utf-8", body: Data(text.utf8), extraHeaders: extraHeaders)
    }

    static func == (lhs: Self, rhs: Self) -> Bool {
        lhs.status == rhs.status && lhs.contentType == rhs.contentType && lhs.body == rhs.body
            && lhs.extraHeaders.map { "\($0.0): \($0.1)" } == rhs.extraHeaders.map { "\($0.0): \($0.1)" }
    }

    /// The refusal for `request`, or nil when it should get the snapshot.
    static func check(_ request: Request, port: UInt16) -> Response? {
        guard !request.method.isEmpty else { return Response(status: 400, text: "Bad request\n") }
        let allowedHosts: Set<String> = ["127.0.0.1:\(port)", "localhost:\(port)"]
        guard let host = request.headers["host"]?.lowercased(), allowedHosts.contains(host) else {
            return Response(status: 421, text: "Host must be 127.0.0.1:\(port) or localhost:\(port)\n")
        }
        guard request.path == DiagnosticsServer.path else {
            return Response(status: 404, text: "Not found; use GET \(DiagnosticsServer.path)\n")
        }
        guard request.method == "GET" else {
            return Response(status: 405, text: "Read-only; use GET\n", extraHeaders: [("Allow", "GET")])
        }
        return nil
    }

    func encoded() -> Data {
        let reason =
            [200: "OK", 400: "Bad Request", 404: "Not Found", 405: "Method Not Allowed", 421: "Misdirected Request",
             431: "Request Header Fields Too Large", 500: "Internal Server Error"][self.status] ?? "Error"
        var head = "HTTP/1.1 \(self.status) \(reason)\r\n"
        head += "Content-Type: \(self.contentType)\r\nContent-Length: \(self.body.count)\r\n"
        head += "Cache-Control: no-store\r\nConnection: close\r\n"
        for (name, value) in self.extraHeaders { head += "\(name): \(value)\r\n" }
        return Data((head + "\r\n").utf8) + self.body
    }
}
