package manager

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dayanio/lattice-cast/internal/cast/adapter"
	"github.com/dayanio/lattice-cast/internal/cast/config"
	"github.com/dayanio/lattice-cast/internal/cast/resolve"
	"github.com/dayanio/lattice-cast/internal/cast/testsupport/fakerenderer"
)

// 测试设备常量：key = mDNS 实例名 = 设备身份。
const (
	devName = "living-room-display"
	devRoom = "living-room"
	devTok  = "tok-lr"
)

// newFakeCfg 构造一台静态渲染端配置：host/port 直接指向 Task 5 Fake 渲染端
// （静态条目使 discovery.Browse 不依赖组播，CI -short 必跑必过）。
func newFakeCfg(f *fakerenderer.Fake) config.Config {
	tgt := f.Target()
	return config.Config{
		AuthToken:    "tok-mcp",
		MediaBaseURL: "http://192.168.1.10:7810",
		Renderers: map[string]config.Renderer{
			devName: {Room: devRoom, Host: tgt.Host, Port: tgt.Port, Token: devTok},
		},
	}
}

// newManager 构造被测 Manager；lib/res 仅满足 New 签名（本包操作不消费它们）。
func newManager(t *testing.T, cfg config.Config) *Manager {
	t.Helper()
	lib := resolve.NewLibrary(nil)
	res := &resolve.Resolver{Lib: lib, Base: cfg.MediaBaseURL}
	return New(cfg, lib, res)
}

// ---- List ----

// TestList_OnlineDevice 在线设备：List 对静态配置的 Fake 渲染端逐台探测，
// 返回 1 台在线设备且 state / now_playing 来自 GET /status。
func TestList_OnlineDevice(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()
	fake.SetPlaying("http://192.168.1.10:7810/media/abc", "夜曲", 180000)

	m := newManager(t, newFakeCfg(fake))

	devices, err := m.List(context.Background())
	require.NoError(t, err)
	require.Len(t, devices, 1, "静态配置只有一台设备")

	got := devices[0]
	assert.Equal(t, devName, got.Name)
	assert.Equal(t, devRoom, got.Room, "Room 来自配置")
	assert.True(t, got.Online, "Fake 在线，应标记 online=true")
	assert.Equal(t, "playing", got.State)
	assert.Equal(t, "夜曲", got.NowPlaying, "now_playing 应来自 /status 的 title")
}

// TestList_OfflineAfterClose 离线标记：停掉 Fake 的 http.Server 后，
// List 仍返回该设备（身份在配置里）但 online=false 且 state/now_playing 为空。
func TestList_OfflineAfterClose(t *testing.T) {
	fake := fakerenderer.New(devTok)
	fake.Close() // 先关再测：连接被拒 → 判离线

	m := newManager(t, newFakeCfg(fake))

	devices, err := m.List(context.Background())
	require.NoError(t, err)
	require.Len(t, devices, 1, "离线设备仍在配置中，不得从清单消失")

	got := devices[0]
	assert.Equal(t, devName, got.Name)
	assert.False(t, got.Online, "服务已停，应标记 online=false")
	assert.Empty(t, got.State, "离线时 state 须为空")
	assert.Empty(t, got.NowPlaying, "离线时 now_playing 须为空")
}

// TestList_AnsweringButErroring 配置 token 不符（渲染端回 401 unauthorized）：
// 渲染端应答了——设备在线但异常，须标 online=true + state="error"，
// 不得误报离线（否则 LLM 会把配置问题当成"电视不在线"）。
func TestList_AnsweringButErroring(t *testing.T) {
	fake := fakerenderer.New("right-token")
	defer fake.Close()

	cfg := newFakeCfg(fake) // 管理器侧故意配错 token
	r := cfg.Renderers[devName]
	r.Token = "wrong-token"
	cfg.Renderers[devName] = r

	m := newManager(t, cfg)

	devices, err := m.List(context.Background())
	require.NoError(t, err)
	require.Len(t, devices, 1)

	got := devices[0]
	assert.Equal(t, devName, got.Name)
	assert.Equal(t, devRoom, got.Room, "Room 来自配置")
	assert.True(t, got.Online, "渲染端应答了（401），设备在线")
	assert.Equal(t, "error", got.State, "应答但异常 → state=error")
	assert.Empty(t, got.NowPlaying, "异常时 now_playing 须为空")
}

// ---- Play / Stop / Status ----

// TestPlay_RoutesToDevice Play 正确路由：Manager 按设备名查当前 host/port/token
// 构造 Target 调协议客户端，Fake 进入 playing；随后 Status 可读到状态。
func TestPlay_RoutesToDevice(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()

	m := newManager(t, newFakeCfg(fake))

	st, err := m.Play(context.Background(), devName, adapter.PlayRequest{
		URL:        "http://192.168.1.10:7810/media/abc",
		Title:      "夜曲",
		PositionMS: 0,
	})
	require.NoError(t, err)
	assert.Equal(t, "playing", st.State, "play 响应应回执 playing")

	st, err = m.Status(context.Background(), devName)
	require.NoError(t, err)
	assert.Equal(t, "playing", st.State)
	assert.Equal(t, "夜曲", st.Title, "Play 须真的下发到该 Fake")
}

// TestStop_IdlesDevice Stop 停止并清空媒体：playing → idle。
func TestStop_IdlesDevice(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()
	fake.SetPlaying("http://x/y.mp3", "曲子", 1000)

	m := newManager(t, newFakeCfg(fake))

	st, err := m.Stop(context.Background(), devName)
	require.NoError(t, err)
	assert.Equal(t, "idle", st.State)

	st, err = m.Status(context.Background(), devName)
	require.NoError(t, err)
	assert.Equal(t, "idle", st.State)
	assert.Empty(t, st.Title, "stop 后媒体信息应清空")
}

// TestPause_PausesDevice Pause 暂停播放：playing → paused，媒体信息保留
// （与 stop 不同，pause 不清标题）。
func TestPause_PausesDevice(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()
	fake.SetPlaying("http://x/y.mp3", "曲子", 1000)

	m := newManager(t, newFakeCfg(fake))

	st, err := m.Pause(context.Background(), devName)
	require.NoError(t, err)
	assert.Equal(t, "paused", st.State, "pause 响应应回执 paused")

	st, err = m.Status(context.Background(), devName)
	require.NoError(t, err)
	assert.Equal(t, "paused", st.State)
	assert.Equal(t, "曲子", st.Title, "pause 后媒体标题应保留")
}

// TestSeek_RoutesToDevice Seek 跳转位置：playing 态下位置生效且状态不变。
func TestSeek_RoutesToDevice(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()
	fake.SetPlaying("http://x/y.mp3", "曲子", 1000)

	m := newManager(t, newFakeCfg(fake))

	st, err := m.Seek(context.Background(), devName, 90000)
	require.NoError(t, err)
	assert.Equal(t, "playing", st.State, "seek 不改变播放状态")

	st, err = m.Status(context.Background(), devName)
	require.NoError(t, err)
	assert.Equal(t, int64(90000), st.PositionMS, "Seek 须真的下发到该 Fake")
	assert.Equal(t, "playing", st.State)
}

// ---- 错误路径 ----

// TestOps_UnknownDevice 未知设备名：Play/Pause/Stop/Seek/Volume/Status 一律报
// unknown_device。
func TestOps_UnknownDevice(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()

	m := newManager(t, newFakeCfg(fake))
	ctx := context.Background()

	_, err := m.Play(ctx, "no-such-device", adapter.PlayRequest{URL: "http://x/y.mp3"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown_device")

	_, err = m.Pause(ctx, "no-such-device")
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown_device")

	_, err = m.Stop(ctx, "no-such-device")
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown_device")

	_, err = m.Seek(ctx, "no-such-device", 1000)
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown_device")

	_, err = m.Volume(ctx, "no-such-device", 30)
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown_device")

	_, err = m.Status(ctx, "no-such-device")
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown_device")
}

// TestVolume_OutOfRange 音量越界：<0 或 >100 拒绝并报 level_out_of_range；
// 边界 0 与 100 合法放行。
func TestVolume_OutOfRange(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()

	m := newManager(t, newFakeCfg(fake))
	ctx := context.Background()

	for _, level := range []int{-1, 101, 255} {
		_, err := m.Volume(ctx, devName, level)
		require.Error(t, err, "level=%d 应被拒绝", level)
		assert.ErrorContains(t, err, "level_out_of_range")
	}

	for _, level := range []int{0, 50, 100} {
		st, err := m.Volume(ctx, devName, level)
		require.NoError(t, err, "level=%d 应放行", level)
		assert.Equal(t, "idle", st.State, "volume 不改变播放状态")
	}
}

// TestPlay_DeviceOffline 网络失败须包上 device_offline 前缀供 MCP 层甄别，
// 且底层 *url.Error 仍可 errors.As 取回。
func TestPlay_DeviceOffline(t *testing.T) {
	fake := fakerenderer.New(devTok)
	fake.Close()

	m := newManager(t, newFakeCfg(fake))

	_, err := m.Play(context.Background(), devName, adapter.PlayRequest{URL: "http://x/y.mp3"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "device_offline")

	var ue *url.Error
	assert.ErrorAs(t, err, &ue, "底层 *url.Error 应可解包")
}

// TestOps_UnaddressedDevice 配置条目无 host（仅组播）且尚未发现：
// 无从寻址，按离线处理（device_offline），不得panic或空指针。
func TestOps_UnaddressedDevice(t *testing.T) {
	cfg := config.Config{
		AuthToken:    "tok-mcp",
		MediaBaseURL: "http://192.168.1.10:7810",
		Renderers: map[string]config.Renderer{
			devName: {Room: devRoom, Token: devTok}, // 无 host：仅 mDNS 发现
		},
	}
	m := newManager(t, cfg)

	_, err := m.Status(context.Background(), devName)
	require.Error(t, err)
	assert.ErrorContains(t, err, "device_offline")
}

// ---- Refresh ----

// TestRefresh_StoresDiscoveryOutput Refresh 走真实 discovery.Browse（静态条目
// 不依赖组播）：发现结果按 Found.Name 逐字并入注册表——host/port 取发现输出，
// token 仍取自配置（发现结果不含 token）。
func TestRefresh_StoresDiscoveryOutput(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()

	m := newManager(t, newFakeCfg(fake))

	// 静态路径不依赖组播：收窄 Browse 窗口让两种环境下都快。
	rctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	m.Refresh(rctx)

	m.mu.Lock()
	d, ok := m.devs[devName]
	m.mu.Unlock()
	require.True(t, ok, "Refresh 后设备应在注册表")

	tgt := fake.Target()
	assert.Equal(t, tgt.Host, d.Host)
	assert.Equal(t, tgt.Port, d.Port)
	assert.Equal(t, devTok, d.Token, "token 来自配置")
	assert.Equal(t, devRoom, d.Room)
}

// ---- 审计 ----

// TestOpenAudit_CreatesFilePerm0600 OpenAudit 创建/追加打开审计文件，权限 0600。
func TestOpenAudit_CreatesFilePerm0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	al, err := OpenAudit(path)
	require.NoError(t, err)
	require.NotNil(t, al)
	t.Cleanup(func() { _ = al.Close() })

	info, err := os.Stat(path)
	require.NoError(t, err, "OpenAudit 应创建文件")
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

// TestRecord_WritesJSONLWithAllFields 每条 Record 恰好追加一行合法 JSON，
// 七个字段齐备；重开同一文件为追加而非截断。
func TestRecord_WritesJSONLWithAllFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	al, err := OpenAudit(path)
	require.NoError(t, err)
	before := time.Now()
	al.Record("claude", "play", `{"device":"living-room-display"}`, `{"state":"playing"}`,
		true, 1500*time.Millisecond)
	require.NoError(t, al.Close())

	// 追加语义：重开同一文件再记一条，原行不得丢失。
	al2, err := OpenAudit(path)
	require.NoError(t, err)
	al2.Record("claude", "volume", `{"device":"living-room-display","level":101}`,
		"level_out_of_range: 101", false, 2*time.Millisecond)
	require.NoError(t, al2.Close())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	require.Len(t, lines, 2, "两次 Record 各一行 JSONL")

	// 第一行：字段齐备 + 值正确。
	var first map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first), "行须为合法 JSON: %s", lines[0])
	for _, k := range []string{"ts", "agent", "tool", "args", "result", "ok", "duration_ms"} {
		require.Contains(t, first, k, "缺少字段 %s", k)
	}
	assert.Equal(t, "claude", first["agent"])
	assert.Equal(t, "play", first["tool"])
	assert.Equal(t, `{"device":"living-room-display"}`, first["args"])
	assert.Equal(t, `{"state":"playing"}`, first["result"])
	assert.Equal(t, true, first["ok"])
	assert.Equal(t, float64(1500), first["duration_ms"])

	var e AuditEntry
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &e))
	assert.WithinDuration(t, before, e.TS, 5*time.Second, "ts 应为记录时刻")

	// 第二行：失败样例 + 追加生效。
	var second AuditEntry
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &second))
	assert.Equal(t, "volume", second.Tool)
	assert.False(t, second.OK)
	assert.Contains(t, second.Result, "level_out_of_range")
	assert.Equal(t, int64(2), second.DurationMS)
}

// TestRecord_NeverPanicsAfterClose Record 对 I/O 错误绝不 panic（关闭后写入静默降级）。
func TestRecord_NeverPanicsAfterClose(t *testing.T) {
	al, err := OpenAudit(filepath.Join(t.TempDir(), "audit.jsonl"))
	require.NoError(t, err)
	require.NoError(t, al.Close())

	assert.NotPanics(t, func() {
		al.Record("agent", "tool", "args", "result", true, time.Millisecond)
	})
}

// ---- Device JSON 形状（Task 11 MCP 序列化契约）----

// TestDevice_JSONShape Device 的 JSON tag 锁定：snake_case 键 + omitempty 生效。
func TestDevice_JSONShape(t *testing.T) {
	b, err := json.Marshal(Device{Name: "d", Room: "r", Online: true, NowPlaying: "s", State: "playing"})
	require.NoError(t, err)
	var full map[string]any
	require.NoError(t, json.Unmarshal(b, &full))
	for _, k := range []string{"name", "room", "online", "now_playing", "state"} {
		require.Contains(t, full, k)
	}

	b, err = json.Marshal(Device{Name: "d", Room: "r"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "now_playing", "空 now_playing 应被 omitempty")
	assert.NotContains(t, string(b), "state", "空 state 应被 omitempty")
	assert.Contains(t, string(b), `"online":false`, "online 无 omitempty，恒出现")
}

// ---- 断点记忆（Task 26：意图快通道的「继续」）----

// TestPauseRecordsBreakpoint_ResumeReplays 断点续播全链：Play（带 url/标题）
// → Seek 到 90 秒 → Pause 记录断点 → Resume 以同 URL 同位置重新起播（渲染端
// 收到的拉流地址与位置逐一断言）。
func TestPauseRecordsBreakpoint_ResumeReplays(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()
	m := newManager(t, newFakeCfg(fake))
	ctx := context.Background()

	const media = "http://192.168.1.10:7810/media/santi.mp4"
	_, err := m.Play(ctx, devName, adapter.PlayRequest{URL: media, Title: "三体"})
	require.NoError(t, err)
	_, err = m.Seek(ctx, devName, 90000)
	require.NoError(t, err)
	_, err = m.Pause(ctx, devName)
	require.NoError(t, err)

	st, err := m.Resume(ctx, devName)
	require.NoError(t, err)
	assert.Equal(t, "playing", st.State)
	assert.Equal(t, media, fake.PlayedURL(), "Resume 应以断点同 URL 重新起播")

	got, err := m.Status(ctx, devName)
	require.NoError(t, err)
	assert.Equal(t, int64(90000), got.PositionMS, "Resume 应从断点位置起播")
	assert.Equal(t, "三体", got.Title, "Resume 应带上断点标题")
}

// TestResumeWithoutBreakpoint 无断点（从未 Pause 过）→ 报 no_last_played，
// 且不发任何网络请求（渲染端关闭也不影响错误类型）。
func TestResumeWithoutBreakpoint(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()
	m := newManager(t, newFakeCfg(fake))

	_, err := m.Resume(context.Background(), devName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no_last_played")
}

// TestResumeUnknownDevice 未知设备 → 既有 unknown_device 契约优先。
func TestResumeUnknownDevice(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()
	m := newManager(t, newFakeCfg(fake))

	_, err := m.Resume(context.Background(), "no-such")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown_device")
}

// TestPauseWithoutPlayMemo 不记无源断点：渲染端在播但本进程没有 Play 入参
// 记忆（如 agent 重启后接手）时，Pause 拿不到 url → 不记断点，Resume 报
// no_last_played（诚实拒绝优于瞎猜 url）。
func TestPauseWithoutPlayMemo(t *testing.T) {
	fake := fakerenderer.New(devTok)
	defer fake.Close()
	fake.SetPlaying("http://192.168.1.10:7810/media/x.mp4", "旧会话的片子", 60000)
	m := newManager(t, newFakeCfg(fake))
	ctx := context.Background()

	_, err := m.Pause(ctx, devName)
	require.NoError(t, err)
	_, err = m.Resume(ctx, devName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no_last_played")
}

// TestResumeOffline 断点在但设备失联 → 既有 device_offline 契约。
func TestResumeOffline(t *testing.T) {
	fake := fakerenderer.New(devTok)
	m := newManager(t, newFakeCfg(fake))
	ctx := context.Background()

	_, err := m.Play(ctx, devName, adapter.PlayRequest{URL: "http://m/a.mp4", Title: "a"})
	require.NoError(t, err)
	_, err = m.Pause(ctx, devName)
	require.NoError(t, err)

	fake.Close() // 断点已记，随后设备离线
	_, err = m.Resume(ctx, devName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device_offline")
}
