# v0.14 实施路线（ROADMAP-v0.14.md，草案）

> Phase 4 草案：把 PROTOCOL.md、SUBAGENTS.md 与生态调研的结论拆成可执行批次。**需在 Phase 3 宿主评审通过后定稿**。顺序按依赖与风险排列：先修现有缺陷、再加纯增量能力、最后上新协议面。

## 批次总览

> **进度（2026-10-23）**：B0–B7 全部完成（B0 第 7 条"告知宿主"按作者决定不做，宿主只需按迁移文档升级）。实现与宿主迁移说明见
> [UPGRADE-v0.14.md](../UPGRADE-v0.14.md)（第 8 节为按宿主的迁移清单）。B7 原定 v0.15，已提前并入 v0.14。

| 批次 | 主题 | 内容 | 风险 | 宿主影响 |
|---|---|---|---|---|
| **B0** | 即时修复 | 补 LICENSE（需作者确认 MIT）；v1 三个缺陷；`tool_is_error` 等三个加字段；删除 websocket/；`GET /protocol` 补全；TS 客户端 `steer()` 等缺陷；engine 侧 MCP 前缀 bug 告知 | 低 | 只有收益 |
| **B1** | 会话事件总线 | 每会话 seq（锁内分配）、交互原语按会话路由、环形缓冲、`/runs/{session}/stream?after=` 续传 | 中（动 http.go 的流管理核心） | 无破坏；amobileCreater 可删掉 3 秒轮询 |
| **B2** | 自定义事件 | `ctx.Emit`、`App.EmitEvent`、`RegisterEvent`；v1 的 `custom` 帧 | 低 | AnimeCreater 可选迁移自建事件总线 |
| **B3** | 子代理治理 | SUBAGENTS.md S1–S4：统一句柄、插话下沉、`agent_send`/`agent_status`、按子代理中断、子事件并入父流 | 中（动 loop.Scope 与 agenttool） | 新增能力，默认行为不变 |
| **B4** | MCP 换芯 | mcp/ 内部换官方 SDK v1.8.0，保留门面；接通取消、进度、图片、list_changed | 中 | engine 编译验证；门面不变 |
| **B5** | AG-UI 剖面 | 从 schema 生成类型；映射 mapper；审批停车；`/agui` 与 `/agui/capabilities`；一致性测试（schema 校验 + 官方 HttpAgent） | 中（新协议面） | 无（新端点） |
| **B6** | 可观测与测试 | OTel 导出（gen_ai.* 语义约定）；公开脚本化模型；httprr 式录制回放 | 低 | 只有收益 |
| B7（v0.15） | 扩展 | AG-UI 前端工具与改参审批；MCP elicitation ↔ ask_user；把 GoAgent 工具/代理作为 MCP 服务端暴露；可插拔分类器接口 | — | — |

## B0 明细（可立即开始）

> **进度（2026-10-23）**：第 2、4、5、6 条已完成；第 3 条完成 `tool_is_error` 与缓存 token（`interrupted.reason` 需要把原因从 `/interrupt` 经中断处理器传到循环，并入 B1）。第 1 条 LICENSE 已补（作者确认 MIT）。**剩余：第 7 条告知宿主。** 记录见 UPGRADE-v0.13.md §1.9。

1. ~~**LICENSE**~~：已补 MIT LICENSE（作者确认）。
2. **v1 缺陷**：
   - seq 在写锁外分配 → 移入锁内（B1 会彻底重构，B0 先做最小修复）。
   - 会话忙且未开插话时，被拒绝的请求不得触碰运行中流的订阅。
   - RunHTTP 默认创建 AskUserHandler，HTTP 模式下不再回退读 stdin。
3. **v1 加字段**：`tool_done.tool_is_error`、`interrupted.reason`、`usage` 缓存 token。
4. **删除** websocket/（未接线，只回显）。
5. **`GET /protocol`**：补全遗漏的 10 个路由；标注 plan_confirm/interrupt 为保留未用。
6. **TS 客户端**：修 `steer()`（改用 `/queue`）、`sessions()` 类型、补 `queued`/`resume_queue`、导出 `FrameType`/`PROTOCOL_VERSION`、加鉴权头选项、重新构建 dist；typescript 移到 devDependencies；版本升到 0.2.0。
7. **告知宿主**（不改宿主代码，评审时同步）：
   - amobileCreater 桌面端按 `'error'` 判断流水线节点失败，框架实际发 `'failed'`。
   - engine 的 MCP 工具归属前缀未做名称清洗（应使用 `mcp.ServerPrefix`）。

## 验收标准（每批通用）

- `go build ./...`、`go vet ./...`、`go test -race ./...` 全部通过。
- amobileCreater engine 针对改动后的框架 `go build ./...` 通过（replace 直连）。
- 新增能力有回归测试；缺陷修复有能复现原缺陷的测试。
- 线协议变化同步更新 `GET /protocol`、TS 客户端与 UPGRADE 文档。

## Phase 3 评审清单（提交给宿主维护者）

1. PROTOCOL.md §8 的 5 个决策点。
2. SUBAGENTS.md §6 的 4 个决策点。
3. 两个宿主 bug（B0 第 7 条）。
4. AnimeCreater 是否计划从 ba8a515 升级到 v0.13+（符号层面安全，但有 v0.12 的行为变化，见 UPGRADE-v0.12.md）。
5. 自定义事件与续传上线后，两个宿主是否迁移各自的自建实现（可选）。
