// Package mcpserver 是 cast-agent 的 MCP 工具层：外部 LLM 客户端
// （Claude Desktop 等）经 LatticeCast 网格抵达的唯一表面。工具名、参数名
// 与错误文本是产品契约，不得擅改：
//
//	list_cast_devices() → []manager.Device
//	search_media(query) → []resolve.Item
//	cast_play(device, media_id?, url?, title?) → {"status":…,"adapter":"latticecast"}
//	cast_stop(device) / cast_status(device) → {"status":…}
//	cast_pause(device) → {"status":…}
//	cast_seek(device, position_ms) → {"status":…}
//	cast_volume(device, level 0-100) → {"status":…}
//
// 每次调用（含失败）经 audit.Record 落一行 JSONL，agent 署名取自鉴权层。
// 工具的执行核心（core* / doPlay）同时供内置大脑（internal/cast/brain）经
// Executor() 适配器内部直调——与 MCP handler 完全同一份实现与审计路径。
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dayanio/lattice-cast/internal/cast/adapter"
	"github.com/dayanio/lattice-cast/internal/cast/manager"
	"github.com/dayanio/lattice-cast/internal/cast/resolve"
)

// Server 持有 MCP 工具层的全部依赖。经 New 挂载 8 个固定契约的工具，
// HTTP() 给出套了鉴权中间件的 streamable HTTP 端点。
type Server struct {
	mgr   *manager.Manager
	lib   *resolve.Library
	res   *resolve.Resolver
	audit *manager.AuditLog
	auth  Authenticator
	mcp   *mcp.Server
}

// 工具入参（json 名即 LLM 侧参数名，固定）。omitempty 使可选字段不进
// required：cast_play 的 media_id/url/title 三者可选，二者必传其一由
// handler 的 exactly-one 规则裁决（schema 层无法表达）。
type playIn struct {
	Device  string `json:"device"`
	MediaID string `json:"media_id,omitempty"`
	URL     string `json:"url,omitempty"`
	Title   string `json:"title,omitempty"`
	// PositionMS 起播/续播位置（毫秒，可选）：LLM 续播模式（cast_status →
	// 带 position_ms 重播）依赖该参数；协议 /play 与 adapter.PlayRequest
	// 本就携带，此处仅补齐 schema 暴露与透传。
	PositionMS int64 `json:"position_ms,omitempty"`
}

type deviceIn struct {
	Device string `json:"device"`
}

type volumeIn struct {
	Device string `json:"device"`
	Level  int    `json:"level"`
}

// seekIn 的 position_ms 无 omitempty：schema 层即为必填（LLM 侧必须显式给出
// 跳转目标）；负数由 handler 以固定文本 position_out_of_range 拒绝。
type seekIn struct {
	Device     string `json:"device"`
	PositionMS int64  `json:"position_ms"`
}

type searchIn struct {
	Query string `json:"query"`
}

// listIn 是 list_cast_devices 的空入参（schema 推断为空 object）。
type listIn struct{}

// 工具出参（structuredContent 的 JSON 形态，固定）。
type statusOut struct {
	Status adapter.Status `json:"status"`
}

type playOut struct {
	Status  adapter.Status `json:"status"`
	Adapter string         `json:"adapter"`
}

// New 构造 Server 并挂载 8 个工具。SDK v1.8.0 的工具挂载用泛型
// mcp.AddTool[In, Out]：入参自动反序列化并按推断 schema 校验，返回的
// 非 nil error 自动包成 IsError 结果（错误文本即 Content[0].Text，
// LLM 所见的字符串）。
func New(mgr *manager.Manager, lib *resolve.Library, res *resolve.Resolver, audit *manager.AuditLog, auth Authenticator) *Server {
	s := &Server{mgr: mgr, lib: lib, res: res, audit: audit, auth: auth}
	srv := mcp.NewServer(&mcp.Implementation{Name: "lattice-cast", Version: "0.1.0"}, nil)

	mcp.AddTool[listIn, []manager.Device](srv,
		&mcp.Tool{Name: "list_cast_devices", Description: "List cast devices with online state and what is playing."},
		s.listDevices)
	mcp.AddTool[searchIn, []resolve.Item](srv,
		&mcp.Tool{Name: "search_media", Description: "Search media by title substring: NAS library plus configured reflux server; returns media_id for cast_play (reflux ids carry a reflux: prefix)."},
		s.searchMedia)
	mcp.AddTool[playIn, playOut](srv,
		&mcp.Tool{Name: "cast_play", Description: "Play media on a device by media_id (library) or url (direct/YouTube page); pass exactly one."},
		s.play)
	mcp.AddTool[deviceIn, statusOut](srv,
		&mcp.Tool{Name: "cast_stop", Description: "Stop playback on a device."},
		s.stop)
	mcp.AddTool[deviceIn, statusOut](srv,
		&mcp.Tool{Name: "cast_pause", Description: "Pause playback on a device."},
		s.pause)
	mcp.AddTool[seekIn, statusOut](srv,
		&mcp.Tool{Name: "cast_seek", Description: "Seek on a device to an absolute position in milliseconds."},
		s.seek)
	mcp.AddTool[volumeIn, statusOut](srv,
		&mcp.Tool{Name: "cast_volume", Description: "Set device volume 0-100."},
		s.volume)
	mcp.AddTool[deviceIn, statusOut](srv,
		&mcp.Tool{Name: "cast_status", Description: "Get a device's current playback state."},
		s.status)
	s.mcp = srv
	return s
}

// HTTP 返回 streamable HTTP 端点：先过鉴权中间件（失败 → 401
// {"error":"unauthorized"}），通过后把 agent 署名注入请求上下文供审计。
func (s *Server) HTTP() http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp }, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent, ok := s.auth.Validate(r)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		streamable.ServeHTTP(w, r.WithContext(withAgent(r.Context(), agent)))
	})
}

// record 把一次工具调用（成功或失败）落一行审计。审计是尽力而为的旁路，
// 序列化失败不阻断主流程（Record 内部亦然）。
func (s *Server) record(ctx context.Context, tool string, args, out any, err error, start time.Time) {
	var result string
	if err != nil {
		result = err.Error()
	} else if b, mErr := json.Marshal(out); mErr == nil {
		result = string(b)
	} else {
		result = mErr.Error()
	}
	argsJSON, mErr := json.Marshal(args)
	if mErr != nil {
		argsJSON = []byte(mErr.Error())
	}
	s.audit.Record(agentFrom(ctx), tool, string(argsJSON), result, err == nil, time.Since(start))
}

// ---- 工具执行核心（MCP handler 与内置大脑的 ToolExecutor 适配器共用）----
//
// 每个核心只做参数裁决与业务执行，不记审计、不碰 MCP 类型：审计由两个调用
// 方各自经 record 落行（同一 JSONL、同一格式）。

func (s *Server) coreListDevices(ctx context.Context) ([]manager.Device, error) {
	return s.mgr.List(ctx)
}

// coreSearchMedia 检索永不失败：reflux 不可达已在 Resolver 内降级为仅 NAS。
func (s *Server) coreSearchMedia(ctx context.Context, in searchIn) ([]resolve.Item, error) {
	return s.res.Search(ctx, in.Query), nil
}

// doPlay 是 cast_play 的执行核心：exactly-one 规则与解析链。media_id →
// Resolver.ByID，url → ByURL；解析错误原样透传（youtube_disabled /
// unknown_media_id 等），随后交 Manager.Play（unknown_device / device_offline
// 亦原样上抛）。
func (s *Server) doPlay(ctx context.Context, in playIn) (playOut, error) {
	switch {
	case in.MediaID == "" && in.URL == "":
		return playOut{}, errors.New("media_id_or_url_required")
	case in.MediaID != "" && in.URL != "":
		return playOut{}, errors.New("media_id_url_exclusive")
	}

	var src resolve.Source
	var err error
	if in.MediaID != "" {
		src, err = s.res.ByID(ctx, in.MediaID)
	} else {
		src, err = s.res.ByURL(ctx, in.URL)
	}
	if err != nil {
		return playOut{}, err
	}

	st, err := s.mgr.Play(ctx, in.Device, adapter.PlayRequest{URL: src.URL, Title: in.Title, PositionMS: in.PositionMS})
	if err != nil {
		return playOut{}, err
	}
	return playOut{Status: st, Adapter: "latticecast"}, nil
}

func (s *Server) coreStop(ctx context.Context, in deviceIn) (statusOut, error) {
	st, err := s.mgr.Stop(ctx, in.Device)
	return statusOut{Status: st}, err
}

func (s *Server) corePause(ctx context.Context, in deviceIn) (statusOut, error) {
	st, err := s.mgr.Pause(ctx, in.Device)
	return statusOut{Status: st}, err
}

// coreSeek 含契约预检：负数位置以固定文本 position_out_of_range 拒绝，
// 不下发网络请求。
func (s *Server) coreSeek(ctx context.Context, in seekIn) (statusOut, error) {
	if in.PositionMS < 0 {
		return statusOut{}, errors.New("position_out_of_range")
	}
	st, err := s.mgr.Seek(ctx, in.Device, in.PositionMS)
	return statusOut{Status: st}, err
}

// coreVolume 含契约预检：越界音量以固定文本 level_out_of_range 拒绝（0 与
// 100 为合法边界），不下发网络请求；Manager 的带前缀版本仅供 Go 直调方使用。
func (s *Server) coreVolume(ctx context.Context, in volumeIn) (statusOut, error) {
	if in.Level < 0 || in.Level > 100 {
		return statusOut{}, errors.New("level_out_of_range")
	}
	st, err := s.mgr.Volume(ctx, in.Device, in.Level)
	return statusOut{Status: st}, err
}

func (s *Server) coreStatus(ctx context.Context, in deviceIn) (statusOut, error) {
	st, err := s.mgr.Status(ctx, in.Device)
	return statusOut{Status: st}, err
}

// coreResume 是 cast_resume 的执行核心：按 Manager 断点记忆续播（同 URL
// 同位置重新起播）。仅供内置大脑的意图快通道（internal/cast/intent）经
// Executor() 直调——刻意不进 MCP 注册表、不在 brain 的 toolDefs 里：
// LLM 看不到该工具名，续播是本地规则引擎的专属能力。
func (s *Server) coreResume(ctx context.Context, in deviceIn) (playOut, error) {
	st, err := s.mgr.Resume(ctx, in.Device)
	return playOut{Status: st, Adapter: "latticecast"}, err
}

// ---- 八个工具 handler（薄壳：执行核心 + 审计）----

func (s *Server) listDevices(ctx context.Context, _ *mcp.CallToolRequest, _ listIn) (*mcp.CallToolResult, []manager.Device, error) {
	start := time.Now()
	devices, err := s.coreListDevices(ctx)
	s.record(ctx, "list_cast_devices", listIn{}, devices, err, start)
	return nil, devices, err
}

func (s *Server) searchMedia(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, []resolve.Item, error) {
	start := time.Now()
	items, err := s.coreSearchMedia(ctx, in)
	s.record(ctx, "search_media", in, items, err, start)
	return nil, items, err
}

func (s *Server) play(ctx context.Context, _ *mcp.CallToolRequest, in playIn) (*mcp.CallToolResult, playOut, error) {
	start := time.Now()
	out, err := s.doPlay(ctx, in)
	s.record(ctx, "cast_play", in, out, err, start)
	return nil, out, err
}

func (s *Server) stop(ctx context.Context, _ *mcp.CallToolRequest, in deviceIn) (*mcp.CallToolResult, statusOut, error) {
	start := time.Now()
	out, err := s.coreStop(ctx, in)
	s.record(ctx, "cast_stop", in, out, err, start)
	return nil, out, err
}

func (s *Server) pause(ctx context.Context, _ *mcp.CallToolRequest, in deviceIn) (*mcp.CallToolResult, statusOut, error) {
	start := time.Now()
	out, err := s.corePause(ctx, in)
	s.record(ctx, "cast_pause", in, out, err, start)
	return nil, out, err
}

func (s *Server) seek(ctx context.Context, _ *mcp.CallToolRequest, in seekIn) (*mcp.CallToolResult, statusOut, error) {
	start := time.Now()
	out, err := s.coreSeek(ctx, in)
	s.record(ctx, "cast_seek", in, out, err, start)
	return nil, out, err
}

func (s *Server) volume(ctx context.Context, _ *mcp.CallToolRequest, in volumeIn) (*mcp.CallToolResult, statusOut, error) {
	start := time.Now()
	out, err := s.coreVolume(ctx, in)
	s.record(ctx, "cast_volume", in, out, err, start)
	return nil, out, err
}

func (s *Server) status(ctx context.Context, _ *mcp.CallToolRequest, in deviceIn) (*mcp.CallToolResult, statusOut, error) {
	start := time.Now()
	out, err := s.coreStatus(ctx, in)
	s.record(ctx, "cast_status", in, out, err, start)
	return nil, out, err
}
