// 本文件：聊天单页。单文件 HTML（原生 JS、无构建工具、中文界面）：首次
// 访问弹层录入访问令牌（存 localStorage，仅发送给本源 cast-agent）；消息
// 经 fetch POST /chat/api/message 读取 SSE 流，渲染 delta/tool/final/error
// 四类事件。JS 不用模板字符串（与 Go raw string 的反引号冲突）。
package webchat

// pageHTML 是 GET /chat 的响应体。
const pageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>LatticeCast 投屏助手</title>
<style>
  :root { color-scheme: light; }
  * { box-sizing: border-box; }
  body { margin: 0; font-family: -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif;
         background: #f5f6f8; color: #1c1e21; display: flex; flex-direction: column; height: 100dvh; }
  header { background: #1f2937; color: #fff; padding: 12px 16px; font-size: 16px; font-weight: 600; }
  main { flex: 1; overflow-y: auto; padding: 16px; max-width: 720px; width: 100%; margin: 0 auto; }
  .row { display: flex; margin: 6px 0; }
  .row.user { justify-content: flex-end; }
  .bubble { max-width: 78%; padding: 10px 14px; border-radius: 14px; line-height: 1.5;
            white-space: pre-wrap; word-break: break-word; }
  .user .bubble { background: #2563eb; color: #fff; border-bottom-right-radius: 4px; }
  .assistant .bubble { background: #fff; border: 1px solid #e2e5e9; border-bottom-left-radius: 4px; }
  .tool { color: #6b7280; font-size: 12px; margin: 4px 0 4px 8px; }
  .tool.error { color: #b91c1c; }
  form.chat { display: flex; gap: 8px; padding: 10px 16px calc(10px + env(safe-area-inset-bottom));
              max-width: 720px; width: 100%; margin: 0 auto; }
  input[type=text] { flex: 1; padding: 10px 14px; border: 1px solid #d1d5db; border-radius: 10px; font-size: 15px; }
  button { padding: 10px 18px; border: 0; border-radius: 10px; background: #2563eb; color: #fff; font-size: 15px; }
  button:disabled { opacity: .5; }
  dialog { border: 1px solid #d1d5db; border-radius: 12px; padding: 20px; max-width: 320px; width: 90%; }
  dialog::backdrop { background: rgba(0,0,0,.4); }
  dialog h3 { margin: 0 0 12px; font-size: 15px; }
  dialog input { width: 100%; padding: 8px 10px; border: 1px solid #d1d5db; border-radius: 8px; font-size: 14px; }
  dialog p { font-size: 12px; color: #6b7280; margin: 10px 0 0; }
</style>
</head>
<body>
<header>LatticeCast 投屏助手</header>
<main id="log"></main>
<form class="chat" id="chat-form">
  <input type="text" id="text" placeholder="想看什么？例如：把夕阳短片投到客厅" autocomplete="off">
  <button id="send" type="submit">发送</button>
</form>
<dialog id="token-dialog">
  <h3>请输入访问令牌</h3>
  <form method="dialog" id="token-form">
    <input type="password" id="token" placeholder="cast-agent 的 auth_token" autofocus>
    <p>令牌保存在本机浏览器（localStorage），仅发送给你的 cast-agent。</p>
  </form>
</dialog>
<script>
(function () {
  var TOKEN_KEY = "lattice-cast-token";
  var sessionId = null;
  var busy = false;
  var token = localStorage.getItem(TOKEN_KEY) || "";

  var log = document.getElementById("log");
  var textInput = document.getElementById("text");
  var sendBtn = document.getElementById("send");
  var tokenDialog = document.getElementById("token-dialog");
  var tokenInput = document.getElementById("token");

  function needToken() {
    if (!token) tokenDialog.showModal();
  }

  document.getElementById("token-form").addEventListener("submit", function () {
    var v = tokenInput.value.trim();
    if (v) {
      token = v;
      localStorage.setItem(TOKEN_KEY, v);
    }
  });

  function scrollBottom() { log.scrollTop = log.scrollHeight; }

  function addRow(cls, text) {
    var row = document.createElement("div");
    row.className = "row " + cls;
    var bubble = document.createElement("div");
    bubble.className = "bubble";
    bubble.textContent = text;
    row.appendChild(bubble);
    log.appendChild(row);
    scrollBottom();
    return bubble;
  }

  function addLine(text, isError) {
    var div = document.createElement("div");
    div.className = "tool" + (isError ? " error" : "");
    div.textContent = text;
    log.appendChild(div);
    scrollBottom();
  }

  function handleEvent(ev, assistantBubble) {
    if (ev.type === "session") { sessionId = ev.session_id; return; }
    if (ev.type === "delta") { assistantBubble.textContent += ev.text; return; }
    if (ev.type === "tool") { addLine("→ " + ev.text + " 执行中"); return; }
    if (ev.type === "final") { assistantBubble.textContent = ev.text || assistantBubble.textContent; return; }
    if (ev.type === "error") { addLine("出错了：" + ev.text, true); }
  }

  // parseFrames 处理缓冲区里所有完整的 SSE 帧（以空行分隔），返回残包。
  function parseFrames(buf, assistantBubble) {
    var parts = buf.split("\n\n");
    var rest = parts.pop();
    parts.forEach(function (frame) {
      frame.split("\n").forEach(function (line) {
        if (line.indexOf("data: ") !== 0) return;
        var payload = line.slice(6);
        if (!payload) return;
        try { handleEvent(JSON.parse(payload), assistantBubble); } catch (e) { /* 忽略坏帧 */ }
      });
    });
    return rest;
  }

  document.getElementById("chat-form").addEventListener("submit", function (e) {
    e.preventDefault();
    if (busy) return;
    if (!token) { needToken(); return; }
    var text = textInput.value.trim();
    if (!text) return;
    textInput.value = "";
    addRow("user", text);
    busy = true;
    sendBtn.disabled = true;
    var assistantBubble = addRow("assistant", "");

    var body = { text: text };
    if (sessionId) body.session_id = sessionId;

    fetch("/chat/api/message", {
      method: "POST",
      headers: { "Content-Type": "application/json", "Authorization": "Bearer " + token },
      body: JSON.stringify(body)
    }).then(function (resp) {
      if (resp.status === 401) {
        localStorage.removeItem(TOKEN_KEY);
        token = "";
        assistantBubble.remove();
        addLine("令牌无效，请重新输入", true);
        needToken();
        return null;
      }
      if (!resp.ok || !resp.body) {
        assistantBubble.remove();
        addLine("请求失败（HTTP " + resp.status + "）", true);
        busy = false;
        sendBtn.disabled = false;
        return null;
      }
      var reader = resp.body.getReader();
      var decoder = new TextDecoder();
      var buf = "";
      function pump() {
        return reader.read().then(function (chunk) {
          if (chunk.done) {
            busy = false;
            sendBtn.disabled = false;
            textInput.focus();
            return;
          }
          buf = parseFrames(buf + decoder.decode(chunk.value, { stream: true }), assistantBubble);
          return pump();
        });
      }
      return pump();
    }).catch(function () {
      assistantBubble.remove();
      addLine("网络错误，请重试", true);
      busy = false;
      sendBtn.disabled = false;
    });
  });

  needToken();
})();
</script>
</body>
</html>
`
