# 云端下载完整性校验 设计规格

> 状态：**草稿待审查** | 日期：2026-10-06 | 分支：feat/integrity-check
> 关联：#732 云端下载转存、#738 三行为收口、cocom pkg/imaging（VerifyImage 移植）、shardseal 算法注册表（注册模式参考）

## 1. 背景与目标

云端下载后需**完整性校验**：字节级（checksum）已存在，本次新增**语义级**（内容是否可用：视频可播放/图片可解码/tar 可遍历）+ **权威匹配**（本地计算 vs 服务端带外值）。

**核心裁定（用户）**：
- 原始链接/官方工具提供的信息（ETag/官方 hash）**最权威**；仅本地计算 checksum + 大小比较**可信度低**
- **必须**本地计算 checksum **与权威信息一致**才完全信任数据完整性
- 校验失败自动重下；**两次本地 checksum 一致仍异常** → 原始文件问题 → **允许继续流程**（默认标记，可选阻断）
- 注册表 + 按类型分发（参考 shardseal 算法注册管理）；tar 标准库够；校验嵌入 runRetryLoop
- 图片校验移植 cocom `/v2/api/nhcomic/verify` 能力；plugin + ext + go.mod 机制注册通用类型

## 2. 完整性模型（三态 + 可信度分层）

### 完整性判定（checksum 阶段，职责分离）
```
下载完成 (Result{Checksum, Size, ETag, IntegrityMode})
  │
  ▼ ① 权威匹配：本地算 sha256/GCID == 服务端带外值（ETag/官方 hash）→ 文件一定完整 → ✅ 跳过语义校验
  ▼ ② 仅本地自洽：本地算 checksum + size（无权威可比）→ 低可信 → 语义校验兜底（确认可用）
  ▼ ③ 下载器确认完整（Content-Length 匹配/自算）→ 需本地计算验证（不替代）
```

### 下载器可信度审计（agent 实测 2026-10-06）

| 下载器 | Checksum 来源 | 可信度 | 完整性模式 |
|---|---|---|---|
| HTTP（http_downloader.go） | 下载后**本地算 sha256** | ② 自洽；ETag 可作弱权威（If-Range 强校验时） | `ModeSelfVerified`/ETag 权威 |
| PikPak（pikpak/downloader.go） | CLI 回传（本地 sha256） | **② 本地自洽**（CLI 无内部校验） | `ModeLocalOnly` + 可选 GCID 权威 |
| PikPak GCID（官方 hash） | `get --raw` hash = **GCID**（sha1(concat(sha1(分块)))，非整文件哈希） | **权威带外源**（弱权威） | `ModeAuthority`（需 GCID 复算） |

### IntegrityMode 接口（下载器自声明完整确认方式）
```go
// pkg/downloader
type IntegrityMode int
const (
    ModeUnknown      IntegrityMode = iota // 无信息
    ModeLocalOnly                         // 仅本地 checksum+size（② 态）
    ModeSelfVerified                      // 下载器自算 checksum（② 态，无权威可比）
    ModeAuthority                         // 权威匹配：本地 == 服务端带外值（① 态）
)
// Result 扩展
type Result struct {
    Size     int64
    Checksum string        // SHA-256 十六进制（本地计算）
    ETag     string
    ModTime  time.Time
    Integrity IntegrityMode // 本下载器完整性模式
    AuthorityHash string     // 服务端带外权威 hash（如 pikpak GCID；可空）
}
// 下载器能力接口（可选）
type IntegrityProvider interface { IntegrityMode() IntegrityMode }
```

## 3. 校验管道（pkg/cloud 整合）

### 流程（checksum 后语义校验）
```
runRetryLoop（下载尝试）
  → runDownloadAttempt 成功 → 完整性判定：
      ① 权威匹配 → result 可信 → 跳过语义校验 → 返回
      ② 仅本地自洽 → 语义校验（CheckByType）：
          ├─ 通过 → 返回
          └─ 异常 → 重下（同次循环 retry；两次本地 checksum 一致仍异常）
                → 原始文件问题 → 按任务配置：
                    ├─ 默认：允许继续 + IntegrityStatus="damaged"（人工审计）
                    └─ integrity_must_pass=true：阻断（failTask）
  → 语义校验通过/权威匹配 → 转存（读回校验已有）→ 取用
```

### 校验嵌入 runRetryLoop
- `runDownloadAttempt` 成功分支**加语义校验点**：校验失败 → `shouldRetryDownload` 语义（重下复用退避/超时/取消）
- 两次本地 checksum 一致仍异常 → 原始文件问题 → 非重试错误（按任务处置）
- **不单独循环**（避免重复退避/取消逻辑）

## 4. pkg/integrity（零外部依赖，根模块）

### 注册表（shardseal 模式）
```go
// pkg/integrity
type Checker interface {
    Kind() string                                   // "image/png" / "video/mp4" / "archive/tar"
    Matches(name string) bool                       // 按扩展名匹配
    Check(ctx context.Context, path string, size int64) (*Report, error)
}
type Report struct { OK bool; Reason string }       // OK=false → 语义异常（可继续/阻断）
func Register(kind string, f CheckerFactory)        // 全局注册 + Kind 唯一性 panic
func Lookup(name string) Checker                    // 按类型分发（nil = 无校验器 → 仅字节级）
```

### 校验器（标准库，零外部依赖）
| Kind | 实现 | 判据 |
|---|---|---|
| `image/*` | 标准库 `image.DecodeConfig`+`Decode`+bounds（移植 cocom VerifyImage） | 可解码 + Bounds 非空 |
| `archive/tar` | 标准库 `archive/tar`+`compressx` 遍历 | 全部条目可遍历 |
| 未知类型 | nil（仅字节级校验） | 不阻断 |

### 插件扩展（ext/ + 独立 go.mod，plugin 模式）
```
pkg/integrity/ext/video/  独立 go.mod：ffprobe 校验器（复用 pkg/media/ffprobe）——唯一需外部依赖
cmd/sproxy/               装配 ext 插件（空白导入注册）
```
- 新类型校验器 → `pkg/integrity/ext/<kind>/`（独立 go.mod），cmd/sproxy 空白导入装配
- 机制化：使用方只查注册表，不硬编码类型

## 5. 任务参数 + 状态

```go
// CloudTask 新增
IntegrityStatus string `json:"integrity_status,omitempty"` // "" | verified | damaged
IntegrityMustPass  bool   `json:"integrity_must_pass,omitempty"`  // true = 校验失败阻断（默认 false 放行标记）
```
- 客户端 flag：`--integrity-must-pass`（默认 false 零回归）
- API 暴露 + WebUI 展示 IntegrityStatus（审计）
- 创建时校验参数合法性（force 需校验器存在？——否，未知类型 force 时按 ② 语义校验，无校验器 → 视为通过）

## 6. PikPak GCID 权威复算（可选增强）

- `get --raw` 的 `hash` = GCID（分块 sha1 矢量），作**权威带外源**
- 下载后本地 GCID 复算：候选分块（256KB ~ 4MB）尝试命中官方 hash
- 命中 → `ModeAuthority`（① 态）；未命中 → `ModeLocalOnly`（② 态语义校验兜底）
- 分块粒度非固定（大文件随上传/离线任务变化）→ 候选集合自适应；分块大小入 result 供审计
- 残余：大文件 GCID 分块未实证（推断），匿名分享无法实测

## 7. 错误处理与审计

- 校验失败重下耗尽 → `IntegrityStatus="damaged"` + 任务保留（不静默丢数据）
- force=true 且 damaged → failTask（明确原因）
- damaged 状态全链可见（API/WebUI/日志）+ 审计字段
- 两次本地 checksum 一致仍异常 = 原始文件问题（不重下，避免死循环）

## 7.5 资源隔离与内存配额（2026-10-07 用户裁定）

**下载并发槽与完整性校验隔离**：校验（含 video ffprobe 最长 5min）不再占用
`cloud_max_concurrent` 下载槽——`runRetryLoop` 每次尝试独立 acquire/释放下载槽
（`acquireDownloadSlot`/`releaseDownloadSlot`），下载完成后**立即释放槽**，校验在槽外
执行（重下时重新 acquire）。markDownloading（置 downloading）在持槽后置，保证
「downloading 状态 ≤ 持槽数」并发上限不变量。

**校验内存配额排队**（`cloud_check_mem_bytes`，ByteSize 语义，默认 512MiB；**<=0（含
0/负值/缺省）一律按默认 512MiB**——用户裁定 2026-10-07：0 处理成默认大小，保证服务
默认行为安全可用，配额治理默认生效防并发校验 OOM）：
- 校验器 `EstimateMem` 在 Check 前估算峰值内存（image=像素×4 钳制 2500 万；tar=64KiB
  固定；video=ffprobe 进程 64MiB + JSON 估算封顶 256MiB）；
- `checkMemSem`（semaphore.Weighted）按估算字节排队（并发校验总估算 ≤ 配额，不足等待）；
- 单文件估算超配额 → **跳过校验标记 unverified**（无校验能力 ≠ 损坏，不误判 damaged）；
- 配额信号量由配置层保证非 nil（<=0 归默认），永不因缺省而禁用。

**不同流程隔离**：下载（`semaphore`）→ 校验（`checkMemSem` 内存配额）→ 转存
（`transferSem`）各独立限流，按各自资源（磁盘/内存/CPU）排队，互不 head-of-line 阻塞。

## 8. 测试计划

- pkg/integrity：注册表唯一性/分发；image 校验（png/jpeg 解码 + 损坏文件拒）；tar 遍历（合法/损坏）
- pkg/downloader：IntegrityMode 声明；Result 扩展
- pkg/cloud：下载→②态→语义校验→重下→两次一致仍异常→默认放行标记 + force 阻断；①态跳过语义校验
- pikpak GCID：小文件复算命中（256KB 分块）
- 客户端：--integrity-must-pass 透传
- 配额治理：超配额跳过 unverified；并发校验排队（信号量不足等待）；并发上限不变量

## 9. 分阶段

1. `pkg/integrity` 注册表 + image/tar 校验器（零依赖）
2. `pkg/downloader` IntegrityMode + Result 扩展 + 各下载器声明
3. `pkg/cloud` 整合（完整性判定 + 语义校验点 + 重下嵌入 + 状态/force）
4. pikpak GCID 复算（候选分块）+ ext/video ffprobe + cmd/sproxy 装配
5. 客户端 flag + API + WebUI 展示

## 10. 关键复用清单

| 能力 | 现位置 | 复用方式 |
|---|---|---|
| 图片校验 | cocom pkg/imaging/verify.go | 移植（标准库 image 解码 + bounds） |
| 视频校验 | pkg/media/ffprobe | ext/video 插件 |
| tar 遍历 | 标准库 archive/tar + compressx | pkg/integrity/tar.go |
| 注册表 | shardseal RegisterAlgorithm | pkg/integrity Register |
| 重下循环 | cloud runRetryLoop | 校验点嵌入 |
| 权威 hash | pikpak get --raw hash（GCID） | GCID 复算 |
| 转存读回校验 | cloud transfer | 已有（M3） |
