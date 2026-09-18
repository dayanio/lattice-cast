package resolve

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
