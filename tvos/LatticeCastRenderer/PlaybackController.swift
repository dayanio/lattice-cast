import AVFoundation
import Foundation

/// 播放后端快照（对齐 Go renderer.Controller.Status() 供给的 adapter.Status）：
/// state 为 nil 表示"状态机全归 Server"（Go fake 的 "" 语义）；"idle" 表示
/// 播放后端自报空闲（AVPlayer 播完 EOF / 无媒体），Server /status 据此迁移。
struct ControllerStatus: Equatable {
    var state: String?
    var positionMS: Int64 = 0
    var durationMS: Int64 = 0
    var title: String = ""
}

/// 播放后端抽象（生产实现 PlaybackController，测试用 FakeController），
/// 与 Go 侧 renderer.Controller 同构：状态机归 RendererServer 所有，
/// 这里只供给位置/时长/标题；命令执行失败以错误串回传（协议侧 ok=false）。
protocol PlaybackControlling: AnyObject {
    func load(url: String, title: String, positionMS: Int64) -> String?
    func pause() -> String?
    func stop() -> String?
    func seekTo(ms: Int64) -> String?
    func volume(level: Int) -> String?
    func status() -> ControllerStatus
}

/// AVPlayer 播放后端（对齐 Go renderer.MpvController 的 mpv 语义）：
///
///   - Load：replaceCurrentItem + 续播位置随 load 折叠下发（positionMS>0 时
///     先 seek 再 play，等价 mpv loadfile 的 start=+<sec>——AVPlayer 同样
///     不接受"加载中"的独立 seek，位置必须随加载一起给）；拉流由渲染端
///     自己发起（AVPlayer 直接拉 url，与 ExoPlayer/mpv 同构）。
///   - Status 映射（mpv.go）：无媒体 / item 加载失败 / 播完 EOF → idle
///     （全零，等价 mpv idle-active / eof-reached）；timeControlStatus ==
///     .paused → paused；其余（playing / waiting 缓冲）→ playing
///     （等价 mpv pause=no）；currentTime().seconds/duration 四舍五入为毫秒。
final class PlaybackController: PlaybackControlling {

    /// 供测试断言音量换算（@testable 访问）。
    let player = AVPlayer()

    private let lock = NSLock()
    private var generation = 0
    private var eofReached = false
    private var itemFailed = false
    private var durationMS: Int64 = 0
    private var mediaTitle = ""
    private var endObserver: NSObjectProtocol?
    private var statusObservation: NSKeyValueObservation?
    private var currentItemRef: AVPlayerItem?

    /// 当前会话代数（测试注入旧回调用，@testable 读取）。
    var currentGeneration: Int {
        lock.lock(); defer { lock.unlock() }
        return generation
    }

    func load(url: String, title: String, positionMS: Int64) -> String? {
        guard let u = URL(string: url), u.scheme != nil, u.host != nil || u.isFileURL else {
            return "invalid_url"
        }

        lock.lock()
        generation += 1
        eofReached = false
        itemFailed = false
        durationMS = 0
        mediaTitle = title
        lock.unlock()
        detachObservers() // 旧 item 的 block 通知 token/KVO 必须显式摘除，不能只覆盖引用

        let asset = AVURLAsset(url: u)
        let item = AVPlayerItem(asset: asset)

        lock.lock()
        currentItemRef = item
        let gen = generation
        lock.unlock()

        // 播完 EOF：置位后 status() 自报 idle（等价 mpv eof-reached → idle）。
        // generation 注册时快照 + item 身份双校验：旧 item 的迟到回调一律忽略。
        let observer = NotificationCenter.default.addObserver(
            forName: NSNotification.Name.AVPlayerItemDidPlayToEndTime,
            object: item, queue: nil
        ) { [weak self] _ in
            self?.handleItemEnded(item, generation: gen)
        }
        // item 加载失败（源不可达/解码失败）：清空媒体 → status() 自报 idle，
        // 等价 mpv loadfile 失败后回到 idle（Server 经 /status 如实迁移）。
        // 仅当"失败的 item 仍是当前 item"才清空——旧 item 的迟到失败不得误清新会话。
        let statusObs = item.observe(\.status, options: [.new]) { [weak self] item, _ in
            self?.handleItemStatus(item, generation: gen)
        }

        lock.lock()
        endObserver = observer
        statusObservation = statusObs
        lock.unlock()

        // 时长异步加载（现代 AVFoundation 无同步阻塞取法）：加载成功前
        // /status 的 duration_ms 如实为 0（协议允许"未知为 0"）。
        // title 仅作 OSD/审计展示，协议 /status 的标题由 Server 记录。
        Task { [weak self] in
            if let duration = try? await asset.load(.duration).seconds, duration.isFinite {
                let ms = Self.secToMS(duration)
                self?.storeDuration(ms, generation: gen)
            }
        }

        player.replaceCurrentItem(with: item)
        if positionMS > 0 {
            // 续播位置折叠进 load：seek 先于 play 下发，AVPlayer 会在 item
            // 就绪后从该位置起播（等价 loadfile start=+<sec>）。
            player.seek(to: CMTime(value: CMTimeValue(positionMS), timescale: 1000))
        }
        player.play()
        return nil
    }

    /// 播完通知入口（generation 为注册时快照；internal 供测试直注旧回调）：
    /// 仅当回调属于当前会话（generation 未过期且 item 仍是 current）才置 EOF。
    func handleItemEnded(_ item: AVPlayerItem, generation gen: Int) {
        lock.lock()
        defer { lock.unlock() }
        guard gen == generation, currentItemRef === item else { return } // 旧 item 迟到回调
        eofReached = true
    }

    /// item 状态变化入口（generation 为注册时快照；internal 供测试直注旧回调）：
    /// 仅当前 item 的失败才清空媒体；旧 item 的迟到失败一律忽略。
    func handleItemStatus(_ item: AVPlayerItem, generation gen: Int) {
        guard item.status == .failed else { return }
        lock.lock()
        let stale = (gen != generation) || (currentItemRef !== item)
        lock.unlock()
        guard !stale else { return }
        lock.lock()
        itemFailed = true
        lock.unlock()
        player.replaceCurrentItem(with: nil)
    }

    func pause() -> String? {
        player.pause() // 幂等，与 mpv set pause yes 语义等价
        return nil
    }

    func stop() -> String? {
        // 先摘除旧 item 的观察者（block 通知 token 须显式 removeObserver，
        // KVO 须 invalidate——否则旧 item 的迟到回调永久存活）并 bump generation
        // 使任何在途回调失效，再清空媒体。
        detachObservers()
        lock.lock()
        generation += 1
        eofReached = false
        itemFailed = false
        durationMS = 0
        mediaTitle = ""
        currentItemRef = nil
        lock.unlock()
        player.replaceCurrentItem(with: nil) // 回到无媒体（等价 mpv stop → idle）
        return nil
    }

    /// 摘除当前 item 的观察者（自行加锁；幂等）。
    private func detachObservers() {
        lock.lock()
        let end = endObserver
        let status = statusObservation
        endObserver = nil
        statusObservation = nil
        lock.unlock()
        if let end = end {
            NotificationCenter.default.removeObserver(end)
        }
        status?.invalidate()
    }

    deinit {
        if let end = endObserver {
            NotificationCenter.default.removeObserver(end)
        }
        statusObservation?.invalidate()
    }

    func seekTo(ms: Int64) -> String? {
        player.seek(to: CMTime(value: CMTimeValue(ms), timescale: 1000)) // 绝对位置，不改变播放状态
        return nil
    }

    func volume(level: Int) -> String? {
        // 协议 level 0-100 → AVPlayer volume 0.0-1.0
        player.volume = Float(max(0, min(100, level))) / 100.0
        return nil
    }

    func status() -> ControllerStatus {
        lock.lock()
        let eof = eofReached
        let failed = itemFailed
        let dur = durationMS
        let title = mediaTitle
        lock.unlock()

        if eof || failed || player.currentItem == nil {
            return ControllerStatus(state: "idle") // 全零（等价 mpv idle-active/eof-reached）
        }

        let paused = player.timeControlStatus == .paused
        let pos = Self.secToMS(player.currentTime().seconds)
        // 双保险：位置已到片尾（等价 mpv eof-reached）亦自报 idle——
        // 播完通知在个别模拟器/真机时序下可能迟到，不能只依赖它。
        // 50ms 容差吸收容器时长与末帧时间戳的舍入差。
        if dur > 0 && pos + 50 >= dur {
            return ControllerStatus(state: "idle")
        }
        return ControllerStatus(
            state: paused ? "paused" : "playing",
            positionMS: pos,
            durationMS: dur,
            title: title
        )
    }

    /// 时长落库（同步方法内加锁，避免在异步上下文直接 lock/unlock）；
    /// generation 已过期（旧 asset 的迟到加载）则丢弃。
    private func storeDuration(_ ms: Int64, generation gen: Int) {
        lock.lock()
        if gen == generation {
            durationMS = ms
        }
        lock.unlock()
    }

    /// 浮点秒 → 毫秒，四舍五入（对齐 Go mpv.go secToMS）。
    private static func secToMS(_ sec: Double) -> Int64 {
        guard sec.isFinite else { return 0 }
        return Int64((sec * 1000.0).rounded())
    }
}
