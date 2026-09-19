// 本文件：MCP 端点鉴权。v1 为静态 Bearer token（config.AuthToken），
// 鉴权成功的请求以 agent="static-token" 进入工具层（供审计署名）。
package mcpserver

import (
	"context"
	"net/http"
)

// Authenticator 校验一条进入 MCP 端点的 HTTP 请求：ok=false 时中间件直接
// 401（{"error":"unauthorized"}），ok=true 时 agent 进入请求上下文供审计。
type Authenticator interface {
	Validate(r *http.Request) (agent string, ok bool)
}

// staticToken 是 Authenticator 的 v1 实现：精确匹配
// Authorization: Bearer <token>。
type staticToken struct{ token string }

// StaticToken 返回静态 token 鉴权器（v1 唯一形态）；token 取自 config.AuthToken。
func StaticToken(token string) Authenticator {
	return staticToken{token: token}
}

// Validate 实现 Authenticator：匹配则返回 agent="static-token"。
func (s staticToken) Validate(r *http.Request) (string, bool) {
	if r.Header.Get("Authorization") == "Bearer "+s.token {
		return "static-token", true
	}
	return "", false
}

// agentKey 是请求上下文中 agent 署名的键（私有类型防碰撞）。
type agentKey struct{}

// withAgent 把鉴权得到的 agent 署名放进请求上下文。
func withAgent(ctx context.Context, agent string) context.Context {
	return context.WithValue(ctx, agentKey{}, agent)
}

// agentFrom 取上下文中的 agent 署名；缺失（理论上不可达：所有通过鉴权的
// 请求都被中间件注入）时回退为 "unknown"，审计仍完整落行。
func agentFrom(ctx context.Context) string {
	if agent, ok := ctx.Value(agentKey{}).(string); ok && agent != "" {
		return agent
	}
	return "unknown"
}
