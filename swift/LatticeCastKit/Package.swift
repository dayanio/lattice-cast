// swift-tools-version:5.9
// LatticeCastKit：LatticeCast 渲染端可复用 Swift 包（protocol.md v1）。
// macOS/iOS/tvOS 通用；HTTP 层复用 Swifter（钉 revision，同 tvOS T18 先例）。
import PackageDescription

let package = Package(
    name: "LatticeCastKit",
    platforms: [
        .macOS(.v13),
        .iOS(.v15),
        .tvOS(.v15),
    ],
    products: [
        .library(name: "LatticeCastKit", targets: ["LatticeCastKit"]),
    ],
    dependencies: [
        // 同 tvos/LatticeCastRenderer 的 Package.resolved 钉（1e4f51c9）。
        .package(url: "https://github.com/httpswift/swifter", revision: "1e4f51c92d7ca486242d8bf0722b99de2c3531aa"),
    ],
    targets: [
        .target(
            name: "LatticeCastKit",
            dependencies: [.product(name: "Swifter", package: "swifter")],
            path: "Sources/LatticeCastKit"
        ),
        .testTarget(
            name: "LatticeCastKitTests",
            dependencies: ["LatticeCastKit"],
            path: "Tests/LatticeCastKitTests",
            resources: [.copy("Fixtures")]
        ),
    ]
)
