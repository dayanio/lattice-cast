// Package latticecast 实现 LatticeCast wire protocol v1 的 Go 客户端
// （cast-agent 侧）。协议唯一权威定义见 docs/protocol.md，
// 契约夹具见 testdata/contract/。
package latticecast

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/dayanio/lattice-cast/internal/cast/adapter"
)

// 与 adapter 包共享的数据类型（同一类型的别名，调用方可任选一个包 import）。
type (
	PlayRequest   = adapter.PlayRequest
	SeekRequest   = adapter.SeekRequest
	VolumeRequest = adapter.VolumeRequest
	Status        = adapter.Status
	Target        = adapter.Target
)

// ProtocolError 是渲染端返回的应用层错误：HTTP 非 2xx，或 HTTP 200 且 ok=false。
type ProtocolError struct {
	Op  string // 端点路径，如 "/play"
	Msg string // 响应体中的 error 字段
}

func (e *ProtocolError) Error() string { return e.Msg }

// Client 是 LatticeCast 协议客户端
type Client struct {
	HTTP *http.Client
}

// NewClient 构造协议客户端；hc 为 nil 时使用零值 http.Client（即 DefaultTransport）。
func NewClient(hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{HTTP: hc}
}

// Play 播放指定 URL（渲染端自行拉流）。
func (c *Client) Play(ctx context.Context, t Target, r PlayRequest) (Status, error) {
	return c.do(ctx, t, http.MethodPost, "/play", r)
}

// Pause 暂停；已在 paused 时幂等。
func (c *Client) Pause(ctx context.Context, t Target) (Status, error) {
	return c.do(ctx, t, http.MethodPost, "/pause", nil)
}

// Stop 停止并清空当前媒体；已在 idle 时幂等。
func (c *Client) Stop(ctx context.Context, t Target) (Status, error) {
	return c.do(ctx, t, http.MethodPost, "/stop", nil)
}

// Seek 跳转播放位置，不改变播放状态。
func (c *Client) Seek(ctx context.Context, t Target, r SeekRequest) (Status, error) {
	return c.do(ctx, t, http.MethodPost, "/seek", r)
}

// Volume 设置音量（Level 0-100），不改变播放状态。
func (c *Client) Volume(ctx context.Context, t Target, r VolumeRequest) (Status, error) {
	return c.do(ctx, t, http.MethodPost, "/volume", r)
}

// Status 查询当前状态。200 响应不含 ok 字段，以 state 为准。
func (c *Client) Status(ctx context.Context, t Target) (Status, error) {
	return c.do(ctx, t, http.MethodGet, "/status", nil)
}

// do 统一执行一次协议调用：构造请求（JSON body、Bearer 头）→ 发送 →
// 非 2xx 或 ok=false 时返回携带响应体 error 字段的 *ProtocolError；
// 网络失败原样上抛（*url.Error，Manager 据此判离线）；2xx 解析为 Status。
func (c *Client) do(ctx context.Context, t Target, method, path string, body any) (Status, error) {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return Status{}, err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.BaseURL()+path, payload)
	if err != nil {
		return Status{}, err
	}
	req.Header.Set("Authorization", "Bearer "+t.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Status{}, err // 网络失败：*url.Error 原样上抛
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Status{}, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Status{}, &ProtocolError{Op: path, Msg: errField(data)}
	}

	var parsed struct {
		OK         *bool  `json:"ok"` // GET /status 响应不含 ok 字段 → nil
		State      string `json:"state"`
		PositionMS int64  `json:"position_ms"`
		DurationMS int64  `json:"duration_ms"`
		Title      string `json:"title"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return Status{}, err
	}
	if parsed.OK != nil && !*parsed.OK {
		return Status{}, &ProtocolError{Op: path, Msg: errField(data)}
	}
	return Status{
		State:      parsed.State,
		PositionMS: parsed.PositionMS,
		DurationMS: parsed.DurationMS,
		Title:      parsed.Title,
		Error:      parsed.Error,
	}, nil
}

// errField 提取响应体中的 error 字段；缺失或非 JSON 时回退为 "unknown"，
// 保证 ProtocolError.Error() 不返回空串。
func errField(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return "unknown"
}
