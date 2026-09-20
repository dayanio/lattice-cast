package resolve

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedFiles 在 dir 下按 name→content 批量落盘（父目录自动创建），供扫描用例造数据。
func seedFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
}

// wantPath 返回路径被 Library 收录后的规范化绝对路径：先跟随符号链接、再取绝对路径。
// macOS 的 t.TempDir 位于 /var → /private/var 符号链接之下，必须归一化后再比较。
func wantPath(t *testing.T, elem ...string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(filepath.Join(elem...))
	require.NoError(t, err)
	abs, err := filepath.Abs(resolved)
	require.NoError(t, err)
	return abs
}

// newIndexedLib 造一个已完成 Rescan 的库：tempdir 内 clip.mp4 内容固定，返回库与文件内容。
func newIndexedLib(t *testing.T) (*Library, string) {
	t.Helper()
	dir := t.TempDir()
	content := "latticecast-media-bytes-0123456789"
	seedFiles(t, dir, map[string]string{"clip.mp4": content})
	lib := NewLibrary([]string{dir})
	require.NoError(t, lib.Rescan())
	return lib, content
}

// TestRescanAndSearch 主用例：t.TempDir 造 Interstellar.mp4 / interstellar-2.mp4 /
// notes.txt，Rescan 后 Search("interstellar") 恰好命中 2 条（txt 不入库）、按 Title
// 升序（'I' 0x49 排在 'i' 0x69 前）；查询大小写不敏感。
func TestRescanAndSearch(t *testing.T) {
	dir := t.TempDir()
	seedFiles(t, dir, map[string]string{
		"Interstellar.mp4":   "video-a",
		"interstellar-2.mp4": "video-b",
		"notes.txt":          "not a media file",
	})

	lib := NewLibrary([]string{dir})
	require.NoError(t, lib.Rescan())

	got := lib.Search("interstellar")
	require.Len(t, got, 2, "应恰好命中两个视频，notes.txt 不是媒体")
	assert.Equal(t, "Interstellar", got[0].Title, "按 Title 升序：'I'(0x49) < 'i'(0x69)")
	assert.Equal(t, "interstellar-2", got[1].Title)
	assert.Equal(t, "video", got[0].Kind)
	assert.Equal(t, "video", got[1].Kind)
	assert.Equal(t, wantPath(t, dir, "Interstellar.mp4"), got[0].Path, "Path 应为可服务的归一化绝对路径")

	assert.Len(t, lib.Search("INTERSTELLAR"), 2, "查询大小写不敏感")
	assert.Empty(t, lib.Search("no-such-media-here"), "无命中返回空")
}

// TestKindMapping 每类扩展名抽一：mp4/mkv/mov→video，mp3/flac/wav/m4a→audio，jpg/png→image。
func TestKindMapping(t *testing.T) {
	dir := t.TempDir()
	seedFiles(t, dir, map[string]string{
		"v-mp4.mp4": "x", "v-mkv.mkv": "x", "v-mov.mov": "x",
		"a-mp3.mp3": "x", "a-flac.flac": "x", "a-wav.wav": "x", "a-m4a.m4a": "x",
		"i-jpg.jpg": "x", "i-png.png": "x",
	})

	lib := NewLibrary([]string{dir})
	require.NoError(t, lib.Rescan())

	want := map[string]string{
		"v-mp4": "video", "v-mkv": "video", "v-mov": "video",
		"a-mp3": "audio", "a-flac": "audio", "a-wav": "audio", "a-m4a": "audio",
		"i-jpg": "image", "i-png": "image",
	}
	got := map[string]string{}
	for _, it := range lib.Search("") { // 9 条 < 20，不会被截断
		got[it.Title] = it.Kind
	}
	assert.Equal(t, want, got, "九类扩展名的 Kind 映射应全部正确")
}

// TestSearch_EmptyQueryCappedAt20 Search("") 等价于全量媒体：按 Title 升序，至多 20 条。
func TestSearch_EmptyQueryCappedAt20(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 25; i++ {
		files[fmt.Sprintf("film-%02d.mp4", i)] = "x"
	}
	seedFiles(t, dir, files)

	lib := NewLibrary([]string{dir})
	require.NoError(t, lib.Rescan())

	got := lib.Search("")
	require.Len(t, got, 20, "命中 25 条但至多返回 20")
	for i, it := range got {
		assert.Equal(t, fmt.Sprintf("film-%02d", i), it.Title, "应按 Title 升序取前 20")
	}
}

// TestGet 按精确 ID 取回；未知/空 ID 返回 ok=false。
func TestGet(t *testing.T) {
	dir := t.TempDir()
	seedFiles(t, dir, map[string]string{"Interstellar.mp4": "x"})
	lib := NewLibrary([]string{dir})
	require.NoError(t, lib.Rescan())

	hits := lib.Search("interstellar")
	require.Len(t, hits, 1)

	got, ok := lib.Get(hits[0].ID)
	require.True(t, ok, "已知 ID 应命中")
	assert.Equal(t, hits[0], got)

	_, ok = lib.Get("000000000000")
	assert.False(t, ok, "未知 ID 应 miss")
	_, ok = lib.Get("")
	assert.False(t, ok, "空 ID 应 miss")
}

// TestItemIDIsSha1OfPathAndStable ID 为 sha1(规范化绝对路径) 前 12 位十六进制，
// 且跨 Rescan 稳定（路径不变 → ID 不变）。
func TestItemIDIsSha1OfPathAndStable(t *testing.T) {
	dir := t.TempDir()
	seedFiles(t, dir, map[string]string{"Interstellar.mp4": "x"})
	lib := NewLibrary([]string{dir})
	require.NoError(t, lib.Rescan())

	hits := lib.Search("interstellar")
	require.Len(t, hits, 1)
	id := hits[0].ID
	require.Len(t, id, 12)

	sum := sha1.Sum([]byte(wantPath(t, dir, "Interstellar.mp4")))
	assert.Equal(t, hex.EncodeToString(sum[:])[:12], id, "ID 应为 sha1(abs path) 前 12 位")

	require.NoError(t, lib.Rescan())
	again, ok := lib.Get(id)
	require.True(t, ok, "重扫后同一 ID 仍应可取回")
	assert.Equal(t, id, again.ID, "路径不变则 ID 稳定")
}

// TestRescanFollowsFileSymlinks 文件符号链接被跟随收录（目标可在库外）；
// 失效符号链接跳过且不令 Rescan 失败。
func TestRescanFollowsFileSymlinks(t *testing.T) {
	libDir, outside := t.TempDir(), t.TempDir()
	seedFiles(t, outside, map[string]string{"real.mkv": "real-bytes"})
	require.NoError(t, os.Symlink(filepath.Join(outside, "real.mkv"), filepath.Join(libDir, "linked.mkv")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "missing.mkv"), filepath.Join(libDir, "broken.mkv")))

	lib := NewLibrary([]string{libDir})
	require.NoError(t, lib.Rescan(), "失效符号链接不应导致 Rescan 失败")

	hits := lib.Search("linked")
	require.Len(t, hits, 1, "库内文件符号链接应被跟随收录")
	assert.Equal(t, "video", hits[0].Kind)
	assert.Equal(t, wantPath(t, outside, "real.mkv"), hits[0].Path, "收录的是符号链接解析后的真实文件")
	assert.Empty(t, lib.Search("broken"), "失效符号链接不入库")
}

// TestRescanSkipsUnreadableEntries 单个不可读子目录（0o000）应被跳过并记
// Warn 日志：Rescan 仍返回 nil，且收录所有可读文件、不收录不可读目录内的文件。
// root 身份下权限检查不生效，跳过（任务书要求的 os.Geteuid()==0 守卫）。
func TestRescanSkipsUnreadableEntries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission checks do not apply")
	}
	dir := t.TempDir()
	seedFiles(t, dir, map[string]string{"open-a.mp4": "x", "open-b.mkv": "x"})
	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.MkdirAll(locked, 0o755))
	seedFiles(t, locked, map[string]string{"hidden.mp4": "x"})
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) }) // 还原权限，让 TempDir 清理可删除

	// 把 slog 默认 logger 指向内存 buffer：既验证 Warn 日志确实产生（含路径），
	// 又保持 go test 输出整洁。
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	lib := NewLibrary([]string{dir})
	require.NoError(t, lib.Rescan(), "不可读子目录应被跳过，而非令整趟扫描失败")

	titles := map[string]bool{}
	for _, it := range lib.Search("") {
		titles[it.Title] = true
	}
	assert.True(t, titles["open-a"], "可读文件应照常入库")
	assert.True(t, titles["open-b"], "可读文件应照常入库")
	assert.False(t, titles["hidden"], "不可读目录内的文件不应入库")

	logged := logBuf.String()
	assert.Contains(t, logged, "WARN", "不可读条目应以 Warn 级日志呈现")
	assert.Contains(t, logged, "locked", "Warn 日志应指出被跳过的条目路径")
}

// TestRescanUnreadableRootKeepsOldIndex 根目录本身不可读（0o000）时 Rescan 应
// 返回错误且旧索引原样保留（已入库条目仍可 Get），而非静默换成空索引。
// root 身份下权限检查不生效，跳过（os.Geteuid()==0 守卫）。
func TestRescanUnreadableRootKeepsOldIndex(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission checks do not apply")
	}
	dir := t.TempDir()
	seedFiles(t, dir, map[string]string{"clip.mp4": "x"})
	lib := NewLibrary([]string{dir})
	require.NoError(t, lib.Rescan())
	hits := lib.Search("clip")
	require.Len(t, hits, 1)
	id := hits[0].ID

	require.NoError(t, os.Chmod(dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) // 还原权限，让 TempDir 清理可删除

	require.Error(t, lib.Rescan(), "根目录本身不可读应令 Rescan 报错")
	_, ok := lib.Get(id)
	assert.True(t, ok, "Rescan 失败时应保留旧索引，已入库条目仍可取回")
}

// TestRescanMissingRootStillErrors 根目录不存在（遍历无法开始）仍返回错误，
// 不被"跳过不可读条目"的宽容语义吞掉。
func TestRescanMissingRootStillErrors(t *testing.T) {
	lib := NewLibrary([]string{filepath.Join(t.TempDir(), "no-such-dir")})
	require.Error(t, lib.Rescan(), "根目录不存在时 Rescan 应继续报错")
}

// TestMediaURL baseURL 去尾部斜杠后拼 /media/<id>。
func TestMediaURL(t *testing.T) {
	lib := NewLibrary(nil)
	assert.Equal(t, "http://192.168.1.10:7810/media/abc123def456",
		lib.MediaURL("http://192.168.1.10:7810", "abc123def456"))
	assert.Equal(t, "http://192.168.1.10:7810/media/abc123def456",
		lib.MediaURL("http://192.168.1.10:7810/", "abc123def456"), "尾部斜杠应被去除")
}

// TestItemJSONNeverExposesPath Path 不序列化（json:"-"），字段名固定 media_id/title/kind。
func TestItemJSONNeverExposesPath(t *testing.T) {
	b, err := json.Marshal(Item{ID: "abc123def456", Title: "movie", Kind: "video", Path: "/nas/secret/movie.mp4"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"media_id":"abc123def456","title":"movie","kind":"video"}`, string(b))
}
