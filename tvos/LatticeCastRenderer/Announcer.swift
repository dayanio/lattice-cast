import Foundation

/// mDNS 发布（protocol.md 第三节）：服务类型 **_latticecast._tcp**，服务实例名 =
/// 设备显示名（即 cast-agent 配置的渲染端 key，字节级一致），TXT 记录携带
/// room=<room> 与协议版本 v=1。
///
/// 与 Go 侧 internal/cast/renderer/announce.go（zeroconf.Register）线上通告等价；
/// Apple TV 上按任务约定用 NSNetService（Bonjour）发布，随应用生命周期
/// start/stop（退后台不撤销——渲染端需要常驻可发现）。
final class Announcer: NSObject, NetServiceDelegate {

    private var service: NetService?
    private(set) var lastError = ""

    /// 发布服务；重复调用会先撤销旧发布（幂等）。
    func start(name: String, room: String, port: Int) {
        stop()
        let svc = NetService(domain: "local.", type: "_latticecast._tcp.", name: name, port: Int32(port))
        let txt = ["room": Data(room.utf8), "v": Data("1".utf8)]
        svc.setTXTRecord(NetService.data(fromTXTRecord: txt))
        svc.delegate = self
        svc.publish()
        service = svc
    }

    /// 撤销发布（幂等，可安全多次调用）。
    func stop() {
        service?.stop()
        service?.delegate = nil
        service = nil
    }

    func netService(_ sender: NetService, didNotPublish errorDict: [String: NSNumber]) {
        lastError = "mDNS 发布失败: \(errorDict)"
        NSLog("LatticeCast Announcer: %@", lastError)
    }
}
