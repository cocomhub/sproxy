# 审查：上传路径（POST /upload）

- **批次**：1
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`（feature-review worktree 已同步）
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 1 / P3 2

## 发现清单

### [P2] checksum 字符串比较非常量时间（4 处）
- **位置**：`pkg/files/service.go:466`（verifyFileWithChecksumRoot）、`pkg/files/write_ops.go:249`（WriteFile 上传校验）、`pkg/files/write_ops.go:1090`（DeleteFile）、`pkg/files/chunked_upload.go:705`（UploadChunk）
- **问题**：`actual == expectedChecksum` 直接字符串比较，理论上存在时序侧信道。但比较对象是 SHA-256 哈希（64 hex，非密钥），攻击者无法通过时序猜出完整哈希值，实际风险极低。
- **建议**：若追求一致性，可收口为 `checksum.Equal`（crypto/subtle）统一比较；属防御纵深，非必须。

### [P3] 上传 ctx 取消中断拷贝但已写内容保留
- **位置**：`pkg/files/write.go:134-171`（copyWithContext）
- **问题**：`copyWithContext` 每次 Read/Write 前检查 `ctx.Done()`，大文件上传中途客户端断开会中断；此时临时文件已写部分由 `writeFileAtomicallyRoot` 的 `defer root.Remove(tmpRel)` 清理，正式 rel 未动——行为正确。仅提示：中断不计审计（`recordUploadSuccess` 未走到），无审计留痕属轻微。
- **建议**：可选——上传中断记一条 `auditResultError` 审计行。

### [P3] 覆盖写 checksum 失败时旧版本依赖 versioning 才保留
- **位置**：`pkg/files/write_ops.go:239-255`（checksum 比对失败分支）
- **问题**：覆盖写场景（rel 已存在）若新内容 checksum 不匹配，`root.Remove(rel)` 删除的是已被 rename 替换的新内容；旧文件在 versioning 关闭时已被覆盖不可恢复（写入前 `handleDuplicateFile` 已判定：checksum 不匹配 + versioning 关 → 409 拒绝，不会走到覆盖写）。因此**实际不可达**——versioning 关闭时同名不同 checksum 在 dup-check 阶段即 409，覆盖写只在 versioning 开启时发生（此时已 SaveVersion）。行为正确，无数据丢失面。
- **结论**：确认无问题，仅记录推理链。

## 通过项（无问题面）

- **checksum 强校验**：上传必填 `X-File-Checksum`（`write.go:79-86` 缺失即 400）；服务端计算值与期望不符 → 删已写内容 + 400（`write_ops.go:249-255`）。
- **幂等**：`handleDuplicateFile`（`write_ops.go:340`）同 checksum 直接 200 不写盘（Idempotent=true）。
- **原子写**：`writeFileAtomicallyRoot`（`write.go:110`）临时文件 O_EXCL → 写入 → rename 原子替换；全程 root 相对防符号链接逃逸。
- **并发防护**：`TryMark(owner, rel, uploadingLockUpload)`（`write_ops.go:117`）同 rel 并发上传 409。
- **配额双账本**：`routeUpload` 双预留（owner Scope + 卷容量池），成功 `route.Commit(prev, written)`（覆盖写 Adjust 差分），失败 `route.Release()`——所有提前 return 路径均有 Release（代码逐路径核对）。
- **路径安全**：`resolveWritePath`（`write_ops.go:278`）= pathguard.ValidateFilePath（拒绝 `..`/绝对路径/空字节/Windows 非法字符）+ `Tenant.UserRel`（拒绝 `. __` 前缀、Windows 保留设备名、尾点/尾空格）。
- **去重**：dedup 硬链接零拷贝 + 台账引用计数（`write_ops.go:199-236`），FAT/exFAT 回退复制且校验 fallback checksum（`dedupFallbackCopy`）。
- **卷路由**：home 卷定位 + forceHomeVol 覆盖写 stay-home（AD-6 防 ACL 绕过）；locate miss 交容量路由。
- **测试**：`go test -run 'TestWrite|TestUpload|TestQuota' ./pkg/files/...` 全绿（1.68s）。

## 验证方式

- 源码逐路径审查（上传 6 步执行序 + 全部提前 return 的配额释放）
- `go test -count=1 -timeout 120s -run 'TestWrite|TestDelete|TestUpload|TestSearch|TestList|TestQuota|TestChunk' ./pkg/files/...` → **ok**
