# REPORT：sclient backup CLI

## 状态
DONE

## Commit
`c98ba33e25fcba6c39de11578a21b38b39167929`（分支 feat/sclient-backup，已 rebase origin/master b772eda69）

## PR
https://github.com/cocomhub/sproxy/pull/609

## 实现摘要
- `pkg/client/export.go`：`FileClient.ExportVolume(vol, dest)` → GET /api/volumes/export?volume=<name> 流式落盘（tmp+Rename 原子写，失败不残留半成品；复用 doRequest 既有 SproxySig 签名/隧道管线；空卷 = 全卷视图不发 volume 参数）
- `cmd/sclient/backup.go`：`sclient backup <vol> <dest>`（`-o` 与第二参数等价；卷名留空 = 导出全部可见卷；缺 dest 明确报错防误写）
- `cmd/sclient/root.go`：注册 backup 子命令
- 文档：`docs/roadmap.md` 11.7-5 / 11.8-A6 标记已落地；`docs/cli.md` 补 backup 节

## 测试
- pkg/client 单测 6：成功流式落盘（校验 volume query + tar 内容 + 无 .tmp 残留）/ 空卷不携带 volume / 403 报错且不生成目标文件 / 路径穿越 fail-closed（零请求）/ 空输出路径 / 签名管线（注入 RequestSigner 被调用）
- cmd/sclient 单测 5：happy path（volume query + 落盘内容 + 成功文案）/ 空卷 / -o flag / 403 报错不写文件 / 缺参 + 缺 dest 报错
- 全部 `t.Parallel()`（R18 棘轮通过），httptest 127.0.0.1，独立连接池（禁共享 http.DefaultClient）

## 验证
- `go build ./...` 通过
- `go test -count=1 -race ./pkg/client/... ./cmd/sclient/...` 全绿
- `go test -count=1 ./pkg/... ./cmd/sclient/...`：70 包 ok，0 失败
- `make archcheck` 通过（含 R18 并发门禁）
- `make lint` / `golangci-lint run ./pkg/client/... ./cmd/sclient/...` 0 issues
- gofmt / goimports 干净

## CI 状态
PR #609 已创建，Build×4 / Test Sub-Modules / UI E2E / E2E ubuntu / Detect docs-only / Conventional Commits 已绿；Test (ubuntu+Vault) / Test (windows) / E2E windows / SonarQube / Lint pending。等全绿后主 agent 合并。
