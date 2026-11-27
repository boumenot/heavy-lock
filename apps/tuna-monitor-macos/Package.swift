// swift-tools-version: 6.1
// The Tuna macOS monitor. Its third-party dependencies are pinned in
// flake.nix like those of packages/tuna-contract-swift; change them there, in
// this manifest and in Package.resolved together.
import PackageDescription

let package = Package(
    name: "TunaMonitor",
    platforms: [.macOS("26.0")],
    products: [.executable(name: "TunaMonitor", targets: ["TunaMonitor"])],
    dependencies: [
        .package(path: "../../packages/tuna-contract-swift"),
        .package(url: "https://github.com/grpc/grpc-swift-2.git", exact: "2.4.4"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", exact: "2.5.0"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", exact: "2.13.3"),
    ],
    targets: [
        .target(
            name: "TunaMonitorCore",
            dependencies: [
                .product(name: "TunaContract", package: "tuna-contract-swift"),
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "GRPCNIOTransportHTTP2TransportServices", package: "grpc-swift-nio-transport"),
            ]
        ),
        .executableTarget(
            name: "TunaMonitor",
            dependencies: ["TunaMonitorCore", .product(name: "TunaContract", package: "tuna-contract-swift")]
        ),
        .testTarget(
            name: "TunaMonitorCoreTests",
            dependencies: ["TunaMonitorCore", .product(name: "TunaContract", package: "tuna-contract-swift")]
        ),
    ]
)
