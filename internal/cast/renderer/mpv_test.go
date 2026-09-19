package renderer

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dayanio/lattice-cast/internal/cast/adapter"
)

// newFakeMpv 是测试内嵌的假 mpv IPC 服务：监听真实 unix socket，逐行接收
// JSON IPC 报文并原样记录，按命令类型回 canned 响应（get_property 按
// 预置属性表取值，其余一律 {"error":"success"}）。用于对 MpvController
// 做报文组帧（framing）与响应解析的单测，不需要真实 mpv 进程。
type fakeMpv struct {
	ln       net.Listener
	sockPath string

	mu       sync.Mutex
	received []string       // 原样记录的请求行
	props    map[string]any // get_property 名 → data 值；缺失 = property unavailable
}

func newFakeMpv(t *testing.T) *fakeMpv {
	t.Helper()
	// unix socket 路径有 sun_path 上限（macOS 104 字节），t.TempDir() 会带
	// 超长测试名路径，这里用短前缀临时目录 + 测试结束清理。
	dir, err := os.MkdirTemp("", "lcr-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "mpv.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	f := &fakeMpv{ln: ln, sockPath: sockPath, props: map[string]any{}}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeMpv) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return // listener 已关闭
		}
		go f.handle(conn)
	}
}

func (f *fakeMpv) handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		var req struct {
			Command   []any `json:"command"`
			RequestID int64 `json:"request_id"`
		}
		if json.Unmarshal([]byte(line), &req) != nil {
			continue
		}

		f.mu.Lock()
		f.received = append(f.received, line)
		resp := f.reply(req.Command, req.RequestID)
		f.mu.Unlock()

		_, _ = conn.Write(append(resp, '\n'))
	}
}

// reply 按 mpv JSON IPC 语义构造单行响应。
func (f *fakeMpv) reply(cmd []any, id int64) []byte {
	base := map[string]any{"request_id": id}
	if len(cmd) == 0 {
		base["error"] = "invalid parameter"
	} else if get, ok := cmd[0].(string); !ok || get != "get_property" {
		base["error"] = "success"
		base["data"] = nil
	} else if len(cmd) < 2 {
		base["error"] = "invalid parameter"
	} else {
		name, _ := cmd[1].(string)
		v, known := f.props[name]
		if !known {
			base["error"] = "property unavailable"
			base["data"] = nil
		} else {
			base["error"] = "success"
			base["data"] = v
		}
	}
	b, _ := json.Marshal(base)
	return b
}

func (f *fakeMpv) setProp(name string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.props[name] = v
}

// commands 解析并返回已接收的全部 command 数组（按到达顺序）。
func (f *fakeMpv) commands() [][]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]any
	for _, line := range f.received {
		var req struct {
			Command []any `json:"command"`
		}
		if json.Unmarshal([]byte(line), &req) == nil {
			out = append(out, req.Command)
		}
	}
	return out
}

// newTestController 返回连接到假 mpv socket 的被测控制器（不起真实进程）。
func newTestController(t *testing.T, f *fakeMpv) *MpvController {
	t.Helper()
	return newIpcController(f.sockPath)
}

func TestIpcLoad_SendsLoadfileThenTitle(t *testing.T) {
	f := newFakeMpv(t)
	ctl := newTestController(t, f)

	require.NoError(t, ctl.Load(context.Background(), "http://192.168.1.10:7810/media/abc123", "星际穿越", 0))

	cmds := f.commands()
	require.Len(t, cmds, 2)
	assert.Equal(t, []any{"loadfile", "http://192.168.1.10:7810/media/abc123", "replace"}, cmds[0])
	assert.Equal(t, []any{"set", "force-media-title", "星际穿越"}, cmds[1])
}

func TestIpcLoad_EmptyTitleSkipsTitleSet(t *testing.T) {
	f := newFakeMpv(t)
	ctl := newTestController(t, f)

	require.NoError(t, ctl.Load(context.Background(), "http://x/a.mp4", "", 0))

	// 位置 0：plain 3 元素 loadfile，无 start 选项，无标题覆写
	assert.Equal(t, [][]any{{"loadfile", "http://x/a.mp4", "replace"}}, f.commands())
}

func TestIpcLoad_ResumePosition_FoldsIntoStartOption(t *testing.T) {
	f := newFakeMpv(t)
	ctl := newTestController(t, f)

	// 12500ms → 恰好 4 元素：["loadfile", url, "replace", "start=+12.5"]
	//（mpv 的 loadfile 异步生效，续播位置必须折进 load，不能 load 后补 seek）
	require.NoError(t, ctl.Load(context.Background(), "http://x/a.mp4", "星际穿越", 12500))

	cmds := f.commands()
	require.Len(t, cmds, 2, "start 折进 loadfile，不应另发 seek")
	assert.Equal(t, []any{"loadfile", "http://x/a.mp4", "replace", "start=+12.5"}, cmds[0])
	assert.Equal(t, []any{"set", "force-media-title", "星际穿越"}, cmds[1])
}

func TestIpcLoad_ResumePosition_SubSecondMs(t *testing.T) {
	f := newFakeMpv(t)
	ctl := newTestController(t, f)

	// 非整百毫秒：-1 精度浮点格式化保留全部毫秒位（90250 → "start=+90.25"）
	require.NoError(t, ctl.Load(context.Background(), "http://x/a.mp4", "", 90250))

	assert.Equal(t, [][]any{{"loadfile", "http://x/a.mp4", "replace", "start=+90.25"}}, f.commands())
}

func TestIpcPause_FromPlaying_SetsPauseTrue(t *testing.T) {
	f := newFakeMpv(t)
	f.setProp("pause", false)
	ctl := newTestController(t, f)

	require.NoError(t, ctl.Pause())

	cmds := f.commands()
	assert.Contains(t, cmds, []any{"get_property", "pause"})
	assert.Contains(t, cmds, []any{"set", "pause", "yes"})
}

func TestIpcPause_AlreadyPaused_NoSet(t *testing.T) {
	f := newFakeMpv(t)
	f.setProp("pause", true)
	ctl := newTestController(t, f)

	require.NoError(t, ctl.Pause())

	assert.Contains(t, f.commands(), []any{"get_property", "pause"})
	for _, cmd := range f.commands() {
		if len(cmd) >= 2 && cmd[0] == "set" && cmd[1] == "pause" {
			t.Fatalf("已暂停时不应再 set pause，收到 %v", cmd)
		}
	}
}

func TestIpcStop_SendsStop(t *testing.T) {
	f := newFakeMpv(t)
	ctl := newTestController(t, f)

	require.NoError(t, ctl.Stop())
	assert.Contains(t, f.commands(), []any{"stop"})
}

func TestIpcSeek_MsToAbsoluteSeconds(t *testing.T) {
	f := newFakeMpv(t)
	ctl := newTestController(t, f)

	require.NoError(t, ctl.SeekTo(90000))
	assert.Contains(t, f.commands(), []any{"seek", 90.0, "absolute"})
}

func TestIpcVolume_LevelDirect(t *testing.T) {
	f := newFakeMpv(t)
	ctl := newTestController(t, f)

	require.NoError(t, ctl.Volume(42))
	assert.Contains(t, f.commands(), []any{"set", "volume", "42"})
}

func TestIpcStatus_PausedWithProps(t *testing.T) {
	f := newFakeMpv(t)
	f.setProp("idle-active", false)
	f.setProp("eof-reached", false)
	f.setProp("pause", true)
	f.setProp("time-pos", 4.2)
	f.setProp("duration", 54.0)
	f.setProp("media-title", "Interstellar")
	ctl := newTestController(t, f)

	st := ctl.Status()
	assert.Equal(t, adapter.Status{
		State:      "paused",
		PositionMS: 4200,
		DurationMS: 54000,
		Title:      "Interstellar",
	}, st)
}

func TestIpcStatus_PlayingWhenNotPaused(t *testing.T) {
	f := newFakeMpv(t)
	f.setProp("idle-active", false)
	f.setProp("eof-reached", false)
	f.setProp("pause", false)
	f.setProp("time-pos", 42.5)
	f.setProp("duration", 90.0)
	ctl := newTestController(t, f)

	st := ctl.Status()
	assert.Equal(t, "playing", st.State)
	assert.Equal(t, int64(42500), st.PositionMS)
	assert.Equal(t, int64(90000), st.DurationMS)
}

func TestIpcStatus_IdleActiveZerosAll(t *testing.T) {
	f := newFakeMpv(t)
	f.setProp("idle-active", true)
	ctl := newTestController(t, f)

	assert.Equal(t, adapter.Status{State: "idle"}, ctl.Status())
}

func TestIpcStatus_EofReachedMapsToIdle(t *testing.T) {
	f := newFakeMpv(t)
	f.setProp("idle-active", false)
	f.setProp("eof-reached", true)
	f.setProp("pause", false)
	f.setProp("time-pos", 30.0)
	f.setProp("duration", 30.0)
	ctl := newTestController(t, f)

	assert.Equal(t, adapter.Status{State: "idle"}, ctl.Status())
}

func TestIpcStatus_PropUnavailableYieldsZero(t *testing.T) {
	f := newFakeMpv(t)
	f.setProp("idle-active", false)
	f.setProp("eof-reached", false)
	f.setProp("pause", false)
	// time-pos/duration/media-title 均未预置 → property unavailable
	ctl := newTestController(t, f)

	st := ctl.Status()
	assert.Equal(t, "playing", st.State)
	assert.Zero(t, st.PositionMS)
	assert.Zero(t, st.DurationMS)
	assert.Empty(t, st.Title)
}

func TestIpcStatus_SecondRoundingToMs(t *testing.T) {
	f := newFakeMpv(t)
	f.setProp("idle-active", false)
	f.setProp("eof-reached", false)
	f.setProp("pause", false)
	f.setProp("time-pos", 1.2345)
	f.setProp("duration", 0.5)
	ctl := newTestController(t, f)

	st := ctl.Status()
	assert.Equal(t, int64(1235), st.PositionMS) // 四舍五入到毫秒
	assert.Equal(t, int64(500), st.DurationMS)
}

func TestNewMpvController_MissingBinary_ActionableError(t *testing.T) {
	_, err := NewMpvController("/no/such/mpv-binary")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mpv not found")
	assert.Contains(t, err.Error(), "brew install mpv")
	assert.Contains(t, err.Error(), "apt install mpv")
}

// TestMpvIntegration_Smoke 是唯一打真实 mpv 的集成测试：本机无 mpv 时跳过
// （exec.LookPath → t.Skip），有则验证进程启动、IPC socket 就绪、idle 态
// 读取与命令通路（volume/stop），覆盖 fake 脚本无法替代的进程装配路径。
func TestMpvIntegration_Smoke(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv 未安装，跳过真实进程集成测试")
	}

	ctl, err := NewMpvController("mpv")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctl.Close() })

	// --idle=yes 启动后应处于 idle 态（无媒体）
	deadline := time.Now().Add(10 * time.Second)
	var st adapter.Status
	for {
		st = ctl.Status()
		if st.State == "idle" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mpv 启动后应为 idle，10s 内始终为 %+v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}

	require.NoError(t, ctl.Volume(50))
	require.NoError(t, ctl.Stop())
}
