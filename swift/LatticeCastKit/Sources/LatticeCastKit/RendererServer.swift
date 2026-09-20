import Foundation
import Swifter

/// 渲染端 HTTP 服务（docs/protocol.md 唯一权威定义），与 Go 侧
/// internal/cast/renderer/server.go 行为同构（移植自 tvOS T18 RendererServer，
/// 泛化为宿主注入 PlaybackController 的可复用包实现）：
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

    let token: String
    let room: String
    let name: String

    private let ctl: PlaybackController
    private let server = HttpServer()
    private let listenPort: Int

    /// 状态机串行队列（对齐 Go Server 的 mu sync.Mutex：state/title/errMsg
    /// 由 Server 持有；handleStatus 须在临界区内读 Controller 快照，避免
    /// 旧媒体 EOF 的在途快照与并发 /play 的新会话错配）。
    private let stateQueue = DispatchQueue(label: "io.lattice.renderer.state")
    private var state = "idle"  // idle|playing|paused|error 之一
    private var title = ""      // 最近一次成功 /play 的标题（error/idle 时清空）
    private var errMsg = ""     // error 态的出错原因，其余状态为 ""

    init(config: LatticeCastConfig, controller: PlaybackController) {
        self.token = config.token
        self.room = config.room
        self.name = config.name
        self.listenPort = config.port
        self.ctl = controller
        registerRoutes()
    }

    /// 绑定并开始服务；config.port 0 = 临时端口（契约测试用），默认 7822。
    /// QoS 取 userInitiated（Swifter 默认 background 会被调度饿死，
    /// 偶发秒级乃至超时不响应）。
    func start() throws {
        guard listenPort >= 0, listenPort <= Int(UInt16.max) else {
            throw RendererError.invalidPort
        }
        try server.start(UInt16(listenPort), forceIPv4: true,
                         priority: DispatchQoS.QoSClass.userInitiated)
    }

    func stop() {
        server.stop()
    }

    /// 实际绑定端口（start 后有效）。
    var boundPort: Int {
        return (try? server.port()) ?? 0
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

    /// /play：成功进入 playing（从 idle/paused/error 任意状态）；后端加载失败
    /// 进入 error 态（HTTP 仍 200，应用层报错）。position_ms>0 时把续播位置
    /// 折叠进 load（对齐 Go：不单独下发 seek）。
    private func handlePlay(_ request: HttpRequest) -> HttpResponse {
        let obj: [String: Any]
        do {
            obj = try Wire.parseObject(Data(request.body))
            let url = try Wire.optString(obj, "url") ?? ""
            guard !url.isEmpty else {
                return Self.respond(400, "Bad Request", Wire.errorBody("bad_request"))
            }
            let reqTitle = try Wire.optString(obj, "title") ?? ""
            let positionMS = try Wire.optInt(obj, "position_ms") ?? 0

            let loadErr: String? = stateQueue.sync {
                // URL 构造/校验对齐 tvOS PlaybackController：须为绝对 URL
                //（有 scheme，且有 host 或 file URL）；不可达/解码失败由后端裁决。
                guard let u = URL(string: url),
                      u.scheme != nil, !u.scheme!.isEmpty,
                      u.host != nil || u.isFileURL else {
                    // 宿主协议要求 URL 类型；构造失败与后端加载失败同构（进 error 态）
                    state = "error"; title = ""; errMsg = "invalid_url"
                    return "invalid_url"
                }
                do {
                    try ctl.load(url: u, title: reqTitle.isEmpty ? nil : reqTitle, positionMS: positionMS)
                } catch {
                    state = "error"; title = ""; errMsg = Self.message(of: error)
                    return errMsg
                }
                state = "playing"; title = reqTitle; errMsg = ""
                return nil
            }
            if let err = loadErr {
                return Self.respond(200, "OK", Wire.cmdBody(ok: false, state: "error", error: err))
            }
            return Self.respond(200, "OK", Wire.cmdBody(ok: true, state: "playing"))
        } catch {
            return Self.respond(400, "Bad Request", Wire.errorBody("bad_request"))
        }
    }

    /// /pause：playing → paused；其余状态幂等不迁移（含 error）。
    private func handlePause() -> HttpResponse {
        let resp: (Bool, String, String) = stateQueue.sync { () -> (Bool, String, String) in
            if state == "playing" {
                do { try ctl.pause() } catch {
                    return (false, state, Self.message(of: error)) // 命令失败：不迁移（对齐 Go）
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
                do { try ctl.stop() } catch {
                    return (false, state, Self.message(of: error))
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
            obj = try Wire.parseObject(Data(request.body))
            let ms = try Wire.optInt(obj, "position_ms") ?? 0
            let resp: (Bool, String, String) = stateQueue.sync { () -> (Bool, String, String) in
                if state == "playing" || state == "paused" {
                    do { try ctl.seek(positionMS: ms) } catch {
                        return (false, state, Self.message(of: error))
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
            obj = try Wire.parseObject(Data(request.body))
            let level = try Wire.optInt(obj, "level") ?? 0
            let resp: (Bool, String, String) = stateQueue.sync { () -> (Bool, String, String) in
                if state == "playing" || state == "paused" {
                    do { try ctl.volume(level: Int(level)) } catch {
                        return (false, state, Self.message(of: error))
                    }
                }
                return (true, state, "")
            }
            return Self.respond(200, "OK", Wire.cmdBody(ok: resp.0, state: resp.1, error: resp.2))
        } catch {
            return Self.respond(400, "Bad Request", Wire.errorBody("bad_request"))
        }
    }

    /// /status：状态机取 Server 记录，位置/时长取 Controller 快照；
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

    /// 控制器错误 → 线上 error 字段内容：LocalizedError.errorDescription 优先
    /// （宿主桥接给出稳定错误码，如 source_unreachable），否则类型描述。
    fileprivate static func message(of error: Error) -> String {
        if let localized = error as? LocalizedError, let desc = localized.errorDescription, !desc.isEmpty {
            return desc
        }
        return String(describing: error)
    }

    // ---- 响应组装（单行紧凑 JSON + 结尾换行，对齐 Go json.Encoder）----

    fileprivate static func respond(_ code: Int, _ reason: String, _ body: String) -> HttpResponse {
        return .raw(code, reason, ["Content-Type": "application/json"]) { writer in
            try writer.write(Data((body + "\n").utf8))
        }
    }
}

/// wire 报文序列化：字段序、转义、结尾换行与 Go encoding/json 字节级一致。
enum Wire {

    /// 解析请求体 JSON 对象；非法 JSON 或顶层非对象 → bad_request。
    /// 顶层 null 对齐 Go json.Decode（成功且零值）→ 空对象（fragmentsAllowed
    /// 让 JSONSerialization 接受标量/ null 片段，非对象片段再行拒绝）。
    static func parseObject(_ body: Data) throws -> [String: Any] {
        guard !body.isEmpty else { throw WireError.badRequest }
        let obj = try JSONSerialization.jsonObject(with: body, options: [.fragmentsAllowed])
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
    /// 控制字符 \u00xx（小写十六进制）、<>& → \u003c/\u003e/\u0026、U+2028/U+2029。
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

/// 包内部错误（端口非法等）。
enum RendererError: Error {
    case invalidPort
}
