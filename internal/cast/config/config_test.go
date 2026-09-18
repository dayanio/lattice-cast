package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTempConfig 在临时目录写入一份 YAML 配置并返回其路径。
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// TestLoad_ValidConfig 解析 testdata 合法配置：逐字段断言解析值，并断言
// 被省略的 mcp_listen / media_http_listen 被填入默认值。
func TestLoad_ValidConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "config.yaml"))
	require.NoError(t, err)

	assert.Equal(t, ":7800", cfg.MCPListen, "省略 mcp_listen 时应填默认值 :7800")
	assert.Equal(t, "0.0.0.0:7810", cfg.MediaListen, "省略 media_http_listen 时应填默认值 0.0.0.0:7810")
	assert.Equal(t, "http://192.168.1.10:7810", cfg.MediaBaseURL)
	assert.Equal(t, []string{"/media/music", "/media/movies"}, cfg.MediaLibrary)
	assert.Equal(t, "/usr/local/bin/yt-dlp", cfg.YtDlp)
	assert.Equal(t, "test-bearer-token", cfg.AuthToken)

	require.Len(t, cfg.Renderers, 2, "应解析出两个渲染端条目")

	// 条目一：host + port 显式给出（静态寻址）。
	living, ok := cfg.Renderers["living-room-display"]
	require.True(t, ok, "key 应为 mDNS 实例名 living-room-display")
	assert.Equal(t, "living-room", living.Room)
	assert.Equal(t, "192.168.1.50", living.Host)
	assert.Equal(t, 7811, living.Port)
	assert.Equal(t, "tok-living", living.Token)

	// 条目二：host/port 留空（仅 mDNS 发现）。
	study, ok := cfg.Renderers["study-display"]
	require.True(t, ok, "key 应为 mDNS 实例名 study-display")
	assert.Equal(t, "study", study.Room)
	assert.Empty(t, study.Host)
	assert.Zero(t, study.Port)
	assert.Equal(t, "tok-study", study.Token)
}

// TestLoad_ExplicitListenAddressesPreserved 显式给出的监听地址应原样保留，不被默认值覆盖。
func TestLoad_ExplicitListenAddressesPreserved(t *testing.T) {
	path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
mcp_listen: ":7900"
media_http_listen: "127.0.0.1:7820"
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, ":7900", cfg.MCPListen)
	assert.Equal(t, "127.0.0.1:7820", cfg.MediaListen)
}

// TestLoad_MissingAuthToken 缺少 auth_token（必填）必须报错。
func TestLoad_MissingAuthToken(t *testing.T) {
	path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
renderers:
  dev-1: {room: dev, token: t}
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_token")
}

// TestLoad_MissingMediaBaseURL 缺少 media_base_url（必填）必须报错。
func TestLoad_MissingMediaBaseURL(t *testing.T) {
	path := writeTempConfig(t, `
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "media_base_url")
}

// TestLoad_UnknownField 未知字段必须报错（yaml.Decoder.KnownFields(true)）。
func TestLoad_UnknownField(t *testing.T) {
	path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
bogus_field: oops
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bogus_field")
}

// TestLoad_EmptyRenderers renderers（必填）为空映射必须报错。
func TestLoad_EmptyRenderers(t *testing.T) {
	path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers: {}
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "renderers")
}

// TestLoad_MissingFile 文件不存在时返回的错误信息必须包含该路径。
func TestLoad_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "config.yaml")
	_, err := Load(missing)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), missing),
		"错误信息应包含路径 %s，实际为: %v", missing, err)
}
