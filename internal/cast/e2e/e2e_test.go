// Package e2e 是 cast-agent 的 CI 端到端测试：渲染端条目静态寻址指向
// Fake（全程不依赖组播，-short 必跑必过），在测试内复刻
// cmd/lattice-cast/main.go 的装配链——审计 → 媒体库 → 媒体服务 →
// Resolver → Manager → MCP Server → http.Server——再经 go-sdk 客户端
// 走通 list_cast_devices → search_media → cast_play → cast_status →
// cast_stop 全链路，并核对审计文件落行。装配顺序与 main.go 保持一致
// （测试内重复是有意为之：main.go 保持单文件薄入口，不为本测试导出装配代码）。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
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
	"github.com/dayanio/lattice-cast/internal/cast/mcpserver"
	"github.com/dayanio/lattice-cast/internal/cast/resolve"
	"github.com/dayanio/lattice-cast/internal/cast/testsupport/fakereflux"
	"github.com/dayanio/lattice-cast/internal/cast/testsupport/fakerenderer"
)

// 测试常量：MCP token 与渲染端 token 各司其职（后者由 Fake 校验）。
const (
	mcpToken    = "testtoken"
	renderTok   = "tok"
	refluxToken = "tok-reflux"
	devName     = "bedroom-tv"
	devRoom     = "卧室"

	// callTimeout 单次 tools/call 的上限：list_cast_devices 内部有一次
	// discovery.Browse（组播窗口至多 3 秒，无组播环境降级为静态条目）。
	callTimeout = 30 * time.Second
)

// bearer 是给 go-sdk 客户端注入 Authorization 头的 RoundTripper（与
// mcpserver 包测试同一模式）。
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// stack 是按 main.go 装配链搭起的被测栈。
type stack struct {
	mcpURL    string // MCP 端点 http://127.0.0.1:<port>
	mediaURL  string // 媒体服务基址（= cfg.MediaBaseURL）
	auditPath string
	fake      *fakerenderer.Fake // fake 渲染端（读回收到的拉流地址）
	reflux    *fakereflux.Fake   // fake reflux 内容源（挂进 Resolver）
}

// startStack 复刻 main.go 的装配顺序（见 main.go run 的注释）：
// 审计 → 媒体库（临时目录 + 一个 mp4）→ 媒体服务 → Resolver → Manager →
// MCP Server → http.Server；渲染端为静态条目指向 Fake。
//
// 端口策略：resolve.MediaServer 的 Start 支持 ":0"（内部 net.Listen），
// 但不暴露实际绑定地址，而 cfg.MediaBaseURL 又必须写给渲染端真实端口，
// 故媒体端口用「net.Listen(127.0.0.1:0) 探测后关闭、MediaServer 复用」
// 取得（标准做法，存在理论上的端口争用窗口，测试环境可接受）；MCP 侧
// 测试自行绑定 127.0.0.1:0 并把真实地址写回 cfg.MCPListen，再 Serve。
func startStack(t *testing.T) *stack {
	t.Helper()

	// 媒体端口：预绑定取空闲端口后关闭，交给 MediaServer 复用。
	mediaLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	mediaAddr := mediaLn.Addr().String()
	mediaPort := mediaLn.Addr().(*net.TCPAddr).Port
	require.NoError(t, mediaLn.Close())

	// MCP 端口：测试自己持有监听器，真实地址回填 cfg。
	mcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	// 渲染端替身（真实 HTTP 服务，token 由其校验）。
	fake := fakerenderer.New(renderTok)
	tgt := fake.Target()

	// 临时媒体库：一个 mp4。
	libDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "sunset-clip.mp4"),
		bytes.Repeat([]byte{0}, 1024), 0o600))

	cfg := config.Config{
		MCPListen:    mcpLn.Addr().String(),
		MediaListen:  mediaAddr,
		MediaBaseURL: fmt.Sprintf("http://127.0.0.1:%d", mediaPort),
		MediaLibrary: []string{libDir},
		YtDlp:        "", // 禁用 YouTube 提流
		AuthToken:    mcpToken,
		Renderers: map[string]config.Renderer{
			devName: {Room: devRoom, Host: tgt.Host, Port: tgt.Port, Token: renderTok},
		},
	}

	// 以下顺序与 main.go 一致。
	auditPath := filepath.Join(t.TempDir(), "lattice-cast.audit.jsonl")
	audit, err := manager.OpenAudit(auditPath)
	require.NoError(t, err)

	lib := resolve.NewLibrary(cfg.MediaLibrary)
	require.NoError(t, lib.Rescan(), "临时媒体目录应可扫描")

	media := resolve.NewMediaServer(lib, cfg.MediaListen)
	require.NoError(t, media.Start())

	// reflux 内容源：fake reflux 挂一部电影（配置了 reflux_url 即挂载，与
	// main.go 的条件装配一致）。
	refluxFake := fakereflux.New(refluxToken)
	refluxFake.SetItems(map[string]any{
		"Id":            "6334",
		"Name":          "让子弹飞",
		"OriginalTitle": "Let the Bullets Fly",
		"Type":          "Movie",
		"MediaType":     "Video",
		"Year":          2010,
	})
	refluxSrc := resolve.NewRefluxSource(refluxFake.URL, refluxToken)
	res := &resolve.Resolver{Lib: lib, Base: cfg.MediaBaseURL, Reflux: refluxSrc} // YtDlp 为空 → Ext=nil
	mgr := manager.New(cfg, lib, res)
	srv := mcpserver.New(mgr, lib, res, audit, mcpserver.StaticToken(cfg.AuthToken))

	httpSrv := &http.Server{Handler: srv.HTTP()}
	go func() { _ = httpSrv.Serve(mcpLn) }()

	s := &stack{
		mcpURL:    "http://" + mcpLn.Addr().String(),
		mediaURL:  cfg.MediaBaseURL,
		auditPath: auditPath,
		fake:      fake,
		reflux:    refluxFake,
	}
	t.Cleanup(func() {
		_ = httpSrv.Close()
		_ = media.Close()
		_ = audit.Close()
		fake.Close()
		refluxFake.Close()
	})
	return s
}

// connect 用 go-sdk 客户端（streamable 传输 + bearer 头）完成 initialize 握手。
func connect(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-llm", Version: "0.0.1"}, nil)
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

// call 发起一次 tools/call，断言成功并把 structuredContent 反序列化进 out。
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err, "tools/call %s 不应产生协议层错误", name)
	require.False(t, res.IsError, "工具 %s 应成功，实际错误：%v", name, errText(res))
	require.NotNil(t, res.StructuredContent, "工具 %s 应返回 structuredContent", name)
	raw, mErr := json.Marshal(res.StructuredContent)
	require.NoError(t, mErr)
	require.NoError(t, json.Unmarshal(raw, out), "工具 %s 的 structuredContent 形态不符", name)
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

// TestEndToEnd 全链路：真 MCP HTTP 客户端 → mcpserver → manager →
// resolve（库 + 媒体服务）→ Fake 渲染端，最后核对审计文件 ≥5 行。
func TestEndToEnd(t *testing.T) {
	s := startStack(t)
	cs := connect(t, s.mcpURL)

	// 1. list_cast_devices：唯一设备（静态条目指向 Fake）在线。
	var devs []manager.Device
	call(t, cs, "list_cast_devices", map[string]any{}, &devs)
	require.Len(t, devs, 1)
	assert.Equal(t, devName, devs[0].Name)
	assert.Equal(t, devRoom, devs[0].Room)
	assert.True(t, devs[0].Online, "静态条目指向的 Fake 渲染端应在线")
	assert.Equal(t, "idle", devs[0].State, "初始未播放应为 idle")

	// 2. search_media：找到临时库里的 mp4；媒体服务对该 id 可服务
	//    （MediaBaseURL/MediaServer/Library 三者装配正确性的直接证据）。
	var items []resolve.Item
	call(t, cs, "search_media", map[string]any{"query": "sunset"}, &items)
	require.Len(t, items, 1)
	assert.Equal(t, "sunset-clip", items[0].Title)
	assert.Equal(t, resolve.KindVideo, items[0].Kind)
	require.NotEmpty(t, items[0].ID, "media_id 必须非空")

	mediaResp, err := http.Get(s.mediaURL + "/media/" + items[0].ID)
	require.NoError(t, err)
	mediaResp.Body.Close()
	assert.Equal(t, http.StatusOK, mediaResp.StatusCode, "媒体服务应能按 media_id 回源文件")

	// 3. cast_play：按 media_id 播放，状态进入 playing。
	var play struct {
		Status  adapter.Status `json:"status"`
		Adapter string         `json:"adapter"`
	}
	call(t, cs, "cast_play", map[string]any{"device": devName, "media_id": items[0].ID}, &play)
	assert.Equal(t, "playing", play.Status.State)
	assert.Equal(t, "latticecast", play.Adapter, "adapter 字段固定为 latticecast")

	// 4. cast_status：仍为 playing。
	var st struct {
		Status adapter.Status `json:"status"`
	}
	call(t, cs, "cast_status", map[string]any{"device": devName}, &st)
	assert.Equal(t, "playing", st.Status.State)

	// 5. cast_stop：回 idle。
	call(t, cs, "cast_stop", map[string]any{"device": devName}, &st)
	assert.Equal(t, "idle", st.Status.State)

	// 6. reflux 内容源：search_media 命中 fake reflux（media_id 带 reflux:
	//    前缀），cast_play 据此前缀路由到 reflux 直链；渲染端收到的拉流地址
	//    逐字为 reflux 的 static 直链，且 Resolver.ByID 探测过 reflux 可用性。
	var refluxItems []resolve.Item
	call(t, cs, "search_media", map[string]any{"query": "让子弹飞"}, &refluxItems)
	require.Len(t, refluxItems, 1)
	assert.Equal(t, "reflux:6334", refluxItems[0].ID)
	assert.Equal(t, "让子弹飞", refluxItems[0].Title)

	call(t, cs, "cast_play", map[string]any{
		"device": devName, "media_id": "reflux:6334", "title": "让子弹飞",
	}, &play)
	assert.Equal(t, "playing", play.Status.State)
	assert.Equal(t, s.reflux.URL+"/Videos/6334/stream?static=true&api_key="+refluxToken,
		s.fake.PlayedURL(), "渲染端应收下 reflux 的 static 直链")
	assert.GreaterOrEqual(t, s.reflux.StreamHits(), 1)
	call(t, cs, "cast_stop", map[string]any{"device": devName}, &st)
	assert.Equal(t, "idle", st.Status.State)

	// 审计：8 次工具调用各落一行，每行均为合法 JSON。
	b, err := os.ReadFile(s.auditPath)
	require.NoError(t, err)
	var lines []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	require.GreaterOrEqual(t, len(lines), 8, "8 次调用后审计应至少 8 行")
	for i, line := range lines {
		var e manager.AuditEntry
		require.NoError(t, json.Unmarshal([]byte(line), &e), "审计第 %d 行应为合法 JSON", i+1)
		assert.True(t, e.OK, "审计第 %d 行（%s）应为成功调用", i+1, e.Tool)
		assert.Equal(t, "static-token", e.Agent)
	}
}
