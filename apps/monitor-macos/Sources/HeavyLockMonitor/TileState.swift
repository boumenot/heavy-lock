struct TileState {
    enum Style {
        case idle, running, unknown, legacy
    }

    let style: Style
    let label: String
    let extraHolders: Int
    let waitingCount: Int

    init(snapshot: StatusSnapshot, legacy: Bool) {
        waitingCount = snapshot.waiters.count
        extraHolders = snapshot.extraHolderCount
        if legacy {
            style = .legacy
            label = "lock"
        } else if snapshot.hasUnknownStatus {
            style = .unknown
            label = snapshot.holders.min(by: { $0.pid < $1.pid })?.displayWorklog ?? "!"
        } else if let holder = snapshot.holders.min(by: { $0.pid < $1.pid }) {
            style = .running
            label = holder.displayWorklog
        } else {
            style = .idle
            label = "—"
        }
    }
}
