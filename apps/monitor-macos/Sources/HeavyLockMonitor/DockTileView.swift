import AppKit

final class DockTileView: NSView {
    var tile = TileState(snapshot: StatusSnapshot(records: [], identity: { _ in nil }), legacy: false)

    override func draw(_ dirtyRect: NSRect) {
        let bounds = self.bounds.insetBy(dx: 4, dy: 4)
        let background = NSBezierPath(roundedRect: bounds, xRadius: bounds.width * 0.22, yRadius: bounds.height * 0.22)
        NSColor(calibratedWhite: 0.13, alpha: 1).setFill()
        background.fill()
        let ring = NSBezierPath(roundedRect: bounds.insetBy(dx: 5, dy: 5), xRadius: bounds.width * 0.18, yRadius: bounds.height * 0.18)
        ring.lineWidth = max(3, bounds.width * 0.04)
        switch tile.style {
        case .idle: NSColor.systemGray.setStroke()
        case .running: NSColor.systemGreen.setStroke()
        case .unknown: NSColor.systemRed.setStroke()
        case .legacy: NSColor.systemOrange.setStroke()
        }
        if tile.style == .unknown {
            ring.setLineDash([6, 4], count: 2, phase: 0)
        }
        ring.stroke()
        let size = min(bounds.width * 0.36, bounds.width * 1.5 / CGFloat(max(tile.label.count, 1)))
        let font = NSFont.systemFont(ofSize: size, weight: .bold)
        let attrs: [NSAttributedString.Key: Any] = [.font: font, .foregroundColor: NSColor.white]
        let textSize = tile.label.size(withAttributes: attrs)
        tile.label.draw(
            at: NSPoint(x: bounds.midX - textSize.width / 2, y: bounds.midY - textSize.height / 2),
            withAttributes: attrs
        )
        if tile.extraHolders > 0 && (tile.style == .running || tile.style == .unknown) {
            let extra = "+\(tile.extraHolders)"
            let attributes: [NSAttributedString.Key: Any] = [
                .font: NSFont.systemFont(ofSize: bounds.width * 0.12, weight: .semibold),
                .foregroundColor: NSColor.white
            ]
            extra.draw(at: NSPoint(x: bounds.midX - extra.size(withAttributes: attributes).width / 2,
                                   y: bounds.minY + bounds.height * 0.15), withAttributes: attributes)
        }
    }
}
