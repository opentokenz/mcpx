# 摊平公开工具 input schema

## Goal

让 `tools/list` 能被 Claude Code 和官方 MCP TypeScript SDK 接受。公开工具的 input schema 根上不再使用 `oneOf` / `anyOf` / `allOf`。`mcp_tool` 不再公布 `outputSchema`。调用参数和各工具 handler 保持现状。

摊平的代价是公开 schema 不再表达「条件必填」与「跨 action 字段互斥」：这两类约束改为只由 `action.description` 说明、由执行期 handler 校验。该变化在 Decisions、Design、Risks 里显式记账，不留给人从实现里推断。

对应上游 issue：<https://github.com/opentokenz/mcpx/issues/28>。

## Context

`cleanActionTool` 把各 action 的字段合并进根 `properties`，同时在根上挂 `oneOf`。根对象故意不设 `additionalProperties`（代码注释写明是为了让客户端先校验属性、再评估联合类型；摊平后这段注释一并删除）。`operation_manage` 在 `cleanCoreTool` 生成的对象上再补两段 `oneOf`。

上游行为（2026-09 查证，<https://code.claude.com/docs/en/mcp> 的 root-level combinator 一节）：Anthropic Messages API 不接受工具 `input_schema` 根上的 `oneOf` / `anyOf` / `allOf`，但接受嵌在 `properties` 里的组合关键字，且任何一个工具的 schema 不合法都会让**整次请求** 400。Claude Code 在 v2.1.195 之前会跳过每个带根组合关键字的工具；v2.1.195 起改为自己把根组合关键字摊平成单个 object，并把分支 `required` 降级成工具 `description` 里的一句说明。也就是说「客户端自愈」取决于客户端版本，且摊平结果不由我们控制；服务端自己摊平能拿到与上游 fallback 等价、但不依赖版本的行为。

`mcp_tool` 的 `outputSchema` 只有 `$id: mcpx.mcp_tool_result.v1` 和 `description`，没有 `type`。官方 TypeScript SDK v1 的 `Tool` 类型要求 `outputSchema.type` 为 `"object"`，缺 `type` 会让整份 `tools/list` 解析失败；v2.0（对应 MCP 2026-07-28）已放开为任意 JSON Schema 类型（modelcontextprotocol/typescript-sdk#1149、#1169），MCP 2026-07-28 也确认 `outputSchema` 可选、`structuredContent` 可以是任意 JSON 值。客户端因此仍处于「v1 拒绝、v2 接受」的分裂状态。`call` 会原样转发上游 `structuredContent`，测试中的值是数组，不能收成单一 object。

受影响的动作工具：`execute`、`plan`、`artifact`、`skill_tool`、`mcp_tool`、`move_out`、`browser`，以及单独拼接的 `operation_manage`。`edit` 已经是无顶层组合关键字的扁平 object，作为同一契约的现成形态。

`operation_batch` 用公开 input schema 做 `validateOperationSchemaValue`。该函数先按 `oneOf` 选分支，所以今天缺分支必填字段会在预检失败。handler 自身仍然校验动作参数，例如 `parseOperationTargets` 拒绝 `operation_id` 与 `operation_ids` 同时出现、同时缺失，以及批量模式下的非法 `action`。

今天谁在校验（grep + 上游 SDK 确认）：服务端对**直接** MCP 调用不做 schema 校验——`internal/server` 用 `mcp.Server.AddTool(&tool, untypedHandler)` 注册，官方 Go SDK v1.7.0 对 untyped `ToolHandler` 原样透传参数，只有 typed handler 才按 `inputSchema` 解码。公开 schema 的实际消费者是客户端（Claude Code、宿主、官方 TS SDK），加上 `operation_batch` 内嵌步骤这一处服务端预检。`validateOperationSchemaValue` 只有 `parseOperationTargets` 一个调用点；`oneOf`、`outputSchema` 只在 `tools_catalog.go`、`tools_clean_core.go`、`tools_operation.go`、`observability.go` 生成，`internal/desktop`、`cmd`、`internal/arc`、`internal/mcpresult` 均无引用。

摊平后根 `properties` 是唯一公开定义，同名字段只能留一份。按现有「排序后先到先得」实测（动作名 ASCII 排序，第一个出现的分支胜出）：`artifact.kind` 取 `list` 的定义（丢掉 `register` 的 `enum`）、`artifact.limit` 取 `list` 的「返回数量」（丢 `read` 的「字节数量」）、`plan.plan_id` 取 `advance`、`plan.evidence` 取 `block`、`plan.reason` 取 `block`、`plan.summary` 取 `create`、`browser.node_id` 取 `click`。其中只有 `artifact.kind` 会丢掉真正的约束（enum），其余只是描述措辞变化。

## Decisions

- 动作工具公布一个根对象：`type: object`、`additionalProperties: false`、`required` 只有 `action`。不公布顶层 `oneOf` / `anyOf` / `allOf`。
- 各 action 的字段继续合并到根 `properties`。除下面 `artifact` 两个字段显式收敛外，同名字段保持现有的排序后先到先得，不新增通用冲突合并器。
- 原分支说明和该分支的条件必填，写进根上 `action.description`。`remote_session_id` 继续从必填中去掉。没有其他条件必填时，只保留该 action 的说明。
- `operation_manage` 删除后补的 `oneOf`。根上同时保留 `operation_id` 与 `operation_ids`。互斥、批量只允许 `status` / `result`，继续只由 `parseOperationTargets` 执行。
- `mcp_tool` 不设置 `OutputSchema`。不补 `"type": "object"`，也不包装上游 `structuredContent`。其余工具继续使用 `arc.OutputSchema()`。
- 同名冲突字段：`artifact` 的 `kind`、`limit` 显式搬到 `artifactCommon`（`kind` 用 `register` 的 enum，描述写成「register：产物类型；list：按类型过滤」；`limit` 描述写成「list：返回数量；read：字节数量」），并从 `list`、`read`、`register` 分支删掉这两个字段。其余冲突字段（`plan.plan_id`、`plan.evidence`、`plan.reason`、`plan.summary`、`browser.node_id`）保持排序后先到先得，不新增通用合并器。
- `action.description` 的分支说明与条件必填字段名，直接从 `actionSchemaBranch.Description`、`actionSchemaBranch.Required`（去掉 `action`、`remote_session_id`）生成，不新增第二处手写列表。
- `addTool` 仍无条件赋值 `OutputSchema`；改动落在 `outputSchemaForTool("mcp_tool")` 返回 nil，靠 `outputSchema,omitempty` 省略该字段，同时删除 `addTool` 上方只为 `mcp_tool` 写的注释。
- 公开 schema 变化会让 `runtime_read` 的 `tool_schema_revision` 改变，属预期，不做兼容。
- `validateOperationSchemaValue` 删除 `oneOf` 匹配。预检只认根对象的 `required`、`properties` 和 `additionalProperties: false`。
- 不保留旧 schema、旧 `$id`、敞开的根对象，或任何第二套入口。

## Rationale

Anthropic 拒绝的是根上的组合关键字，不是扁平字段本身。嵌进某个属性虽然能过 API，但会改变调用参数的形状。当前 handler 读的是扁平参数，把参数包一层是新契约，不是这次要修的兼容问题。

`outputSchema` 在 MCP 里可选。`mcp_tool` 的 `call` 没有单一 JSON 类型，声明 `type: object` 会和透传契约矛盾，并让按 schema 校验结果的客户端拒绝合法上游结果。不声明才是诚实的。

条件必填无法在不使用根上组合关键字的前提下继续由 JSON Schema 表达。执行期校验已经在 handler 里。schema 改成模型能读到的说明，而不是再留一套只给预检用的 `oneOf`。

上游 Claude Code 自己摊平根组合关键字时的语义与这份计划一致：属性合并进一个 object，`required` 降级成说明文本。服务端先做同样的摊平，是为了不再让「工具是否可用、分支约束是否还在」取决于客户端版本，也避免客户端每次请求重写工具描述。

根上的 `if`/`then` 没有被 Anthropic 禁止，理论上仍能表达条件必填；但那要求客户端和模型理解 2020-12 条件分支，而 `operation_manage` 的双模式互斥是多字段、多动作的组合，写成 `if`/`then` 只会比说明文本更晦涩。

`artifact.kind` 选择显式收敛而不是先到先得：摊平后根定义是模型唯一能看到的定义，让它由动作名排序隐式决定、并丢掉唯一一处 enum，等于把实现细节泄漏成公开契约。把两个字段放进 `artifactCommon` 是最小且显式的写法，不引入合并器。

## Alternatives Considered

- 把 `oneOf` 嵌到某个属性下。Anthropic 允许嵌套组合关键字，但调用必须改成包装对象。否决，因为现有 handler 和调用方都使用扁平参数。
- 给 `mcp_tool.outputSchema` 写 `type: object` 且 `additionalProperties: true`，并总是返回 `structuredContent`。否决，因为 `call` 会原样转发数组等非 object 结果。
- 保留隐藏的 `oneOf` 只给 `operation_batch` 预检用。否决。公开 schema 是唯一契约，不留第二套旧校验。
- 只依赖 Claude Code ≥ 2.1.195 的客户端摊平，服务端不改。否决。旧版本客户端会逐个跳过工具，`claude -p` 等拿不到远程配置的场景仍会 400；把可用性押在客户端版本上，问题会在别人的环境里复现。
- 用根 `if`/`then` 保留条件必填。否决，理由见 Rationale。
- 写一个通用冲突字段合并器（例如取「更严」的定义、合并 enum 与描述）。否决。7 个工具里只有 `artifact.kind` 存在真实约束差异，通用器会引入第二套 schema 语义推断；在两个工具上显式收敛即可。

## Scope

### In

- `cleanActionTool` 生成的 input schema。
- `operation_manage` 注册时后补的 `oneOf`。
- 只为 `oneOf` 分支服务的 activity 占位和 description 去重。
- `mcp_tool` 的 `outputSchema`，以及 `addTool` 里为该特例写的注释。
- `validateOperationSchemaValue` 对 `oneOf` 的匹配。
- `artifactCommon` 对 `kind`、`limit` 的显式声明。
- 上述契约的测试（含 `public_catalog_test` 的 `outputSchema` 非空断言与 schema 字节预算断言）、分支读取 helper，以及 README 对 `outputSchema` 的现行说明。
- `docs/specs/2026-08-07-tool-catalog-converge-design.md` 顶部加一行标注：该公开 schema 契约已被本计划取代。

### Out

- 各工具 handler 的参数解析、错误码和执行行为。
- `mcp_tool` `call` 的透传结果，包括非 object 的 `structuredContent`。
- 其余工具的 ARC `outputSchema`。
- `edit` 条目内部已有的 `allOf`。它不在工具根上。
- `docs/plans/` 里已经写完的历史设计和实施记录，以及 `docs/specs/` 除上面那一行标注之外的正文（它们记录当时的决策，只补指向本计划的取代说明）。
- `internal/server/tools_ext.go` 里 skill/mcp arguments 的 `additionalProperties: true` 校验链路。
- 把动作拆成多个工具，或把参数嵌进新属性。

## Design

动作工具的 input schema 形态：

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["action"],
  "properties": {
    "action": {
      "type": "string",
      "enum": ["prepare", "submit"],
      "description": "prepare：……必填 purpose、targets。submit：……必填 confirmation_uuid。"
    }
  }
}
```

`action.description` 按 action 名排序。每一段包含动作名、原来的分支说明，以及去掉 `action` 和 `remote_session_id` 之后的必填字段名。分支说明为空时，沿用现在的兜底句：「仅执行「该动作」操作；失败时按返回的 next_action 继续。」测试只断言动作名和条件必填字段名出现在说明里，不锁定整句标点。

`operation_manage` 的根 `action.enum` 仍是 `status`、`wait`、`result`、`cancel`、`resume`。说明写明：单操作必填 `operation_id`；批量查询必填 `operation_ids`，且只支持 `status` 和 `result`；二者互斥。不要把根 enum 收成只有 `status` / `result`。

同名字段（摊平后根上是唯一公开定义）：

| 工具 | 字段 | 处理 |
| --- | --- | --- |
| `artifact` | `kind` | 搬到 `artifactCommon`：保留 `register` 的 enum，描述同时写清 list 的过滤语义 |
| `artifact` | `limit` | 搬到 `artifactCommon`：合并描述（list 返回数量 / read 字节数量） |
| `plan` | `plan_id` | 先到先得，取 `advance` 的定义 |
| `plan` | `evidence` | 先到先得，取 `block`（两组 items 完全相同） |
| `plan` | `reason` | 先到先得，取 `block` |
| `plan` | `summary` | 先到先得，取 `create` |
| `browser` | `node_id` | 先到先得，取 `click` |

`artifact.kind` 带 enum 后，`list` 的过滤值也被 enum 约束。`artifact.List` 按等值匹配已登记的 kind，而登记时的 kind 只能来自同一 enum，所以只有「查一个不可能存在的 kind」会从「客户端放行 + 返回空」变成「客户端拒绝」，不改变任何能返回结果的调用。

`operation_batch` 预检语义（`validateOperationSchemaValue` 去掉分支后）：

- 放宽一：只带 `action`、缺原分支必填字段的参数现在能通过预检（执行时 handler 仍拒绝）。
- 放宽二：属于另一个 action、但出现在根 `properties` 里的字段，不再被预检拒绝；是否报错由各 handler 决定，不新增交叉字段校验器。
- 不放宽：未知字段仍被 `additionalProperties: false` 拒绝；`isOperationInjectedField`（`session_id`、`remote_session_id`、`goal`、`purpose`、`intent`、`progress_summary`、`execution_mode`）的放行行为不变。

`mcp_tool` 省略 `outputSchema` 的机制：`addTool` 无条件执行 `tool.OutputSchema = outputSchemaForTool(tool.Name)`，所以改动是让 `outputSchemaForTool` 对 `mcp_tool` 返回 nil，字段由 `json:"outputSchema,omitempty"` 省略；`mcpx.mcp_tool_result.v1` 与配套注释一并删除。`mcp_tool` 不在 `publishedLimits()` 里，因此不损失 `x-mcpx-limits`。

schema 字节预算：`public_catalog_test` 给公开 schema 总量设了硬上限（input ≤ 70 000、output ≤ 18 000、total ≤ 90 000）。实测当前为 input 66 458 / output 16 444 / total 82 902。摊平会去掉分支重复使 input 下降，`mcp_tool` 去掉 output schema 再减 175 字节；改动后需重新记录日志值并确认仍在阈值内。

`withEmbeddedActivitySchema` 只把可选 `activity` 放在根 `properties`。删除向 `oneOf` 分支写入 `{type: object}` 占位，以及 `deduplicateBranchDescriptions` 与其辅助函数 `removeMatchingDescriptions`。没有引用后，这两段代码不保留。

`validateOperationSchemaValue` 遇到 schema 时直接按 `type` / `properties` / `required` / `additionalProperties` 检查，不再尝试分支（放宽项见上面「`operation_batch` 预检语义」）。

## Risks / 已知取舍

- 条件必填不再由公开 schema 表达：客户端与宿主不再能机器校验「该 action 必填哪些字段」，只剩 `action.description` 文本。服务端行为不受影响（今天对直接调用本来就不做 schema 校验），但 `suggested_action_contract_test` 的断言强度下降：它原本从分支 `required` 读出必填字段、再检查服务端生成的 next_action 是否带齐，之后只能断言「字段名出现在说明文本里」。保留的行为保护是各 handler 的错误路径测试（缺必填字段仍返回结构化错误）。
- 同名冲突字段的根定义由实现顺序或显式声明决定，模型看到的是取舍后的措辞；除 `artifact.kind` 外没有约束损失。
- 跨 action 多余字段不再被客户端拒绝（例如 `plan(action=create)` 带 `plan_id`）。选择不新增交叉校验器；若后续发现误用造成实际问题，再在对应 handler 加显式拒绝。
- 公开 schema 变化会让 `tool_schema_revision` 变化，已连接的客户端需要重新 `tools/list`；与仓库「不考虑旧版本兼容」一致。
- 上游仍在变动：官方 Go SDK 当前不校验根组合关键字，TS SDK v1/v2 对 `outputSchema` 的要求不同。本计划只保证「不依赖客户端版本」这一条。

## Acceptance Criteria

- 每个公开工具的 input schema 根上没有 `oneOf`、`anyOf`、`allOf`，并且是 `type: object`、`additionalProperties: false`。
- `execute`、`plan`、`artifact`、`skill_tool`、`mcp_tool`、`move_out`、`browser`、`operation_manage` 的 `action.description` 含有每个动作名，以及该动作去掉 `action` 与 `remote_session_id` 后的必填字段名。
- 这些工具的调用参数仍是扁平对象。`move_out` 根上仍有 `prepare` 用的 `purpose`、`targets` 和 `submit` 用的 `confirmation_uuid`；`targets` 条目仍要求 `path`，并保留 `expected_sha256`，不暴露 `kind` 或 `workspace`。
- `operation_manage` 根上同时有 `operation_id` 和 `operation_ids`，根 `required` 不包含 `operation_id`。
- `mcp_tool` 没有 `outputSchema`。其他公开工具仍公布 `$id` 为 `mcpx.structured_content.v2.0` 的 output schema。
- `mcp_tool` `call` 仍原样转发上游 `structuredContent`，包括数组。
- `operation_manage` 的互斥和批量动作限制仍由现有 handler 返回错误。
- README 写明除 `mcp_tool` 外公布 `outputSchema`，并说明 `mcp_tool` 不公布的原因。
- `artifact` 根上的 `kind` 保留 `register` 的 enum，描述同时覆盖 register 与 list；`limit` 描述覆盖 list 与 read。
- 公开 schema 字节总量仍在 `public_catalog_test` 的阈值内（记录改动后的 input/output/total 实测值）。
- `mcpx.mcp_tool_result.v1` 在实现、测试、README 中不再出现。
- `docs/specs/2026-08-07-tool-catalog-converge-design.md` 顶部标注该公开 schema 契约已被本计划取代。
- `runtime_read` 的 `tool_schema_revision` 随 schema 变化而变化，属预期。

## Test Strategy

先改断言，确认当前实现失败，再改生成逻辑。失败原因必须是旧 schema 仍含顶层 `oneOf` 或 `mcp_tool` 仍带无 `type` 的 `outputSchema`，而不是断言写错。

受影响测试文件与改法（实测：`oneOf` 只在这 6 个文件里被引用）：

| 文件 | 改法 |
| --- | --- |
| `public_catalog_test.go` | `oneOf` 例外分支（「有 `oneOf` 就可以不设 `additionalProperties`」那段）改为断言无组合关键字且 `additionalProperties: false`；`mcp_tool` 的 `OutputSchema == nil` 从失败改为通过、其余工具仍要求 ARC output schema；`TestActionSchemasExposeBranchPropertiesAtRoot`、`TestOneOfBranchDescriptionsAreNotRepeatedFromRoot` 删除或改写为根 `properties` + `action.description` 断言；`operation_manage` 的分支断言改为根 `properties`/`action.description`；字节预算断言重新核对（阈值不变）。 |
| `intent_test.go` | `actionBranch` 的 5 处条件必填断言（`skill_tool`/`mcp_tool` 的 `call` 必填 `purpose`，`execute(run)`、`move_out(prepare)` 必填 `purpose`，`move_out(submit)` 不要求 `purpose`）改为断言 `action.description` 文本；`assertNoSchemaFields` 删除 `oneOf` 递归；helper `actionBranch` 删除。 |
| `agent_guidance_test.go` | 遍历分支说明的循环改为：无顶层组合关键字；有 `action.enum` 的工具 `action.description` 非空。`edit` 无顶层 `oneOf` 的断言保留（现在对所有工具都成立）。 |
| `suggested_action_contract_test.go` | helper `schemaActionBranch` 删除，必填判断改为根 `required` + 说明文本中的条件必填字段名。断言强度下降记录在 Risks。 |
| `move_out_ergonomics_test.go` | 用 `schemaActionBranch(schema, "prepare")` 读分支 properties 的一处改为读根 `properties`；`targets` items 的断言不变。 |
| `acceptance_protocol_test.go` | `mcp_tool` 分支改为断言没有 `outputSchema`；`move_out` 的 2 分支断言改为根 `properties`/`action.description`；保留「schema 不含 `kind`」等既有断言。 |

`http_gateway_test.go`、`mcp_passthrough_test.go` 的 `OutputSchema` 用法与 fixture 不受影响，应保持绿色。

Happy path：

- 公开目录测试断言没有顶层组合关键字，且 `additionalProperties` 为 `false`。删掉「有 `oneOf` 就可以不设 `additionalProperties`」的例外。
- `move_out`、`operation_manage` 改为断言根 `properties` 和 `action.description`，不再读取分支。
- `acceptance_protocol_test` 断言 `mcp_tool` 没有 `outputSchema`，其他工具仍是 ARC output schema。

Edge cases：

- `intent_test`、`move_out_ergonomics_test`、`suggested_action_contract_test` 不再用分支 `required`。条件必填只检查说明文本。`edit`、`operation_batch`、`screenshot_capture`、`secret_provide` 这些根上本来就要求 `purpose` 的工具，断言保持不变。
- `agent_guidance_test` 里遍历分支说明的测试，改为断言没有顶层组合关键字；有 `action.enum` 的工具，其 `action.description` 非空。`edit` 不得有顶层 `oneOf` 的断言保留。
- 同名字段除 `artifact` 的 `kind`、`limit` 显式收敛外，其余保持排序后先到先得；不新增通用冲突合并器。

Regression：

- `mcp_tool` 透传测试继续断言上游 `structuredContent` 不被改写。
- `http_gateway_test` 里那两个非 `mcp_tool` 工具仍公布 ARC output schema。
- 可选 `activity` 仍出现在支持它的工具根 `properties` 上。

Error cases：

- `validateOperationSchemaValue` 对动作工具：未知字段失败；只有 `action`、缺少原分支必填字段时成功。
- 已有的 `operation_manage` handler 测试继续覆盖二者同时出现、同时缺失，以及批量 `action` 不是 `status` / `result`。不把这些规则搬回 schema。

## Implementation Plan

1. [x] 按上表改公开 schema 断言，并确认 `go test ./internal/server -count=1` 失败；失败原因必须是顶层 `oneOf`、`mcp_tool` output schema、分支 helper，而不是断言写错。
2. [x] 摊平 `cleanActionTool`：根 `type: object` + `additionalProperties: false` + `required: ["action"]`，分支字段合并进根 `properties`，分支说明与条件必填写进 `action.description`；删除根 `oneOf`、敞开的根对象与解释该敞开行为的注释。
3. [x] `artifactCommon` 显式声明 `kind`、`limit` 并从 `list`、`read`、`register` 分支移除；补「根 `kind` 保留 enum」的断言。
4. [x] 摊平 `operation_manage`：删除后补的 `oneOf` 与 `operationBranchProperties`，`action.description` 写明单操作/批量必填与互斥，`required` 只留 `action`。
5. [x] `withEmbeddedActivitySchema` 只保留根 `properties` 注入，删除分支 `{type: object}` 占位、`deduplicateBranchDescriptions`、`removeMatchingDescriptions`。
6. [x] `mcp_tool` 不再设置 `OutputSchema`：删除 `mcpx.mcp_tool_result.v1` 分支与 `addTool` 注释，让 output schema 断言通过；透传测试保持绿色。
7. [x] 删除 `validateOperationSchemaValue` 的 `oneOf` 匹配，并补上「缺条件必填可通过预检、未知字段被拒绝」的测试；handler 错误测试保持绿色。
8. [x] 更新 README 第 7 节对 `outputSchema` 的现行说明，并给 converge spec 加取代标注。

## Verification

- `gofmt -l ./cmd ./internal` 无输出。
- `go test ./internal/server -count=1` 通过。
- `go build ./...` 通过；若第 7 步的预检改动使 `internal/server` 以外的包无法编译，再跑 `go test ./... -count=1`。改动集中在 `internal/server` 与 README，`oneOf`/`outputSchema` 没有其他包消费（已 grep 确认），不把全仓库测试当成必跑项。
- 收口 grep 证据：实现代码里不再出现 `"oneOf"`；`allOf` 只剩 `edit` 条目内部那处（不在工具根上）；`mcpx.mcp_tool_result.v1` 无残留。
- `public_catalog_test` 的字节预算日志记录改动后的 input/output/total 实测值。
