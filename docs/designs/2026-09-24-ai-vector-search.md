# 向量索引 + 语义搜索（11.9-④）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：content index 升级 embedding + /api/search/semantic
> 只读源码：pkg/files/search_index.go（contentTokens 内容索引基座）、s3_server.go/dav.go（写路径旁路先例）
> 引用：[2026-09-24-ai-event-pipeline.md](./2026-09-24-ai-event-pipeline.md)（事件驱动向量化）、[2026-09-24-ai-privacy.md](./2026-09-24-ai-privacy.md)（落盘加密）

## 1. 背景与目标

- **现状**：`search_index.go` 已有 `contentTokens`（首 4KiB 抽样小写词元，`content` 开关默认 false）——**子串匹配级**内容索引，无语义能力；entry 为内存态 + 快照落盘（`saveIndexSnapshot`/`loadIndexSnapshot`）。
- **目标**：在 contentTokens 之上叠加 embedding 向量层——文件内容 → embedding（外部 embedding API 或本地模型）→ 向量持久化 → `GET /api/search/semantic?q=...` 余弦相似度 top-k 返回；**词元子串匹配保留为降级路径**（无 embedding 配置/API 失败时 `/api/search/semantic` 回退关键词检索，fail-safe）。
- **非目标**：不做向量数据库（无外部依赖——向量存本地 at-rest 加密文件 + 内存暴力 top-k，文件数 ≤1e5 规模够用）；不做 RAG 完整链路（只出检索端点）；不做 embedding 训练。

## 2. 组件与接口

### 2.1 `pkg/aiembed`（新包，embedding 客户端，标准库 + netutil）

```go
type Client struct { cfg Config; hc *http.Client }  // netutil.IsolatedTransport 基座（R19）

type Config struct {
    Provider string // "openai" | "ollama"（本地，默认）| "disabled"
    BaseURL  string // ollama: http://127.0.0.1:11434；openai: https://api.openai.com/v1
    APIKey   string // openai 需要；ollama 可空
    Model    string // openai: text-embedding-3-small；ollama: nomic-embed-text
    Dim      int    // 向量维数（openai 1536；ollama 由模型决定，可在运行时探测）
    Timeout  time.Duration
}
// Embed 单条文本 → []float32（归一化，供余弦）
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error)
// EmbedBatch 批量（≤32 条）——openai /embeddings batch；ollama /api/embed 数组
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
```

- **协议**：OpenAI 兼容 `/v1/embeddings`（`{"input": [...]}` → `data[i].embedding`）与 Ollama `/api/embed`（`{"input": [...]}` → `embeddings`），`New` 按 Provider 归一。响应体 `io.LimitReader`（如 1MB）防畸形响应。
- **降级语义**：Provider=disabled 或 APIKey 空 → `Client` 构造为 nil（装配层决定不启用向量层）。

### 2.2 向量存储（`pkg/files` 内 `vector_index.go`，与 searchIndex 同包同模式）

```go
// vectorEntry：per-rel 向量 + 元数据（挂在 indexEntry 旁，不改变现有索引结构）。
type vectorEntry struct {
    Vec  []float32 // 归一化 embedding
    Rev  int64     // 来源文件 rev/mtime（幂等）
    Text string    // 已 embedding 的文本摘录（溯源/重建）
}
// VectorStore：按 owner 分片，映射 rel → vectorEntry；内存 map + 周期落盘快照。
// 落盘走 at-rest 加密卷（ai-privacy 设计）：<tenant>/meta/vectors/<owner>.bin（gob+encrypt）。
type VectorStore struct { mu sync.RWMutex; byOwner map[string]map[string]*vectorEntry }
```

- **写路径接线**（仿 `upsert` 的 `sampleTokens` 调用点）：`upsert` 内 content 开关开启时——先取 `sampleTokens` 词元文本（现有），拼接为 embedding 输入文本（首 4KiB 文本）→ `EmbedBatch` 异步（不入写路径同步链，经事件消费 worker，见 ai-event-pipeline）。**写路径同步只登记占位**（rev/mtime），向量由 worker 补齐。
- **删除/重命名**：`remove`/`rename` 同步更新 VectorStore（同 map 操作，低成本）。
- **搜索**：`GET /api/search/semantic?q=...&topk=10`（受认证，owner 隔离）：
  1. `q` → `Embed(q)`（单条）；
  2. 遍历该 owner 所有 vectorEntry 余弦 top-k；
  3. 结果与 `FileInfo` 元数据合并（复用 `csMap`/volume）→ JSON `{results:[{name, score, size, ...}]}`。
  4. embedding 失败/未装配 → **回退**：现有 `search(q)` 关键词子串匹配（响应带 `"mode":"keyword"` 字段，可观测）。

### 2.3 配置（config.go 新增 `ai.search` 段）

```go
type AISearchConfig struct {
    Enabled bool       // 默认 false 零回归
    Embed   aiembed.Config // 复用 2.1
    TopK    int        // 默认 10（上限 50）
    Async   bool       // 默认 true：写路径异步 embedding（事件 worker）；false = 同步（测试用）
}
```

## 3. 数据流

```
启用（ai.search.enabled=true + Embed 配置）
  写路径 upload/complete 成功
    → 索引 upsert（现状）→ 占位 vectorEntry{rev, mtime} → 事件入队（embed 任务）
    → worker：EmbedBatch(文本) → 填 vectorEntry.Vec → 落盘快照（加密）
  查询 GET /api/search/semantic?q=
    → Embed(q) → 余弦 top-k → 合并 FileInfo → JSON
    → 失败/未启用 → search(q) 关键词模式（"mode":"keyword"）
  周期兜底：未补向量的占位条目（rev 落后/缺失 Vec）→ 扫描重embedding
```

## 4. 错误处理

| 场景 | 处理 |
|---|---|
| embedding API 不可用 | 查询回退关键词模式 + 响应带 `mode:keyword`；worker 退避重试 3 次 → 终败 audit |
| 大文件 | 仍首 4KiB 文本 embedding（与 sampleTokens 同源，成本有界）；文档注明语义只覆盖头部 |
| 非文本文件 | 无文本 → 无向量条目；关键词模式仍可搜文件名 |
| 向量快照损坏 | 忽略 + 全量重建（幂等） |
| 内存占用 | 归一化 []float32 × 文件数（1536 维 ≈ 6KB/文件，1e5 文件 ≈ 600MB——文档注明规模上限；超出建议关闭语义搜索） |

## 5. 测试 + 变异点

1. `aiembed` 单测（httptest mock /v1/embeddings 与 /api/embed）：解析/批量/非 2xx/畸形/超时/空 input → error（**变异：去掉 data 解析 → 红**）。
2. `TestVectorStore_PutGetDeleteRename`：map 生命周期（**变异：rename 不同步 → 红**）。
3. `TestVectorStore_PersistRoundtrip`：落盘→载入一致（加密卷 mock——gob 往返 + 密钥异则解不开：**变异：改密钥仍解 → 红**，衔接 ai-privacy）。
4. `TestSemanticSearch_TopK_ScoreOrder`：假嵌入函数（注入余弦易算向量）→ top-k 排序断言（**变异：排序反 → 红**）。
5. `TestSemanticSearch_KeywordFallback`：Embed 失败/未装配 → 返回关键词结果 + mode=keyword（**变异：失败 500 → 红**）。
6. `TestSemanticSearch_OwnerIsolation`：他人文件不可见（**变异：跨 owner 泄漏 → 红**）。
7. `TestSemanticSearch_AuthRequired`：未认证 401。
8. 集成：上传文件 → 事件入队 → worker 补向量 → 语义搜索命中（WaitFor 条件轮询）。

## 6. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **V1** | `pkg/aiembed` 客户端 + 单测 1 | 单测绿 | 无 |
| **V2** | `vector_index.go`（VectorStore/占位/同步增删改）+ 单测 2–3 | 单测绿 | V1、ai-privacy（加密卷） |
| **V3** | 查询端点 `/api/search/semantic` + 回退 + 单测 4–7 | 单测绿 | V2 |
| **V4** | 事件 worker 接线（ai-event-pipeline E1）+ 周期兜底 + 集成 8 + docs/config.md | 集成绿 | V2、E1 |

V1→V2→V3；V4 ∥ V3（worker 可后接）。

## 7. 风险与零回归保证

- **零回归**：默认 `ai.search.enabled=false` → 无 embedding 调用、无新端点行为（semantic 端点 404 或 400）、写路径零额外同步开销；`contentTokens` 关键词路径原样保留。
- **外部依赖风险**：embedding API 故障 → 回退关键词（fail-safe，不 500）；本地模型（ollama）可离线。
- **成本风险**：批量 embedding + 首 4KiB 有界 + 去重（rev 未变不重embed）；文档注明成本量级。
- **语义质量风险**：头部采样语义不完整 → 文档注明局限；未来可升级 chunk 级索引（片外）。
- **隐私风险**：向量/文本摘录落盘加密（ai-privacy）；embedding 外发 = 配置即同意（文档明示）。
