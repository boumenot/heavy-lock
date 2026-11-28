import SwiftUI
import TunaContract
import TunaMonitorCore

struct FinishedHistoryView<Details: View>: View {
    let state: MonitorState
    @ViewBuilder let details: (Tuna_Job) -> Details

    @State private var limit = 20
    @State private var outcome: FinishedHistory.Outcome?
    @State private var worklog: FinishedHistory.Worklog?
    @State private var selectedID: String?
    @State private var matrixOpen = false
    @FocusState private var focusedID: String?

    var body: some View {
        let history = FinishedHistory(jobs: self.state.recent, limit: self.limit)
        let visible = history.visible(outcome: self.outcome, worklog: self.worklog)
        let selected = visible.first { $0.id == self.selectedID } ?? visible.first
        return VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text("Last")
                Picker("Finished job window", selection: self.$limit) {
                    ForEach([10, 20, 25, 50], id: \.self) { count in Text("\(count)").tag(count) }
                }
                .labelsHidden()
                .frame(width: 65)
                Text("jobs · \(history.summary.total) retained")
                Spacer()
            }
            .font(.caption)
            .padding(8)
            if let message = self.historyStatus {
                Label(message, systemImage: "exclamationmark.triangle")
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .padding(.horizontal, 8)
                    .padding(.bottom, 6)
            }
            Divider()
            GeometryReader { geometry in
                HStack(alignment: .top, spacing: 0) {
                    VStack(alignment: .leading, spacing: 0) {
                        self.ribbon(history, visible: visible, selected: selected)
                        Divider()
                        self.tabs(history)
                        Divider()
                        self.matrix(history)
                        Divider()
                        if let worklog = self.worklog {
                            HStack {
                                Text(worklog.label).lineLimit(1)
                                Spacer()
                                Button("All worklogs") {
                                    self.setWorklog(nil, history: history)
                                }
                                .buttonStyle(.borderless)
                            }
                            .font(.caption2)
                            .padding(6)
                            Divider()
                        }
                        self.columnHeader(visible: visible.count, total: history.summary.total)
                        Divider()
                        self.rows(visible, history: history, selected: selected)
                    }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    Divider()
                    VStack(alignment: .leading, spacing: 0) {
                        self.summary(history.summary)
                        Divider()
                        ScrollView {
                            VStack(alignment: .leading, spacing: 7) {
                                Text("Selected job").font(.caption.bold())
                                if let selected {
                                    Text(selected.id).font(.caption2).foregroundStyle(.secondary)
                                    Label(selected.outcome.title, systemImage: Self.symbol(selected.outcome))
                                        .font(.caption.bold())
                                        .foregroundStyle(Self.color(selected.outcome))
                                    self.details(selected.job)
                                } else {
                                    Text(history.entries.isEmpty
                                        ? "Finished job details will appear here."
                                        : "Choose another outcome or clear the worklog filter.")
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                }
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(8)
                        }
                    }
                    .frame(width: min(236, max(175, geometry.size.width * 0.32)))
                    .frame(maxHeight: .infinity)
                    .background(Color.secondary.opacity(0.06))
                }
            }
            .frame(height: 430)
            Divider()
            Text("Elapsed is root-command wall-clock time, not CPU · — means not recorded.")
                .font(.caption2)
                .foregroundStyle(.secondary)
                .padding(6)
        }
        .onChange(of: visible.map(\.id), initial: true) { _, _ in
            self.reconcileSelection(visible)
        }
    }

    private var historyStatus: String? {
        switch self.state.recentLoad {
        case .loading:
            "Loading finished history; shown results are not current."
        case .unavailable(let message):
            "Finished history unavailable: \(message). Shown results are not current; retrying."
        case .available:
            self.state.isRecentCurrent ? nil : "Finished history is catching up or not live; shown results are not current."
        }
    }

    private func ribbon(
        _ history: FinishedHistory, visible: [FinishedHistory.Entry], selected: FinishedHistory.Entry?
    ) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("Outcome ribbon · full window")
                Spacer()
                Text("Newest → oldest")
            }
            .font(.caption2)
            .foregroundStyle(.secondary)
            if history.entries.isEmpty {
                Text("No retained outcomes").font(.caption).foregroundStyle(.secondary)
            } else {
                ScrollView(.horizontal) {
                    HStack(spacing: 3) {
                        ForEach(history.entries) { entry in
                            Button {
                                if !visible.contains(where: { $0.id == entry.id }) {
                                    self.outcome = nil
                                    self.worklog = nil
                                }
                                self.select(entry.id)
                            } label: {
                                Image(systemName: Self.symbol(entry.outcome))
                                    .font(.caption2.bold())
                                    .foregroundStyle(Self.color(entry.outcome))
                                    .frame(width: 20, height: 20)
                                    .background(Self.color(entry.outcome).opacity(0.12), in: .rect(cornerRadius: 3))
                                    .overlay {
                                        RoundedRectangle(cornerRadius: 3)
                                            .stroke(selected?.id == entry.id ? Color.accentColor : .clear, lineWidth: 2)
                                    }
                                    .opacity(visible.contains(where: { $0.id == entry.id }) ? 1 : 0.5)
                            }
                            .buttonStyle(.plain)
                            .help("\(entry.outcome.title): \(entry.job.command)")
                            .accessibilityLabel("\(entry.outcome.title), \(entry.job.command)")
                        }
                    }
                }
                .scrollIndicators(.hidden)
            }
        }
        .padding(7)
    }

    private func tabs(_ history: FinishedHistory) -> some View {
        ScrollView(.horizontal) {
            HStack(spacing: 4) {
                self.tab("All", count: history.summary.total, selected: self.outcome == nil) {
                    self.setOutcome(nil, history: history)
                }
                ForEach(
                    [FinishedHistory.Outcome.failed, .cancelled, .unknown, .success], id: \.self
                ) { outcome in
                    self.tab(
                        outcome.title, count: history.summary.counts[outcome, default: 0],
                        selected: self.outcome == outcome
                    ) {
                        self.setOutcome(outcome, history: history)
                    }
                }
            }
            .padding(6)
        }
        .scrollIndicators(.hidden)
        .accessibilityLabel("Filter finished jobs by outcome")
    }

    private func tab(_ title: String, count: Int, selected: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Text("\(title) \(count)")
                .font(.caption2)
                .padding(.horizontal, 6)
                .padding(.vertical, 4)
                .background(selected ? Color.accentColor.opacity(0.16) : Color.secondary.opacity(0.06), in: .rect(cornerRadius: 4))
                .overlay {
                    RoundedRectangle(cornerRadius: 4)
                        .stroke(selected ? Color.accentColor.opacity(0.7) : Color.secondary.opacity(0.25))
                }
        }
        .buttonStyle(.plain)
        .accessibilityValue(selected ? "Selected" : "")
    }

    private func matrix(_ history: FinishedHistory) -> some View {
        DisclosureGroup(isExpanded: self.$matrixOpen) {
            ScrollView {
                VStack(spacing: 0) {
                    HStack(spacing: 3) {
                        Text("Worklog").frame(maxWidth: .infinity, alignment: .leading)
                        ForEach(FinishedHistory.Outcome.allCases, id: \.self) { outcome in
                            Image(systemName: Self.symbol(outcome))
                                .foregroundStyle(Self.color(outcome))
                                .frame(width: 20)
                                .help(outcome.title)
                        }
                        Text("Jobs").frame(width: 40)
                    }
                    .foregroundStyle(.secondary)
                    ForEach(history.groups) { group in
                        HStack(spacing: 3) {
                            Text(group.label)
                                .lineLimit(1)
                                .truncationMode(.middle)
                                .help(group.label)
                                .frame(maxWidth: .infinity, alignment: .leading)
                            ForEach(FinishedHistory.Outcome.allCases, id: \.self) { outcome in
                                Text("\(group.counts[outcome, default: 0])")
                                    .monospacedDigit()
                                    .frame(width: 20)
                            }
                            Button("\(group.total)") {
                                self.setWorklog(group.id, history: history)
                            }
                            .frame(width: 40)
                            .help("Show jobs for \(group.label)")
                            .accessibilityLabel("Show \(group.total) jobs for \(group.label)")
                        }
                        .padding(.vertical, 2)
                    }
                }
                .font(.caption2)
            }
            .frame(maxHeight: 115)
        } label: {
            HStack {
                Text("Worklog outcome matrix")
                Text("\(history.groups.count) groups · full window").foregroundStyle(.secondary)
            }
            .font(.caption2)
        }
        .padding(7)
    }

    private func columnHeader(visible: Int, total: Int) -> some View {
        HStack(spacing: 4) {
            Text("Finished").frame(width: 53, alignment: .leading)
            Text("").frame(width: 18)
            Text("Command · \(visible)/\(total)").frame(maxWidth: .infinity, alignment: .leading)
            Text("Outcome").frame(width: 56, alignment: .leading)
            Text("Elapsed").frame(width: 85, alignment: .trailing)
        }
        .font(.caption2)
        .foregroundStyle(.secondary)
        .padding(.horizontal, 6)
        .padding(.vertical, 4)
        .background(Color.secondary.opacity(0.06))
    }

    private func rows(
        _ entries: [FinishedHistory.Entry], history: FinishedHistory, selected: FinishedHistory.Entry?
    ) -> some View {
        ScrollViewReader { proxy in
            ScrollView {
                if entries.isEmpty {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(history.entries.isEmpty ? "No retained finished jobs" : "No matching jobs")
                            .font(.caption.bold())
                        Text(history.entries.isEmpty
                            ? "Finished jobs will appear here."
                            : "Choose another outcome or clear the worklog filter.")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(12)
                } else {
                    LazyVStack(spacing: 0) {
                        ForEach(entries) { entry in
                            self.row(entry, longest: history.summary.longest, selected: selected?.id == entry.id)
                                .id(entry.id)
                            Divider()
                        }
                    }
                }
            }
            .onChange(of: self.selectedID) { _, id in
                if let id { proxy.scrollTo(id, anchor: .center) }
            }
            .onMoveCommand { direction in
                guard direction == .up || direction == .down, !entries.isEmpty else { return }
                let current = entries.firstIndex { $0.id == selected?.id } ?? 0
                let next = min(max(0, current + (direction == .down ? 1 : -1)), entries.count - 1)
                self.select(entries[next].id)
            }
        }
    }

    private func row(_ entry: FinishedHistory.Entry, longest: TimeInterval?, selected: Bool) -> some View {
        Button {
            self.select(entry.id)
        } label: {
            HStack(spacing: 4) {
                Group {
                    if entry.job.hasFinishedAt {
                        Text(entry.job.finishedAt.date, style: .time)
                    } else {
                        Text("—")
                    }
                }
                .font(.caption2.monospacedDigit())
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .frame(width: 53, alignment: .leading)
                ZStack {
                    Rectangle().fill(Color.secondary.opacity(0.25)).frame(width: 1)
                    Image(systemName: Self.symbol(entry.outcome))
                        .font(.caption2.bold())
                        .foregroundStyle(Self.color(entry.outcome))
                        .frame(width: 18, height: 18)
                        .background(.background, in: .circle)
                        .overlay(Circle().stroke(Color.secondary.opacity(0.3)))
                }
                .frame(width: 18)
                Text(entry.job.command)
                    .font(.caption.monospaced())
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .help(entry.job.command)
                Text(entry.outcome.title)
                    .font(.caption2)
                    .foregroundStyle(Self.color(entry.outcome))
                    .lineLimit(1)
                    .frame(width: 56, alignment: .leading)
                self.elapsedBar(entry.elapsed, longest: longest, outcome: entry.outcome)
                Text(FinishedHistory.formatElapsed(entry.elapsed))
                    .font(.caption2.monospacedDigit())
                    .frame(width: 45, alignment: .trailing)
            }
            .frame(height: 32)
            .padding(.horizontal, 6)
            .contentShape(Rectangle())
            .background(selected ? Color.accentColor.opacity(0.15) : .clear)
        }
        .buttonStyle(.plain)
        .focused(self.$focusedID, equals: entry.id)
        .accessibilityLabel("\(entry.outcome.title), \(entry.job.command), elapsed \(FinishedHistory.formatElapsed(entry.elapsed))")
        .accessibilityValue(selected ? "Selected" : "")
        .help("\(entry.outcome.title) · select for details")
    }

    @ViewBuilder
    private func elapsedBar(
        _ elapsed: TimeInterval?, longest: TimeInterval?, outcome: FinishedHistory.Outcome
    ) -> some View {
        if let elapsed, let longest {
            GeometryReader { geometry in
                ZStack(alignment: .leading) {
                    Capsule().fill(Color.secondary.opacity(0.15))
                    Capsule()
                        .fill(Self.color(outcome).opacity(0.7))
                        .frame(width: longest > 0 ? geometry.size.width * min(elapsed / longest, 1) : 0)
                }
            }
            .frame(width: 34, height: 5)
            .help("Elapsed relative to the longest measured job in the full window")
        } else {
            Text("—").font(.caption2).foregroundStyle(.secondary).frame(width: 34)
                .help("Elapsed not recorded")
        }
    }

    private func summary(_ summary: FinishedHistory.Summary) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Last \(summary.total) finished jobs").font(.caption.bold())
            Grid(alignment: .leading, horizontalSpacing: 8, verticalSpacing: 4) {
                GridRow {
                    self.summaryCount(.success, summary: summary)
                    self.summaryCount(.failed, summary: summary)
                }
                GridRow {
                    self.summaryCount(.cancelled, summary: summary)
                    self.summaryCount(.unknown, summary: summary)
                }
            }
            Text(
                "Mean \(FinishedHistory.formatElapsed(summary.mean)) · median \(FinishedHistory.formatElapsed(summary.median))"
            )
            Text("Longest \(FinishedHistory.formatElapsed(summary.longest)) · measured \(summary.measured)/\(summary.total)")
            Text("Root wall-clock elapsed · not CPU")
        }
        .font(.caption2)
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(8)
    }

    private func summaryCount(_ outcome: FinishedHistory.Outcome, summary: FinishedHistory.Summary) -> some View {
        Label("\(summary.counts[outcome, default: 0]) \(outcome.title)", systemImage: Self.symbol(outcome))
            .font(.caption2)
            .foregroundStyle(Self.color(outcome))
    }

    private func setOutcome(_ outcome: FinishedHistory.Outcome?, history: FinishedHistory) {
        self.outcome = outcome
        self.reconcileSelection(history.visible(outcome: outcome, worklog: self.worklog))
    }

    private func setWorklog(_ worklog: FinishedHistory.Worklog?, history: FinishedHistory) {
        self.worklog = worklog
        self.reconcileSelection(history.visible(outcome: self.outcome, worklog: worklog))
    }

    private func reconcileSelection(_ entries: [FinishedHistory.Entry]) {
        if !entries.contains(where: { $0.id == self.selectedID }) {
            self.selectedID = entries.first?.id
        }
    }

    private func select(_ id: String) {
        self.selectedID = id
        self.focusedID = id
    }

    private static func symbol(_ outcome: FinishedHistory.Outcome) -> String {
        switch outcome {
        case .success: "checkmark"
        case .failed: "xmark"
        case .cancelled: "minus"
        case .unknown: "questionmark"
        }
    }

    private static func color(_ outcome: FinishedHistory.Outcome) -> Color {
        switch outcome {
        case .success: .green
        case .failed: .red
        case .cancelled: .orange
        case .unknown: .secondary
        }
    }
}
