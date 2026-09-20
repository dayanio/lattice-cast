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

// Brain 是内置大脑（LLM 工具循环 + 网页聊天入口）的可选配置；Provider 为
// 空 = 禁用（MCP-only 向后兼容）。三家 provider 统一走 OpenAI 兼容的 chat
// completions：GLM 与 ollama 原生兼容，claude 走 Anthropic 官方 OpenAI
// 兼容层（https://api.anthropic.com/v1）。
type Brain struct {
	Provider string `yaml:"provider"` // glm|claude|ollama；空 = 禁用
	APIKey   string `yaml:"api_key"`  // provider 非空时必填（ollama 可空）
	Model    string `yaml:"model"`    // provider 非空时必填
	BaseURL  string `yaml:"base_url"` // 留空 = 按 provider 取预设（brainBaseURLPresets）
}

// brain provider 的三个合法取值。
const (
	brainProviderGLM    = "glm"
	brainProviderClaude = "claude"
	brainProviderOllama = "ollama"
)

// brainBaseURLPresets 是 provider → OpenAI 兼容端点预设（brain.base_url 留空
// 时由 Load 填充）。claude 使用 Anthropic 官方的 OpenAI 兼容层。
var brainBaseURLPresets = map[string]string{
	brainProviderGLM:    "https://open.bigmodel.cn/api/paas/v4",
	brainProviderClaude: "https://api.anthropic.com/v1",
	brainProviderOllama: "http://127.0.0.1:11434/v1",
}

// validBrainProvider 判定 provider 是否为三家之一。
func validBrainProvider(p string) bool {
	_, ok := brainBaseURLPresets[p]
	return ok
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

	// 内置大脑（可选，v1.1）：LLM 工具循环 + /chat 网页聊天入口；
	// Provider 为空 = 禁用（不装配，MCP-only 行为不变）。
	Brain Brain `yaml:"brain"`
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
//     兼容 API 所有端点都要求 api_key，缺 token 等于不可用，宁启动报错）；
//   - 条件必填：brain.provider 非空时必须是 glm|claude|ollama 之一，model
//     必填，api_key 必填（ollama 除外）；brain.base_url 留空时按 provider
//     填预设端点。
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
	case cfg.Brain.Provider != "" && !validBrainProvider(cfg.Brain.Provider):
		return Config{}, fmt.Errorf("config: %s: brain.provider must be one of glm, claude, ollama", path)
	case cfg.Brain.Provider != "" && cfg.Brain.Provider != brainProviderOllama && cfg.Brain.APIKey == "":
		return Config{}, fmt.Errorf("config: %s: brain.api_key is required when brain.provider is set (except ollama)", path)
	case cfg.Brain.Provider != "" && cfg.Brain.Model == "":
		return Config{}, fmt.Errorf("config: %s: brain.model is required when brain.provider is set", path)
	}

	// brain 预设端点：provider 合法且 base_url 留空时按预设填充。
	if cfg.Brain.Provider != "" && cfg.Brain.BaseURL == "" {
		cfg.Brain.BaseURL = brainBaseURLPresets[cfg.Brain.Provider]
	}
	return cfg, nil
}
