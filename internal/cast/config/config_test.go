package config

import (
	"fmt"
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
	assert.Equal(t, "http://192.168.1.20:8096", cfg.RefluxURL)
	assert.Equal(t, "reflux-api-token", cfg.RefluxToken)
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
	assert.Empty(t, cfg.RefluxURL, "未填 reflux_url = reflux 内容源禁用")
}

// TestLoad_RefluxTokenRequired 填了 reflux_url 但缺 reflux_token：必须报错
// （reflux 的 Jellyfin 兼容 API 全部端点要求 api_key，无 token 等于不可用）。
func TestLoad_RefluxTokenRequired(t *testing.T) {
	path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
reflux_url: http://192.168.1.20:8096
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reflux_token")
}

// TestLoad_RefluxBothFields 流程正向：reflux_url + reflux_token 成对给出应
// 原样解析（与 testdata/config.yaml 的断言互为补充，覆盖"临时配置"路径）。
func TestLoad_RefluxBothFields(t *testing.T) {
	path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
reflux_url: http://192.168.1.20:8096/
reflux_token: another-reflux-token
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "http://192.168.1.20:8096/", cfg.RefluxURL)
	assert.Equal(t, "another-reflux-token", cfg.RefluxToken)
}

// TestLoad_RefluxTokenWithoutURL 只给 reflux_token 不给 reflux_url：reflux
// 保持禁用（token 无副作用），不报错。
func TestLoad_RefluxTokenWithoutURL(t *testing.T) {
	path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
reflux_token: orphan-token
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.RefluxURL)
	assert.Equal(t, "orphan-token", cfg.RefluxToken)
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

// ---- brain 段（可选，v1.1：内置大脑 + 网页聊天）----

// TestLoad_BrainDisabledByDefault 未写 brain 段（或写空段）＝禁用：Brain 为
// 零值，既有配置解析不受影响（MCP-only 向后兼容）。
func TestLoad_BrainDisabledByDefault(t *testing.T) {
	path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Zero(t, cfg.Brain, "缺 brain 段 = 内置大脑禁用")

	path = writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
brain:
`)
	cfg, err = Load(path)
	require.NoError(t, err)
	assert.Zero(t, cfg.Brain, "空 brain 段 = 内置大脑禁用")
}

// TestLoad_BrainSectionParse brain 段完整解析；base_url 显式给出时原样保留。
func TestLoad_BrainSectionParse(t *testing.T) {
	path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
brain:
  provider: glm
  api_key: sk-brain-123
  model: glm-4.7
  base_url: http://llm.example/v1
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "glm", cfg.Brain.Provider)
	assert.Equal(t, "sk-brain-123", cfg.Brain.APIKey)
	assert.Equal(t, "glm-4.7", cfg.Brain.Model)
	assert.Equal(t, "http://llm.example/v1", cfg.Brain.BaseURL, "显式 base_url 原样保留")
}

// TestLoad_BrainBaseURLPreset base_url 留空时按 provider 填预设端点
// （三家 provider 统一走 OpenAI 兼容 chat completions）；ollama 允许 api_key
// 留空。
func TestLoad_BrainBaseURLPreset(t *testing.T) {
	tpl := `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
brain:
  provider: %s
  api_key: sk-x
  model: m
`
	for provider, want := range map[string]string{
		"glm":    "https://open.bigmodel.cn/api/paas/v4",
		"claude": "https://api.anthropic.com/v1",
		"ollama": "http://127.0.0.1:11434/v1",
	} {
		cfg, err := Load(writeTempConfig(t, fmt.Sprintf(tpl, provider)))
		require.NoError(t, err, "provider %s", provider)
		assert.Equal(t, want, cfg.Brain.BaseURL, "provider %s 应填预设端点", provider)
	}

	cfg, err := Load(writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
brain:
  provider: ollama
  model: qwen2.5
`))
	require.NoError(t, err, "ollama 允许 api_key 留空")
	assert.Empty(t, cfg.Brain.APIKey)
	assert.Equal(t, "http://127.0.0.1:11434/v1", cfg.Brain.BaseURL)
}

// TestLoad_BrainValidation brain 段校验：provider 必须是三家之一；api_key
// 必填（ollama 除外）；model 必填。孤儿字段（provider 为空时给了 model）
// 无副作用，与 reflux_token 的宽松处理一致。
func TestLoad_BrainValidation(t *testing.T) {
	t.Run("unknown provider", func(t *testing.T) {
		path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
brain:
  provider: openai
  api_key: sk-x
  model: gpt
`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "brain.provider")
	})

	t.Run("api_key required for glm", func(t *testing.T) {
		path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
brain:
  provider: glm
  model: glm-4.7
`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "brain.api_key")
	})

	t.Run("model required", func(t *testing.T) {
		path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
brain:
  provider: glm
  api_key: sk-x
`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "brain.model")
	})

	t.Run("orphan model without provider tolerated", func(t *testing.T) {
		path := writeTempConfig(t, `
media_base_url: http://10.0.0.2:7810
auth_token: tok
renderers:
  dev-1: {room: dev, token: t}
brain:
  model: glm-4.7
`)
		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Empty(t, cfg.Brain.Provider, "provider 为空 = 禁用")
	})
}
