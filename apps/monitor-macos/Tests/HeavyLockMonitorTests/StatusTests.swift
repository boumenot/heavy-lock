import AppKit
import Foundation
import XCTest
@testable import HeavyLockMonitor

final class StatusTests: XCTestCase {
    private let fixture = """
    {"version":1,"worklog":456,"attribution":"declared","command":["swift","test"],
    "cwd":"/tmp","weight":1,"capacity":2,"state":"running","reason":null,
    "pid":321,"processStart":"1700000000:123","created":"2026-01-01T21:40:00.000Z",
    "stateChanged":"2026-01-01T21:40:00.000Z","heartbeat":"2026-01-01T21:40:00.000Z"}
    """

    func testMonitorUsesConfiguredOrDefaultNeutralGate() {
        let home = "/tmp/example-home"
        let fallback = "/tmp/example-temp"
        XCTAssertEqual(
            MonitorReader.gateURL(environment: ["HOME": home], fallbackDirectory: fallback).path,
            "\(home)/.cache/heavy-lock.lock"
        )
        XCTAssertEqual(
            MonitorReader.gateURL(environment: [:], fallbackDirectory: fallback).path,
            "\(fallback)/.cache/heavy-lock.lock"
        )
        XCTAssertEqual(
            MonitorReader.gateURL(
                environment: ["HOME": home, "HEAVY_LOCK_PATH": "/tmp/custom.lock"],
                fallbackDirectory: fallback
            ).path,
            "/tmp/custom.lock"
        )
    }

    func testReusedPidIsNeverReportedAsLive() throws {
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let snapshot = StatusSnapshot(records: [record], identity: { _ in "1700000000:456" })
        XCTAssertTrue(snapshot.holders.isEmpty)
        XCTAssertEqual(snapshot.staleCount, 1)
    }

    func testUnreadableIdentityIsUnknownRatherThanConfirmedStale() throws {
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let snapshot = StatusSnapshot(records: [record], identity: { _ in nil })
        XCTAssertTrue(snapshot.holders.isEmpty)
        XCTAssertTrue(snapshot.hasUnknownLock)
        XCTAssertEqual(snapshot.staleCount, 0)
        XCTAssertEqual(snapshot.unavailableCount, 1)
    }

    func testMissingProcessIsConfirmedStale() throws {
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let snapshot = StatusSnapshot(records: [record], identity: { _ in nil }, isGone: { _ in true })
        XCTAssertTrue(snapshot.holders.isEmpty)
        XCTAssertEqual(snapshot.staleCount, 1)
        XCTAssertEqual(snapshot.unavailableCount, 0)
    }

    func testConfirmedStaleIsNotAnUnreadableSnapshotButStillWarns() throws {
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let snapshot = StatusSnapshot(records: [record], identity: { _ in nil }, isGone: { _ in true })

        XCTAssertFalse(snapshot.hasUnknownLock)
        XCTAssertEqual(TileState(snapshot: snapshot, legacy: false).style, .unknown)
        let window = formatStatusWindow(
            snapshot: snapshot, legacy: false, recentRuns: [], lineWidth: 350,
            title: { _ in "Title" }, describe: { _, _ in NSAttributedString(string: "") }
        )
        XCTAssertTrue(window.string.contains("STATUS UNKNOWN"))
    }

    func testConcurrentHoldersAreSummarizedWithoutQueuePositions() throws {
        let first = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let secondData = fixture.replacingOccurrences(of: "\"pid\":321", with: "\"pid\":322")
            .replacingOccurrences(of: "\"worklog\":456", with: "\"worklog\":null")
            .replacingOccurrences(of: "\"attribution\":\"declared\"", with: "\"attribution\":\"unknown\"")
        let second = try JSONDecoder().decode(StatusRecord.self, from: Data(secondData.utf8))
        let snapshot = StatusSnapshot(records: [first, second], identity: { _ in "1700000000:123" })
        XCTAssertEqual(snapshot.holders.count, 2)
        XCTAssertEqual(snapshot.extraHolderCount, 1)
        XCTAssertEqual(snapshot.holders.last?.displayWorklog, "#?")
    }

    func testWaitingRecordDoesNotBecomeAHolder() throws {
        let waitingData = fixture.replacingOccurrences(of: "\"running\"", with: "\"waiting\"")
        let waiting = try JSONDecoder().decode(StatusRecord.self, from: Data(waitingData.utf8))
        let snapshot = StatusSnapshot(records: [waiting], identity: { _ in "1700000000:123" })
        XCTAssertTrue(snapshot.holders.isEmpty)
        XCTAssertEqual(snapshot.waiters.count, 1)
    }

    func testReservedSlotForWaitingRunDoesNotMakeHolderUnknown() throws {
        let holder = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let waitingData = fixture.replacingOccurrences(of: "\"running\"", with: "\"waiting\"")
            .replacingOccurrences(of: "\"pid\":321", with: "\"pid\":322")
        let waiting = try JSONDecoder().decode(StatusRecord.self, from: Data(waitingData.utf8))
        let snapshot = StatusSnapshot(records: [holder, waiting], identity: { _ in "1700000000:123" }, slotOwners: [321, 322])
        let tile = TileState(snapshot: snapshot, legacy: false)
        XCTAssertFalse(snapshot.hasUnknownLock)
        XCTAssertEqual(tile.style, .running)
        XCTAssertEqual(tile.label, "#456")
        XCTAssertEqual(snapshot.waiters.count, 1)
    }

    func testMissingMetadataWithHeldSlotsIsNotIdle() {
        let snapshot = StatusSnapshot(records: [], identity: { _ in nil }, slotOwners: [321])
        XCTAssertTrue(snapshot.hasUnknownLock)
    }

    func testUnreadableStatusDoesNotAppearIdle() {
        let snapshot = StatusSnapshot(records: [], identity: { _ in nil }, readError: true)
        XCTAssertTrue(snapshot.hasUnknownLock)
    }

    func testCurrentProcessIdentityIncludesKernelStartTime() {
        XCTAssertNotNil(processStart(getpid()))
        XCTAssertNil(processStart(-1))
        XCTAssertFalse(processGone(getpid()))
        XCTAssertTrue(processGone(Int32.max))
    }

    func testReaderReportsMalformedRecordInsteadOfIdle() throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        try Data("{".utf8).write(to: directory.appendingPathComponent("broken.json"))
        let result = StatusReader.read(directory: directory)
        XCTAssertTrue(result.readError)
        XCTAssertTrue(result.records.isEmpty)
    }

    func testReaderTreatsAbsentDirectoryAsEmpty() {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let result = StatusReader.read(directory: directory)
        XCTAssertFalse(result.readError)
        XCTAssertTrue(result.records.isEmpty)
    }

    func testReadOnlyProbeDetectsLegacyExclusiveLock() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let gate = directory.appendingPathComponent("gate.lock")
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/python3")
        process.arguments = ["-u", "-c",
            "import fcntl,sys; f=open(sys.argv[1],'w'); fcntl.flock(f,fcntl.LOCK_EX); print('ready',flush=True); sys.stdin.read()",
            gate.path]
        let output = Pipe()
        let input = Pipe()
        process.standardOutput = output
        process.standardInput = input
        try process.run()
        defer {
            try? input.fileHandleForWriting.close()
            process.waitUntilExit()
        }
        XCTAssertEqual(String(data: try output.fileHandleForReading.read(upToCount: 6)!, encoding: .utf8), "ready\n")
        XCTAssertEqual(LockProbe.conflict(path: gate.path, requestWrite: false), .held(nil))
        XCTAssertTrue(MonitorReader.read(gate: gate).legacy)
    }

    func testTileShowsOneHolderAndAdditionalCount() throws {
        let first = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let secondData = fixture.replacingOccurrences(of: "\"pid\":321", with: "\"pid\":322")
        let second = try JSONDecoder().decode(StatusRecord.self, from: Data(secondData.utf8))
        let snapshot = StatusSnapshot(records: [first, second], identity: { _ in "1700000000:123" })
        let tile = TileState(snapshot: snapshot, legacy: false)
        XCTAssertEqual(tile.label, "#456")
        XCTAssertEqual(tile.extraHolders, 1)
        XCTAssertEqual(tile.style, .running)
    }

    func testTileDoesNotUseStaleHolderAsItsLabel() throws {
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let snapshot = StatusSnapshot(records: [record], identity: { _ in nil })
        let tile = TileState(snapshot: snapshot, legacy: false)
        XCTAssertEqual(tile.style, .unknown)
        XCTAssertNotEqual(tile.label, "#456")
    }

    func testRunningMetadataWithoutHeldSlotsIsNotReportedAsHolder() throws {
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let snapshot = StatusSnapshot(records: [record], identity: { _ in "1700000000:123" }, slotOwners: [])
        XCTAssertTrue(snapshot.holders.isEmpty)
        XCTAssertTrue(snapshot.hasUnknownLock)
    }

    func testInvalidRecordCannotBecomeALiveHolder() throws {
        let invalid = fixture.replacingOccurrences(of: "\"weight\":1", with: "\"weight\":-1")
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(invalid.utf8))
        let snapshot = StatusSnapshot(records: [record], identity: { _ in "1700000000:123" })
        XCTAssertTrue(snapshot.holders.isEmpty)
        XCTAssertTrue(snapshot.hasUnknownLock)
    }

    func testKnownHolderRemainsVisibleWhenAnotherSlotIsUnattributed() throws {
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let snapshot = StatusSnapshot(records: [record], identity: { _ in "1700000000:123" }, slotOwners: [321, 322])
        let tile = TileState(snapshot: snapshot, legacy: false)
        XCTAssertEqual(tile.style, .unknown)
        XCTAssertEqual(tile.label, "#456")
    }

    func testWaitingOwnerCannotMakeAFormerHolderAppearLive() throws {
        let holder = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let waitingData = fixture.replacingOccurrences(of: "\"running\"", with: "\"waiting\"")
            .replacingOccurrences(of: "\"pid\":321", with: "\"pid\":322")
        let waiting = try JSONDecoder().decode(StatusRecord.self, from: Data(waitingData.utf8))
        let snapshot = StatusSnapshot(records: [holder, waiting], identity: { _ in "1700000000:123" }, slotOwners: [322])
        XCTAssertTrue(snapshot.holders.isEmpty)
        XCTAssertTrue(snapshot.hasUnknownLock)
    }

    func testSlotWithUnknownOwnerDoesNotAppearIdle() {
        let snapshot = StatusSnapshot(records: [], identity: { _ in nil }, slotOwners: [nil])
        XCTAssertTrue(snapshot.hasUnknownLock)
    }

    func testSlotOwnerWithReusedPidIsNotLive() throws {
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let snapshot = StatusSnapshot(records: [record], identity: { _ in "1700000000:456" }, slotOwners: [321])
        XCTAssertTrue(snapshot.holders.isEmpty)
        XCTAssertTrue(snapshot.hasUnknownLock)
    }

    func testReaderRecognizesLiveIdentityWithLockedSlot() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let statusDirectory = directory.appendingPathComponent("gate.status")
        try FileManager.default.createDirectory(at: statusDirectory, withIntermediateDirectories: true)
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/python3")
        process.arguments = ["-u", "-c",
            "import fcntl,sys; f=open(sys.argv[1],'w'); fcntl.lockf(f,fcntl.LOCK_EX); print('ready',flush=True); sys.stdin.read()",
            directory.appendingPathComponent("gate.slot-0.lock").path]
        let output = Pipe()
        let input = Pipe()
        process.standardOutput = output
        process.standardInput = input
        try process.run()
        defer {
            try? input.fileHandleForWriting.close()
            process.waitUntilExit()
        }
        _ = try output.fileHandleForReading.read(upToCount: 6)
        let record = fixture.replacingOccurrences(of: "\"pid\":321", with: "\"pid\":\(process.processIdentifier)")
            .replacingOccurrences(of: "1700000000:123", with: try XCTUnwrap(processStart(process.processIdentifier)))
        try Data(record.utf8).write(to: statusDirectory.appendingPathComponent("live.json"))
        XCTAssertEqual(
            LockProbe.conflict(path: directory.appendingPathComponent("gate.slot-0.lock").path, requestWrite: true),
            .held(process.processIdentifier)
        )
        let result = MonitorReader.read(gate: directory.appendingPathComponent("gate.lock"))
        XCTAssertFalse(result.snapshot.hasUnknownLock)
        XCTAssertEqual(result.snapshot.holders.map(\.displayWorklog), ["#456"])
    }

    func testRunDetailsShowTimestampLabelsAndLinkedBranch() throws {
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let directory = URL(fileURLWithPath: "/tmp/checkout", isDirectory: true)
        let details = formatStatusRecord(
            record, title: "Worklog: Example task", branch: "feature/example",
            directory: directory, elapsed: "2m 14s", role: .holder,
            issueURL: URL(string: "https://github.com/example/cli/issues/456")
        )
        let value = details.string as NSString
        XCTAssertTrue(details.string.contains("[2026-01-01T21:40:00.000Z] #456 Example task"))
        let titleRange = value.range(of: "#456 Example task")
        guard titleRange.location != NSNotFound else { return XCTFail("Missing worklog title") }
        XCTAssertEqual(
            details.attribute(.link, at: titleRange.location, effectiveRange: nil) as? URL,
            URL(string: "https://github.com/example/cli/issues/456")
        )
        XCTAssertEqual(
            details.attribute(.link, at: NSMaxRange(titleRange) - 1, effectiveRange: nil) as? URL,
            URL(string: "https://github.com/example/cli/issues/456")
        )
        XCTAssertTrue(details.string.contains("running · 2m 14s"))
        XCTAssertTrue(details.string.contains("Command: swift test"))
        XCTAssertTrue(details.string.contains("Checkout: feature/example"))
        XCTAssertFalse(details.string.contains(record.cwd))
        let labelRange = value.range(of: "Command")
        guard labelRange.location != NSNotFound else { return XCTFail("Missing command label") }
        let labelFont = try XCTUnwrap(details.attribute(.font, at: labelRange.location, effectiveRange: nil) as? NSFont)
        XCTAssertTrue(labelFont.fontDescriptor.symbolicTraits.contains(.bold))
        let branchRange = value.range(of: "feature/example")
        guard branchRange.location != NSNotFound else { return XCTFail("Missing branch") }
        XCTAssertEqual(details.attribute(.link, at: branchRange.location, effectiveRange: nil) as? URL, directory)
    }

    func testLiveCommandShowsSummaryWithFullCommandTooltip() throws {
        let data = fixture.replacingOccurrences(
            of: "\"command\":[\"swift\",\"test\"]",
            with: "\"command\":[\"fvm\",\"flutter\",\"test\",\"--coverage\"]"
        )
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(data.utf8))
        let details = formatStatusRecord(
            record, title: "Worklog: Test", branch: "main", directory: nil, elapsed: "1m 0s", role: .holder
        )
        let range = (details.string as NSString).range(of: "fvm flutter test …")
        guard range.location != NSNotFound else { return XCTFail("Missing command summary") }
        XCTAssertEqual(
            details.attribute(.toolTip, at: range.location, effectiveRange: nil) as? String,
            "fvm flutter test --coverage"
        )
        XCTAssertEqual(
            details.attribute(.toolTip, at: NSMaxRange(range) - 1, effectiveRange: nil) as? String,
            "fvm flutter test --coverage"
        )
    }

    func testUnavailableCheckoutHasNoLink() throws {
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(fixture.utf8))
        let details = formatStatusRecord(
            record, title: nil, branch: "Branch unavailable", directory: nil, elapsed: "1m 0s", role: .holder
        )
        let branchRange = (details.string as NSString).range(of: "Branch unavailable")
        XCTAssertNotNil(details.string.range(of: "Checkout: Branch unavailable"))
        guard branchRange.location != NSNotFound else { return XCTFail("Missing checkout fallback") }
        XCTAssertNil(details.attribute(.link, at: branchRange.location, effectiveRange: nil))
        let fallbackRange = (details.string as NSString).range(of: "Working directory: Open")
        guard fallbackRange.location != NSNotFound else { return XCTFail("Missing working directory fallback") }
        let openRange = (details.string as NSString).range(of: "Open", options: [], range: fallbackRange)
        XCTAssertEqual(details.attribute(.link, at: openRange.location, effectiveRange: nil) as? URL,
                       URL(fileURLWithPath: record.cwd, isDirectory: true))
    }

    func testDetachedCheckoutIsNotShownAsBranchNamedHead() {
        let checkout = parseCheckout("/tmp/checkout\nHEAD")
        XCTAssertEqual(checkout.branch, "Detached HEAD")
        XCTAssertEqual(checkout.directory, URL(fileURLWithPath: "/tmp/checkout", isDirectory: true))
    }

    func testUnattributedRunHasNoIssueLinkAndUsesMillisecondTimestamp() throws {
        let data = fixture.replacingOccurrences(of: "\"worklog\":456", with: "\"worklog\":null")
            .replacingOccurrences(of: "\"attribution\":\"declared\"", with: "\"attribution\":\"unknown\"")
            .replacingOccurrences(of: "21:40:00.000Z", with: "21:40:00.123456Z")
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(data.utf8))
        let details = formatStatusRecord(
            record, title: nil, branch: "main", directory: nil, elapsed: "1m 0s", role: .waiter
        )
        XCTAssertTrue(details.string.contains("[2026-01-01T21:40:00.123Z] #? Unattributed"))
        let range = (details.string as NSString).range(of: "#?")
        XCTAssertNil(details.attribute(.link, at: range.location, effectiveRange: nil))
    }

    func testCheckoutLookupLinksToRepositoryRootFromNestedDirectory() {
        let loaded = expectation(description: "checkout metadata loaded")
        let cache = MetadataCache { loaded.fulfill() }
        let tests = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
        let root = tests.deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent()
        _ = cache.checkout(tests.path)
        wait(for: [loaded], timeout: 10)
        let checkout = cache.checkout(tests.path)
        XCTAssertNotEqual(checkout.branch, "Branch unavailable")
        XCTAssertNotEqual(checkout.branch, "Loading branch…")
        XCTAssertEqual(checkout.directory?.standardizedFileURL, root.standardizedFileURL)
    }

    func testStatusTextUsesVisualHierarchyWithoutLosingContent() {
        let text = formatStatusText([
            ("HOLDING (1)", .section),
            ("#456  Worklog", .holder),
            ("  Command: swift test", .body),
            ("STATUS UNKNOWN", .warning),
        ])
        XCTAssertEqual(text.string, "HOLDING (1)\n#456  Worklog\n  Command: swift test\nSTATUS UNKNOWN")
        let content = text.string as NSString
        let bodyIndex = content.range(of: "  Command").location
        let bodyFont = text.attribute(.font, at: bodyIndex, effectiveRange: nil) as? NSFont
        let holderIndex = content.range(of: "#456").location
        let holderFont = text.attribute(.font, at: holderIndex, effectiveRange: nil) as? NSFont
        XCTAssertGreaterThan(holderFont?.pointSize ?? 0, bodyFont?.pointSize ?? 0)
        let warningIndex = content.range(of: "STATUS UNKNOWN").location
        let holderColor = text.attribute(.foregroundColor, at: holderIndex, effectiveRange: nil) as? NSColor
        let warningColor = text.attribute(.foregroundColor, at: warningIndex, effectiveRange: nil) as? NSColor
        let bodyColor = text.attribute(.foregroundColor, at: bodyIndex, effectiveRange: nil) as? NSColor
        XCTAssertNotEqual(holderColor, bodyColor)
        XCTAssertNotEqual(warningColor, bodyColor)
    }

    func testStatusAccentsRemainLegibleInLightAndDarkAppearance() throws {
        let text = formatStatusText([("holder", .holder), ("warning", .warning)])
        let warningIndex = (text.string as NSString).range(of: "warning").location
        let colors = [
            try XCTUnwrap(text.attribute(.foregroundColor, at: 0, effectiveRange: nil) as? NSColor),
            try XCTUnwrap(text.attribute(.foregroundColor, at: warningIndex, effectiveRange: nil) as? NSColor),
        ]
        for name in [NSAppearance.Name.aqua, .darkAqua] {
            let appearance = try XCTUnwrap(NSAppearance(named: name))
            appearance.performAsCurrentDrawingAppearance {
                func luminance(_ color: NSColor) -> Double {
                    let rgb = color.usingColorSpace(.sRGB)!
                    func linear(_ value: CGFloat) -> Double {
                        let component = Double(value)
                        return component <= 0.04045 ? component / 12.92 : pow((component + 0.055) / 1.055, 2.4)
                    }
                    return 0.2126 * linear(rgb.redComponent) + 0.7152 * linear(rgb.greenComponent) + 0.0722 * linear(rgb.blueComponent)
                }
                let background = luminance(.textBackgroundColor)
                for color in colors {
                    let foreground = luminance(color)
                    let contrast = (max(background, foreground) + 0.05) / (min(background, foreground) + 0.05)
                    XCTAssertGreaterThanOrEqual(contrast, 4.5, "\(name.rawValue): \(contrast)")
                }
            }
        }
    }

    func testStatusTextTracksWindowWidth() {
        let scroll = makeStatusScrollView(frame: NSRect(x: 0, y: 0, width: 720, height: 520))
        scroll.frame.size.width = 420
        scroll.layoutSubtreeIfNeeded()
        let text = scroll.documentView as? NSTextView
        XCTAssertEqual(text?.textContainer?.widthTracksTextView, true)
        XCTAssertEqual(text?.frame.width ?? 0, scroll.contentSize.width, accuracy: 1)
    }

    func testMainMenuProvidesStandardQuitShortcut() {
        let menu = makeApplicationMenu()
        let quit = menu.items.first?.submenu?.items.first
        XCTAssertEqual(quit?.keyEquivalent, "q")
        XCTAssertEqual(quit?.action, #selector(NSApplication.terminate(_:)))
    }

    func testMainMenuProvidesCopyAndSelectAllShortcuts() throws {
        let menu = makeApplicationMenu()
        let edit = try XCTUnwrap(menu.items.first { $0.title == "Edit" }?.submenu)
        let copy = try XCTUnwrap(edit.items.first { $0.title == "Copy" })
        let selectAll = try XCTUnwrap(edit.items.first { $0.title == "Select All" })
        XCTAssertEqual(copy.keyEquivalent, "c")
        XCTAssertEqual(copy.action, #selector(NSText.copy(_:)))
        XCTAssertEqual(selectAll.keyEquivalent, "a")
        XCTAssertEqual(selectAll.action, #selector(NSText.selectAll(_:)))
    }

    func testCopyingCommandSummaryPreservesSurroundingTextAndFullArguments() throws {
        let data = fixture.replacingOccurrences(
            of: "\"command\":[\"swift\",\"test\"]",
            with: "\"command\":[\"python3\",\"tool/run_checks.py\",\"--suite\",\"abc def\"]"
        )
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(data.utf8))
        let details = formatStatusRecord(
            record, title: "Worklog: Test", branch: "main", directory: nil, elapsed: "1m 0s", role: .holder
        )
        let scroll = makeStatusScrollView(frame: NSRect(x: 0, y: 0, width: 720, height: 520))
        let text = try XCTUnwrap(scroll.documentView as? StatusTextView)
        text.textStorage?.setAttributedString(details)
        let range = (details.string as NSString).range(of: "Command: run_checks.py")
        guard range.location != NSNotFound else { return XCTFail("Missing script summary") }
        text.setSelectedRange(range)

        let pasteboard = NSPasteboard.withUniqueName()
        defer { pasteboard.releaseGlobally() }
        XCTAssertEqual(text.writeSelection(to: pasteboard), true)
        XCTAssertEqual(
            pasteboard.string(forType: .string),
            "Command: python3 tool/run_checks.py --suite 'abc def'"
        )
    }

    func testCopyingPartOfSummaryExpandsToFullCommand() throws {
        let data = fixture.replacingOccurrences(
            of: "\"command\":[\"swift\",\"test\"]",
            with: "\"command\":[\"fvm\",\"flutter\",\"test\",\"--coverage\"]"
        )
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(data.utf8))
        let details = formatStatusRecord(
            record, title: nil, branch: "main", directory: nil, elapsed: "1m 0s", role: .holder
        )
        let scroll = makeStatusScrollView(frame: NSRect(x: 0, y: 0, width: 720, height: 520))
        let text = try XCTUnwrap(scroll.documentView as? StatusTextView)
        text.textStorage?.setAttributedString(details)
        let range = (details.string as NSString).range(of: "fvm flutter test …")
        guard range.location != NSNotFound else { return XCTFail("Missing command summary") }
        text.setSelectedRange(NSRange(location: range.location + 4, length: 7))

        let pasteboard = NSPasteboard.withUniqueName()
        defer { pasteboard.releaseGlobally() }
        XCTAssertEqual(text.writeSelection(to: pasteboard), true)
        XCTAssertEqual(pasteboard.string(forType: .string), "fvm flutter test --coverage")
    }

    func testRefreshingStatusPreservesTheSelectedCommand() throws {
        let data = fixture.replacingOccurrences(
            of: "\"command\":[\"swift\",\"test\"]",
            with: "\"command\":[\"fvm\",\"flutter\",\"test\",\"--coverage\"]"
        )
        let record = try JSONDecoder().decode(StatusRecord.self, from: Data(data.utf8))
        func details(_ elapsed: String) -> NSAttributedString {
            formatStatusRecord(record, title: nil, branch: "main", directory: nil, elapsed: elapsed, role: .holder)
        }
        let text = try XCTUnwrap(
            makeStatusScrollView(frame: NSRect(x: 0, y: 0, width: 720, height: 520)).documentView as? StatusTextView
        )
        let before = details("9m 9s")
        text.replaceStatus(before)
        let original = (before.string as NSString).range(of: "fvm flutter test …")
        text.setSelectedRange(original)
        let after = details("10m 0s")
        text.replaceStatus(after)
        XCTAssertEqual(text.selectedRange(), (after.string as NSString).range(of: "fvm flutter test …"))

        let pasteboard = NSPasteboard.withUniqueName()
        defer { pasteboard.releaseGlobally() }
        XCTAssertEqual(text.writeSelection(to: pasteboard), true)
        XCTAssertEqual(pasteboard.string(forType: .string), "fvm flutter test --coverage")
    }

    func testRefreshDoesNotSelectADifferentCommandWithTheSameSummary() throws {
        func details(_ option: String) throws -> NSAttributedString {
            let data = fixture.replacingOccurrences(
                of: "\"command\":[\"swift\",\"test\"]",
                with: "\"command\":[\"fvm\",\"flutter\",\"test\",\"\(option)\"]"
            )
            let record = try JSONDecoder().decode(StatusRecord.self, from: Data(data.utf8))
            return formatStatusRecord(record, title: nil, branch: "main", directory: nil, elapsed: "1m 0s", role: .holder)
        }
        let first = try details("--coverage")
        let second = try details("--no-pub")
        let before = NSMutableAttributedString(attributedString: first)
        before.append(second)
        let text = try XCTUnwrap(
            makeStatusScrollView(frame: NSRect(x: 0, y: 0, width: 720, height: 520)).documentView as? StatusTextView
        )
        text.replaceStatus(before)
        text.setSelectedRange((before.string as NSString).range(of: "fvm flutter test …"))
        text.replaceStatus(second)
        XCTAssertEqual(text.selectedRange().length, 0)
    }

    func testSelectAllRemainsSelectedAcrossARefresh() throws {
        let text = try XCTUnwrap(
            makeStatusScrollView(frame: NSRect(x: 0, y: 0, width: 720, height: 520)).documentView as? StatusTextView
        )
        text.replaceStatus(NSAttributedString(string: "old status"))
        text.selectAll(nil)
        text.replaceStatus(NSAttributedString(string: "updated status is longer"))
        XCTAssertEqual(text.selectedRange(), NSRange(location: 0, length: 24))
    }

    func testReadsRealRunnerAndItsLiveSlot() throws {
        guard let dart = ProcessInfo.processInfo.environment["HEAVY_LOCK_MONITOR_DART"] else {
            throw XCTSkip("Set HEAVY_LOCK_MONITOR_DART to the Dart executable for runner integration")
        }
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let root = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent()
        let gate = directory.appendingPathComponent("gate.lock")
        let release = directory.appendingPathComponent("release")
        let process = Process()
        process.executableURL = URL(fileURLWithPath: dart)
        let package = root.appendingPathComponent("apps/heavy-lock")
        process.currentDirectoryURL = package
        process.arguments = [package.appendingPathComponent("bin/heavy_lock.dart").path,
            "--worklog", "456", "--", "/bin/sh", "-c",
            "while [ ! -f \"$1\" ]; do sleep 0.1; done", "job", release.path]
        var env = ProcessInfo.processInfo.environment
        env["HEAVY_LOCK_PATH"] = gate.path
        env["HEAVY_LOCK_HELD"] = ""
        env["HEAVY_LOCK_CAPACITY"] = "2"
        env["HEAVY_LOCK_PRIORITY"] = "normal"
        env.removeValue(forKey: "HEAVY_LOCK_WORKLOG")
        process.environment = env
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        try process.run()
        var released = false
        defer {
            if !released {
                _ = FileManager.default.createFile(atPath: release.path, contents: Data())
                process.waitUntilExit()
            }
        }
        let deadline = Date().addingTimeInterval(15)
        var snapshot = MonitorReader.read(gate: gate).snapshot
        while snapshot.holders.isEmpty && process.isRunning && Date() < deadline {
            Thread.sleep(forTimeInterval: 0.05)
            snapshot = MonitorReader.read(gate: gate).snapshot
        }
        XCTAssertEqual(snapshot.holders.map(\.displayWorklog), ["#456"])
        XCTAssertFalse(snapshot.hasUnknownLock)
        let history = RecentRuns()
        history.observe(snapshot, at: Date())
        XCTAssertTrue(FileManager.default.createFile(atPath: release.path, contents: Data()))
        process.waitUntilExit()
        released = true
        XCTAssertEqual(process.terminationStatus, 0)
        history.observe(MonitorReader.read(gate: gate).snapshot, at: Date())
        XCTAssertEqual(history.entries.map(\.record.worklog), [456])
    }
}
