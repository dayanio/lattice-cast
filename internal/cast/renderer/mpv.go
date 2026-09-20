package renderer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/dayanio/lattice-cast/internal/cast/adapter"
)

// ipcDialTimeout 单次 unix socket 连接超时；ipcBootTimeout 等待 mpv 创建
// IPC socket 的上限（mpv 启动即建 socket，通常毫秒级）；closeReapTimeout
// 是 Close 等待 reaper（cmd.Wait）收尸的上限兜底。
const (
	ipcDialTimeout   = 2 * time.Second
	ipcBootTimeout   = 5 * time.Second
	closeReapTimeout = 2 * time.Second

	// 起播确认窗口：loadfile 对不可播源也回 success（mpv 异步验证媒体，本机
	// 实测闭合端口 URL 约 5.9s 后才放弃、本地不存在文件约 110ms），故下发后
	// 须继续观测真实起播——上限 10s、每 100ms 轮询一次 IPC 属性。
	loadStartTimeout = 10 * time.Second
	loadStartPoll    = 100 * time.Millisecond
)

// MpvController 是 Controller 的 mpv 生产实现：整个进程生命周期内拉起一个
//
//	mpv --input-ipc-server=<unix sock> --idle=yes --keep-open=yes --fullscreen
//
// 以空闲模式常驻，经 JSON IPC（按行分隔的 JSON 对象）下发 loadfile/pause/
// stop/seek/volume 并轮询 time-pos/duration/pause/media-title 等属性。
type MpvController struct {
	binPath   string   // 解析后的 mpv 可执行路径；空 = 测试直连既有 socket（不拥有进程、不参与重生）
	extraArgs []string // 常驻基础旗标之后的附加旗标（每个 epoch 按同一形态拉起）

	sockPath string // 当前 epoch 的 --input-ipc-server unix socket 路径

	tmpDir   string    // 当前 epoch socket 所在临时目录（进程归我们管时非空，换代/Close 时清理）
	cmd      *exec.Cmd // 当前 epoch 的 mpv 进程（测试直连 socket 时为 nil）
	reapDone chan struct{}
	// reaper（独占 cmd.Wait）完成信号：Close/重生只 kill 不 Wait，据它同步收尾
	// （带超时兜底）；nil 表示无进程可等（测试直连 / 已 Close），保证幂等。

	mu     sync.Mutex
	conn   net.Conn      // IPC 连接（懒建立，断线重连）
	br     *bufio.Reader // 与 conn 绑定的行读取器（跨命令复用缓冲）
	id     int64         // request_id 自增，用于响应关联
	closed bool          // Close 已调用：此后不得再重生新进程（防 Close 后复活）
}

// NewMpvController 校验 mpv 可用、拉起常驻 mpv 进程并等待 IPC socket 就绪。
// mpv 不在 PATH 时返回带安装提示的错误（调用方应打印后退出）。
func NewMpvController(mpvBin string) (*MpvController, error) {
	return newMpvController(mpvBin, "--fullscreen")
}

// newMpvController 是构造主体；extraArgs 追加在常驻基础旗标之后（生产传
// --fullscreen；真实进程测试传 --vo=null --ao=null 等保持无头不侵入）。
// 构造即拉起首个 epoch；此后的进程生命周期见 ensureProcessLocked（惰性重生）。
func newMpvController(mpvBin string, extraArgs ...string) (*MpvController, error) {
	path, err := exec.LookPath(mpvBin)
	if err != nil {
		return nil, errors.New("mpv not found: brew install mpv (macOS) / apt install mpv (Debian)")
	}
	m := &MpvController{binPath: path, extraArgs: extraArgs}
	if err := m.spawn(); err != nil {
		return nil, err
	}
	return m, nil
}

// newIpcController 直连既有 IPC socket（测试用：不起真实 mpv 进程）。
func newIpcController(sockPath string) *MpvController {
	return &MpvController{sockPath: sockPath}
}

// Close 断开 IPC、杀掉 mpv 进程并清理临时目录（幂等：重复 Close 时进程
// 字段已清空，直接落到目录清理收尾）。Wait 由 reaper 独占——这里只 kill，
// 再等 reaper 收尸（超时兜底则放弃等待，进程由 reaper 迟缓收尾）。
// 终止的是"当前 epoch"：若此前发生过惰性重生，杀的就是重生后的进程。
// Close 后不再重生（closed 闸，防关停路径被并发 Load 复活进程）。
func (m *MpvController) Close() error {
	m.mu.Lock()
	m.closed = true
	m.disconnectLocked()
	cmd := m.cmd
	done := m.reapDone
	m.cmd, m.reapDone = nil, nil
	dir := m.tmpDir
	m.tmpDir = ""
	m.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(closeReapTimeout):
		}
	}
	if dir != "" {
		return os.RemoveAll(dir)
	}
	return nil
}

// ---- 进程 epoch 管理（惰性重生）----
//
// mpv 进程按 epoch 计：每个 epoch = 一次 spawn 产出的 (tmpdir, socket, cmd,
// reaper)。用户在 mpv 窗口按 q 或 mpv 崩溃都会终结当前 epoch——此前渲染端
// 会就此"变砖"（socket 拒连，所有 /play 永久失败）；现在 Load 在进入时经
// ensureProcessLocked 确认 epoch 存活，已死则换代重生。换代与 reaper/Close
// 的并发安全：reaper 独占 cmd.Wait，任何路径只 Kill 不 Wait；Close 与重生
// 在 mu 下串行读写 epoch 字段，Close 置 closed 闸后不再重生。

// spawn 拉起一个全新的 mpv epoch（spawnLocked 的加锁包装）。
func (m *MpvController) spawn() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spawnLocked()
}

// spawnLocked 拉起一个全新的 mpv epoch：新 tmpdir/socket → 常驻进程 →
// reaper → 等待 IPC socket 就绪；未就绪或启动失败则按 epoch 清理后上抛。
// 须持有 m.mu。
func (m *MpvController) spawnLocked() error {
	tmpDir, err := os.MkdirTemp("", "latticecast-mpv-")
	if err != nil {
		return fmt.Errorf("create mpv tmpdir: %w", err)
	}
	m.tmpDir = tmpDir
	m.sockPath = filepath.Join(tmpDir, "mpv-ipc.sock")
	args := []string{
		"--input-ipc-server=" + m.sockPath,
		"--idle=yes",      // 无媒体时驻留
		"--keep-open=yes", // 播完不退出（eof 后回 idle 而非退出进程）
	}
	args = append(args, m.extraArgs...)
	m.cmd = exec.Command(m.binPath, args...)
	// 静默 mpv 自身输出：测试要求 pristine output，排障靠 IPC 层报错。
	m.cmd.Stdout = io.Discard
	m.cmd.Stderr = io.Discard
	if err := m.cmd.Start(); err != nil {
		m.killEpochLocked()
		return fmt.Errorf("start mpv %s: %w", m.binPath, err)
	}
	// reaper 独占 cmd.Wait（exec.Cmd 的 Wait 不允许并发调用）：mpv 死亡时
	// 记日志收尸，避免僵尸进程；Close/重生经 reapDone 与之同步，不二次 Wait。
	cmd := m.cmd
	done := make(chan struct{})
	m.reapDone = done
	go func() {
		defer close(done)
		err := cmd.Wait()
		slog.Warn("mpv exited", "err", err) // kill 停机时为 signal: killed，属预期
	}()
	if err := m.waitSocket(ipcBootTimeout); err != nil {
		m.killEpochLocked()
		return err
	}
	return nil
}

// ensureProcessLocked 确保当前 epoch 的 mpv 存活；reaper 已观察到退出则惰性
// 重生（Load 专用入口）。Status 是只读路径不走这里（mpv 死时如实报 idle，
// 不得凭空拉起窗口）；Pause/Stop/SeekTo/Volume 也不重生——死进程上干净失败，
// 由下一次 /play 恢复。须持有 m.mu。
func (m *MpvController) ensureProcessLocked() error {
	if m.binPath == "" {
		return nil // 测试直连 socket 模式：不拥有进程
	}
	if m.closed {
		return errors.New("mpv controller closed")
	}
	if m.cmd != nil && m.reapDone != nil {
		select {
		case <-m.reapDone: // reaper 已收尸：进程确死，走重生
		default:
			return nil // 本 epoch 存活
		}
	}
	return m.respawnLocked()
}

// respawnLocked 丢弃当前 epoch（Kill、有界等 reaper 收尸、清 tmpdir）并拉起
// 全新 epoch。须持有 m.mu。
func (m *MpvController) respawnLocked() error {
	if m.binPath == "" {
		return nil // 测试直连 socket 模式：无可重生之物
	}
	if m.closed {
		return errors.New("mpv controller closed")
	}
	m.killEpochLocked()
	return m.spawnLocked()
}

// respawn 是 respawnLocked 的加锁包装（Load 的连接级故障重试路径用）。
func (m *MpvController) respawn() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.respawnLocked()
}

// killEpochLocked 终止并清空当前 epoch：断开 IPC、Kill 进程（不 Wait——
// reaper 独占 cmd.Wait）、有界等待收尸、清理 tmpdir 并清空进程字段。幂等。
// 须持有 m.mu。
func (m *MpvController) killEpochLocked() {
	done := m.reapDone
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill() // 已退出时 ErrProcessDone，忽略
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(closeReapTimeout):
		}
	}
	m.disconnectLocked()
	m.cmd, m.reapDone = nil, nil
	if m.tmpDir != "" {
		_ = os.RemoveAll(m.tmpDir)
		m.tmpDir = ""
	}
}

// ---- Controller 接口 ----

// Load 播放指定 URL：loadfile <url> replace；positionMS>0 时把续播位置折叠为
// loadfile 选项串的 start=+<sec>（12500 → "start=+12.5"，-1 精度去掉尾零，
// 整秒输出 "start=+30"）。注意 loadfile 的第 4 个位置参数是插入 index 而非
// 选项（本机 mpv 0.41 实测 4-arg "start=..." 回 invalid parameter），选项须
// 作第 5 参数，index 用 -1 追加到播放列表末尾。mpv 的 loadfile 异步生效，
// load 后立即 seek <sec> absolute 会在文件加载完成前被 mpv 拒绝——错误一旦
// 被吞就表现为续播从 0 开始，故位置必须随 load 一起下发；title 非空时补发
// set force-media-title <title>。mpv 的 media-title 属性本身只读
// （实测 0.41 报 error running command），force-media-title 是官方的
// 展示标题覆写位，media-title 随之生效。
//
// 进程管理：Load 是唯一的惰性复活入口——先 ensureProcess 确认当前 epoch
// 存活（用户 q 退出 / 崩溃后在此重拉全新 mpv，首次 Load 即成功）；若进程在
// 检查与命令之间死亡（reaper 收尸未落、socket 先拒连的窗口），命令返回
// 连接级故障时换代重试一次。命令级错误（如 invalid parameter）不触发重生。
//
// 起播确认：loadfile 的成功回执不代表媒体可播——mpv 异步验证媒体，对不存在/
// 不可达的源照样回 success，随后才静默退回 idle（线上事故：/play 谎报
// playing，调用方一直以为在播）。故下发后经 awaitPlaybackStart 有界等待真实
// 起播（在调用方 ctx 上，可取消），不可播源如实报错——Server 的 /play 据此
// 进 error 态（protocol.md 第六节）。确认阶段命中连接级故障同样走上面的换代
// 重试路径。
func (m *MpvController) Load(ctx context.Context, url, title string, positionMS int64) error {
	m.mu.Lock()
	err := m.ensureProcessLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}

	if err := m.loadAndConfirmStart(ctx, url, title, positionMS); !isConnErr(err) {
		return err
	}
	// epoch 在检查与命令之间死亡：换代重生后重试一次
	if err := m.respawn(); err != nil {
		return err
	}
	return m.loadAndConfirmStart(ctx, url, title, positionMS)
}

// loadAndConfirmStart 下发 loadfile（与可选的标题覆写）后等待真实起播。
func (m *MpvController) loadAndConfirmStart(ctx context.Context, url, title string, positionMS int64) error {
	if err := m.load(url, title, positionMS); err != nil {
		return err
	}
	return m.awaitPlaybackStart(ctx)
}

// awaitPlaybackStart 起播确认：loadfile 之后有界轮询（10s 上限、100ms 间隔）
// IPC 属性，直到出现明确结论，不再 fire-and-forget：
//   - started：time-pos 可用且 > 0（解码真正到达播放）→ 立即成功返回；
//   - eof-reached=true 且 time-pos 停在 0（打开即完的空源）→ 不可播；
//   - idle-active 翻回 true（曾离开 idle 又回来：mpv 已放弃加载）→ 不可播；
//   - 超时 / 调用方 ctx 取消 → 失败。
//
// 属性读取的 "property unavailable" 是命令级错误（mpv 尚无答案，继续等）；
// 连接级故障上抛（Load 据此换代重试）。等待发生在调用方 ctx 上，取消即中止。
// 轮询按次进出 m.mu，不跨睡眠持锁，/status 等并发命令不受阻塞。
func (m *MpvController) awaitPlaybackStart(ctx context.Context) error {
	deadline := time.Now().Add(loadStartTimeout)
	ticker := time.NewTicker(loadStartPoll)
	defer ticker.Stop()

	var leftIdle bool // 已观察到 idle-active=false：loadfile 已被 mpv 接受
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("mpv load aborted: %w", ctx.Err())
		case <-ticker.C:
		}

		// started：解码已到达播放（起播即返回，不等满窗口）
		if v, err := m.getFloat("time-pos"); err == nil && v > 0 {
			return nil
		} else if isConnErr(err) {
			return fmt.Errorf("mpv ipc lost while confirming start: %w", err)
		}
		// eof：媒体打开即完（time-pos 未越过 0）——不可播的空源
		if eof, err := m.getBool("eof-reached"); err == nil && eof {
			return errors.New("mpv reached eof at position 0: source unplayable")
		} else if isConnErr(err) {
			return fmt.Errorf("mpv ipc lost while confirming start: %w", err)
		}
		// idle 回环：曾离开 idle（load 被接受）又翻回 idle——加载失败
		if idle, err := m.getBool("idle-active"); err == nil {
			if idle && leftIdle {
				return errors.New("mpv returned to idle without playback: source unplayable")
			}
			leftIdle = !idle
		} else if isConnErr(err) {
			return fmt.Errorf("mpv ipc lost while confirming start: %w", err)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("mpv did not start playback within %s: source unplayable", loadStartTimeout)
		}
	}
}

// load 仅下发 loadfile（与可选的标题覆写），不含进程管理。
func (m *MpvController) load(url, title string, positionMS int64) error {
	args := []any{"loadfile", url, "replace"}
	if positionMS > 0 {
		sec := strconv.FormatFloat(float64(positionMS)/1000.0, 'f', -1, 64)
		args = append(args, "-1", "start=+"+sec)
	}
	if _, err := m.command(args...); err != nil {
		return err
	}
	if title != "" {
		// 注：title 为空时不覆写，mpv 沿用媒体自身元数据标题；
		// 协议侧 /status 的标题由 Server 记录，不依赖该值。
		if _, err := m.command("set", "force-media-title", title); err != nil {
			return err
		}
	}
	return nil
}

// Pause 暂停：读 pause 属性，未暂停才 set pause yes（对调用方等价于
// 播放中切换到暂停；已暂停时幂等不抖动）。mpv 的 set 命令按选项字符串
// 语义解析参数，布尔/数值须以字符串形式下发（实测裸 JSON true 会报
// invalid parameter），"yes"/"no" 由 mpv 按属性类型转换。
func (m *MpvController) Pause() error {
	paused, err := m.getBool("pause")
	if err != nil {
		return err
	}
	if paused {
		return nil
	}
	_, err = m.command("set", "pause", "yes")
	return err
}

// Stop 停止并清空当前媒体：mpv 回到 idle（--idle=yes 驻留不退出）。
func (m *MpvController) Stop() error {
	_, err := m.command("stop")
	return err
}

// SeekTo 跳转到绝对位置：毫秒 → 秒后 seek <sec> absolute。
// （不命名 Seek，避免与 io.Seeker 的 stdmethods 惯例冲突。）
func (m *MpvController) SeekTo(ms int64) error {
	_, err := m.command("seek", float64(ms)/1000.0, "absolute")
	return err
}

// Volume 设置音量：协议 level 0-100 与 mpv volume 量纲一致，直接下发
// （字符串形式，理由同 Pause）。
func (m *MpvController) Volume(level int) error {
	_, err := m.command("set", "volume", strconv.Itoa(level))
	return err
}

// Status 从 mpv 属性映射协议状态：idle-active 或 eof-reached → idle（全零）；
// pause → paused；有媒体且未暂停 → playing；time-pos/duration 为浮点秒，
// 四舍五入为毫秒。属性读取失败按零值处理（mpv 无该属性 / 连接刚断）。
// IPC 连接级故障（mpv 已死 / socket 不可达）不再吞成零值 playing——那会让
// agent 看到一个永远 playing 的僵尸——而是告警并按 idle 上报（对协议层等价
// 于"无媒体可播"）。告警每次轮询触发一次，未做限频（v1 /status 轮询频率
// 低，可接受；mpv 被杀后 mpv exited 由 reaper 记录根因）。
func (m *MpvController) Status() adapter.Status {
	var connErr error // 首个 IPC 连接级错误（非 nil = mpv 不可达）
	pick := func(err error) {
		if connErr == nil && isConnErr(err) {
			connErr = err
		}
	}
	boolProp := func(name string) bool {
		v, err := m.getBool(name)
		pick(err)
		return err == nil && v
	}
	floatProp := func(name string) (float64, bool) {
		v, err := m.getFloat(name)
		pick(err)
		return v, err == nil
	}
	strProp := func(name string) (string, bool) {
		v, err := m.getString(name)
		pick(err)
		return v, err == nil
	}

	if boolProp("idle-active") || boolProp("eof-reached") {
		return adapter.Status{State: stateIdle}
	}

	st := adapter.Status{State: statePlaying}
	if boolProp("pause") {
		st.State = statePaused
	}
	if v, ok := floatProp("time-pos"); ok {
		st.PositionMS = secToMS(v)
	}
	if v, ok := floatProp("duration"); ok {
		st.DurationMS = secToMS(v)
	}
	if v, ok := strProp("media-title"); ok {
		st.Title = v
	}

	if connErr != nil {
		slog.Warn("mpv ipc unreachable; reporting idle", "err", connErr)
		return adapter.Status{State: stateIdle}
	}
	return st
}

// isConnErr 判定 err 是否为 IPC 传输层故障（dial/读写失败），与 mpv 命令级
// 错误（如 "property unavailable"，errors.New 纯文本）区分：command() 的
// 传输错误均包裹底层 net/io 错误，据此识别。
func isConnErr(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// ---- JSON IPC 传输 ----

// command 发送一条命令并等待其响应（mpv 的事件推送在读取时跳过）。
// 连接懒建立；写失败重连一次再试（mpv 不会中途换 socket，重连兜底足够）。
func (m *MpvController) command(args ...any) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.id++
	req, err := json.Marshal(struct {
		Command   []any `json:"command"`
		RequestID int64 `json:"request_id"`
	}{args, m.id})
	if err != nil {
		return nil, err
	}

	if err := m.ensureConnLocked(); err != nil {
		return nil, err
	}
	if _, err := m.conn.Write(append(req, '\n')); err != nil {
		m.disconnectLocked()
		if err := m.ensureConnLocked(); err != nil {
			return nil, err
		}
		if _, err := m.conn.Write(append(req, '\n')); err != nil {
			return nil, err
		}
	}
	return m.readResponseLocked()
}

// readResponseLocked 逐行读取响应：跳过事件推送（含 event 字段），第一条
// 带本命令响应形态（error 字段）的行即结果；error 非 "success" 视为命令失败。
func (m *MpvController) readResponseLocked() (json.RawMessage, error) {
	for {
		line, err := m.br.ReadBytes('\n')
		if err != nil {
			m.disconnectLocked()
			return nil, fmt.Errorf("mpv ipc read: %w", err)
		}
		var resp struct {
			Error string          `json:"error"`
			Data  json.RawMessage `json:"data"`
			Event string          `json:"event"`
		}
		if json.Unmarshal(line, &resp) != nil {
			continue // 非法行：跳过
		}
		if resp.Event != "" {
			continue // 事件推送：与命令响应无关
		}
		if resp.Error != "success" {
			return nil, errors.New(resp.Error)
		}
		return resp.Data, nil
	}
}

// ensureConnLocked 懒建立 IPC 连接（调用方须持有 m.mu）。
func (m *MpvController) ensureConnLocked() error {
	if m.conn != nil {
		return nil
	}
	conn, err := net.DialTimeout("unix", m.sockPath, ipcDialTimeout)
	if err != nil {
		return fmt.Errorf("mpv ipc dial %s: %w", m.sockPath, err)
	}
	m.conn = conn
	m.br = bufio.NewReader(conn)
	return nil
}

// disconnectLocked 关闭并清空当前连接（调用方须持有 m.mu）。
func (m *MpvController) disconnectLocked() {
	if m.conn != nil {
		_ = m.conn.Close()
		m.conn = nil
		m.br = nil
	}
}

// waitSocket 轮询直到 mpv 创建出 IPC socket 或超时。
func (m *MpvController) waitSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("unix", m.sockPath, ipcDialTimeout)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mpv ipc socket %s not ready within %s: %w", m.sockPath, timeout, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---- 属性读取辅助 ----

func (m *MpvController) getBool(name string) (bool, error) {
	data, err := m.command("get_property", name)
	if err != nil {
		return false, err
	}
	var v bool
	return v, json.Unmarshal(data, &v)
}

func (m *MpvController) getFloat(name string) (float64, error) {
	data, err := m.command("get_property", name)
	if err != nil {
		return 0, err
	}
	var v float64
	return v, json.Unmarshal(data, &v)
}

func (m *MpvController) getString(name string) (string, error) {
	data, err := m.command("get_property", name)
	if err != nil {
		return "", err
	}
	var v string
	return v, json.Unmarshal(data, &v)
}

// secToMS 浮点秒 → 毫秒（四舍五入）。
func secToMS(sec float64) int64 {
	return int64(math.Round(sec * 1000))
}
