// Package fakellm 提供内存态 OpenAI 兼容 chat completions 测试替身：按脚本
// 顺序回放响应（状态码 + JSON 体），并逐个记录收到的请求体与 Authorization
// 头，供测试断言多轮历史（system/assistant/tool 消息是否按预期进入上下文）
// 与鉴权头发送。内置大脑（brain）与网页聊天（webchat）的测试复用本包。
package fakellm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Resp 是脚本中的一步：状态码 + 响应体（JSON 字符串）。Status 为 0 时按 200。
type Resp struct {
	Status int
	Body   string
}

// Fake 是脚本化 LLM 服务器，内嵌 *httptest.Server（监听 127.0.0.1 随机端口）。
type Fake struct {
	*httptest.Server

	mu    sync.Mutex
	resps []Resp
	reqs  [][]byte
	auth  []string
}

// New 启动 Fake 并写入初始脚本。脚本耗尽后的请求一律回 500
// {"error":"script_exhausted"}：暴露"测试剧本比实际调用次数短"的错误。
func New(resps ...Resp) *Fake {
	f := &Fake{resps: append([]Resp(nil), resps...)}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", f.handle)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
	})
	f.Server = httptest.NewServer(mux)
	return f
}

// Script 追加剧本（分轮构建测试用）。
func (f *Fake) Script(resps ...Resp) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resps = append(f.resps, resps...)
}

// Requests 返回到目前为止收到的全部请求体（深拷贝，调用方可自由解析）。
func (f *Fake) Requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.reqs))
	for i, r := range f.reqs {
		out[i] = append([]byte(nil), r...)
	}
	return out
}

// AuthHeaders 返回各请求携带的 Authorization 头（按到达顺序，可为空串）。
func (f *Fake) AuthHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	i := len(f.reqs)
	f.reqs = append(f.reqs, append([]byte(nil), body...))
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if i >= len(f.resps) {
		f.mu.Unlock()
		http.Error(w, `{"error":"script_exhausted"}`, http.StatusInternalServerError)
		return
	}
	resp := f.resps[i]
	f.mu.Unlock()

	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resp.Body)
}
