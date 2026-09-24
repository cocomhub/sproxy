# 压缩算法扩展（11.10-⑨）

## 背景/目标
- 现状：检索源码未见现有压缩实现入口（归档/传输压缩语义在 `pkg/archive`、传输层为密文流不可压缩）；存档与上传/下载通道无 zstd/brotli 选项。
- 目标：为**存档（archive 压缩）**与**存储传输**引入 zstd 高压缩比算法扩展，brotli 作为存档备选；默认 gzip（或现状）零变化，算法可选且可观测（响应头/归档元数据标注）。

## 组件与接口
- 依赖策略：`github.com/klauspost/compress/zstd`（纯 Go、社区活跃——符合「优先纯 Go 实现、API 稳定、社区活跃」）；brotli 同库 `klauspost/compress/brotli`；两者均归入 `pkg/archive` 与传输层各自独立 module 边界（核心库 go.mod 增加依赖需评审——设计上把压缩编解码收进 `pkg/compressx`（新包）集中管理）。
- `pkg/compressx`（新包，核心库）：
  - `type Algorithm string`：`gzip`（默认）/`zstd`/`brotli`；`Parse(string) (Algorithm, error)`（非法 → error，不静默回退）。
  - `NewReader(algo, r io.Reader) (io.ReadCloser, error)` / `NewWriter(algo, w io.Writer, level int) (io.WriteCloser, error)`：封装三个实现；zstd 用 `zstd.NewWriter`（level 默认 SpeedDefault），brotli `brotli.NewWriterLevel`。
- `pkg/archive`（现状文件未在本批阅读范围，落地核对）：
  - 配置/参数加 `compression`（默认 gzip 零回归）；归档任务结果元数据加 `compression` 字段（可观测）。
- 传输层（xfer）**:密文不可压**——设计明确**不做** xfer 层压缩（AES-256-GCM 后压缩比≈1，白耗 CPU）；若未来做明文端压缩，挂应用层 HTTP 响应 `Content-Encoding: zstd`（标准库无 zstd——用 compressx 包装，客户端解压在 FileClient 内）。

## 数据流
`POST /api/archive`（带 `compression=zstd`）→ compressx.NewWriter(zstd) → 归档写流 → 元数据记录算法 → 下载时 `Content-Encoding`/归档列表展示。FileClient 侧 `Accept-Encoding` + `Content-Encoding` 对拍（仅 zstd/gzip 白名单，brotli 不进传输通道——浏览器/客户端生态差异，避免互操作坑）。

## 错误处理
- 非法算法参数 → 400（不静默回退 gzip——显式语义）；解压失败（损坏流）→ 归档任务 failed + 历史记录；compressx 构造失败（无效 level）→ 任务创建即失败。
- zstd 流**不具备** gzip 的流式逐块可读性（有 frame 边界）——分块上传场景用 `DecodeAll`/编码器同步 flush 语义需明确：**块级**（每 chunk 独立 frame，可随机读）vs **流级**（单 frame 顺序读）；设计选**块级**（与 chunked upload/download 现有 offset/size 语义兼容，随机读可用）。

## 测试+变异点
- compressx 单测：三算法 round-trip 一致性（写入 → 读回字节相等）；损坏流解压失败；非法算法 error；level 边界。
- archive 集成：zstd 归档 → 解压 → 内容一致；块级 zstd（多 frame 拼接）随机读语义（offset/size 命中帧边界）。
- 变异验证：① 算法解析静默回退 gzip → 非法参数用例红；② zstd round-trip 少 flush → 数据不一致测试红；③ 块级变流级 → 随机读用例红。

## 片划分
- 片1：`pkg/compressx` + 单测（新依赖引入，独立 PR 评审依赖）。
- 片2：archive 接线（compression 参数 + 元数据 + 集成测试）。
- 片3：FileClient 传输通道 zstd 协商 + docs（压缩选型指南：什么时候 zstd/brotli 值得）。

## 风险与零回归
- 默认 gzip 逐字旧行为（golden 归档测试锁定）；compressx 新包无侵入既有调用方；块级 frame 语义与分块上传/下载兼容（不破坏 offset/size 契约）；新增依赖走评审流程（纯 Go、活跃、无 cgo）；**明确排除** xfer 密文层压缩（无收益，防误接）。
