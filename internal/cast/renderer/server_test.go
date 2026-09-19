package renderer

import (
	"context"
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

	failLoadArmed bool
	failLoadMsg   string
	pauseErr      error
}

func (f *fakeController) Load(_ context.Context, u, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failLoadArmed {
		f.failLoadArmed = false
		return errors.New(f.failLoadMsg)
	}
	f.url, f.title = u, title
	f.positionMS = 0 // 新媒体从头起播
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

// Status 返回播放后端快照：State 恒空（状态机归 Server 所有），仅回传
// 位置/时长/标题供 /status 组装。
func (f *fakeController) Status() adapter.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return adapter.Status{PositionMS: f.positionMS, DurationMS: f.durationMS, Title: f.title}
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

func (f *fakeController) loaded() (u, title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.url, f.title
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

func TestPlay_ResumePosition_SeeksAfterLoad(t *testing.T) {
	ctx := context.Background()
	ctl, _, c, tgt := newTestServer(t, "s3cret")
	ctl.setDuration(5400000)

	// 携 position_ms 的 /play：Load 后应跟进 seek 到起播位置（续播）
	_, err := c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar", PositionMS: 30000})
	require.NoError(t, err)
	st, err := c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, int64(30000), st.PositionMS)
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
