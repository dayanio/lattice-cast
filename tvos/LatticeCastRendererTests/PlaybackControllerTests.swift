import AVFoundation
import XCTest

@testable import LatticeCastRenderer

/// AVPlayer 播放后端真机行为测试（tvOS 模拟器 + 内嵌样例媒体 sample.mp4）：
/// 起播位置折叠进 Load、EOF → idle（T19）、pause/stop/volume 映射。
final class PlaybackControllerTests: XCTestCase {

    private let ctl = PlaybackController()

    override func tearDown() {
        ctl.stop() // 释放播放资源，避免用例间媒体会话相互干扰
        super.tearDown()
    }

    private func sampleURL() throws -> URL {
        // 测试资源打进测试 bundle（Bundle(for:) 须指向测试类，而非被测 app 类）
        let bundle = Bundle(for: PlaybackControllerTests.self)
        guard let url = bundle.url(forResource: "sample", withExtension: "mp4") else {
            XCTFail("测试媒体 sample.mp4 应存在于测试 bundle")
            throw NSError(domain: "fixture", code: 1)
        }
        return url
    }

    /// 轮询等待：泵主 runloop（AVFoundation 的 item 就绪/时长加载依赖主队列），
    /// 纯 Thread.sleep 阻塞主线程会饿死媒体管线导致模拟器下假性卡死。
    private func waitUntil(timeout: TimeInterval, _ cond: @escaping () -> Bool) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if cond() { return true }
            RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.1))
            Thread.sleep(forTimeInterval: 0.01)
        }
        return cond()
    }

    func testLoadPlaysFromStartPosition() throws {
        let err = ctl.load(url: try sampleURL().absoluteString, title: "样例", positionMS: 2000)
        XCTAssertNil(err)
        let ok = waitUntil(timeout: 10) {
            let st = self.ctl.status()
            return st.state == "playing" && st.positionMS >= 1000
        }
        XCTAssertTrue(ok, "应从起播位置 ≈2000ms 附近开始播放（实际 \(ctl.status().positionMS)ms）")
    }

    func testPauseReportsPaused() throws {
        _ = ctl.load(url: try sampleURL().absoluteString, title: "样例", positionMS: 0)
        XCTAssertTrue(waitUntil(timeout: 10) { self.ctl.status().state == "playing" })
        XCTAssertNil(ctl.pause())
        XCTAssertTrue(waitUntil(timeout: 5) { self.ctl.status().state == "paused" })
    }

    func testStopClearsToIdle() throws {
        _ = ctl.load(url: try sampleURL().absoluteString, title: "样例", positionMS: 0)
        XCTAssertTrue(waitUntil(timeout: 10) { self.ctl.status().state == "playing" })
        XCTAssertNil(ctl.stop())
        XCTAssertEqual(ctl.status().state, "idle", "stop 后应立即自报 idle")
    }

    func testVolumeScalesToPlayerVolume() {
        XCTAssertNil(ctl.volume(level: 42))
        XCTAssertEqual(ctl.player.volume, 0.42, accuracy: 0.001)
        XCTAssertNil(ctl.volume(level: 0))
        XCTAssertEqual(ctl.player.volume, 0.0, accuracy: 0.001)
        XCTAssertNil(ctl.volume(level: 100))
        XCTAssertEqual(ctl.player.volume, 1.0, accuracy: 0.001)
    }

    func testInvalidURLReportsError() {
        // 无 scheme 的相对串无法作为媒体地址（协议要求渲染端自行拉流的绝对 URL）
        XCTAssertNotNil(ctl.load(url: "noscheme.mp4", title: "x", positionMS: 0), "无法拉流的 URL 应报错（协议侧 ok=false）")
    }

    /// T19 对应：媒体自然播完（EOF），控制器自报 idle（全零）。
    func testEofTransitionsControllerToIdle() throws {
        _ = ctl.load(url: try sampleURL().absoluteString, title: "样例", positionMS: 0)
        let ok = waitUntil(timeout: 15) { self.ctl.status().state == "idle" }
        XCTAssertTrue(ok, "6 秒样例播完后应自报 idle")
        let st = ctl.status()
        XCTAssertEqual(st.positionMS, 0)
        XCTAssertEqual(st.durationMS, 0)
    }

    /// EOF → idle 的线上形态：真实 Controller + RendererServer，
    /// /status 必须从 playing 迁到 idle（不谎报 playing），且粘滞。
    func testServerWireEofReportsIdle() throws {
        let server = RendererServer(token: "s3cret", room: "卧室", name: "t", controller: ctl)
        try server.start(port: 0)
        defer { server.stop() }
        let port = server.boundPort

        let playBody = try JSONSerialization.data(withJSONObject: ["url": try sampleURL().absoluteString, "title": "样例"])
        let (playCode, _) = try TestHTTP.request(port, method: "POST", path: "/play", token: "s3cret", body: playBody)
        XCTAssertEqual(playCode, 200)

        // 播放中（起播后的头几秒）
        let playingFirst = waitUntil(timeout: 10) {
            guard let (code, body) = try? TestHTTP.request(port, method: "GET", path: "/status", token: "s3cret", body: nil),
                  code == 200 else { return false }
            return String(data: body, encoding: .utf8)!.contains("\"state\":\"playing\"")
        }
        XCTAssertTrue(playingFirst, "起播后 /status 应为 playing")

        // EOF 后：idle 全零（进度与标题清零），不谎报 playing
        let idle = waitUntil(timeout: 15) {
            guard let (code, body) = try? TestHTTP.request(port, method: "GET", path: "/status", token: "s3cret", body: nil),
                  code == 200 else { return false }
            return String(data: body, encoding: .utf8)!.contains("\"state\":\"idle\"")
        }
        XCTAssertTrue(idle, "播完（EOF）后 /status 应如实上报 idle")
        let (code, body) = try TestHTTP.request(port, method: "GET", path: "/status", token: "s3cret", body: nil)
        TestHTTP.assertWireBody(body, equals: "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0,\"title\":\"\",\"error\":\"\"}")
    }
}
