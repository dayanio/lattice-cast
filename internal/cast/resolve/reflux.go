package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RefluxSource 对接用户自建的 reflux 媒体服务器（索引 115 网盘等，TMDB 元数据，
// 对外暴露 Jellyfin 兼容 API）。实现依据 reflux 产品源码
// （github.com/refluxio/reflux/jellyfin）而非通用 Jellyfin 猜测：
//
//   - 检索：register.go 把同一套路由注册在 /jellyfin、/emby 与根路径三种前缀下，
//     检索端点是 /Users/{userId}/Items（items.go getItems 只读 searchTerm 等
//     查询参数、不校验路径中的 userId），响应形态 {"Items":[…],"TotalRecordCount":N}，
//     条目字段见 items.go toJellyfinSeries/toJellyfinItem（Id/Name/Type/…）；
//   - 拉流：/Videos/{itemId}/stream（register.go），getStream 支持按 DB 主键或
//     TMDB ID 寻址、按 Range 起播，stream?static=true 即直链语义；
//   - 鉴权：auth.go getTokenFromRequest 首选 api_key 查询参数，失败统一回
//     401 {"error":"unauthorized"}——据此 401 可判为 token 失效。
//
// Item.ID 打上 "reflux:" tagged 前缀（如 "reflux:6334"）：media_id 在 NAS 库
// （无前缀 sha1）与 reflux 库之间保持无歧义，ByID 据前缀路由。
type RefluxSource struct {
	Base  string // 实例基址，如 http://192.168.1.20:8096（尾部斜杠归一化）
	Token string // api_key（reflux 的 Jellyfin token）

	HC *http.Client // 为 nil 时 Search/StreamURL 内部补默认客户端
}

// refluxIDPrefix 是 reflux 条目在 media_id 命名空间里的 tagged 前缀。
const refluxIDPrefix = "reflux:"

// refluxSearchUser 占位 userId：reflux 的 getItems 不读路径中的 userId，
// 任意值可达（不可把用户 token 换成 userId，reflux 亦无 /Users/Me 端点）。
const refluxSearchUser = "reflux"

// searchLimit 单次检索条数上限：与 NAS 库的 searchMax 对齐，防全库倾泻。
const searchLimit = 20

// NewRefluxSource 创建指向 base、以 token 鉴权的 reflux 内容源。
func NewRefluxSource(base, token string) *RefluxSource {
	return &RefluxSource{
		Base:  strings.TrimRight(base, "/"),
		Token: token,
		HC:    &http.Client{Timeout: 10 * time.Second},
	}
}

// jellyfinItemsResp 是 /Users/{userId}/Items 的响应外壳（items.go getItems）。
type jellyfinItemsResp struct {
	Items            []jellyfinItem `json:"Items"`
	TotalRecordCount int            `json:"TotalRecordCount"`
}

// jellyfinItem 只取映射需要的字段（reflux 响应字段是超集，多出的忽略）。
type jellyfinItem struct {
	ID            string `json:"Id"` // 字符串：DB 主键 / TMDB ID / 分组 ID
	Name          string `json:"Name"`
	OriginalTitle string `json:"OriginalTitle"`
	Type          string `json:"Type"`      // Movie | Series | Episode | Audio | …
	MediaType     string `json:"MediaType"` // Video | Audio | Photo
}

// Search 在 reflux 库按标题检索：GET {Base}/Users/{userId}/Items
// ?searchTerm=<q>&Limit=<n>&api_key=<token>，把 Jellyfin 条目映射成 resolve.Item。
//   - 连接失败 → 包裹底层原因的错误（reflux search: …）；
//   - 非 2xx → 携带状态码；401 → 含 reflux_auth_failed（token 失效，可区分）。
func (r *RefluxSource) Search(ctx context.Context, q string) ([]Item, error) {
	u := fmt.Sprintf("%s/Users/%s/Items?searchTerm=%s&Limit=%d&api_key=%s",
		r.Base, refluxSearchUser, url.QueryEscape(q), searchLimit, url.QueryEscape(r.Token))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("reflux search: %w", redactTransportErr(err))
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("reflux search: %w", redactTransportErr(err))
	}
	defer resp.Body.Close()
	if err := checkRefluxStatus(resp, "reflux search"); err != nil {
		return nil, err
	}

	var parsed jellyfinItemsResp
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("reflux search: decode response: %w", err)
	}

	items := make([]Item, 0, len(parsed.Items))
	for _, jf := range parsed.Items {
		id := strings.TrimSpace(jf.ID)
		if id == "" {
			continue // reflux 不会省略 Id；防御性跳过避免产生 "reflux:" 裸前缀
		}
		title := jf.Name
		if title == "" {
			title = jf.OriginalTitle
		}
		if title == "" {
			title = id
		}
		items = append(items, Item{
			ID:    refluxIDPrefix + id,
			Title: title,
			Kind:  kindFromJellyfin(jf.Type, jf.MediaType),
		})
	}
	return items, nil
}

// StreamURL 返回条目的 reflux 直链 {Base}/Videos/{id}/stream?static=true
// &api_key=<token>。构造前发一次 bytes=0-0 的轻量 Range 探测（reflux 对此类
// 探测走直通路径，不建 StreamBuffer），保证返回的地址当下可用——不可达/
// token 失效/条目不存在都以结构化错误暴露，而不是等渲染端拉流才失败。
func (r *RefluxSource) StreamURL(ctx context.Context, id string) (string, error) {
	streamURL := fmt.Sprintf("%s/Videos/%s/stream?static=true&api_key=%s",
		r.Base, url.PathEscape(id), url.QueryEscape(r.Token))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return "", fmt.Errorf("reflux stream %s: %w", id, redactTransportErr(err))
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := r.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("reflux stream %s: %w", id, redactTransportErr(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1)) // 探测：不消费完整流
	if err := checkRefluxStatus(resp, fmt.Sprintf("reflux stream %s", id)); err != nil {
		return "", err
	}
	return streamURL, nil
}

// checkRefluxStatus 把非 2xx 归一为可转述的结构化错误；401 判为 token 失效
// （reflux auth.go 对无效 token 统一回 401 {"error":"unauthorized"}）。
func checkRefluxStatus(resp *http.Response, prefix string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%s: status 401: reflux_auth_failed", prefix)
	}
	return fmt.Errorf("%s: status %d", prefix, resp.StatusCode)
}

// redactTransportErr 把传输层错误改写成不含凭证的等价错误。*url.Error 的文本
// 内嵌完整请求 URL（含 api_key=<token>），而这段文本会一路进审计 JSONL、
// slog.Warn 与 LLM 转录——reflux 不可达是常态故障，凭证明文落盘即泄露。
// 改写为 "<op> <scheme://host/path（去查询串/用户信息/片段）>: <底层原因>"，
// 底层原因仍以 %w 包裹保留（errors.Is/As 对连接错误的判定不受影响）。
func redactTransportErr(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	safeURL := ue.URL
	if parsed, perr := url.Parse(ue.URL); perr == nil {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		parsed.User = nil
		safeURL = parsed.String()
	}
	return fmt.Errorf("%s %s: %w", ue.Op, safeURL, ue.Err)
}

// defaultHTTPClient 是零值 RefluxSource 兜底的 HTTP 客户端：带超时——
// http.DefaultClient 无超时，实例半开（接受 TCP 却不响应）会把检索/拉流
// 永久挂起。
var defaultHTTPClient = &http.Client{Timeout: 10 * time.Second}

// client 兜底 HC（零值 RefluxSource 仍可用，且必带超时）。
func (r *RefluxSource) client() *http.Client {
	if r.HC != nil {
		return r.HC
	}
	return defaultHTTPClient
}

// kindFromJellyfin 把 Jellyfin 条目类别映射成 resolve 的 video/audio/image；
// 未知类型按视频兜底（reflux 语料以影视为主）。
func kindFromJellyfin(jfType, mediaType string) string {
	switch strings.ToLower(jfType) {
	case "audio", "musicalbum", "audiobook":
		return KindAudio
	case "photo", "image":
		return KindImage
	}
	switch strings.ToLower(mediaType) {
	case "audio":
		return KindAudio
	case "photo":
		return KindImage
	}
	return KindVideo
}
