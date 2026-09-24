# AI 数据隐私（11.9-⑧）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：向量/摘要落盘加密 + 外发最小化 + 用户可见性
> 只读源码：pkg/files/search_index.go（内容抽样与快照落盘——saveIndexSnapshot/loadIndexSnapshot 明文先例）、pkg/server/audit_store.go（落盘先例）
> 被引用：ai-vector-search（向量落盘）、ai-file-insight（摘要缓存）、ai-quota-audit（审计）

## 1. 背景与目标

- **现状**：文件本体经 at-rest 加密卷（`OpenDecrypted`/`Root` 解密层，storage 包已有）；但索引快照（`saveIndexSnapshot`）与审计日志（audit_store）为**明文 JSON/快照落盘**——AI 派生数据（向量、摘要文本、标签）若照抄此模式会明文落盘，泄露文件内容指纹。
- **目标**：所有 AI 派生数据（向量 embedding、摘要/标签文本、抽样文本摘录）落盘**复用 at-rest 加密卷**（`<tenant>/meta/` 下加密桶）；外发最小化（只发送必要抽样）；用户可见性（`GET /api/ai/privacy` 展示落盘了什么、密钥来源、关闭即清除）。
- **非目标**：不做用户级端到端加密（E2EE——服务端运维视角仍需可审计，加密卷是静态加密语义）；不改文件本体加密现状；不加密审计日志元数据（action/时间戳不敏感——沿用现状，但 AI 事件 detail 不含全文）。

## 2. 组件与接口

### 2.1 复用 at-rest 加密卷（`pkg/storage`）

- 现有 `Root.OpenDecrypted(rel)`/`OpenFile` 是加密卷的读写入口。**AI 元数据落盘同样走 `tnt.Root()`**：路径 `<tenant根>/meta/ai/<owner>/<kind>.bin`（kind = vectors | insight | tags），经既有加密层透明加解密。
- 关键约束：**不得**绕过 Root 直接 `os.WriteFile`（与 `saveIndexSnapshot` 不同——那是既有明文快照，AI 数据必须加密）。装配层判断：`ai.privacy.enabled=true` 时 AI 快照全部走 Root；未启用 → AI 功能整体不可用（fail-closed，不落明文）。

### 2.2 `pkg/server/ai_privacy.go`（新文件）

```go
// AIPrivacy 管理 AI 派生数据落盘 + 用户可见性。
type AIPrivacy struct {
    enabled bool
    root    func(tnt *storage.Tenant) *storage.Root // 加密卷根
    logger  *slog.Logger
}
// StoreAI 写 AI 派生数据（向量/摘要/标签；经加密卷 Root）。
func (p *AIPrivacy) StoreAI(tnt *storage.Tenant, kind, owner, rel string, data []byte) error
// LoadAI 读（解密）；不存在 → 缓存未命中。
func (p *AIPrivacy) LoadAI(tnt *storage.Tenant, kind, owner, rel string) ([]byte, error)
// DeleteAI 删除（文件删除/rename 时清理）。
func (p *AIPrivacy) DeleteAI(tnt *storage.Tenant, kind, owner, rel string) error
// List 返回 owner 落盘清单（GET /api/ai/privacy 用：kind/rel/大小/时间）。
func (p *AIPrivacy) List(tnt *storage.Tenant, owner string) []AIArtifactInfo
// PurgeOwner 关闭即清除：删除 owner 全部 AI 派生数据（用户行使删除权）。
func (p *AIPrivacy) PurgeOwner(tnt *storage.Tenant, owner string) (int, error)
```

### 2.3 落盘格式与密钥

- **格式**：gob（`[]byte` 载荷）+ 加密卷透明加密。不做二次加密（卷密钥即边界）；文档注明「密钥 = 卷加密密钥（master key 派生），与文件本体同界」。
- **外发最小化**：embedding/摘要只发送抽样文本（首 4KiB，现有 `sampleTokens` 同源）；**不做**整文件外发；prompt 不拼接任意用户注入内容（user 消息仅含文件名+抽样）。
- **元数据最小化**：审计 `ai.*` 事件 detail 只记 rel/耗时/估算 tokens——**不含摘要文本/标签内容**（防审计日志明文泄露内容）。

### 2.4 端点

```go
// GET /api/ai/privacy → 200 {enabled, key_source, artifacts:[{kind, rel, bytes, ts}]}
//   （受认证，owner 自见；enabled=false → 400「AI 未启用」）
// POST /api/ai/privacy/purge → 200 {deleted:N}（删除该 owner 全部 AI 派生数据；
//   破坏性操作——按授权策略需用户显式触发，端点语义即用户行使权利，非服务端自动）
```

## 3. 数据流

```
向量/摘要生成完成
  → StoreAI(root, kind, owner, rel, gob(bytes))   // 经加密卷 Root 落盘
查询命中缓存
  → LoadAI(root, ...) → 解密 → 使用
文件 delete/rename
  → DeleteAI（与索引 remove/rename 同调用点，见 vector-search/event-pipeline）
用户查可见性
  → GET /api/ai/privacy → List → JSON
用户清除
  → POST /api/ai/privacy/purge → PurgeOwner → 审计（ai.privacy_purge）
```

## 4. 错误处理

| 场景 | 处理 |
|---|---|
| AI 功能开启但卷无加密（storage 未启用加密） | 装配期 **Warn + 拒绝启用**（fail-closed：不落明文）；日志明示「AI 需要加密卷」 |
| 落盘失败 | Warn + 缓存不命中（下次重算）；不阻断业务 |
| 数据损坏 | LoadAI 失败 → 视为未命中 → 重新生成（幂等） |
| 密钥轮换 | 沿用卷密钥轮换机制（既有）；AI 派生数据随卷重新加密 |
| purge 中断 | 幂等删除（按清单逐个删，失败重试）；返回已删数量 |

## 5. 测试 + 变异点

1. `TestAIPrivacy_StoreLoadRoundtrip`：Store → Load 一致（**变异：绕过 Root 明文写 → 红**——测试断言落盘文件内容在加密卷外不可读/不存在明文路径）。
2. `TestAIPrivacy_NoEncryptedVolume_FailsClosed`：卷未加密 → 启用拒绝（**变异：允许明文启用 → 红**）。
3. `TestAIPrivacy_DeleteOnFileRemove`：delete 事件 → DeleteAI 调用（fake recorder；**变异：漏删 → 红**）。
4. `TestAIPrivacy_PurgeOwner`：purge 后 List 空 + 审计记录（**变异：purge 不删 → 红**）。
5. `TestAIPrivacy_ListShape`：可见性端点字段齐全（kind/rel/bytes/ts）。
6. `TestAIArtifactNoContentInAudit`：ai.* 审计 detail 不含摘要文本/标签内容（**变异：detail 带全文 → 红**——防审计明文泄露）。
7. `TestAIPrivacy_Disabled`：enabled=false → 端点 400（零回归）。
8. 集成：加密卷装配 → 向量落盘 → 重启 → LoadAI 命中（解密恢复）。

## 6. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **P1** | AIPrivacy（Store/Load/Delete/List/Purge）+ 单测 1、3–5 | 单测绿 | storage 加密卷 |
| **P2** | 装配 fail-closed（卷未加密拒绝启用）+ 端点 /api/ai/privacy + 单测 2、7 | 单测绿 | P1 |
| **P3** | 接入 vector-store 与 insight-cache 落盘替换（明文快照 → 加密）+ 审计 detail 脱敏 + 单测 6 | 单测绿 | P1、V2、I1 |
| **P4** | 集成 8 + docs/config.md `ai.privacy` 段 + 隐私说明文档（R15） | 集成绿 + 文档门禁绿 | P3 |

P1 → P2 → P3；P4 收尾。

## 7. 风险与零回归保证

- **零回归**：默认 `ai.privacy.enabled=false`（AI 族功能默认全关）→ 无新落盘、无新端点；既有 `saveIndexSnapshot` 明文快照**不动**（那是既有行为，本期只约束 AI 派生数据）。
- **明文泄漏风险**：AI 数据**一律**走加密卷（fail-closed 装配门禁）；审计 detail 脱敏（单测 6 变异保护）。
- **性能风险**：加密卷读写开销与文件本体同界（既有层）；向量快照按 owner 批量读写（非逐条）控 I/O。
- **删除权风险**：purge 端点=用户行使权利（非自动 GC）；文档明示清除范围（向量/摘要/标签，不含审计日志——审计是合规留存，单独说明）。
- **密钥风险**：不引入新密钥（复用卷主密钥）；密钥不落日志/配置明文。
