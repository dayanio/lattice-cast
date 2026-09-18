package resolve

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// freePort 借一个空闲端口（listen :0 后关闭），供 MediaServer 绑定。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// startServer 起一个 MediaServer 并返回其 base URL；测试结束自动 Close。
func startServer(t *testing.T, lib *Library) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	srv := NewMediaServer(lib, addr)
	require.NoError(t, srv.Start(), "MediaServer 应能绑定并启动")
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + addr
}

// TestMediaServer_ServesFileBytes GET /media/<已知 id> 应原样返回文件字节。
func TestMediaServer_ServesFileBytes(t *testing.T) {
	lib, content := newIndexedLib(t)
	hits := lib.Search("clip")
	require.Len(t, hits, 1)
	base := startServer(t, lib)

	resp, err := http.Get(base + "/media/" + hits[0].ID)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, content, string(body), "应原样返回文件内容")
}

// TestMediaServer_UnknownID404 未收录 id 返回 404，响应体为纯文本。
func TestMediaServer_UnknownID404(t *testing.T) {
	lib, _ := newIndexedLib(t)
	base := startServer(t, lib)

	resp, err := http.Get(base + "/media/000000000000")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain", "404 应为纯文本")
}

// TestMediaServer_TraversalID404 目录穿越形态的 id 一律 404：id 是路径哈希，
// 索引中不可能存在，绝无文件被送出。
func TestMediaServer_TraversalID404(t *testing.T) {
	lib, _ := newIndexedLib(t)
	base := startServer(t, lib)

	for _, id := range []string{
		url.PathEscape("../../../etc/passwd"),
		"..%2f..%2fetc%2fpasswd", // 任务书示例形态，保持原样不二次转义
	} {
		resp, err := http.Get(base + "/media/" + id)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "穿越 id %q 应 404，body=%q", id, body)
	}
}

// TestMediaServer_MethodNotAllowed 非 GET（POST）→ 405：服务严格只读。
func TestMediaServer_MethodNotAllowed(t *testing.T) {
	lib, _ := newIndexedLib(t)
	hits := lib.Search("clip")
	require.Len(t, hits, 1)
	base := startServer(t, lib)

	resp, err := http.Post(base+"/media/"+hits[0].ID, "text/plain", strings.NewReader("payload"))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// TestMediaServer_StartBindError 端口已被占用时 Start 应同步返回错误。
func TestMediaServer_StartBindError(t *testing.T) {
	lib, _ := newIndexedLib(t)
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	first := NewMediaServer(lib, addr)
	require.NoError(t, first.Start())
	t.Cleanup(func() { _ = first.Close() })

	second := NewMediaServer(lib, addr)
	require.Error(t, second.Start(), "同端口第二个实例应绑定失败")
}
