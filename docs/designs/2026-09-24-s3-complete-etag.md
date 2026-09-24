# 2026-09-24 S3 CompleteMultipartUpload：ETag 校验 + meta key 一致性

## 背景/目标

现状（`pkg/server/s3_multipart.go` 的 `s3CompleteMultipart`，已核实）：
- complete **不读 init 写的 meta**（`chunk/s3mp-<uploadID>.meta`，内容=key 字符串）——会话归属无法确认：abort 过的 uploadID 只要 part 文件还在就能继续拼接，任意 uploadID 可拼出文件。
- **完全不校验 `req.Parts[].ETag`**——客户端可提交任意/伪造 ETag，与落盘 part 内容脱钩；part 被篡改/替换也发现不了。
- 请求体 `io.ReadAll(r.Body)` **无上限**（OOM DoS，与 upload-part 已修的 `MaxBytesReader` 同族问题，审查批次 10 P1 的姊妹面）。
- 失败路径**不清理已部分写入的目标文件**（如 part 缺失时目标残留半截文件）。
- 输入合法性缺口：complete 对 `PartNumber` 不校验（负数/超 `s3MaxParts`/重复均可进拼接循环）。

目标（对齐 S3 `CompleteMultipartUpload` 协议语义）：
1. 读 meta 校验会话 key 与当前请求 key 一致（不一致 → 409）。
2. 逐 part 校验客户端 ETag（去引号）== `md5Hex(落盘 part 内容)`（不匹配 → 400，消息含 partNumber）。
3. 补输入合法性：PartNumber ∈ [1, s3MaxParts]、拒绝重复、body 有界（413）。
4. **清理不变量**：任一失败路径不残留半截目标文件（best-effort Remove）；part 文件仅成功时移除（失败保留，允许重试 complete）。

## 组件与接口

改动面：仅 `pkg/server/s3_multipart.go`（Handlers 无新字段、无新装配）。

新增/调整的私有函数：
- `readMultipartMeta(root *storage.Root, uploadID string) (string, error)`：读 `chunk/s3mp-<uploadID>.meta` 返回 key 字符串（root.Open + io.ReadAll，`rootWriteFile` 的反向）；不存在/读失败 → 调用方 409。
- `normalizeETag(s string) string`：`strings.Trim(s, "\"")`——兼容带引号（upload-part 返回 `"md5"`，AWS SDK/rclone 原样回传）与不带引号两种形态。
- `validateCompleteParts(parts []struct{PartNumber int; ETag string}) error`：**排序前**校验每个 `PartNumber ∈ [1, s3MaxParts]` 且无重复。
- `s3CompleteMultipart` 重排流程（见数据流），拼接循环改造为**单遍拷贝+哈希**：`dst := io.MultiWriter(f, hasher)`（md5.New），拷贝完成后比对 ETag——避免为校验读两遍 part。

接口不变：路由分发（`s3Handler` 的 `POST ?uploadId` 分支）、XML 响应结构（Key 元素保留；新增 ETag 元素为可选增强，见风险）。

## 数据流

1. body 有界读取（`http.MaxBytesReader(w, r.Body, size.DefaultChunkBodyLimit)`，对齐 upload-part）→ 超限 413 / 读失败 400。
2. 验签 `s3AuthOwner(w, r, body)`（body 参与 SigV4）→ 401/403。
3. `s3TenantFor` → 卷不可用 400。
4. `uploadId` 空 → 400。
5. `xml.Unmarshal` 失败 → 400；Parts 空 → 400。
6. **新增** `validateCompleteParts`：范围/重复 → 400。
7. **新增** `readMultipartMeta`：meta 缺失**或**内容 != 当前 key → 409（统一「会话无效/key 不匹配」）。
8. `sort.Slice` 按 PartNumber 升序。
9. `tnt.UserRel(key)` 失败 → 400；MkdirAll 父目录。
10. `OpenFile(rel, O_CREATE|O_WRONLY|O_TRUNC)` 失败 → 500。
11. 逐 part（排序序）：`Open(partRel)` 失败 → **改 400**（原 500；客户端引用了不存在的 part，属请求错误）+ 关目标 + best-effort Remove(rel)。
12. `io.MultiWriter(f, md5.New())` 拷贝；IO 错误 → 关目标 + Remove(rel) + 500。
13. 比对 `normalizeETag(p.ETag)` == `md5Hex(copied)`：不匹配 → 关目标 + Remove(rel) + 400（消息含 partNumber 与实际/期望 md5）。
14. 匹配 → `Remove(partRel)`。
15. 全部完成：关 f、Remove(meta)；响应 XML 含 `<Key>`；**可选增强**：响应 `ETag` = `hex(md5(concat(各 part 原始 md5 字节))) + "-" + partCount`（S3 分块复合 ETag 形态；哈希已在循环内，增量成本≈0）。

## 错误处理（对外契约）

| 状态 | 场景 | 说明 |
|---|---|---|
| 400 | body 读失败 / XML 解析失败 / 无 part / PartNumber 非法或重复 / ETag 不匹配 / part 缺失 | 消息含 partNumber（ETag/缺失场景） |
| 409 | meta 缺失或 key 不一致（会话无效） | 文案：`s3: 会话无效或 key 不匹配` |
| 413 | body 超 `size.DefaultChunkBodyLimit` | 对齐 upload-part |
| 500 | 目标创建失败 / 拼接 IO 错误 | — |

清理不变量：**任何 400/500 返回前，若目标已 OpenFile，必须 Close + best-effort Remove(rel)**；part 文件只在成功路径移除。

## 测试 + 变异点（TDD，先红灯）

测试（pkg/server，复用既有 S3 测试装配：init → upload-part → complete 真实序列）：
1. happy path：2 part → complete（真实 ETag）→ 200，目标内容=两 part 拼接，响应含 Key。
2. meta key 不一致（init 后用不同 key complete）→ 409。
3. 无效 uploadId（无 meta）→ 409。
4. 篡改 ETag（改一位）→ 400，且断言目标 rel **不存在**（无半截文件）。
5. Parts 引用未上传的 partNumber → 400。
6. 重复 PartNumber → 400。
7. PartNumber = 0 / 10001 → 400。
8. 超限 body → 413。
9. 不带引号 ETag → 200（normalize 兼容）。
10. （可选增强）响应 ETag == `hex(md5(md5(p1)+md5(p2)))+"-2"`。

变异点（变异后必须红）：
- 删 meta 读取/比对 → 测试 2/3 红。
- 删 ETag 比对 → 测试 4 红。
- 删失败清理 Remove(rel) → 测试 4「无残留」断言红。
- 删 PartNumber 范围/重复校验 → 测试 6/7 红。
- 删 MaxBytesReader → 测试 8 红。

## 片划分

单片可交付（改动集中一个文件、无新装配），内部按依赖序：
- P1 输入合法性 + meta 一致性（测试 2/3/6/7）。
- P2 ETag 单遍校验 + 清理不变量（测试 4/5/9/10）。
- P3 body 有界（测试 8）。
若拆 PR：P1+P3 一 PR（纯输入防御），P2 一 PR（校验+清理）。

## 风险与零回归保证

- **行为变更（有意，协议合规修复）**：此前接受任意/空 ETag 的 complete 现在 400；abort 过的会话由「碰巧成功」变 409。AWS SDK/rclone 均回传 upload-part 的真实 ETag，不受影响。
- **响应 XML**：新增 ETag 元素为加法（S3 客户端忽略未知字段）；若零回归优先可先不加（标注可选）。
- **零回归**：happy path 状态码、响应结构、part 清理时机不变；改动局限 `s3CompleteMultipart` 内部，不触碰 init/upload-part/abort 与其他路由。
- **已知残余（不属本片）**：complete 与 abort/并发 complete 无会话级锁（沿用现状）；`s3mp-*` part 无 TTL GC（崩溃残留占盘）——见配额片残余项。
