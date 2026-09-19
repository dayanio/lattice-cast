import AVFoundation
import SwiftUI

/// LatticeCast Apple TV 渲染端（tvOS + AVPlayer）。
///
/// 首启经 Provisioning UI 录入 name/room/token（persist 于 Prefs；name 即
/// cast-agent 配置的渲染端 key，字节级一致），随后：
///   - RendererServer 监听 7822（docs/protocol.md 五端点）；
///   - Announcer 以 NSNetService 发布 _latticecast._tcp（TXT room=/v=1）；
///   - 常显状态（房间/协议态/正在播/地址），播放时全屏渲染视频画面。
@main
struct RendererApp: App {
    @StateObject private var model = RendererModel()

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environmentObject(model)
        }
    }
}

// MARK: - 模型

final class RendererModel: ObservableObject {

    @Published var provisioned: Bool
    @Published var snap = RendererServer.Snapshot(state: "idle", title: "", error: "", port: 0)
    @Published var address = ""   // IP:port（常显状态用）
    @Published var lastError = ""

    let prefs = Prefs.shared
    private let controller = PlaybackController()
    private var server: RendererServer?
    private var announcer: Announcer?
    private var poll: Timer?

    init() {
        // 开发/冒烟便利：`simctl launch <dev> <bundle> -latticecastSeed <name> <room> <token>`
        // 直接写入录入配置（等价首启 UI 录入；正式入口仍是 ProvisioningView）。
        let args = ProcessInfo.processInfo.arguments
        if let i = args.firstIndex(of: "-latticecastSeed"), args.count >= i + 4 {
            prefs.name = args[i + 1]
            prefs.room = args[i + 2]
            prefs.token = args[i + 3]
        }
        provisioned = prefs.isProvisioned
    }

    /// 播放器供视频画面层读取（AVPlayerLayer 渲染）。
    var player: AVPlayer { controller.player }

    /// 录入并启动（重复调用先撤销旧服务/发布，幂等）。
    func start(name: String, room: String, token: String) {
        shutdown()
        // name 是实例名/cast-agent 配置 key：字节级一致，仅挡住首尾空白录入
        let trimmedName = name.trimmingCharacters(in: .whitespacesAndNewlines)
        let trimmedRoom = room.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmedName.isEmpty, !trimmedRoom.isEmpty, !token.isEmpty else {
            lastError = "名称、房间、令牌均不能为空"
            return
        }
        prefs.name = trimmedName
        prefs.room = trimmedRoom
        prefs.token = token

        let server = RendererServer(token: token, room: trimmedRoom, name: trimmedName, controller: controller)
        do {
            try server.start(port: 7822) // 协议默认端口（与 cast-agent 渲染端一致）
        } catch {
            // 端口被占（如同机残留进程）：回退系统分配的临时端口。渲染端可发现性
            // 与可达性由 mDNS（room=/v=1）承载，通告端口为实际绑定端口，不影响协议。
            do {
                try server.start(port: 0)
                lastError = "7822 被占用，已改用临时端口 \(server.boundPort)"
            } catch {
                lastError = "端口监听失败：\(error.localizedDescription)"
                return
            }
        }
        self.server = server

        let announcer = Announcer()
        announcer.start(name: trimmedName, room: trimmedRoom, port: server.boundPort)
        self.announcer = announcer

        address = "\(Self.localIPv4()):\(server.boundPort)"
        provisioned = true
        lastError = announcer.lastError

        poll = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] _ in
            self?.refresh()
        }
        refresh()
    }

    /// 已录入配置时自动恢复服务（应用重启后无需重新录入即可被发现/控制）。
    func startIfNeeded() {
        guard server == nil, prefs.isProvisioned else { return }
        start(name: prefs.name, room: prefs.room, token: prefs.token)
    }

    /// 停止服务并清除配置（回到首启录入页）。
    func shutdown() {
        poll?.invalidate()
        poll = nil
        announcer?.stop()
        announcer = nil
        server?.stop()
        server = nil
        _ = controller.stop()
        prefs.reset()
        address = ""
        snap = RendererServer.Snapshot(state: "idle", title: "", error: "", port: 0)
        provisioned = false
    }

    /// UI 常显状态刷新：与 GET /status 同一状态机评估（EOF→idle 迁移同步可见）。
    func refresh() {
        guard let server = server else { return }
        snap = server.evaluateStatus()
    }

    /// 本机局域网 IPv4（常显地址用；取 en0 优先，无则返回 127.0.0.1）。
    static func localIPv4() -> String {
        var address = "127.0.0.1"
        var ifaddr: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&ifaddr) == 0, let first = ifaddr else { return address }
        defer { freeifaddrs(ifaddr) }

        var en0: String?
        var any: String?
        var ptr: UnsafeMutablePointer<ifaddrs>? = first
        while let current = ptr {
            defer { ptr = current.pointee.ifa_next }
            guard let sa = current.pointee.ifa_addr, sa.pointee.sa_family == UInt8(AF_INET),
                  (current.pointee.ifa_flags & UInt32(IFF_LOOPBACK)) == 0 else { continue }
            var host = [CChar](repeating: 0, count: Int(NI_MAXHOST))
            let result = getnameinfo(sa, socklen_t(sa.pointee.sa_len), &host, socklen_t(host.count),
                                     nil, 0, NI_NUMERICHOST)
            guard result == 0 else { continue }
            let ip = String(cString: host)
            if current.pointee.ifa_name.map({ String(cString: $0) }) == "en0" {
                en0 = ip
            }
            if any == nil { any = ip }
        }
        if let ip = en0 ?? any { address = ip }
        return address
    }
}

// MARK: - 视图

struct ContentView: View {
    @EnvironmentObject var model: RendererModel

    var body: some View {
        Group {
            if model.provisioned {
                StatusView()
            } else {
                ProvisioningView()
            }
        }
        .onAppear { model.startIfNeeded() }
    }
}

/// 首启配置 UI：name/room/token 录入（token 与 cast-agent 配置一致）。
struct ProvisioningView: View {
    @EnvironmentObject var model: RendererModel
    @State private var name = ""
    @State private var room = ""
    @State private var token = ""

    var body: some View {
        VStack(spacing: 28) {
            Text("LatticeCast 渲染端").font(.title2).bold()
            Text("录入后本机将以该名称在局域网发布 _latticecast._tcp 服务").font(.caption).foregroundStyle(.secondary)

            Form {
                TextField("设备名称（实例名）", text: $name)
                TextField("房间（room）", text: $room)
                SecureField("令牌（Bearer Token）", text: $token)
                Section {
                    Button("启动服务") {
                        model.start(name: name, room: room, token: token)
                    }
                    .disabled(name.isEmpty || room.isEmpty || token.isEmpty)
                }
            }
            .frame(maxWidth: 900)

            if !model.lastError.isEmpty {
                Text(model.lastError).foregroundStyle(.red).font(.caption)
            }
        }
        .padding(60)
        .onAppear {
            name = model.prefs.name
            room = model.prefs.room
        }
    }
}

/// 常显状态页：视频画面（播放中全屏铺底）+ 状态栏（房间/状态/正在播/地址）。
struct StatusView: View {
    @EnvironmentObject var model: RendererModel

    private var isPlaying: Bool {
        model.snap.state == "playing" || model.snap.state == "paused"
    }

    var body: some View {
        ZStack {
            if isPlaying {
                PlayerView(player: model.player)
                    .ignoresSafeArea()
            } else {
                Color.black.ignoresSafeArea()
            }

            VStack {
                Spacer()
                HStack(spacing: 24) {
                    Text("\(model.prefs.name) · \(model.prefs.room)")
                    Text(stateLabel(model.snap.state))
                        .foregroundStyle(stateColor(model.snap.state))
                    if !model.snap.title.isEmpty {
                        Text("正在播：\(model.snap.title)").lineLimit(1)
                    }
                    Text(model.address).foregroundStyle(.secondary)
                }
                .font(.caption)
                .padding(.horizontal, 24)
                .padding(.vertical, 12)
                .background(.thinMaterial, in: RoundedRectangle(cornerRadius: 12))
                .padding(.bottom, 32)
            }
        }
        .contextMenu {
            Button("重新配置（停止服务并清除录入）", action: model.shutdown)
        }
    }

    private func stateLabel(_ state: String) -> String {
        switch state {
        case "playing": return "▶ 播放中"
        case "paused": return "⏸ 已暂停"
        case "error": return "⚠ 错误：\(model.snap.error)"
        default: return "待机"
        }
    }

    private func stateColor(_ state: String) -> Color {
        switch state {
        case "playing": return .green
        case "paused": return .yellow
        case "error": return .red
        default: return .secondary
        }
    }
}

/// AVPlayerLayer 桥接（tvOS）：播放中全屏渲染视频画面。
struct PlayerView: UIViewRepresentable {
    let player: AVPlayer

    final class PlayerUIView: UIView {
        override static var layerClass: AnyClass { AVPlayerLayer.self }
        var playerLayer: AVPlayerLayer { layer as! AVPlayerLayer }
    }

    func makeUIView(context: Context) -> PlayerUIView {
        let view = PlayerUIView()
        view.playerLayer.videoGravity = .resizeAspect
        view.playerLayer.player = player
        return view
    }

    func updateUIView(_ uiView: PlayerUIView, context: Context) {
        if uiView.playerLayer.player !== player {
            uiView.playerLayer.player = player
        }
    }
}
