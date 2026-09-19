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
        XCTAssertFalse(ctl.player.isMuted, "tvOS 上 AVPlayer 即音频通路，默认不得静音")
        XCTAssertNil(ctl.volume(level: 42))
        XCTAssertEqual(ctl.player.volume, 0.42, accuracy: 0.001)
        XCTAssertNil(ctl.volume(level: 0))
        XCTAssertEqual(ctl.player.volume, 0.0, accuracy: 0.001)
        XCTAssertNil(ctl.volume(level: 100))
        XCTAssertEqual(ctl.player.volume, 1.0, accuracy: 0.001)
        XCTAssertFalse(ctl.player.isMuted, "volume 调节不得顺带静音")
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

    // MARK: - 旧 item 回调隔离（generation guard + current-item 身份校验）

    /// 旧 item 的迟到播完通知不得杀死新会话（服务端会把"控制器自报 idle"翻译成
    /// eof→idle 迁移，误触发即掐断刚 /play 的新媒体——T19 的反面事故）。
    func testStaleEndNotificationFromPreviousItemDoesNotKillNewSession() throws {
        _ = ctl.load(url: try sampleURL().absoluteString, title: "A", positionMS: 0)
        XCTAssertTrue(waitUntil(timeout: 10) { self.ctl.status().state == "playing" })
        let itemA = ctl.player.currentItem
        XCTAssertNotNil(itemA)

        _ = ctl.load(url: try sampleURL().absoluteString, title: "B", positionMS: 0)
        XCTAssertTrue(waitUntil(timeout: 10) { self.ctl.status().state == "playing" })

        // 模拟旧 item A 的迟到播完通知：block observer 按 object 匹配仍会送达旧
        // handler（除非显式 removeObserver），generation guard 须忽略它
        NotificationCenter.default.post(name: NSNotification.Name.AVPlayerItemDidPlayToEndTime, object: itemA)

        XCTAssertEqual(ctl.status().state, "playing", "旧 item 的 EOF 不得让新会话迁 idle")

        // 当前 item 自然播完仍应正常 EOF→idle（guard 不误伤当前会话）
        XCTAssertTrue(waitUntil(timeout: 15) { self.ctl.status().state == "idle" }, "新 item 播完仍应 EOF→idle")
    }

    /// 旧 item 的失败回调不得清空当前 item（KVO 失败处理须校验"失败者仍是 current"）。
    func testStaleItemFailureDoesNotClearCurrentItem() throws {
        // 构造一个必然失败的 item（不存在的文件）。item 只有挂到 player 上才会
        // 开始评估状态（否则永远 .unknown），故用一次性 probe player 触发。
        let broken = AVPlayerItem(url: URL(fileURLWithPath: "/nonexistent/stale.mp4"))
        let probe = AVPlayer()
        probe.isMuted = true
        probe.replaceCurrentItem(with: broken)
        XCTAssertTrue(waitUntil(timeout: 10) { broken.status == .failed })
        probe.replaceCurrentItem(with: nil)

        _ = ctl.load(url: try sampleURL().absoluteString, title: "B", positionMS: 0)
        XCTAssertTrue(waitUntil(timeout: 10) { self.ctl.status().state == "playing" })
        let good = ctl.player.currentItem
        XCTAssertNotNil(good)

        // 注入旧 item 的失败回调：旧 generation
        ctl.handleItemStatus(broken, generation: -1)
        XCTAssertEqual(ctl.status().state, "playing", "旧 generation 的失败不得影响新会话")
        XCTAssertTrue(ctl.player.currentItem === good, "旧 item 失败不得清空当前 item")

        // 注入旧 item 的失败回调：generation 相同但 item 非 current（身份校验兜底）
        ctl.handleItemStatus(broken, generation: ctl.currentGeneration)
        XCTAssertEqual(ctl.status().state, "playing")
        XCTAssertTrue(ctl.player.currentItem === good)
    }

    /// 当前 item 加载失败（源不可达）→ 自报 idle 的真实 KVO 路径仍须工作。
    func testCurrentItemFailureReportsIdle() throws {
        let brokenURL = URL(fileURLWithPath: "/nonexistent/current.mp4")
        XCTAssertNil(ctl.load(url: brokenURL.absoluteString, title: "x", positionMS: 0))
        XCTAssertTrue(waitUntil(timeout: 10) { self.ctl.status().state == "idle" }, "当前 item 加载失败应自报 idle")
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
