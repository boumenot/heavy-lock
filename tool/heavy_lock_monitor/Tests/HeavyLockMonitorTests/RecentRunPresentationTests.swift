import AppKit
import Foundation
import XCTest
@testable import HeavyLockMonitor

final class RecentRunPresentationTests: XCTestCase {
    private func record(
        _ pid: Int32, worklog: Int? = 123, command: [String] = ["fvm", "flutter", "test", "--coverage"],
        cwd: String = "/tmp"
    ) -> StatusRecord {
        StatusRecord(
            version: 1, worklog: worklog, attribution: worklog == nil ? "unknown" : "declared",
            command: command, cwd: cwd, weight: 1, capacity: 2, state: .running,
            reason: nil, pid: pid, processStart: "\(pid):1", created: "1970-01-01T00:00:00.000Z",
            stateChanged: "1970-01-01T00:00:00.000Z", heartbeat: "1970-01-01T00:00:00.000Z"
        )
    }

    func testStatusWindowRemovesSubtitleAndKeepsWaitingHeadingAndEmptyHistory() {
        let snapshot = StatusSnapshot(records: [], identity: { _ in nil })
        let text = formatStatusWindow(
            snapshot: snapshot, legacy: false, recentRuns: [], lineWidth: 350,
            title: { _ in "Title" }, describe: { _, _ in NSAttributedString(string: "") }
        )
        XCTAssertTrue(text.string.hasPrefix("HOLDING (0)"))
        XCTAssertTrue(text.string.contains("WAITING (0) — NOT AN ORDER"))
        XCTAssertTrue(text.string.contains("RECENTLY ENDED (0 of last 100)"))
        XCTAssertTrue(text.string.contains("No runs observed ending yet."))
        XCTAssertFalse(text.string.contains("Read-only"))
    }

    func testRecentRowStartsWithTheLocalObservedEndTime() {
        let observedEnd = Date(timeIntervalSince1970: 760)
        let run = RecentRun(record: record(1), observedEnd: observedEnd)
        let formatter = DateFormatter()
        formatter.timeStyle = .short
        formatter.dateStyle = .none
        let row = formatRecentRun(run, title: "Worklog: Title", lineWidth: 600)
        XCTAssertTrue(row.string.hasPrefix(formatter.string(from: observedEnd) + "  "))
    }

    func testRecentRowKeepsCommandVisibleAndLinksTruncatedTitle() throws {
        let run = RecentRun(record: record(1), observedEnd: Date(timeIntervalSince1970: 760))
        let row = formatRecentRun(run, title: "Worklog: " + String(repeating: "A", count: 160), lineWidth: 350,
                                  issueURL: URL(string: "https://github.com/example/cli/issues/123"))
        XCTAssertFalse(row.string.contains("\n"))
        XCTAssertFalse(row.string.contains("Worklog:"))
        XCTAssertTrue(row.string.contains("~12m 40s"))
        XCTAssertTrue(row.string.contains("… · fvm flutter test …"))

        let content = row.string as NSString
        let issueRange = content.range(of: "#123")
        let commandRange = content.range(of: "fvm flutter test …")
        guard issueRange.location != NSNotFound, commandRange.location != NSNotFound else {
            return XCTFail("Missing issue or command summary")
        }
        XCTAssertEqual(
            row.attribute(.link, at: issueRange.location, effectiveRange: nil) as? URL,
            URL(string: "https://github.com/example/cli/issues/123")
        )
        XCTAssertEqual(
            row.attribute(.toolTip, at: commandRange.location, effectiveRange: nil) as? String,
            "fvm flutter test --coverage"
        )
        let paragraph = try XCTUnwrap(row.attribute(.paragraphStyle, at: 0, effectiveRange: nil) as? NSParagraphStyle)
        XCTAssertEqual(paragraph.lineBreakMode, NSLineBreakMode.byTruncatingTail)

        let storage = NSTextStorage(attributedString: row)
        let layout = NSLayoutManager()
        let container = NSTextContainer(containerSize: NSSize(width: 350, height: 1000))
        container.lineFragmentPadding = 0
        storage.addLayoutManager(layout)
        layout.addTextContainer(container)
        layout.ensureLayout(for: container)
        let glyphs = layout.glyphRange(for: container)
        var lineCount = 0
        layout.enumerateLineFragments(forGlyphRange: glyphs) { _, _, _, _, _ in lineCount += 1 }
        XCTAssertEqual(lineCount, 1)
        let hidden = layout.truncatedGlyphRange(inLineFragmentForGlyphAt: 0)
        let commandGlyphs = layout.glyphRange(forCharacterRange: commandRange, actualCharacterRange: nil)
        XCTAssertEqual(NSIntersectionRange(hidden, commandGlyphs).length, 0)
    }

    func testUnattributedRecentRowHasNoIssueLink() {
        let run = RecentRun(record: record(1, worklog: nil), observedEnd: Date(timeIntervalSince1970: 2))
        let row = formatRecentRun(run, title: nil, lineWidth: 600)
        let range = (row.string as NSString).range(of: "#? Unattributed")
        guard range.location != NSNotFound else { return XCTFail("Missing unattributed heading") }
        XCTAssertNil(row.attribute(.link, at: range.location, effectiveRange: nil))
    }

    func testDeclaredWorklogWithoutResolvedRepositoryHasNoGuessedLink() {
        let run = RecentRun(record: record(1), observedEnd: Date(timeIntervalSince1970: 2))
        let row = formatRecentRun(run, title: "Title unavailable", lineWidth: 600)
        let range = (row.string as NSString).range(of: "#123")
        XCTAssertNotEqual(range.location, NSNotFound)
        XCTAssertNil(row.attribute(.link, at: range.location, effectiveRange: nil))
    }

    func testMetadataSeparatesSameIssueNumberInDifferentCheckouts() throws {
        var calls: [String] = []
        let cache = MetadataCache(changed: {}, runner: { executable, arguments, cwd, completion in
            XCTAssertEqual(executable, "gh")
            XCTAssertEqual(arguments, ["issue", "view", "123", "--json", "title,url"])
            guard let cwd else { return XCTFail("Missing issue working directory") }
            calls.append(cwd)
            let repo = cwd == "/tmp/first" ? "first" : "second"
            completion(#"{"title":"\#(repo)","url":"https://github.com/example/\#(repo)/issues/123"}"#)
        })
        let first = record(1, cwd: "/tmp/first")
        let second = record(2, cwd: "/tmp/second")
        _ = cache.issue(first)
        _ = cache.issue(second)
        XCTAssertEqual(cache.issue(first).title, "first")
        XCTAssertEqual(cache.issue(second).title, "second")
        XCTAssertEqual(cache.issue(first).url, URL(string: "https://github.com/example/first/issues/123"))
        XCTAssertEqual(cache.issue(second).url, URL(string: "https://github.com/example/second/issues/123"))
        XCTAssertEqual(calls, ["/tmp/first", "/tmp/second"])
    }

    func testMetadataFailureDoesNotInventAnIssueURL() {
        let cache = MetadataCache(changed: {}, runner: { _, _, _, completion in completion(nil) })
        _ = cache.issue(record(1))
        XCTAssertEqual(cache.issue(record(1)).title, "Title unavailable")
        XCTAssertNil(cache.issue(record(1)).url)
    }

    func testNarrowHistoryRowKeepsAVisibleCopyableCommandSummary() throws {
        let run = RecentRun(record: record(1), observedEnd: Date(timeIntervalSince1970: 760))
        let row = formatRecentRun(run, title: "Worklog: Long title for this issue", lineWidth: 240)
        var commandRange = NSRange(location: NSNotFound, length: 0)
        row.enumerateAttribute(.fullCommand, in: NSRange(location: 0, length: row.length)) { value, range, _ in
            if value != nil { commandRange = range }
        }
        guard commandRange.location != NSNotFound else { return XCTFail("Missing command summary") }
        XCTAssertTrue((row.string as NSString).substring(with: commandRange).hasPrefix("fvm"))
        XCTAssertEqual(
            row.attribute(.toolTip, at: commandRange.location, effectiveRange: nil) as? String,
            "fvm flutter test --coverage"
        )

        let storage = NSTextStorage(attributedString: row)
        let layout = NSLayoutManager()
        let container = NSTextContainer(containerSize: NSSize(width: 240, height: 1000))
        container.lineFragmentPadding = 0
        storage.addLayoutManager(layout)
        layout.addTextContainer(container)
        layout.ensureLayout(for: container)
        let hidden = layout.truncatedGlyphRange(inLineFragmentForGlyphAt: 0)
        let commandGlyphs = layout.glyphRange(forCharacterRange: commandRange, actualCharacterRange: nil)
        XCTAssertEqual(NSIntersectionRange(hidden, commandGlyphs).length, 0)
    }

    func testWindowDisplaysRecentRunsInModelOrder() {
        let snapshot = StatusSnapshot(records: [], identity: { _ in nil })
        let runs = [
            RecentRun(record: record(2, worklog: 123), observedEnd: Date(timeIntervalSince1970: 20)),
            RecentRun(record: record(1, worklog: 456), observedEnd: Date(timeIntervalSince1970: 10)),
        ]
        let text = formatStatusWindow(
            snapshot: snapshot, legacy: false, recentRuns: runs, lineWidth: 600,
            title: { $0.worklog == 123 ? "Worklog: Newer" : "Worklog: Older" },
            describe: { _, _ in NSAttributedString(string: "") }
        ).string as NSString
        XCTAssertTrue(text.contains("RECENTLY ENDED (2 of last 100)"))
        XCTAssertLessThan(text.range(of: "#123 Newer").location, text.range(of: "#456 Older").location)
    }

    func testHistoryRowStaysOnOneLineInsideTheStatusWindow() {
        let snapshot = StatusSnapshot(records: [], identity: { _ in nil })
        let run = RecentRun(record: record(1), observedEnd: Date(timeIntervalSince1970: 760))
        let text = formatStatusWindow(
            snapshot: snapshot, legacy: false, recentRuns: [run], lineWidth: 350,
            title: { _ in "Worklog: " + String(repeating: "A", count: 160) },
            describe: { _, _ in NSAttributedString(string: "") }
        )
        let content = text.string as NSString
        let issue = content.range(of: "#123")
        let command = content.range(of: "fvm flutter test …")
        guard issue.location != NSNotFound, command.location != NSNotFound else {
            return XCTFail("Missing history row")
        }

        let storage = NSTextStorage(attributedString: text)
        let layout = NSLayoutManager()
        let container = NSTextContainer(containerSize: NSSize(width: 350, height: 1000))
        container.lineFragmentPadding = 0
        storage.addLayoutManager(layout)
        layout.addTextContainer(container)
        layout.ensureLayout(for: container)
        let issueGlyph = layout.glyphIndexForCharacter(at: issue.location)
        let commandGlyph = layout.glyphIndexForCharacter(at: command.location)
        let issueLine = layout.lineFragmentRect(forGlyphAt: issueGlyph, effectiveRange: nil)
        let commandLine = layout.lineFragmentRect(forGlyphAt: commandGlyph, effectiveRange: nil)
        XCTAssertEqual(issueLine.minY, commandLine.minY)
        let hidden = layout.truncatedGlyphRange(inLineFragmentForGlyphAt: issueGlyph)
        XCTAssertEqual(NSIntersectionRange(hidden, NSRange(location: commandGlyph, length: 1)).length, 0)
    }

    func testCopyingHistorySummaryWritesTheFullScriptCommand() throws {
        let run = RecentRun(
            record: record(1, command: ["python3", "tool/run_checks.py", "--suite", "abc"]),
            observedEnd: Date(timeIntervalSince1970: 2)
        )
        let row = formatRecentRun(run, title: "Worklog: Test suite", lineWidth: 600)
        let scroll = makeStatusScrollView(frame: NSRect(x: 0, y: 0, width: 720, height: 520))
        let text = try XCTUnwrap(scroll.documentView as? StatusTextView)
        text.textStorage?.setAttributedString(row)
        let range = (row.string as NSString).range(of: "run_checks.py")
        guard range.location != NSNotFound else { return XCTFail("Missing script summary") }
        text.setSelectedRange(range)
        let pasteboard = NSPasteboard.withUniqueName()
        defer { pasteboard.releaseGlobally() }
        XCTAssertEqual(text.writeSelection(to: pasteboard), true)
        XCTAssertEqual(pasteboard.string(forType: .string), "python3 tool/run_checks.py --suite abc")
    }

    func testLaidOutSelectionAcrossHistoryRowsSurvivesRefreshAndCopiesCommands() throws {
        let snapshot = StatusSnapshot(records: [], identity: { _ in nil })
        let first = RecentRun(record: record(1), observedEnd: Date(timeIntervalSince1970: 20))
        let second = RecentRun(
            record: record(2, worklog: 456, command: ["python3", "tool/run_checks.py", "--suite", "abc"]),
            observedEnd: Date(timeIntervalSince1970: 10)
        )
        func window(_ runs: [RecentRun]) -> NSAttributedString {
            formatStatusWindow(
                snapshot: snapshot, legacy: false, recentRuns: runs, lineWidth: 680,
                title: { _ in "Worklog: Title" }, describe: { _, _ in NSAttributedString(string: "") }
            )
        }
        let text = try XCTUnwrap(
            makeStatusScrollView(frame: NSRect(x: 0, y: 0, width: 720, height: 520)).documentView as? StatusTextView
        )
        let before = window([first, second])
        text.replaceStatus(before)
        let layout = try XCTUnwrap(text.textLayoutManager)
        layout.ensureLayout(for: layout.documentRange)

        let content = before.string as NSString
        let firstCommand = content.range(of: "fvm flutter test …")
        let secondCommand = content.range(of: "run_checks.py")
        guard firstCommand.location != NSNotFound, secondCommand.location != NSNotFound else {
            return XCTFail("Missing history commands")
        }
        let selection = NSRange(
            location: firstCommand.location,
            length: NSMaxRange(secondCommand) - firstCommand.location
        )
        let expected = content.substring(with: selection)
        text.setSelectedRange(selection)
        text.replaceStatus(window([first, second]))
        XCTAssertEqual(text.selectedRange(), selection)

        let added = RecentRun(record: record(3, worklog: 789), observedEnd: Date(timeIntervalSince1970: 30))
        text.replaceStatus(window([added, first, second]))
        let restored = text.selectedRange()
        guard restored.length > 0 else { return XCTFail("Selection was cleared during refresh") }
        XCTAssertEqual((text.string as NSString).substring(with: restored), expected)
        let pasteboard = NSPasteboard.withUniqueName()
        defer { pasteboard.releaseGlobally() }
        XCTAssertEqual(text.writeSelection(to: pasteboard), true)
        let copied = try XCTUnwrap(pasteboard.string(forType: .string))
        XCTAssertTrue(copied.contains("fvm flutter test --coverage"))
        XCTAssertTrue(copied.contains("python3 tool/run_checks.py --suite abc"))
    }
}
