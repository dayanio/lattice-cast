import Foundation
@testable import LatticeCastKit

/// 测试用播放后端（镜像 Go 侧 internal/cast/renderer/server_test.go 的
/// fakeController 与 tvOS T18 的 FakeController）：内存态记录调用，可脚本化
/// 注入 Load 失败（模拟源不可达 → error 态）、Pause 失败、控制器自报 idle
/// （模拟播完 EOF）。协议状态机归 Server 所有——Status.state 语义：
/// ""（空串）表示"状态机全归 Server"，"idle" 表示后端自报空闲（EOF）。
final class MockPlaybackController: PlaybackController {

    struct LoadCall: Equatable {
        var url: URL
        var title: String?
        var positionMS: Int64
    }

    private let lock = NSLock()

    // ---- 调用记录 ----
    private var loadCallsArr: [LoadCall] = []
    private(set) var pauseCalls = 0
    private(set) var stopCalls = 0
    private var seekCallsN = 0
    private var lastSeekMSN: Int64 = 0
    private var volumeLevelN = -1

    // ---- 可脚本化状态 ----
    private var failLoadArmed = false
    private var failLoadMsg = ""
    private var pauseErr: String?
    private var ctlState: String?   // nil → Status.state ""
    private var durationMSN: Int64 = 0
    private var positionMSN: Int64 = 0

    // ---- PlaybackController ----

    func load(url: URL, title: String?, positionMS: Int64) throws {
        lock.lock(); defer { lock.unlock() }
        if failLoadArmed {
            failLoadArmed = false
            throw MockError(failLoadMsg)
        }
        loadCallsArr.append(LoadCall(url: url, title: title, positionMS: positionMS))
        positionMSN = positionMS // 位置随 load 折叠下发（对齐 Go：不单独 seek）
    }

    func pause() throws {
        lock.lock(); defer { lock.unlock() }
        pauseCalls += 1
        if let pauseErr { throw MockError(pauseErr) }
    }

    func stop() throws {
        lock.lock(); defer { lock.unlock() }
        stopCalls += 1
    }

    func seek(positionMS: Int64) throws {
        lock.lock(); defer { lock.unlock() }
        seekCallsN += 1
        lastSeekMSN = positionMS
        positionMSN = positionMS // /status 进度来自后端（对齐 Go fake）
    }

    func volume(level: Int) throws {
        lock.lock(); defer { lock.unlock() }
        volumeLevelN = level
    }

    func status() -> Status {
        lock.lock(); defer { lock.unlock() }
        return Status(
            state: ctlState ?? "",
            positionMS: positionMSN,
            durationMS: durationMSN,
            title: loadCallsArr.last?.title ?? "",
            error: ""
        )
    }

    // ---- 断言辅助 ----

    var loadCalls: [LoadCall] {
        lock.lock(); defer { lock.unlock() }
        return loadCallsArr
    }

    var lastLoad: LoadCall? {
        lock.lock(); defer { lock.unlock() }
        return loadCallsArr.last
    }

    var seekCalls: Int {
        lock.lock(); defer { lock.unlock() }
        return seekCallsN
    }

    var lastSeekMS: Int64 {
        lock.lock(); defer { lock.unlock() }
        return lastSeekMSN
    }

    var volumeLevel: Int {
        lock.lock(); defer { lock.unlock() }
        return volumeLevelN
    }

    // ---- 测试注入 ----

    /// 武装下一次 /play 失败（渲染端进 error 态，HTTP 仍 200）。
    func failNextLoad(msg: String) {
        lock.lock(); defer { lock.unlock() }
        failLoadArmed = true
        failLoadMsg = msg
    }

    /// 注入 pause 命令失败（命令失败：不迁移，ok=false）。
    func setPauseError(_ msg: String?) {
        lock.lock(); defer { lock.unlock() }
        pauseErr = msg
    }

    func setDuration(ms: Int64) {
        lock.lock(); defer { lock.unlock() }
        durationMSN = ms
    }

    func setPosition(ms: Int64) {
        lock.lock(); defer { lock.unlock() }
        positionMSN = ms
    }

    /// 控制器自报状态：nil = 状态机全归 Server；"idle" = 播完 EOF。
    func setControllerState(_ state: String?) {
        lock.lock(); defer { lock.unlock() }
        ctlState = state
    }

    struct MockError: LocalizedError {
        let message: String
        var errorDescription: String? { message }
        init(_ message: String) { self.message = message }
    }
}
