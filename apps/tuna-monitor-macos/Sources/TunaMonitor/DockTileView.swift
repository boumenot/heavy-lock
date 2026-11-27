import AppKit
import TunaMonitorCore

/// Draws the Dock tile: the worklog of the longest-running
/// job inside a ring coloured by state, with `+N` for other running jobs,
/// plus a bordered icon and a stop light for the queue.
final class DockTileView: NSView {
    var tile = Presentation.DockTile(style: .notLive, label: "!", otherRunning: 0, badge: nil, light: .unknown)

    override func draw(_ dirtyRect: NSRect) {
        let bounds = self.bounds.insetBy(dx: 4, dy: 4)
        let background = NSBezierPath(roundedRect: bounds, xRadius: bounds.width * 0.22, yRadius: bounds.height * 0.22)
        NSColor(calibratedWhite: 0.13, alpha: 1).setFill()
        background.fill()
        NSColor(calibratedWhite: 0.85, alpha: 1).setStroke()
        background.lineWidth = max(2, bounds.width * 0.025)
        background.stroke()
        let ring = NSBezierPath(
            roundedRect: bounds.insetBy(dx: 5, dy: 5), xRadius: bounds.width * 0.18, yRadius: bounds.height * 0.18)
        ring.lineWidth = max(3, bounds.width * 0.04)
        switch self.tile.style {
        case .idle: NSColor.systemGray.setStroke()
        case .running: NSColor.systemGreen.setStroke()
        case .notLive:
            NSColor.systemRed.setStroke()
            ring.setLineDash([6, 4], count: 2, phase: 0)
        }
        ring.stroke()
        self.drawStopLight(in: bounds)
        let label = self.tile.label
        let size = min(bounds.width * 0.3, bounds.width * 1.2 / CGFloat(max(label.count, 1)))
        let attributes: [NSAttributedString.Key: Any] = [
            .font: NSFont.systemFont(ofSize: size, weight: .bold), .foregroundColor: NSColor.white,
        ]
        let textSize = label.size(withAttributes: attributes)
        let textCenter = bounds.midX + bounds.width * 0.09
        label.draw(
            at: NSPoint(x: textCenter - textSize.width / 2, y: bounds.midY - textSize.height / 2),
            withAttributes: attributes)
        if self.tile.otherRunning > 0 {
            let extra = "+\(self.tile.otherRunning)"
            let extraAttributes: [NSAttributedString.Key: Any] = [
                .font: NSFont.systemFont(ofSize: bounds.width * 0.12, weight: .semibold),
                .foregroundColor: NSColor.white,
            ]
            extra.draw(
                at: NSPoint(
                    x: textCenter - extra.size(withAttributes: extraAttributes).width / 2,
                    y: bounds.minY + bounds.height * 0.15),
                withAttributes: extraAttributes)
        }
    }

    /// A vertical stop light at the left from the broker's queue state: red
    /// when paused, amber when blocked, recovering or in error, green when
    /// idle or running, all dark when the state is unknown.
    private func drawStopLight(in bounds: NSRect) {
        let width = bounds.width * 0.17
        let height = bounds.height * 0.5
        let housing = NSRect(
            x: bounds.minX + bounds.width * 0.14, y: bounds.midY - height / 2, width: width, height: height)
        NSColor.black.setFill()
        NSBezierPath(roundedRect: housing, xRadius: width / 2, yRadius: width / 2).fill()
        let lamps: [(NSColor, Presentation.QueueLight)] = [
            (.systemRed, .stopped), (.systemYellow, .stalled), (.systemGreen, .go),
        ]
        let diameter = width * 0.7
        let step = height / 3
        for (index, (color, light)) in lamps.enumerated() {
            let center = NSPoint(x: housing.midX, y: housing.maxY - step * (CGFloat(index) + 0.5))
            let lamp = NSRect(
                x: center.x - diameter / 2, y: center.y - diameter / 2, width: diameter, height: diameter)
            (self.tile.light == light ? color : color.withAlphaComponent(0.18)).setFill()
            NSBezierPath(ovalIn: lamp).fill()
        }
    }

    /// Installs this view as the Dock tile and shows `tile`.
    @MainActor
    func show(_ tile: Presentation.DockTile) {
        if NSApp.dockTile.contentView !== self { NSApp.dockTile.contentView = self }
        self.tile = tile
        NSApp.dockTile.badgeLabel = tile.badge
        NSApp.dockTile.display()
    }
}
