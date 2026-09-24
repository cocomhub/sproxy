# 卷备份/导出（11.3-②）

## 背景 / 目标
- 现状：无卷导出/导入能力（卷数据在 `<storage_root>/<owner>/` 多桶布局，备份只能直接拷磁盘目录，无版本/校验和语义）。
- 目标：`POST /api/volumes/export`（tar 流式导出卷，含文件内容 + 台账 checksum 清单）+ `POST /api/volumes/import`（恢复）+ sclient `backup` CLI（`backup <volume>` / `backup restore <archive>`）。

## 组件与接口
- `pkg/server/volume_export.go`（新文件）：
  - `exportVolumeHandler`：`GET/POST /api/volumes/export?volume=<name>` → `application/x-tar` 流式响应。tar 条目：`user/<rel>`（文件内容）+ 尾部 `manifest.json`（每条目相对路径 + SHA-256 + size + mtime + 台账 checksum 交叉校验）。流式 = 边读边写，不整卷入内存；响应流可中断（ctx 取消即止）。
  - `importVolumeHandler`：`POST /api/volumes/import?volume=<name>`（body = tar 流）→ 逐条目解包 → 复用既有写路径（`ValidateFilePath` 校验 + 配额 Reserve + 写后 checksum 登记——**不 bypass 台账**），manifest 校验每条目 checksum 一致才落盘（不一致 → 该条目标记失败并跳过，报告列出）。
- sclient：`backup <volume> -o <file.tar>`（下载导出流落盘）；`backup restore <file.tar> --volume <name>`（上传导入）。纯 HTTP 客户端复用 `FileClient` 既有隧道/签名机制。
- 权限：导出 = 读卷权限；导入 = 写卷权限 + 配额（复用 authMiddleware 主体语义）。

## 数据流
1. 导出：枚举卷文件（`ListDir` 递归，root 内）→ tar writer 流式写 `user/<rel>` → 写完 append `manifest.json` → 客户端落盘 `.tar`。
2. 导入：客户端上传 tar → 读 manifest → 逐条目解包 → `ValidateFilePath` + 配额 Reserve → 写文件 → 重算 checksum vs manifest（不符 → 跳过 + 报告）→ 登记台账。
3. 往返校验：导出后 sclient 本地重算 manifest 内每条目 checksum 与服务器返回一致；导入恢复后 `POST /api/verify`（功能 4）确认全卷一致。

## 错误处理
- 导出中卷文件被修改/删除 → 该条目记错误继续（或 409 中止，取决于 `strict` 参数：默认宽松继续，`strict=true` 中止）。
- 导入 tar 损坏/条目路径穿越（`ValidateFilePath` 拒绝 `..`/绝对路径）→ 该条目跳过 + 报告（**绝不写入卷外**）。
- 配额不足 → 该条目跳过 + `quota exceeded` 报告（不半卷提交）。
- 导入半途失败 → 已写入条目保留（可重跑，幂等：同名覆盖需 `--overwrite` 显式，否则跳过已有）。
- 超大单文件/整卷 → 流式不设整卷上限；单文件超 `MaxUploadBytes` 时走分块上传路径或文档标注限制。

## 测试 + 变异点
- `TestExport_StreamsTar`：导出流可解析为合法 tar 且文件内容与源一致（变异：非流式/漏文件 → 红）。
- `TestExport_ManifestChecksums`：manifest 每条目 SHA-256 与源文件一致（变异：checksum 错/缺 → 红）。
- `TestImport_RestoreConsistent`：导出 → 导入新卷 → `POST /api/verify` 全绿（往返 checksum 一致，变异：导入不校验 → 红）。
- `TestImport_PathTraversalRejected`：恶意 tar 条目 `../../x` → 拒绝不写卷外（变异：不过 ValidateFilePath → 红）。
- `TestImport_QuotaExceededSkipped`：超配额条目跳过 + 报告（变异：硬失败/写穿配额 → 红）。
- 变异验证核心：导出校验和清单与导入恢复校验两处各命中。

## 片划分
- P1：导出端点（tar 流 + manifest）+ 单测。
- P2：导入端点（解包 + 校验 + 台账登记）+ 单测。
- P3：sclient `backup`/`backup restore` CLI + 端到端往返测试。

## 风险与零回归
- 新增只读导出 + 显式导入端点，默认不挂定时行为；导入严格校验（路径/配额/checksum），绝不写卷外。
- 导入复用既有写路径（ValidateFilePath/配额/台账），不与现有上传语义分叉。
- 导出为快照语义：并发修改不保证一致性（文档标注，重跑即可）。
- 不删除任何源数据；导入覆盖需显式 `--overwrite`（防误覆盖）。
