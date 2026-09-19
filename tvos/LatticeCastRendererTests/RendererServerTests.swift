import Foundation
import XCTest

@testable import LatticeCastRenderer

/// 渲染端 wire protocol v1 契约测试（docs/protocol.md 唯一权威定义）。
/// 与 Go 侧 internal/cast/renderer/server_test.go 同构：以 testdata/contract/
/// 契约夹具为线上基准，经真实 HTTP（URLSession → Swifter 内嵌 server）驱动。
final class RendererServerTests: XCTestCase {
    private var server: RendererServer?
    private var fake = FakeController()
    private var port = 0

    override func tearDown() {
        server?.stop()
        server = nil
        super.tearDown()
    }

    private func startServer(token: String = TestHTTP.token) throws {
        let (srv, p) = try TestHTTP.makeServer(token: token, controller: fake)
        server = srv
        port = p
    }

    // ---- 状态机（protocol.md 第六节）----

    func testStatusInitiallyIdle() throws {
        try startServer()
        let (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(code, 200)
        XCTAssertEqual(
            String(data: body, encoding: .utf8),
            "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"\"}\n"
        )
    }

    func testPlayPauseStopStateMachine() throws {
        try startServer()

        // play：idle → playing，url/title/position 下发到 Controller（夹具驱动）
        let req = try TestHTTP.fixture("play_request.json")
        var (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token, json: req)
        XCTAssertEqual(code, 200)
        try TestHTTP.assertWireBody(body, equalsFixture: "play_response.json")
        XCTAssertEqual(fake.loadedURL, "http://192.168.1.10:7810/media/abc123", "url 应原样下发 Controller")
        XCTAssertEqual(fake.loadedTitle, "Interstellar")
        XCTAssertEqual(fake.loadedPositionMS, 0, "夹具 position_ms=0 应原样下发")

        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(code, 200)
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"state\":\"playing\""))
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"title\":\"Interstellar\""))

        // pause：playing → paused，标题保留
        (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/pause", token: TestHTTP.token, json: "")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"paused\"}")
        XCTAssertEqual(fake.pauseCalls, 1)

        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"state\":\"paused\""))
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"title\":\"Interstellar\""))

        // stop：paused → idle，媒体清空
        (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/stop", token: TestHTTP.token, json: "")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"idle\"}")
        XCTAssertEqual(fake.stopCalls, 1)

        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        TestHTTP.assertWireBody(body, equals: "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"\"}")
    }

    /// /play 携 position_ms 时把续播位置折叠进 Load（对齐 Go server.go：不再单独 seek）。
    func testPlayStartPositionPassedToController() throws {
        try startServer()
        let (code, body) = try TestHTTP.requestJSON(
            port, method: "POST", path: "/play", token: TestHTTP.token,
            json: "{\"url\":\"http://x/y.mp4\",\"title\":\"续播\",\"position_ms\":12500}")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"playing\"}")
        XCTAssertEqual(fake.loadedPositionMS, 12500, "起播位置应传给播放后端")
        XCTAssertEqual(fake.seekCalls, 0, "位置随 Load 下发，不单独 seek（对齐 Go 实现）")
    }

    /// GET /status 播放中样例：完整 JSON 与契约夹具字节级一致
    /// （position_ms 42000 随 /play 起播位置下发，对齐 Go fake 的 Load 折叠语义）。
    func testStatusFullJSONMatchesFixture() throws {
        try startServer()
        fake.setDuration(ms: 5400000)
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://192.168.1.10:7810/media/abc123\",\"title\":\"Interstellar\",\"position_ms\":42000}")

        let (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(code, 200)
        XCTAssertEqual(fake.loadedPositionMS, 42000, "夹具起播位置应传给播放后端")
        try TestHTTP.assertWireBody(body, equalsFixture: "status_response.json")
    }

    // ---- 鉴权（protocol.md 第二节）----

    func testUnauthorizedWrongOrMissingToken() throws {
        try startServer()
        for token in ["", "wrong"] {
            let (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: token.isEmpty ? nil : token, body: nil)
            XCTAssertEqual(code, 401, "token=\(token.isEmpty ? "<缺失>" : token) 应 401")
            TestHTTP.assertWireBody(body, equals: "{\"ok\":false,\"error\":\"unauthorized\"}")
        }
        // 401 覆盖所有端点（含错误方法：鉴权先于方法校验，对齐 Go mux 包装顺序）
        let (code, body) = try TestHTTP.request(port, method: "POST", path: "/play", token: "wrong", body: Data("{}".utf8))
        XCTAssertEqual(code, 401)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":false,\"error\":\"unauthorized\"}")
    }

    // ---- 通用错误（protocol.md 第五节）----

    func testWireErrors() throws {
        try startServer()
        let cases: [(String, String, String, String, Int)] = [
            ("unknown path", "POST", "/nope", "", 404),
            ("method not allowed", "POST", "/status", "", 405),
            ("get on play", "GET", "/play", "", 405),
            ("put on volume", "PUT", "/volume", "{}", 405),
            ("invalid json body", "POST", "/play", "{not-json", 400),
            ("missing url", "POST", "/play", "{\"title\":\"x\"}", 400),
            ("wrong url type", "POST", "/play", "{\"url\":123}", 400),
            ("invalid seek body", "POST", "/seek", "{oops", 400),
            ("invalid volume body", "POST", "/volume", "{oops", 400),
        ]
        for (name, method, path, json, wantCode) in cases {
            let (code, body) = try TestHTTP.requestJSON(port, method: method, path: path, token: TestHTTP.token, json: json)
            XCTAssertEqual(code, wantCode, name)
            let want: String
            switch wantCode {
            case 404: want = "{\"ok\":false,\"error\":\"not_found\"}"
            case 405: want = "{\"ok\":false,\"error\":\"method_not_allowed\"}"
            default: want = "{\"ok\":false,\"error\":\"bad_request\"}"
            }
            TestHTTP.assertWireBody(body, equals: want)
        }
    }

    // ---- error 态粘滞（protocol.md 第六节）----

    func testPlayFailureErrorStickyUntilNextPlay() throws {
        try startServer()

        // 先正常播放并 seek，让旧媒体信息在位（失败后应被清空）
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://x/a.mp4\",\"title\":\"Interstellar\"}")
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/seek", token: TestHTTP.token,
                                     json: "{\"position_ms\":90000}")

        fake.failNextLoad(msg: "source_unreachable")
        var (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                                    json: "{\"url\":\"http://x/a.mp4\",\"title\":\"Interstellar\"}")
        XCTAssertEqual(code, 200, "渲染端失败仍是 HTTP 200（应用层报错）")
        TestHTTP.assertWireBody(body, equals: "{\"ok\":false,\"state\":\"error\",\"error\":\"source_unreachable\"}")

        // 渲染端进入 error 态（旧媒体信息清零），且粘滞
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        try TestHTTP.assertWireBody(body, equalsFixture: "status_error_response.json")

        // error 态下 /stop /pause /seek /volume 均不产生迁移
        for (method, path, json) in [("POST", "/stop", ""), ("POST", "/pause", ""), ("POST", "/seek", "{\"position_ms\":1000}"), ("POST", "/volume", "{\"level\":10}")] {
            (code, body) = try TestHTTP.requestJSON(port, method: method, path: path, token: TestHTTP.token, json: json)
            XCTAssertEqual(code, 200)
            TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"error\"}")
        }
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        try TestHTTP.assertWireBody(body, equalsFixture: "status_error_response.json")

        // 离开 error 的唯一方式：下一次 /play 成功
        (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                                json: "{\"url\":\"http://x/a.mp4\",\"title\":\"Interstellar\"}")
        TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"playing\"}")
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"state\":\"playing\""))
    }

    func testPlayFailureResponseIs200WithOkFalse() throws {
        try startServer()
        fake.failNextLoad(msg: "source_unreachable")
        let (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                                    json: "{\"url\":\"http://x/y.mp4\"}")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":false,\"state\":\"error\",\"error\":\"source_unreachable\"}")
    }

    func testControllerCommandFailureReportsOkFalseWithoutMigration() throws {
        try startServer()
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://x/y.mp4\",\"title\":\"Interstellar\"}")

        fake.setPauseError("ipc down")
        let (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/pause", token: TestHTTP.token, json: "")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":false,\"state\":\"playing\",\"error\":\"ipc down\"}")

        fake.setPauseError(nil)
        let (code2, body2) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(code2, 200)
        XCTAssertTrue(String(data: body2, encoding: .utf8)!.contains("\"state\":\"playing\""))
    }

    // ---- idle 幂等（protocol.md 第四节）----

    func testIdleCommandsIdempotentNoMigration() throws {
        try startServer()
        for (method, path, json) in [("POST", "/pause", ""), ("POST", "/stop", ""), ("POST", "/seek", "{\"position_ms\":5000}"), ("POST", "/volume", "{\"level\":30}")] {
            let (code, body) = try TestHTTP.requestJSON(port, method: method, path: path, token: TestHTTP.token, json: json)
            XCTAssertEqual(code, 200)
            TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"idle\"}")
        }
        XCTAssertNil(fake.loadedURL, "idle 下不应下发 Controller load")
        XCTAssertEqual(fake.volumeLevel, 0, "idle 下 volume 不应下发")
        XCTAssertEqual(fake.seekCalls, 0, "idle 下 seek 不应下发")
    }

    func testSeekVolumePositionFromController() throws {
        try startServer()
        _ = fake.load(url: "http://x/y.mp4", title: "Interstellar", positionMS: 0)
        fake.setControllerState("playing")
        fake.setDuration(ms: 5400000)
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://x/y.mp4\",\"title\":\"Interstellar\"}")

        // seek → 状态不变，进度来自 Controller.Status()
        var (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/seek", token: TestHTTP.token,
                                                    json: "{\"position_ms\":90000}")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"playing\"}")
        var (c, b) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(c, 200)
        XCTAssertEqual(
            String(data: b, encoding: .utf8),
            "{\"state\":\"playing\",\"position_ms\":90000,\"duration_ms\":5400000,\"title\":\"Interstellar\",\"error\":\"\"}\n"
        )

        // volume（夹具驱动）→ 状态与进度均不变
        let volReq = try TestHTTP.fixture("volume_request.json")
        (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/volume", token: TestHTTP.token, json: volReq)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"playing\"}")
        XCTAssertEqual(fake.volumeLevel, 42, "夹具 level=42 应原样下发")
        (c, b) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(
            String(data: b, encoding: .utf8),
            "{\"state\":\"playing\",\"position_ms\":90000,\"duration_ms\":5400000,\"title\":\"Interstellar\",\"error\":\"\"}\n"
        )
    }

    // ---- EOF → idle（T19：播完如实上报 idle，粘滞至下一次 /play）----

    func testStatusControllerEofTransitionsToIdle() throws {
        try startServer()
        fake.setDuration(ms: 5400000)
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://x/y.mp4\",\"title\":\"Interstellar\"}")
        var (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(code, 200)
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"state\":\"playing\""))

        // 播放后端到达 EOF：控制器自报 idle → Server 状态机一并迁移（进度与标题清零）
        fake.setControllerState("idle")
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        TestHTTP.assertWireBody(body, equals: "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"\"}")

        // 迁移粘滞：再次 /status 仍 idle，不得回 playing
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        TestHTTP.assertWireBody(body, equals: "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"\"}")

        // paused 态同理：暂停中播完后端空转也须落到 idle
        fake.setControllerState(nil)
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://x/y.mp4\",\"title\":\"Interstellar\"}")
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/pause", token: TestHTTP.token, json: "")
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"state\":\"paused\""))

        fake.setControllerState("idle")
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        TestHTTP.assertWireBody(body, equals: "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"\"}")

        // 新一次 /play 显然重置：playing 恢复（Fake 的 durationMS 未清，仍在）
        fake.setControllerState(nil)
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://x/y.mp4\",\"title\":\"Interstellar\"}")
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"state\":\"playing\""))
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"duration_ms\":5400000"))
    }
}
