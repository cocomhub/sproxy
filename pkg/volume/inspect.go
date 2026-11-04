// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

// inspect.go 定义「真实存储可解释」能力（设计 docs/designs/2026-11-04-secretdata-storage-inspection.md）。
//
// 为什么定义在 pkg/volume 而不是 pkg/server 或 CLI：真实落点（容器链、meta pad、分块/parity、
// blocklet、去重共享容器）**只有掌握布局的卷自己知道**；调用方无法反推。写在别处就是第二份
// 布局事实，必然与写路径漂移（历史 P0 同类）。所有消费方（HTTP / CLI / 未来 Web UI）共用
// 本接口的实现。
//
// 未实现该能力的卷 ⇒ 类型断言失败，调用方必须**显式处置**（HTTP 501 / CLI 报错），
// **禁止**返回空结果——空结果会被误读为「该路径没有任何落点」。

import (
	"context"
	"errors"
)

// InspectMode 是审查模式。
type InspectMode int

const (
	// InspectIndex 只读卷内索引（默认；快，不保证与磁盘一致）。
	InspectIndex InspectMode = iota
	// InspectDisk 同时对比真实磁盘（慢；输出差异码）。
	InspectDisk
)

// String 返回稳定的模式名（JSON / CLI 共用）。
func (m InspectMode) String() string {
	if m == InspectDisk {
		return "disk"
	}
	return "index"
}

// ErrOutsideVolume 表示请求的真实路径越出卷根（含 `..`、绝对路径、卷根之外的符号链接语义）。
// 调用方必须按「拒绝」处置（HTTP 403），**禁止**静默夹取或规范化后继续。
var ErrOutsideVolume = errors.New("volume: 路径越出卷根")

// StorageInspector 是「真实存储可解释」可选能力（只读）。
//
// 语义要点：
//   - 只解 meta（名称/尺寸/分块/parity 等元数据），**不读取内容、不落地明文**；
//   - 不修改任何状态（无删除/修复）；
//   - 实现必须**说明判定依据**（`Evidence`），让使用者能核对而不是盲信。
type StorageInspector interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），表达能力语义，设计保留
	// Trace 返回逻辑路径 rel 在真实存储上的全部落点。
	// rel 不存在 ⇒ (nil, fs.ErrNotExist)。rel 为空 = 卷根目录。
	Trace(ctx context.Context, rel string, mode InspectMode) (*PathTrace, error)

	// Explain 返回真实路径 realRel（**卷根相对**）的用途与逻辑归属。
	// 越出卷根 ⇒ ErrOutsideVolume。路径不存在 ⇒ 返回 Kind=unknown + not_found 异常（不是错误：
	// 审查工具需要解释「这里为什么是空的」）。
	Explain(ctx context.Context, realRel string, mode InspectMode) (*RealExplanation, error)
}

// AssertStorageInspector 探测卷 FS 是否实现审查能力（仓内既有「可选能力 + Assert*」模式）。
func AssertStorageInspector(v any) (StorageInspector, bool) {
	if v == nil {
		return nil, false
	}
	insp, ok := v.(StorageInspector) //nolint:forcetypeassert // 类型断言探测，ok 语义即能力存在性
	return insp, ok
}

// ---- 对外数据模型（JSON 字段即契约；变更须升 SchemaVersion）----

// SchemaVersion 是审查输出的结构版本（字段增删必须同步升级，CLI/UI 据此兼容）。
const SchemaVersion = 1

// PathTrace 是正向落点：一个逻辑对象在真实存储上的完整分布。
type PathTrace struct {
	SchemaVersion int    `json:"schema_version"`
	Volume        string `json:"volume,omitempty"`
	Mode          string `json:"mode"`
	Rel           string `json:"rel"`
	Kind          string `json:"kind"` // file | dir
	Size          int64  `json:"size"` // 逻辑明文大小
	MTime         int64  `json:"mtime"`

	// Containers 是容器链（卷根 → 叶），每层带**真实容器名**与其逻辑目录名。
	Containers []ContainerRef `json:"containers,omitempty"`

	Meta   *BlobRef   `json:"meta,omitempty"` // 文件 meta / 目录 meta blob
	Chunks []ChunkRef `json:"chunks,omitempty"`
	Parity *BlobRef   `json:"parity,omitempty"`
	Dedup  *DedupRef  `json:"dedup,omitempty"`

	// Upload 非空 = 该路径**正在上传**（meta 尚未提交，索引里还没有）。
	// mode 同为 "uploading"；此时 Chunks/Meta 为空。
	Upload *UploadProgress `json:"upload,omitempty"`

	Anomalies []Anomaly `json:"anomalies,omitempty"`
}

// ChunkRef 是一个内容分块的落点。
type ChunkRef struct {
	Index     int     `json:"index"`
	Blob      BlobRef `json:"blob"`
	Blocklets int     `json:"blocklets"` // 块内 blocklet 数（>1 = 大块/关键帧分段）
}

// DedupRef 标记内容位于卷级去重容器（共享分块，非本文件容器）。
type DedupRef struct {
	Container string `json:"container"`
	Refs      int    `json:"refs"` // 索引内引用该共享内容的文件数
}

// BlobRef 是一个真实 blob 的引用。
type BlobRef struct {
	// RealPath 是**卷根相对**路径（形如 "k3f9a/2026/xxxx"）。
	RealPath string `json:"real_path"`
	// Bytes 是声明大小（index 模式 = meta/索引记录；disk 模式仍保留声明值，便于对比）。
	Bytes int64 `json:"bytes"`
	// Plain 是明文大小（分块/parity 有意义；meta 为 0）。
	Plain int64 `json:"plain,omitempty"`
	// OnDisk 是磁盘存在性（仅 disk 模式非 nil）。
	OnDisk *bool `json:"on_disk,omitempty"`
	// DiskBytes 是磁盘实际大小（仅 disk 模式且存在时非 nil）。
	DiskBytes *int64 `json:"disk_bytes,omitempty"`
}

// LogicalRef 指向逻辑对象。
type LogicalRef struct {
	Rel  string `json:"rel"`
	Role string `json:"role"` // file | dir
	// ChunkIndex 非 nil = 该 blob 是某文件的第 N 个分块。
	ChunkIndex *int `json:"chunk_index,omitempty"`
	// Parity 标记该 blob 是 parity 段（与普通分块**命名同形**，只能由 meta 判定）。
	Parity bool `json:"parity,omitempty"`
}

// ContainerRef 描述一个容器目录。
type ContainerRef struct {
	RealPath   string `json:"real_path"`
	LogicalDir string `json:"logical_dir"` // 逻辑目录（根 = ""）
	DirID      string `json:"dir_id,omitempty"`
	ParentID   string `json:"parent_id,omitempty"`
}

// RealEntry 是容器内单个真实条目的用途（反向解释用）。
type RealEntry struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"` // container | dir_meta | file_meta | chunk | parity | unknown
	Bytes int64  `json:"bytes"`
	// Owner 非空 = 该条目属于某逻辑文件（file_meta/分块/parity）。
	Owner *LogicalRef `json:"owner,omitempty"`
	// Container 非空 = 该条目是**子容器目录**（逻辑目录归属）。
	Container *ContainerRef `json:"container,omitempty"`
	Note      string        `json:"note,omitempty"`
}

// UploadProgress 是在途上传的进度快照（仅**本进程**可见的上传可上报）。
type UploadProgress struct {
	Rel       string `json:"rel"`
	Container string `json:"container"`
	// Total 是逻辑大小（调用方声明；0 = 未知）。
	Total int64 `json:"total"`
	// Committed 是**已上传密文字节**（含每块头/nonce/tag 开销，略大于对应明文）。
	Committed int64 `json:"committed"`
	// Chunks 是已完成的块数。
	Chunks int64 `json:"chunks"`
	// StartedAt 是 UnixNano 起始时刻（仅展示用，不参与判定）。
	StartedAt int64 `json:"started_at,omitempty"`
}

// RealExplanation 是反向解释：一个真实路径是干什么的。
type RealExplanation struct {
	SchemaVersion int    `json:"schema_version"`
	Volume        string `json:"volume,omitempty"`
	Mode          string `json:"mode"`
	RealPath      string `json:"real_path"`
	// Kind 取值：container | dir_meta | file_meta | chunk | parity | unknown
	Kind string `json:"kind"`

	Owner     *LogicalRef   `json:"owner,omitempty"`
	Container *ContainerRef `json:"container,omitempty"`
	Evidence  []string      `json:"evidence,omitempty"`
	Entries   []RealEntry   `json:"entries,omitempty"`
	Anomalies []Anomaly     `json:"anomalies,omitempty"`
}

// Anomaly 是「索引声明」与「磁盘事实」之间的差异（或布局异常）。
//
// 注意：Anomaly **不是删除建议**。例如 blob_orphan 只表示「索引无引用」，
// 实际能否回收由 GC 的 re-confirm 流程决定（crash 窗口内可能仍被使用）。
type Anomaly struct {
	Code     string `json:"code"`
	RealPath string `json:"real_path,omitempty"`
	Detail   string `json:"detail"`
}

// 差异码（稳定契约；CLI 退出码与 UI 高亮据此判定）。
const (
	AnomalyBlobMissing         = "blob_missing"          // 索引/meta 声明，磁盘不存在
	AnomalyBlobOrphan          = "blob_orphan"           // 磁盘存在，索引无引用（疑似孤儿）
	AnomalySizeMismatch        = "size_mismatch"         // 磁盘大小 ≠ 声明大小
	AnomalyMetaUnreadable      = "meta_unreadable"       // meta 无法解密/损坏
	AnomalyContainerNotIndexed = "container_not_indexed" // 容器存在但索引无对应逻辑目录
	AnomalyParentUnresolved    = "parent_unresolved"     // dir-meta 的父引用在索引中缺失
	AnomalyDirMetaAmbiguous    = "dir_meta_ambiguous"    // 同一容器多个 dir-meta（异常布局）
	AnomalyNotFound            = "not_found"             // 真实路径在磁盘不存在
	AnomalyTruncated           = "truncated"             // 条目过多被截断
	AnomalyDedupShared         = "dedup_shared"          // 分块位于去重池（提示，非异常）
	// AnomalyUploadInProgress 不是差异：该 blob 属于**本进程在途上传**（meta 尚未提交），
	// 不得当作孤儿处置（GC 写进行时本就不会运行）。
	AnomalyUploadInProgress = "upload_in_progress"
	// AnomalyProbeFailed 是 disk 模式探测 blob 时的 IO 失败（**不是**大小不符/缺失：
	// 无法判定存在性与大小，必须与「明确缺失」区分，否则会误判为数据丢失）。
	AnomalyProbeFailed = "probe_failed"
)
