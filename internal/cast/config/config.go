// Package config 加载 cast-agent 的 YAML 配置文件：
// 解析（严格模式，未知字段报错）→ 默认值填充 → 必填项校验。
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Renderer 描述一台渲染端设备。key（配置中 renderers 映射的键）
// 即 mDNS 实例名，是设备的唯一身份。
type Renderer struct {
	Room  string `yaml:"room"`
	Host  string `yaml:"host,omitempty"` // 留空 = 仅 mDNS 发现
	Port  int    `yaml:"port,omitempty"` // mDNS 发现时可不填
	Token string `yaml:"token"`
}

// Config 是 cast-agent 配置文件的根结构。
type Config struct {
	MCPListen    string              `yaml:"mcp_listen"`        // 默认 ":7800"
	MediaListen  string              `yaml:"media_http_listen"` // 默认 "0.0.0.0:7810"
	MediaBaseURL string              `yaml:"media_base_url"`    // 渲染端访问媒体服务的基址，如 http://192.168.1.10:7810；必填
	MediaLibrary []string            `yaml:"media_library"`
	YtDlp        string              `yaml:"yt_dlp"`     // 二进制路径；空=禁用 YouTube
	AuthToken    string              `yaml:"auth_token"` // MCP Bearer（v1 静态）
	Renderers    map[string]Renderer `yaml:"renderers"`  // key = mDNS 实例名 = 设备身份；必填非空

	// reflux 内容源（可选，v1.1）：指向用户自建 reflux 实例的基址
	//（Jellyfin 兼容 API，如 http://192.168.1.20:8096）；空 = 禁用。
	RefluxURL   string `yaml:"reflux_url"`
	RefluxToken string `yaml:"reflux_token"` // reflux api_key；reflux_url 非空时必填
}

// 监听地址默认值（字段缺省或为空字符串时应用）。
const (
	defaultMCPListen   = ":7800"
	defaultMediaListen = "0.0.0.0:7810"
)

// Load 读取并解析 path 指向的 YAML 配置，填充默认值并做必填校验。
// 规则：
//   - 文件缺失或不可读 → 错误（信息中包含 path）；
//   - 未知字段 → 错误（yaml.Decoder.KnownFields(true)）；
//   - 必填：auth_token 非空、media_base_url 非空、renderers 至少一个条目；
//   - 条件必填：reflux_url 非空时 reflux_token 必须非空（reflux 的 Jellyfin
//     兼容 API 所有端点都要求 api_key，缺 token 等于不可用，宁启动报错）。
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && err != io.EOF {
		// io.EOF = 空文件，交给下方必填校验给出更明确的错误。
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if cfg.MCPListen == "" {
		cfg.MCPListen = defaultMCPListen
	}
	if cfg.MediaListen == "" {
		cfg.MediaListen = defaultMediaListen
	}

	switch {
	case cfg.AuthToken == "":
		return Config{}, fmt.Errorf("config: %s: auth_token is required", path)
	case cfg.MediaBaseURL == "":
		return Config{}, fmt.Errorf("config: %s: media_base_url is required", path)
	case len(cfg.Renderers) == 0:
		return Config{}, fmt.Errorf("config: %s: renderers must contain at least one entry", path)
	case cfg.RefluxURL != "" && cfg.RefluxToken == "":
		return Config{}, fmt.Errorf("config: %s: reflux_token is required when reflux_url is set", path)
	}
	return cfg, nil
}
