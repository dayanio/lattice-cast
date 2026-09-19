// Package fakereflux 提供内存态 reflux 测试替身：按 reflux 产品源码
// （github.com/refluxio/reflux/jellyfin 的 register.go 路由、items.go 响应形态、
// auth.go 鉴权）实现内容源集成所需的最小 Jellyfin 兼容面：
//
//	GET /Users/{userId}/Items?searchTerm=…&api_key=…   → {"Items":[…],"TotalRecordCount":N}
//	GET /Videos/{id}/stream?static=true&api_key=…      → 媒体字节（200）或 404
//
// 鉴权与真实产品一致：token 只认 api_key 查询参数（auth.go getTokenFromRequest
// 的第一优先级），不符回 401 {"error":"unauthorized"}。条目 JSON 形态取自
// items.go 的 toJellyfinSeries / toJellyfinItem（Id/Name/Type/MediaType 等字段）。
package fakereflux

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// streamBytes 是 /Videos/{id}/stream 回的字节（内容无所谓，头部照抄 reflux：
// Accept-Ranges: bytes + video/x-matroska）。
const streamBytes = "reflux-stream-bytes"

// Fake 是内存态 reflux，内嵌 *httptest.Server（监听 127.0.0.1 随机端口）。
type Fake struct {
	*httptest.Server

	token string

	mu              sync.Mutex
	items           []map[string]any // 检索语料（Jellyfin 条目 JSON）
	searchHits      int
	streamHits      int
	lastSearchPath  string
	lastSearchQuery url.Values
	lastStreamPath  string
	lastStreamQuery url.Values
	lastStreamRange string
}

// New 启动一个监听随机端口的 Fake reflux，token 为合法 api_key。
func New(token string) *Fake {
	f := &Fake{token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/Users/", f.handleItems)
	mux.HandleFunc("/Videos/", f.handleStream)
	f.Server = httptest.NewServer(mux)
	return f
}

// SetItems 设置检索语料（Jellyfin 条目 JSON，Id/Name/Type/MediaType 形态）。
func (f *Fake) SetItems(items ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = items
}

// SearchHits 返回 /Users/{userId}/Items 被合法调用的次数。
func (f *Fake) SearchHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searchHits
}

// StreamHits 返回 /Videos/{id}/stream 被合法调用的次数。
func (f *Fake) StreamHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streamHits
}

// LastSearch 返回最近一次检索的路径与查询串（鉴权失败调用不记录）。
func (f *Fake) LastSearch() (path string, query url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSearchPath, f.lastSearchQuery
}

// LastStream 返回最近一次拉流的路径、查询串与 Range 头。
func (f *Fake) LastStream() (path string, query url.Values, rangeHdr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastStreamPath, f.lastStreamQuery, f.lastStreamRange
}

// authorized 校验 api_key 查询参数（reflux auth.go 的第一优先级 token 载体）。
func (f *Fake) authorized(q url.Values) bool {
	return q.Get("api_key") == f.token
}

// handleItems 复刻 reflux jellyfin.getItems 的检索面：按 Name 子串过滤，
// 响应 {"Items":[…],"TotalRecordCount":N}。
func (f *Fake) handleItems(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/Items") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if !f.authorized(r.URL.Query()) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	needle := strings.ToLower(r.URL.Query().Get("searchTerm"))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchHits++
	f.lastSearchPath = r.URL.Path
	f.lastSearchQuery = r.URL.Query()

	matched := make([]map[string]any, 0, len(f.items))
	for _, it := range f.items {
		name, _ := it["Name"].(string)
		if needle == "" || strings.Contains(strings.ToLower(name), needle) {
			matched = append(matched, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Items":            matched,
		"TotalRecordCount": len(matched),
	})
}

// handleStream 复刻 reflux jellyfin.getStream 的寻址面：{id} 命中语料回媒体
// 字节，未命中回 404 {"error":"not found"}。
func (f *Fake) handleStream(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r.URL.Query()) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	id := streamID(r.URL.Path)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamHits++
	f.lastStreamPath = r.URL.Path
	f.lastStreamQuery = r.URL.Query()
	f.lastStreamRange = r.Header.Get("Range")

	for _, it := range f.items {
		if got, _ := it["Id"].(string); got == id {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Type", "video/x-matroska")
			w.Header().Set("Content-Length", strconv.Itoa(len(streamBytes)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(streamBytes))
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

// streamID 从 /Videos/{id}/stream(.ext) 形态的路径里取出条目 ID。
func streamID(path string) string {
	trimmed := strings.TrimPrefix(path, "/Videos/")
	if i := strings.Index(trimmed, "/"); i >= 0 {
		trimmed = trimmed[:i]
	}
	return strings.TrimSuffix(trimmed, ".mkv")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
