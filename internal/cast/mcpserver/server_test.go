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
	"github.com/dayanio/lattice-cast/internal/cast/testsupport/fakereflux"
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
	return newStackWith(t, nil)
}

// newStackWith 同 newStack，但允许注入可选的 reflux 内容源（nil = 禁用，
// 与真实部署"未配置 reflux_url"一致）。
func newStackWith(t *testing.T, reflux *resolve.RefluxSource) (url string, auditPath string, lib *resolve.Library, fake *fakerenderer.Fake) {
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
	res := &resolve.Resolver{Lib: lib, Base: cfg.MediaBaseURL, Reflux: reflux} // Ext=nil：YouTube 禁用

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

// TestRefluxContentSource 接了 fake reflux 后：search_media 返回 NAS ∪ reflux
// （reflux 条目带 reflux: 前缀）；cast_play 用 reflux media_id 时渲染端收到的
// 是 reflux 的 static 直链。
func TestRefluxContentSource(t *testing.T) {
	const refluxTok = "tok-reflux-test"
	rf := fakereflux.New(refluxTok)
	rf.SetItems(map[string]any{
		"Id":            "6334",
		"Name":          "让子弹飞",
		"OriginalTitle": "Let the Bullets Fly",
		"Type":          "Movie",
		"MediaType":     "Video",
		"Year":          2010,
	})
	t.Cleanup(rf.Close)

	url, auditPath, _, fake := newStackWith(t, resolve.NewRefluxSource(rf.URL, refluxTok))
	f := &fixture{t: t, fake: fake, cs: connect(t, url), auditPath: auditPath}

	// NAS 检索不回流（reflux 条目不含 "night"）。
	var items []resolve.Item
	f.callOK("search_media", map[string]any{"query": "night"}, &items)
	require.Len(t, items, 1)
	assert.NotContains(t, items[0].ID, "reflux:", "纯 NAS 命中不得带 reflux: 前缀")

	// reflux 检索只回 reflux 条目，media_id 带 tagged 前缀。
	f.callOK("search_media", map[string]any{"query": "让子弹飞"}, &items)
	require.Len(t, items, 1)
	assert.Equal(t, "reflux:6334", items[0].ID, "reflux 条目的 media_id 应带 tagged 前缀")
	assert.Equal(t, "让子弹飞", items[0].Title)
	assert.Equal(t, resolve.KindVideo, items[0].Kind)

	// cast_play 走 reflux: id：渲染端收到的 URL 逐字为 reflux static 直链。
	var out struct {
		Status  adapter.Status `json:"status"`
		Adapter string         `json:"adapter"`
	}
	f.callOK("cast_play", map[string]any{
		"device": devName, "media_id": "reflux:6334", "title": "让子弹飞",
	}, &out)
	assert.Equal(t, "playing", out.Status.State)
	assert.Equal(t, rf.URL+"/Videos/6334/stream?static=true&api_key="+refluxTok,
		fake.PlayedURL(), "渲染端应收下 reflux 的 static 直链")
	assert.GreaterOrEqual(t, rf.StreamHits(), 1, "Resolver.ByID 应对 reflux 做过可用性探测")

	// 混合检索：NAS sunset-clip ∪ reflux 让子弹飞 并存（空查询全量合并）。
	f.callOK("search_media", map[string]any{"query": ""}, &items)
	ids := map[string]bool{}
	for _, it := range items {
		ids[it.ID] = true
	}
	assert.Len(t, items, 3, "NAS 2 条 + reflux 1 条")
	assert.True(t, ids["reflux:6334"], "reflux 条目应在合并结果里")
}

// TestRefluxDownCastPlayAuditRedactsToken reflux 宕机时 cast_play 失败：错误
// 文本（LLM 转录）与审计 JSONL 都不得出现 reflux api_key——reflux 不可达是
// 常态故障，传输层错误原文（*url.Error）内嵌完整请求 URL（含 api_key），
// 明文落盘即凭证泄露。
func TestRefluxDownCastPlayAuditRedactsToken(t *testing.T) {
	const refluxTok = "tok-reflux-down-leak"
	rf := fakereflux.New(refluxTok)
	downURL := rf.URL
	rf.Close() // 端口关闭 → 连接被拒（reflux DOWN）

	url, auditPath, _, fake := newStackWith(t, resolve.NewRefluxSource(downURL, refluxTok))
	f := &fixture{t: t, fake: fake, cs: connect(t, url), auditPath: auditPath}

	got := f.callErr("cast_play", map[string]any{"device": devName, "media_id": "reflux:6334"})
	assert.NotContains(t, got, refluxTok, "LLM 所见错误文本不得含 api_key")
	assert.NotContains(t, got, "api_key=", "LLM 所见错误文本不得含查询串")

	b, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	assert.NotContains(t, string(b), refluxTok, "审计 JSONL 不得落 api_key 明文")
	assert.NotContains(t, string(b), "api_key=", "审计 JSONL 不得落查询串")
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

// TestCastPlay_ResumePosition position_ms 随 cast_play 透传到渲染端：LLM 的
// 续播模式（cast_status → 带 position_ms 重播）依赖该参数（协议 /play 本就
// 携带 position_ms）。
func TestCastPlay_ResumePosition(t *testing.T) {
	f := newFixture(t)
	var items []resolve.Item
	f.callOK("search_media", map[string]any{"query": "night"}, &items)
	require.Len(t, items, 1)

	var out struct {
		Status adapter.Status `json:"status"`
	}
	f.callOK("cast_play", map[string]any{
		"device": devName, "media_id": items[0].ID, "position_ms": 30000,
	}, &out)
	assert.Equal(t, "playing", out.Status.State)

	// 位置真的到达渲染端：Fake 记录 PlayRequest.PositionMS，/status 可读回。
	var st struct {
		Status adapter.Status `json:"status"`
	}
	f.callOK("cast_status", map[string]any{"device": devName}, &st)
	assert.Equal(t, int64(30000), st.Status.PositionMS, "position_ms 应透传到渲染端")
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

	// reflux: 前缀 id 但未配置 reflux → reflux_disabled（配置错误可转述）。
	got = f.callErr("cast_play", map[string]any{"device": devName, "media_id": "reflux:6334"})
	assert.Equal(t, "reflux_disabled", got)

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

// ---- 意图快通道内部工具：cast_resume（Executor 直调面，不在 MCP 注册表）----

// TestExecutor_CastResume 断点续播经执行核心适配器（Task 26 意图快通道的
// 专属工具）：无断点 → no_last_played 契约错误；Play → Seek → Pause 记断点
// 后 cast_resume 以同 URL 同位置续播；失败与成功都落审计（与八个 MCP 工具
// 同一 JSONL）。
func TestExecutor_CastResume(t *testing.T) {
	fake := fakerenderer.New(renderTok)
	t.Cleanup(fake.Close)
	tgt := fake.Target()

	cfg := config.Config{
		AuthToken:    mcpToken,
		MediaBaseURL: "http://192.168.1.10:7810",
		Renderers: map[string]config.Renderer{
			devName: {Room: devRoom, Host: tgt.Host, Port: tgt.Port, Token: renderTok},
		},
	}
	lib := resolve.NewLibrary(nil)
	res := &resolve.Resolver{Lib: lib, Base: cfg.MediaBaseURL}
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := manager.OpenAudit(auditPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = audit.Close() })

	mgr := manager.New(cfg, lib, res)
	srv := New(mgr, lib, res, audit, StaticToken(cfg.AuthToken))
	exec := srv.Executor()
	ctx := context.Background()
	resumeArgs := json.RawMessage(`{"device":"` + devName + `"}`)

	// 无断点 → 契约错误（且已落审计）。
	_, err = exec.Execute(ctx, "cast_resume", resumeArgs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no_last_played")

	// 记断点：Play → Seek 到 90 秒 → Pause。
	const media = "http://192.168.1.10:7810/media/santi.mp4"
	_, err = mgr.Play(ctx, devName, adapter.PlayRequest{URL: media, Title: "三体"})
	require.NoError(t, err)
	_, err = mgr.Seek(ctx, devName, 90000)
	require.NoError(t, err)
	_, err = mgr.Pause(ctx, devName)
	require.NoError(t, err)

	// 续播：同 URL 同位置。
	out, err := exec.Execute(ctx, "cast_resume", resumeArgs)
	require.NoError(t, err)
	var po playOut
	require.NoError(t, json.Unmarshal([]byte(out), &po))
	assert.Equal(t, "playing", po.Status.State)
	assert.Equal(t, "latticecast", po.Adapter)
	assert.Equal(t, media, fake.PlayedURL(), "cast_resume 应以断点同 URL 重新起播")
	st, err := mgr.Status(ctx, devName)
	require.NoError(t, err)
	assert.Equal(t, int64(90000), st.PositionMS, "cast_resume 应从断点位置起播")

	// 审计：失败 + 成功各一行。
	raw, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(raw), `"cast_resume"`), "cast_resume 的失败与成功都应落审计")
}
