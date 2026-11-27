import HeavyLockV2
import SwiftUI
import TunaMonitorCore

struct MonitorView: View {
    let session: MonitorSession
    let port: Int
    @State private var metadata = JobMetadata()
    @State private var dockTile = DockTileView(frame: NSRect(x: 0, y: 0, width: 128, height: 128))

    var body: some View {
        let state = self.session.state
        VStack(alignment: .leading, spacing: 0) {
            TimelineView(.periodic(from: .now, by: 1)) { context in
                self.header(state, now: context.date)
            }
            Divider()
            TimelineView(.periodic(from: .now, by: 1)) { context in
                List {
                    self.section("Running", jobs: state.active, state: state, now: context.date, empty: "No running jobs")
                    self.section("Queued", jobs: state.queue, state: state, now: context.date, empty: "Queue is empty")
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
        .onChange(of: Presentation.dockTile(state), initial: true) { _, tile in
            self.dockTile.show(tile)
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
                    let action: ControlAction = status.queueMode == .paused ? .resume : .pause
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
                if let worklog = row.worklog {
                    self.worklogLink(worklog, cwd: row.cwd)
                }
                if let session = row.session {
                    Self.sessionLink(session)
                }
                if let cwd = row.cwd {
                    self.checkoutLink(cwd, display: row.cwdDisplay ?? cwd)
                }
                ForEach(row.details, id: \.self) { Text($0).font(.caption).foregroundStyle(.secondary) }
                ForEach(row.warnings, id: \.self) { warning in
                    Label(warning, systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
            }
            .textSelection(.enabled)
            Spacer()
            let actions = ControlAction.jobActions(for: job, in: self.session.state.queue)
            if !actions.isEmpty {
                HStack(spacing: 3) {
                    ForEach(actions, id: \.self) { action in
                        Button {
                            self.session.request(action)
                        } label: {
                            Image(systemName: action.systemImageName)
                                .imageScale(.large)
                                .frame(width: 24, height: 24)
                                .contentShape(Rectangle())
                        }
                        .buttonStyle(.borderless)
                        .overlay {
                            RoundedRectangle(cornerRadius: 4)
                                .stroke(.quaternary)
                        }
                        .help(action.title)
                        .accessibilityLabel(action.title)
                        .disabled(!state.isLive || self.session.inFlight.contains(action))
                    }
                }
                .fixedSize()
            }
        }
        .padding(.vertical, 2)
    }

    @ViewBuilder
    private func worklogLink(_ worklog: WorklogReference, cwd: String?) -> some View {
        let label = worklog.qualifiedLabel
        switch self.metadata.issue(worklog, cwd: cwd) {
        case .resolved(let issue):
            if let url = issue.url ?? worklog.url {
                Link("\(label) \(issue.title)", destination: url).font(.caption)
            } else {
                Text("\(label) \(issue.title)").font(.caption)
            }
        case .loading:
            Self.worklogFallback(worklog, label: label, note: "Loading title…")
        case .unavailable:
            Self.worklogFallback(worklog, label: label, note: "Title unavailable")
        }
    }

    /// The recorded issue link stays usable when `gh` cannot supply a title.
    @ViewBuilder
    private static func worklogFallback(_ worklog: WorklogReference, label: String, note: String) -> some View {
        HStack(spacing: 4) {
            if let url = worklog.url {
                Link(label, destination: url)
            } else {
                Text(label)
            }
            Text(note).foregroundStyle(.secondary)
        }
        .font(.caption)
    }

    @ViewBuilder
    private static func sessionLink(_ session: AgentSession) -> some View {
        if let url = session.url {
            Link("Copilot session \(session.id.prefix(8))", destination: url)
                .font(.caption)
                .help("Open session \(session.id) in the GitHub Copilot app")
        } else {
            Text("Agent session \(session.id)").font(.caption).foregroundStyle(.secondary)
        }
    }

    @ViewBuilder
    private func checkoutLink(_ cwd: String, display: String) -> some View {
        if case .resolved(let checkout) = self.metadata.checkout(cwd), let directory = checkout.directory {
            let branch = checkout.branch.map { " · \($0)" } ?? ""
            Link(
                Presentation.homeRelative(directory) + branch,
                destination: URL(fileURLWithPath: directory, isDirectory: true)
            )
            .font(.caption)
            .help("Open the checkout in Finder")
            if directory != cwd {
                Text("in \(display)").font(.caption).foregroundStyle(.secondary)
            }
        } else {
            Link(display, destination: URL(fileURLWithPath: cwd, isDirectory: true))
                .font(.caption)
                .help("Open the working directory in Finder")
        }
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
