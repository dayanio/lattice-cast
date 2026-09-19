package renderer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dayanio/lattice-cast/internal/cast/adapter"
	"github.com/dayanio/lattice-cast/internal/cast/adapter/latticecast"
)

const mediaURL = "http://192.168.1.10:7810/media/abc123"

// fixtureDir 定位仓库根下两端共用的契约夹具（protocol.md 第七节）。
var fixtureDir = filepath.Join("..", "..", "..", "testdata", "contract")

// fakeController 是测试用 Controller：内存态记录播放后端状态，可注入
// Load 失败（模拟源不可达 → error 态）与 Pause 失败（模拟命令执行出错）。
// 只供给位置/时长/标题（Server 的 /status 数据源）；播放状态机由 Server 持有。
type fakeController struct {
	mu         sync.Mutex
	url        string
	title      string
	positionMS int64
	durationMS int64
	volume     int
	ctlState   string // 播放后端自报状态：""（缺省，状态机全归 Server）或 stateIdle（模拟 mpv 播完 EOF / 空闲）

	failLoadArmed bool
	failLoadMsg   string
	pauseErr      error
}

func (f *fakeController) Load(_ context.Context, u, title string, positionMS int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failLoadArmed {
		f.failLoadArmed = false
		return errors.New(f.failLoadMsg)
	}
	f.url, f.title = u, title
	f.positionMS = positionMS // 续播位置随 Load 折叠下发（loadfile start）
	return nil
}

func (f *fakeController) Pause() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pauseErr
}

func (f *fakeController) Stop() error { return nil }

func (f *fakeController) SeekTo(ms int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.positionMS = ms
	return nil
}

func (f *fakeController) Volume(level int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volume = level
	return nil
}

// Status 返回播放后端快照：State 缺省为空（状态机归 Server 所有），可注入
// stateIdle 模拟 mpv 播完 EOF（--keep-open 下 eof-reached=true 映射为 idle）；
// 位置/时长/标题供 /status 组装。
func (f *fakeController) Status() adapter.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return adapter.Status{State: f.ctlState, PositionMS: f.positionMS, DurationMS: f.durationMS, Title: f.title}
}

func (f *fakeController) FailNextLoad(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failLoadArmed = true
	f.failLoadMsg = msg
}

func (f *fakeController) setDuration(ms int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.durationMS = ms
}

// setControllerState 注入播放后端自报状态（仅 stateIdle 有观察意义：
// 模拟 mpv 播完 EOF 或空闲；"" 恢复缺省的"状态机全归 Server"）。
func (f *fakeController) setControllerState(state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctlState = state
}

func (f *fakeController) loaded() (u, title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.url, f.title
}

func (f *fakeController) loadedPosition() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.positionMS
}

func (f *fakeController) volumeLevel() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.volume
}

// newTestServer 构造被测 Server 并挂上 httptest 容器（测试结束自动关闭），
// 返回配套的真实协议客户端（外部驱动 Handler()）、服务根地址与指向同一
// 渲染端的 Target。
func newTestServer(t *testing.T, token string) (*fakeController, string, *latticecast.Client, adapter.Target) {
	t.Helper()
	ctl := &fakeController{}
	srv := NewServer(token, "卧室", "test-renderer", 0, ctl)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return ctl, ts.URL, latticecast.NewClient(ts.Client()),
		adapter.Target{Host: u.Hostname(), Port: port, Token: token}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	require.NoError(t, err, "契约夹具 %s 应存在", name)
	return b
}

// rawDo 发一个带 Bearer 头的裸 HTTP 请求，返回状态码与响应体。
func rawDo(t *testing.T, tsURL, method, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, tsURL+path, strings.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

func TestStatus_InitiallyIdle(t *testing.T) {
	_, _, c, tgt := newTestServer(t, "s3cret")
	st, err := c.Status(context.Background(), tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)
}

func TestPlayPauseStop_StateMachine(t *testing.T) {
	ctx := context.Background()
	ctl, _, c, tgt := newTestServer(t, "s3cret")

	// play：idle → playing，url/title 下发到 Controller
	st, err := c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing"}, st)

	u, title := ctl.loaded()
	assert.Equal(t, mediaURL, u)
	assert.Equal(t, "Interstellar", title)

	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing", Title: "Interstellar"}, st)

	// pause：playing → paused，标题保留
	st, err = c.Pause(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused", Title: "Interstellar"}, st)

	// stop：paused → idle，媒体清空
	st, err = c.Stop(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)
}

// TestStatus_ControllerEof_TransitionsToIdle 播放后端播完（EOF，--keep-open
// 下 mpv eof-reached=true → 控制器自报 idle）：/status 须把 Server 状态机一并
// 迁到 idle（进度与标题清零），不得谎报"还在播"；迁移粘滞——此后 /status 恒为
// idle，直到下一次 /play。
func TestStatus_ControllerEof_TransitionsToIdle(t *testing.T) {
	ctx := context.Background()
	ctl, _, c, tgt := newTestServer(t, "s3cret")
	ctl.setDuration(5400000)

	_, err := c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.NoError(t, err)
	st, err := c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing", DurationMS: 5400000, Title: "Interstellar"}, st)

	// 播放后端到达 EOF：控制器自报 idle
	ctl.setControllerState(stateIdle)

	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st, "EOF 后 wire 状态应为 idle（进度与标题清零）")

	// 迁移粘滞：再次 /status 仍 idle，不得回 playing
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)

	// paused 态同理：暂停中播完后端空转也须落到 idle
	ctl.setControllerState("") // 后端恢复正常上报（模拟新的 load 之前）
	_, err = c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.NoError(t, err)
	_, err = c.Pause(ctx, tgt)
	require.NoError(t, err)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	require.Equal(t, "paused", st.State)

	ctl.setControllerState(stateIdle)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st, "paused 下控制器自报 idle 同样迁移")

	// 新一次 /play 显然重置：playing 恢复（Fake 的 durationMS 未清，仍在）
	ctl.setControllerState("")
	_, err = c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.NoError(t, err)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing", DurationMS: 5400000, Title: "Interstellar"}, st)
}

func TestUnauthorized_WrongOrMissingToken(t *testing.T) {
	_, tsURL, c, tgt := newTestServer(t, "s3cret")

	// 线上形态：401 + {"ok":false,"error":"unauthorized"}（含缺 token）
	for _, token := range []string{"", "wrong"} {
		code, body := rawDo(t, tsURL, http.MethodGet, "/status", token, "")
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.JSONEq(t, `{"ok":false,"error":"unauthorized"}`, body)
	}

	// 客户端形态：*ProtocolError("unauthorized")
	bad := tgt
	bad.Token = "wrong"
	_, err := c.Status(context.Background(), bad)
	require.Error(t, err)
	var pe *latticecast.ProtocolError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "unauthorized", pe.Msg)
}

func TestWireErrors(t *testing.T) {
	_, tsURL, _, _ := newTestServer(t, "s3cret")

	tests := []struct {
		name      string
		method    string
		path      string
		body      string
		wantCode  int
		wantError string
	}{
		{"unknown path", http.MethodPost, "/nope", "", http.StatusNotFound, "not_found"},
		{"method not allowed", http.MethodPost, "/status", "", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"get on play", http.MethodGet, "/play", "", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"invalid json body", http.MethodPost, "/play", "{not-json", http.StatusBadRequest, "bad_request"},
		{"missing url", http.MethodPost, "/play", `{"title":"x"}`, http.StatusBadRequest, "bad_request"},
		{"invalid seek body", http.MethodPost, "/seek", "{oops", http.StatusBadRequest, "bad_request"},
		{"invalid volume body", http.MethodPost, "/volume", "{oops", http.StatusBadRequest, "bad_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := rawDo(t, tsURL, tt.method, tt.path, "s3cret", tt.body)
			assert.Equal(t, tt.wantCode, code)
			assert.JSONEq(t, `{"ok":false,"error":"`+tt.wantError+`"}`, body)
		})
	}
}

func TestPlayFailure_ErrorStickyUntilNextPlay(t *testing.T) {
	ctx := context.Background()
	ctl, _, c, tgt := newTestServer(t, "s3cret")

	// 先正常播放并 seek，让旧媒体信息在位（失败后应被清空）
	_, err := c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.NoError(t, err)
	_, err = c.Seek(ctx, tgt, adapter.SeekRequest{PositionMS: 90000})
	require.NoError(t, err)

	// 注入 Load 失败：该次 /play 返回 HTTP 200 + ok=false，客户端收到 *ProtocolError
	ctl.FailNextLoad("source_unreachable")
	st, err := c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.Error(t, err)
	var pe *latticecast.ProtocolError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "source_unreachable", pe.Msg)
	assert.Equal(t, "/play", pe.Op)
	assert.Equal(t, adapter.Status{}, st)

	// 渲染端进入 error 态（旧媒体信息清零），且粘滞
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "error", Error: "source_unreachable"}, st)

	// error 态下 /stop /pause /seek /volume 均不产生迁移（protocol.md 第六节）
	st, err = c.Stop(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "error"}, st)
	st, err = c.Pause(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "error"}, st)
	st, err = c.Seek(ctx, tgt, adapter.SeekRequest{PositionMS: 1000})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "error"}, st)
	st, err = c.Volume(ctx, tgt, adapter.VolumeRequest{Level: 10})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "error"}, st)

	// 离开 error 的唯一方式：下一次 /play 成功
	st, err = c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing", Title: "Interstellar"}, st)
}

func TestPlayFailure_ResponseIs200WithOkFalse(t *testing.T) {
	ctl, tsURL, _, _ := newTestServer(t, "s3cret")
	ctl.FailNextLoad("source_unreachable")
	code, body := rawDo(t, tsURL, http.MethodPost, "/play", "s3cret", `{"url":"http://x/y.mp4"}`)
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, `{"ok":false,"state":"error","error":"source_unreachable"}`, body)
}

func TestControllerCommandFailure_ReportsOkFalseWithoutMigration(t *testing.T) {
	ctx := context.Background()
	ctl, _, c, tgt := newTestServer(t, "s3cret")

	_, err := c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.NoError(t, err)

	// Pause 命令执行失败：HTTP 200 + ok=false，播放状态不迁移
	ctl.pauseErr = errors.New("ipc down")
	_, err = c.Pause(ctx, tgt)
	require.Error(t, err)
	var pe *latticecast.ProtocolError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "ipc down", pe.Msg)

	ctl.pauseErr = nil
	st, err := c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing", Title: "Interstellar"}, st)
}

func TestIdleCommands_IdempotentNoMigration(t *testing.T) {
	ctx := context.Background()
	ctl, _, c, tgt := newTestServer(t, "s3cret")

	// idle 下 pause/stop/seek/volume 幂等：不报错、不迁移、不下发 Controller
	st, err := c.Pause(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)
	st, err = c.Stop(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)
	st, err = c.Seek(ctx, tgt, adapter.SeekRequest{PositionMS: 5000})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)
	st, err = c.Volume(ctx, tgt, adapter.VolumeRequest{Level: 30})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)

	u, title := ctl.loaded()
	assert.Empty(t, u)
	assert.Empty(t, title)
	assert.Zero(t, ctl.volumeLevel())
}

func TestSeekVolume_PositionFromController(t *testing.T) {
	ctx := context.Background()
	ctl, _, c, tgt := newTestServer(t, "s3cret")
	ctl.setDuration(5400000)

	_, err := c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.NoError(t, err)

	// seek → 状态不变，进度来自 Controller.Status()
	st, err := c.Seek(ctx, tgt, adapter.SeekRequest{PositionMS: 90000})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing", PositionMS: 90000, DurationMS: 5400000, Title: "Interstellar"}, st)

	// volume → 状态与进度均不变
	st, err = c.Volume(ctx, tgt, adapter.VolumeRequest{Level: 42})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing", PositionMS: 90000, DurationMS: 5400000, Title: "Interstellar"}, st)
}

func TestPlay_ResumePosition_FoldedIntoLoad(t *testing.T) {
	ctx := context.Background()
	ctl, _, c, tgt := newTestServer(t, "s3cret")
	ctl.setDuration(5400000)

	// 携 position_ms 的 /play：位置应随 Load 一起下发（渲染端折进 loadfile
	// start 起播），而非 Load 后补发 seek——真实 mpv 的 loadfile 异步生效，
	// 立即 seek 会被打在加载中的文件上而静默失效。
	_, err := c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar", PositionMS: 30000})
	require.NoError(t, err)
	assert.Equal(t, int64(30000), ctl.loadedPosition())
	st, err := c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, int64(30000), st.PositionMS)

	// 不携 position_ms：位置 0（从头起播）
	_, err = c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.NoError(t, err)
	assert.Equal(t, int64(0), ctl.loadedPosition())
}

// TestClientConformance 是渲染端一致性套件：真实 latticecast.Client 外部驱动
// Server.Handler()，全序列 play→status→pause→seek→volume→status→stop，
// 断言与 fakerenderer（internal/cast/testsupport/fakerenderer）同一调用序列
// 的行为一致——渲染端产品实现与测试替身不漂移。
func TestClientConformance(t *testing.T) {
	ctx := context.Background()
	ctl, _, c, tgt := newTestServer(t, "s3cret")
	ctl.setDuration(5400000)

	// 初始：idle
	st, err := c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)

	// play → playing
	st, err = c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar", PositionMS: 0})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing", DurationMS: 5400000, Title: "Interstellar"}, st)

	// pause → paused（进度保留）
	st, err = c.Pause(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused", DurationMS: 5400000, Title: "Interstellar"}, st)

	// seek → 状态不变，进度生效
	st, err = c.Seek(ctx, tgt, adapter.SeekRequest{PositionMS: 90000})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused", PositionMS: 90000, DurationMS: 5400000, Title: "Interstellar"}, st)

	// volume → 状态与进度均不变
	st, err = c.Volume(ctx, tgt, adapter.VolumeRequest{Level: 42})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused", PositionMS: 90000, DurationMS: 5400000, Title: "Interstellar"}, st)

	// stop → idle，媒体清空
	st, err = c.Stop(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)
}

// TestContractFixtures 直接以 testdata/contract/ 夹具为请求/期望（protocol.md
// 第七节）：渲染端产品实现与 Android 渲染端共用同一基准，保证两端不漂移。
func TestContractFixtures(t *testing.T) {
	ctl, tsURL, c, tgt := newTestServer(t, "s3cret")
	ctx := context.Background()

	// POST /play：夹具请求体 → 夹具成功响应体
	code, body := rawDo(t, tsURL, http.MethodPost, "/play", "s3cret", string(readFixture(t, "play_request.json")))
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, string(readFixture(t, "play_response.json")), body)

	// POST /volume：夹具请求体 → ok=true + 当前状态
	code, body = rawDo(t, tsURL, http.MethodPost, "/volume", "s3cret", string(readFixture(t, "volume_request.json")))
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, `{"ok":true,"state":"playing"}`, body)

	// GET /status：播放中形态对齐 status_response.json（playing@42000/5400000）
	ctl.setDuration(5400000)
	_, err := c.Seek(ctx, tgt, adapter.SeekRequest{PositionMS: 42000})
	require.NoError(t, err)
	code, body = rawDo(t, tsURL, http.MethodGet, "/status", "s3cret", "")
	assert.Equal(t, http.StatusOK, code)
	assert.NotContains(t, body, `"ok"`, "GET /status 的 200 响应不含 ok 字段")
	assert.JSONEq(t, string(readFixture(t, "status_response.json")), body)

	// GET /status：出错形态对齐 status_error_response.json（error 态信息清零）
	ctl.FailNextLoad("source_unreachable")
	_, err = c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL})
	require.Error(t, err)
	code, body = rawDo(t, tsURL, http.MethodGet, "/status", "s3cret", "")
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, string(readFixture(t, "status_error_response.json")), body)
}

// ---- 回归测试：handleStatus 必须在 s.mu 临界区内读取 Controller.Status() ----

// lockProbeController 是专测锁序的 Controller 假实现（仅服务
// TestStatusHoldsStateLockAcrossControllerRead）：Status() 进入即定格快照、
// 随后阻塞在 release 上（模拟慢速本地 IPC、在途 /status）。eof 模拟旧媒体
// 播完（EOF → 后端自报 idle）；Load() 下发新媒体即清 eof（新会话在播）。
// 快照在进入时定格（而非返回时）模拟的正是竞态前提——旧媒体 EOF 的在途
// idle 快照，即便随后有新 Load 也不改写。
type lockProbeController struct {
	entered     chan struct{} // Status() 已进入（缓冲发信，不阻塞调用方）
	release     chan struct{} // 关闭以放行阻塞中的 Status()
	loadEntered chan struct{} // Load() 已进入
	eof         bool          // 旧媒体已播完（EOF → 快照 idle）；Load 新媒体即清除
}

func newLockProbeController() *lockProbeController {
	return &lockProbeController{
		entered:     make(chan struct{}, 8),
		release:     make(chan struct{}),
		loadEntered: make(chan struct{}, 8),
	}
}

func (p *lockProbeController) Load(_ context.Context, _, _ string, _ int64) error {
	p.eof = false // 新媒体起播，EOF 清除
	select {
	case p.loadEntered <- struct{}{}:
	default:
	}
	return nil
}

func (p *lockProbeController) Pause() error       { return nil }
func (p *lockProbeController) Stop() error        { return nil }
func (p *lockProbeController) SeekTo(int64) error { return nil }
func (p *lockProbeController) Volume(int) error   { return nil }

func (p *lockProbeController) Status() adapter.Status {
	snap := adapter.Status{State: statePlaying}
	if p.eof {
		snap = adapter.Status{State: stateIdle}
	}
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-p.release // 阻塞直至放行：把 /status 钉在"快照已取、状态机未判"的窗口上
	return snap
}

// waitCh 等待信号通道，超时 fatal（防测试自身挂死）。
func waitCh(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

// TestStatusHoldsStateLockAcrossControllerRead 钉死锁序回归：handleStatus 必须在
// 持有 s.mu 期间读取 Controller.Status()（修复前在取锁前读取）。
//
// 该测试钉住的不变量：
//  1. /status 阻塞在 ctl.Status() 里时，并发 /play 无法完成状态提交——
//     两 handler 在"快照已取、状态机未判"窗口上互斥（真竞态难以确定性命中，
//     以此等价的锁序断言代替）；
//  2. 因此在途 EOF idle 快照只配对旧会话：旧会话迁 idle 后，新 /play 会话
//     最终仍是 playing，不会被旧快照错误地拍死在 idle（粘滞错态）。
//
// 修复前的形态下本测试失败：/play 会在 Status() 阻塞窗口内完成提交，随后
// /status 拿到锁看到"新会话 playing + 旧 idle 快照"，把新会话迁到 idle——
// 最末的新会话 playing 断言落空。
func TestStatusHoldsStateLockAcrossControllerRead(t *testing.T) {
	pctl := newLockProbeController()
	s := NewServer("s3cret", "卧室", "test-renderer", 0, pctl)

	// 旧媒体在播（Server state=playing），随后播完 EOF：后端自报 idle
	rec := httptest.NewRecorder()
	s.handlePlay(rec, httptest.NewRequest(http.MethodPost, "/play",
		strings.NewReader(`{"url":"`+mediaURL+`","title":"Interstellar"}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	pctl.eof = true

	// 在途 /status：Status() 一进入即阻塞
	statusDone := make(chan struct{})
	recA := httptest.NewRecorder()
	go func() {
		defer close(statusDone)
		s.handleStatus(recA, httptest.NewRequest(http.MethodGet, "/status", nil))
	}()
	waitCh(t, pctl.entered, "handleStatus 未调用 Controller.Status()")

	// 并发 /play（新媒体）：Load 下发后、状态提交前应被 s.mu 挡住
	playDone := make(chan struct{})
	recB := httptest.NewRecorder()
	go func() {
		defer close(playDone)
		s.handlePlay(recB, httptest.NewRequest(http.MethodPost, "/play",
			strings.NewReader(`{"url":"`+mediaURL+`2","title":"Interstellar II"}`)))
	}()
	waitCh(t, pctl.loadEntered, "并发 /play 未调用 Controller.Load()")

	// 放行前给 /play 一次完成机会：持锁（修复后）它必然完不成，超时放行；
	// 未持锁（修复前）它会在窗口内完成提交，最末的新会话断言随之落空。
	select {
	case <-playDone:
		t.Log("/play 在 /status 仍阻塞于 Status() 时完成提交（修复前的交错形态）")
	case <-time.After(time.Second):
	}

	close(pctl.release) // 放行阻塞中的 Status()
	waitCh(t, statusDone, "/status 未随放行返回")
	waitCh(t, playDone, "放行后 /play 仍未返回（未被 s.mu 放行？）")

	// 在途 /status 配对旧会话：旧会话随 EOF 快照迁 idle（eof 迁移语义不变）
	var stA statusResp
	require.NoError(t, json.Unmarshal(recA.Body.Bytes(), &stA))
	assert.Equal(t, stateIdle, stA.State, "旧会话应随在途 EOF 快照迁 idle")

	// 新会话 /play 提交成功
	assert.JSONEq(t, `{"ok":true,"state":"playing"}`, recB.Body.String())

	// 关键断言：新 /play 会话保持 playing，未被旧 idle 快照迁成 idle
	var final statusResp
	recF := httptest.NewRecorder()
	s.handleStatus(recF, httptest.NewRequest(http.MethodGet, "/status", nil))
	require.NoError(t, json.Unmarshal(recF.Body.Bytes(), &final))
	assert.Equal(t, statePlaying, final.State, "新 /play 会话不得被在途 EOF 快照拍死成 idle")
	assert.Equal(t, "Interstellar II", final.Title)

	s.mu.Lock()
	got := s.state
	s.mu.Unlock()
	assert.Equal(t, statePlaying, got, "Server 状态机最终应为新会话的 playing")
}
