import Foundation
import Swifter

/// 渲染端 HTTP 服务（docs/protocol.md 唯一权威定义），与 Go 侧
/// internal/cast/renderer/server.go 行为同构：
///
///   - 五端点：/play /pause /stop /seek /volume（POST）+ /status（GET）；
///   - 鉴权：Bearer Token，缺失或不符 401（鉴权先于方法校验，对齐 Go mux 的
///     auth(postOnly(h)) 包装顺序）；
///   - 通用错误体：401/404/405/400 → {"ok":false,"error":"…"}；
///   - 状态机：idle →(play)→ playing ⇄(pause/play) paused →(stop)→ idle，
///     error 仅由 /play 失败进入、粘滞至下一次 /play，其余命令不改 error 态；
///   - EOF → idle：playing/paused 中控制器自报 idle（播完），Server 状态机
///     一并迁到 idle（进度与标题清零、粘滞）——/status 不谎报 playing；
///   - 报文：单行紧凑 JSON、小写 snake_case、字段序与 Go 结构体一致、
///     尾部换行（Go json.Encoder 行为），字符串转义与 encoding/json 一致
///     （含 <>& 的 \u003c 序列），保证两端线上字节级一致。
final class RendererServer {

    private let token: String
    let room: String
    let name: String

    private let ctl: PlaybackControlling
    private let server = HttpServer()

    /// 状态机串行队列（对齐 Go Server 的 mu sync.Mutex：state/title/errMsg
    /// 由 Server 持有；handleStatus 须在临界区内读 Controller 快照，避免
    /// 旧媒体 EOF 的在途快照与并发 /play 的新会话错配）。
    private let stateQueue = DispatchQueue(label: "io.lattice.renderer.state")
    private var state = "idle"  // idle|playing|paused|error 之一
    private var title = ""      // 最近一次成功 /play 的标题（error/idle 时清空）
    private var errMsg = ""     // error 态的出错原因，其余状态为 ""

    init(token: String, room: String, name: String, controller: PlaybackControlling) {
        self.token = token
        self.room = room
        self.name = name
        self.ctl = controller
        registerRoutes()
    }

    /// 绑定并开始服务；port 0 = 临时端口（契约测试用），默认 7822。
    /// QoS 取 userInitiated（Swifter 默认 background 会被模拟器调度饿死，
    /// 偶发秒级乃至超时不响应）。
    func start(port: UInt16 = 7822) throws {
        try server.start(port, forceIPv4: true, priority: DispatchQoS.QoSClass.userInitiated)
    }

    func stop() {
        server.stop()
    }

    /// 实际绑定端口（start 后有效）。
    var boundPort: Int {
        return (try? server.port()) ?? 0
    }

    /// UI 本地快照（不经 HTTP，渲染端常显状态用）。
    struct Snapshot: Equatable {
        var state: String
        var title: String
        var error: String
        var port: Int
    }

    func snapshot() -> Snapshot {
        return stateQueue.sync {
            Snapshot(state: state, title: title, error: errMsg, port: boundPort)
        }
    }

    /// UI 轮询入口：与 GET /status 走同一状态机评估（含控制器自报 idle 的
    /// EOF→idle 粘滞迁移），保证常显状态与 agent 看到的一致。
    func evaluateStatus() -> Snapshot {
        _ = handleStatus()
        return snapshot()
    }

    // ---- 路由：鉴权 + 方法校验 + 分发（protocol.md 第二、五节）----

    private func registerRoutes() {
        let routes: [String: String] = [
            "/play": "POST", "/pause": "POST", "/stop": "POST",
            "/seek": "POST", "/volume": "POST", "/status": "GET",
        ]
        // Swifter 的 server[path] 注册为 method 无关路由（任意方法命中），
        // 方法不符在处理器内返回 405 —— 等价 Go ServeMux 对已注册路径的
        // postOnly/getOnly 语义（任意方法先进入包装链，鉴权最外层）。
        for (path, method) in routes {
            server[path] = { [weak self] request in
                guard let self = self else {
                    return Self.respond(404, "Not Found", Wire.errorBody("not_found"))
                }
                return self.dispatch(request, allowed: method, path: path)
            }
        }
        // 未知路径（对齐 Go mux 的 "/" 兜底）
        server.notFoundHandler = { _ in
            Self.respond(404, "Not Found", Wire.errorBody("not_found"))
        }
    }

    private func dispatch(_ request: HttpRequest, allowed: String, path: String) -> HttpResponse {
        // 鉴权最外层：401 先于 405（对齐 Go：mux.HandleFunc("/play", s.auth(s.postOnly(s.handlePlay)))）
        guard request.headers["authorization"] == "Bearer " + token else {
            return Self.respond(401, "Unauthorized", Wire.errorBody("unauthorized"))
        }
        guard request.method == allowed else {
            return Self.respond(405, "Method Not Allowed", Wire.errorBody("method_not_allowed"))
        }
        switch path {
        case "/play": return handlePlay(request)
        case "/pause": return handlePause()
        case "/stop": return handleStop()
        case "/seek": return handleSeek(request)
        case "/volume": return handleVolume(request)
        case "/status": return handleStatus()
        default: return Self.respond(404, "Not Found", Wire.errorBody("not_found"))
        }
    }

    // ---- 端点处理 ----

    /// /play：成功进入 playing（从 idle/paused/error 任意状态）；Load 失败进入
    /// error 态（HTTP 仍 200，应用层报错）。position_ms>0 时把续播位置折叠进
    /// Load（对齐 Go：不单独下发 seek）。
    private func handlePlay(_ request: HttpRequest) -> HttpResponse {
        let obj: [String: Any]
        do {
            obj = try Wire.parseObject(request.body)
            let url = try Wire.optString(obj, "url") ?? ""
            guard !url.isEmpty else {
                return Self.respond(400, "Bad Request", Wire.errorBody("bad_request"))
            }
            let reqTitle = try Wire.optString(obj, "title") ?? ""
            let positionMS = try Wire.optInt(obj, "position_ms") ?? 0

            if let err = ctl.load(url: url, title: reqTitle, positionMS: positionMS) {
                return stateQueue.sync {
                    state = "error"; title = ""; errMsg = err
                    return Self.respond(200, "OK", Wire.cmdBody(ok: false, state: "error", error: err))
                }
            }
            return stateQueue.sync {
                state = "playing"; title = reqTitle; errMsg = ""
                return Self.respond(200, "OK", Wire.cmdBody(ok: true, state: "playing"))
            }
        } catch {
            return Self.respond(400, "Bad Request", Wire.errorBody("bad_request"))
        }
    }

    /// /pause：playing → paused；其余状态幂等不迁移（含 error）。
    private func handlePause() -> HttpResponse {
        let resp: (Bool, String, String) = stateQueue.sync { () -> (Bool, String, String) in
            if state == "playing" {
                if let err = ctl.pause() {
                    return (false, state, err) // 命令失败：不迁移（对齐 Go）
                }
                state = "paused"
            }
            return (true, state, "")
        }
        return Self.respond(200, "OK", Wire.cmdBody(ok: resp.0, state: resp.1, error: resp.2))
    }

    /// /stop：playing|paused → idle 并清空媒体；idle 幂等；error 态不迁移。
    private func handleStop() -> HttpResponse {
        let resp: (Bool, String, String) = stateQueue.sync { () -> (Bool, String, String) in
            if state == "playing" || state == "paused" {
                if let err = ctl.stop() {
                    return (false, state, err)
                }
                state = "idle"; title = ""; errMsg = ""
            }
            return (true, state, "")
        }
        return Self.respond(200, "OK", Wire.cmdBody(ok: resp.0, state: resp.1, error: resp.2))
    }

    /// /seek：playing|paused 时生效，不改变状态；其余状态幂等不下发。
    private func handleSeek(_ request: HttpRequest) -> HttpResponse {
        let obj: [String: Any]
        do {
            obj = try Wire.parseObject(request.body)
            let ms = try Wire.optInt(obj, "position_ms") ?? 0
            let resp: (Bool, String, String) = stateQueue.sync { () -> (Bool, String, String) in
                if state == "playing" || state == "paused" {
                    if let err = ctl.seekTo(ms: ms) {
                        return (false, state, err)
                    }
                }
                return (true, state, "")
            }
            return Self.respond(200, "OK", Wire.cmdBody(ok: resp.0, state: resp.1, error: resp.2))
        } catch {
            return Self.respond(400, "Bad Request", Wire.errorBody("bad_request"))
        }
    }

    /// /volume：校验请求体，不改变播放状态与进度（v1 /status 无音量字段）。
    private func handleVolume(_ request: HttpRequest) -> HttpResponse {
        let obj: [String: Any]
        do {
            obj = try Wire.parseObject(request.body)
            let level = try Wire.optInt(obj, "level") ?? 0
            let resp: (Bool, String, String) = stateQueue.sync { () -> (Bool, String, String) in
                if state == "playing" || state == "paused" {
                    if let err = ctl.volume(level: Int(level)) {
                        return (false, state, err)
                    }
                }
                return (true, state, "")
            }
            return Self.respond(200, "OK", Wire.cmdBody(ok: resp.0, state: resp.1, error: resp.2))
        } catch {
            return Self.respond(400, "Bad Request", Wire.errorBody("bad_request"))
        }
    }

    /// /status：状态机取 Server 记录，位置/时长/标题取 Controller 快照；
    /// error/idle 态信息清零，对齐契约夹具的零值形态；200 响应不含 ok 字段，
    /// error 为空串而非省略。播放中若 Controller 自报 idle（播完 EOF），
    /// Server 状态机随之迁到 idle（进度与标题清零），迁移粘滞至下一次 /play。
    /// 与 Go 相同：Controller 快照必须在状态机临界区内读取。
    private func handleStatus() -> HttpResponse {
        return stateQueue.sync {
            let cs = ctl.status()
            switch state {
            case "error":
                return Self.respond(200, "OK", Wire.statusBody(
                    state: "error", positionMS: 0, durationMS: 0, title: "", error: errMsg))
            case "idle":
                return Self.respond(200, "OK", Wire.statusBody(
                    state: "idle", positionMS: 0, durationMS: 0, title: "", error: ""))
            default: // playing|paused
                if cs.state == "idle" {
                    state = "idle"; title = ""; errMsg = ""
                    return Self.respond(200, "OK", Wire.statusBody(
                        state: "idle", positionMS: 0, durationMS: 0, title: "", error: ""))
                }
                return Self.respond(200, "OK", Wire.statusBody(
                    state: state, positionMS: cs.positionMS, durationMS: cs.durationMS,
                    title: title, error: ""))
            }
        }
    }

    // ---- 响应组装（单行紧凑 JSON + 结尾换行，对齐 Go json.Encoder）----

    private static func respond(_ code: Int, _ reason: String, _ body: String) -> HttpResponse {
        return .raw(code, reason, ["Content-Type": "application/json"]) { writer in
            try writer.write(Data((body + "\n").utf8))
        }
    }
}

/// wire 报文序列化：字段序、转义、结尾换行与 Go encoding/json 字节级一致。
enum Wire {

    /// 解析请求体 JSON 对象；非法 JSON 或顶层非对象 → bad_request。
    /// 顶层 null 对齐 Go json.Decode（成功且零值）→ 空对象。
    static func parseObject(_ body: [UInt8]) throws -> [String: Any] {
        guard !body.isEmpty else { throw WireError.badRequest }
        let obj = try JSONSerialization.jsonObject(with: Data(body), options: [])
        if obj is NSNull { return [:] }
        guard let dict = obj as? [String: Any] else { throw WireError.badRequest }
        return dict
    }

    /// 可缺省字符串字段：缺省/NULL → nil；存在但类型不符 → bad_request（对齐 Go 解码错误）。
    static func optString(_ obj: [String: Any], _ key: String) throws -> String? {
        guard let v = obj[key] else { return nil }
        if v is NSNull { return nil }
        guard let s = v as? String else { throw WireError.badRequest }
        return s
    }

    /// 可缺省整数字段：缺省/NULL → nil；存在但类型不符（浮点/布尔/字符串）→ bad_request。
    static func optInt(_ obj: [String: Any], _ key: String) throws -> Int64? {
        guard let v = obj[key] else { return nil }
        if v is NSNull { return nil }
        guard let n = v as? NSNumber else { throw WireError.badRequest }
        if CFGetTypeID(n) == CFBooleanGetTypeID() { throw WireError.badRequest } // 布尔不是整数
        let type = String(cString: n.objCType)
        guard ["c", "i", "l", "q", "C", "I", "L", "Q"].contains(type) else { throw WireError.badRequest }
        return n.int64Value
    }

    /// play/pause/stop/seek/volume 响应体（Go cmdResp：error 仅失败时出现）。
    static func cmdBody(ok: Bool, state: String, error: String = "") -> String {
        var s = "{\"ok\":" + (ok ? "true" : "false") + ",\"state\":" + jsonString(state)
        if !error.isEmpty {
            s += ",\"error\":" + jsonString(error)
        }
        return s + "}"
    }

    /// GET /status 响应体（Go statusResp：无 ok 字段，字段全量出现）。
    static func statusBody(state: String, positionMS: Int64, durationMS: Int64, title: String, error: String) -> String {
        return "{\"state\":" + jsonString(state)
            + ",\"position_ms\":\(positionMS)"
            + ",\"duration_ms\":\(durationMS)"
            + ",\"title\":" + jsonString(title)
            + ",\"error\":" + jsonString(error) + "}"
    }

    /// 401/404/405/400 通用错误体（Go errResp）。
    static func errorBody(_ error: String) -> String {
        return "{\"ok\":false,\"error\":" + jsonString(error) + "}"
    }

    /// Go encoding/json 字符串转义（默认 HTML 转义开启）：\" \\ \n \r \t、
    /// 控制字符 \u00xx、<>& → \u003c/\u003e/\u0026、U+2028/U+2029。
    static func jsonString(_ s: String) -> String {
        var out = "\""
        for scalar in s.unicodeScalars {
            switch scalar {
            case "\"": out += "\\\""
            case "\\": out += "\\\\"
            case "\n": out += "\\n"
            case "\r": out += "\\r"
            case "\t": out += "\\t"
            case "<": out += "\\u003c"
            case ">": out += "\\u003e"
            case "&": out += "\\u0026"
            case "\u{2028}": out += "\\u2028"
            case "\u{2029}": out += "\\u2029"
            default:
                if scalar.value < 0x20 {
                    out += String(format: "\\u%04x", scalar.value)
                } else {
                    out.unicodeScalars.append(scalar)
                }
            }
        }
        return out + "\""
    }
}

enum WireError: Error {
    case badRequest
}
