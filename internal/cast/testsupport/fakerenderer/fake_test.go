package fakerenderer

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dayanio/lattice-cast/internal/cast/adapter"
	"github.com/dayanio/lattice-cast/internal/cast/adapter/latticecast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mediaURL = "http://192.168.1.10:7810/media/abc123"

// newTestFake 构造测试用 Fake（测试结束自动关闭），并返回配套的真实协议客户端与 Target。
func newTestFake(t *testing.T, token string) (*Fake, *latticecast.Client, adapter.Target) {
	t.Helper()
	f := New(token)
	t.Cleanup(f.Close)
	return f, latticecast.NewClient(f.Client()), f.Target()
}

func TestStatus_InitiallyIdle(t *testing.T) {
	_, c, tgt := newTestFake(t, "s3cret")
	st, err := c.Status(context.Background(), tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)
}

func TestPlayPauseStop_StateMachine(t *testing.T) {
	ctx := context.Background()
	f, c, tgt := newTestFake(t, "s3cret")

	// play：idle → playing，title/url 生效
	st, err := c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing"}, st)

	f.mu.Lock()
	assert.Equal(t, mediaURL, f.url) // url 被渲染端记录（协议不回传，白盒断言）
	f.mu.Unlock()

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
	f, c, _ := newTestFake(t, "s3cret")

	// 线上形态：401 + {"ok":false,"error":"unauthorized"}（含缺 token）
	for _, token := range []string{"", "wrong"} {
		req, err := http.NewRequest(http.MethodGet, f.URL+"/status", nil)
		require.NoError(t, err)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.JSONEq(t, `{"ok":false,"error":"unauthorized"}`, string(body))
	}

	// 客户端形态：*ProtocolError("unauthorized")
	bad := f.Target()
	bad.Token = "wrong"
	_, err := c.Status(context.Background(), bad)
	require.Error(t, err)
	var pe *latticecast.ProtocolError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "unauthorized", pe.Msg)
}

func TestWireErrors(t *testing.T) {
	f, _, _ := newTestFake(t, "s3cret")

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
		{"invalid json body", http.MethodPost, "/play", "{not-json", http.StatusBadRequest, "bad_request"},
		{"missing url", http.MethodPost, "/play", `{"title":"x"}`, http.StatusBadRequest, "bad_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, f.URL+tt.path, strings.NewReader(tt.body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer s3cret")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, tt.wantCode, resp.StatusCode)
			assert.JSONEq(t, `{"ok":false,"error":"`+tt.wantError+`"}`, string(body))
		})
	}
}

func TestFailNextPlay_ErrorStickyUntilNextPlay(t *testing.T) {
	ctx := context.Background()
	f, c, tgt := newTestFake(t, "s3cret")

	// 先正常播放并 seek，让旧媒体信息在位（失败后应被清空）
	_, err := c.Play(ctx, tgt, adapter.PlayRequest{URL: mediaURL, Title: "Interstellar"})
	require.NoError(t, err)
	_, err = c.Seek(ctx, tgt, adapter.SeekRequest{PositionMS: 90000})
	require.NoError(t, err)

	// 注入失败：该次 /play 返回 ok=false，客户端收到 *ProtocolError
	f.FailNextPlay("source_unreachable")
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

	// error 态下 /stop /pause 均不产生迁移（protocol.md 第六节）
	st, err = c.Stop(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "error"}, st)
	st, err = c.Pause(ctx, tgt)
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

func TestSetPlaying_PresetsPlayingState(t *testing.T) {
	ctx := context.Background()
	f, c, tgt := newTestFake(t, "s3cret")

	f.SetPlaying(mediaURL, "Interstellar", 5400000)
	st, err := c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "playing", Title: "Interstellar", DurationMS: 5400000}, st)

	// 预置态直接参与状态机
	st, err = c.Pause(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused"}, st)
}

// TestClientConformance 是“适配器一致性套件”落点：用真实 latticecast.Client
// 对 Fake 跑完整链路 play→status→pause→seek→volume→status→stop，断言每步
// 返回与渲染端状态。未来 DLNA / 树莓派适配器复用同一调用序列。
func TestClientConformance(t *testing.T) {
	ctx := context.Background()
	f, c, tgt := newTestFake(t, "s3cret")
	assert.Equal(t, f.URL, tgt.BaseURL()) // Target 与 Server 指向同一渲染端

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
	assert.Equal(t, adapter.Status{State: "playing", Title: "Interstellar"}, st)

	// pause → paused（进度保留）
	st, err = c.Pause(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused", Title: "Interstellar"}, st)

	// seek → 状态不变，进度生效
	st, err = c.Seek(ctx, tgt, adapter.SeekRequest{PositionMS: 90000})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused", PositionMS: 90000, Title: "Interstellar"}, st)

	// volume → 状态与进度均不变
	st, err = c.Volume(ctx, tgt, adapter.VolumeRequest{Level: 42})
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "paused", PositionMS: 90000, Title: "Interstellar"}, st)

	// stop → idle，媒体清空
	st, err = c.Stop(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)
	st, err = c.Status(ctx, tgt)
	require.NoError(t, err)
	assert.Equal(t, adapter.Status{State: "idle"}, st)
}
