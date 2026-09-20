package resolve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubExtractor 记录收到的 pageURL 并返回固定直链（或固定错误），用于
// Resolver 分支测试，不真正执行外部进程。
type stubExtractor struct {
	got   string // 最近一次收到的 pageURL
	media string // 固定返回的直链
	fail  error  // 非 nil 时直接返回该错误
}

func (s *stubExtractor) Extract(_ context.Context, pageURL string) (string, error) {
	s.got = pageURL
	if s.fail != nil {
		return "", s.fail
	}
	return s.media, nil
}

// writeFakeYtDlp 在 t.TempDir 写一个可执行的假 yt-dlp 脚本（#!/bin/sh）：
// 把收到的命令行参数逐行记录到环境变量 FAKE_YTDLP_ARGS_FILE 指向的文件，
// 再把 lines 依次打到 stdout。返回脚本路径与参数记录文件路径。
func writeFakeYtDlp(t *testing.T, lines ...string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "yt-dlp")
	argsFile = filepath.Join(dir, "args.txt")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`printf '%s\n' "$@" > "$FAKE_YTDLP_ARGS_FILE"` + "\n")
	for _, line := range lines {
		fmt.Fprintf(&b, "echo %q\n", line)
	}
	require.NoError(t, os.WriteFile(bin, []byte(b.String()), 0o755), "假 yt-dlp 脚本应可执行权限落盘")
	return bin, argsFile
}

// recordedArgs 读取假 yt-dlp 记录的命令行参数（每行一个）。
func recordedArgs(t *testing.T, argsFile string) []string {
	t.Helper()
	raw, err := os.ReadFile(argsFile)
	require.NoError(t, err, "假 yt-dlp 应已把参数写入记录文件")
	trimmed := strings.TrimRight(string(raw), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// TestYtDlpExtract 主用例：假脚本首行刻意输出空行、随后两行 URL——
// Extract 应取"第一个非空行"；并用记录文件逐字断言命令行参数。
func TestYtDlpExtract(t *testing.T) {
	page := "https://youtu.be/dQw4w9WgXcQ"
	bin, argsFile := writeFakeYtDlp(t, "",
		"http://media.example/video-720p.mp4",
		"http://media.example/video-360p.mp4")
	t.Setenv("FAKE_YTDLP_ARGS_FILE", argsFile)

	got, err := NewYtDlp(bin).Extract(context.Background(), page)
	require.NoError(t, err)
	assert.Equal(t, "http://media.example/video-720p.mp4", got, "应取 stdout 第一个非空行，而非后续候选")

	assert.Equal(t, []string{"-f", "best[ext=mp4]/best", "-g", page},
		recordedArgs(t, argsFile), "命令行参数应逐字一致")
}

// TestYtDlpExtractNonZeroExit 非零退出：错误信息应携带 stderr 尾部。
func TestYtDlpExtractNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "yt-dlp")
	script := "#!/bin/sh\n" +
		"echo 'usage: yt-dlp [OPTIONS] URL' >&2\n" +
		"echo 'ERROR: Signature extraction failed' >&2\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))

	_, err := NewYtDlp(bin).Extract(context.Background(), "https://youtu.be/xyz")
	require.Error(t, err, "非零退出应返回错误")
	assert.Contains(t, err.Error(), "Signature extraction failed", "错误应携带 stderr 尾部")
}

// TestYtDlpExtractNoOutput 退出码为 0 但 stdout 只有空白行：应报错而非返回空直链。
func TestYtDlpExtractNoOutput(t *testing.T) {
	bin, _ := writeFakeYtDlp(t, "", "   ")

	_, err := NewYtDlp(bin).Extract(context.Background(), "https://youtu.be/xyz")
	require.Error(t, err, "stdout 无非空行应报错")
	assert.Contains(t, err.Error(), "no url in output")
}

// TestByURLDirectPassthrough 非 YouTube 一律原样透传：不联网、不校验、
// 不做任何 URL 归一化（大小写/空格/查询串/片段原样保留）。
// 主机名指向保留地址：若实现偷偷发起请求，用例会立刻失败或超时。
func TestByURLDirectPassthrough(t *testing.T) {
	r := &Resolver{Lib: NewLibrary(nil), Base: "http://192.168.1.10:7810"}
	raw := "http://Example.com:8080/Movies/Big Buck Bunny.mkv?token=abc#frag"

	src, err := r.ByURL(context.Background(), raw)
	require.NoError(t, err)
	assert.Equal(t, Source{URL: raw, Kind: KindDirect}, src, "应原样透传且 Kind=direct")
}

// TestByURLYouTubeHosts 四个 YouTube 域名（youtube.com / www.youtube.com /
// m.youtube.com / youtu.be）都应走 Extractor，且把原始页面地址原样交给它。
func TestByURLYouTubeHosts(t *testing.T) {
	ext := &stubExtractor{media: "http://media.example/stream.mp4"}
	r := &Resolver{Lib: NewLibrary(nil), Base: "http://base", Ext: ext}
	pages := []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://youtube.com/watch?v=dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ",
		"https://m.youtube.com/watch?v=dQw4w9WgXcQ",
	}
	for _, page := range pages {
		src, err := r.ByURL(context.Background(), page)
		require.NoError(t, err, page)
		assert.Equal(t, Source{URL: "http://media.example/stream.mp4", Kind: KindYouTube}, src, page)
		assert.Equal(t, page, ext.got, "应把原始页面地址（而非改写形式）交给 Extractor")
	}
}

// TestByURLYouTubeDisabled 未配置 Extractor（Ext==nil）时命中 YouTube 域名
// 应报 "youtube_disabled"，而非透传页面地址。
func TestByURLYouTubeDisabled(t *testing.T) {
	r := &Resolver{Lib: NewLibrary(nil), Base: "http://base"} // Ext 为 nil

	_, err := r.ByURL(context.Background(), "https://youtu.be/dQw4w9WgXcQ")
	require.Error(t, err)
	assert.Equal(t, "youtube_disabled", err.Error())
}

// TestByURLYouTubeWithFakeYtDlp 端到端接线：Resolver.Ext 用真实的 *YtDlp
// （假脚本），验证 youtube 分支取回脚本打印的直链、参数逐字正确。
func TestByURLYouTubeWithFakeYtDlp(t *testing.T) {
	page := "https://youtube.com/watch?v=abc123"
	bin, argsFile := writeFakeYtDlp(t, "http://media.example/fake-720.mp4")
	t.Setenv("FAKE_YTDLP_ARGS_FILE", argsFile)

	r := &Resolver{Lib: NewLibrary(nil), Base: "http://base", Ext: NewYtDlp(bin)}
	src, err := r.ByURL(context.Background(), page)
	require.NoError(t, err)
	assert.Equal(t, Source{URL: "http://media.example/fake-720.mp4", Kind: KindYouTube}, src)
	assert.Equal(t, []string{"-f", "best[ext=mp4]/best", "-g", page}, recordedArgs(t, argsFile))
}

// TestByIDNASHappyPath 已知 media_id：返回 MediaURL(r.Base, id) 且 Kind=nas。
func TestByIDNASHappyPath(t *testing.T) {
	lib, _ := newIndexedLib(t) // 复用 library_test.go 辅助：clip.mp4 已入库
	base := "http://192.168.1.10:7810/"
	r := &Resolver{Lib: lib, Base: base}
	id := lib.Search("clip")[0].ID

	src, err := r.ByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, Source{URL: lib.MediaURL(base, id), Kind: KindNAS}, src)
	assert.Equal(t, "http://192.168.1.10:7810/media/"+id, src.URL, "应经 MediaURL 拼内网地址")
}

// TestByIDUnknown 未知 media_id 应报错。
func TestByIDUnknown(t *testing.T) {
	r := &Resolver{Lib: NewLibrary(nil), Base: "http://base"}

	_, err := r.ByID(context.Background(), "000000000000")
	require.Error(t, err, "未知 media_id 应报错")
	assert.Contains(t, err.Error(), "unknown")
}
