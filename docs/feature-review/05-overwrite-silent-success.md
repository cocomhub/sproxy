# 05 · 覆盖上传被静默当成成功（P1，2026-11-27）

**发现路径**：为 nightly 扩「覆盖写一致性」用例时，构造「先传 A 再传 B 到同一路径」，
真实服务端上 `Download` 读回 **A**（22 字节），而客户端返回 **nil error + Success**。

## 缺陷

`FileClient.ChunkedUpload` 的续传探测调用 `GET /upload/status`。服务端
`checkFileExistsStatus`（`pkg/files/chunked_upload.go:1181`）只要**该路径已有完整文件**
就回 `Completed: true`，并带上**那个文件**的 checksum —— 它回答的是「路径上已有完整文件」，
**不是**「你要传的这份内容已经在上面」。

客户端旧逻辑（`pkg/client/chunked.go` `tryResumeSession`）：

```go
if statusData.Completed {
    // 直接成功返回，不比对 checksum
}
```

于是「把 B 覆盖写到一个已有 A 的路径」：

- 客户端：`Success: true`，`Message: "文件已存在"`；
- 服务端：什么都没写，路径上仍是 A。

### 影响面

任何「重传/纠错」流程都会中招，且**无法从返回值发现**：

| 场景 | 后果 |
|---|---|
| sdserver 重传修复过的文件到同名路径 | 旧文件保留，报告上传成功 |
| 任何 sync 工具「确保内容 = 本地版」 | 远端保持旧内容，工具认为已同步 |
| 覆盖写后立即读回校验 | 读到旧内容，误判为「读侧陈旧」而非「根本没写」 |

### 为什么既有测试全绿

单次上传用例永远走空路径；覆盖写用例只在「路径不存在」的假设下构造。而服务端的
`Completed` 语义（按路径存在性回答）与客户端的解读（按内容相同理解）之间没有交叉断言。

## 修复

`tryResumeSession` 的 `Completed` 分支**核对内容**：

- 状态回报的 checksum 与本次待传一致（或状态未给 checksum，兼容老服务端）⇒ 幂等命中，短路成功；
- 不一致 ⇒ **不短路**，继续新建会话 ⇒ 服务端按自身策略处理：
  - versioning 开启 / 自动路由：备份旧版本后覆盖（内容真正更新）；
  - 显式指名卷（AD-4 卷唯一性查重）或 versioning 关闭：明确 **409**。

绝不「静默成功」。

## 断言

| 层 | 用例 | 判据 |
|---|---|---|
| 单元 | `TestChunkedUpload_ExistingDifferentChecksumNotShortCircuited` | 状态说 completed 但 checksum 不同 ⇒ 必须新建会话并真正传分块（旧代码此处 init=0） |
| 单元 | `TestChunkedUpload_ExistingSameChecksumStillShortCircuits` | 内容一致 ⇒ 零分块传输（不因修复而退化成全量重传） |
| e2e（真实服务端） | `TestOverwriteConsistencyRealVolumes` | 显式卷：同名不同内容**必须被拒**，且拒绝后内容/mtime/meta **原样保留**（不半覆盖） |
| e2e（真实服务端） | `TestOverwriteDefaultRouteRealVolumes` | 自动路由 + versioning：覆盖后内容/大小/checksum/meta 全量是 B |

### 变异验证（真实服务端，不是 mock）

把 `sameContentChecksum(...)` 改成 `true`（恢复无条件短路）：

```
meta_consistency_e2e_test.go:304: 自动路由覆盖后内容不是 B（len=26 want 43）⇒ 覆盖未生效或读侧陈旧
--- FAIL: TestOverwriteDefaultRouteRealVolumes
```

恢复修复后三个用例全绿。

## 顺带发现并修掉的 UI 缺陷（同一批）

`#v2-settings` 抽屉是 `z-index: 71`，而 `.modal-backdrop` 是 `z-index: 50` ⇒ **从设置抽屉里
打开的确认框被抽屉压住**（`elementFromPoint` 命中抽屉里的 toolbar，点击 30s 超时）。
原生 `confirm()` 不会有这个问题，所以是「把原生弹窗换成页面内模态」时引入的层级回归。
修：模态层 `z-index: 90`（高于设置抽屉 71 / 命令面板 71 / 拖拽提示 78 / 上传队列 55），
并写明理由 —— 模态是「用户必须先回应」的语义，必须压在任何非模态浮层之上。

## 边界

- 修复只在客户端：服务端 `Completed` 语义不变（它按路径回答是正确且被其它调用方依赖的）；
- 状态未给 checksum 时仍按「内容一致」短路（兼容老服务端，避免退化）；
- 显式指名卷的覆盖仍然被 AD-4 卷唯一性查重拒绝（这是刻意语义，不是缺陷）；
- 未覆盖「客户端拿到 409 后自动改走删除+重传」——是否自动删除旧文件属产品决策，未擅自实现。
