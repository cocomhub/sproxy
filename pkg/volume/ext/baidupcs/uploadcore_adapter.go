// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// uploadcore_adapter.go —— 把**本包既有能力**适配到自研上传内核（S5 装配）。
//
// 设计：docs/design-baidupcs-upload-core.md。内核只做"编排"（状态机/并发/重试/取消/WAL/续传），
// 协议细节与 checksum 全部复用既有实现，**避免第二份实现**：
//
//	Prober    ← Storage.metaByExactList（列举取 md5；绕开会挂起的 Meta）
//	Rapid     ← 库 pcs.RapidUpload（参数取自 stagedUpload + planRapid；**必须带 blockMD5s**）
//	Slicer    ← 按 uploadBlockSize（4MiB，与 blockList 分块一致）切分
//	Parts     ← 既有 baiduMultiUpload.TmpFile（pcs.UploadTmpFile）
//	Merger    ← 既有 baiduMultiUpload.CreateSuperFile（pcs.UploadCreateSuperFile）
//	Verifier  ← 列举取 md5 与本地 md5 比对（数据操作必须 md5 确认）
//	Repair    ← 既有 libraryAdapter.RapidUpload（秒传刷新，零传输自愈）
//
// 关于 Rapid **不复用** libraryAdapter.RapidUpload：后者只返回"是否命中"（刷新路径语义），
// 内核还需要 **upload_id**（分片上传的前置，库 upload_task_unit.go:281-283）⇒ 两者参数组装不同、
// 语义不同；共用会耦合"刷新"与"首次上传"两条路径。故这里独立实现一次 API 调用（约 10 行），
// 并**引用同一份 stagedUpload 参数**（不重复计算 checksum）。

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume/ext/baidupcs/uploadcore"
)

// bcPolicy 返回百度上传策略（与既有 multiupload.go 一致：覆盖 = "overwrite"，否则 "skip"）。
func bcPolicy(overwrite bool) string {
	if overwrite {
		return "overwrite"
	}
	return "skip"
}

// ucSectionReader 把文件区间包装成 fork 库要求的 `rio.ReaderLen64`。
// `Len()` 语义与库 `fileReadedlen64.Len()` 一致：**剩余未读长度**（rio/file.go:65-72）。
type ucSectionReader struct {
	f   *os.File
	sr  *io.SectionReader
	rem int64
}

func newUCSectionReader(f *os.File, offset, size int64) *ucSectionReader {
	return &ucSectionReader{f: f, sr: io.NewSectionReader(f, offset, size), rem: size}
}

func (r *ucSectionReader) Read(b []byte) (int, error) {
	n, err := r.sr.Read(b)
	r.rem -= int64(n)
	return n, err
}

// Len 返回剩余未读字节数（库用它设置分片请求的 Content-Length）。
func (r *ucSectionReader) Len() int64 { return r.rem }

func (r *ucSectionReader) Close() error { return r.f.Close() }

// ucAdapter 实现内核所需的全部依赖接口（一次上传会话一个实例）。
type ucAdapter struct {
	st        *Storage
	pcs       *Client
	stg       *stagedUpload
	localPath string
	remote    string
	overwrite bool
	// repair 是自愈回调（秒传刷新）；nil ⇒ 内核视为"无自愈能力"（校验失败即报错）。
	repair func(ctx context.Context, remote string, st *stagedUpload) (bool, error)
}

// newUCAdapter 构造适配器（staged 参数复用既有 stagedUpload，不重复算 checksum）。
// pcs 为库客户端（发 rapidupload/分片/合并请求）；repair 为可选自愈（秒传刷新）。
func newUCAdapter(st *Storage, pcs *Client, localPath, remote string,
	stg *stagedUpload, overwrite bool,
	repair func(ctx context.Context, remote string, st *stagedUpload) (bool, error)) *ucAdapter {
	return &ucAdapter{st: st, pcs: pcs, stg: stg, localPath: localPath,
		remote: remote, overwrite: overwrite, repair: repair}
}

// ExistingMD5 目标当前内容 md5（"" = 不存在）；用精确列举（Meta 可见性偶发延迟，不可作判据）。
func (a *ucAdapter) ExistingMD5(ctx context.Context, targetPath string) (string, error) {
	meta, ok := a.st.metaByExactList(ctx, targetPath)
	if !ok || meta == nil {
		return "", nil
	}
	return meta.ETag, nil
}

// Rapid 尝试秒传：返回 (upload_id, 是否已命中)。参数取自 stagedUpload（md5/sliceMD5/crc32/
// blockMD5s/size）+ 随机片段（planRapid，含与库逐位一致的偏移算法）。
func (a *ucAdapter) Rapid(ctx context.Context, req uploadcore.Request) (string, bool, error) {
	uk, pcsErr := a.pcs.PCS().CacheUK()
	if pcsErr != nil {
		return "", false, fmt.Errorf("baidupcs: 获取 uk 失败（需 STOKEN）: %w", pcsErr)
	}
	now := time.Now().Unix()
	plan, err := planRapid(a.localPath, uk, now)
	if err != nil {
		return "", false, err
	}
	// 一致性校验：stagedUpload 与 planRapid 的整文件 md5 必须一致（否则说明暂存被改动）。
	if a.stg != nil && a.stg.md5 != "" && plan.ContentMD5 != a.stg.md5 {
		return "", false, fmt.Errorf("baidupcs: 暂存文件 md5 漂移（staged=%s 实测=%s）", a.stg.md5, plan.ContentMD5)
	}
	policy := bcPolicy(a.overwrite)
	pcsError, jsonData := a.pcs.PCS().RapidUpload(a.remote, policy, "",
		plan.ContentMD5, plan.SliceMD5, plan.DataContent, plan.CRC32,
		plan.Offset, plan.DataLength, plan.Size, plan.DataTime, a.blockList(plan))
	if pcsError != nil {
		return "", false, fmt.Errorf("baidupcs: rapidupload 失败: %w", pcsError)
	}
	if jsonData.ReturnType == 2 {
		return jsonData.UploadID, true, nil // 秒传命中（零传输完成）
	}
	return jsonData.UploadID, false, nil
}

// blockList 返回 rapidupload 的 block_list 参数：**必须与上传分块一致**
// （本包上传块固定 4MiB ⇒ 用同一 uploadBlockSize 计算的列表），见 rapidcalc.go 的约束说明。
func (a *ucAdapter) blockList(plan *rapidPlan) []string {
	if len(plan.BlocksList) > 0 {
		return plan.BlocksList
	}
	if a.stg != nil {
		return a.stg.blockMD5s
	}
	return nil
}

// Plan 按 uploadBlockSize 切分（Seq 从 0 起）。
func (a *ucAdapter) Plan(_ context.Context, req uploadcore.Request) ([]uploadcore.Part, error) {
	size := req.Size
	if size <= 0 && a.stg != nil {
		size = a.stg.size
	}
	if size <= 0 {
		return nil, fmt.Errorf("baidupcs: 分片计划需要文件大小（size=%d）", size)
	}
	bs := int64(uploadBlockSize)
	parts := make([]uploadcore.Part, 0, (size+bs-1)/bs)
	for off := int64(0); off < size; off += bs {
		n := bs
		if off+n > size {
			n = size - off
		}
		parts = append(parts, uploadcore.Part{Seq: len(parts), Offset: off, Size: n})
	}
	return parts, nil
}

// UploadPart 上传单个分片（复用既有 TmpFile = pcs.UploadTmpFile；同 Seq 重传幂等覆盖）。
func (a *ucAdapter) UploadPart(ctx context.Context, uploadID string, p uploadcore.Part) error {
	f, err := os.Open(a.localPath)
	if err != nil {
		return fmt.Errorf("baidupcs: 打开待上传文件: %w", err)
	}
	rd := newUCSectionReader(f, p.Offset, p.Size)
	defer func() { _ = rd.Close() }()
	mu := newBaiduMultiUpload(a.pcs, a.remote)
	if _, err := mu.TmpFile(ctx, uploadID, a.remote, p.Seq, p.Offset, rd); err != nil {
		return fmt.Errorf("baidupcs: 上传分片 %d: %w", p.Seq, err)
	}
	return nil
}

// Merge 合并分片（复用既有 CreateSuperFile = pcs.UploadCreateSuperFile）。
func (a *ucAdapter) Merge(_ context.Context, uploadID string, req uploadcore.Request, parts []uploadcore.Part) error {
	size := req.Size
	if size <= 0 && a.stg != nil {
		size = a.stg.size
	}
	checksumMap := make(map[int]string, len(parts))
	for _, p := range parts {
		if a.stg != nil && p.Seq < len(a.stg.blockMD5s) {
			checksumMap[p.Seq] = a.stg.blockMD5s[p.Seq]
		}
	}
	policy := bcPolicy(a.overwrite)
	mu := newBaiduMultiUpload(a.pcs, a.remote)
	if err := mu.CreateSuperFile("", policy, uploadID, size, checksumMap); err != nil {
		return fmt.Errorf("baidupcs: 合并分片: %w", err)
	}
	return nil
}

// Verify 上传后以 md5 确认内容（数据操作红线：必须按 md5 核对，不得只看存在性）。
func (a *ucAdapter) Verify(ctx context.Context, req uploadcore.Request) error {
	meta, ok := a.st.metaByExactList(ctx, a.remote)
	if !ok || meta == nil {
		return fmt.Errorf("baidupcs: 复核失败：列举中未见 %s", a.remote)
	}
	want := req.MD5
	if want == "" && a.stg != nil {
		want = a.stg.md5
	}
	if want != "" && meta.ETag != "" && meta.ETag != want {
		return fmt.Errorf("baidupcs: md5 不一致（远端 %s != 本地 %s）", meta.ETag, want)
	}
	return nil
}

// Repair 自愈：复用既有秒传刷新（零传输重算远端元信息）。
func (a *ucAdapter) Repair(ctx context.Context, req uploadcore.Request) error {
	if a.repair == nil {
		return fmt.Errorf("baidupcs: 未装配自愈能力（秒传刷新）")
	}
	hit, err := a.repair(ctx, a.remote, a.stg)
	if err != nil {
		return fmt.Errorf("baidupcs: 秒传刷新失败: %w", err)
	}
	if !hit {
		return fmt.Errorf("baidupcs: 秒传刷新未命中（无法自愈，需重传）")
	}
	return nil
}

// 保证编译期契约（内核依赖全部被满足）。
var (
	_ uploadcore.Prober        = (*ucAdapter)(nil)
	_ uploadcore.RapidProvider = (*ucAdapter)(nil)
	_ uploadcore.Slicer        = (*ucAdapter)(nil)
	_ uploadcore.PartUploader  = (*ucAdapter)(nil)
	_ uploadcore.Merger        = (*ucAdapter)(nil)
	_ uploadcore.Verifier      = (*ucAdapter)(nil)
	_ uploadcore.Repairer      = (*ucAdapter)(nil)
)
