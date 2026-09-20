// 本文件：内置大脑（internal/cast/brain）的 ToolExecutor 适配器。Execute
// 与 MCP handler 调用完全相同的执行核心（core* / doPlay）并走同一 record
// 审计路径——工具执行即审计，不因调用方是进程内的大脑而豁免。本包不
// import brain：适配器经 Go 结构化类型隐式满足 brain.ToolExecutor，由
// main（与测试）把 Server.Executor() 的返回值直接赋给 brain.New。
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// toolExecutor 是 Server 工具核心的 JSON 进/出适配器。
type toolExecutor struct{ s *Server }

// Executor 返回工具执行核心适配器（满足 brain.ToolExecutor 接口）。
func (s *Server) Executor() *toolExecutor { return &toolExecutor{s: s} }

// Execute 按工具名分派：入参 JSON 反序列化进与 MCP schema 同形的结构体，
// 执行核心跑完后把出参序列化成 JSON 字符串返回；失败返回 error（错误文本
// 与 MCP IsError 的 Content[0].Text 同源，由大脑原样回灌 LLM）。
func (e *toolExecutor) Execute(ctx context.Context, tool string, argsJSON json.RawMessage) (string, error) {
	// 大脑直调不经鉴权中间件（同进程）：审计署名补为 brain，与 MCP 的
	// static-token 区分。
	if agentFrom(ctx) == "unknown" {
		ctx = withAgent(ctx, "brain")
	}
	switch tool {
	case "list_cast_devices":
		var in listIn
		if err := decodeArgs(tool, argsJSON, &in); err != nil {
			return "", err
		}
		start := time.Now()
		out, err := e.s.coreListDevices(ctx)
		e.s.record(ctx, tool, in, out, err, start)
		return marshalOut(out, err)
	case "search_media":
		var in searchIn
		if err := decodeArgs(tool, argsJSON, &in); err != nil {
			return "", err
		}
		start := time.Now()
		out, err := e.s.coreSearchMedia(ctx, in)
		e.s.record(ctx, tool, in, out, err, start)
		return marshalOut(out, err)
	case "cast_play":
		var in playIn
		if err := decodeArgs(tool, argsJSON, &in); err != nil {
			return "", err
		}
		start := time.Now()
		out, err := e.s.doPlay(ctx, in)
		e.s.record(ctx, tool, in, out, err, start)
		return marshalOut(out, err)
	case "cast_stop":
		var in deviceIn
		if err := decodeArgs(tool, argsJSON, &in); err != nil {
			return "", err
		}
		start := time.Now()
		out, err := e.s.coreStop(ctx, in)
		e.s.record(ctx, tool, in, out, err, start)
		return marshalOut(out, err)
	case "cast_pause":
		var in deviceIn
		if err := decodeArgs(tool, argsJSON, &in); err != nil {
			return "", err
		}
		start := time.Now()
		out, err := e.s.corePause(ctx, in)
		e.s.record(ctx, tool, in, out, err, start)
		return marshalOut(out, err)
	case "cast_seek":
		var in seekIn
		if err := decodeArgs(tool, argsJSON, &in); err != nil {
			return "", err
		}
		start := time.Now()
		out, err := e.s.coreSeek(ctx, in)
		e.s.record(ctx, tool, in, out, err, start)
		return marshalOut(out, err)
	case "cast_volume":
		var in volumeIn
		if err := decodeArgs(tool, argsJSON, &in); err != nil {
			return "", err
		}
		start := time.Now()
		out, err := e.s.coreVolume(ctx, in)
		e.s.record(ctx, tool, in, out, err, start)
		return marshalOut(out, err)
	case "cast_status":
		var in deviceIn
		if err := decodeArgs(tool, argsJSON, &in); err != nil {
			return "", err
		}
		start := time.Now()
		out, err := e.s.coreStatus(ctx, in)
		e.s.record(ctx, tool, in, out, err, start)
		return marshalOut(out, err)
	case "cast_resume":
		// 意图快通道专属（Task 26）：断点续播。与八个 MCP 工具同一 record
		// 审计路径；不进 MCP 注册表与 brain toolDefs（见 coreResume 注释）。
		var in deviceIn
		if err := decodeArgs(tool, argsJSON, &in); err != nil {
			return "", err
		}
		start := time.Now()
		out, err := e.s.coreResume(ctx, in)
		e.s.record(ctx, tool, in, out, err, start)
		return marshalOut(out, err)
	default:
		return "", fmt.Errorf("unknown_tool: %s", tool)
	}
}

// decodeArgs 把 LLM 给出的入参 JSON 反序列化进工具入参结构体；空体按空
// 对象处理（list_cast_devices 无参）。
func decodeArgs(tool string, argsJSON json.RawMessage, into any) error {
	if len(argsJSON) == 0 {
		argsJSON = json.RawMessage("{}")
	}
	if err := json.Unmarshal(argsJSON, into); err != nil {
		return fmt.Errorf("%s: bad_arguments: %v", tool, err)
	}
	return nil
}

// marshalOut 把工具出参序列化为 JSON；失败路径直接透传 error（大脑以错误
// 文本回灌，不产出 JSON）。
func marshalOut(out any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	b, mErr := json.Marshal(out)
	if mErr != nil {
		return "", mErr
	}
	return string(b), nil
}
