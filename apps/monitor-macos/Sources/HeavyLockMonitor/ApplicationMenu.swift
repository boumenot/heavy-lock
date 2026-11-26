import AppKit

func makeApplicationMenu() -> NSMenu {
    let main = NSMenu()
    let item = NSMenuItem()
    main.addItem(item)
    let menu = NSMenu()
    menu.addItem(withTitle: "Quit Heavy Lock Status", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
    item.submenu = menu
    let editItem = NSMenuItem(title: "Edit", action: nil, keyEquivalent: "")
    let edit = NSMenu(title: "Edit")
    edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
    edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
    editItem.submenu = edit
    main.addItem(editItem)
    return main
}
