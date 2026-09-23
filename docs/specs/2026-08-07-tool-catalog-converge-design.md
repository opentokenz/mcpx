# MCPX：公开工具收敛、域分包与提示词文件化

- 记录日期：2026-08-07
- 状态：设计已确认；2026-09-23 起公开 schema 根上不再使用 `oneOf` 收紧 required，改为扁平 object + `action.description` 说明，详见 `docs/plans/2026-09-23-flatten-tool-input-schema.md`
- 前置：`2026-08-07-official-go-sdk-human-model-split-design.md`（content / structuredContent 分流已落地）
- 宿主：能调用 MCP 的 GPT；本地终端观测仅人看

## 1. 目标

1. **A — 减 tools/list 数量**：同域多动词合并为 `action` / `view` + `oneOf`，约 **31 → 20–22** 个公开工具。
2. **B — 降选错率与 schema 噪声**：缩短工具 description；硬约束留在 property description 与 SC `error` / `actions`；删除公开 `progress_report`。
3. **Breaking 一次到位**：无旧工具名、无别名路由、无双写兼容层。
4. **代码按域分包**：同一种能力的读/写 handler、schema 片段、测试落在同一包。
5. **提示词文件化（P1）**：工具文案与 agent guidance 以 **go:embed** 配置文件维护；**不做**运行时用户覆盖。
6. **Artifact Resource 编码**：宿主通过 Resource URL 直接打开时，文本以 UTF-8 + MIME charset 正确交付（与收敛同批）。

## 2. 非目标

- 不改 changeset / command / session 等**业务语义**（确认流、digest、base_sha256、命令策略）。
- 不重做 go-sdk 与 content·SC 契约。
- 不把读+写合成单一 MCP tool（避免污染 `ReadOnlyHint`）。
- 不把 `skill_call` / `mcp_call` 并进 `extension_discover`。
- 不从配置文件生成 JSON Schema 的 type/required/enum 结构。
- 不提供 `~/.mcpx` 热更新 prompt（可后续单独立项）。
- 不做 bash-only 极简 agent（MCPX 是受控网关）。
- **本轮不做**无 BOM 的 GBK/GB18030 自动解码（避免误判）；unknown charset 宁 Blob 不花屏。

## 3. 背景与依据

- 模型主路径已是 **structuredContent**，不再依赖「工具名 + 长摘要」猜下一步；此前拆分 `change_prepare|apply|…` 的前提弱化。
- 高分 coding agent 实践（OpenHands 核心 3 工具 + command 分支、Claude Code SIMPLE=Read/Edit/Bash、Cline 读写分工具、SWE-agent 短 docstring）支持：**少公开名 + 分支参数 + 读写分界 + 错误导航**。
- MCPX 额外保留 session / operation / secret 等网关工具，公开面目标约 20–22，而非 3。

## 4. 公开工具目录（冻结）

### 4.1 目标清单

| 工具 | 角色 | 分支 | Annotation |
| --- | --- | --- | --- |
| `workspace_read` | 读 | `view=list \| changes \| snapshot \| diff \| watch \| memory \| history` | ReadOnly |
| `session` | 写/生命周期 | `action=open \| update \| handoff \| attach \| close` | Session |
| `session_read` | 读 | `view=list \| summary \| events` | ReadOnly |
| `source_read` | 读 | `view=file \| search \| list \| context` | ReadOnly |
| `change` | 写 | `action=prepare \| discard \| apply \| revert` | Mutating |
| `change_read` | 读 | `view=diff \| history` | ReadOnly |
| `command_run` | 执行 | （无 action 树） | Command |
| `task` | 写 | `action=attach \| stop \| stdin` | Mutating |
| `task_read` | 读 | `view=list \| status \| logs \| ports \| diagnostics` | ReadOnly |
| `plan` | 写 | `action=create \| start_task \| complete_task \| block_task \| replan \| deliver` | Mutating |
| `plan_read` | 读 | 按 `plan_id` | ReadOnly |
| `operation_batch` | 编排 | — | Mutating |
| `operation_manage` | 异步 | `action=status \| wait \| result \| cancel \| resume` | Session |
| `runtime_read` | 读 | `view=capabilities \| project \| instructions` | ReadOnly |
| `environment_read` | 读 | `view=current \| compare` | ReadOnly |
| `environment` | 写 | `action=snapshot_create` | Session |
| `extension_discover` | 读 | `kind` + `view=list \| describe` | ReadOnly |
| `skill_call` | 执行 | — | Command |
| `mcp_call` | 执行 | — | Command |
| `artifact_read` | 读 | `view=list \| content` | ReadOnly |
| `artifact` | 写 | 固定登记产物（可无 enum，或仅 `action=register`） | Mutating |
| `screenshot_capture` | 读 | — | ReadOnly |
| `secret_provide` | 敏感写 | — | Secret |

约 **20–22** 个；不为凑整数硬并 skill/mcp。

### 4.2 删除的公开名

- `change_prepare` / `change_discard` / `change_apply` / `change_revert` → `change`
- `session_open` / `session_transition` → `session`
- `task_control` → `task`
- `plan_create` / `plan_transition` → `plan`
- `workspace_list` / `workspace_observe` / `workspace_history_read` → `workspace_read`
- `environment_snapshot_create` → `environment`
- `artifact_register` → `artifact`
- **`progress_report`**：从 tools/list **删除**；进度靠终端观测与对话，不单开公开工具

旧名调用 → 标准「tool not registered」。无兼容层。

### 4.3 统一约定

1. **写工具**：顶层必填 `action`（或固定单动作）；`oneOf` 按 action 收紧 required。
2. **读工具**：顶层必填 `view`（`plan_read` 等以 id 为主的可例外）。
3. **对外会话键**：仅 **`session_id`**（catalog 映射可保留内部 remote 字段，**不对外双名**）。
4. **工具 description**：≤ 约 2 句；字段级硬约束（digest、sha256 原样复制）留在 property description + 服务端校验。
5. **失败/确认导航**：SC `error.details` / `actions` / `next_action` 使用**新工具名 + action/view**。
6. **业务 handler**：优先 dispatch 到现有实现，不重写语义。

### 4.4 `change` 分支（核心）

| action | 必填要点 | 对应现逻辑 |
| --- | --- | --- |
| `prepare` | `operations`, `purpose`；可选 apply/format/verify/idempotency | `change_prepare` |
| `discard` | `changeset_id`, `purpose` | `change_discard` |
| `apply` | `changeset_id`, `expected_digest`, `purpose`；可选 confirmation_token | `change_apply` |
| `revert` | `changeset_id`, `purpose`；可选 confirmation_token | `change_revert` |

`change_read`：`view=diff|history` 语义不变。
`source_read` 文案中「填入 change_prepare…」改为 **`change` action=prepare**。

### 4.5 其他写域分支

| 工具 | action |
| --- | --- |
| `session` | open, update, handoff, attach, close |
| `task` | attach, stop, stdin |
| `plan` | create, start_task, complete_task, block_task, replan, deliver |
| `environment` | snapshot_create |
| `artifact` | register（或固定单动作无 enum） |

### 4.6 environment 说明

- **`environment_read`**：读当前环境或与快照比较（`view=current|compare`）。
- **`environment`**：写侧，目前仅 `action=snapshot_create`（原 `environment_snapshot_create`）。
- 读写分 tool 名，**代码同包**；不是新业务能力。

## 5. 代码目录架构

### 5.1 原则

1. **同域读写同一包**：handler、schema 片段、域内测试、prompts 同目录。
2. **catalog 只编排**：`registerTools` 调用各域 `Register`，不塞业务。
3. **业务语义留在 `internal/<domain>`**（changeset、environment、remotesession…）；`server/tools/*` 为 MCP 适配层。
4. 公开 tool 名可读写分拆；包内函数无需再按旧公开名拆散。

### 5.2 目标结构

```text
internal/server/
  catalog.go                      # registerTools → 各域 Register
  guidance/
    agent.yaml                    # go:embed；跨工具行为规则
    load.go
  tools/
    change/
      register.go                 # 注册 change + change_read
      write.go                    # action 分支
      read.go
      schema.go                   # type/required/enum 结构
      prompts.yaml                # go:embed 文案
      change_test.go
    session/
    source/
    task/
    plan/
    workspace/
    environment/                  # environment_read + environment
    operation/
    extension/                    # discover + skill_call + mcp_call
    artifact/
    command/
    runtime/
    secret/
    screenshot/
```

迁移策略：可先以 `change` 为样板，再批量搬迁；或一次按域切开。实现计划中选定。

### 5.3 与现状对照

- 现状：`internal/server/tools_*.go` 扁平 + 巨型 `tools_catalog.go`。
- 目标：域包 + 薄 catalog；删除或收缩旧扁平文件。

## 6. 提示词文件化（方案 P1）

### 6.1 范围

| 进文件（embed） | 留代码 |
| --- | --- |
| 工具 `description` | JSON Schema 的 type / required / enum / oneOf 结构 |
| 参数说明文案 | annotation（ReadOnly 等） |
| action/view 的说明句（可选） | handler 逻辑、校验 |
| `guidance/agent.yaml` 中 rules、routing、response contract 文案 | 版本号可与文件同处或代码常量 |

### 6.2 加载规则

1. 使用 **`go:embed`**，文案随二进制版本固定。
2. **不做** `~/.mcpx` 运行时覆盖（非目标）。
3. 注册时：Go 构建 schema 骨架 → 注入 prompts 中的 description 字符串。
4. **缺 key / 解析失败 → 启动或注册 fail-fast**（禁止静默空 description）。
5. `agent_guidance` 从 YAML 加载，替换 `defaultAgentGuidance` 中大段 Go 字符串；结构字段（Required bool 等）可仍在 YAML 或少量 Go 默认。

### 6.3 示例形状（非最终字段名）

```yaml
# tools/change/prompts.yaml
tools:
  change:
    description: |
      准备、丢弃、应用或回滚 Changeset。通过 action 选择动作。
    actions:
      prepare: 校验 operations 并生成草稿；可选同次 apply。
      apply: 应用已准备草稿；需要时带 confirmation_token 重试。
    properties:
      expected_digest: 必须原样复制 prepare/read 返回的 digest…
  change_read:
    description: 读取 Changeset diff 或 history。
```

```yaml
# guidance/agent.yaml
version: "2.0"
priority: high
summary: 使用专用 MCPX 工具检查和修改；command_run 仅用于明确要求的命令。
rules:
  - 工具结果：content 是人读摘要；模型必须读 structuredContent…
  - 文件修改使用 change（action=prepare|apply|…）；…
tool_routing:
  read_code: [source_read]
  edit_code: [change, change_read]
```

### 6.4 与 B 的关系

- tools/list 上的 description **短**；细则在 property prompts 与 guidance，而非单工具 2KB 散文。
- 收敛改名时只改 YAML + schema 枚举，减少 Go 字符串遗漏。

## 7. 错误、导航与观测

- 缺/非法 `action` 或 `view` → `bad_request`，message 列出合法枚举。
- 业务错误码、confirmation_token、digest 校验语义不变；文案中的工具名改为新名。
- 删除 `progress_report` 后：guidance 去掉「必须先 progress_report」类规则；可选保留顶层 `progress_summary` 请求字段（若现网仍用）——实现时对照当前 envelope，**不**为删除 progress 工具而强制删请求字段，除非无引用。
- 终端观测继续 human-only 异步；不新增模型可读观测 API。

## 8. Artifact Resource 编码（与收敛同批必做）

### 8.1 问题（用户路径）

宿主通过登记返回的 **Resource URL**（`mcpx://remote-sessions/{session}/artifacts/{id}`）直接 `resources/read` 打开时，经常编码不对。

根因（现状）：

- `resourceArtifact` / `artifact.Read` 用 `utf8.Valid` + 扩展名 MIME 决定 Text vs Blob，**不走** `file.DetectFormat`。
- 文本用 `string(rawBytes)` 当 UTF-8；MIME **无 charset**，中文宿主易按系统默认编码打开。
- MIME 仅 `mime.TypeByExtension`，`.log` / 无后缀等易成 `application/octet-stream` → 错误走 Blob。
- 分片 `ReadAt` 可切断多字节序列，误判为非文本。

### 8.2 目标行为

| 路径 | 要求 |
| --- | --- |
| **resources/read（Resource URL）** | 文本类：以 **UTF-8 `Text`** 交付；`MIMEType` **必须带 charset**（如 `text/plain; charset=utf-8` 或原类型 + charset）。真二进制才 `Blob`。 |
| **artifact_read 工具** | SC 增加 `format`（对齐 source：`charset` / `bom` / `line_ending` 等，能检则检）；`encoding` 仅表示 `data` 载荷是 `utf-8` 字符串还是 `base64`，**不是**磁盘原始编码。 |
| **Register** | 推断 MIME 时复用 `file` 侧检测能力（路径 + 内容前缀），避免纯扩展名误判；可选持久化检测到的 charset 供后续打开一致。 |

### 8.3 实现要点

1. 复用 `internal/file.DetectFormat`（及既有 MIME 辅助，若有 `detectMIME` 则复用）。
2. **UTF-8（含 BOM 剥离后逻辑与 source 一致）**：Resource 返回 `Text`，MIME 带 `charset=utf-8`。
3. **UTF-16 LE/BE（有 BOM）**：解码为 UTF-8 再 `Text` + `charset=utf-8`（宿主「直接打开」统一可读）；SC `format.charset` 保留原始检测值。
4. **charset=unknown 且非可靠文本**：走 `Blob`，MIME 保持二进制；**禁止**把非法 UTF-8 字节 `string()` 后当 Text。
5. **本轮不做完整 GBK 启发式解码**（`file.detectCharset` 当前也不覆盖无 BOM 的 GBK）；若 unknown，宁 Blob/明确错误，不静默花屏。若后续加 GBK，单独立项。
6. 分片 `artifact_read`：文本窗口避免切断 UTF-8 码点（回退到 rune 边界）；或对非 0 offset 的 text 窗口在 SC 标明可能截断。
7. 代码落在 `tools/artifact` 与 `internal/artifact`，与域分包一起改。

### 8.4 验收（编码）

- 单测：UTF-8 无 BOM、UTF-8 BOM、UTF-16LE BOM 的 Resource → `Text` 可读且 MIME 含 `charset=utf-8`。
- 单测：随机二进制 / invalid UTF-8 → `Blob`，不出现花屏 Text。
- 单测：`artifact_read` SC 含 `format.charset`（至少 UTF-8 路径）。
- 回归：登记 ResourceLink 的 URI 仍可 `resources/read`。

## 9. 测试与验收

1. `public_catalog_test`：目标名单、数量区间、关键 schema（`change` oneOf、`operation_manage` 分支）。
2. change recovery / session / plan / task / workspace / environment 测试改用新名与 payload。
3. agent_guidance 测试：新工具名、无 `progress_report` 强制、无旧 change_* 名。
4. prompt loader：缺 key 失败；关键 description 非空。
5. operation_batch 嵌套禁止名单含新名。
6. **§8 artifact Resource 编码单测**。
7. `go test ./internal/server/...` 与 `./internal/artifact/...`（及触及包）通过。
8. 全仓字符串检索旧公开工具名（测试夹具与文档除外策略在实现计划中约定）。

## 10. 实现分期（设计层）

| 期 | 内容 |
| --- | --- |
| P0 | 目录骨架 + prompt/guidance loader；`change` + `change_read`；catalog/guidance/测试 |
| P1 | `session`/`session_read`、`task`/`task_read`、`plan`/`plan_read` |
| P2 | `workspace_read`、`environment`/`environment_read`、**`artifact`（含 §8 Resource 编码）**；删 `progress_report`；其余域迁入分包；description 全面文件化；全仓改名 |

**节奏**：用户确认 **一次做完 P0–P2**（无兼容，可单 PR 或连续提交）。计划中可按域 commit，但不保留旧公开 API。
**artifact 编码与目录收敛同批交付**，不拆独立发布。

## 11. 风险

| 风险 | 缓解 |
| --- | --- |
| 客户端/文档瞬时失效 | 已接受 breaking；README/agent 文案同步 |
| `session.open` 参数面大 | oneOf 严格拆 open vs close 的 required |
| description 过短漏约束 | digest/sha256 等硬约束禁止从 property prompts 删除；服务端校验保留 |
| embed 与代码双源 | 仅文案在文件；结构只在 Go；缺 key fail-fast |
| 大搬迁 diff | 以 change 为样板；测试锁行为 |
| 无 BOM 的 GBK 仍可能不对 | 本轮明确不启发式解码；unknown 走 Blob；避免假 UTF-8 Text |
| Resource 从 Blob 改为 Text 的宿主差异 | MIME 显式 charset；单测锁 Text 路径 |

## 12. 已确认决策摘要

| 决策 | 选择 |
| --- | --- |
| 收敛策略 | 方案 1：域合并 + 读写分工具 |
| 兼容 | 无 |
| progress_report | 公开删除 |
| artifact | 合并为 `artifact`；**Resource URL 编码与收敛同批修** |
| environment | `environment_read` + `environment(action=snapshot_create)` |
| 代码组织 | A：`internal/server/tools/<domain>` |
| 提示词 | P1：go:embed 文件；不做运行时覆盖 |
| 实现 | 一次完成 P0–P2（含 §8） |

## 13. 下一步

1. 用户审查本规格（含 §8 Artifact Resource 编码）。
2. 调用 writing-plans 编写可执行实现计划（文件级步骤、测试、提交节奏；artifact 编码与域分包同批）。
3. 按计划实现；完成前 verification-before-completion。
