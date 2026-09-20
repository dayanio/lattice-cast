import Foundation

/// 首启录入的持久化配置（name/room/token，RendererApp 首启 Provisioning UI 写入）。
///
/// 关键约定：**name 是服务实例名 = cast-agent 侧配置的渲染端 key**，两侧字节级
/// 一致（存储原样回读，不做 trim / 编码变换；录入 UI 已用 whitespace 校验挡住
/// 前后空格）。v1 存 UserDefaults（单设备单渲染端，无多账户隔离需求）。
struct Prefs {
    static let shared = Prefs()

    private let defaults: UserDefaults

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
    }

    private static let nameKey = "latticecast.renderer.name"
    private static let roomKey = "latticecast.renderer.room"
    private static let tokenKey = "latticecast.renderer.token"

    /// 设备显示名（mDNS 实例名 / cast-agent 配置 key）。
    var name: String {
        get { defaults.string(forKey: Self.nameKey) ?? "" }
        nonmutating set { defaults.set(newValue, forKey: Self.nameKey) }
    }

    /// 房间名（mDNS TXT room=）。
    var room: String {
        get { defaults.string(forKey: Self.roomKey) ?? "" }
        nonmutating set { defaults.set(newValue, forKey: Self.roomKey) }
    }

    /// Bearer Token（与 cast-agent 配置一致）。
    var token: String {
        get { defaults.string(forKey: Self.tokenKey) ?? "" }
        nonmutating set { defaults.set(newValue, forKey: Self.tokenKey) }
    }

    var isProvisioned: Bool {
        !name.isEmpty && !room.isEmpty && !token.isEmpty
    }

    func reset() {
        defaults.removeObject(forKey: Self.nameKey)
        defaults.removeObject(forKey: Self.roomKey)
        defaults.removeObject(forKey: Self.tokenKey)
    }
}
