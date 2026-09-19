import Foundation

/// 首启配对钩子（T21b：reflux 配对 UI 经此读写渲染端身份配置）。
/// cast-agent 侧生成 room/token 后，宿主 UI 录入并 `save`；渲染端启动时
/// `loadConfig` 取回身份。存储为 UserDefaults 里的单个 JSON 键（可注入
/// suite 供测试/ App Group 共享）。
public enum LatticeCastProvisioning {

    /// UserDefaults 存储键。
    public static let storageKey = "latticecast.renderer.config"

    /// 读取已配对的渲染端配置；未配置或数据损坏时返回 nil（不抛错——
    /// 配对 UI 据此进入引导流程）。
    public static func loadConfig(defaults: UserDefaults = .standard) -> LatticeCastConfig? {
        guard let data = defaults.data(forKey: storageKey) else { return nil }
        return try? JSONDecoder().decode(LatticeCastConfig.self, from: data)
    }

    /// 保存配对结果（覆盖写）。
    public static func save(_ config: LatticeCastConfig, defaults: UserDefaults = .standard) {
        if let data = try? JSONEncoder().encode(config) {
            defaults.set(data, forKey: storageKey)
        }
    }

    /// 清除配对（重新配对流程用）。
    public static func clear(defaults: UserDefaults = .standard) {
        defaults.removeObject(forKey: storageKey)
    }
}
