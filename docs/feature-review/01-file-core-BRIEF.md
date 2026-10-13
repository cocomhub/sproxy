# 批次 1 审查：文件服务核心面

> 本批 3 路并发对抗审查：R1.1 REST 面 / R1.2 checksum 完整性 / R1.3 分块传输。
> 基线：master `e428acbe`。产出文件 `01-file-core-*.md`。

## 审查目标功能（roadmap 2.1）

- **R1.1 REST 面**：上传（`X-File-Checksum` 强校验 + 幂等）、下载（Range/分块）、删除（checksum 匹配）、
  重命名/移动、批量操作、目录、`/api/files` 列表、`/api/files/search` 搜索、stat 单文件元信息。
- **R1.2 数据完整性**：全链路 SHA-256 checksum 强制（上传必填、下载可校验、rename/delete 匹配）。
- **R1.3 分块上传/下载**：`/upload/init|chunk|status|complete` + `/download/chunk`；默认 4 MiB 块、
  并发 4、断点续传（会话 TTL 24h）、服务端块计划上界 65536。

## 审查维度与关注点

### 正确性
- 上传/下载数据一致性（checksum 全链路、Range 边界、断点续传跨会话恢复）
- 删除的 checksum 匹配语义、rename/move 的原子性（并发竞争）
- 分块会话状态机（init→chunk→complete）异常路径（超时/重复 chunk/缺块）

### 可用性
- 错误码与错误信息（400/401/404/409/413/416 语义正确）
- 幂等性（重复上传/重复 delete/重复 complete）
- 大文件边界（上限、块计划上界 65536、超限自动转分块）

### 安全性
- 路径穿越（`ValidateFilePath`：`..`、绝对路径、空字节、Windows 非法字符）
- 上传大小限制（MaxBytesReader、413 响应）
- 符号链接/目录逃逸（os.Root 约束）
- Range 请求的边界（负偏移、超范围、多段）

### 可维护性
- 处理器结构（handler 职责单一、错误集中处理）
- 测试覆盖（核心路径 + 边界 + fuzz）
- 文档同步（api.md 与实现一致）

## 输出格式

每路审查产出 `01-file-core-<名字>.md`，按 `README.md` 模板。重点给出**证据**（文件:行号 + 代码/测试引用），
发现分级 P0-P3。若无问题面列出「通过项」。
