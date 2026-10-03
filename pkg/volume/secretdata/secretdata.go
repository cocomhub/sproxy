// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package secretdata 是 secret_data 加密封装卷（设计 docs/designs/2026-10-01-secret-volume.md
// §6.2）：wrapper 卷，写入手动分块加密上传底层卷随机容器目录、读取经 meta 解密还原。
// 实现 sync.FS，对上层透明（OpenRead/WriteFile/Stat/Delete/MakeDir 转发）。
//
// 卷内布局（无顶层 data/meta 结构词，全随机容器）：
//
//	<secretdata根>/<randDir 5-30>/           # 一个逻辑目录 = 一个随机命名容器目录
//	      目录meta（@ 标记，含逻辑 path，加密）
//	      文件meta（-/_ 标记，含文件根信息，加密）
//	      加密分块（无标记）
//
// 寻址 `secretdata://<卷名>/<path>`；旧卷加载 = 扫描容器目录 meta → path、文件 meta →
// name 重建完整路径索引 + 按需还原（修复 F-1：逻辑路径与索引键在写路径与重启恢复一致）。
package secretdata

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

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
	// **已知边界（M-6）**：去重内容池键 = Hash16（整文件 SHA-256 前 16 hex = **64-bit
	// 熵**）作内容寻址。64 位碰撞概率在单实例文件数量级（≤10^8）下可忽略（生日界
	// ~2^32），但碰撞即静默复用错误内容（两不同文件命中间一池键 → 引用同一 blob）。
	// 未来若支持跨实例大规模共享/内容寻址，应升级为完整 256-bit 键（meta.Extra 已可
	// 容纳）；当前单实例数量级保留 64-bit 权衡（键长与命名格式兼容）。
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
	// 背景（Imp-2 已知限制）：当前写路径为「先 io.ReadAll 全文 → 分块加密」，加密核心
	// 采用内存明文变体（EncryptShardsBytes，峰值 1× 文件）；未做逐块流式（blocklet 双层
	// 规划 + meta 名锚定全内容哈希要求先有完整明文）。故超限拦截是「大文件内存防护」的
	// 兜底，部署侧按卷容量/内存设置。
	MaxFileBytes sizex.ByteSize
}

// ErrMaxFileBytes 是单文件超上限哨兵错误（writeFile 在读取前拦截）。
var ErrMaxFileBytes = fmt.Errorf("secretdata: 文件超过单文件大小上限")

// ErrVersionConflict 是乐观锁版本冲突哨兵错误：WriteFileIfVersion/DeleteIfVersion 传入的
// 期望版本与卷当前版本不一致（多进程写前 CAS 失败）。
var ErrVersionConflict = fmt.Errorf("secretdata: 乐观锁版本冲突")

// metaEntry 是索引条目：逻辑文件路径 ↔ 底层容器/分块位置。
type metaEntry struct {
	size     int64
	mtime    int64  // 逻辑层 mtime（meta 内原始值，不被 blob mtime 打散影响）
	dirSeg   string // 随机容器目录名（5-30）
	metaName string // 文件 meta 加密 blob 名（含 -/_ 标记）
	meta     *shardseal.Meta
	// dataDir 是数据分块所在的容器目录。默认 = dirSeg（自包含 blob）；去重引用文件时
	// 指向卷级去重容器（分块在该容器，meta blob 仍在 dirSeg）。"" = 与 dirSeg 相同。
	dataDir string
	// baseVersion 是本条目的乐观锁版本（来自 meta.BaseVersion，写/删 CAS 用）。
	baseVersion int64
}

// dedupBlob 是去重内容池条目：一个整文件内容对应一个共享加密分块 blob 与其引用计数。
// 计数 = 当前卷内引用该 blob 的文件 meta 数；归零物理删。meta 是首个写入者的完整 Meta
// 模板（Salt/Chunks/Extra），供后续引用文件克隆（读路径复用同一 salt/key）。
type dedupBlob struct {
	name string // dedupDir 内的分块 blob 文件名（内容寻址三段哈希）
	refs int64
	meta *shardseal.Meta
}

// dirMeta 是目录 meta 的加密 JSON（@ 标记，记录本目录逻辑名与父目录 dir_id；目录移动/改名
// 仅重写本容器 meta 的 Name/ParentDirID 一个文件，子树零改动——目录间完全解耦）。
// 旧格式（存完整逻辑 path，耦合父目录）loadIndex fail-closed（未上线，见 decryptDirMeta）。
type dirMeta struct {
	Version     int    `json:"version"`
	Algorithm   string `json:"algorithm"`
	KDF         string `json:"kdf"`
	Type        string `json:"type"`                    // "dir"
	Name        string `json:"name"`                    // 本目录逻辑名（不含路径；根容器为空串）
	ParentDirID string `json:"parent_dir_id,omitempty"` // 父目录 dir_id（根为空）
	DirID       string `json:"dir_id"`                  // 本目录唯一 ID（16 hex）
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

	mu      sync.RWMutex
	index   map[string]*metaEntry
	dirs    map[string]struct{} // 已知逻辑目录（子目录发现/进入用）
	dirSegs map[string]string   // 逻辑目录 → 随机容器目录名
	// dirIDs 是逻辑目录 → 该容器目录 meta 的 dir_id（写新容器 meta 的 parent_dir_id、
	// 目录移动改写父引用用）。
	dirIDs map[string]string
	// dirParents 是逻辑目录 → 其父逻辑目录（空目录回收/子树判定用；根无条目）。
	dirParents map[string]string
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
		opts.Block = shardseal.DefaultBlockPolicy()
	}
	// Erasure 与 Dedup 组合未支持（去重文件走卷级单 blob 池，无可纠的 k 分块）：
	// 显式互斥 fail-closed，避免组合下用户以为有纠错而实际没有。
	if opts.Erasure && opts.Dedup {
		return nil, fmt.Errorf("secretdata: Erasure 与 Dedup 互斥（纠错仅常规写路径，组合未支持）")
	}
	ownedTemp := false
	if opts.TempDir == "" {
		// 默认临时目录：os.MkdirTemp 生成随机名（不可预测 + 0700），避免
		// os.TempDir() 下固定可预测的公开可写路径（Sonar go:S5445）。
		dir, err := os.MkdirTemp("", "sproxy-secretdata-")
		if err != nil {
			return nil, fmt.Errorf("secretdata: 创建默认临时目录失败: %w", err)
		}
		opts.TempDir = dir
		ownedTemp = true
	}
	// 显式配置的 TempDir 同样收紧为 0700 并在创建时校验归属。
	if err := os.MkdirAll(opts.TempDir, 0o700); err != nil {
		return nil, fmt.Errorf("secretdata: 创建临时目录 %s 失败: %w", opts.TempDir, err)
	}
	fs := &SecretdataFS{
		inner: inner, secret: opts.Secret, opts: opts, algoVer: algoVer, temp: opts.TempDir, ownedTemp: ownedTemp,
		index: map[string]*metaEntry{}, dirs: map[string]struct{}{}, dirSegs: map[string]string{},
		dirIDs: map[string]string{}, dirParents: map[string]string{}, keyCache: newDeriveCache(64),
		loadGate:  make(chan struct{}, maxParallelLoads(&alg)),
		dedupPool: map[string]*dedupBlob{},
	}
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
func (b *backend) Close() error {
	if b.fs.gcCancel != nil {
		b.fs.gcCancel() // 停止后台 GC goroutine
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
func entryOf(rel, seg string, size int64, isDir bool) syncpkg.Entry {
	p := path.Join(rel, seg)
	return syncpkg.Entry{Name: seg, Path: p, Size: size, IsDir: isDir}
}

// ListDir 列出逻辑 rel 目录下的直接子项（透明子目录可发现/进入）。
// 索引只存完整逻辑路径文件键；这里对每个路径把下一段作为子项聚合，
// 跨段前缀呈现为目录条目（审查 F-2：ListDir 永不返回子目录）。
func collectChildSegments(paths map[string]*metaEntry, dirs map[string]struct{}, prefix, rel string, entrySize func(*metaEntry) int64) []syncpkg.Entry {
	out := collectIndexSegments(paths, prefix, rel, entrySize)
	return append(out, collectDirSegments(dirs, prefix, rel)...)
}

// collectIndexSegments 从 index 文件键聚合：首段为目录则目录条目，否则文件条目。
func collectIndexSegments(paths map[string]*metaEntry, prefix, rel string, entrySize func(*metaEntry) int64) []syncpkg.Entry {
	var out []syncpkg.Entry
	seenDirs := map[string]bool{}
	for p := range paths {
		if !strings.HasPrefix(p, prefix) || len(p) <= len(prefix) {
			continue
		}
		rest := p[len(prefix):]
		if before, _, has := strings.Cut(rest, "/"); has {
			if !seenDirs[before] {
				out = append(out, entryOf(rel, before, 0, true))
				seenDirs[before] = true
			}
			continue
		}
		out = append(out, entryOf(rel, rest, entrySize(paths[p]), false))
	}
	return out
}

// collectDirSegments 从 dirs 目录键聚合：只贡献目录条目（空/字节目录可见性）。
func collectDirSegments(dirs map[string]struct{}, prefix, rel string) []syncpkg.Entry {
	var out []syncpkg.Entry
	seenDirs := map[string]bool{}
	for d := range dirs {
		if !strings.HasPrefix(d, prefix) || len(d) <= len(prefix) {
			continue
		}
		rest := d[len(prefix):]
		if before, _, has := strings.Cut(rest, "/"); has {
			if !seenDirs[before] {
				out = append(out, entryOf(rel, before, 0, true))
				seenDirs[before] = true
			}
			continue
		}
		if !seenDirs[rest] {
			out = append(out, entryOf(rel, rest, 0, true))
			seenDirs[rest] = true
		}
	}
	return out
}

func (s *SecretdataFS) ListDir(ctx context.Context, rel string) ([]syncpkg.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := splitDirPrefix(rel)
	out := collectChildSegments(s.index, s.dirs, prefix, rel, func(e *metaEntry) int64 {
		if e == nil {
			return 0
		}
		return e.size
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
		return &syncpkg.Entry{Name: path.Base(rel), Path: rel, Size: 0, IsDir: true}, nil
	}
	e, ok := s.index[key]
	if !ok {
		return nil, nil
	}
	return &syncpkg.Entry{Name: path.Base(rel), Path: rel, Size: e.size, IsDir: false}, nil
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
		return nil, fmt.Errorf("secretdata: 文件 %q 不存在", rel)
	}
	if e.meta == nil {
		return nil, fmt.Errorf("secretdata: %q 是目录", rel)
	}
	if size < 0 || offset < 0 || size > e.meta.Original.Size-offset {
		return nil, fmt.Errorf("secretdata: 区间 [%d,%d) 越出文件（size=%d）", offset, offset+size, e.meta.Original.Size)
	}
	if size == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	salt, serr := base64.StdEncoding.DecodeString(e.meta.Salt)
	if serr != nil || len(salt) != shardseal.SaltLen {
		return nil, fmt.Errorf("secretdata: 文件 meta salt 非法: %v", serr)
	}
	keyBytes, kerr := shardseal.DeriveKey(s.secret, salt, e.meta.AlgoVersion)
	if kerr != nil {
		return nil, fmt.Errorf("secretdata: 派生文件密钥失败: %w", kerr)
	}
	out, rerr := s.rangeReadBytes(ctx, e, keyBytes, salt, offset, offset+size)
	if rerr != nil {
		return nil, rerr
	}
	return io.NopCloser(bytes.NewReader(out)), nil
}

// rangeReadBytes 逐块只下载/解密含 [offset,end) 的 blocklet 段并拼接覆盖区间明文。
func (s *SecretdataFS) rangeReadBytes(ctx context.Context, e *metaEntry, keyBytes, salt []byte, offset, end int64) ([]byte, error) {
	var out []byte
	for _, ci := range e.meta.Chunks {
		if ci.Offset >= end || ci.Offset+ci.OrigSize <= offset {
			continue // 该块与目标区间不相交 → 不下载
		}
		seg, rerr := s.readChunkRangeBytes(ctx, e, keyBytes, salt, ci, offset, end)
		if rerr != nil {
			return nil, rerr
		}
		out = append(out, seg...)
	}
	return out, nil
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

func (s *SecretdataFS) WriteFile(ctx context.Context, rel string, r io.Reader, size, mtime int64) error {
	return s.writeFile(ctx, strings.TrimPrefix(rel, "/"), r, size, mtime, -1)
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
	s.removeVersionMeta(e)
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

// MakeDir 创建逻辑空目录：递归创建随机容器目录 + 目录 meta（@ 标记，{name, parent_dir_id}
// 父引用模型），不再提若干层（旧 inner.MakeDir(rel) 会向底层泄漏目录名，违背目录名保密）。
func (s *SecretdataFS) MakeDir(ctx context.Context, rel string) error {
	key := strings.TrimPrefix(rel, "/")
	_, _, err := s.ensureContainer(ctx, key, 0)
	return err
}

var _ syncpkg.FS = (*SecretdataFS)(nil)

// ---- 内部实现 ----

// writeCtx 是一次写路径的共享执行上下文（S107 收敛）：常规（writeFileEncrypted）与去重
// （writeFileDedup*/commitDedupEntry）写路径共用的入参组，避免逐参数传递（与 fileOp /
// backupWorker 的 Go S107 收敛范式一致）。字段语义见 writeFile：sv 为 CAS 起始卷版本、
// newVer 为写内锁内一次性分配的乐观锁版本（== sv+1）并在写路径全程固定；expected 为
// 调用方期望卷版本（<0 不校验）。
type writeCtx struct {
	rel       string        // 逻辑相对路径（索引键）
	data      []byte        // 明文（Imp 内存明文变体）
	container string        // 文件 meta 所在容器名
	created   []dirCreation // 本次新建容器（失败回滚删新留旧）
	mtime     int64         // 逻辑 mtime（原始值；底层 blob 写入经 blobMTime 打散）
	sv        int64         // 起始卷版本（CAS 起始校验）
	newVer    int64         // 本次写路径分配的乐观锁版本 == sv+1
	expected  int64         // 乐观锁期望版本（<0 不校验）
}

// writeFile：全程登记在途写（GC 以 writesInFlight 判定无在途写才清理）→ 乐观锁 CAS
// （起始 expected 校验 + 提交前再校验，原子推进版本）→ 去重或常规分块加密 → 上传
// → 全部成功后才原子切换索引（覆盖写中途失败删新留旧，F-2）。mtime 打散仅在底层
// blob 写入应用，逻辑层 entry.mtime 恒为原始 mtime。
func (s *SecretdataFS) writeFile(ctx context.Context, rel string, r io.Reader, size, mtime, expected int64) error {
	// 单文件上限拦截（读取全文前，按调用方 size 判定；0=不限制）。Imp-2 写路径为
	// 内存明文变体（峰值 1× 文件），大文件用上限兜底，避免把任意大小文件整读进内存。
	if s.opts.MaxFileBytes > 0 && size > int64(s.opts.MaxFileBytes) {
		return fmt.Errorf("%w: 大小 %d，上限 %d", ErrMaxFileBytes, size, int64(s.opts.MaxFileBytes))
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("secretdata: 读明文失败: %w", err)
	}
	// 登记在途写入 + 起始 CAS（expected ≥ 0 须匹配卷当前版本）+ **一次性分配本条目的
	// 乐观锁版本 newVer = sv+1**（Imp-1 修复：写前在锁内就定版本，注意它在写路径全程
	// 固定，落盘 meta.BaseVersion 与内存 e.baseVersion/卷版本三者恒等——跨进程 CAS 与
	// 重启后 loadIndex 恢复的卷版本以此为准；不再由 commitEntry 锁内重算 +1，避免
	// 「落盘 meta 恒 0」导致的跨重启 CAS 空心化）。
	s.mu.Lock()
	sv := s.volVersion
	if expected >= 0 && expected != sv {
		s.mu.Unlock()
		return fmt.Errorf("secretdata: 版本冲突：卷当前 %d，调用方期望 %d: %w", sv, expected, ErrVersionConflict)
	}
	newVer := sv + 1
	s.writesInFlight++
	s.mu.Unlock()
	defer s.endWrite()

	parent := parentDirOf(rel)
	container, created, err := s.ensureContainer(ctx, parent, mtime)
	if err != nil {
		return err
	}
	wc := writeCtx{
		rel: rel, data: data, container: container, created: created,
		mtime: mtime, sv: sv, newVer: newVer, expected: expected,
	}
	if s.opts.Dedup && len(data) > 0 {
		return s.writeFileDedup(ctx, wc)
	}
	return s.writeFileEncrypted(ctx, wc)
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
func (s *SecretdataFS) writeFileEncrypted(ctx context.Context, wc writeCtx) error {
	tmp, err := os.MkdirTemp(s.temp, "chunks-*")
	if err != nil {
		s.rollbackWrite(ctx, nil, wc.created)
		return fmt.Errorf("secretdata: 创建临时分块目录失败: %w", err)
	}
	defer os.RemoveAll(tmp)

	out, perr := encryptContent(wc.data, tmp, s.secret, s.opts.Block, wc.rel, s.metaPadTarget(), s.algoVer)
	if perr != nil {
		s.rollbackWrite(ctx, nil, wc.created)
		return fmt.Errorf("secretdata: 分块加密失败: %w", perr)
	}
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
		if eerr := s.writeErasureParity(ctx, wc.container, out, wc.data, wc.mtime, &uploaded); eerr != nil {
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
	if werr := s.inner.WriteFile(ctx, path.Join(wc.container, metaName), bytes.NewReader(metaBlob), int64(len(metaBlob)), mt); werr != nil {
		s.rollbackWrite(ctx, uploaded, wc.created)
		return fmt.Errorf("secretdata: 上传 meta 失败: %w", werr)
	}
	return s.commitEntry(ctx, wc, &metaEntry{
		size: int64(len(wc.data)), mtime: wc.mtime, dirSeg: wc.container, metaName: metaName, meta: out.Meta,
	}, uploaded)
}

// commitEntry 原子提交索引并推进 volVersion + usage。提交前再持锁校验 CAS：
// expected ≥0 须 volVersion==sv（否则回滚已上传 blob 并返回 ErrVersionConflict，让并发
// 同 expected 双写只能一胜一败）；expected<0 不校验、以写路径分配的 newVer 定型版本
// （last-write-wins）。newVer 由 writeFile 在锁内一次性分配（== sv+1）并在写路径全程
// 固定——commit 直接采用它，保证内存 e.baseVersion == 落盘 meta.BaseVersion == 卷版本
// 恒等（Imp-1 修复：不再锁内 +1 重算致落盘版本漂移）。
func (s *SecretdataFS) commitEntry(ctx context.Context, wc writeCtx, e *metaEntry, uploaded []string) error {
	s.mu.Lock()
	if wc.expected >= 0 && s.volVersion != wc.sv {
		s.mu.Unlock()
		s.rollbackWrite(ctx, uploaded, wc.created)
		return fmt.Errorf("secretdata: 版本冲突：卷当前 %d，调用方期望 %d: %w", s.volVersion, wc.expected, ErrVersionConflict)
	}
	prev := s.index[wc.rel]
	s.index[wc.rel] = e
	e.baseVersion = wc.newVer
	addDirKeysLocked(s.dirs, wc.rel)
	s.volVersion = wc.newVer
	s.usage += int64(len(wc.data))
	if prev != nil {
		s.usage -= prev.size
		if s.usage < 0 {
			s.usage = 0
		}
	}
	s.mu.Unlock()
	if prev != nil {
		// 锁外 best-effort 删旧版本 meta/分块（覆盖写清理；M-8 标注并发语义）：此时
		// 索引已指向新条目，旧分块不再被引用。并发 openRead 若在删旧瞬间以旧条目在途
		// 读取，可能中途读到已被删除的分块而报错（非数据损坏——读失败而非读到错内容）；
		// 属可接受的并发读-写语义（读要么拿到新内容要么报错，不返回混合/陈旧内容）。
		s.removeVersionMeta(prev)
	}
	return nil
}

// blobMTime 返回底层 blob 文件系统 mtime：默认 = 原始 mtime + 随机偏移（0-48h，防同文件
// 分片时间聚类）；PreserveMTime=true 或 mtime≤0 时透传原值。逻辑层 mtime 不受影响。
func (s *SecretdataFS) blobMTime(mtime int64) int64 {
	if s.opts.PreserveMTime || mtime <= 0 {
		return mtime
	}
	return mtime + shardseal.RandN(int64(48*time.Hour))
}

// uploadChunks 逐块上传到容器目录，并把已上传路径追加到 uploaded（供失败回滚删新留旧）。
// blob 写入 mtime 应用打散（blobMTime）。
func (s *SecretdataFS) uploadChunks(ctx context.Context, container, tmp string, mtime int64, chunkNames []string, uploaded *[]string) error {
	mt := s.blobMTime(mtime)
	for _, cn := range chunkNames {
		blob, rerr := os.ReadFile(filepath.Join(tmp, cn))
		if rerr != nil {
			return fmt.Errorf("secretdata: 读本地分块 %s 失败: %w", cn, rerr)
		}
		p := path.Join(container, cn)
		if werr := s.inner.WriteFile(ctx, p, bytes.NewReader(blob), int64(len(blob)), mt); werr != nil {
			return fmt.Errorf("secretdata: 上传分块 %s 失败: %w", cn, werr)
		}
		*uploaded = append(*uploaded, p)
	}
	return nil
}

// rollbackWrite 失败回滚：删除本次已上传的新 meta/分块；再回收本次新建且已成空的容器
// （目录 meta + 注销映射，删新留旧，F-2）。保有他人并发写入内容的容器不删（安全）。
func (s *SecretdataFS) rollbackWrite(ctx context.Context, uploaded []string, created []dirCreation) {
	for _, p := range uploaded {
		_ = s.inner.Delete(ctx, p)
	}
	s.pruneCreatedDirs(ctx, created)
}

// pruneCreatedDirs 写路径失败回滚：删除本次新建且已成空目录的容器（删除目录 meta + 注销
// 映射）。目录内保有他者新写入文件/子目录则不删（安全不损他人）；根容器恒不删。
func (s *SecretdataFS) pruneCreatedDirs(ctx context.Context, created []dirCreation) {
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
	if inner, err := s.inner.ListDir(ctx, container); err == nil {
		for _, f := range inner {
			if !f.IsDir {
				_ = s.inner.Delete(ctx, path.Join(container, f.Name))
			}
		}
	}
	delete(s.dirSegs, dirPath)
	delete(s.dirIDs, dirPath)
	delete(s.dirParents, dirPath)
	delete(s.dirs, dirPath)
}

// openRead：读取逻辑路径 → 还原解密到临时文件 → 返回 ReadCloser。
func (s *SecretdataFS) openRead(ctx context.Context, rel string) (io.ReadCloser, error) {
	s.mu.RLock()
	e, ok := s.index[rel]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("secretdata: 文件 %q 不存在", rel)
	}
	if e.meta == nil {
		return nil, fmt.Errorf("secretdata: %q 是目录", rel)
	}
	tmp, err := os.CreateTemp(s.temp, "decrypt-*")
	if err != nil {
		return nil, fmt.Errorf("secretdata: 创建解密临时文件失败: %w", err)
	}
	// 唯一临时分块目录（审查 F-5：固定 view/view-<hash> 并发读同文件有竞态窗口）。
	chunkLocalDir, err := os.MkdirTemp(s.temp, "view-*")
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	defer os.RemoveAll(chunkLocalDir)
	for _, ci := range e.meta.Chunks {
		blob, rerr := s.readChunkBlobOrErase(ctx, e, ci)
		if rerr != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, rerr
		}
		if werr := os.WriteFile(filepath.Join(chunkLocalDir, ci.FileName), blob, 0o600); werr != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, werr
		}
	}
	if derr := shardseal.DecryptFile(e.meta, chunkLocalDir, tmp.Name(), s.secret); derr != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, derr
	}
	if _, serr := tmp.Seek(0, 0); serr != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, serr
	}
	return &cleanupReadCloser{rc: tmp, path: tmp.Name()}, nil
}

// dirScan 是容器目录 meta 扫描结果（loadIndex Phase 1，并行产生）。
type dirScan struct {
	container string
	dm        *dirMeta
	hasFiles  bool
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
	scans := scanAllContainerDirMetas(ctx, s, dirContainers(root))
	byID := dirMetaTable(scans)
	res := s.resolveContainerPaths(ctx, byID, scans)
	loadAllFileMetas(ctx, s, res)
	return nil
}

// dirContainers 收集底层根下随机容器目录名（Phase 0）。
func dirContainers(root []syncpkg.Entry) []string {
	var containers []string
	for _, d := range root {
		if d.IsDir {
			containers = append(containers, d.Name)
		}
	}
	return containers
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
func scanAllContainerDirMetas(ctx context.Context, s *SecretdataFS, containers []string) []dirScan {
	scans := make([]dirScan, len(containers))
	var wg sync.WaitGroup
	for i, c := range containers {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			s.loadGate <- struct{}{} // 派生并发上界（scrypt 内存）
			defer func() { <-s.loadGate }()
			scans[i] = dirScan{container: c}
			dm, hasFiles, ok := s.scanContainerDirMeta(ctx, c)
			if ok {
				scans[i].dm = dm
			}
			scans[i].hasFiles = hasFiles
		}(i, c)
	}
	wg.Wait()
	return scans
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
		s.dirSegs[rel] = sc.container
		s.dirIDs[rel] = sc.dm.DirID
		if rel != "" {
			s.dirs[rel] = struct{}{}
			s.dirParents[rel] = parentDirOf(rel)
		}
	}
	return res
}

// loadAllFileMetas Phase 3：各容器文件 meta 并行解密登记索引。
func loadAllFileMetas(ctx context.Context, s *SecretdataFS, res []dirRes) {
	var wg sync.WaitGroup
	for _, r := range res {
		if !r.ok {
			continue
		}
		wg.Add(1)
		go func(r dirRes) {
			defer wg.Done()
			s.loadContainerFileMetas(ctx, r.container, r.rel)
		}(r)
	}
	wg.Wait()
}

// scanContainerDirMeta 定位容器目录 meta 并解密（旧格式 fail-closed）。返回
// (dirMeta, 容器是否含文件 meta, 是否找到可解析目录 meta)。目录 meta 缺失/损坏/旧格式
// → dm=nil；含文件 meta 时记日志（fail-closed：不压平到根，避免跨容器同名遮蔽，F-1）。
func (s *SecretdataFS) scanContainerDirMeta(ctx context.Context, container string) (*dirMeta, bool, bool) {
	inner, err := s.inner.ListDir(ctx, container)
	if err != nil {
		return nil, false, false
	}
	hasFiles := hasFileMeta(inner)
	if dm, found := scanContainerDirMetaBlob(ctx, s, container, inner, hasFiles); found {
		return dm, hasFiles, true
	}
	if hasFiles {
		slog.Warn("secretdata: 容器缺少可解析目录 meta，跳过恢复其中文件（fail-closed）",
			"container", container)
	}
	return nil, hasFiles, false
}

// scanContainerDirMetaBlob 定位并解密容器内目录 meta（旧格式 fail-closed 判定明确并专用
// 日志，不落通用缺 meta 日志——M-6 双日志噪点）。返回 (dirMeta, 是否找到可解析目录 meta)。
func scanContainerDirMetaBlob(ctx context.Context, s *SecretdataFS, container string, inner []syncpkg.Entry, hasFiles bool) (*dirMeta, bool) {
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
		return dm, true
	}
	return nil, false
}

// hasFileMeta 报告容器内是否存在文件 meta（-/_ 标记）条目。
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
func (s *SecretdataFS) loadContainerFileMetas(ctx context.Context, container, dirPath string) {
	inner, err := s.inner.ListDir(ctx, container)
	if err != nil {
		return
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
		if dd, ok := mm.Extra["dedup"]; ok && len(dd) > 0 && len(mm.Chunks) > 0 {
			dataDir = string(dd)
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
	s.registerLoadedEntryLocked(rel, container, f.Name, mm, dataDir, mt, prev)
	s.mu.Unlock()
}

// registerLoadedEntryLocked 登记已通过校验/冲突解决的加载条目并推进 usage/volVersion。
// 调用方持 s.mu（且已 resolveSameRelConflict 通过）。prev 是冲突前旧条目（同 rel 已被
// 新版本覆盖时非 nil；调用方负责对 usage 换账）。
func (s *SecretdataFS) registerLoadedEntryLocked(rel, container, metaName string, mm *shardseal.Meta, dataDir string, mt int64, prev *metaEntry) {
	// 去重引用文件：dataDir 指向池容器；池引用计数随加载恢复（重启后仍可物理删归零）。
	if dataDir != "" {
		s.registerPoolEntryLocked(mm)
	}
	s.index[rel] = &metaEntry{
		size: mm.Original.Size, mtime: mt, dirSeg: container, metaName: metaName, meta: mm,
		dataDir: dataDir, baseVersion: mm.BaseVersion,
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
	s.dedupPool[key] = &dedupBlob{name: mm.Chunks[0].FileName, refs: 1, meta: mm}
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

// ensureContainerLocked 是 ensureContainer 的持锁实现（递归建祖先链；调用方持 s.mu）。
func (s *SecretdataFS) ensureContainerLocked(ctx context.Context, dirPath string, mtime int64) (string, []dirCreation, error) {
	if c, ok := s.dirSegs[dirPath]; ok {
		return c, nil, nil
	}
	var created []dirCreation
	var parentID string
	if dirPath != "" {
		parent := parentDirOf(dirPath)
		_, pcreated, perr := s.ensureContainerLocked(ctx, parent, mtime)
		if perr != nil {
			return "", nil, perr
		}
		created = append(created, pcreated...)
		parentID = s.dirIDs[parent]
		if parentID == "" {
			return "", nil, fmt.Errorf("secretdata: 父目录 %q 无 dir_id，无法建立父子引用", parent)
		}
	}
	c, cerr := shardseal.RandDirName()
	if cerr != nil {
		return "", nil, cerr
	}
	dirID, iderr := shardseal.RandIDHex()
	if iderr != nil {
		return "", nil, iderr
	}
	salt, serr := shardseal.RandSalt()
	if serr != nil {
		return "", nil, serr
	}
	key, kerr := shardseal.DeriveKey(s.secret, salt, s.algoVer)
	if kerr != nil {
		return "", nil, kerr
	}
	dm := dirMeta{Version: 1, Algorithm: s.opts.Algorithm, KDF: "scrypt", Type: "dir",
		Name: logicalDirName(dirPath), ParentDirID: parentID, DirID: dirID, MTime: mtimeString(mtime)}
	dmJSON, merr := json.Marshal(dm)
	if merr != nil {
		return "", nil, merr
	}
	blob, berr := shardseal.EncryptMetaJSON(key, salt, dmJSON, s.metaPadTarget())
	if berr != nil {
		return "", nil, berr
	}
	origHex, _ := shardseal.Hash16(dmJSON)
	encHex, _ := shardseal.Hash16(blob)
	name := shardseal.DirMetaName(origHex, dirID, encHex)
	if werr := s.inner.WriteFile(ctx, path.Join(c, name), bytes.NewReader(blob), int64(len(blob)), s.blobMTime(mtime)); werr != nil {
		return "", nil, fmt.Errorf("secretdata: 写目录 meta %s 失败: %w", name, werr)
	}
	s.dirSegs[dirPath] = c
	s.dirIDs[dirPath] = dirID
	if dirPath != "" {
		s.dirs[dirPath] = struct{}{}
		s.dirParents[dirPath] = parentDirOf(dirPath)
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
	// 仅重写本目录 meta 的 Name/ParentDirID（一个文件），子树零改动。
	if werr := s.rewriteDirMetaRef(ctx, container, toName, toParentID); werr != nil {
		return werr
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
	newName, werr := s.writeDirMetaLocked(ctx, container, dm)
	if werr != nil {
		return werr
	}
	if newName != oldName {
		_ = s.inner.Delete(ctx, path.Join(container, oldName))
	}
	return nil
}

// applyDirMoveLocked 提交目录移动后的内存态（调用方持 s.mu）：dirSegs/dirIDs/index 键
// 整体迁移（旧键删、新键留，避免 ListDir 幽灵目录与 split-brain）；dirs/dirParents 由
// dirSegs 全量重建（逻辑路径全决定，不删漏）。
func (s *SecretdataFS) applyDirMoveLocked(from, to string) {
	prefix := from + "/"
	// dirSegs / dirIDs：键迁移（新旧路径同容器/同 dir_id）。
	segShifts := map[string]string{}
	for oldDir, c := range s.dirSegs {
		newDir := oldDir
		switch {
		case oldDir == from:
			newDir = to
		case strings.HasPrefix(oldDir, prefix):
			newDir = to + oldDir[len(from):]
		}
		segShifts[newDir] = c
	}
	s.dirSegs = segShifts
	idShifts := map[string]string{}
	for oldDir, id := range s.dirIDs {
		newDir := oldDir
		switch {
		case oldDir == from:
			newDir = to
		case strings.HasPrefix(oldDir, prefix):
			newDir = to + oldDir[len(from):]
		}
		idShifts[newDir] = id
	}
	s.dirIDs = idShifts
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
	if path.Base(from) != path.Base(to) {
		return fmt.Errorf("secretdata: 文件 %q 改名不可经 Rename（basename 锚定于 meta，逻辑改名走 delete+write）", from)
	}
	targetParent := parentDirOf(to)
	targetContainer := s.dirSegs[targetParent]
	if targetContainer == "" {
		return fmt.Errorf("secretdata: 目标目录 %q 不存在或无容器", targetParent)
	}
	if _, exists := s.index[to]; exists {
		return fmt.Errorf("secretdata: 目标 %q 已是文件", to)
	}
	if e.dirSeg == targetContainer {
		return nil // 同容器同 basename 同路径（from==to 已早退），防御性无操作
	}
	// 物理复制分块 + 文件 meta 到目标容器（blob 内容零改动；去重文件分块在池容器不动），
	// 删源副本（best-effort），原子迁移索引。
	if cerr := s.copyFileBlobs(ctx, e, targetContainer); cerr != nil {
		return cerr
	}
	s.deleteFileBlobs(ctx, e)
	moved := *e
	moved.dirSeg = targetContainer
	delete(s.index, from)
	s.index[to] = &moved
	addDirKeysLocked(s.dirs, to)
	// 源目录若因移出最后一个文件而成空 → 一并回收（Imp-1 空目录语义一致）。
	s.pruneEmptyDirsLocked(ctx, parentDirOf(from))
	s.volVersion++
	return nil
}

// copyFileBlobs 物理复制条目全部 blob（分块 + 文件 meta）到目标容器（内容零改动 → 哈希
// 不变）；去重文件仅复制本容器 meta（分块在池容器不动）。任一失败回滚已复制并返回错误
// （不落半态）。
func (s *SecretdataFS) copyFileBlobs(ctx context.Context, e *metaEntry, targetContainer string) error {
	rollback := func(uploaded []string) {
		for _, p := range uploaded {
			_ = s.inner.Delete(ctx, p)
		}
	}
	uploaded := []string{}
	if cerr := s.copyBlob(ctx, path.Join(e.dirSeg, e.metaName), path.Join(targetContainer, e.metaName), e.mtime); cerr != nil {
		rollback(uploaded)
		return cerr
	}
	uploaded = append(uploaded, path.Join(targetContainer, e.metaName))
	if e.dataDir != "" {
		return nil // 去重文件：分块在池容器不动
	}
	for _, ci := range e.meta.Chunks {
		dst := path.Join(targetContainer, ci.FileName)
		if cerr := s.copyBlob(ctx, path.Join(e.dirSeg, ci.FileName), dst, e.mtime); cerr != nil {
			rollback(uploaded)
			return cerr
		}
		uploaded = append(uploaded, dst)
	}
	// Erasure parity：随文件一并搬移（Imp-A：目标保留 XOR 冗余、源 parity 不再孤立）。
	if p := e.meta.Parity; p != nil {
		dst := path.Join(targetContainer, p.FileName)
		if cerr := s.copyBlob(ctx, path.Join(e.dirSeg, p.FileName), dst, e.mtime); cerr != nil {
			rollback(uploaded)
			return cerr
		}
	}
	return nil
}

// deleteFileBlobs 删除条目在源容器的分块 + 文件 meta + parity（best-effort：失败残留孤儿
// 交 GC 回收，不阻塞移动成功）。去重文件仅删本容器 meta（分块在池容器不删）。
// **meta 先删 + 删失败即早退（对齐 removeVersionMeta，修复轮 M2）**：meta 是重启重建锚点——
// meta 删失败则分块全在 → 重启重建完整文件（删除丢失的安全失败），杜绝「分块已删、meta
// 残留」的旧路径幽灵条目（Move 成功重启后源路径复活）。
func (s *SecretdataFS) deleteFileBlobs(ctx context.Context, e *metaEntry) {
	if err := s.inner.Delete(ctx, path.Join(e.dirSeg, e.metaName)); err != nil {
		return // meta 删失败：分块保留（防源路径幽灵条目）
	}
	if e.dataDir != "" {
		return
	}
	for _, ci := range e.meta.Chunks {
		_ = s.inner.Delete(ctx, path.Join(e.dirSeg, ci.FileName))
	}
	if p := e.meta.Parity; p != nil {
		_ = s.inner.Delete(ctx, path.Join(e.dirSeg, p.FileName))
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
	if werr := s.inner.WriteFile(ctx, dst, bytes.NewReader(data), int64(len(data)), mt); werr != nil {
		return fmt.Errorf("secretdata: 写入目标 blob %s 失败: %w", dst, werr)
	}
	return nil
}

// pruneEmptyDirsLocked 从 from 起向上回收空目录（Imp-1：删干净、重启不复现）：目录无文件
// 且无子目录 → 删除其容器残留（目录 meta + 遗留/孤儿分块）并注销映射；遇到非空目录或根
// 停止。调用方持有 s.mu（deleteFile 在 Lock 下调用）。
func (s *SecretdataFS) pruneEmptyDirsLocked(ctx context.Context, from string) {
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

// findDirMetaBlob 定位容器内的目录 meta（@ 标记）并读回其加密 blob。
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
	origHex, _ := shardseal.Hash16(dmJSON)
	encHex, _ := shardseal.Hash16(blob)
	name := shardseal.DirMetaName(origHex, dm.DirID, encHex)
	mt := dirMetaMTime(dm.MTime)
	if werr := s.inner.WriteFile(ctx, path.Join(container, name), bytes.NewReader(blob), int64(len(blob)), s.blobMTime(mt)); werr != nil {
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
func (s *SecretdataFS) removeVersionMeta(e *metaEntry) {
	if e == nil || e.meta == nil {
		return
	}
	// 先删本容器 meta（重启重建锚点）：meta 删**失败** → 立即返回、分块全保留 → 重启重建
	// 完整文件（删除丢失的安全失败，无幽灵/坏数据）；meta 删成功才继续删分块 + parity
	// （分块删失败 → 无 meta 引用，成孤儿，可选 GC 上收）。
	if err := s.inner.Delete(context.Background(), path.Join(e.dirSeg, e.metaName)); err != nil {
		return // meta 删失败：分块保留（反幽灵：meta 声明的 size 与分块俱在）
	}
	if e.dataDir != "" {
		// 去重引用文件：meta 已删 → 释放池引用（归零物理删池 blob；失败 → 池 blob 成孤儿）。
		s.unrefPoolEntry(e)
		return
	}
	for _, ci := range e.meta.Chunks {
		_ = s.inner.Delete(context.Background(), path.Join(e.dirSeg, ci.FileName))
	}
	if p := e.meta.Parity; p != nil && p.FileName != "" {
		_ = s.inner.Delete(context.Background(), path.Join(e.dirSeg, p.FileName))
	}
}

// unrefPoolEntry 释放去重池引用：引用计数归零时物理删除池 blob。调用方须持 s.mu。
// 若池条目不存在或 blob 名与条目不符（并发 miss 的私有分块）→ 不递减，私有分块留待
// GC 清孤儿（不误删他人共享 blob）。
func (s *SecretdataFS) unrefPoolEntry(e *metaEntry) {
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
			_ = s.inner.Delete(context.Background(), path.Join(s.dedupDir, b.name))
		}
		delete(s.dedupPool, key)
	}
}
