# LatticeCast 协议（v1）

> 本文是 LatticeCast 自有协议的**唯一权威定义**（wire protocol v1），覆盖 cast-agent（Go）与渲染端（Android APK）之间的全部线上行为。
> 两端各自实现（Go 侧 `internal/cast/adapter/latticecast`，Kotlin 侧 `RendererServer`），共用 `testdata/contract/` 下的 JSON 契约夹具做契约测试，保证两端不漂移。
> **修改本文任何字段名 / 取值 / 状态迁移，必须同步修改夹具与两端实现。**

## 一、总则

```
传输:   HTTP/JSON over 家庭局域网
鉴权:   Bearer Token（cast-agent 配置持有，APK 首次运行录入）
发现:   渲染端 mDNS 发布 _latticecast._tcp，TXT 携带 room / 协议版本
```

约定：

- 报文编码 UTF-8，`Content-Type: application/json`；字段名全部小写 snake_case；
- 时长 / 位置一律为毫秒整数（`*_ms`）；
- 所有成功响应 HTTP `200`；应用层失败通过 `ok=false` / `error` 字段表达，不滥用 HTTP 状态码。

设计原则：

- **命令式而非会话式**——无连接状态，cast-agent 重启即恢复控制；
- **进度由渲染端维护**，cast-agent 轮询 `GET /status`（v1 不做推送）；
- 拉流由**渲染端自己发起**（ExoPlayer 直接拉 `url`），cast-agent 只下发渲染端可达的播放 URL。

端点一览：

```
POST /play    { url, title?, position_ms? } -> { ok, state }
POST /pause   -> { ok, state }
POST /stop    -> { ok, state }
POST /seek    { position_ms }               -> { ok, state }
POST /volume  { level }                     -> { ok, state }   // level: 0-100
GET  /status  -> { state: playing|paused|idle|error, position_ms, duration_ms, title?, error? }
```

## 二、鉴权

除 mDNS 发现外，所有请求必须携带请求头：

```
Authorization: Bearer <token>
```

token 缺失或不符时返回 `401`：

```json
{"ok":false,"error":"unauthorized"}
```

## 三、mDNS 发现

渲染端在家庭局域网发布服务类型 **`_latticecast._tcp`**，服务实例名即设备显示名（如 `卧室电视`）。TXT 记录：

| 键 | 含义 | 示例 |
|---|---|---|
| `room` | 房间名，cast-agent 据此归位（人工确认后以配置固化为准） | `room=卧室电视` |
| `v` | 协议版本 | `v=1` |

## 四、端点

### POST /play

播放指定 URL（渲染端自行拉流）。

请求：

```json
{"url":"http://…","title":"星际穿越","position_ms":0}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `url` | string | 是 | 渲染端可达的媒体地址（NAS 直链 / 公网 MP4/HLS / YouTube 提流地址） |
| `title` | string | 否 | 展示标题（极简 UI / 审计用） |
| `position_ms` | int | 否 | 起播位置（毫秒），缺省 0 |

成功响应：

```json
{"ok":true,"state":"playing"}
```

渲染端解码失败 / URL 不可达时（HTTP 仍为 200，应用层报错，渲染端停留在 `error` 状态）：

```json
{"ok":false,"state":"error","error":"source_unreachable"}
```

### POST /pause

暂停。`playing → paused`；已在 `paused` 时幂等。

```json
{"ok":true,"state":"paused"}
```

### POST /stop

停止并清空当前媒体。`playing|paused → idle`；已在 `idle` 时幂等；在 `error` 态不产生迁移（见第六节）。

```json
{"ok":true,"state":"idle"}
```

### POST /seek

跳转播放位置，不改变播放状态。

请求：

```json
{"position_ms":N}
```

响应（`state` 为执行后的当前状态，原样返回）：

```json
{"ok":true,"state":"…"}
```

### POST /volume

设置音量，不改变播放状态。

请求（`level` 为 0-100 整数）：

```json
{"level":42}
```

响应（`state` 为执行后的当前状态，原样返回）：

```json
{"ok":true,"state":"…"}
```

> `/pause` `/seek` `/volume` 在不适用状态下（如 `idle` 时收到 seek）幂等：不报错，返回当前 `state`，不产生状态迁移。
> v1 无独立 resume 端点：`paused → playing` 通过再次 `POST /play` 完成（携原 `url`，可用 `position_ms` 续播）。

### GET /status

查询当前状态。**200 响应不含 `ok` 字段**，以 `state` 为准。

```json
{"state":"playing|paused|idle|error","position_ms":N,"duration_ms":N,"title":"…","error":"…"}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `state` | string | `playing` / `paused` / `idle` / `error` 四态之一 |
| `position_ms` | int | 当前播放位置（毫秒）；`idle` 时为 0 |
| `duration_ms` | int | 媒体总时长（毫秒）；未知或 `idle` 时为 0 |
| `title` | string | 当前媒体标题；无媒体时为 `""` |
| `error` | string | 出错原因（如 `source_unreachable`）；无错误时为 `""` |

播放中：

```json
{"state":"playing","position_ms":42000,"duration_ms":5400000,"title":"Interstellar","error":""}
```

出错：

```json
{"state":"error","position_ms":0,"duration_ms":0,"title":"","error":"source_unreachable"}
```

## 五、通用错误

| HTTP 状态码 | 响应体 | 场景 |
|---|---|---|
| `401` | `{"ok":false,"error":"unauthorized"}` | Bearer Token 缺失或不符 |
| `404` | `{"ok":false,"error":"not_found"}` | 未知路径 |
| `405` | `{"ok":false,"error":"method_not_allowed"}` | 路径存在但 HTTP 方法不符 |
| `400` | `{"ok":false,"error":"bad_request"}` | 请求体非合法 JSON 或缺失必填字段 |

## 六、状态机

```
            play                     pause
   idle ───────────▶ playing ────────────▶ paused
    ▲                 │  ▲                 │
    │                 │  └───── play ──────┘
    └────── stop ─────┴─────────────────────┘
```

- `idle →(play)→ playing`：`/play` 开始播放新媒体；
- `playing ⇄ paused`：`/pause` 暂停；再次 `/play`（携原 `url`，可带 `position_ms`）恢复；
- `(playing|paused) →(stop)→ idle`：`/stop` 停止并清空媒体；
- **`error` 仅由 `/play` 失败进入**（解码失败 / URL 不可达，从任意状态均可）；离开 `error` 只有一种方式——**下一次 `/play`**：成功则 `playing`，再次失败则停留 `error`。`/pause` `/stop` `/seek` `/volume` 均不改变 `error` 态。

## 七、契约夹具

`testdata/contract/` 是两端契约测试的共同基准，Go 与 Kotlin 两侧测试直接读取：

| 文件 | 内容 |
|---|---|
| `play_request.json` | `POST /play` 请求体样例 |
| `play_response.json` | `POST /play` 成功响应样例 |
| `status_response.json` | `GET /status` 播放中样例 |
| `status_error_response.json` | `GET /status` 出错样例 |
| `volume_request.json` | `POST /volume` 请求体样例 |

编码约定：所有报文为单层 JSON 对象、单行紧凑格式；`error` 无错误时为空字符串 `""`（不是 `null`）；`GET /status` 的 200 响应不含 `ok` 字段。
