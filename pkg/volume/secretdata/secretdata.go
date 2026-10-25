// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package secretdata 是 secret_data 加密封装卷（设计 docs/designs/2026-10-01-secret-volume.md
// §6.2）：wrapper 卷，写入手动分块加密上传底层卷随机容器目录、读取经 meta 解密还原。
// 实现 sync.FS，对上层透明（OpenRead/WriteFile/Stat/Delete/MakeDir 转发）。
//
// 卷内布局（无顶层 data/meta 结构词，全随机容器）：
//
//	<secretdata根>/<randDir 5-30>/           # 一个逻辑目录 = 一个随机命名容器目录
//	      目录meta（q 标记，含逻辑 path，加密）
//	      文件meta（z 标记，含文件根信息，加密）
//	      加密分块（无标记）
//
// 寻址 `secretdata://<卷名>/<path>`；旧卷加载 = 扫描容器目录 meta → path、文件 meta →
// name 重建完整路径索引 + 按需还原（修复 F-1：逻辑路径与索引键在写路径与重启恢复一致）。
package secretdata

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // 密文块 md5（baidupcs ETag 复核/秒传基准），非安全用途
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// Options 是 SecretdataFS 的构造参数（装配层从 v.Extra 解析填充）。
type Options struct {
	// Secret 是密钥字节（来自 secrets:// 卷，装配层解析后注入；必填）。
	Secret []byte
	// Algorithm 加密算法（默认 shardseal/aes-256-gcm）。
	Algorithm string
	// Block 分块策略（默认 random 1MB-200MB）。
	Block shardseal.BlockPolicy
	// TempDir 本地临时空间（默认 os.MkdirTemp 随机目录，0700 不可预测）。
	TempDir string
	// MetaPadBytes 文件/目录 meta 加密落盘的 pad 目标基准（默认 = Block.Min）。
	// pad 目标 = MetaPadBytes + rand(MetaPadBytes)，受统一格式 R 地板（8B 长度头 196B）约束：
	// 取 max(196, 目标) 使 meta blob 与底层分块大小分布重叠，难以凭文件大小区分。
	// 装配层从 extra.meta_pad_bytes 传入（任务 6）。字节大小配置统一 sizex.ByteSize
	// （2026-10-03 用户裁决：禁止 config 裸 int64 字节字段；内部 int64 计算处显式转换）。
	MetaPadBytes sizex.ByteSize

	// PreserveMTime 是否透传原始 mtime 到底层 blob（默认 false = blob mtime 打散）。
	// 默认：底层 blob/容器 meta 文件系统 mtime = 原始 mtime + 随机偏移（0-48h，防同文件
	// 分片时间聚类特征）；PreserveMTime=true 才透传原始 mtime。逻辑层 Stat/ListDir 恒用
	// meta 内原始 mtime 排序，不受打散影响。装配层从 extra.preserve_mtime 传入。
	PreserveMTime bool

	// Dedup 是否启用整文件内容去重（**实验性，默认 false=预留能力，不实现整文件池常驻**）。
	// 方案 A 裁决：Dedup 从「可启用功能」**降级为预留**——装配层不接线（secret_register.go
	// 不解析 extra.dedup 键，生产不可达）；实现路径（writeFileDedup/卷级池/引用计数）保留
	// 为**实验代码**（features.go，未生产验证，未来由独立内容寻址子系统承接），测试框住
	// 实验代码正确性但注释明确实验性。true 时同卷内相同内容的整文件复用同一加密分块
	// （卷级内容池），meta 引用计数管理，归零物理删。跨 secret 卷共享由底层 blob 自包含
	// 保证（不必跨实例）。
	//
	// **已知边界（M-6，2026-10-05 升级）**：去重内容池键 = 整文件完整 SHA-256（**256-bit**，
	// 不再 48/64bit 截断——完整熵消除内容寻址碰撞歧义；键即 Chunks[0].OrigSHA256，重启可重建）。
	Dedup bool

	// GCInterval 周期孤儿/墓碑 GC 间隔（0=默认禁用，GC 为**可选维护工具**：仅在远程卷 /
	// 多进程场景作为孤儿兜底，不再作为默认正确性依赖；调用方显式 fs.GC() 触发或
	// >0 时 NewFS 启动后台 goroutine 周期清理）。
	GCInterval time.Duration

	// Erasure 是否启用 XOR 奇偶纠错（k-of-k+1，纯 XOR、纯 stdlib）。**实验性**：k-of-k+1
	// XOR parity，**单块**损坏/丢失恢复，未生产验证（§13.3 标注）。写入时对 ≥2 个数据分块
	// 按 max 长度补零对齐后逐字节 XOR 生成奇偶校验段（独立加密分块文件，同 blob 格式），
	// meta.Parity 记录映射；读取时某分块缺失（底层读失败）→ 按 parity ^ 其余分块恢复其
	// 明文再解密。属可选能力，默认关闭不影响既有单卷行为。
	Erasure bool

	// Targets 是多 target 复制的副本卷名列表（装配元数据；写路径把容器复制到全部
	// target——容器自包含、blob 不变；读取主 target 失败 → 回退副本。实际底层副本映射
	// 由 NewFSMultiplicas 注入的底层 FS 承担；此处记录配置名供审计/装配）。
	Targets []string

	// MaxFileBytes 单文件最大字节上限（0=不限制）。写路径**在读取全文前**按调用方传入
	// size 拦截超限文件（ErrMaxFileBytes），防大文件整读内存峰值不可控。
	//
	// 字节大小配置统一 sizex.ByteSize（2026-10-03 用户裁决：禁止 config 裸 int64 字节
	// 字段）；与 size 比较处显式 int64 转换。
	//
	// 背景（2026-10-19 更正）：写路径**已有逐块流式变体**——可 seek 源走
	// `writeFileStream`（`EncryptShardsReaderToChunks` 逐块产出 + 块级交错上传，内存 O(1 块)）；
	// 非 seek 源与 dedup/erasure 仍走内存明文变体（`EncryptShardsBytes`，峰值 1× 文件）。
	// 故超限拦截是「**内存写路径**大文件防护」的兜底，部署侧按卷容量/内存设置。
	// 注：`size<0`（未知长度）时按声明值的预检不可用，内存路径改按本上限 +1 读取并超限拒绝
	// （`readAllBounded`，2026-10-19），流式路径对未知长度直接 fail-closed（ErrUnknownSourceSize）。
	MaxFileBytes sizex.ByteSize

	// BlockletCacheBytes 是 blocklet 级明文 LRU 缓存的**字节预算**（2026-10-22 缺陷
	// 修复，见 blocklet_cache.go）。0 = 默认 64MiB；**负值 = 关闭缓存**（测试/极小内存
	// 场景显式关）。
	//
	// 为何需要：RangeSeeker 外部卷窗口 256KiB « 视频 GOP blocklet（可达 10MB）⇒ 无缓存
	// 时每窗口重拉整段密文（实测 10MB 顺序读发 120 次 Range GET，且窗口超时中断使下载
	// 内容错位）。缓存把「窗口读」与「网络读」解耦（同一 blocklet 只取一次、只解一次）。
	BlockletCacheBytes sizex.ByteSize

	// SpanCacheDir 是块密文**本机磁盘**缓存目录（2026-10-22，见 span_disk_cache.go）。
	// 空 = 关闭（**默认关闭零回归**——用户明确启用才落盘）。内层为 staging 目录风格：
	// 落 `<SpanCacheDir>/<卷桶>` 下，受 `SpanCacheBytes` 预算 + 定期清理 + 空间不足降级管束。
	SpanCacheDir string
	// SpanCacheBytes 是磁盘缓存字节预算（0 = 默认 512MiB；仅 `SpanCacheDir` 非空时生效）。
	SpanCacheBytes sizex.ByteSize
}

// ErrMaxFileBytes 是单文件超上限哨兵错误（writeFile 在读取前拦截）。
var ErrMaxFileBytes = fmt.Errorf("secretdata: 文件超过单文件大小上限")

// ErrVersionConflict 是乐观锁版本冲突哨兵错误：WriteFileIfVersion/DeleteIfVersion 传入的
// 期望版本与卷当前版本不一致（多进程写前 CAS 失败）。
var ErrVersionConflict = fmt.Errorf("secretdata: 乐观锁版本冲突")

// ErrUnknownSourceSize 报告「可 seek 源 + 未知长度（size<0）」的流式写被拒绝：分块规划需要
// 确定长度（Plan 要求 size>0），未知长度源应走内存路径（非 seek）或先确定长度。
var ErrUnknownSourceSize = fmt.Errorf("secretdata: 未知长度源不可流式加密")

// errEntryExists 是 WriteIfAbsent 哨兵（包内）：提交时刻目标 rel 已被写入（含被并发
// 者抢先）→ 回滚本写并拒绝覆盖。WriteIfAbsent 把它映射为 (false,nil)。
var errEntryExists = errors.New("secretdata: 目标条目已存在")

// metaEntry 是索引条目：逻辑文件路径 ↔ 底层容器/分块位置。
type metaEntry struct {
	size     int64
	mtime    int64  // 逻辑层 mtime（meta 内原始值，不被 blob mtime 打散影响）
	dirSeg   string // 随机容器目录名（5-30）
	metaName string // 文件 meta 加密 blob 名（含 z 标记）
	meta     *shardseal.Meta
	// dataDir 是数据分块所在的容器目录。默认 = dirSeg（自包含 blob）；去重引用文件时
	// 指向卷级去重容器（分块在该容器，meta blob 仍在 dirSeg）。"" = 与 dirSeg 相同。
	dataDir string
	// baseVersion 是本条目的乐观锁版本（来自 meta.BaseVersion，写/删 CAS 用）。
	baseVersion int64
	// phys 是本条目在**本卷**的物理落盘字节（内容分块 EncSize 之和 + meta blob 含 pad 与
	// parity 的落盘长度；去重引用条目只计本条目 meta blob，共享内容归 dedupBlob 计数）。
	// 由写路径/加载路径填充，供 TrueUsage/TrueSize 报告真值（trueusage.go）。
	phys int64
}

// dedupBlob 是去重内容池条目：一个整文件内容对应一个共享加密分块 blob 与其引用计数。
// 计数 = 当前卷内引用该 blob 的文件 meta 数；归零物理删。meta 是首个写入者的完整 Meta
// 模板（Salt/Chunks/Extra），供后续引用文件克隆（读路径复用同一 salt/key）。
type dedupBlob struct {
	name string // dedupDir 内的分块 blob 文件名（内容寻址三段哈希）
	refs int64
	meta *shardseal.Meta
	// phys 是该共享内容 blob 的物理落盘字节（内容分块 + parity；meta 归各引用条目）。
	// 卷真值里每 blob 只计一次（多引用不重复计），见 trueusage.go。
	phys int64
}

// dirMeta 是目录 meta 的加密 JSON（q 标记，记录本目录逻辑名与父目录 dir_id；目录移动/改名
// 仅重写本容器 meta 的 Name/ParentDirID 一个文件，子树零改动——目录间完全解耦）。
// 旧格式（存完整逻辑 path，耦合父目录）loadIndex fail-closed（未上线，见 decryptDirMeta）。
type dirMeta struct {
	Version     int    `json:"version"`
	Algorithm   string `json:"algorithm"`
	KDF         string `json:"kdf"`
	Type        string `json:"type"`                    // "dir"
	Name        string `json:"name"`                    // 本目录逻辑名（不含路径；根容器为空串）
	ParentDirID string `json:"parent_dir_id,omitempty"` // 父目录 dir_id（根为空）
	DirID       string `json:"dir_id"`                  // 本目录唯一 ID（base62 9 字符，与文件名中段同字符集）
	MTime       string `json:"mtime"`
}

// dirCreation 是一次写路径新建容器的记录（失败回滚按空目录回收；含叶子与祖先链）。
type dirCreation struct {
	dirPath   string // 逻辑目录
	container string // 随机容器目录名
	dmName    string // 目录 meta blob 名
}

// SecretdataFS 是透明加解密的 sync.FS 包装，底层为任意 sync.FS。
// index/dirs/dirSegs 是内存态：index 逻辑文件 → meta；dirs 逻辑目录集合；
// dirSegs 逻辑目录 → 随机容器目录名（写路径定位/创建容器，旧卷加载由目录 meta 重建）。
type SecretdataFS struct {
	inner  syncpkg.FS
	secret []byte
	opts   Options
	// algoVer 是 Options.Algorithm 解析出的已注册算法版本（NewFS fail-fast 校验；
	// 写路径 DeriveKey/EncryptShards 用，不硬编码版本——只经注册表）。
	algoVer shardseal.AlgoVersion
	temp    string
	// ownedTemp 标记 temp 目录由 NewFS 自建（os.MkdirTemp 随机目录），
	// backend.Close 负责清理；显式配置的 TempDir 不清理（调用方所有）。
	ownedTemp bool
	// stagingQuota 是**本地 staging 配额**句柄（装配层经 StagingQuotaCapable 注入，
	// 2026-10-18）：本卷的写路径会在 temp 目录落**加密分块目录 + meta blob**（≈ 明文 + pad），
	// 这些本地临时物由本卷**自管记账**（ReserveUsage/ReleaseUsage）——复用装配层既有的
	// 独立 staging Scope（`syncpkg.ApplyStagingQuota` 的 StagingQuotaCapable 分支），
	// 不另造机制；未装配（nil）= 不记账（与 gate 的零回归口径一致）。
	// stagingMu 保护注入与读（装配期注入、服务期读取）。
	stagingMu    sync.RWMutex
	stagingQuota syncpkg.StagingQuotaTracker

	// blockCache 是 blocklet 级明文 LRU 缓存（2026-10-22 缺陷修复，见 blocklet_cache.go）。
	// 无缓存（未装配/预算<=0）时 nil，读路径零回归。写/删后经 invalidatePrefix 失效对应文件
	// 条目，防陈旧明文被后续读命中。
	blockCache *blockletCache
	// spanCache 是本机**磁盘**块密文跨度缓存（2026-10-22，见 span_disk_cache.go）：staging
	// 目录 + 字节预算 + 定期清理 + 空间不足自动降级。nil = 未装配（默认关闭零回归）。
	spanCache *spanDiskCache
	// stageDL 是**窗口级缓存下载器**（2026-10-25 v2，见 stage_downloader.go）：块 blob 的
	// blocklet 按密文边界聚合为 ≤4MiB 窗口，每窗独立 Range 下载 + 独立缓存；读只下目标窗。
	// nil = 未装配（SpanCacheDir 为空/预算 0，默认关闭零回归）。
	stageDL *stageDL

	mu      sync.RWMutex
	index   map[string]*metaEntry
	dirs    map[string]struct{} // 已知逻辑目录（子目录发现/进入用）
	dirSegs map[string]string   // 逻辑目录 → 随机容器目录名
	// dirPhysTotal 是**全部目录 meta blob 的落盘字节之和**（真值报告用；见 trueusage.go）。
	// 为什么必须跟踪：目录 meta 也 pad 到 [base, 2·base)（默认 1–2MB/目录）——「每目录数百字节」
	// 的旧假设已不成立；若不计入，周期对账（TrueUsage → 卷池校准）会把刚记的目录字节**改回去**
	// （账本与磁盘漂移）。三处维护：目录 meta 写入 +、容器回收 −（列目录时顺带求和，零额外 I/O）、
	// 挂载扫描 +（Phase 1 已读每个 dir meta blob）。
	dirPhysTotal int64
	// dirIDs 是逻辑目录 → 该容器目录 meta 的 dir_id（写新容器 meta 的 parent_dir_id、
	// 目录移动改写父引用用）。
	dirIDs map[string]string
	// dirParents 是逻辑目录 → 其父逻辑目录（空目录回收/子树判定用；根无条目）。
	dirParents map[string]string
	// dirMTime 是逻辑目录 → 目录 meta 内原始 mtime（UnixNano；未知/未维护 = 0）。`dirs` 集合只表达
	// 「目录存在」，而 Stat/ListDir 的**目录条目**同样要暴露 mod_time（P2 修复，2026-10-18：此前目录
	// 条目 MTime 恒 0 ⇒ `list --json` 的目录行 mod_time=0）。维护点：加载（resolveContainerPaths）、
	// 新建（ensureContainerLocked）、目录移动（applyDirMoveLocked 键迁移）、空目录回收（prune 删除）。
	dirMTime map[string]int64
	// keyCache 是派生密钥小容量 LRU 缓存（(salt)→key；并行 loadIndex 缓解重复 scrypt，
	// 去重克隆 meta 共享 salt）。
	keyCache *deriveCache
	// loadGate 是 loadIndex 并行派生并发上界（Imp-C）：并发 = maxParallelLoads(alg)，随
	// KDF 档位**真实 scrypt 内存**（high 128MiB、standard 16MiB、low 4MiB）自适应，使
	// 并发×单次派生内存 ≤ maxLoadMemBudget（512MiB），无界并行不炸内存、低档不被拖慢。
	loadGate chan struct{}

	// volVersion 是卷级乐观锁基版本（loadIndex 初始化为现存 meta.BaseVersion 最大值，
	// 每次写/删 +1）。等版本写路径并发安全；跨进程 CAS 用 WriteFileIfVersion。
	volVersion int64
	// usage 是卷级已用字节（写入累计 - 删除释放；loadIndex 按 meta size 累加）。
	usage int64
	// dedupPool 是去重内容池（key=整文件明文 SHA-16hex，value=blob+引用计数）。
	// 仅 opts.Dedup 时有值；调用方持 s.mu。
	dedupPool map[string]*dedupBlob
	// dedupDir 是卷级去重分块容器（仅 opts.Dedup 时存在；懒创建在首个去重写路径）。
	dedupDir string

	// writesInFlight 是正在途写入计数（写路径开始持锁 +1、defer endWrite −1）。
	// GC 借此判定没有在途写（避免把未提交分块当孤立删掉），且 GC 全程持锁扫描，
	// 故在途写一旦归零、GC 持锁期间不会有新写插入（check-then-act 窗口被锁关闭）。
	writesInFlight int64
	// gcCancel 取消后台 GC goroutine（backend.Close / loadIndex 失败路径调用停止）。
	gcCancel context.CancelFunc
}

// attachStageDownloader 装配整块 staging 下载器（SpanCacheDir 非空时；失败仅告警）。
// 目录落 `<SpanCacheDir>/stage`，单块上限复用 SpanCacheBytes（0 = 默认 512MiB）。
// **只在外部/网盘卷有意义**：本地卷底层 OpenRead 已是本地读，整块 staging 无收益。
func attachStageDownloader(fs *SecretdataFS, opts Options) {
	if opts.SpanCacheDir == "" {
		return
	}
	if lv, ok := fs.inner.(syncpkg.LocalVolume); ok && lv.IsLocalVolume() {
		return // 本地卷无需 staging（避免多一份磁盘占用）
	}
	budget := opts.SpanCacheBytes.Bytes()
	if budget == 0 {
		budget = defaultSpanCacheBytes
	}
	fs.stageDL = newStageDL(filepath.Join(opts.SpanCacheDir, "stage"), budget, fs)
}

// attachSpanDiskCache 装配本机磁盘缓存（SpanCacheDir 非空时；失败仅告警不阻断读路径）。
func attachSpanDiskCache(fs *SecretdataFS, opts Options) {
	if opts.SpanCacheDir == "" {
		return
	}
	budget := opts.SpanCacheBytes.Bytes()
	if budget == 0 {
		budget = defaultSpanCacheBytes
	}
	cache, cerr := newSpanDiskCache(filepath.Join(opts.SpanCacheDir, "secretdata"), budget)
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "secretdata: 磁盘缓存装配失败（继续直连远端）: %v\n", cerr)
		return
	}
	fs.spanCache = cache
	cache.startSweeper(context.Background(), defaultSpanCacheSweepInterval)
}

// prepareTempDir 解析并创建临时目录：未配置时用 os.MkdirTemp（随机名 + 0700，避免
// os.TempDir() 下固定可预测的公开可写路径，Sonar go:S5445）；显式配置的同样收紧为 0700。
// 返回 owned=true 表示目录由本 FS 自建（Close 时清理）。
func prepareTempDir(opts *Options) (owned bool, err error) {
	if opts.TempDir == "" {
		dir, mkErr := os.MkdirTemp("", "sproxy-secretdata-")
		if mkErr != nil {
			return false, fmt.Errorf("secretdata: 创建默认临时目录失败: %w", mkErr)
		}
		opts.TempDir = dir
		owned = true
	}
	if mkErr := os.MkdirAll(opts.TempDir, 0o700); mkErr != nil {
		return false, fmt.Errorf("secretdata: 创建临时目录 %s 失败: %w", opts.TempDir, mkErr)
	}
	return owned, nil
}

// NewFS 构造 secretdata FS。inner：底层卷 FS（secretdata 根视图）；opts：参数。
func NewFS(inner syncpkg.FS, opts Options) (*SecretdataFS, error) {
	if opts.Secret == nil {
		return nil, fmt.Errorf("secretdata: 密钥未提供（Options.Secret 必填）")
	}
	if opts.Algorithm == "" {
		opts.Algorithm = shardseal.AlgorithmName
	}
	// 创建路径 fail-fast 解析算法版本：未知/未注册算法立刻报错，不静默回落默认
	// （写路径按此版本加密、读路径按 meta 内版本解密，secretdata 只经注册表）。
	algoVer, err := shardseal.ResolveAlgorithm(opts.Algorithm)
	if err != nil {
		return nil, fmt.Errorf("secretdata: 加密算法 %q 未注册（fail-fast）: %w", opts.Algorithm, err)
	}
	// 取已注册算法的完整定义（含 KDF 档位参数），loadGate 并发上界随档位派生内存自适应。
	alg, ok := shardseal.AlgoByVersion(algoVer)
	if !ok {
		return nil, fmt.Errorf("secretdata: 加密算法版本 %d 未注册（fail-fast）", algoVer)
	}
	if opts.Block.Min <= 0 || opts.Block.Max <= 0 {
		opts.Block = defaultBlockPolicyFor(inner)
	}
	// Erasure 与 Dedup 组合未支持（去重文件走卷级单 blob 池，无可纠的 k 分块）：
	// 显式互斥 fail-closed，避免组合下用户以为有纠错而实际没有。
	if opts.Erasure && opts.Dedup {
		return nil, fmt.Errorf("secretdata: Erasure 与 Dedup 互斥（纠错仅常规写路径，组合未支持）")
	}
	ownedTemp, terr := prepareTempDir(&opts)
	if terr != nil {
		return nil, terr
	}
	// **启动清理陈旧明文残留**（Step 3，2026-10-18）：Step 2 起读路径**按需解密、不落任何临时
	// 文件** ⇒ temp 目录里任何 `decrypt-*` 都只能是**升级前崩溃**遗留的明文 ⇒ 启动时 best-effort
	// 清除，避免明文长期驻留磁盘。写路径的临时物是**密文**（分块目录），前缀不同、不会被误删。
	cleanupStaleReadTemp(opts.TempDir)
	fs := &SecretdataFS{
		inner: inner, secret: opts.Secret, opts: opts, algoVer: algoVer, temp: opts.TempDir, ownedTemp: ownedTemp,
		index: map[string]*metaEntry{}, dirs: map[string]struct{}{}, dirSegs: map[string]string{},
		dirIDs: map[string]string{}, dirParents: map[string]string{},
		dirMTime: map[string]int64{}, keyCache: newDeriveCache(64),
		loadGate:   make(chan struct{}, maxParallelLoads(&alg)),
		dedupPool:  map[string]*dedupBlob{},
		blockCache: newBlockletCache(opts.BlockletCacheBytes.Bytes()),
	}
	// 本机磁盘缓存（可选；目录非空才装配，失败仅告警不阻断——缓存是尽力而为设施）。
	attachSpanDiskCache(fs, opts)
	// 整块 staging 下载器（同开关；SpanCacheDir 非空才启用）。
	attachStageDownloader(fs, opts)
	if err := fs.loadIndex(context.Background()); err != nil {
		return nil, err
	}
	if opts.GCInterval > 0 {
		// 后台 GC 用可取消 ctx：backend.Close 负责 cancel，避免 ticker 无法停止。
		ctx, cancel := context.WithCancel(context.Background())
		fs.gcCancel = cancel
		fs.startGC(ctx)
	}
	return fs, nil
}

// backend 是 ExternalBackend 实现。
type backend struct{ fs *SecretdataFS }

func (b *backend) FS() syncpkg.FS { return b.fs }

// OpenURL 实现 registry.URLResolver：解析 `secretdata://<卷>/<路径>` → 解密返回内容。
// 客户端转存后经 ResolveURL(secretdata://<卷>/<rel>) 取用加密卷文件。
func (b *backend) OpenURL(ctx context.Context, urlStr string) (io.ReadCloser, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("secretdata: OpenURL 解析 %q 失败: %w", urlStr, err)
	}
	if !strings.EqualFold(u.Scheme, "secretdata") {
		return nil, fmt.Errorf("secretdata: 不支持 scheme %q（需 secretdata://）", u.Scheme)
	}
	rel := strings.TrimPrefix(u.Path, "/")
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return nil, fmt.Errorf("secretdata: OpenURL 缺路径（需 secretdata://<卷>/<路径>）")
	}
	return b.fs.OpenRead(ctx, rel)
}
func (b *backend) Close() error {
	if b.fs.gcCancel != nil {
		b.fs.gcCancel() // 停止后台 GC goroutine
	}
	if b.fs.stageDL != nil {
		_ = b.fs.stageDL.close() // 窗口级缓存下载器（保留缓存文件，由清理纪律管）
	}
	if b.fs.spanCache != nil {
		b.fs.spanCache.close() // 停止磁盘缓存 sweeper 并落盘索引
	}
	// 仅清理 NewFS 自建的随机临时目录；显式配置的 TempDir 归调用方所有，不碰。
	if b.fs.ownedTemp {
		return os.RemoveAll(b.fs.temp)
	}
	return nil
}

var _ registry.ExternalBackend = (*backend)(nil)

// NewBackend 构造 secretdata 的 registry.ExternalBackend。底层 target FS 由装配层注入
// （NewFS 负责构造）；v.Extra 只消费 target 元数据用于错误文案与类型判定。
func NewBackend(ctx context.Context, v volume.Volume, inner syncpkg.FS, opts Options) (registry.ExternalBackend, error) {
	if v.Type == "" || v.Type == volume.TypeLocal {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 类型 %q 不是外部 secretdata 卷", v.Name, v.Type)
	}
	if inner == nil {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 底层 FS 未注入", v.Name)
	}
	fs, err := NewFS(inner, opts)
	if err != nil {
		return nil, fmt.Errorf("secretdata backend: 卷 %q FS 构造失败: %w", v.Name, err)
	}
	return &backend{fs: fs}, nil
}

// NewBackendMultiplicas 构造多副本 secretdata 的 registry.ExternalBackend（生产多 target
// 装配）。primary 是主底层 FS，replicas 是副本底层 FS（写入复制到全部 target、读主失败
// 回退副本、删除 Multi 删）。replicas 为空退化为单卷（等价 NewBackend）。底层 FS 由装配
// 层解析 extra.targets（多 local root）注入。
func NewBackendMultiplicas(ctx context.Context, v volume.Volume, primary syncpkg.FS, replicas []syncpkg.FS, opts Options) (registry.ExternalBackend, error) {
	if v.Type == "" || v.Type == volume.TypeLocal {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 类型 %q 不是外部 secretdata 卷", v.Name, v.Type)
	}
	if primary == nil {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 主底层 FS 未注入", v.Name)
	}
	fs, err := NewFSMultiplicas(primary, replicas, opts)
	if err != nil {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 多 target FS 构造失败: %w", v.Name, err)
	}
	return &backend{fs: fs}, nil
}

// ---- sync.FS 接口 ----

// splitDirPrefix 把 rel 规范化为目录前缀（含尾部 /；根为 ""）。
func splitDirPrefix(rel string) string {
	p := strings.Trim(rel, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// entryOf 构造逻辑条目（正斜杠相对路径）。
// entryOf 构造一个条目。mtime 是**逻辑层原始 mtime（UnixNano）**——上层
// pkg/server/files_service 直接把它当 ModTime（`list --json`/`meta` 的 mod_time 来源），
// 不填会让加密卷的 mod_time 恒为 0（P2 修复，2026-10-18）。
// 目录条目暂不携带 mtime（目录 mtime 不在内存索引 dirs 集合里，属已知残余，见 ListDir 注释）。
func entryOf(rel, seg string, size, mtime int64, isDir bool) syncpkg.Entry {
	p := path.Join(rel, seg)
	return syncpkg.Entry{Name: seg, Path: p, Size: size, MTime: mtime, IsDir: isDir}
}

// ListDir 列出逻辑 rel 目录下的直接子项（透明子目录可发现/进入）。
// 索引只存完整逻辑路径文件键；这里对每个路径把下一段作为子项聚合，
// 跨段前缀呈现为目录条目（审查 F-2：ListDir 永不返回子目录）。
// collectChildSegments 聚合子条目：**目录集合优先**（`dirs` 是真实目录的事实源），再用 index
// （文件键首段）补齐——**按名字跨源去重**。
//
// 为什么必须跨源去重（用户实测 2026-10-14：secretdata 卷 `list 1/` 输出两个 `2/`）：同一个子目录
// 会**同时**出现在两处——
//   - `dirs` 含 `1/2`（目录键，来自 dir meta 加载）⇒ `collectDirSegments` 产出目录 `2`；
//   - `index` 含 `1/2/f.bin`（文件键）⇒ `collectIndexSegments` 由首段推出**目录** `2`。
//
// 两个函数各自内部去重（`seenDirs`）但**互不知道对方** ⇒ 简单 append 必然重复。这里改为
// 「先目录集合、后 index 补齐 + 共享 seen」：index 推出的目录仅在 `dirs` 缺失时才作为兜底出现
// （容器/目录 meta 缺失的部分加载场景仍可见），已由 `dirs` 覆盖的名字不再重复。
func collectChildSegments(paths map[string]*metaEntry, dirs map[string]struct{}, dirMTime map[string]int64, prefix, rel string, entryInfo func(*metaEntry) (size, mtime int64)) []syncpkg.Entry {
	out := collectDirSegments(dirs, dirMTime, prefix, rel)
	seen := make(map[string]bool, len(out))
	for _, e := range out {
		seen[e.Name] = true
	}
	for _, e := range collectIndexSegments(paths, prefix, rel, entryInfo) {
		if seen[e.Name] {
			continue // 已由目录集合贡献（同名不重复；目录是事实源，优先）
		}
		seen[e.Name] = true
		out = append(out, e)
	}
	return out
}

// collectIndexSegments 从 index 文件键聚合：首段为目录则目录条目，否则文件条目。
func collectIndexSegments(paths map[string]*metaEntry, prefix, rel string, entryInfo func(*metaEntry) (size, mtime int64)) []syncpkg.Entry {
	var out []syncpkg.Entry
	seenDirs := map[string]bool{}
	for p := range paths {
		if !strings.HasPrefix(p, prefix) || len(p) <= len(prefix) {
			continue
		}
		rest := p[len(prefix):]
		if before, _, has := strings.Cut(rest, "/"); has {
			if !seenDirs[before] {
				out = append(out, entryOf(rel, before, 0, 0, true))
				seenDirs[before] = true
			}
			continue
		}
		size, mtime := entryInfo(paths[p])
		out = append(out, entryOf(rel, rest, size, mtime, false))
	}
	return out
}

// collectDirSegments 从 dirs 目录键聚合：只贡献目录条目（空/字节目录可见性）。
func collectDirSegments(dirs map[string]struct{}, dirMTime map[string]int64, prefix, rel string) []syncpkg.Entry {
	var out []syncpkg.Entry
	seenDirs := map[string]bool{}
	for d := range dirs {
		if !strings.HasPrefix(d, prefix) || len(d) <= len(prefix) {
			continue
		}
		rest := d[len(prefix):]
		if before, _, has := strings.Cut(rest, "/"); has {
			if !seenDirs[before] {
				out = append(out, entryOf(rel, before, 0, 0, true))
				seenDirs[before] = true
			}
			continue
		}
		if !seenDirs[rest] {
			out = append(out, entryOf(rel, rest, 0, dirMTime[path.Join(rel, rest)], true))
			seenDirs[rest] = true
		}
	}
	return out
}

func (s *SecretdataFS) ListDir(ctx context.Context, rel string) ([]syncpkg.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := splitDirPrefix(rel)
	out := collectChildSegments(s.index, s.dirs, s.dirMTime, prefix, rel, func(e *metaEntry) (int64, int64) {
		if e == nil {
			return 0, 0
		}
		return e.size, e.mtime
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir // 目录在前
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *SecretdataFS) Stat(ctx context.Context, rel string) (*syncpkg.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := strings.TrimPrefix(rel, "/")
	if _, ok := s.dirs[key]; ok {
		return &syncpkg.Entry{Name: path.Base(rel), Path: rel, Size: 0, MTime: s.dirMTime[key], IsDir: true}, nil
	}
	e, ok := s.index[key]
	if !ok {
		return nil, nil
	}
	return &syncpkg.Entry{Name: path.Base(rel), Path: rel, Size: e.size, MTime: e.mtime, IsDir: false}, nil
}

func (s *SecretdataFS) OpenRead(ctx context.Context, rel string) (io.ReadCloser, error) {
	return s.openRead(ctx, strings.TrimPrefix(rel, "/"))
}

// OpenRangeRead 随机读取逻辑文件 rel 的 [offset, offset+size) 区间：按 meta 定位块 →
// 定位 blocklet。**下载粒度 = 整块（chunk），解密粒度 = blocklet 段**（M-3 纠偏）：
// readChunkBlob 对含目标范围的块整块 io.ReadAll 读入内存（底层无 range-read 时下载
// 整块密文），仅解密到含目标范围的 blocklet 段（视频关键帧随机访问：省去**解密**非目标
// blocklet 的 CPU，但**下载**仍是整块粒度——200MB 块随机访问某 4MB 段仍需下载 200MB，
// 设计 §6.2 与旧注释「只下载 blocklet 段」对下载粒度夸大，已按实现写明）。返回覆盖
// 区间的明文读流。区间越出文件 fail-closed。
func (s *SecretdataFS) OpenRangeRead(ctx context.Context, rel string, offset, size int64) (io.ReadCloser, error) {
	key := strings.TrimPrefix(rel, "/")
	s.mu.RLock()
	e, ok := s.index[key]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("secretdata: 文件 %q 不存在: %w", rel, os.ErrNotExist)
	}
	if e.meta == nil {
		return nil, fmt.Errorf("secretdata: %q 是目录", rel)
	}
	// **本函数是既有「按需随机读」原语**（blocklet 级定位 + 最小下载 + erasure 感知），
	// 由 `sync.RangeSeeker`（通用 RangeReader→io.ReadSeeker 适配器，4MiB 窗口）与 DAV/Range
	// 消费方共用 ⇒ **不重复实现定位/解密**。
	if size < 0 || offset < 0 || size > e.meta.Original.Size-offset {
		return nil, fmt.Errorf("secretdata: 区间 [%d,%d) 越出文件（size=%d）", offset, offset+size, e.meta.Original.Size)
	}
	if size == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	// ctx 早检查（2026-10-21）：取消/超时的请求**不得**构造读流——即使惰性流会在首次 Read
	// 才做 I/O，提前拒绝可让调用方立即拿到可识别的 context.Canceled（且不白建状态）。
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("secretdata: 随机读被取消（不构造读流）: %w", cerr)
	}
	// 复用 fileLevelKey（salt 解码 + 一次派生）——**唯一**的文件级密钥派生入口，避免各处重复。
	keyBytes, salt, kerr := s.fileLevelKey(e)
	if kerr != nil {
		return nil, kerr
	}
	// **惰性流式（2026-10-21 缺陷修复）**：旧实现 `rangeReadBytes` 把窗口内全部分块
	// 先下载+解密成 []byte 再包流 ⇒ 首帧要等整个 4MiB 窗口物化（网盘场景数秒）。
	// 现改为 `rangeStream`（逐块「读→解→交」，首字节只需第一块）——crypto 层段级解密
	// 能力（DecryptBlockletSegmentStandalone/decryptBlockletAt）本就支持，这里只做流式接线。
	return newRangeStream(ctx, s, e, keyBytes, salt, key, offset, size), nil
}

// readChunkBlob 读取底层单个分块 blob（仅含目标区间的块被下载）。去重引用文件的数据
// 分块在卷级去重容器（e.dataDir），非去重在 e.dirSeg。
func (s *SecretdataFS) readChunkBlob(ctx context.Context, e *metaEntry, ci shardseal.ChunkInfo) ([]byte, error) {
	rc, rerr := s.inner.OpenRead(ctx, path.Join(s.dataSeg(e), ci.FileName))
	if rerr != nil {
		return nil, fmt.Errorf("secretdata: 读底层分块 %s 失败: %w", ci.FileName, rerr)
	}
	blob, berr := io.ReadAll(rc)
	rc.Close()
	if berr != nil {
		return nil, fmt.Errorf("secretdata: 读底层分块 %s 失败: %w", ci.FileName, berr)
	}
	return blob, nil
}

// dataSeg 返回条目的数据分块容器（去重引用文件的 dataDir；否则 dirSeg）。
func (s *SecretdataFS) dataSeg(e *metaEntry) string {
	if e.dataDir != "" {
		return e.dataDir
	}
	return e.dirSeg
}

// decryptRangeBlocklet 按 meta 段描述跳读只解密含目标区间的单个 blocklet，返回
// [offset,end)∩blocklet 的明文切片；blocklet meta 与 blob 自描述不一致 fail-closed。
func decryptRangeBlocklet(keyBytes, salt, blob []byte, blockOffset int64, bl shardseal.BlockletInfo, offset, end int64) ([]byte, error) {
	plain, got, derr := shardseal.DecryptBlockletAt(keyBytes, salt, blob, blockOffset, bl)
	if derr != nil {
		return nil, fmt.Errorf("secretdata: 解密 blocklet（offset=%d）失败: %w", bl.Offset, derr)
	}
	if got.Offset != bl.Offset || int64(len(plain)) != bl.Size {
		return nil, fmt.Errorf("secretdata: blocklet meta 与 blob 不一致（offset=%d size=%d，got %+v len=%d）", bl.Offset, bl.Size, got, len(plain))
	}
	segStart := max(offset, bl.Offset)
	segEnd := end
	if blEnd := bl.Offset + bl.Size; segEnd > blEnd {
		segEnd = blEnd
	}
	return plain[segStart-bl.Offset : segEnd-bl.Offset], nil
}

func (s *SecretdataFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, rel string, r io.Reader, size, mtime int64) error {
	return s.writeFile(ctx, strings.TrimPrefix(rel, "/"), r, size, mtime, -1)
}

// IsLocalVolume 封装卷本地性自述（syncpkg.LocalVolume 能力接口，用户裁定 2026-10-05）：
// **封装卷必须实现并委派被封装的底层卷**——「只要有一层是外部就是外部」。底层卷
// 实现 `IsLocalVolume()==true`（内部，如 LocalFS）→ 本卷内部（走用户配额）；否则
// 默认外部（远程，容量/配额由底层自管）。**不靠类型名硬编码**——secretdata 封装 s3
// 即外部、封装本地即内部，层层判断自动正确。
func (s *SecretdataFS) IsLocalVolume() bool {
	if rv, ok := s.inner.(syncpkg.LocalVolume); ok {
		return rv.IsLocalVolume()
	}
	// 底层未自述 → 默认外部（registry 域内未实现接口的都是外部卷后端）。
	return false
}

// WriteIfAbsent 实现 pkg/sync.WriteIfAbsent（W1/W3：转存目标须唯一、拒绝静默覆盖）。
// 存在性判定与写入在**同一把锁**内（commitEntry 持锁时校验 ifAbsent），无 TOCTOU 窗口；
// 并发双写者恰好一胜一败——败者在提交时刻发现 rel 已被他人写入，回滚本次上传的
// blob 并返回 (false,nil)，不留孤儿、不覆盖他人。目标已存在返回 (false,nil) 不覆盖。
func (s *SecretdataFS) WriteIfAbsent(ctx context.Context, rel string, r io.Reader, size, mtime int64) (bool, error) {
	rel = strings.TrimPrefix(rel, "/")
	// 快速路径：目标已存在则直接拒绝（免做加密/上传开销；最终以提交时刻为准）。
	s.mu.RLock()
	_, exists := s.index[rel]
	s.mu.RUnlock()
	if exists {
		return false, nil
	}
	if err := s.writeFileIfAbsent(ctx, rel, r, size, mtime); err != nil {
		if errors.Is(err, errEntryExists) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// WriteFileIfVersion 带乐观锁的写入：expected < 0 表示不校验；expected ≥ 0 时只有
// 卷当前版本 == expected 才写入（多进程 CAS），否则返回 ErrVersionConflict。
// 成功后该文件 meta.BaseVersion 与卷版本 +1。
//
// **乐观锁标注（方案 A）**：多进程**预留 API**（远程卷/多进程共享场景）；单进程路径内建
// 版本（writeFile 落盘 meta.BaseVersion = newVer，单实例写路径天然版本单调，无需外部
// expected 参与）。未验证的并发多写者不在单实例承诺内。
func (s *SecretdataFS) WriteFileIfVersion(ctx context.Context, rel string, r io.Reader, size, mtime, expected int64) error {
	return s.writeFile(ctx, strings.TrimPrefix(rel, "/"), r, size, mtime, expected)
}

// Rename 移动/重命名逻辑路径（task10 目录解耦）。
//
// 目录移动语义（用户需求 4/5，方案 B）：目录 meta 存 {name, parent_dir_id} 父引用模型，
// 移动 = 仅改根容器 meta 的 Name/ParentDirID（一个文件重写），子树内其它目录/文件 blob
// 零改动（子树父引用指向本 dir_id 不变）→ 最理想仅移动相关文件即可用。目标必须为已存在
// 目录下的新名（或根）；失败不落半态（先校验目标合法）。
// 文件移动语义（跨目录、basename 不变）：文件 blob 自包含 → 物理复制分块 + 文件 meta 到
// 目标容器（内容零改动）→ 删源副本 → 索引迁移，重启后在新容器路径解析。文件改名
// （basename 变）仍返回错误，引导走 delete+write。
func (s *SecretdataFS) Rename(ctx context.Context, from, to string) error {
	f := strings.Trim(strings.TrimPrefix(from, "/"), "/")
	t := strings.Trim(strings.TrimPrefix(to, "/"), "/")
	if f == "" || t == "" {
		return fmt.Errorf("secretdata: Rename 路径不能为空")
	}
	if f == t {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 先校验 to 不存在（fail-closed，不落半态），并禁止移入自身子树（成环）。
	if _, ok := s.index[t]; ok {
		return fmt.Errorf("secretdata: 目标 %q 已是文件", t)
	}
	if _, ok := s.dirs[t]; ok {
		return fmt.Errorf("secretdata: 目标 %q 已是目录", t)
	}
	if strings.HasPrefix(t+"/", f+"/") {
		return fmt.Errorf("secretdata: 不能把 %q 移入其自身子树", f)
	}
	if _, isDir := s.dirs[f]; isDir {
		return s.renameDirLocked(ctx, f, t)
	}
	if _, ok := s.index[f]; ok {
		return s.renameFileLocked(ctx, f, t)
	}
	return fmt.Errorf("secretdata: 待移动 %q 不存在", f)
}

// Delete 删除逻辑文件：**即时物理删（方案 A 默认路径，删即释放）**——持锁一次删完
// meta + 全部分块（+ parity + 去重引用递减），不再写墓碑。usage 与去重引用计数在删除时
// 同步释放。openRead 与 Delete 同持 s.mu 门禁在途读取一致性（无真正并发读半态）。
//
// 墓碑语义收敛（方案 A 裁决）：墓碑仅在「多进程共享卷 + GC 启用」场景保留（跨进程并发
// 读需要分块保护）；单实例即时删后无需墓碑。本路径**不再生成墓碑**；GC 作为可选维护
// 工具仍保留对磁盘既有墓碑 meta / 孤儿分块的清理能力（见 features.go），但正确性不再
// 依赖 GC——删即释放。
func (s *SecretdataFS) Delete(ctx context.Context, rel string) error {
	return s.deleteFile(ctx, strings.TrimPrefix(rel, "/"), -1)
}

// DeleteIfVersion 带乐观锁的删除：expected ≥ 0 时须匹配该条目 baseVersion，否则
// ErrVersionConflict。expected < 0 不校验。
func (s *SecretdataFS) DeleteIfVersion(ctx context.Context, rel string, expected int64) error {
	return s.deleteFile(ctx, strings.TrimPrefix(rel, "/"), expected)
}

// deleteFile 实现删除：即时物理删 + 移出索引 + 账本/版本一次性提交（merge）。
// 方案 A：不再写墓碑——先在锁内物理删尽条目全部 blob（removeVersionMeta：meta + 部分
// 块 + parity；去重文件 release 池引用 + 删本容器 meta），再一次性提交内存态（移出索引、
// usage -= size、volVersion++）。物理删为 best-effort（失败残留孤儿仅留空位，可选 GC 上收；
// 无墓碑 → 无「先写后删失败漂台账本」路径，账本与索引原子切换，重试幂等不重复扣）。
func (s *SecretdataFS) deleteFile(ctx context.Context, key string, expected int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.index[key]
	if !ok {
		return nil // 幂等：不存在视为已删
	}
	if expected >= 0 && expected != e.baseVersion {
		return fmt.Errorf("secretdata: 删除版本冲突：条目版本 %d，调用方期望 %d: %w", e.baseVersion, expected, ErrVersionConflict)
	}
	// 即时物理删（删即释放）：meta + 全部分块 + parity 一次删完（best-effort；去重文件
	// 释放去重池引用 + 删本容器 meta）。
	s.removeVersionMeta(ctx, key, e)
	// 失效该文件全部 blocklet 缓存（防陈旧明文被后续读命中）。
	s.invalidateBlockletCache(e)
	// 一次性提交内存态：移出索引、扣 usage、推进版本、回收空目录（原子，无半态）。
	delete(s.index, key)
	s.volVersion++
	s.usage -= e.size
	if s.usage < 0 {
		s.usage = 0
	}
	s.pruneEmptyDirsLocked(ctx, parentDirOf(key))
	return nil
}

// RemoveDir 删除**空**目录（非空 → 错误；子树递归删由调用方先清空）。
//
// 为什么必须单独提供：卷内**目录不参与文件索引**（`s.index` 只含文件条目）⇒ `Delete` 对目录键
// 是 no-op（历史上空目录只能靠「最后一个文件被删」时的 `pruneEmptyDirsLocked` 顺带回收）⇒
// 显式删空目录必须走本方法：删掉该目录自己的容器（目录 meta + 残留文件）并向上回收变空的祖先。
// 这是 `syncpkg.FS` 之外的**可选能力**（Delete 的删除语义只对文件成立），调用方按类型断言使用。
func (s *SecretdataFS) RemoveDir(ctx context.Context, rel string) error {
	key := strings.TrimPrefix(rel, "/")
	if key == "" {
		return fmt.Errorf("secretdata: 不允许删除卷根目录")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	container, ok := s.dirSegs[key]
	if !ok {
		return nil // 幂等：目录不存在（含纯虚拟中间目录）
	}
	if s.dirHasContentLocked(key) {
		return fmt.Errorf("secretdata: 目录 %q 非空，拒绝删除", key)
	}
	s.removeEmptyContainerLocked(ctx, container, key)
	s.pruneEmptyDirsLocked(ctx, parentDirOf(key))
	s.volVersion++
	return nil
}

// MakeDir 创建逻辑空目录：递归创建随机容器目录 + 目录 meta（q 标记，{name, parent_dir_id}
// 父引用模型），不再提若干层（旧 inner.MakeDir(rel) 会向底层泄漏目录名，违背目录名保密）。
func (s *SecretdataFS) MakeDir(ctx context.Context, rel string) error {
	key := strings.TrimPrefix(rel, "/")
	// 目录 mtime = 创建时刻（与 LocalFS 新建目录语义一致，P2 修复 2026-10-18）：此前传 0 ⇒ 目录
	// meta 的 mtime 为空串 ⇒ Stat/ListDir 的目录条目 mod_time 恒 0（`list --json` 目录行不可读）。
	_, _, err := s.ensureContainer(ctx, key, time.Now().UnixNano())
	return err
}

var _ syncpkg.FS = (*SecretdataFS)(nil)

// ---- 内部实现 ----

// writeCtx 是一次写路径的共享执行上下文（S107 收敛）：常规（writeFileEncrypted）与去重
// （writeFileDedup*/commitDedupEntry）写路径共用的入参组，避免逐参数传递（与 fileOp /
// backupWorker 的 Go S107 收敛范式一致）。字段语义见 writeFile：sv 为 CAS 起始卷版本、
// newVer 为写内锁内一次性分配的乐观锁版本（== sv+1）并在写路径全程固定；expected 为
// 调用方期望卷版本（<0 不校验）。data 为明文（内存变体）；流式路径 data 为 nil、改用
// reader 流式加密（size 为文件大小，commit 的 usage/审计用 size）。
type writeCtx struct {
	rel       string        // 逻辑相对路径（索引键）
	data      []byte        // 明文（Imp 内存明文变体；流式路径 nil）
	size      int64         // 文件大小（流式路径使用；内存变体 = len(data)）
	reader    io.Reader     // 流式明文源（内存变体 nil；可 seek 时走 EncryptShardsReader）
	container string        // 文件 meta 所在容器名
	created   []dirCreation // 本次新建容器（失败回滚删新留旧）
	mtime     int64         // 逻辑 mtime（原始值；底层 blob 写入经 blobMTime 打散）
	sv        int64         // 起始卷版本（CAS 起始校验）
	newVer    int64         // 本次写路径分配的乐观锁版本 == sv+1
	expected  int64         // 乐观锁期望版本（<0 不校验）
	ifAbsent  bool          // WriteIfAbsent 语义：仅目标不存在才提交（commit 持锁时判定）
	// sourcePath 是**本地可重读源路径**（本地文件 / 调用方已暂存路径；"" = 不可重读，如管道 /
	// 内存 reader）。块级交错丢弃本地产物后仍需能从源重算 ⇒ 回退链（docs/roadmap.md ①-附 §2）
	// 以它判定能否降级为内存交付。
	sourcePath string
}

// writeFile：全程登记在途写（GC 以 writesInFlight 判定无在途写才清理）→ 乐观锁 CAS
// （起始 expected 校验 + 提交前再校验，原子推进版本）→ 去重或常规分块加密 → 上传
// → 全部成功后才原子切换索引（覆盖写中途失败删新留旧，F-2）。mtime 打散仅在底层
// blob 写入应用，逻辑层 entry.mtime 恒为原始 mtime。
func (s *SecretdataFS) writeFile(ctx context.Context, rel string, r io.Reader, size, mtime, expected int64) error {
	return s.writeFileOpt(ctx, rel, r, size, mtime, expected, false)
}

// writeFileIfAbsent 仅目标不存在才提交（WriteIfAbsent 语义）的写路径。
func (s *SecretdataFS) writeFileIfAbsent(ctx context.Context, rel string, r io.Reader, size, mtime int64) error {
	return s.writeFileOpt(ctx, rel, r, size, mtime, -1, true)
}

// writeFileOpt 是 writeFile 的实现体；ifAbsent=true 时在 commit 持锁时刻判定目标不存在
// 才提交（原子，无 TOCTOU），目标已存在则回滚本次上传并返回 errEntryExists。
func (s *SecretdataFS) writeFileOpt(ctx context.Context, rel string, r io.Reader, size, mtime, expected int64, ifAbsent bool) error {
	// 单文件上限拦截（读取全文前，按调用方 size 判定；0=不限制）。Imp-2 写路径为
	// 内存明文变体（峰值 1× 文件），大文件用上限兜底，避免把任意大小文件整读进内存。
	if s.opts.MaxFileBytes > 0 && size > int64(s.opts.MaxFileBytes) {
		return fmt.Errorf("%w: 大小 %d，上限 %d", ErrMaxFileBytes, size, int64(s.opts.MaxFileBytes))
	}
	// 流式优先：可 seek（io.Seeker/io.ReaderAt）且非 dedup/erasure（这两者需全文内存）→
	// 流式加密（逐块 ReadAt + 流式哈希，内存峰值 O(单块上限)）；否则读全文走现状内存变体。
	if canStream(r) && !s.opts.Dedup && !s.opts.Erasure {
		// U1 fail-closed（2026-10-19）：流式路径按调用方声明的 size 规划分块（Plan 要求 size>0），
		// size<0（chunked/未知长度）会被规划器以「空文件不可分块（size=-1）」拒绝 ⇒ 文案误导。
		// 此处提前拒绝并给出准确指引（未知长度源请走内存路径：非 seek 源或先确定长度）。
		if size < 0 {
			return fmt.Errorf("%w（size=%d；可 seek 源未知长度 ⇒ 无法规划分块）："+
				"请提供确定长度，或让源不可 seek 以走内存路径", ErrUnknownSourceSize, size)
		}
		warnStreamSizeMismatch(rel, r, size)
		return s.writeFileStreamEntry(ctx, rel, r, size, mtime, expected, ifAbsent)
	}
	// 上限兜底（P2 修复，2026-10-19）：上面的预检按**声明 size** 判定，`size<0`（chunked / 未知
	// 长度）时它**无法生效** ⇒ 内存路径的 ReadAll 会随流无界增长（大文件整读 ⇒ OOM）。
	// 配置了 MaxFileBytes（>0）时按**上限 +1** 读，超限即 fail-closed（与流式路径对未知长度
	// 的拒绝同一口径）；未配置（0=不限）时保持既有「读全」语义
	// （TestWriteFile_NonSeekableReadsAll 显式锁定，不得回归）。
	data, err := readAllBounded(r, size, int64(s.opts.MaxFileBytes))
	if err != nil {
		return err
	}
	// **size 不匹配告警**（2026-10-18 用户裁定；零行为变更）：内存路径按设计「读全」（非 seek 源
	// 回退 ReadAll，忽略调用方声明 size，见 TestWriteFile_NonSeekableReadsAll 锁定），而流式路径
	// 按声明 size 读。两条路径各自对**自己的调用方类别**安全（HTTP 上传 = 客户端声明 size + 非 seek
	// ⇒ 走本路径；服务端侧来源 = 可 seek + size 精确 ⇒ 走流式），但调用方**谎报/算错** size 时
	// 两条路径结果不同 ⇒ 记录显著日志 + 指标，便于定位（尤其配额预留是按声明 size 做的）。
	if size >= 0 && int64(len(data)) != size {
		slog.Warn("secretdata: 明文实际长度与调用方声明 size 不一致（内存写路径按实际长度落盘；"+
			"配额预留按声明 size 记账，随后按实测足迹结算）",
			"rel", rel, "declared_size", size, "actual_size", len(data))
	}
	// 登记在途写入 + 起始 CAS（expected ≥ 0 须匹配卷当前版本）+ **一次性分配本条目的
	// 乐观锁版本 newVer = sv+1**（Imp-1 修复：写前在锁内就定版本，注意它在写路径全程
	// 固定，落盘 meta.BaseVersion 与内存 e.baseVersion/卷版本三者恒等——跨进程 CAS 与
	// 重启后 loadIndex 恢复的卷版本以此为准；不再由 commitEntry 锁内重算 +1，避免
	// 「落盘 meta 恒 0」导致的跨重启 CAS 空心化）。
	sv, newVer, unlock, cerr := s.beginWrite(expected)
	if cerr != nil {
		return cerr
	}
	defer unlock()

	parent := parentDirOf(rel)
	container, created, err := s.ensureContainer(ctx, parent, mtime)
	if err != nil {
		return err
	}
	// 注（2026-10-18 审核）：两条写路径对「调用方声明 size」的口径**有意不同**——流式路径按 size
	// 读（多余内容丢弃），内存路径**读全**（`TestWriteFile_NonSeekableReadsAll` 显式锁定：非 seek
	// 源回退 io.ReadAll ⇒ 忽略声明 size）。曾尝试统一为「按 size 截断」⇒ 该用例红 ⇒ 按「不回归既有
	// 锁定场景」回滚，改列为**已知残余**（见 docs/secret-volume.md §9.1）。
	wc := writeCtx{
		rel: rel, data: data,
		// size 必须显式填：commitEntry/覆盖写清理用 wc.size 记账（s.usage += wc.size、覆盖写
		// 再 -= prev.size）。此前内存路径漏填 ⇒ 每次内存写都 +0（Usage() 少计、覆盖写还会把
		// 已计入的部分减掉）✗（P2 修复，2026-10-18；流式路径一直有填）。
		size:      int64(len(data)),
		container: container, created: created,
		mtime: mtime, sv: sv, newVer: newVer, expected: expected, ifAbsent: ifAbsent,
	}
	if s.opts.Dedup && len(data) > 0 {
		return s.writeFileDedup(ctx, wc)
	}
	return s.writeFileEncrypted(ctx, wc)
}

// readAllBounded 读取整份明文：声明长度已知（size>=0）或未配置上限（maxBytes<=0）时等价于
// io.ReadAll；**未知长度（size<0）且配置了上限**时按上限 +1 读，超限返回 ErrMaxFileBytes
// （与写路径按声明 size 的预检同一哨兵）——防 chunked/未知长度上传绕过 MaxFileBytes 后
// 把任意大小内容整读进内存（P2 修复，2026-10-19）。
func readAllBounded(r io.Reader, size, maxBytes int64) ([]byte, error) {
	if size >= 0 || maxBytes <= 0 {
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("secretdata: 读明文失败: %w", err)
		}
		return data, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("secretdata: 读明文失败: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: 未知长度源超过上限 %d（size 未声明 ⇒ 预检不可用）", ErrMaxFileBytes, maxBytes)
	}
	return data, nil
}

// warnStreamSizeMismatch 在流式写路径前对「声明 size 与源文件实际大小不符」告警（**行为不变**，
// 仅可观测性）：流式路径按声明 size 读（多余内容丢弃）、内存路径按实际长度落盘，两条路径口径
// **有意不同**（见 writeFileOpt 注），但调用方谎报/算错 size 时此前流式路径**静默截断**且无日志
// （P2 修复，2026-10-19）。抽成函数亦降低 writeFileOpt 的认知复杂度（gocognit 门禁）。
func warnStreamSizeMismatch(rel string, r io.Reader, size int64) {
	n, ok := sourceFileSize(r)
	if !ok || n == size {
		return
	}
	slog.Warn("secretdata: 流式写路径声明 size 与源文件实际大小不一致（流式按声明 size 读、"+
		"多余内容丢弃；内存路径按实际长度落盘）",
		"rel", rel, "declared_size", size, "source_size", n)
}

// sourceFileSize 返回源的**实际字节数**（仅本地常规文件，与 replayableSourcePath 同一判据）：
// 供流式写路径对「声明 size 与实际不符」告警（内存路径按实际长度落盘，两条路径口径有意不同）。
func sourceFileSize(r io.Reader) (int64, bool) {
	f, ok := r.(*os.File)
	if !ok {
		return 0, false
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	return st.Size(), true
}

// canStream 判定 r 是否可流式加密（io.Seeker / io.ReaderAt 之一）。
func canStream(r io.Reader) bool {
	if _, ok := r.(io.Seeker); ok {
		return true
	}
	_, ok := r.(io.ReaderAt)
	return ok
}

// writeFileStreamEntry 流式入口：登记在途 + CAS + 建容器 + 调 writeFileStream。
func (s *SecretdataFS) writeFileStreamEntry(ctx context.Context, rel string, r io.Reader, size, mtime, expected int64, ifAbsent bool) error {
	sv, newVer, unlock, cerr := s.beginWrite(expected)
	if cerr != nil {
		return cerr
	}
	defer unlock()
	parent := parentDirOf(rel)
	container, created, err := s.ensureContainer(ctx, parent, mtime)
	if err != nil {
		return err
	}
	wc := writeCtx{
		rel: rel, size: size, reader: r, container: container, created: created,
		mtime: mtime, sv: sv, newVer: newVer, expected: expected, ifAbsent: ifAbsent,
		sourcePath: replayableSourcePath(r),
	}
	return s.writeFileStream(ctx, wc)
}

// beginWrite 登记在途写入 + 起始 CAS（expected ≥ 0 须匹配卷当前版本）+ 一次性分配
// 乐观锁版本 newVer = sv+1（Imp-1 修复：写前锁内定版本；落盘 meta.BaseVersion 与
// 内存 e.baseVersion/卷版本恒等）。返回 (sv, newVer, unlock, err)。
func (s *SecretdataFS) beginWrite(expected int64) (int64, int64, func(), error) {
	s.mu.Lock()
	sv := s.volVersion
	if expected >= 0 && expected != sv {
		s.mu.Unlock()
		return 0, 0, nil, fmt.Errorf("secretdata: 版本冲突：卷当前 %d，调用方期望 %d: %w", sv, expected, ErrVersionConflict)
	}
	newVer := sv + 1
	s.writesInFlight++
	s.mu.Unlock()
	return sv, newVer, func() { s.endWrite() }, nil
}

// writeFileStream 流式写路径（可 seek 源）：逐块加密 + 流式哈希 + **块级交错上传**，内存峰值
// O(单块上限)、本地临时峰值 O(1 块)。与 writeFileEncrypted 同构（临时目录 → 加密 → 上传 →
// commit），差别在：
//   - `wc.reader` 可 seek ⇒ 走 EncryptShardsReaderToChunks（逐块产出即上传，不再先把整份分块落 temp）；
//   - meta 需要**全部分块**的信息（哈希/偏移/parity）⇒ 必须留到最后，以内存态写出（不落盘）。
//
// video-keyframe 模式由 EncryptShardsReaderToChunks 内部按 seek 判定（非 keyframe 才流式；
// keyframe 亦走流式——parseOnce 经 io.ReaderAt 扫描关键帧表，与逐块加密兼容，上层**无**
// ReadAll 回退分支，见 shardseal_stream_test.go:TestEncryptShardsReader_VideoKeyframeParity）。
func (s *SecretdataFS) writeFileStream(ctx context.Context, wc writeCtx) (retErr error) {
	// 审计：encrypt 阶段 span（与 writeFileEncrypted 对称；仅 ctx 携带 audit logger 时
	// 生效，nil-safe no-op）。明文 = wc.size，密文 = 分块物理字节（physBytesOf 同 commitEntry
	// 口径）。流式路径此前缺此埋点——转存到加密卷（WriteIfAbsent 走流式）审计无 encrypt 行。
	encSpan := audit.BeginSpan(ctx, "encrypt")
	var cipherBytes int64
	defer func() {
		if retErr != nil {
			encSpan.Fail(retErr)
			return
		}
		// applyKV 把 "bytes"→Bytes，"encrypted"/"cipher_bytes"/"algorithm" 进 Meta。
		encSpan.End("bytes", wc.size, "encrypted", true, "cipher_bytes", cipherBytes,
			"algorithm", s.auditAlgoName())
	}()
	policy, perr := s.blockletPolicyFor(wc.rel)
	if perr != nil {
		s.rollbackWrite(ctx, nil, wc.created)
		return fmt.Errorf("secretdata: blocklet 模式选型失败: %w", perr)
	}
	// 本地 staging 配额：**只按单个分块上界**预留（交错上传 ⇒ 不再随文件大小线性增长），
	// 本地临时物不可用时按回退链评估降级（见 beginInterleavedUpload）。
	iu, ierr := s.beginInterleavedUpload(ctx, wc, policy)
	if ierr != nil {
		s.rollbackWrite(ctx, nil, wc.created)
		return ierr
	}
	defer iu.close()
	// 流式加密 + **逐块交付**（sink）：每块产出即上传并丢弃本地副本，只保留块名/哈希等最小
	// 状态给 meta（EncryptionResult.Meta）⇒ 本地峰值 O(1 块)。整文件 SHA-256、parity（本路径
	// 不可达，见下）、dedup、审计 span、staging 配额预留/释放均由本路径保持。
	out, cerr := shardseal.EncryptShardsReaderToChunks(wc.reader, wc.size, sanitizeName(wc.rel), s.secret,
		policy, s.algoVer, s.interleavedChunkSink(ctx, iu, wc))
	if cerr != nil {
		s.rollbackWrite(ctx, iu.uploaded, wc.created)
		return fmt.Errorf("secretdata: 流式分块加密失败: %w", cerr)
	}
	out.Meta.Original.MTime = mtimeString(wc.mtime)
	out.Meta.BaseVersion = wc.newVer
	if s.opts.Erasure {
		// **流式路径不支持 Erasure**（P2 修复，2026-10-18）：parity 需要**明文分块**做 XOR，而流式
		// 路径的明文已随加密流丢弃（不驻留内存）⇒ 旧代码传 nil 会让 chunkPlainTexts 对任意非空
		// 分块报「区间越出明文 0」（错误信息误导）。当前 `writeFileOpt` 以 `!s.opts.Erasure` 为分流
		// 条件 ⇒ 本分支**不可达**；此处显式 fail-closed 是为了「将来放宽分流条件」时给出**准确**
		// 错误，而不是静默写出坏 parity。
		s.rollbackWrite(ctx, iu.uploaded, wc.created)
		return fmt.Errorf("secretdata: 流式写路径不支持 Erasure（parity 需明文分块）；" +
			"装配层须保证 Erasure 卷走内存写路径")
	}
	metaName, metaBlob, merr := s.encryptMetaBlob(out.Meta)
	if merr != nil {
		s.rollbackWrite(ctx, iu.uploaded, wc.created)
		return merr
	}
	// meta 为**内存态**（无本地路径，设计如此）：以内存 blob 直接上传（无需本地临时物）。
	mt := s.blobMTime(wc.mtime)
	metaPath := path.Join(wc.container, metaName)
	// **先登记后上传**（P2 修复，2026-10-19）：内层可能「写成功但复核失败」返回错误
	// （如 baidupcs 上传后 ETag 复核不匹配 ⇒ ErrTransient）⇒ 远端 blob 已存在；若成功后
	// 才登记，回滚集合漏掉它 ⇒ 远端孤儿。块名含随机 salt/nonce ⇒ 提前登记即使上传未真正
	// 发生，回滚删除也只是 no-op（不会误删他人内容）。
	iu.uploaded = append(iu.uploaded, metaPath)
	if werr := s.uploadBlob(ctx, metaPath, metaBlob, mt); werr != nil {
		s.rollbackWrite(ctx, iu.uploaded, wc.created)
		return fmt.Errorf("secretdata: 上传 meta 失败: %w", werr)
	}
	cipherBytes = physBytesOf(out.Meta, int64(len(metaBlob)), false)
	return s.commitEntry(ctx, wc, &metaEntry{
		size: wc.size, mtime: wc.mtime, dirSeg: wc.container, metaName: metaName, meta: out.Meta,
		phys: cipherBytes,
	}, iu.uploaded)
}

// chunkDelivery 是**块级交错上传**的交付形态（回退链见 docs/roadmap.md ①-附 §2）。
type chunkDelivery int

const (
	// deliverLocalPath：密文落本地临时文件后经 WithStagedLocalPath 交内层——内层跨其 md5 预扫 /
	// 上传 / 重试 / 秒传刷新全部复用同一路径（零重算、零二次拷贝），上传后立即丢弃 ⇒ 本地峰值 O(1 块)。
	deliverLocalPath chunkDelivery = iota
	// deliverInMemory：本地临时物不可用（staging 配额不足 / 临时目录不可用）但源可重读 ⇒ 直接把
	// 内存密文交内层（不落本地产物；产物随时可由源重算）⇒ 写仍成功（「空间不足回退」）。
	deliverInMemory
)

// interleavedUpload 是一次块级交错上传的执行句柄：交付形态 + 临时目录 + staging 配额释放 +
// 已上传路径表（失败回滚删新留旧）。
type interleavedUpload struct {
	mode     chunkDelivery
	tmp      string
	release  func()
	uploaded []string
	// chunkMD5/chunkCRC32 是当前待传密文块的整块 md5/crc32（sink 收 blob 时算好，
	// uploadLocalChunk 交给内层 UploadHints——baidupcs 整块免重扫）。单块维度（每次覆盖）。
	chunkMD5   string
	chunkCRC32 string
}

// close 释放 staging 配额预留并删除本地临时目录（幂等，写路径 defer 调用）。
func (iu *interleavedUpload) close() {
	if iu.tmp != "" {
		_ = os.RemoveAll(iu.tmp)
	}
	if iu.release != nil {
		iu.release()
	}
}

// beginInterleavedUpload 准备块级交错上传（含**回退链**，docs/roadmap.md ①-附 §2）：
//
//	① 产物在场（本轮加密即将产出分块密文）且本地可落 ⇒ 本地路径交付（常规形态，本地峰值 O(1 块)）；
//	② 本地产物不可用（staging 配额不足 / 临时目录不可用）但**源可重读**（本地文件 / 已暂存路径）
//	   ⇒ 内存交付（不落本地产物；该产物随时可由源重算）⇒ 写仍成功，不直接失败；
//	③ 都不可行（源不可重读：管道 / 内存 reader 等一次性源）⇒ fail-closed，返回明确错误——
//	   绝不静默降级为「不可重试且不可校验的写」。
//
// 注：staging **配额**拒绝（策略门）同样走本链（②/③）——配额拒绝在源可重读时不再等于写失败，
// 但源不可重读时仍 fail-closed（既有锁定语义：防本地磁盘打满）。
func (s *SecretdataFS) beginInterleavedUpload(ctx context.Context, wc writeCtx, policy shardseal.BlockPolicy) (*interleavedUpload, error) {
	release, rerr := s.stagingReserveInterleaved(ctx, wc.size, policy)
	tmp := ""
	if rerr == nil {
		if t, terr := os.MkdirTemp(s.temp, "chunks-*"); terr == nil {
			tmp = t
		} else {
			slog.Warn("secretdata: 本地临时分块目录不可用，按回退链评估降级", "rel", wc.rel, "error", terr)
			release()
			release, rerr = nil, terr
		}
	}
	if tmp != "" {
		return &interleavedUpload{mode: deliverLocalPath, tmp: tmp, release: release}, nil
	}
	if !wc.sourceReplayable() {
		return nil, fmt.Errorf("secretdata: 本地临时物不可用（%s）且源不可重读（无法从源重算产物；"+
			"交错上传需要本地产物在场或源可重读）: %w", stagingDegradeReason(rerr), rerr)
	}
	// **降级必须可观测**（P2 修复，2026-10-19）：此前降级为内存交付时**无任何日志**，且
	// ctx 取消/配额拒绝被笼统描述成「配额不足」。此处显式 Warn + 区分原因。
	slog.Warn("secretdata: 本地 staging 不可用，按回退链降级为内存交付（源可重读 ⇒ 写仍成功）",
		"rel", wc.rel, "reason", stagingDegradeReason(rerr), "error", rerr)
	return &interleavedUpload{mode: deliverInMemory, release: func() {}}, nil
}

// stagingDegradeReason 把 staging 预留/临时目录失败归类为**准确**的原因文案（供日志/错误
// 使用）：区分「调用方 ctx 取消或超时」与「配额拒绝」，避免把 ctx 取消说成配额不足。
func stagingDegradeReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "调用方上下文已取消"
	case errors.Is(err, context.DeadlineExceeded):
		return "调用方上下文超时"
	case errors.Is(err, syscall.ENOSPC):
		return "本地临时目录磁盘满"
	case errors.Is(err, syncpkg.ErrStagingQuotaExceeded):
		return "staging 配额不足"
	default:
		return "本地临时目录不可用"
	}
}

// sourceReplayable 报告本写路径的源能否在产物丢失后**重读**（本地文件 / 调用方已暂存路径）。
func (wc writeCtx) sourceReplayable() bool { return wc.sourcePath != "" }

// interleavedChunkSink 返回逐块交付内层的 sink（块级交错）：
//   - deliverLocalPath：密文落 tmp/<name> → 经 WithStagedLocalPath 交内层 → **立即删除**本地副本
//     （只留块名/哈希等最小状态给 meta）⇒ 本地峰值 O(1 块)；删除必须在**内层返回之后**——
//     内层的 md5 预扫 / 上传 / 重试 / 秒传刷新都要重读该文件（生命周期契约：stagedPath 只读）。
//   - deliverInMemory：直接把内存密文交内层（无本地副本）。
func (s *SecretdataFS) interleavedChunkSink(ctx context.Context, iu *interleavedUpload, wc writeCtx) shardseal.ChunkSink {
	mt := s.blobMTime(wc.mtime)
	return func(name string, blob []byte) error {
		// P1（2026-10-21 完整收益）：sink 已持有完整密文块（内存）——顺带算整块 md5/crc32
		// 一次（baidupcs 需要它们做 ETag 复核基准与秒传参数；此处省去 baidupcs 的整块重扫）。
		iu.chunkMD5 = md5HexOf(blob)
		iu.chunkCRC32 = strconv.FormatUint(uint64(crc32.ChecksumIEEE(blob)), 10)
		if iu.mode == deliverLocalPath {
			local := filepath.Join(iu.tmp, name)
			if werr := os.WriteFile(local, blob, 0o600); werr != nil {
				return fmt.Errorf("secretdata: 落本地分块 %s 失败: %w", name, werr)
			}
			defer func() { _ = os.Remove(local) }() // 交错：此刻之后本地不再保留该块
			return s.uploadLocalChunk(ctx, wc.container, name, local, mt, chunkHash{md5: iu.chunkMD5, crc32: iu.chunkCRC32}, &iu.uploaded)
		}
		p := path.Join(wc.container, name)
		// **先登记后上传**（P2 修复，2026-10-19；同 uploadLocalChunk）：内层「写成功但复核
		// 失败」时必须能从回滚集合里找到该 blob。
		iu.uploaded = append(iu.uploaded, p)
		if werr := s.uploadBlob(ctx, p, blob, mt); werr != nil {
			return fmt.Errorf("secretdata: 上传分块 %s 失败: %w", name, werr)
		}
		return nil
	}
}

// endWrite 结束一段在途写入（defer 调用，成功/失败统一释放计数）。
func (s *SecretdataFS) endWrite() {
	s.mu.Lock()
	if s.writesInFlight > 0 {
		s.writesInFlight--
	}
	s.mu.Unlock()
}

// writeFileEncrypted 常规（非去重）路径：分块加密 → 上传分块 + meta → 原子切换索引。
func (s *SecretdataFS) writeFileEncrypted(ctx context.Context, wc writeCtx) (err error) {
	// 审计：encrypt 阶段 span（仅 ctx 携带 audit logger 时生效；nil-safe no-op）。
	// 明密文对比 + 密文字节：明文 = len(data)，密文 = 临时分块目录字节和（见下）。
	encSpan := audit.BeginSpan(ctx, "encrypt")
	var cipherBytes int64
	defer func() {
		if err != nil {
			encSpan.Fail(err)
			return
		}
		// applyKV 把 "bytes"→Bytes，"encrypted"/"cipher_bytes"/"algorithm" 进 Meta。
		encSpan.End("bytes", int64(len(wc.data)), "encrypted", true, "cipher_bytes", cipherBytes,
			"algorithm", s.auditAlgoName())
	}()
	// 本地 staging 配额：写 temp 前预留、结束后释放（自管记账，见 stagingReserve 注释）。
	releaseStaging, sres := s.stagingReserve(ctx, wc.size)
	if sres != nil {
		s.rollbackWrite(ctx, nil, wc.created)
		return sres
	}
	defer releaseStaging()
	tmp, err := os.MkdirTemp(s.temp, "chunks-*")
	if err != nil {
		s.rollbackWrite(ctx, nil, wc.created)
		return fmt.Errorf("secretdata: 创建临时分块目录失败: %w", err)
	}
	defer os.RemoveAll(tmp)

	policy, perr := s.blockletPolicyFor(wc.rel)
	if perr != nil {
		s.rollbackWrite(ctx, nil, wc.created)
		return fmt.Errorf("secretdata: blocklet 模式选型失败: %w", perr)
	}
	out, cerr := encryptContent(wc.data, tmp, s.secret, policy, wc.rel, s.metaPadTarget(), s.algoVer)
	if cerr != nil {
		s.rollbackWrite(ctx, nil, wc.created)
		return fmt.Errorf("secretdata: 分块加密失败: %w", cerr)
	}
	// 密文体积 = 临时目录内数据分块字节和（不含 meta，meta 单独加密上传）。
	cipherBytes = sumDirFileBytes(tmp)
	// 逻辑层 mtime 记入 meta.Original.MTime（EncryptShards 用临时文件 ModTime=now，
	// 非调用方 mtime）；meta 名锚定内容 → 改名后重新加密。
	out.Meta.Original.MTime = mtimeString(wc.mtime)
	// 写前指定本条目的乐观锁版本（Imp-1 修复：落盘 meta.BaseVersion = newVer，与
	// commitEntry 内存 e.baseVersion/卷版本恒等）。encryptMetaBlob 在 marshal 时读
	// 该字段，故落盘 meta 携带真实版本——重启 loadIndex 后 volVersion = max(磁盘
	// BaseVersion)，跨进程 CAS 与跨重启预期版本校验真正成立。
	out.Meta.BaseVersion = wc.newVer
	// 纠错（Erasure=true 且 ≥2 分块）：先生成 parity 段并记录 meta.Parity，再加密 meta。
	uploaded := []string{}
	if s.opts.Erasure && len(out.Meta.Chunks) > 1 {
		if eerr := s.writeErasureParity(ctx, wc.container, tmp, out, wc.data, wc.mtime, &uploaded); eerr != nil {
			s.rollbackWrite(ctx, uploaded, wc.created)
			return eerr
		}
	}
	metaName, metaBlob, merr := s.encryptMetaBlob(out.Meta)
	if merr != nil {
		s.rollbackWrite(ctx, uploaded, wc.created)
		return merr
	}
	uploaded = append(uploaded, path.Join(wc.container, metaName))
	if uerr := s.uploadChunks(ctx, wc.container, tmp, wc.mtime, out.ChunkNames, &uploaded); uerr != nil {
		s.rollbackWrite(ctx, uploaded, wc.created)
		return uerr
	}
	mt := s.blobMTime(wc.mtime)
	// 内存态 meta 经 uploadBlob（2026-10-21：顺带算 md5/crc32 经 UploadHints，内层免整块重扫）。
	if werr := s.uploadBlob(ctx, path.Join(wc.container, metaName), metaBlob, mt); werr != nil {
		s.rollbackWrite(ctx, uploaded, wc.created)
		return fmt.Errorf("secretdata: 上传 meta 失败: %w", werr)
	}
	return s.commitEntry(ctx, wc, &metaEntry{
		size: int64(len(wc.data)), mtime: wc.mtime, dirSeg: wc.container, metaName: metaName, meta: out.Meta,
		phys: physBytesOf(out.Meta, int64(len(metaBlob)), false),
	}, uploaded)
}

// auditAlgoName 返回本卷加密算法的注册标识（含档位后缀，如 shardseal/aes-256-gcm、
// -high、-low），供审计 Meta 的 algorithm 字段（公文明密文对比埋点的算法档位）。从
// s.algoVer 经注册表读权威名；注册表异常回落 opts.Algorithm（NewFS 恒把空值归一为
// 已注册名，该回落理论不可达，仅防御）。
func (s *SecretdataFS) auditAlgoName() string {
	if alg, ok := shardseal.AlgoByVersion(s.algoVer); ok {
		return alg.Name
	}
	return s.opts.Algorithm
}

// sumDirFileBytes 递归统计 dir 下所有常规文件字节和（审计密文体积用；目录不存在/不可读显式
// 返回 0——加密失败路径本就由其它分支分类）。
func sumDirFileBytes(dir string) (total int64) {
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// commitEntry 原子提交索引并推进 volVersion + usage。提交前再持锁校验 CAS：
// expected ≥0 须 volVersion==sv（否则回滚已上传 blob 并返回 ErrVersionConflict，让并发
// 同 expected 双写只能一胜一败）；expected<0 不校验、以写路径分配的 newVer 定型版本
// （last-write-wins）。newVer 由 writeFile 在锁内一次性分配（== sv+1）并在写路径全程
// 固定——commit 直接采用它，保证内存 e.baseVersion == 落盘 meta.BaseVersion == 卷版本
// 恒等（Imp-1 修复：不再锁内 +1 重算致落盘版本漂移）。
// commitLocked 是**锁内提交**的单一实现（P2 收敛，2026-10-19）：主写路径、去重命中、去重 miss
// 三处此前各自抄一遍「CAS + 安装条目 + usage 记账 + 旧版本清理」，任何语义修正都需改三处、漏改
// 即静默偏差。调用方**不得持锁**（本函数自行加锁）。
//
//   - usageBytes：计入 s.usage 的字节（主写路径 = wc.size；去重路径 = len(wc.data)）；
//   - preInstall：锁内、CAS 通过后、安装条目前的额外动作（去重池登记；nil = 无）；
//   - onFail：CAS 失败/目标已存在时的清理（回滚本次已上传 blob；**必须**用有界清理，见
//     rollbackWrite）。
//
// **旧版本清理必须在锁内**（P1 修复，2026-10-18）：removeVersionMeta → chunkSetStillReferenced
// **遍历 s.index**，其文档明确「调用方须持 s.mu」（Delete 路径正是如此）。此前主路径在
// s.mu.Unlock() **之后**调用 ⇒ 与其它 goroutine 的索引写入并发 ⇒ Go 运行时
// **fatal error: concurrent map iteration and map write**（不可 recover，进程级崩溃）——
// 已由确定性用例 TestConcurrentOverwrite_NoIndexDataRace 实证（修复前必红）。删除 I/O 放锁内与
// Delete 路径一致；**并发读语义不变**：在途读旧条目的 openRead 早已在 RLock 内取到条目、随后
// 无锁读分块 ⇒「要么读到新内容、要么报错、绝不返回混合/陈旧内容」的既有承诺不受影响。
// 附带收益：删除判定与执行相对索引变更**原子化** ⇒ 关闭「并发 CopyFile 在判定之后、删除之前
// 采纳旧分块集合」的窗口。
func (s *SecretdataFS) commitLocked(ctx context.Context, wc writeCtx, e *metaEntry, usageBytes int64, preInstall func(), onFail func()) error {
	s.mu.Lock()
	if wc.ifAbsent {
		// WriteIfAbsent：目标已存在（含本次写期间被并发者抢先提交）→ 回滚本写上传、
		// 返回 errEntryExists（调用方映射为 (false,nil)），绝不覆盖他人条目。
		if _, ok := s.index[wc.rel]; ok {
			s.mu.Unlock()
			onFail()
			return errEntryExists
		}
	}
	if wc.expected >= 0 && s.volVersion != wc.sv {
		s.mu.Unlock()
		onFail()
		return versionConflictErr(wc.sv, wc.expected)
	}
	if preInstall != nil {
		preInstall()
	}
	prev := s.index[wc.rel]
	s.index[wc.rel] = e
	e.baseVersion = wc.newVer
	addDirKeysLocked(s.dirs, wc.rel)
	s.volVersion = wc.newVer
	s.usage += usageBytes
	if prev != nil {
		s.usage -= prev.size
		if s.usage < 0 {
			s.usage = 0
		}
		// 覆盖写：旧条目物理删，缓存一并失效（防旧明文被后续读命中）。
		s.removeVersionMeta(ctx, wc.rel, prev)
		s.invalidateBlockletCache(prev)
	}
	s.mu.Unlock()
	return nil
}

// commitEntry 主写路径提交（薄封装：走单一实现 commitLocked，见其文档的锁内清理理由）。
func (s *SecretdataFS) commitEntry(ctx context.Context, wc writeCtx, e *metaEntry, uploaded []string) error {
	return s.commitLocked(ctx, wc, e, wc.size, nil, func() {
		s.rollbackWrite(ctx, uploaded, wc.created)
	})
}

// blobMTime 返回底层 blob 文件系统 mtime：默认 = 原始 mtime + 随机偏移（0-48h，防同文件
// 分片时间聚类）；PreserveMTime=true 或 mtime≤0 时透传原值。逻辑层 mtime 不受影响。
func (s *SecretdataFS) blobMTime(mtime int64) int64 {
	if s.opts.PreserveMTime || mtime <= 0 {
		return mtime
	}
	return mtime + shardseal.RandN(int64(48*time.Hour))
}

// uploadChunks 逐块上传到容器目录（分块密文已在 tmp 目录内），并把已上传路径追加到
// uploaded（供失败回滚删新留旧）。blob 写入 mtime 应用打散（blobMTime）。
func (s *SecretdataFS) uploadChunks(ctx context.Context, container, tmp string, mtime int64, chunkNames []string, uploaded *[]string) error {
	mt := s.blobMTime(mtime)
	for _, cn := range chunkNames {
		if err := s.uploadLocalChunk(ctx, container, cn, filepath.Join(tmp, cn), mt, chunkHash{}, uploaded); err != nil {
			return err
		}
	}
	return nil
}

// uploadBlobTracked 上传**内存 blob**并**先登记后上传**（登记进调用方的回滚集合；调用方失败时
// 统一 rollbackWrite）。与 uploadLocalChunk / interleavedChunkSink 同口径——**单一机制**
// （roadmap ①-附残余 4 提出的 uploadAndTrack 收敛，P2 修复 2026-10-19）。
func (s *SecretdataFS) uploadBlobTracked(ctx context.Context, uploaded *[]string, p string, data []byte, mt int64) error {
	*uploaded = append(*uploaded, p)
	return s.uploadBlob(ctx, p, data, mt)
}

// uploadAndCleanupOnFail 上传**内存 blob**，失败时立即按**有界清理 ctx** 删除（「无调用方回滚
// 集合」场景的单一机制）：内层可能「写成功但复核失败」返回错误（远端 blob 已存在）⇒ 不删即
// **永久孤儿**；blob 名含随机 salt/padding（不复用）⇒ 未真正上传时删除只是 no-op（不误删他人内容）。
func (s *SecretdataFS) uploadAndCleanupOnFail(ctx context.Context, p string, data []byte, mt int64) error {
	if werr := s.uploadBlob(ctx, p, data, mt); werr != nil {
		s.rollbackWrite(ctx, []string{p}, nil)
		return werr
	}
	return nil
}

// retainPoolEntry 增加去重池引用（与 unrefPoolEntry 对称；调用方须持 s.mu）。同卷复制/去重
// 命中都会让新条目与源**共享同一池 blob** ⇒ 必须 +1，否则任一方删除即可能归零、池 blob 被物理
// 删除而另一方悬空（P2 修复，2026-10-19：此前 CopyFile 既丢 dataDir 也不加引用）。
func (s *SecretdataFS) retainPoolEntry(mm *shardseal.Meta) {
	if !s.opts.Dedup || mm == nil || len(mm.Chunks) == 0 {
		return
	}
	key := mm.Chunks[0].OrigSHA256
	if b, ok := s.dedupPool[key]; ok && b.name == mm.Chunks[0].FileName {
		b.refs++
	}
}

// uploadLocalChunk 上传**一个已落本地的密文分块**（分块/blob 通用）到 container，并把已上传
// 路径追加到 uploaded。
//
// 两条既有保证（不得削弱）：
//   - **流式上传**（2026-10-18 性能修复）：以本地文件为源直接交给内层（内存 O(1)，不整块驻留）；
//   - **复用调用方暂存路径**（既有 staging 架构，`syncpkg.StagedPathReuse` 的 ctx 传递形态）：
//     本地路径经 ctx 交给支持该能力的内层（如 baidupcs）⇒ 内层直接消费该路径（跨其
//     md5 预扫 / 上传 / 重试 / 秒传刷新全部复用同一文件），不再落第二份 staging。
//     生命周期契约（pkg/sync/stagedpath.go）：stagedPath **只读**——本函数返回前调用方不得删除它
//     （秒传刷新要重读它），删除一律在返回后进行。
func (s *SecretdataFS) uploadLocalChunk(ctx context.Context, container, name, local string, mt int64, ch chunkHash, uploaded *[]string) error {
	f, oerr := os.Open(local)
	if oerr != nil {
		return fmt.Errorf("secretdata: 打开本地分块 %s 失败: %w", name, oerr)
	}
	size, serr := f.Seek(0, io.SeekEnd)
	if serr == nil {
		_, serr = f.Seek(0, io.SeekStart)
	}
	if serr != nil {
		f.Close()
		return fmt.Errorf("secretdata: 取本地分块 %s 长度失败: %w", name, serr)
	}
	wctx := syncpkg.WithStagedLocalPath(ctx, local)
	// 通用上传提示（跨后端中性）：RandomContent 标记随机新建内容（baidupcs 识别为
	// RandomBlob → 跳 pre-Stat；其它后端未知即忽略）；MD5 为 sink 算好的整块密文 md5
	// （baidupcs 免整块重扫——外层已算，内层首次计算即复用，零重复计算）。
	wctx = syncpkg.WithUploadHints(wctx, &syncpkg.UploadHints{
		RandomContent: true,
		Size:          size,
		MD5:           ch.md5,
		CRC32:         ch.crc32,
		Path:          local,
	})
	dst := path.Join(container, name)
	// **先登记后上传**（P2 修复，2026-10-19）：内层可能「写成功但复核失败」返回错误（如
	// baidupcs 上传后 ETag 复核不匹配）⇒ 远端 blob 已存在；成功后登记会漏掉它 ⇒ 远端孤儿。
	// 块名含随机 salt/nonce ⇒ 上传未真正发生时回滚删除只是 no-op（不误删他人内容）。
	*uploaded = append(*uploaded, dst)
	werr := s.inner.WriteFile(wctx, syncpkg.LogicalKey{}, dst, f, size, mt)
	f.Close()
	if werr != nil {
		return fmt.Errorf("secretdata: 上传分块 %s 失败: %w", name, werr)
	}
	return nil
}

// replayableSourcePath 返回源的**本地可重读路径**（"" = 不可重读）。
//
// 「可重读」= 本次调用返回后仍能按**路径重新打开**同一内容（本地文件 / 调用方已暂存路径）：
// 只有这样的源才能在产物不可用时重算（重读 + 重加密），也才能支撑块级交错丢弃产物（见
// docs/roadmap.md ①-附 §2 回退链）。管道 / 内存 reader 等一次性源返回 ""。
// sourcePathProvider 是「包装了本地可重读源」的 reader 暴露路径的**结构化约定**（双方各自
// 声明接口，避免 pkg/volume 反向依赖 pkg/files）：限速包装（pkg/files.rateLimitReadAtReader）
// 会透传它 ⇒ 限速开启时块级交错的回退链②（源可重读 ⇒ 降级内存交付）仍然可用。
type sourcePathProvider interface{ SourcePath() string }

func replayableSourcePath(r io.Reader) string {
	if sp, ok := r.(sourcePathProvider); ok {
		if p := sp.SourcePath(); p != "" {
			return p
		}
	}
	f, ok := r.(*os.File)
	if !ok {
		return ""
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "" // 管道/设备/未知：不可按路径重读
	}
	name := f.Name()
	if name == "" || strings.HasPrefix(name, "|") {
		return ""
	}
	return name
}

// rollbackTimeout 是回滚清理（删已上传 blob + 回收空容器）的硬上限：回滚不能随调用方
// 取消而放弃（见 rollbackWrite），但也**不能无界**——远端 Delete/ListDir 挂起会永久阻塞，
// 且 pruneCreatedDirs 持 s.mu 做 I/O ⇒ 可以拖死整卷（P2 修复，2026-10-19）。30s 覆盖
// 单次远端删除的正常上限并给回滚集合（通常 1..N 个 blob）留足余量。
const rollbackTimeout = 30 * time.Second

// boundedCleanupCtx 返回**清理动作**（回滚删已上传 blob / 删旧版本 meta+分块）使用的 ctx：
// **不受调用方取消影响**（否则客户端断开/上层 deadline 场景下清理立即失败 ⇒ 远端孤儿、
// 或删除路径留下幽灵条目），但**必须有界**——远端 Delete/ListDir 挂起会**持卷锁**（s.mu）
// 把整卷读写拖死，无界等待不可接受。
//
// 单一机制（P2 修复，2026-10-19）：回滚与「成功路径删旧版本」（removeVersionMeta）此前
// 两种写法——后者用裸 context.Background()（**无界**）⇒ 与本函数同源风险未收口。
func boundedCleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
}

// rollbackWrite 失败回滚：删除本次已上传的新 meta/分块；再回收本次新建且已成空的容器
// （目录 meta + 注销映射，删新留旧，F-2）。保有他人并发写入内容的容器不删（安全）。
func (s *SecretdataFS) rollbackWrite(ctx context.Context, uploaded []string, created []dirCreation) {
	// 清理必须**不受调用方 ctx 取消影响**（P2 修复，2026-10-19）：写失败常伴随 ctx 取消/
	// 超时（客户端断开、上层 deadline）——若用已取消的 ctx 做 Delete/ListDir，底层立即返回
	// ctx.Err() ⇒ 已上传分块/meta 全部残留为远端孤儿。回滚是本卷自身的补偿动作，不随调用方
	// 取消而放弃；但必须有界（见 rollbackTimeout / boundedCleanupCtx），否则挂起的远端 I/O
	// 会持锁拖死整卷。
	cctx, cancel := boundedCleanupCtx(ctx)
	defer cancel()
	for _, p := range uploaded {
		_ = s.inner.Delete(cctx, p)
	}
	s.pruneCreatedDirs(cctx, created)
}

// pruneCreatedDirs 写路径失败回滚：删除本次新建且已成空目录的容器（删除目录 meta + 注销
// 映射）。目录内保有他者新写入文件/子目录则不删（安全不损他人）；根容器恒不删。
func (s *SecretdataFS) pruneCreatedDirs(ctx context.Context, created []dirCreation) {
	// **空集合必须早退**（P0 修复，2026-10-21）：本函数会取 s.mu 写锁，而 rollbackWrite 的
	// 「无新建目录」调用方（rename/copy 路径的 metadata 上传、copyContentBlobs 回滚）**正持有
	// s.mu**（如 writeRenamedMetaLocked 的调用方持锁）⇒ 无条件 Lock 会自锁死（sync.Mutex
	// 不可重入），表现为该卷所有读写永久挂起、只能重启进程。created 为空时无任何回收动作，
	// 直接返回即可（语义等价）。
	if len(created) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, dc := range slices.Backward(created) { // 叶子→根逆序（先回收最深空目录）
		if dc.dirPath == "" {
			continue // 根容器恒保留（路径解析锚点）
		}
		if s.dirHasContentLocked(dc.dirPath) {
			continue // 已被并发写占用（文件/子目录）→ 保留容器
		}
		s.removeEmptyContainerLocked(ctx, dc.container, dc.dirPath)
	}
}

// dirHasContentLocked 判定逻辑目录是否仍含内容（文件或子目录）。调用方持 s.mu。
func (s *SecretdataFS) dirHasContentLocked(d string) bool {
	prefix := d + "/"
	for k := range s.index {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	for _, parent := range s.dirParents {
		if parent == d {
			return true
		}
	}
	return false
}

// removeEmptyContainerLocked 删除空目录的容器残留（Imp-1：删干净、重启不复现）：删除容器内
// 全部文件（目录 meta + 遗留/孤儿分块）并注销 dirSegs/dirs/dirIDs/dirParents。调用方持
// s.mu；仅当目录确为空（dirHasContentLocked==false）时调用，删除全部文件是安全的。
func (s *SecretdataFS) removeEmptyContainerLocked(ctx context.Context, container, dirPath string) {
	// **自带界定**（P2 修复，2026-10-21）：本函数在 s.mu 写锁内被 pruneEmptyDirsLocked 直接
	// 调用（删除路径传入的是调用方 ctx）⇒ 远端 ListDir/Delete 挂起会持锁拖死整卷，与本文件
	// 「锁内远端删除必须有界」的既有判据（boundedCleanupCtx）一致化；已界定 ctx 再套一层无害。
	cctx, cancel := boundedCleanupCtx(ctx)
	defer cancel()
	ctx = cctx
	inner, err := s.inner.ListDir(ctx, container)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// **列目录失败必须 fail-closed**（独立审核 P2，2026-10-18）：此前仅在 `err == nil` 时进
		// 循环，失败即**静默跳过删除**，却仍注销内存映射 ⇒ 磁盘上目录 meta 仍在（重启后 loadIndex
		// 扫到 ⇒ **目录"复活"**：幽灵空目录），且容器删除必然失败（残留层级）。改为：**不注销映射、
		// 不删容器**并显式告警 ⇒ 本会话内该目录仍可见（与磁盘一致），下次 rmdir 再尝试回收。
		slog.Warn("secretdata: 列目录失败，跳过空容器回收（保留内存映射，防重启后幽灵目录）",
			"container", container, "dir", dirPath, "error", err)
		return
	}
	// 逐文件删除：**失败必须 fail-closed**（独立复审 P2，2026-10-18）——此前 `_ =` 忽略错误 ⇒
	// 文件仍在盘上却继续走后面的「删容器 + 注销映射」⇒ 容器删不掉（残留）且映射已丢 ⇒ 重启后
	// 该目录 meta 又被扫到（幽灵目录"复活"），与上面 ListDir 分支的处置不一致。
	// 删除失败则保留映射并告警，下次 rmdir 再尝试回收。
	for _, f := range inner {
		if f.IsDir {
			continue
		}
		// 目录 meta blob 的落盘字节从真值累计中扣减（列目录时顺带求和 ⇒ 零额外 I/O）。
		if shardseal.ClassifyName(f.Name) == shardseal.KindDirMeta && f.Size > 0 {
			s.dirPhysTotal = max(s.dirPhysTotal-f.Size, 0)
		}
		if ferr := s.inner.Delete(ctx, path.Join(container, f.Name)); ferr != nil && !errors.Is(ferr, os.ErrNotExist) {
			s.dirPhysTotal += f.Size // 回滚上面的扣减（文件仍在盘上 ⇒ 真值必须与磁盘一致）
			slog.Warn("secretdata: 删除容器内残留文件失败，跳过空容器回收（保留内存映射）",
				"container", container, "dir", dirPath, "file", f.Name, "error", ferr)
			return
		}
	}
	// 容器目录本身也删除（物理嵌套下必须回收：子容器已先删 ⇒ 目录为空可删；残留空容器目录会让
	// 底层目录树出现「无 meta 的层级」，也会让父容器无法回收）。根容器由调用方保证不被走到
	// （prune 循环 `for d != ""`），此处 `container` 恒为某逻辑目录的容器。
	if derr := s.inner.Delete(ctx, container); derr != nil && !errors.Is(derr, os.ErrNotExist) {
		slog.Warn("secretdata: 删除空容器目录失败（残留空目录，不影响正确性）",
			"container", container, "dir", dirPath, "error", derr)
	}
	delete(s.dirSegs, dirPath)
	delete(s.dirIDs, dirPath)
	delete(s.dirParents, dirPath)
	delete(s.dirs, dirPath)
	delete(s.dirMTime, dirPath)
}

// openRead：读取逻辑路径 → 返回按需解密的 ReadCloser（**不落任何临时文件**：见
// read_seeker.go 的按需解密 + 既有 sync.RangeSeeker 窗口；temp → 0）。
func (s *SecretdataFS) openRead(ctx context.Context, rel string) (io.ReadCloser, error) {
	s.mu.RLock()
	e, ok := s.index[rel]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("secretdata: 文件 %q 不存在: %w", rel, os.ErrNotExist)
	}
	if e.meta == nil {
		return nil, fmt.Errorf("secretdata: %q 是目录", rel)
	}
	// **复用既有原语，不重复实现随机访问**（2026-10-18 重新整合）：
	// 随机读的唯一原语是 `OpenRangeRead`（blocklet 级定位 + 最小下载 + erasure 感知），
	// 而 `sync.RangeSeeker` 早已是「RangeReader → io.ReadSeeker」的通用适配器（4MiB 窗口、
	// 跨窗续读、ctx 闭包捕获不存 struct）。故 `OpenRead` = **RangeSeeker(over 本卷 OpenRangeRead)**
	// + 一个**薄**装饰器（整文件 SHA-256 校验 + 读侧上限），**不落任何临时文件**（temp → 0）。
	// 消费侧 io.ReadSeeker 强要求由此满足（不牺牲可用性）。
	return s.openReader(ctx, rel)
}

// dirScan 是容器目录 meta 扫描结果（loadIndex Phase 1，并行产生）。
type dirScan struct {
	container string
	dm        *dirMeta
	hasFiles  bool
	// dirPhys 是本容器目录 meta blob 的落盘字节（Phase 1 已读取 ⇒ 顺带求和，供 dirPhysTotal）。
	dirPhys int64
}

// dirRes 是容器逻辑路径解析结果（loadIndex Phase 2，供 Phase 3 文件加载）。
type dirRes struct {
	container string
	rel       string
	ok        bool
}

// loadIndex 重建内存索引（旧卷加载，§10 / 修复 F-1：逻辑键与写路径一致）。task10 目录
// 解耦：目录 meta 存 {name, parent_dir_id}，逻辑路径沿 parent 链重算；移动子树仅改根节点
// parent 引用。并行化（Imp-2）：Phase 1 按容器并行解密目录 meta（建 dir_id → dirMeta 表），
// Phase 2 沿 parent 链解析各容器逻辑路径并登记容器映射，Phase 3 各容器文件 meta 并行解密
// 登记索引（s.mu 按容器/文件细粒度保护，可安全并行）。
//
// 首层 ListDir 区分「首次使用」与「底层故障」（Minor 修复，Imp-3 硬前提）：仅 os.ErrNotExist
// 视为空卷（首次使用，返回 nil 空索引）；其它错误（瞬时 IO/云盘故障）→ 记 Error 日志并返回
// 错误使 NewFS fail-closed——GC 复用内存索引（gcMarkIndex）依赖索引完整性，若底层故障被吞
// 为空卷，GC 会把全部 meta 当孤儿清扫（数据丢失）。挂载失败比静默空视图安全。
func (s *SecretdataFS) loadIndex(ctx context.Context) error {
	root, err := s.inner.ListDir(ctx, "")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return nil // 首次使用：根不存在 → 空卷
		}
		return fmt.Errorf("secretdata: 扫描卷根失败（非首次使用故障，拒绝空视图挂载）: %w", err)
	}
	containers, cerr := s.dirContainers(ctx, root)
	if cerr != nil {
		return fmt.Errorf("secretdata: 扫描容器树失败（拒绝空视图挂载）: %w", cerr)
	}
	scans, serr := scanAllContainerDirMetas(ctx, s, containers)
	if serr != nil {
		return serr
	}
	byID := dirMetaTable(scans)
	// **修复「被中断的目录移动」**（P1 修复，2026-10-18）：目录移动是两步（重写目录 meta +
	// 物理搬容器），崩在两步之间会留下「物理位置 ≠ meta 声明位置」的中间态；旧实现由
	// validateNestedContainers 直接 fail-closed ⇒ 整卷拒绝挂载 = **服务启动失败** ✗（数据不丢：
	// 容器 rename 是原子步；但可用性阻塞）。修复必须在 Phase 2 登记 dirSegs **之前**做——修复会
	// 改变容器的物理路径，后登记才能拿到正确路径。
	if rerr := s.repairInterruptedDirMoves(ctx, scans); rerr != nil {
		return rerr
	}
	res := s.resolveContainerPaths(ctx, byID, scans)
	// **只支持嵌套布局**（用户口径 2026-10-14：不支持旧格式目录、无兼容）⇒ 旧扁平布局 fail-closed。
	// 判据所需：有多少容器**成功解密**了目录 meta（注意与「解析出逻辑路径」不同——根 meta 缺失时
	// 子容器父链断裂、ok=false，但其自身 meta 是**能解开的**，这正是「损坏」的信号）。
	decryptedDirs := 0
	for _, sc := range scans {
		if sc.dm != nil {
			decryptedDirs++
		}
	}
	if verr := s.validateNestedContainers(res, decryptedDirs); verr != nil {
		return verr
	}
	return loadAllFileMetas(ctx, s, res)
}

// maxContainerDepth 是容器树递归深度上限（防御异常/恶意深树导致栈/IO 爆炸；正常逻辑树远小于此）。
const maxContainerDepth = 64

// dirContainers 递归收集底层**全部容器目录**（Phase 0）。
//
// 容器是**物理嵌套**的（子容器落在父容器内，2026-10-14 用户口径：底层目录树与逻辑目录树同形）
// ⇒ 只列卷根会漏掉全部嵌套层级（表现为「多级目录只剩一层」）。收集后由 `validateNestedContainers`
// 校验布局——**旧扁平布局不支持、无兼容**（fail-closed 拒绝挂载）。
func (s *SecretdataFS) dirContainers(ctx context.Context, root []syncpkg.Entry) ([]string, error) {
	var out []string
	if err := s.walkContainers(ctx, "", root, 0, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// walkContainers 递归收集容器目录（depth 防异常/恶意深树导致栈/IO 爆炸）。
// 独立复审 P2 修复：对 ErrNotExist 容忍（并发删除/GC 是良性的），与
// scanContainerDirMeta/loadContainerFileMetas 一致（见 listSubContainer）。
func (s *SecretdataFS) walkContainers(ctx context.Context, prefix string, entries []syncpkg.Entry, depth int, out *[]string) error {
	if depth > maxContainerDepth {
		return fmt.Errorf("secretdata: 容器树深度超过上限 %d（目录 %q）", maxContainerDepth, prefix)
	}
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		p := path.Join(prefix, e.Name)
		*out = append(*out, p)
		sub, skip, err := s.listSubContainer(ctx, p)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if werr := s.walkContainers(ctx, p, sub, depth+1, out); werr != nil {
			return werr
		}
	}
	return nil
}

// listSubContainer 列子容器：ErrNotExist（并发删除/GC）⇒ skip=true（不算故障）；其它错误上抛。
// 与 scanContainerDirMeta/loadContainerFileMetas 的处置**保持一致**（独立复审 P2，2026-10-18：
// 此前这里对 ErrNotExist 也直接拒绝挂载 ⇒ 与那两处不一致）。
func (s *SecretdataFS) listSubContainer(ctx context.Context, p string) ([]syncpkg.Entry, bool, error) {
	sub, err := s.inner.ListDir(ctx, p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("列容器 %q 失败: %w", p, err)
	}
	return sub, false, nil
}

// repairInterruptedDirMoves 修复「被中断的目录移动」。
//
// 目录移动是**两步**（重写目录 meta 的 Name/ParentDirID + 物理搬容器目录），崩在两步之间会留下
// 「物理位置 ≠ meta 声明位置」的中间态。修复方向**恒为「向 meta 收敛」**（meta 是逻辑树的持久
// 声明 ⇒ 逻辑位置、目录名、父引用全部来自它，信息不丢）：
//   - 移动的新序（先写 meta、后搬容器，见 renameDirLocked）下崩溃 ⇒ 修复 = **完成**该移动；
//   - 旧序遗留的中间态（先搬容器、后写 meta）⇒ 修复 = **回滚**该移动。
//
// 两种崩溃窗口都收敛到自洽状态，且**数据零改动**（只搬容器目录 = 原子步，内容与 meta 一起走）。
//
// **不削弱旧扁平布局的拒绝**：判据是「物理父目录是否为**已识别的容器**」——旧扁平布局把容器全
// 放在**卷根**（物理父 = ""，不是容器）⇒ 仍由 validateNestedContainers fail-closed 拒绝；只有
// 「物理父是容器、但 ≠ meta 声明的父」才判定为被中断的移动。父引用缺失/成环不在此修（交给既有
// resolveContainerPaths 的 fail-closed 跳过）。
//
// 调用时机：必须在 Phase 2（resolveContainerPaths 登记 dirSegs）**之前**（修复会改物理路径）。
// scans 为递归**前序**（父先于子）⇒ 先修父容器，后代路径随搬移整体 rebase。
func (s *SecretdataFS) repairInterruptedDirMoves(ctx context.Context, scans []dirScan) error {
	for i := range scans {
		dst, ok := interruptedMoveTarget(scans, i)
		if !ok {
			continue
		}
		from := scans[i].container
		if rerr := s.inner.Rename(ctx, from, dst); rerr != nil {
			return fmt.Errorf("secretdata: 修复被中断的目录移动失败（容器 %s → %s）: %w", from, dst, rerr)
		}
		slog.Warn("secretdata: 检测到被中断的目录移动，已按目录 meta 声明的位置修复（完成或回滚该移动）",
			"container_from", from, "container_to", dst)
		rebaseScanContainer(scans, from, dst)
	}
	return nil
}

// interruptedMoveTarget 判定 scans[i] 是否处于「被中断的目录移动」，返回应搬到的目标容器路径
// （ok=false ⇒ 不属于该类，交回既有 fail-closed 校验）。判定规则见 repairInterruptedDirMoves 注释。
func interruptedMoveTarget(scans []dirScan, i int) (string, bool) {
	sc := scans[i]
	if sc.dm == nil {
		return "", false
	}
	// 每轮按**当前**（可能已被 rebase 的）路径重建两张表，避免用陈旧键判定。
	isContainer, byID := dirScanTables(scans)
	physParent := parentDirOf(sc.container)
	// 物理父不是容器（含卷根 ""）⇒ 不是「被中断的移动」⇒ 交给既有 fail-closed 校验（旧扁平布局）。
	if physParent == "" || !isContainer[physParent] {
		return "", false
	}
	want := ""
	if sc.dm.ParentDirID != "" {
		w, ok := byID[sc.dm.ParentDirID]
		if !ok {
			return "", false // 父引用缺失 ⇒ 交给既有 fail-closed 处置
		}
		want = w
	}
	if want == physParent {
		return "", false // 物理位置与 meta 声明一致
	}
	// **自指/成环守卫**：父引用成环（A 的父是 A，或父落在 A 的子树内）时 want 会等于本容器或位于
	// 本容器之内 ⇒ 搬移会把自己嵌套进自己（实测：成环用例会直接挂载失败）。此类**畸形元数据**
	// 不属于「被中断的移动」⇒ 交回既有 fail-closed 处置，本函数不猜。
	if want == sc.container || strings.HasPrefix(want, sc.container+"/") {
		return "", false
	}
	dst := path.Base(sc.container)
	if want != "" {
		dst = path.Join(want, path.Base(sc.container))
	}
	if dst == sc.container {
		return "", false
	}
	return dst, true
}

// dirScanTables 按当前扫描记录重建「容器集合」与「DirID → 容器路径」两张表。
func dirScanTables(scans []dirScan) (map[string]bool, map[string]string) {
	isContainer := make(map[string]bool, len(scans))
	byID := make(map[string]string, len(scans))
	for _, o := range scans {
		isContainer[o.container] = true
		if o.dm != nil && o.dm.DirID != "" {
			byID[o.dm.DirID] = o.container
		}
	}
	return isContainer, byID
}

// rebaseScanContainer 把扫描记录中「被搬走的容器及其后代」的路径前缀同步更新（与
// rebaseContainerPathsLocked 同口径；Phase 2 尚未登记 dirSegs，故此处只改扫描记录）。
func rebaseScanContainer(scans []dirScan, oldC, newC string) {
	for i := range scans {
		switch c := scans[i].container; {
		case c == oldC:
			scans[i].container = newC
		case strings.HasPrefix(c, oldC+"/"):
			scans[i].container = newC + c[len(oldC):]
		}
	}
}

// validateNestedContainers 校验**只支持嵌套布局**（用户口径 2026-10-14：不支持旧格式目录、无兼容）。
//
// 判据（稳定且不与既有语义冲突）：**卷根只允许一个容器**——逻辑根目录的容器。旧**扁平**布局把每个
// 逻辑目录的容器都放在卷根（随机名兄弟目录）⇒ 卷根出现多个容器 ⇒ **fail-closed 拒绝挂载**。
//
// 判据为**严格嵌套**（用户口径 2026-10-14「物理树始终镜像逻辑树」）：**容器物理位置必须等于其逻辑父
// 目录的容器位置**——目录移动会**搬容器**（底层 mv）⇒ 移动后仍镜像；旧**扁平**布局（容器都是卷根下的
// 兄弟目录）以及任何物理/逻辑错位（如搬移中途崩溃）都会被本校验拒绝（fail-closed）。
//
// 注：去重池目录（`dedupDir`，实验能力）是卷根的**非逻辑目录**（无 dir meta）⇒ 不进 `res`，不参与校验。
func (s *SecretdataFS) validateNestedContainers(res []dirRes, decryptedDirs int) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rootCount := 0
	for _, r := range res {
		if !r.ok {
			continue
		}
		isRoot, verr := s.validateContainerPlacement(r)
		if verr != nil {
			return verr
		}
		if isRoot {
			rootCount++
		}
	}
	if rootCount > 1 {
		return fmt.Errorf("secretdata: 卷根存在 %d 个根容器（应恰好 1 个；检测到旧扁平布局）——"+
			"本版本只支持嵌套布局、不提供兼容，请重建该卷", rootCount)
	}
	// **精细化挂载判据 C**（2026-10-18 用户裁定）：卷非空却无任何容器解析为卷根时，按**成因**分流
	// （`decryptedDirs` = 成功解密目录 meta 的容器数；**与「解析出逻辑路径」不同**——根 meta 缺失时
	// 子容器父链断裂、ok=false，但其自身 meta 能解开，这正是「损坏」的信号）：
	//
	//   - `decryptedDirs > 0` ⇒ **损坏**（有容器能解密却没有根：根目录 meta 被删/损坏/父引用断裂）
	//     ⇒ **fail-closed 拒绝挂载**。否则用户看到**静默空卷**（list 空）⇒ 可能误判「数据丢了」并重传 ✗。
	//   - `decryptedDirs == 0` ⇒ **更可能是密钥/配置错误**（全部容器都解不开；错密钥与「全部损坏」在
	//     挂载期不可区分）⇒ **放行挂载**（读时失败）+ **Error 日志**。放行是既有锁定语义
	//     （`TestWrongSecretFails`：NewFS 成功、OpenRead 失败），且便于运维诊断；日志保证不「静默」。
	if rootCount == 0 && len(res) > 0 {
		if decryptedDirs > 0 {
			return fmt.Errorf("secretdata: 有 %d 个容器的目录 meta 可解密，却**无任何容器解析为卷根**"+
				"（根目录 meta 缺失/损坏或父引用断裂）——拒绝挂载以避免静默空视图（请从备份恢复根容器 meta）",
				decryptedDirs)
		}
		slog.Error("secretdata: 卷非空但**无任何容器目录 meta 可解密** ⇒ 按既有语义放行挂载（读时会失败）。"+
			"最可能原因是密钥错误（secret_url 指向的密钥与建卷时不一致）；请先核对密钥，再排查损坏",
			"containers", len(res))
	}
	return nil
}

// validateContainerPlacement 校验单个容器的物理位置：返回 (是否为根容器, 错误)。
// 严格判据（用户口径「物理树始终镜像逻辑树」）：容器物理位置 == 其逻辑父目录的容器位置。
func (s *SecretdataFS) validateContainerPlacement(r dirRes) (bool, error) {
	physParent := path.Dir(r.container)
	if physParent == "." || physParent == "/" {
		physParent = ""
	}
	if r.rel == "" {
		if physParent != "" {
			return false, fmt.Errorf("secretdata: 卷根容器 %q 不在卷根（布局非嵌套）", r.container)
		}
		return true, nil
	}
	want := s.dirSegs[parentDirOf(r.rel)]
	if want == "" {
		return false, fmt.Errorf("secretdata: 容器 %q 的逻辑父目录 %q 无容器（布局不完整）",
			r.container, parentDirOf(r.rel))
	}
	if physParent != want {
		return false, fmt.Errorf("secretdata: 容器 %q 未嵌套在逻辑父目录容器 %q 内（检测到旧扁平布局或物理/逻辑错位）"+
			"——本版本要求物理树镜像逻辑树、不提供兼容，请重建该卷", r.container, want)
	}
	return false, nil
}

// maxLoadMemBudget 是 loadIndex 并行派生允许的峰值内存预算（512MiB）。loadGate 并发
// 上界随 KDF 档位自适应，使 并发×单次派生内存 保持在预算内（Imp-C；2026-10-03 档位化后
// 修正）。
const maxLoadMemBudget = 512 << 20

// maxParallelLoads 返回 loadIndex 并行派生并发上界，随 KDF 档位**真实 scrypt 内存自适应**
// （RFC 7914：单次派生内存 = 128×r×N，见 shardseal.Algorithm.ScryptMemEstimate）：
// 并发 = clamp(内存预算/单次派生内存, 1, NumCPU)。**每个并发槽按 ~2× RFC 最小内存记账**
// （Go 堆 allocator/GC 节奏：GOGC=100 允许堆增长到 2× live，实测单次派生峰值 RSS ≈
// 2×128MiB / 2×16MiB / 2×4MiB = 256/32/8 MB——与用户「实测 256/32/8MB」口径一致），
// 使 loadIndex 实测堆峰值 ≤ 预算。三档：
//
//	low 档（2^12, RFC 4MiB/次）→ 512MiB/(2×4MiB)=64 → 钳到 NumCPU（不再被 8 无谓拖慢）；
//	standard（2^14, RFC 16MiB/次）→ 512MiB/(2×16MiB)=16 → min(16, NumCPU)；
//	high 档（2^17, RFC 128MiB/次）→ 512MiB/(2×128MiB)=2 → min(2, NumCPU)（实测峰值 ≤ 预算）。
//
// alg 为解析出的算法档位；nil 时按 standard 档兜底。
func maxParallelLoads(alg *shardseal.Algorithm) int {
	if alg == nil || alg.ScryptN <= 0 {
		alg = &shardseal.Algorithm{ScryptN: 1 << 14, ScryptR: 8, ScryptP: 1}
	}
	// 单次派生所需真实内存 = 128×r×N；每个并发槽按 2× 记账（allocator/GC 节奏）。
	mem := 2 * int64(alg.ScryptN) * int64(alg.ScryptR) * 128
	n := max(int(int64(maxLoadMemBudget)/mem), 1)
	if ncpu := runtime.NumCPU(); n > ncpu {
		n = ncpu
	}
	return n
}

// scanAllContainerDirMetas Phase 1：按容器并行扫描目录 meta（解密 + 旧格式 fail-closed）。
// 经 s.loadGate 限并发（Imp-C：单次 scrypt 派生内存随 KDF 档位，无界并行会内存爆炸）。
func scanAllContainerDirMetas(ctx context.Context, s *SecretdataFS, containers []string) ([]dirScan, error) {
	scans := make([]dirScan, len(containers))
	errs := make([]error, len(containers))
	var wg sync.WaitGroup
	for i, c := range containers {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			s.loadGate <- struct{}{} // 派生并发上界（scrypt 内存）
			defer func() { <-s.loadGate }()
			scans[i] = dirScan{container: c}
			dm, blobLen, hasFiles, ok, serr := s.scanContainerDirMeta(ctx, c)
			if serr != nil {
				errs[i] = serr
				return
			}
			if ok {
				scans[i].dm = dm
				scans[i].dirPhys = blobLen
			}
			scans[i].hasFiles = hasFiles
		}(i, c)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	return scans, nil
}

// dirMetaTable 建 dir_id → dirMeta 表（parent 链解析用）。
func dirMetaTable(scans []dirScan) map[string]*dirMeta {
	byID := make(map[string]*dirMeta, len(scans))
	for _, sc := range scans {
		if sc.dm != nil {
			byID[sc.dm.DirID] = sc.dm
		}
	}
	return byID
}

// resolveContainerPaths Phase 2：沿 parent 链解析容器逻辑路径并登记容器映射。
// 父引用断裂（parent 缺失/未解析）→ fail-closed 跳过该容器（含文件 meta 时记日志）。
func (s *SecretdataFS) resolveContainerPaths(ctx context.Context, byID map[string]*dirMeta, scans []dirScan) []dirRes {
	res := make([]dirRes, len(scans))
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sc := range scans {
		if sc.dm == nil {
			continue
		}
		rel, rok := resolveDirMetaPath(byID, sc.dm)
		if !rok {
			if sc.hasFiles {
				slog.Warn("secretdata: 容器目录 meta 父引用断裂或成环，跳过恢复其中文件（fail-closed）",
					"container", sc.container)
			}
			continue
		}
		res[i] = dirRes{container: sc.container, rel: rel, ok: true}
		s.dirPhysTotal += sc.dirPhys // 挂载时重建目录 meta 真值累计（Phase 1 已读 blob）
		s.dirSegs[rel] = sc.container
		s.dirIDs[rel] = sc.dm.DirID
		s.dirMTime[rel] = dirMetaMTime(sc.dm.MTime)
		if rel != "" {
			s.dirs[rel] = struct{}{}
			s.dirParents[rel] = parentDirOf(rel)
		}
	}
	return res
}

// loadAllFileMetas Phase 3：各容器文件 meta 并行解密登记索引。
func loadAllFileMetas(ctx context.Context, s *SecretdataFS, res []dirRes) error {
	var wg sync.WaitGroup
	errs := make([]error, len(res))
	for i, r := range res {
		if !r.ok {
			continue
		}
		wg.Add(1)
		go func(i int, r dirRes) {
			defer wg.Done()
			errs[i] = s.loadContainerFileMetas(ctx, r.container, r.rel)
		}(i, r)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// scanContainerDirMeta 定位容器目录 meta 并解密（旧格式 fail-closed）。返回
// (dirMeta, 容器是否含文件 meta, 是否找到可解析目录 meta)。目录 meta 缺失/损坏/旧格式
// → dm=nil；含文件 meta 时记日志（fail-closed：不压平到根，避免跨容器同名遮蔽，F-1）。
func (s *SecretdataFS) scanContainerDirMeta(ctx context.Context, container string) (*dirMeta, int64, bool, bool, error) {
	inner, err := s.inner.ListDir(ctx, container)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			// 容器在扫描与列目录之间消失（并发删除/GC）⇒ 视为不存在，不算"不完整视图"。
			return nil, 0, false, false, nil
		}
		// **容器级列目录失败必须 fail-closed**（独立审核 P2，2026-10-18）：此前静默 return ⇒
		// 该容器**全部文件**不进索引（用户看到"文件消失"）且**零日志**；更危险的是 GC 复用内存
		// 索引（gcMarkIndex）⇒ 会把缺失的 meta 当孤儿**清扫**（数据丢失），另有 BaseVersion 只
		// 基于残余索引 ⇒ 重启后 resolveSameRelConflict 可能把新上传内容换回旧内容（内容回滚）。
		// 与本文件既有原则一致：「挂载失败比静默空视图安全」（见 loadIndex 首层 ListDir 注释）。
		return nil, 0, false, false,
			fmt.Errorf("secretdata: 列容器 %q 失败（拒绝不完整视图挂载）: %w", container, err)
	}
	hasFiles := hasFileMeta(inner)
	if dm, blobLen, found := scanContainerDirMetaBlob(ctx, s, container, inner, hasFiles); found {
		return dm, blobLen, hasFiles, true, nil
	}
	if hasFiles {
		slog.Warn("secretdata: 容器缺少可解析目录 meta，跳过恢复其中文件（fail-closed）",
			"container", container)
	}
	return nil, 0, hasFiles, false, nil
}

// scanContainerDirMetaBlob 定位并解密容器内目录 meta（旧格式 fail-closed 判定明确并专用
// 日志，不落通用缺 meta 日志——M-6 双日志噪点）。返回 (dirMeta, 是否找到可解析目录 meta)。
// dirMetaCand 是容器内的一个**可解密**目录 meta 候选（多候选取最新时间，见 scanContainerDirMetaBlob）。
type dirMetaCand struct {
	dm    *dirMeta
	name  string
	size  int64
	mtime int64
}

func scanContainerDirMetaBlob(ctx context.Context, s *SecretdataFS, container string, inner []syncpkg.Entry, hasFiles bool) (*dirMeta, int64, bool) {
	cands, ok := collectDirMetaCands(ctx, s, container, inner, hasFiles)
	if !ok || len(cands) == 0 {
		return nil, 0, false
	}
	// **多目录 meta 必须确定性仲裁**（P1 修复，2026-10-21）：
	// 目录改名/移动是「先写新 meta、后删旧 meta」两步；若第二步失败（客户端断开、远端 5xx），
	// 容器内会并存两个可解密的目录 meta。此前**按 ListDir 顺序取第一个** ⇒ 重启后可能取到旧
	// meta：`repairInterruptedDirMoves` 会把容器按旧声明搬回去 ⇒ 改名**静默回退**（再次重启可能
	// 又翻回新路径），用户按新路径写入的文件也随之改名。这里改为「取**最新 mtime**」：新 meta
	// 必然后写（同一容器内顺序执行），与「已完成但清理失败」的真实意图一致，且**可重现**。
	// 排序键：Version（改名递增）→ mtime → 名字；并显式 Warn 便于运维发现清理残留。
	slices.SortStableFunc(cands, func(a, b dirMetaCand) int {
		// ① 版本号（改名/移动单调递增）——权威信号，不依赖文件系统时间精度；
		// ② mtime（兼容旧数据：旧版改名不递增版本，同版本时按写入先后）；
		// ③ 名字（纯兜底，保证完全确定性）。
		if av, bv := a.dm.Version, b.dm.Version; av != bv {
			if av > bv {
				return -1
			}
			return 1
		}
		if a.mtime != b.mtime {
			if a.mtime > b.mtime {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	if len(cands) > 1 {
		stale := make([]string, 0, len(cands)-1)
		for _, c := range cands[1:] {
			stale = append(stale, c.name)
		}
		slog.Warn("secretdata: 容器内存在多个目录 meta（改名/移动的旧 meta 清理失败），按最新时间仲裁",
			"container", container, "chosen", cands[0].name, "chosen_dir", cands[0].dm.Name, "stale", stale)
	}
	return cands[0].dm, cands[0].size, true
}

// collectDirMetaCands 收集容器内全部**可解密**目录 meta 候选。oldFormat=true 表示容器目录 meta
// 为旧格式（调用方 fail-closed；ok=false）。损坏/读失败的候选跳过（继续尝试其余）。
func collectDirMetaCands(ctx context.Context, s *SecretdataFS, container string, inner []syncpkg.Entry, hasFiles bool) ([]dirMetaCand, bool) {
	var cands []dirMetaCand
	for _, f := range inner {
		if f.IsDir || shardseal.ClassifyName(f.Name) != shardseal.KindDirMeta {
			continue
		}
		blob, oerr := readBlob(ctx, s.inner, path.Join(container, f.Name))
		if oerr != nil {
			continue
		}
		dm, derr := s.decryptDirMeta(blob)
		if derr != nil {
			if errors.Is(derr, errOldFormatDirMeta) {
				if hasFiles {
					slog.Warn("secretdata: 容器目录 meta 为旧格式（path 字段），跳过恢复其中文件（fail-closed）",
						"container", container)
				}
				return nil, false
			}
			continue // 其它解密失败（损坏）继续尝试容器内其余 dir meta
		}
		cands = append(cands, dirMetaCand{dm: dm, name: f.Name, size: int64(len(blob)), mtime: f.MTime})
	}
	return cands, true
}

// hasFileMeta 报告容器内是否存在文件 meta（z 标记）条目。
func hasFileMeta(entries []syncpkg.Entry) bool {
	for _, f := range entries {
		if !f.IsDir && shardseal.ClassifyName(f.Name) == shardseal.KindFileMeta {
			return true
		}
	}
	return false
}

// resolveDirMetaPath 沿 parent 链重算逻辑路径（根 → … → name）。根容器（ParentDirID 空、
// Name 空）→ ""。父引用缺失（悬挂）或成环（含自环）→ ok=false（调用方 fail-closed 跳过，
// 不崩溃——Imp-B：未信任底层存储上损坏/篡改的父引用环不得导致挂载无限递归栈溢出）。
func resolveDirMetaPath(byID map[string]*dirMeta, dm *dirMeta) (string, bool) {
	return resolveDirMetaPathVisited(byID, dm, map[string]bool{})
}

// resolveDirMetaPathVisited 是带 visited 集的递归实现：沿 parent 链上溯时记录已访问 dir_id，
// 再次遇到（环/自环）→ fail-closed 返回 false（不无限递归）。
func resolveDirMetaPathVisited(byID map[string]*dirMeta, dm *dirMeta, visited map[string]bool) (string, bool) {
	if dm == nil {
		return "", false
	}
	if dm.ParentDirID == "" {
		return dm.Name, true // 根容器 Name="" → 根路径 ""
	}
	if visited[dm.DirID] {
		return "", false // 环（含自环）→ fail-closed
	}
	visited[dm.DirID] = true
	parent, ok := byID[dm.ParentDirID]
	if !ok {
		return "", false
	}
	p, ok := resolveDirMetaPathVisited(byID, parent, visited)
	if !ok {
		return "", false
	}
	return path.Join(p, dm.Name), true
}

// logicalDirName 返回逻辑目录名（根为空串；非根 = 路径末段）。
func logicalDirName(dirPath string) string {
	if dirPath == "" {
		return ""
	}
	return path.Base(dirPath)
}

// loadContainerFileMetas 并行解密容器内全部文件 meta 并登记索引（Imp-2：容器内文件并行）。
// 经 s.loadGate 限并发（Imp-C：单次 scrypt 派生内存随 KDF 档位，无界并行会内存爆炸）。
func (s *SecretdataFS) loadContainerFileMetas(ctx context.Context, container, dirPath string) error {
	inner, err := s.inner.ListDir(ctx, container)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return nil // 容器已消失（并发删除）⇒ 非"不完整视图"
		}
		// 同 scanContainerDirMeta：容器级列目录失败必须 fail-closed（否则索引不完整 ⇒ GC 误清扫
		// /BaseVersion 回退）。此处在 Phase 3（文件 meta 登记），同样拒绝不完整视图。
		return fmt.Errorf("secretdata: 列容器 %q 失败（拒绝不完整视图挂载）: %w", container, err)
	}
	var wg sync.WaitGroup
	for _, f := range inner {
		if f.IsDir || shardseal.ClassifyName(f.Name) != shardseal.KindFileMeta {
			continue
		}
		wg.Add(1)
		go func(f syncpkg.Entry) {
			defer wg.Done()
			s.loadGate <- struct{}{} // 派生并发上界（scrypt 内存）
			defer func() { <-s.loadGate }()
			s.loadContainerFileMeta(ctx, container, dirPath, f)
		}(f)
	}
	wg.Wait()
	return nil
}

// loadContainerFileMeta 解密单个文件 meta 并登记索引（逻辑 rel = 容器逻辑路径 + basename，
// 容器逻辑路径由 parent 链解析，loadIndex Phase 3 传入）。墓碑（Deleted=true）跳过不重建；
// usage 累计、volVersion 取 max、去重引用重新登记池。逻辑层 mtime 恒取 meta 内原始值
// （mm.Original.MTime），不受 blob mtime 打散影响。持 s.mu 只做登记，解密在锁外（可并行）。
func (s *SecretdataFS) loadContainerFileMeta(ctx context.Context, container, dirPath string, f syncpkg.Entry) {
	blob, oerr := readBlob(ctx, s.inner, path.Join(container, f.Name))
	if oerr != nil {
		// 读失败可能是瞬时底层故障：本文件不进内存索引（数据不可见），但**不删磁盘 blob**
		// ——GC 的 gcReconfirmContainer 会重读确认死活（Imp-3 复审：防瞬时故障被 GC 误删）。
		slog.Warn("secretdata: 挂载读文件 meta 失败（本文件暂不可见，磁盘 blob 保留，GC 将重确认）",
			"container", container, "meta", f.Name, "error", oerr)
		return
	}
	mm, merr := s.decryptFileMeta(blob)
	if merr != nil {
		slog.Warn("secretdata: 挂载解密文件 meta 失败（本文件暂不可见，磁盘 blob 保留，GC 将重确认）",
			"container", container, "meta", f.Name, "error", merr)
		return
	}
	if mm.Deleted {
		// 多进程共享卷 + GC 场景下磁盘既有墓碑（旧格式/外部写入）跳过重建索引；本实例
		// Delete 即时物理删、不产墓碑。字段（BaseVersion 并入卷版本）为格式预留——若
		// 跨进程 CAS 的第二个进程以删除前旧版本继续写，推送 volVersion 可避免版本回退。
		s.mu.Lock()
		if mm.BaseVersion > s.volVersion {
			s.volVersion = mm.BaseVersion
		}
		s.mu.Unlock()
		return
	}
	rel := path.Join(dirPath, mm.Original.Name)
	mt := dirMetaMTime(mm.Original.MTime)
	dataDir := ""
	if s.opts.Dedup && len(mm.Extra) > 0 {
		if dd, ok := mm.Extra["dedup"]; ok {
			if ds, dok := dd.(string); dok && ds != "" && len(mm.Chunks) > 0 {
				dataDir = ds
			}
		}
	}
	s.mu.Lock()
	// **同 rel 冲突处理（方案 A 修复轮 I2）**：覆盖写/Delete 物理删失败的旧 meta 落盘非墓碑，
	// 重启后与存活 meta 同路径碰撞——**按 BaseVersion 保留较大者** + 日志，杜绝「按遍历序
	// 静默旧覆盖新」的旧版本复活吞新（narrow 双故障窗口；反幽灵排序未根治此窗口）。
	prev, keep := s.resolveSameRelConflict(rel, container, f.Name, mm)
	if !keep {
		s.mu.Unlock()
		return
	}
	s.registerLoadedEntryLocked(rel, container, f.Name, mm, dataDir, mt, int64(len(blob)), prev)
	s.mu.Unlock()
}

// registerLoadedEntryLocked 登记已通过校验/冲突解决的加载条目并推进 usage/volVersion。
// 调用方持 s.mu（且已 resolveSameRelConflict 通过）。prev 是冲突前旧条目（同 rel 已被
// 新版本覆盖时非 nil；调用方负责对 usage 换账）。
func (s *SecretdataFS) registerLoadedEntryLocked(rel, container, metaName string, mm *shardseal.Meta, dataDir string, mt, metaBlobLen int64, prev *metaEntry) {
	// 去重引用文件：dataDir 指向池容器；池引用计数随加载恢复（重启后仍可物理删归零）。
	if dataDir != "" {
		s.registerPoolEntryLocked(mm)
	}
	s.index[rel] = &metaEntry{
		size: mm.Original.Size, mtime: mt, dirSeg: container, metaName: metaName, meta: mm,
		dataDir: dataDir, baseVersion: mm.BaseVersion,
		phys: physBytesOf(mm, metaBlobLen, dataDir != ""),
	}
	addDirKeysLocked(s.dirs, rel)
	s.usage += mm.Original.Size
	if prev != nil {
		s.usage -= prev.size
		if s.usage < 0 {
			s.usage = 0
		}
	}
	if mm.BaseVersion > s.volVersion {
		s.volVersion = mm.BaseVersion
	}
}

// resolveSameRelConflict 处理同 rel 第二 meta（覆盖写/Delete 物理删失败的旧 meta 落盘非墓碑，
// 重启复活，修复轮 I2）：**按 BaseVersion 保留较大者**，杜绝「按遍历序静默旧覆盖新」。
// 调用方须持 s.mu（纯读 s.index）。返回 (prev, keep)：
//   - keep=false：本条目（版本 ≤ 已载条目）被跳过（旧版本复活被拒），调用方直接 return；
//   - keep=true 且 prev==nil：首次载入本 rel；
//   - keep=true 且 prev!=nil：新版本覆盖旧条目（正常覆盖写重启；调用方对 usage 换账）。
func (s *SecretdataFS) resolveSameRelConflict(rel, container, metaName string, mm *shardseal.Meta) (prev *metaEntry, keep bool) {
	prev = s.index[rel]
	if prev == nil {
		return nil, true
	}
	switch {
	case mm.BaseVersion < prev.baseVersion:
		slog.Warn("secretdata: 同路径 meta 冲突，保留较大版本（旧 meta 复活被拒绝，防旧覆盖新）",
			"rel", rel, "keep_version", prev.baseVersion, "skip_version", mm.BaseVersion,
			"container", container, "meta", metaName)
		return prev, false
	case mm.BaseVersion == prev.baseVersion:
		slog.Warn("secretdata: 同路径 meta 同版本重复落盘，保留先载条目",
			"rel", rel, "version", mm.BaseVersion, "container", container, "meta", metaName)
		return prev, false
	}
	// mm.BaseVersion > prev.baseVersion：新版本覆盖旧条目。
	return prev, true
}

// registerPoolEntryLocked 把去重引用文件的分块登记/合并进卷级池（重启后引用计数恢复）。
// 调用方须持 s.mu。
func (s *SecretdataFS) registerPoolEntryLocked(mm *shardseal.Meta) {
	if !s.opts.Dedup || len(mm.Chunks) == 0 {
		return
	}
	key := mm.Chunks[0].OrigSHA256
	if b, ok := s.dedupPool[key]; ok {
		b.refs++
		return
	}
	s.dedupPool[key] = &dedupBlob{name: mm.Chunks[0].FileName, refs: 1, meta: mm, phys: contentPhysBytes(mm)}
}

// readBlob 读取底层文件全部字节（容错：失败返回 error）。
func readBlob(ctx context.Context, inner syncpkg.FS, p string) ([]byte, error) {
	rc, err := inner.OpenRead(ctx, p)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// errOldFormatDirMeta 是旧格式目录 meta（含 path 键）的 fail-closed 哨兵（task10：未上线
// 可仅新格式；识别靠明文含 "path" 键——旧根 meta 的 name/parent 字段与新生根均空，不可仅
// 凭字段区分，须按键存在性判定）。
var errOldFormatDirMeta = fmt.Errorf("secretdata: 旧格式目录 meta（path 字段）不再支持（fail-closed）")

// decryptFileMeta 解密文件 meta blob 并反序列化为 shardseal.Meta。
func (s *SecretdataFS) decryptFileMeta(blob []byte) (*shardseal.Meta, error) {
	raw, err := s.decryptBlob(blob)
	if err != nil {
		return nil, err
	}
	var m shardseal.Meta
	if json.Unmarshal(raw, &m) != nil {
		return nil, fmt.Errorf("secretdata: 文件 meta 反序列化失败")
	}
	if m.Original.Name == "" || len(m.Chunks) == 0 {
		return nil, fmt.Errorf("secretdata: 文件 meta 字段不完整")
	}
	// 纵深加固（P3/P6 落地，修复轮 M1）：逻辑文件名必须为不含路径分隔符的裸 basename
	// （`path.Join(dirPath, mm.Original.Name)` 依赖裸名，非法名会致索引键水平漂移）；
	// 原始整文件 SHA-256 必须非空（DecryptFile 完整性校验依赖它）。写路径恒写裸名 + 全量
	// SHA256，故仅拦异常 meta（fail-closed，不静默改写）。
	if strings.Contains(m.Original.Name, "/") || strings.Contains(m.Original.Name, `\`) {
		return nil, fmt.Errorf("secretdata: 文件 meta 原始名非法（须为不含路径分隔符的裸名，name=%q）", m.Original.Name)
	}
	if m.Original.SHA256 == "" {
		return nil, fmt.Errorf("secretdata: 文件 meta 原始整文件 SHA-256 缺失（fail-closed）")
	}
	for _, c := range m.Chunks {
		if strings.Contains(c.FileName, "/") || strings.Contains(c.FileName, `\`) {
			return nil, fmt.Errorf("secretdata: 文件 meta 分块名非法（须为裸名，file=%q）", c.FileName)
		}
	}
	return &m, nil
}

// decryptDirMeta 解密目录 meta blob 并反序列化为 dirMeta。旧格式（含 "path" 键）fail-closed。
func (s *SecretdataFS) decryptDirMeta(blob []byte) (*dirMeta, error) {
	raw, err := s.decryptBlob(blob)
	if err != nil {
		return nil, err
	}
	// 旧格式判别：明文含 "path" 键（新格式不写该键）。旧根 meta 的 name/parent 均空，
	// 与新格式根容器不可仅凭字段区分，须按键存在性 fail-closed（未上线，无需兼容）。
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return nil, fmt.Errorf("secretdata: 目录 meta 反序列化失败")
	}
	if _, hasPath := probe["path"]; hasPath {
		return nil, errOldFormatDirMeta
	}
	var dm dirMeta
	if json.Unmarshal(raw, &dm) != nil {
		return nil, fmt.Errorf("secretdata: 目录 meta 反序列化失败")
	}
	if dm.Type != "dir" {
		return nil, fmt.Errorf("secretdata: 目录 meta type=%q，应为 dir", dm.Type)
	}
	// DirID 校验（fail-closed）：DirID 用作 DirMetaName 的中段（base62 9 字符，见命名规则）。
	// 长度/字符集不符的 DirID 会在后续写路径 interleaveCore 处 panic——这里在解密边界
	// 提前拒绝（旧格式/损坏/外来数据，未上线无兼容负担；拒绝即跳过该容器）。
	if len(dm.DirID) != shardseal.NameCharsLen || !shardseal.IsBase62ID(dm.DirID) {
		return nil, fmt.Errorf("secretdata: 目录 meta dir_id 非法（应为 %d 字符 base62，got %q）",
			shardseal.NameCharsLen, dm.DirID)
	}
	return &dm, nil
}

// decryptBlob 解密统一格式 meta blob。先读内嵌盐 → 查派生缓存（同 salt 免重复 scrypt，
// Imp-2）；未命中按本 FS 算法版本派生并缓存；配置版本不匹配（跨算法卷）回落
// DecryptMetaStandalone 全版本尝试（版本不可缓存）。
func (s *SecretdataFS) decryptBlob(blob []byte) ([]byte, error) {
	salt, serr := shardseal.MetaBlobSalt(blob)
	if serr != nil {
		return nil, serr
	}
	saltHex := hex.EncodeToString(salt)
	if key, ok := s.keyCache.get(saltHex); ok {
		if plain, derr := shardseal.DecryptMetaJSON(key, blob); derr == nil {
			return plain, nil
		}
		s.keyCache.delete(saltHex) // 缓存键失配则剔除（理论上不应发生）
	}
	key, kerr := shardseal.DeriveKey(s.secret, salt, s.algoVer)
	if kerr == nil {
		if plain, derr := shardseal.DecryptMetaJSON(key, blob); derr == nil {
			s.keyCache.put(saltHex, key)
			return plain, nil
		}
	}
	return shardseal.DecryptMetaStandalone(s.secret, blob)
}

// ensureContainer 返回 dirPath 逻辑目录的随机容器目录名；不存在则**递归创建整条祖先链**
// （根 → … → dirPath，每级一个随机容器 + 目录 meta：{name, parent_dir_id} 父引用模型）。
// created 记录本次新建容器（叶子 + 祖先链，供失败回滚按空目录回收）；根容器恒不回收。
// 持有 s.mu（含底层 I/O，避免并发建容器 TOCTOU）。
func (s *SecretdataFS) ensureContainer(ctx context.Context, dirPath string, mtime int64) (container string, created []dirCreation, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureContainerLocked(ctx, dirPath, mtime)
}

// createContainerLocked 创建一层容器目录并写入其加密目录 meta，返回
// (容器相对路径, dir_id, dir-meta blob 名, blob 落盘长度)。调用方持 s.mu。
//
// **物理嵌套**（2026-10-14 用户口径）：子容器落在**父容器目录内** ⇒ 底层目录树与逻辑目录树同形
// （此前所有容器都是卷根下的随机名兄弟目录 ⇒ 存储上看不出父子关系）。卷根容器（parentContainer==""）
// 仍在卷根（顶层）——逻辑树根无父容器。**旧扁平布局不支持、无兼容**：加载侧校验卷根容器唯一
// （`validateNestedContainers`），违反即 fail-closed 拒绝挂载。
func (s *SecretdataFS) createContainerLocked(ctx context.Context, dirPath, parentContainer, parentID string, mtime int64) (string, string, string, int64, error) {
	c, cerr := shardseal.RandDirName()
	if cerr != nil {
		return "", "", "", 0, cerr
	}
	if parentContainer != "" {
		c = path.Join(parentContainer, c)
	}
	// 显式建容器目录（幂等 MkdirAll）：嵌套层级下不能依赖「写文件隐式建父目录」（部分后端只建
	// 一级），且**空目录**也必须有容器目录存在。
	if merr := s.inner.MakeDir(ctx, c); merr != nil {
		return "", "", "", 0, fmt.Errorf("secretdata: 创建容器目录 %s 失败: %w", c, merr)
	}
	dirID, iderr := shardseal.RandID62()
	if iderr != nil {
		return "", "", "", 0, iderr
	}
	salt, serr := shardseal.RandSalt()
	if serr != nil {
		return "", "", "", 0, serr
	}
	key, kerr := shardseal.DeriveKey(s.secret, salt, s.algoVer)
	if kerr != nil {
		return "", "", "", 0, kerr
	}
	dm := dirMeta{Version: 1, Algorithm: s.opts.Algorithm, KDF: "scrypt", Type: "dir",
		Name: logicalDirName(dirPath), ParentDirID: parentID, DirID: dirID, MTime: mtimeString(mtime)}
	dmJSON, merr := json.Marshal(dm)
	if merr != nil {
		return "", "", "", 0, merr
	}
	blob, berr := shardseal.EncryptMetaJSON(key, salt, dmJSON, s.metaPadTarget())
	if berr != nil {
		return "", "", "", 0, berr
	}
	// 新命名：首/末段 = 同一加密 dir-meta blob 的两窗口（offset 0/16）base62，
	// 自包含可不解密验证密文完整；中段 = 目录随机 id（base62 9 字符，匿名性一致）。
	blobA, blobB := shardseal.Hash48Pair(blob, 0, 16)
	name, nerr := shardseal.DirMetaName(blobA, dirID, blobB)
	if nerr != nil {
		return "", "", "", 0, nerr
	}
	if werr := s.uploadBlob(ctx, path.Join(c, name), blob, s.blobMTime(mtime)); werr != nil {
		return "", "", "", 0, fmt.Errorf("secretdata: 写目录 meta %s 失败: %w", name, werr)
	}
	return c, dirID, name, int64(len(blob)), nil
}

// ensureContainerLocked 是 ensureContainer 的持锁实现（递归建祖先链；调用方持 s.mu）。
func (s *SecretdataFS) ensureContainerLocked(ctx context.Context, dirPath string, mtime int64) (string, []dirCreation, error) {
	if c, ok := s.dirSegs[dirPath]; ok {
		return c, nil, nil
	}
	var created []dirCreation
	var parentID, parentContainer string
	if dirPath != "" {
		parent := parentDirOf(dirPath)
		pc, pcreated, perr := s.ensureContainerLocked(ctx, parent, mtime)
		if perr != nil {
			return "", nil, perr
		}
		created = append(created, pcreated...)
		parentContainer = pc
		parentID = s.dirIDs[parent]
		if parentID == "" {
			return "", nil, fmt.Errorf("secretdata: 父目录 %q 无 dir_id，无法建立父子引用", parent)
		}
	}
	c, dirID, name, blobLen, cerr := s.createContainerLocked(ctx, dirPath, parentContainer, parentID, mtime)
	if cerr != nil {
		return "", nil, cerr
	}
	s.dirPhysTotal += blobLen
	s.dirSegs[dirPath] = c
	s.dirIDs[dirPath] = dirID
	if dirPath != "" {
		s.dirs[dirPath] = struct{}{}
		s.dirParents[dirPath] = parentDirOf(dirPath)
		s.dirMTime[dirPath] = mtime
	}
	created = append(created, dirCreation{dirPath: dirPath, container: c, dmName: name})
	return c, created, nil
}

// renameDirLocked 目录移动/改名：仅重写本目录容器 meta 的 Name/ParentDirID（一个文件），
// 子树内其它目录/文件 blob 零改动（子树父引用指向本 dir_id 不变，按 dir_id 解耦）。
// 调用方须持有 s.mu 且已校验 to 不存在、目标父目录存在（fail-closed，不落半态：先写新
// 后删旧，成功才提交内存态）。
func (s *SecretdataFS) renameDirLocked(ctx context.Context, from, to string) error {
	container := s.dirSegs[from]
	if container == "" {
		return fmt.Errorf("secretdata: 目录 %q 无对应容器，无法移动", from)
	}
	toParent := parentDirOf(to)
	toName := path.Base(to)
	var toParentID string
	if toParent != "" {
		toParentID = s.dirIDs[toParent]
		if toParentID == "" {
			return fmt.Errorf("secretdata: 目标父目录 %q 不存在或无目录 ID", toParent)
		}
	} else {
		toParentID = s.dirIDs[""]
		if toParentID == "" {
			return fmt.Errorf("secretdata: 根目录容器未就绪")
		}
	}
	// **物理树镜像逻辑树**（用户口径 2026-10-14）：目录移动时把**容器目录**一并搬到新父目录的容器内
	// ——用**底层卷的 mv**（`s.inner.Rename`）做真实目录搬移，子树随之整体移动（一次 rename，内容零改动）。
	// 同父改名（仅 basename 变化）无需搬容器。
	oldContainer := container
	newContainer := oldContainer
	// **顺序：先重写目录 meta、后物理搬容器**（P1 修复，2026-10-18）。
	//
	// 为什么必须先写 meta：目录移动是**两步**，进程崩在两步之间会留下「物理位置 ≠ meta 声明位置」
	// 的中间态。先写 meta ⇒ 该中间态**可被加载侧确定性修复**（meta 是逻辑树的持久声明，新名与新父
	// 都在其中、信息不丢 ⇒ 修复方向恒为「向 meta 收敛」= **完成**该移动，见
	// repairInterruptedDirMoves）；旧序（先搬容器、后写 meta）崩在中间时**新名只存在于尚未写出的
	// meta 里** ⇒ 既无法完成、也无法保住改名意图，而旧实现直接 fail-closed ⇒ **整卷拒绝挂载 =
	// 服务启动失败** ✗（确定性用例
	// TestRenameDir_InterruptedMove_CrashAtAnyStep_MountsAndKeepsData 逐崩溃点实证）。
	if werr := s.rewriteDirMetaRef(ctx, oldContainer, toName, toParentID); werr != nil {
		return werr // meta 未变 ⇒ 物理未动 ⇒ 无中间态（无需回滚）
	}
	// 容器名是**随机名**（与逻辑 basename 无关）⇒ 同父改名时目标容器路径**与源相同**，此时不得
	// 调 Rename（`rename X X` 会报 file exists）——只在路径真的变化（跨父目录）时才搬。
	if cand := path.Join(s.dirSegs[toParent], path.Base(oldContainer)); cand != oldContainer {
		if rerr := s.inner.Rename(ctx, oldContainer, cand); rerr != nil {
			return fmt.Errorf("secretdata: 搬移容器 %s → %s 失败: %w（目录 meta 已按新名/新父写好，"+
				"加载侧会按 meta 声明位置自动修复该中间态）", oldContainer, cand, rerr)
		}
		newContainer = cand
	}
	// 容器路径变化 ⇒ 子树所有容器路径前缀 rebase（dirSegs 值），再迁移键。
	if newContainer != oldContainer {
		s.rebaseContainerPathsLocked(oldContainer, newContainer)
	}
	// 内存态整链迁移（dirSegs/dirIDs/index 键）+ dirs/dirParents 重建。
	s.applyDirMoveLocked(from, to)
	s.volVersion++
	return nil
}

// rewriteDirMetaRef 重写容器内目录 meta 的 Name/ParentDirID（目录移动/改名；一个文件，
// 子树其它目录/文件 blob 零改动）。内容变化 → 目录 meta 名内嵌哈希变化，故生成新随机盐
// 与新 blob 名：先写新、后删旧（同容器内，无半态）。无变化则跳过（目标同名同父）。
func (s *SecretdataFS) rewriteDirMetaRef(ctx context.Context, container, newName, newParentID string) error {
	oldName, oldBlob, err := findDirMetaBlob(ctx, s.inner, container)
	if err != nil {
		return err
	}
	dm, derr := s.decryptDirMeta(oldBlob)
	if derr != nil {
		return derr
	}
	if dm.Name == newName && dm.ParentDirID == newParentID {
		return nil
	}
	dm.Name = newName
	dm.ParentDirID = newParentID
	// **版本单调递增**（P1 修复，2026-10-21）：改名/移动会给同一目录写**新** meta 并删旧 meta；
	// 若删旧失败，容器内并存两个 meta（同一 DirID、不同 Name/Parent）。仲裁需要「哪个更新」的
	// 权威信号——文件系统 mtime 在不同后端分辨率/精度不可靠（本地卷实测可出现相等，退化成按
	// 随机盐文件名排序 ⇒ 约 50% 概率回退旧名）。Version 是 meta 自身字段，单调 +1 即确定。
	dm.Version++
	newName, werr := s.writeDirMetaLocked(ctx, container, dm)
	if werr != nil {
		return werr
	}
	if newName != oldName {
		// **有界 + 可见**（P2 修复，2026-10-21）：此前用调用方 ctx 且 `_ =` 吞错 ⇒ 客户端在
		// 两步之间断开/远端 5xx 时留下**新旧两个目录 meta**，重启时按 ListDir 顺序任取其一
		// ⇒ 改名「静默回退」（可能来回翻覆）；且无任何日志，运维无从发现。清理必须不随调用方
		// 取消而放弃（否则必然失败）且有界（锁内远端 I/O 不能无限等待）。
		cctx, cancel := boundedCleanupCtx(ctx)
		if derr := s.inner.Delete(cctx, path.Join(container, oldName)); derr != nil && !errors.Is(derr, os.ErrNotExist) {
			slog.Warn("secretdata: 删除旧目录 meta 失败，容器内将短暂并存两个目录 meta（加载期按最新时间仲裁）",
				"container", container, "old", oldName, "new", newName, "error", derr)
		}
		cancel()
		// 注：独立审核曾疑「改名不更新 dirPhysTotal ⇒ 真值漂移」——**已实测证伪**（2026-10-18）：
		// 目录 meta blob 的落盘长度与内容无关（pad 目标随机但最终长度恒定，实测 5 次改名恒 353B）
		// ⇒ 改名不产生漂移，故**不引入**额外的 Stat/累计更新（避免无收益 I/O 与推测性代码）。
	}
	return nil
}

// applyDirMoveLocked 提交目录移动后的内存态（调用方持 s.mu）：dirSegs/dirIDs/index 键
// 整体迁移（旧键删、新键留，避免 ListDir 幽灵目录与 split-brain）；dirs/dirParents 由
// dirSegs 全量重建（逻辑路径全决定，不删漏）。
// rebaseContainerPathsLocked 把 dirSegs 中所有以 old 为前缀的容器路径改写到 new（目录移动搬容器后，
// 子树容器随父容器整体移动 ⇒ 其路径前缀随之变化）。调用方持 s.mu。
func (s *SecretdataFS) rebaseContainerPathsLocked(oldC, newC string) {
	rebase := func(c string) string {
		switch {
		case c == oldC:
			return newC
		case strings.HasPrefix(c, oldC+"/"):
			return newC + c[len(oldC):]
		}
		return c
	}
	for dir, c := range s.dirSegs {
		s.dirSegs[dir] = rebase(c)
	}
	// **索引条目也持有容器路径**（每个文件 meta 记录其所在容器 `dirSeg`）⇒ 必须同步，否则
	// 文件读/删会指向已搬走（不存在）的旧路径（实测：目录移动后子树文件 dirSeg 未 rebase ⇒ 读失败）。
	for _, e := range s.index {
		if e != nil {
			e.dirSeg = rebase(e.dirSeg)
		}
	}
}

// shiftDirKey 把单个目录键按「from → to」做前缀迁移（子树整体 rebase）：等于 from ⇒ to；
// 在 from 子树内 ⇒ 前缀替换；其余不变。目录移动的 dirSegs/dirIDs/dirMTime 三张表共用。
func shiftDirKey(oldDir, from, to, prefix string) string {
	switch {
	case oldDir == from:
		return to
	case strings.HasPrefix(oldDir, prefix):
		return to + oldDir[len(from):]
	}
	return oldDir
}

// shiftDirKeys 按 shiftDirKey 规则迁移一张「目录键 → V」表（返回新表，不改原表）。
func shiftDirKeys[V any](m map[string]V, from, to, prefix string) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[shiftDirKey(k, from, to, prefix)] = v
	}
	return out
}

func (s *SecretdataFS) applyDirMoveLocked(from, to string) {
	prefix := from + "/"
	// dirSegs / dirIDs / dirMTime：键迁移（新旧路径同容器/同 dir_id/同 mtime）——三张表共用同一规则。
	s.dirSegs = shiftDirKeys(s.dirSegs, from, to, prefix)
	s.dirIDs = shiftDirKeys(s.dirIDs, from, to, prefix)
	s.dirMTime = shiftDirKeys(s.dirMTime, from, to, prefix)
	// index：from 下文件键迁移。
	newIndex := make(map[string]*metaEntry, len(s.index))
	for key, e := range s.index {
		if strings.HasPrefix(key, prefix) {
			newIndex[to+key[len(from):]] = e
		} else {
			newIndex[key] = e
		}
	}
	s.index = newIndex
	// dirs / dirParents 由 dirSegs 重建（根不入 dirs/dirParents）。
	s.dirs = make(map[string]struct{}, len(s.dirSegs))
	s.dirParents = make(map[string]string, len(s.dirSegs))
	for d := range s.dirSegs {
		if d == "" {
			continue
		}
		s.dirs[d] = struct{}{}
		s.dirParents[d] = parentDirOf(d)
	}
}

// renameFileLocked 文件跨目录移动（basename 不变）：blob 自包含 → 物理复制分块 + 文件
// meta 到目标容器（blob 内容零改动，首段哈希不变、重启后在新容器路径解析）→ 删源副本
// （best-effort，残留孤儿交由 GC 上收）→ 索引迁移。文件改名（basename 变）返回错误引导
// 走 delete+write。调用方持有 s.mu；失败不落半态（先复制全部成功再删源，失败回滚已复制）。
//
// **乐观锁版本语义（Imp-1 对齐）**：文件移动是「内容零改动」——meta.BaseVersion 保持
// 原值落盘（复制不变），卷版本内存推进仅用于同进程后续写 CAS。跨进程/重启 CAS 以**内容
// 版本**为基准：移动不改内容 → 磁盘 meta.BaseVersion 不变 → 另一进程重启后 loadIndex
// volVersion 从磁盘恢复与移动前一致，后续写 expected 基于内容版本仍成立（与写/删推进
// 内容版本不同，移动不产生新内容版本，故不落盘推进——设计 §6.2「零改内容、哈希不变」）。
func (s *SecretdataFS) renameFileLocked(ctx context.Context, from, to string) error {
	e, ok := s.index[from]
	if !ok || e.meta == nil {
		return fmt.Errorf("secretdata: 文件 %q 不存在", from)
	}
	targetParent := parentDirOf(to)
	targetContainer := s.dirSegs[targetParent]
	if targetContainer == "" {
		return fmt.Errorf("secretdata: 目标目录 %q 不存在或无容器", targetParent)
	}
	if _, exists := s.index[to]; exists {
		return fmt.Errorf("secretdata: 目标 %q 已是文件", to)
	}
	sameBase := path.Base(from) == path.Base(to)
	if sameBase && e.dirSeg == targetContainer {
		return nil // 同容器同 basename 同路径（from==to 已早退），防御性无操作
	}

	// **改名 = 只重写 meta blob**（文件名只存在于加密 meta 内 ⇒ `Original.Name`）：
	// 分块内容零改动、**不重加密文件内容**（用户口径 2026-10-13）。理由与安全性：
	//   - 分块 blob 名 = 密文内容哈希，与逻辑名无关 ⇒ 改名不必动分块；
	//   - meta blob 名（`MetaName`）派生自密文内容 ⇒ 改名后必须换名重写（新 salt 沿用
	//     `m.Salt`，故**不改内容密钥**、不动分块）；
	//   - **先写新 meta 成功、再删旧 meta**：崩溃窗口最多留下两份 meta（重启后旧路径仍可读，
	//     多一个副本而已），绝不出现「两份都没有」的丢数据方向。
	moved := *e
	if !sameBase {
		name, nm, merr := s.writeRenamedMetaLocked(ctx, e, targetContainer, path.Base(to))
		if merr != nil {
			return merr
		}
		moved.meta = nm
		moved.metaName = name
	}
	if rerr := s.relocateRenamedBlobsLocked(ctx, from, e, targetContainer, sameBase); rerr != nil {
		if !sameBase {
			// 回滚刚写的新 meta（避免重启后新路径出现重影条目）。
			_ = s.inner.Delete(ctx, path.Join(targetContainer, moved.metaName))
		}
		return rerr
	}
	moved.dirSeg = targetContainer
	delete(s.index, from)
	s.index[to] = &moved
	addDirKeysLocked(s.dirs, to)
	// 源目录若因移出最后一个文件而成空 → 一并回收（Imp-1 空目录语义一致）。
	s.pruneEmptyDirsLocked(ctx, parentDirOf(from))
	s.volVersion++
	return nil
}

// writeRenamedMetaLocked 按新 basename **只重加密 meta blob**（分块内容零改动），写入
// 目标容器并返回 (新 meta blob 名, 新 meta)。沿用原 `Salt` ⇒ 内容密钥与分块均不变。
// 先写成功才返回 ⇒ 崩溃窗口最多多一份 meta（不丢数据）。
// 调用方持有 s.mu。
func (s *SecretdataFS) writeRenamedMetaLocked(ctx context.Context, e *metaEntry, targetContainer, newBase string) (string, *shardseal.Meta, error) {
	nm := cloneMeta(e.meta)
	nm.Original.Name = newBase
	name, blob, merr := s.encryptMetaBlob(nm)
	if merr != nil {
		return "", nil, merr
	}
	// **失败即清理**（P2 修复，2026-10-19）：改名新 meta 是「写后登记」族漏点——内层写成功但
	// 复核失败时该 blob 已落远端，调用方的回滚只在**后续步骤**失败时执行 ⇒ 这里是唯一收口点。
	if werr := s.uploadAndCleanupOnFail(ctx, path.Join(targetContainer, name), blob, e.mtime); werr != nil {
		return "", nil, werr
	}
	return name, nm, nil
}

// relocateRenamedBlobsLocked 处理改名后的 blob 布局（调用方已按需写好新 meta），并删源：
//   - 同容器：分块原地不动，只删旧 meta（**O(meta)**，不重加密内容）；
//   - 跨容器 + 同 basename：meta 原样随迁（名字未变，无需重写）；
//   - 跨容器 + 改名：只搬内容分块（**绝不复制旧 meta**——否则重启会按旧 basename 重建幽灵条目）。
//
// 删源为 best-effort（残留孤儿交 GC）；调用方持有 s.mu。
func (s *SecretdataFS) relocateRenamedBlobsLocked(ctx context.Context, from string, e *metaEntry, targetContainer string, sameBase bool) error {
	if e.dirSeg == targetContainer {
		_ = s.inner.Delete(ctx, path.Join(e.dirSeg, e.metaName))
		return nil
	}
	// **共享集合搬不动，只能复制一份**（独立审核 P1，2026-10-18）：同容器复制会让两份 meta
	// 引用**同一批分块 blob**（存在即引用）。若此处按 mv 物理搬走分块，兄弟条目的 meta 仍指向
	// **原容器**里的分块名 ⇒ 其 OpenRead 报「读底层分块失败」，而 ListDir/Stat 仍可见（size
	// 正常）⇒ 用户侧表现为**静默数据丢失**（幽灵条目）。⇒ 被兄弟引用时改**字节拷贝**（内容
	// 零改动、不重加密），原分块留给兄弟；且**不得** `deleteFileBlobs`（那会删掉兄弟的分块）。
	if s.chunkSetStillReferenced(from, e) {
		// 已复制项**不**在此回滚：后续步骤（meta 搬移）失败时残留的目标 blob 交既有可选 GC 上收
		// ——目标容器可能已有**同内容** blob（内容寻址命名）被他人引用，盲目删除会破坏他人条目。
		if _, cerr := s.copyContentBlobs(ctx, e, targetContainer); cerr != nil {
			return cerr
		}
		srcMeta := path.Join(e.dirSeg, e.metaName)
		if sameBase {
			// meta 名派生自分块内容 ⇒ 改名/改路径不换名 ⇒ 必须搬到目标容器，否则目标条目解析不到。
			if merr := s.inner.Rename(ctx, srcMeta, path.Join(targetContainer, e.metaName)); merr != nil {
				return fmt.Errorf("secretdata: 搬移 meta blob 失败: %w", merr)
			}
		} else {
			// 新 meta 已由 writeRenamedMetaLocked 写好 ⇒ 删源 meta（**只删 meta**，不碰分块）。
			_ = s.inner.Delete(ctx, srcMeta)
		}
		return nil
	}
	// **崩溃安全：复制分块 → 搬/换 meta → 最后删源分块**（P1 修复，2026-10-18）。
	//
	// 旧实现逐个 blob 调 `s.inner.Rename`（**真 mv，源立刻消失**）且 **meta 最后**搬 ⇒ 崩在
	// 中途时：源容器 meta 仍在、其引用的分块却已被搬走（源 `OpenRead` 报「读底层分块失败」，
	// 但 `Stat`/`ListDir` 仍可见 ⇒ 静默数据丢失），目标容器的分块又无任何 meta 引用 ⇒ 开启 GC
	// 后被当孤儿**物理清扫** ⇒ **单文件永久不可恢复** ✗。确定性用例
	// `TestRename_CrossContainer_CrashAtAnyStep_NoDataLoss` 枚举**每一个**崩溃点实证（修复前
	// k=1..8 全部「源与目标都不可读」）；该用例同时钉住与文档 §5「文件移动 = 物理**复制**分块
	// → 删源副本」以及本函数旧注释「先复制全部成功再删源」的一致性。
	//
	// 新序的每个崩溃点都保底：① 复制内容分块 + parity 时**源完好**（失败回滚已复制项）；
	// ② 搬/换 meta 是**单文件原子**步；③ **最后**才删源分块。⇒ 崩溃后要么源完整、要么目标
	// 完整，**不丢数据**。代价：同卷跨容器移动不再是零拷贝 mv 而是字节复制（正确性优先；
	// 跨容器移动是低频操作，**同容器改名/移动**仍走 O(meta) 零 blob 写路径）。
	// 已复制项不在此回滚（同上：目标容器可能已有同内容 blob 被他人引用；残留交 GC）。
	if _, cerr := s.copyContentBlobs(ctx, e, targetContainer); cerr != nil {
		return cerr
	}
	if sameBase {
		// meta blob 名派生自其内容 ⇒ 改名/改路径不换名 ⇒ 必须搬到目标容器（单文件原子）。
		if merr := s.inner.Rename(ctx, path.Join(e.dirSeg, e.metaName),
			path.Join(targetContainer, e.metaName)); merr != nil {
			return fmt.Errorf("secretdata: 搬移 meta blob 失败: %w", merr)
		}
	} else {
		// 新 meta 已由 writeRenamedMetaLocked 写好 ⇒ 删源 meta（**只删 meta**，不碰分块）。
		_ = s.inner.Delete(ctx, path.Join(e.dirSeg, e.metaName))
	}
	// **最后**删源内容分块（+parity）：此刻目标已自洽 ⇒ 删除失败/崩溃只留下孤儿分块
	// （可选 GC 上收），不会造成任何一侧不可读。
	s.deleteContentBlobs(ctx, e)
	return nil
}

// copyContentBlobs 把条目的**内容分块 + parity** 物理复制到目标容器（不复制 meta——
// 改名路径的 meta 由 renameFileLocked 单独按新名写入）。内容零改动（纯字节复制，保 blob
// mtime）；去重文件分块在池容器 ⇒ 无需搬移，直接返回。失败回滚已复制项。
func (s *SecretdataFS) copyContentBlobs(ctx context.Context, e *metaEntry, targetContainer string) ([]string, error) {
	if e.dataDir != "" {
		return nil, nil // 去重文件：分块在池容器（全卷共享）⇒ 不拷贝（副本保留 dataDir 指向池）
	}
	var uploaded []string
	rollback := func() { s.rollbackWrite(ctx, uploaded, nil) } // 有界且不被调用方取消打断（P2 修复 2026-10-19）
	for _, ci := range e.meta.Chunks {
		dst := path.Join(targetContainer, ci.FileName)
		// **先登记后拷贝**（P2 修复，2026-10-19）：内层「写成功但复核失败」时目标 blob 已落远端，
		// 登记在后即漏 ⇒ 孤儿。
		uploaded = append(uploaded, dst)
		if cerr := s.copyBlob(ctx, path.Join(e.dirSeg, ci.FileName), dst, e.mtime); cerr != nil {
			rollback()
			return nil, cerr
		}
	}
	// Erasure parity：随文件一并搬移（目标保留 XOR 冗余、源 parity 不再孤立）。
	if p := e.meta.Parity; p != nil {
		dst := path.Join(targetContainer, p.FileName)
		uploaded = append(uploaded, dst)
		if cerr := s.copyBlob(ctx, path.Join(e.dirSeg, p.FileName), dst, e.mtime); cerr != nil {
			rollback()
			return nil, cerr
		}
	}
	return uploaded, nil
}

// deleteContentBlobs 删条目在源容器的**内容分块 + parity**（**不动 meta**）：跨容器移动的
// 收尾步（目标已自洽后才调用）。去重引用条目的分块在池容器（共享）⇒ 不删（归 unrefPoolEntry
// 裁决）。best-effort：失败残留孤儿，交可选 GC 上收。
func (s *SecretdataFS) deleteContentBlobs(ctx context.Context, e *metaEntry) {
	if e.dataDir != "" {
		return
	}
	// 调用方持 s.mu（移动/删除收尾）⇒ 远端删除必须**有界**（挂起即持锁拖死整卷，P2 修复 2026-10-19）。
	cctx, cancel := boundedCleanupCtx(ctx)
	defer cancel()
	for _, ci := range e.meta.Chunks {
		_ = s.inner.Delete(cctx, path.Join(e.dirSeg, ci.FileName))
	}
	if p := e.meta.Parity; p != nil && p.FileName != "" {
		_ = s.inner.Delete(cctx, path.Join(e.dirSeg, p.FileName))
	}
}

// copyBlob 物理复制底层 blob 到目标路径（保持源 blob mtime，移动不改物理时间戳；
// 内容零改动 → 哈希不变）。
func (s *SecretdataFS) copyBlob(ctx context.Context, src, dst string, mtime int64) error {
	data, rerr := readBlob(ctx, s.inner, src)
	if rerr != nil {
		return fmt.Errorf("secretdata: 读源 blob %s 失败: %w", src, rerr)
	}
	mt := mtime
	if e, serr := s.inner.Stat(ctx, src); serr == nil && e != nil && e.MTime > 0 {
		mt = e.MTime
	}
	if werr := s.uploadBlob(ctx, dst, data, mt); werr != nil {
		return fmt.Errorf("secretdata: 写入目标 blob %s 失败: %w", dst, werr)
	}
	return nil
}

// pruneEmptyDirsLocked 从 from 起向上回收空目录（Imp-1：删干净、重启不复现）：目录无文件
// 且无子目录 → 删除其容器残留（目录 meta + 遗留/孤儿分块）并注销映射；遇到非空目录或根
// 停止。调用方持有 s.mu（deleteFile 在 Lock 下调用）。
func (s *SecretdataFS) pruneEmptyDirsLocked(ctx context.Context, from string) {
	// **有在途写时不回收容器**（P0 修复，2026-10-21）：写路径在 ensureContainer 与 commitLocked
	// **之间不持锁**上传分块/meta；此刻 dirHasContentLocked 只看 s.index（新条目尚未提交）⇒
	// 会判「空目录」并 removeEmptyContainerLocked 把在途写**已上传的分块与 meta 全部删掉**
	// （随后 commit 仍返回成功 ⇒ 幽灵条目；若底层重建容器目录则留下无目录 meta 的容器，
	// 重启后整容器 fail-closed 跳过 ⇒ 已确认的上传永久消失）。
	// 判据与 GC 一致（GC 亦有 writesInFlight>0 即放弃本轮的门禁）：代价只是空容器延后回收
	// ——下一次删除/GC 会再收。
	if s.writesInFlight > 0 {
		return
	}
	d := from
	for d != "" {
		if s.dirHasContentLocked(d) {
			return
		}
		container, ok := s.dirSegs[d]
		if !ok {
			return // 无容器（虚拟/已删）
		}
		s.removeEmptyContainerLocked(ctx, container, d)
		d = parentDirOf(d)
	}
}

// findDirMetaBlob 定位容器内的目录 meta（q 标记）并读回其加密 blob。
func findDirMetaBlob(ctx context.Context, inner syncpkg.FS, container string) (string, []byte, error) {
	entries, err := inner.ListDir(ctx, container)
	if err != nil {
		return "", nil, fmt.Errorf("secretdata: 列容器 %s 失败: %w", container, err)
	}
	for _, f := range entries {
		if f.IsDir || shardseal.ClassifyName(f.Name) != shardseal.KindDirMeta {
			continue
		}
		b, rerr := readBlob(ctx, inner, path.Join(container, f.Name))
		if rerr != nil {
			continue
		}
		return f.Name, b, nil
	}
	return "", nil, fmt.Errorf("secretdata: 容器 %s 无目录 meta，无法更新目录引用", container)
}

// writeDirMetaLocked 加密落盘目录 meta（新随机盐 + 新 blob 名，沿用原 mtime）。
func (s *SecretdataFS) writeDirMetaLocked(ctx context.Context, container string, dm *dirMeta) (string, error) {
	salt, serr := shardseal.RandSalt()
	if serr != nil {
		return "", serr
	}
	key, kerr := shardseal.DeriveKey(s.secret, salt, s.algoVer)
	if kerr != nil {
		return "", kerr
	}
	dmJSON, merr := json.Marshal(dm)
	if merr != nil {
		return "", merr
	}
	blob, berr := shardseal.EncryptMetaJSON(key, salt, dmJSON, s.metaPadTarget())
	if berr != nil {
		return "", berr
	}
	// 新命名：首/末段 = 同一加密 dir-meta blob 的两窗口（offset 0/16）base62；
	// 中段 = 目录随机 id（base62 9 字符，与生成端一致）。
	blobA, blobB := shardseal.Hash48Pair(blob, 0, 16)
	name, nerr := shardseal.DirMetaName(blobA, dm.DirID, blobB)
	if nerr != nil {
		return "", nerr
	}
	mt := dirMetaMTime(dm.MTime)
	// **失败即清理**（P2 修复，2026-10-19）：内层「写成功但复核失败」时 dir meta blob 已落远端，
	// 不删即永久孤儿（此前是「写后登记」族漏点之一）。
	if werr := s.uploadAndCleanupOnFail(ctx, path.Join(container, name), blob, s.blobMTime(mt)); werr != nil {
		return "", fmt.Errorf("secretdata: 写目录 meta %s 失败: %w", name, werr)
	}
	return name, nil
}

// dirMetaMTime 把目录 meta 的 RFC3339Nano 时间串转回 UnixNano（写新 meta 沿用原时间）。
func dirMetaMTime(s string) int64 {
	if s == "" {
		return 0
	}
	if tm, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return tm.UnixNano()
	}
	return 0
}

// metaPadTarget 返回文件/目录 meta 加密 pad 目标（整块落盘总长），默认 = Block.Min
// 抖动态：[base, 2×base)，受统一格式 R 地板（8B 长度头：128+8+32+12+16=196B）约束向下钳制到 ≥196。
// MetaPadBytes 为 sizex.ByteSize，内部按 int64 计算（显式转换）。
func (s *SecretdataFS) metaPadTarget() int {
	base := int64(s.opts.MetaPadBytes)
	if base <= 0 {
		base = s.opts.Block.Min
	}
	if base <= 0 {
		base = 196
	}
	t := max(base+shardseal.RandN(base), 196)
	return int(t)
}

// mtimeString 把 UnixNano 转 RFC3339Nano（目录 meta 的 mtime 字段）。
func mtimeString(mtime int64) string {
	if mtime <= 0 {
		return ""
	}
	return time.Unix(0, mtime).UTC().Format(time.RFC3339Nano)
}

// ---- 本地 staging 配额自管（StagingQuotaCapable，2026-10-18）----
//
// 为什么自管而非让装配层的 StagingQuotaGateFS 代理记账：
//   - gate 只能看见**经本卷 FS 的 WriteFile**，按「逻辑上传量」代理预留——而本卷真正的本地
//     临时物是 temp 目录里的**加密分块目录**（≈ 明文大小）**加每个文件的 meta blob pad**
//     （1–2MiB），gate 既看不到 meta pad、也无法与实际并发临时占用对齐；
//   - 本卷写入目标卷（内层 FS）的字节是**最终数据**、不是 staging——由内层卷自己的容量/配额
//     记账 ⇒ 由本卷自管可避免与内层卷**双计**。
//
// 复用同一套机制（同一个 `syncpkg.StagingQuotaTracker` + 装配层独立 staging Scope），
// 仅把记账点从「gate 代理」改为「本卷在真正写 temp 前后预留/释放」。
var _ syncpkg.StagingQuotaCapable = (*SecretdataFS)(nil)

// WithStagingQuota 注入本地 staging 配额句柄（装配层调用；nil = 不记账）。
func (s *SecretdataFS) WithStagingQuota(q syncpkg.StagingQuotaTracker) {
	s.stagingMu.Lock()
	s.stagingQuota = q
	s.stagingMu.Unlock()
}

// stagingTracker 读取当前配额句柄（未注入 ⇒ nil）。
func (s *SecretdataFS) stagingTracker() syncpkg.StagingQuotaTracker {
	s.stagingMu.RLock()
	defer s.stagingMu.RUnlock()
	return s.stagingQuota
}

// stagingReserve 为一次写路径的**本地临时物**预留 staging 配额，返回幂等释放函数。
// 记账口径 = 明文大小的**密文近似上界**（`cipherPeak`：本地分块目录装的是密文块 > 明文；
// 「近似」= video-keyframe 密集关键帧可超出该比值，见 cipherPeak 注释）
// + **2×metaPadTarget**（每文件一个 meta blob 的 pad 上界）。预留失败（配额不足/ctx 取消）
// ⇒ 调用方必须 fail-closed（不落临时物）。未装配配额（nil）⇒ 直通（返回 no-op 释放），
// 与 gate 的零回归口径一致。
func (s *SecretdataFS) stagingReserve(ctx context.Context, size int64) (func(), error) {
	// 密文块总量上界 + 纠错卷 parity 段上界 + meta pad 上界。后者**不是**多余余量：本路径
	// （内存写路径 / 去重）经 `EncryptShardsBytes` 的落盘形态产出 meta ⇒ temp 内确有 1 份
	// meta blob（大小 ≈ metaPadTarget）⇒ 记 2×（1× 实际 + 1× 抖动/实现余量）。
	// （块级交错路径的 meta 才是内存态直传，见 stagingReserveInterleaved。）
	need := cipherPeak(max(size, 0)) + s.erasureParityPeak() + 2*int64(s.metaPadTarget())
	return s.stagingReserveBytes(ctx, need)
}

// cipherPeak 返回**明文大小 ≤ plainBytes 的单块密文长度近似上界**（staging 预留口径，P2 修复
// 2026-10-19）：本地分块副本是**密文块**（sink 形态直接落加密后的 blob），其长度 > 明文——
// 密文 = [blob 头 R128 + 8B 长度 + 32B salt] + boot 段 + 段序列（每段 12B nonce + 明文 +
// 16B tag）+ 段间随机间隙（<64B each）+ 索引块（每段一条 JSON 条目）。按明文预留会**低估**
// 实际落盘量（多段/多间隙时尤甚），故统一按含头/tag/索引的近似上界预留。
//
// 近似口径取 plain + plain/32 + 512：plain/32（≈3.1%）覆盖每段 nonce/tag/间隙/索引开销——
// fixed 段规划实测 <0.2%（4MB 段）；**但 video-keyframe 的密集关键帧**会产生远超 plain/32 的
// 极小段（段头/索引开销占比放大）⇒ 本值是**工程近似、非形式化最坏界**（staging 是本卷自管的
// 软门，超预留不损坏数据，仅可能触发内存降级）。512B 覆盖 blob 固定头 + boot 段 + 索引块头 + 抖动余量。
func cipherPeak(plainBytes int64) int64 {
	if plainBytes <= 0 {
		return 0
	}
	return plainBytes + plainBytes/32 + 512
}

// erasureParityPeak 返回纠错卷额外写入本地 temp 的 parity 段上界：parity 明文 ≤ 单分块明文
// 上界（policy.Max）⇒ 其密文上界 = cipherPeak(policy.Max)。此前 staging 预留漏计 parity，
// 使纠错卷本地临时物被低估（P2 修复，2026-10-19）。非纠错卷 / 无有效块上界 ⇒ 0。
// 注：parity 仅在分块数 ≥2 时生成；单分块文件会多预留一个块上界（保守，不误拒大文件）。
func (s *SecretdataFS) erasureParityPeak() int64 {
	if !s.opts.Erasure || s.opts.Block.Max <= 0 {
		return 0
	}
	return cipherPeak(int64(s.opts.Block.Max))
}

// stagingReserveInterleaved 为**块级交错**写路径预留本地 staging 配额：本地峰值 = 单个分块密文
// 上界（≤ policy.Max）+ meta pad 上界，而**不是整份文件** ⇒ 不再随文件大小线性增长
// （docs/roadmap.md ①-附：「空间不足」在多数场景自然消失）。
func (s *SecretdataFS) stagingReserveInterleaved(ctx context.Context, size int64, policy shardseal.BlockPolicy) (func(), error) {
	peak := max(size, 0)
	if policy.Max > 0 && int64(policy.Max) < peak {
		peak = int64(policy.Max)
	}
	// 本地副本是**密文块**（> 明文）⇒ 按密文上界预留（cipherPeak），避免低估导致实际落盘
	// 超出预留（P2 修复，2026-10-19）。**不再叠加 meta pad**：本路径 meta 走内存态直传、
	// 不落 temp（P2 修复，2026-10-19）——此前叠加 2×metaPad（默认 1–2MiB）使小块文件预留
	// 3–5× 实际 temp，触发不必要的内存降级。纠错不走本路径（流式与 Erasure 互斥），故无 parity 项。
	return s.stagingReserveBytes(ctx, cipherPeak(peak))
}

// stagingReserveBytes 按**显式字节数**预留 staging 配额（口径见 stagingReserve 注释）。
func (s *SecretdataFS) stagingReserveBytes(ctx context.Context, need int64) (func(), error) {
	q := s.stagingTracker()
	if q == nil {
		return func() {}, nil
	}
	if need <= 0 {
		return func() {}, nil
	}
	if err := q.ReserveUsage(ctx, need); err != nil {
		// 包**哨兵**（%w 链首）：上层按 errors.Is 判定降级原因，不匹配文案子串（P2 修复，2026-10-19）。
		return nil, fmt.Errorf("secretdata: %w（防本地磁盘打满）: %w", syncpkg.ErrStagingQuotaExceeded, err)
	}
	var once sync.Once
	return func() { once.Do(func() { q.ReleaseUsage(need) }) }, nil
}

// parentDirOf 返回逻辑文件 rel 所在逻辑目录（根为 ""）。
func parentDirOf(rel string) string {
	d := path.Dir(rel)
	if d == "." || d == "/" {
		return ""
	}
	return strings.Trim(d, "/")
}

// addDirKeysLocked 登记 rel 的全部祖先目录（不含 rel 自身）到 dirs 集合。
// 调用方需持有 s.mu。
func addDirKeysLocked(dirs map[string]struct{}, rel string) {
	p := strings.Trim(rel, "/")
	for i := range p {
		if p[i] != '/' {
			continue
		}
		parent := strings.TrimSuffix(p[:i], "/")
		if parent != "" {
			dirs[parent] = struct{}{}
		}
	}
}

// removeVersionMeta 删除旧版本条目的底层分块与文件 meta（覆盖写清理 / Delete 即时物理删，
// best-effort）。去重引用条目：先删本容器 meta、再释放去重池引用（归零物理删池 blob）。
//
// **删除顺序（修复轮 Imp-1：先删 meta、再删分块）**：meta 是「重启重建」的锚点——
//   - meta 先删：meta 删失败 → 分块全在 → 重启重建**完整文件**（删除丢失的安全失败，无
//     幽灵/坏数据）；meta 删成功而分块删失败 → 无 meta → 重启不重建，分块成孤儿（可选 GC
//     上收，孤儿方向，§6.2 已记录）。
//   - 反向（分块先删、meta 后删）的幽灵方向（破坏 §6.2「重启不复现」）：meta 删失败但
//     分块已删 → 重启把非 Deleted 的 meta 重建为条目（meta 声明 size、分块已缺、OpenRead
//     失败），且 GC 视非 Deleted meta 为存活而永久保护不清理。**故 meta 恒最先删**。
func (s *SecretdataFS) removeVersionMeta(ctx context.Context, ownerKey string, e *metaEntry) {
	if e == nil || e.meta == nil {
		return
	}
	// 远端删除一律走**有界且不被调用方取消打断**的清理 ctx（P2 修复，2026-10-19）：本函数
	// 在 s.mu **写锁内**被调用（commitEntry / deleteFile / dedup 提交），远端 Delete 挂起会
	// 持锁拖死整卷；裸 context.Background() 则完全没有上限。与 rollbackWrite 同一机制。
	cctx, cancel := boundedCleanupCtx(ctx)
	defer cancel()
	// 先删本容器 meta（重启重建锚点）：meta 删**失败** → 立即返回、分块全保留 → 重启重建
	// 完整文件（删除丢失的安全失败，无幽灵/坏数据）；meta 删成功才继续删分块 + parity
	// （分块删失败 → 无 meta 引用，成孤儿，可选 GC 上收）。
	if err := s.inner.Delete(cctx, path.Join(e.dirSeg, e.metaName)); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// **可观测**（P2 修复，2026-10-21）：此前完全静默 ⇒ 用户删除「成功」（条目已移出
			// 索引）但密文与 meta 仍在盘上；同进程内再删是幂等 no-op、GC 又把「非墓碑 = 存活」
			// 的 meta 保护起来 ⇒ 只能重启后再删一次，运维无从发现。方向仍保持「先删 meta」的
			// 安全失败语义（分块全保留 ⇒ 重启重建完整文件），只补告警。
			slog.Warn("secretdata: 删除条目 meta 失败（条目已移出索引，物理残留待重启后重试/由 GC 上收）",
				"key", ownerKey, "meta", e.metaName, "container", e.dirSeg, "error", err)
		}
		return // meta 删失败：分块保留（反幽灵：meta 声明的 size 与分块俱在）
	}
	if e.dataDir != "" {
		// 预留去重池记账（opts.Dedup 时生效；池 blob 的物理删由下方存在性判定统一裁决）。
		s.unrefPoolEntry(cctx, e)
	}
	// **共享分块（同卷复制）的「存在即引用」裁决**：仍有其他条目引用同一
	// (数据容器, 分块名集合) ⇒ 不删（源与副本谁先删都安全）；否则物理删分块 + parity。
	// 不以 dataDir/标记为前提（布局事实由 `dataSeg` 表达；重启后由索引天然恢复）。
	if s.chunkSetStillReferenced(ownerKey, e) {
		return
	}
	dir := s.dataSeg(e)
	for _, ci := range e.meta.Chunks {
		_ = s.inner.Delete(cctx, path.Join(dir, ci.FileName))
	}
	if p := e.meta.Parity; p != nil && p.FileName != "" {
		_ = s.inner.Delete(cctx, path.Join(dir, p.FileName))
	}
}

// chunkSetStillReferenced 判定「该条目的分块集合是否仍被**其他**条目共享引用」
// （同卷复制的**存在即引用**引用计数）：按内存索引比对同一数据容器 + 同名分块集合。
//
// 为什么不需要持久化引用计数：共享关系就是「两份 meta 引用同一批 blob」这一事实本身，
// 重启后由索引重建天然恢复；删除时按索引判定最后引用者（O(索引)），因此**源与副本谁先
// 删都安全**，且无需任何新元数据字段/标记。ownerKey 是正在删除或覆盖的条目键（覆盖时索
// 引已指向新条目），必须排除以免把自己算作引用者。调用方须持 s.mu。
func (s *SecretdataFS) chunkSetStillReferenced(ownerKey string, e *metaEntry) bool {
	if e == nil || e.meta == nil {
		return false
	}
	dir := s.dataSeg(e)
	for k, other := range s.index {
		if k == ownerKey || other == nil || other.meta == nil {
			continue
		}
		if s.dataSeg(other) != dir {
			continue
		}
		if sameChunkNames(other.meta, e.meta) {
			return true
		}
	}
	return false
}

// sameChunkNames 判定两份 meta 是否引用同一批分块 blob（同容器 + 同名集合；parity 一并比对）。
func sameChunkNames(a, b *shardseal.Meta) bool {
	if len(a.Chunks) != len(b.Chunks) {
		return false
	}
	for i := range a.Chunks {
		if a.Chunks[i].FileName != b.Chunks[i].FileName {
			return false
		}
	}
	if (a.Parity == nil) != (b.Parity == nil) {
		return false
	}
	return a.Parity == nil || a.Parity.FileName == b.Parity.FileName
}

// unrefPoolEntry 释放去重池引用：引用计数归零时物理删除池 blob。调用方须持 s.mu。
// 若池条目不存在或 blob 名与条目不符（并发 miss 的私有分块）→ 不递减，私有分块留待
// GC 清孤儿（不误删他人共享 blob）。
func (s *SecretdataFS) unrefPoolEntry(ctx context.Context, e *metaEntry) {
	if !s.opts.Dedup || e == nil || e.meta == nil || len(e.meta.Chunks) == 0 {
		return
	}
	key := e.meta.Chunks[0].OrigSHA256
	fileName := e.meta.Chunks[0].FileName
	b, ok := s.dedupPool[key]
	if !ok || b.name != fileName {
		return
	}
	b.refs--
	if b.refs <= 0 {
		if s.dedupDir != "" {
			// 与其它锁内远端删除同口径：**有界且不被调用方取消打断**（P2 修复，2026-10-19）。
			cctx, cancel := boundedCleanupCtx(ctx)
			_ = s.inner.Delete(cctx, path.Join(s.dedupDir, b.name))
			cancel()
		}
		delete(s.dedupPool, key)
	}
}

// chunkHash 是密文块的整块校验值（sink 顺带算，交给内层 UploadHints）。
type chunkHash struct{ md5, crc32 string }

// md5HexOf 计算密文块整块 md5（hex；sink 顺带算，交给内层 UploadHints.MD5——
// baidupcs 免整块重扫；密文是随机内容，md5 非安全用途，仅供百度 ETag 复核/秒传基准）。
func md5HexOf(b []byte) string {
	h := md5.Sum(b) //nolint:gosec // 见函数头注释
	return hex.EncodeToString(h[:])
}

// defaultBlockPolicyFor 返回未显式配置时的**默认分块策略**（按底层本地性分流，2026-10-21
// 用户裁定）：本地卷用 DefaultBlockPolicy（4MiB blocklet，零网络开销大段优）；外部/网盘卷用
// ExternalDefaultBlockPolicy（256KiB blocklet——网盘随机读实测 8 倍提速）。判据委托底层
// LocalVolume 自述（IsLocalVolume()，与 IsLocalVolume 方法同源）；未实现该接口 ⇒ 按外部
// 处理（外部卷无本地性自述是默认语义）。显式 opts.Block 由调用方覆盖，不进本函数。
func defaultBlockPolicyFor(inner syncpkg.FS) shardseal.BlockPolicy {
	if lv, ok := inner.(syncpkg.LocalVolume); ok && lv.IsLocalVolume() {
		return shardseal.DefaultBlockPolicy()
	}
	return shardseal.ExternalDefaultBlockPolicy()
}
