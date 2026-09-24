# Screenshot Artifact Fallback

## Goal

让 `screenshot_capture` 在继续返回 MCP `ImageContent` 的同时，自动生成一个可持久读取的 screenshot artifact，并在 structuredContent 中返回 `artifact_id` 与 `resource_uri`。这样即使上游 Host/Connector 丢失多模态 `ImageContent`，模型仍可通过 artifact API/resource 取回截图。

## Context

当前截图链路先写系统临时文件，编码为 PNG/JPEG 后把字节放入 `screenshot.Result.Data`，随后删除临时文件。`toolScreenshotCapture` 把这些字节追加为 MCP `ImageContent`，但 structuredContent 只包含截图 metadata。当前 ChatGPT connector 实际只暴露 structuredContent，因此图片在 connector 不透传 `ImageContent` 时不可恢复。

现有 artifact 服务只注册 Workspace 内已有文件，并在读取时重新访问 Workspace 路径；截图字节不是 Workspace 文件，不能直接复用现有 `Register`。

## Decisions

- 保留现有 MCP `ImageContent` 返回，不把完整 base64 复制进 structuredContent。
- 为 artifact 服务增加“托管字节”注册能力，用于 Runtime 自己产生的二进制产物。
- 托管字节存入 SQLite 的独立 artifact blob 表，以 artifact ID 为外键；普通 Workspace artifact 的现有路径语义保持不变。
- screenshot artifact 的公开 `Artifact.Path` 为空，不伪造 Workspace 路径；读取时由 artifact 服务透明识别托管字节。
- `screenshot_capture` 成功契约包含 artifact 持久化：只有截图和 artifact 都成功时才返回成功。
- structuredContent 在现有截图 metadata 基础上增加顶层 `artifact_id` 和 `resource_uri`。
- 自动注册的 artifact 使用 `kind=screenshot`，MIME 与截图输出一致，并记录 `artifact.registered` Remote Session event。

## Rationale

- 双通道可以同时服务支持多模态 MCP 的 Host 和只保留 structuredContent 的 Host。
- 不在 structuredContent 中复制 base64，避免将最多 8 MiB 的图片膨胀约 4/3 后再次塞入模型上下文。
- 不写入 Workspace，避免一个标记为 read-only 的截图工具污染项目文件、触发 Git/FileWatch 变更或受 Workspace 文件策略影响。
- 独立 blob 表比把二进制塞入现有 artifacts 行更清晰：现有 Workspace artifact schema/读取路径保持稳定，托管内容可通过外键随 artifact/Session 级联清理。

## Alternatives Considered

### 在 Workspace 下创建隐藏截图文件

未采用。它会让截图工具产生项目文件副作用，与现有 read-only 工具语义冲突，也可能污染 Git 状态。

### 在 structuredContent 中直接返回 base64

未采用。会重复 `ImageContent` 数据并显著放大上下文。

### 在 `~/.mcpx/artifacts` 下增加托管文件目录

可行，但需要引入额外文件生命周期、清理与路径管理。本次截图已受 8 MiB 上限约束，使用现有 state SQLite + 外键级联能以更小改动提供持久 fallback；若后续出现大量/大体积 artifact，再单独迁移为文件存储。

## Scope

### In

- 为 state schema 增加 artifact blob 持久化表。
- 为 artifact service 增加从 `[]byte` 注册托管 artifact 的能力。
- 让 `Get/List/Read/ReadAll` 对 Workspace artifact 与托管 artifact 保持统一公开契约。
- `screenshot_capture` 自动注册 screenshot artifact，并返回 `artifact_id/resource_uri`。
- 保留当前 `ImageContent`、截图 metadata、审计字段。
- 增加针对 artifact blob 和 screenshot result 的回归测试。

### Out

- 不改变普通 `artifact(action=register)` 的 Workspace 文件注册语义。
- 不新增 screenshot 的用户指定输出路径。
- 不把 base64 放入 screenshot structuredContent。
- 不改 browser 工具自己的 screenshot contract。
- 不增加 artifact TTL/配额策略；沿用 Remote Session / SQLite 现有生命周期，本次只保证可恢复性。

## Design

新增一个与 `artifacts` 一对一关联的托管内容表，概念结构：

```sql
CREATE TABLE artifact_blobs (
    artifact_id TEXT PRIMARY KEY,
    content BLOB NOT NULL,
    FOREIGN KEY (artifact_id) REFERENCES artifacts(id) ON DELETE CASCADE
);
```

artifact service 新增类似 `RegisterBytes(ctx, remoteSessionID, principalID, data, name, kind, mimeType)` 的内部 API：

1. 校验/补全 name、kind、MIME。
2. 对字节计算 size、SHA-256、source encoding。
3. 在同一数据库事务中写入 `artifacts` metadata 与 `artifact_blobs` content。
4. 返回标准 `Artifact`，其中 `Path=""`，`ResourceURI` 与普通 artifact 一致。

读取流程先取得 artifact metadata，再查询是否存在托管 blob：

- 有 blob：直接基于 blob 执行窗口切片、编码检测、base64 delivery、SHA 校验。
- 无 blob：继续现有 Workspace 路径读取逻辑。

`toolScreenshotCapture` 在 `Capture` 成功后立即注册字节 artifact；注册成功后构造结构化数据：

```json
{
  "mode": "fullscreen",
  "mime_type": "image/png",
  "bytes": 420962,
  "sha256": "sha256:...",
  "artifact_id": "art_...",
  "resource_uri": "mcpx://remote-sessions/rs_.../artifacts/art_..."
}
```

然后继续追加现有 `ImageContent`。artifact 注册失败时返回 screenshot artifact 错误，不报告截图成功。

## Acceptance Criteria

- 成功调用 `screenshot_capture` 时，structuredContent 同时包含原截图 metadata、`artifact_id`、`resource_uri`。
- 同一结果仍包含 MCP `ImageContent`，其 MIME/字节与 artifact 内容一致。
- `artifact(action=list, kind=screenshot)` 能列出自动生成的截图 artifact。
- `artifact(action=read, artifact_id=...)` 对截图返回 base64 delivery；resource URI 能读取同一二进制内容。
- screenshot metadata 的 `bytes`、`sha256` 与注册 artifact 完全一致。
- 普通 Workspace artifact 注册、读取和外部变更检测行为不回归。
- artifact 持久化失败时 `screenshot_capture` 不返回成功状态。

## Test Strategy

- Happy path：artifact service 注册托管 PNG/JPEG bytes，List/Get/Read/ReadAll 能完整恢复。
- Regression：现有 Workspace artifact 的 Register/List/Read/ReadAll 与 ErrChanged 测试继续通过。
- Screenshot contract：fake capturer 返回固定字节，断言 structuredContent 有 artifact ID/URI，Content 保留 ImageContent，并通过 artifact service 回读同一字节。
- Error case：模拟 artifact 注册失败，断言 screenshot tool 返回失败而不是仅 metadata 成功。
- Migration：新数据库和从现有 schema 升级都能创建 artifact blob 表。
- Verification：先跑相关 artifact/server 包测试，再跑 `go test ./... -count=1`、`gofmt` 检查；若时间允许再跑 `go vet ./...`。

## Implementation Plan

1. 先写 artifact service 托管字节的失败测试与 screenshot contract 回归测试，确认 RED。
2. 增加 artifact blob migration 和 `RegisterBytes`/统一读取实现，使 artifact 层测试 GREEN。
3. 将 `screenshot_capture` 接入自动 artifact 注册、事件和结构化引用，使 server 测试 GREEN。
4. 做必要的最小重构，确保 Workspace artifact 路径逻辑与托管字节逻辑边界清晰。
5. 运行相关测试、全量测试、格式与静态检查，记录验证结果。

## Verification

- `go test ./internal/artifact ./internal/server -count=1`
- `go test ./... -count=1`
- `test -z "$(gofmt -l ./cmd ./internal)"`
- `go vet ./...`
