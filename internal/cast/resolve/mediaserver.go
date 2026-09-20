package resolve

import (
	"net"
	"net/http"
)

// MediaServer 在家庭内网提供只读媒体文件服务：GET /media/{id} 依 Library
// 索引取出文件用 http.ServeFile 应答（天然支持 Range，渲染端拖动进度可用）。
//
// 安全边界：
//   - 只读：仅注册 GET（HEAD 由 GET 模式顺带覆盖），其余方法一律 405；
//   - 无目录列表：索引中的 Path 在 Rescan 时已校验为常规文件；
//   - 无路径穿越：URL 中的 id 只是索引键（路径哈希），穿越形态的请求串
//     （如 ..%2f..%2fetc%2fpasswd）查不到索引，一律 404 纯文本，绝不落盘拼路径。
type MediaServer struct {
	lib    *Library
	listen string
	srv    *http.Server
}

// NewMediaServer 创建绑定在 listen（如 "0.0.0.0:7810"）上的只读媒体服务，
// 文件均经 lib 索引只读取用。
func NewMediaServer(lib *Library, listen string) *MediaServer {
	return &MediaServer{lib: lib, listen: listen}
}

// Start 监听地址并启动服务 goroutine；绑定失败（端口占用等）同步返回错误。
func (s *MediaServer) Start() error {
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return err
	}
	s.srv = &http.Server{Handler: s.handler()}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Close 关闭监听与活动连接，终止服务；未启动过则返回 nil。
func (s *MediaServer) Close() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

// handler 组装路由：GET /media/{id} → 索引命中则 ServeFile、未命中 404；
// 其余方法 → 405。未注册的路径由 ServeMux 兜底 404。
func (s *MediaServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /media/{id}", s.serveMedia)
	mux.HandleFunc("/media/{id}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
	return mux
}

// serveMedia 依索引取文件并 ServeFile；item.Path 来自扫描期归一化结果，
// 非用户输入，不存在穿越风险。
func (s *MediaServer) serveMedia(w http.ResponseWriter, r *http.Request) {
	item, ok := s.lib.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "media not found", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, item.Path)
}
