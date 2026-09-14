package server

import (
	"encoding/json"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"

	"mcpx/internal/envelope"
	"mcpx/internal/environment"
	"mcpx/internal/operation"
	"mcpx/internal/server/prompts"
)

// registerTools is the sole public tool registration point.
func (r *Runtime) registerTools(s *mcp.Server) {
	r.registerCleanCoreTools(s)
	r.captureToolIndex(s)
}

func (r *Runtime) captureToolIndex(s *mcp.Server) {
	// Official go-sdk has no ListTools snapshot API; addTool fills toolIndex.
	_ = s
}

func (r *Runtime) registeredTools() []mcp.Tool {
	r.toolIndexMu.RLock()
	defer r.toolIndexMu.RUnlock()
	tools := make([]mcp.Tool, 0, len(r.toolIndex))
	for _, tool := range r.toolIndex {
		tools = append(tools, tool)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

// listedToolMap is a test/helper snapshot of the registered tool catalog.
func (r *Runtime) listedToolMap() map[string]mcp.Tool {
	r.toolIndexMu.RLock()
	defer r.toolIndexMu.RUnlock()
	out := make(map[string]mcp.Tool, len(r.toolIndex))
	for name, tool := range r.toolIndex {
		out[name] = tool
	}
	return out
}

// currentToolSchemaRevision is derived from the actual MCP registration,
// including name, description, input schema, and annotations. It deliberately
// excludes Session state so opening or handing off a session cannot refresh a
// client's tools/list cache.
func (r *Runtime) currentToolSchemaRevision() string {
	tools := r.registeredTools()
	items := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		encoded, _ := json.Marshal(tool)
		var item map[string]any
		_ = json.Unmarshal(encoded, &item)
		items = append(items, item)
	}
	return hashRevision(items)
}

// compactToolResult is the unified success-path tool result builder.
//
//	content[0].text   — human summary only
//	structuredContent — machine wire {status, data} (or an already-formed wire map)
//
// Models must consume structuredContent after ARC wrap; hosts render the text.
func compactToolResult(data any, summary string) *mcp.CallToolResult {
	if summary == "" {
		summary = "succeeded"
	}
	var wire map[string]any
	if existing, ok := asPublicWireEnvelope(data); ok {
		wire = existing
	} else {
		wire = map[string]any{
			"status": string(envelope.StatusOK),
			"data":   data,
		}
	}
	// JSON-normalize so nested slices are []any (stable for tests and hosts).
	return mcpresult.NewStructured(jsonNormalizeMap(wire), summary)
}

func jsonNormalizeMap(value map[string]any) map[string]any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return value
	}
	return normalized
}

// asPublicWireEnvelope detects handler payloads already in the public wire shape
// {status, data?, error?} so success helpers do not double-wrap them.
func asPublicWireEnvelope(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	status, _ := m["status"].(string)
	switch status {
	case string(envelope.StatusOK), string(envelope.StatusAccepted),
		string(envelope.StatusNeedConfirmation), string(envelope.StatusInterrupted),
		string(envelope.StatusError):
		if _, hasData := m["data"]; hasData {
			return m, true
		}
		if _, hasError := m["error"]; hasError {
			return m, true
		}
	}
	return nil, false
}

type toolAnnotation struct {
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	OpenWorld   bool
	Title       string
	Meta        mcp.Meta
}

var (
	readOnlyToolAnnotation = toolAnnotation{ReadOnly: true, Destructive: false, Idempotent: true, OpenWorld: false}
	mutatingToolAnnotation = toolAnnotation{ReadOnly: false, Destructive: true, Idempotent: false, OpenWorld: true}
	sessionToolAnnotation  = toolAnnotation{ReadOnly: false, Destructive: false, Idempotent: false, OpenWorld: false}
	secretToolAnnotation   = toolAnnotation{ReadOnly: false, Destructive: false, Idempotent: false, OpenWorld: false}
	// commandExecutionToolAnnotation: whether a command is destructive is
	// decided by the server-side command policy, not by the tool itself, so
	// hosts must not gate the call on the destructive hint.
	commandExecutionToolAnnotation = toolAnnotation{ReadOnly: false, Destructive: false, Idempotent: false, OpenWorld: true}
)

func annotatedTool(tool mcp.Tool, annotation toolAnnotation) mcp.Tool {
	dest, open := annotation.Destructive, annotation.OpenWorld
	tool.Annotations = &mcp.ToolAnnotations{
		ReadOnlyHint:    annotation.ReadOnly,
		DestructiveHint: &dest,
		IdempotentHint:  annotation.Idempotent,
		OpenWorldHint:   &open,
		Title:           annotation.Title,
	}
	if annotation.Title != "" {
		tool.Title = annotation.Title
	}
	if annotation.Meta != nil {
		tool.Meta = annotation.Meta
	}
	return tool
}

type actionSchemaBranch struct {
	Description string
	Properties  map[string]any
	Required    []string
}

// cleanActionTool is the strict action schema used by the final core catalog.
// Every branch repeats common properties so remote models can validate the
// selected action without relying on permissive root-level fallbacks.
func cleanActionTool(name, description string, common map[string]any, branches map[string]actionSchemaBranch, annotation toolAnnotation) mcp.Tool {
	actions := make([]string, 0, len(branches))
	for action := range branches {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	rootProperties := map[string]any{"action": map[string]any{"type": "string", "enum": actions}}
	for key, value := range common {
		rootProperties[key] = value
	}
	oneOf := make([]map[string]any, 0, len(actions))
	for _, action := range actions {
		branch := branches[action]
		if branch.Description == "" {
			branch.Description = "仅执行「" + action + "」操作；失败时按返回的 next_action 继续。"
		}
		// JSON Schema evaluates additionalProperties against the object schema
		// where it is declared. Keep every branch field in the root property set
		// as well as in the selected branch; otherwise strict client validators
		// reject valid branch arguments before oneOf is evaluated.
		for key, value := range branch.Properties {
			if _, exists := rootProperties[key]; !exists {
				rootProperties[key] = value
			}
		}
		properties := map[string]any{"action": map[string]any{"const": action}}
		for key, value := range common {
			properties[key] = value
		}
		for key, value := range branch.Properties {
			properties[key] = value
		}
		required := append([]string{}, branch.Required...)
		required = append([]string{"action"}, required...)
		oneOf = append(oneOf, map[string]any{
			"type":                 "object",
			"description":          branch.Description,
			"properties":           properties,
			"required":             required,
			"additionalProperties": false,
		})
	}
	// Keep the root object open for clients that validate object properties
	// before evaluating oneOf. Each selected branch remains strict and carries
	// the common fields plus its own fields, so the action contract is still
	// enforced by validators that implement oneOf correctly. An open root is
	// required for connectors that flatten or pre-validate discriminated unions.
	raw, _ := json.Marshal(map[string]any{
		"type": "object", "description": description, "properties": rootProperties,
		"required": []string{"action"}, "oneOf": oneOf,
	})
	return annotatedTool(mcp.Tool{Name: name, Description: description, InputSchema: json.RawMessage(raw)}, annotation)
}

func activityInputSchema() map[string]any {
	properties := map[string]any{
		"intent":     stringSchema("当前工作 turn 的目标或要解决的问题；仅在开始新的实质工作 turn 时填写，非空 intent 会开启新 turn，不要用它描述单个工具动作"),
		"hypothesis": stringSchema("尚未被证据确认、可被后续读取或验证推翻的暂定判断；只在假设新建或发生实质变化时填写，不要把已观察事实写成 hypothesis"),
		"evidence":   stringSchema("刚刚获得的可核验事实、代码现状、命令结果或其他直接观察；只写事实，不写由事实推导出的判断，并避免重复上一条 evidence"),
		"conclusion": stringSchema("由已有 evidence 支持的当前稳定判断或问题结论；它是推断结果而不是原始事实，也不是下一步动作"),
		"next":       stringSchema("基于当前理解选择的立即下一动作；应与本次正在发起的 tool call 对齐，例如“读取自动取消任务”，不要描述更远期计划"),
		"status":     stringSchema("无法归入前五类但值得公开的当前阶段、等待或阻塞状态；仅在状态发生实质变化时填写，不作为工具 heartbeat，也不替代 progress 的业务里程碑"),
	}
	return map[string]any{
		"type":                 "object",
		"description":          "可选公开 Activity。只填写本次发生实质变化的语义字段，不重复未变化内容；可一次提供多个字段。Runtime 自动生成 turn_id、sequence、state 和 related_call_id，并按 intent、hypothesis、evidence、conclusion、next、status 顺序展开非空字段",
		"properties":           properties,
		"additionalProperties": false,
	}
}

func withEmbeddedActivitySchema(tool mcp.Tool) mcp.Tool {
	if !toolSupportsEmbeddedActivity(tool.Name) || tool.InputSchema == nil {
		return tool
	}
	encoded, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return tool
	}
	var schema map[string]any
	if json.Unmarshal(encoded, &schema) != nil || schema == nil {
		return tool
	}
	inject := func(properties map[string]any) {
		if properties != nil {
			properties["activity"] = activityInputSchema()
		}
	}
	rootProperties, _ := schema["properties"].(map[string]any)
	inject(rootProperties)
	if branches, ok := schema["oneOf"].([]any); ok {
		for _, raw := range branches {
			branch, _ := raw.(map[string]any)
			properties, _ := branch["properties"].(map[string]any)
			inject(properties)
		}
	}
	raw, err := json.Marshal(schema)
	if err == nil {
		tool.InputSchema = json.RawMessage(raw)
	}
	return tool
}

func stringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func promptToolDescription(descriptions map[string]string, name, fallback string) string {
	if description := descriptions[name]; description != "" {
		return description
	}
	return fallback
}

func numberSchema(description string) map[string]any {
	return map[string]any{"type": "number", "description": description}
}

func booleanSchema(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func arraySchema(items map[string]any, description string) map[string]any {
	return map[string]any{"type": "array", "items": items, "description": description}
}

func enumSchema(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": description}
}

// registerConsolidatedToolsCatalog registers the clean-core support tools.
func (r *Runtime) registerConsolidatedToolsCatalog(s *mcp.Server) {
	toolDesc := prompts.MustDescriptions()
	remoteSession := stringSchema("持久化的 Remote Session 标识")
	workspace := stringSchema("已注册的 Workspace 名称")
	path := stringSchema("工作区相对路径")
	supportTool := cleanCoreTool

	operationSteps := arraySchema(map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"id":         stringSchema("批次内唯一的步骤 ID"),
			"tool":       stringSchema("已注册的公开工具名称"),
			"arguments":  map[string]any{"type": "object", "additionalProperties": true, "description": "目标工具的业务参数"},
			"depends_on": arraySchema(map[string]any{"type": "string"}, "前置步骤 ID"),
		},
		"required": []string{"id", "tool", "arguments"},
	}, "带依赖关系的公开工具操作")
	operationSteps["maxItems"] = operation.MaxSteps
	r.addTool(s, supportTool("operation_batch", toolDesc["operation_batch"], map[string]any{
		"run_id": stringSchema("调用方稳定运行 ID"), "operation_id": stringSchema("调用方稳定操作 ID；同 ID 同计划回读原操作，不重复执行"),
		"remote_session_id": remoteSession, "operations": operationSteps, "purpose": stringSchema("本次调用的目的；必须由用户明确提供"),
	}, []string{"remote_session_id", "purpose", "operations"}, mutatingToolAnnotation), r.toolOperationBatch)
	operationIDsSchema := arraySchema(map[string]any{"type": "string"}, "批量查询的异步操作 ID；最多 32 个，不要把 operation_manage 嵌套进 operation_batch")
	operationIDsSchema["minItems"] = 1
	operationIDsSchema["maxItems"] = operation.MaxBatchQueries
	operationManage := supportTool("operation_manage", toolDesc["operation_manage"], map[string]any{
		"remote_session_id": remoteSession,
		"operation_id":      stringSchema("单个异步操作 ID；与 operation_ids 二选一"), "operation_ids": operationIDsSchema,
		"action":  enumSchema("操作动作；operation_ids 批量模式只支持 status、result", "status", "wait", "result", "cancel", "resume"),
		"step_id": stringSchema("批量操作子步骤 ID"), "timeout_ms": numberSchema("wait 最长等待毫秒数"),
		"confirmation_token": stringSchema("仅表示用户已确认同一子操作，不是认证凭据"), "cursor": stringSchema("结果分页游标"), "limit": numberSchema("结果字节或列表数量限制"),
	}, []string{"remote_session_id", "action"}, sessionToolAnnotation)
	var operationManageSchema map[string]any
	_ = json.Unmarshal(mcpresult.ToolSchemaJSON(operationManage), &operationManageSchema)
	// 客户端可能直接把 oneOf 投影为参数联合类型，不合并外层 properties。
	// 每个分支都必须独立描述实际调用所需的完整身份和参数。
	operationBranchProperties := func(batch bool) map[string]any {
		properties := make(map[string]any)
		for key, value := range operationManageSchema["properties"].(map[string]any) {
			properties[key] = value
		}
		if batch {
			delete(properties, "operation_id")
			delete(properties, "step_id")
			delete(properties, "cursor")
			delete(properties, "confirmation_token")
			delete(properties, "timeout_ms")
			properties["action"] = enumSchema("批量查询动作", "status", "result")
		} else {
			delete(properties, "operation_ids")
		}
		return properties
	}
	operationManageSchema["oneOf"] = []any{
		map[string]any{
			"type":                 "object",
			"description":          "单操作模式；支持 status、wait、result、cancel、resume。",
			"properties":           operationBranchProperties(false),
			"required":             []string{"remote_session_id", "action", "operation_id"},
			"additionalProperties": false,
		},
		map[string]any{
			"type":                 "object",
			"description":          "批量查询模式；仅支持 status、result，直接传 operation_ids。",
			"properties":           operationBranchProperties(true),
			"required":             []string{"remote_session_id", "action", "operation_ids"},
			"additionalProperties": false,
		},
	}
	operationManageSchema["required"] = []string{"remote_session_id", "action"}
	operationManage.InputSchema = mustSchemaJSON(operationManageSchema)
	r.addTool(s, operationManage, r.toolOperationManage)

	r.addTool(s, supportTool("runtime_read", toolDesc["runtime_read"], map[string]any{
		"remote_session_id": remoteSession, "workspace": workspace, "view": enumSchema("读取视图；省略时默认 capabilities，anchor_path/paths 出现时推导 instructions", "capabilities", "project", "instructions"),
		"anchor_path": stringSchema("指令锚点路径；出现时可省略 view"), "paths": arraySchema(map[string]any{"type": "string"}, "指令路径；出现时可省略 view"),
	}, nil, readOnlyToolAnnotation), r.toolRuntimeRead)
	r.addTool(s, supportTool("environment_read", toolDesc["environment_read"], map[string]any{
		"remote_session_id": remoteSession, "workspace": workspace, "view": enumSchema("读取视图；省略时 snapshot_id 推导 compare，否则默认 current", "current", "compare"),
		"sections": arraySchema(map[string]any{"type": "string", "enum": environment.ValidSections}, "环境分区"), "snapshot_id": stringSchema("比较用快照 ID；出现时可省略 view"),
	}, nil, readOnlyToolAnnotation), r.toolEnvironmentRead)
	r.addTool(s, supportTool("environment", toolDesc["environment"], map[string]any{
		"remote_session_id": remoteSession,
		"sections":          arraySchema(map[string]any{"type": "string", "enum": environment.ValidSections}, "环境分区"),
	}, []string{"remote_session_id"}, sessionToolAnnotation), r.toolEnvironment)

	executeCommon := map[string]any{
		"remote_session_id": remoteSession, "purpose": stringSchema("本次执行的用户目标"),
		"idempotency_key": stringSchema("同一执行请求重试时复用的幂等键"),
		"scope":           enumSchema("执行范围", "workspace"), "yield_time_ms": numberSchema("等待时长"),
		"user_confirmed": booleanSchema("用户已确认同一命令或 runtime+script；服务端仍会校验待确认摘要与脚本 SHA"),
		"execution_mode": enumSchema("async 表示外层 Operation 异步调度；python/node runtime 使用 Task 生命周期，sqlite readonly query 直接返回结构化结果", "sync", "async"),
	}
	executeBranches := map[string]actionSchemaBranch{
		"run": {Description: "执行 argv+shell=false、command、项目 task，或一次性 runtime+script，四种模式互斥。argv 按元素直接传递；python/node 源码经 stdin+EOF 直接执行且不经过 shell，sqlite 对 Workspace 内现有数据库执行只读单查询并返回结构化行。", Properties: map[string]any{
			"command": stringSchema("Workspace 内待执行的简单命令"), "task": stringSchema("项目任务名称，与 command/runtime+script 互斥"),
			"argv":  arraySchema(stringSchema("单个原样传递的参数；第一个元素为可执行文件"), "直接执行的参数数组，与 command/task/runtime+script 互斥"),
			"shell": booleanSchema("argv 模式必须显式为 false；不通过 shell 或二次拆分"),
			"workspace_transition": map[string]any{"type": "object", "additionalProperties": false,
				"properties": map[string]any{"operation_id": stringSchema("稳定身份推进操作 ID"), "head": stringSchema("预先明确的新 Commit SHA"), "tree": stringSchema("该 Commit 的精确 tree SHA")},
				"required":   []string{"operation_id", "head", "tree"}, "description": "仅用于 git update-ref frozen-ref new-head old-head；执行成功且后置回读吻合才推进冻结身份"},
			"expected_workspace": map[string]any{
				"type": "object", "additionalProperties": false,
				"description": "冻结本次 Git 操作的物理目标与前置身份；路径须在注册 Workspace 内，启动进程前再次核对，实际 cwd 使用此路径",
				"properties": map[string]any{
					"canonical_path": stringSchema("精确物理 Git root"), "remote_name": stringSchema("目标 remote 名称"),
					"remote_sha256": stringSchema("fetch/push remote 精确配置摘要，不传凭据"), "ref": stringSchema("完整 ref 或 DETACHED"),
					"head": stringSchema("预期前置 HEAD"), "tree": stringSchema("预期前置 HEAD tree"), "run_id": stringSchema("稳定运行 ID"),
				}, "required": []string{"canonical_path", "remote_name", "remote_sha256", "ref", "head", "tree", "run_id"},
			},
			"runtime":  enumSchema("一次性临时运行时；sqlite 仅支持只读查询", "python", "node", "sqlite"),
			"script":   stringSchema("Python/Node 源码或 SQLite 单条只读查询；最大 65536 bytes。服务端只持久化 SHA/字节数，不把源码写入 Task/audit/observation。需要跨轮恢复时，首次调用前由调用方在获准位置保存非敏感脚本及完整原始请求并独立回读校验；确认时保留原始参数与 idempotency_key，仅设置 user_confirmed=true。原文缺失或校验失败时停止"),
			"database": stringSchema("仅 sqlite runtime 使用；Workspace 内现有 SQLite 数据库的相对路径"),
		}, Required: []string{"remote_session_id", "purpose"}},
		"attach": {Description: "等待并读取已有执行 Task 的输出；延续既有 Task，不需要客户端重复 purpose。", Properties: map[string]any{
			"execution_task_id": stringSchema("服务端返回的执行 Task ID"), "stdout_offset": numberSchema("stdout 字节偏移"),
			"stderr_offset": numberSchema("stderr 字节偏移"),
		}, Required: []string{"remote_session_id", "execution_task_id"}},
		"stop": {Description: "停止属于当前 Remote Session 的执行 Task。", Properties: map[string]any{
			"execution_task_id": stringSchema("服务端返回的执行 Task ID"),
		}, Required: []string{"remote_session_id", "purpose", "execution_task_id"}},
		"stdin": {Description: "向交互式执行 Task 写入 stdin。", Properties: map[string]any{
			"execution_task_id": stringSchema("服务端返回的执行 Task ID"), "input": stringSchema("写入 stdin 的文本"),
		}, Required: []string{"remote_session_id", "purpose", "execution_task_id", "input"}},
	}
	r.addTool(s, cleanActionTool("execute", toolDesc["execute"], executeCommon, executeBranches, commandExecutionToolAnnotation), r.toolExecute)

	planCommon := map[string]any{
		"remote_session_id": remoteSession, "purpose": stringSchema("本次计划操作的用户目标"),
		"idempotency_key": stringSchema("同一计划写操作重试时复用的幂等键"), "execution_mode": enumSchema("执行模式", "sync", "async"),
	}
	planBranches := map[string]actionSchemaBranch{
		"create":   {Properties: map[string]any{"summary": stringSchema("计划摘要"), "tasks": arraySchema(planTaskInputSchema(), "有序计划任务")}, Required: []string{"remote_session_id", "purpose", "tasks"}},
		"read":     {Properties: map[string]any{"plan_id": stringSchema("服务端返回的 Plan ID")}, Required: []string{"remote_session_id", "plan_id"}},
		"advance":  {Properties: map[string]any{"plan_id": stringSchema("Plan ID"), "plan_task_id": stringSchema("服务端返回的正式 Plan Task ID")}, Required: []string{"remote_session_id", "purpose", "plan_id", "plan_task_id"}},
		"complete": {Properties: map[string]any{"plan_id": stringSchema("Plan ID"), "plan_task_id": stringSchema("服务端返回的正式 Plan Task ID"), "evidence": arraySchema(planEvidenceSchema(), "完成任务所需证据")}, Required: []string{"remote_session_id", "purpose", "plan_id", "plan_task_id", "evidence"}},
		"block":    {Properties: map[string]any{"plan_id": stringSchema("Plan ID"), "plan_task_id": stringSchema("服务端返回的正式 Plan Task ID"), "reason": stringSchema("阻塞原因"), "evidence": arraySchema(planEvidenceSchema(), "已获得证据")}, Required: []string{"remote_session_id", "purpose", "plan_id", "plan_task_id", "reason"}},
		"replan":   {Properties: map[string]any{"plan_id": stringSchema("Plan ID"), "summary": stringSchema("新的计划摘要"), "reason": stringSchema("重新规划原因"), "operations": arraySchema(planOperationSchema(), "新增、更新或移除任务")}, Required: []string{"remote_session_id", "purpose", "plan_id", "reason", "operations"}},
		"deliver":  {Properties: map[string]any{"plan_id": stringSchema("Plan ID")}, Required: []string{"remote_session_id", "purpose", "plan_id"}},
	}
	r.addTool(s, cleanActionTool("plan", toolDesc["plan"], planCommon, planBranches, planToolAnnotation), r.toolPlanClean)

	artifactCommon := map[string]any{"remote_session_id": remoteSession, "purpose": stringSchema("本次产物操作的用户目标"), "idempotency_key": stringSchema("同一登记操作重试时复用的幂等键"), "execution_mode": enumSchema("执行模式", "sync", "async")}
	artifactBranches := map[string]actionSchemaBranch{
		"register": {Properties: map[string]any{"path": path, "name": stringSchema("显示名称"), "kind": enumSchema("产物类型", "test_report", "coverage", "build", "screenshot", "log", "other"), "mime_type": stringSchema("MIME 类型")}, Required: []string{"remote_session_id", "purpose", "path"}},
		"list":     {Properties: map[string]any{"kind": stringSchema("按产物类型过滤"), "limit": numberSchema("返回数量")}, Required: []string{"remote_session_id"}},
		"read":     {Properties: map[string]any{"artifact_id": stringSchema("服务端返回的 Artifact ID"), "offset": numberSchema("字节偏移"), "limit": numberSchema("字节数量")}, Required: []string{"remote_session_id", "artifact_id"}},
	}
	r.addTool(s, cleanActionTool("artifact", toolDesc["artifact"], artifactCommon, artifactBranches, artifactToolAnnotation), r.toolArtifactClean)

	skillCommon := map[string]any{
		"remote_session_id": remoteSession,
		"query":             stringSchema("按关键词筛选 Skill"),
		"name":              stringSchema("Skill 名称"),
		"purpose":           stringSchema("调用 Skill 的用户目标"),
		"arguments":         map[string]any{"type": "object", "additionalProperties": true},
		"idempotency_key":   stringSchema("同一调用重试时复用的幂等键"),
		"execution_mode":    enumSchema("执行模式", "sync", "async"),
	}
	skillBranches := map[string]actionSchemaBranch{
		"list":     {Description: "列出或搜索当前 Session 可用 Skill。", Required: []string{"remote_session_id"}},
		"describe": {Description: "读取某个 Skill 的详细能力、instructions 与参数 schema。", Required: []string{"remote_session_id", "name"}},
		"call":     {Description: "调用 Skill；Runtime 负责 revision 与参数校验。", Required: []string{"remote_session_id", "purpose", "name"}},
	}
	r.addTool(s, cleanActionTool("skill_tool", toolDesc["skill_tool"], skillCommon, skillBranches, skillToolAnnotation), r.toolSkillTool)

	mcpCommon := map[string]any{
		"remote_session_id": remoteSession,
		"query":             stringSchema("按关键词筛选 MCP Server"),
		"server":            stringSchema("MCP Server 名称"),
		"tool":              stringSchema("上游 MCP Tool 名称"),
		"purpose":           stringSchema("调用上游 MCP 的用户目标"),
		"arguments":         map[string]any{"type": "object", "additionalProperties": true},
		"idempotency_key":   stringSchema("同一调用重试时复用的幂等键"),
		"execution_mode":    enumSchema("执行模式", "sync", "async"),
	}
	mcpBranches := map[string]actionSchemaBranch{
		"list":     {Description: "列出 MCP Servers；提供 server 时列出该 Server 的 Tools。", Required: []string{"remote_session_id"}},
		"describe": {Description: "读取某个 MCP Tool 的完整 input schema。", Required: []string{"remote_session_id", "server", "tool"}},
		"call":     {Description: "调用上游 MCP Tool；Runtime 负责当前 schema 与参数校验。", Required: []string{"remote_session_id", "purpose", "server", "tool"}},
	}
	r.addTool(s, cleanActionTool("mcp_tool", toolDesc["mcp_tool"], mcpCommon, mcpBranches, mcpToolAnnotation), r.toolMCPTool)

	r.addTool(s, supportTool("screenshot_capture", toolDesc["screenshot_capture"], map[string]any{
		"remote_session_id": remoteSession, "purpose": stringSchema("截取屏幕的用户目标和范围"),
		"mode": stringSchema("全屏或区域"), "display": numberSchema("显示器索引"),
		"x": numberSchema("区域 X"), "y": numberSchema("区域 Y"), "width": numberSchema("宽度"), "height": numberSchema("高度"),
		"compression": stringSchema("压缩模式"), "format": stringSchema("png 或 jpeg"), "quality": numberSchema("JPEG 质量"),
		"max_width": numberSchema("输出宽度上限"), "max_height": numberSchema("输出高度上限"),
	}, []string{"remote_session_id", "purpose"}, readOnlyToolAnnotation), r.toolScreenshotCapture)
	r.addTool(s, supportTool("secret_provide", toolDesc["secret_provide"], map[string]any{
		"remote_session_id": remoteSession, "purpose": stringSchema("向当前会话提供 Secret 的用户目标"), "secret_id": stringSchema("Secret ID"),
		"values": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Secret 名称和值"},
	}, []string{"remote_session_id", "purpose"}, secretToolAnnotation), r.toolSecretsProvide)

	r.registerResources(s)
}

func (r *Runtime) registerResources(s *mcp.Server) {
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "mcpx://remote-sessions/{remote_session_id}/artifacts/{artifact_id}",
		Name:        "Remote Session 产物",
		Description: "读取已注册的 MCPX 开发产物",
	}, r.resourceArtifact)
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "mcpx://remote-sessions/{remote_session_id}/tasks/{execution_task_id}/logs",
		Name:        "终端 Task 日志",
		Description: "读取 MCPX 终端 Task 的完整日志",
		MIMEType:    "text/plain",
	}, r.resourceTaskLogs)
}

func mustSchemaJSON(schema map[string]any) json.RawMessage {
	raw, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return raw
}
