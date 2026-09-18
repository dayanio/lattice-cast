package resolve

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// YtDlp 是 Extractor 的 yt-dlp 实现：把 YouTube 页面地址解析成直链媒体地址。
// Bin 为 yt-dlp 可执行文件（PATH 上的名字或绝对路径），通常由配置注入。
type YtDlp struct {
	Bin string
}

// 编译期保证 *YtDlp 满足 Extractor。
var _ Extractor = (*YtDlp)(nil)

// stderrTailMax 错误信息里携带的 stderr 尾部长度上限（字节）。
const stderrTailMax = 512

// NewYtDlp 创建使用 bin 的 yt-dlp 提取器。
func NewYtDlp(bin string) *YtDlp {
	return &YtDlp{Bin: bin}
}

// Extract 执行 `<bin> -f best[ext=mp4]/best -g <pageURL>`：
//   - 退出码非 0：返回携带 stderr 尾部的错误；
//   - 成功：stdout 中第一个非空行即直链媒体地址；
//   - 成功但无非空输出：返回错误（不存在空直链）。
//
// ctx 取消时由 exec.CommandContext 终止子进程。
func (y *YtDlp) Extract(ctx context.Context, pageURL string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, y.Bin, "-f", "best[ext=mp4]/best", "-g", pageURL)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("yt-dlp extract %s: %w: %s", pageURL, err, stderrTail(&stderr))
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line, nil
		}
	}
	return "", fmt.Errorf("yt-dlp extract %s: no url in output", pageURL)
}

// stderrTail 取 stderr 的尾部（至多 stderrTailMax 字节）并去除首尾空白；
// 为空时返回占位说明，保证错误信息始终可读。
func stderrTail(buf *bytes.Buffer) string {
	b := buf.Bytes()
	if len(b) > stderrTailMax {
		b = b[len(b)-stderrTailMax:]
	}
	if tail := strings.TrimSpace(string(b)); tail != "" {
		return tail
	}
	return "(no stderr)"
}
