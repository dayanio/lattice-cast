// 审计日志（本文件）：JSONL 追加写，每个操作一行，供 v1 审计留痕。
// 记录职责在 MCP 工具层（Task 11）：Manager 的操作方法不自动写审计。
package manager

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// AuditEntry 是一行审计记录：ts 为记录时刻，duration_ms 为操作耗时。
// Args/Result 为 JSON 字符串（由调用方序列化），保持行内单层 JSON 结构。
type AuditEntry struct {
	TS         time.Time `json:"ts"`
	Agent      string    `json:"agent"`
	Tool       string    `json:"tool"`
	Args       string    `json:"args"`
	Result     string    `json:"result"`
	OK         bool      `json:"ok"`
	DurationMS int64     `json:"duration_ms"`
}

// AuditLog 是追加写 JSONL 的审计文件句柄；写操作互斥，Record 对 I/O 错误
// 只记日志、绝不 panic（审计失败不得影响主流程）。
type AuditLog struct {
	mu sync.Mutex
	f  *os.File
}

// OpenAudit 以创建/追加模式打开 path 指向的审计文件（不存在则创建，权限 0600），
// 已有内容保留（追加语义）。
func OpenAudit(path string) (*AuditLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	return &AuditLog{f: f}, nil
}

// Record 追加一行审计 JSON。序列化或写入失败时仅记 slog 错误，不 panic、
// 不向调用方报错（审计是尽力而为的旁路）。
func (a *AuditLog) Record(agent, tool, args, result string, ok bool, dur time.Duration) {
	if a == nil {
		return
	}
	entry := AuditEntry{
		TS:         time.Now(),
		Agent:      agent,
		Tool:       tool,
		Args:       args,
		Result:     result,
		OK:         ok,
		DurationMS: dur.Milliseconds(),
	}
	line, err := json.Marshal(entry)
	if err != nil {
		slog.Error("audit: marshal entry", "err", err)
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return // 已关闭：静默降级，绝不 panic
	}
	if _, err := a.f.Write(append(line, '\n')); err != nil {
		slog.Error("audit: write", "file", a.f.Name(), "err", err)
	}
}

// Close 关闭底层文件；重复关闭返回 nil。
func (a *AuditLog) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	f := a.f
	a.f = nil
	return f.Close()
}
