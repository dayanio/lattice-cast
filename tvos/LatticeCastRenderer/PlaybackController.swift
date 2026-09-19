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
    private var eofReached = false
    private var itemFailed = false
    private var durationMS: Int64 = 0
    private var mediaTitle = ""
    private var endObserver: NSObjectProtocol?
    private var statusObservation: NSKeyValueObservation?

    func load(url: String, title: String, positionMS: Int64) -> String? {
        guard let u = URL(string: url), u.scheme != nil, u.host != nil || u.isFileURL else {
            return "invalid_url"
        }

        lock.lock()
        eofReached = false
        itemFailed = false
        durationMS = 0
        mediaTitle = title
        lock.unlock()

        let asset = AVURLAsset(url: u)
        let item = AVPlayerItem(asset: asset)

        // 播完 EOF：置位后 status() 自报 idle（等价 mpv eof-reached → idle）
        let observer = NotificationCenter.default.addObserver(
            forName: NSNotification.Name.AVPlayerItemDidPlayToEndTime,
            object: item, queue: nil
        ) { [weak self] _ in
            self?.lock.lock()
            self?.eofReached = true
            self?.lock.unlock()
        }
        // item 加载失败（源不可达/解码失败）：清空媒体 → status() 自报 idle，
        // 等价 mpv loadfile 失败后回到 idle（Server 经 /status 如实迁移）。
        let statusObs = item.observe(\.status, options: [.new]) { [weak self] item, _ in
            guard item.status == .failed else { return }
            self?.lock.lock()
            self?.itemFailed = true
            self?.lock.unlock()
            self?.player.replaceCurrentItem(with: nil)
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
                self?.storeDuration(ms)
            }
        }

        player.replaceCurrentItem(with: item)
        player.isMuted = true // 渲染端只管视频画面/进度，音频交给系统输出设备（避免独占 audio session）
        if positionMS > 0 {
            // 续播位置折叠进 load：seek 先于 play 下发，AVPlayer 会在 item
            // 就绪后从该位置起播（等价 loadfile start=+<sec>）。
            player.seek(to: CMTime(value: CMTimeValue(positionMS), timescale: 1000))
        }
        player.play()
        return nil
    }

    func pause() -> String? {
        player.pause() // 幂等，与 mpv set pause yes 语义等价
        return nil
    }

    func stop() -> String? {
        lock.lock()
        eofReached = false
        itemFailed = false
        durationMS = 0
        mediaTitle = ""
        endObserver = nil
        statusObservation = nil
        lock.unlock()
        player.replaceCurrentItem(with: nil) // 回到无媒体（等价 mpv stop → idle）
        return nil
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

    /// 时长落库（同步方法内加锁，避免在异步上下文直接 lock/unlock）。
    private func storeDuration(_ ms: Int64) {
        lock.lock()
        durationMS = ms
        lock.unlock()
    }

    /// 浮点秒 → 毫秒，四舍五入（对齐 Go mpv.go secToMS）。
    private static func secToMS(_ sec: Double) -> Int64 {
        guard sec.isFinite else { return 0 }
        return Int64((sec * 1000.0).rounded())
    }
}
