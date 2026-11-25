import AppKit

extension NSAttributedString.Key {
    static let fullCommand = NSAttributedString.Key("HeavyLockFullCommand")
}

enum StatusTextRole {
    case section, holder, waiter, warning, body
}

func formatStatusWindow(
    snapshot: StatusSnapshot, legacy: Bool, recentRuns: [RecentRun], lineWidth: CGFloat,
    title: (StatusRecord) -> String, issueURL: (StatusRecord) -> URL? = { _ in nil },
    describe: (StatusRecord, StatusTextRole) -> NSAttributedString
) -> NSAttributedString {
    var lines: [(String, StatusTextRole)] = []
    if legacy {
        lines += [("LEGACY EXCLUSIVE LOCK", .warning), ("A legacy process holds the gate. Its PID may be unavailable.", .body), ("", .body)]
    }
    if snapshot.hasUnknownStatus {
        lines += [("STATUS UNKNOWN", .warning), ("Some status is stale, unreadable, or has no live record.", .body), ("", .body)]
    }
    lines.append(("HOLDING (\(snapshot.holders.count))", .section))
    if snapshot.holders.isEmpty { lines.append(("No known holders.", .body)) }
    let text = NSMutableAttributedString(attributedString: formatStatusText(lines))
    for record in snapshot.holders { text.append(describe(record, .holder)) }

    lines = [("", .body), ("", .body), ("WAITING (\(snapshot.waiters.count)) — NOT AN ORDER", .section)]
    if snapshot.waiters.isEmpty { lines.append(("No known waiters.", .body)) }
    text.append(formatStatusText(lines))
    for record in snapshot.waiters { text.append(describe(record, .waiter)) }

    text.append(formatStatusText([
        ("", .body), ("", .body),
        ("RECENTLY ENDED (\(recentRuns.count) of last \(RecentRuns.limit)) — observed while this app was open", .section),
    ]))
    if recentRuns.isEmpty {
        text.append(formatStatusText([("\nNo runs observed ending yet.", .body)]))
    } else {
        text.append(formatStatusText([("\n", .body)]))
        for (index, run) in recentRuns.enumerated() {
            let worklogTitle = run.record.attribution == "declared" ? title(run.record) : nil
            let row = formatRecentRun(run, title: worklogTitle, lineWidth: lineWidth, issueURL: issueURL(run.record))
            text.append(row)
            if index < recentRuns.count - 1 {
                text.append(NSAttributedString(string: "\n", attributes: row.attributes(at: 0, effectiveRange: nil)))
            }
        }
    }
    return text
}

private let recentRunTimeFormatter: DateFormatter = {
    let formatter = DateFormatter()
    formatter.timeStyle = .short
    formatter.dateStyle = .none
    formatter.locale = .autoupdatingCurrent
    formatter.timeZone = .autoupdatingCurrent
    return formatter
}()

func formatRecentRun(_ run: RecentRun, title: String?, lineWidth: CGFloat, issueURL: URL? = nil) -> NSAttributedString {
    let time = recentRunTimeFormatter.string(from: run.observedEnd)
    let elapsed = run.duration.map { duration in
        let seconds = Int(duration)
        return "~\(seconds / 60)m \(seconds % 60)s"
    } ?? "time unavailable"
    let font = NSFont.systemFont(ofSize: 13)
    let prefix = "\(time)  \(elapsed)  \(run.record.displayWorklog) "
    let attributes: [NSAttributedString.Key: Any] = [.font: font]
    let minimumLeadingWidth = ((prefix + "… · ") as NSString).size(withAttributes: attributes).width
    let command = truncateToWidth(
        summarizeCommand(run.record.command), width: max(0, lineWidth - minimumLeadingWidth - 4), font: font
    )
    let suffix = " · \(command)"
    let fixedWidth = ((prefix + suffix) as NSString).size(withAttributes: attributes).width
    let issueTitle = "\(run.record.displayWorklog) \(truncateToWidth(cleanedWorklogTitle(title), width: max(0, lineWidth - fixedWidth - 4), font: font))"
    let leading = "\(time)  \(elapsed)  \(issueTitle) · "
    let paragraph = NSMutableParagraphStyle()
    paragraph.lineSpacing = 3
    paragraph.lineBreakMode = .byTruncatingTail
    let row = NSMutableAttributedString(string: leading + command, attributes: [
        .font: font,
        .foregroundColor: NSColor.labelColor,
        .paragraphStyle: paragraph,
    ])
    if run.record.worklog != nil, run.record.attribution == "declared", let url = issueURL {
        let issueRange = (row.string as NSString).range(of: issueTitle)
        row.addAttribute(.link, value: url, range: issueRange)
        row.addAttribute(.underlineStyle, value: NSUnderlineStyle.single.rawValue, range: issueRange)
    }
    let commandRange = NSRange(location: (leading as NSString).length, length: (command as NSString).length)
    let commandText = fullCommand(run.record.command)
    row.addAttribute(.toolTip, value: commandText, range: commandRange)
    row.addAttribute(.fullCommand, value: commandText, range: commandRange)
    return row
}

private func cleanedWorklogTitle(_ title: String?) -> String {
    guard let title else { return "Unattributed" }
    let cleaned = title.hasPrefix("Worklog:")
        ? String(title.dropFirst("Worklog:".count)).trimmingCharacters(in: .whitespaces)
        : title
    return String(cleaned.map { $0.isNewline ? " " : $0 })
}

private func truncateToWidth(_ text: String, width: CGFloat, font: NSFont) -> String {
    let attributes: [NSAttributedString.Key: Any] = [.font: font]
    if (text as NSString).size(withAttributes: attributes).width <= width { return text }
    let characters = Array(text)
    var lower = 0
    var upper = characters.count
    while lower < upper {
        let middle = (lower + upper + 1) / 2
        let candidate = String(characters.prefix(middle)) + "…"
        if (candidate as NSString).size(withAttributes: attributes).width <= width {
            lower = middle
        } else {
            upper = middle - 1
        }
    }
    return String(characters.prefix(lower)) + "…"
}

func formatStatusRecord(
    _ record: StatusRecord, title: String?, branch: String, directory: URL?,
    elapsed: String, role: StatusTextRole, issueURL: URL? = nil
) -> NSAttributedString {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    let timestamp = formatter.date(from: record.created).map { formatter.string(from: $0) } ?? record.created
    let cleanedTitle = cleanedWorklogTitle(title)
    let issueTitle = "\(record.displayWorklog) \(cleanedTitle)"
    let heading = "[\(timestamp)] \(issueTitle)"
    let text = NSMutableAttributedString(attributedString: formatStatusText([
        ("", .body),
        ("", .body),
        (heading, role),
        ("  \(record.state.rawValue) · \(elapsed) · weight \(record.weight)/\(record.capacity) · \(record.attribution)", .body),
    ]))
    if record.worklog != nil, record.attribution == "declared", let url = issueURL {
        let range = (text.string as NSString).range(of: issueTitle)
        text.addAttribute(.link, value: url, range: range)
        text.addAttribute(.underlineStyle, value: NSUnderlineStyle.single.rawValue, range: range)
    }

    func field(_ label: String, _ value: String, link: URL? = nil, command: String? = nil) {
        let line = NSMutableAttributedString(attributedString: formatStatusText([("\n  \(label): \(value)", .body)]))
        let labelRange = (line.string as NSString).range(of: label)
        line.addAttribute(.font, value: NSFont.systemFont(ofSize: 13, weight: .semibold), range: labelRange)
        let valueRange = NSRange(location: (line.string as NSString).length - (value as NSString).length, length: (value as NSString).length)
        if let link {
            line.addAttribute(.link, value: link, range: valueRange)
            line.addAttribute(.underlineStyle, value: NSUnderlineStyle.single.rawValue, range: valueRange)
            line.addAttribute(.foregroundColor, value: holderColor, range: valueRange)
        }
        if let command {
            line.addAttribute(.toolTip, value: command, range: valueRange)
            line.addAttribute(.fullCommand, value: command, range: valueRange)
        }
        text.append(line)
    }
    field("Command", summarizeCommand(record.command), command: fullCommand(record.command))
    field("Checkout", branch, link: directory)
    if directory == nil, branch == "Branch unavailable", record.cwd.hasPrefix("/") {
        field("Working directory", "Open", link: URL(fileURLWithPath: record.cwd, isDirectory: true))
    }
    field("Runner", "PID \(record.pid)")
    if let reason = record.reason { field("Reason", reason) }
    return text
}

private let holderColor = NSColor(name: nil) { appearance in
    appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
        ? .systemBlue : NSColor(srgbRed: 0, green: 0.28, blue: 0.65, alpha: 1)
}

private let warningColor = NSColor(name: nil) { appearance in
    appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
        ? .systemOrange : NSColor(srgbRed: 0.53, green: 0.25, blue: 0, alpha: 1)
}

func formatStatusText(_ lines: [(String, StatusTextRole)]) -> NSAttributedString {
    let text = NSMutableAttributedString(string: "")
    let paragraph = NSMutableParagraphStyle()
    paragraph.lineSpacing = 3
    for (index, line) in lines.enumerated() {
        let font: NSFont
        let color: NSColor
        switch line.1 {
        case .section:
            font = .systemFont(ofSize: 12, weight: .bold)
            color = .secondaryLabelColor
        case .holder:
            font = .systemFont(ofSize: 15, weight: .semibold)
            color = holderColor
        case .waiter, .warning:
            font = .systemFont(ofSize: 15, weight: .semibold)
            color = warningColor
        case .body:
            font = .systemFont(ofSize: 13)
            color = .labelColor
        }
        let prefix = index == 0 ? "" : "\n"
        text.append(NSAttributedString(string: prefix + line.0, attributes: [
            .font: font,
            .foregroundColor: color,
            .paragraphStyle: paragraph,
        ]))
    }
    return text
}

final class StatusTextView: NSTextView {
    func replaceStatus(_ text: NSAttributedString) {
        guard let storage = textStorage, !storage.isEqual(to: text) else { return }
        let ranges = selectedRanges.map(\.rangeValue).filter { $0.length > 0 }
        var restored: [NSRange] = []
        if ranges.count == 1, ranges[0] == NSRange(location: 0, length: storage.length) {
            restored = [NSRange(location: 0, length: text.length)]
        } else {
            for range in ranges where range.location != NSNotFound &&
                range.location <= storage.length && range.length <= storage.length - range.location {
                let selection = storage.attributedSubstring(from: range)
                if let match = matchingSelection(selection, near: range.location, in: text) {
                    restored.append(match)
                }
            }
        }
        storage.setAttributedString(text)
        if !restored.isEmpty {
            setSelectedRanges(restored.map { NSValue(range: $0) }, affinity: .downstream, stillSelecting: false)
        }
    }

    private func matchingSelection(_ selection: NSAttributedString, near location: Int, in text: NSAttributedString) -> NSRange? {
        let content = text.string as NSString
        var start = 0
        var best: NSRange?
        while start <= content.length - selection.length {
            let range = content.range(
                of: selection.string, options: .literal,
                range: NSRange(location: start, length: content.length - start)
            )
            if range.location == NSNotFound { break }
            if text.attributedSubstring(from: range).isEqual(to: selection) {
                if let current = best {
                    if abs(range.location - location) < abs(current.location - location) { best = range }
                } else {
                    best = range
                }
            }
            start = range.location + 1
        }
        return best
    }

    func writeSelection(to pasteboard: NSPasteboard) -> Bool? {
        guard let storage = textStorage else { return nil }
        let ranges = selectedRanges.map(\.rangeValue).filter { $0.length > 0 }
        guard !ranges.isEmpty else { return nil }
        guard ranges.allSatisfy({
            $0.location != NSNotFound && $0.location <= storage.length && $0.length <= storage.length - $0.location
        }) else {
            NSLog("Cannot copy an invalid status text selection")
            return false
        }

        var includesCommand = false
        var pieces: [String] = []
        for selection in ranges {
            let selected = NSMutableString(string: (storage.string as NSString).substring(with: selection))
            storage.enumerateAttribute(.fullCommand, in: selection, options: .reverse) { value, range, _ in
                guard let command = value as? String else { return }
                let overlap = NSIntersectionRange(selection, range)
                guard overlap.length > 0 else { return }
                selected.replaceCharacters(
                    in: NSRange(location: overlap.location - selection.location, length: overlap.length),
                    with: command
                )
                includesCommand = true
            }
            pieces.append(selected as String)
        }
        guard includesCommand else { return nil }
        pasteboard.clearContents()
        return pasteboard.setString(pieces.joined(separator: "\n"), forType: .string)
    }

    override func copy(_ sender: Any?) {
        guard let copied = writeSelection(to: .general) else {
            super.copy(sender)
            return
        }
        if !copied {
            NSLog("Unable to copy status text to the pasteboard")
            NSSound.beep()
        }
    }
}

func makeStatusScrollView(frame: NSRect) -> NSScrollView {
    let scroll = NSScrollView(frame: frame)
    scroll.autoresizingMask = [.width, .height]
    scroll.hasVerticalScroller = true
    scroll.hasHorizontalScroller = false
    let text = StatusTextView(frame: NSRect(origin: .zero, size: scroll.contentSize))
    text.isEditable = false
    text.isSelectable = true
    text.linkTextAttributes = [:]
    text.isHorizontallyResizable = false
    text.isVerticallyResizable = true
    text.autoresizingMask = [.width]
    text.textContainer?.widthTracksTextView = true
    text.textContainerInset = NSSize(width: 20, height: 16)
    text.font = NSFont.systemFont(ofSize: 13)
    scroll.documentView = text
    return scroll
}
