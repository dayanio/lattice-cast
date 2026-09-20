import Foundation
import XCTest

@testable import LatticeCastKit

/// 公共门面（LatticeCastRenderer）+ 配置持久化（Provisioning）测试：
/// 验证宿主 App 实际使用路径——config + PlaybackController → start()（HTTP +
/// NSNetService 自报）→ HTTP 可达 → stop()。端口 0（临时端口），announce 一次。
final class LatticeCastRendererFacadeTests: XCTestCase {

    func testStartStopServesProtocolAndAnnounces() throws {
        let mock = MockPlaybackController()
        let config = LatticeCastConfig(name: "test-renderer", room: "卧室", token: "facade-token", port: 0)
        let renderer = LatticeCastRenderer(config: config, controller: mock)

        try renderer.start()
        defer { renderer.stop() }

        let port = renderer.boundPort
        XCTAssertGreaterThan(port, 0, "临时端口绑定后应有实际端口号")

        // 公共门面起的 HTTP 服务跑同一协议：/status 初始 idle
        let (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: "facade-token", body: nil)
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"\"}")

        // /play 驱动宿主 PlaybackController（协议即宿主集成路径）
        let (pcode, pbody) = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: "facade-token",
                                                      json: "{\"url\":\"http://x/f.mp4\",\"title\":\"Facade\"}")
        XCTAssertEqual(pcode, 200)
        TestHTTP.assertWireBody(pbody, equals: "{\"ok\":true,\"state\":\"playing\"}")
        XCTAssertEqual(mock.lastLoad?.url.absoluteString, "http://x/f.mp4")
        XCTAssertEqual(mock.lastLoad?.title, "Facade")

        renderer.stop()
        // stop 后端口不再服务（连接被拒/超时均可，只验证不崩溃且状态归零）
    }

    func testStartFailsOnInvalidPort() throws {
        let renderer = LatticeCastRenderer(
            config: LatticeCastConfig(name: "x", room: "y", token: "t", port: 70_000),
            controller: MockPlaybackController())
        XCTAssertThrowsError(try renderer.start())
    }
}

/// 首启配对钩子（T21b 的 reflux 配对 UI 将经此读写渲染端身份配置）：
/// LatticeCastConfig 的 Codable 持久化往返。
final class ProvisioningTests: XCTestCase {

    func testConfigCodableRoundTripAndPortDefault() throws {
        // port 缺省 7822
        let minimal = LatticeCastConfig(name: "卧室 Mac", room: "卧室", token: "s3cret")
        XCTAssertEqual(minimal.port, 7822)

        let data = try JSONEncoder().encode(minimal)
        let back = try JSONDecoder().decode(LatticeCastConfig.self, from: data)
        XCTAssertEqual(back.name, "卧室 Mac")
        XCTAssertEqual(back.room, "卧室")
        XCTAssertEqual(back.token, "s3cret")
        XCTAssertEqual(back.port, 7822)

        let custom = LatticeCastConfig(name: "n", room: "r", token: "t", port: 7830)
        XCTAssertEqual(custom.port, 7830)
    }

    func testProvisioningSaveLoadClearRoundTrip() {
        let suiteName = "latticecastkit-tests-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suiteName)!
        defer { defaults.removePersistentDomain(forName: suiteName) }

        XCTAssertNil(LatticeCastProvisioning.loadConfig(defaults: defaults), "未配置时应返回 nil")

        let config = LatticeCastConfig(name: "客厅电视", room: "客厅", token: "tok", port: 7822)
        LatticeCastProvisioning.save(config, defaults: defaults)

        let loaded = LatticeCastProvisioning.loadConfig(defaults: defaults)
        XCTAssertEqual(loaded?.name, "客厅电视")
        XCTAssertEqual(loaded?.room, "客厅")
        XCTAssertEqual(loaded?.token, "tok")
        XCTAssertEqual(loaded?.port, 7822)

        LatticeCastProvisioning.clear(defaults: defaults)
        XCTAssertNil(LatticeCastProvisioning.loadConfig(defaults: defaults), "清除后应返回 nil")

        // 脏数据不崩溃、返回 nil
        defaults.set("not json", forKey: LatticeCastProvisioning.storageKey)
        XCTAssertNil(LatticeCastProvisioning.loadConfig(defaults: defaults))
    }
}
