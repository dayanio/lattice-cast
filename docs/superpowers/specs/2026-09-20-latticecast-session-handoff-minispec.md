# LatticeCast 会话模型与设备迁移（session + handoff）mini-spec

> 日期: 2026-09-20
> 性质: mini-spec（实现规范）——形态轨①，主 spec `2026-09-18-latticecast-design.md` v7 修订的直接产物
> 关联: `2026-09-18-latticecast-design.md`（§十一 v2 形态轨）、`lattice-cast` 仓 `docs/protocol.md`（唯一权威协议定义）、
>       `lattice-cast` 仓 `internal/cast/manager.go`（Manager API：Refresh/List/Play/Stop/Volume/Status + MCP 审计层）、
>       `lattice-cast` 仓 `testdata/contract/`（契约夹具，权威版本）
> 状态: 待评审（2026-09-20 对话中已评审方向，可直接排期；实施前按惯例过一遍即可）

## 修订记录

- **v1（2026-09-20）**: 初稿。会话模型 + 跨渲染端迁移（handoff）+ resume。原则：**会话只住 cast-agent**，渲染端协议仅加可选 `session_id` 回显，v1 命令式设计不受影响。

---

## 一、目标与定位

**一句话**：播放会话成为 cast-agent 的一等公民对象——"客厅看到一半，挪到卧室电视接着看"、"接着放昨晚的蚁人"这类意图，由 agent 用会话状态直接完成，进度跟着人走。

**用户旅程（验收线）**：

1. 客厅 Mac 渲染器（mac-living）播放《蚁人》至 20:00；
2. 用户对入口（MCP 客户端或 v1.1 网页聊天）说："把蚁人挪到卧室电视"；
3. LLM 调 `cast_sessions`（拿到会话）→ `cast_handoff {to_device: "卧室电视"}`；
4. 卧室渲染器（mac-bed）从 ≈20:00 继续播放，客厅停止；**位置误差 ≤ 5 秒**；
5. cast-agent 重启后，用户说"接着放昨晚的蚁人"→ LLM `cast_resume` → 从落盘会话恢复；
6. 异常路径：目标设备正在播别的 → LLM 收到 `device_busy` 向用户澄清；目标离线 → `device_offline`，旧端 best-effort 停止后照样迁移。

**对主 spec 的意义**：这是"界面跟着人走 → **会话跟着人走**"哲学在播放侧的落地，也是形态轨三件中**改动最小、用户可感知最大**的一件——渲染端三平台各改约十行，主要工作量在 agent 侧。

---

## 二、需求澄清结论

| 问题 | 结论 |
|---|---|
| 会话状态住哪？ | **只住 cast-agent**（内存 + 落盘）。渲染端零会话状态——保持 v1"渲染端是哑终端"的纪律，handoff 才能只是"旧端 stop + 新端 play"。 |
| 渲染端协议要改多少？ | 仅一处**可选字段**：`POST /play` 请求与 `GET /status` 响应增加 `session_id`（`omitempty`），渲染端存储并回显，用于审计关联与端侧日志对账。协议版本保持 v1（加法演进，不破坏契约）。 |
| `cast_play` 与会话的关系？ | `cast_play` **总是创建新会话**；同渲染端重播时旧会话自动置 `stopped`。继续已有会话走 `cast_resume`——规则单一，LLM 不需要猜。 |
| 目标端正忙怎么办？ | `cast_handoff` **不自动杀目标端播放**，返回 `device_busy` 让 LLM 向用户澄清（问"要停掉卧室正在放的 XX 吗"）。与 `cast_play` 的直接覆盖行为有意不同：迁移是重操作，宁可多问一句。 |
| 旧端离线（电视被关）怎么办？ | best-effort：stop 失败仅记审计日志，**迁移照常执行**。离线端的会话仍正常落盘（LastPosition 为最后一次轮询值）。 |
| 位置精度？ | pause/seek 时精确；stop/离线时为最后一次状态轮询值（可能滞后一个轮询间隔）。验收口径 ±5s，文档如实写"从上次位置附近继续"。 |
| 会话保留多久？ | 配置 `session.ttl`，默认 7 天，过期清扫。resume 的默认目标 = 最近更新会话。 |
| 多个活动会话时 handoff 不带 session_id？ | 返回 `ambiguous_session: N active — specify session_id`，让 LLM 列出会话问用户。**不猜**。 |

---

## 三、数据模型（agent 侧，`lattice-cast` 仓）

```go
// internal/cast/session.go
type Session struct {
    ID           string        // "sess_" + randHex(8)
    Ref          string        // 用户可见的内容引用（search_media 引用或原始 URL）
    ResolvedURL  string        // 渲染端实际拉流地址（仅为落盘完整性；失效重解析属形态轨④，本期不做）
    Title        string
    RendererID   string        // 当前挂载的渲染端 ID（manager 设备 ID）
    Room         string
    State        string        // playing | paused | idle | stopped
    LastPosition time.Duration
    CreatedAt    time.Time
    UpdatedAt    time.Time
}
```

```go
// internal/cast/sessionstore.go —— 落盘（JSON 数组，save-on-mutation，文件小）
type SessionStore struct{ path string; mu sync.Mutex }
func LoadSessionStore(path string, ttl time.Duration) ([]Session, error) // 过期滤除；损坏→改名 .bak 后视为空
func (s *SessionStore) Save(all []Session) error
```

**归属与接线**：会话状态挂在既有 `internal/cast.Manager` 上（它已串行化全部设备操作），新增字段 `sessions []*Session` + `store *SessionStore`。捕获点：

| 事件 | 会话动作 |
|---|---|
| `Manager.Play` 成功 | upsert 新会话（同端旧活动会话置 `stopped`）；`/play` 下发时携带 `session_id` |
| `Manager.Status` 轮询回包 | 按 `session_id` 更新 `State` / `LastPosition` / `UpdatedAt` |
| `Manager.Pause` / `Seek` | 立即更新（pause 精确） |
| 渲染端 EOF→idle（既有行为） | `State=idle`，位置保留 |
| `Manager.Stop` | `State=stopped`，位置为最后轮询值 |
| 进程启动 | 载入落盘会话（TTL 滤除）+ 每小时清扫 |

---

## 四、协议变更（`docs/protocol.md` 唯一权威）

```
POST /play  { url, title?, position_ms?, session_id? }   # session_id 可选，渲染端存储并回显
GET  /status -> { state, position_ms, duration_ms, title?, session_id?, error? }
```

- `session_id` 序列化带 `omitempty`（旧流程不出现）；夹具新增两组：play-with-session、status-echo。
- **向后兼容**：字段可选、加法演进，协议版本号不变；三平台渲染端（Go+mpv / tvOS+AVPlayer / Android+ExoPlayer）各改约十行（存字符串 + 回显），共用同一组契约夹具。
- 渲染端对 `session_id` **零行为依赖**——丢了也不影响播放，只影响审计对账。这条写进 protocol.md。

---

## 五、MCP 工具（新增三个，走既有审计层）

```
cast_sessions (读)
  -> [{session_id, title, ref, state, device, room, position_ms, updated_at}]  # 按 UpdatedAt 降序
cast_handoff (写)  { to_device: string (必填), session_id?: string }
  流程: 目标端 idle 校验 → best-effort stop 旧端 → 新端 /play{url, position_ms=LastPosition, session_id} → 会话迁移落盘
cast_resume (写)   { session_id?, device?, position_ms? }
  默认: 会话=最近更新者; 设备=该会话上次的渲染器(在线) else default_room 的设备 else 报 need_device
```

错误字符串（LLM 可直接向用户转述，与既有 `unknown_device` 风格一致）：

```
unknown_device: %s
ambiguous_session: %d active — specify session_id
session_not_found: %s
device_offline: %s
device_busy: %s (playing %q) — ask user or cast_stop first
need_device: no online renderer for session — specify device
```

---

## 六、配置（`config.example.yaml` 同步；注意 KnownFields 严格解析——旧二进制拒绝新字段，发布说明必须提示重建重启）

```yaml
session:
  ttl: 168h          # 会话保留时长，默认 7 天
  store_path: ""     # 默认 <config 目录>/sessions.json
  default_room: ""   # 可选：resume 无在线旧端时的首选房间
```

---

## 七、测试策略

- **单元**（`internal/cast` 会话部分）：创建/覆盖（同端旧会话置 stopped）、轮询回写、handoff 迁移与落盘、resume 默认规则（旧端在线/离线/default_room/全无）、TTL 清扫、store 损坏恢复（.bak）；
- **契约**：play-with-session / status-echo 两组夹具进 `testdata/contract/`，三平台实现共享（tvOS/Android 侧跑各自契约 runner）；
- **MCP 层**：三个工具的错误字符串逐字断言（对齐 T11 风格）；
- **e2e（演示环境，双渲染器）**：播放中 handoff 位置连续性 ±5s；agent 重启后 resume；目标忙/离线路径。

## 八、验收标准

1. 演示环境（mac-living / mac-bed）播放中 `cast_handoff` → 新端从最近位置继续、旧端停止，误差 ≤ 5s；
2. cast-agent 重启后 `cast_resume` 恢复上次会话（含内容与位置）；
3. 三平台渲染端 `/status` 均回显 `session_id`，共享契约夹具全绿；
4. 经 v1.1 网页聊天一句话"接着放昨晚的蚁人"达成——LLM 仅凭 `cast_sessions` + `cast_resume` 完成，无人工参数；
5. `device_busy` / `device_offline` / `ambiguous_session` 路径下 LLM 能依据错误字符串向用户澄清并继续完成任务。

## 九、非目标（本期明确不做）

- 多用户/按人区分的会话（需家庭身份体系，v3；届时与 AgentIdentity 自然人身份衔接）；
- 连播/意图队列（形态轨④，独立 mini-spec）；
- 渲染端侧任何会话状态（会话只住 agent）；
- 跨 cast-agent 实例共享会话（单 agent 架构不变）；
- stop 位置秒级精确保证（轮询语义如实文档化）。

## 十、实施备注

- 工作量预估：agent 侧 2-3 天 + 三平台渲染端各 <0.5 天 + 契约/e2e 1 天；
- **实施时机：不阻塞 v1 收口**（v1 用户侧待办：115 重登/APK 侧载/Xcode 装 Apple TV/T16/T15）。建议 v1 merge 后开 `feat/v2-session` 分支实施；
- 能力协商（形态轨②）与 DLNA 兜底同批出联合 mini-spec；overlay 通道（形态轨③）独立 mini-spec。
