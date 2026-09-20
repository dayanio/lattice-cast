import Foundation
import XCTest

@testable import LatticeCastKit

/// 契约测试驱动工具（移植 tvOS T18 TestSupport）：经真实 HTTP（URLSession →
/// Swifter 内嵌 server）驱动渲染端，读取打进测试 bundle 的契约夹具
/// （testdata/contract/*.json 的拷贝，canonical 源仍在仓根 testdata/）。
enum TestHTTP {
    static let token = "s3cret"

    /// 构造被测内部 server（临时端口）+ 同一 controller，返回端口供 URLSession 驱动。
    static func makeServer(token: String = TestHTTP.token,
                           controller: PlaybackController) throws -> (server: RendererServer, port: Int) {
        let server = RendererServer(config: LatticeCastConfig(
            name: "test-renderer", room: "卧室", token: token, port: 0), controller: controller)
        try server.start()
        return (server, server.boundPort)
    }

    static func request(_ port: Int, method: String, path: String, token: String?, body: Data?) throws -> (Int, Data) {
        // 回环偶发调度抖动：单请求 5s 超时 + 最多 3 次尝试。
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
            if outCode == 0 { return (0, Data()) } // 连接级错误按超时处理，走重试
            XCTFail("request \(method) \(path) failed: \(error)")
        }
        return (outCode, outBody)
    }

    static func requestJSON(_ port: Int, method: String, path: String, token: String?, json: String) throws -> (Int, Data) {
        return try request(port, method: method, path: path, token: token, body: Data(json.utf8))
    }

    /// 读取打进测试 bundle 的契约夹具。
    static func fixture(_ name: String) throws -> String {
        guard let url = Bundle.module.url(forResource: "Fixtures/\(name)", withExtension: nil) else {
            XCTFail("契约夹具 \(name) 应存在于测试 bundle（Fixtures/）")
            throw NSError(domain: "fixture", code: 1)
        }
        return try String(contentsOf: url, encoding: .utf8)
    }

    /// 线上形态断言：夹具即单行紧凑 JSON + 结尾换行（与 Go json.Encoder
    /// 线上输出字节级一致），渲染端响应体应与其完全相同。
    static func assertWireBody(_ actual: Data, equalsFixture name: String, file: StaticString = #filePath, line: UInt = #line) throws {
        let expected = try fixture(name) // 已含结尾换行
        let actualStr = String(data: actual, encoding: .utf8)
        XCTAssertEqual(actualStr, expected, "响应体应与契约夹具字节级一致（单行紧凑 JSON + 换行）", file: file, line: #line)
        // 同时做 JSON 层等价断言（防字节比较被编码细节掩盖漂移）
        let actualObj = try JSONSerialization.jsonObject(with: actual)
        let expectedObj = try JSONSerialization.jsonObject(with: Data(expected.utf8))
        let a = try JSONSerialization.data(withJSONObject: actualObj, options: [.sortedKeys])
        let b = try JSONSerialization.data(withJSONObject: expectedObj, options: [.sortedKeys])
        XCTAssertEqual(a, b, file: file, line: #line)
    }

    static func assertWireBody(_ actual: Data, equals expected: String, file: StaticString = #filePath, line: UInt = #line) {
        XCTAssertEqual(String(data: actual, encoding: .utf8), expected + "\n", file: file, line: line)
    }
}
