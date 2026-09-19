// Package mcpserver 是 cast-agent 的 MCP 工具层：外部 LLM 客户端
// （Claude Desktop 等）经 LatticeCast 网格抵达的唯一表面。工具名、参数名
// 与错误文本是产品契约，不得擅改：
//
//	list_cast_devices() → []manager.Device
//	search_media(query) → []resolve.Item
//	cast_play(device, media_id?, url?, title?) → {"status":…,"adapter":"latticecast"}
//	cast_stop(device) / cast_status(device) → {"status":…}
//	cast_volume(device, level 0-100) → {"status":…}
//
// 每次调用（含失败）经 audit.Record 落一行 JSONL，agent 署名取自鉴权层。
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

// Server 持有 MCP 工具层的全部依赖。经 New 挂载 6 个固定契约的工具，
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
}

type deviceIn struct {
	Device string `json:"device"`
}

type volumeIn struct {
	Device string `json:"device"`
	Level  int    `json:"level"`
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

// New 构造 Server 并挂载 6 个工具。SDK v1.8.0 的工具挂载用泛型
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
		&mcp.Tool{Name: "search_media", Description: "Search the NAS media library by title substring; returns media_id for cast_play."},
		s.searchMedia)
	mcp.AddTool[playIn, playOut](srv,
		&mcp.Tool{Name: "cast_play", Description: "Play media on a device by media_id (library) or url (direct/YouTube page); pass exactly one."},
		s.play)
	mcp.AddTool[deviceIn, statusOut](srv,
		&mcp.Tool{Name: "cast_stop", Description: "Stop playback on a device."},
		s.stop)
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

// ---- 六个工具 handler（返回 nil result：由 SDK 以 Out 填充 structuredContent）----

func (s *Server) listDevices(ctx context.Context, _ *mcp.CallToolRequest, _ listIn) (*mcp.CallToolResult, []manager.Device, error) {
	start := time.Now()
	devices, err := s.mgr.List(ctx)
	s.record(ctx, "list_cast_devices", listIn{}, devices, err, start)
	return nil, devices, err
}

func (s *Server) searchMedia(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, []resolve.Item, error) {
	start := time.Now()
	items := s.lib.Search(in.Query) // 检索不失败：无命中即空数组
	s.record(ctx, "search_media", in, items, nil, start)
	return nil, items, nil
}

func (s *Server) play(ctx context.Context, _ *mcp.CallToolRequest, in playIn) (*mcp.CallToolResult, playOut, error) {
	start := time.Now()
	out, err := s.doPlay(ctx, in)
	s.record(ctx, "cast_play", in, out, err, start)
	return nil, out, err
}

// doPlay 实现 cast_play 的 exactly-one 规则与解析链：media_id → Resolver.ByID，
// url → ByURL；解析错误原样透传（youtube_disabled / unknown_media_id 等），
// 随后交 Manager.Play（unknown_device / device_offline 亦原样上抛）。
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

	st, err := s.mgr.Play(ctx, in.Device, adapter.PlayRequest{URL: src.URL, Title: in.Title})
	if err != nil {
		return playOut{}, err
	}
	return playOut{Status: st, Adapter: "latticecast"}, nil
}

func (s *Server) stop(ctx context.Context, _ *mcp.CallToolRequest, in deviceIn) (*mcp.CallToolResult, statusOut, error) {
	start := time.Now()
	st, err := s.mgr.Stop(ctx, in.Device)
	out := statusOut{Status: st}
	s.record(ctx, "cast_stop", in, out, err, start)
	return nil, out, err
}

func (s *Server) volume(ctx context.Context, _ *mcp.CallToolRequest, in volumeIn) (*mcp.CallToolResult, statusOut, error) {
	start := time.Now()
	st, err := s.mgr.Volume(ctx, in.Device, in.Level) // 越界由 Manager 拒绝：level_out_of_range: <n>
	out := statusOut{Status: st}
	s.record(ctx, "cast_volume", in, out, err, start)
	return nil, out, err
}

func (s *Server) status(ctx context.Context, _ *mcp.CallToolRequest, in deviceIn) (*mcp.CallToolResult, statusOut, error) {
	start := time.Now()
	st, err := s.mgr.Status(ctx, in.Device)
	out := statusOut{Status: st}
	s.record(ctx, "cast_status", in, out, err, start)
	return nil, out, err
}
