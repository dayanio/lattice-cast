import Foundation
import XCTest

@testable import LatticeCastRenderer

// 测试用 Controller（镜像 Go 侧 internal/cast/renderer/server_test.go 的
// fakeController）：内存态记录播放后端状态，可注入 Load 失败（模拟源不可达
// → error 态）与 Pause 失败；只供给位置/时长/标题，协议状态机归 Server 所有。
// ctlState 语义与 Go 一致：nil 表示"状态机全归 Server"，"idle" 模拟播完 EOF。
final class FakeController: PlaybackControlling {
    private let lock = NSLock()

    private(set) var loadedURL: String?
    private(set) var loadedTitle: String?
    private(set) var loadedPositionMS: Int64 = 0
    private(set) var durationMS: Int64 = 0
    private(set) var volumeLevel = 0
    private(set) var seekCalls = 0
    private(set) var stopCalls = 0
    private(set) var pauseCalls = 0

    private var ctlState: String?
    private var failLoadArmed = false
    private var failLoadMsg = ""
    private var pauseErr: String?

    func load(url: String, title: String, positionMS: Int64) -> String? {
        lock.lock(); defer { lock.unlock() }
        if failLoadArmed {
            failLoadArmed = false
            return failLoadMsg
        }
        loadedURL = url
        loadedTitle = title
        loadedPositionMS = positionMS // 续播位置随 Load 折叠下发
        return nil
    }

    func pause() -> String? {
        lock.lock(); defer { lock.unlock() }
        pauseCalls += 1
        return pauseErr
    }

    func stop() -> String? {
        lock.lock(); defer { lock.unlock() }
        stopCalls += 1
        return nil
    }

    func seekTo(ms: Int64) -> String? {
        lock.lock(); defer { lock.unlock() }
        seekCalls += 1
        loadedPositionMS = ms
        return nil
    }

    func volume(level: Int) -> String? {
        lock.lock(); defer { lock.unlock() }
        volumeLevel = level
        return nil
    }

    func status() -> ControllerStatus {
        lock.lock(); defer { lock.unlock() }
        return ControllerStatus(
            state: ctlState,
            positionMS: loadedPositionMS,
            durationMS: durationMS,
            title: loadedTitle ?? ""
        )
    }

    // ---- 测试注入 ----
    func failNextLoad(msg: String) {
        lock.lock(); defer { lock.unlock() }
        failLoadArmed = true
        failLoadMsg = msg
    }

    func setPauseError(_ msg: String?) {
        lock.lock(); defer { lock.unlock() }
        pauseErr = msg
    }

    func setDuration(ms: Int64) {
        lock.lock(); defer { lock.unlock() }
        durationMS = ms
    }

    func setPosition(ms: Int64) {
        lock.lock(); defer { lock.unlock() }
        loadedPositionMS = ms
    }

    func setControllerState(_ state: String?) {
        lock.lock(); defer { lock.unlock() }
        ctlState = state
    }
}

// 契约测试驱动工具：经真实 HTTP（URLSession）驱动内嵌 server，读契约夹具。
enum TestHTTP {
    static let token = "s3cret"

    /// 构造被测 server（临时端口）+ 同一 controller，返回端口供 URLSession 驱动。
    static func makeServer(token: String = TestHTTP.token,
                           controller: PlaybackControlling) throws -> (server: RendererServer, port: Int) {
        let server = RendererServer(token: token, room: "卧室", name: "test-renderer", controller: controller)
        try server.start(port: 0)
        return (server, server.boundPort)
    }

    static func request(_ port: Int, method: String, path: String, token: String?, body: Data?) throws -> (Int, Data) {
        // 模拟器回环偶发调度抖动：单请求 5s 超时 + 最多 3 次尝试。
        // 持续超时（真实回归）仍以失败收场。
        var last = (0, Data())
        for attempt in 1...3 {
            let result = try singleRequest(port, method: method, path: path, token: token, body: body, timeout: 5)
            if result.0 != 0 { return result }
            last = result
            if attempt < 3 { Thread.sleep(forTimeInterval: 0.2 * Double(attempt)) }
        }
        XCTFail("request \(method) \(path) timed out after retries")
        return last
    }

    private static func singleRequest(_ port: Int, method: String, path: String, token: String?, body: Data?, timeout: TimeInterval) throws -> (Int, Data) {
        let semaphore = DispatchSemaphore(value: 0)
        var outCode = 0
        var outBody = Data()
        var outError: Error?
        var req = URLRequest(url: URL(string: "http://127.0.0.1:\(port)\(path)")!)
        req.httpMethod = method
        req.timeoutInterval = timeout
        if let token = token {
            req.setValue("Bearer " + token, forHTTPHeaderField: "Authorization")
        }
        if let body = body {
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            req.httpBody = body
        }
        let task = URLSession.shared.dataTask(with: req) { data, resp, error in
            outError = error
            if let http = resp as? HTTPURLResponse {
                outCode = http.statusCode
            }
            outBody = data ?? Data()
            semaphore.signal()
        }
        task.resume()
        if semaphore.wait(timeout: .now() + timeout + 1) == .timedOut {
            return (0, Data())
        }
        if let error = outError {
            if (outCode == 0) { return (0, Data()) } // 连接级错误按超时处理，走重试
            XCTFail("request \(method) \(path) failed: \(error)")
        }
        return (outCode, outBody)
    }

    static func requestJSON(_ port: Int, method: String, path: String, token: String?, json: String) throws -> (Int, Data) {
        return try request(port, method: method, path: path, token: token, body: Data(json.utf8))
    }

    /// 读取打进测试 bundle 的契约夹具（testdata/contract/*.json 的拷贝）。
    static func fixture(_ name: String) throws -> String {
        let bundle = Bundle(for: FakeController.self)
        guard let url = bundle.url(forResource: name, withExtension: nil) else {
            XCTFail("契约夹具 \(name) 应存在于测试 bundle")
            throw NSError(domain: "fixture", code: 1)
        }
        return try String(contentsOf: url, encoding: .utf8)
    }

    /// 线上形态断言：夹具即单行紧凑 JSON + 结尾换行（与 Go json.Encoder
    /// 线上输出字节级一致），tvOS 响应体应与其完全相同。
    static func assertWireBody(_ actual: Data, equalsFixture name: String, file: StaticString = #filePath, line: UInt = #line) throws {
        let expected = try fixture(name) // 已含结尾换行
        let actualStr = String(data: actual, encoding: .utf8)
        XCTAssertEqual(actualStr, expected, "响应体应与契约夹具字节级一致（单行紧凑 JSON + 换行）", file: file, line: line)
        // 同时做 JSON 层等价断言（防字节比较被编码细节掩盖漂移）
        let actualObj = try JSONSerialization.jsonObject(with: actual)
        let expectedObj = try JSONSerialization.jsonObject(with: Data(expected.utf8))
        let a = try JSONSerialization.data(withJSONObject: actualObj, options: [.sortedKeys])
        let b = try JSONSerialization.data(withJSONObject: expectedObj, options: [.sortedKeys])
        XCTAssertEqual(a, b, file: file, line: line)
    }

    static func assertWireBody(_ actual: Data, equals expected: String, file: StaticString = #filePath, line: UInt = #line) {
        XCTAssertEqual(String(data: actual, encoding: .utf8), expected + "\n", file: file, line: line)
    }
}
