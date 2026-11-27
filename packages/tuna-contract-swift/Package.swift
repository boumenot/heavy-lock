// swift-tools-version: 6.1
// Generated Swift bindings for docs/tuna.proto. The protoc plugins are
// built from the same pinned dependencies as the runtime libraries, so the
// generated code always matches them. Regenerate with tool/generate.sh.
import PackageDescription

let package = Package(
    name: "TunaContract",
    platforms: [.macOS("26.0")],
    products: [.library(name: "TunaContract", targets: ["TunaContract"])],
    dependencies: [
        .package(url: "https://github.com/grpc/grpc-swift-2.git", exact: "2.4.4"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", exact: "2.5.0"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", exact: "2.13.3"),
        .package(url: "https://github.com/apple/swift-protobuf.git", exact: "1.41.0"),
    ],
    targets: [
        .target(
            name: "TunaContract",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
            ]
        ),
        .testTarget(
            name: "TunaContractTests",
            dependencies: [
                "TunaContract",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "GRPCNIOTransportHTTP2TransportServices", package: "grpc-swift-nio-transport"),
            ]
        ),
    ]
)
