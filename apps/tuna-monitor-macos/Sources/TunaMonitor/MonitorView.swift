import HeavyLockV2
import SwiftUI
import TunaMonitorCore

struct MonitorView: View {
    let session: MonitorSession
    let port: Int

    var body: some View {
        let state = self.session.state
        VStack(alignment: .leading, spacing: 0) {
            TimelineView(.periodic(from: .now, by: 1)) { context in
                self.header(state, now: context.date)
            }
            Divider()
            TimelineView(.periodic(from: .now, by: 1)) { context in
                List {
                    self.section("Active", jobs: state.active, state: state, now: context.date, empty: "No active jobs")
                    self.section("Queue", jobs: state.queue, state: state, now: context.date, empty: "Queue is empty")
                    self.section(
                        "Recently finished", jobs: state.recent, state: state, now: context.date,
                        empty: "No retained finished jobs"
                    )
                }
                .opacity(state.isLive ? 1 : 0.55)
            }
            if let error = state.controlError {
                Divider()
                Text(error)
                    .foregroundStyle(.red)
                    .textSelection(.enabled)
                    .padding(8)
            }
        }
        .alert(
            self.session.pendingConfirmation?.confirmation?.title ?? "",
            isPresented: Binding(
                get: { self.session.pendingConfirmation != nil },
                set: { if !$0 { self.session.dismissPending() } }
            ),
            presenting: self.session.pendingConfirmation?.confirmation
        ) { confirmation in
            Button(confirmation.confirmTitle, role: .destructive) { self.session.confirmPending() }
            Button(confirmation.keepTitle, role: .cancel) { self.session.dismissPending() }
        } message: { confirmation in
            Text(confirmation.message)
        }
        .onChange(of: state.queue.count, initial: true) { _, _ in
            NSApp.dockTile.badgeLabel = state.isLive && !state.queue.isEmpty ? String(state.queue.count) : nil
        }
        .onChange(of: state.isLive) { _, live in
            NSApp.dockTile.badgeLabel = live && !state.queue.isEmpty ? String(state.queue.count) : nil
        }
    }

    @ViewBuilder
    private func header(_ state: MonitorState, now: Date) -> some View {
        let banner = Presentation.banner(state, port: self.port, now: now)
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline) {
                Image(systemName: Self.symbol(banner.tone, live: state.isLive))
                    .foregroundStyle(Self.color(banner.tone, live: state.isLive))
                Text(banner.title).font(.headline)
                Spacer()
                if let status = state.status {
                    let action: ControlAction = status.paused ? .resume : .pause
                    Button(action.title) { self.session.request(action) }
                        .disabled(!state.isLive || self.session.inFlight.contains(action))
                }
            }
            if let detail = banner.detail {
                Text(detail).font(.callout).foregroundStyle(.secondary)
            }
            if let line = Presentation.statusLine(state) {
                Text(line).font(.callout)
            }
            if let notice = state.notice {
                Text(notice).font(.callout).foregroundStyle(.orange)
            }
        }
        .padding(10)
        .textSelection(.enabled)
    }

    @ViewBuilder
    private func section(
        _ title: String, jobs: [Heavylock_V2_Job], state: MonitorState, now: Date, empty: String
    ) -> some View {
        Section(title) {
            if jobs.isEmpty {
                Text(empty).foregroundStyle(.secondary)
            }
            ForEach(jobs, id: \.jobID) { job in
                self.jobRow(job, state: state, now: now)
            }
        }
    }

    private func jobRow(_ job: Heavylock_V2_Job, state: MonitorState, now: Date) -> some View {
        let row = Presentation.row(job, capacity: state.status?.capacity, now: now)
        return HStack(alignment: .top) {
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    if row.forced {
                        Text("FORCED")
                            .font(.caption.bold())
                            .padding(.horizontal, 4)
                            .background(.orange.opacity(0.25), in: .rect(cornerRadius: 3))
                            .help("Force-started: admitted out of order, bypassing resource checks")
                    }
                    Text(row.command).font(.body.monospaced()).lineLimit(2).truncationMode(.middle)
                }
                Text(row.summary).font(.caption)
                ForEach(row.details, id: \.self) { Text($0).font(.caption).foregroundStyle(.secondary) }
                ForEach(row.warnings, id: \.self) { warning in
                    Label(warning, systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
            }
            .textSelection(.enabled)
            Spacer()
            let actions = ControlAction.jobActions(for: job)
            if !actions.isEmpty {
                Menu("Actions") {
                    ForEach(actions, id: \.self) { action in
                        Button(action.title) { self.session.request(action) }
                            .disabled(self.session.inFlight.contains(action))
                    }
                }
                .fixedSize()
                .disabled(!state.isLive)
            }
        }
        .padding(.vertical, 2)
    }

    private static func symbol(_ tone: Presentation.Tone, live: Bool) -> String {
        switch tone {
        case .error: "xmark.octagon.fill"
        case .warning: "exclamationmark.triangle.fill"
        case .normal: live ? "checkmark.circle.fill" : "circle.dotted"
        }
    }

    private static func color(_ tone: Presentation.Tone, live: Bool) -> Color {
        switch tone {
        case .error: .red
        case .warning: .orange
        case .normal: live ? .green : .secondary
        }
    }
}
