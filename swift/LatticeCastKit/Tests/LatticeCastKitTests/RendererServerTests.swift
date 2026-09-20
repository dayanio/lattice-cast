import Foundation
import XCTest

@testable import LatticeCastKit

/// 渲染端 wire protocol v1 契约测试（docs/protocol.md 唯一权威定义）。
/// 与 Go 侧 internal/cast/renderer/server_test.go 及 tvOS T18 契约测试同构：
/// 以 testdata/contract/ 契约夹具为线上基准，经真实 HTTP（URLSession →
/// Swifter 内嵌 server）驱动，响应体与夹具字节级比对。
final class RendererServerTests: XCTestCase {
    private var server: RendererServer?
    private var mock = MockPlaybackController()
    private var port = 0

    override func tearDown() {
        server?.stop()
        server = nil
        super.tearDown()
    }

    private func startServer(token: String = TestHTTP.token) throws {
        let (srv, p) = try TestHTTP.makeServer(token: token, controller: mock)
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
        XCTAssertEqual(mock.lastLoad?.url.absoluteString, "http://192.168.1.10:7810/media/abc123", "url 应原样下发 Controller")
        XCTAssertEqual(mock.lastLoad?.title, "Interstellar")
        XCTAssertEqual(mock.lastLoad?.positionMS, 0, "夹具 position_ms=0 应原样下发")

        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(code, 200)
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"state\":\"playing\""))
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"title\":\"Interstellar\""))

        // pause：playing → paused，标题保留
        (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/pause", token: TestHTTP.token, json: "")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"paused\"}")
        XCTAssertEqual(mock.pauseCalls, 1)

        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"state\":\"paused\""))
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"title\":\"Interstellar\""))

        // stop：paused → idle，媒体清空
        (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/stop", token: TestHTTP.token, json: "")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"idle\"}")
        XCTAssertEqual(mock.stopCalls, 1)

        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        TestHTTP.assertWireBody(body, equals: "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"\"}")
    }

    /// /play 携 position_ms 时把续播位置折叠进 load（对齐 Go server.go：不再单独 seek）。
    func testPlayStartPositionPassedToController() throws {
        try startServer()
        let (code, body) = try TestHTTP.requestJSON(
            port, method: "POST", path: "/play", token: TestHTTP.token,
            json: "{\"url\":\"http://x/y.mp4\",\"title\":\"续播\",\"position_ms\":12500}")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"playing\"}")
        XCTAssertEqual(mock.lastLoad?.positionMS, 12500, "起播位置应传给播放后端")
        XCTAssertEqual(mock.lastLoad?.url.absoluteString, "http://x/y.mp4")
        XCTAssertEqual(mock.lastLoad?.title, "续播")
        XCTAssertEqual(mock.seekCalls, 0, "位置随 load 下发，不单独 seek（对齐 Go 实现）")
    }

    /// GET /status 播放中样例：完整 JSON 与契约夹具字节级一致
    /// （position_ms 42000 随 /play 起播位置下发，对齐 Go fake 的 Load 折叠语义）。
    func testStatusFullJSONMatchesFixture() throws {
        try startServer()
        mock.setDuration(ms: 5_400_000)
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://192.168.1.10:7810/media/abc123\",\"title\":\"Interstellar\",\"position_ms\":42000}")

        let (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(code, 200)
        XCTAssertEqual(mock.lastLoad?.positionMS, 42000, "夹具起播位置应传给播放后端")
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

        mock.failNextLoad(msg: "source_unreachable")
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
        mock.failNextLoad(msg: "source_unreachable")
        let (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                                    json: "{\"url\":\"http://x/y.mp4\"}")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":false,\"state\":\"error\",\"error\":\"source_unreachable\"}")
    }

    /// /play 的 url 无法构造绝对 URL 时（宿主 PlaybackController 要求 URL 类型），
    /// 与后端加载失败同构：HTTP 200 + error 态（对齐 Go：url 可用性由渲染后端裁决）。
    func testUnparseableURLBecomesErrorState() throws {
        try startServer()
        // 构造失败（Foundation 新解析器对 scheme 含空格返回 nil）
        var (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                                    json: "{\"url\":\"ht tp://x/y.mp4\"}")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":false,\"state\":\"error\",\"error\":\"invalid_url\"}")
        // 非 URL（无 scheme/host）：可构造但非绝对地址，同样拒绝
        (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                                json: "{\"url\":\"not a url\"}")
        TestHTTP.assertWireBody(body, equals: "{\"ok\":false,\"state\":\"error\",\"error\":\"invalid_url\"}")
        // error 态粘滞：后续 /status 如实上报（error 态信息清零形态对齐夹具）
        let (scode, sbody) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(scode, 200)
        TestHTTP.assertWireBody(sbody, equals: "{\"state\":\"error\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"invalid_url\"}")
        XCTAssertTrue(mock.loadCalls.isEmpty, "URL 不可构造时不应下发 Controller")
        // 下一次 /play 成功即可离开 error 态
        let (rcode, rbody) = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                                      json: "{\"url\":\"http://x/ok.mp4\"}")
        XCTAssertEqual(rcode, 200)
        TestHTTP.assertWireBody(rbody, equals: "{\"ok\":true,\"state\":\"playing\"}")
    }

    func testControllerCommandFailureReportsOkFalseWithoutMigration() throws {
        try startServer()
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://x/y.mp4\",\"title\":\"Interstellar\"}")

        mock.setPauseError("ipc down")
        let (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/pause", token: TestHTTP.token, json: "")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":false,\"state\":\"playing\",\"error\":\"ipc down\"}")

        mock.setPauseError(nil)
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
        XCTAssertTrue(mock.loadCalls.isEmpty, "idle 下不应下发 Controller load")
        XCTAssertEqual(mock.volumeLevel, -1, "idle 下 volume 不应下发")
        XCTAssertEqual(mock.seekCalls, 0, "idle 下 seek 不应下发")
    }

    func testSeekVolumePositionFromController() throws {
        try startServer()
        _ = try? mock.load(url: URL(string: "http://x/y.mp4")!, title: "Interstellar", positionMS: 0)
        mock.setControllerState("")
        mock.setDuration(ms: 5_400_000)
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://x/y.mp4\",\"title\":\"Interstellar\"}")

        // seek → 状态不变，进度来自 Controller.status()
        var (code, body) = try TestHTTP.requestJSON(port, method: "POST", path: "/seek", token: TestHTTP.token,
                                                    json: "{\"position_ms\":90000}")
        XCTAssertEqual(code, 200)
        TestHTTP.assertWireBody(body, equals: "{\"ok\":true,\"state\":\"playing\"}")
        XCTAssertEqual(mock.lastSeekMS, 90000, "seek 位置应原样下发 Controller")
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
        XCTAssertEqual(mock.volumeLevel, 42, "夹具 level=42 应原样下发")
        (c, b) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(
            String(data: b, encoding: .utf8),
            "{\"state\":\"playing\",\"position_ms\":90000,\"duration_ms\":5400000,\"title\":\"Interstellar\",\"error\":\"\"}\n"
        )
    }

    // ---- EOF → idle（T19：播完如实上报 idle，粘滞至下一次 /play）----

    func testStatusControllerEofTransitionsToIdle() throws {
        try startServer()
        mock.setDuration(ms: 5_400_000)
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://x/y.mp4\",\"title\":\"Interstellar\"}")
        var (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertEqual(code, 200)
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"state\":\"playing\""))

        // 播放后端到达 EOF：控制器自报 idle → Server 状态机一并迁移（进度与标题清零）
        mock.setControllerState("idle")
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        TestHTTP.assertWireBody(body, equals: "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"\"}")

        // 迁移粘滞：再次 /status 仍 idle，不得回 playing
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        TestHTTP.assertWireBody(body, equals: "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"\"}")

        // paused 态同理：暂停中播完后端空转也须落到 idle
        mock.setControllerState(nil)
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://x/y.mp4\",\"title\":\"Interstellar\"}")
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/pause", token: TestHTTP.token, json: "")
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"state\":\"paused\""))

        mock.setControllerState("idle")
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        TestHTTP.assertWireBody(body, equals: "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"\"}")

        // 新一次 /play 显然重置：playing 恢复（Mock 的 durationMS 未清，仍在）
        mock.setControllerState(nil)
        _ = try TestHTTP.requestJSON(port, method: "POST", path: "/play", token: TestHTTP.token,
                                     json: "{\"url\":\"http://x/y.mp4\",\"title\":\"Interstellar\"}")
        (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: TestHTTP.token, body: nil)
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"state\":\"playing\""))
        XCTAssertTrue(String(data: body, encoding: .utf8)!.contains("\"duration_ms\":5400000"))
    }
}

/// Wire 序列化单元测试：转义规则钉死到 Go encoding/json 的实测输出
/// （生成命令见任务报告；HTML 转义 + 小写 \u00xx 控制符 + 非法字符原样 UTF-8）。
final class WireEncodingTests: XCTestCase {

    func testJsonStringEscapingMatchesGoEncodingJSON() {
        let cases: [(String, String)] = [
            ("<>&", "\"\\u003c\\u003e\\u0026\""),                        // Go: {"v":"\u003c\u003e\u0026"}
            ("a\"b\\c\nd\te", "\"a\\\"b\\\\c\\nd\\te\""),                // Go: {"v":"a\"b\\c\nd\te"}
            ("", "\"\""),                                                // Go: {"v":""}
            ("\u{01}\u{1f}", "\"\\u0001\\u001f\""),                      // Go: {"v":"\u0001\u001f"}
            ("卧室 Interstellar — é漢", "\"卧室 Interstellar — é漢\""), // 非 ASCII 原样
            ("\u{2028}\u{2029}", "\"\\u2028\\u2029\""),                  // Go: LS/PS 转义
            ("tab\tless\0", "\"tab\\tless\\u0000\""),
        ]
        for (input, expected) in cases {
            XCTAssertEqual(Wire.jsonString(input), expected, "转义须与 Go encoding/json 字节级一致")
        }
    }

    func testCmdBodyOmitsErrorWhenEmpty() {
        XCTAssertEqual(Wire.cmdBody(ok: true, state: "playing"), "{\"ok\":true,\"state\":\"playing\"}")
        XCTAssertEqual(Wire.cmdBody(ok: false, state: "error", error: "source_unreachable"),
                       "{\"ok\":false,\"state\":\"error\",\"error\":\"source_unreachable\"}")
    }

    func testStatusBodyFullFieldOrder() {
        XCTAssertEqual(
            Wire.statusBody(state: "playing", positionMS: 42000, durationMS: 5_400_000, title: "Interstellar", error: ""),
            "{\"state\":\"playing\",\"position_ms\":42000,\"duration_ms\":5400000,\"title\":\"Interstellar\",\"error\":\"\"}"
        )
    }

    func testErrorBody() {
        XCTAssertEqual(Wire.errorBody("unauthorized"), "{\"ok\":false,\"error\":\"unauthorized\"}")
    }

    /// 解析器防御：顶层 null 对齐 Go json.Decode（成功且零值）→ 空对象；
    /// 顶层非对象 → bad_request。
    func testParseObjectDefensive() throws {
        XCTAssertEqual(try Wire.parseObject(Data("null".utf8)).count, 0, "顶层 null 对齐 Go：成功且零值 → 空对象")
        XCTAssertThrowsError(try Wire.parseObject(Data("[1,2]".utf8)))
        XCTAssertThrowsError(try Wire.parseObject(Data("\"str\"".utf8)))
        XCTAssertThrowsError(try Wire.parseObject(Data()))

        // 类型不符 → bad_request（对齐 Go 解码错误）
        let obj = try Wire.parseObject(Data("{\"title\":123,\"position_ms\":true,\"position_ms2\":1.5}".utf8))
        XCTAssertThrowsError(try Wire.optString(obj, "title"))
        XCTAssertThrowsError(try Wire.optInt(obj, "position_ms"))
        XCTAssertThrowsError(try Wire.optInt(obj, "position_ms2"))
        XCTAssertNil(try Wire.optString(obj, "absent"))
        XCTAssertNil(try Wire.optInt(obj, "absent"))
        // JSON null 字段 → 缺省（对齐 Go：null 解码为零值）
        let obj2 = try Wire.parseObject(Data("{\"title\":null,\"position_ms\":null}".utf8))
        XCTAssertNil(try Wire.optString(obj2, "title"))
        XCTAssertNil(try Wire.optInt(obj2, "position_ms"))
    }
}
