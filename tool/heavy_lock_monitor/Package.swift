// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "HeavyLockMonitor",
    platforms: [.macOS(.v13)],
    products: [.executable(name: "HeavyLockMonitor", targets: ["HeavyLockMonitor"])],
    targets: [
        .executableTarget(name: "HeavyLockMonitor"),
        .testTarget(name: "HeavyLockMonitorTests", dependencies: ["HeavyLockMonitor"]),
    ]
)
