// Package mcpserver 的验收测试：经 go-sdk 客户端（streamable HTTP 传输）连
// httptest 服务器，逐工具断言 LLM 侧契约（名称/参数/错误文本固定），并核对
// 审计文件随每次调用（含失败）增长。
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dayanio/lattice-cast/internal/cast/adapter"
	"github.com/dayanio/lattice-cast/internal/cast/config"
	"github.com/dayanio/lattice-cast/internal/cast/manager"
	"github.com/dayanio/lattice-cast/internal/cast/resolve"
	"github.com/dayanio/lattice-cast/internal/cast/testsupport/fakerenderer"
)

// 测试常量：MCP token 与渲染端 token 各司其职（后者由 fakerenderer 校验）。
const (
	mcpToken    = "tok-mcp-test"
	renderTok   = "tok-renderer"
	devName     = "living-room-display"
	devRoom     = "living-room"
	callTimeout = 15 * time.Second
)

// bearer 是给 go-sdk 客户端注入 Authorization 头的 RoundTripper。
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// fixture 是一套完整的被测栈：Fake 渲染端 + 静态配置 Manager + 临时媒体库 +
// Resolver（Ext=nil → youtube_disabled）+ 审计文件 + 被测 Server 的 httptest
// 外壳 + 已握手的 go-sdk 客户端会话。
type fixture struct {
	t         *testing.T
	fake      *fakerenderer.Fake
	lib       *resolve.Library
	cs        *mcp.ClientSession
	auditPath string
}

// newStack 构造 Server 及其 httptest 外壳（不含客户端），返回外壳 URL。
func newStack(t *testing.T) (url string, auditPath string, lib *resolve.Library, fake *fakerenderer.Fake) {
	t.Helper()

	fake = fakerenderer.New(renderTok)
	t.Cleanup(fake.Close)
	tgt := fake.Target()

	cfg := config.Config{
		AuthToken:    mcpToken,
		MediaBaseURL: "http://192.168.1.10:7810",
		Renderers: map[string]config.Renderer{
			devName: {Room: devRoom, Host: tgt.Host, Port: tgt.Port, Token: renderTok},
		},
	}

	libDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "night-song.mp3"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "sunset-clip.mp4"), []byte("x"), 0o600))
	lib = resolve.NewLibrary([]string{libDir})
	require.NoError(t, lib.Rescan())
	res := &resolve.Resolver{Lib: lib, Base: cfg.MediaBaseURL} // Ext=nil：YouTube 禁用

	auditPath = filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := manager.OpenAudit(auditPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = audit.Close() })

	srv := New(manager.New(cfg, lib, res), lib, res, audit, StaticToken(cfg.AuthToken))
	ts := httptest.NewServer(srv.HTTP())
	t.Cleanup(ts.Close)
	return ts.URL, auditPath, lib, fake
}

// connect 用 go-sdk 客户端（streamable 传输 + bearer 头）完成 initialize 握手。
func connect(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-llm", Version: "0.0.1"}, nil)
	tr := &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearer{token: mcpToken}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cs, err := client.Connect(ctx, tr, nil)
	require.NoError(t, err, "initialize 握手应成功")
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url, auditPath, lib, fake := newStack(t)
	return &fixture{t: t, fake: fake, lib: lib, cs: connect(t, url), auditPath: auditPath}
}

// call 发起一次 tools/call（成功与失败皆返回原始结果）。
func (f *fixture) call(name string, args map[string]any) *mcp.CallToolResult {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	res, err := f.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(f.t, err, "tools/call %s 不应产生协议层错误", name)
	return res
}

// callOK 断言工具成功并把 structuredContent 反序列化进 out。
func (f *fixture) callOK(name string, args map[string]any, out any) {
	f.t.Helper()
	res := f.call(name, args)
	require.False(f.t, res.IsError, "工具 %s 应成功，实际错误：%v", name, errText(res))
	require.NotNil(f.t, res.StructuredContent, "工具 %s 应返回 structuredContent", name)
	raw, mErr := json.Marshal(res.StructuredContent)
	require.NoError(f.t, mErr)
	require.NoError(f.t, json.Unmarshal(raw, out), "工具 %s 的 structuredContent 形态不符", name)
}

// callErr 断言工具以 IsError 结果失败并返回错误文本（LLM 所见的字符串）。
func (f *fixture) callErr(name string, args map[string]any) string {
	f.t.Helper()
	res := f.call(name, args)
	require.True(f.t, res.IsError, "工具 %s 应报工具错误，实际：%s", name, summary(res))
	return errText(res)
}

// errText 提取 IsError 结果中的错误文本。
func errText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return fmt.Sprintf("%v", res.Content[0])
}

func summary(res *mcp.CallToolResult) string {
	b, _ := json.Marshal(res.StructuredContent)
	return string(b)
}

// auditEntries 读回审计文件并逐行解析。
func (f *fixture) auditEntries() []manager.AuditEntry {
	f.t.Helper()
	b, err := os.ReadFile(f.auditPath)
	require.NoError(f.t, err)
	var entries []manager.AuditEntry
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e manager.AuditEntry
		require.NoError(f.t, json.Unmarshal([]byte(line), &e), "审计行应为单层 JSON：%q", line)
		entries = append(entries, e)
	}
	return entries
}

// ---- 鉴权 ----

// TestUnauthorized 无 token / 错 token：任何请求（含 initialize）都被中间件
// 以 401 {"error":"unauthorized"} 拒绝，SDK 处理器不可达。
func TestUnauthorized(t *testing.T) {
	url, _, _, _ := newStack(t)

	for _, tc := range []struct {
		name   string
		token  string
		method string
	}{
		{"no token", "", http.MethodPost},
		{"wrong token", "tok-wrong", http.MethodPost},
		{"no token GET (SSE)", "", http.MethodGet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, url, strings.NewReader("{}"))
			require.NoError(t, err)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

			var body map[string]string
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
			assert.Equal(t, "unauthorized", body["error"], "401 响应体固定为 {\"error\":\"unauthorized\"}")
		})
	}
}

// ---- list_cast_devices ----

// TestListCastDevices 经 MCP 客户端列出设备：Fake 在线且正在播放，
// 清单含唯一设备且 online/state/now_playing 来自 /status。
func TestListCastDevices(t *testing.T) {
	f := newFixture(t)
	f.fake.SetPlaying("http://192.168.1.10:7810/media/abc", "夜曲", 180000)

	var devs []manager.Device
	f.callOK("list_cast_devices", map[string]any{}, &devs)
	require.Len(t, devs, 1)
	got := devs[0]
	assert.Equal(t, devName, got.Name)
	assert.Equal(t, devRoom, got.Room)
	assert.True(t, got.Online)
	assert.Equal(t, "playing", got.State)
	assert.Equal(t, "夜曲", got.NowPlaying)
}

// ---- search_media ----

// TestSearchMedia 子串检索媒体库，返回 resolve.Item 形态（media_id/title/kind）。
func TestSearchMedia(t *testing.T) {
	f := newFixture(t)

	var items []resolve.Item
	f.callOK("search_media", map[string]any{"query": "night"}, &items)
	require.Len(t, items, 1)
	assert.Equal(t, "night-song", items[0].Title)
	assert.NotEmpty(t, items[0].ID, "media_id 必须非空")
	assert.Equal(t, resolve.KindAudio, items[0].Kind)

	// 无命中 → 空数组（不是错误）。
	f.callOK("search_media", map[string]any{"query": "no-such-media"}, &items)
	assert.Empty(t, items)
}

// ---- cast_play：成功路径 ----

// TestCastPlay_ByMediaID 走 Resolver.ByID（NAS 直链）播放成功：
// 状态 playing、adapter 固定 "latticecast"、审计落一行 ok=true（agent 取自鉴权）。
func TestCastPlay_ByMediaID(t *testing.T) {
	f := newFixture(t)
	var items []resolve.Item
	f.callOK("search_media", map[string]any{"query": "night"}, &items)
	require.Len(t, items, 1)

	var out struct {
		Status  adapter.Status `json:"status"`
		Adapter string         `json:"adapter"`
	}
	f.callOK("cast_play", map[string]any{"device": devName, "media_id": items[0].ID}, &out)
	assert.Equal(t, "playing", out.Status.State)
	assert.Equal(t, "latticecast", out.Adapter, "adapter 字段固定为 latticecast")

	entries := f.auditEntries()
	require.Len(t, entries, 2, "search_media + cast_play 各一行")
	last := entries[1]
	assert.Equal(t, "cast_play", last.Tool)
	assert.Equal(t, "static-token", last.Agent, "agent 取自 Authenticator")
	assert.True(t, last.OK)
	assert.Contains(t, last.Args, items[0].ID, "审计 args 应含 media_id")
}

// TestCastPlay_ByURL 直链透传播放成功。
func TestCastPlay_ByURL(t *testing.T) {
	f := newFixture(t)

	var out struct {
		Status  adapter.Status `json:"status"`
		Adapter string         `json:"adapter"`
	}
	f.callOK("cast_play", map[string]any{
		"device": devName, "url": "http://192.168.1.50:8080/movie.mkv", "title": "Movie",
	}, &out)
	assert.Equal(t, "playing", out.Status.State)
}

// ---- cast_play：参数与解析错误（错误文本即契约） ----

// TestCastPlay_ArgumentRules both/neither 分支的固定错误文本。
func TestCastPlay_ArgumentRules(t *testing.T) {
	f := newFixture(t)

	got := f.callErr("cast_play", map[string]any{"device": devName})
	assert.Equal(t, "media_id_or_url_required", got, "都不传的错误文本固定")

	got = f.callErr("cast_play", map[string]any{
		"device": devName, "media_id": "abc123", "url": "http://example.com/x.mp3",
	})
	assert.Equal(t, "media_id_url_exclusive", got, "都传的错误文本固定")
}

// TestCastPlay_ResolveErrors 解析错误原样透传给 LLM。
func TestCastPlay_ResolveErrors(t *testing.T) {
	f := newFixture(t)

	got := f.callErr("cast_play", map[string]any{"device": devName, "media_id": "deadbeef0000"})
	assert.Equal(t, "unknown_media_id: deadbeef0000", got)

	got = f.callErr("cast_play", map[string]any{
		"device": devName, "url": "https://www.youtube.com/watch?v=x",
	})
	assert.Equal(t, "youtube_disabled", got, "未配置提取器时 YouTube 报 youtube_disabled")

	got = f.callErr("cast_play", map[string]any{"device": "nope", "url": "http://example.com/x.mp3"})
	assert.Equal(t, "unknown_device: nope", got)
}

// TestCastPlay_OfflineDevice 网络不可达 → device_offline 前缀。
func TestCastPlay_OfflineDevice(t *testing.T) {
	url, auditPath, _, fake := newStack(t)
	fake.Close() // 连接被拒 → 离线
	f := &fixture{t: t, cs: connect(t, url), auditPath: auditPath}

	got := f.callErr("cast_play", map[string]any{"device": devName, "url": "http://example.com/x.mp3"})
	assert.True(t, strings.HasPrefix(got, "device_offline:"), "离线错误应带 device_offline 前缀，实际：%s", got)
}

// ---- cast_volume ----

// TestCastVolume 越界（101 / -1）报 level_out_of_range；合法值成功。
func TestCastVolume(t *testing.T) {
	f := newFixture(t)

	got := f.callErr("cast_volume", map[string]any{"device": devName, "level": 101})
	assert.Equal(t, "level_out_of_range", got)

	got = f.callErr("cast_volume", map[string]any{"device": devName, "level": -1})
	assert.Equal(t, "level_out_of_range", got)

	var out struct {
		Status adapter.Status `json:"status"`
	}
	f.callOK("cast_volume", map[string]any{"device": devName, "level": 50}, &out)
	assert.NotEmpty(t, out.Status.State)

	// 失败也必须落审计。
	entries := f.auditEntries()
	require.GreaterOrEqual(t, len(entries), 3)
	var failLines int
	for _, e := range entries {
		if !e.OK {
			failLines++
			assert.Equal(t, "cast_volume", e.Tool)
		}
	}
	assert.Equal(t, 2, failLines, "两次越界调用各落一行 ok=false 审计")
}

// ---- cast_stop / cast_status ----

// TestCastStopAndStatus stop 后状态回 idle；status 反映当前播放态。
func TestCastStopAndStatus(t *testing.T) {
	f := newFixture(t)
	f.fake.SetPlaying("http://192.168.1.10:7810/media/abc", "夜曲", 180000)

	var st struct {
		Status adapter.Status `json:"status"`
	}
	f.callOK("cast_status", map[string]any{"device": devName}, &st)
	assert.Equal(t, "playing", st.Status.State)
	assert.Equal(t, "夜曲", st.Status.Title)

	f.callOK("cast_stop", map[string]any{"device": devName}, &st)
	assert.Equal(t, "idle", st.Status.State)

	f.callOK("cast_status", map[string]any{"device": devName}, &st)
	assert.Equal(t, "idle", st.Status.State)
	assert.Empty(t, st.Status.Title, "stop 后媒体标题清空")
}

// ---- cast_pause / cast_seek ----

// TestCastPause pause 后状态回 paused 且标题保留；未知设备报 unknown_device；
// 成功与失败各落一行审计。
func TestCastPause(t *testing.T) {
	f := newFixture(t)
	f.fake.SetPlaying("http://192.168.1.10:7810/media/abc", "夜曲", 180000)

	var st struct {
		Status adapter.Status `json:"status"`
	}
	f.callOK("cast_pause", map[string]any{"device": devName}, &st)
	assert.Equal(t, "paused", st.Status.State)

	f.callOK("cast_status", map[string]any{"device": devName}, &st)
	assert.Equal(t, "paused", st.Status.State)
	assert.Equal(t, "夜曲", st.Status.Title, "pause 后标题应保留")

	got := f.callErr("cast_pause", map[string]any{"device": "nope"})
	assert.Equal(t, "unknown_device: nope", got)

	entries := f.auditEntries()
	require.Len(t, entries, 3, "3 次 MCP 调用各一行审计（含失败；SetPlaying 是直调 Fake，不落审计）")
	assert.Equal(t, "cast_pause", entries[0].Tool)
	assert.True(t, entries[0].OK)
	assert.Equal(t, "cast_pause", entries[2].Tool)
	assert.False(t, entries[2].OK, "失败调用也须落审计")
}

// TestCastSeek 跳转位置生效且状态不变；负数报固定文本 position_out_of_range；
// 未知设备报 unknown_device；每次调用（含失败）各落一行审计。
func TestCastSeek(t *testing.T) {
	f := newFixture(t)
	f.fake.SetPlaying("http://192.168.1.10:7810/media/abc", "夜曲", 180000)

	var st struct {
		Status adapter.Status `json:"status"`
	}
	f.callOK("cast_seek", map[string]any{"device": devName, "position_ms": 90000}, &st)
	assert.Equal(t, "playing", st.Status.State, "seek 不改变播放状态")

	f.callOK("cast_status", map[string]any{"device": devName}, &st)
	assert.Equal(t, int64(90000), st.Status.PositionMS, "进度应来自渲染端")
	assert.Equal(t, "playing", st.Status.State)

	got := f.callErr("cast_seek", map[string]any{"device": devName, "position_ms": -1})
	assert.Equal(t, "position_out_of_range", got, "负数位置的错误文本固定")

	got = f.callErr("cast_seek", map[string]any{"device": "nope", "position_ms": 1000})
	assert.Equal(t, "unknown_device: nope", got)

	entries := f.auditEntries()
	require.Len(t, entries, 4, "4 次 MCP 调用各一行审计（含失败；SetPlaying 是直调 Fake，不落审计）")
	assert.Equal(t, "cast_seek", entries[0].Tool)
	assert.True(t, entries[0].OK)
	for _, i := range []int{2, 3} {
		assert.Equal(t, "cast_seek", entries[i].Tool)
		assert.False(t, entries[i].OK, "失败调用也须落审计")
	}
}

// ---- 审计 ----

// TestAuditGrowsWithEveryCall 审计行数随每次调用（含失败）严格增长，
// 每行 agent/tool/ok/duration_ms 形态正确。
func TestAuditGrowsWithEveryCall(t *testing.T) {
	f := newFixture(t)

	steps := []struct {
		tool string
		args map[string]any
		ok   bool
	}{
		{"list_cast_devices", map[string]any{}, true},
		{"search_media", map[string]any{"query": "song"}, true},
		{"cast_play", map[string]any{"device": devName}, false}, // media_id_or_url_required
		{"cast_play", map[string]any{"device": devName, "url": "http://example.com/x.mp3"}, true},
		{"cast_volume", map[string]any{"device": devName, "level": 101}, false},
		{"cast_stop", map[string]any{"device": devName}, true},
		{"cast_status", map[string]any{"device": devName}, true},
		{"cast_pause", map[string]any{"device": devName}, true},
		{"cast_seek", map[string]any{"device": devName, "position_ms": 1000}, true},
		{"cast_seek", map[string]any{"device": devName, "position_ms": -5}, false}, // position_out_of_range
	}

	for i, step := range steps {
		var out any // 形态随工具而异（数组/对象），这里只关心调用成功与否
		if step.ok {
			f.callOK(step.tool, step.args, &out)
		} else {
			f.callErr(step.tool, step.args)
		}
		entries := f.auditEntries()
		require.Len(t, entries, i+1, "第 %d 次调用后审计应有 %d 行（含失败调用）", i+1, i+1)
		e := entries[len(entries)-1]
		assert.Equal(t, step.tool, e.Tool)
		assert.Equal(t, "static-token", e.Agent)
		assert.Equal(t, step.ok, e.OK, "工具 %s 本行 ok 应为 %v", step.tool, step.ok)
		assert.NotEmpty(t, e.Args, "args 应序列化进审计")
		assert.NotEmpty(t, e.Result, "result 应写入审计")
		assert.GreaterOrEqual(t, e.DurationMS, int64(0))
	}
}
