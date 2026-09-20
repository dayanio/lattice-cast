import Foundation

/// LatticeCast 渲染端身份配置（protocol.md 第一节 + 第三节 mDNS 元数据）。
/// `port` 缺省 7822；Codable 以便首启配对 UI 持久化（见 LatticeCastProvisioning）。
public struct LatticeCastConfig: Codable, Sendable, Equatable {
    /// 设备显示名（= mDNS 服务实例名，须与 cast-agent 配置的渲染端 key 字节级一致）。
    public var name: String
    /// 房间名（mDNS TXT `room=<room>`，cast-agent 据此归位）。
    public var room: String
    /// Bearer Token（cast-agent 配置持有，首启配对录入）。
    public var token: String
    /// HTTP 监听端口（0 = 临时端口，测试用）。
    public var port: Int

    public init(name: String, room: String, token: String, port: Int = 7822) {
        self.name = name
        self.room = room
        self.token = token
        self.port = port
    }
}

/// 播放后端快照（对齐 Go renderer.Controller.Status() 供给的 adapter.Status）：
/// `state` 语义——""（空串）表示"状态机全归渲染端 Server"；"idle" 表示后端
/// 自报空闲（播完 EOF / 无媒体），Server 的 /status 据此粘滞迁移到 idle。
/// playing/paused 等其他取值不参与协议状态机判定（协议态归 Server 所有），
/// 仅作宿主本地信息。`title`/`error` 为宿主本地信息（协议 /status 的标题取
/// 最近一次成功 /play 的值、error 取 sticky 错误，与 Go server.go 一致）。
public struct Status: Sendable, Equatable {
    public var state: String
    public var positionMS: Int64
    public var durationMS: Int64
    public var title: String
    public var error: String

    public init(state: String, positionMS: Int64 = 0, durationMS: Int64 = 0, title: String = "", error: String = "") {
        self.state = state
        self.positionMS = positionMS
        self.durationMS = durationMS
        self.title = title
        self.error = error
    }
}

/// 宿主播放后端抽象（reflux 的 PlayerKit / AVPlayer 播放器实现本协议注入），
/// 与 Go 侧 renderer.Controller 同构：命令失败抛错（错误消息即线上
/// `error` 字段内容，如 `source_unreachable`）；位置/时长由 `status()` 供给。
/// 命令语义（对齐 Go MpvController）：
///   - `load`：拉流由渲染端自己发起（直接拉 `url`），`positionMS>0` 为续播
///     位置、须随加载一起下发（不得 load 后单独 seek）；
///   - `pause`/`stop` 幂等；`seek` 绝对位置、不改变播放状态；
///   - `volume`：协议 level 0-100；
///   - `status()`：后端播完 EOF / 无媒体时返回 `state == "idle"`（进度清零）。
public protocol PlaybackController {
    func load(url: URL, title: String?, positionMS: Int64) throws
    func pause() throws
    func stop() throws
    func seek(positionMS: Int64) throws
    func volume(level: Int) throws
    func status() -> Status
}

/// LatticeCast 渲染端公共门面：start() 起 HTTP 服务（protocol.md 五端点）并经
/// NSNetService 发布 _latticecast._tcp（TXT room=<room>, v=1）供 cast-agent
/// 发现；stop() 一并撤销。宿主 App 只与本类和 PlaybackController 打交道。
public final class LatticeCastRenderer {

    private let server: RendererServer
    private let announcer = Announcer()

    public init(config: LatticeCastConfig, controller: PlaybackController) {
        self.server = RendererServer(config: config, controller: controller)
    }

    /// 绑定并开始服务 + mDNS 自报。config.port == 0 时绑临时端口（测试用）。
    public func start() throws {
        try server.start()
        announcer.start(name: server.name, room: server.room, port: server.boundPort)
    }

    /// 停止服务并撤销 mDNS 发布（幂等）。
    public func stop() {
        announcer.stop()
        server.stop()
    }

    /// 实际绑定端口（start 后有效；未 start 为 0）。internal 供契约测试读取。
    var boundPort: Int { server.boundPort }
}
