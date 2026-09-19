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
)

// MpvController 是 Controller 的 mpv 生产实现：整个进程生命周期内拉起一个
//
//	mpv --input-ipc-server=<unix sock> --idle=yes --keep-open=yes --fullscreen
//
// 以空闲模式常驻，经 JSON IPC（按行分隔的 JSON 对象）下发 loadfile/pause/
// stop/seek/volume 并轮询 time-pos/duration/pause/media-title 等属性。
type MpvController struct {
	sockPath string // mpv --input-ipc-server 的 unix socket 路径

	tmpDir   string    // socket 所在临时目录（进程归我们管时非空，Close 时清理）
	cmd      *exec.Cmd // 常驻 mpv 进程（测试直连 socket 时为 nil）
	reapDone chan struct{}
	// reaper（独占 cmd.Wait）完成信号：Close 只 kill 不 Wait，据它同步收尾
	// （带超时兜底）；nil 表示无进程可等（测试直连 / 已 Close），保证幂等。

	mu   sync.Mutex
	conn net.Conn      // IPC 连接（懒建立，断线重连）
	br   *bufio.Reader // 与 conn 绑定的行读取器（跨命令复用缓冲）
	id   int64         // request_id 自增，用于响应关联
}

// NewMpvController 校验 mpv 可用、拉起常驻 mpv 进程并等待 IPC socket 就绪。
// mpv 不在 PATH 时返回带安装提示的错误（调用方应打印后退出）。
func NewMpvController(mpvBin string) (*MpvController, error) {
	return newMpvController(mpvBin, "--fullscreen")
}

// newMpvController 是构造主体；extraArgs 追加在常驻基础旗标之后（生产传
// --fullscreen；真实进程测试传 --vo=null --ao=null 等保持无头不侵入）。
func newMpvController(mpvBin string, extraArgs ...string) (*MpvController, error) {
	path, err := exec.LookPath(mpvBin)
	if err != nil {
		return nil, errors.New("mpv not found: brew install mpv (macOS) / apt install mpv (Debian)")
	}
	tmpDir, err := os.MkdirTemp("", "latticecast-mpv-")
	if err != nil {
		return nil, fmt.Errorf("create mpv tmpdir: %w", err)
	}
	m := &MpvController{
		sockPath: filepath.Join(tmpDir, "mpv-ipc.sock"),
		tmpDir:   tmpDir,
	}
	args := []string{
		"--input-ipc-server=" + m.sockPath,
		"--idle=yes",      // 无媒体时驻留
		"--keep-open=yes", // 播完不退出（eof 后回 idle 而非退出进程）
	}
	args = append(args, extraArgs...)
	m.cmd = exec.Command(path, args...)
	// 静默 mpv 自身输出：测试要求 pristine output，排障靠 IPC 层报错。
	m.cmd.Stdout = io.Discard
	m.cmd.Stderr = io.Discard
	if err := m.cmd.Start(); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("start mpv %s: %w", path, err)
	}
	// reaper 独占 cmd.Wait（exec.Cmd 的 Wait 不允许并发调用）：mpv 死亡时
	// 记日志收尸，避免僵尸进程；Close 经 reapDone 与之同步，不二次 Wait。
	cmd := m.cmd
	done := make(chan struct{})
	m.reapDone = done
	go func() {
		defer close(done)
		err := cmd.Wait()
		slog.Warn("mpv exited", "err", err) // kill 停机时为 signal: killed，属预期
	}()
	if err := m.waitSocket(ipcBootTimeout); err != nil {
		_ = m.Close()
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
func (m *MpvController) Close() error {
	m.mu.Lock()
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
func (m *MpvController) Load(_ context.Context, url, title string, positionMS int64) error {
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
