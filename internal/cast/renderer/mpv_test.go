package renderer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
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

// armStartConfirm 把 fake 预置成"已起播"形态（time-pos>0 即 started）：Load 的
// 起播确认轮询读到即返回。供组帧单测在下发路径上叠加确认通路。
func armStartConfirm(f *fakeMpv) {
	f.setProp("time-pos", 1.5)
}

// payloadCommands 过滤出组帧断言关心的载荷命令（loadfile/set/seek/stop…），
// 剔除起播确认轮询产生的 get_property 探测帧。
func payloadCommands(f *fakeMpv) [][]any {
	var out [][]any
	for _, cmd := range f.commands() {
		if len(cmd) > 0 && cmd[0] == "get_property" {
			continue
		}
		out = append(out, cmd)
	}
	return out
}

func TestIpcLoad_SendsLoadfileThenTitle(t *testing.T) {
	f := newFakeMpv(t)
	armStartConfirm(f)
	ctl := newTestController(t, f)

	require.NoError(t, ctl.Load(context.Background(), "http://192.168.1.10:7810/media/abc123", "星际穿越", 0))

	// 起播确认发生在下发之后：确有 time-pos 探测（fire-and-forget 旧形态没有）
	assert.Contains(t, f.commands(), []any{"get_property", "time-pos"})

	cmds := payloadCommands(f)
	require.Len(t, cmds, 2)
	assert.Equal(t, []any{"loadfile", "http://192.168.1.10:7810/media/abc123", "replace"}, cmds[0])
	assert.Equal(t, []any{"set", "force-media-title", "星际穿越"}, cmds[1])
}

func TestIpcLoad_EmptyTitleSkipsTitleSet(t *testing.T) {
	f := newFakeMpv(t)
	armStartConfirm(f)
	ctl := newTestController(t, f)

	require.NoError(t, ctl.Load(context.Background(), "http://x/a.mp4", "", 0))

	// 位置 0：plain 3 元素 loadfile，无 start 选项，无标题覆写
	cmds := payloadCommands(f)
	require.Len(t, cmds, 1)
	assert.Equal(t, []any{"loadfile", "http://x/a.mp4", "replace"}, cmds[0])
}

func TestIpcLoad_ResumePosition_FoldsIntoStartOption(t *testing.T) {
	f := newFakeMpv(t)
	armStartConfirm(f)
	ctl := newTestController(t, f)

	// 12500ms → 恰好 5 元素：["loadfile", url, "replace", "-1", "start=+12.5"]
	//（mpv ≥0.38 的 loadfile 第 4 个位置参数是插入 index 而非选项——本机
	// 0.41 实测 4-arg "start=..." 直接报 invalid parameter——选项须作第 5
	// 参数，index 用 -1 追加；loadfile 异步生效，续播位置必须折进 load，
	// 不能 load 后补 seek）
	require.NoError(t, ctl.Load(context.Background(), "http://x/a.mp4", "星际穿越", 12500))

	cmds := payloadCommands(f)
	require.Len(t, cmds, 2, "start 折进 loadfile，不应另发 seek")
	assert.Equal(t, []any{"loadfile", "http://x/a.mp4", "replace", "-1", "start=+12.5"}, cmds[0])
	assert.Equal(t, []any{"set", "force-media-title", "星际穿越"}, cmds[1])
}

func TestIpcLoad_ResumePosition_SubSecondMs(t *testing.T) {
	f := newFakeMpv(t)
	armStartConfirm(f)
	ctl := newTestController(t, f)

	// 非整百毫秒：-1 精度浮点格式化保留全部毫秒位（90250 → "start=+90.25"）
	require.NoError(t, ctl.Load(context.Background(), "http://x/a.mp4", "", 90250))

	cmds := payloadCommands(f)
	require.Len(t, cmds, 1)
	assert.Equal(t, []any{"loadfile", "http://x/a.mp4", "replace", "-1", "start=+90.25"}, cmds[0])
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

func TestIpcStatus_DeadIPC_ReportsIdle(t *testing.T) {
	f := newFakeMpv(t)
	f.setProp("idle-active", false)
	f.setProp("eof-reached", false)
	f.setProp("pause", false)
	f.setProp("time-pos", 30.0)
	ctl := newTestController(t, f)

	// 模拟 mpv 死亡：IPC socket 不再 accept（dial 失败）。此前会被吞成
	// 零值 playing（僵尸假活），现应告警并上报 idle。
	require.NoError(t, f.ln.Close())

	assert.Equal(t, adapter.Status{State: "idle"}, ctl.Status())
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

	// Close 幂等：reaper 独占 cmd.Wait，重复 Close 不二次 Wait、不报错
	require.NoError(t, ctl.Close())
	require.NoError(t, ctl.Close())
}

// writeSineWAV 生成 seconds 秒 8kHz/16bit/单声道 440Hz 正弦波 WAV（测试自产
// 可播媒体：零外部依赖、零网络、时长精确），返回绝对路径供真实 mpv 播放。
func writeSineWAV(t *testing.T, seconds int) string {
	t.Helper()
	const sampleRate = 8000
	total := sampleRate * seconds
	pcm := make([]byte, 0, total*2)
	for i := 0; i < total; i++ {
		v := int16(20000 * math.Sin(2*math.Pi*440*float64(i)/sampleRate))
		pcm = append(pcm, byte(v), byte(v>>8))
	}
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+len(pcm)))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))           // fmt 块长度
	binary.Write(&buf, binary.LittleEndian, uint16(1))            // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1))            // 单声道
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))   // 采样率
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2)) // byte rate
	binary.Write(&buf, binary.LittleEndian, uint16(2))            // block align
	binary.Write(&buf, binary.LittleEndian, uint16(16))           // 位深
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)

	path := filepath.Join(t.TempDir(), "sine.wav")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

// TestMpvIntegration_ResumePosition 是真实 mpv 的续播回归——IPC fake 只记录
// 参数帧，发不出 fake 与现实 mpv 的行为漂移：本机 0.41 实测 loadfile 第 4 个
// 位置参数是插入 index，4-arg 的 "start=..." 选项直接报 invalid parameter，
// 只有真进程能守住（选项须作第 5 参数，index 用 -1）。无头启动
// （--vo=null --ao=null）保持测试不侵入桌面；断言位置落在 start(30s)+~1s。
func TestMpvIntegration_ResumePosition(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv 未安装，跳过真实进程集成测试")
	}

	media := writeSineWAV(t, 60)
	ctl, err := newMpvController("mpv", "--vo=null", "--ao=null")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctl.Close() })

	// 4-arg 旧形态下 mpv 对该 loadfile 回 invalid parameter → 此处 NoError 即 RED
	require.NoError(t, ctl.Load(context.Background(), media, "正弦波", 30000))

	// loadfile 异步生效：3s 内 time-pos 应从 start≈30s 起播并推进。
	deadline := time.Now().Add(3 * time.Second)
	var last adapter.Status
	for {
		last = ctl.Status()
		if last.PositionMS >= 25000 && last.PositionMS <= 40000 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("续播位置应落在 [25000,40000]ms，3s 内始终为 %+v", last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestMpvIntegration_LoadBadSource_HonestError 是"起播确认"的端到端回归
// （线上事故：agent 脑补了不存在的 NAS 路径，loadfile 对不可播源照样回
// success，渲染端 /play 谎报 {"ok":true,"state":"playing"}，mpv 异步加载
// 失败后静默回 idle——调用方从此以为在播）。无头启动（--vo=null --ao=null）。
// 契约：
//   - Controller.Load 对不可播源（闭合端口 URL）必须有界（~10s）返回 error；
//   - /play 同源回 {"ok":false,"state":"error"}（HTTP 200，应用层失败），
//     error 态粘滞（/status 恒 error、信息清零）；
//   - 下一次 /play 好源恢复 playing（error 只被下一次 /play 打破）。
//
// 修复前（fire-and-forget）形态下本测试必然失败：loadfile 对闭合端口 URL
// 照样回 success，Load 返回 nil、/play 回 {"ok":true,"state":"playing"}——
// 第 1) 步 require.Error 与第 2) 步 ok=false 断言分别落空（本机实测：闭合
// 端口 loadfile 响应 success，约 5.9s 后 idle-active 翻回 true 而始终无
// time-pos，旧代码对这一切无感知）。
func TestMpvIntegration_LoadBadSource_HonestError(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv 未安装，跳过真实进程集成测试")
	}

	ctl, err := newMpvController("mpv", "--vo=null", "--ao=null")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctl.Close() })

	const badURL = "http://127.0.0.1:1/x.mp4" // 闭合端口：连接必被拒

	// 1) Controller 层：不可播源必须报错，且有界返回
	start := time.Now()
	err = ctl.Load(context.Background(), badURL, "幻觉片源", 0)
	elapsed := time.Since(start)
	require.Error(t, err, "不可播源 loadfile 异步失败，Load 不得谎报成功")
	t.Logf("不可播源 Load 在 %v 后返回: %v", elapsed.Round(time.Millisecond), err)
	assert.LessOrEqual(t, elapsed, 12*time.Second, "起播确认应在有界窗口内返回")

	// 2) Server 层：/play 同源 → ok=false + state=error，且 error 粘滞
	srv := NewServer("s3cret", "卧室", "test-renderer", 0, ctl)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	code, body := rawDo(t, ts.URL, http.MethodPost, "/play", "s3cret", `{"url":"`+badURL+`"}`)
	require.Equal(t, http.StatusOK, code)
	var resp cmdResp
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	assert.False(t, resp.OK, "不可播源 /play 不得回 ok=true（旧形态即败于此）")
	assert.Equal(t, stateError, resp.State)
	assert.NotEmpty(t, resp.Error)

	code, body = rawDo(t, ts.URL, http.MethodGet, "/status", "s3cret", "")
	require.Equal(t, http.StatusOK, code)
	var st statusResp
	require.NoError(t, json.Unmarshal([]byte(body), &st))
	assert.Equal(t, stateError, st.State, "error 态粘滞：/status 恒 error")
	assert.NotEmpty(t, st.Error)
	assert.Zero(t, st.PositionMS)
	assert.Zero(t, st.DurationMS)

	// 3) 恢复：error 只被下一次 /play（好源）打破——且这次 /play 是真的在播
	media := writeSineWAV(t, 60)
	code, body = rawDo(t, ts.URL, http.MethodPost, "/play", "s3cret",
		`{"url":"`+media+`","title":"正弦波"}`)
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.True(t, resp.OK, "好源 /play 应回 ok=true，body=%s", body)
	assert.Equal(t, statePlaying, resp.State)

	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body = rawDo(t, ts.URL, http.MethodGet, "/status", "s3cret", "")
		require.Equal(t, http.StatusOK, code)
		require.NoError(t, json.Unmarshal([]byte(body), &st))
		if st.State == statePlaying && st.DurationMS == 60000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("好源应恢复 playing 且时长 60000ms，5s 内始终为 %+v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestMpvIntegration_RespawnAfterQuit 是真实 mpv 的"用户按 q 退出后必须能
// 复活"回归（线上事故：mpv 进程一生只拉起一次，用户 q 退出后 socket 拒连，
// 此后每个 /play 永久失败，渲染端"变砖"直到整个进程重启）。无头启动
// （--vo=null --ao=null）。契约：
//   - 外部杀死进程后，Status 恒 idle（只读路径不触发复活）；
//   - 下一次 Load 按需重拉全新 mpv epoch（socket/tmpdir 换代），首次即成功；
//   - Close 只杀当前 epoch、清当前 tmpdir（reaper 独占 Wait 不受换代影响）。
func TestMpvIntegration_RespawnAfterQuit(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv 未安装，跳过真实进程集成测试")
	}

	media := writeSineWAV(t, 60)
	ctl, err := newMpvController("mpv", "--vo=null", "--ao=null")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctl.Close() })

	// 初始 epoch 播放正常
	require.NoError(t, ctl.Load(context.Background(), media, "正弦波", 0))

	ctl.mu.Lock()
	oldDir := ctl.tmpDir
	cmd, done := ctl.cmd, ctl.reapDone
	ctl.mu.Unlock()
	require.NotNil(t, cmd)
	require.NotEmpty(t, oldDir)

	// 模拟用户 q / 崩溃：外部杀死进程，等 reaper 观察到退出
	require.NoError(t, cmd.Process.Kill())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper 5s 内未观察到 mpv 退出")
	}

	// 死亡期间 Status 恒 idle（只读路径，不得复活）
	st := ctl.Status()
	assert.Equal(t, adapter.Status{State: "idle"}, st)

	// 复活入口：退出后的第一次 Load 必须成功，且落在全新 epoch 上
	require.NoError(t, ctl.Load(context.Background(), media, "正弦波", 0))

	ctl.mu.Lock()
	newDir := ctl.tmpDir
	ctl.mu.Unlock()
	assert.NotEqual(t, oldDir, newDir, "重生必须换新 tmpdir/socket epoch")

	// 新 epoch 真的在播：媒体已加载（时长即自产 WAV 的精确 60s）
	deadline := time.Now().Add(3 * time.Second)
	for {
		st = ctl.Status()
		if st.State == "playing" && st.DurationMS == 60000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("重生后应能播放自产媒体，3s 内始终为 %+v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Close 仍只杀当前 epoch（不与 reaper 双重 Wait、不碰已清理的旧目录）
	require.NoError(t, ctl.Close())
	ctl.mu.Lock()
	dirAfterClose := ctl.tmpDir
	ctl.mu.Unlock()
	assert.Empty(t, dirAfterClose, "Close 应清空当前 epoch 的 tmpdir")
}
