import AppKit

final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate {
    private let gate: URL
    private let tileView = DockTileView(frame: NSRect(x: 0, y: 0, width: 128, height: 128))
    private let recentRuns = RecentRuns()
    private var window: NSWindow?
    private var textView: StatusTextView?
    private var parentWatcher: DirectoryWatcher?
    private var statusWatcher: DirectoryWatcher?
    private var timer: Timer?
    private var current = MonitorReader.read(gate: URL(fileURLWithPath: "/nonexistent-heavy-lock-gate"))
    private lazy var metadata = MetadataCache { [weak self] in self?.updateDetails() }

    override init() {
        let env = ProcessInfo.processInfo.environment
        gate = MonitorReader.gateURL(environment: env, fallbackDirectory: NSTemporaryDirectory())
        super.init()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.regular)
        NSApp.mainMenu = makeApplicationMenu()
        NSApp.dockTile.contentView = tileView
        installWatchers()
        refresh()
        timer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
            self?.installWatchers()
            self?.refresh()
        }
        showWindow(nil)
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        showWindow(nil)
        return true
    }

    func applicationDockMenu(_ sender: NSApplication) -> NSMenu? {
        let menu = NSMenu()
        addHeading("HOLDING THE LOCK", to: menu)
        if current.legacy { addHeading("Legacy exclusive holder", to: menu) }
        if current.snapshot.holders.isEmpty && !current.legacy { addHeading("No known holders", to: menu) }
        for holder in current.snapshot.holders {
            addHeading("\(holder.displayWorklog) · \(duration(holder.stateChanged)) · weight \(holder.weight)/\(holder.capacity)", to: menu)
        }
        menu.addItem(.separator())
        addHeading("WAITING (\(current.snapshot.waiters.count)) — NOT AN ORDER", to: menu)
        for waiter in current.snapshot.waiters {
            addHeading("\(waiter.displayWorklog) · \(duration(waiter.stateChanged)) · \(waiter.reason ?? "waiting")", to: menu)
        }
        if current.snapshot.hasUnknownStatus { addHeading("Some status is unavailable or stale", to: menu) }
        menu.addItem(.separator())
        menu.addItem(withTitle: "Show Status Window", action: #selector(showWindow(_:)), keyEquivalent: "")
        return menu
    }

    private func addHeading(_ title: String, to menu: NSMenu) {
        let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        item.isEnabled = false
        menu.addItem(item)
    }

    @objc private func showWindow(_ sender: Any?) {
        if window == nil {
            let window = NSWindow(
                contentRect: NSRect(x: 0, y: 0, width: 720, height: 520),
                styleMask: [.titled, .closable, .miniaturizable, .resizable],
                backing: .buffered, defer: false
            )
            window.title = "Heavy Lock Status"
            window.center()
            window.isReleasedWhenClosed = false
            window.delegate = self
            let scroll = makeStatusScrollView(frame: window.contentView!.bounds)
            window.contentView = scroll
            self.window = window
            textView = scroll.documentView as? StatusTextView
        }
        updateDetails()
        window?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func installWatchers() {
        if parentWatcher == nil {
            parentWatcher = DirectoryWatcher(url: gate.deletingLastPathComponent()) { [weak self] in
                self?.statusWatcher = nil
                self?.installWatchers()
                self?.refresh()
            }
        }
        if statusWatcher == nil {
            statusWatcher = DirectoryWatcher(url: MonitorReader.statusDirectory(gate: gate)) { [weak self] in
                self?.refresh()
            }
        }
    }

    private func refresh() {
        current = MonitorReader.read(gate: gate)
        recentRuns.observe(current.snapshot, at: Date())
        tileView.tile = TileState(snapshot: current.snapshot, legacy: current.legacy)
        NSApp.dockTile.badgeLabel = current.snapshot.waiters.isEmpty ? nil : String(current.snapshot.waiters.count)
        NSApp.dockTile.display()
        updateDetails()
    }

    private func updateDetails() {
        guard let textView else { return }
        let padding = textView.textContainer?.lineFragmentPadding ?? 0
        let lineWidth = max(0, textView.bounds.width - 2 * textView.textContainerInset.width - 2 * padding)
        let text = formatStatusWindow(
            snapshot: current.snapshot, legacy: current.legacy, recentRuns: recentRuns.entries, lineWidth: lineWidth,
            title: { self.metadata.issue($0).title }, issueURL: { self.metadata.issue($0).url },
            describe: { self.describe($0, role: $1) }
        )
        textView.replaceStatus(text)
    }

    func windowDidResize(_ notification: Notification) {
        updateDetails()
    }

    private func describe(_ record: StatusRecord, role: StatusTextRole) -> NSAttributedString {
        let issue = metadata.issue(record)
        let checkout = metadata.checkout(record.cwd)
        return formatStatusRecord(record, title: issue.title, branch: checkout.branch, directory: checkout.directory,
                                  elapsed: duration(record.stateChanged), role: role, issueURL: issue.url)
    }

    private func duration(_ timestamp: String) -> String {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        guard let date = formatter.date(from: timestamp) else { return "time unavailable" }
        let seconds = max(0, Int(Date().timeIntervalSince(date)))
        return "\(seconds / 60)m \(seconds % 60)s"
    }
}
