package resolve

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dayanio/lattice-cast/internal/cast/testsupport/fakereflux"
)

// refluxToken / refluxMovieID 是 reflux 用例共用常量：token 与 fake 语料里的
// 电影条目 Id（形态取自 reflux jellyfin/items.go 的 toJellyfinItem）。
const (
	refluxToken   = "tok-reflux"
	refluxMovieID = "6334"
)

// refluxFixtures 造一批 Jellyfin 风格条目：剧集（toJellyfinSeries 形态）、
// 电影（grouped Movie 形态）、单集（flat Episode 形态）、音频。
// 字段名与 reflux 源码逐一对应：Id/Name/OriginalTitle/Type/MediaType/Year/
// ProviderIds/IsFolder。
func refluxFixtures() []map[string]any {
	return []map[string]any{
		{
			"Id":            "118805",
			"Name":          "琅琊榜",
			"OriginalTitle": "Nirvana in Fire",
			"Type":          "Series",
			"MediaType":     "Video",
			"IsFolder":      true,
			"Year":          2015,
			"ProviderIds":   map[string]any{"Tmdb": "118805"},
		},
		{
			"Id":            refluxMovieID,
			"Name":          "让子弹飞",
			"OriginalTitle": "Let the Bullets Fly",
			"Type":          "Movie",
			"MediaType":     "Video",
			"IsFolder":      false,
			"Year":          2010,
			"ProviderIds":   map[string]any{"Tmdb": refluxMovieID},
		},
		{
			"Id":                "65714",
			"Name":              "大秦帝国之裂变 EP01",
			"OriginalTitle":     "The Qin Empire EP01",
			"Type":              "Episode",
			"MediaType":         "Video",
			"IsFolder":          false,
			"SeriesId":          "22116",
			"ParentIndexNumber": 1,
			"IndexNumber":       1,
		},
		{
			"Id":        "77",
			"Name":      "夜曲",
			"Type":      "Audio",
			"MediaType": "Audio",
		},
	}
}

// TestRefluxSearchMapsJellyfinItems 主用例：GET {Base}/Users/{userId}/Items
// 携带 searchTerm + api_key；响应条目逐字段映射成 resolve.Item——
// ID 加 "reflux:" 前缀、Title 取 Name、Kind 按 Type/MediaType 映射、Path 不外泄。
func TestRefluxSearchMapsJellyfinItems(t *testing.T) {
	fake := fakereflux.New(refluxToken)
	fake.SetItems(refluxFixtures()...)
	t.Cleanup(fake.Close)

	src := NewRefluxSource(fake.URL, refluxToken)
	items, err := src.Search(context.Background(), "")
	require.NoError(t, err)

	path, query := fake.LastSearch()
	require.NotEmpty(t, path, "应发出过一次检索请求")
	assert.True(t, len(path) > 4 && path[len(path)-6:] == "/Items", "检索应打在 /Users/{userId}/Items 上，实际 %s", path)
	require.NotNil(t, query)
	assert.Equal(t, refluxToken, query.Get("api_key"), "token 应走 api_key 查询参数（reflux auth.go）")

	require.Len(t, items, 4, "空检索词应返回全部语料")

	byID := map[string]Item{}
	for _, it := range items {
		byID[it.ID] = it
		assert.Empty(t, it.Path, "reflux 条目不得携带本地 Path")
	}

	// ID = "reflux:" + reflux 条目 Id（tagged 前缀，跨库无歧义）。
	want := []Item{
		{ID: "reflux:118805", Title: "琅琊榜", Kind: KindVideo},
		{ID: "reflux:" + refluxMovieID, Title: "让子弹飞", Kind: KindVideo},
		{ID: "reflux:65714", Title: "大秦帝国之裂变 EP01", Kind: KindVideo},
		{ID: "reflux:77", Title: "夜曲", Kind: KindAudio},
	}
	for _, w := range want {
		assert.Equal(t, w, byID[w.ID], "条目 %s 映射不符", w.ID)
	}

	// 子串检索只回命中条目。
	items, err = src.Search(context.Background(), "琅琊榜")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "reflux:118805", items[0].ID)
	assert.Equal(t, "琅琊榜", items[0].Title)
}

// TestRefluxSearchAuthFailure token 错误（reflux 回 401）：错误必须含
// reflux_auth_failed，让上层可与非鉴权失败区分。
func TestRefluxSearchAuthFailure(t *testing.T) {
	fake := fakereflux.New(refluxToken)
	fake.SetItems(refluxFixtures()...)
	t.Cleanup(fake.Close)

	src := NewRefluxSource(fake.URL, "wrong-token")
	_, err := src.Search(context.Background(), "琅琊榜")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reflux_auth_failed")
	assert.Contains(t, err.Error(), "401")
	assert.Zero(t, fake.SearchHits(), "鉴权失败的调用不应计入合法检索")
}

// TestRefluxSearchServerError 非 2xx：错误须携带状态码。
func TestRefluxSearchServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, err := NewRefluxSource(srv.URL, refluxToken).Search(context.Background(), "q")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

// TestRefluxSearchBadJSON 200 但响应体不是 Jellyfin 形态：应报解码错误而非吞掉。
func TestRefluxSearchBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	t.Cleanup(srv.Close)

	_, err := NewRefluxSource(srv.URL, refluxToken).Search(context.Background(), "q")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reflux search")
}

// TestRefluxSearchUnreachable 连接失败：错误包裹底层原因（保留给调用方），
// 且文本必须脱敏——*url.Error 的原文内嵌完整请求 URL（含 api_key=<token>），
// 而这段文本会进审计 JSONL、slog.Warn 与 LLM 转录。
func TestRefluxSearchUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // 端口已关闭 → 连接被拒

	_, err := NewRefluxSource(srv.URL, refluxToken).Search(context.Background(), "q")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reflux search", "错误应带 reflux search 前缀")
	assert.NotContains(t, err.Error(), refluxToken, "错误文本不得内嵌 api_key")
	assert.NotContains(t, err.Error(), "api_key=", "错误文本不得携带查询串")
	var opErr *net.OpError
	assert.ErrorAs(t, err, &opErr, "底层连接错误应以 %w 保留给调用方")
}

// TestRefluxStreamURL 拉流地址 = {Base}/Videos/{id}/stream?static=true&api_key=…；
// 构造前做一次轻量探测（Range: bytes=0-0），探测命中才算可用。
func TestRefluxStreamURL(t *testing.T) {
	fake := fakereflux.New(refluxToken)
	fake.SetItems(refluxFixtures()...)
	t.Cleanup(fake.Close)

	src := NewRefluxSource(fake.URL, refluxToken)
	u, err := src.StreamURL(context.Background(), refluxMovieID)
	require.NoError(t, err)
	assert.Equal(t, fake.URL+"/Videos/"+refluxMovieID+"/stream?static=true&api_key="+refluxToken, u,
		"拉流地址应逐字为 reflux 的 static 直链")

	path, query, rangeHdr := fake.LastStream()
	assert.Equal(t, "/Videos/"+refluxMovieID+"/stream", path)
	assert.Equal(t, refluxToken, query.Get("api_key"))
	assert.Equal(t, "true", query.Get("static"))
	assert.Equal(t, "bytes=0-0", rangeHdr, "探测应为 0-0 轻量 Range 请求")
	assert.Equal(t, 1, fake.StreamHits())
}

// TestRefluxStreamURLErrors 拉流地址错误路径：401 → reflux_auth_failed；
// 未知条目 → 404 状态码入错；实例不可达 → 包裹底层原因。
func TestRefluxStreamURLErrors(t *testing.T) {
	t.Run("auth failed", func(t *testing.T) {
		fake := fakereflux.New(refluxToken)
		fake.SetItems(refluxFixtures()...)
		t.Cleanup(fake.Close)

		_, err := NewRefluxSource(fake.URL, "wrong-token").StreamURL(context.Background(), refluxMovieID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reflux_auth_failed")
	})

	t.Run("unknown item", func(t *testing.T) {
		fake := fakereflux.New(refluxToken)
		t.Cleanup(fake.Close)

		_, err := NewRefluxSource(fake.URL, refluxToken).StreamURL(context.Background(), "999999")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "404")
	})

	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()

		_, err := NewRefluxSource(srv.URL, refluxToken).StreamURL(context.Background(), refluxMovieID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reflux stream")
		assert.NotContains(t, err.Error(), refluxToken, "错误文本不得内嵌 api_key")
		assert.NotContains(t, err.Error(), "api_key=", "错误文本不得携带查询串")
	})
}

// TestRefluxClientFallbackHasTimeout 零值 RefluxSource 兜底的 HTTP 客户端必须
// 带超时：http.DefaultClient 无超时，实例半开（接受 TCP 不响应）会把检索/拉流
// 永久挂起。
func TestRefluxClientFallbackHasTimeout(t *testing.T) {
	var zero RefluxSource
	c := zero.client()
	require.NotNil(t, c)
	assert.Greater(t, c.Timeout, time.Duration(0), "兜底客户端必须有超时")

	assert.Equal(t, 10*time.Second, NewRefluxSource("http://base", refluxToken).HC.Timeout,
		"NewRefluxSource 显式客户端超时应为 10s")
}

// TestRefluxBaseTrailingSlash Base 带尾部斜杠时应归一化，不得产生双斜杠地址。
func TestRefluxBaseTrailingSlash(t *testing.T) {
	fake := fakereflux.New(refluxToken)
	fake.SetItems(refluxFixtures()...)
	t.Cleanup(fake.Close)

	src := NewRefluxSource(fake.URL+"/", refluxToken)
	u, err := src.StreamURL(context.Background(), refluxMovieID)
	require.NoError(t, err)
	assert.NotContains(t, u, "//Videos", "不应出现双斜杠")
}

// ---- Resolver 集成 ----

// newNASLib 造一个含 clip.mp4 的已扫描 NAS 库。
func newNASLib(t *testing.T) *Library {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "clip.mp4"), []byte("x"), 0o644))
	lib := NewLibrary([]string{dir})
	require.NoError(t, lib.Rescan())
	return lib
}

// captureLogs 把 slog 默认 logger 暂时指到内存 buffer，返回读取函数。
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// TestResolverSearchMergesNASAndReflux search = NAS 库 ∪ reflux 库：两种命中
// 都在结果里，reflux 条目带 reflux: 前缀。
func TestResolverSearchMergesNASAndReflux(t *testing.T) {
	fake := fakereflux.New(refluxToken)
	fake.SetItems(refluxFixtures()...)
	t.Cleanup(fake.Close)

	lib := newNASLib(t)
	r := &Resolver{Lib: lib, Base: "http://base", Reflux: NewRefluxSource(fake.URL, refluxToken)}

	// 空查询：NAS（clip）∪ reflux（4 条）全量合并。
	items := r.Search(context.Background(), "")
	ids := map[string]bool{}
	for _, it := range items {
		ids[it.ID] = true
	}
	assert.True(t, ids[lib.Search("clip")[0].ID], "NAS 命中应在结果里")
	assert.True(t, ids["reflux:"+refluxMovieID], "reflux 命中应在结果里")
	assert.Len(t, items, 5, "NAS 1 条 + reflux 4 条")

	// 只命中 NAS。
	items = r.Search(context.Background(), "clip")
	require.Len(t, items, 1)
	assert.Equal(t, KindVideo, items[0].Kind)

	// 只命中 reflux。
	items = r.Search(context.Background(), "琅琊榜")
	require.Len(t, items, 1)
	assert.Equal(t, "reflux:118805", items[0].ID)
}

// TestResolverSearchDegradesWhenRefluxDown reflux 不可达：slog.Warn 且返回
// 仅 NAS 结果（不拖垮本地库搜索，保持非致命）。
func TestResolverSearchDegradesWhenRefluxDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	logs := captureLogs(t)
	r := &Resolver{Lib: newNASLib(t), Base: "http://base", Reflux: NewRefluxSource(srv.URL, refluxToken)}

	items := r.Search(context.Background(), "")
	require.Len(t, items, 1, "reflux 挂掉只应回 NAS 结果")
	assert.Contains(t, logs(), "reflux", "降级必须留 Warn 日志")
}

// TestResolverSearchDegradesOnAuthFailure reflux 可达但 token 失效：同样降级
// 为 NAS-only + Warn，不致命。
func TestResolverSearchDegradesOnAuthFailure(t *testing.T) {
	fake := fakereflux.New(refluxToken)
	fake.SetItems(refluxFixtures()...)
	t.Cleanup(fake.Close)

	logs := captureLogs(t)
	r := &Resolver{Lib: newNASLib(t), Base: "http://base", Reflux: NewRefluxSource(fake.URL, "wrong-token")}

	items := r.Search(context.Background(), "")
	require.Len(t, items, 1)
	assert.Contains(t, logs(), "reflux", "鉴权失败同样应留 Warn 日志")
}

// TestResolverByIDRefluxTagged reflux: 前缀 id → RefluxSource.StreamURL，
// Kind=reflux。
func TestResolverByIDRefluxTagged(t *testing.T) {
	fake := fakereflux.New(refluxToken)
	fake.SetItems(refluxFixtures()...)
	t.Cleanup(fake.Close)

	r := &Resolver{Lib: newNASLib(t), Base: "http://base", Reflux: NewRefluxSource(fake.URL, refluxToken)}

	src, err := r.ByID(context.Background(), "reflux:"+refluxMovieID)
	require.NoError(t, err)
	assert.Equal(t, Source{
		URL:  fake.URL + "/Videos/" + refluxMovieID + "/stream?static=true&api_key=" + refluxToken,
		Kind: KindReflux,
	}, src)
	assert.GreaterOrEqual(t, fake.StreamHits(), 1, "ByID 应对 reflux 做过可用性探测")
}

// TestResolverByIDRefluxDisabled 未配置 reflux（Reflux=nil）时 reflux: id
// 报 reflux_disabled；无前缀的未知 id 保持 unknown_media_id。
func TestResolverByIDRefluxDisabled(t *testing.T) {
	r := &Resolver{Lib: NewLibrary(nil), Base: "http://base"}

	_, err := r.ByID(context.Background(), "reflux:6334")
	require.Error(t, err)
	assert.Equal(t, "reflux_disabled", err.Error())

	_, err = r.ByID(context.Background(), "000000000000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown_media_id", "无前缀 id 的错误语义不得改变")
}

// TestResolverSearchDisabledReflux 未配置 reflux：Search 退化为纯 NAS，
// 不发任何请求。
func TestResolverSearchDisabledReflux(t *testing.T) {
	r := &Resolver{Lib: newNASLib(t), Base: "http://base"}
	assert.Len(t, r.Search(context.Background(), ""), 1)
}
