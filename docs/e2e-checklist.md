# v1 端到端真机验收

按 `docs/superpowers/plans/2026-09-19-latticecast.md` Task 16、参照
`docs/superpowers/specs/2026-09-18-latticecast-design.md` 第一节验收线执行。

补验背景：v1 全部 18 个任务、以及 v1.1/v2.0/v2.1 都已实现并合并，但 Task 16 要求的这份文件此前从未
提交过——`docs/e2e-checklist.md` 在仓库历史里不存在，只有 v2 设计文档里"MP4 轻微卡顿、passthrough
真机矩阵未测"这一句侧面提及，没有正式记录测过什么、结果如何。这份文件补上这条记录。

执行日期：2026-09-27。执行环境：真实 cast-agent 进程 + 真实渲染端进程（非 mock/fake），
真实 mpv 播放真实生成的测试视频，经真实 MCP（Streamable HTTP，`github.com/modelcontextprotocol/go-sdk`）
协议驱动，命令与回包见 `docs/e2e-transcript.txt`（本次运行的完整原始记录）。

## 执行时发现并修复的两个问题

真机跑通过程中，`list_cast_devices` 把"配置了但从未被发现过的渲染端"（仅 mDNS、无静态 host，
如真实场景里侧载 APK/装 tvOS App 之前的设备）误报成 `online:true, state:"error"`，而不是
`online:false`——这正是本清单反例 6"拔电视电源/未上线应报不在线"要验的场景，之前完全没有
单元测试覆盖这条路径（`Manager.List` 内联构造 `adapter.Target{}` 发请求，没有走
`Manager.target()` 已有的判据）。根因：对 `Host=""` 的设备直接拼出 `http://:0/status` 这种
畸形地址发出去，得到的到底是连接失败还是某种响应，取决于运行机器的网络环境（有全局代理/VPN
的机器上可能被代答成正常 HTTP 响应而非连接失败）——单元测试环境里侥幸判对，真实环境（本机跑着
Clash Verge TUN）没有侥幸对。同一判据下 `manager_test.go` 里另有一个更窄的单测
（`TestOps_UnaddressedDevice`，只测 `Status()`，不测 `List()`）当时也是失败的。

两处都已按同一判据修好：`Host==""` 直接判 `device_offline`，不再发出畸形请求。
新增回归测试 `TestList_UnaddressedDevice`（原有 `TestOps_UnaddressedDevice` 保留），
`make test` 全绿。提交：见本次 PR。

## 一、真实渲染端环境

| 渲染端 | 真机/真进程 | 结果 |
|---|---|---|
| macOS/Linux（Go + mpv，`cmd/latticecast-renderer`） | 是——本机真实进程 + 真实 `mpv 0.41.0`（`/opt/homebrew/bin/mpv`），非 mock | **完整跑通，见下表** |
| Apple TV（tvOS + AVPlayer） | 是——真实 Apple TV 4K（"主卧"，`AppleTV11,1`，tvOS） | **部分**：`xcodegen generate` 后 `xcodebuild ... -destination generic/platform=tvOS` 构建成功（补了 `DEVELOPMENT_TEAM`，之前 project.yml 没配，首次构建即报签名错误，已修）；`devicectl device install app` 成功装机（`io.lattice.cast.renderer` 1.0(1)）；**启动失败**：`devicectl device process launch` 报 `System is asleep - foreground app launch forbidden`——设备处于休眠，devicectl 无法唤醒电视也无法模拟遥控器操作，需要人拿遥控器唤醒并走一遍首启配对 UI（录入 name/room/token）。这一步计划里本来就标了"手动·用户"，没有绕过的自动化路径。 |
| Android APK | 否 | 手边没有 Android TV/盒子真机或可用的 adb 环境，本轮未测。 |

## 二、核心链路（Go+mpv 渲染端，全部真实进程/真实网络请求）

配置：`mcp_listen: ":7800"`、`media_http_listen: "0.0.0.0:7810"`、媒体库指向一个用 `ffmpeg` 生成的
300 秒测试视频（`testsrc` 图案 + 440Hz 音频，非静态图片，可验证播放位置真的在前进）。渲染端
`study-mpv` 用静态 host/port 注册（`127.0.0.1:7822`），`bedroom-tv` 仅 mDNS（代表尚未发现/未上线
的设备，用于反例）。全部调用经真实 MCP 工具（`tools/list` confirm 8 个：list_cast_devices /
search_media / cast_play / cast_pause / cast_seek / cast_volume / cast_stop / cast_status），
Bearer token 鉴权，非直连内部函数。

- [x] `tools/list` 返回 8 个工具，与协议文档/MCP Server 注册一致
- [x] `list_cast_devices`：study-mpv 在线、bedroom-tv 离线（配置了但没发现过的设备，见上面的 bug 记录）
- [x] `search_media` 按标题子串命中媒体库里生成的测试片，拿到 `media_id`
- [x] `cast_play`（media_id）→ mpv 真实开始播放；3 秒后 `cast_status` 显示 `state=playing`，
      `duration_ms=300000`（与生成的 300 秒片长一致），`position_ms=3067`（≈真实流逝时间）
- [x] `cast_pause` → `state=paused`，位置停在暂停时刻（3067ms），2 秒后再查一次位置不变
- [x] `cast_seek`（跳到 60000ms）→ `cast_status` position_ms 精确变为 60000
- [x] `cast_play` 带 `position_ms` 续播 → 3 秒后位置变为 63067（60000 + 真实流逝时间），证明是真播放不是回显
- [x] `cast_volume`（level=40）→ 调用成功，无报错
- [x] `cast_stop` → `state=idle`；随后 `cast_status` 保持 idle
- [x] 反例 1——投到从未上线的设备（bedroom-tv）：`cast_play` 返回 `isError=true`，
      `device_offline: bedroom-tv not yet discovered`（对应"拔电视电源/未上线应报不在线"）
- [x] 反例 2——投到配置里根本没有的设备名：`unknown_device: living-room-does-not-exist`
- [x] 反例 3——投一个渲染端够不着的地址（模拟"笔记本本地文件"/资源不可达）：
      `cast_play` 本身就同步报错 `mpv returned to idle without playback: source unplayable`
      （3e804db 那次修复的"等真实播放开始才算成功"在这里生效了）；随后 `cast_status` 仍保留
      `state=error` 和同一条错误信息，不是静默恢复成 idle
- [x] mpv 进程随渲染端一起启动、随其退出而清理（`ps` 确认播放期间存在、`kill` 后消失），
      IPC socket 直接查询（`get_property pause/time-pos/duration/eof-reached`）与 API 报的状态一致，
      交叉验证过 API 不是在编造状态

**播放质量**：本轮用的是本机生成的合成测试片（H.264/AAC，640×360），不是真实电影文件，没有复现
v2 设计文档提到的"MP4 轻微卡顿"——那需要用真实来源的媒体文件复测，本轮未覆盖。

## 三、mesh 远程可达性（"手机在外面/蜂窝网络连回家"）

- [x] cast-agent 监听 `:7800`（全网卡），本机验证过服务本身对未认证请求正确回 401、认证后正常应答
- [ ] **未在真实第二设备上、经 Lattice mesh overlay 复测**：这台 Mac 连上 Lattice 隧道后
      （overlay IP `10.96.0.6`），尝试从共享的云测试机（`101.36.119.12`，同一 workspace 的
      mesh 对端）经 overlay IP 访问本机 `:7800`，该云测试机在测试当时 TCP 层完全不可达
      （`ping` 通、`ssh`/`curl` 到它自己的 18090 端口和到本机 overlay IP 都超时，且 Lattice
      面板同时显示"在线 0 台"）——像是那台云测试机本身或其网络当时出了问题，与 LatticeCast
      代码无关，未继续排查。
- **不算完全空白**：mesh overlay 的可达性（直连/中继、NAT 穿透、出口切换）在本项目其他会话里
  已经用这台 Mac、这台云测试机、一台 iPhone 反复验证过多次（连接建立、断线重连、出口选择延迟等），
  不是没验证过，只是这次没能用"phone 经 mesh 打真实 MCP 请求"这个具体动作再证一遍。
- 结论：cast-agent 的 HTTP/鉴权层本身没问题；"回家"这条腿依赖的是 Lattice mesh 本身的可达性，
  而不是 LatticeCast 自己的代码——这条腿建议等云测试机恢复后单独找一次时间跑一遍，不必因此
  卡住这份验收。

## 四、结果

- **核心投屏循环（说话→找媒体→下发→真实播放→查状态→暂停/继续/跳转/停止→三种反例）在真实
  Go+mpv 渲染端上完整跑通，证据是原始 MCP 请求/响应记录，不是复述。**
- 过程中发现并修了一个真实 bug（`list_cast_devices` 对未寻址设备的误报），补了回归测试。
- Apple TV 渲染端：构建 + 真机安装通过；受限于设备休眠且无法远程/自动化操作 Siri Remote，
  首启配对与真实播放未做到（计划里这一步本就标注需要人工）。
- Android APK 渲染端：本轮无可用真机/模拟环境，未测。
- mesh 远程可达性：本次会话内因共享云测试机不可达未能用第二设备复测，判定为环境问题而非
  LatticeCast 自身缺陷；建议单独找时间用一台真手机在蜂窝网络下补一次。

签署：本次由 Claude（代 winstonfly 操作）在 2026-09-27 执行并记录；上面每一条打勾的结论均来自
`docs/e2e-transcript.txt` 里的原始命令输出，没有一条是凭代码读出来的推测。
