# MCPX 开发与贡献说明

## 分支策略

- `main` 是受保护分支，禁止直接推送、强制推送和删除。
- 功能、缺陷修复和文档变更都从最新 `main` 创建独立分支。
- 分支命名建议使用 `feat/<name>`、`fix/<name>`、`docs/<name>` 或 `chore/<name>`。

```bash
git switch main
git pull --ff-only origin main
git switch -c feat/example
```

## 本地验证

提交前至少运行：

```bash
gofmt -w ./cmd ./internal
test -z "$(gofmt -l ./cmd ./internal)"
go test ./... -count=1
go vet ./...
```

涉及并发、状态、鉴权或网关改动时，再运行：

```bash
go test -race ./... -count=1
```

## 桌面端（internal/desktop）

桌面端只在 Windows 上编译，其余平台是占位实现，因此 `go vet ./...` 与 CI 不受影响。

前端构建产物 `internal/desktop/frontend/dist/` **必须提交进仓库**：`go:embed` 在编译期
就需要它，提交后 `go build` 和 GoReleaser 在没有 npm 的环境下也能工作。改动前端后请执行：

```bash
cd internal/desktop/frontend
npm ci
npm run build      # 同时跑 vue-tsc 类型检查
```

并把 `dist/` 的变更一并提交。仅改 Go 代码时不需要这一步。

本地调试桌面端：

```bash
go build -o bin/mcpx.exe ./cmd/mcpx-server
./bin/mcpx.exe desktop
```

## 提交与 Pull Request

提交信息使用 Conventional Commits，subject 使用中文动词描述：

```text
feat(模块): 添加功能
fix(模块): 修复问题
docs(模块): 完善说明
```

完成本地验证后推送工作分支并创建 PR：

```bash
git push -u origin feat/example
```

PR 合并要求：

1. CI 全部通过，包括 test、gofmt、vet、race 和 build。
2. 至少一名维护者完成代码审查。
3. 变更说明、兼容性影响和测试结果填写完整。
4. 使用 GitHub PR 合并到 `main`；禁止绕过 PR 直接推送。

建议仓库设置为：必须通过 CI 状态检查、必须经过审查、禁止 force push、禁止删除 `main`，并在合并后删除已合并分支。

## 发布

发布只能从已合并且 CI 通过的 `main` 提交创建版本标签：

```bash
git switch main
git pull --ff-only origin main
git tag v0.1.0
git push origin v0.1.0
```

版本标签会触发 GitHub Actions 和 GoReleaser。发布前不要在 `main` 上重写历史；版本修复继续通过 PR 合并后再创建新标签。
