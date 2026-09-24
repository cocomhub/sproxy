# sclient trash 命令设计

> 设计任务 S2a-2。需求源：`.design-ledger/task-s2a.md`。源码核实：`pkg/server/trash.go`（listTrashHandler / restoreTrashHandler / emptyTrashHandler）、`pkg/files/trash.go`（域语义）、`cmd/sclient/root.go`（注册模式）、`cmd/sclient/list.go` / `stat.go`（CLI 输出模式）、`pkg/client/client_ops.go`（SDK 操作面）。

## 背景 / 目标

- **现状**：服务端回收站端点已完备——`GET /api/trash`（列出 `{entries:[{trash_rel,name}]}`）、`POST /api/trash/restore?file=<trashRel>`（409 目标已存在/404 条目不存在/400 非法）、`POST /api/trash/empty`；软删经 `delete?soft=true`。**sclient 无封装**，用户只能 curl。
- **目标**：`sclient trash [list|restore <trashRel>|empty]`，复用 factory/IOStreams/OutputFormatter 既有模式。
- **语义边界**：restore 的操作对象是 **trash_rel**（list 输出的可操作令牌），不是原名——原名→trash_rel 映射存在歧义（同 rel 多次软删产生多个 nano 后缀条目），由 list 展示原名供人识别、trash_rel 供机器操作。

## 组件与接口

### SDK 层（pkg/client，新增 client_trash.go）

- `type TrashEntry struct { TrashRel string `json:"trash_rel"`; Name string `json:"name"` }`
- `func (c *FileClient) ListTrash(ctx) ([]TrashEntry, error)` → `GET /api/trash`，解析 `{entries:[...]}`；非 2xx → 包装错误。
- `func (c *FileClient) RestoreTrash(ctx, trashRel string) error` → `POST /api/trash/restore?file=`+`url.QueryEscape(trashRel)`；解析 `UploadResponse`，`Success=false` → 带 Message 的错误（409/404/400 的服务端语义经 Message 透传，供 CLI 直接展示）。
- `func (c *FileClient) EmptyTrash(ctx) error` → `POST /api/trash/empty`；同 RestoreTrash 的错误模式。
- 三个方法统一走 `c.doRequest`（自动带 SproxySig 签名/隧道），不引入新鉴权。

### CLI 层（cmd/sclient/trash.go）

- `NewCmdTrash(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command`：Use=`trash`，Short=「回收站管理」，无 RunE（仅容器），注册三个子命令（与 `stat` 容器命令同构）：
  - `list`：`cobra.NoArgs`；`factory.NewClient(cmd)` → `ListTrash` → `buildFormatterWithWriter(ios.Out, cmd)` → `PrintTrashList`。
  - `restore <trashRel>`：`cobra.ExactArgs(1)`；`RestoreTrash` → 成功 `ios.WriteOutLine("已恢复: %s", trashRel)`，失败 `WriteErrLine` + 包装错误。
  - `empty`：`cobra.NoArgs`；`EmptyTrash` → 成功 `ios.WriteOutLine("回收站已清空")`。
- **OutputFormatter 扩展**（output.go）：接口新增 `PrintTrashList(entries []client.TrashEntry)`；Text 实现输出表格 `原路径 <name> / 操作令牌 <trash_rel>`（空列表 → `fm.Println("回收站为空")`，对齐 `PrintVersionList` 空态）；JSON 实现输出 `{"entries":[...]}`（对齐 `PrintFileList`）。**必须**同时实现于 TextFormatter 与 JSONFormatter（interface 新增方法编译期强制），仓库内 mock formatter 一并补齐（若有，见风险 2）。
- **注册**：root.go `root.AddCommand(NewCmdTrash(factory, ios))`（紧邻 `NewCmdDelete` 后，文件操作族）。
- 卷语义：trash 端点 owner 维度、无 volume 参数（handler 走 `tenantFor(owner)`），CLI 不加 volume 透传——文档注明该边界。

## 数据流

1. `sclient trash list`：`factory.NewClient(cmd)`（config/凭据装配）→ `GET /api/trash`（doRequest 签名）→ 服务端 `listTrashHandler`（读 trash 桶 + 解析原路径）→ JSON → `PrintTrashList` 文本/JSON 输出。
2. `sclient trash restore <trashRel>`：`POST /api/trash/restore?file=<escaped>` → 服务端 `restoreTrashHandler` → `files.RestoreTrash`（校验前缀/后缀 → 目标冲突 409 → 父目录 MkdirAll → atomicRenameRoot）→ `UploadResponse{success}` → CLI 按 success 输出。
3. `sclient trash empty`：`POST /api/trash/empty` → `emptyTrashHandler` → `files.EmptyTrash`（不存在 trash 桶 = no-op）→ 成功消息。

## 错误处理

- SDK 层：非 2xx → `io.ReadAll(LimitReader(4<<10))` 读 body → `fmt.Errorf("...(HTTP %d): %s", ...)`（对齐 `Mkdir`/`Delete` 模式）；`UploadResponse.Success=false` → `fmt.Errorf("%s", Message)`（服务端 Message 已含 409/404/400 语义文案）。
- CLI 层：`factory.NewClient` 失败 → `ios.WriteErrLine("初始化客户端失败: %v", err)` + 包装错误（对齐 list.go）；命令错误 → `WriteErrLine` + `fmt.Errorf("...: %w", err)` 返回（cobra RunE 会打印）。
- restore 无参/多参 → cobra `ExactArgs` 报用法错误（对齐现有命令）。
- 空回收站：list 显示空态（非错误）；empty 幂等成功（服务端 no-op，CLI 不特殊处理）。

## 测试 + 变异点

TDD 红灯先行，测试只绑 127.0.0.1，纯标准库：

1. **pkg/client/client_trash_test.go**（newMockServer 模式）：ListTrash 解析 entries（trash_rel/name 双字段）；RestoreTrash 断言请求带 `file=` 参数且 409/404 时 Message 透传；EmptyTrash 断言命中 `/api/trash/empty` 且 success。变异点：restore 参数名错/漏 → 红；不解析 entries → 红；非 2xx 不报错 → 红。
2. **cmd/sclient/trash_test.go**（httptest mock server + cli test 模式）：`trash list` 输出含 name 与 trash_rel（--json 模式断言 JSON 结构）；`trash restore <rel>` 输出「已恢复」且 mock 收到正确 query；`trash empty` 输出「回收站已清空」；空回收站输出空态。变异点：子命令缺注册 → 红（unknown command）；restore 用原名而非 trash_rel 调接口 → 红（mock 断言 query）；错误不返回 → 红。
3. 零回归：现有 list/stat/output 测试全绿；OutputFormatter 新增方法后 Text/JSON 双实现编译通过（interface 扩展强制）。

R18 门禁：新增测试默认 `t.Parallel()`；确需串行（如复用固定端口 mock）显式标记 `// sproxy:serial: <理由>` 并登记棘轮。

## 片划分

- **片 1**：pkg/client 三方法 + 红灯测试（变异 1 组）→ 绿。
- **片 2**：cmd/sclient/trash.go + OutputFormatter 扩展 + CLI 红灯测试（变异 2 组）→ 绿 + root.go 注册。
- **片 3**：变异全命中复核 + `go test ./pkg/client/... ./cmd/sclient/... -race` + `make lint` + `gofmt`；docs/cli.md 补 trash 命令（随代码 PR，不单开纯文档 PR）。

## 风险与零回归

- **零回归保证**：纯新增——SDK 三方法为新符号；CLI 新命令不触碰既有命令 flag/语义；OutputFormatter 只增不改（Text/JSON 同步实现，编译期强制）。服务端零改动（端点已存在且稳定）。
- **风险 1**：OutputFormatter 为 `cmd/sclient` 包内接口，除 Text/JSON 外若有测试 mock 实现，新增方法会编译失败——先 `grep "OutputFormatter"` 定位全部实现再动手（脚本化检查，见片 2 前置）。
- **风险 2**：`trash restore` 的入参是 trash_rel（含 `.__deleted__<nano>` 后缀）而非原名，用户可能误传原名 → CLI help 与 list 输出中显式提示「恢复时使用 trash_rel 列」，并在文档写明；不做原名模糊匹配（歧义不可消解，YAGNI）。
- **风险 3**：软删入口（`delete?soft=true`）目前 CLI 未暴露——本任务不扩 delete 命令（不在范围），文档标注「回收站依赖服务端软删配置/既有调用方」，后续片可选加 `delete --soft`。
