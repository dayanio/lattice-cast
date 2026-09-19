package resolve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// Source.Kind 的取值：Source 是 MediaResolver 下发给渲染端的可拉流地址，
// Kind 说明来源类别。
const (
	KindDirect  = "direct"  // 直链透传：非 YouTube 的 URL 原样下发，渲染端自行拉取
	KindYouTube = "youtube" // 经 Extractor（yt-dlp）从页面地址提取出的直链
	KindNAS     = "nas"     // 家庭 NAS 文件：MediaServer 内网地址（MediaURL 生成）
	KindReflux  = "reflux"  // reflux 内容源：Jellyfin 兼容直链（RefluxSource 生成）
)

// Source 是一次解析的结果：URL 为渲染端可直接拉流的地址，Kind 标注来源类别
// （direct|youtube|nas）。
type Source struct {
	URL  string
	Kind string
}

// Extractor 把页面地址解析成直链媒体地址（如 yt-dlp 对 YouTube 页面的提取）。
type Extractor interface {
	Extract(ctx context.Context, pageURL string) (string, error)
}

// youtubeHosts 视为 YouTube 的主机名集合（Host 去端口后小写比较）。
var youtubeHosts = map[string]bool{
	"youtube.com":     true,
	"www.youtube.com": true,
	"m.youtube.com":   true,
	"youtu.be":        true,
}

// Resolver 把用户请求解析成渲染端可拉的 Source：
//   - Search：NAS 库 ∪ reflux 库（reflux 失败降级为仅 NAS，见 Search 注释）；
//   - ByID：reflux: 前缀 id → RefluxSource 直链（Kind reflux）；库内媒体 →
//     MediaServer 内网直链（Kind nas）；
//   - ByURL：YouTube 域名 → Extractor 提流（未配置 Extractor 则报
//     youtube_disabled），其余一律原样透传（Kind direct，不联网、不校验）。
type Resolver struct {
	Lib  *Library  // 媒体库（ByID 依赖）
	Base string    // MediaServer 对外 base URL，如 http://192.168.1.10:7810
	Ext  Extractor // 可为 nil：禁用 YouTube 提流

	// Reflux 是可选的 reflux 内容源（用户自建媒体服务器，Jellyfin 兼容 API）；
	// nil = 禁用：Search 只查 NAS 库，reflux: 前缀 id 报 reflux_disabled。
	Reflux *RefluxSource
}

// Search 检索 media_id：NAS 库命中 ∪ reflux 库命中（reflux 条目带 reflux:
// 前缀，ID 跨库无歧义）。reflux 不可达或鉴权失败（含 401）一律降级：slog.Warn
// 后仅返回 NAS 结果——reflux 是增益内容源，不得拖垮本地库搜索。可安全并发调用。
func (r *Resolver) Search(ctx context.Context, q string) []Item {
	items := r.Lib.Search(q)
	if r.Reflux == nil {
		return items
	}
	remote, err := r.Reflux.Search(ctx, q)
	if err != nil {
		slog.Warn("reflux search failed, serving NAS results only", "err", err)
		return items
	}
	return append(items, remote...)
}

// ByID 按 media_id 解析：
//   - "reflux:" 前缀 → RefluxSource.StreamURL（Kind reflux）；未配置 reflux
//     报 "reflux_disabled"，reflux 不可用报结构化错误（reflux stream …）；
//   - 其余 id 查 NAS 库：未知 ID 返回 "unknown_media_id: …"；命中则返回
//     MediaServer 内网地址（Lib.MediaURL(Base, id)，Kind nas）。
func (r *Resolver) ByID(ctx context.Context, id string) (Source, error) {
	if rest, ok := strings.CutPrefix(id, refluxIDPrefix); ok {
		if r.Reflux == nil {
			return Source{}, errors.New("reflux_disabled")
		}
		u, err := r.Reflux.StreamURL(ctx, rest)
		if err != nil {
			return Source{}, err
		}
		return Source{URL: u, Kind: KindReflux}, nil
	}
	if _, ok := r.Lib.Get(id); !ok {
		return Source{}, fmt.Errorf("unknown_media_id: %s", id)
	}
	return Source{URL: r.Lib.MediaURL(r.Base, id), Kind: KindNAS}, nil
}

// ByURL 按 URL 解析：
//   - 主机名为 youtube.com / www.youtube.com / m.youtube.com / youtu.be 之一
//     时交给 Extractor 提流（Kind youtube）；Ext 为 nil 报 "youtube_disabled"；
//   - 其余字符串原样透传（Kind direct）——不发起网络请求、不做合法性校验、
//     不改写输入（URL 解析失败也按非 YouTube 透传）。
func (r *Resolver) ByURL(ctx context.Context, raw string) (Source, error) {
	if isYouTubeURL(raw) {
		if r.Ext == nil {
			return Source{}, errors.New("youtube_disabled")
		}
		media, err := r.Ext.Extract(ctx, raw)
		if err != nil {
			return Source{}, err
		}
		return Source{URL: media, Kind: KindYouTube}, nil
	}
	return Source{URL: raw, Kind: KindDirect}, nil
}

// isYouTubeURL 判断 raw 是否指向 YouTube 域名；解析失败按非 YouTube 处理。
func isYouTubeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return youtubeHosts[strings.ToLower(u.Hostname())]
}
